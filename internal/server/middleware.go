package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// RequestIDHeader carries the request id in both directions.
const RequestIDHeader = "X-Request-Id"

// middleware adds request ids, panic recovery and access logging.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		rec := &recorder{ResponseWriter: w}
		start := time.Now()
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.o.Logger.Error("handler panic", "request_id", id, "panic", v, "stack", string(debug.Stack()))
				if !rec.wrote {
					writeJSONError(rec, http.StatusInternalServerError, "internal", "internal error")
				}
			}
			s.o.Logger.LogAttrs(r.Context(), accessLevel(rec.status), "http",
				slog.String("request_id", id),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.code()),
				slog.Int64("bytes", rec.bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
				slog.String("ua", r.UserAgent()))
		}()
		next.ServeHTTP(rec, r)
	})
}

func accessLevel(status int) slog.Level {
	if status >= 500 {
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c == '-' || c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// recorder captures status and size while keeping Flush and the
// ResponseController escape hatch working for streams.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if !r.wrote {
		r.status, r.wrote = http.StatusOK, true
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if !r.wrote {
		r.status, r.wrote = http.StatusOK, true
	}
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) code() int {
	if !r.wrote {
		return http.StatusOK
	}
	return r.status
}

// writeJSONError writes the admin-style error body used by non-protocol
// routes.
func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}
