package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/middleware/access"
)

// nativeRelay starts a relay with one upstream pointing at h and returns
// its base URL, a platform key and the store.
func nativeRelay(t *testing.T, enabled bool, h http.HandlerFunc) (string, string, store.Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	cfg.Native.Enabled = enabled
	a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	st := a.Store()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.PutUpstream(ctx, &store.Upstream{ID: "local", Provider: "provider.openai-compatible", Enabled: true,
		Config: map[string]any{"manifest": "vllm", "base_url": up.URL + "/v1", configAllowPrivate: true}}))
	must(st.PutCredential(ctx, &store.Credential{ID: "c1", UpstreamID: "local", Kind: store.CredShared, Secret: "sk-upstream",
		Status: store.CredActive}))
	key, err := st.CreateKey(ctx, &store.Key{Name: "it"})
	must(err)
	must(a.Reload(ctx))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	done := make(chan error, 1)
	go func() { done <- a.ServeListener(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("no graceful shutdown")
		}
	})
	return "http://" + ln.Addr().String(), key, st
}

func nativeGet(t *testing.T, url, key string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func upstreamFiles(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/files" || r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	}
}

// relay.yaml's native.enabled is off by default: the route does not exist.
func TestNativeOffByDefault(t *testing.T) {
	if config.Defaults().Native.Enabled {
		t.Fatal("native passthrough on by default")
	}
	base, key, _ := nativeRelay(t, false, upstreamFiles(t))
	if st, body := nativeGet(t, base+"/native/local/files", key); st != 404 {
		t.Fatalf("%d %s", st, body)
	}
}

// Switched on, a key still needs the middleware.access grant; a key-level
// grant opens the upstream and the request is logged as native.
func TestNativeGrant(t *testing.T) {
	base, key, st := nativeRelay(t, true, upstreamFiles(t))
	if code, body := nativeGet(t, base+"/native/local/files", ""); code != 401 {
		t.Fatalf("no key: %d %s", code, body)
	}
	if code, body := nativeGet(t, base+"/native/local/files", key); code != 403 || !strings.Contains(body, "not allowed") {
		t.Fatalf("no grant: %d %s", code, body)
	}
	ctx := context.Background()
	keys, err := st.ListKeys(ctx, store.KeyFilter{})
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys %v %v", keys, err)
	}
	grant, _ := json.Marshal([]string{"local"})
	if err := st.PutSetting(ctx, store.SettingRow{Scope: plugin.Key, ScopeID: keys[0].ID, PluginID: access.ID,
		Key: access.SettingNativeUpstreams, Value: grant}); err != nil {
		t.Fatal(err)
	}
	var code int
	var body string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if code, body = nativeGet(t, base+"/native/local/files", key); code != 403 {
			break
		}
	}
	if code != 200 || body != `{"object":"list","data":[]}` {
		t.Fatalf("granted: %d %s", code, body)
	}
	if code, body := nativeGet(t, base+"/native/other/files", key); code != 403 {
		t.Errorf("other upstream: %d %s", code, body)
	}
	var rec *plugin.Record
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && rec == nil; time.Sleep(50 * time.Millisecond) {
		page, err := st.QueryRecords(ctx, store.RecordQuery{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Records {
			if r.Status == 200 {
				rec, _ = st.GetRecord(ctx, r.ID)
				break
			}
		}
	}
	if rec == nil || rec.Protocol != "native" || len(rec.Attempts) != 1 || rec.Attempts[0].BytesOut != int64(len(body)) ||
		rec.Attempts[0].Tier != plugin.TierPassthrough || rec.CostMicro != 0 {
		t.Fatalf("record %+v", rec)
	}
}

// A WebSocket handshake runs no request hooks, so the middleware.access
// source-address allowlist is applied to it directly.
func TestWebSocketHandshakeAccessChecked(t *testing.T) {
	base, key, st := nativeRelay(t, false, upstreamFiles(t))
	handshake := func() (int, string) {
		req, _ := http.NewRequest("GET", base+"/v1/realtime?model=nope", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := handshake(); code == 403 {
		t.Fatalf("no allowlist: %d %s", code, body)
	}
	ctx := context.Background()
	keys, err := st.ListKeys(ctx, store.KeyFilter{})
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys %v %v", keys, err)
	}
	if err := st.PutGroup(ctx, &store.Group{ID: "office", Name: "Office"}); err != nil {
		t.Fatal(err)
	}
	k := keys[0]
	k.GroupID = "office"
	if err := st.UpdateKey(ctx, &k); err != nil {
		t.Fatal(err)
	}
	ips, _ := json.Marshal([]string{"10.0.0.0/8"})
	if err := st.PutSetting(ctx, store.SettingRow{Scope: plugin.Group, ScopeID: "office", PluginID: access.ID,
		Key: "allowed_ips", Value: ips}); err != nil {
		t.Fatal(err)
	}
	var code int
	var body string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if code, body = handshake(); code == 403 {
			break
		}
	}
	if code != 403 || !strings.Contains(body, "source address is not allowed") {
		t.Fatalf("outside the allowlist: %d %s", code, body)
	}
}
