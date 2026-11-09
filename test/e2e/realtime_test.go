//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestWebSocket checks the WebSocket relays live: OpenAI Realtime on
// /v1/realtime and the Responses WebSocket mode on /v1/responses. Each case
// first dials the upstream directly and skips, saying so, when it does not
// upgrade.
//
//	PLUGRELAY_E2E_REALTIME_MODEL  realtime model (default gpt-realtime)
func TestWebSocket(t *testing.T) {
	upBase, upKey := os.Getenv("PLUGRELAY_E2E_BASE"), os.Getenv("PLUGRELAY_E2E_KEY")
	if upBase == "" || upKey == "" {
		t.Skip("set PLUGRELAY_E2E_BASE and PLUGRELAY_E2E_KEY to run against a live upstream")
	}
	model := envOr("PLUGRELAY_E2E_MODEL", defaultModel)
	rtModel := envOr("PLUGRELAY_E2E_REALTIME_MODEL", "gpt-realtime")
	base := strings.TrimRight(upBase, "/")
	realtime := wsServes(t, base+"/realtime?model="+rtModel, upKey, nil)
	responses := wsServes(t, base+"/responses", upKey, http.Header{"OpenAI-Beta": {"responses_websockets=2026-02-06"}})
	if !realtime && !responses {
		t.Skip("the upstream upgrades neither /realtime nor /responses to WebSocket; not checked live")
	}
	// No model on the generic upstream: a Responses upgrade is accepted only
	// when every model routes to the WebSocket upstream. The generic
	// manifest declares neither capability; a stored copy that does serves
	// the WebSocket routes.
	r := start(t, upBase)
	doc := filepath.Join(t.TempDir(), "ws.yaml")
	yml := "id: e2e-ws\nkind: provider\ninterpreter: openai-compatible\nvariables:\n  base_url: {default: \"" + upBase + "\"}\n" +
		"auth: {header: Authorization, scheme: Bearer}\nendpoints:\n  - {id: responses, protocol: openai-responses, base_url: \"{base_url}\", " +
		"capabilities: {realtime: true, responses_websocket: true}}\n"
	b, _ := json.Marshal(map[string]any{"manifests": []map[string]string{{"id": "e2e-ws", "yaml": yml}}})
	_ = os.WriteFile(doc, b, 0o600)
	r.run("apply", "-f", doc)
	r.run("upstream", "add", "--manifest", "e2e-ws", "--id", "e2e-ws", "--credential-env", "PLUGRELAY_E2E_KEY")
	r.run("model", "add", "--name", "ws-"+rtModel, "--route", "e2e-ws:"+rtModel)
	r.run("model", "add", "--name", "ws-"+model, "--route", "e2e-ws:"+model)
	time.Sleep(3 * time.Second) // the next reload picks the upstream up
	wsBase := "ws" + strings.TrimPrefix(r.base, "http")
	auth := http.Header{"Authorization": {"Bearer " + r.key}}

	t.Run("realtime", func(t *testing.T) {
		if !realtime {
			t.Skipf("the upstream does not upgrade /realtime?model=%s; not checked live", rtModel)
		}
		c := wsDialLive(t, wsBase+"/v1/realtime?model=ws-"+rtModel, auth)
		wsWrite(t, c, map[string]any{"type": "response.create", "response": map[string]any{
			"output_modalities": []string{"text"}, "instructions": prompt}})
		done := wsUntil(t, c, "response.done")
		var ev struct {
			Response struct {
				Usage struct {
					Output int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		_ = json.Unmarshal(done, &ev)
		if ev.Response.Usage.Output == 0 {
			t.Fatalf("response.done without usage: %s", done)
		}
	})

	t.Run("responses websocket", func(t *testing.T) {
		if !responses {
			t.Skip("the upstream does not upgrade /responses; not checked live")
		}
		h := http.Header{"Authorization": {"Bearer " + r.key}, "OpenAI-Beta": {"responses_websockets=2026-02-06"}}
		c := wsDialLive(t, wsBase+"/v1/responses", h)
		wsWrite(t, c, map[string]any{"type": "response.create", "model": "ws-" + model, "store": false,
			"input": []any{map[string]any{"role": "user", "content": prompt}}})
		done := wsUntil(t, c, "response.completed")
		var ev struct {
			Response struct {
				Usage struct {
					Output int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		_ = json.Unmarshal(done, &ev)
		if ev.Response.Usage.Output == 0 {
			t.Fatalf("response.completed without usage: %s", done)
		}
	})
}

// wsServes reports whether url answers a WebSocket upgrade.
func wsServes(t *testing.T, url, key string, h http.Header) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hdr := http.Header{"Authorization": {"Bearer " + key}}
	for k, v := range h {
		hdr[k] = v
	}
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err == nil {
		_ = c.Close(websocket.StatusNormalClosure, "")
		return true
	}
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	t.Logf("upstream WebSocket probe %s: status %d: %v", strings.SplitN(url, "?", 2)[0], status, err)
	return false
}

func wsDialLive(t *testing.T, url string, h http.Header) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s: status %d: %v", url, status, err)
	}
	c.SetReadLimit(16 << 20)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func wsWrite(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := c.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// wsUntil reads events until one of type typ arrives and returns it.
func wsUntil(t *testing.T, c *websocket.Conn, typ string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		var ev struct{ Type string }
		_ = json.Unmarshal(b, &ev)
		if ev.Type == "error" {
			t.Fatalf("error event: %s", b)
		}
		if ev.Type == typ {
			return b
		}
	}
}
