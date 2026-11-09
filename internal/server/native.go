package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Native serves /native/<upstream>/<path> for an authenticated caller;
// *pipeline.Pipeline implements it. upstream is the unescaped first
// segment, path the escaped rest ("/files?..." without the query).
type Native interface {
	ServeNative(w http.ResponseWriter, r *http.Request, caller *plugin.Caller, upstream, path string)
}

// serveNative authenticates a native passthrough request and hands it on.
// It runs without a write deadline: downloads and streams may be long.
func (s *Server) serveNative(w http.ResponseWriter, r *http.Request) {
	if s.o.Native == nil {
		setWriteDeadline(w, WriteTimeout)
		notFound(w, r)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/native/")
	ref, path, _ := strings.Cut(rest, "/")
	ref, err := url.PathUnescape(ref)
	if !ok || err != nil || ref == "" {
		notFound(w, r)
		return
	}
	caller, gw := s.authenticate(r)
	if gw != nil {
		status := gw.Status
		if status == 0 {
			status = http.StatusUnauthorized
			if gw.Kind != ir.ErrAuthentication {
				status = http.StatusInternalServerError
				if gw.Kind == ir.ErrPermission {
					status = http.StatusForbidden
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": string(gw.Kind), "message": gw.Message}})
		return
	}
	s.o.Native.ServeNative(w, r, caller, ref, "/"+path)
}
