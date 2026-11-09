package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// hookPlugin is a middleware whose hooks log into a shared journal.
type hookPlugin struct {
	id      string
	policy  plugin.FailPolicy
	mutates []string
	log     *journal
	reqErr  error
	reply   *plugin.Reply
	mutate  func(body []byte) []byte
	attempt func(c *plugin.Call, a *plugin.Attempt) error
}

type journal struct {
	mu  sync.Mutex
	got []string
}

func (j *journal) add(s string) { j.mu.Lock(); j.got = append(j.got, s); j.mu.Unlock() }
func (j *journal) String() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return strings.Join(j.got, ",")
}

func (h *hookPlugin) Info() plugin.Info {
	return plugin.Info{ID: h.id, Kind: plugin.KindMiddleware, FailPolicy: h.policy, Mutates: h.mutates}
}

func (h *hookPlugin) OnRequest(*plugin.Call) (*plugin.Reply, error) {
	h.log.add("req:" + h.id)
	return h.reply, h.reqErr
}

func (h *hookPlugin) OnAttempt(c *plugin.Call, a *plugin.Attempt) error {
	h.log.add("attempt:" + h.id + ":" + string(a.Tier))
	if h.attempt != nil {
		return h.attempt(c, a)
	}
	return nil
}

func (h *hookPlugin) OnFinish(_ *plugin.Call, r *plugin.Result) {
	h.log.add(fmt.Sprintf("finish:%s:%d", h.id, r.Status))
}

func (h *hookPlugin) MutateBody(_ *plugin.Call, _ *plugin.Attempt, body []byte) ([]byte, error) {
	if h.mutate == nil {
		return body, nil
	}
	return h.mutate(body), nil
}

// fakeMeter counts its calls.
type fakeMeter struct {
	mu                      sync.Mutex
	reserve, settle, refund int
	charged                 int64
	reserveErr              error
}

func (m *fakeMeter) Info() plugin.Info { return plugin.Info{ID: "meter.fake", Kind: plugin.KindMeter} }
func (m *fakeMeter) Reserve(*plugin.Call, int64) (*plugin.Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reserve++
	return &plugin.Reservation{ID: "r"}, m.reserveErr
}
func (m *fakeMeter) Settle(_ *plugin.Call, _ *plugin.Reservation, _ ir.Usage, cost int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settle++
	m.charged = cost
	return nil
}
func (m *fakeMeter) Refund(*plugin.Call, *plugin.Reservation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refund++
	return nil
}
func (m *fakeMeter) Headers(*plugin.Call) http.Header {
	return http.Header{"X-Quota-Used-Percent": {"42"}}
}

const chatReq = `{"model":"gpt-x","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`
const chatResp = `{"id":"c1","object":"chat.completion","model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`

func chatEnv(t *testing.T, ups ...*fakeUpstream) *env {
	var specs []upSpec
	for i, u := range ups {
		specs = append(specs, upSpec{id: fmt.Sprintf("u%d", i), protocol: "openai-chat", issuer: "i", fake: u, priority: 10 - i})
	}
	return newEnv(t, "gpt-x", specs)
}

func TestMeterSettleOrRefundExactlyOnce(t *testing.T) {
	up := newFake(t, jsonReply(200, chatResp))
	e := chatEnv(t, up)
	m := &fakeMeter{}
	e.chain.Meter = m
	resp, _ := e.call("openai-chat", "/v1/chat/completions", chatReq)
	if resp.StatusCode != 200 || resp.Header.Get("X-Quota-Used-Percent") != "42" {
		t.Fatalf("status %d %v", resp.StatusCode, resp.Header)
	}
	if m.reserve != 1 || m.settle != 1 || m.refund != 0 || m.charged == 0 || m.charged != e.records.last(t).ChargeMicro {
		t.Errorf("success: %+v", m)
	}

	up.handle = jsonReply(500, `{"error":{"message":"down"}}`)
	m2 := &fakeMeter{}
	e.chain.Meter = m2
	resp, _ = e.call("openai-chat", "/v1/chat/completions", chatReq)
	if resp.StatusCode != 500 || m2.reserve != 1 || m2.settle != 0 || m2.refund != 1 {
		t.Errorf("failure: status %d %+v", resp.StatusCode, m2)
	}

	m3 := &fakeMeter{reserveErr: &ir.GwError{Kind: ir.ErrQuotaExhausted, Message: "monthly quota used"}}
	e.chain.Meter = m3
	calls := up.calls()
	resp, body := e.call("anthropic", "/v1/messages", `{"model":"gpt-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 429 || resp.Header.Get("X-Should-Retry") != "false" || up.calls() != calls || m3.refund != 0 {
		t.Errorf("quota: status %d %v %s", resp.StatusCode, resp.Header, body)
	}
}

func TestFinishHooksReverseOrder(t *testing.T) {
	up := newFake(t, jsonReply(200, chatResp))
	e := chatEnv(t, up)
	j := &journal{}
	e.chain.Plugins = []plugin.Plugin{&hookPlugin{id: "a", log: j}, &hookPlugin{id: "b", log: j}, &hookPlugin{id: "c", log: j}}
	e.call("openai-chat", "/v1/chat/completions", chatReq)
	want := "req:a,req:b,req:c,attempt:a:passthrough,attempt:b:passthrough,attempt:c:passthrough,finish:c:200,finish:b:200,finish:a:200"
	if j.String() != want {
		t.Errorf("order\n got %s\nwant %s", j, want)
	}

	// A reply short-circuits: later plugins and the upstream never run.
	j2 := &journal{}
	reply := &plugin.Reply{Response: &ir.Response{ID: "cached", Model: "gpt-x", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockText, Text: "from cache"}}}}
	e.chain.Plugins = []plugin.Plugin{&hookPlugin{id: "a", log: j2}, &hookPlugin{id: "b", log: j2, reply: reply}, &hookPlugin{id: "c", log: j2}}
	calls := up.calls()
	resp, body := e.call("openai-chat", "/v1/chat/completions", chatReq)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "from cache") || up.calls() != calls {
		t.Fatalf("reply: %d %s", resp.StatusCode, body)
	}
	if j2.String() != "req:a,req:b,finish:b:200,finish:a:200" {
		t.Errorf("short-circuit order %s", j2)
	}
	// Streamed replies are rendered as a stream.
	_, body = e.call("anthropic", "/v1/messages", `{"model":"gpt-x","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(string(body), "from cache") || !strings.Contains(string(body), "message_stop") {
		t.Errorf("streamed reply: %s", body)
	}
}

func TestFailPolicy(t *testing.T) {
	up := newFake(t, jsonReply(200, chatResp))
	e := chatEnv(t, up)
	j := &journal{}

	e.chain.Plugins = []plugin.Plugin{&hookPlugin{id: "open", policy: plugin.FailOpen, log: j, reqErr: errors.New("crashed")}}
	if resp, body := e.call("openai-chat", "/v1/chat/completions", chatReq); resp.StatusCode != 200 {
		t.Errorf("open policy blocked: %d %s", resp.StatusCode, body)
	}

	e.chain.Plugins = []plugin.Plugin{&hookPlugin{id: "closed", log: j, reqErr: fmt.Errorf("timeout: %w", plugin.ErrPluginFailure)}}
	calls := up.calls()
	resp, body := e.call("openai-chat", "/v1/chat/completions", chatReq)
	if resp.StatusCode != 500 || up.calls() != calls || !strings.Contains(string(body), "plugin closed failed") {
		t.Errorf("closed policy: %d %s", resp.StatusCode, body)
	}

	// A deliberate rejection is sent as is, whatever the policy.
	e.chain.Plugins = []plugin.Plugin{&hookPlugin{id: "guard", policy: plugin.FailOpen, log: j,
		reqErr: &ir.GwError{Kind: ir.ErrPermission, Message: "blocked by guardrail"}}}
	resp, body = e.call("openai-chat", "/v1/chat/completions", chatReq)
	if resp.StatusCode != 403 || !strings.Contains(string(body), "blocked by guardrail") {
		t.Errorf("rejection: %d %s", resp.StatusCode, body)
	}
}

// A declared mutator upgrades passthrough to patch and may only touch its
// declared paths.
func TestBodyMutatorDeclaredPathsOnly(t *testing.T) {
	up := newFake(t, jsonReply(200, chatResp))
	e := chatEnv(t, up)
	j := &journal{}
	e.chain.Plugins = []plugin.Plugin{&hookPlugin{id: "cap", log: j, mutates: []string{"temperature"},
		mutate: func(b []byte) []byte {
			b, _ = sjson.SetBytes(b, "temperature", 0.2)
			b, _ = sjson.SetBytes(b, "model", "sneaky")
			return b
		}}}
	e.call("openai-chat", "/v1/chat/completions", chatReq)
	want, _ := sjson.Set(chatReq, "temperature", 0.2)
	if got := string(up.body(0)); got != want {
		t.Errorf("mutated body\n got %s\nwant %s", got, want)
	}
	if tier := e.records.last(t).Attempts[0].Tier; tier != plugin.TierPatch {
		t.Errorf("tier %s", tier)
	}
	// A mutator that changes nothing leaves the attempt a byte-exact passthrough.
	e.chain.Plugins = []plugin.Plugin{&hookPlugin{id: "noop", log: j, mutates: []string{"temperature"}}}
	e.call("openai-chat", "/v1/chat/completions", chatReq)
	if tier := e.records.last(t).Attempts[0].Tier; tier != plugin.TierPassthrough || string(up.body(1)) != chatReq {
		t.Errorf("noop mutator: tier %s body %s", tier, up.body(1))
	}
}

// In the convert tier attempt hooks run before encoding and edit the IR.
func TestAttemptHookEditsIRInConvertTier(t *testing.T) {
	up := newFake(t, jsonReply(200, anthropicThinkingResp))
	e := newEnv(t, "gpt-x", []upSpec{{id: "a", protocol: "anthropic", issuer: "i", fake: up}})
	j := &journal{}
	e.chain.Plugins = []plugin.Plugin{&hookPlugin{id: "cap", log: j, attempt: func(c *plugin.Call, a *plugin.Attempt) error {
		if a.Tier != plugin.TierConvert {
			return nil
		}
		req, err := c.Request()
		if err != nil {
			return err
		}
		n := 77
		req.MaxOutputTokens = &n
		return nil
	}}}
	resp, body := e.call("openai-chat", "/v1/chat/completions", `{"model":"gpt-x","max_completion_tokens":5000,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Hello.") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if got := gjson.GetBytes(up.body(0), "max_tokens").Int(); got != 77 {
		t.Errorf("max_tokens %d: %s", got, up.body(0))
	}
}

// fakeSender is a provider that performs the call itself.
type fakeSender struct {
	plugin.Provider
	events []ir.Event
}

func (f *fakeSender) Send(_ context.Context, _ *plugin.Outgoing, req *ir.Request, emit func(*ir.Event) error) (*plugin.Outcome, error) {
	if req == nil || len(req.Messages) == 0 {
		return &plugin.Outcome{Status: 400, Sent: true}, nil
	}
	for i := range f.events {
		if err := emit(&f.events[i]); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func TestSenderProvider(t *testing.T) {
	up := newFake(t, jsonReply(500, `{}`))
	e := newEnv(t, "claude-x", []upSpec{{id: "s", protocol: "anthropic", issuer: "i", fake: up}})
	u := e.snap.Upstreams["s"]
	u.Provider = &fakeSender{Provider: u.Provider, events: responseEvents(&ir.Response{ID: "x", Model: "claude-x",
		StopReason: ir.StopEndTurn, Usage: ir.Usage{Input: 3, Output: 2},
		Content: []ir.Block{{Type: ir.BlockText, Text: "sent by provider"}}})}
	_, body := e.call("anthropic", "/v1/messages", anthropicReq)
	if !strings.Contains(string(body), "sent by provider") || !strings.Contains(string(body), "message_stop") || up.calls() != 0 {
		t.Fatalf("stream: %s", body)
	}
	_, body = e.call("anthropic", "/v1/messages", strings.Replace(anthropicReq, `"stream":true`, `"stream":false`, 1))
	if gjson.GetBytes(body, "content.0.text").Str != "sent by provider" || gjson.GetBytes(body, "usage.output_tokens").Int() != 2 {
		t.Fatalf("full: %s", body)
	}
}
