// Package extplugin runs external process plugins and adapts them to the
// pkg/plugin interfaces, so to the core an external plugin is just another
// plugin.Authenticator, plugin.RequestHook, plugin.Selector, plugin.Meter,
// plugin.Sink, plugin.Job, plugin.Admin or plugin.Provider.
//
// A plugin is a directory holding plugin.yaml (see Manifest) and an
// executable. The host starts the process, speaks JSON-RPC 2.0 to it over
// Content-Length framed stdio (or a unix socket), performs the initialize
// handshake and supervises it: every call has a timeout and a concurrency
// limit; a crashed process is restarted with exponential backoff; while it
// is down calls fail fast with a *CallError wrapping ErrUnavailable. Every
// CallError matches plugin.ErrPluginFailure, so the core applies the
// plugin's FailPolicy (plugin.IsFailure); deliberate refusals come back as
// *ir.GwError instead. Adapters implement plugin.Supporter, and the provider
// adapter plugin.Sender.
//
// Per-event stream hooks (middleware.onEvent) are not offered: a process
// round trip per event is too slow for the hot path.
//
// The wire protocol is specified in sdk/PROTOCOL.md.
package extplugin
