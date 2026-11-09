package epay_test

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

	"github.com/initxy/plugrelay/plugins/admin/payment"
	"github.com/initxy/plugrelay/plugins/admin/payment/epay"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger/ledgertest"
)

const (
	pid = "1001"
	key = "merchant-secret-key"
)

// fakeGateway is an epay gateway: mapi.php checks the signature and
// answers as the 彩虹易支付 code does; notify builds the signed
// parameters the gateway sends to notify_url and return_url.
type fakeGateway struct {
	mu     sync.Mutex
	orders map[string]url.Values
	srv    *httptest.Server
}

func newGateway(t *testing.T) *fakeGateway {
	g := &fakeGateway{orders: map[string]url.Values{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/mapi.php" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.PostForm.Get("pid") != pid || epay.Sign(r.PostForm, key) != r.PostForm.Get("sign") || r.PostForm.Get("sign_type") != "MD5" {
			_, _ = io.WriteString(w, `{"code":-1,"msg":"签名错误"}`)
			return
		}
		g.mu.Lock()
		g.orders[r.PostForm.Get("out_trade_no")] = r.PostForm
		g.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"code":1,"msg":"","trade_no":"2026110600001","payurl":"%s/pay/2026110600001"}`, g.srv.URL)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// notify returns the parameters epay sends for a paid order.
func notify(order, tradeNo, money, status string) url.Values {
	v := url.Values{"pid": {pid}, "trade_no": {tradeNo}, "out_trade_no": {order}, "type": {"alipay"}, "name": {"Balance top-up"},
		"money": {money}, "trade_status": {status}}
	v.Set("sign", epay.Sign(v, key))
	v.Set("sign_type", "MD5")
	return v
}

type env struct {
	l      *ledgertest.Memory
	routes map[string]http.HandlerFunc
}

func setup(t *testing.T, currency, mode, gateway string) *env {
	l := ledgertest.New()
	l.Cur = currency
	kv := testkit.NewState(time.Now)
	p := payment.New(l, kv)
	h := &testkit.Host{Store: kv, Set: testkit.Settings{"epay_url": gateway, "epay_pid": pid, "epay_key": key, "epay_mode": mode,
		"epay_types":      []string{"alipay", "wxpay"},
		"min_topup_micro": 1_000_000, "max_topup_micro": 500_000_000}, Public: "https://relay.example.com"}
	if err := p.Init(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	e := &env{l: l, routes: map[string]http.HandlerFunc{}}
	for _, rt := range p.Routes() {
		e.routes[rt.Method+" "+rt.Path] = rt.Handler
	}
	return e
}

func (e *env) do(route, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(strings.Fields(route)[0], "/api/plugins/admin.payment"+path, strings.NewReader(body))
	req.Header.Set("X-Plugrelay-User", "u1")
	if strings.HasPrefix(route, "POST /webhook") {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	e.routes[route](rec, req)
	return rec
}

func (e *env) checkout(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	rec := e.do("POST /checkout", "/checkout", body)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (e *env) notify(v url.Values) (int, string) {
	rec := e.do("GET /webhook/{provider}", "/webhook/epay?"+v.Encode(), "")
	return rec.Code, rec.Body.String()
}

func (e *env) balance(t *testing.T) int64 {
	a, err := e.l.Account(t.Context(), "user:u1")
	if err != nil {
		t.Fatal(err)
	}
	return a.BalanceMicro
}

func TestSignature(t *testing.T) {
	// The documented algorithm: sign and sign_type and empty values left
	// out, sorted, k=v joined by &, the key appended, MD5 lowercase hex.
	v := url.Values{"pid": {"1001"}, "type": {"alipay"}, "out_trade_no": {"ORDER123"}, "notify_url": {"https://example.com/notify"},
		"return_url": {"https://example.com/return"}, "name": {"TUC1"}, "money": {"1.00"}, "device": {"pc"}, "param": {""},
		"sign_type": {"MD5"}}
	// printf '%s' 'device=pc&money=1.00&name=TUC1&notify_url=https://example.com/notify&out_trade_no=ORDER123&pid=1001&return_url=https://example.com/return&type=alipayTESTKEY' | md5sum
	if got := epay.Sign(v, "TESTKEY"); got != "d96c2e8c9751f45589cdc70068a287e6" {
		t.Fatalf("sign = %s", got)
	}
	for micro, want := range map[int64]string{10_000_000: "10.00", 10_500_000: "10.50", 10_000: "0.01", 123_450_000: "123.45"} {
		if got, ok := epay.Yuan(micro); !ok || got != want {
			t.Errorf("Yuan(%d) = %q %v", micro, got, ok)
		}
	}
	if _, ok := epay.Yuan(10_005_000); ok {
		t.Error("a fraction of a fen accepted")
	}
}

func TestSubmitCheckoutAndNotify(t *testing.T) {
	g := newGateway(t)
	e := setup(t, "CNY", "submit", g.srv.URL)
	rec := e.do("GET /config", "/config", "")
	if !strings.Contains(rec.Body.String(), `{"provider":"epay","method":"alipay"}`) || !strings.Contains(rec.Body.String(), `"method":"wxpay"`) {
		t.Fatalf("config methods: %s", rec.Body.String())
	}
	if code, out := e.checkout(t, `{"amount_micro":10000000,"provider":"epay","method":"unionpay"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown method: %d %v", code, out)
	}
	code, out := e.checkout(t, `{"amount_micro":10000000,"provider":"epay","method":"wxpay"}`)
	if code != http.StatusCreated {
		t.Fatalf("checkout: %d %v", code, out)
	}
	order := out["order_id"].(string)
	u, _ := url.Parse(out["url"].(string))
	q := u.Query()
	if u.Path != "/submit.php" || q.Get("pid") != pid || q.Get("type") != "wxpay" || q.Get("out_trade_no") != order ||
		q.Get("money") != "10.00" || q.Get("sign_type") != "MD5" || q.Get("sign") != epay.Sign(q, key) ||
		q.Get("notify_url") != "https://relay.example.com/api/plugins/admin.payment/webhook/epay" ||
		q.Get("return_url") != "https://relay.example.com/api/plugins/admin.payment/return/epay" {
		t.Fatalf("submit url %s", u)
	}

	// Bad signature and another merchant's notify: refused, nothing credited.
	forged := notify(order, "T1", "10.00", "TRADE_SUCCESS")
	forged.Set("money", "1000.00")
	other := notify(order, "T1", "10.00", "TRADE_SUCCESS")
	other.Set("pid", "2002")
	other.Set("sign", epay.Sign(other, key))
	for name, v := range map[string]url.Values{"bad sign": forged, "other merchant": other} {
		if code, body := e.notify(v); code != http.StatusBadRequest || body != "fail" {
			t.Fatalf("%s: %d %q", name, code, body)
		}
	}
	// Not paid yet, amount mismatch, unknown order: acknowledged, not credited.
	for name, v := range map[string]url.Values{
		"unpaid":         notify(order, "T0", "10.00", "WAIT_BUYER_PAY"),
		"amount differs": notify(order, "T9", "1.00", "TRADE_SUCCESS"),
		"unknown order":  notify("ord_nope", "T8", "10.00", "TRADE_SUCCESS"),
	} {
		if code, body := e.notify(v); code != http.StatusOK || body != "success" {
			t.Fatalf("%s: %d %q", name, code, body)
		}
	}
	if b := e.balance(t); b != 0 {
		t.Fatalf("credited %d before a valid notify", b)
	}
	// The real notify credits; a duplicate delivery, a replay under another
	// trade number and the user's signed return credit nothing more. The
	// gateway may drop the trailing zeros of money.
	paid := notify(order, "T1", "10", "TRADE_SUCCESS")
	for range 2 {
		if code, body := e.notify(paid); code != http.StatusOK || body != "success" {
			t.Fatalf("notify: %d %q", code, body)
		}
	}
	if code, body := e.notify(notify(order, "T2", "10.00", "TRADE_SUCCESS")); code != http.StatusOK || body != "success" {
		t.Fatalf("replay: %d %q", code, body)
	}
	ret := e.do("GET /return/{provider}", "/return/epay?"+paid.Encode(), "")
	if ret.Code != http.StatusSeeOther || ret.Header().Get("Location") != "https://relay.example.com/billing?order="+order {
		t.Fatalf("return: %d %s", ret.Code, ret.Header().Get("Location"))
	}
	if b := e.balance(t); b != 10_000_000 {
		t.Fatalf("balance %d", b)
	}
	rec = e.do("GET /orders/{id}", "/orders/"+order, "")
	if !strings.Contains(rec.Body.String(), `"status":"paid"`) {
		t.Fatalf("order: %s", rec.Body.String())
	}
}

// The user's signed return credits the order when the notify never
// arrived (the gateway could not reach the relay).
func TestReturnCredits(t *testing.T) {
	g := newGateway(t)
	e := setup(t, "CNY", "submit", g.srv.URL)
	_, out := e.checkout(t, `{"amount_micro":5000000,"provider":"epay","method":"alipay"}`)
	order := out["order_id"].(string)
	bad := notify(order, "T1", "5.00", "TRADE_SUCCESS")
	bad.Set("sign", "0000")
	if ret := e.do("GET /return/{provider}", "/return/epay?"+bad.Encode(), ""); ret.Code != http.StatusSeeOther || e.balance(t) != 0 {
		t.Fatalf("forged return: %d, balance %d", ret.Code, e.balance(t))
	}
	e.do("GET /return/{provider}", "/return/epay?"+notify(order, "T1", "5.00", "TRADE_SUCCESS").Encode(), "")
	if b := e.balance(t); b != 5_000_000 {
		t.Fatalf("balance %d", b)
	}
}

func TestMapiCheckout(t *testing.T) {
	g := newGateway(t)
	e := setup(t, "CNY", "mapi", g.srv.URL)
	code, out := e.checkout(t, `{"amount_micro":20000000,"provider":"epay","method":"alipay"}`)
	if code != http.StatusCreated || out["url"] != g.srv.URL+"/pay/2026110600001" {
		t.Fatalf("mapi checkout: %d %v", code, out)
	}
	sent := g.orders[out["order_id"].(string)]
	if sent.Get("money") != "20.00" || sent.Get("device") != "pc" || sent.Get("clientip") == "" {
		t.Fatalf("mapi form %v", sent)
	}
	// A gateway that refuses (wrong key) surfaces its message.
	badHost := &epay.Provider{URL: g.srv.URL, PID: pid, Key: "wrong", Mode: "mapi", Client: http.DefaultClient}
	_, err := badHost.Checkout(t.Context(), payment.Order{ID: "o1", AmountMicro: 1_000_000, Currency: "CNY", CallbackURL: "https://r/api/plugins/admin.payment"}, "", "")
	if err == nil || !strings.Contains(err.Error(), "签名错误") {
		t.Fatalf("refused order: %v", err)
	}
}

// epay has no currency: a site in another currency cannot use it.
func TestOtherCurrencyRefused(t *testing.T) {
	g := newGateway(t)
	e := setup(t, "USD", "submit", g.srv.URL)
	if rec := e.do("GET /config", "/config", ""); strings.Contains(rec.Body.String(), "epay") && strings.Contains(rec.Body.String(), `"method":"alipay"`) {
		t.Fatalf("epay offered in USD: %s", rec.Body.String())
	}
	if code, out := e.checkout(t, `{"amount_micro":10000000,"provider":"epay","method":"alipay"}`); code != http.StatusBadRequest {
		t.Fatalf("USD checkout: %d %v", code, out)
	}
}

// A notice that credited nothing does not hide a later one under the same
// trade number: not paid yet then paid, a wrong amount then the right one,
// the user's return then the notify.
func TestNotifySameTradeNo(t *testing.T) {
	g := newGateway(t)
	for name, first := range map[string]func(order string) url.Values{
		"wait then success": func(o string) url.Values { return notify(o, "T1", "10.00", "WAIT_BUYER_PAY") },
		"wrong amount":      func(o string) url.Values { return notify(o, "T1", "1.00", "TRADE_SUCCESS") },
	} {
		e := setup(t, "CNY", "submit", g.srv.URL)
		_, out := e.checkout(t, `{"amount_micro":10000000,"provider":"epay","method":"alipay"}`)
		order := out["order_id"].(string)
		if code, body := e.notify(first(order)); code != http.StatusOK || body != "success" || e.balance(t) != 0 {
			t.Fatalf("%s: first %d %q, balance %d", name, code, body, e.balance(t))
		}
		if code, body := e.notify(notify(order, "T1", "10.00", "TRADE_SUCCESS")); code != http.StatusOK || body != "success" {
			t.Fatalf("%s: success %d %q", name, code, body)
		}
		if b := e.balance(t); b != 10_000_000 {
			t.Fatalf("%s: balance %d", name, b)
		}
	}
	e := setup(t, "CNY", "submit", g.srv.URL)
	_, out := e.checkout(t, `{"amount_micro":10000000,"provider":"epay","method":"alipay"}`)
	paid := notify(out["order_id"].(string), "T1", "10.00", "TRADE_SUCCESS")
	e.do("GET /return/{provider}", "/return/epay?"+paid.Encode(), "")
	if code, body := e.notify(paid); code != http.StatusOK || body != "success" || e.balance(t) != 10_000_000 {
		t.Fatalf("return then notify: %d %q, balance %d", code, body, e.balance(t))
	}
}

// Without the site public URL checkout is refused: return
// and notify URLs are never built from the request's Host header.
func TestCheckoutNeedsConsoleURL(t *testing.T) {
	g := newGateway(t)
	l := ledgertest.New()
	l.Cur = "CNY"
	kv := testkit.NewState(time.Now)
	p := payment.New(l, kv)
	if err := p.Init(t.Context(), &testkit.Host{Store: kv, Set: testkit.Settings{"epay_url": g.srv.URL, "epay_pid": pid, "epay_key": key}}); err != nil {
		t.Fatal(err)
	}
	var checkout http.HandlerFunc
	for _, rt := range p.Routes() {
		if rt.Method == http.MethodPost && rt.Path == "/checkout" {
			checkout = rt.Handler
		}
	}
	req := httptest.NewRequest("POST", "/api/plugins/admin.payment/checkout", strings.NewReader(`{"amount_micro":10000000,"provider":"epay","method":"alipay"}`))
	req.Host = "evil.example"
	req.Header.Set("X-Plugrelay-User", "u1")
	rec := httptest.NewRecorder()
	checkout(rec, req)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "evil.example") {
		t.Fatalf("checkout without a console URL: %d %s", rec.Code, rec.Body.String())
	}
}
