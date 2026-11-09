package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/initxy/plugrelay/internal/app"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/plugins/admin/payment/stripe"
)

// The demo's Stripe: the fake upstream answers Checkout Sessions, sends
// the payer straight back to the console and, once the relay is
// listening, delivers the signed checkout.session.completed a moment
// later, as Stripe would.
const (
	demoStripeKey    = "sk_test_demo"
	demoStripeSecret = "whsec_demo"
)

// demoRelay is the relay's own address, for the fake Stripe's callbacks;
// empty while seeding (the seed delivers its events itself).
var demoRelay atomic.Pointer[string]

var stripeSessions atomic.Int64

// fakeCheckout answers POST /v1/checkout/sessions.
func fakeCheckout(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id := fmt.Sprintf("cs_demo_%d", stripeSessions.Add(1))
	order := r.PostForm.Get("client_reference_id")
	cents, _ := strconv.ParseInt(r.PostForm.Get("line_items[0][price_data][unit_amount]"), 10, 64)
	writeJSON(w, map[string]any{"id": id, "object": "checkout.session", "mode": r.PostForm.Get("mode"),
		"url": r.PostForm.Get("success_url"), "subscription": nil})
	if base := demoRelay.Load(); base != nil && r.PostForm.Get("mode") == "payment" {
		go func() {
			time.Sleep(1500 * time.Millisecond)
			_ = deliverPaid(*base, order, cents)
		}()
	}
}

// deliverPaid posts a signed checkout.session.completed for order to the
// relay at base.
func deliverPaid(base, order string, cents int64) error {
	body, _ := json.Marshal(map[string]any{"id": "evt_" + order, "object": "event", "type": "checkout.session.completed",
		"data": map[string]any{"object": map[string]any{"object": "checkout.session", "id": "cs_" + order,
			"client_reference_id": order, "metadata": map[string]string{"order_id": order}, "payment_status": "paid",
			"amount_total": cents, "currency": "usd"}}})
	req, err := http.NewRequest(http.MethodPost, base+"/api/plugins/admin.payment/webhook/stripe", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", stripe.Sign(demoStripeSecret, body, time.Now()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("webhook: %d", resp.StatusCode)
	}
	return nil
}

// billingSettings are the billing feature's demo settings: Stripe keys
// the fake Stripe accepts, and every operations switch on.
func billingSettings() map[string]any {
	return map[string]any{
		"admin.payment": map[string]any{"stripe_secret_key": demoStripeKey, "stripe_webhook_secret": demoStripeSecret,
			"coupons_enabled": true},
		"admin.rewards": map[string]any{"checkin_enabled": true, "checkin_min_micro": 0.1, "checkin_max_micro": 0.5,
			"referral_enabled": true, "referral_mode": "percent", "referral_percent": 10, "referral_cap_micro": 50},
		"admin.announcements": map[string]any{"enabled": true},
	}
}

// pointPayments points the demo's Stripe at this run's fake upstream and
// its payment return address (the site's public URL) at consoleURL.
func pointPayments(c *api, fakeURL, consoleURL string) error {
	stripe.UseAPI(fakeURL)
	return c.v1("PATCH", "/settings", map[string]any{"values": map[string]any{"site.public_base_url": consoleURL}}, nil)
}

// seedOperations gives the operations pages their data through the same
// calls people make: coupons, announcements, sign-ups through invite
// links, top-ups paid (some with a coupon, one never paid, one canceled),
// check-ins and a plan bought from the balance.
func seedOperations(ctx context.Context, a *app.App, c *api, w *world, fakeURL string, now time.Time) error {
	if err := pointPayments(c, fakeURL, c.base); err != nil {
		return err
	}
	if err := c.v1("PATCH", "/features/billing", map[string]any{"settings": billingSettings()}, nil); err != nil {
		return err
	}
	if err := c.v1("PATCH", "/features/signup", map[string]any{"enabled": true}, nil); err != nil {
		return err
	}
	if err := a.Reload(ctx); err != nil {
		return err
	}
	day := func(d int) string { return now.Add(time.Duration(d) * 24 * time.Hour).UTC().Format(time.RFC3339) }
	for _, cp := range []map[string]any{
		{"code": "WELCOME10", "percent": 10, "max_discount_micro": micro(5), "max_uses": 200, "per_user": 1, "note": "New customers"},
		{"code": "SPRING5", "amount_micro": micro(5), "min_topup_micro": micro(20), "max_uses": 50, "per_user": 1, "ends_at": day(14),
			"note": "Spring promotion"},
		{"code": "LAUNCH30", "percent": 30, "max_discount_micro": micro(30), "max_uses": 20, "starts_at": day(-30), "ends_at": day(-23),
			"note": "Launch week"},
		{"code": "PARTNER20", "percent": 20, "per_user": 0, "starts_at": day(3), "note": "Partner program, starts next week"},
	} {
		if err := c.plugin("admin.payment", "POST", "/coupons", cp, nil); err != nil {
			return err
		}
	}
	for _, an := range []map[string]any{
		{"title": "Claude Opus 4.7 is available", "audience": "all",
			"body": "Pick **claude-opus-4-7** in your client. Members of the vip group pay 20% less, as with every model."},
		{"title": "Maintenance on Saturday", "audience": "members", "starts_at": day(-1), "ends_at": day(5),
			"body": "Saturday 02:00–02:30 UTC: requests may fail for a few seconds while we move the database.\n\n- Streams in flight are retried by most clients\n- Nothing to do on your side"},
		{"title": "Spring top-up offer", "audience": "members", "starts_at": day(2), "ends_at": day(16),
			"body": "Use code `SPRING5` for 5 USD off a top-up of 20 USD or more."},
		{"title": "New invoices page", "audience": "admins", "starts_at": day(-20), "ends_at": day(-10), "body": "Orders now live under Operations."},
	} {
		if err := c.plugin("admin.announcements", "POST", "/announcements", an, nil); err != nil {
			return err
		}
	}

	member := func(email string) (*api, error) {
		m := newAPI(c.base)
		return m, m.login(email, password)
	}
	alice, err := member(userSpecs[1].email)
	if err != nil {
		return err
	}
	var rw struct {
		Referral struct {
			Code string `json:"code"`
		} `json:"referral"`
	}
	if err := alice.plugin("admin.rewards", "GET", "/me", nil, &rw); err != nil {
		return err
	}
	invited := []string{"erin@demo.local", "frank@demo.local", "grace@demo.local"}
	for i, email := range invited {
		in := map[string]any{"email": email, "password": password, "name": []string{"Erin Lopez", "Frank Ito", "Grace Park"}[i],
			"referral_code": rw.Referral.Code}
		if err := c.do("POST", "/api/plugins/admin.signup/register", in, nil); err != nil {
			return err
		}
	}

	// Top-ups: who, how much, coupon, and whether the payment arrives.
	type topup struct {
		email, coupon string
		usd           float64
		outcome       string // paid, pending, canceled
	}
	for _, t := range []topup{
		{userSpecs[2].email, "", 20, "paid"},
		{userSpecs[3].email, "WELCOME10", 50, "paid"},
		{"erin@demo.local", "WELCOME10", 30, "paid"},
		{"frank@demo.local", "SPRING5", 25, "paid"},
		{"grace@demo.local", "", 10, "pending"},
		{userSpecs[4].email, "", 100, "canceled"},
		{userSpecs[1].email, "", 50, "paid"},
	} {
		m, err := member(t.email)
		if err != nil {
			return err
		}
		in := map[string]any{"amount_micro": micro(t.usd), "provider": "stripe"}
		if t.coupon != "" {
			in["coupon"] = t.coupon
		}
		var out struct {
			OrderID string `json:"order_id"`
		}
		if err := m.plugin("admin.payment", "POST", "/checkout", in, &out); err != nil {
			return fmt.Errorf("checkout for %s: %w", t.email, err)
		}
		var o struct {
			Pay struct {
				Micro int64 `json:"micro"`
			} `json:"pay"`
		}
		if err := m.plugin("admin.payment", "GET", "/orders/"+out.OrderID, nil, &o); err != nil {
			return err
		}
		switch t.outcome {
		case "paid":
			if err := deliverPaid(c.base, out.OrderID, o.Pay.Micro/10_000); err != nil {
				return err
			}
		case "canceled":
			if err := m.plugin("admin.payment", "POST", "/orders/"+out.OrderID+"/cancel", map[string]any{}, nil); err != nil {
				return err
			}
		}
	}

	// Check-ins: today through the console, earlier days through the store
	// (the console only checks in for today).
	l, _ := store.LedgerOf(a.Store())
	for i, u := range []string{"alice", "bob", "dave"} {
		for d := 1; d <= 6-2*i; d++ {
			at := now.Add(-time.Duration(d) * 24 * time.Hour)
			if _, _, err := l.CheckIn(ctx, w.users[u], "user:"+w.users[u], at.UTC().Format(time.DateOnly),
				int64(100_000+(d*37%5)*100_000), at); err != nil {
				return err
			}
		}
	}
	for _, email := range []string{userSpecs[1].email, userSpecs[2].email} {
		m, err := member(email)
		if err != nil {
			return err
		}
		if err := m.plugin("admin.rewards", "POST", "/checkin", map[string]any{}, nil); err != nil {
			return err
		}
	}

	// Bob buys the weekly token plan from his balance.
	var plans struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := c.plugin("admin.subscriptions", "GET", "/plans?all=1", nil, &plans); err != nil {
		return err
	}
	bob, err := member(userSpecs[2].email)
	if err != nil {
		return err
	}
	for _, p := range plans.Items {
		if p.Name == "Starter" {
			if err := bob.plugin("admin.subscriptions", "POST", "/subscribe", map[string]any{"plan_id": p.ID, "provider": "balance"}, nil); err != nil {
				return err
			}
		}
	}
	return nil
}
