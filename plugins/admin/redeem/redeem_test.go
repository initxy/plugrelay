package redeem

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/plugins/internal/testkit"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger/ledgertest"
)

func TestBatchAndRedeem(t *testing.T) {
	l := ledgertest.New()
	kv := testkit.NewState(nil)
	p := New(l, kv)
	if err := p.Init(t.Context(), &testkit.Host{Store: kv, Set: testkit.Settings{"failures_per_hour": 3}}); err != nil {
		t.Fatal(err)
	}
	routes := map[string]http.HandlerFunc{}
	for _, rt := range p.Routes() {
		routes[rt.Method+" "+rt.Path] = rt.Handler
	}
	do := func(route, user, body string) (int, map[string]any) {
		req := httptest.NewRequest(strings.Fields(route)[0], "/x", strings.NewReader(body))
		req.Header.Set("X-Plugrelay-User", user)
		rec := httptest.NewRecorder()
		routes[route](rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	code, out := do("POST /batches", "admin", `{"count":3,"amount_micro":2000000,"max_uses":1,"expires_at":"`+exp+`","batch":"promo"}`)
	if code != http.StatusCreated {
		t.Fatalf("batch: %d %v", code, out)
	}
	codes := out["codes"].([]any)
	if len(codes) != 3 || !strings.HasPrefix(codes[0].(string), "RC-") {
		t.Fatalf("codes: %v", codes)
	}
	// Stored hashed: the listing never carries the code.
	_, list := do("GET /codes", "admin", "")
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), codes[0].(string)) || len(list["items"].([]any)) != 3 {
		t.Fatalf("listing: %s", raw)
	}
	// Typed in lower case without dashes still works.
	typed := strings.ToLower(strings.ReplaceAll(codes[0].(string), "-", ""))
	if code, out := do("POST /redeem", "u1", `{"code":"`+typed+`"}`); code != 200 || out["balance"].(map[string]any)["micro"].(float64) != 2_000_000 {
		t.Fatalf("redeem: %d %v", code, out)
	}
	if code, _ := do("POST /redeem", "u1", `{"code":"`+codes[0].(string)+`"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("double redeem: %d", code)
	}
	if code, _ := do("POST /redeem", "u2", `{"code":"`+codes[0].(string)+`"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("used-up code: %d", code)
	}
	// Failures are limited per user.
	for range 2 {
		do("POST /redeem", "u1", `{"code":"RC-NOPE"}`)
	}
	if code, _ := do("POST /redeem", "u1", `{"code":"`+codes[1].(string)+`"}`); code != http.StatusTooManyRequests {
		t.Fatalf("after 3 failures: %d", code)
	}
	if code, _ := do("POST /redeem", "u3", `{"code":"`+codes[1].(string)+`"}`); code != 200 {
		t.Fatalf("other user: %d", code)
	}
	if code, _ := do("POST /batches", "admin", `{"count":1,"amount_micro":1,"expires_at":"2001-01-01T00:00:00Z"}`); code != http.StatusBadRequest {
		t.Fatalf("past expiry accepted: %d", code)
	}
}
