package app

import (
	"context"
	"crypto/sha256"
	"net/http"

	"github.com/initxy/plugrelay/internal/mcp"
	"github.com/initxy/plugrelay/internal/version"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/middleware/access"
	"github.com/initxy/plugrelay/plugins/middleware/ratelimit"
)

// newMCP builds the MCP gateway over the store's plugin KV.
func (a *App) newMCP(master []byte) *mcp.Gateway {
	sum := sha256.Sum256(append(append([]byte{}, master...), "plugrelay mcp session"...))
	return mcp.New(mcp.Deps{
		Registry: mcp.NewRegistry(a.store.KV(mcp.KVNamespace), a.box),
		Egress:   a.egress,
		Secret:   sum[:],
		Access:   a.mcpAccess,
		Limit:    a.mcpLimit,
		Records:  a.queue,
		ClientIP: a.clientIP,
		Version:  version.Version,
		Logger:   a.log.With("component", "mcp"),
	})
}

// mcpAccess reads the caller's MCP grants from middleware.access settings,
// whether or not that plugin runs: the grant is the only way in.
func (a *App) mcpAccess(c *plugin.Caller) mcp.Access {
	get := func(key string) []string { return a.settingFor(c, access.ID, key).List() }
	return mcp.Access{Servers: get(access.SettingMCPServers),
		ToolsAllow: a.settingFor(c, access.ID, access.SettingMCPToolsAllow).ListLayers(),
		ToolsDeny:  get(access.SettingMCPToolsDeny)}
}

// mcpLimit counts a tool call against middleware.ratelimit when it runs:
// the same requests-per-minute and concurrency counters as model calls.
func (a *App) mcpLimit(r *http.Request, c *plugin.Caller, tool string) (func(int), *ir.GwError) {
	var rl plugin.Plugin
	for _, p := range a.runtime().chain.Plugins {
		if p.Info().ID == ratelimit.ID {
			rl = p
		}
	}
	hook, ok := rl.(plugin.RequestHook)
	if !ok {
		return func(int) {}, nil
	}
	call := plugin.NewCall(context.WithoutCancel(r.Context()), "mcp", func(id, key string) plugin.Value {
		return a.settingFor(c, id, key)
	}, nil)
	call.Caller, call.Protocol, call.Model, call.ClientIP, call.Header = c, mcp.Protocol, tool, a.clientIP(r), r.Header
	reply, err := hook.OnRequest(call.WithPlugin(ratelimit.ID))
	if err != nil {
		a.log.Warn("mcp: rate limiter failed", "err", err)
		return nil, ir.Errorf(ir.ErrInternal, "rate limiter unavailable")
	}
	if reply != nil && reply.Error != nil {
		return nil, reply.Error
	}
	return func(status int) {
		if fh, ok := rl.(plugin.FinishHook); ok {
			fh.OnFinish(call.WithPlugin(ratelimit.ID), &plugin.Result{Status: status, Attempts: 1})
		}
	}, nil
}
