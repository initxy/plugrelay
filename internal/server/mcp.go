package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/initxy/plugrelay/internal/mcp"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// MCP serves the MCP gateway for an authenticated caller; *mcp.Gateway
// implements it. server is "" for the aggregated endpoint /mcp.
type MCP interface {
	ServeMCP(w http.ResponseWriter, r *http.Request, caller *plugin.Caller, server string)
}

// serveMCP authenticates an MCP request and hands it on. It runs without a
// write deadline: a tool call may stream progress for a long time.
func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	if s.o.MCP == nil || (s.o.MCPOn != nil && !s.o.MCPOn()) {
		setWriteDeadline(w, WriteTimeout)
		notFound(w, r)
		return
	}
	rest, _ := strings.CutPrefix(r.URL.EscapedPath(), "/mcp")
	rest = strings.Trim(rest, "/")
	id, err := url.PathUnescape(rest)
	if err != nil || strings.Contains(id, "/") {
		notFound(w, r)
		return
	}
	caller, gw := s.authenticate(r)
	if gw != nil {
		mcp.WriteAuthError(w, gw)
		return
	}
	if s.o.MCPAuthorize != nil {
		if gw := s.o.MCPAuthorize(caller, r); gw != nil {
			mcp.WriteAuthError(w, gw)
			return
		}
	}
	s.o.MCP.ServeMCP(w, r, caller, id)
}
