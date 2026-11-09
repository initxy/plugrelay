package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// builtinPlugins adds a compiled-in plugin with public routes to the fake
// runtime.
type builtinPlugins struct {
	*fakeRuntime
	enabled bool
	source  string
}

func (b builtinPlugins) Plugins() []PluginEntry {
	echo := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "user="+r.Header.Get("X-Plugrelay-User")+" cookie="+r.Header.Get("Cookie"))
	}
	return append(b.fakeRuntime.Plugins(), PluginEntry{
		Info: plugin.Info{ID: "admin.demo", Kind: plugin.KindAdmin}, Source: b.source, Enabled: b.enabled,
		Routes: []plugin.AdminRoute{
			{Method: "POST", Path: "/hook/{name}", Handler: echo, Public: true},
			{Method: "POST", Path: "/signup", Handler: echo, Public: true},
			{Method: "GET", Path: "/private", Handler: echo},
			{Method: "POST", Path: "/danger", Handler: echo, Role: "admin", Reauth: true},
		},
	})
}

func TestBuiltinPluginPublicRoutes(t *testing.T) {
	var rt builtinPlugins
	e := newEnv(t, sqliteStore, false, func(d *Deps) {
		rt = builtinPlugins{fakeRuntime: d.Runtime.(*fakeRuntime), enabled: true, source: SourceBuiltin}
		d.Runtime = rt
	})
	ctx := context.Background()
	if err := e.st.CreateUser(ctx, &store.User{Name: "a", Email: "admin@x", Role: store.RoleAdmin}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}

	post := func(path string, hdr map[string]string) (int, string) {
		req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader("{}"))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// Public: no login needed, and no identity or cookie reaches the plugin.
	if code, body := post("/api/plugins/admin.demo/hook/stripe", map[string]string{"X-Plugrelay-User": "admin", "Cookie": "s=1"}); code != 200 || body != "user= cookie=" {
		t.Fatalf("public route: %d %q", code, body)
	}
	resp, err := http.Get(e.srv.URL + "/api/plugins/admin.demo/private")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("private route without login: %d", resp.StatusCode)
	}

	// Calls per address are bounded: past the limit, 429 with Retry-After.
	var last int
	for range publicMaxCalls - 1 { // one call above
		last, _ = post("/api/plugins/admin.demo/hook/stripe", nil)
	}
	if last != 200 {
		t.Fatalf("within the limit: %d", last)
	}
	if code, body := post("/api/plugins/admin.demo/hook/stripe", nil); code != http.StatusTooManyRequests || !strings.Contains(body, "rate_limited") {
		t.Fatalf("past the limit: %d %s", code, body)
	}
	// Counted per route: another public route of the address is served.
	if code, _ := post("/api/plugins/admin.demo/signup", nil); code != 200 {
		t.Fatalf("another route past the first's limit: %d", code)
	}
	e.h.publicCalls = newPublicLimiter()

	// Disabled, or not compiled in: public routes are not served.
	for _, alt := range []builtinPlugins{{fakeRuntime: rt.fakeRuntime, enabled: false, source: SourceBuiltin},
		{fakeRuntime: rt.fakeRuntime, enabled: true, source: SourceExternal}} {
		e.h.d.Runtime = alt
		if code, _ := post("/api/plugins/admin.demo/hook/stripe", nil); code == 200 {
			t.Fatalf("public route served for %+v", alt)
		}
	}
}

type balancePlugins struct{ *fakeRuntime }

func (b balancePlugins) Plugins() []PluginEntry {
	return append(b.fakeRuntime.Plugins(), PluginEntry{Info: plugin.Info{ID: balanceMeter, Kind: plugin.KindMeter},
		Source: SourceBuiltin, Enabled: true})
}

func TestOverviewShowsBalance(t *testing.T) {
	e := newEnv(t, sqliteStore, false, func(d *Deps) { d.Runtime = balancePlugins{d.Runtime.(*fakeRuntime)} })
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.st.CreateUser(ctx, &store.User{Name: "a", Email: "admin", Role: store.RoleAdmin}, "correct-horse-1"))
	ann := &store.User{Name: "ann", Email: "ann", Role: store.RoleUser}
	must(e.st.CreateUser(ctx, ann, "anns-password-1"))
	l, _ := store.LedgerOf(e.st)
	_, _, err := l.Credit(ctx, &store.BalanceTxn{ID: "t1", Subject: "user:" + ann.ID, Kind: store.TxnTopup, AmountMicro: 12_500_000})
	must(err)
	c := e.browser()
	c.must(200, "POST", "/api/v1/auth/login", map[string]any{"username": "ann", "password": "anns-password-1"})
	_, out := c.do("GET", "/api/v1/overview?range=7d", nil)
	b, ok := out["balance"].(map[string]any)
	if !ok || b["amount"].(map[string]any)["micro"].(float64) != 12_500_000 {
		t.Fatalf("overview balance: %v", out)
	}
}

func TestUserListBalance(t *testing.T) {
	for _, on := range []bool{false, true} {
		e := newEnv(t, sqliteStore, false, func(d *Deps) {
			if on {
				d.Runtime = balancePlugins{d.Runtime.(*fakeRuntime)}
			}
		})
		ctx := context.Background()
		if err := e.st.CreateUser(ctx, &store.User{Name: "a", Email: "admin", Role: store.RoleAdmin}, "correct-horse-1"); err != nil {
			t.Fatal(err)
		}
		ann := &store.User{Name: "ann", Email: "ann", Role: store.RoleUser}
		if err := e.st.CreateUser(ctx, ann, "anns-password-1"); err != nil {
			t.Fatal(err)
		}
		l, _ := store.LedgerOf(e.st)
		if _, _, err := l.Credit(ctx, &store.BalanceTxn{ID: "t1", Subject: "user:" + ann.ID, Kind: store.TxnTopup, AmountMicro: 7_000_000}); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Hold(ctx, "user:"+ann.ID, "req-1", 2_000_000); err != nil {
			t.Fatal(err)
		}
		c := e.browser()
		c.must(200, "POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "correct-horse-1"})
		_, out := c.do("GET", "/api/v1/users", nil)
		got := map[string]any{}
		for _, it := range out["items"].([]any) {
			u := it.(map[string]any)
			got[u["username"].(string)] = u["balance"]
		}
		_, one := c.do("GET", "/api/v1/users/"+ann.ID, nil)
		if !on {
			if got["ann"] != nil || got["admin"] != nil || one["balance"] != nil {
				t.Fatalf("balance shown without meter.balance: %v / %v", got, one)
			}
			continue
		}
		micro := func(v any) float64 { return v.(map[string]any)["micro"].(float64) }
		if got["ann"] == nil || micro(got["ann"]) != 5_000_000 || got["admin"] == nil || micro(got["admin"]) != 0 {
			t.Fatalf("user list balances = %v, want ann 5000000 available (7 credited, 2 held) and admin 0", got)
		}
		if one["balance"] == nil || micro(one["balance"]) != 5_000_000 {
			t.Fatalf("get user balance = %v", one["balance"])
		}
	}
}

// A plugin route marked Reauth needs a recent re-authentication of the
// console session; plugins' user-delete and settings checks can refuse.
func TestPluginReauthRouteAndChecks(t *testing.T) {
	var rt builtinPlugins
	e := newEnv(t, sqliteStore, false, func(d *Deps) {
		rt = builtinPlugins{fakeRuntime: d.Runtime.(*fakeRuntime), enabled: true, source: SourceBuiltin}
		d.Runtime = rt
	})
	admin := e.setup()
	admin.dropReauth()
	if code, out := admin.do("POST", "/api/plugins/admin.demo/danger", map[string]any{}); code != 403 || errCode(out) != "reauth_required" {
		t.Fatalf("without reauth: %d %v", code, out)
	}
	admin.must(200, "POST", "/api/v1/me/reauth", map[string]any{"method": "password", "password": "correct-horse-1"})
	if code, _ := admin.do("POST", "/api/plugins/admin.demo/danger", map[string]any{}); code != 200 {
		t.Fatalf("after reauth: %d", code)
	}

	bob := admin.must(201, "POST", "/api/v1/users", map[string]any{"username": "bob", "role": "member", "password": "bobs-password-1"})["id"].(string)
	rt.deleteErr = errors.New("the provider did not cancel subscription s1")
	if code, out := admin.do("DELETE", "/api/v1/users/"+bob, nil); code != 409 || !strings.Contains(fmt.Sprint(out), "s1") {
		t.Fatalf("delete refused by a plugin: %d %v", code, out)
	}
	if _, err := e.st.GetUser(context.Background(), bob); err != nil {
		t.Fatalf("refused deletion deleted: %v", err)
	}
	rt.deleteErr = nil
	admin.must(204, "DELETE", "/api/v1/users/"+bob, nil)
	if len(rt.deleted) != 1 || rt.deleted[0] != bob {
		t.Fatalf("hook ran for %v", rt.deleted)
	}

	rt.settingsErr = errors.New("plans have groups")
	if code, out := admin.do("PATCH", "/api/v1/features/limits", map[string]any{"settings": map[string]any{"middleware.ratelimit": map[string]any{"rpm": 60}}}); code != 409 ||
		!strings.Contains(fmt.Sprint(out), "plans have groups") {
		t.Fatalf("settings refused by a plugin: %d %v", code, out)
	}
}
