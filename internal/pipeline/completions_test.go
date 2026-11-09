package pipeline

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/selector/routing"
)

// continueFIM is an editor's inline-completion request: prompt before the
// cursor, suffix after it, streamed with a usage chunk.
const continueFIM = `{"model":"coder","prompt":"def fib(n):\n    ","suffix":"\n\nprint(fib(10))",` +
	`"max_tokens":128,"temperature":0.01,"stop":["\n\n\n"],"stream":true,"stream_options":{"include_usage":true}}`

const completionsStream = "data: {\"id\":\"cmpl-1\",\"object\":\"text_completion\",\"created\":1,\"model\":\"coder\",\"choices\":[{\"text\":\"if n < 2:\",\"index\":0,\"logprobs\":null,\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"cmpl-1\",\"object\":\"text_completion\",\"created\":1,\"model\":\"coder\",\"choices\":[{\"text\":\" return n\",\"index\":0,\"logprobs\":null,\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"id\":\"cmpl-1\",\"object\":\"text_completion\",\"created\":1,\"model\":\"coder\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":6,\"total_tokens\":18}}\n\n" +
	"data: [DONE]\n\n"

const chatTextStream = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"if n < 2:\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" return n\"},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-x\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":6,\"total_tokens\":18}}\n\n" +
	"data: [DONE]\n\n"

// A fill-in-the-middle request to a completions endpoint passes through
// untouched in both directions.
func TestCompletionsFIMPassthrough(t *testing.T) {
	up := newFake(t, sseReply(0, completionsStream))
	e := newEnv(t, "coder", []upSpec{{id: "d", protocol: "openai-completions", issuer: "iss-d", fake: up, caps: "{fill_in_middle: true}"}})
	resp, body := e.call("openai-completions", "/v1/completions", continueFIM, "Authorization", "Bearer client-key")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if got := string(up.body(0)); got != continueFIM {
		t.Errorf("upstream body changed:\n%s", got)
	}
	if up.paths[0] != "/v1/completions" || up.headers[0].Get("Authorization") != "Bearer sk-d" {
		t.Errorf("upstream %s %v", up.paths[0], up.headers[0])
	}
	if string(body) != completionsStream {
		t.Errorf("client stream differs:\n%s", body)
	}
	rec := e.records.last(t)
	if rec.Protocol != "openai-completions" || rec.Attempts[0].Tier != plugin.TierPassthrough || rec.Usage.Input != 12 || rec.Usage.Output != 6 {
		t.Errorf("record %+v attempts %+v", rec, rec.Attempts)
	}
}

// A suffix is never sent where it cannot be honoured: an endpoint that
// does not declare fill_in_middle, or one of another protocol, is not
// routed and the client gets a capability error in its own format.
func TestCompletionsFIMNeedsCapability(t *testing.T) {
	for name, spec := range map[string]upSpec{
		"completions without fim": {id: "v", protocol: "openai-completions", issuer: "iss-v", caps: "{fill_in_middle: false}"},
		"chat":                    {id: "o", protocol: "openai-chat", issuer: "iss-o"},
	} {
		t.Run(name, func(t *testing.T) {
			up := newFake(t, sseReply(0, completionsStream))
			spec.fake = up
			e := newEnv(t, "coder", []upSpec{spec})
			resp, body := e.call("openai-completions", "/v1/completions", continueFIM)
			if resp.StatusCode != 400 || !strings.Contains(gjson.GetBytes(body, "error.message").Str, "fill_in_middle") {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			if up.calls() != 0 {
				t.Error("upstream called")
			}
		})
	}
}

// Without a suffix, a completions client can be served by a chat
// endpoint: the prompt becomes one user message and the answer comes back
// as text_completion chunks, ending with the usage chunk it asked for.
func TestCompletionsToChatStream(t *testing.T) {
	up := newFake(t, sseReply(0, chatTextStream))
	e := newEnv(t, "coder", []upSpec{{id: "o", protocol: "openai-chat", issuer: "iss-o", fake: up, model: "gpt-x"}})
	req := `{"model":"coder","prompt":"// add two numbers\nfunc add(","max_tokens":64,"stop":"\n}","stream":true,"stream_options":{"include_usage":true}}`
	resp, body := e.call("openai-completions", "/v1/completions", req)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	ub := up.body(0)
	if up.paths[0] != "/v1/chat/completions" || gjson.GetBytes(ub, "model").Str != "gpt-x" ||
		gjson.GetBytes(ub, "messages.#").Int() != 1 || gjson.GetBytes(ub, "messages.0.role").Str != "user" ||
		gjson.GetBytes(ub, "messages.0.content").Str != "// add two numbers\nfunc add(" || gjson.GetBytes(ub, "stop.0").Str != "\n}" {
		t.Errorf("upstream request %s %s", up.paths[0], ub)
	}
	var text string
	var finish, usage, done bool
	for _, f := range strings.Split(strings.TrimSpace(string(body)), "\n\n") {
		data := strings.TrimPrefix(f, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		if gjson.Get(data, "object").Str != "text_completion" {
			t.Errorf("chunk %s", data)
		}
		text += gjson.Get(data, "choices.0.text").Str
		finish = finish || gjson.Get(data, "choices.0.finish_reason").Str == "stop"
		usage = usage || gjson.Get(data, "usage.completion_tokens").Int() == 6
	}
	if text != "if n < 2: return n" || !finish || !usage || !done {
		t.Errorf("client stream (text %q finish %v usage %v done %v):\n%s", text, finish, usage, done, body)
	}
	rec := e.records.last(t)
	if a := rec.Attempts[0]; a.Tier != plugin.TierConvert || a.Diagnostic == nil || a.Diagnostic.Fidelity != "lossy" {
		t.Errorf("attempt %+v", a)
	}
}

// A chat conversation is never flattened into a prompt: a model served
// only by a completions endpoint rejects it.
func TestChatNotRoutedToCompletions(t *testing.T) {
	up := newFake(t, sseReply(0, completionsStream))
	e := newEnv(t, "coder", []upSpec{{id: "d", protocol: "openai-completions", issuer: "iss-d", fake: up}})
	resp, body := e.call("openai-chat", "/v1/chat/completions", `{"model":"coder","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "conversation") || up.calls() != 0 {
		t.Fatalf("status %d calls %d: %s", resp.StatusCode, up.calls(), body)
	}
}

// An endpoint that twice answers 404 for the route itself is tried last for
// this operation: the third request goes straight to chat. One answer is
// not enough (a stray gateway page), and a 404 that names a missing model
// says nothing about the route, so it is tried every time.
func TestCompletionsRouteMissingRemembered(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		skip   bool
	}{
		{"fastapi not found", 404, `{"detail":"Not Found"}`, true},
		{"plain page", 404, "404 page not found", true},
		{"method not allowed", 405, ``, true},
		{"model not found", 404, `{"error":{"message":"The model 'coder' does not exist","type":"invalid_request_error","code":"model_not_found"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gen := newFake(t, jsonReply(tc.status, tc.body))
			chat := newFake(t, sseReply(0, chatTextStream))
			e := newEnv(t, "coder", []upSpec{
				{id: "gen", protocol: "openai-completions", issuer: "i", fake: gen, priority: 1},
				{id: "o", protocol: "openai-chat", issuer: "i", fake: chat, model: "gpt-x"},
			})
			for i := range 3 {
				// Different prompts: no session binding pins the second request.
				req := `{"model":"coder","prompt":"line ` + string(rune('a'+i)) + `","max_tokens":8,"stream":true}`
				if resp, body := e.call("openai-completions", "/v1/completions", req); resp.StatusCode != 200 {
					t.Fatalf("request %d: %d %s", i, resp.StatusCode, body)
				}
			}
			want := 3
			if tc.skip {
				want = unservedSightings
			}
			if gen.calls() != want || chat.calls() != 3 {
				t.Errorf("completions endpoint %d calls (want %d), chat %d", gen.calls(), want, chat.calls())
			}
		})
	}
}

// A demoted endpoint is still tried when every other candidate fails, so a
// wrong mark costs a fallback, not the request.
func TestUnservedEndpointTriedLast(t *testing.T) {
	// With the default selector too: it ranks by priority and stickiness,
	// which would put the higher-priority demoted endpoint back in front.
	for name, sel := range map[string]plugin.Selector{"no selector": nil, "default selector": routing.New()} {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var order []string
			note := func(name string) { mu.Lock(); order = append(order, name); mu.Unlock() }
			notFound := jsonReply(404, `{"detail":"Not Found"}`)
			gen := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) { note("gen"); notFound(w, r, b) })
			var chatCalls atomic.Int64
			ok, limited := sseReply(0, chatTextStream), jsonReply(429, `{"error":{"message":"slow down","type":"rate_limit"}}`)
			chat := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
				note("chat")
				if chatCalls.Add(1) <= unservedSightings+1 {
					ok(w, r, b)
					return
				}
				limited(w, r, b)
			})
			e := newEnv(t, "coder", []upSpec{
				{id: "gen", protocol: "openai-completions", issuer: "i", fake: gen, priority: 1},
				{id: "o", protocol: "openai-chat", issuer: "i", fake: chat, model: "gpt-x"},
			})
			if sel != nil {
				e.chain.Selector = sel
			}
			for i := range unservedSightings + 2 {
				req := `{"model":"coder","prompt":"line ` + string(rune('a'+i)) + `","max_tokens":8,"stream":true}`
				resp, _ := e.call("openai-completions", "/v1/completions", req)
				mu.Lock()
				order = append(order, strconv.Itoa(resp.StatusCode))
				mu.Unlock()
			}
			// Two sightings demote the endpoint: the third request goes
			// straight to chat; on the fourth chat fails and the demoted
			// endpoint still gets its turn.
			want := []string{"gen", "chat", "200", "gen", "chat", "200", "chat", "200", "chat", "gen", "404"}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(order, want) {
				t.Fatalf("call order %v, want %v", order, want)
			}
		})
	}
}

func TestRouteMissing(t *testing.T) {
	req := plugin.Verdict{Cause: plugin.CauseRequest}
	cases := []struct {
		status int
		body   string
		v      plugin.Verdict
		want   bool
	}{
		{404, ``, req, true},
		{404, `<html><body>Not Found</body></html>`, req, true},
		{404, `{"error":{"message":"Invalid URL (POST /v1/completions)","type":"invalid_request_error"}}`, req, true},
		{404, `{"error":"Unexpected endpoint or method. (POST /v1/completions)"}`, req, true},
		{405, `{"detail":"Method Not Allowed"}`, req, true},
		{404, `{"error":{"message":"model 'x' not found"}}`, req, false},
		{404, `{"error":{"code":"DeploymentNotFound","message":"The API deployment for this resource does not exist."}}`, req, false},
		{404, `{"error":{"message":"Previous response with id 'resp_1' not found.","type":"invalid_request_error"}}`, req, false},
		{404, `{"detail":"Not Found"}`, plugin.Verdict{Cause: plugin.CauseAuthExpired}, false},
		{400, ``, req, false},
	}
	for _, c := range cases {
		if got := routeMissing(&plugin.Outcome{Status: c.status, Body: []byte(c.body)}, c.v); got != c.want {
			t.Errorf("%d %s %v: got %v", c.status, c.body, c.v.Cause, got)
		}
	}
}
