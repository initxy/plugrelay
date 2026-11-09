package pipeline

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/pkg/plugin"
)

const anthropicReq = `{"model":"claude-x","max_tokens":1024,"stream":true,` +
	`"system":[{"type":"text","text":"You are terse.","cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"List the files."}]}],` +
	`"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}]}`

// Hard rules 3 and 6: same protocol, same model → the upstream receives the
// client's bytes and the client receives the upstream's bytes.
func TestPassthroughByteExact(t *testing.T) {
	stream := fixture(t, "anthropic_stream.sse")
	up := newFake(t, sseReply(0, stream))
	e := newEnv(t, "claude-x", []upSpec{{id: "a", protocol: "anthropic", issuer: "iss-a", fake: up}})

	resp, body := e.call("anthropic", "/v1/messages", anthropicReq,
		"anthropic-beta", "tools-2024", "X-Api-Key", "client-key", "X-Claude-Code-Session-Id", "s1")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if got := string(up.body(0)); got != anthropicReq {
		t.Errorf("upstream body changed:\n%s", got)
	}
	if string(body) != stream {
		t.Errorf("client stream differs from upstream stream:\n%s", body)
	}
	h := up.headers[0]
	if h.Get("X-Api-Key") != "sk-a" || h.Get("Anthropic-Beta") != "tools-2024" {
		t.Errorf("upstream headers: %v", h)
	}
	if resp.Header.Get("Request-Id") != "req_upstream" {
		t.Errorf("upstream response header not passed: %v", resp.Header)
	}
	rec := e.records.last(t)
	if len(rec.Attempts) != 1 || rec.Attempts[0].Tier != plugin.TierPassthrough {
		t.Fatalf("attempts %+v", rec.Attempts)
	}
	if rec.Usage.CacheRead != 30000 || rec.Usage.Output != 87 || rec.CostMicro == 0 || rec.ChargeMicro != rec.CostMicro {
		t.Errorf("usage %+v cost %d charge %d", rec.Usage, rec.CostMicro, rec.ChargeMicro)
	}
}

func TestPatchChangesOnlyModel(t *testing.T) {
	up := newFake(t, sseReply(0, fixture(t, "anthropic_stream.sse")))
	e := newEnv(t, "claude-x", []upSpec{{id: "a", protocol: "anthropic", issuer: "iss-a", fake: up, model: "claude-real-1"}})
	resp, body := e.call("anthropic", "/v1/messages", anthropicReq)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	want, _ := sjson.Set(anthropicReq, "model", "claude-real-1")
	if got := string(up.body(0)); got != want {
		t.Errorf("patched body:\n got %s\nwant %s", got, want)
	}
	if tier := e.records.last(t).Attempts[0].Tier; tier != plugin.TierPatch {
		t.Errorf("tier %s", tier)
	}
}

// Cross protocol: an anthropic client served by an openai-chat upstream,
// streaming parallel tool calls back as tool_use blocks.
func TestCrossAnthropicToChatStream(t *testing.T) {
	up := newFake(t, sseReply(0, fixture(t, "chat_stream_tools.sse")))
	e := newEnv(t, "claude-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "iss-o", fake: up, model: "gpt-x"}})
	resp, body := e.call("anthropic", "/v1/messages", anthropicReq, "anthropic-beta", "tools-2024")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d %v: %s", resp.StatusCode, resp.Header, body)
	}
	ub := up.body(0)
	if gjson.GetBytes(ub, "model").Str != "gpt-x" || gjson.GetBytes(ub, "tools.0.function.name").Str != "Bash" ||
		!gjson.GetBytes(ub, "stream").Bool() || up.paths[0] != "/v1/chat/completions" {
		t.Errorf("upstream request %s %s", up.paths[0], ub)
	}
	if up.headers[0].Get("Anthropic-Beta") != "" {
		t.Error("source-protocol header forwarded across protocols")
	}
	s := string(body)
	for _, want := range []string{"event: message_start", `"type":"tool_use"`, `"name":"get_weather"`, "input_json_delta", `"stop_reason":"tool_use"`, "event: message_stop"} {
		if !strings.Contains(s, want) {
			t.Errorf("client stream lacks %s:\n%s", want, s)
		}
	}
	if strings.Count(s, "event: content_block_start") != 2 {
		t.Errorf("want two tool blocks:\n%s", s)
	}
	rec := e.records.last(t)
	if rec.Attempts[0].Tier != plugin.TierConvert || rec.Usage.Input != 57 || rec.Usage.Output != 48 {
		t.Errorf("record %+v usage %+v", rec.Attempts[0], rec.Usage)
	}
}

const responsesReq = `{"model":"gpt-x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
	`"reasoning":{"effort":"high"},"include":["reasoning.encrypted_content"],"max_output_tokens":2000}`

const anthropicThinkingResp = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-real",` +
	`"content":[{"type":"thinking","thinking":"Plan it.","signature":"SIG-A-123"},{"type":"text","text":"Hello."}],` +
	`"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5}}`

// Hard rule 7: upstream-private state is sealed for a client of another
// protocol, restored only for the same issuer and dropped for another.
func TestSealedReasoningRoundTrip(t *testing.T) {
	upA := newFake(t, jsonReply(200, anthropicThinkingResp))
	e := newEnv(t, "gpt-x", []upSpec{{id: "a", protocol: "anthropic", issuer: "iss-a", fake: upA, model: "claude-real"}})
	resp, body := e.call("openai-responses", "/v1/responses", responsesReq)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var reasoning gjson.Result
	gjson.GetBytes(body, "output").ForEach(func(_, it gjson.Result) bool {
		if it.Get("type").Str == "reasoning" {
			reasoning = it
		}
		return true
	})
	enc := reasoning.Get("encrypted_content").Str
	if !strings.HasPrefix(enc, "prs1.") || strings.Contains(string(body), "SIG-A-123") {
		t.Fatalf("reasoning not sealed: %s", body)
	}
	if gjson.GetBytes(upA.body(0), "thinking.budget_tokens").Int() == 0 {
		t.Errorf("effort not mapped to a budget: %s", upA.body(0))
	}

	// Next turn echoes the reasoning item back.
	input := `[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` + reasoning.Raw +
		`,{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello."}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}]`
	turn2, _ := sjson.SetRaw(responsesReq, "input", input)

	if resp, body := e.call("openai-responses", "/v1/responses", turn2); resp.StatusCode != 200 {
		t.Fatalf("turn 2 status %d: %s", resp.StatusCode, body)
	}
	sent := upA.body(1)
	if gjson.GetBytes(sent, `messages.1.content.#(type=="thinking").signature`).Str != "SIG-A-123" {
		t.Errorf("same issuer did not get its signature back: %s", sent)
	}

	// Another issuer: the state is dropped, not sent and not an error.
	upB := newFake(t, jsonReply(200, anthropicThinkingResp))
	e2 := newEnv(t, "gpt-x", []upSpec{{id: "b", protocol: "anthropic", issuer: "iss-b", fake: upB, model: "claude-real"}})
	e2.p.d.Sealer = e.p.d.Sealer
	if resp, body := e2.call("openai-responses", "/v1/responses", turn2); resp.StatusCode != 200 {
		t.Fatalf("other issuer status %d: %s", resp.StatusCode, body)
	}
	if b := string(upB.body(0)); strings.Contains(b, "SIG-A-123") || strings.Contains(b, `"thinking","thinking"`) || strings.Contains(b, "prs1.") {
		t.Errorf("foreign state reached another issuer: %s", b)
	}
}

// Hard rule 2 and design rule 4: a 429 before any content moves to the
// next candidate; the failed one is cooled down.
func TestRetryOn429ThenSuccess(t *testing.T) {
	bad := newFake(t, jsonReply(429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, "Retry-After", "30"))
	good := newFake(t, sseReply(0, fixture(t, "anthropic_stream.sse")))
	e := newEnv(t, "claude-x", []upSpec{
		{id: "bad", protocol: "anthropic", issuer: "i", fake: bad, priority: 10},
		{id: "good", protocol: "anthropic", issuer: "i", fake: good, priority: 5},
	})
	resp, body := e.call("anthropic", "/v1/messages", anthropicReq)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "message_stop") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if strings.Count(string(body), "event: message_start") != 1 {
		t.Errorf("stream opened twice:\n%s", body)
	}
	rec := e.records.last(t)
	if len(rec.Attempts) != 2 || rec.Attempts[0].Cause != plugin.CauseRateLimit || rec.Attempts[0].Status != 429 ||
		rec.Attempts[1].UpstreamID != "good" {
		t.Fatalf("attempts %+v", rec.Attempts)
	}
	if rec.Attempts[0].CostMicro != 0 {
		t.Errorf("a rejected attempt was costed: %d", rec.Attempts[0].CostMicro)
	}
	// The cooled credential is skipped on the next request.
	e.call("anthropic", "/v1/messages", anthropicReq)
	if bad.calls() != 1 {
		t.Errorf("cooling credential called again: %d", bad.calls())
	}
}

// Hard rules 1 and 2: once content went out the request never moves to
// another upstream, and a cut stream ends with the protocol's error event.
func TestNoRetryAfterContent(t *testing.T) {
	cut := newFake(t, sseReply(0,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-x\",\"content\":[],\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial answer\"}}\n\n"))
	other := newFake(t, sseReply(0, fixture(t, "anthropic_stream.sse")))
	e := newEnv(t, "claude-x", []upSpec{
		{id: "cut", protocol: "anthropic", issuer: "i", fake: cut, priority: 10},
		{id: "other", protocol: "anthropic", issuer: "i", fake: other, priority: 5},
	})
	_, body := e.call("anthropic", "/v1/messages", anthropicReq)
	s := string(body)
	if other.calls() != 0 {
		t.Fatal("retried on another upstream after content")
	}
	if !strings.Contains(s, "partial answer") || !strings.HasSuffix(strings.TrimSpace(s), "}") || !strings.Contains(s, "event: error") {
		t.Fatalf("want the partial content then an error event:\n%s", s)
	}
	if strings.Contains(s, "message_stop") || strings.Count(s, "event: message_start") != 1 {
		t.Errorf("stream faked a clean end or reopened:\n%s", s)
	}
	rec := e.records.last(t)
	if rec.ErrorKind == "" || len(rec.Attempts) != 1 || rec.Usage.Input != 100 {
		t.Errorf("record %+v", rec)
	}
}

// Hard rule 19: an attempt that reached the upstream and broke before
// content is costed (estimated when no usage came); only the delivered
// attempt is charged.
func TestCostRecordedForFailedSentAttempt(t *testing.T) {
	cut := newFake(t, sseReply(0, "event: ping\ndata: {\"type\":\"ping\"}\n\n"))
	good := newFake(t, sseReply(0, fixture(t, "anthropic_stream.sse")))
	e := newEnv(t, "claude-x", []upSpec{
		{id: "cut", protocol: "anthropic", issuer: "i", fake: cut, priority: 10},
		{id: "good", protocol: "anthropic", issuer: "i", fake: good, priority: 5},
	})
	resp, body := e.call("anthropic", "/v1/messages", anthropicReq)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "message_stop") || strings.Contains(string(body), `"ping"}`+"\n\nevent: ping") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	rec := e.records.last(t)
	if len(rec.Attempts) != 2 {
		t.Fatalf("attempts %+v", rec.Attempts)
	}
	first := rec.Attempts[0]
	if !first.Sent || !first.Usage.Estimated || first.Usage.Input == 0 || first.CostMicro == 0 {
		t.Errorf("failed sent attempt not costed: %+v", first)
	}
	if rec.CostMicro != first.CostMicro+rec.Attempts[1].CostMicro || rec.ChargeMicro != rec.Attempts[1].CostMicro {
		t.Errorf("cost %d charge %d attempts %+v", rec.CostMicro, rec.ChargeMicro, rec.Attempts)
	}
}

// Hard rule 10: no upstream byte for KeepaliveDelay commits the stream
// and sends keepalives; a failure after that may still retry elsewhere
// because keepalives are not content.
func TestKeepaliveBeforeFirstByteThenRetry(t *testing.T) {
	slowFail := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		time.Sleep(250 * time.Millisecond)
		jsonReply(500, `{"type":"error","error":{"type":"api_error","message":"boom"}}`)(w, r, nil)
	})
	good := newFake(t, sseReply(0, fixture(t, "anthropic_stream.sse")))
	e := newEnv(t, "claude-x", []upSpec{
		{id: "slow", protocol: "anthropic", issuer: "i", fake: slowFail, priority: 10},
		{id: "good", protocol: "anthropic", issuer: "i", fake: good, priority: 5},
	}, func(d *Deps) { d.KeepaliveDelay, d.Keepalive = 60*time.Millisecond, 50*time.Millisecond })
	resp, body := e.call("anthropic", "/v1/messages", anthropicReq)
	s := string(body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	i := strings.Index(s, "event: message_start")
	if i < 0 || !strings.Contains(s[:i], "event: ping") || !strings.Contains(s, "message_stop") {
		t.Fatalf("want keepalives before the stream, then the retried stream:\n%s", s)
	}
	if n := len(e.records.last(t).Attempts); n != 2 {
		t.Errorf("attempts %d", n)
	}
}

// After commit a final failure can only be an in-stream error event.
func TestKeepaliveThenFailureIsStreamError(t *testing.T) {
	slowFail := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		time.Sleep(200 * time.Millisecond)
		jsonReply(400, `{"error":{"message":"bad things","type":"invalid_request_error"}}`)(w, r, nil)
	})
	e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: slowFail}},
		func(d *Deps) { d.KeepaliveDelay, d.Keepalive = 50*time.Millisecond, 40*time.Millisecond })
	resp, body := e.call("openai-chat", "/v1/chat/completions",
		`{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	s := string(body)
	if resp.StatusCode != 200 || !strings.HasPrefix(s, ": keepalive") || !strings.Contains(s, "bad things") {
		t.Fatalf("status %d:\n%s", resp.StatusCode, s)
	}
}

// Hard rule 9: a same-protocol upstream error passes through verbatim with
// its retry and rate-limit headers.
func Test429PassthroughVerbatim(t *testing.T) {
	errBody := `{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"}}`
	up := newFake(t, jsonReply(429, errBody, "Retry-After", "7", "Anthropic-Ratelimit-Requests-Remaining", "0", "X-Should-Retry", "true"))
	e := newEnv(t, "claude-x", []upSpec{{id: "a", protocol: "anthropic", issuer: "i", fake: up}})
	resp, body := e.call("anthropic", "/v1/messages", strings.Replace(anthropicReq, `"stream":true`, `"stream":false`, 1))
	if resp.StatusCode != 429 || string(body) != errBody || resp.Header.Get("Retry-After") != "7" ||
		resp.Header.Get("Anthropic-Ratelimit-Requests-Remaining") != "0" || resp.Header.Get("X-Should-Retry") != "true" {
		t.Fatalf("status %d %v: %s", resp.StatusCode, resp.Header, body)
	}
}

// Hard rule 8: across protocols the client gets its own protocol's error,
// keeping the upstream's retry hint.
func Test429CrossProtocolNative(t *testing.T) {
	up := newFake(t, jsonReply(429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`, "Retry-After", "7"))
	e := newEnv(t, "gpt-x", []upSpec{{id: "a", protocol: "anthropic", issuer: "i", fake: up}})
	resp, body := e.call("openai-chat", "/v1/chat/completions", `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "7" || gjson.GetBytes(body, "error.message").Str == "" ||
		gjson.GetBytes(body, "type").Str == "error" {
		t.Fatalf("status %d %v: %s", resp.StatusCode, resp.Header, body)
	}
}

// Content rejections are never retried on another credential.
func TestContentViolationNotRetried(t *testing.T) {
	bad := newFake(t, jsonReply(400, `{"error":{"message":"flagged by safety system","type":"invalid_request_error","code":"content_policy_violation"}}`))
	good := newFake(t, jsonReply(200, `{}`))
	e := newEnv(t, "gpt-x", []upSpec{
		{id: "bad", protocol: "openai-chat", issuer: "i", fake: bad, priority: 10},
		{id: "good", protocol: "openai-chat", issuer: "i", fake: good, priority: 5},
	})
	resp, body := e.call("openai-chat", "/v1/chat/completions", `{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 400 || good.calls() != 0 || !strings.Contains(string(body), "content_policy_violation") {
		t.Fatalf("status %d good=%d: %s", resp.StatusCode, good.calls(), body)
	}
	if c := e.records.last(t).Attempts[0].Cause; c != plugin.CauseContent {
		t.Errorf("cause %s", c)
	}
}
