// Package server is plugrelay's HTTP front: it routes client protocol
// requests, the batch APIs and native passthrough (/native/, when wired)
// to the pipeline, mounts the admin API, plugins' HTTP routes and the embedded
// console, and serves health and metrics endpoints.
//
// Protocol routes accept every registered codec's canonical path, the same
// path without its "/v1" or "/v1beta" prefix, with a trailing slash, and
// with Azure-style query parameters. A model listing on the shared
// "/v1/models" path is answered in the format the request's features ask
// for. Protocol routes have no write timeout so streams can run for as long
// as the upstream does, nor do native passthrough and WebSocket sessions;
// every other route gets one.
package server

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/httpx"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// Server timeouts.
const (
	ReadHeaderTimeout = 10 * time.Second
	IdleTimeout       = 2 * time.Minute
	// BodyReadTimeout bounds reading a protocol request body.
	BodyReadTimeout = 5 * time.Minute
	// WriteTimeout applies to every non-protocol route.
	WriteTimeout = 3 * time.Minute
	// ShutdownGrace is how long Serve waits for in-flight requests by
	// default (Options.ShutdownGrace).
	ShutdownGrace = 30 * time.Second
)

// Protocol serves one authenticated client request; *pipeline.Pipeline
// implements it.
type Protocol interface {
	ServeProtocol(w http.ResponseWriter, r *http.Request, codec protocol.Codec, route protocol.Route, caller *plugin.Caller, body []byte)
}

// Options wires a Server. Pipeline and Auth are required.
type Options struct {
	Pipeline Protocol
	// Auth returns the enabled authenticators in order.
	Auth func() []plugin.Authenticator
	// Limits bound protocol request bodies.
	Limits httpx.Limits
	// Codecs defaults to every registered codec.
	Codecs []protocol.Codec

	// API serves /api/ (admin API and plugin APIs); nil answers 404.
	API http.Handler
	// Native serves /native/<upstream>/<path> after authentication; nil
	// answers 404 (native passthrough is off).
	Native Native
	// MCP serves the MCP gateway (/mcp and /mcp/<server>) after
	// authentication; nil answers 404.
	MCP MCP
	// MCPAuthorize applies the caller's source-address and client
	// allowlists to an MCP request, which runs no request hooks; nil allows.
	MCPAuthorize func(*plugin.Caller, *http.Request) *ir.GwError
	// MCPOn reports whether the MCP gateway feature is on; off, /mcp
	// answers 404. nil means on.
	MCPOn func() bool
	// Batch serves the batch APIs (/v1/files, /v1/batches,
	// /v1/messages/batches) after authentication; nil leaves those paths
	// to the other handlers (404).
	Batch Batch
	// WebSocket serves WebSocket upgrades on /v1/realtime and
	// /v1/responses after authentication; nil answers /v1/realtime with
	// 404 and a /v1/responses upgrade with 426.
	WebSocket WebSocket
	// Console is the console build; nil disables the console.
	Console fs.FS
	// CaptchaProviders lists the captcha providers in use, whose widget
	// origins the console's CSP then allows; nil allows none.
	CaptchaProviders func(ctx context.Context) []string

	// Metrics serves /metrics; nil answers 404. MetricsAuth, when set,
	// must approve each scrape.
	Metrics     http.Handler
	MetricsAuth func(r *http.Request) bool
	// Ready reports readiness for /readyz; nil means always ready.
	Ready func(ctx context.Context) error

	// ShutdownGrace overrides the package default when > 0.
	ShutdownGrace time.Duration

	Logger *slog.Logger
}

// Server is the HTTP front. Build it with New.
type Server struct {
	o      Options
	routes *routeTable
	mux    http.Handler
	// stopping is cancelled when shutdown starts; hijacked WebSocket
	// connections are not closed by the HTTP server, so they watch it.
	stopping context.Context
	stop     context.CancelFunc
}

// New returns a Server.
func New(o Options) *Server {
	if o.Pipeline == nil || o.Auth == nil {
		panic("server: Pipeline and Auth are required")
	}
	if o.Codecs == nil {
		o.Codecs = protocol.All()
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.ShutdownGrace <= 0 {
		o.ShutdownGrace = ShutdownGrace
	}
	s := &Server{o: o, routes: newRouteTable(o.Codecs)}
	s.stopping, s.stop = context.WithCancel(context.Background())
	s.mux = s.middleware(http.HandlerFunc(s.dispatch))
	return s
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.mux }

// Serve serves on ln until ctx is cancelled, then shuts down gracefully:
// new connections are refused and in-flight requests get the shutdown grace
// to finish before their connections are closed. Closing a connection
// cancels its request but does not wait for the handler, so handlers may
// still be running when Serve returns; see pipeline.Pipeline.Wait.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	hs := &http.Server{
		Handler:           s.mux,
		ReadHeaderTimeout: ReadHeaderTimeout,
		IdleTimeout:       IdleTimeout,
		ErrorLog:          slog.NewLogLogger(s.o.Logger.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	hs.RegisterOnShutdown(s.stop)
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), s.o.ShutdownGrace)
	defer cancel()
	err := hs.Shutdown(sctx)
	if errors.Is(err, context.DeadlineExceeded) {
		s.o.Logger.Warn("shutdown grace expired; closing remaining connections")
		err = hs.Close()
	}
	if serr := <-errc; serr != nil && !errors.Is(serr, http.ErrServerClosed) {
		return serr
	}
	return err
}

// dispatch picks the handler family for a request.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/healthz":
		s.healthz(w, r)
		return
	case p == "/readyz":
		s.readyz(w, r)
		return
	case p == "/metrics":
		s.metrics(w, r)
		return
	case hasPrefix(p, "/native/"):
		s.serveNative(w, r)
		return
	case hasPrefix(p, "/mcp/") && !(s.o.Console != nil && isBrowserNavigation(r)):
		s.serveMCP(w, r)
		return
	}
	if isWebSocket(r) {
		if s.serveWebSocket(w, r) {
			return
		}
		if s.refuseUpgrade(w, r) {
			return
		}
	}
	if s.o.Batch != nil && !(s.o.Console != nil && isBrowserNavigation(r)) {
		if codec, ok := s.o.Batch.BatchFamily(p); ok {
			s.serveBatch(w, r, codec)
			return
		}
	}
	if m, ok := s.routes.match(r); ok && !(s.o.Console != nil && isBrowserNavigation(r) && m.route.Op != protocol.OpHello) {
		s.serveProtocol(w, r, m)
		return
	}
	setWriteDeadline(w, WriteTimeout)
	switch {
	case hasPrefix(p, "/api/"):
		serveOr404(w, r, s.o.API)
	case s.o.Console != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		serveConsole(w, r, s.o.Console, s.o.CaptchaProviders)
	default:
		notFound(w, r)
	}
}

// hasPrefix matches a "/dir/" prefix or the bare "/dir".
func hasPrefix(p, prefix string) bool {
	return strings.HasPrefix(p, prefix) || p == strings.TrimSuffix(prefix, "/")
}

func serveOr404(w http.ResponseWriter, r *http.Request, h http.Handler) {
	if h == nil {
		notFound(w, r)
		return
	}
	h.ServeHTTP(w, r)
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeJSONError(w, http.StatusNotFound, "not_found", "no such endpoint")
}

// setWriteDeadline bounds writing the response of a non-streaming route.
func setWriteDeadline(w http.ResponseWriter, d time.Duration) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d))
}
