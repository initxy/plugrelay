package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/settings"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// fakeRuntime stands in for the running relay.
type fakeRuntime struct {
	restored  []string
	checkErr  error
	reloadErr string
	// deleteErr and settingsErr are what the plugins' user-delete and
	// settings checks answer.
	deleteErr, settingsErr error
	deleted                []string
	notifyOff              bool
	// moduleSettings are what SettingsFor answers, by module and key
	// (JSON values).
	moduleSettings map[string]map[string]string
}

func (f *fakeRuntime) BeforeUserDelete(_ context.Context, userID string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, userID)
	return nil
}

func (f *fakeRuntime) CheckSettings(context.Context, string, plugin.Scope, map[string]json.RawMessage) error {
	return f.settingsErr
}

func (f *fakeRuntime) CheckPlugin(context.Context, *store.PluginConfig) error { return f.checkErr }
func (f *fakeRuntime) CheckFeature(context.Context, store.FeatureState) error { return f.checkErr }
func (f *fakeRuntime) ReloadError() string                                    { return f.reloadErr }

func (f *fakeRuntime) Live(context.Context) router.Live {
	return router.Live{Upstreams: []router.UpstreamLive{{ID: "up1", Enabled: true, Breaker: router.BreakerClosed}}}
}

func (f *fakeRuntime) RestoreCredential(_ context.Context, up, cred string) error {
	f.restored = append(f.restored, up+"/"+cred)
	return nil
}

func (f *fakeRuntime) Plugins() []PluginEntry {
	return []PluginEntry{{Info: plugin.Info{ID: "middleware.ratelimit", Kind: plugin.KindMiddleware, Version: "1.0.0",
		Settings: []plugin.Setting{{Key: "rpm", Type: plugin.TypeInt, Scopes: plugin.Global | plugin.Group | plugin.Key, Merge: plugin.Strictest},
			{Key: "subject", Type: plugin.TypeString, Scopes: plugin.Global, Merge: plugin.Override}}},
		Source: SourceBuiltin, Enabled: true, Order: 10},
		{Info: plugin.Info{ID: "selector.routing", Kind: plugin.KindSelector, Version: "1.0.0",
			Settings: []plugin.Setting{{Key: "strategy", Type: plugin.TypeString, Scopes: plugin.Global | plugin.Group, Merge: plugin.Override,
				Enum: []string{"priority_weight", "cheapest"}}}},
			Source: SourceBuiltin, Enabled: true, Order: 20},
		{Info: plugin.Info{ID: "meter.quota", Kind: plugin.KindMeter, Version: "1.0.0",
			Settings: []plugin.Setting{{Key: "key_total_micro", Type: plugin.TypeInt, Scopes: plugin.Global | plugin.Key,
				Merge: plugin.Strictest, Unit: plugin.UnitMoney},
				{Key: "key_daily_micro", Type: plugin.TypeInt, Scopes: plugin.Global | plugin.Key, Merge: plugin.Strictest, Unit: plugin.UnitMoney}}},
			Source: SourceBuiltin, Enabled: true, Order: 30},
		{Info: plugin.Info{ID: "sink.vendor", Kind: plugin.KindSink, Version: "0.1.0",
			Settings: []plugin.Setting{{Key: "endpoint", Type: plugin.TypeString, Scopes: plugin.Global, Merge: plugin.Override}}},
			Source: SourceExternal, Order: 1000}}
}

func (f *fakeRuntime) SettingsFor(module string, _ plugin.Caller) plugin.Settings {
	return mapSettings(f.moduleSettings[module])
}

type mapSettings map[string]string

func (m mapSettings) Get(key string) plugin.Value { return plugin.NewValue(json.RawMessage(m[key])) }

func (f *fakeRuntime) Manifests(context.Context) []ManifestInfo {
	return []ManifestInfo{{Name: "vllm", Title: "vLLM", Kind: "official", Protocols: []string{"openai-chat"}, Provider: "provider.openai-compatible"}}
}

func (f *fakeRuntime) Endpoints(u *store.Upstream) ([]plugin.Endpoint, error) {
	if b, _ := u.Config["base_url"].(string); b == "bad" {
		return nil, fmt.Errorf("base URL %q is not http(s)", b)
	}
	return []plugin.Endpoint{{ID: "chat", Protocol: "openai-chat", BaseURL: "http://x/v1"}}, nil
}

func (f *fakeRuntime) TestUpstream(_ context.Context, id string, req TestRequest) []TestResult {
	return []TestResult{{OK: true, UpstreamID: id, Model: req.Model, Status: 200}}
}

func (f *fakeRuntime) TestKey(_ context.Context, k *store.Key, proto, model string) TestResult {
	return TestResult{OK: true, Model: model + "@" + proto}
}

func (f *fakeRuntime) Playground(_ context.Context, caller plugin.Caller, req PlaygroundRequest, emit func(PlaygroundEvent)) {
	emit(PlaygroundEvent{Type: "start", RequestID: "r-play"})
	emit(PlaygroundEvent{Type: "text", Text: caller.KeyName + ":" + req.Model})
	emit(PlaygroundEvent{Type: "done", OK: true, Status: 200, RequestID: "r-play"})
}

func (f *fakeRuntime) RemoteModels(context.Context, string) ([]ir.ModelInfo, error) {
	return []ir.ModelInfo{{ID: "m1"}}, nil
}

func (f *fakeRuntime) SyncModels(context.Context, []string) SyncResult {
	return SyncResult{Added: []string{"m1"}}
}

func (f *fakeRuntime) TestNotifyChannel(_ context.Context, c *store.NotifyChannel) (NotifyTest, bool) {
	if f.notifyOff {
		return NotifyTest{}, false
	}
	return NotifyTest{OK: c.Secrets.URL != "", Attempts: 1}, true
}

type opener func(t *testing.T, box *secret.Box) store.Store

func sqliteStore(t *testing.T, box *secret.Box) store.Store {
	s, err := store.Open(context.Background(), store.Config{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "a.db"), Secrets: box})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func pgStore(dsn string) opener {
	return func(t *testing.T, box *secret.Box) store.Store {
		ctx := context.Background()
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		schema := fmt.Sprintf("admin_test_%d", time.Now().UnixNano())
		if _, err := db.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		s, err := store.Open(ctx, store.Config{Driver: "postgres", DSN: dsn + sep + "search_path=" + schema, Secrets: box})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			s.Close()
			_, _ = db.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE")
			db.Close()
		})
		return s
	}
}

func mysqlStore(dsn string) opener {
	return func(t *testing.T, box *secret.Box) store.Store {
		ctx := context.Background()
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.DBName = fmt.Sprintf("admin_test_%d", time.Now().UnixNano())
		if _, err := db.ExecContext(ctx, "CREATE DATABASE "+cfg.DBName); err != nil {
			t.Fatal(err)
		}
		s, err := store.Open(ctx, store.Config{Driver: "mysql", DSN: cfg.FormatDSN(), Secrets: box})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			s.Close()
			_, _ = db.ExecContext(ctx, "DROP DATABASE "+cfg.DBName)
			db.Close()
		})
		return s
	}
}

const adminToken = "operator-token-0123456789"

type env struct {
	t   *testing.T
	h   *Handler
	st  store.Store
	srv *httptest.Server
	rt  *fakeRuntime
}

// newEnv starts the admin API over a fresh store; opts adjust its Deps.
// localOwner false turns the local-owner rule off, as on a shared relay.
func newEnv(t *testing.T, open opener, localOwner bool, opts ...func(*Deps)) *env {
	k, _ := secret.GenerateMasterKey()
	mk, _ := secret.ParseMasterKey(k)
	box, _ := secret.NewBox(mk)
	st := open(t, box)
	cfg := config.Defaults()
	rt := &fakeRuntime{}
	d := Deps{Store: st, Box: box, Config: &cfg, Runtime: rt, Version: "test", AdminToken: adminToken,
		Logger: slog.New(slog.DiscardHandler), Listen: func() (string, bool) { return "127.0.0.1:8787", true }}
	for _, o := range opts {
		o(&d)
	}
	if !localOwner {
		if err := saveSite(context.Background(), st, Site{SettingLocalOwner: json.RawMessage("false")}); err != nil {
			t.Fatal(err)
		}
	}
	h := New(d)
	mux := http.NewServeMux()
	mux.Handle("/api/", h.API())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{t: t, h: h, st: st, srv: srv, rt: rt}
}

// setSite stores one site setting.
func (e *env) setSite(key string, v any) {
	e.t.Helper()
	s, err := LoadSite(context.Background(), e.st)
	if err != nil {
		e.t.Fatal(err)
	}
	if v == nil {
		delete(s, key)
	} else {
		s[key], _ = json.Marshal(v)
	}
	if err := saveSite(context.Background(), e.st, s); err != nil {
		e.t.Fatal(err)
	}
}

// listenPublic makes the relay listen beyond loopback.
func (e *env) listenPublic() {
	e.h.d.Listen = func() (string, bool) { return "0.0.0.0:8787", false }
}

// client is a browser-like session.
type client struct {
	e    *env
	http *http.Client
	csrf bool
	auth string
	// header is added to every request.
	header http.Header
}

func (e *env) browser() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, http: &http.Client{Jar: jar}, csrf: true}
}

func (c *client) csrfToken() string {
	u, _ := url.Parse(c.e.srv.URL)
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == CSRFCookie {
			return ck.Value
		}
	}
	return ""
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, c.e.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if c.csrf && method != "GET" {
		req.Header.Set(CSRFHeader, c.csrfToken())
	}
	if c.auth != "" {
		req.Header.Set("Authorization", "Bearer "+c.auth)
	}
	for k, vs := range c.header {
		req.Header[k] = vs
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (c *client) must(want int, method, path string, body any) map[string]any {
	c.e.t.Helper()
	code, out := c.do(method, path, body)
	if code != want {
		c.e.t.Fatalf("%s %s: status %d, want %d: %v", method, path, code, want, out)
	}
	return out
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func (e *env) setup() *client {
	c := e.browser()
	c.must(200, "POST", "/api/v1/setup", map[string]any{"admin_username": "admin", "admin_password": "correct-horse-1",
		"usage": "team", "listen_public": false, "currency": "CNY"})
	return c
}

func stores(t *testing.T) map[string]opener {
	m := map[string]opener{"sqlite": sqliteStore}
	if dsn := os.Getenv("PLUGRELAY_TEST_PG"); dsn != "" {
		m["postgres"] = pgStore(dsn)
	}
	if dsn := os.Getenv("PLUGRELAY_TEST_MYSQL"); dsn != "" {
		m["mysql"] = mysqlStore(dsn)
	}
	return m
}

func TestAdmin(t *testing.T) {
	for name, open := range stores(t) {
		t.Run(name, func(t *testing.T) {
			t.Run("Setup", func(t *testing.T) { testSetup(t, open) })
			t.Run("AuthAndCSRF", func(t *testing.T) { testAuthAndCSRF(t, open) })
			t.Run("Roles", func(t *testing.T) { testRoles(t, open) })
			t.Run("Resources", func(t *testing.T) { testResources(t, open) })
			t.Run("LogsAndAudit", func(t *testing.T) { testLogsAndAudit(t, open) })
			t.Run("LocalOwner", func(t *testing.T) { testLocalOwner(t, open) })
		})
	}
}

func testSetup(t *testing.T, open opener) {
	e := newEnv(t, open, false)
	anon := e.browser()
	out := anon.must(200, "GET", "/api/v1/setup/status", nil)
	if out["needs_setup"] != true {
		t.Fatalf("status: %v", out)
	}
	if code, out := anon.do("GET", "/api/v1/keys", nil); code != 403 || errCode(out) != "setup_required" {
		t.Fatalf("before setup: %d %v", code, out)
	}
	code, out := anon.do("POST", "/api/v1/setup", map[string]any{"admin_username": "", "admin_password": "short", "usage": "x"})
	if code != 422 || len(out["error"].(map[string]any)["fields"].([]any)) < 3 {
		t.Fatalf("invalid setup: %d %v", code, out)
	}
	c := e.setup()
	me := c.must(200, "GET", "/api/v1/me", nil)
	if me["auth"] != "session" || me["site"].(map[string]any)["currency"] != "CNY" {
		t.Fatalf("me: %v", me)
	}
	if code, _ := anon.do("POST", "/api/v1/setup", map[string]any{"admin_username": "x", "admin_password": "correct-horse-1",
		"usage": "team", "listen_public": true}); code != 409 {
		t.Fatalf("second setup: %d", code)
	}
	if out := anon.must(200, "GET", "/api/v1/setup/status", nil); out["needs_setup"] != false {
		t.Fatalf("status after: %v", out)
	}
	site, _ := LoadSite(context.Background(), e.st)
	if pub, set := site.Bool(SettingListenPublic); !set || pub {
		t.Fatalf("listen_public stored: %v %v", pub, set)
	}
	if v := site.String(SettingPublicURL); v != "" {
		t.Fatalf("public url without one given: %q", v)
	}

	// The public URL the wizard asks for lands in the site settings; a bad one is refused.
	e2 := newEnv(t, open, false)
	b := e2.browser()
	if code, out := b.do("POST", "/api/v1/setup", map[string]any{"admin_username": "admin", "admin_password": "correct-horse-1",
		"usage": "team", "listen_public": true, "public_url": "relay.example.com"}); code != 422 {
		t.Fatalf("relative public url: %d %v", code, out)
	}
	b.must(200, "POST", "/api/v1/setup", map[string]any{"admin_username": "admin", "admin_password": "correct-horse-1",
		"usage": "team", "listen_public": true, "public_url": " https://relay.example.com/ "})
	site2, _ := LoadSite(context.Background(), e2.st)
	if v := site2.String(SettingPublicURL); v != "https://relay.example.com" {
		t.Fatalf("public url stored: %q", v)
	}
}

func testAuthAndCSRF(t *testing.T, open opener) {
	e := newEnv(t, open, false)
	c := e.setup()
	c.must(204, "POST", "/api/v1/auth/logout", nil)
	if code, _ := c.do("GET", "/api/v1/me", nil); code != 401 {
		t.Fatalf("after logout: %d", code)
	}
	if code, _ := c.do("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "wrong-password"}); code != 401 {
		t.Fatalf("bad login: %d", code)
	}
	// Login CSRF: a cross-site form posts text/plain or carries a foreign Origin.
	good := map[string]any{"username": "admin", "password": "correct-horse-1"}
	for _, h := range []http.Header{{"Content-Type": {"text/plain"}}, {"Origin": {"https://evil.example"}}} {
		c.header = h
		if code, _ := c.do("POST", "/api/v1/auth/login", good); code != 403 {
			t.Fatalf("login with %v: %d", h, code)
		}
	}
	c.header = nil
	c.must(200, "POST", "/api/v1/auth/login", good)

	c.csrf = false
	if code, out := c.do("POST", "/api/v1/keys", map[string]any{"name": "k"}); code != 403 || errCode(out) != "csrf" {
		t.Fatalf("no csrf header: %d %v", code, out)
	}
	c.csrf = true
	c.must(201, "POST", "/api/v1/keys", map[string]any{"name": "k"})

	bearer := &client{e: e, http: http.DefaultClient, auth: adminToken}
	if out := bearer.must(200, "GET", "/api/v1/me", nil); out["auth"] != "bearer" {
		t.Fatalf("bearer me: %v", out)
	}
	bearer.must(201, "POST", "/api/v1/groups", map[string]any{"name": "g"})
	if code, _ := (&client{e: e, http: http.DefaultClient, auth: "pr_platformkey"}).do("GET", "/api/v1/me", nil); code != 401 {
		t.Fatalf("platform key accepted: %d", code)
	}
	if !e.h.IsAdminToken(httptest.NewRequest("GET", "/metrics", nil)) == false {
		t.Fatal("metrics without token allowed")
	}

	// A token set in site settings works too; the stored value is a hash.
	c.must(200, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"security.admin_token": "site-token-abcdefghijkl"}})
	(&client{e: e, http: http.DefaultClient, auth: "site-token-abcdefghijkl"}).must(200, "GET", "/api/v1/features", nil)
	if code, _ := c.do("PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"listen": "0.0.0.0:1"}}); code != 409 {
		t.Fatalf("file setting change: %d", code)
	}

	// Login throttling.
	for range loginMaxFails {
		e.browser().do("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "nope-nope-nope"})
	}
	if code, _ := e.browser().do("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "correct-horse-1"}); code != 429 {
		t.Fatalf("throttle: %d", code)
	}
}

func testRoles(t *testing.T, open opener) {
	e := newEnv(t, open, false)
	admin := e.setup()
	admin.must(201, "POST", "/api/v1/users", map[string]any{"username": "bob", "role": "member", "password": "bobs-password-1"})
	adminKey := admin.must(201, "POST", "/api/v1/keys", map[string]any{"name": "admin-key"})["key"].(map[string]any)["id"].(string)

	bob := e.browser()
	bob.must(200, "POST", "/api/v1/auth/login", map[string]any{"username": "bob", "password": "bobs-password-1"})
	if out := bob.must(200, "GET", "/api/v1/me", nil); len(out["permissions"].([]any)) != 0 {
		t.Fatalf("member permissions: %v", out)
	}
	for _, p := range []string{"/api/v1/users", "/api/v1/upstreams", "/api/v1/settings", "/api/v1/audit", "/api/v1/features", "/api/v1/extensions"} {
		if code, _ := bob.do("GET", p, nil); code != 403 {
			t.Errorf("member GET %s: %d", p, code)
		}
	}
	created := bob.must(201, "POST", "/api/v1/keys", map[string]any{"name": "bob-key"})
	if created["secret"] == "" || !strings.HasPrefix(created["secret"].(string), "pr_") {
		t.Fatalf("secret: %v", created)
	}
	if items := bob.must(200, "GET", "/api/v1/keys", nil)["items"].([]any); len(items) != 1 {
		t.Fatalf("member sees %d keys", len(items))
	}
	if code, _ := bob.do("GET", "/api/v1/keys/"+adminKey, nil); code != 404 {
		t.Fatalf("member reads admin key: %d", code)
	}
	if code, _ := bob.do("POST", "/api/v1/keys", map[string]any{"name": "x", "settings": map[string]any{"exception": true}}); code != 422 {
		t.Fatalf("member sets exception: %d", code)
	}
	if items := admin.must(200, "GET", "/api/v1/keys", nil)["items"].([]any); len(items) != 2 {
		t.Fatalf("admin sees %d keys", len(items))
	}
}

func testResources(t *testing.T, open opener) {
	e := newEnv(t, open, false)
	c := e.setup()

	// Keys: create with settings, get, connect, patch, test, rotate, delete.
	code, out := c.do("POST", "/api/v1/keys", map[string]any{"name": "k", "settings": map[string]any{"ip_allowlist": []string{"nope"}}})
	if code != 422 || out["error"].(map[string]any)["fields"].([]any)[0].(map[string]any)["path"] != "settings.ip_allowlist.0" {
		t.Fatalf("key validation: %d %v", code, out)
	}
	// Allowed clients are the request log's client ids; "other" is not one.
	code, out = c.do("POST", "/api/v1/keys", map[string]any{"name": "k", "settings": map[string]any{"clients": []string{"roo", "other"}}})
	if code != 422 || out["error"].(map[string]any)["fields"].([]any)[0].(map[string]any)["path"] != "settings.clients.1" {
		t.Fatalf("client validation: %d %v", code, out)
	}
	roo := c.must(201, "POST", "/api/v1/keys", map[string]any{"name": "roo", "settings": map[string]any{"clients": []string{"roo", "kilo", "sdk-python"}}})
	if got := roo["key"].(map[string]any)["settings"].(map[string]any)["clients"].([]any); len(got) != 3 {
		t.Fatalf("clients: %v", got)
	}
	c.must(204, "DELETE", "/api/v1/keys/"+roo["key"].(map[string]any)["id"].(string), nil)
	kc := c.must(201, "POST", "/api/v1/keys", map[string]any{"name": "laptop",
		"settings": map[string]any{"models": []string{"gpt-*"}, "rpm": 30, "budget_monthly": map[string]any{"micro": 5_000_000, "currency": "CNY"}}})
	key := kc["key"].(map[string]any)
	kid := key["id"].(string)
	if key["status"] != "active" || key["month_spend"].(map[string]any)["currency"] != "CNY" {
		t.Fatalf("key: %v", key)
	}
	rows, _ := e.st.ListSettings(context.Background(), store.SettingFilter{Scope: plugin.Key, ScopeID: kid})
	if len(rows) != 0 {
		t.Fatalf("key limits copied into plugin settings: %+v", rows)
	}
	conn := c.must(200, "GET", "/api/v1/keys/"+kid+"/connect?os=linux", nil)
	if conn["placeholder"] != "{{api_key}}" || len(conn["clients"].([]any)) != 7 {
		t.Fatalf("connect: %v", conn)
	}
	first := conn["clients"].([]any)[0].(map[string]any)
	if !strings.HasPrefix(first["ccswitch_url"].(string), "ccswitch://") || !strings.Contains(first["ccswitch_url"].(string), "{{api_key}}") {
		t.Fatalf("ccswitch: %v", first["ccswitch_url"])
	}
	for _, cl := range conn["clients"].([]any) {
		if cl := cl.(map[string]any); cl["client"] == "codex" && (!strings.Contains(fmt.Sprint(cl["notes"]), "at least one model") || !strings.Contains(fmt.Sprint(cl["notes"]), "至少要能用一个模型")) {
			t.Fatalf("codex notes: %v", cl["notes"])
		}
	}
	c.must(200, "PATCH", "/api/v1/keys/"+kid, map[string]any{"enabled": false})
	if out := c.must(200, "GET", "/api/v1/keys/"+kid, nil); out["status"] != "disabled" {
		t.Fatalf("disabled key: %v", out)
	}
	if out := c.must(200, "POST", "/api/v1/keys/"+kid+"/test", map[string]any{"client": "codex", "model": "m"}); out["ok"] != true || out["results"].([]any)[0].(map[string]any)["model"] != "m@openai-responses" {
		t.Fatalf("key test: %v", out)
	}
	rot := c.must(200, "POST", "/api/v1/keys/"+kid+"/rotate", nil)
	nid := rot["key"].(map[string]any)["id"].(string)
	if nid == kid || rot["secret"] == kc["secret"] {
		t.Fatalf("rotate: %v", rot)
	}
	if code, _ := c.do("GET", "/api/v1/keys/"+kid, nil); code != 404 {
		t.Fatalf("old key survives rotation: %d", code)
	}
	c.must(204, "DELETE", "/api/v1/keys/"+nid, nil)

	// Users and groups.
	g := c.must(201, "POST", "/api/v1/groups", map[string]any{"name": "devs", "models": []string{"gpt-*"}, "log_bodies": true})
	gid := g["id"].(string)
	if g["price_multiplier"] != float64(1) {
		t.Fatalf("default multiplier: %v", g)
	}
	if code, out := c.do("PATCH", "/api/v1/groups/"+gid, map[string]any{"price_multiplier": 0}); code != 422 || !strings.Contains(fmt.Sprint(out), "price_multiplier") {
		t.Fatalf("zero multiplier: %d %v", code, out)
	}
	if out := c.must(200, "PATCH", "/api/v1/groups/"+gid, map[string]any{"price_multiplier": 1.25}); out["price_multiplier"] != 1.25 {
		t.Fatalf("multiplier: %v", out)
	}
	if sg, _ := e.st.GetGroup(context.Background(), gid); sg.PriceMultiplier != 1.25 {
		t.Fatalf("stored multiplier %v", sg.PriceMultiplier)
	}
	u := c.must(201, "POST", "/api/v1/users", map[string]any{"username": "eve@example.com", "role": "user", "group_id": gid})
	if u["email"] != "eve@example.com" || u["group"].(map[string]any)["name"] != "devs" {
		t.Fatalf("user: %v", u)
	}
	if code, _ := c.do("DELETE", "/api/v1/groups/"+gid, nil); code != 409 {
		t.Fatalf("delete group with members: %d", code)
	}
	c.must(200, "PATCH", "/api/v1/users/"+u["id"].(string), map[string]any{"status": "disabled"})
	c.must(204, "DELETE", "/api/v1/users/"+u["id"].(string), nil)
	c.must(204, "DELETE", "/api/v1/groups/"+gid, nil)
	me := c.must(200, "GET", "/api/v1/me", nil)["user"].(map[string]any)
	if code, _ := c.do("DELETE", "/api/v1/users/"+me["id"].(string), nil); code != 409 {
		t.Fatalf("delete last admin: %d", code)
	}

	// Upstreams, credentials, models, pricing.
	if code, out := c.do("POST", "/api/v1/upstreams", map[string]any{"name": "Local", "manifest": "vllm", "config": map[string]any{"base_url": "bad"}}); code != 422 || !strings.Contains(fmt.Sprint(out), "config") {
		t.Fatalf("bad upstream config: %d %v", code, out)
	}
	up := c.must(201, "POST", "/api/v1/upstreams", map[string]any{"name": "Local vLLM", "manifest": "vllm",
		"tags": []string{"lab"}})
	upID := up["id"].(string)
	if up["model_sync"] != nil {
		t.Fatalf("model sync set by default: %v", up["model_sync"])
	}
	c.must(201, "POST", "/api/v1/models", map[string]any{"name": "qwen",
		"routes": []map[string]any{{"upstream_id": upID, "upstream_model": "Qwen/Qwen3"}}})
	if out := c.must(200, "PATCH", "/api/v1/upstreams/"+upID, map[string]any{"model_sync": false, "quota_probe": "0"}); out["model_sync"] != false || out["quota_probe"] != "0" {
		t.Fatalf("per-upstream jobs: %v", out)
	}
	for _, bad := range []map[string]any{
		{"headers": map[string]string{"Authorization": "Bearer x"}},
		{"headers": map[string]string{"X-Forwarded-For": "1.2.3.4"}},
		{"proxy": "ftp://p"},
	} {
		if code, _ := c.do("PATCH", "/api/v1/upstreams/"+upID, bad); code != 422 {
			t.Fatalf("PATCH %v: %d", bad, code)
		}
	}
	eg := c.must(200, "PATCH", "/api/v1/upstreams/"+upID, map[string]any{"headers": map[string]string{"X-Team": "ops"},
		"proxy": "socks5://p:1080", "timeouts": map[string]any{"connect_ms": 3000, "total_ms": 1_800_000}})
	if tm := eg["timeouts"].(map[string]any); tm["connect_ms"] != float64(3000) || tm["total_ms"] != float64(1_800_000) || eg["proxy"] != "socks5://p:1080" {
		t.Fatalf("egress overrides: %v", eg)
	}
	if su, _ := e.st.GetUpstream(context.Background(), upID); su != nil {
		h, p, tm := UpstreamEgress(su.Config)
		if h["X-Team"] != "ops" || p != "socks5://p:1080" || tm.Connect != 3*time.Second || tm.Total != 30*time.Minute {
			t.Fatalf("UpstreamEgress: %v %q %+v", h, p, tm)
		}
	}
	if code, _ := c.do("POST", "/api/v1/upstreams/"+upID+"/credentials", map[string]any{"label": "x", "kind": "shared", "proxy": "nope"}); code != 422 {
		t.Fatalf("bad credential proxy: %d", code)
	}
	if upID != "local-vllm" || up["provider"] != "provider.openai-compatible" || up["source"].(map[string]any)["kind"] != "official" ||
		up["circuit"].(map[string]any)["state"] != "closed" {
		t.Fatalf("upstream: %v", up)
	}
	cr := c.must(201, "POST", "/api/v1/upstreams/"+upID+"/credentials", map[string]any{"label": "main", "kind": "shared", "secret": "sk-abcdefghijkl"})
	if cr["secret_hint"] != "sk-••••ijkl" || cr["state"].(map[string]any)["status"] != "ok" {
		t.Fatalf("credential: %v", cr)
	}
	stored, _ := e.st.GetCredential(context.Background(), cr["id"].(string))
	if strings.Contains(stored.Sealed, "abcdefghijkl") || stored.Sealed == "" {
		t.Fatal("secret not sealed")
	}
	c.must(200, "PATCH", "/api/v1/credentials/"+cr["id"].(string), map[string]any{"enabled": false})
	if out := c.must(200, "GET", "/api/v1/credentials/"+cr["id"].(string), nil); out["enabled"] != false || out["state"].(map[string]any)["status"] != "disabled" {
		t.Fatalf("disabled credential: %v", out)
	}
	c.must(200, "POST", "/api/v1/credentials/"+cr["id"].(string)+"/reset", nil)
	if len(e.rt.restored) != 1 {
		t.Fatal("reset did not reach the router")
	}
	if out := c.must(200, "POST", "/api/v1/upstreams/"+upID+"/test", map[string]any{"model": "x"}); len(out["results"].([]any)) != 1 || out["ok"] != true {
		t.Fatalf("test: %v", out)
	}
	c.must(200, "GET", "/api/v1/upstreams/"+upID+"/remote-models", nil)
	c.must(200, "GET", "/api/v1/manifests", nil)
	if items := c.must(200, "GET", "/api/v1/upstreams?q=lab", nil)["items"].([]any); len(items) != 1 {
		t.Fatalf("upstream search: %d", len(items))
	}

	pre := c.must(201, "POST", "/api/v1/presets", map[string]any{"name": "auto", "models": []string{"qwen"}, "strategy": "cheapest"})
	if pre["strategy"] != "cheapest" || len(pre["models"].([]any)) != 1 {
		t.Fatalf("preset: %v", pre)
	}
	for _, bad := range []map[string]any{
		{"name": "broken", "models": []string{"nope"}},
		{"name": "qwen", "models": []string{"qwen"}},
		{"name": "odd", "models": []string{"qwen"}, "strategy": "random"},
		{"name": "none", "models": []string{}},
	} {
		if code, out := c.do("POST", "/api/v1/presets", bad); code != 422 {
			t.Fatalf("bad preset %v: %d %v", bad, code, out)
		}
	}
	if code, _ := c.do("POST", "/api/v1/models", map[string]any{"name": "auto"}); code != 422 {
		t.Fatalf("model named like a preset: %d", code)
	}
	c.must(200, "PATCH", "/api/v1/presets/auto", map[string]any{"strategy": "in_order"})
	if items := c.must(200, "GET", "/api/v1/presets", nil)["items"].([]any); len(items) != 1 || items[0].(map[string]any)["strategy"] != "in_order" {
		t.Fatalf("presets: %v", items)
	}
	m := c.must(200, "PATCH", "/api/v1/models/qwen", map[string]any{"routes": []map[string]any{{"upstream_id": upID, "upstream_model": "Qwen/Qwen3", "priority": 5, "weight": 1}}})
	if len(m["routes"].([]any)) != 1 {
		t.Fatalf("model: %v", m)
	}
	c.must(200, "POST", "/api/v1/models/sync", map[string]any{})
	pr := c.must(201, "POST", "/api/v1/pricing-rules", map[string]any{"match": "qwen*", "prices": map[string]any{"input": 1_000_000, "output": 2_000_000}})
	if pr["prices"].(map[string]any)["currency"] != "CNY" {
		t.Fatalf("price: %v", pr)
	}
	c.must(200, "PATCH", "/api/v1/pricing-rules/"+pr["id"].(string), map[string]any{"prices": map[string]any{"input": 3, "output": 4}})
	// Media unit prices round-trip; unknown units are refused.
	img := c.must(201, "POST", "/api/v1/pricing-rules", map[string]any{"match": "img-*",
		"prices": map[string]any{"units": map[string]any{"image": 40000, "image:1024x1024:hd": 80000, "audio_second": 100}}})
	if u := img["prices"].(map[string]any)["units"].(map[string]any); u["image:1024x1024:hd"] != float64(80000) {
		t.Fatalf("units: %v", img)
	}
	if code, _ := c.do("POST", "/api/v1/pricing-rules", map[string]any{"match": "x", "prices": map[string]any{"units": map[string]any{"per_token": 1}}}); code != 422 {
		t.Fatalf("unknown unit: %d", code)
	}
	c.must(204, "DELETE", "/api/v1/pricing-rules/"+img["id"].(string), nil)
	cat := c.must(200, "GET", "/api/v1/catalog", nil)["items"].([]any)
	if len(cat) != 2 {
		t.Fatalf("catalog: %v", cat)
	}
	c.must(204, "DELETE", "/api/v1/pricing-rules/"+pr["id"].(string), nil)
	c.must(204, "DELETE", "/api/v1/presets/auto", nil)
	c.must(204, "DELETE", "/api/v1/upstreams/"+upID, nil)

	// Features, extensions and layered settings.
	fl := c.must(200, "GET", "/api/v1/features", nil)["items"].([]any)
	limits := fl[0].(map[string]any)
	if limits["id"] != "limits" || limits["enabled"] != true || len(limits["settings"].([]any)) == 0 {
		t.Fatalf("features: %v", fl)
	}
	set := map[string]any{"settings": map[string]any{"middleware.ratelimit": map[string]any{"rpm": "many"}}}
	if code, out := c.do("PATCH", "/api/v1/features/limits", set); code != 422 ||
		out["error"].(map[string]any)["fields"].([]any)[0].(map[string]any)["path"] != "settings.middleware.ratelimit.rpm" {
		t.Fatalf("invalid feature setting: %d %v", code, out)
	}
	set = map[string]any{"settings": map[string]any{"middleware.ratelimit": map[string]any{"rpm": 60}, "meter.quota": map[string]any{"key_total_micro": 1.5}}}
	got := c.must(200, "PATCH", "/api/v1/features/limits", set)
	for _, sv := range got["settings"].([]any) {
		sv := sv.(map[string]any)
		if (sv["key"] == "rpm" && sv["value"] != float64(60)) || (sv["key"] == "key_total_micro" && (sv["value"] != 1.5 || sv["unit"] != "money")) {
			t.Fatalf("saved feature setting: %v", sv)
		}
	}
	if rows, _ := e.st.ListSettings(context.Background(), store.SettingFilter{PluginID: "meter.quota"}); len(rows) != 1 || string(rows[0].Value) != "1500000" {
		t.Fatalf("money stored as micro-units: %v", rows)
	}
	if code, _ := c.do("PATCH", "/api/v1/features/limits", map[string]any{"settings": map[string]any{"selector.routing": map[string]any{"strategy": "cheapest"}}}); code != 422 {
		t.Fatalf("another feature's setting: %d", code)
	}
	c.must(200, "PATCH", "/api/v1/features/limits", map[string]any{"enabled": false})
	if code, _ := c.do("PATCH", "/api/v1/features/routing", map[string]any{"enabled": false}); code != 409 {
		t.Fatalf("always-on feature turned off: %d", code)
	}
	c.must(200, "PATCH", "/api/v1/features/routing", map[string]any{"settings": map[string]any{"selector.routing": map[string]any{"strategy": "cheapest"}}})
	if code, _ := c.do("PATCH", "/api/v1/features/nope", map[string]any{"enabled": true}); code != 404 {
		t.Fatalf("unknown feature: %d", code)
	}
	ext := c.must(200, "GET", "/api/v1/extensions", nil)["items"].([]any)
	if len(ext) != 1 || ext[0].(map[string]any)["id"] != "sink.vendor" {
		t.Fatalf("extensions: %v", ext)
	}
	if got := c.must(200, "PATCH", "/api/v1/extensions/sink.vendor", map[string]any{"enabled": true, "order": 5,
		"settings": map[string]any{"endpoint": "http://x"}}); got["enabled"] != true || got["order"] != float64(5) {
		t.Fatalf("extension: %v", got)
	}
	if code, _ := c.do("PATCH", "/api/v1/extensions/selector.routing", map[string]any{"enabled": false}); code != 404 {
		t.Fatalf("a core module as an extension: %d", code)
	}
	// A group's layer: core modules' settings are there, a feature that is
	// off adds none.
	grp := c.must(201, "POST", "/api/v1/groups", map[string]any{"name": "team"})["id"].(string)
	gs := c.must(200, "PUT", "/api/v1/groups/"+grp+"/settings", map[string]any{"settings": map[string]any{"selector.routing": map[string]any{"strategy": "cheapest"}}})
	if items := gs["items"].([]any); len(items) != 1 || items[0].(map[string]any)["value"] != "cheapest" ||
		items[0].(map[string]any)["feature"] != "routing" {
		t.Fatalf("group settings: %v", gs)
	}
	if code, _ := c.do("PUT", "/api/v1/groups/"+grp+"/settings", map[string]any{"settings": map[string]any{"middleware.ratelimit": map[string]any{"rpm": 5}}}); code != 422 {
		t.Fatalf("setting of a feature that is off: %d", code)
	}
	// A key's own rpm lives on the key record, not in layered settings.
	c.must(200, "PATCH", "/api/v1/features/limits", map[string]any{"enabled": true})
	rk := c.must(201, "POST", "/api/v1/keys", map[string]any{"name": "rk"})["key"].(map[string]any)["id"].(string)
	if code, out := c.do("PUT", "/api/v1/keys/"+rk+"/settings",
		map[string]any{"settings": map[string]any{"middleware.ratelimit": map[string]any{"rpm": 5}}}); code != 422 || !strings.Contains(fmt.Sprint(out), "set on the key itself") {
		t.Fatalf("key-scope rpm: %d %v", code, out)
	}
	ks := c.must(200, "PUT", "/api/v1/keys/"+rk+"/settings", map[string]any{"settings": map[string]any{"meter.quota": map[string]any{"key_daily_micro": 2}}})
	if items := ks["items"].([]any); len(items) != 1 || items[0].(map[string]any)["value"] != float64(2) {
		t.Fatalf("key settings: %v", ks)
	}
	if me := c.must(200, "GET", "/api/v1/me", nil); !slices.Contains(me["features"].([]any), any("limits")) || me["plugins"] != nil {
		t.Fatalf("me features: %v", me)
	}
	set = c.must(200, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"site.name": "Acme", "network.listen_public": true}})
	if set["restart_required"] != true {
		t.Fatalf("settings: %v", set)
	}
	if out := c.must(200, "GET", "/api/v1/me", nil); out["site"].(map[string]any)["name"] != "Acme" {
		t.Fatalf("site name: %v", out)
	}
	for _, p := range []string{"/api/v1/overview?range=24h", "/api/v1/usage/timeseries?metric=tokens&group_by=model",
		"/api/v1/quota", "/api/v1/status", "/api/v1/search?q=lap", "/api/v1/models", "/api/v1/users", "/api/v1/groups"} {
		c.must(200, "GET", p, nil)
	}
	if code, _ := c.do("GET", "/api/v1/overview?range=2y", nil); code != 422 {
		t.Fatalf("bad range: %d", code)
	}
}

func testLogsAndAudit(t *testing.T, open opener) {
	e := newEnv(t, open, false)
	c := e.setup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	var recs []*plugin.Record
	for i := range 5 {
		recs = append(recs, &plugin.Record{ID: fmt.Sprintf("req%02d", i), Time: now.Add(time.Duration(i) * time.Second),
			Caller: plugin.Caller{KeyID: "k1", KeyName: "laptop"}, Protocol: "anthropic", Model: "qwen", Status: 200,
			ChargeMicro: 1500, Usage: ir.Usage{Input: 100, CacheRead: 300, Output: 20},
			Attempts: []plugin.AttemptRecord{{N: 1, UpstreamID: "up1", Tier: plugin.TierConvert, Status: 200, Sent: true}}})
	}
	for i, r := range recs[:4] {
		r.TTFB = time.Duration(i+1) * 100 * time.Millisecond
	}
	recs[0].Body = &plugin.RecordBody{Request: json.RawMessage(`{"model":"qwen"}`)}
	recs[4].Status, recs[4].ErrorKind = 502, ir.ErrUpstream
	recs[4].Attempts = append(recs[4].Attempts, plugin.AttemptRecord{N: 2, UpstreamID: "up1", Status: 502, Cause: plugin.CauseTransient})
	if err := e.st.InsertRecords(ctx, recs); err != nil {
		t.Fatal(err)
	}
	page := c.must(200, "GET", "/api/v1/logs?limit=2", nil)
	items := page["items"].([]any)
	if len(items) != 2 || page["next_cursor"] == nil || items[0].(map[string]any)["id"] != "req04" {
		t.Fatalf("page 1: %v", page)
	}
	page2 := c.must(200, "GET", "/api/v1/logs?limit=2&cursor="+url.QueryEscape(page["next_cursor"].(string)), nil)
	if page2["items"].([]any)[0].(map[string]any)["id"] != "req02" {
		t.Fatalf("page 2: %v", page2)
	}
	errs := c.must(200, "GET", "/api/v1/logs?status=error", nil)["items"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["cause"] != "transient" {
		t.Fatalf("error filter: %v", errs)
	}
	d := c.must(200, "GET", "/api/v1/logs/req04", nil)
	if len(d["attempts_detail"].([]any)) != 2 || d["billing"] == nil {
		t.Fatalf("detail: %v", d)
	}
	// The formula uses the rates recorded on the billed attempt.
	charged := &plugin.Record{ID: "reqcharged", Time: now, Caller: plugin.Caller{KeyID: "k1"}, Protocol: "anthropic", Model: "qwen",
		Status: 200, ChargeMicro: 6663, Usage: ir.Usage{Input: 1000, Output: 333},
		Attempts: []plugin.AttemptRecord{{N: 1, UpstreamID: "up1", Status: 200, Sent: true,
			Charge: &plugin.Charge{Multiplier: 1.25, Source: "model", Rates: plugin.Rates{Input: 2_500_000, Output: 12_500_000}}}}}
	if err := e.st.InsertRecords(ctx, []*plugin.Record{charged}); err != nil {
		t.Fatal(err)
	}
	b := c.must(200, "GET", "/api/v1/logs/reqcharged", nil)["billing"].(map[string]any)
	lines := b["lines"].([]any)
	out := lines[1].(map[string]any)
	if b["multiplier"] != 1.25 || len(lines) != 2 || out["unit_price"].(map[string]any)["micro"] != float64(12_500_000) ||
		out["amount"].(map[string]any)["micro"] != float64(4163) {
		t.Fatalf("billing: %v", b)
	}
	if hits := c.must(200, "GET", "/api/v1/search?q=req03", nil)["items"].([]any); len(hits) == 0 {
		t.Fatal("search by request id")
	}
	if ov := c.must(200, "GET", "/api/v1/overview?range=24h", nil); ov["requests"] != float64(6) {
		t.Fatalf("overview: %v", ov)
	}
	// p95 of first-token times 100..400 ms over the four successful
	// requests that have one is the largest (nearest rank).
	soon := url.QueryEscape(now.Add(time.Minute).Format(time.RFC3339))
	if ov := c.must(200, "GET", "/api/v1/overview?range=24h&to="+soon, nil); ov["ttfb_p95_ms"] != float64(400) ||
		ov["requests_prev"] != float64(0) || ov["ttfb_p95_prev_ms"] != nil {
		t.Fatalf("overview ttfb: %v", ov)
	}
	// Every bucket of the window is there, empty ones zero.
	ts := c.must(200, "GET", "/api/v1/usage/timeseries?range=7d&metric=requests&group_by=none", nil)["series"].([]any)
	if pts := ts[0].(map[string]any)["points"].([]any); len(ts) != 1 || len(pts) < 28 || len(pts) > 30 {
		t.Fatalf("timeseries buckets: %v", ts)
	}
	from := url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339))
	hist := c.must(200, "GET", "/api/v1/logs/histogram?from="+from+"&to="+soon, nil)
	var reqs, failed float64
	for _, p := range hist["points"].([]any) {
		reqs += p.(map[string]any)["requests"].(float64)
		failed += p.(map[string]any)["failed"].(float64)
	}
	if hist["bucket_seconds"] != float64(120) || len(hist["points"].([]any)) < 30 || len(hist["points"].([]any)) > 32 || reqs != 6 || failed != 1 {
		t.Fatalf("histogram: %v", hist)
	}
	if h := c.must(200, "GET", "/api/v1/logs/histogram?retried=true&from="+from+"&to="+soon, nil); fmt.Sprint(h["points"]) == "" ||
		sumField(h["points"].([]any), "requests") != 1 {
		t.Fatalf("histogram retried: %v", h)
	}
	if d := c.must(200, "GET", "/api/v1/logs/req00", nil); d["body"].(map[string]any)["request"].(map[string]any)["model"] != "qwen" ||
		d["has_body"] != true {
		t.Fatalf("body: %v", d)
	}
	c.must(201, "POST", "/api/v1/keys", map[string]any{"name": "a"})
	audit := c.must(200, "GET", "/api/v1/audit?action=key.create", nil)["items"].([]any)
	if len(audit) != 1 || audit[0].(map[string]any)["actor"].(map[string]any)["name"] != "admin" {
		t.Fatalf("audit: %v", audit)
	}
	key := c.must(201, "POST", "/api/v1/keys", map[string]any{"name": "b"})["key"].(map[string]any)
	c.must(200, "PATCH", "/api/v1/keys/"+key["id"].(string), map[string]any{"name": "b2"})
	if got := c.must(200, "GET", "/api/v1/audit?action=key.*,group.*", nil)["items"].([]any); len(got) != 3 {
		t.Fatalf("audit by prefix: %v", got)
	}
	one := c.must(200, "GET", "/api/v1/audit?target_type=key&target_id="+key["id"].(string), nil)["items"].([]any)
	if len(one) != 2 || one[0].(map[string]any)["action"] != "key.update" {
		t.Fatalf("audit by target: %v", one)
	}
	c.must(422, "GET", "/api/v1/audit?target_id=x", nil)
}

func testLocalOwner(t *testing.T, open opener) {
	e := newEnv(t, open, true)
	e.setup().must(200, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"security.local_owner_login": true}})
	anon := e.browser()
	out := anon.must(200, "GET", "/api/v1/me", nil)
	if out["auth"] != "none" || out["user"].(map[string]any)["role"] != "admin" {
		t.Fatalf("local me: %v", out)
	}
	// The CSRF cookie issued on the first call protects writes.
	anon.must(201, "POST", "/api/v1/keys", map[string]any{"name": "local"})
	anon.csrf = false
	if code, _ := anon.do("POST", "/api/v1/keys", map[string]any{"name": "x"}); code != 403 {
		t.Fatalf("local write without csrf: %d", code)
	}
}

func TestPluginAPI(t *testing.T) {
	e := newEnv(t, sqliteStore, false)
	c := e.setup()
	called := 0
	e.h.d.Runtime = pluginRuntime{fakeRuntime: e.rt, hit: &called}
	resp, err := http.Get(e.srv.URL + "/api/plugins/admin.demo/stats")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous plugin call: %d", resp.StatusCode)
	}
	if out := c.must(200, "GET", "/api/plugins/admin.demo/stats", nil); out["value"] != float64(42) {
		t.Fatalf("plugin call: %v", out)
	}
	if called != 1 {
		t.Fatalf("plugin handler calls: %d", called)
	}
}

type pluginRuntime struct {
	*fakeRuntime
	hit *int
}

func (p pluginRuntime) Plugins() []PluginEntry {
	man := &extpluginManifest
	return []PluginEntry{{Info: plugin.Info{ID: "admin.demo", Kind: plugin.KindAdmin, Version: "0.1.0"},
		Source: SourceExternal, Enabled: true, Manifest: man,
		Routes: []plugin.AdminRoute{{Method: "GET", Path: "/stats", Handler: func(w http.ResponseWriter, r *http.Request) {
			*p.hit++
			_, _ = io.WriteString(w, `{"value":42}`)
		}}}}}
}

// TestUpdateFeatureRefusesRequirementConflict: a change the running relay
// could not load (it would fail every reload and the next start) is refused
// with 409 before anything is stored; a failed reload shows on the list.
func TestUpdateFeatureRefusesRequirementConflict(t *testing.T) {
	e := newEnv(t, sqliteStore, false)
	c := e.setup()
	e.rt.checkErr = errors.New("plugin meter.quota requires sink.requestlog, which is not installed")
	code, out := c.do("PATCH", "/api/v1/features/limits", map[string]any{"enabled": false})
	if code != 409 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "meter.quota requires sink.requestlog") {
		t.Fatalf("conflicting change: %d %v", code, out)
	}
	if fs, err := e.st.ListFeatures(context.Background()); err != nil || len(fs) != 0 {
		t.Fatalf("refused change stored: %v %v", fs, err)
	}
	if code, _ := c.do("PATCH", "/api/v1/extensions/sink.vendor", map[string]any{"enabled": true}); code != 409 {
		t.Fatalf("conflicting extension change: %d", code)
	}

	e.rt.checkErr = nil
	if got := c.must(200, "GET", "/api/v1/features", nil); got["reload_error"] != nil {
		t.Fatalf("reload_error without a failed reload: %v", got["reload_error"])
	}
	e.rt.reloadErr = "plugins: boom"
	if got := c.must(200, "GET", "/api/v1/extensions", nil); got["reload_error"] != "plugins: boom" {
		t.Fatalf("reload_error %v", got["reload_error"])
	}
}

// A stored value that does not fit its setting is marked invalid on the
// layer's settings, so the console can say what it does; saving a valid
// value clears the mark.
func TestObjectSettingsMarkInvalidValue(t *testing.T) {
	e := newEnv(t, sqliteStore, false)
	c := e.setup()
	grp := c.must(201, "POST", "/api/v1/groups", map[string]any{"name": "team"})["id"].(string)
	if err := e.st.PutSetting(context.Background(), store.SettingRow{Scope: plugin.Group, ScopeID: grp, PluginID: "selector.routing",
		Key: "strategy", Value: json.RawMessage(`"nope"`)}); err != nil {
		t.Fatal(err)
	}
	strategy := func(out map[string]any) map[string]any {
		for _, it := range out["items"].([]any) {
			if it := it.(map[string]any); it["module"] == "selector.routing" && it["key"] == "strategy" {
				return it
			}
		}
		t.Fatalf("no strategy setting: %v", out)
		return nil
	}
	if it := strategy(c.must(200, "GET", "/api/v1/groups/"+grp+"/settings", nil)); it["invalid"] != true || it["value"] != "nope" {
		t.Fatalf("invalid stored value: %v", it)
	}
	out := c.must(200, "PUT", "/api/v1/groups/"+grp+"/settings", map[string]any{"settings": map[string]any{"selector.routing": map[string]any{"strategy": "cheapest"}}})
	if it := strategy(out); it["invalid"] != nil {
		t.Fatalf("valid value still marked: %v", it)
	}
}

// The effective view of a strictest list merged from several layers lists
// each layer and names the one that leaves no common entry, instead of
// the intersection's deny-all marker.
func TestStrictListEffectiveLayers(t *testing.T) {
	d := plugin.Setting{Key: "models", Type: plugin.TypeList, Scopes: plugin.Global | plugin.Group | plugin.Key, Merge: plugin.Strictest}
	for _, tc := range []struct {
		l        settings.Layered
		layers   int
		deniedBy string
	}{
		{settings.Layered{Global: json.RawMessage(`["a","b"]`), Key: json.RawMessage(`["b"]`)}, 2, ""},
		{settings.Layered{Global: json.RawMessage(`["a"]`), Group: json.RawMessage(`["a","c"]`), Key: json.RawMessage(`["c"]`)}, 3, "key"},
		{settings.Layered{Global: json.RawMessage(`["a"]`)}, 0, ""},
	} {
		v := settings.Merge(d, tc.l, false)
		at := func(s plugin.Scope) (json.RawMessage, bool) {
			raw := map[plugin.Scope]json.RawMessage{plugin.Global: tc.l.Global, plugin.Group: tc.l.Group, plugin.Key: tc.l.Key}[s]
			return raw, raw != nil
		}
		layers, denied := strictLayers(d, v, at)
		if len(layers) != tc.layers || denied != tc.deniedBy {
			t.Fatalf("%+v: layers %+v denied by %q", tc.l, layers, denied)
		}
	}
}

func sumField(points []any, field string) float64 {
	var n float64
	for _, p := range points {
		n += p.(map[string]any)[field].(float64)
	}
	return n
}

// An open breaker's failure count is reset by the router, so the view
// must not report it ("0 consecutive failures").
func TestCircuitReasonWhenOpen(t *testing.T) {
	h := New(Deps{Runtime: &fakeRuntime{}, Now: time.Now})
	until := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		live   router.UpstreamLive
		reason string
		until  bool
	}{
		{router.UpstreamLive{ID: "up1", Breaker: router.BreakerOpen, BreakerOpen: until}, circuitFailures, true},
		{router.UpstreamLive{ID: "up1", Breaker: router.BreakerHalfOpen}, circuitProbing, false},
		{router.UpstreamLive{ID: "up1", Breaker: router.BreakerClosed, Failures: 2}, "", false},
	} {
		uc := &upstreamCtx{live: map[string]router.UpstreamLive{"up1": tc.live}, manifests: map[string]ManifestInfo{}}
		v := h.upstreamView(context.Background(), &store.Upstream{ID: "up1", Provider: "provider.openai-compatible"}, uc)
		if v.Circuit.State != tc.live.Breaker || v.Circuit.Reason != tc.reason || (v.Circuit.Until != nil) != tc.until {
			t.Errorf("%s: circuit %+v", tc.live.Breaker, v.Circuit)
		}
		if tc.until && !v.Circuit.Until.Equal(until) {
			t.Errorf("until %v", v.Circuit.Until)
		}
	}
}
