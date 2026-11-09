// Package subscriptions is the admin.subscriptions plugin (sell kit):
// admin-defined plans that users pay for per period, each period carrying
// an allowance (money or tokens) that requests draw on before the prepaid
// balance; meter.balance does the drawing.
//
// Plans live in the plugin's KV; a subscription copies its plan's terms
// into the core's store, so editing a plan changes only new subscriptions.
// A period is paid from the balance ("balance"), by a provider's recurring
// billing (Stripe: renewed by its invoice.paid webhook), or once at a
// one-off provider (epay), after which it renews from the balance. Paying
// providers are admin.payment's, configured on its settings; callbacks for
// plan payments come to this plugin's /webhook/<provider>. There is no
// proration: switching plans means cancelling and subscribing again once
// the current period ends. A user has one live subscription (the store
// enforces it); a payment that arrives for another one is recorded and
// flagged for a refund, which an admin makes at the provider.
package subscriptions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/admin/payment"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

// ID is the plugin id.
const ID = "admin.subscriptions"

// Balance is the pseudo-provider that pays from the prepaid balance.
const Balance = "balance"

func init() { plugin.Register(ID, func() plugin.Plugin { return New(nil, nil, nil) }) }

// Plan is an admin-defined plan.
type Plan struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	PriceMicro  int64  `json:"price_micro"`
	Currency    string `json:"currency"`
	Period      string `json:"period"`         // week, month, year
	Unit        string `json:"allowance_unit"` // money, tokens
	Allowance   int64  `json:"allowance"`
	Overage     string `json:"overage"` // block, balance
	// GroupID is the group subscribers are moved into while subscribed:
	// its model allowlist, rate limits and price multiplier apply to them.
	GroupID   string    `json:"group_id,omitempty"`
	Disabled  bool      `json:"disabled,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// Plugin implements plugin.Admin.
type Plugin struct {
	l    ledger.Ledger
	subs ledger.Subscriptions
	kv   plugin.KV
	// pay is admin.payment's settings (provider keys); nil when the host
	// does not share them, and plans are then paid from the balance only.
	pay plugin.Settings
	// public is the relay's configured public URL, when the host knows it.
	public func(context.Context) string
	// client is the core egress, for providers.
	client *http.Client
	log    *slog.Logger
	now    func() time.Time
	mu     sync.Mutex // plan edits
}

var (
	_ plugin.Admin          = (*Plugin)(nil)
	_ plugin.UserDeleteHook = (*Plugin)(nil)
	_ plugin.SettingsCheck  = (*Plugin)(nil)
)

// New returns the plugin; nil fields are taken from the Host in Init.
func New(l ledger.Ledger, subs ledger.Subscriptions, kv plugin.KV) *Plugin {
	return &Plugin{l: l, subs: subs, kv: kv, now: time.Now, log: slog.New(slog.DiscardHandler)}
}

// Info implements plugin.Plugin.
func (p *Plugin) Info() plugin.Info {
	return plugin.Info{ID: ID, Kind: plugin.KindAdmin, Version: "1.0.0", ReportsOffBox: true,
		Description: "Subscription plans: a price per period with an allowance requests draw on before the balance.",
		Requires:    []string{"meter.balance"}}
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
	if p.subs == nil {
		if p.subs = ledger.SubscriptionsFrom(h); p.subs == nil {
			return errors.New("this relay offers no subscription store")
		}
	}
	if p.kv == nil {
		p.kv = h.KV()
	}
	if sh, ok := h.(payment.SettingsHost); ok {
		p.pay = sh.PaymentSettings()
	}
	p.client, p.log, p.public = h.HTTPClient(), h.Logger(), payment.PublicURLOf(h)
	return nil
}

// Routes implements plugin.Admin.
func (p *Plugin) Routes() []plugin.AdminRoute {
	return []plugin.AdminRoute{
		{Method: http.MethodGet, Path: "/config", Handler: p.getConfig},
		{Method: http.MethodGet, Path: "/plans", Handler: p.getPlans},
		{Method: http.MethodPost, Path: "/plans", Handler: p.postPlan, Role: "admin"},
		{Method: http.MethodGet, Path: "/me", Handler: p.getMe},
		{Method: http.MethodPost, Path: "/subscribe", Handler: p.postSubscribe},
		{Method: http.MethodPost, Path: "/me/cancel", Handler: p.postMyCancel},
		{Method: http.MethodGet, Path: "/me/transactions", Handler: p.getMyTxns},
		{Method: http.MethodGet, Path: "/subscriptions", Handler: p.getSubscriptions, Role: "admin"},
		{Method: http.MethodPost, Path: "/subscriptions/cancel", Handler: p.postCancel, Role: "admin"},
		// The id is in the path so that the core's audit entry names it.
		{Method: http.MethodPost, Path: "/subscriptions/{id}/end", Handler: p.postEnd, Role: "admin", Reauth: true},
		{Method: http.MethodPost, Path: "/subscriptions/{id}/refunded", Handler: p.postRefunded, Role: "admin", Reauth: true},
		{Method: http.MethodPost, Path: "/webhook/{provider}", Handler: p.webhook, Public: true},
		{Method: http.MethodGet, Path: "/webhook/{provider}", Handler: p.webhook, Public: true},
		{Method: http.MethodGet, Path: "/return/{provider}", Handler: p.getReturn, Public: true},
	}
}

func (p *Plugin) payCfg() payment.Config { return payment.Config{Settings: p.pay, Client: p.client} }

// methods are the ways to pay for a plan: the balance, providers billing
// every period, and one-off providers.
func (p *Plugin) methods(currency string) []payment.Method {
	out := []payment.Method{{Provider: Balance}}
	for _, m := range payment.Methods(p.payCfg(), currency) {
		prov, _ := payment.Build(m.Provider, p.payCfg())
		if rp, ok := prov.(payment.RecurringProvider); ok && !rp.SubscriptionsReady() {
			continue
		}
		out = append(out, m)
	}
	return out
}

func (p *Plugin) getConfig(w http.ResponseWriter, r *http.Request) {
	cur := p.l.Currency(r.Context())
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"currency": cur, "methods": p.methods(cur)})
}

// ---------------------------------------------------------------- plans

func (p *Plugin) plans(ctx context.Context) ([]Plan, error) {
	b, ok, err := p.kv.Get(ctx, "plans")
	if err != nil || !ok {
		return nil, err
	}
	var out []Plan
	return out, json.Unmarshal(b, &out)
}

func (p *Plugin) plan(ctx context.Context, id string) (Plan, bool, error) {
	ps, err := p.plans(ctx)
	if err != nil {
		return Plan{}, false, err
	}
	for _, pl := range ps {
		if pl.ID == id {
			return pl, true, nil
		}
	}
	return Plan{}, false, nil
}

func (p *Plugin) getPlans(w http.ResponseWriter, r *http.Request) {
	_, role := ledger.Who(r)
	ps, err := p.plans(r.Context())
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	all := role == "admin" && r.URL.Query().Get("all") == "1"
	items := make([]Plan, 0, len(ps))
	for _, pl := range ps {
		if all || !pl.Disabled {
			items = append(items, pl)
		}
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func newID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

var (
	periods  = []string{"week", "month", "year"}
	units    = []string{ledger.UnitMoney, ledger.UnitTokens}
	overages = []string{ledger.OverageBlock, ledger.OverageBalance}
)

func (p *Plugin) postPlan(w http.ResponseWriter, r *http.Request) {
	var in Plan
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	switch {
	case in.Name == "":
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "a plan needs a name")
		return
	case in.PriceMicro < 0 || in.Allowance <= 0:
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "the price must not be negative and the allowance must be positive")
		return
	case !slices.Contains(periods, in.Period):
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "period must be week, month or year")
		return
	case !slices.Contains(units, in.Unit):
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "allowance_unit must be money or tokens")
		return
	case !slices.Contains(overages, in.Overage):
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "overage must be block or balance")
		return
	}
	ctx := r.Context()
	if in.GroupID != "" && p.groupsPay(ctx) {
		writeGroupsPay(w)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ps, err := p.plans(ctx)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	now := p.now().UTC()
	in.Currency = p.l.Currency(ctx)
	status := http.StatusCreated
	if i := slices.IndexFunc(ps, func(pl Plan) bool { return pl.ID == in.ID && in.ID != "" }); i >= 0 {
		in.CreatedAt, in.UpdatedAt, status = ps[i].CreatedAt, now, http.StatusOK
		ps[i] = in
	} else {
		if in.ID != "" {
			ledger.WriteError(w, http.StatusNotFound, "not_found", "plan not found")
			return
		}
		in.ID, in.CreatedAt = newID("plan_"), now
		ps = append(ps, in)
	}
	b, _ := json.Marshal(ps)
	if err := p.kv.Put(ctx, "plans", b, 0); err != nil {
		ledger.Fail(w, err)
		return
	}
	ledger.WriteJSON(w, status, in)
}

// groupsPay reports whether meter.balance charges groups (payer=group):
// a plan group would then change which account pays, so plans with a
// group are refused.
func (p *Plugin) groupsPay(ctx context.Context) bool { return p.l.PayerMode(ctx) == "group" }

func writeGroupsPay(w http.ResponseWriter) {
	ledger.WriteError(w, http.StatusConflict, "payer_group",
		"plans with a group cannot be used while groups pay (meter.balance payer=group): moving a subscriber into the plan group would change which balance pays")
}

// ---------------------------------------------------------------- views

type subView struct {
	ledger.Sub
	Current   bool  `json:"current"`
	Remaining int64 `json:"remaining"`
	// Currency is the site currency, of the price and of money allowances.
	Currency string `json:"currency"`
	Unit     string `json:"allowance_unit"`
}

func (p *Plugin) view(ctx context.Context, s ledger.Sub) subView {
	return subView{Sub: s, Current: s.Current(p.now()), Remaining: max(s.Left(), 0), Currency: p.l.Currency(ctx), Unit: s.Unit}
}

// shown is the subscription a user's page shows: the current one, else
// the newest one not ended.
func (p *Plugin) shown(ctx context.Context, uid string) (ledger.Sub, bool, error) {
	ss, err := p.subs.ListSubscriptions(ctx, uid, "")
	if err != nil {
		return ledger.Sub{}, false, err
	}
	now := p.now()
	for _, s := range ss {
		if s.Current(now) {
			return s, true, nil
		}
	}
	for _, s := range ss {
		if s.Status != ledger.SubEnded {
			return s, true, nil
		}
	}
	return ledger.Sub{}, false, nil
}

func user(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid, _ := ledger.Who(r)
	if uid == "" {
		ledger.WriteError(w, http.StatusForbidden, "forbidden", "a console user is required")
	}
	return uid, uid != ""
}

func (p *Plugin) getMe(w http.ResponseWriter, r *http.Request) {
	uid, ok := user(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	s, found, err := p.shown(ctx, uid)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	if !found {
		ledger.WriteJSON(w, http.StatusOK, map[string]any{"subscription": nil})
		return
	}
	out := map[string]any{"subscription": p.view(ctx, s)}
	if pl, ok, _ := p.plan(ctx, s.PlanID); ok {
		out["plan"] = pl
	}
	ledger.WriteJSON(w, http.StatusOK, out)
}

type txnView struct {
	ledger.Txn
	Amount       ledger.Money `json:"amount"`
	BalanceAfter ledger.Money `json:"balance_after"`
}

func (p *Plugin) getMyTxns(w http.ResponseWriter, r *http.Request) {
	uid, ok := user(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	s, found, err := p.shown(ctx, uid)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	if !found {
		ledger.WriteJSON(w, http.StatusOK, map[string]any{"items": []txnView{}, "next_cursor": ""})
		return
	}
	txns, next, err := p.l.Transactions(ctx, ledger.Query{Subject: ledger.AllowanceSubject(s.ID), Cursor: r.URL.Query().Get("cursor")})
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	cur := ""
	if s.Unit == ledger.UnitMoney {
		cur = p.l.Currency(ctx)
	}
	items := make([]txnView, 0, len(txns))
	for _, t := range txns {
		items = append(items, txnView{Txn: t, Amount: ledger.Money{Micro: t.AmountMicro, Currency: cur},
			BalanceAfter: ledger.Money{Micro: t.BalanceAfterMicro, Currency: cur}})
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (p *Plugin) getSubscriptions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ss, err := p.subs.ListSubscriptions(ctx, "", r.URL.Query().Get("status"))
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	items := make([]subView, 0, len(ss))
	for _, s := range ss {
		items = append(items, p.view(ctx, s))
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ---------------------------------------------------------------- subscribe

func (p *Plugin) postSubscribe(w http.ResponseWriter, r *http.Request) {
	uid, ok := user(w, r)
	if !ok {
		return
	}
	var in struct {
		PlanID   string `json:"plan_id"`
		Provider string `json:"provider"`
		Method   string `json:"method"`
		// KeyID picks the account renewals and overage are charged to when
		// keys or groups pay (meter.balance payer).
		KeyID string `json:"key_id"`
	}
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	pl, found, err := p.plan(ctx, in.PlanID)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	if !found || pl.Disabled {
		ledger.WriteError(w, http.StatusNotFound, "not_found", "plan not found")
		return
	}
	cur := p.l.Currency(ctx)
	if !strings.EqualFold(pl.Currency, cur) {
		ledger.WriteError(w, http.StatusConflict, "currency_changed", "the plan is priced in "+pl.Currency+" but the site currency is "+cur+"; an admin must save the plan again")
		return
	}
	if !slices.Contains(p.methods(cur), payment.Method{Provider: in.Provider, Method: in.Method}) {
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "payment method not offered")
		return
	}
	if pl.GroupID != "" && p.groupsPay(ctx) {
		writeGroupsPay(w)
		return
	}
	var base string
	if in.Provider != Balance {
		if base, err = payment.ConsoleURL(ctx, p.public); err != nil {
			payment.WriteNoConsoleURL(w, p.log)
			return
		}
	}
	payer, err := p.l.PayerOf(ctx, uid, in.KeyID)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	var prov payment.Provider
	renewal := ledger.RenewBalance
	if in.Provider != Balance {
		prov, _ = payment.Build(in.Provider, p.payCfg())
		if _, ok := prov.(payment.RecurringProvider); ok {
			renewal = ledger.RenewProvider
		}
	}
	s, err := p.subs.CreateSubscription(ctx, ledger.Sub{UserID: uid, Payer: payer, PlanID: pl.ID, PlanName: pl.Name,
		Period: pl.Period, PriceMicro: pl.PriceMicro, Unit: pl.Unit, Allowance: pl.Allowance, Overage: pl.Overage,
		GroupID: pl.GroupID, Provider: in.Provider, Renewal: renewal})
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	if in.Provider == Balance {
		now := p.now().UTC()
		_, err := p.subs.StartPeriod(ctx, s.ID, now, ledger.AddPeriod(now, pl.Period), ledger.Txn{
			ID: "subscription:" + s.ID + ":first", Subject: payer, Kind: ledger.Subscription, AmountMicro: -pl.PriceMicro,
			Note: pl.Name, Actor: uid})
		if err != nil {
			p.drop(s.ID)
			ledger.Fail(w, err)
			return
		}
		ledger.WriteJSON(w, http.StatusCreated, map[string]any{"subscription_id": s.ID, "status": ledger.SubActive})
		return
	}
	back := base + PlanPath + "?subscription=" + url.QueryEscape(s.ID)
	var sess payment.Session
	if rp, ok := prov.(payment.RecurringProvider); ok {
		sess, err = rp.CheckoutSubscription(ctx, payment.SubscriptionOrder{ID: s.ID, UserID: uid, Name: pl.Name,
			AmountMicro: pl.PriceMicro, Currency: cur, Period: pl.Period}, back, back+"&canceled=1")
	} else {
		o := ledger.Order{ID: newID("sord_"), Kind: ledger.OrderPlan, UserID: uid, Subject: payer, AmountMicro: pl.PriceMicro,
			PayMicro: pl.PriceMicro, Currency: cur, Provider: in.Provider, Method: in.Method, Ref: s.ID, Note: pl.Name,
			Status: ledger.OrderPending, CreatedAt: p.now().UTC()}
		po := payment.Order{ID: o.ID, UserID: uid, Subject: payer, AmountMicro: o.PayMicro, Currency: cur, Provider: in.Provider,
			Method: in.Method, Status: o.Status, CreatedAt: o.CreatedAt, CallbackURL: base + "/api/plugins/" + ID, ClientIP: payment.ClientIP(r)}
		if sess, err = prov.Checkout(ctx, po, back, back+"&canceled=1"); err == nil {
			o.SessionID = sess.ID
			err = p.l.CreateOrder(ctx, o)
		}
	}
	if err != nil {
		p.drop(s.ID)
		p.log.Warn("subscriptions: checkout failed", "provider", in.Provider, "err", err)
		payment.WriteProviderError(w)
		return
	}
	ledger.WriteJSON(w, http.StatusCreated, map[string]any{"subscription_id": s.ID, "status": ledger.SubPending, "url": sess.URL})
}

// drop ends a subscription whose first payment could not start.
func (p *Plugin) drop(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := p.subs.EndSubscription(ctx, id); err != nil {
		p.log.Warn("subscriptions: dropping an unpaid subscription failed", "subscription", id, "err", err)
	}
}

// PlanPath is the console page users come back to after paying for a plan.
const PlanPath = "/billing/plan"

// order is the plan order id names; ok false when there is none.
func (p *Plugin) order(ctx context.Context, id string) (ledger.Order, bool, error) {
	if id == "" {
		return ledger.Order{}, false, nil
	}
	o, err := p.l.Order(ctx, id)
	if errors.Is(err, ledger.ErrNotFound) {
		return o, false, nil
	}
	return o, err == nil && o.Kind == ledger.OrderPlan, err
}

// ---------------------------------------------------------------- cancel

func (p *Plugin) postMyCancel(w http.ResponseWriter, r *http.Request) {
	uid, ok := user(w, r)
	if !ok {
		return
	}
	var in struct {
		Cancel bool `json:"cancel"`
	}
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	s, found, err := p.shown(r.Context(), uid)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	if !found || s.Status != ledger.SubActive {
		ledger.WriteError(w, http.StatusNotFound, "not_found", "no active subscription")
		return
	}
	p.cancel(w, r, s, in.Cancel)
}

func (p *Plugin) postCancel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Cancel bool   `json:"cancel"`
	}
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	s, err := p.subs.Subscription(r.Context(), in.ID)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	if s.Status != ledger.SubActive {
		ledger.WriteError(w, http.StatusConflict, "not_active", "the subscription is not active")
		return
	}
	p.cancel(w, r, s, in.Cancel)
}

// cancel sets whether s ends at its period end instead of renewing; a
// provider-billed subscription is changed at the provider first.
func (p *Plugin) cancel(w http.ResponseWriter, r *http.Request, s ledger.Sub, cancel bool) {
	ctx := r.Context()
	if s.Renewal == ledger.RenewProvider {
		prov, _ := payment.Build(s.Provider, p.payCfg())
		rp, ok := prov.(payment.RecurringProvider)
		if !ok || s.ProviderRef == "" {
			ledger.WriteError(w, http.StatusConflict, "not_confirmed", "the provider has not confirmed this subscription yet")
			return
		}
		if err := rp.CancelAtPeriodEnd(ctx, s.ProviderRef, cancel); err != nil {
			p.log.Warn("subscriptions: provider cancel failed", "provider", s.Provider, "err", err)
			payment.WriteProviderError(w)
			return
		}
	}
	if err := p.subs.SetCancelAtPeriodEnd(ctx, s.ID, cancel); err != nil {
		ledger.Fail(w, err)
		return
	}
	s.CancelAtPeriodEnd = cancel
	ledger.WriteJSON(w, http.StatusOK, p.view(ctx, s))
}

// postEnd ends a subscription now (a refund or chargeback): its allowance
// is no longer drawn on and the user leaves the plan group. A
// provider-billed one is set to cancel at the provider first, so it bills
// no further period. The core audits the call.
func (p *Plugin) postEnd(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s, err := p.subs.Subscription(ctx, pathID(r))
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	if s.Renewal == ledger.RenewProvider && s.Status != ledger.SubEnded {
		// Ended here even when the provider refuses (it may have ended
		// the subscription already); the failure is logged.
		_ = p.stopAtProvider(ctx, s)
	}
	if _, err := p.subs.EndSubscription(ctx, s.ID); err != nil {
		ledger.Fail(w, err)
		return
	}
	uid, _ := ledger.Who(r)
	p.log.Warn("subscriptions: ended by an admin", "subscription", s.ID, "user", s.UserID, "admin", uid)
	if s, err = p.subs.Subscription(ctx, s.ID); err != nil {
		ledger.Fail(w, err)
		return
	}
	ledger.WriteJSON(w, http.StatusOK, p.view(ctx, s))
}

// BeforeUserDelete implements plugin.UserDeleteHook: the user's live
// subscriptions end, a provider-billed one cancelled at the provider
// first; when that fails, the user is not deleted (it would bill on)
// unless the deletion is forced, and an admin can end the subscription by
// hand first.
func (p *Plugin) BeforeUserDelete(ctx context.Context, userID string) error {
	for _, status := range []string{ledger.SubPending, ledger.SubActive} {
		subs, err := p.subs.ListSubscriptions(ctx, userID, status)
		if err != nil {
			return err
		}
		for _, s := range subs {
			if s.Renewal == ledger.RenewProvider {
				if err := p.stopAtProvider(ctx, s); err != nil {
					if !plugin.ForcedDelete(ctx) {
						return errors.New("subscription " + s.ID + " could not be cancelled at " + s.Provider + " (" + err.Error() +
							"), so it may bill on; end it under Plans, then delete the user, or force the deletion")
					}
					p.log.Warn("subscriptions: user deleted by force; not cancelled at the provider", "subscription", s.ID, "user", userID, "err", err)
				}
			}
			if _, err := p.subs.EndSubscription(ctx, s.ID); err != nil {
				return err
			}
			p.log.Warn("subscriptions: ended with its user", "subscription", s.ID, "user", userID)
		}
	}
	return nil
}

// CheckSettings implements plugin.SettingsCheck: meter.balance's payer
// cannot become "group" while a plan or a live subscription has a group
// (moving the subscriber would change which balance pays; see groupsPay).
func (p *Plugin) CheckSettings(ctx context.Context, pluginID string, scope plugin.Scope, values map[string]json.RawMessage) error {
	if pluginID != "meter.balance" || scope != plugin.Global {
		return nil
	}
	var payer string
	// Not skipped when groups pay already: settings stored while this
	// plugin was off are checked when it is turned on.
	if err := json.Unmarshal(values["payer"], &payer); err != nil || payer != "group" {
		return nil
	}
	ps, err := p.plans(ctx)
	if err != nil {
		return err
	}
	for _, pl := range ps {
		if pl.GroupID != "" {
			return errors.New("plan " + pl.Name + " moves subscribers into a group; groups cannot pay (meter.balance payer=group) while plans have groups: remove the plan groups first")
		}
	}
	for _, status := range []string{ledger.SubPending, ledger.SubActive} {
		subs, err := p.subs.ListSubscriptions(ctx, "", status)
		if err != nil {
			return err
		}
		for _, s := range subs {
			if s.GroupID != "" {
				return errors.New("subscription " + s.ID + " of plan " + s.PlanName + " has a group; groups cannot pay (meter.balance payer=group) until it ends")
			}
		}
	}
	return nil
}

// stopAtProvider cancels a provider-billed subscription at the end of its
// period at the provider, so it bills nothing more. It fails when that
// cannot be done: the provider is not configured (or bills no
// subscriptions), or the subscription has no provider reference yet (a
// checkout not completed, which may still start billing).
func (p *Plugin) stopAtProvider(ctx context.Context, s ledger.Sub) error {
	var err error
	prov, ok := payment.Build(s.Provider, p.payCfg())
	rp, recurring := prov.(payment.RecurringProvider)
	switch {
	case !ok:
		err = errors.New("payment provider " + s.Provider + " is not configured")
	case !recurring:
		err = errors.New("payment provider " + s.Provider + " cannot cancel subscriptions")
	case s.ProviderRef == "":
		err = errors.New("the provider has no subscription for it yet (checkout not completed)")
	default:
		err = rp.CancelAtPeriodEnd(ctx, s.ProviderRef, true)
	}
	if err != nil {
		p.log.Warn("subscriptions: provider cancel failed", "provider", s.Provider, "subscription", s.ID, "err", err)
	}
	return err
}

// pathID is the {id} of /subscriptions/{id}/<action>.
func pathID(r *http.Request) string {
	parts := strings.Split(strings.TrimRight(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2]
}

// postRefunded clears (or, with {"refunded": false}, sets again) a
// subscription's refund-due flag once an admin refunded the payment at the
// provider.
func (p *Plugin) postRefunded(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Refunded *bool `json:"refunded"`
	}
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	id := pathID(r)
	due := in.Refunded != nil && !*in.Refunded
	if err := p.subs.SetRefundDue(ctx, id, due); err != nil {
		ledger.Fail(w, err)
		return
	}
	s, err := p.subs.Subscription(ctx, id)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	ledger.WriteJSON(w, http.StatusOK, p.view(ctx, s))
}

// flagged reports a payment StartPeriod recorded without starting the
// subscription (another one of the user is active): it is logged for a
// refund and a provider-billed one is stopped from billing again.
func (p *Plugin) flagged(ctx context.Context, id, ref string) {
	s, err := p.subs.Subscription(ctx, id)
	if err != nil || !s.RefundDue || s.Status == ledger.SubActive {
		return
	}
	p.log.Error("subscriptions: payment for a subscription that cannot start (the user has another active one, or was deleted); refund it at the provider, then mark it refunded",
		"subscription", s.ID, "user", s.UserID, "provider", s.Provider, "payment", ref)
	if s.Renewal == ledger.RenewProvider {
		_ = p.stopAtProvider(ctx, s)
	}
}

// ---------------------------------------------------------------- callbacks

// webhook is the providers' callback for plan payments; it is public and
// authenticated by the provider's signature only.
func (p *Plugin) webhook(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	prov, ok := payment.Build(name, p.payCfg())
	if !ok {
		ledger.WriteError(w, http.StatusNotFound, "not_found", "payment provider not configured")
		return
	}
	ctx := r.Context()
	if fp, ok := prov.(payment.FormProvider); ok {
		form, err := payment.CallbackForm(r)
		if err != nil {
			payment.WriteAck(w, fp, http.StatusBadRequest, false)
			return
		}
		ev, err := fp.VerifyForm(form, p.now())
		if err != nil {
			p.log.Warn("subscriptions: callback rejected", "provider", name, "err", err)
			payment.WriteAck(w, fp, http.StatusBadRequest, false)
			return
		}
		if err := p.once(ctx, name, ev.ID, func() (bool, error) { return p.applyOrder(ctx, name, ev) }); err != nil {
			payment.WriteAck(w, fp, http.StatusInternalServerError, false)
			return
		}
		payment.WriteAck(w, fp, http.StatusOK, true)
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
	var apply func() (bool, error)
	var evID string
	if rp, ok := prov.(payment.RecurringProvider); ok {
		ev, verr := rp.VerifySubscription(r.Header, body, p.now())
		err, evID, apply = verr, ev.ID, func() (bool, error) { return true, p.applyRecurring(ctx, name, prov, ev) }
	} else {
		ev, verr := prov.Verify(r.Header, body, p.now())
		err, evID, apply = verr, ev.ID, func() (bool, error) { return p.applyOrder(ctx, name, ev) }
	}
	if err != nil {
		p.log.Warn("subscriptions: callback rejected", "provider", name, "err", err)
		ledger.WriteError(w, http.StatusBadRequest, "invalid_signature", "callback not verified")
		return
	}
	if err := p.once(ctx, name, evID, apply); err != nil {
		ledger.WriteError(w, http.StatusInternalServerError, "internal", "could not record the payment")
		return
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"received": true})
}

// once runs apply for an event id not done in the last week; one that did
// nothing (not paid yet, a mismatched amount) is not remembered.
func (p *Plugin) once(ctx context.Context, provider, id string, apply func() (bool, error)) error {
	_, err := payment.Once(ctx, p.kv, provider, id, apply)
	if err != nil {
		p.log.Error("subscriptions: applying a callback failed; the provider will retry", "provider", provider, "event", id, "err", err)
	}
	return err
}

// applyOrder starts the period a one-off payment paid for, once per order;
// done when the payment is recorded.
func (p *Plugin) applyOrder(ctx context.Context, provider string, ev payment.Event) (bool, error) {
	if ev.OrderID == "" || !ev.Paid {
		return false, nil
	}
	o, ok, err := p.order(ctx, ev.OrderID)
	if err != nil {
		return false, err
	}
	if !ok || o.Provider != provider {
		p.log.Warn("subscriptions: callback for an unknown order", "provider", provider, "order", ev.OrderID)
		return false, nil
	}
	if ev.AmountMicro != o.PayMicro || !strings.EqualFold(ev.Currency, o.Currency) {
		p.log.Error("subscriptions: paid amount differs from the order; not started", "order", o.ID,
			"paid", ev.AmountMicro, "currency", ev.Currency, "ordered", o.PayMicro)
		return false, nil
	}
	s, err := p.subs.Subscription(ctx, o.Ref)
	if err != nil {
		return false, err
	}
	start := p.now().UTC()
	if s.Current(start) {
		start = s.PeriodEnd // paid ahead: the next period
	}
	applied, err := p.subs.StartPeriod(ctx, s.ID, start, ledger.AddPeriod(start, s.Period), ledger.Txn{
		ID: "subscription:" + provider + ":" + o.ID, Subject: s.Payer, Kind: ledger.Subscription,
		Ref: provider + ":" + ev.ID, Note: s.PlanName + " (" + provider + ")", Actor: o.UserID})
	if err != nil {
		return false, err
	}
	if applied {
		p.flagged(ctx, s.ID, provider+":"+ev.ID)
	}
	if _, err := p.l.PayOrder(ctx, o.ID, p.now().UTC()); err != nil {
		return false, err
	}
	return true, nil
}

// applyRecurring applies a recurring provider's event to its subscription.
func (p *Plugin) applyRecurring(ctx context.Context, provider string, prov payment.Provider, ev payment.SubscriptionEvent) error {
	if ev.Kind == "" {
		return nil
	}
	if ev.Kind == "refunded" || ev.Kind == "disputed" {
		return p.applyReversal(ctx, provider, prov, ev)
	}
	var s ledger.Sub
	err := ledger.ErrNotFound
	if ev.SubscriptionID != "" {
		s, err = p.subs.Subscription(ctx, ev.SubscriptionID)
	}
	if errors.Is(err, ledger.ErrNotFound) && ev.Ref != "" {
		s, err = p.subs.SubscriptionByRef(ctx, provider, ev.Ref)
	}
	if errors.Is(err, ledger.ErrNotFound) {
		p.log.Warn("subscriptions: event for an unknown subscription", "provider", provider, "ref", ev.Ref)
		return nil
	}
	if err != nil {
		return err
	}
	if s.Provider != provider || (s.ProviderRef != "" && ev.Ref != "" && s.ProviderRef != ev.Ref) {
		p.log.Warn("subscriptions: event does not match the subscription", "provider", provider, "subscription", s.ID, "ref", ev.Ref)
		return nil
	}
	if s.ProviderRef == "" && ev.Ref != "" {
		if err := p.subs.SetSubscriptionRef(ctx, s.ID, ev.Ref); err != nil {
			return err
		}
	}
	switch ev.Kind {
	case "paid":
		if ev.AmountMicro != s.PriceMicro || !strings.EqualFold(ev.Currency, p.l.Currency(ctx)) {
			p.log.Error("subscriptions: invoice amount differs from the plan price; period not started", "subscription", s.ID,
				"paid", ev.AmountMicro, "currency", ev.Currency, "price", s.PriceMicro)
			return nil
		}
		applied, err := p.subs.StartPeriod(ctx, s.ID, ev.PeriodStart, ev.PeriodEnd, ledger.Txn{
			ID: "subscription:" + provider + ":" + ev.InvoiceID, Subject: s.Payer, Kind: ledger.Subscription,
			Ref: provider + ":" + ev.InvoiceID, Note: s.PlanName + " (" + provider + ")", Actor: s.UserID})
		if err == nil && applied {
			p.flagged(ctx, s.ID, provider+":"+ev.InvoiceID)
		}
		return err
	case "updated":
		if ev.CancelAtPeriodEnd != s.CancelAtPeriodEnd {
			return p.subs.SetCancelAtPeriodEnd(ctx, s.ID, ev.CancelAtPeriodEnd)
		}
	case "ended":
		_, err := p.subs.EndSubscription(ctx, s.ID)
		return err
	}
	return nil
}

// applyReversal handles a refunded payment or an opened dispute of a
// subscription invoice: the subscription it paid for ends now (and bills
// no more at the provider); a subscription already ended has its
// refund-due flag cleared by a refund. Payments the provider cannot tie
// to a subscription of ours are ignored.
func (p *Plugin) applyReversal(ctx context.Context, provider string, prov payment.Provider, ev payment.SubscriptionEvent) error {
	pr, ok := prov.(payment.PaymentResolver)
	if !ok {
		return nil
	}
	ref, err := pr.SubscriptionOfPayment(ctx, ev)
	if err != nil {
		return err
	}
	if ref == "" {
		return nil
	}
	s, err := p.subs.SubscriptionByRef(ctx, provider, ref)
	if errors.Is(err, ledger.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if s.Status == ledger.SubEnded {
		if s.RefundDue && ev.Kind == "refunded" {
			p.log.Info("subscriptions: flagged payment refunded", "subscription", s.ID, "payment", ev.PaymentRef)
			return p.subs.SetRefundDue(ctx, s.ID, false)
		}
		return nil
	}
	_ = p.stopAtProvider(ctx, s)
	if _, err := p.subs.EndSubscription(ctx, s.ID); err != nil {
		return err
	}
	p.log.Error("subscriptions: payment "+ev.Kind+"; subscription ended", "subscription", s.ID, "user", s.UserID,
		"provider", provider, "payment", ev.PaymentRef)
	return nil
}

// getReturn is where a form provider sends the user back after paying:
// its signed parameters start the period like the callback, then the user
// lands on the plan page.
func (p *Plugin) getReturn(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	base, _ := payment.ConsoleURL(r.Context(), p.public)
	back := base + PlanPath
	prov, _ := payment.Build(name, p.payCfg())
	fp, ok := prov.(payment.FormProvider)
	if !ok {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	form, _ := payment.CallbackForm(r)
	ev, err := fp.VerifyForm(form, p.now())
	if err != nil {
		p.log.Warn("subscriptions: return rejected", "provider", name, "err", err)
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	_ = p.once(ctx, name, ev.ID, func() (bool, error) { return p.applyOrder(ctx, name, ev) })
	if o, ok, _ := p.order(ctx, ev.OrderID); ok {
		back += "?subscription=" + url.QueryEscape(o.Ref)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
