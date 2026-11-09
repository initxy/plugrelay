package stripe_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/admin/payment"
	"github.com/initxy/plugrelay/plugins/admin/payment/stripe"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
	"github.com/initxy/plugrelay/plugins/meter/balance"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger/ledgertest"
)

const whsec = "whsec_test_secret"

// fakeStripe answers POST /v1/checkout/sessions as Stripe does and keeps
// the sessions it created.
type fakeStripe struct {
	mu       sync.Mutex
	sessions map[string]url.Values
}

func newFakeStripe(t *testing.T) (*fakeStripe, *httptest.Server) {
	f := &fakeStripe{sessions: map[string]url.Values{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/checkout/sessions" || r.Header.Get("Authorization") != "Bearer sk_test_123" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"Invalid API Key provided"}}`)
			return
		}
		_ = r.ParseForm()
		f.mu.Lock()
		id := fmt.Sprintf("cs_test_%d", len(f.sessions)+1)
		f.sessions[id] = r.PostForm
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"id":%q,"object":"checkout.session","url":"https://checkout.stripe.com/c/pay/%s"}`, id, id)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

type env struct {
	p      *payment.Plugin
	l      *ledgertest.Memory
	routes map[string]http.HandlerFunc
	now    time.Time
}

func setup(t *testing.T, api string) *env {
	l := ledgertest.New()
	kv := testkit.NewState(time.Now)
	p := payment.New(l, kv)
	h := &testkit.Host{Store: kv, Set: testkit.Settings{"stripe_secret_key": "sk_test_123", "stripe_webhook_secret": whsec,
		"min_topup_micro": 1_000_000, "max_topup_micro": 500_000_000}, Public: "https://relay.example.com"}
	t.Cleanup(stripe.UseAPI(api))
	if err := p.Init(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	e := &env{p: p, l: l, routes: map[string]http.HandlerFunc{}, now: time.Now()}
	for _, rt := range p.Routes() {
		e.routes[rt.Method+" "+rt.Path] = rt.Handler
	}
	return e
}

func (e *env) do(route, path, body string, hdr map[string]string) (int, map[string]any) {
	req := httptest.NewRequest(strings.Fields(route)[0], "/api/plugins/admin.payment"+path, strings.NewReader(body))
	req.Header.Set("X-Plugrelay-User", "u1")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.routes[route](rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func event(id, typ, order string, cents int64, status string) []byte {
	b, _ := json.Marshal(map[string]any{"id": id, "object": "event", "type": typ, "data": map[string]any{"object": map[string]any{
		"object": "checkout.session", "id": "cs_test_1", "client_reference_id": order, "metadata": map[string]string{"order_id": order},
		"payment_status": status, "amount_total": cents, "currency": "usd"}}})
	return b
}

func (e *env) webhook(body []byte, sig string) (int, map[string]any) {
	return e.do("POST /webhook/{provider}", "/webhook/stripe", string(body), map[string]string{"Stripe-Signature": sig})
}

func TestCheckoutAndWebhook(t *testing.T) {
	fake, srv := newFakeStripe(t)
	e := setup(t, srv.URL)

	code, out := e.do("POST /checkout", "/checkout", `{"amount_micro":20000000}`, nil)
	if code != http.StatusCreated || !strings.HasPrefix(out["url"].(string), "https://checkout.stripe.com/") {
		t.Fatalf("checkout: %d %v", code, out)
	}
	order := out["order_id"].(string)
	form := fake.sessions["cs_test_1"]
	if form.Get("line_items[0][price_data][unit_amount]") != "2000" || form.Get("line_items[0][price_data][currency]") != "usd" ||
		form.Get("client_reference_id") != order || form.Get("mode") != "payment" ||
		!strings.HasPrefix(form.Get("success_url"), "https://relay.example.com/billing?order=") {
		t.Fatalf("session form: %v", form)
	}

	body := event("evt_1", "checkout.session.completed", order, 2000, "paid")
	// Bad signature, wrong secret, stale timestamp (replay): rejected, nothing credited.
	for name, sig := range map[string]string{
		"garbage":      "t=1,v1=00",
		"wrong secret": stripe.Sign("whsec_other", body, e.now),
		"replayed":     stripe.Sign(whsec, body, e.now.Add(-10*time.Minute)),
		"tampered":     stripe.Sign(whsec, event("evt_1", "checkout.session.completed", order, 999999, "paid"), e.now),
	} {
		if code, _ := e.webhook(body, sig); code != http.StatusBadRequest {
			t.Errorf("%s signature: %d", name, code)
		}
	}
	if a, _ := e.l.Account(t.Context(), "user:u1"); a.BalanceMicro != 0 {
		t.Fatalf("credited by a bad callback: %+v", a)
	}

	if code, out := e.webhook(body, stripe.Sign(whsec, body, e.now)); code != 200 || out["received"] != true {
		t.Fatalf("webhook: %d %v", code, out)
	}
	// The same event again, and a second event for the same payment.
	if code, out := e.webhook(body, stripe.Sign(whsec, body, e.now)); code != 200 || out["duplicate"] != true {
		t.Fatalf("duplicate event: %d %v", code, out)
	}
	other := event("evt_2", "checkout.session.async_payment_succeeded", order, 2000, "paid")
	if code, _ := e.webhook(other, stripe.Sign(whsec, other, e.now)); code != 200 {
		t.Fatalf("second event: %d", code)
	}
	if a, _ := e.l.Account(t.Context(), "user:u1"); a.BalanceMicro != 20_000_000 {
		t.Fatalf("balance after payment: %+v", a)
	}
	code, out = e.do("GET /orders/{id}", "/orders/"+order, "", nil)
	if code != 200 || out["status"] != "paid" {
		t.Fatalf("order: %d %v", code, out)
	}
}

func TestWebhookIgnoresUnpaidAndUnknown(t *testing.T) {
	_, srv := newFakeStripe(t)
	e := setup(t, srv.URL)
	_, out := e.do("POST /checkout", "/checkout", `{"amount_micro":5000000}`, nil)
	order := out["order_id"].(string)
	for _, b := range [][]byte{
		event("evt_a", "checkout.session.completed", order, 500, "unpaid"),
		event("evt_b", "checkout.session.completed", "ord_nope", 500, "paid"),
		event("evt_c", "checkout.session.completed", order, 400, "paid"), // amount differs
		event("evt_d", "customer.created", order, 500, "paid"),
	} {
		if code, _ := e.webhook(b, stripe.Sign(whsec, b, e.now)); code != 200 {
			t.Fatalf("callback answered %d", code)
		}
	}
	if a, _ := e.l.Account(t.Context(), "user:u1"); a.BalanceMicro != 0 {
		t.Fatalf("credited: %+v", a)
	}
}

func TestCheckoutLimitsAndProviderErrors(t *testing.T) {
	_, srv := newFakeStripe(t)
	e := setup(t, srv.URL)
	for _, amt := range []string{"10", "999999999999", "1000001"} { // below min, above max, not whole cents
		if code, _ := e.do("POST /checkout", "/checkout", `{"amount_micro":`+amt+`}`, nil); code == http.StatusCreated {
			t.Errorf("amount %s accepted", amt)
		}
	}
	bad := setup(t, srv.URL+"/nowhere")
	if code, _ := bad.do("POST /checkout", "/checkout", `{"amount_micro":5000000}`, nil); code != http.StatusBadGateway {
		t.Fatalf("provider error: %d", code)
	}
}

// Each currency's exponent, both ways: the Checkout amount Stripe gets and
// the micro-units its webhook credits (https://docs.stripe.com/currencies).
func TestCurrencyExponents(t *testing.T) {
	for _, c := range []struct {
		cur   string
		micro int64
		want  string // unit_amount sent; "" = refused
	}{
		{"USD", 5_000_000, "500"},
		{"EUR", 5_010_000, "501"},
		{"HUF", 5_010_000, "501"}, // zero-decimal for payouts only
		{"TWD", 5_010_000, "501"},
		{"JPY", 5_000_000, "5"},
		{"JPY", 5_500_000, ""},
		{"KRW", 7_000_000, "7"},
		{"UGX", 5_000_000, "500"}, // zero-decimal sent as two-decimal
		{"UGX", 5_500_000, ""},
		{"ISK", 5_000_000, "500"},
		{"ISK", 5_010_000, ""},
		{"KWD", 5_120_000, "5120"}, // three-decimal, last digit 0
		{"KWD", 5_125_000, ""},
		{"BHD", 1_000_000, "1000"},
		{"JOD", 2_340_000, "2340"},
		{"OMR", 3_000_000, "3000"},
		{"TND", 4_560_000, "4560"},
		{"USD", 5_001_000, ""}, // fraction of a cent
		{"XYZ", 5_000_000, ""}, // unknown currency
		{"BGN", 5_000_000, ""}, // replaced by the euro
	} {
		t.Run(fmt.Sprintf("%s_%d", c.cur, c.micro), func(t *testing.T) {
			fake, srv := newFakeStripe(t)
			e := setup(t, srv.URL)
			e.l.Cur = c.cur
			code, out := e.do("POST /checkout", "/checkout", fmt.Sprintf(`{"amount_micro":%d}`, c.micro), nil)
			if c.want == "" {
				if code == http.StatusCreated {
					t.Fatalf("accepted: %v", out)
				}
				return
			}
			if code != http.StatusCreated {
				t.Fatalf("checkout: %d %v", code, out)
			}
			form := fake.sessions["cs_test_1"]
			if got := form.Get("line_items[0][price_data][unit_amount]"); got != c.want ||
				form.Get("line_items[0][price_data][currency]") != strings.ToLower(c.cur) ||
				form.Get("automatic_tax[enabled]") != "false" || form.Get("allow_promotion_codes") != "false" {
				t.Fatalf("session form: %v", form)
			}
			var n int64
			_, _ = fmt.Sscan(c.want, &n)
			body := eventIn("evt_1", out["order_id"].(string), n, strings.ToLower(c.cur))
			if code, _ := e.webhook(body, stripe.Sign(whsec, body, e.now)); code != 200 {
				t.Fatalf("webhook: %d", code)
			}
			if a, _ := e.l.Account(t.Context(), "user:u1"); a.BalanceMicro != c.micro {
				t.Fatalf("credited %d, want %d", a.BalanceMicro, c.micro)
			}
		})
	}
}

// A paid total that differs from the order (tax or a discount applied on
// Stripe's side) is not credited.
func TestWebhookRefusesChangedTotal(t *testing.T) {
	_, srv := newFakeStripe(t)
	e := setup(t, srv.URL)
	_, out := e.do("POST /checkout", "/checkout", `{"amount_micro":5000000}`, nil)
	for i, total := range []int64{550, 450} {
		b := eventIn(fmt.Sprintf("evt_%d", i), out["order_id"].(string), total, "usd")
		if code, _ := e.webhook(b, stripe.Sign(whsec, b, e.now)); code != 200 {
			t.Fatalf("webhook: %d", code)
		}
	}
	if a, _ := e.l.Account(t.Context(), "user:u1"); a.BalanceMicro != 0 {
		t.Fatalf("credited: %+v", a)
	}
}

func eventIn(id, order string, amount int64, cur string) []byte {
	b, _ := json.Marshal(map[string]any{"id": id, "object": "event", "type": "checkout.session.completed", "data": map[string]any{
		"object": map[string]any{"object": "checkout.session", "id": "cs_test_1", "client_reference_id": order,
			"metadata": map[string]string{"order_id": order}, "payment_status": "paid", "amount_total": amount, "currency": cur}}})
	return b
}

// payer=key: a Stripe top-up credits the key the user chose, and that
// key's requests draw on it.
func TestTopupPayerKey(t *testing.T) {
	_, srv := newFakeStripe(t)
	e := setup(t, srv.URL)
	e.l.Payer = "key"
	e.l.Keys["k1"] = ledgertest.Key{UserID: "u1"}
	e.l.Keys["kx"] = ledgertest.Key{UserID: "u2"}
	if code, _ := e.do("POST /checkout", "/checkout", `{"amount_micro":5000000}`, nil); code != http.StatusBadRequest {
		t.Fatalf("checkout without key_id: %d", code)
	}
	if code, _ := e.do("POST /checkout", "/checkout", `{"amount_micro":5000000,"key_id":"kx"}`, nil); code != http.StatusNotFound {
		t.Fatalf("checkout for another user's key: %d", code)
	}
	code, out := e.do("POST /checkout", "/checkout", `{"amount_micro":5000000,"key_id":"k1"}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("checkout: %d %v", code, out)
	}
	body := eventIn("evt_k", out["order_id"].(string), 500, "usd")
	if code, _ := e.webhook(body, stripe.Sign(whsec, body, e.now)); code != 200 {
		t.Fatalf("webhook: %d", code)
	}
	m := balance.New(e.l)
	call := testkit.NewCall(balance.ID, testkit.CallOpts{ID: "r1", Protocol: "openai-chat", Model: "m",
		Caller: &plugin.Caller{KeyID: "k1", UserID: "u1"}, Settings: testkit.Settings{"payer": "key"}})
	res, err := m.Reserve(call, 1_000_000)
	if err != nil {
		t.Fatalf("request refused after the top-up: %v", err)
	}
	if err := m.Settle(call, res, ir.Usage{}, 400_000); err != nil {
		t.Fatal(err)
	}
	if a, _ := e.l.Account(t.Context(), "key:k1"); a.BalanceMicro != 4_600_000 || a.HeldMicro != 0 {
		t.Fatalf("key account: %+v", a)
	}
	if a, _ := e.l.Account(t.Context(), "user:u1"); a.BalanceMicro != 0 {
		t.Fatalf("user account credited: %+v", a)
	}
}
