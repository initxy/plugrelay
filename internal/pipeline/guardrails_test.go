package pipeline

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/middleware/guardrails"
)

// guardEnv runs middleware.guardrails with fixed settings.
func guardEnv(t *testing.T, set map[string]any, ups ...upSpec) *env {
	t.Helper()
	settings := func(_ *plugin.Caller, id, key string) plugin.Value {
		v, ok := set[key]
		if id != guardrails.ID || !ok {
			return plugin.Value{}
		}
		b, _ := json.Marshal(v)
		return plugin.NewValue(b)
	}
	e := newEnv(t, ups[0].model, ups, func(d *Deps) { d.Settings = settings })
	e.chain.Plugins = []plugin.Plugin{guardrails.New()}
	return e
}

func notesOf(rec *plugin.Record) []string {
	var out []string
	for _, a := range rec.Attempts {
		if a.Diagnostic != nil {
			out = append(out, a.Diagnostic.Notes...)
		}
	}
	return out
}

func TestGuardrailsStreamCut(t *testing.T) {
	up := newFake(t, sseReply(0,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-x\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"the key is sk-abcdefgh\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ijklmnopqrstuv, keep it\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" NEVER-SENT\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	for _, client := range []string{"anthropic", "openai-chat"} {
		t.Run(client, func(t *testing.T) {
			e := guardEnv(t, map[string]any{"detect": []string{"secret"}, "check_response": true},
				upSpec{id: "a", protocol: "anthropic", issuer: "i", fake: up, model: "claude-x"})
			path, body := "/v1/messages", anthropicReq
			if client == "openai-chat" {
				path, body = "/v1/chat/completions", `{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			}
			_, out := e.call(client, path, body)
			s := string(out)
			if strings.Contains(s, "NEVER-SENT") || strings.Contains(s, "message_stop") || strings.Contains(s, "[DONE]") ||
				strings.Contains(s, "sk-abcdefgh") {
				t.Fatalf("stream not stopped, or the secret's start left:\n%s", s)
			}
			if !strings.Contains(s, "error") || !strings.Contains(s, "blocked by guardrails (secret)") {
				t.Fatalf("no protocol error event:\n%s", s)
			}
			rec := e.waitRecords(1)
			if !slices.Contains(notesOf(rec), "guardrails: block stream secret") || rec.ErrorKind != "content_filtered" {
				t.Fatalf("record: %+v notes %v", rec, notesOf(rec))
			}
		})
	}
}

// TestGuardrailsStreamHoldsKeyword: in block mode a keyword never reaches
// the client, whether one frame carries it whole or frames split it, while
// a stream the keyword never completes arrives in full.
func TestGuardrailsStreamHoldsKeyword(t *testing.T) {
	stream := func(texts ...string) *fakeUpstream {
		frames := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-x\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		}
		for _, x := range texts {
			frames = append(frames, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\""+x+"\"}}\n\n")
		}
		frames = append(frames, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		return newFake(t, sseReply(0, frames...))
	}
	for _, client := range []string{"anthropic", "openai-chat"} {
		run := func(up *fakeUpstream) string {
			e := guardEnv(t, map[string]any{"deny_keywords": []string{"forbidden-word"}, "check_response": true},
				upSpec{id: "a", protocol: "anthropic", issuer: "i", fake: up, model: "claude-x"})
			path, body := "/v1/messages", anthropicReq
			if client == "openai-chat" {
				path, body = "/v1/chat/completions", `{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			}
			_, out := e.call(client, path, body)
			e.waitRecords(1)
			return string(out)
		}
		t.Run(client, func(t *testing.T) {
			for _, texts := range [][]string{{"say ", "forbidden-word", " ok"}, {"say ", "forbid", "den-word ok"}} {
				s := run(stream(texts...))
				if strings.Contains(s, "forbid") || strings.Contains(s, " ok") || !strings.Contains(s, "blocked by guardrails (keyword)") {
					t.Fatalf("%q: keyword left or no error:\n%s", texts, s)
				}
			}
			s := run(stream("say ", "forbid", "s nothing"))
			if !strings.Contains(s, "forbid") || !strings.Contains(s, "s nothing") || strings.Contains(s, "guardrails") {
				t.Fatalf("clean stream not delivered in full:\n%s", s)
			}
		})
	}
}

// An upstream error event while guardrails hold text: the held text was
// never judged and is dropped, and the client still gets the error event.
func TestGuardrailsHeldThenUpstreamError(t *testing.T) {
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-x\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello there. \"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"more text forb\"}}\n\n",
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"UPSTREAM-OVERLOADED\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"after the error\"}}\n\n",
	}
	for _, client := range []string{"anthropic", "openai-chat"} {
		t.Run(client, func(t *testing.T) {
			e := guardEnv(t, map[string]any{"deny_keywords": []string{"forbidden-word"}, "check_response": true},
				upSpec{id: "a", protocol: "anthropic", issuer: "i", fake: newFake(t, sseReply(0, frames...)), model: "claude-x"})
			path, body := "/v1/messages", anthropicReq
			if client == "openai-chat" {
				path, body = "/v1/chat/completions", `{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			}
			_, out := e.call(client, path, body)
			rec := e.waitRecords(1)
			s := string(out)
			if !strings.Contains(s, "hello there") || strings.Contains(s, "forb") || strings.Contains(s, "after the error") ||
				!strings.Contains(s, "UPSTREAM-OVERLOADED") || rec.Error != "upstream stream failed" {
				t.Fatalf("record %q, client got:\n%s", rec.Error, s)
			}
		})
	}
}

func TestGuardrailsRequestAndResponse(t *testing.T) {
	up := newFake(t, jsonReply(200, `{"id":"c1","object":"chat.completion","model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":"write to bob@example.com"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	spec := upSpec{id: "u", protocol: "openai-chat", issuer: "i", fake: up, model: "gpt-x"}

	// Request block: the upstream is never called; the client error is native.
	e := guardEnv(t, map[string]any{"detect": []string{"email"}}, spec)
	resp, body := e.call("openai-chat", "/v1/chat/completions", `{"model":"gpt-x","messages":[{"role":"user","content":"I am a@b.io"}]}`)
	if resp.StatusCode != 400 || gjson.GetBytes(body, "error.code").Str != "content_filter" || up.calls() != 0 {
		t.Fatalf("request block: %d %s calls=%d", resp.StatusCode, body, up.calls())
	}
	if rec := e.waitRecords(1); !strings.Contains(rec.Error, "email") {
		t.Fatalf("record error %q", rec.Error)
	}

	// Redact: the upstream gets masked user text (patch tier), the verdict is noted.
	e = guardEnv(t, map[string]any{"detect": []string{"email"}, "action": "redact"}, spec)
	resp, _ = e.call("openai-chat", "/v1/chat/completions", `{"model":"gpt-x","messages":[{"role":"user","content":"I am a@b.io"}]}`)
	if resp.StatusCode != 200 || gjson.GetBytes(up.body(0), "messages.0.content").Str != "I am [REDACTED:email]" {
		t.Fatalf("redact: %d %s", resp.StatusCode, up.body(0))
	}
	rec := e.waitRecords(1)
	if rec.Attempts[0].Tier != plugin.TierPatch || !slices.Contains(notesOf(rec), "guardrails: redact request email") {
		t.Fatalf("redact record %+v %v", rec.Attempts, notesOf(rec))
	}

	// Response block on a complete response: nothing retried, nothing charged.
	e = guardEnv(t, map[string]any{"detect": []string{"email"}, "check_response": true}, spec)
	calls := up.calls()
	resp, body = e.call("openai-chat", "/v1/chat/completions", chatReq)
	if resp.StatusCode != 400 || strings.Contains(string(body), "bob@") || up.calls() != calls+1 {
		t.Fatalf("response block: %d %s", resp.StatusCode, body)
	}
	rec = e.waitRecords(1)
	if rec.ChargeMicro != 0 || rec.CostMicro == 0 || !slices.Contains(notesOf(rec), "guardrails: block response email") {
		t.Fatalf("response block record: charge %d cost %d notes %v", rec.ChargeMicro, rec.CostMicro, notesOf(rec))
	}

	// Response redact in passthrough re-encodes the masked response.
	e = guardEnv(t, map[string]any{"detect": []string{"email"}, "check_response": true, "action": "redact"}, spec)
	resp, body = e.call("openai-chat", "/v1/chat/completions", chatReq)
	if resp.StatusCode != 200 || gjson.GetBytes(body, "choices.0.message.content").Str != "write to [REDACTED:email]" {
		t.Fatalf("response redact: %d %s", resp.StatusCode, body)
	}

	// Flag: bytes untouched, verdict recorded.
	e = guardEnv(t, map[string]any{"detect": []string{"email"}, "check_response": true, "action": "flag"}, spec)
	resp, body = e.call("openai-chat", "/v1/chat/completions", chatReq)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "bob@example.com") {
		t.Fatalf("flag: %d %s", resp.StatusCode, body)
	}
	if rec := e.waitRecords(1); !slices.Contains(notesOf(rec), "guardrails: flag response email") {
		t.Fatalf("flag notes %v", notesOf(rec))
	}
}

// TestKeepDeclaredIndexWildcard: a declared path with "#" covers every
// index, and only the declared fields change.
func TestKeepDeclaredIndexWildcard(t *testing.T) {
	orig := []byte(`{"model":"m","messages":[{"role":"user","content":"a@b.io"},{"role":"user","content":[{"type":"text","text":"x"},{"type":"image","url":"u"}]}]}`)
	edited := []byte(`{"model":"EDITED","messages":[{"role":"user","content":"[R]"},{"role":"user","content":[{"type":"text","text":"[R2]"},{"type":"image","url":"u"}]}]}`)
	out, changed := keepDeclared(orig, edited, []string{"messages.#.content.#.text", "messages.#.content"})
	want := `{"model":"m","messages":[{"role":"user","content":"[R]"},{"role":"user","content":[{"type":"text","text":"[R2]"},{"type":"image","url":"u"}]}]}`
	if !changed || string(out) != want {
		t.Fatalf("got %s", out)
	}
}

// TestKeepDeclaredLargeArray: a "#" path walks each array once, so a body
// with tens of thousands of messages stays cheap whether or not a mutator
// touched it.
func TestKeepDeclaredLargeArray(t *testing.T) {
	const n = 32000
	body := []byte(`{"model":"m","messages":[` + strings.TrimSuffix(strings.Repeat(`{"role":"user","content":"a"},`, n), ",") + `]}`)
	paths := []string{"messages.#.content", "messages.#.content.#.text", "input.#.content", "contents.#.parts.#.text", "prompt"}
	edited := bytes.Replace(body, []byte(`"content":"a"`), []byte(`"content":"b"`), 1)
	all := bytes.ReplaceAll(body, []byte(`"content":"a"`), []byte(`"content":"c"`))
	start := time.Now()
	if _, ch := keepDeclared(body, bytes.Clone(body), paths); ch {
		t.Fatal("unchanged body reported changed")
	}
	out, ch := keepDeclared(body, edited, paths)
	if !ch || !bytes.Equal(out, edited) {
		t.Fatal("single edit not kept")
	}
	out, ch = keepDeclared(body, all, paths)
	if !ch || !bytes.Equal(out, all) {
		t.Fatal("full edit not kept")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("keepDeclared over %d messages took %v", n, d)
	}
}

// TestKeepDeclaredShrinkAndUndeclared: elements dropped by the edit lose
// only the declared field, and undeclared edits inside elements are ignored.
func TestKeepDeclaredShrinkAndUndeclared(t *testing.T) {
	orig := []byte(`{"messages":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`)
	edited := []byte(`{"messages":[{"role":"system","content":"A"}]}`)
	out, ch := keepDeclared(orig, edited, []string{"messages.#.content"})
	if want := `{"messages":[{"role":"user","content":"A"},{"role":"user"}]}`; !ch || string(out) != want {
		t.Fatalf("got %s", out)
	}
}

func BenchmarkKeepDeclared(b *testing.B) {
	body := []byte(`{"model":"m","messages":[` + strings.TrimSuffix(strings.Repeat(`{"role":"user","content":"a"},`, 32000), ",") + `]}`)
	edited := bytes.Replace(body, []byte(`"content":"a"`), []byte(`"content":"b"`), 1)
	for b.Loop() {
		keepDeclared(body, edited, []string{"messages.#.content", "messages.#.content.#.text"})
	}
}
