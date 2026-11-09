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

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/mcp"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/middleware/access"
)

// TestMCPGatewayThroughRelay: /mcp authenticates with the platform key,
// needs the middleware.access grant, injects the server's secret header,
// counts tool calls against middleware.ratelimit and logs them.
func TestMCPGatewayThroughRelay(t *testing.T) {
	var gotAuth []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&m)
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch m.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "u1")
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(m.ID)+`,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"f","version":"1"}}}`)
		case "tools/list":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(m.ID)+`,"result":{"tools":[{"name":"echo","inputSchema":{"type":"object"}}]}}`)
		case "tools/call":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(m.ID)+`,"result":{"content":[{"type":"text","text":"pong"}]}}`)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(up.Close)

	ctx, cancel := context.WithCancel(context.Background())
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	cfg.Egress.AllowPrivateCIDRs = []string{"127.0.0.0/8"}
	a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	st := a.Store()
	key, err := st.CreateKey(ctx, &store.Key{Name: "it"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.mcp.Registry().Create(ctx, mcp.Input{ID: ptrTo("fake"), URL: ptrTo(up.URL),
		SecretHeaders: map[string]string{"Authorization": "Bearer server-secret"}}); err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
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
	base := "http://" + ln.Addr().String()
	post := func(path, k, sid, body string) (*http.Response, string) {
		req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
		req.Header.Set("Accept", "application/json, text/event-stream")
		if k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`
	// The MCP gateway feature is off by default: /mcp is not there.
	if resp, _ := post("/mcp/fake", "", "", initBody); resp.StatusCode != 404 {
		t.Fatalf("gateway off: %d", resp.StatusCode)
	}
	if err := st.PutFeature(ctx, store.FeatureState{ID: "mcp", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if resp, body := post("/mcp/fake", "", "", initBody); resp.StatusCode != 401 || gjson.Get(body, "error.message").Str == "" {
		t.Fatalf("no key: %d %s", resp.StatusCode, body)
	}
	if resp, _ := post("/mcp/fake", key, "", initBody); resp.StatusCode != 404 {
		t.Fatalf("no grant: %d", resp.StatusCode)
	}
	keys, _ := st.ListKeys(ctx, store.KeyFilter{})
	put := func(pluginID, k string, v any) {
		raw, _ := json.Marshal(v)
		if err := st.PutSetting(ctx, store.SettingRow{Scope: plugin.Key, ScopeID: keys[0].ID, PluginID: pluginID, Key: k, Value: raw}); err != nil {
			t.Fatal(err)
		}
	}
	put(access.ID, access.SettingMCPServers, []string{"fake"})
	// The key's own rpm lives on the key record.
	keys[0].Settings = map[string]json.RawMessage{"rpm": json.RawMessage("2")}
	if err := st.UpdateKey(ctx, &keys[0]); err != nil {
		t.Fatal(err)
	}
	var resp *http.Response
	var body string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if resp, body = post("/mcp/fake", key, "", initBody); resp.StatusCode != 404 {
			break
		}
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || sid == "" {
		t.Fatalf("granted: %d %s", resp.StatusCode, body)
	}
	_, body = post("/mcp", key, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if gjson.Get(body, "result.tools.0.name").Str != "fake__echo" {
		t.Fatalf("aggregated list: %s", body)
	}
	_, body = post("/mcp/fake", key, sid, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	if gjson.Get(body, "result.content.0.text").Str != "pong" {
		t.Fatalf("call: %s", body)
	}
	_, _ = post("/mcp/fake", key, sid, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	if resp, body := post("/mcp/fake", key, sid, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{}}}`); resp.StatusCode != 429 {
		t.Fatalf("rpm 2 not enforced on the third call: %d %s", resp.StatusCode, body)
	}
	for _, h := range gotAuth {
		if h != "Bearer server-secret" {
			t.Fatalf("server saw Authorization %q", h)
		}
	}
	var page *store.RecordPage
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if page, err = st.QueryRecords(ctx, store.RecordQuery{Model: "fake__echo"}); err == nil && len(page.Records) == 3 {
			break
		}
	}
	if page == nil || len(page.Records) != 3 {
		t.Fatalf("records %+v", page)
	}
	// The three calls can share a millisecond, so the log's order among
	// them is not fixed: check the set, not the position.
	limited := 0
	for _, r := range page.Records {
		if r.Protocol != mcp.Protocol {
			t.Fatalf("record protocol %q, want %q", r.Protocol, mcp.Protocol)
		}
		if r.Status == 429 {
			limited++
		}
	}
	if limited != 1 {
		t.Fatalf("%d rate-limited records among %+v, want 1", limited, page.Records)
	}
	// The group's source-address allowlist covers MCP as it does model calls.
	if err := st.PutGroup(ctx, &store.Group{ID: "office", Name: "Office"}); err != nil {
		t.Fatal(err)
	}
	keys[0].GroupID = "office"
	keys[0].Settings = nil
	if err := st.UpdateKey(ctx, &keys[0]); err != nil {
		t.Fatal(err)
	}
	ips, _ := json.Marshal([]string{"10.0.0.0/8"})
	if err := st.PutSetting(ctx, store.SettingRow{Scope: plugin.Group, ScopeID: "office", PluginID: access.ID,
		Key: "allowed_ips", Value: ips}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if resp, body = post("/mcp/fake", key, "", initBody); resp.StatusCode == 403 {
			break
		}
	}
	if resp.StatusCode != 403 || !strings.Contains(body, "source address is not allowed") {
		t.Fatalf("outside the group allowlist: %d %s", resp.StatusCode, body)
	}
}

func ptrTo[T any](v T) *T { return &v }
