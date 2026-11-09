package server

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	var err error
	if s.o.Ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		err = s.o.Ready(ctx)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready: " + err.Error() + "\n"))
		return
	}
	_, _ = w.Write([]byte("ready\n"))
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if s.o.Metrics == nil {
		notFound(w, r)
		return
	}
	if s.o.MetricsAuth != nil && !s.o.MetricsAuth(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", "metrics need an admin token")
		return
	}
	setWriteDeadline(w, WriteTimeout)
	s.o.Metrics.ServeHTTP(w, r)
}
