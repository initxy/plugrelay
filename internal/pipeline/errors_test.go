package pipeline

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Hard rule 8: relay-generated errors use the client's protocol.
func TestRelayErrorsAreNative(t *testing.T) {
	up := newFake(t, jsonReply(200, chatResp))
	e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up, caps: "{images: false}"}})

	resp, body := e.call("anthropic", "/v1/messages", `{"model":"nope","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 404 || gjson.GetBytes(body, "error.type").Str != "not_found_error" {
		t.Errorf("unknown model: %d %s", resp.StatusCode, body)
	}

	img := `{"model":"gpt-x","max_tokens":5,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}},{"type":"text","text":"what?"}]}]}`
	resp, body = e.call("anthropic", "/v1/messages", img)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "images") || up.calls() != 0 {
		t.Errorf("capability missing: %d %s", resp.StatusCode, body)
	}
	if e.records.last(t).ErrorKind != ir.ErrCapabilityMissing {
		t.Errorf("record kind %s", e.records.last(t).ErrorKind)
	}

	resp, body = e.call("openai-chat", "/v1/chat/completions", `{"messages":[]}`)
	if resp.StatusCode != 400 || gjson.GetBytes(body, "error.message").Str == "" {
		t.Errorf("missing model: %d %s", resp.StatusCode, body)
	}

	e.snap.Visible = func(_ *plugin.Caller, m string) bool { return m != "gpt-x" }
	resp, _ = e.call("openai-chat", "/v1/chat/completions", chatReq)
	if resp.StatusCode != 404 {
		t.Errorf("invisible model: %d", resp.StatusCode)
	}
}

// Hard rule 10: the first-byte timer fails an attempt that gets no answer.
func TestFirstByteTimeout(t *testing.T) {
	hang := newFake(t, sseReply(2*time.Second, "data: [DONE]\n\n"))
	e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: hang}},
		func(d *Deps) { d.Timeouts = egress.Timeouts{FirstByte: 100 * time.Millisecond} })
	start := time.Now()
	resp, body := e.call("anthropic", "/v1/messages", `{"model":"gpt-x","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if time.Since(start) > time.Second || resp.StatusCode < 500 || !strings.Contains(string(body), "timeout") {
		t.Errorf("status %d after %v: %s", resp.StatusCode, time.Since(start), body)
	}
	if a := e.records.last(t).Attempts[0]; !a.Sent || !a.Usage.Estimated {
		t.Errorf("timed-out sent attempt not costed: %+v", a)
	}
}

func TestModelsETag(t *testing.T) {
	e := newEnv(t, "gpt-x", nil)
	e.snap.Visible = func(_ *plugin.Caller, m string) bool { return m == "gpt-x" }
	resp, body := e.call("openai-chat", "/v1/models", "")
	if resp.StatusCode != 200 || gjson.GetBytes(body, "data.#").Int() != 1 || resp.Header.Get("ETag") == "" {
		t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, body)
	}
	resp, _ = e.call("openai-chat", "/v1/models", "", "If-None-Match", resp.Header.Get("ETag"))
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match: %d", resp.StatusCode)
	}
}

func TestCountTokens(t *testing.T) {
	up := newFake(t, jsonReply(200, `{"input_tokens":1234}`))
	e := newEnv(t, "claude-x", []upSpec{{id: "a", protocol: "anthropic", issuer: "i", fake: up, model: "claude-real"}})
	resp, body := e.call("anthropic", "/v1/messages/count_tokens", `{"model":"claude-x","messages":[{"role":"user","content":"hello there"}]}`)
	if resp.StatusCode != 200 || gjson.GetBytes(body, "input_tokens").Int() != 1234 || up.paths[0] != "/v1/messages/count_tokens" ||
		gjson.GetBytes(up.body(0), "model").Str != "claude-real" {
		t.Fatalf("forwarded: %d %s %v", resp.StatusCode, body, up.paths)
	}
	// No same-protocol endpoint: local estimate.
	chat := newFake(t, jsonReply(200, chatResp))
	e2 := newEnv(t, "claude-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: chat}})
	resp, body = e2.call("anthropic", "/v1/messages/count_tokens", `{"model":"claude-x","messages":[{"role":"user","content":"hello there"}]}`)
	if resp.StatusCode != 200 || gjson.GetBytes(body, "input_tokens").Int() == 0 || chat.calls() != 0 {
		t.Errorf("local: %d %s", resp.StatusCode, body)
	}
}

// A gemini client renamed onto another model: the model lives in the
// path, so the body still passes through byte for byte.
func TestGeminiModelInPath(t *testing.T) {
	reply := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1}}`
	up := newFake(t, jsonReply(200, reply))
	e := newEnv(t, "gem-x", []upSpec{{id: "g", protocol: "gemini", issuer: "i", fake: up, model: "gemini-real"}})
	req := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	resp, body := e.call("gemini", "/v1beta/models/gem-x:generateContent", req, "X-Goog-Api-Key", "client")
	if resp.StatusCode != 200 || string(body) != reply {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if up.paths[0] != "/v1beta/models/gemini-real:generateContent" || string(up.body(0)) != req ||
		up.headers[0].Get("X-Goog-Api-Key") != "sk-g" {
		t.Errorf("upstream %s %s %v", up.paths[0], up.body(0), up.headers[0])
	}
	if a := e.records.last(t).Attempts[0]; a.Tier != plugin.TierPassthrough || a.Usage.Input != 3 {
		t.Errorf("attempt %+v", a)
	}
}

// Opt-in check against a live OpenAI-compatible upstream (PLUGRELAY_E2E_BASE,
// with /v1, and PLUGRELAY_E2E_KEY).
func TestE2ELiveChatUpstream(t *testing.T) {
	key := os.Getenv("PLUGRELAY_E2E_KEY")
	if key == "" {
		t.Skip("PLUGRELAY_E2E_KEY not set")
	}
	base := os.Getenv("PLUGRELAY_E2E_BASE")
	if base == "" {
		t.Skip("PLUGRELAY_E2E_BASE not set")
	}
	model := os.Getenv("PLUGRELAY_E2E_CHAT_MODEL")
	if model == "" {
		model = "deepseek-chat"
	}
	up := newFake(t, nil)
	up.srv.Close()
	e := newEnv(t, "claude-x", []upSpec{{id: "live", protocol: "openai-chat", issuer: "live", fake: up, model: model}})
	u := e.snap.Upstreams["live"]
	u.Credentials["c1"].Secret = key
	for i := range u.Config.Endpoints {
		u.Config.Endpoints[i].BaseURL = strings.TrimRight(base, "/") // includes /v1, as in test/e2e
	}
	tu := e.snap.Table.Upstreams["live"]
	tu.Endpoints = u.Config.Endpoints
	e.snap.Table.Upstreams["live"] = tu
	e.p.d.Timeouts = egress.Timeouts{FirstByte: 60 * time.Second, Idle: 60 * time.Second, Total: 120 * time.Second}
	resp, body := e.call("anthropic", "/v1/messages",
		`{"model":"claude-x","max_tokens":200,"stream":true,"messages":[{"role":"user","content":"Reply with the single word: pong"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "event: message_stop") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if rec := e.records.last(t); rec.Usage.IsZero() {
		t.Errorf("no usage: %+v", rec)
	}
}
