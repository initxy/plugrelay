package app

import (
	"context"
	"encoding/json"
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
)

// TestKeyAllowedClients: a key's allowed clients are client ids, matched
// the way the request log names the client (User-Agent and attribution
// headers), and a refused client gets the same 403 as ever.
func TestKeyAllowedClients(t *testing.T) {
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
	newKey := func(clients ...string) string {
		s := map[string]json.RawMessage{}
		if len(clients) > 0 {
			s["clients"], _ = json.Marshal(clients)
		}
		k, err := st.CreateKey(ctx, &store.Key{Name: "k", Settings: s})
		mustOK(t, err)
		return k
	}
	editors := newKey("cline", "roo", "kilo")
	cli := newKey("claude-code", "codex")
	sdk := newKey("sdk-python")
	stale := newKey("no-such-client")
	open := newKey()
	gone := &store.User{ID: "u-gone", Name: "gone", Role: store.RoleMember}
	mustOK(t, st.CreateUser(ctx, gone, ""))
	goneKey, err := st.CreateKey(ctx, &store.Key{Name: "k", UserID: gone.ID})
	mustOK(t, err)
	gone.Disabled = true
	mustOK(t, st.UpdateUser(ctx, gone))
	mustOK(t, a.Reload(ctx))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	mustOK(t, err)
	go func() { _ = a.ServeListener(ctx, ln) }()

	const denied = `{"error":{"message":"client is not allowed for this API key","type":"permission_error","param":null,"code":"permission_denied"}}`
	for _, tc := range []struct {
		name    string
		key     string
		headers map[string]string
		ok      bool
	}{
		{"cline by user agent", editors, map[string]string{"User-Agent": "Cline/3.38.2"}, true},
		{"cline by title", editors, map[string]string{"User-Agent": "OpenAI/JS 4.104.0", "X-Title": "Cline"}, true},
		{"roo by title", editors, map[string]string{"User-Agent": "OpenAI/JS 5.12.2", "X-Title": "Roo Code", "Http-Referer": "https://github.com/RooVetGit/Roo-Cline"}, true},
		{"kilo by referer", editors, map[string]string{"User-Agent": "OpenAI/JS 5.12.2", "Http-Referer": "https://kilocode.ai"}, true},
		{"plain node sdk refused", editors, map[string]string{"User-Agent": "OpenAI/JS 5.12.2"}, false},
		{"claude code refused", editors, map[string]string{"User-Agent": "claude-cli/2.1.31 (external, cli)"}, false},
		{"claude code", cli, map[string]string{"User-Agent": "claude-cli/2.1.31 (external, cli)"}, true},
		{"codex", cli, map[string]string{"User-Agent": "codex_cli_rs/0.81.0 (Mac OS 15.6.1; arm64) iTerm.app/3.6.2"}, true},
		{"codex by originator", cli, map[string]string{"User-Agent": "reqwest", "Originator": "codex_cli_rs"}, true},
		{"cursor refused", cli, map[string]string{"User-Agent": "Cursor/1.8.3 (darwin arm64)"}, false},
		{"curl refused", cli, map[string]string{"User-Agent": "curl/8.7.1"}, false},
		{"no user agent refused", cli, nil, false},
		{"python sdk", sdk, map[string]string{"User-Agent": "OpenAI/Python 1.109.1"}, true},
		{"python sdk by stainless", sdk, map[string]string{"User-Agent": "httpx", "X-Stainless-Lang": "python"}, true},
		{"python requests refused", sdk, map[string]string{"User-Agent": "python-requests/2.32"}, false},
		{"unknown id matches nothing", stale, map[string]string{"User-Agent": "curl/8.7.1"}, false},
		{"no restriction", open, map[string]string{"User-Agent": "anything/1"}, true},
		{"disabled owner", goneKey, map[string]string{"User-Agent": "anything/1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/v1/chat/completions",
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer "+tc.key)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			mustOK(t, err)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if tc.ok {
				if resp.StatusCode != 200 {
					t.Fatalf("status %d, want 200: %s", resp.StatusCode, body)
				}
				return
			}
			if tc.name == "disabled owner" {
				if resp.StatusCode != 401 {
					t.Fatalf("status %d %s, want 401", resp.StatusCode, body)
				}
				return
			}
			if resp.StatusCode != 403 || strings.TrimSpace(string(body)) != denied {
				t.Fatalf("status %d %s, want 403 %s", resp.StatusCode, body, denied)
			}
		})
	}
}
