package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/admin/payment/epay"
	"github.com/initxy/plugrelay/plugins/admin/payment/stripe"
)

const subWhsec = "whsec_subscriptions"

// paySetting stores an admin.payment setting (provider keys) and reloads.
func (r *sellRelay) paySettings(kv map[string]any) {
	r.t.Helper()
	ctx := context.Background()
	for k, v := range kv {
		raw, _ := json.Marshal(v)
		if err := r.st.PutSetting(ctx, store.SettingRow{Scope: plugin.Global, PluginID: "admin.payment", Key: k, Value: raw}); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := r.a.Reload(ctx); err != nil {
		r.t.Fatal(err)
	}
}

func (r *sellRelay) json(uid, path, body string) (int, map[string]any) {
	r.t.Helper()
	code, out := r.consolePost(uid, path, body)
	var m map[string]any
	_ = json.Unmarshal([]byte(out), &m)
	if m == nil {
		m = map[string]any{"raw": out}
	}
	return code, m
}

func (r *sellRelay) plan(body string) string {
	r.t.Helper()
	code, out := r.json("admin", "/api/plugins/admin.subscriptions/plans", body)
	if code != http.StatusCreated {
		r.t.Fatalf("plan: %d %v", code, out)
	}
	return out["id"].(string)
}

func (r *sellRelay) topup(amount int64) {
	r.t.Helper()
	if _, _, err := r.l.Credit(context.Background(), &store.BalanceTxn{ID: fmt.Sprintf("topup:%d", time.Now().UnixNano()),
		Subject: "user:u1", Kind: store.TxnTopup, AmountMicro: amount}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *sellRelay) sub() store.Subscription {
	r.t.Helper()
	ss, _ := store.SubscriptionsOf(r.st)
	list, err := ss.ListSubscriptions(context.Background(), store.SubscriptionFilter{UserID: "u1"})
	if err != nil || len(list) == 0 {
		r.t.Fatalf("subscriptions: %v %v", list, err)
	}
	return list[0]
}

// settled waits until no request draws on the subscription or the balance.
func (r *sellRelay) settled() store.Subscription {
	r.t.Helper()
	var s store.Subscription
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if s = r.sub(); s.Held == 0 && r.account().HeldMicro == 0 {
			break
		}
	}
	return s
}

func (r *sellRelay) userGroup() string {
	r.t.Helper()
	u, err := r.st.GetUser(context.Background(), "u1")
	if err != nil {
		r.t.Fatal(err)
	}
	return u.GroupID
}

// A plan paid from the balance: requests draw on the allowance (the one
// that crosses its end pays the rest from the balance), then are refused
// with the client's out-of-credit error; the user sits in the plan's
// group.
func TestSellKitPlanFromBalance(t *testing.T) {
	r := newSellRelay(t, "admin.subscriptions")
	ctx := context.Background()
	if err := r.st.PutGroup(ctx, &store.Group{ID: "pro", Name: "Pro", PriceMultiplier: 1}); err != nil {
		t.Fatal(err)
	}
	// Each chat costs 9 micro-units (see TestSellKitChargeAndRefund).
	id := r.plan(`{"name":"Pro","price_micro":1000000,"period":"month","allowance_unit":"money","allowance":15,"overage":"block","group_id":"pro"}`)
	if code, out := r.json("u1", "/api/plugins/admin.subscriptions/subscribe", `{"plan_id":"`+id+`","provider":"balance"}`); code != http.StatusConflict ||
		out["error"].(map[string]any)["code"] != "insufficient_balance" {
		t.Fatalf("subscribe without balance: %d %v", code, out)
	}
	r.topup(1_500_000)
	code, out := r.json("u1", "/api/plugins/admin.subscriptions/subscribe", `{"plan_id":"`+id+`","provider":"balance"}`)
	if code != http.StatusCreated || out["status"] != "active" {
		t.Fatalf("subscribe: %d %v", code, out)
	}
	if code, out := r.json("u1", "/api/plugins/admin.subscriptions/subscribe", `{"plan_id":"`+id+`","provider":"balance"}`); code != http.StatusConflict ||
		out["error"].(map[string]any)["code"] != "already_subscribed" {
		t.Fatalf("second subscribe: %d %v", code, out)
	}
	if a := r.account(); a.BalanceMicro != 500_000 || r.userGroup() != "pro" {
		t.Fatalf("after subscribing: %+v group %q", a, r.userGroup())
	}
	for i := range 2 {
		if code, body := r.chat("m"); code != 200 {
			t.Fatalf("chat %d: %d %s", i, code, body)
		}
		r.settled()
	}
	if s := r.sub(); s.Used != 15 || r.account().BalanceMicro != 500_000-3 {
		t.Fatalf("after two requests: used %d, balance %d", s.Used, r.account().BalanceMicro)
	}
	code, _, body := r.post("/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer " + r.key})
	if code != http.StatusTooManyRequests || !strings.Contains(body, "insufficient_quota") || !strings.Contains(body, "allowance for this period is used up") {
		t.Fatalf("past the allowance: %d %s", code, body)
	}
	code, _, body = r.post("/v1/messages", `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": r.key, "anthropic-version": "2023-06-01"})
	if code != http.StatusPaymentRequired || !strings.Contains(body, "billing_error") {
		t.Fatalf("anthropic past the allowance: %d %s", code, body)
	}
	// The console page: used and left in money, the draws in the ledger.
	code, me := r.json("u1", "/api/plugins/admin.subscriptions/me", "")
	s := me["subscription"].(map[string]any)
	if code != 200 || s["used"].(float64) != 15 || s["remaining"].(float64) != 0 || s["current"] != true || me["plan"] == nil {
		t.Fatalf("me: %d %v", code, me)
	}
	_, txns := r.json("u1", "/api/plugins/admin.subscriptions/me/transactions", "")
	if items := txns["items"].([]any); len(items) != 3 { // the grant and two draws
		t.Fatalf("allowance history: %v", txns)
	}
	// Cancelling ends it at the period end, back in the old group.
	if code, out := r.json("u1", "/api/plugins/admin.subscriptions/me/cancel", `{"cancel":true}`); code != 200 || out["cancel_at_period_end"] != true {
		t.Fatalf("cancel: %d %v", code, out)
	}
	ss, _ := store.SubscriptionsOf(r.st)
	if _, err := ss.RenewDue(ctx, time.Now().AddDate(0, 2, 0)); err != nil {
		t.Fatal(err)
	}
	if s := r.sub(); s.Status != store.SubEnded || r.userGroup() != "" || r.account().BalanceMicro != 500_000-3 {
		t.Fatalf("after the period: %+v group %q", s, r.userGroup())
	}
	// Without a plan the balance pays again.
	if code, body := r.chat("m"); code != 200 {
		t.Fatalf("chat after the plan: %d %s", code, body)
	}
	r.settled()
	if a := r.account(); a.BalanceMicro != 500_000-3-9 {
		t.Fatalf("balance after the plan: %+v", a)
	}
}

// Overage on: past the allowance the balance pays the rest, then all.
func TestSellKitPlanOverageFromBalance(t *testing.T) {
	r := newSellRelay(t, "admin.subscriptions")
	id := r.plan(`{"name":"Flex","price_micro":0,"period":"week","allowance_unit":"money","allowance":15,"overage":"balance"}`)
	r.topup(1_000)
	if code, out := r.json("u1", "/api/plugins/admin.subscriptions/subscribe", `{"plan_id":"`+id+`","provider":"balance"}`); code != http.StatusCreated {
		t.Fatalf("subscribe: %d %v", code, out)
	}
	for range 3 {
		if code, body := r.chat("m"); code != 200 {
			t.Fatalf("chat: %d %s", code, body)
		}
		r.settled()
	}
	// 9 + 6 from the allowance; 3 + 9 from the balance.
	if s, a := r.sub(), r.account(); s.Used != 15 || a.BalanceMicro != 1_000-12 {
		t.Fatalf("used %d, balance %d", s.Used, a.BalanceMicro)
	}
}

// A token plan counts tokens (7 per chat here, 9 micro-units); the tokens
// past the allowance are charged to the balance at the request's rate.
func TestSellKitTokenPlan(t *testing.T) {
	r := newSellRelay(t, "admin.subscriptions")
	id := r.plan(`{"name":"Tokens","price_micro":0,"period":"month","allowance_unit":"tokens","allowance":10,"overage":"block"}`)
	if code, out := r.json("u1", "/api/plugins/admin.subscriptions/subscribe", `{"plan_id":"`+id+`","provider":"balance"}`); code != http.StatusCreated {
		t.Fatalf("subscribe: %d %v", code, out)
	}
	for range 2 {
		if code, body := r.chat("m"); code != 200 {
			t.Fatalf("chat: %d %s", code, body)
		}
		r.settled()
	}
	// 7 + 3 from the allowance; 4 of 7 tokens of 9 is 5 from the balance.
	if s := r.sub(); s.Used != 10 || r.account().BalanceMicro != -5 {
		t.Fatalf("used %d, balance %d", s.Used, r.account().BalanceMicro)
	}
	if code, _ := r.chat("m"); code != http.StatusTooManyRequests {
		t.Fatalf("past the tokens: %d", code)
	}
}

// fakeStripeSubs answers Checkout Sessions and subscription updates as
// Stripe does (https://docs.stripe.com/api/checkout/sessions/create,
// https://docs.stripe.com/api/subscriptions/update); the relay's Stripe
// calls reach it for the test (stripe.UseAPI).
type fakeStripeSubs struct {
	mu       sync.Mutex
	sessions []url.Values
	updates  map[string]url.Values
	// invoices maps invoice ids to their subscription, payments
	// PaymentIntents to the invoice they paid
	// (https://docs.stripe.com/api/invoice-payment/list).
	invoices map[string]string
	payments map[string]string
	expired  []string // checkout sessions expired by a cancel
}

func newFakeStripeSubs(t *testing.T) *fakeStripeSubs {
	f := &fakeStripeSubs{updates: map[string]url.Values{}, invoices: map[string]string{}, payments: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sk_test_123" && r.Method == http.MethodGet {
			f.mu.Lock()
			defer f.mu.Unlock()
			q := r.URL.Query()
			switch {
			case r.URL.Path == "/v1/invoice_payments" && q.Get("payment[type]") == "payment_intent" && q.Get("status") == "paid":
				data := []any{}
				if inv := f.payments[q.Get("payment[payment_intent]")]; inv != "" {
					data = append(data, map[string]any{"object": "invoice_payment", "invoice": inv, "status": "paid"})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "has_more": false})
			case strings.HasPrefix(r.URL.Path, "/v1/invoices/") && f.invoices[strings.TrimPrefix(r.URL.Path, "/v1/invoices/")] != "":
				id := strings.TrimPrefix(r.URL.Path, "/v1/invoices/")
				_ = json.NewEncoder(w).Encode(map[string]any{"object": "invoice", "id": id, "parent": map[string]any{
					"type": "subscription_details", "subscription_details": map[string]any{"subscription": f.invoices[id]}}})
			default:
				http.NotFound(w, r)
			}
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk_test_123" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"Invalid API Key provided"}}`)
			return
		}
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/checkout/sessions/") && strings.HasSuffix(r.URL.Path, "/expire"):
			f.expired = append(f.expired, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/checkout/sessions/"), "/expire"))
			_, _ = io.WriteString(w, `{"object":"checkout.session","status":"expired"}`)
		case r.URL.Path == "/v1/checkout/sessions":
			f.sessions = append(f.sessions, r.PostForm)
			id := fmt.Sprintf("cs_test_%d", len(f.sessions))
			_, _ = fmt.Fprintf(w, `{"id":%q,"object":"checkout.session","mode":"subscription","url":"https://checkout.stripe.com/c/pay/%s","subscription":null}`, id, id)
		case strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
			sid := strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/")
			f.updates[sid] = r.PostForm
			_, _ = fmt.Fprintf(w, `{"id":%q,"object":"subscription","status":"active","cancel_at_period_end":%s}`, sid, r.PostForm.Get("cancel_at_period_end"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(stripe.UseAPI(srv.URL))
	return f
}

func (r *sellRelay) stripeEvent(body []byte, secret string) (int, string) {
	r.t.Helper()
	code, _, out := r.post("/api/plugins/admin.subscriptions/webhook/stripe", string(body),
		map[string]string{"Stripe-Signature": stripe.Sign(secret, body, time.Now())})
	return code, out
}

// invoicePaid is an invoice.paid event as Stripe sends it: since
// 2025-03-31.basil the subscription is under parent.subscription_details;
// before it, at the top level (https://docs.stripe.com/api/invoices/object,
// https://docs.stripe.com/billing/subscriptions/webhooks).
func invoicePaid(evt, invoice, subID string, cents int64, start, end time.Time, basil bool) []byte {
	return invoicePaidRef(evt, invoice, subID, "sub_123", cents, start, end, basil)
}

func invoicePaidRef(evt, invoice, subID, ref string, cents int64, start, end time.Time, basil bool) []byte {
	line := map[string]any{"object": "line_item", "id": "il_" + invoice, "amount": cents, "currency": "usd",
		"metadata": map[string]string{"subscription_id": subID}, "period": map[string]int64{"start": start.Unix(), "end": end.Unix()}}
	inv := map[string]any{"object": "invoice", "id": invoice, "amount_paid": cents, "currency": "usd", "status": "paid",
		"billing_reason": "subscription_cycle", "period_start": start.Unix(), "period_end": start.Unix()}
	version := "2025-03-31.basil"
	if basil {
		line["parent"] = map[string]any{"type": "subscription_item_details",
			"subscription_item_details": map[string]any{"subscription": ref, "subscription_item": "si_1"}}
		inv["parent"] = map[string]any{"type": "subscription_details",
			"subscription_details": map[string]any{"subscription": ref, "metadata": map[string]string{"subscription_id": subID}}}
	} else {
		version = "2024-06-20"
		line["type"] = "subscription"
		inv["subscription"] = ref
		inv["subscription_details"] = map[string]any{"metadata": map[string]string{"subscription_id": subID}}
	}
	inv["lines"] = map[string]any{"object": "list", "data": []any{line}}
	b, _ := json.Marshal(map[string]any{"id": evt, "object": "event", "api_version": version, "type": "invoice.paid",
		"data": map[string]any{"object": inv}})
	return b
}

func subscriptionEvent(evt, typ, status string, cancel bool, subID string) []byte {
	b, _ := json.Marshal(map[string]any{"id": evt, "object": "event", "type": typ, "data": map[string]any{"object": map[string]any{
		"object": "subscription", "id": "sub_123", "status": status, "cancel_at_period_end": cancel,
		"metadata": map[string]string{"subscription_id": subID}}}})
	return b
}

func TestSellKitStripeSubscription(t *testing.T) {
	fake := newFakeStripeSubs(t)
	r := newSellRelay(t, "admin.subscriptions")
	r.paySettings(map[string]any{"stripe_secret_key": "sk_test_123", "stripe_webhook_secret": "whsec_topups",
		"stripe_subscription_webhook_secret": subWhsec})
	id := r.plan(`{"name":"Pro","price_micro":5000000,"period":"month","allowance_unit":"money","allowance":15,"overage":"block"}`)
	_, cfg := r.json("u1", "/api/plugins/admin.subscriptions/config", "")
	if b, _ := json.Marshal(cfg["methods"]); string(b) != `[{"provider":"balance"},{"provider":"stripe"}]` {
		t.Fatalf("methods %s", b)
	}
	code, out := r.json("u1", "/api/plugins/admin.subscriptions/subscribe", `{"plan_id":"`+id+`","provider":"stripe"}`)
	if code != http.StatusCreated || out["status"] != "pending" || !strings.HasPrefix(out["url"].(string), "https://checkout.stripe.com/") {
		t.Fatalf("subscribe: %d %v", code, out)
	}
	subID := out["subscription_id"].(string)
	form := fake.sessions[0]
	if form.Get("mode") != "subscription" || form.Get("line_items[0][price_data][recurring][interval]") != "month" ||
		form.Get("line_items[0][price_data][unit_amount]") != "500" || form.Get("subscription_data[metadata][subscription_id]") != subID ||
		form.Get("client_reference_id") != subID || form.Get("success_url") != "https://relay.example.com/billing/plan?subscription="+subID {
		t.Fatalf("session form %v", form)
	}
	if s := r.sub(); s.Status != store.SubPending {
		t.Fatalf("before payment: %+v", s)
	}
	start := time.Now().Add(-time.Minute).Truncate(time.Second)
	end := start.AddDate(0, 1, 0)
	paid := invoicePaid("evt_1", "in_1", subID, 500, start, end, true)
	// A forged signature, the top-up endpoint's secret, a stale timestamp.
	for name, sig := range map[string]string{"garbage": "t=1,v1=00", "other endpoint": stripe.Sign("whsec_topups", paid, time.Now()),
		"replayed": stripe.Sign(subWhsec, paid, time.Now().Add(-10*time.Minute))} {
		code, _, out := r.post("/api/plugins/admin.subscriptions/webhook/stripe", string(paid), map[string]string{"Stripe-Signature": sig})
		if code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, code, out)
		}
	}
	if s := r.sub(); s.Status != store.SubPending {
		t.Fatalf("activated by a forged event: %+v", s)
	}
	// The invoice starts the period; a duplicate delivery and the same
	// invoice under another event id start nothing more.
	for _, b := range [][]byte{paid, paid, invoicePaid("evt_2", "in_1", subID, 500, start, end, true)} {
		if code, out := r.stripeEvent(b, subWhsec); code != 200 {
			t.Fatalf("invoice.paid: %d %s", code, out)
		}
	}
	s := r.sub()
	if s.Status != store.SubActive || !s.PeriodEnd.Equal(end) || s.ProviderRef != "sub_123" || s.Renewal != store.RenewProvider {
		t.Fatalf("after invoice.paid: %+v", s)
	}
	txns, _, _ := r.l.Transactions(context.Background(), store.TxnQuery{Subject: "user:u1", Kind: store.TxnSubscription})
	if len(txns) != 1 || txns[0].AmountMicro != 0 || txns[0].ID != "subscription:stripe:in_1" {
		t.Fatalf("payment rows %+v", txns)
	}
	if code, body := r.chat("m"); code != 200 {
		t.Fatalf("chat: %d %s", code, body)
	}
	if s := r.settled(); s.Used != 9 || r.account().BalanceMicro != 0 {
		t.Fatalf("draw %+v", s)
	}
	// Cancel at period end goes to Stripe; its update event agrees.
	if code, out := r.json("u1", "/api/plugins/admin.subscriptions/me/cancel", `{"cancel":true}`); code != 200 || out["cancel_at_period_end"] != true {
		t.Fatalf("cancel: %d %v", code, out)
	}
	if fake.updates["sub_123"].Get("cancel_at_period_end") != "true" {
		t.Fatalf("stripe update %v", fake.updates)
	}
	if code, _ := r.stripeEvent(subscriptionEvent("evt_3", "customer.subscription.updated", "active", false, subID), subWhsec); code != 200 || r.sub().CancelAtPeriodEnd {
		t.Fatalf("resumed at Stripe: %+v", r.sub())
	}
	// Renewal (an older API version's payload) starts the next period with
	// a fresh allowance; a price that differs starts nothing.
	next := end.AddDate(0, 1, 0)
	if code, _ := r.stripeEvent(invoicePaid("evt_4", "in_bad", subID, 100, end, next, false), subWhsec); code != 200 || !r.sub().PeriodEnd.Equal(end) {
		t.Fatalf("wrong amount started a period: %+v", r.sub())
	}
	if code, out := r.stripeEvent(invoicePaid("evt_5", "in_2", subID, 500, end, next, false), subWhsec); code != 200 {
		t.Fatalf("renewal: %d %s", code, out)
	}
	if s := r.sub(); !s.PeriodEnd.Equal(next) || s.Used != 0 || s.Status != store.SubActive {
		t.Fatalf("after renewal: %+v", s)
	}
	if code, _ := r.stripeEvent(subscriptionEvent("evt_6", "customer.subscription.deleted", "canceled", false, subID), subWhsec); code != 200 || r.sub().Status != store.SubEnded {
		t.Fatalf("deleted: %+v", r.sub())
	}
}

// A plan bought through epay (CNY): the signed notify starts the period
// once; forged, mismatched and replayed notifies change nothing.
func TestSellKitEpaySubscription(t *testing.T) {
	r := newSellRelay(t, "admin.subscriptions")
	ctx := context.Background()
	if err := r.st.KV("core.site").Put(ctx, "settings", []byte(`{"site.currency":"CNY","site.public_base_url":"https://relay.example.com"}`), 0); err != nil {
		t.Fatal(err)
	}
	r.paySettings(map[string]any{"epay_url": "https://pay.example.com", "epay_pid": "1001", "epay_key": "k3y"})
	id := r.plan(`{"name":"月卡","price_micro":29900000,"period":"month","allowance_unit":"money","allowance":30000000,"overage":"block"}`)
	_, cfg := r.json("u1", "/api/plugins/admin.subscriptions/config", "")
	if b, _ := json.Marshal(cfg["methods"]); !strings.Contains(string(b), `{"method":"wxpay","provider":"epay"}`) {
		t.Fatalf("methods %s", b)
	}
	code, out := r.json("u1", "/api/plugins/admin.subscriptions/subscribe", `{"plan_id":"`+id+`","provider":"epay","method":"alipay"}`)
	if code != http.StatusCreated {
		t.Fatalf("subscribe: %d %v", code, out)
	}
	u, _ := url.Parse(out["url"].(string))
	q := u.Query()
	if u.Host != "pay.example.com" || q.Get("money") != "29.90" || q.Get("type") != "alipay" ||
		q.Get("notify_url") != "https://relay.example.com/api/plugins/admin.subscriptions/webhook/epay" {
		t.Fatalf("epay url %s", u)
	}
	orderID := q.Get("out_trade_no")
	notify := func(money, tradeNo string, mutate func(url.Values)) (int, string) {
		v := url.Values{"pid": {"1001"}, "trade_no": {tradeNo}, "out_trade_no": {orderID}, "type": {"alipay"}, "name": {"月卡"},
			"money": {money}, "trade_status": {"TRADE_SUCCESS"}}
		v.Set("sign", epay.Sign(v, "k3y"))
		v.Set("sign_type", "MD5")
		if mutate != nil {
			mutate(v)
		}
		resp, err := http.Get(r.base + "/api/plugins/admin.subscriptions/webhook/epay?" + v.Encode())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := notify("29.90", "T1", func(v url.Values) { v.Set("sign", strings.Repeat("0", 32)) }); code != 400 || body != "fail" {
		t.Fatalf("bad sign: %d %q", code, body)
	}
	// Not paid yet and a wrong amount start nothing, and do not hide the
	// success that follows under the same trade number.
	if code, body := notify("29.90", "T1", func(v url.Values) {
		v.Set("trade_status", "WAIT_BUYER_PAY")
		v.Set("sign", epay.Sign(v, "k3y"))
	}); code != 200 || body != "success" || r.sub().Status != store.SubPending {
		t.Fatalf("unpaid: %d %q %+v", code, body, r.sub())
	}
	if code, body := notify("0.01", "T1", nil); code != 200 || body != "success" || r.sub().Status != store.SubPending {
		t.Fatalf("amount mismatch: %d %q %+v", code, body, r.sub())
	}
	for range 2 {
		if code, body := notify("29.9", "T1", nil); code != 200 || body != "success" {
			t.Fatalf("notify: %d %q", code, body)
		}
	}
	if code, body := notify("29.90", "T2", nil); code != 200 || body != "success" {
		t.Fatalf("replay: %d %q", code, body)
	}
	s := r.sub()
	if s.Status != store.SubActive || s.Renewal != store.RenewBalance || s.PeriodEnd.Sub(s.PeriodStart) < 27*24*time.Hour {
		t.Fatalf("after notify: %+v", s)
	}
	txns, _, _ := r.l.Transactions(ctx, store.TxnQuery{Subject: "user:u1", Kind: store.TxnSubscription})
	if len(txns) != 1 {
		t.Fatalf("payment rows %+v", txns)
	}
}

func chargeEvent(evt, typ string, object map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"id": evt, "object": "event", "type": typ, "data": map[string]any{"object": object}})
	return b
}

// A late invoice for a plan the user left is recorded and flagged for a
// refund (and stopped at Stripe); its refund clears the flag. A dispute
// on the current plan's invoice ends it now.
func TestSellKitStripeLatePaymentAndReversals(t *testing.T) {
	fake := newFakeStripeSubs(t)
	r := newSellRelay(t, "admin.subscriptions")
	if err := r.st.PutGroup(context.Background(), &store.Group{ID: "pro", Name: "Pro", PriceMultiplier: 1}); err != nil {
		t.Fatal(err)
	}
	r.paySettings(map[string]any{"stripe_secret_key": "sk_test_123", "stripe_webhook_secret": "whsec_topups",
		"stripe_subscription_webhook_secret": subWhsec})
	id := r.plan(`{"name":"Pro","price_micro":5000000,"period":"month","allowance_unit":"money","allowance":15,"overage":"block","group_id":"pro"}`)
	subscribe := func() string {
		code, out := r.json("u1", "/api/plugins/admin.subscriptions/subscribe", `{"plan_id":"`+id+`","provider":"stripe"}`)
		if code != http.StatusCreated {
			t.Fatalf("subscribe: %d %v", code, out)
		}
		return out["subscription_id"].(string)
	}
	ss, _ := store.SubscriptionsOf(r.st)
	get := func(id string) *store.Subscription {
		s, err := ss.GetSubscription(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	start := time.Now().Add(-time.Minute).Truncate(time.Second)
	end := start.AddDate(0, 1, 0)
	a := subscribe()
	if code, out := r.stripeEvent(invoicePaidRef("evt_a1", "in_a1", a, "sub_A", 500, start, end, true), subWhsec); code != 200 {
		t.Fatalf("invoice A: %d %s", code, out)
	}
	// A ends (cancelled at the period end); the user moves to B.
	if _, err := ss.EndSubscription(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	b := subscribe()
	if code, _ := r.stripeEvent(invoicePaidRef("evt_b1", "in_b1", b, "sub_B", 500, start, end, true), subWhsec); code != 200 || get(b).Status != store.SubActive {
		t.Fatalf("invoice B: %+v", get(b))
	}
	// A's late invoice: recorded, not started, flagged, stopped at Stripe.
	if code, _ := r.stripeEvent(invoicePaidRef("evt_a3", "in_a2", a, "sub_A", 500, end, end.AddDate(0, 1, 0), true), subWhsec); code != 200 {
		t.Fatal("late invoice")
	}
	if s := get(a); s.Status != store.SubEnded || !s.RefundDue || get(b).Status != store.SubActive {
		t.Fatalf("late invoice: %+v", s)
	}
	if fake.updates["sub_A"].Get("cancel_at_period_end") != "true" {
		t.Fatalf("not stopped at Stripe: %v", fake.updates)
	}
	_, list := r.json("admin", "/api/plugins/admin.subscriptions/subscriptions", "")
	if !strings.Contains(fmt.Sprint(list), "refund_due:true") {
		t.Fatalf("admin list: %v", list)
	}
	fake.invoices["in_a2"], fake.payments["pi_a2"] = "sub_A", "in_a2"
	fake.invoices["in_b1"], fake.payments["pi_b1"] = "sub_B", "in_b1"
	// A partial refund changes nothing; the full refund clears the flag.
	if code, _ := r.stripeEvent(chargeEvent("evt_r0", "charge.refunded", map[string]any{"object": "charge", "id": "ch_1",
		"refunded": false, "payment_intent": "pi_b1"}), subWhsec); code != 200 || get(b).Status != store.SubActive {
		t.Fatalf("partial refund ended B: %+v", get(b))
	}
	if code, out := r.stripeEvent(chargeEvent("evt_r1", "charge.refunded", map[string]any{"object": "charge", "id": "ch_2",
		"refunded": true, "payment_intent": "pi_a2"}), subWhsec); code != 200 || get(a).RefundDue {
		t.Fatalf("refund of the flagged payment: %d %s %+v", code, out, get(a))
	}
	// A dispute on B's invoice ends B now, out of the plan group.
	if r.userGroup() != "pro" {
		t.Fatalf("group %q", r.userGroup())
	}
	if code, out := r.stripeEvent(chargeEvent("evt_d1", "charge.dispute.created", map[string]any{"object": "dispute", "id": "dp_1",
		"charge": "ch_3", "payment_intent": "pi_b1"}), subWhsec); code != 200 || get(b).Status != store.SubEnded {
		t.Fatalf("dispute: %d %s %+v", code, out, get(b))
	}
	if r.userGroup() != "" || fake.updates["sub_B"].Get("cancel_at_period_end") != "true" {
		t.Fatalf("after the dispute: group %q, updates %v", r.userGroup(), fake.updates)
	}
}

// An admin ends a subscription now (a refund made by hand) and clears a
// refund flag; plans with a group are refused while groups pay.
func TestSellKitAdminEndNowAndPayerGroup(t *testing.T) {
	r := newSellRelay(t, "admin.subscriptions")
	ctx := context.Background()
	if err := r.st.PutGroup(ctx, &store.Group{ID: "pro", Name: "Pro", PriceMultiplier: 1}); err != nil {
		t.Fatal(err)
	}
	id := r.plan(`{"name":"Pro","price_micro":1000000,"period":"month","allowance_unit":"money","allowance":15,"overage":"block","group_id":"pro"}`)
	r.topup(1_000_000)
	if code, out := r.json("u1", "/api/plugins/admin.subscriptions/subscribe", `{"plan_id":"`+id+`","provider":"balance"}`); code != http.StatusCreated {
		t.Fatalf("subscribe: %d %v", code, out)
	}
	subID := r.sub().ID
	if code, out := r.json("u1", "/api/plugins/admin.subscriptions/subscriptions/"+subID+"/end", `{}`); code != http.StatusForbidden {
		t.Fatalf("a user ended it: %d %v", code, out)
	}
	code, out := r.adminCall("POST", "/api/plugins/admin.subscriptions/subscriptions/"+subID+"/end", `{}`)
	if code != 200 || out["status"] != store.SubEnded || r.userGroup() != "" {
		t.Fatalf("end now: %d %v group %q", code, out, r.userGroup())
	}
	if code, _ := r.chat("m"); code != http.StatusTooManyRequests && code != http.StatusOK {
		t.Fatalf("chat after the end: %d", code)
	}
	ss, _ := store.SubscriptionsOf(r.st)
	if err := ss.SetRefundDue(ctx, subID, true); err != nil {
		t.Fatal(err)
	}
	if code, out := r.adminCall("POST", "/api/plugins/admin.subscriptions/subscriptions/"+subID+"/refunded", `{"refunded":true}`); code != 200 || out["refund_due"] == true {
		t.Fatalf("mark refunded: %d %v", code, out)
	}
	audit, err := r.st.ListAudit(ctx, store.AuditQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var audited bool
	for _, a := range audit {
		audited = audited || strings.Contains(string(a.Diff), "POST /subscriptions/"+subID+"/end")
	}
	if !audited {
		t.Fatalf("end now not audited: %v", audit)
	}

	r.setPayer("group")
	if code, out := r.json("admin", "/api/plugins/admin.subscriptions/plans", `{"name":"G","price_micro":1,"period":"month","allowance_unit":"money","allowance":1,"overage":"block","group_id":"pro"}`); code != http.StatusConflict {
		t.Fatalf("plan with a group while groups pay: %d %v", code, out)
	}
	if code, out := r.json("u1", "/api/plugins/admin.subscriptions/subscribe", `{"plan_id":"`+id+`","provider":"balance"}`); code != http.StatusConflict ||
		out["error"].(map[string]any)["code"] != "payer_group" {
		t.Fatalf("subscribe while groups pay: %d %v", code, out)
	}
}

// adminCall calls the console as the admin with a freshly re-authenticated
// session (password "correct-horse-1").
func (r *sellRelay) adminCall(method, path, body string) (int, map[string]any) {
	r.t.Helper()
	tok, err := r.st.CreateSession(context.Background(), "admin", time.Hour)
	if err != nil {
		r.t.Fatal(err)
	}
	do := func(method, path, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, r.base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "pr_session", Value: tok})
		req.AddCookie(&http.Cookie{Name: "pr_csrf", Value: "csrf-1"})
		req.Header.Set("X-CSRF-Token", "csrf-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			r.t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	if code, out := do("POST", "/api/v1/me/reauth", `{"method":"password","password":"correct-horse-1"}`); code != 200 {
		r.t.Fatalf("reauth: %d %v", code, out)
	}
	return do(method, path, body)
}

// A token plan covers tokens only: 20 images on a 100-token plan are paid
// from the balance, with either overage policy, and refused there once
// the balance is used up.
func TestSellKitTokenPlanImages(t *testing.T) {
	for _, ov := range []string{"block", "balance"} {
		t.Run(ov, func(t *testing.T) {
			r := newSellRelay(t, "admin.subscriptions")
			ctx := context.Background()
			img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"aGk="}]}`)
			}))
			t.Cleanup(img.Close)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(r.st.PutUpstream(ctx, &store.Upstream{ID: "img", Provider: "provider.openai-compatible", Enabled: true,
				Config: map[string]any{"manifest": "openai-compatible", "base_url": img.URL + "/v1", configAllowPrivate: true}}))
			must(r.st.PutCredential(ctx, &store.Credential{ID: "img-c", UpstreamID: "img", Kind: store.CredShared, Secret: "sk-upstream", Status: store.CredActive}))
			must(r.st.PutModel(ctx, &store.Model{Name: "dall-e-3", Enabled: true}))
			must(r.st.ReplaceRoutes(ctx, "dall-e-3", []store.Route{{Model: "dall-e-3", UpstreamID: "img", UpstreamModel: "dall-e-3", Weight: 1}}))
			must(r.st.PutPrice(ctx, &store.PriceRule{Model: "dall-e-3", Currency: "USD", Units: map[string]int64{plugin.UnitImage: 40_000}}))
			must(r.a.Reload(ctx))
			id := r.plan(`{"name":"Tokens","price_micro":0,"period":"month","allowance_unit":"tokens","allowance":100,"overage":"` + ov + `"}`)
			if code, out := r.json("u1", "/api/plugins/admin.subscriptions/subscribe", `{"plan_id":"`+id+`","provider":"balance"}`); code != http.StatusCreated {
				t.Fatalf("subscribe: %d %v", code, out)
			}
			r.topup(800_000)
			image := func() int {
				code, _, body := r.post("/v1/images/generations", `{"model":"dall-e-3","prompt":"apple"}`, map[string]string{"Authorization": "Bearer " + r.key})
				if code != 200 && code != http.StatusTooManyRequests {
					t.Fatalf("image: %d %s", code, body)
				}
				r.settled()
				return code
			}
			for i := range 20 {
				if code := image(); code != 200 {
					t.Fatalf("image %d: %d", i, code)
				}
			}
			if s := r.sub(); s.Used != 0 || r.account().BalanceMicro != 0 {
				t.Fatalf("plan used %d, balance %d (800000 before)", s.Used, r.account().BalanceMicro)
			}
			if code := image(); code != http.StatusTooManyRequests {
				t.Fatalf("image past the balance: %d", code)
			}
		})
	}
}

// Deleting a user ends the user's subscription and cancels it at Stripe,
// so the provider bills no further period; ending and marking refunded
// need a recent re-authentication.
func TestSellKitDeleteUserEndsSubscription(t *testing.T) {
	fake := newFakeStripeSubs(t)
	r := newSellRelay(t, "admin.subscriptions")
	r.paySettings(map[string]any{"stripe_secret_key": "sk_test_123", "stripe_webhook_secret": "whsec_topups",
		"stripe_subscription_webhook_secret": subWhsec})
	id := r.plan(`{"name":"Pro","price_micro":5000000,"period":"month","allowance_unit":"money","allowance":15,"overage":"block"}`)
	code, out := r.json("u1", "/api/plugins/admin.subscriptions/subscribe", `{"plan_id":"`+id+`","provider":"stripe"}`)
	if code != http.StatusCreated {
		t.Fatalf("subscribe: %d %v", code, out)
	}
	subID := out["subscription_id"].(string)
	start := time.Now().Add(-time.Minute).Truncate(time.Second)
	if code, out := r.stripeEvent(invoicePaid("evt_1", "in_1", subID, 500, start, start.AddDate(0, 1, 0), true), subWhsec); code != 200 {
		t.Fatalf("invoice.paid: %d %s", code, out)
	}
	for _, path := range []string{"/end", "/refunded"} {
		if code, out := r.json("admin", "/api/plugins/admin.subscriptions/subscriptions/"+subID+path, `{}`); code != http.StatusForbidden ||
			out["error"].(map[string]any)["code"] != "reauth_required" {
			t.Fatalf("%s without reauth: %d %v", path, code, out)
		}
	}
	if code, out := r.adminCall("DELETE", "/api/v1/users/u1", ""); code != http.StatusNoContent {
		t.Fatalf("delete user: %d %v", code, out)
	}
	ss, _ := store.SubscriptionsOf(r.st)
	s, err := ss.GetSubscription(context.Background(), subID)
	if err != nil || s.Status != store.SubEnded {
		t.Fatalf("after deleting the user: %+v %v", s, err)
	}
	if fake.updates["sub_123"].Get("cancel_at_period_end") != "true" {
		t.Fatalf("not cancelled at Stripe: %v", fake.updates)
	}
}

// meter.balance's payer cannot become "group" while a plan has a group.
func TestSellKitPayerGroupRefusedWithPlanGroups(t *testing.T) {
	r := newSellRelay(t, "admin.subscriptions")
	if err := r.st.PutGroup(context.Background(), &store.Group{ID: "pro", Name: "Pro", PriceMultiplier: 1}); err != nil {
		t.Fatal(err)
	}
	r.plan(`{"name":"Pro","price_micro":1000000,"period":"month","allowance_unit":"money","allowance":15,"overage":"block","group_id":"pro"}`)
	code, out := r.adminCall("PATCH", "/api/v1/features/billing", `{"settings":{"meter.balance":{"payer":"group"}}}`)
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(out), "Pro") {
		t.Fatalf("payer=group with a plan group: %d %v", code, out)
	}
	if code, out := r.adminCall("PATCH", "/api/v1/features/billing", `{"settings":{"meter.balance":{"payer":"key"}}}`); code != 200 {
		t.Fatalf("payer=key: %d %v", code, out)
	}
}

// A checkout left pending has no Stripe subscription to cancel yet: the
// user's deletion is refused unless forced, and an invoice paid for it
// afterwards is recorded and flagged for a refund instead of failing.
func TestSellKitDeleteUserPendingCheckout(t *testing.T) {
	fake := newFakeStripeSubs(t)
	r := newSellRelay(t, "admin.subscriptions")
	r.paySettings(map[string]any{"stripe_secret_key": "sk_test_123", "stripe_webhook_secret": "whsec_topups",
		"stripe_subscription_webhook_secret": subWhsec})
	id := r.plan(`{"name":"Pro","price_micro":5000000,"period":"month","allowance_unit":"money","allowance":15,"overage":"block"}`)
	code, out := r.json("u1", "/api/plugins/admin.subscriptions/subscribe", `{"plan_id":"`+id+`","provider":"stripe"}`)
	if code != http.StatusCreated {
		t.Fatalf("subscribe: %d %v", code, out)
	}
	subID := out["subscription_id"].(string)
	if code, out := r.adminCall("DELETE", "/api/v1/users/u1", ""); code != http.StatusConflict || !strings.Contains(fmt.Sprint(out), "force") || !strings.Contains(fmt.Sprint(out), "user_delete_blocked") {
		t.Fatalf("delete with a pending checkout: %d %v", code, out)
	}
	if code, out := r.adminCall("DELETE", "/api/v1/users/u1?force=true", ""); code != http.StatusNoContent {
		t.Fatalf("forced delete: %d %v", code, out)
	}
	start := time.Now().Add(-time.Minute).Truncate(time.Second)
	for range 2 {
		if code, out := r.stripeEvent(invoicePaid("evt_1", "in_1", subID, 500, start, start.AddDate(0, 1, 0), true), subWhsec); code != 200 {
			t.Fatalf("invoice.paid after the user was deleted: %d %s", code, out)
		}
	}
	ss, _ := store.SubscriptionsOf(r.st)
	s, err := ss.GetSubscription(context.Background(), subID)
	if err != nil || s.Status != store.SubEnded || !s.RefundDue {
		t.Fatalf("after the late payment: %+v %v", s, err)
	}
	if fake.updates["sub_123"].Get("cancel_at_period_end") != "true" {
		t.Fatalf("not cancelled at Stripe: %v", fake.updates)
	}
}

// `plugrelay apply` (and the new-api import, through App.Apply) runs the
// plugins' settings checks: admin.subscriptions refuses meter.balance
// payer=group while a live subscription or a plan has a group.
func TestSellKitApplyPayerGroup(t *testing.T) {
	r := newSellRelay(t, "admin.subscriptions")
	ctx := context.Background()
	doc := []byte("settings:\n  - {scope: global, scope_id: '', plugin: meter.balance, key: payer, value: group}\n")
	ss, _ := store.SubscriptionsOf(r.st)
	sub := &store.Subscription{UserID: "u1", Payer: "user:u1", PlanID: "p1", PlanName: "Pro", Period: "month", Unit: store.UnitMoney,
		Allowance: 1, Overage: store.OverageBlock, GroupID: "pro", Renewal: store.RenewBalance, Provider: "balance"}
	if err := ss.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.Apply(ctx, doc, true); !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), sub.ID) {
		t.Fatalf("live subscription with a group: %v", err)
	}
	if _, err := ApplyDocument(ctx, r.a.cfg, doc, false); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("plugrelay apply with a live subscription with a group: %v", err)
	}
	if _, err := ss.EndSubscription(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	kv := r.st.KV("admin.subscriptions")
	if err := kv.Put(ctx, "plans", []byte(`[{"id":"p1","name":"Pro","group_id":"pro"}]`), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.Apply(ctx, doc, false); !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "plan Pro") {
		t.Fatalf("plan with a group: %v", err)
	}
	if rows, _ := r.st.ListSettings(ctx, store.SettingFilter{PluginID: "meter.balance"}); len(rows) != 0 {
		t.Fatalf("refused apply was written: %v", rows)
	}
	if err := kv.Put(ctx, "plans", []byte(`[{"id":"p1","name":"Pro"}]`), 0); err != nil {
		t.Fatal(err)
	}
	res, err := ApplyDocument(ctx, r.a.cfg, doc, false)
	if err != nil || res.Created != 1 || len(res.Changes) != 1 {
		t.Fatalf("applied %+v %v", res, err)
	}
}

// Settings stored while admin.subscriptions was off must pass its check
// when it is turned back on: turning billing off, storing payer=group and
// turning billing on again is refused, on the console and through apply.
func TestSellKitEnableChecksStoredSettings(t *testing.T) {
	r := newSellRelay(t, "admin.subscriptions")
	ctx := context.Background()
	if err := r.st.PutGroup(ctx, &store.Group{ID: "pro", Name: "Pro", PriceMultiplier: 1}); err != nil {
		t.Fatal(err)
	}
	r.plan(`{"name":"Pro","price_micro":1000000,"period":"month","allowance_unit":"money","allowance":15,"overage":"block","group_id":"pro"}`)
	if code, out := r.adminCall("PATCH", "/api/v1/features/billing", `{"enabled":false}`); code != 200 {
		t.Fatalf("disable: %d %v", code, out)
	}
	if err := r.a.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if code, out := r.adminCall("PATCH", "/api/v1/features/billing", `{"settings":{"meter.balance":{"payer":"group"}}}`); code != 200 {
		t.Fatalf("payer=group with admin.subscriptions off: %d %v", code, out)
	}
	code, out := r.adminCall("PATCH", "/api/v1/features/billing", `{"enabled":true}`)
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(out), "Pro") {
		t.Fatalf("enable over payer=group with a plan group: %d %v", code, out)
	}
	doc := []byte("features:\n  - {id: billing, enabled: true}\n")
	if _, err := r.a.Apply(ctx, doc, true); !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "Pro") {
		t.Fatalf("apply turning billing on: %v", err)
	}
	// Turning it on and payer back to user in one document passes.
	doc = append(doc, "settings:\n  - {scope: global, scope_id: '', plugin: meter.balance, key: payer, value: user}\n"...)
	if _, err := r.a.Apply(ctx, doc, true); err != nil {
		t.Fatalf("apply turning billing on with payer=user: %v", err)
	}
}
