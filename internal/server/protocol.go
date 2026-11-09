package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/httpx"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/protocol/openairesponses"
)

// upgradeRefusers are codecs that answer a WebSocket upgrade on their
// routes with their own body (426 so the client falls back to HTTP
// streaming).
var upgradeRefusers = map[string]func() (int, []byte){
	codecResponses: openairesponses.UpgradeRequired,
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// refuseUpgrade answers a WebSocket upgrade on a protocol path; it reports
// false when the path is not a protocol route.
func (s *Server) refuseUpgrade(w http.ResponseWriter, r *http.Request) bool {
	m, ok := s.routes.matchAny(r)
	if !ok {
		return false
	}
	var status int
	var body []byte
	if f, ok := upgradeRefusers[m.codec.Name()]; ok {
		status, body = f()
	} else {
		st, h, b := m.codec.EncodeError(ir.Errorf(ir.ErrInvalidRequest, "WebSocket transport is not supported; use HTTP"))
		copyHeader(w.Header(), h)
		status, body = st, b
		if status < 400 {
			status = http.StatusUpgradeRequired
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return true
}

func (s *Server) serveProtocol(w http.ResponseWriter, r *http.Request, m matched) {
	if m.route.Op == protocol.OpHello {
		w.WriteHeader(http.StatusOK)
		return
	}
	caller, gw := s.authenticate(r)
	if gw != nil {
		writeCodecError(w, m.codec, gw, 0)
		return
	}
	var body []byte
	if m.route.Upload {
		// Streamed by the pipeline; only the wire size is bounded here.
		if gw, status := s.limitUpload(w, r); gw != nil {
			writeCodecError(w, m.codec, gw, status)
			return
		}
	} else if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(BodyReadTimeout))
		b, err := httpx.ReadBody(r, s.o.Limits)
		_ = rc.SetReadDeadline(time.Time{})
		if err != nil {
			writeCodecError(w, m.codec, ir.Errorf(ir.ErrInvalidRequest, "%s", err.Error()), httpx.Status(err))
			return
		}
		body = b
	}
	s.o.Pipeline.ServeProtocol(w, r, m.codec, m.route, caller, body)
}

// authenticate runs the enabled authenticators in order; the first caller
// wins and the first error rejects.
func (s *Server) authenticate(r *http.Request) (*plugin.Caller, *ir.GwError) {
	for _, a := range s.o.Auth() {
		c, err := a.Authenticate(r)
		if err != nil {
			var gw *ir.GwError
			if errors.As(err, &gw) {
				return nil, gw
			}
			if plugin.IsFailure(err) && a.Info().FailPolicy == plugin.FailOpen {
				continue
			}
			return nil, ir.Errorf(ir.ErrInternal, "authentication failed")
		}
		if c != nil {
			return c, nil
		}
	}
	return nil, ir.Errorf(ir.ErrAuthentication, "missing or invalid API key")
}

// writeCodecError renders gw in the client's protocol; status, when
// non-zero, overrides the codec's choice.
func writeCodecError(w http.ResponseWriter, c protocol.Codec, gw *ir.GwError, status int) {
	st, h, body := c.EncodeError(gw)
	copyHeader(w.Header(), h)
	if status != 0 {
		st = status
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(st)
	_, _ = w.Write(body)
}

func copyHeader(dst, src http.Header) {
	for k, v := range src {
		dst[k] = append([]string(nil), v...)
	}
}

// limitUpload bounds an upload route's body to the request body limit
// without reading it; compressed uploads are refused.
func (s *Server) limitUpload(w http.ResponseWriter, r *http.Request) (*ir.GwError, int) {
	for _, v := range r.Header.Values("Content-Encoding") {
		if c := strings.ToLower(strings.TrimSpace(v)); c != "" && c != "identity" {
			return ir.Errorf(ir.ErrInvalidRequest, "uploads must not be compressed (content-encoding %q)", v), http.StatusUnsupportedMediaType
		}
	}
	limit := s.o.Limits.MaxCompressed
	if limit <= 0 {
		return nil, 0
	}
	if r.ContentLength > limit {
		return ir.Errorf(ir.ErrInvalidRequest, "%s", (&httpx.TooLargeError{Limit: limit}).Error()), http.StatusRequestEntityTooLarge
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}
	return nil, 0
}
