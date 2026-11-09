package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/initxy/plugrelay/internal/httpx"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// WebSocket serves one authenticated WebSocket upgrade; *pipeline.Pipeline
// implements it. kind is "realtime" or "responses".
type WebSocket interface {
	ServeWebSocket(w http.ResponseWriter, r *http.Request, caller *plugin.Caller, kind string)
}

// insecureKeyProtocol prefixes the subprotocol browsers carry an API key in,
// since they cannot set headers on a WebSocket; matched ignoring case.
const insecureKeyProtocol = "openai-insecure-api-key."

// serveWebSocket authenticates a WebSocket upgrade on /v1/realtime or
// /v1/responses (with the server's usual path variants) and hands it on;
// it reports false for other paths, or when WebSocket serving is off. The
// session's context also ends when the server starts shutting down.
func (s *Server) serveWebSocket(w http.ResponseWriter, r *http.Request) bool {
	if s.o.WebSocket == nil {
		return false
	}
	var kind string
	switch httpx.CleanPath(r.URL.Path) {
	case "/v1/realtime", "/realtime", "/openai/v1/realtime":
		kind = "realtime"
	case "/v1/responses", "/responses", "/openai/v1/responses":
		kind = "responses"
	default:
		return false
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(s.stopping, cancel)
	defer stop()
	r = r.Clone(ctx)
	// The upgrade skips the origin check, so an ambient browser credential
	// (the console's session cookie) must not authenticate it.
	r.Header.Del("Cookie")
	liftSubprotocolKey(r)
	caller, gw := s.authenticate(r)
	if gw != nil {
		if c, ok := protocol.Get(codecResponses); ok {
			writeCodecError(w, c, gw, 0)
		} else {
			writeJSONError(w, http.StatusUnauthorized, "authentication_error", gw.Message)
		}
		return true
	}
	s.o.WebSocket.ServeWebSocket(w, r, caller, kind)
	return true
}

// liftSubprotocolKey moves an API key carried as an
// "openai-insecure-api-key.<key>" subprotocol into the Authorization header
// (unless one is set) and removes it from the offered subprotocols, so it
// is neither echoed nor forwarded.
func liftSubprotocolKey(r *http.Request) {
	var keep []string
	key, found := "", false
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if len(p) >= len(insecureKeyProtocol) && strings.EqualFold(p[:len(insecureKeyProtocol)], insecureKeyProtocol) {
				key, found = p[len(insecureKeyProtocol):], true
				continue
			}
			if p != "" {
				keep = append(keep, p)
			}
		}
	}
	if !found {
		return
	}
	r.Header.Del("Sec-WebSocket-Protocol")
	if len(keep) > 0 {
		r.Header.Set("Sec-WebSocket-Protocol", strings.Join(keep, ", "))
	}
	if key != "" && r.Header.Get("Authorization") == "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
}
