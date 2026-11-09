package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
)

// A just-created upstream (no reload yet, no routes) is tested with a
// chat model from its own model list.
func TestTestNewUpstreamPicksListedModel(t *testing.T) {
	ctx := context.Background()
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"text-embedding-x","object":"model"},{"id":"chat-x","object":"model"}]}`)
		case "/v1/chat/completions":
			b, _ := io.ReadAll(r.Body)
			asked = string(b)
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"chat-x","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	man := keyOptionalChat()
	man["models"] = map[string]any{"list": "/models"}
	if err := a.Store().PutUpstream(ctx, &store.Upstream{ID: "new", Provider: "provider.openai-compatible", Enabled: false,
		Config: map[string]any{"manifest": man, "base_url": srv.URL + "/v1", configAllowPrivate: true}}); err != nil {
		t.Fatal(err)
	}
	res := a.testUpstream(ctx, "new", admin.TestRequest{})
	if len(res) != 1 || !res[0].OK || res[0].Model != "chat-x" {
		t.Fatalf("results %+v", res)
	}
	if asked == "" {
		t.Fatal("no chat probe sent")
	}
	if got := testModel([]ir.ModelInfo{{ID: "whisper-1"}, {ID: "tts-1"}}); got != "" {
		t.Errorf("testModel = %q", got)
	}
}
