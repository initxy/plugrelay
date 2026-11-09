package pipeline

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

const partialFrames = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-x\",\"content\":[],\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial answer\"}}\n\n"

// An upstream that breaks after content: refunded, operator cost kept.
func TestUpstreamCutAfterContentRefunds(t *testing.T) {
	up := newFake(t, sseReply(0, partialFrames))
	e := newEnv(t, "claude-x", []upSpec{{id: "a", protocol: "anthropic", issuer: "i", fake: up}})
	m := &fakeMeter{}
	e.chain.Meter = m
	_, body := e.call("anthropic", "/v1/messages", anthropicReq)
	if !strings.Contains(string(body), "event: error") {
		t.Fatalf("no error event: %s", body)
	}
	rec := e.records.last(t)
	if m.settle != 0 || m.refund != 1 || rec.ChargeMicro != 0 || rec.CostMicro == 0 || rec.ErrorKind != ir.ErrUpstream {
		t.Errorf("meter %+v record kind %s cost %d charge %d", m, rec.ErrorKind, rec.CostMicro, rec.ChargeMicro)
	}
}

// A client that leaves mid-stream is charged for what was delivered.
func TestClientDisconnectSettles(t *testing.T) {
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		sseReply(0, partialFrames)(w, r, b)
		<-r.Context().Done()
	})
	e := newEnv(t, "claude-x", []upSpec{{id: "a", protocol: "anthropic", issuer: "i", fake: up}})
	m := &fakeMeter{}
	e.chain.Meter = m
	srv := e.server("anthropic", "/v1/messages")
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req := e.request(srv.URL, "anthropic", "/v1/messages", anthropicReq).WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "partial answer") {
			break
		}
	}
	cancel()
	resp.Body.Close()
	rec := e.waitRecords(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.settle != 1 || m.refund != 0 || rec.ErrorKind != ErrClientClosed || rec.Status != statusClientClosed || rec.ChargeMicro == 0 {
		t.Errorf("meter %+v record kind %s status %d charge %d", m, rec.ErrorKind, rec.Status, rec.ChargeMicro)
	}
}

// A client that leaves before the end event pays for the output it received,
// not the output_tokens: 1 that message_start reported.
func TestClientDisconnectBillsReceivedOutput(t *testing.T) {
	long := strings.Repeat("word ", 200) // 1000 chars
	frames := strings.Replace(partialFrames, "partial answer", long+"partial answer", 1)
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		sseReply(0, frames)(w, r, b)
		<-r.Context().Done()
	})
	e := newEnv(t, "claude-x", []upSpec{{id: "a", protocol: "anthropic", issuer: "i", fake: up}})
	srv := e.server("anthropic", "/v1/messages")
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req := e.request(srv.URL, "anthropic", "/v1/messages", anthropicReq).WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "partial answer") {
			break
		}
	}
	cancel()
	resp.Body.Close()
	rec := e.waitRecords(1)
	if rec.Usage.Output < 200 || !rec.Usage.Estimated || rec.Usage.Input != 100 {
		t.Errorf("usage %+v", rec.Usage)
	}
}

// Hard rule 11: each source of a session id yields the same id on every
// turn, and the binding records the id that actually went upstream.
func TestSessionIDStableAcrossTurns(t *testing.T) {
	ctx := context.Background()
	turn := func(text string) string {
		return `{"model":"claude-x","max_tokens":50,"system":"Be brief.","messages":[{"role":"user","content":"first question"},` +
			`{"role":"assistant","content":"ok"},{"role":"user","content":"` + text + `"}]}`
	}

	t.Run("header", func(t *testing.T) {
		up := newFake(t, jsonReply(200, anthropicThinkingResp))
		e := newEnv(t, "claude-x", []upSpec{{id: "a", protocol: "anthropic", issuer: "i", fake: up}})
		for _, q := range []string{"one", "two"} {
			e.call("anthropic", "/v1/messages", turn(q), "X-Claude-Code-Session-Id", "sess-1")
			if rec := e.records.last(t); rec.SessionID != "k1/sess-1" {
				t.Errorf("session %q", rec.SessionID)
			}
		}
		b, ok := e.rt.Binding(ctx, "k1/sess-1")
		if !ok || b.SentSessionID != "sess-1" || up.headers[1].Get("X-Claude-Code-Session-Id") != "sess-1" {
			t.Errorf("binding %+v %v", b, ok)
		}
	})

	t.Run("cache key", func(t *testing.T) {
		up := newFake(t, jsonReply(200, chatResp))
		e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}})
		for _, q := range []string{"one", "two"} {
			e.call("openai-chat", "/v1/chat/completions",
				`{"model":"gpt-x","prompt_cache_key":"pk-9","messages":[{"role":"user","content":"`+q+`"}]}`)
			if rec := e.records.last(t); rec.SessionID != "k1/pk-9" {
				t.Errorf("session %q", rec.SessionID)
			}
		}
		if b, ok := e.rt.Binding(ctx, "k1/pk-9"); !ok || b.SentSessionID != "pk-9" {
			t.Errorf("binding %+v", b)
		}
	})

	t.Run("prefix hash across protocols", func(t *testing.T) {
		up := newFake(t, jsonReply(200, chatResp))
		e := newEnv(t, "claude-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}})
		var ids []string
		for _, q := range []string{"one", "two"} {
			e.call("anthropic", "/v1/messages", turn(q))
			ids = append(ids, e.records.last(t).SessionID)
		}
		if ids[0] == "" || ids[0] != ids[1] || !strings.HasPrefix(ids[0], "k1/h:") {
			t.Fatalf("ids %v", ids)
		}
		sent0 := gjson.GetBytes(up.body(0), "prompt_cache_key").Str
		sent1 := gjson.GetBytes(up.body(1), "prompt_cache_key").Str
		if sent0 == "" || sent0 != sent1 || strings.Contains(sent0, "k1") {
			t.Errorf("sent session ids %q %q", sent0, sent1)
		}
		if b, ok := e.rt.Binding(ctx, ids[0]); !ok || b.SentSessionID != sent0 {
			t.Errorf("binding %+v, sent %q", b, sent0)
		}
		// Another key never shares the session.
		e.caller.KeyID = "k2"
		e.call("anthropic", "/v1/messages", turn("three"))
		if id := e.records.last(t).SessionID; id == ids[0] {
			t.Errorf("session shared across keys: %s", id)
		}
	})
}

// slowHook overruns the event budget and tries to rewrite text.
type slowHook struct {
	policy plugin.FailPolicy
}

func (s *slowHook) Info() plugin.Info {
	return plugin.Info{ID: "middleware.slow", Kind: plugin.KindMiddleware, FailPolicy: s.policy}
}

func (s *slowHook) OnEvent(_ *plugin.Call, ev *ir.Event) (plugin.EventAction, error) {
	if ev.Kind == ir.EvDelta && ev.Delta != nil && ev.Delta.Kind == ir.DeltaText {
		time.Sleep(20 * time.Millisecond)
		ev.Delta.Text = "REWRITTEN"
	}
	return plugin.EventPass, nil
}

func TestEventHookBudget(t *testing.T) {
	stream := fixture(t, "chat_stream_tools.sse")
	text := "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n"
	up := newFake(t, sseReply(0, text+stream))
	budget := func(d *Deps) { d.EventHookBudget = 5 * time.Millisecond }

	e := newEnv(t, "claude-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}}, budget)
	e.chain.Plugins = []plugin.Plugin{&slowHook{policy: plugin.FailOpen}}
	_, body := e.call("anthropic", "/v1/messages", anthropicReq)
	if s := string(body); !strings.Contains(s, "hello") || strings.Contains(s, "REWRITTEN") || !strings.Contains(s, "message_stop") {
		t.Errorf("open policy: edits of an overrun must be discarded:\n%s", s)
	}

	e2 := newEnv(t, "claude-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}}, budget)
	e2.chain.Plugins = []plugin.Plugin{&slowHook{policy: plugin.FailClosed}}
	_, body = e2.call("anthropic", "/v1/messages", anthropicReq)
	if s := string(body); strings.Contains(s, "message_stop") || !strings.Contains(s, "event: error") {
		t.Errorf("closed policy: overrun must end the stream with an error:\n%s", s)
	}
}

// gatedQueue is a RecordQueue whose Enqueue blocks until gate closes, like
// a full ledger lane.
type gatedQueue struct {
	gate     chan struct{}
	entered  chan struct{}
	recQueue recQueue
}

func (q *gatedQueue) Enqueue(r *plugin.Record) bool {
	close(q.entered)
	<-q.gate
	return q.recQueue.Enqueue(r)
}

// TestPlainResponseNotHeldByRecord: a non-stream response reaches the
// client in full while the record is still waiting to be enqueued.
func TestPlainResponseNotHeldByRecord(t *testing.T) {
	up := newFake(t, jsonReply(200, chatResp))
	q := &gatedQueue{gate: make(chan struct{}), entered: make(chan struct{})}
	e := newEnv(t, "gpt-x", []upSpec{{id: "u0", protocol: "openai-chat", issuer: "i", fake: up}},
		func(d *Deps) { d.Records = q })
	srv := e.server("openai-chat", "/v1/chat/completions")
	defer srv.Close()
	defer close(q.gate)
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.DefaultClient.Do(e.request(srv.URL, "openai-chat", "/v1/chat/completions", chatReq))
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		done <- result{b, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || !gjson.ValidBytes(r.body) {
			t.Fatalf("body %q: %v", r.body, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("response held until the record was enqueued")
	}
}

// TestWaitCoversFinish: Pipeline.Wait returns only after every request has
// enqueued its record, and gives up when its context ends first.
func TestWaitCoversFinish(t *testing.T) {
	up := newFake(t, jsonReply(200, chatResp))
	q := &gatedQueue{gate: make(chan struct{}), entered: make(chan struct{})}
	e := newEnv(t, "gpt-x", []upSpec{{id: "u0", protocol: "openai-chat", issuer: "i", fake: up}},
		func(d *Deps) { d.Records = q })
	if err := e.p.Wait(context.Background()); err != nil {
		t.Fatalf("idle pipeline: %v", err)
	}
	srv := e.server("openai-chat", "/v1/chat/completions")
	defer srv.Close()
	go func() {
		if resp, err := http.DefaultClient.Do(e.request(srv.URL, "openai-chat", "/v1/chat/completions", chatReq)); err == nil {
			resp.Body.Close()
		}
	}()
	<-q.entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := e.p.Wait(ctx); err == nil {
		t.Fatal("Wait returned while a request was still finishing")
	}
	close(q.gate)
	if err := e.p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if q.recQueue.last(t).Status != 200 {
		t.Fatal("record not enqueued before Wait returned")
	}
}

// A client that hangs up right after its terminal event (Codex stops at
// response.completed) while a converted upstream still holds its stream
// open after [DONE]: the response is complete, so it is a success, not a
// client disconnect, and it is charged in full.
func TestHangupAfterTerminalEventSucceeds(t *testing.T) {
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		sseReply(0, fixture(t, "chat_stream_tools.sse"))(w, r, b)
		<-r.Context().Done() // never closes on its own
	})
	e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}})
	m := &fakeMeter{}
	e.chain.Meter = m
	srv := e.server("openai-responses", "/v1/responses")
	defer srv.Close()
	stream, _ := sjson.Set(responsesReq, "stream", true)
	resp, err := http.DefaultClient.Do(e.request(srv.URL, "openai-responses", "/v1/responses", stream))
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	sawCompleted := false
	for sc.Scan() {
		if strings.Contains(sc.Text(), "response.completed") {
			sawCompleted = true
			break
		}
	}
	resp.Body.Close()
	if !sawCompleted {
		t.Fatal("no response.completed")
	}
	rec := e.waitRecords(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec.Status != http.StatusOK || rec.ErrorKind != "" || m.settle != 1 || m.refund != 0 {
		t.Errorf("record status %d kind %q; meter %+v", rec.Status, rec.ErrorKind, m)
	}
}

// A repeated key is refused: the relay would check and bill the first
// "model" or "stream" while the upstream serves the last.
func TestDuplicateKeyRefused(t *testing.T) {
	up := newFake(t, jsonReply(200, chatResp))
	e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}})
	for _, body := range []string{
		`{"model":"gpt-x","model":"gpt-5-pro","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"gpt-x","stream":false,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
	} {
		resp, out := e.call("openai-chat", "/v1/chat/completions", body)
		if resp.StatusCode != 400 || !strings.Contains(string(out), "repeats the key") {
			t.Errorf("%s: %d %s", body, resp.StatusCode, out)
		}
	}
	if len(up.paths) != 0 {
		t.Errorf("upstream called: %v", up.paths)
	}
}

// A same-protocol chat stream without include_usage still gets the
// upstream's usage for billing; the client does not see the extra chunk.
func TestChatStreamAsksForUsage(t *testing.T) {
	up := newFake(t, sseReply(0, fixture(t, "chat_stream_tools.sse")))
	e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}})
	_, body := e.call("openai-chat", "/v1/chat/completions", `{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !gjson.GetBytes(up.body(0), "stream_options.include_usage").Bool() {
		t.Errorf("upstream body %s", up.body(0))
	}
	if strings.Contains(string(body), `"usage"`) || !strings.Contains(string(body), "[DONE]") {
		t.Errorf("client stream:\n%s", body)
	}
	if rec := e.records.last(t); rec.Usage.Input != 57 || rec.Usage.Output != 48 || rec.Usage.Estimated {
		t.Errorf("usage %+v", rec.Usage)
	}

	// A client that asked keeps the chunk.
	_, body = e.call("openai-chat", "/v1/chat/completions", `{"model":"gpt-x","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(string(body), `"usage"`) {
		t.Errorf("client asked for usage:\n%s", body)
	}
}
