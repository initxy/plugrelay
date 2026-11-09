package pipeline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// wsUpstream is a fake upstream WebSocket server. It sends hello on
// connect, echoes every client message as {"type":"echo","of":<message>},
// and answers response.create with response.created and done (the
// protocol's terminal event, carrying usage).
type wsUpstream struct {
	*fakeUpstream
	mu    sync.Mutex
	got   []string
	hello string
	done  string
}

const realtimeUsage = `{"total_tokens":253,"input_tokens":132,"output_tokens":121,"input_token_details":{"text_tokens":119,"audio_tokens":13,"cached_tokens":64,"cached_tokens_details":{"text_tokens":64,"audio_tokens":0}},"output_token_details":{"text_tokens":30,"audio_tokens":91}}`

func newWSUpstream(t *testing.T, done string) *wsUpstream {
	u := &wsUpstream{hello: `{"type":"session.created"}`, done: done}
	u.fakeUpstream = newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		var subs []string
		if strings.Contains(r.Header.Get("Sec-WebSocket-Protocol"), "realtime") {
			subs = []string{"realtime"}
		}
		w.Header().Set("X-Codex-Turn-State", "ts1")
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: subs})
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(1 << 20)
		ctx := context.Background()
		_ = c.Write(ctx, websocket.MessageText, []byte(u.hello))
		for {
			typ, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			u.mu.Lock()
			u.got = append(u.got, string(b))
			u.mu.Unlock()
			if typ == websocket.MessageText && gjson.GetBytes(b, "type").String() == "response.create" {
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.created"}`))
				_ = c.Write(ctx, websocket.MessageText, []byte(u.done))
				continue
			}
			_ = c.Write(ctx, typ, append(append([]byte(`{"type":"echo","of":`), b...), '}'))
		}
	})
	return u
}

func (u *wsUpstream) received() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.got...)
}

// wsMeter records settlements and refuses reservations after allow, or
// (balance > 0) when the amounts held would exceed the balance.
type wsMeter struct {
	mu       sync.Mutex
	allow    int
	balance  int64
	reserved []string
	amounts  map[string]int64
	held     map[string]int64
	settled  map[string]ir.Usage
	charges  map[string]int64
	refunded []string
}

func (m *wsMeter) Info() plugin.Info { return plugin.Info{ID: "meter.ws", Kind: plugin.KindMeter} }

func (m *wsMeter) Reserve(c *plugin.Call, amount int64) (*plugin.Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var held int64
	for _, v := range m.held {
		held += v
	}
	if len(m.reserved) >= m.allow || (m.balance > 0 && held+amount > m.balance) {
		e := ir.Errorf(ir.ErrQuotaExhausted, "balance too low")
		e.Code = "insufficient_quota"
		return nil, e
	}
	m.reserved = append(m.reserved, c.ID)
	m.amounts[c.ID], m.held[c.ID] = amount, amount
	return &plugin.Reservation{ID: c.ID, AmountMicro: amount}, nil
}

func (m *wsMeter) Settle(c *plugin.Call, r *plugin.Reservation, u ir.Usage, charge int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settled[r.ID], m.charges[r.ID] = u, charge
	delete(m.held, r.ID)
	return nil
}

func (m *wsMeter) Refund(c *plugin.Call, r *plugin.Reservation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refunded = append(m.refunded, r.ID)
	delete(m.held, r.ID)
	return nil
}

func newWSMeter(allow int) *wsMeter {
	return &wsMeter{allow: allow, settled: map[string]ir.Usage{}, charges: map[string]int64{},
		amounts: map[string]int64{}, held: map[string]int64{}}
}

// wsServer serves ServeWebSocket the way the HTTP server does after
// authentication.
func (e *env) wsServer(kind string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := e.caller
		e.p.ServeWebSocket(w, r, &caller, kind)
	}))
	e.t.Cleanup(srv.Close)
	return srv
}

func wsDial(t *testing.T, url string, h http.Header, subs ...string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h, Subprotocols: subs})
	if c != nil {
		c.SetReadLimit(1 << 20)
		t.Cleanup(func() { _ = c.CloseNow() })
	}
	return c, resp, err
}

func wsRead(t *testing.T, c *websocket.Conn) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	return string(b), err
}

func wsSend(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	if err := c.Write(context.Background(), websocket.MessageText, []byte(s)); err != nil {
		t.Fatal(err)
	}
}

func realtimeEnv(t *testing.T, up *wsUpstream, caps string, mod ...func(*Deps)) *env {
	t.Helper()
	return newEnv(t, "m", []upSpec{{id: "o", protocol: "openai-responses", issuer: "iss-o", fake: up.fakeUpstream,
		model: "up-m", caps: caps}}, mod...)
}

// waitRecordsOf waits until n records were written and returns them all.
func (e *env) waitRecordsOf(n int) []*plugin.Record {
	e.t.Helper()
	e.waitRecords(n)
	e.records.mu.Lock()
	defer e.records.mu.Unlock()
	return append([]*plugin.Record(nil), e.records.recs...)
}

// A realtime session reaches the endpoint declaring realtime with the
// credential's key, the upstream model and no client credential; messages
// cross both ways in order; each response.done is settled and recorded,
// and the session gets its own record.
func TestRealtimeRelaysAndMeters(t *testing.T) {
	up := newWSUpstream(t, `{"type":"response.done","response":{"status":"completed","usage":`+realtimeUsage+`}}`)
	meter := newWSMeter(10)
	e := realtimeEnv(t, up, "{realtime: true}")
	e.chain.Meter = meter
	srv := e.wsServer(WSRealtime)
	h := http.Header{"Authorization": {"Bearer pr_client"}, "Openai-Beta": {"realtime=v1"}}
	c, resp, err := wsDial(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime?model=m&key=pr_client", h,
		"realtime", InsecureKeyProtocol+"pr_client")
	if err != nil {
		t.Fatalf("dial: %v %+v", err, resp)
	}
	if c.Subprotocol() != "realtime" || resp.Header.Get("X-Codex-Turn-State") != "ts1" {
		t.Errorf("subprotocol %q, headers %v", c.Subprotocol(), resp.Header)
	}
	if got, _ := wsRead(t, c); got != `{"type":"session.created"}` {
		t.Fatalf("first event %q", got)
	}
	for _, m := range []string{`{"type":"a"}`, `{"type":"b"}`, `{"type":"c"}`} {
		wsSend(t, c, m)
	}
	for _, m := range []string{`{"type":"a"}`, `{"type":"b"}`, `{"type":"c"}`} {
		if got, _ := wsRead(t, c); got != `{"type":"echo","of":`+m+`}` {
			t.Fatalf("got %q, want echo of %s", got, m)
		}
	}
	wsSend(t, c, `{"type":"response.create"}`)
	for _, want := range []string{"response.created", "response.done"} {
		if got, _ := wsRead(t, c); gjson.Get(got, "type").String() != want {
			t.Fatalf("got %q, want %s", got, want)
		}
	}
	_ = c.Close(websocket.StatusNormalClosure, "bye")

	recs := e.waitRecordsOf(2)
	resp1, sess := recs[0], recs[1]
	if resp1.Protocol != RealtimeProtocol || resp1.Model != "m" || resp1.SessionID != sess.ID {
		t.Fatalf("response record %+v", resp1)
	}
	if u := resp1.Usage; u.Input != 68 || u.CacheRead != 64 || u.Output != 121 || !strings.Contains(string(u.Raw), `"audio_tokens":91`) {
		t.Errorf("usage %+v", u)
	}
	// testPrice: 3/M input, 0.3/M cache read, 15/M output: 204 + 19.2 + 1815.
	if want := int64(2038); resp1.CostMicro != want || resp1.ChargeMicro != want {
		t.Errorf("cost %d charge %d, want %d", resp1.CostMicro, resp1.ChargeMicro, want)
	}
	if meter.settled[resp1.ID].Output != 121 || meter.charges[resp1.ID] != resp1.ChargeMicro {
		t.Errorf("meter settled %+v", meter.settled)
	}
	if len(meter.reserved) != 2 || meter.reserved[0] != resp1.ID || len(meter.refunded) != 1 || meter.refunded[0] != meter.reserved[1] {
		t.Errorf("reserved %v refunded %v", meter.reserved, meter.refunded)
	}
	if sess.Protocol != RealtimeProtocol || sess.Status != http.StatusSwitchingProtocols || sess.Usage.Output != 0 || sess.ChargeMicro != 0 || len(sess.Attempts) != 1 {
		t.Fatalf("session record %+v", sess)
	}
	a := sess.Attempts[0]
	if a.BytesIn == 0 || a.BytesOut == 0 || a.UpstreamID != "o" || a.CredentialID != "c1" || a.UpstreamModel != "up-m" {
		t.Errorf("session attempt %+v", a)
	}
	if n := a.Diagnostic.Notes; len(n) != 1 || !strings.Contains(n[0], "1 responses") || !strings.Contains(n[0], "closed by client (1000)") {
		t.Errorf("notes %v", n)
	}

	if up.paths[0] != "/v1/realtime?model=up-m" {
		t.Errorf("upstream path %s", up.paths[0])
	}
	uh := up.headers[0]
	if uh.Get("Authorization") != "Bearer sk-o" || uh.Get("Openai-Beta") != "realtime=v1" {
		t.Errorf("upstream headers %v", uh)
	}
	if p := uh.Get("Sec-WebSocket-Protocol"); strings.Contains(p, "pr_client") || p != "realtime" {
		t.Errorf("upstream subprotocols %q", p)
	}
	got := up.received()
	if len(got) != 4 || got[0] != `{"type":"a"}` || got[3] != `{"type":"response.create"}` {
		t.Errorf("upstream received %v", got)
	}
}

// Without an endpoint declaring realtime the handshake is refused with an
// HTTP error and nothing is dialed.
func TestRealtimeNeedsCapability(t *testing.T) {
	up := newWSUpstream(t, `{}`)
	e := realtimeEnv(t, up, "")
	srv := e.wsServer(WSRealtime)
	_, resp, err := wsDial(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime?model=m", nil)
	if err == nil || resp == nil || resp.StatusCode < 400 || up.calls() != 0 {
		t.Fatalf("err %v resp %+v calls %d", err, resp, up.calls())
	}
	_, resp, _ = wsDial(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime", nil)
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing model: %+v", resp)
	}
}

// A balance that cannot pay refuses the handshake; one that runs out after
// a response ends the session with an error event and close code 1008.
func TestRealtimeMeterRefusal(t *testing.T) {
	up := newWSUpstream(t, `{"type":"response.done","response":{"usage":`+realtimeUsage+`}}`)
	e := realtimeEnv(t, up, "{realtime: true}")
	e.chain.Meter = newWSMeter(0)
	srv := e.wsServer(WSRealtime)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime?model=m"
	_, resp, err := wsDial(t, url, nil)
	if err == nil || resp == nil || resp.StatusCode < 400 || up.calls() != 0 {
		t.Fatalf("refused start: err %v resp %+v calls %d", err, resp, up.calls())
	}

	meter := newWSMeter(1)
	e.chain.Meter = meter
	c, _, err := wsDial(t, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wsRead(t, c)
	wsSend(t, c, `{"type":"response.create"}`)
	var last string
	for {
		got, err := wsRead(t, c)
		if err != nil {
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("close: %v", err)
			}
			break
		}
		last = got
	}
	if gjson.Get(last, "type").String() != "error" || gjson.Get(last, "error.code").String() != "insufficient_quota" {
		t.Fatalf("last event %s", last)
	}
	recs := e.waitRecordsOf(3)
	if len(meter.settled) != 1 || recs[1].Usage.Output != 121 || recs[2].ErrorKind != ir.ErrQuotaExhausted {
		t.Fatalf("settled %v records %+v %+v", meter.settled, recs[1], recs[2])
	}
}

// The upstream's own answer to the handshake reaches the client verbatim.
func TestWebSocketUpstreamHandshakeError(t *testing.T) {
	body := `{"error":{"message":"Model not found up-m","type":"invalid_request_error","code":"model_not_found"}}`
	up := &wsUpstream{fakeUpstream: newFake(t, jsonReply(http.StatusBadRequest, body))}
	e := realtimeEnv(t, up, "{realtime: true}")
	srv := e.wsServer(WSRealtime)
	_, resp, err := wsDial(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime?model=m", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("err %v resp %+v", err, resp)
	}
	b := make([]byte, 512)
	n, _ := resp.Body.Read(b)
	if !strings.Contains(string(b[:n]), "model_not_found") {
		t.Errorf("body %q", b[:n])
	}
}

// The credential's concurrency slot is held for the session and freed when
// it closes.
func TestWebSocketHoldsConcurrencySlot(t *testing.T) {
	up := newWSUpstream(t, `{}`)
	e := realtimeEnv(t, up, "{realtime: true}")
	u := e.snap.Table.Upstreams["o"]
	u.Credentials[0].MaxConcurrency = 1
	e.snap.Table.Upstreams["o"] = u
	srv := e.wsServer(WSRealtime)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime?model=m"
	c, _, err := wsDial(t, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wsRead(t, c)
	if _, resp, err := wsDial(t, url, nil); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second session while the first runs: %v %+v", err, resp)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	e.waitRecordsOf(2)
	c2, _, err := wsDial(t, url, nil)
	if err != nil {
		t.Fatalf("after close: %v", err)
	}
	_ = c2.CloseNow()
}

// The upstream WebSocket is dialed through the core egress: a private
// address is refused unless the upstream allows it.
func TestWebSocketSSRFGuard(t *testing.T) {
	up := newWSUpstream(t, `{}`)
	e := realtimeEnv(t, up, "{realtime: true}")
	e.snap.Upstreams["o"].AllowPrivate = false
	srv := e.wsServer(WSRealtime)
	_, resp, err := wsDial(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime?model=m", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusBadGateway || up.calls() != 0 {
		t.Fatalf("err %v resp %+v calls %d", err, resp, up.calls())
	}
	rec := e.waitRecords(1)
	if len(rec.Attempts) != 1 || !strings.Contains(rec.Attempts[0].Error, "non-public address") {
		t.Fatalf("record %+v", rec)
	}
}

// A message over the limit closes the session with 1009 and is not
// forwarded.
func TestWebSocketMaxMessage(t *testing.T) {
	up := newWSUpstream(t, `{}`)
	e := realtimeEnv(t, up, "{realtime: true}", func(d *Deps) { d.WebSocket.MaxMessage = 1024 })
	srv := e.wsServer(WSRealtime)
	c, _, err := wsDial(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime?model=m", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wsRead(t, c)
	wsSend(t, c, `{"type":"x","pad":"`+strings.Repeat("a", 2048)+`"}`)
	if _, err := wsRead(t, c); websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("read after oversized message: %v", err)
	}
	if got := up.received(); len(got) != 0 {
		t.Errorf("upstream received %d messages", len(got))
	}
}

// A session with no message either way for the idle timeout is closed.
func TestWebSocketIdleTimeout(t *testing.T) {
	up := newWSUpstream(t, `{}`)
	e := realtimeEnv(t, up, "{realtime: true}", func(d *Deps) { d.WebSocket.Idle = 200 * time.Millisecond })
	srv := e.wsServer(WSRealtime)
	c, _, err := wsDial(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime?model=m", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wsRead(t, c)
	start := time.Now()
	_, err = wsRead(t, c)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusNormalClosure || ce.Reason != "idle timeout" {
		t.Fatalf("read: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("closed after %v", d)
	}
}

// The Responses WebSocket mode is relayed only when one upstream with
// responses_websocket serves every model; response.create carries the
// upstream model and response.completed is billed. Otherwise: 426.
func TestResponsesWebSocket(t *testing.T) {
	done := `{"type":"response.completed","response":{"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},"output_tokens":10,"output_tokens_details":{"reasoning_tokens":4}}}}`
	up := newWSUpstream(t, done)
	e := realtimeEnv(t, up, "{responses_websocket: true}")
	srv := e.wsServer(WSResponses)
	c, resp, err := wsDial(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/responses", http.Header{"Openai-Beta": {"responses_websockets=2026-02-06"}})
	if err != nil {
		t.Fatalf("dial: %v %+v", err, resp)
	}
	if resp.Header.Get("X-Codex-Turn-State") != "ts1" {
		t.Errorf("101 headers %v", resp.Header)
	}
	_, _ = wsRead(t, c)
	wsSend(t, c, `{"type":"response.create","model":"other-model","input":[]}`)
	if got, _ := wsRead(t, c); gjson.Get(got, "type").String() != "error" || gjson.Get(got, "status").Int() != 404 {
		t.Fatalf("unknown model: %s", got)
	}
	wsSend(t, c, `{"type":"response.create","model":"m","input":[]}`)
	for _, want := range []string{"response.created", "response.completed"} {
		if got, _ := wsRead(t, c); gjson.Get(got, "type").String() != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	recs := e.waitRecordsOf(2)
	if r := recs[0]; r.Protocol != ResponsesWSProtocol || r.Model != "m" || r.Usage.Input != 60 || r.Usage.CacheRead != 40 || r.Usage.Reasoning != 4 {
		t.Errorf("response record %+v", r)
	}
	if got := up.received(); len(got) != 1 || gjson.Get(got[0], "model").String() != "up-m" {
		t.Errorf("upstream received %v", got)
	}
	if up.headers[0].Get("Openai-Beta") != "responses_websockets=2026-02-06" || up.paths[0] != "/v1/responses" {
		t.Errorf("upstream %s %v", up.paths[0], up.headers[0])
	}

	plain := newWSUpstream(t, done)
	e2 := newEnv(t, "m", []upSpec{
		{id: "o", protocol: "openai-responses", issuer: "iss-o", fake: up.fakeUpstream, caps: "{responses_websocket: true}"},
		{id: "p", protocol: "openai-responses", issuer: "iss-p", fake: plain.fakeUpstream},
	})
	e2.snap.Table.Models["only-p"] = e2.snap.Table.Models["m"]
	mp := e2.snap.Table.Models["only-p"]
	mp.Name = "only-p"
	mp.Routes = mp.Routes[1:]
	e2.snap.Table.Models["only-p"] = mp
	srv2 := e2.wsServer(WSResponses)
	_, resp, err = wsDial(t, "ws"+strings.TrimPrefix(srv2.URL, "http")+"/v1/responses", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("mixed upstreams: %v %+v", err, resp)
	}
	if plain.calls() != 0 {
		t.Errorf("plain upstream dialed")
	}
}
