// Package stripe is the Stripe Checkout provider of admin.payment.
//
// Checkout creates a Checkout Session (POST /v1/checkout/sessions, mode
// "payment", one price_data line item, client_reference_id and
// metadata.order_id = the order) and sends the user to its url. The
// webhook accepts checkout.session.completed with payment_status "paid"
// and checkout.session.async_payment_succeeded; the Stripe-Signature header
// is checked as Stripe documents it: HMAC-SHA256 of "<t>.<raw body>" with
// the endpoint secret against every v1 value, and t within five minutes.
// Automatic tax and promotion codes are switched off on the session: the
// paid amount_total must equal the order, or the payment is logged and
// not credited.
//
// Plan subscriptions (admin.subscriptions) use Checkout in mode
// "subscription" with recurring price_data, the relay's subscription id
// as client_reference_id, metadata and subscription_data.metadata. Their
// webhook endpoint has its own signing secret and takes invoice.paid (a
// period paid: the subscription line's period is started, as Stripe
// advises provisioning on invoice.paid), customer.subscription.updated
// (cancel_at_period_end; a canceled, unpaid or incomplete_expired status
// ends it) and customer.subscription.deleted. Invoices are read in both
// payload shapes: since API version 2025-03-31.basil the subscription is
// under parent.subscription_details, before it at the top level
// (subscription, subscription_details).
// References: https://docs.stripe.com/api/checkout/sessions/create,
// https://docs.stripe.com/webhooks#verify-manually,
// https://docs.stripe.com/currencies,
// https://docs.stripe.com/billing/subscriptions/webhooks,
// https://docs.stripe.com/api/invoices/object,
// https://docs.stripe.com/api/subscriptions/object,
// https://docs.stripe.com/api/subscriptions/update.
package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/admin/payment"
)

// Name is the provider name in admin.payment routes.
const Name = "stripe"

// DefaultAPI is Stripe's API base.
const DefaultAPI = "https://api.stripe.com"

// Tolerance is how old a signed callback may be.
const Tolerance = 5 * time.Minute

func init() {
	payment.RegisterProvider(Name, factory,
		plugin.Setting{Key: "stripe_secret_key", Type: plugin.TypeSecret, Scopes: plugin.Global, Merge: plugin.Override,
			Label: plugin.T("Stripe secret key", "Stripe 密钥"),
			Help:  plugin.T("Starts with sk_live_ or sk_test_.", "以 sk_live_ 或 sk_test_ 开头。")},
		plugin.Setting{Key: "stripe_webhook_secret", Type: plugin.TypeSecret, Scopes: plugin.Global, Merge: plugin.Override,
			Label: plugin.T("Stripe top-up callback secret", "Stripe 充值回调签名密钥"),
			Help:  plugin.T("The signing secret (whsec_…) of the webhook pointing at /api/plugins/admin.payment/webhook/stripe.", "指向 /api/plugins/admin.payment/webhook/stripe 的回调的签名密钥（whsec_ 开头）。")},
		plugin.Setting{Key: "stripe_subscription_webhook_secret", Type: plugin.TypeSecret, Scopes: plugin.Global, Merge: plugin.Override,
			Label: plugin.T("Stripe plan callback secret", "Stripe 套餐回调签名密钥"),
			Help:  plugin.T("The signing secret (whsec_…) of the webhook pointing at /api/plugins/admin.subscriptions/webhook/stripe.", "指向 /api/plugins/admin.subscriptions/webhook/stripe 的回调的签名密钥（whsec_ 开头）。")},
		plugin.Setting{Key: "stripe_product_name", Type: plugin.TypeString, Default: "Balance top-up", Scopes: plugin.Global,
			Merge: plugin.Override, Label: plugin.T("Product name on the payment page", "支付页显示的商品名"),
			Help: plugin.T("Shown on the Stripe payment page.", "显示在 Stripe 支付页上。")},
	)
}

func factory(cfg payment.Config) (payment.Provider, bool) {
	get := func(k string) string {
		if cfg.Settings == nil {
			return ""
		}
		return cfg.Settings.Get(k).String()
	}
	p := &Provider{SecretKey: get("stripe_secret_key"), WebhookSecret: get("stripe_webhook_secret"),
		SubscriptionWebhookSecret: get("stripe_subscription_webhook_secret"),
		API:                       *apiBase.Load(), Product: get("stripe_product_name"), Client: cfg.Client}
	return p, p.SecretKey != "" && p.WebhookSecret != ""
}

// apiBase is the Stripe API the registered provider calls. Only tests and
// "plugrelay demo" change it (UseAPI), to reach a fake Stripe; it is not
// an operator setting.
var apiBase = func() *atomic.Pointer[string] {
	var p atomic.Pointer[string]
	s := DefaultAPI
	p.Store(&s)
	return &p
}()

// UseAPI points the registered provider at a fake Stripe at base, for
// tests and the demo; restore puts the previous address back.
func UseAPI(base string) (restore func()) {
	prev := apiBase.Swap(&base)
	return func() { apiBase.Store(prev) }
}

// Provider implements payment.Provider.
type Provider struct {
	SecretKey     string
	WebhookSecret string
	// SubscriptionWebhookSecret signs the plan subscriptions endpoint.
	SubscriptionWebhookSecret string
	API                       string
	Product                   string
	Client                    *http.Client
}

var (
	_ payment.RecurringProvider = (*Provider)(nil)
	_ payment.PaymentResolver   = (*Provider)(nil)
)

// unit is how a currency's amount is sent to Stripe: per is micro-units
// per Stripe amount unit, step the smallest amount Stripe can charge.
type unit struct{ per, step int64 }

// Stripe amounts are integers in the currency's minor unit
// (https://docs.stripe.com/currencies#minor-units):
//   - two-decimal (the default): cents;
//   - zero-decimal (https://docs.stripe.com/currencies#zero-decimal): whole units;
//   - ISK and UGX are zero-decimal but, for backward compatibility, sent as
//     two-decimal values whose decimals are always 00
//     (https://docs.stripe.com/currencies#special-cases);
//   - three-decimal BHD, JOD, KWD, OMR, TND
//     (https://docs.stripe.com/currencies#three-decimal): thousandths. The
//     last digit is kept 0 (0.010 the smallest step); that is a
//     conservative assumption, not a rule Stripe states for every account
//     (see also
//     https://support.stripe.com/questions/which-payments-methods-and-products-are-available-in-the-uae).
//
// HUF and TWD are zero-decimal only for payouts; charges are two-decimal.
// A currency missing here is refused rather than guessed.
var (
	twoDecimal   = unit{per: 10_000, step: 10_000}
	zeroDecimal  = unit{per: 1_000_000, step: 1_000_000}
	wholeAsCents = unit{per: 10_000, step: 1_000_000}
	threeDecimal = unit{per: 1_000, step: 10_000}
)

// units are Stripe's presentment currencies
// (https://docs.stripe.com/currencies#presentment-currencies).
var units = func() map[string]unit {
	m := map[string]unit{}
	for _, c := range strings.Fields(`usd aed afn all amd ang aoa ars aud awg azn bam bbd bdt bmd bnd bob brl bsd bwp
byn bzd cad cdf chf cny cop crc cve czk dkk dop dzd egp etb eur fjd fkp gbp gel gip gmd gtq gyd hkd hnl htg huf
idr ils inr jmd kes kgs khr kyd kzt lak lbp lkr lrd lsl mad mdl mkd mmk mnt mop mur mvr mwk mxn myr mzn nad ngn
nio nok npr nzd pab pen pgk php pkr pln qar ron rsd rub sar sbd scr sek sgd shp sle sos srd std szl thb tjs top
try ttd twd tzs uah uyu uzs wst xcd xcg yer zar zmw`) {
		m[c] = twoDecimal
	}
	for _, c := range strings.Fields("bif clp djf gnf jpy kmf krw mga pyg rwf vnd vuv xaf xof xpf") {
		m[c] = zeroDecimal
	}
	for _, c := range strings.Fields("isk ugx") {
		m[c] = wholeAsCents
	}
	for _, c := range strings.Fields("bhd jod kwd omr tnd") {
		m[c] = threeDecimal
	}
	return m
}()

// MinorUnits converts micro-units to Stripe's amount; ok=false for a
// currency Stripe does not charge in or an amount with a fraction it
// cannot charge.
func MinorUnits(micro int64, currency string) (int64, bool) {
	u, ok := units[strings.ToLower(currency)]
	if !ok || micro%u.step != 0 {
		return 0, false
	}
	return micro / u.per, true
}

// FromMinor converts a Stripe amount back to micro-units; ok=false for a
// currency it does not know.
func FromMinor(n int64, currency string) (int64, bool) {
	u, ok := units[strings.ToLower(currency)]
	if !ok {
		return 0, false
	}
	return n * u.per, true
}

// Checkout implements payment.Provider.
func (p *Provider) Checkout(ctx context.Context, o payment.Order, successURL, cancelURL string) (payment.Session, error) {
	amount, ok := MinorUnits(o.AmountMicro, o.Currency)
	if !ok || amount <= 0 {
		return payment.Session{}, fmt.Errorf("amount %d micro-units cannot be charged in %s through Stripe (unsupported currency or a fraction the currency cannot charge)", o.AmountMicro, o.Currency)
	}
	cur := strings.ToLower(o.Currency)
	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("success_url", successURL)
	form.Set("cancel_url", cancelURL)
	form.Set("client_reference_id", o.ID)
	form.Set("metadata[order_id]", o.ID)
	form.Set("payment_intent_data[metadata][order_id]", o.ID)
	// Stripe's shortest expiry, well inside ledger.CouponHold: a discounted
	// page cannot outlive the coupon use it holds.
	form.Set("expires_at", strconv.FormatInt(time.Now().Add(30*time.Minute+time.Minute).Unix(), 10))
	// The webhook credits exactly the ordered amount and refuses any other
	// amount_total, so nothing may change the price on Stripe's page:
	// no tax added on top, no promotion codes.
	form.Set("automatic_tax[enabled]", "false")
	form.Set("allow_promotion_codes", "false")
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", cur)
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(amount, 10))
	name := p.Product
	if name == "" {
		name = "Balance top-up"
	}
	form.Set("line_items[0][price_data][product_data][name]", name)
	return p.session(ctx, form, o.ID)
}

// ExpireSession implements payment.SessionExpirer. Stripe refuses it for a
// session already paid, so a payment never races a released coupon.
func (p *Provider) ExpireSession(ctx context.Context, sessionID string) error {
	if sessionID == "" || strings.ContainsAny(sessionID, "/?#") {
		return fmt.Errorf("stripe: bad session id %q", sessionID)
	}
	return p.post(ctx, "/v1/checkout/sessions/"+sessionID+"/expire", url.Values{}, "", &struct{}{})
}

// session creates a Checkout Session from form.
func (p *Provider) session(ctx context.Context, form url.Values, idempotencyKey string) (payment.Session, error) {
	var out struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := p.post(ctx, "/v1/checkout/sessions", form, idempotencyKey, &out); err != nil {
		return payment.Session{}, err
	}
	if out.URL == "" {
		return payment.Session{}, errors.New("stripe: the session has no url")
	}
	return payment.Session{ID: out.ID, URL: out.URL}, nil
}

// post sends a form to Stripe's API and decodes the answer into out.
func (p *Provider) post(ctx context.Context, path string, form url.Values, idempotencyKey string, out any) error {
	return p.call(ctx, http.MethodPost, path, form, idempotencyKey, out)
}

// get reads from Stripe's API with query parameters.
func (p *Provider) get(ctx context.Context, path string, query url.Values, out any) error {
	return p.call(ctx, http.MethodGet, path, query, "", out)
}

func (p *Provider) call(ctx context.Context, method, path string, form url.Values, idempotencyKey string, out any) error {
	base := strings.TrimRight(p.API, "/")
	if base == "" {
		base = DefaultAPI
	}
	var body io.Reader
	u := base + path
	if method == http.MethodGet {
		if len(form) > 0 {
			u += "?" + form.Encode()
		}
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.SecretKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if p.Client == nil {
		return errors.New("no HTTP client")
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var e struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	if resp.StatusCode/100 != 2 {
		msg := resp.Status
		if e.Error != nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return fmt.Errorf("stripe: %s", msg)
	}
	return json.Unmarshal(raw, out)
}

// Sign returns a Stripe-Signature header value for body at t, as Stripe
// computes it; tests and the fake server use it.
func Sign(secret string, body []byte, t time.Time) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// verifySignature checks a Stripe-Signature header.
func verifySignature(secret, header string, body []byte, now time.Time) error {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return fmt.Errorf("%w: malformed Stripe-Signature", payment.ErrBadSignature)
	}
	if d := now.Sub(time.Unix(sec, 0)); d > Tolerance || d < -Tolerance {
		return fmt.Errorf("%w: timestamp outside the tolerance", payment.ErrBadSignature)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, s := range sigs {
		if got, err := hex.DecodeString(s); err == nil && hmac.Equal(got, want) {
			return nil
		}
	}
	return fmt.Errorf("%w: no matching v1 signature", payment.ErrBadSignature)
}

// Verify implements payment.Provider.
func (p *Provider) Verify(h http.Header, body []byte, now time.Time) (payment.Event, error) {
	if err := verifySignature(p.WebhookSecret, h.Get("Stripe-Signature"), body, now); err != nil {
		return payment.Event{}, err
	}
	var ev struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Object struct {
				Object            string            `json:"object"`
				ID                string            `json:"id"`
				ClientReferenceID string            `json:"client_reference_id"`
				Metadata          map[string]string `json:"metadata"`
				PaymentStatus     string            `json:"payment_status"`
				AmountTotal       int64             `json:"amount_total"`
				Currency          string            `json:"currency"`
			} `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" {
		return payment.Event{}, fmt.Errorf("stripe: unreadable event")
	}
	out := payment.Event{ID: ev.ID}
	o := ev.Data.Object
	if o.Object != "checkout.session" {
		return out, nil
	}
	switch ev.Type {
	case "checkout.session.completed":
		out.Paid = o.PaymentStatus == "paid"
	case "checkout.session.async_payment_succeeded":
		out.Paid = true
	default:
		return out, nil
	}
	out.OrderID = o.Metadata["order_id"]
	if out.OrderID == "" {
		out.OrderID = o.ClientReferenceID
	}
	// An unknown currency reads as zero: never equal to an order, so
	// logged and not credited.
	out.AmountMicro, _ = FromMinor(o.AmountTotal, o.Currency)
	out.Currency = strings.ToUpper(o.Currency)
	return out, nil
}

// intervals are Stripe's recurring intervals for the plan periods.
var intervals = map[string]string{"week": "week", "month": "month", "year": "year"}

// SubscriptionsReady implements payment.RecurringProvider: the plan
// subscriptions endpoint needs its own signing secret.
func (p *Provider) SubscriptionsReady() bool { return p.SubscriptionWebhookSecret != "" }

// CheckoutSubscription implements payment.RecurringProvider.
func (p *Provider) CheckoutSubscription(ctx context.Context, s payment.SubscriptionOrder, successURL, cancelURL string) (payment.Session, error) {
	amount, ok := MinorUnits(s.AmountMicro, s.Currency)
	if !ok || amount <= 0 {
		return payment.Session{}, fmt.Errorf("price %d micro-units cannot be charged in %s through Stripe", s.AmountMicro, s.Currency)
	}
	interval, ok := intervals[s.Period]
	if !ok {
		return payment.Session{}, fmt.Errorf("stripe: unknown period %q", s.Period)
	}
	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("success_url", successURL)
	form.Set("cancel_url", cancelURL)
	form.Set("client_reference_id", s.ID)
	form.Set("metadata[subscription_id]", s.ID)
	form.Set("subscription_data[metadata][subscription_id]", s.ID)
	form.Set("automatic_tax[enabled]", "false")
	form.Set("allow_promotion_codes", "false")
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", strings.ToLower(s.Currency))
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(amount, 10))
	form.Set("line_items[0][price_data][recurring][interval]", interval)
	form.Set("line_items[0][price_data][product_data][name]", s.Name)
	return p.session(ctx, form, "sub:"+s.ID)
}

// CancelAtPeriodEnd implements payment.RecurringProvider
// (POST /v1/subscriptions/{id} with cancel_at_period_end).
func (p *Provider) CancelAtPeriodEnd(ctx context.Context, ref string, cancel bool) error {
	if ref == "" || strings.ContainsAny(ref, "/?#") {
		return fmt.Errorf("stripe: invalid subscription id %q", ref)
	}
	form := url.Values{"cancel_at_period_end": {strconv.FormatBool(cancel)}}
	var out struct {
		ID string `json:"id"`
	}
	return p.post(ctx, "/v1/subscriptions/"+ref, form, "", &out)
}

// stringOrID reads an expandable field: an id string or an object with id.
type stringOrID string

func (s *stringOrID) UnmarshalJSON(b []byte) error {
	var v string
	if json.Unmarshal(b, &v) == nil {
		*s = stringOrID(v)
		return nil
	}
	var o struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	*s = stringOrID(o.ID)
	return nil
}

type subDetails struct {
	Subscription stringOrID        `json:"subscription"`
	Metadata     map[string]string `json:"metadata"`
}

// VerifySubscription implements payment.RecurringProvider.
func (p *Provider) VerifySubscription(h http.Header, body []byte, now time.Time) (payment.SubscriptionEvent, error) {
	if err := verifySignature(p.SubscriptionWebhookSecret, h.Get("Stripe-Signature"), body, now); err != nil {
		return payment.SubscriptionEvent{}, err
	}
	var ev struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Data struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" {
		return payment.SubscriptionEvent{}, errors.New("stripe: unreadable event")
	}
	out := payment.SubscriptionEvent{ID: ev.ID}
	switch ev.Type {
	case "invoice.paid":
		var inv struct {
			Object     string `json:"object"`
			ID         string `json:"id"`
			AmountPaid int64  `json:"amount_paid"`
			Currency   string `json:"currency"`
			// Before 2025-03-31.basil.
			Subscription        stringOrID `json:"subscription"`
			SubscriptionDetails subDetails `json:"subscription_details"`
			// Since 2025-03-31.basil.
			Parent struct {
				Type                string     `json:"type"`
				SubscriptionDetails subDetails `json:"subscription_details"`
			} `json:"parent"`
			Lines struct {
				Data []struct {
					Type     string            `json:"type"`
					Metadata map[string]string `json:"metadata"`
					Period   struct {
						Start int64 `json:"start"`
						End   int64 `json:"end"`
					} `json:"period"`
					Parent struct {
						Type string `json:"type"`
					} `json:"parent"`
				} `json:"data"`
			} `json:"lines"`
		}
		if err := json.Unmarshal(ev.Data.Object, &inv); err != nil || inv.Object != "invoice" {
			return out, nil
		}
		out.Ref = string(inv.Parent.SubscriptionDetails.Subscription)
		if out.Ref == "" {
			out.Ref = string(inv.Subscription)
		}
		out.SubscriptionID = inv.Parent.SubscriptionDetails.Metadata["subscription_id"]
		if out.SubscriptionID == "" {
			out.SubscriptionID = inv.SubscriptionDetails.Metadata["subscription_id"]
		}
		// The subscription's line carries its period (the service period;
		// the invoice's own period_start/period_end are not).
		for _, l := range inv.Lines.Data {
			if l.Type != "subscription" && l.Parent.Type != "subscription_item_details" {
				continue
			}
			if l.Period.End > out.PeriodEnd.Unix() {
				out.PeriodStart, out.PeriodEnd = time.Unix(l.Period.Start, 0).UTC(), time.Unix(l.Period.End, 0).UTC()
			}
			if out.SubscriptionID == "" {
				out.SubscriptionID = l.Metadata["subscription_id"]
			}
		}
		if out.Ref == "" || out.PeriodEnd.IsZero() {
			return out, nil // not a subscription invoice
		}
		out.Kind, out.InvoiceID = "paid", inv.ID
		out.AmountMicro, _ = FromMinor(inv.AmountPaid, inv.Currency)
		out.Currency = strings.ToUpper(inv.Currency)
	case "charge.refunded":
		var ch struct {
			Object        string     `json:"object"`
			Refunded      bool       `json:"refunded"`
			PaymentIntent stringOrID `json:"payment_intent"`
			Invoice       stringOrID `json:"invoice"` // before 2025-03-31.basil
		}
		if err := json.Unmarshal(ev.Data.Object, &ch); err != nil || ch.Object != "charge" || !ch.Refunded {
			return out, nil // a partial refund leaves the subscription alone
		}
		out.Kind, out.PaymentRef, out.InvoiceID = "refunded", string(ch.PaymentIntent), string(ch.Invoice)
	case "charge.dispute.created":
		var d struct {
			Object        string     `json:"object"`
			Charge        stringOrID `json:"charge"`
			PaymentIntent stringOrID `json:"payment_intent"`
		}
		if err := json.Unmarshal(ev.Data.Object, &d); err != nil || d.Object != "dispute" {
			return out, nil
		}
		out.Kind, out.PaymentRef = "disputed", string(d.PaymentIntent)
		if out.PaymentRef == "" {
			out.PaymentRef = string(d.Charge)
		}
	case "customer.subscription.updated", "customer.subscription.deleted":
		var sub struct {
			Object            string            `json:"object"`
			ID                string            `json:"id"`
			Status            string            `json:"status"`
			CancelAtPeriodEnd bool              `json:"cancel_at_period_end"`
			Metadata          map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(ev.Data.Object, &sub); err != nil || sub.Object != "subscription" {
			return out, nil
		}
		out.Ref, out.SubscriptionID, out.CancelAtPeriodEnd = sub.ID, sub.Metadata["subscription_id"], sub.CancelAtPeriodEnd
		switch {
		case ev.Type == "customer.subscription.deleted", sub.Status == "canceled", sub.Status == "unpaid", sub.Status == "incomplete_expired":
			out.Kind = "ended"
		default:
			out.Kind = "updated"
		}
	}
	return out, nil
}

// SubscriptionOfPayment implements payment.PaymentResolver as Stripe
// documents for refunds and disputes since 2025-03-31.basil
// (https://docs.stripe.com/billing/subscriptions/webhooks): the
// PaymentIntent's paid invoice payments, then each invoice's subscription.
// An older event naming the invoice reads it directly; a PaymentRef that
// is a charge, not a PaymentIntent, is read for its PaymentIntent first.
func (p *Provider) SubscriptionOfPayment(ctx context.Context, ev payment.SubscriptionEvent) (string, error) {
	pi := ev.PaymentRef
	var invoices []string
	switch {
	case ev.InvoiceID != "":
		invoices = append(invoices, ev.InvoiceID)
	case pi != "" && !strings.HasPrefix(pi, "pi_"):
		id := pi
		if !validID(id) {
			return "", nil
		}
		var ch struct {
			PaymentIntent stringOrID `json:"payment_intent"`
			Invoice       stringOrID `json:"invoice"`
		}
		if err := p.get(ctx, "/v1/charges/"+id, nil, &ch); err != nil {
			return "", err
		}
		pi = string(ch.PaymentIntent)
		if ch.Invoice != "" {
			invoices = append(invoices, string(ch.Invoice))
		}
	}
	if len(invoices) == 0 && pi != "" {
		q := url.Values{"payment[type]": {"payment_intent"}, "payment[payment_intent]": {pi}, "status": {"paid"}, "limit": {"100"}}
		var list struct {
			Data []struct {
				Invoice stringOrID `json:"invoice"`
			} `json:"data"`
		}
		if err := p.get(ctx, "/v1/invoice_payments", q, &list); err != nil {
			return "", err
		}
		for _, ip := range list.Data {
			invoices = append(invoices, string(ip.Invoice))
		}
	}
	for _, id := range invoices {
		if !validID(id) {
			continue
		}
		var inv struct {
			Subscription stringOrID `json:"subscription"`
			Parent       struct {
				SubscriptionDetails subDetails `json:"subscription_details"`
			} `json:"parent"`
		}
		if err := p.get(ctx, "/v1/invoices/"+id, nil, &inv); err != nil {
			return "", err
		}
		if ref := string(inv.Parent.SubscriptionDetails.Subscription); ref != "" {
			return ref, nil
		}
		if inv.Subscription != "" {
			return string(inv.Subscription), nil
		}
	}
	return "", nil
}

func validID(id string) bool { return id != "" && !strings.ContainsAny(id, "/?#") }
