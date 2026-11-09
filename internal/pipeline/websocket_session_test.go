package pipeline

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/middleware/access"
	"github.com/initxy/plugrelay/plugins/middleware/ratelimit"
	"github.com/initxy/plugrelay/plugins/provider/compatible"
)

// scriptedUpstream is a fake upstream WebSocket that sends hello on
// connect and hands every client message to on.
func scriptedUpstream(t *testing.T, hello []string, on func(ctx context.Context, c *websocket.Conn, msg string)) *wsUpstream {
	u := &wsUpstream{}
	u.fakeUpstream = newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(1 << 20)
		ctx := context.Background()
		for _, h := range hello {
			_ = c.Write(ctx, websocket.MessageText, []byte(h))
		}
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			u.mu.Lock()
			u.got = append(u.got, string(b))
			u.mu.Unlock()
			on(ctx, c, string(b))
		}
	})
	return u
}

func send(ctx context.Context, c *websocket.Conn, msgs ...string) {
	for _, m := range msgs {
		_ = c.Write(ctx, websocket.MessageText, []byte(m))
	}
}

const textDelta = `{"type":"response.output_text.delta","response_id":"resp_1","delta":"hello"}`

// slowResponse answers response.create with response.created and five
// deltas, then response.done after wait, reading on meanwhile.
func slowResponse(wait time.Duration) func(ctx context.Context, c *websocket.Conn, msg string) {
	return func(ctx context.Context, c *websocket.Conn, msg string) {
		if gjson.Get(msg, "type").String() != "response.create" {
			return
		}
		send(ctx, c, `{"type":"response.created","response":{"id":"resp_1"}}`)
		for range 5 {
			send(ctx, c, textDelta)
		}
		time.AfterFunc(wait, func() {
			send(ctx, c, `{"type":"response.done","response":{"id":"resp_1","usage":`+realtimeUsage+`}}`)
		})
	}
}

func wsURL(srv *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + path
}

// readUntilClose reads events until the connection closes and returns them
// with the close error.
func readUntilClose(t *testing.T, c *websocket.Conn) ([]string, error) {
	t.Helper()
	var got []string
	for {
		m, err := wsRead(t, c)
		if err != nil {
			return got, err
		}
		got = append(got, m)
	}
}

// startResponse opens a realtime session, starts a response and reads its
// created event and deltas.
func startResponse(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	c, _, err := wsDial(t, wsURL(srv, "/v1/realtime?model=m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := wsRead(t, c); got != `{"type":"session.created"}` {
		t.Fatalf("hello %q", got)
	}
	wsSend(t, c, `{"type":"response.create"}`)
	for range 6 {
		if _, err := wsRead(t, c); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// A client that leaves mid-response does not escape billing: the upstream
// stays open until response.done brings the usage; when it does not come
// within the drain time the response is billed from an estimate.
func TestWebSocketClientLeavesMidResponse(t *testing.T) {
	t.Run("drained", func(t *testing.T) {
		up := scriptedUpstream(t, []string{`{"type":"session.created"}`}, slowResponse(300*time.Millisecond))
		meter := newWSMeter(10)
		e := realtimeEnv(t, up, "{realtime: true}")
		e.chain.Meter = meter
		c := startResponse(t, e.wsServer(WSRealtime))
		_ = c.Close(websocket.StatusNormalClosure, "bye")
		recs := e.waitRecordsOf(2)
		r, sess := recs[0], recs[1]
		if r.Usage.Estimated || r.Usage.Output != 121 || r.Status != http.StatusOK || r.ChargeMicro != 2038 {
			t.Fatalf("response record %+v", r)
		}
		meter.mu.Lock()
		defer meter.mu.Unlock()
		if meter.charges[r.ID] != 2038 {
			t.Errorf("meter charges %v", meter.charges)
		}
		if n := sess.Attempts[0].Diagnostic.Notes; !strings.Contains(n[0], "1 responses") || !strings.Contains(n[0], "drained") {
			t.Errorf("notes %v", n)
		}
	})
	t.Run("estimated", func(t *testing.T) {
		up := scriptedUpstream(t, []string{`{"type":"session.created"}`}, slowResponse(1500*time.Millisecond))
		meter := newWSMeter(10)
		e := realtimeEnv(t, up, "{realtime: true}", func(d *Deps) { d.WebSocket.Drain = 200 * time.Millisecond })
		e.chain.Meter = meter
		c := startResponse(t, e.wsServer(WSRealtime))
		_ = c.CloseNow()
		recs := e.waitRecordsOf(2)
		r, sess := recs[0], recs[1]
		// Five deltas of "hello": 25 characters, 6 tokens at 15/M.
		if !r.Usage.Estimated || r.Usage.Output != 6 || r.Status != statusClientClosed || r.ChargeMicro != 90 || !strings.Contains(r.Error, "unfinished") {
			t.Fatalf("response record %+v", r)
		}
		meter.mu.Lock()
		defer meter.mu.Unlock()
		if meter.charges[r.ID] != 90 || !meter.settled[r.ID].Estimated {
			t.Errorf("meter charges %v", meter.charges)
		}
		if n := sess.Attempts[0].Diagnostic.Notes; !strings.Contains(n[0], "drain time ran out") {
			t.Errorf("notes %v", n)
		}
	})
}

// A lost upstream closes the client with 1014; a response it left open is
// billed from an estimate.
func TestWebSocketUpstreamDrop(t *testing.T) {
	up := scriptedUpstream(t, []string{`{"type":"session.created"}`}, func(ctx context.Context, c *websocket.Conn, msg string) {
		send(ctx, c, `{"type":"response.created","response":{"id":"resp_1"}}`, textDelta, textDelta)
		_ = c.CloseNow()
	})
	meter := newWSMeter(10)
	e := realtimeEnv(t, up, "{realtime: true}")
	e.chain.Meter = meter
	c, _, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	wsSend(t, c, `{"type":"response.create"}`)
	if _, err := readUntilClose(t, c); websocket.CloseStatus(err) != websocket.StatusBadGateway {
		t.Fatalf("close: %v", err)
	}
	recs := e.waitRecordsOf(2)
	if r := recs[0]; !r.Usage.Estimated || r.Usage.Output != 2 || r.Status != http.StatusBadGateway {
		t.Fatalf("response record %+v", r)
	}
	if s := recs[1]; s.ErrorKind != ir.ErrUpstream {
		t.Errorf("session record %+v", s)
	}
}

// A relay shutdown closes the client with 1001 at once; an open response
// is billed from an estimate.
func TestWebSocketShutdown(t *testing.T) {
	up := scriptedUpstream(t, []string{`{"type":"session.created"}`}, slowResponse(5*time.Second))
	e := realtimeEnv(t, up, "{realtime: true}")
	e.chain.Meter = newWSMeter(10)
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		caller := e.caller
		e.p.ServeWebSocket(w, r.WithContext(ctx), &caller, WSRealtime)
	}))
	defer srv.Close()
	c := startResponse(t, srv)
	start := time.Now()
	close(stop)
	if _, err := readUntilClose(t, c); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("close: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("closed after %v", d)
	}
	if r := e.waitRecordsOf(2)[0]; !r.Usage.Estimated || r.Usage.Output != 6 {
		t.Fatalf("response record %+v", r)
	}
}

// A session ends at websocket.max_session with an error event and 1000.
func TestWebSocketMaxSession(t *testing.T) {
	up := newWSUpstream(t, `{}`)
	e := realtimeEnv(t, up, "{realtime: true}", func(d *Deps) { d.WebSocket.MaxSession = 300 * time.Millisecond })
	c, _, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readUntilClose(t, c)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure || len(got) != 2 || gjson.Get(got[1], "error.code").String() != "session_expired" {
		t.Fatalf("events %v, close %v", got, err)
	}
}

// An upstream message over the limit closes the client with 1009.
func TestWebSocketUpstreamMessageTooBig(t *testing.T) {
	up := scriptedUpstream(t, nil, func(ctx context.Context, c *websocket.Conn, msg string) {
		send(ctx, c, `{"type":"x","pad":"`+strings.Repeat("a", 2048)+`"}`)
	})
	e := realtimeEnv(t, up, "{realtime: true}", func(d *Deps) { d.WebSocket.MaxMessage = 1024 })
	c, _, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	wsSend(t, c, `{"type":"go"}`)
	if got, err := readUntilClose(t, c); websocket.CloseStatus(err) != websocket.StatusMessageTooBig || len(got) != 0 {
		t.Fatalf("events %v, close %v", got, err)
	}
	if s := e.waitRecordsOf(1)[0]; s.ErrorKind != ir.ErrUpstream || !strings.Contains(s.Error, "exceeds 1024 bytes") {
		t.Errorf("session record %+v", s)
	}
}

// A client event with a repeated key, invalid JSON or too deep nesting is
// answered with an error event and never forwarded: the relay reads the
// first of two keys, the upstream may read the last.
func TestWebSocketRefusesAmbiguousEvents(t *testing.T) {
	done := `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`
	up := newWSUpstream(t, done)
	e := realtimeEnv(t, up, "{responses_websocket: true}")
	c, _, err := wsDial(t, wsURL(e.wsServer(WSResponses), "/v1/responses"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wsRead(t, c)
	for _, m := range []string{
		`{"type":"response.create","model":"m","model":"gpt-secret-expensive","input":[]}`,
		`{"type":"x","type":"response.create","model":"gpt-secret-expensive"}`,
		`{"type":"response.create","model":"m","input":[{"role":"user","content":"a","content":"b"}]}`,
		`{"type":"response.create","model":"m"`,
		`["response.create"]`,
		`{"type":"x","a":` + strings.Repeat("[", 200) + strings.Repeat("]", 200) + `}`,
	} {
		wsSend(t, c, m)
		got, _ := wsRead(t, c)
		if gjson.Get(got, "type").String() != "error" || gjson.Get(got, "status").Int() != 400 {
			t.Errorf("%s: got %s", m, got)
		}
	}
	if got := up.received(); len(got) != 0 {
		t.Errorf("upstream received %v", got)
	}
}

// Responses mode: the first response.create must name a model, and an
// error event without a status (a refused response.create) does not shift
// which model the next finished response is billed as.
func TestResponsesWebSocketModelBookkeeping(t *testing.T) {
	up := scriptedUpstream(t, nil, func(ctx context.Context, c *websocket.Conn, msg string) {
		if gjson.Get(msg, "model").String() == "up-m2" {
			send(ctx, c, `{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`)
			return
		}
		send(ctx, c, `{"type":"response.created","response":{"id":"r1"}}`,
			`{"type":"response.completed","response":{"id":"r1","usage":{"input_tokens":10,"output_tokens":7}}}`)
	})
	meter := newWSMeter(10)
	e := realtimeEnv(t, up, "{responses_websocket: true}")
	e.chain.Meter = meter
	addModel(e, "m2", "up-m2")
	c, _, err := wsDial(t, wsURL(e.wsServer(WSResponses), "/v1/responses"), nil)
	if err != nil {
		t.Fatal(err)
	}
	wsSend(t, c, `{"type":"response.create","input":[]}`)
	if got, _ := wsRead(t, c); gjson.Get(got, "status").Int() != 400 || gjson.Get(got, "error.code").String() != "missing_required_parameter" {
		t.Fatalf("no model: %s", got)
	}
	wsSend(t, c, `{"type":"response.create","model":"m2","input":[]}`)
	if got, _ := wsRead(t, c); gjson.Get(got, "type").String() != "error" {
		t.Fatalf("m2: %s", got)
	}
	wsSend(t, c, `{"type":"response.create","model":"m","input":[]}`)
	for _, want := range []string{"response.created", "response.completed"} {
		if got, _ := wsRead(t, c); gjson.Get(got, "type").String() != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	recs := e.waitRecordsOf(2)
	if r := recs[0]; r.Model != "m" || r.Attempts[0].UpstreamModel != "up-m" || r.Usage.Output != 7 {
		t.Fatalf("response record %+v", r)
	}
	meter.mu.Lock()
	defer meter.mu.Unlock()
	// The handshake probe and m2's hold are refunded, m's settled.
	if len(meter.reserved) != 3 || len(meter.refunded) != 2 || len(meter.settled) != 1 {
		t.Errorf("reserved %v refunded %v settled %v", meter.reserved, meter.refunded, meter.settled)
	}
}

func addModel(e *env, name, upModel string) {
	m := e.snap.Table.Models["m"]
	m.Name = name
	m.Routes = []router.Route{{UpstreamID: "o", UpstreamModel: upModel}}
	e.snap.Table.Models[name] = m
}

const gateID = "middleware.model-gate"

// modelGate refuses models missing from its "models" setting, as a request
// plugin restricting models would.
type modelGate struct{}

func (modelGate) Info() plugin.Info {
	return plugin.Info{ID: gateID, Kind: plugin.KindMiddleware, FailPolicy: plugin.FailClosed}
}

func (modelGate) OnRequest(c *plugin.Call) (*plugin.Reply, error) {
	if ms := c.Setting("models").List(); len(ms) > 0 && !slices.Contains(ms, c.Model) {
		return &plugin.Reply{Error: ir.Errorf(ir.ErrPermission, "model %s is not allowed", c.Model)}, nil
	}
	return nil, nil
}

// Request plugins and rate limits apply: a Realtime session is one
// request at the handshake (its model, concurrency held for the session),
// each Responses response.create is one request; Authorize runs before
// anything is dialed.
func TestWebSocketRequestPlugins(t *testing.T) {
	settings := func(set map[string]map[string]any) func(*Deps) {
		return func(d *Deps) { d.Settings = pluginSettings(set) }
	}
	plugins := func(e *env) {
		e.chain.Plugins = []plugin.Plugin{access.New(), modelGate{}, ratelimit.New(newMemState())}
	}
	t.Run("realtime model", func(t *testing.T) {
		up := newWSUpstream(t, `{}`)
		e := realtimeEnv(t, up, "{realtime: true}", settings(map[string]map[string]any{gateID: {"models": []string{"other"}}}))
		plugins(e)
		_, resp, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m"), nil)
		if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden || up.calls() != 0 {
			t.Fatalf("err %v resp %+v calls %d", err, resp, up.calls())
		}
	})
	t.Run("realtime concurrency", func(t *testing.T) {
		up := newWSUpstream(t, `{}`)
		e := realtimeEnv(t, up, "{realtime: true}", settings(map[string]map[string]any{
			gateID: {"models": []string{"m"}}, ratelimit.ID: {"concurrency": 1}}))
		plugins(e)
		url := wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m")
		c, _, err := wsDial(t, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, resp, err := wsDial(t, url, nil); err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("second session: %v %+v", err, resp)
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
		e.waitRecordsOf(2)
		if _, _, err := wsDial(t, url, nil); err != nil {
			t.Fatalf("after close: %v", err)
		}
	})
	t.Run("responses per create", func(t *testing.T) {
		done := `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`
		up := newWSUpstream(t, done)
		e := realtimeEnv(t, up, "{responses_websocket: true}", settings(map[string]map[string]any{
			gateID: {"models": []string{"m"}}, ratelimit.ID: {"rpm": 1}}))
		plugins(e)
		addModel(e, "m2", "up-m2")
		c, _, err := wsDial(t, wsURL(e.wsServer(WSResponses), "/v1/responses"), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = wsRead(t, c)
		wsSend(t, c, `{"type":"response.create","model":"m2","input":[]}`)
		if got, _ := wsRead(t, c); gjson.Get(got, "status").Int() != 403 {
			t.Fatalf("m2: %s", got)
		}
		wsSend(t, c, `{"type":"response.create","model":"m","input":[]}`)
		for _, want := range []string{"response.created", "response.completed"} {
			if got, _ := wsRead(t, c); gjson.Get(got, "type").String() != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		}
		wsSend(t, c, `{"type":"response.create","model":"m","input":[]}`)
		if got, _ := wsRead(t, c); gjson.Get(got, "status").Int() != 429 {
			t.Fatalf("over rpm: %s", got)
		}
		if got := up.received(); len(got) != 1 {
			t.Errorf("upstream received %v", got)
		}
	})
	t.Run("authorize", func(t *testing.T) {
		up := newWSUpstream(t, `{}`)
		e := realtimeEnv(t, up, "{responses_websocket: true}", func(d *Deps) {
			d.WebSocket.Authorize = func(*plugin.Caller, *http.Request) *ir.GwError {
				return ir.Errorf(ir.ErrPermission, "source address is not allowed")
			}
		})
		_, resp, err := wsDial(t, wsURL(e.wsServer(WSResponses), "/v1/responses"), nil)
		if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden || up.calls() != 0 {
			t.Fatalf("err %v resp %+v calls %d", err, resp, up.calls())
		}
	})
}

// The meter holds each response's output cap at the model's price: the
// session's max_output_tokens, the request's, else the configured default;
// holds of concurrent sessions add up.
func TestWebSocketReservesPerResponse(t *testing.T) {
	t.Run("realtime", func(t *testing.T) {
		up := scriptedUpstream(t, []string{`{"type":"session.created","session":{"max_output_tokens":100}}`},
			func(ctx context.Context, c *websocket.Conn, msg string) {
				send(ctx, c, `{"type":"response.created","response":{"id":"r1"}}`,
					`{"type":"response.done","response":{"id":"r1","usage":{"input_tokens":1,"output_tokens":1}}}`)
			})
		meter := newWSMeter(10)
		e := realtimeEnv(t, up, "{realtime: true}")
		e.chain.Meter = meter
		c, _, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m"), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = wsRead(t, c)
		wsSend(t, c, `{"type":"response.create"}`)
		_, _ = wsRead(t, c)
		_, _ = wsRead(t, c)
		_ = c.Close(websocket.StatusNormalClosure, "")
		e.waitRecordsOf(2)
		meter.mu.Lock()
		defer meter.mu.Unlock()
		// 4096 tokens (default) and then 100 (the session's cap) at 15/M.
		if len(meter.reserved) != 2 || meter.amounts[meter.reserved[0]] != 61440 || meter.amounts[meter.reserved[1]] != 1500 {
			t.Fatalf("reserved %v amounts %v", meter.reserved, meter.amounts)
		}
	})
	t.Run("responses", func(t *testing.T) {
		up := newWSUpstream(t, `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`)
		meter := newWSMeter(10)
		e := realtimeEnv(t, up, "{responses_websocket: true}", func(d *Deps) { d.WebSocket.ResponseReserve = 1000 })
		e.chain.Meter = meter
		c, _, err := wsDial(t, wsURL(e.wsServer(WSResponses), "/v1/responses"), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = wsRead(t, c)
		for _, m := range []string{`{"type":"response.create","model":"m","max_output_tokens":200,"input":[]}`,
			`{"type":"response.create","model":"m","input":"` + strings.Repeat("x", 4000) + `"}`} {
			wsSend(t, c, m)
			_, _ = wsRead(t, c)
			_, _ = wsRead(t, c)
		}
		meter.mu.Lock()
		defer meter.mu.Unlock()
		// The handshake probe holds nothing; then 200 tokens out, then
		// 1000 tokens out plus the codec's input estimate (4000 characters
		// and one message: 1004 tokens) at 3/M.
		if len(meter.reserved) != 3 || meter.amounts[meter.reserved[0]] != 0 || meter.amounts[meter.reserved[1]] != 3000 ||
			meter.amounts[meter.reserved[2]] != 15000+3012 {
			t.Fatalf("reserved %v amounts %v", meter.reserved, meter.amounts)
		}
	})
	t.Run("stacked", func(t *testing.T) {
		up := newWSUpstream(t, `{}`)
		meter := newWSMeter(10)
		meter.balance = 61440 + 100
		e := realtimeEnv(t, up, "{realtime: true}")
		e.chain.Meter = meter
		url := wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m")
		if _, _, err := wsDial(t, url, nil); err != nil {
			t.Fatal(err)
		}
		if _, resp, err := wsDial(t, url, nil); err == nil || resp == nil || resp.StatusCode < 400 || up.calls() != 1 {
			t.Fatalf("second session: %v %+v calls %d", err, resp, up.calls())
		}
	})
}

// panicMeter panics on settlement.
type panicMeter struct{ *wsMeter }

func (panicMeter) Settle(*plugin.Call, *plugin.Reservation, ir.Usage, int64) error {
	panic("meter boom")
}

// A panic while relaying ends the session with 1011 instead of taking the
// process down; the session is still recorded.
func TestWebSocketRecoversPanic(t *testing.T) {
	up := newWSUpstream(t, `{"type":"response.done","response":{"usage":`+realtimeUsage+`}}`)
	e := realtimeEnv(t, up, "{realtime: true}")
	e.chain.Meter = panicMeter{newWSMeter(10)}
	c, _, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wsRead(t, c)
	wsSend(t, c, `{"type":"response.create"}`)
	if _, err := readUntilClose(t, c); websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("close: %v", err)
	}
	recs := e.waitRecordsOf(1)
	if s := recs[len(recs)-1]; s.ErrorKind != ir.ErrInternal || s.Status != http.StatusSwitchingProtocols {
		t.Errorf("session record %+v", s)
	}
}

// Input audio transcription is billed as its own request at the
// transcription model's price: tokens, or seconds of audio at its
// audio_second rate.
func TestRealtimeTranscriptionBilled(t *testing.T) {
	up := scriptedUpstream(t, []string{`{"type":"session.created","session":{"audio":{"input":{"transcription":{"model":"gpt-4o-transcribe"}}}}}`},
		func(ctx context.Context, c *websocket.Conn, msg string) {
			switch gjson.Get(msg, "type").String() {
			case "tokens":
				send(ctx, c, `{"type":"conversation.item.input_audio_transcription.completed","item_id":"i1","transcript":"hi",`+
					`"usage":{"type":"tokens","input_tokens":50,"output_tokens":10,"total_tokens":60,"input_token_details":{"audio_tokens":50}}}`)
			case "duration":
				send(ctx, c, `{"type":"session.updated","session":{"input_audio_transcription":{"model":"whisper-1"}}}`,
					`{"type":"conversation.item.input_audio_transcription.completed","item_id":"i2","transcript":"hi","usage":{"type":"duration","seconds":2.5}}`)
			}
		})
	meter := newWSMeter(10)
	e := realtimeEnv(t, up, "{realtime: true}")
	e.chain.Meter = meter
	e.snap.Price = func(_, model string) (Price, bool) {
		if model == "whisper-1" {
			return Price{Units: map[string]int64{plugin.UnitAudioSecond: 100}}, true
		}
		return testPrice, true
	}
	c, _, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wsRead(t, c)
	wsSend(t, c, `{"type":"tokens"}`)
	_, _ = wsRead(t, c)
	wsSend(t, c, `{"type":"duration"}`)
	_, _ = wsRead(t, c)
	_, _ = wsRead(t, c)
	_ = c.Close(websocket.StatusNormalClosure, "")
	recs := e.waitRecordsOf(3)
	if r := recs[0]; r.Model != "gpt-4o-transcribe" || r.Usage.Input != 50 || r.Usage.Output != 10 || r.ChargeMicro != 300 {
		t.Errorf("token transcription %+v", r)
	}
	if r := recs[1]; r.Model != "whisper-1" || r.Usage.AudioMs != 2500 || r.ChargeMicro != 250 {
		t.Errorf("duration transcription %+v", r)
	}
	meter.mu.Lock()
	defer meter.mu.Unlock()
	if meter.charges[recs[0].ID] != 300 || meter.charges[recs[1].ID] != 250 {
		t.Errorf("meter charges %v", meter.charges)
	}
	if n := recs[2].Attempts[0].Diagnostic.Notes; !strings.Contains(n[0], "2 transcriptions") {
		t.Errorf("notes %v", n)
	}
}

// A Realtime session.update may name the session's model (renamed to the
// upstream's) but not switch to another.
func TestRealtimeSessionModelFixed(t *testing.T) {
	up := newWSUpstream(t, `{}`)
	e := realtimeEnv(t, up, "{realtime: true}")
	c, _, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wsRead(t, c)
	wsSend(t, c, `{"type":"session.update","session":{"model":"gpt-other"}}`)
	if got, _ := wsRead(t, c); gjson.Get(got, "type").String() != "error" {
		t.Fatalf("switch: %s", got)
	}
	wsSend(t, c, `{"type":"session.update","session":{"model":"m"}}`)
	if got, _ := wsRead(t, c); gjson.Get(got, "of.session.model").String() != "up-m" {
		t.Fatalf("same model: %s", got)
	}
}

// Azure OpenAI's GA realtime path: /openai/v1/realtime?model=<deployment>
// with the api-key header and no api-version.
func TestRealtimeAzureGAPath(t *testing.T) {
	up := newWSUpstream(t, `{}`)
	e := realtimeEnv(t, up, "{realtime: true}")
	prov := compatible.New("openai", compatible.WithDefaultManifest("openai"), compatible.WithIssuers(compatible.OpenAIIssuers...))
	cfg := &plugin.Upstream{ID: "o", Provider: prov.Info().ID, Config: map[string]any{"manifest": "azure-openai", "resource": "acme"}}
	eps, err := prov.Endpoints(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range eps {
		eps[i].BaseURL = strings.Replace(eps[i].BaseURL, "https://acme.openai.azure.com", up.srv.URL, 1)
	}
	cfg.Endpoints = eps
	e.snap.Upstreams["o"] = &Upstream{Config: cfg, Provider: prov, AllowPrivate: true,
		Credentials: map[string]*plugin.Credential{"c1": {ID: "c1", UpstreamID: "o", Kind: "shared", Secret: "az-key"}}}
	u := e.snap.Table.Upstreams["o"]
	u.Endpoints = eps
	e.snap.Table.Upstreams["o"] = u
	c, _, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m"), http.Header{"Authorization": {"Bearer pr_client"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wsRead(t, c)
	if p := up.paths[0]; p != "/openai/v1/realtime?model=up-m" {
		t.Errorf("upstream path %s", p)
	}
	if h := up.headers[0]; h.Get("Api-Key") != "az-key" || h.Get("Authorization") != "" {
		t.Errorf("upstream headers %v", h)
	}
}

// A malformed handshake is refused before anything is dialed.
func TestWebSocketHandshakeChecked(t *testing.T) {
	up := newWSUpstream(t, `{}`)
	e := realtimeEnv(t, up, "{realtime: true}")
	srv := e.wsServer(WSRealtime)
	for _, tc := range []struct {
		version, key string
		status       int
	}{{"8", "dGhlIHNhbXBsZSBub25jZQ==", http.StatusUpgradeRequired}, {"13", "short", http.StatusBadRequest}} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/realtime?model=m", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", tc.version)
		req.Header.Set("Sec-WebSocket-Key", tc.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status || (tc.status == http.StatusUpgradeRequired && resp.Header.Get("Sec-WebSocket-Version") != "13") {
			t.Errorf("version %s key %s: %d %v", tc.version, tc.key, resp.StatusCode, resp.Header)
		}
	}
	if up.calls() != 0 {
		t.Errorf("upstream dialed %d times", up.calls())
	}
}

// The browser key subprotocol is dropped in any letter case.
func TestWebSocketKeySubprotocolCase(t *testing.T) {
	up := newWSUpstream(t, `{}`)
	e := realtimeEnv(t, up, "{realtime: true}")
	c, _, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m"), nil, "realtime", "OpenAI-Insecure-API-Key.pr_x")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wsRead(t, c)
	if p := up.headers[0].Get("Sec-WebSocket-Protocol"); p != "realtime" {
		t.Errorf("upstream subprotocols %q", p)
	}
}

// An upstream handshake error longer than the WebSocket client keeps is
// wrapped in a whole relay error instead of passed on cut mid-JSON.
func TestWebSocketLongHandshakeError(t *testing.T) {
	body := `{"error":{"message":"` + strings.Repeat("x", 3000) + `","type":"server_error"}}`
	up := &wsUpstream{fakeUpstream: newFake(t, jsonReply(http.StatusServiceUnavailable, body))}
	e := realtimeEnv(t, up, "{realtime: true}")
	_, resp, err := wsDial(t, wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m"), nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err %v resp %+v", err, resp)
	}
	b, _ := io.ReadAll(resp.Body)
	if !gjson.ValidBytes(b) || !strings.Contains(gjson.GetBytes(b, "error.message").String(), "xxxx") {
		t.Errorf("body %s", b)
	}
}

// Many sessions closing every way leave no goroutines behind.
func TestWebSocketNoLeaks(t *testing.T) {
	up := newWSUpstream(t, `{"type":"response.done","response":{"usage":`+realtimeUsage+`}}`)
	e := realtimeEnv(t, up, "{realtime: true}")
	url := wsURL(e.wsServer(WSRealtime), "/v1/realtime?model=m")
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, err := wsDial(t, url, nil)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = wsRead(t, c)
			wsSend(t, c, `{"type":"response.create"}`)
			_, _ = wsRead(t, c)
			if i%2 == 0 {
				_ = c.Close(websocket.StatusNormalClosure, "")
			} else {
				_ = c.CloseNow()
			}
		}()
	}
	wg.Wait()
	e.waitRecordsOf(10)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.p.Wait(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	} else if err != nil {
		t.Fatal("sessions still running")
	}
}

// An upstream handshake answer whose body is not whole JSON (the WebSocket
// client keeps its first KiB) is wrapped in a relay error; whole JSON
// passes verbatim.
func TestWebSocketFailVerbatim(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"error":{"message":"cut`, "upstream answered 502 to the WebSocket handshake: {\"error\":{\"message\":\"cut"},
		{`{"error":{"message":"whole"}}`, "whole"},
	} {
		w := httptest.NewRecorder()
		s := &wsSession{w: w, rec: &plugin.Record{}}
		s.failVerbatim(&upstreamError{status: http.StatusBadGateway, header: http.Header{"Content-Length": {"1024"}}, body: []byte(tc.body)})
		b := w.Body.Bytes()
		if w.Code != http.StatusBadGateway || !gjson.ValidBytes(b) || gjson.GetBytes(b, "error.message").String() != tc.want ||
			w.Header().Get("Content-Length") != "" {
			t.Errorf("%s: %d %v %s", tc.body, w.Code, w.Header(), b)
		}
	}
}
