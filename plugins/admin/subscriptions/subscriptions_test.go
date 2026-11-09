package subscriptions_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/plugins/admin/subscriptions"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger/ledgertest"
)

// noSubs is a subscription store the plan routes never reach.
type noSubs struct{ ledger.Subscriptions }

func setup(t *testing.T) map[string]http.HandlerFunc {
	kv := testkit.NewState(time.Now)
	p := subscriptions.New(ledgertest.New(), noSubs{}, kv)
	if err := p.Init(t.Context(), &testkit.Host{Store: kv, Set: testkit.Settings{}}); err != nil {
		t.Fatal(err)
	}
	routes := map[string]http.HandlerFunc{}
	for _, rt := range p.Routes() {
		routes[rt.Method+" "+rt.Path] = rt.Handler
	}
	return routes
}

func call(h http.HandlerFunc, method, path, body, role string) (int, map[string]any) {
	req := httptest.NewRequest(method, "/api/plugins/admin.subscriptions"+path, strings.NewReader(body))
	req.Header.Set("X-Plugrelay-User", "u1")
	req.Header.Set("X-Plugrelay-Role", role)
	rec := httptest.NewRecorder()
	h(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestPlans(t *testing.T) {
	routes := setup(t)
	post := routes["POST /plans"]
	for name, body := range map[string]string{
		"no name":      `{"price_micro":1,"period":"month","allowance_unit":"money","allowance":1,"overage":"block"}`,
		"bad period":   `{"name":"x","price_micro":1,"period":"day","allowance_unit":"money","allowance":1,"overage":"block"}`,
		"bad unit":     `{"name":"x","price_micro":1,"period":"month","allowance_unit":"points","allowance":1,"overage":"block"}`,
		"no allowance": `{"name":"x","price_micro":1,"period":"month","allowance_unit":"money","allowance":0,"overage":"block"}`,
		"bad overage":  `{"name":"x","price_micro":1,"period":"month","allowance_unit":"money","allowance":1,"overage":"free"}`,
		"unknown id":   `{"id":"plan_x","name":"x","price_micro":1,"period":"month","allowance_unit":"money","allowance":1,"overage":"block"}`,
	} {
		if code, out := call(post, "POST", "/plans", body, "admin"); code/100 != 4 {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	code, pl := call(post, "POST", "/plans", `{"name":"Pro","price_micro":9900000,"period":"month","allowance_unit":"money","allowance":15000000,"overage":"balance","group_id":"pro"}`, "admin")
	if code != http.StatusCreated || pl["currency"] != "USD" || !strings.HasPrefix(pl["id"].(string), "plan_") {
		t.Fatalf("create: %d %v", code, pl)
	}
	// Disabling hides it from users, not from admins asking for all.
	upd := `{"id":"` + pl["id"].(string) + `","name":"Pro","price_micro":9900000,"period":"month","allowance_unit":"money","allowance":15000000,"overage":"balance","disabled":true}`
	if code, out := call(post, "POST", "/plans", upd, "admin"); code != http.StatusOK || out["disabled"] != true || out["created_at"] != pl["created_at"] {
		t.Fatalf("update: %d %v", code, out)
	}
	get := routes["GET /plans"]
	if _, out := call(get, "GET", "/plans?all=1", "", "user"); len(out["items"].([]any)) != 0 {
		t.Fatalf("user sees a disabled plan: %v", out)
	}
	if _, out := call(get, "GET", "/plans?all=1", "", "admin"); len(out["items"].([]any)) != 1 {
		t.Fatalf("admin listing: %v", out)
	}
	// Only the balance is offered without a configured provider.
	if _, out := call(routes["GET /config"], "GET", "/config", "", "user"); len(out["methods"].([]any)) != 1 {
		t.Fatalf("config: %v", out)
	}
}
