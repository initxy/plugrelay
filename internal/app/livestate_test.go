package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

type liveCred struct {
	ID    string `json:"id"`
	State struct {
		Status   string     `json:"status"`
		Reason   string     `json:"reason"`
		Until    *time.Time `json:"until"`
		InFlight int        `json:"in_flight"`
	} `json:"state"`
}

type liveUpstream struct {
	ID      string `json:"id"`
	Circuit struct {
		State string     `json:"state"`
		Until *time.Time `json:"until"`
	} `json:"circuit"`
	Credentials []liveCred `json:"credentials"`
}

// consoleStatus is the console's name for a router credential status.
var consoleStatus = map[router.CredStatus]string{
	router.StatusActive: "ok", router.StatusCooling: "cooldown", router.StatusRefreshing: "refreshing",
	router.StatusQuarantined: "quarantined", router.StatusManual: "manual", router.StatusDisabled: "disabled",
}

// TestConsoleShowsRouterLiveState pins hard rule 25: after the router puts
// credentials into cooldown and quarantine and opens an upstream's breaker,
// the admin API reports exactly router.Live, while the stored rows still say
// active.
func TestConsoleShowsRouterLiveState(t *testing.T) {
	const token = "admin-test-token"
	t.Setenv(EnvAdminToken, token)
	ctx := context.Background()
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeAll)
	st := a.Store()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.CreateUser(ctx, &store.User{Name: "admin", Email: "admin", Role: store.RoleAdmin}, "correct-horse-1"))
	for _, up := range []string{"up-a", "up-b"} {
		must(st.PutUpstream(ctx, &store.Upstream{ID: up, Provider: "provider.openai-compatible", Enabled: true,
			Config: map[string]any{"manifest": "vllm", "base_url": "http://127.0.0.1:1/v1", configAllowPrivate: true}}))
	}
	for _, c := range []struct{ id, up string }{{"cool", "up-a"}, {"quar", "up-a"}, {"fine", "up-a"}, {"behind-breaker", "up-b"}} {
		must(st.PutCredential(ctx, &store.Credential{ID: c.id, UpstreamID: c.up, Kind: store.CredShared, Secret: "sk-x", Status: store.CredActive}))
	}
	must(st.PutModel(ctx, &store.Model{Name: "m", Enabled: true}))
	must(st.ReplaceRoutes(ctx, "m", []store.Route{{Model: "m", UpstreamID: "up-a", UpstreamModel: "m", Weight: 1},
		{Model: "m", UpstreamID: "up-b", UpstreamModel: "m", Weight: 1}}))
	must(a.Reload(ctx))

	cand := func(up, cred string) plugin.Candidate { return plugin.Candidate{UpstreamID: up, CredentialID: cred} }
	coolUntil := time.Now().Add(10 * time.Minute).Truncate(time.Millisecond)
	must(a.router.Report(ctx, cand("up-a", "cool"), plugin.Verdict{Cause: plugin.CauseRateLimit, Credential: plugin.CredCooldown, Until: coolUntil}, "m"))
	must(a.router.Report(ctx, cand("up-a", "quar"), plugin.Verdict{Cause: plugin.CauseBanned, Credential: plugin.CredQuarantine}, "m"))
	for range 5 {
		must(a.router.Report(ctx, cand("up-b", "behind-breaker"), plugin.Verdict{Cause: plugin.CauseTransient}, "m"))
	}

	srv := httptest.NewServer(a.Handler().Handler())
	t.Cleanup(srv.Close)
	get := func(path string, out any) {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		must(err)
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %d", path, resp.StatusCode)
		}
		must(json.NewDecoder(resp.Body).Decode(out))
	}
	var ups struct{ Items []liveUpstream }
	get("/api/v1/upstreams", &ups)
	shown := map[string]liveUpstream{}
	for _, u := range ups.Items {
		shown[u.ID] = u
	}

	live := a.router.Live(ctx)
	if len(live.Upstreams) != 2 {
		t.Fatalf("router live has %d upstreams", len(live.Upstreams))
	}
	sameTime := func(a *time.Time, b time.Time) bool { return (a == nil && b.IsZero()) || (a != nil && a.Equal(b)) }
	got := map[string]string{} // credential id -> console status
	for _, ul := range live.Upstreams {
		u, ok := shown[ul.ID]
		if !ok {
			t.Fatalf("upstream %s missing from the console", ul.ID)
		}
		if u.Circuit.State != ul.Breaker || !sameTime(u.Circuit.Until, ul.BreakerOpen) {
			t.Errorf("%s circuit = %+v, router %s until %v", ul.ID, u.Circuit, ul.Breaker, ul.BreakerOpen)
		}
		var listed struct{ Items []liveCred }
		get("/api/v1/upstreams/"+ul.ID+"/credentials", &listed)
		for _, cl := range ul.Credentials {
			var one liveCred
			get("/api/v1/credentials/"+cl.ID, &one)
			views := []liveCred{one}
			for _, src := range [][]liveCred{u.Credentials, listed.Items} {
				for _, c := range src {
					if c.ID == cl.ID {
						views = append(views, c)
					}
				}
			}
			if len(views) != 3 {
				t.Fatalf("credential %s shown %d times, want once per endpoint", cl.ID, len(views)-1)
			}
			want, until, reason := consoleStatus[cl.Status], cl.Until, cl.Reason
			if cl.Status == router.StatusActive && ul.Breaker == router.BreakerOpen {
				want, until, reason = "circuit_open", ul.BreakerOpen, "upstream circuit breaker is open"
			}
			for _, v := range views {
				if v.State.Status != want || v.State.Reason != reason || !sameTime(v.State.Until, until) || v.State.InFlight != cl.InFlight {
					t.Errorf("credential %s shown as %+v, router live %+v (breaker %s)", cl.ID, v.State, cl, ul.Breaker)
				}
			}
			got[cl.ID] = views[0].State.Status
		}
	}

	// The scenario really produced each state.
	for id, want := range map[string]string{"cool": "cooldown", "quar": "quarantined", "fine": "ok", "behind-breaker": "circuit_open"} {
		if got[id] != want {
			t.Errorf("credential %s = %q, want %q", id, got[id], want)
		}
	}
	// Cooldowns and breakers live only in router state (quarantine is also
	// persisted, so it survives losing that state): the console cannot have
	// read them from the stored rows.
	for _, id := range []string{"cool", "behind-breaker"} {
		c, err := st.GetCredential(ctx, id)
		must(err)
		if c.Status != store.CredActive {
			t.Errorf("stored status of %s = %q, want it untouched", id, c.Status)
		}
	}
	if got := shown["up-b"].Circuit.State; got != router.BreakerOpen {
		t.Errorf("up-b circuit = %q, want open", got)
	}
}

// TestConsoleCreatesUpstreamWithManifestVariables: the console reads a
// manifest's variables from /manifests and sets them in config, like the
// CLI's upstream add --var.
func TestConsoleCreatesUpstreamWithManifestVariables(t *testing.T) {
	const token = "admin-test-token"
	t.Setenv(EnvAdminToken, token)
	ctx := context.Background()
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeAll)
	if err := a.Store().CreateUser(ctx, &store.User{Name: "admin", Email: "admin", Role: store.RoleAdmin}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler().Handler())
	t.Cleanup(srv.Close)
	call := func(method, path string, body any, out any) int {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			_ = json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}
	type variable struct {
		Name, Default string
		Required      bool
	}
	var ms struct {
		Items []struct {
			Name      string
			Variables []variable
		}
	}
	if code := call("GET", "/api/v1/manifests", nil, &ms); code != 200 {
		t.Fatalf("GET /manifests: %d", code)
	}
	vars := map[string][]variable{}
	for _, m := range ms.Items {
		vars[m.Name] = m.Variables
	}
	if got := vars["azure-ai-foundry"]; len(got) != 1 || got[0] != (variable{Name: "resource", Required: true}) {
		t.Fatalf("azure-ai-foundry variables = %+v", got)
	}
	if got := vars["mistral"]; len(got) != 1 || got[0] != (variable{Name: "host", Default: "api.mistral.ai"}) {
		t.Fatalf("mistral variables = %+v", got)
	}
	if vars["deepseek"] == nil {
		t.Fatal("a manifest without variables lists null instead of []")
	}

	var bad struct {
		Error struct{ Fields []struct{ Path string } }
	}
	if code := call("POST", "/api/v1/upstreams", map[string]any{"name": "foundry", "manifest": "azure-ai-foundry"}, &bad); code != 422 ||
		len(bad.Error.Fields) == 0 || bad.Error.Fields[0].Path != "config" {
		t.Fatalf("missing required variable: %d %+v", code, bad)
	}
	var up struct {
		ID     string
		Config map[string]any
	}
	if code := call("POST", "/api/v1/upstreams", map[string]any{"name": "foundry", "manifest": "azure-ai-foundry",
		"config": map[string]any{"resource": "acme-eu"}}, &up); code != 201 || up.Config["resource"] != "acme-eu" {
		t.Fatalf("create with variable: %d %+v", code, up)
	}
	u, err := a.Store().GetUpstream(ctx, up.ID)
	if err != nil || u.Config["resource"] != "acme-eu" || u.Manifest != "azure-ai-foundry" {
		t.Fatalf("stored upstream = %+v, %v", u, err)
	}
}
