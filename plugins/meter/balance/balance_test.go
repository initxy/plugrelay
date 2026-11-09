package balance

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger/ledgertest"
)

func call(id, protocol string, set testkit.Settings) *plugin.Call {
	return testkit.NewCall(ID, testkit.CallOpts{ID: id, Protocol: protocol, Model: "m",
		Caller: &plugin.Caller{KeyID: "k1", UserID: "u1", GroupID: "g1"}, Settings: set})
}

func topup(t *testing.T, l *ledgertest.Memory, subject string, micro int64) {
	t.Helper()
	if _, _, err := l.Credit(t.Context(), ledger.Txn{ID: "t-" + subject, Subject: subject, Kind: ledger.Topup, AmountMicro: micro}); err != nil {
		t.Fatal(err)
	}
}

func TestReserveSettleRefund(t *testing.T) {
	l := ledgertest.New()
	m := New(l)
	topup(t, l, "user:u1", 1000)

	c := call("r1", "openai-chat", nil)
	res, err := m.Reserve(c, 400)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := l.Account(t.Context(), "user:u1"); a.HeldMicro != 400 {
		t.Fatalf("held %+v", a)
	}
	// Settle twice: charged once.
	for range 2 {
		if err := m.Settle(c, res, ir.Usage{}, 250); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Refund(c, res); err != nil {
		t.Fatal(err)
	}
	if a, _ := l.Account(t.Context(), "user:u1"); a.BalanceMicro != 750 || a.HeldMicro != 0 {
		t.Fatalf("after settle %+v", a)
	}

	// Upstream failure: refunded, nothing charged.
	c2 := call("r2", "openai-chat", nil)
	res2, err := m.Reserve(c2, 700)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Refund(c2, res2); err != nil {
		t.Fatal(err)
	}
	if err := m.Settle(c2, res2, ir.Usage{}, 700); err != nil {
		t.Fatal(err)
	}
	if a, _ := l.Account(t.Context(), "user:u1"); a.BalanceMicro != 750 || a.HeldMicro != 0 {
		t.Fatalf("after refund %+v", a)
	}
}

func TestInsufficientBalancePerProtocol(t *testing.T) {
	l := ledgertest.New()
	m := New(l)
	topup(t, l, "user:u1", 100)
	for _, tc := range []struct {
		protocol string
		status   int
		code     string
	}{
		{"anthropic", http.StatusPaymentRequired, "billing_error"},
		{"openai-chat", 0, "insufficient_quota"},
		{"openai-responses", 0, "insufficient_quota"},
		{"gemini", 0, ""},
	} {
		_, err := m.Reserve(call("r-"+tc.protocol, tc.protocol, nil), 500)
		var gw *ir.GwError
		if !errors.As(err, &gw) {
			t.Fatalf("%s: %v", tc.protocol, err)
		}
		if gw.Kind != ir.ErrQuotaExhausted || !gw.NoRetry || gw.Status != tc.status || gw.Code != tc.code ||
			!strings.Contains(gw.Message, "0.0001 USD") || plugin.IsFailure(err) {
			t.Errorf("%s: %+v", tc.protocol, gw)
		}
	}
	if a, _ := l.Account(t.Context(), "user:u1"); a.HeldMicro != 0 {
		t.Fatalf("refused requests left holds: %+v", a)
	}
	// A request with no estimate still needs a positive balance.
	if _, err := m.Reserve(call("r-zero", "openai-chat", nil), 0); err != nil {
		t.Fatalf("positive balance, zero estimate: %v", err)
	}
	empty := ledgertest.New()
	if _, err := New(empty).Reserve(call("r-empty", "openai-chat", nil), 0); err == nil {
		t.Fatal("empty balance served")
	}
}

func TestPayerExemptAndMinReserve(t *testing.T) {
	l := ledgertest.New()
	m := New(l)
	topup(t, l, "group:g1", 1000)
	c := call("r1", "anthropic", testkit.Settings{"payer": "group", "min_reserve_micro": 300})
	if _, err := m.Reserve(c, 10); err != nil {
		t.Fatal(err)
	}
	if a, _ := l.Account(t.Context(), "group:g1"); a.HeldMicro != 300 {
		t.Fatalf("group hold %+v", a)
	}
	ex := call("r2", "anthropic", testkit.Settings{"exempt": true})
	res, err := m.Reserve(ex, 1_000_000)
	if err != nil {
		t.Fatalf("exempt caller refused: %v", err)
	}
	if err := m.Settle(ex, res, ir.Usage{}, 99); err != nil {
		t.Fatal(err)
	}
	if a, _ := l.Account(t.Context(), "user:u1"); a.BalanceMicro != 0 {
		t.Fatalf("exempt caller charged %+v", a)
	}
}

func TestBalanceHeader(t *testing.T) {
	l := ledgertest.New()
	m := New(l)
	topup(t, l, "user:u1", 2_500_000)
	c := call("r1", "openai-chat", testkit.Settings{"balance_header": "x-balance", "low_balance_micro": 3_000_000})
	if _, err := m.Reserve(c, 500_000); err != nil {
		t.Fatal(err)
	}
	if got := m.Headers(c).Get("x-balance"); got != "2.000000" {
		t.Fatalf("header %q", got)
	}
	c2 := call("r2", "openai-chat", testkit.Settings{"balance_header": "x-balance", "low_balance_micro": 1_000_000})
	if _, err := m.Reserve(c2, 0); err != nil {
		t.Fatal(err)
	}
	if h := m.Headers(c2); h != nil {
		t.Fatalf("header above the threshold: %v", h)
	}
}

func TestConcurrentReserveNeverOverdraws(t *testing.T) {
	l := ledgertest.New()
	m := New(l)
	topup(t, l, "user:u1", 1000)
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := call("c"+string(rune('a'+i)), "openai-chat", nil)
			if res, err := m.Reserve(c, 100); err == nil {
				_ = m.Settle(c, res, ir.Usage{}, 100)
			}
		}()
	}
	wg.Wait()
	if a, _ := l.Account(t.Context(), "user:u1"); a.BalanceMicro != 0 || a.HeldMicro != 0 {
		t.Fatalf("balance %+v", a)
	}
}

func TestAdminRoutes(t *testing.T) {
	l := ledgertest.New()
	m := New(l)
	do := func(method, path, role, body string) (int, map[string]any) {
		var h http.HandlerFunc
		for _, rt := range m.Routes() {
			if rt.Method == method && rt.Path == path {
				if rt.Role == "admin" && role != "admin" {
					return http.StatusForbidden, nil
				}
				h = rt.Handler
			}
		}
		req := httptest.NewRequest(method, "/api/plugins/meter.balance"+path, strings.NewReader(body))
		req.Header.Set("X-Plugrelay-User", "u1")
		req.Header.Set("X-Plugrelay-Role", role)
		rec := httptest.NewRecorder()
		h(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, out := do("POST", "/adjust", "admin", `{"subject":"user:u1","amount_micro":5000000,"note":"gift","idempotency_key":"a1"}`); code != 200 || out["applied"] != true {
		t.Fatalf("adjust: %d %v", code, out)
	}
	if code, out := do("POST", "/adjust", "admin", `{"subject":"user:u1","amount_micro":5000000,"idempotency_key":"a1"}`); code != 200 || out["applied"] != false {
		t.Fatalf("resubmitted adjust: %d %v", code, out)
	}
	if code, _ := do("POST", "/adjust", "admin", `{"subject":"user:u1","amount_micro":-9000000,"idempotency_key":"a2"}`); code != http.StatusConflict {
		t.Fatalf("overdrawing adjust: %d", code)
	}
	code, out := do("GET", "/me", "user", "")
	if code != 200 || out["balance"].(map[string]any)["micro"].(float64) != 5_000_000 {
		t.Fatalf("me: %d %v", code, out)
	}
	// Refund of a charged request, capped at its charge, once.
	c := call("req-9", "openai-chat", nil)
	res, _ := m.Reserve(c, 100)
	_ = m.Settle(c, res, ir.Usage{}, 1_000_000)
	if code, out := do("POST", "/refund", "admin", `{"request_id":"req-9","amount_micro":9000000}`); code != 200 ||
		out["account"].(map[string]any)["balance"].(map[string]any)["micro"].(float64) != 5_000_000 {
		t.Fatalf("refund: %d %v", code, out)
	}
	if code, out := do("POST", "/refund", "admin", `{"request_id":"req-9"}`); code != 200 || out["applied"] != false {
		t.Fatalf("second refund: %d %v", code, out)
	}
	code, out = do("GET", "/me/transactions", "user", "")
	if items := out["items"].([]any); code != 200 || len(items) != 3 {
		t.Fatalf("transactions: %d %v", code, out)
	}
	if code, _ := do("GET", "/accounts", "user", ""); code != http.StatusForbidden {
		t.Fatalf("accounts as user: %d", code)
	}
}

// A batch holds its estimate across requests; its charge is taken in
// full under the hold id even when the balance ran out, once, and an
// exempt payer only has the hold released.
func TestDeferredHoldAndCharge(t *testing.T) {
	l := ledgertest.New()
	m := New(l)
	topup(t, l, "user:u1", 1000)
	if err := m.Hold(call("req1", "anthropic", nil), "batch:1", 2000); err == nil {
		t.Fatal("hold past the balance accepted")
	} else if gw, ok := err.(*ir.GwError); !ok || gw.Status != http.StatusPaymentRequired {
		t.Fatalf("refusal %v", err)
	}
	if err := m.Hold(call("req1", "anthropic", nil), "batch:1", 800); err != nil {
		t.Fatal(err)
	}
	if a, _ := l.Account(t.Context(), "user:u1"); a.HeldMicro != 800 {
		t.Fatalf("held %+v", a)
	}
	for range 2 {
		if err := m.Charge(call("req2", "anthropic", nil), "batch:1", ir.Usage{}, 1500); err != nil {
			t.Fatal(err)
		}
	}
	if a, _ := l.Account(t.Context(), "user:u1"); a.BalanceMicro != -500 || a.HeldMicro != 0 {
		t.Fatalf("after charge %+v", a)
	}
	exempt := testkit.Settings{"exempt": true}
	if err := m.Hold(call("req3", "anthropic", nil), "batch:2", 100); err == nil {
		t.Fatal("hold on a negative balance accepted")
	}
	if err := m.Charge(call("req4", "anthropic", exempt), "batch:3", ir.Usage{}, 100); err != nil {
		t.Fatal(err)
	}
	if a, _ := l.Account(t.Context(), "user:u1"); a.BalanceMicro != -500 {
		t.Fatalf("exempt payer charged %+v", a)
	}
}
