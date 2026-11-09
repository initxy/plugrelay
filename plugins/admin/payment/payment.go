// Package payment is the admin.payment plugin (sell kit): users top up
// their balance through a payment provider; the provider's signed callback
// credits it.
//
// Providers implement Provider and register with RegisterProvider; Stripe
// Checkout ships in plugins/admin/payment/stripe and the epay protocol
// (Alipay, WeChat Pay) in plugins/admin/payment/epay. A top-up is an order in
// the core's order table; the provider callback is verified by the provider,
// then the order is credited as the ledger transaction "topup:<order>", so
// a replayed or duplicated callback, or two events for one payment, credit
// once. Event ids that credited are remembered for a week and answered
// 200 without work; one that credited nothing (not paid yet, an amount
// that did not match) is not, so a later event under the same id still
// credits.
package payment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

// ID is the plugin id.
const ID = "admin.payment"

func init() { plugin.Register(ID, func() plugin.Plugin { return New(nil, nil) }) }

// Order is what a provider charges for: one top-up or plan period.
// AmountMicro is the amount to charge, after any coupon.
type Order struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	Subject     string `json:"subject"`
	AmountMicro int64  `json:"amount_micro"`
	Currency    string `json:"currency"`
	Provider    string `json:"provider"`
	// Method is the provider's payment method when it offers several
	// (epay: "alipay", "wxpay").
	Method    string    `json:"method,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Status    string    `json:"status"` // pending, paid
	CreatedAt time.Time `json:"created_at"`
	PaidAt    time.Time `json:"paid_at,omitzero"`

	// CallbackURL is the public base of the ordering plugin's API
	// (https://relay/api/plugins/<id>), for providers that take callback
	// URLs per order: <CallbackURL>/webhook/<provider> and
	// <CallbackURL>/return/<provider>.
	CallbackURL string `json:"-"`
	// ClientIP is the paying user's address, for providers that want it.
	ClientIP string `json:"-"`
}

// Session is where to send the user to pay.
type Session struct {
	ID  string
	URL string
}

// Event is a verified provider callback.
type Event struct {
	ID string
	// OrderID is set when the event completes a payment for an order.
	OrderID     string
	Paid        bool
	AmountMicro int64
	Currency    string
}

// ErrBadSignature: the callback was not signed by the provider.
var ErrBadSignature = errors.New("payment: callback signature invalid")

// Provider is one payment service.
type Provider interface {
	// Checkout starts paying for o; the user is sent to Session.URL.
	Checkout(ctx context.Context, o Order, successURL, cancelURL string) (Session, error)
	// Verify authenticates a callback and parses it. A callback that
	// changes no order returns an Event with an empty OrderID.
	Verify(header http.Header, body []byte, now time.Time) (Event, error)
}

// SessionExpirer is implemented by providers that can end a checkout
// session before it is paid. A canceled order whose session was expired
// gives its coupon use back at once.
type SessionExpirer interface {
	ExpireSession(ctx context.Context, sessionID string) error
}

// FormProvider is implemented by providers whose callbacks are form
// parameters (a GET query or a POST form) instead of a signed body, and
// that expect a reply of their own: epay notifies with GET and wants the
// text "success". The same signed parameters come back on the user's
// return to the site (/return/<provider>), so either credits the order.
type FormProvider interface {
	VerifyForm(form url.Values, now time.Time) (Event, error)
	// Ack is the reply body to a callback, handled (ok) or not.
	Ack(ok bool) string
}

// MethodProvider is implemented by providers that offer several payment
// methods; Order.Method is one of them.
type MethodProvider interface {
	Methods() []string
}

// CurrencyProvider is implemented by providers that charge only in some
// currencies; others are offered for every site currency.
type CurrencyProvider interface {
	Currencies() []string
}

// SubscriptionOrder is a plan subscription a recurring provider bills
// every period.
type SubscriptionOrder struct {
	// ID is the relay's subscription id; the provider echoes it in its
	// subscription events.
	ID          string
	UserID      string
	Name        string
	AmountMicro int64
	Currency    string
	Period      string // week, month, year
}

// SubscriptionEvent is a verified recurring-billing callback.
type SubscriptionEvent struct {
	ID string
	// Kind is "paid" (a period was paid: start it), "updated" (the
	// cancel-at-period-end flag changed), "ended", "refunded" (a payment
	// was refunded in full) or "disputed" (a chargeback was opened); empty
	// for events that change nothing. Refunds and disputes name a payment
	// (PaymentRef, InvoiceID when known), not a subscription:
	// PaymentResolver finds it.
	Kind string
	// PaymentRef is the provider's payment id (Stripe: the PaymentIntent)
	// of a refund or dispute.
	PaymentRef string
	// SubscriptionID is the relay's id; Ref the provider's own.
	SubscriptionID    string
	Ref               string
	InvoiceID         string
	PeriodStart       time.Time
	PeriodEnd         time.Time
	AmountMicro       int64
	Currency          string
	CancelAtPeriodEnd bool
}

// RecurringProvider is implemented by providers that bill a subscription
// every period themselves (Stripe).
type RecurringProvider interface {
	// SubscriptionsReady reports whether recurring billing is configured
	// (Stripe: the subscriptions endpoint's signing secret).
	SubscriptionsReady() bool
	CheckoutSubscription(ctx context.Context, s SubscriptionOrder, successURL, cancelURL string) (Session, error)
	// CancelAtPeriodEnd sets (or clears) cancelling the provider's
	// subscription ref at the end of its period.
	CancelAtPeriodEnd(ctx context.Context, ref string, cancel bool) error
	VerifySubscription(header http.Header, body []byte, now time.Time) (SubscriptionEvent, error)
}

// PaymentResolver is implemented by recurring providers whose refund and
// dispute events name a payment: it returns the provider's subscription
// ref the payment paid for, "" when it paid for none.
type PaymentResolver interface {
	SubscriptionOfPayment(ctx context.Context, ev SubscriptionEvent) (string, error)
}

// SettingsHost is implemented by a plugin.Host that gives other sell-kit
// plugins this plugin's settings (provider keys), so that admin.subscriptions
// pays through the same providers.
type SettingsHost interface {
	PaymentSettings() plugin.Settings
}

// PublicURLHost is implemented by a plugin.Host that knows the relay's
// public URL (a site setting).
type PublicURLHost interface {
	PublicURL(ctx context.Context) string
}

// PublicURLOf returns h's public URL lookup, or nil.
func PublicURLOf(h plugin.Host) func(context.Context) string {
	if ph, ok := h.(PublicURLHost); ok {
		return ph.PublicURL
	}
	return nil
}

// Method is one way to pay offered to users.
type Method struct {
	Provider string `json:"provider"`
	Method   string `json:"method,omitempty"`
}

// Build returns the named provider built from cfg, if it is registered
// and configured.
func Build(name string, cfg Config) (Provider, bool) {
	regMu.RLock()
	f, ok := providers[name]
	regMu.RUnlock()
	if !ok {
		return nil, false
	}
	return f(cfg)
}

// Methods lists the configured providers' payment methods that can charge
// currency.
func Methods(cfg Config, currency string) []Method {
	var out []Method
	for _, n := range providerNames() {
		p, ok := Build(n, cfg)
		if !ok {
			continue
		}
		if !Charges(p, currency) {
			continue
		}
		mp, ok := p.(MethodProvider)
		if !ok {
			out = append(out, Method{Provider: n})
			continue
		}
		for _, m := range mp.Methods() {
			out = append(out, Method{Provider: n, Method: m})
		}
	}
	return out
}

// Charges reports whether prov can charge in currency.
func Charges(prov Provider, currency string) bool {
	cp, ok := prov.(CurrencyProvider)
	return !ok || slices.ContainsFunc(cp.Currencies(), func(c string) bool { return strings.EqualFold(c, currency) })
}

// CallbackForm returns a form callback's parameters: the query of a GET,
// the form of a POST.
func CallbackForm(r *http.Request) (url.Values, error) {
	if r.Method == http.MethodGet {
		return r.URL.Query(), nil
	}
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	return r.PostForm, nil
}

// WriteAck writes a FormProvider's reply.
func WriteAck(w http.ResponseWriter, prov FormProvider, status int, ok bool) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, prov.Ack(ok))
}

// Config is what a provider factory gets: the plugin's settings and the
// core's egress client.
type Config struct {
	Settings plugin.Settings
	Client   *http.Client
}

// Factory builds a provider from the current settings; it reports
// ok=false when the provider is not configured.
type Factory func(cfg Config) (p Provider, ok bool)

var (
	regMu     sync.RWMutex
	providers = map[string]Factory{}
	settings  []plugin.Setting
)

// RegisterProvider adds a provider and the settings it reads (named
// "<name>_..."). Call it from init.
func RegisterProvider(name string, f Factory, s ...plugin.Setting) {
	regMu.Lock()
	defer regMu.Unlock()
	providers[name] = f
	settings = append(settings, s...)
}

func providerNames() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(providers))
	for n := range providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Plugin implements plugin.Admin.
type Plugin struct {
	l        ledger.Ledger
	kv       plugin.KV
	settings plugin.Settings
	public   func(context.Context) string
	client   *http.Client
	log      *slog.Logger
	now      func() time.Time
}

var _ plugin.Admin = (*Plugin)(nil)

// New returns the plugin; nil fields are taken from the Host in Init.
func New(l ledger.Ledger, kv plugin.KV) *Plugin {
	return &Plugin{l: l, kv: kv, now: time.Now, log: slog.New(slog.DiscardHandler)}
}

// Info implements plugin.Plugin.
func (p *Plugin) Info() plugin.Info {
	regMu.RLock()
	extra := slices.Clone(settings)
	regMu.RUnlock()
	return plugin.Info{ID: ID, Kind: plugin.KindAdmin, Version: "1.0.0", ReportsOffBox: true,
		Description: "Top up balances through a payment provider (Stripe Checkout or epay); signed callbacks credit once.",
		Requires:    []string{"meter.balance"},
		Settings: append([]plugin.Setting{
			{Key: "min_topup_micro", Type: plugin.TypeInt, Default: 1_000_000, Scopes: plugin.Global, Merge: plugin.Override, Unit: plugin.UnitMoney,
				Label: plugin.T("Smallest top-up", "单次最少充值"),
				Help:  plugin.T("In the site currency.", "按站点币种。")},
			{Key: "max_topup_micro", Type: plugin.TypeInt, Default: 1_000_000_000, Scopes: plugin.Global, Merge: plugin.Override, Unit: plugin.UnitMoney,
				Label: plugin.T("Largest top-up", "单次最多充值"),
				Help:  plugin.T("In the site currency.", "按站点币种。")},
			{Key: "presets_micro", Type: plugin.TypeList, Default: []string{"5000000", "20000000", "100000000"},
				Scopes: plugin.Global, Merge: plugin.Override, Unit: plugin.UnitMoney, Label: plugin.T("Quick top-up amounts", "快捷充值金额"),
				Help: plugin.T("Offered as buttons on the top-up page, in the site currency.", "充值页上直接点选的金额，按站点币种。")},
			{Key: "coupons_enabled", Type: plugin.TypeBool, Default: false, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Coupons at top-up", "充值优惠券"),
				Help: plugin.T("Members can enter a coupon code when topping up; coupons are made on the Coupons page.",
					"成员充值时可以填优惠码；优惠码在「优惠券」页创建。")},
		}, extra...)}
}

// Init implements plugin.Initializer.
func (p *Plugin) Init(_ context.Context, h plugin.Host) error {
	if p.l == nil {
		l, err := ledger.From(h)
		if err != nil {
			return err
		}
		p.l = l
	}
	if p.kv == nil {
		p.kv = h.KV()
	}
	p.settings, p.client, p.log, p.public = h.Settings(), h.HTTPClient(), h.Logger(), PublicURLOf(h)
	return nil
}

// Routes implements plugin.Admin.
func (p *Plugin) Routes() []plugin.AdminRoute {
	return []plugin.AdminRoute{
		{Method: http.MethodGet, Path: "/config", Handler: p.getConfig},
		{Method: http.MethodPost, Path: "/checkout", Handler: p.postCheckout},
		{Method: http.MethodGet, Path: "/orders", Handler: p.getOrders, Role: "admin"},
		{Method: http.MethodGet, Path: "/orders/{id}", Handler: p.getOrder},
		{Method: http.MethodPost, Path: "/orders/{id}/cancel", Handler: p.postCancel},
		{Method: http.MethodPost, Path: "/coupon-check", Handler: p.postCouponCheck},
		{Method: http.MethodGet, Path: "/coupons", Handler: p.getCoupons, Role: "admin"},
		{Method: http.MethodPost, Path: "/coupons", Handler: p.postCoupon, Role: "admin"},
		{Method: http.MethodPut, Path: "/coupons/{id}", Handler: p.putCoupon, Role: "admin"},
		{Method: http.MethodGet, Path: "/coupons/{id}/uses", Handler: p.getCouponUses, Role: "admin"},
		{Method: http.MethodPost, Path: "/webhook/{provider}", Handler: p.postWebhook, Public: true},
		{Method: http.MethodGet, Path: "/webhook/{provider}", Handler: p.postWebhook, Public: true},
		{Method: http.MethodGet, Path: "/return/{provider}", Handler: p.getReturn, Public: true},
	}
}

func (p *Plugin) provider(name string) (Provider, bool) {
	return Build(name, Config{Settings: p.settings, Client: p.client})
}

// configured lists providers with their settings filled in.
func (p *Plugin) configured() []string {
	var out []string
	for _, n := range providerNames() {
		if _, ok := p.provider(n); ok {
			out = append(out, n)
		}
	}
	return out
}

func (p *Plugin) intSetting(key string, def int64) int64 {
	if p.settings == nil {
		return def
	}
	if v := p.settings.Get(key); v.IsSet() {
		return v.Int()
	}
	return def
}

func (p *Plugin) getConfig(w http.ResponseWriter, r *http.Request) {
	var presets []int64
	if p.settings != nil {
		for _, s := range p.settings.Get("presets_micro").List() {
			var n int64
			if _, err := fmt.Sscan(s, &n); err == nil && n > 0 {
				presets = append(presets, n)
			}
		}
	}
	cur := p.l.Currency(r.Context())
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"providers": p.configured(), "currency": cur,
		"methods":   Methods(Config{Settings: p.settings, Client: p.client}, cur),
		"min_micro": p.intSetting("min_topup_micro", 1_000_000), "max_micro": p.intSetting("max_topup_micro", 1_000_000_000),
		"presets_micro": presets, "coupons": p.couponsOn()})
}

func newOrderID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "ord_" + hex.EncodeToString(b)
}

func (p *Plugin) postCheckout(w http.ResponseWriter, r *http.Request) {
	uid, _ := ledger.Who(r)
	if uid == "" {
		ledger.WriteError(w, http.StatusForbidden, "forbidden", "a console user is required")
		return
	}
	var in struct {
		AmountMicro int64  `json:"amount_micro"`
		Provider    string `json:"provider"`
		Method      string `json:"method"`
		// KeyID picks the account when keys or groups pay (meter.balance payer).
		KeyID  string `json:"key_id"`
		Coupon string `json:"coupon"`
	}
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	names := p.configured()
	if in.Provider == "" && len(names) > 0 {
		in.Provider = names[0]
	}
	prov, ok := p.provider(in.Provider)
	if !ok {
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "payment provider not configured")
		return
	}
	if !Charges(prov, p.l.Currency(r.Context())) {
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", in.Provider+" does not charge in "+p.l.Currency(r.Context()))
		return
	}
	if !ValidMethod(prov, in.Method) {
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "payment method not offered by "+in.Provider)
		return
	}
	lo, hi := p.intSetting("min_topup_micro", 1_000_000), p.intSetting("max_topup_micro", 1_000_000_000)
	if in.AmountMicro < lo || in.AmountMicro > hi {
		cur := p.l.Currency(r.Context())
		ledger.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": "amount_out_of_range",
			"message": fmt.Sprintf("amount must be between %s and %s", ledger.Format(lo, cur), ledger.Format(hi, cur)),
			"params":  map[string]any{"min": ledger.Format(lo, cur), "max": ledger.Format(hi, cur)}}})
		return
	}
	if strings.TrimSpace(in.Coupon) != "" && !p.couponsOn() {
		ledger.Fail(w, ledger.ErrCouponInvalid)
		return
	}
	ctx := r.Context()
	subject, err := p.l.PayerOf(ctx, uid, in.KeyID)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	base, err := ConsoleURL(ctx, p.public)
	if err != nil {
		WriteNoConsoleURL(w, p.log)
		return
	}
	lo2 := ledger.Order{ID: newOrderID(), Kind: ledger.OrderTopup, UserID: uid, Subject: subject, AmountMicro: in.AmountMicro,
		PayMicro: in.AmountMicro, Currency: p.l.Currency(ctx), Provider: in.Provider, Method: in.Method, Status: ledger.OrderPending,
		CreatedAt: p.now().UTC()}
	if code := strings.TrimSpace(in.Coupon); code != "" {
		c, d, err := p.l.ReserveCoupon(ctx, code, uid, lo2.ID, in.AmountMicro, p.now())
		if err != nil {
			ledger.Fail(w, err)
			return
		}
		lo2.CouponID, lo2.CouponCode, lo2.DiscountMicro, lo2.PayMicro = c.ID, c.Code, d, in.AmountMicro-d
	}
	release := func() {
		if lo2.CouponID != "" {
			_ = p.l.ReleaseCoupon(context.WithoutCancel(ctx), lo2.ID)
		}
	}
	o := Order{ID: lo2.ID, UserID: uid, Subject: subject, AmountMicro: lo2.PayMicro, Currency: lo2.Currency,
		Provider: in.Provider, Method: in.Method, Status: ledger.OrderPending, CreatedAt: lo2.CreatedAt}
	o.CallbackURL, o.ClientIP = base+"/api/plugins/"+ID, ClientIP(r)
	back := base + TopupPath + "?order=" + o.ID
	sess, err := prov.Checkout(ctx, o, back, back+"&canceled=1")
	if err != nil {
		release()
		p.log.Warn("payment: checkout failed", "provider", in.Provider, "err", err)
		WriteProviderError(w)
		return
	}
	lo2.SessionID = sess.ID
	if err := p.l.CreateOrder(ctx, lo2); err != nil {
		release()
		ledger.Fail(w, err)
		return
	}
	ledger.WriteJSON(w, http.StatusCreated, map[string]any{"order_id": o.ID, "url": sess.URL})
}

// TopupPath is the console page users come back to after paying.
const TopupPath = "/billing"

func (p *Plugin) couponsOn() bool {
	return p.settings != nil && p.settings.Get("coupons_enabled").Bool()
}

// ErrNoConsoleURL: the relay's public URL is not set.
var ErrNoConsoleURL = errors.New("payment: set the site's public URL")

// ConsoleURL is the public console URL, without a trailing slash: the
// relay's public URL (a site setting). Never the request's Host header,
// which the client chooses: return and callback URLs built from it could
// send users and payment notices elsewhere.
func ConsoleURL(ctx context.Context, public func(context.Context) string) (string, error) {
	if public != nil {
		if u := public(ctx); u != "" {
			return strings.TrimRight(u, "/"), nil
		}
	}
	return "", ErrNoConsoleURL
}

// WriteNoConsoleURL answers a checkout that cannot build its return URLs.
func WriteNoConsoleURL(w http.ResponseWriter, log *slog.Logger) {
	log.Error("payment: checkout refused", "err", ErrNoConsoleURL)
	ledger.WriteError(w, http.StatusServiceUnavailable, "not_configured", "payments are not fully configured; ask the site operator")
}

// WriteProviderError answers a provider call that failed; the provider's
// own message is logged, not shown.
func WriteProviderError(w http.ResponseWriter) {
	ledger.WriteError(w, http.StatusBadGateway, "upstream", "the payment provider could not be reached or refused the request; try again later")
}

// ClientIP is the request's remote address without the port.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ValidMethod reports whether method is one prov offers (empty for a
// provider with a single method, or to take its first).
func ValidMethod(prov Provider, method string) bool {
	mp, ok := prov.(MethodProvider)
	if !ok {
		return method == ""
	}
	return method == "" || slices.Contains(mp.Methods(), method)
}

// order is the stored order id names; ok false when there is none.
func (p *Plugin) order(ctx context.Context, id string) (ledger.Order, bool, error) {
	o, err := p.l.Order(ctx, id)
	if errors.Is(err, ledger.ErrNotFound) {
		return o, false, nil
	}
	return o, err == nil, err
}

// pathID is the {id} segment of /orders/{id}/… and /coupons/{id}/….
func pathID(r *http.Request) string {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for i, s := range parts {
		if (s == "orders" || s == "coupons") && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func (p *Plugin) getOrder(w http.ResponseWriter, r *http.Request) {
	uid, role := ledger.Who(r)
	o, ok, err := p.order(r.Context(), pathID(r))
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	if !ok || (o.UserID != uid && role != "admin") {
		ledger.WriteError(w, http.StatusNotFound, "not_found", "order not found")
		return
	}
	ledger.WriteJSON(w, http.StatusOK, ViewOrder(o))
}

// OrderView is an order as the console shows it.
type OrderView struct {
	ledger.Order
	Amount   ledger.Money `json:"amount"`
	Pay      ledger.Money `json:"pay"`
	Discount ledger.Money `json:"discount"`
}

// ViewOrder adds o's amounts as money.
func ViewOrder(o ledger.Order) OrderView {
	return OrderView{Order: o, Amount: ledger.Money{Micro: o.AmountMicro, Currency: o.Currency},
		Pay: ledger.Money{Micro: o.PayMicro, Currency: o.Currency}, Discount: ledger.Money{Micro: o.DiscountMicro, Currency: o.Currency}}
}

// postCancel is the user coming back from the provider without paying:
// the order is canceled. Its coupon use comes back at once when the
// provider expired the checkout, else after ledger.CouponHold. A payment
// that still arrives for it credits as usual.
func (p *Plugin) postCancel(w http.ResponseWriter, r *http.Request) {
	uid, role := ledger.Who(r)
	ctx := r.Context()
	o, ok, err := p.order(ctx, pathID(r))
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	if !ok || (o.UserID != uid && role != "admin") {
		ledger.WriteError(w, http.StatusNotFound, "not_found", "order not found")
		return
	}
	release := false
	if prov, ok := p.provider(o.Provider); ok && o.Status == ledger.OrderPending && o.CouponID != "" && o.SessionID != "" {
		if ex, ok := prov.(SessionExpirer); ok {
			if err := ex.ExpireSession(ctx, o.SessionID); err != nil {
				p.log.Warn("payment: could not expire the checkout; its coupon use stays held", "order", o.ID, "err", err)
			} else {
				release = true
			}
		}
	}
	if _, err := p.l.CancelOrder(ctx, o.ID, release); err != nil {
		ledger.Fail(w, err)
		return
	}
	if o, _, err = p.order(ctx, o.ID); err != nil {
		ledger.Fail(w, err)
		return
	}
	ledger.WriteJSON(w, http.StatusOK, ViewOrder(o))
}

func (p *Plugin) getOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	ctx := r.Context()
	orders, next, err := p.l.Orders(ctx, ledger.OrderQuery{UserID: q.Get("user_id"), Status: q.Get("status"),
		Provider: q.Get("provider"), Kind: q.Get("kind"), Cursor: q.Get("cursor"), Limit: limit})
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	items := make([]OrderView, 0, len(orders))
	for _, o := range orders {
		items = append(items, ViewOrder(o))
	}
	out := map[string]any{"items": items, "next_cursor": next, "currency": p.l.Currency(ctx)}
	if q.Get("cursor") == "" {
		t, err := p.l.OrderTotals(ctx, p.now().Add(-30*24*time.Hour))
		if err != nil {
			ledger.Fail(w, err)
			return
		}
		out["totals"] = t
	}
	ledger.WriteJSON(w, http.StatusOK, out)
}

// postWebhook is the provider callback; it is public and authenticated by
// the provider's signature only.
func (p *Plugin) postWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	prov, ok := p.provider(name)
	if !ok {
		ledger.WriteError(w, http.StatusNotFound, "not_found", "payment provider not configured")
		return
	}
	fp, form := prov.(FormProvider)
	if form {
		vals, err := CallbackForm(r)
		if err != nil {
			WriteAck(w, fp, http.StatusBadRequest, false)
			return
		}
		ev, err := fp.VerifyForm(vals, p.now())
		if err != nil {
			p.log.Warn("payment: callback rejected", "provider", name, "err", err)
			WriteAck(w, fp, http.StatusBadRequest, false)
			return
		}
		if _, err := p.handle(r.Context(), name, ev); err != nil {
			WriteAck(w, fp, http.StatusInternalServerError, false)
			return
		}
		WriteAck(w, fp, http.StatusOK, true)
		return
	}
	if r.Method != http.MethodPost {
		ledger.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "callbacks are POSTed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "unreadable body")
		return
	}
	ev, err := prov.Verify(r.Header, body, p.now())
	if err != nil {
		p.log.Warn("payment: callback rejected", "provider", name, "err", err)
		ledger.WriteError(w, http.StatusBadRequest, "invalid_signature", "callback not verified")
		return
	}
	dup, err := p.handle(r.Context(), name, ev)
	if err != nil {
		ledger.WriteError(w, http.StatusInternalServerError, "internal", "could not record the payment")
		return
	}
	if dup {
		ledger.WriteJSON(w, http.StatusOK, map[string]any{"received": true, "duplicate": true})
		return
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"received": true})
}

// handle applies a verified event; dup when it credited before. An event
// is remembered only once it credited: a "not paid yet" notice or a
// mismatched amount under some id must not hide a later success under it.
func (p *Plugin) handle(ctx context.Context, name string, ev Event) (dup bool, err error) {
	return Once(ctx, p.kv, name, ev.ID, func() (bool, error) {
		done, err := p.apply(ctx, name, ev)
		if err != nil {
			p.log.Error("payment: crediting callback failed; the provider will retry", "provider", name, "event", ev.ID, "err", err)
		}
		return done, err
	})
}

// Once runs apply unless event id of provider is remembered as done, and
// remembers it for a week when apply reports done. dup is true when it was
// done before. Every change is idempotent in the store as well; this only
// saves work.
func Once(ctx context.Context, kv plugin.KV, provider, id string, apply func() (bool, error)) (dup bool, err error) {
	seen := "event:" + provider + ":" + id
	if _, dup, err := kv.Get(ctx, seen); err == nil && dup {
		return true, nil
	}
	done, err := apply()
	if err != nil || !done {
		return false, err
	}
	_ = kv.Put(ctx, seen, []byte("1"), 7*24*time.Hour)
	return false, nil
}

// getReturn is where a form provider sends the user back after paying,
// with the same signed parameters as its callback: a valid one credits the
// order (once, like the callback), then the user lands on the top-up page.
func (p *Plugin) getReturn(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	base, _ := ConsoleURL(r.Context(), p.public)
	back := base + TopupPath
	prov, ok := p.provider(name)
	fp, form := prov.(FormProvider)
	if !ok || !form {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	vals, _ := CallbackForm(r)
	ev, err := fp.VerifyForm(vals, p.now())
	if err != nil {
		p.log.Warn("payment: return rejected", "provider", name, "err", err)
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	if _, err := p.handle(r.Context(), name, ev); err != nil {
		p.log.Warn("payment: crediting on return failed; the callback will", "provider", name, "err", err)
	}
	if ev.OrderID != "" {
		back += "?order=" + url.QueryEscape(ev.OrderID)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// apply credits a paid order once; done when the order is credited.
func (p *Plugin) apply(ctx context.Context, provider string, ev Event) (bool, error) {
	if ev.OrderID == "" || !ev.Paid {
		return false, nil
	}
	o, ok, err := p.order(ctx, ev.OrderID)
	if err != nil {
		return false, err
	}
	if !ok || o.Provider != provider {
		p.log.Warn("payment: callback for an unknown order", "provider", provider, "order", ev.OrderID)
		return false, nil
	}
	if o.Kind != ledger.OrderTopup || ev.AmountMicro != o.PayMicro || !strings.EqualFold(ev.Currency, o.Currency) {
		p.log.Error("payment: paid amount differs from the order; not credited", "order", o.ID,
			"paid", ev.AmountMicro, "currency", ev.Currency, "ordered", o.PayMicro)
		return false, nil
	}
	note := provider + " " + ev.ID
	credit := o.AmountMicro
	if o.CouponCode != "" {
		note += " · " + o.CouponCode
		// Past the hold the use no longer counted against the coupon's
		// limits, so its discount is not honored: what was paid is credited.
		if p.now().Sub(o.CreatedAt) > ledger.CouponHold {
			credit = o.PayMicro
			note += " (coupon lapsed)"
			p.log.Warn("payment: paid after the coupon hold; credited the amount paid", "order", o.ID)
		}
	}
	if _, _, err := p.l.Credit(ctx, ledger.Txn{ID: "topup:" + o.ID, Subject: o.Subject, Kind: ledger.Topup,
		AmountMicro: credit, Ref: provider + ":" + o.SessionID, Note: note, Actor: o.UserID}); err != nil {
		return false, err
	}
	if _, err := p.l.PayOrder(ctx, o.ID, p.now().UTC()); err != nil {
		return false, err
	}
	// The inviter's reward is decided once per invitee in the store; a
	// failure here is retried by the provider's next notice.
	if _, err := p.l.RewardReferral(ctx, o.UserID, o.ID, o.PayMicro); err != nil {
		return false, err
	}
	return true, nil
}
