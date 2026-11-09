package app

import (
	"net/http"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/middleware/access"
)

// authorizeNative applies the middleware.access grant and allowlists to a
// native passthrough request. The settings resolve whether or not the
// plugin is enabled: the grant is the only way in.
func (a *App) authorizeNative(c *plugin.Caller, r *http.Request, upstreamID string) *ir.GwError {
	setting := func(key string) plugin.Value { return a.settingFor(c, access.ID, key) }
	return access.CheckNative(setting, a.clientIP(r), r.Header, upstreamID)
}

// authorizeWebSocket applies the middleware.access (a core module)
// source-address and client allowlists to a WebSocket handshake or an MCP
// request; neither runs request hooks, which apply them to model calls.
func (a *App) authorizeWebSocket(c *plugin.Caller, r *http.Request) *ir.GwError {
	setting := func(key string) plugin.Value { return a.settingFor(c, access.ID, key) }
	return access.CheckClient(setting, a.clientIP(r), r.Header)
}
