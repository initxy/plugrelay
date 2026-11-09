package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/middleware/guardrails"
	"github.com/initxy/plugrelay/plugins/middleware/parampolicy"
	"github.com/initxy/plugrelay/plugins/middleware/ratelimit"
)

// refuseAttempt refuses every attempt.
type refuseAttempt struct{}

func (refuseAttempt) Info() plugin.Info {
	return plugin.Info{ID: "middleware.refuse-attempt", Kind: plugin.KindMiddleware}
}

func (refuseAttempt) OnAttempt(*plugin.Call, *plugin.Attempt) error {
	return ir.Errorf(ir.ErrPermission, "attempt refused")
}

// Every body mutator edits a Responses response.create as on a patch-tier
// attempt (middleware.param-policy caps and strips), and attempt hooks may
// refuse it.
func TestResponsesWebSocketBodyMutatorsAndAttemptHooks(t *testing.T) {
	done := `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`
	t.Run("param-policy", func(t *testing.T) {
		up := newWSUpstream(t, done)
		set := map[string]map[string]any{parampolicy.ID: {"max_output_tokens": 100, "max_reasoning_effort": "low", "strip_params": []string{"service_tier"}}}
		e := wsGuardEnv(t, up, "{responses_websocket: true}", set, parampolicy.New())
		c := dialSession(t, e, WSResponses)
		wsSend(t, c, `{"type":"response.create","model":"m","max_output_tokens":100000,"service_tier":"priority","reasoning":{"effort":"xhigh"},"input":[]}`)
		_, _ = wsRead(t, c)
		_, _ = wsRead(t, c)
		got := up.received()
		if len(got) != 1 {
			t.Fatalf("upstream received %v", got)
		}
		v := gjson.Parse(got[0])
		if v.Get("max_output_tokens").Int() != 100 || v.Get("service_tier").Exists() || v.Get("reasoning.effort").String() != "low" {
			t.Fatalf("upstream received %s", got[0])
		}
	})
	t.Run("attempt hook", func(t *testing.T) {
		up := newWSUpstream(t, done)
		e := wsGuardEnv(t, up, "{responses_websocket: true}", nil, refuseAttempt{})
		c := dialSession(t, e, WSResponses)
		wsSend(t, c, `{"type":"response.create","model":"m","input":[]}`)
		if got, _ := wsRead(t, c); gjson.Get(got, "status").Int() != http.StatusForbidden {
			t.Fatalf("got %s", got)
		}
		if got := up.received(); len(got) != 0 {
			t.Fatalf("upstream received %v", got)
		}
	})
}

// On Realtime, middleware.param-policy lowers the output limits of
// session.update and response.create, strips parameters, and binds the
// session's unlimited default through a session.update of the relay's own.
func TestRealtimeParamPolicy(t *testing.T) {
	up := scriptedUpstream(t, []string{`{"type":"session.created","session":{"type":"realtime","max_output_tokens":"inf"}}`},
		func(ctx context.Context, c *websocket.Conn, msg string) {
			if gjson.Get(msg, "type").String() == "session.update" {
				send(ctx, c, `{"type":"session.updated","session":{"max_output_tokens":`+gjson.Get(msg, "session.max_output_tokens").Raw+`}}`)
			}
		})
	set := map[string]map[string]any{parampolicy.ID: {"max_output_tokens": 100, "strip_params": []string{"temperature"}}}
	e := wsGuardEnv(t, up, "{realtime: true}", set, parampolicy.New())
	meter := newWSMeter(10)
	e.chain.Meter = meter
	c := dialSession(t, e, WSRealtime)
	if got, _ := wsRead(t, c); gjson.Get(got, "session.max_output_tokens").Int() != 100 {
		t.Fatalf("relay's session.update answered %s", got)
	}
	wsSend(t, c, `{"type":"session.update","session":{"type":"realtime","max_output_tokens":5000,"temperature":0.9,"instructions":"hi"}}`)
	_, _ = wsRead(t, c)
	wsSend(t, c, `{"type":"response.create","response":{"max_output_tokens":"inf"}}`)
	wsSend(t, c, `{"type":"session.update","session":{"max_output_tokens":50}}`)
	_, _ = wsRead(t, c)
	got := up.received()
	if len(got) != 4 {
		t.Fatalf("upstream received %v", got)
	}
	if v := gjson.Parse(got[0]); v.Get("session.max_output_tokens").Int() != 100 || v.Get("session.type").String() != "realtime" ||
		!strings.HasPrefix(v.Get("event_id").String(), relayEventPrefix) {
		t.Errorf("relay session.update %s", got[0])
	}
	if v := gjson.Parse(got[1]); v.Get("session.max_output_tokens").Int() != 100 || v.Get("session.temperature").Exists() ||
		v.Get("session.instructions").String() != "hi" {
		t.Errorf("client session.update %s", got[1])
	}
	if v := gjson.Parse(got[2]); v.Get("response.max_output_tokens").Int() != 100 {
		t.Errorf("response.create %s", got[2])
	}
	if got[3] != `{"type":"session.update","session":{"max_output_tokens":50}}` {
		t.Errorf("a lower limit changed: %s", got[3])
	}
}

// tokenPlanMeter holds each reservation's EstimateTokens (at least 1)
// against a token allowance with no balance behind it, as a token plan
// with a zero balance does.
type tokenPlanMeter struct {
	mu        sync.Mutex
	allowance int64
	held      map[string]int64
	estimates []int64
}

func (m *tokenPlanMeter) Info() plugin.Info {
	return plugin.Info{ID: "meter.tokens", Kind: plugin.KindMeter}
}

func (m *tokenPlanMeter) Reserve(c *plugin.Call, _ int64) (*plugin.Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := max(c.EstimateTokens, 1)
	var sum int64
	for _, h := range m.held {
		sum += h
	}
	if sum+n > m.allowance {
		e := ir.Errorf(ir.ErrQuotaExhausted, "allowance used up")
		e.Code = "insufficient_quota"
		return nil, e
	}
	m.held[c.ID] = n
	m.estimates = append(m.estimates, c.EstimateTokens)
	return &plugin.Reservation{ID: c.ID}, nil
}

func (m *tokenPlanMeter) Settle(c *plugin.Call, _ *plugin.Reservation, _ ir.Usage, _ int64) error {
	m.mu.Lock()
	delete(m.held, c.ID)
	m.mu.Unlock()
	return nil
}

func (m *tokenPlanMeter) Refund(c *plugin.Call, _ *plugin.Reservation) error {
	m.mu.Lock()
	delete(m.held, c.ID)
	m.mu.Unlock()
	return nil
}

// Each response's hold carries its token estimate (input plus output
// cap), so concurrent responses on a token plan with no balance cannot
// together draw more than the allowance.
func TestWebSocketTokenPlanEstimate(t *testing.T) {
	const sessions = 50
	run := func(t *testing.T, kind, caps string, start func(c *websocket.Conn) bool) *tokenPlanMeter {
		up := scriptedUpstream(t, []string{`{"type":"session.created"}`}, func(ctx context.Context, c *websocket.Conn, msg string) {
			if gjson.Get(msg, "type").String() == "response.create" {
				send(ctx, c, `{"type":"response.created","response":{"id":"r"}}`) // never finishes
			}
		})
		meter := &tokenPlanMeter{allowance: 10_000, held: map[string]int64{}}
		e := realtimeEnv(t, up, caps)
		e.chain.Meter = meter
		srv := e.wsServer(kind)
		path := "/v1/realtime?model=m"
		if kind == WSResponses {
			path = "/v1/responses"
		}
		var admitted atomic.Int64
		var wg sync.WaitGroup
		for range sessions {
			wg.Go(func() {
				c, _, err := wsDial(t, wsURL(srv, path), nil)
				if err != nil {
					return // refused at the handshake
				}
				_, _ = wsRead(t, c)
				if start(c) {
					admitted.Add(1)
				}
			})
		}
		wg.Wait()
		meter.mu.Lock()
		defer meter.mu.Unlock()
		var held int64
		for _, h := range meter.held {
			held += h
		}
		if held > meter.allowance || admitted.Load() > 2 {
			t.Fatalf("admitted %d responses holding %d tokens of %d", admitted.Load(), held, meter.allowance)
		}
		return meter
	}
	t.Run("responses", func(t *testing.T) {
		m := run(t, WSResponses, "{responses_websocket: true}", func(c *websocket.Conn) bool {
			_ = c.Write(context.Background(), websocket.MessageText,
				[]byte(`{"type":"response.create","model":"m","max_output_tokens":4096,"input":"`+strings.Repeat("x", 400)+`"}`))
			got, _ := wsRead(t, c)
			return gjson.Get(got, "type").String() == "response.created"
		})
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, est := range m.estimates {
			if est != 0 && est != 4096+104 {
				t.Errorf("estimate %d, want 4200 (input 104 + cap 4096)", est)
			}
		}
	})
	t.Run("realtime", func(t *testing.T) {
		m := run(t, WSRealtime, "{realtime: true}", func(*websocket.Conn) bool { return true })
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, est := range m.estimates {
			if est != DefaultWSResponseReserve {
				t.Errorf("estimate %d, want %d", est, DefaultWSResponseReserve)
			}
		}
	})
}

// A Realtime response left unfinished is billed with the conversation's
// input estimated from what the client added (text, audio), and the next
// hold includes it; a Responses one reads its input as HTTP does (an image
// is not counted as text).
func TestWebSocketUnfinishedInputEstimate(t *testing.T) {
	t.Run("realtime", func(t *testing.T) {
		up := scriptedUpstream(t, []string{`{"type":"session.created"}`}, slowResponse(3*time.Second))
		meter := newWSMeter(10)
		e := realtimeEnv(t, up, "{realtime: true}", func(d *Deps) { d.WebSocket.Drain = 200 * time.Millisecond })
		e.chain.Meter = meter
		c, _, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m"), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = wsRead(t, c)
		big := strings.Repeat("lorem ipsum ", 40000) // 480,000 characters
		wsSend(t, c, `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"`+big+`"}]}}`)
		wsSend(t, c, `{"type":"input_audio_buffer.append","audio":"`+strings.Repeat("A", 64000*10)+`"}`) // 10 s of pcm16
		wsSend(t, c, `{"type":"response.create"}`)
		for range 6 {
			_, _ = wsRead(t, c)
		}
		_ = c.CloseNow()
		recs := e.waitRecordsOf(2)
		if u := recs[0].Usage; !u.Estimated || u.Input < 120_100 || u.Input > 120_200 {
			t.Errorf("estimated usage %+v", u)
		}
		meter.mu.Lock()
		defer meter.mu.Unlock()
		if len(meter.reserved) != 2 || meter.amounts[meter.reserved[1]] < 61440+360_000 {
			t.Errorf("holds %v", meter.amounts)
		}
	})
	t.Run("responses", func(t *testing.T) {
		up := scriptedUpstream(t, nil, func(ctx context.Context, c *websocket.Conn, msg string) {
			if gjson.Get(msg, "type").String() == "response.create" {
				send(ctx, c, `{"type":"response.created","response":{"id":"resp_1"}}`, `{"type":"response.output_text.delta","delta":"hi"}`)
			}
		})
		e := realtimeEnv(t, up, "{responses_websocket: true}", func(d *Deps) { d.WebSocket.Drain = 200 * time.Millisecond })
		e.chain.Meter = newWSMeter(10)
		c, _, err := wsDial(t, wsURL(e.wsServer(WSResponses), "/v1/responses"), nil)
		if err != nil {
			t.Fatal(err)
		}
		img := strings.Repeat("A", 800_000)
		wsSend(t, c, `{"type":"response.create","model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image_url":"data:image/png;base64,`+img+`"}]}]}`)
		_, _ = wsRead(t, c)
		_, _ = wsRead(t, c)
		_ = c.CloseNow()
		recs := e.waitRecordsOf(2)
		if u := recs[0].Usage; !u.Estimated || u.Input < 800 || u.Input > 1000 {
			t.Errorf("estimated usage %+v", u)
		}
	})
}

// An upstream error about no client event, while output is held, ends the
// held response: its held text, clean once complete, is released before
// the error, its later frames are dropped; later responses flow.
func TestWebSocketHeldOutputUpstreamError(t *testing.T) {
	n := 0
	up := scriptedUpstream(t, []string{`{"type":"session.created"}`}, func(ctx context.Context, c *websocket.Conn, msg string) {
		if gjson.Get(msg, "type").String() != "response.create" {
			return
		}
		n++
		if n == 1 {
			send(ctx, c, `{"type":"response.created","response":{"id":"resp_1"}}`,
				`{"type":"response.output_text.delta","response_id":"resp_1","item_id":"i1","delta":"the forb"}`,
				`{"type":"error","error":{"type":"server_error","message":"upstream broke"}}`,
				`{"type":"response.output_text.delta","response_id":"resp_1","item_id":"i1","delta":"idden"}`)
			return
		}
		send(ctx, c, `{"type":"response.created","response":{"id":"resp_2"}}`,
			`{"type":"response.output_text.delta","response_id":"resp_2","item_id":"i2","delta":"hello"}`,
			`{"type":"response.done","response":{"id":"resp_2","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`)
	})
	e := wsGuardEnv(t, up, "{realtime: true}", guardSettings(), guardrails.New())
	c := dialSession(t, e, WSRealtime)
	wsSend(t, c, `{"type":"response.create"}`)
	var got []string
	for range 3 {
		m, err := wsRead(t, c)
		if err != nil {
			t.Fatalf("after %v: %v", got, err)
		}
		got = append(got, gjson.Get(m, "type").String()+":"+gjson.Get(m, "delta").String()+gjson.Get(m, "error.message").String())
	}
	if strings.Join(got, ",") != "response.created:,response.output_text.delta:the forb,error:upstream broke" {
		t.Fatalf("got %v", got)
	}
	wsSend(t, c, `{"type":"response.create"}`)
	var rest []string
	for {
		m, err := wsRead(t, c)
		if err != nil {
			t.Fatalf("after %v: %v", rest, err)
		}
		rest = append(rest, gjson.Get(m, "type").String()+":"+gjson.Get(m, "delta").String())
		if gjson.Get(m, "type").String() == "response.done" {
			break
		}
	}
	if strings.Contains(strings.Join(rest, ","), "idden") || !strings.Contains(strings.Join(rest, ","), "response.output_text.delta:hello") {
		t.Fatalf("second response %v", rest)
	}
	if got := strings.Join(up.received(), ","); !strings.Contains(got, `"response.cancel"`) || !strings.Contains(got, "resp_1") {
		t.Fatalf("upstream received %s", got)
	}
}

// Held output and upstream errors: an error answering a client event
// (its event_id) leaves the held response running and reaches the client
// at once; an error that ends the response finds a match in the held text
// once complete and stops it instead of releasing it.
func TestWebSocketHeldOutputErrorKinds(t *testing.T) {
	read := func(t *testing.T, c *websocket.Conn, until string) []string {
		t.Helper()
		var got []string
		for {
			m, err := wsRead(t, c)
			if err != nil {
				t.Fatalf("after %v: %v", got, err)
			}
			typ := gjson.Get(m, "type").String()
			got = append(got, typ+":"+gjson.Get(m, "delta").String()+gjson.Get(m, "error.code").String()+
				gjson.Get(m, "response.output.0.content.0.text").String())
			if typ == until || (until == "guard" && isGuardError(m)) {
				return got
			}
		}
	}
	t.Run("client event", func(t *testing.T) {
		for _, code := range []string{"input_audio_buffer_commit_empty", "conversation_already_has_active_response"} {
			up := scriptedUpstream(t, []string{`{"type":"session.created"}`}, func(ctx context.Context, c *websocket.Conn, msg string) {
				switch gjson.Get(msg, "type").String() {
				case "response.create":
					send(ctx, c, `{"type":"response.created","response":{"id":"resp_1"}}`,
						`{"type":"response.output_text.delta","response_id":"resp_1","item_id":"i1","delta":"the for"}`)
				case "input_audio_buffer.commit":
					send(ctx, c, `{"type":"error","error":{"type":"invalid_request_error","code":"`+code+`","event_id":"`+gjson.Get(msg, "event_id").String()+`"}}`,
						`{"type":"response.output_text.delta","response_id":"resp_1","item_id":"i1","delta":"mat is fine"}`,
						`{"type":"response.done","response":{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"the format is fine"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`)
				}
			})
			e := wsGuardEnv(t, up, "{realtime: true}", guardSettings(), guardrails.New())
			c := dialSession(t, e, WSRealtime)
			wsSend(t, c, `{"type":"response.create"}`)
			if got := read(t, c, "response.created"); len(got) != 1 {
				t.Fatalf("got %v", got)
			}
			wsSend(t, c, `{"type":"input_audio_buffer.commit","event_id":"evt_c1"}`)
			want := "error:" + code + ",response.output_text.delta:the for,response.output_text.delta:mat is fine,response.done:the format is fine"
			if got := strings.Join(read(t, c, "response.done"), ","); got != want {
				t.Fatalf("%s: got %s", code, got)
			}
			if got := strings.Join(up.received(), ","); strings.Contains(got, "response.cancel") {
				t.Fatalf("upstream received %s", got)
			}
		}
	})
	t.Run("match once complete", func(t *testing.T) {
		up := scriptedUpstream(t, []string{`{"type":"session.created"}`}, func(ctx context.Context, c *websocket.Conn, msg string) {
			if gjson.Get(msg, "type").String() == "response.create" {
				send(ctx, c, `{"type":"response.created","response":{"id":"resp_1"}}`,
					`{"type":"response.output_text.delta","response_id":"resp_1","item_id":"i1","delta":"code 1234"}`,
					`{"type":"error","error":{"type":"server_error","code":"server_error"}}`,
					`{"type":"response.done","response":{"id":"resp_1","status":"failed","output":[{"type":"message","content":[{"type":"output_text","text":"code 1234"}]}]}}`)
			}
		})
		// A number may go on, so "1234" at the end is held until complete.
		e := wsGuardEnv(t, up, "{realtime: true}", guardSettings("deny_patterns", []string{`\b\d{4}\b`}), guardrails.New())
		c := dialSession(t, e, WSRealtime)
		wsSend(t, c, `{"type":"response.create"}`)
		got := strings.Join(read(t, c, "response.done"), ",")
		if strings.Contains(got, "1234") || !strings.Contains(got, "error:server_error") {
			t.Fatalf("got %s", got)
		}
	})
	// Clean once complete, the held text goes out ahead of the error, and
	// the terminal event is relayed unchanged with no response.cancel; an
	// error naming the response at the top level is never dropped with it.
	for _, idKey := range []string{"", "response_id"} {
		t.Run("clean once complete "+idKey, func(t *testing.T) {
			errEv := `{"type":"error","error":{"type":"server_error","code":"server_error"}}`
			if idKey != "" {
				errEv = `{"type":"error","` + idKey + `":"resp_1","error":{"type":"server_error","code":"server_error"}}`
			}
			up := scriptedUpstream(t, []string{`{"type":"session.created"}`}, func(ctx context.Context, c *websocket.Conn, msg string) {
				if gjson.Get(msg, "type").String() == "response.create" {
					send(ctx, c, `{"type":"response.created","response":{"id":"resp_1"}}`,
						`{"type":"response.output_text.delta","response_id":"resp_1","item_id":"i1","delta":"the for"}`,
						errEv,
						`{"type":"response.done","response":{"id":"resp_1","status":"failed","output":[{"type":"message","content":[{"type":"output_text","text":"the for"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`)
				}
			})
			e := wsGuardEnv(t, up, "{realtime: true}", guardSettings(), guardrails.New())
			c := dialSession(t, e, WSRealtime)
			wsSend(t, c, `{"type":"response.create"}`)
			want := "response.created:,response.output_text.delta:the for,error:server_error,response.done:the for"
			if got := strings.Join(read(t, c, "response.done"), ","); got != want {
				t.Fatalf("got %s", got)
			}
			if got := strings.Join(up.received(), ","); strings.Contains(got, "response.cancel") {
				t.Fatalf("upstream received %s", got)
			}
		})
	}
	t.Run("error naming a stopped response", func(t *testing.T) {
		up := scriptedUpstream(t, []string{`{"type":"session.created"}`}, func(ctx context.Context, c *websocket.Conn, msg string) {
			if gjson.Get(msg, "type").String() == "response.create" {
				send(ctx, c, `{"type":"response.created","response":{"id":"resp_1"}}`,
					`{"type":"response.output_text.delta","response_id":"resp_1","item_id":"i1","delta":"forbidden "}`,
					`{"type":"error","response_id":"resp_1","error":{"type":"server_error","code":"server_error"}}`,
					`{"type":"response.done","response":{"id":"resp_1","status":"failed","output":[]}}`)
			}
		})
		e := wsGuardEnv(t, up, "{realtime: true}", guardSettings(), guardrails.New())
		c := dialSession(t, e, WSRealtime)
		wsSend(t, c, `{"type":"response.create"}`)
		if got := strings.Join(read(t, c, "response.done"), ","); !strings.Contains(got, "error:server_error") {
			t.Fatalf("got %s", got)
		}
	})
}

// A response's checking state is dropped once its terminal event went out,
// a stopped response's included.
func TestWebSocketScreenPrunedPerResponse(t *testing.T) {
	e := wsGuardEnv(t, &wsUpstream{fakeUpstream: newFake(t, nil)}, "{realtime: true}", guardSettings(), guardrails.New())
	pair := func() *websocket.Conn {
		conns := make(chan *websocket.Conn, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			conns <- c
			<-r.Context().Done()
		}))
		t.Cleanup(srv.Close)
		peer, _, err := wsDial(t, wsURL(srv, "/"), nil)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				if _, _, err := peer.Read(context.Background()); err != nil {
					return
				}
			}
		}()
		c := <-conns
		t.Cleanup(func() { c.CloseNow() })
		return c
	}
	uc, cc := pair(), pair()
	s := &wsSession{p: e.p, r: httptest.NewRequest(http.MethodGet, "/v1/realtime", nil), caller: &plugin.Caller{},
		kind: WSRealtime, lim: e.p.d.WebSocket.withDefaults(), id: "sess", eventHooks: []plugin.Plugin{guardrails.New()}}
	for i, d := range []string{"fine", "forbidden", "fine again"} {
		id := "resp_" + strconv.Itoa(i)
		for _, m := range []string{
			`{"type":"response.created","response":{"id":"` + id + `"}}`,
			`{"type":"response.output_item.added","response_id":"` + id + `","item":{"id":"item_` + id + `","type":"message"}}`,
			`{"type":"response.output_text.delta","response_id":"` + id + `","item_id":"item_` + id + `","delta":"` + d + `"}`,
			`{"type":"response.output_text.done","response_id":"` + id + `","item_id":"item_` + id + `"}`,
			`{"type":"response.done","response":{"id":"` + id + `","status":"completed","output":[]}}`,
		} {
			if err := s.toClient(uc, cc, websocket.MessageText, []byte(m)); err != nil {
				t.Fatal(err)
			}
		}
	}
	sc := &s.screen
	if len(sc.calls)+len(sc.items)+len(sc.blocked)+len(sc.loose)+len(sc.queue)+len(sc.held.seen) != 0 {
		t.Fatalf("screen kept %d calls, %v items, %v blocked, %d loose, %d queued, %v held",
			len(sc.calls), sc.items, sc.blocked, len(sc.loose), len(sc.queue), sc.held.seen)
	}
}

// Client events wait for the session the upstream created, so the relay's
// output cap goes out first and a lower limit the client sets is kept;
// a response.create's own limit is lowered only to the policy cap.
func TestRealtimeCapBeforeClientEvents(t *testing.T) {
	up := &wsUpstream{}
	up.fakeUpstream = newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := context.Background()
		go func() {
			time.Sleep(50 * time.Millisecond)
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created","session":{"type":"realtime","max_output_tokens":"inf"}}`))
		}()
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			up.mu.Lock()
			up.got = append(up.got, string(b))
			up.mu.Unlock()
		}
	})
	set := map[string]map[string]any{parampolicy.ID: {"max_output_tokens": 100}}
	e := wsGuardEnv(t, up, "{realtime: true}", set, parampolicy.New())
	c, _, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	wsSend(t, c, `{"type":"session.update","session":{"type":"realtime","max_output_tokens":50}}`)
	wsSend(t, c, `{"type":"response.create"}`)
	wsSend(t, c, `{"type":"response.create","response":{"max_output_tokens":80}}`)
	wsSend(t, c, `{"type":"response.create","response":{"max_output_tokens":800}}`)
	var got []string
	for range 100 {
		if got = up.received(); len(got) == 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) != 5 {
		t.Fatalf("upstream received %v", got)
	}
	if v := gjson.Parse(got[0]); v.Get("session.max_output_tokens").Int() != 100 || !strings.HasPrefix(v.Get("event_id").String(), relayEventPrefix) {
		t.Errorf("first %s", got[0])
	}
	if v := gjson.Parse(got[1]); v.Get("session.max_output_tokens").Int() != 50 {
		t.Errorf("client session.update %s", got[1])
	}
	if got[2] != `{"type":"response.create"}` {
		t.Errorf("response.create %s", got[2])
	}
	// A response's own limit overrides the session's; only the policy
	// cap lowers it.
	if v := gjson.Parse(got[3]); v.Get("response.max_output_tokens").Int() != 80 {
		t.Errorf("response.create over the session's limit %s", got[3])
	}
	if v := gjson.Parse(got[4]); v.Get("response.max_output_tokens").Int() != 100 {
		t.Errorf("response.create over the policy cap %s", got[4])
	}
}

// A transcription model a session.update names is admitted like a request
// model: visible to the caller, allowed by the request plugins, and priced
// when the relay bills; otherwise the update is refused.
func TestRealtimeTranscriptionModelChecked(t *testing.T) {
	update := func(model string) string {
		return `{"type":"session.update","session":{"audio":{"input":{"transcription":{"model":"` + model + `"}}}}}`
	}
	for _, tc := range []struct {
		name, model string
		status      int64
	}{
		{"unpriced", "unpriced-model", 400},
		{"hidden", "hidden-model", 404},
		{"not allowed", "gpt-other-transcribe", 403},
		{"allowed", "gpt-4o-transcribe", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newWSUpstream(t, `{}`)
			e := realtimeEnv(t, up, "{realtime: true}", func(d *Deps) {
				d.Settings = pluginSettings(map[string]map[string]any{gateID: {"models": []string{"m", "gpt-4o-transcribe", "unpriced-model"}},
					ratelimit.ID: {"concurrency": 1}})
			})
			e.chain.Plugins = []plugin.Plugin{modelGate{}, ratelimit.New(newMemState())}
			e.chain.Meter = newWSMeter(10)
			e.snap.Visible = func(_ *plugin.Caller, m string) bool { return m != "hidden-model" }
			e.snap.Price = func(_, model string) (Price, bool) { return testPrice, model != "unpriced-model" }
			c := dialSession(t, e, WSRealtime)
			wsSend(t, c, update(tc.model))
			got, _ := wsRead(t, c)
			if tc.status == 0 {
				if gjson.Get(got, "type").String() != "echo" {
					t.Fatalf("got %s", got)
				}
				return
			}
			if gjson.Get(got, "status").Int() != tc.status || len(up.received()) != 0 {
				t.Fatalf("got %s, upstream %v", got, up.received())
			}
		})
	}
}

// A transcription session (intent=transcription, beta
// transcription_session.update) is checked the same way: the update must
// name a priced, visible model, audio waits until one is set, and the
// transcriptions are billed at the model the upstream reports.
func TestRealtimeTranscriptionSession(t *testing.T) {
	up := scriptedUpstream(t, []string{`{"type":"transcription_session.created","session":{"input_audio_transcription":null}}`},
		func(ctx context.Context, c *websocket.Conn, msg string) {
			switch gjson.Get(msg, "type").String() {
			case "transcription_session.update":
				send(ctx, c, `{"type":"transcription_session.updated","session":{"input_audio_transcription":{"model":"`+
					gjson.Get(msg, "session.input_audio_transcription.model").String()+`"}}}`)
			case "input_audio_buffer.append":
				send(ctx, c, `{"type":"conversation.item.input_audio_transcription.completed","item_id":"i1","usage":{"type":"tokens","input_tokens":50,"output_tokens":10}}`)
			}
		})
	e := realtimeEnv(t, up, "{realtime: true}")
	meter := newWSMeter(10)
	e.chain.Meter = meter
	e.snap.Visible = func(_ *plugin.Caller, m string) bool { return m != "hidden-model" }
	e.snap.Price = func(_, model string) (Price, bool) { return testPrice, model != "unpriced-model" }
	c, _, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m&intent=transcription"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wsRead(t, c)
	update := func(tr string) string {
		return `{"type":"transcription_session.update","session":{"input_audio_transcription":` + tr + `}}`
	}
	for _, ev := range []string{
		`{"type":"input_audio_buffer.append","audio":"AAAA"}`,
		update(`{"model":"unpriced-model"}`),
		update(`{"model":"hidden-model"}`),
		update(`{"language":"en"}`),
	} {
		wsSend(t, c, ev)
		if got, _ := wsRead(t, c); gjson.Get(got, "type").String() != "error" || gjson.Get(got, "status").Int() < 400 {
			t.Fatalf("%s: got %s", ev, got)
		}
	}
	if got := up.received(); len(got) != 0 {
		t.Fatalf("upstream received %v", got)
	}
	wsSend(t, c, update(`{"model":"gpt-4o-transcribe"}`))
	_, _ = wsRead(t, c)
	wsSend(t, c, `{"type":"input_audio_buffer.append","audio":"AAAA"}`)
	_, _ = wsRead(t, c)
	_ = c.Close(websocket.StatusNormalClosure, "")
	recs := e.waitRecordsOf(2)
	if r := recs[0]; r.Model != "gpt-4o-transcribe" || r.ChargeMicro != 300 {
		t.Errorf("transcription %+v", r)
	}
}

// moderationCalls counts moderation calls and finds nothing.
type moderationCalls struct{ n atomic.Int64 }

func (m *moderationCalls) CallUpstream(context.Context, string, string, string, []byte) (int, []byte, error) {
	m.n.Add(1)
	return 200, []byte(`{"results":[{"flagged":false,"categories":{}}]}`), nil
}

// Realtime text events count toward the request rate, and moderation
// checks at most max_moderated_events of a session.
func TestRealtimeEventLimits(t *testing.T) {
	item := `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}`
	t.Run("rpm", func(t *testing.T) {
		up := newWSUpstream(t, `{}`)
		e := wsGuardEnv(t, up, "{realtime: true}", map[string]map[string]any{ratelimit.ID: {"rpm": 3, "concurrency": 1}},
			ratelimit.New(newMemState()))
		c := dialSession(t, e, WSRealtime) // the session is one request
		for i := range 3 {
			wsSend(t, c, item)
			got, _ := wsRead(t, c)
			if want := i < 2; (gjson.Get(got, "type").String() == "echo") != want {
				t.Fatalf("event %d: %s", i+1, got)
			}
			if i == 2 && gjson.Get(got, "status").Int() != http.StatusTooManyRequests {
				t.Fatalf("over rpm: %s", got)
			}
		}
	})
	t.Run("moderation", func(t *testing.T) {
		up := newWSUpstream(t, `{}`)
		mod := &moderationCalls{}
		g := guardrails.New()
		g.SetUpstreams(mod)
		e := wsGuardEnv(t, up, "{realtime: true}", map[string]map[string]any{guardrails.ID: {
			"moderation_upstream": "mod", "max_moderated_events": 2, "moderation_fail": "closed"}}, g)
		c := dialSession(t, e, WSRealtime)
		// Events with no user text are checked but not sent to moderation.
		for range 3 {
			wsSend(t, c, `{"type":"session.update","session":{"instructions":"be brief"}}`)
			if got, _ := wsRead(t, c); gjson.Get(got, "type").String() != "echo" {
				t.Fatalf("instructions: %s", got)
			}
		}
		for i := range 3 {
			wsSend(t, c, item)
			got, _ := wsRead(t, c)
			if want := i < 2; (gjson.Get(got, "type").String() == "echo") != want {
				t.Fatalf("event %d: %s", i+1, got)
			}
			if i == 2 && !strings.Contains(gjson.Get(got, "error.message").String(), "new session") {
				t.Fatalf("past the cap: %s", got)
			}
		}
		if n := mod.n.Load(); n != 2 {
			t.Fatalf("moderation calls %d", n)
		}
	})
}

// A client event may not carry the relay's own event_id prefix, whose
// errors the relay swallows.
func TestWebSocketRefusesRelayEventID(t *testing.T) {
	up := newWSUpstream(t, `{}`)
	e := realtimeEnv(t, up, "{realtime: true}")
	c := dialSession(t, e, WSRealtime)
	wsSend(t, c, `{"type":"response.create","event_id":"`+relayEventPrefix+`x"}`)
	if got, _ := wsRead(t, c); gjson.Get(got, "type").String() != "error" || len(up.received()) != 0 {
		t.Fatalf("got %s, upstream %v", got, up.received())
	}
}

// A meter panic while settling a Responses response still runs its finish
// hooks, so the rate limiter's concurrency slot is released.
func TestResponsesWebSocketMeterPanicReleases(t *testing.T) {
	up := newWSUpstream(t, `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`)
	e := wsGuardEnv(t, up, "{responses_websocket: true}", map[string]map[string]any{ratelimit.ID: {"concurrency": 1}},
		ratelimit.New(newMemState()))
	e.chain.Meter = panicMeter{newWSMeter(10)}
	srv := e.wsServer(WSResponses)
	create := func() []string {
		c, _, err := wsDial(t, wsURL(srv, "/v1/responses"), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = wsRead(t, c)
		wsSend(t, c, `{"type":"response.create","model":"m","input":[]}`)
		got, _ := readUntilClose(t, c)
		return got
	}
	if got := create(); len(got) == 0 || gjson.Get(got[0], "type").String() != "response.created" {
		t.Fatalf("first: %v", got)
	}
	e.waitRecordsOf(1)
	if got := create(); len(got) == 0 || gjson.Get(got[0], "type").String() != "response.created" {
		t.Fatalf("second (slot not released?): %v", got)
	}
}

// An upstream that never sends session.created while client events wait
// for it closes the session after the ready timeout: an error event, then
// 1013; no client event reaches the upstream.
func TestRealtimeSessionReadyTimeout(t *testing.T) {
	up := &wsUpstream{}
	up.fakeUpstream = newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				return
			}
			up.mu.Lock()
			up.got = append(up.got, string(b))
			up.mu.Unlock()
		}
	})
	set := map[string]map[string]any{parampolicy.ID: {"max_output_tokens": 100}}
	e := realtimeEnv(t, up, "{realtime: true}", func(d *Deps) {
		d.Settings = pluginSettings(set)
		d.WebSocket.ReadyTimeout = 200 * time.Millisecond
	})
	e.chain.Plugins = append(e.chain.Plugins, parampolicy.New())
	c, _, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	wsSend(t, c, `{"type":"response.create"}`)
	got, err := readUntilClose(t, c)
	if websocket.CloseStatus(err) != websocket.StatusTryAgainLater || len(got) != 1 ||
		gjson.Get(got[0], "error.code").String() != "session_not_ready" {
		t.Fatalf("got %v, %v", got, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("closed after %v", took)
	}
	if r := up.received(); len(r) != 0 {
		t.Errorf("upstream received %v", r)
	}
}
