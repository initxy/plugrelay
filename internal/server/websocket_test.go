package server

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/initxy/plugrelay/pkg/plugin"
)

type fakeWS struct {
	kind, auth, protocols string
	caller                *plugin.Caller
}

func (f *fakeWS) ServeWebSocket(w http.ResponseWriter, r *http.Request, caller *plugin.Caller, kind string) {
	f.kind, f.caller = kind, caller
	f.auth, f.protocols = r.Header.Get("Authorization"), r.Header.Get("Sec-WebSocket-Protocol")
	w.WriteHeader(http.StatusTeapot)
}

// WebSocket upgrades on the realtime and responses paths are authenticated
// (also by the browser subprotocol key, which is then removed) before they
// reach the pipeline; without a WebSocket handler realtime is 404 and the
// responses upgrade keeps its 426.
func TestWebSocketDispatch(t *testing.T) {
	s, _ := newTestServer(false)
	up := []string{"Upgrade", "websocket", "Connection", "Upgrade"}
	if w := do(s, "GET", "/v1/realtime?model=m", "", append(up, "Authorization", "Bearer pr_good")...); w.Code != http.StatusNotFound {
		t.Errorf("realtime without handler: %d", w.Code)
	}

	ws := &fakeWS{}
	s.o.WebSocket = ws
	w := do(s, "GET", "/v1/realtime?model=m", "", append(up, "Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.pr_good")...)
	if w.Code != http.StatusTeapot || ws.kind != "realtime" || ws.caller == nil || ws.caller.KeyID != "k1" {
		t.Fatalf("subprotocol key: %d %+v", w.Code, ws)
	}
	if ws.protocols != "realtime" || ws.auth != "Bearer pr_good" {
		t.Errorf("handed on with protocols %q auth %q", ws.protocols, ws.auth)
	}
	*ws = fakeWS{}
	if w := do(s, "GET", "/responses", "", append(up, "Authorization", "Bearer pr_good")...); w.Code != http.StatusTeapot || ws.kind != "responses" {
		t.Errorf("responses: %d %+v", w.Code, ws)
	}
	*ws = fakeWS{}
	if w := do(s, "GET", "/v1/realtime?model=m", "", up...); w.Code != http.StatusUnauthorized || ws.kind != "" {
		t.Errorf("no key: %d %+v", w.Code, ws)
	}
	if w := do(s, "GET", "/v1/realtime?model=m", "", append(up, "Authorization", "Bearer pr_bad")...); w.Code != http.StatusUnauthorized || ws.kind != "" {
		t.Errorf("bad key: %d %+v", w.Code, ws)
	}
}

// The console's session cookie never reaches the authenticators of a
// WebSocket upgrade (it skips the origin check), and the browser key
// subprotocol is lifted in any letter case.
func TestWebSocketDropsCookie(t *testing.T) {
	s, _ := newTestServer(false)
	ws := &fakeWS{}
	s.o.WebSocket = ws
	var sawCookie bool
	s.o.Auth = func() []plugin.Authenticator {
		return []plugin.Authenticator{cookieSpy{seen: &sawCookie}, keyAuth{}}
	}
	up := []string{"Upgrade", "websocket", "Connection", "Upgrade", "Cookie", "pr_session=abc",
		"Sec-WebSocket-Protocol", "realtime, OpenAI-Insecure-API-Key.pr_good"}
	w := do(s, "GET", "/v1/realtime?model=m", "", up...)
	if w.Code != http.StatusTeapot || sawCookie || ws.auth != "Bearer pr_good" || ws.protocols != "realtime" {
		t.Fatalf("%d cookie %v %+v", w.Code, sawCookie, ws)
	}
}

type cookieSpy struct{ seen *bool }

func (cookieSpy) Info() plugin.Info { return plugin.Info{ID: "auth.cookiespy", Kind: plugin.KindAuth} }

func (c cookieSpy) Authenticate(r *http.Request) (*plugin.Caller, error) {
	*c.seen = r.Header.Get("Cookie") != ""
	return nil, nil
}

type shutdownWS struct{ ended chan string }

func (e *shutdownWS) ServeWebSocket(w http.ResponseWriter, r *http.Request, _ *plugin.Caller, _ string) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		e.ended <- "accept: " + err.Error()
		return
	}
	select {
	case <-r.Context().Done():
		e.ended <- "ctx done"
	case <-time.After(4 * time.Second):
		e.ended <- "not canceled"
	}
	_ = c.Close(websocket.StatusGoingAway, "relay shutting down")
}

// Shutting the real server down cancels a hijacked WebSocket session's
// context at once, so the session closes with 1001 instead of running on.
func TestWebSocketShutdownThroughServer(t *testing.T) {
	s, _ := newTestServer(false)
	ws := &shutdownWS{ended: make(chan string, 1)}
	s.o.WebSocket = ws
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	c, _, err := websocket.Dial(context.Background(), "ws://"+ln.Addr().String()+"/v1/realtime?model=m",
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer pr_good"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	cancel()
	select {
	case m := <-ws.ended:
		if m != "ctx done" {
			t.Fatalf("session ended: %s", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session not ended by shutdown")
	}
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	if _, _, err := c.Read(rctx); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Errorf("client read: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("server did not stop")
	}
}
