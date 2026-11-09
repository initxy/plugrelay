package app

import (
	"bufio"
	"context"
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
)

// fakeChat is an OpenAI-chat upstream that streams "Hello".
func fakeChat(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"bad auth","type":"invalid_request_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"fake-model","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"fake-model","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"fake-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"fake-model","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
			`[DONE]`,
		} {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAnthropicClientThroughChatUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(dir, "relay.db")
	cfg.MasterKey = mk
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
	must(st.CreateUser(ctx, &store.User{Name: "admin", Email: "admin", Role: store.RoleAdmin}, "correct-horse-1"))
	must(st.PutUpstream(ctx, &store.Upstream{ID: "fake", Provider: "provider.openai-compatible", Enabled: true,
		Config: map[string]any{"manifest": chatOnlyManifest(), "base_url": up.URL + "/v1", configAllowPrivate: true}}))
	must(st.PutCredential(ctx, &store.Credential{ID: "c1", UpstreamID: "fake", Kind: store.CredShared, Secret: "sk-upstream",
		Status: store.CredActive}))
	must(st.PutModel(ctx, &store.Model{Name: "claude-x", Enabled: true}))
	must(st.ReplaceRoutes(ctx, "claude-x", []store.Route{{Model: "claude-x", UpstreamID: "fake", UpstreamModel: "fake-model", Weight: 1}}))
	key, err := st.CreateKey(ctx, &store.Key{Name: "it"})
	must(err)
	must(a.Reload(ctx))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	done := make(chan error, 1)
	go func() { done <- a.ServeListener(ctx, ln) }()
	base := "http://" + ln.Addr().String()

	body := `{"model":"claude-x","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", base+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	must(err)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var events []string
	var text strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if ev, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, ev)
		}
		if strings.Contains(line, `"text_delta"`) {
			i := strings.Index(line, `"text":"`)
			rest := line[i+8:]
			text.WriteString(rest[:strings.Index(rest, `"`)])
		}
	}
	resp.Body.Close()
	joined := strings.Join(events, ",")
	if !strings.HasPrefix(joined, "message_start") || !strings.HasSuffix(joined, "message_delta,message_stop") ||
		!strings.Contains(joined, "content_block_delta") {
		t.Fatalf("events: %s", joined)
	}
	if text.String() != "Hello" {
		t.Fatalf("text: %q", text.String())
	}

	// The request log row carries the attempt.
	var rec *plugin.Record
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		page, err := st.QueryRecords(ctx, store.RecordQuery{Limit: 10})
		must(err)
		if len(page.Records) > 0 {
			rec, err = st.GetRecord(ctx, page.Records[0].ID)
			must(err)
			break
		}
	}
	if rec == nil {
		t.Fatal("no request log row")
	}
	if rec.Protocol != "anthropic" || rec.Model != "claude-x" || rec.Status != 200 || len(rec.Attempts) != 1 {
		t.Fatalf("record: %+v", rec)
	}
	at := rec.Attempts[0]
	if at.UpstreamID != "fake" || at.CredentialID != "c1" || at.Protocol != "openai-chat" || at.Tier != plugin.TierConvert ||
		at.UpstreamModel != "fake-model" || at.Status != 200 || rec.Usage.Output != 2 {
		t.Fatalf("attempt: %+v usage %+v", at, rec.Usage)
	}

	// Live router state and the admin API see the upstream.
	r2, _ := http.NewRequest("GET", base+"/api/v1/upstreams", nil)
	r2.Header.Set("Authorization", "Bearer pr_nope")
	if resp, err := http.DefaultClient.Do(r2); err != nil || resp.StatusCode != 401 {
		t.Fatalf("admin API with platform key: %v %v", resp.StatusCode, err)
	}
	if resp, err := http.Get(base + "/readyz"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("readyz: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no graceful shutdown")
	}
}

// TestShutdownRecordsInFlightStream: a stream still running when the
// shutdown grace runs out is cut off, and its request log entry is still
// written before the store closes.
func TestShutdownRecordsInFlightStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(dir, "relay.db")
	cfg.MasterKey = mk
	cfg.Shutdown.Grace = config.Duration(50 * time.Millisecond)
	a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"fake-model","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // never finishes on its own
	}))
	defer up.Close()
	st := a.Store()
	must(st.PutUpstream(ctx, &store.Upstream{ID: "fake", Provider: "provider.openai-compatible", Enabled: true,
		Config: map[string]any{"manifest": chatOnlyManifest(), "base_url": up.URL + "/v1", configAllowPrivate: true}}))
	must(st.PutCredential(ctx, &store.Credential{ID: "c1", UpstreamID: "fake", Kind: store.CredShared, Secret: "sk-upstream",
		Status: store.CredActive}))
	must(st.PutModel(ctx, &store.Model{Name: "m", Enabled: true}))
	must(st.ReplaceRoutes(ctx, "m", []store.Route{{Model: "m", UpstreamID: "fake", UpstreamModel: "fake-model", Weight: 1}}))
	key, err := st.CreateKey(ctx, &store.Key{Name: "it"})
	must(err)
	must(a.Reload(ctx))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	done := make(chan error, 1)
	go func() { done <- a.ServeListener(ctx, ln) }()
	req, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	must(err)
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() && !strings.Contains(sc.Text(), "Hel") {
	}

	cancel()
	select {
	case err := <-done:
		must(err)
	case <-time.After(15 * time.Second):
		t.Fatal("no shutdown")
	}
	reopened, err := store.Open(context.Background(), store.Config{Driver: "sqlite", DSN: cfg.Store.DSN})
	must(err)
	defer reopened.Close()
	page, err := reopened.QueryRecords(context.Background(), store.RecordQuery{Limit: 10})
	must(err)
	if len(page.Records) != 1 || page.Records[0].Model != "m" {
		t.Fatalf("records after shutdown: %+v", page.Records)
	}
}

// chatOnlyManifest is an upstream that speaks only chat completions, so a
// request in any other protocol must be converted, never passed through.
// It is defined here rather than borrowed from a shipped manifest that may
// gain endpoints.
func chatOnlyManifest() map[string]any {
	return map[string]any{"id": "test-chat-only", "kind": "provider", "name": "Chat only (test)",
		"interpreter": "openai-compatible",
		"variables":   map[string]any{"base_url": map[string]any{"default": "http://localhost/v1"}},
		"auth":        map[string]any{"header": "Authorization", "scheme": "Bearer"},
		"endpoints":   []any{map[string]any{"id": "chat", "protocol": "openai-chat", "base_url": "{base_url}"}}}
}
