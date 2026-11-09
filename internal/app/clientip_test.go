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
	"time"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
)

// TestTrustedProxyClientIP: a key's IP allowlist and the request log see
// the client named by X-Forwarded-For only when the peer is a configured
// trusted proxy (the test client connects from loopback).
func TestTrustedProxyClientIP(t *testing.T) {
	const client = "203.0.113.7"
	for _, tc := range []struct {
		name    string
		trusted []string
		xff     string
		want    int
	}{
		{"trusted proxy, allowed client", []string{"127.0.0.1", "::1"}, client, 200},
		{"trusted proxy, other client", []string{"127.0.0.0/8", "::1"}, "198.51.100.1", 403},
		{"no trusted proxies, header ignored", nil, client, 403},
		{"peer not in trusted range, header ignored", []string{"10.0.0.0/8"}, client, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mk, _ := secret.GenerateMasterKey()
			cfg := config.Defaults()
			cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
			cfg.MasterKey = mk
			cfg.TrustedProxies = tc.trusted
			a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			st := a.Store()
			up := fakeChat(t)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(st.PutUpstream(ctx, &store.Upstream{ID: "fake", Provider: "provider.openai-compatible", Enabled: true,
				Config: map[string]any{"manifest": "vllm", "base_url": up.URL + "/v1", configAllowPrivate: true}}))
			must(st.PutCredential(ctx, &store.Credential{ID: "c1", UpstreamID: "fake", Kind: store.CredShared, Secret: "sk-upstream",
				Status: store.CredActive}))
			must(st.PutModel(ctx, &store.Model{Name: "m", Enabled: true}))
			must(st.ReplaceRoutes(ctx, "m", []store.Route{{Model: "m", UpstreamID: "fake", UpstreamModel: "fake-model", Weight: 1}}))
			allow, _ := json.Marshal([]string{client})
			key, err := st.CreateKey(ctx, &store.Key{Name: "it", Settings: map[string]json.RawMessage{"ip_allowlist": allow}})
			must(err)
			must(a.Reload(ctx))

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			must(err)
			go func() { _ = a.ServeListener(ctx, ln) }()

			req, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/v1/chat/completions",
				strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Forwarded-For", tc.xff)
			resp, err := http.DefaultClient.Do(req)
			must(err)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tc.want, body)
			}
			if tc.want != 200 {
				return
			}
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				page, err := st.QueryRecords(ctx, store.RecordQuery{Limit: 1})
				must(err)
				if len(page.Records) > 0 {
					if got := page.Records[0].ClientIP; got != client {
						t.Fatalf("request log client IP %q, want %q", got, client)
					}
					return
				}
			}
			t.Fatal("no request log row")
		})
	}
}
