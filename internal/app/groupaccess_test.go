package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/middleware/access"
)

// TestGroupAllowlistInvalidValueDeniesAll: a group allowlist stored with a
// value that fails validation (an id the relay does not know, a string in
// place of a list) refuses every request through the real merge and
// middleware; it never reads as "no restriction".
func TestGroupAllowlistInvalidValueDeniesAll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	st := a.Store()
	up := fakeChat(t)
	mustOK(t, st.PutUpstream(ctx, &store.Upstream{ID: "fake", Provider: "provider.openai-compatible", Enabled: true,
		Config: map[string]any{"manifest": "vllm", "base_url": up.URL + "/v1", configAllowPrivate: true}}))
	mustOK(t, st.PutCredential(ctx, &store.Credential{ID: "c1", UpstreamID: "fake", Kind: store.CredShared, Secret: "sk-upstream",
		Status: store.CredActive}))
	mustOK(t, st.PutModel(ctx, &store.Model{Name: "m", Enabled: true}))
	mustOK(t, st.ReplaceRoutes(ctx, "m", []store.Route{{Model: "m", UpstreamID: "fake", UpstreamModel: "fake-model", Weight: 1}}))
	// Stored directly, as an old relay or a hand-edited database may hold them.
	group := func(id, key, value string) string {
		mustOK(t, st.PutGroup(ctx, &store.Group{ID: id, Name: id}))
		if key != "" {
			mustOK(t, st.PutSetting(ctx, store.SettingRow{Scope: plugin.Group, ScopeID: id, PluginID: access.ID, Key: key,
				Value: []byte(value)}))
		}
		k, err := st.CreateKey(ctx, &store.Key{Name: id, GroupID: id})
		mustOK(t, err)
		return k
	}
	keys := map[string]string{
		"curl allowed":         group("curl", "allowed_clients", `["curl"]`),
		"unknown id among ids": group("mixed", "allowed_clients", `["curl","claude-cli/"]`),
		"only an unknown id":   group("prefix", "allowed_clients", `["claude-cli/"]`),
		"a string":             group("string", "allowed_clients", `"curl"`),
		"ip range as a string": group("ips", "allowed_ips", `"127.0.0.0/8"`),
		"no restriction":       group("open", "", ""),
	}
	mustOK(t, a.Reload(ctx))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	mustOK(t, err)
	go func() { _ = a.ServeListener(ctx, ln) }()

	for name, key := range keys {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/v1/chat/completions",
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "curl/8.7.1")
			resp, err := http.DefaultClient.Do(req)
			mustOK(t, err)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			want := 403
			if name == "curl allowed" || name == "no restriction" {
				want = 200
			}
			if resp.StatusCode != want {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, want, body)
			}
		})
	}
}

// `plugrelay apply` refuses a setting value the console would refuse, and
// writes nothing of the document.
func TestApplyRefusesInvalidSettingValues(t *testing.T) {
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
	mustOK(t, a.Store().PutGroup(ctx, &store.Group{ID: "g", Name: "g"}))
	doc := func(value string) []byte {
		return []byte("settings:\n  - {scope: group, scope_id: g, plugin: middleware.access, key: allowed_clients, value: " + value + "}\n")
	}
	for name, d := range map[string][]byte{
		"unknown client id": doc(`["codex", "claude-cli/"]`),
		"string, not list":  doc(`codex`),
		"unknown key":       []byte("settings:\n  - {scope: group, scope_id: g, plugin: middleware.access, key: allowed_clientz, value: [codex]}\n"),
		"scope not allowed": []byte("settings:\n  - {scope: global, plugin: middleware.access, key: allowed_clients, value: [codex]}\n"),
	} {
		if _, err := ApplyDocument(ctx, &cfg, d, false); err == nil {
			t.Errorf("%s: applied", name)
		}
	}
	if rows, _ := a.Store().ListSettings(ctx, store.SettingFilter{PluginID: access.ID}); len(rows) != 0 {
		t.Fatalf("refused apply was written: %v", rows)
	}
	if _, err := ApplyDocument(ctx, &cfg, doc(`[codex, curl]`), false); err != nil {
		t.Fatalf("valid value refused: %v", err)
	}
}
