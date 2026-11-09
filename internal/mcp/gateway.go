// Package mcp is the relay's MCP gateway: it exposes configured upstream
// MCP servers to clients at /mcp/<server-id>, and every tool a key may use
// at /mcp with "<server>__<tool>" names, authenticated with the platform
// key.
//
// Clients speak Streamable HTTP: the legacy revisions 2025-03-26,
// 2025-06-18 and 2025-11-25 (initialize, Mcp-Session-Id, JSON-RPC over
// POST; GET answers 405, so no server-initiated stream) and the stateless
// 2026-07-28 revision (server/discover, MCP-Protocol-Version on every
// request). Session ids are signed tokens bound to the key, so any relay
// node accepts them.
//
// Upstream servers are reached through the core egress (fixed proxy, SSRF
// guard) over Streamable HTTP — legacy or modern, detected at connect — or
// the HTTP+SSE transport of 2024-11-05. One connection per server and node
// is shared by all clients; the client's headers never reach the server,
// only the server's configured (and sealed secret) headers. stdio servers
// are not supported.
//
// The gateway offers tools (and, on a single server's endpoint, resources,
// prompts and completions passed through). It declares no listChanged or
// subscriptions and does not forward the upstream's own requests
// (sampling, elicitation, roots), which it refuses. Progress notifications
// for a tools/call are forwarded on an SSE response when the client
// accepts one; their tokens are rewritten so clients sharing a connection
// never see each other's.
//
// Every tools/call is checked against the caller's grants (Access),
// passed through the rate limiter and recorded in the request log as
// protocol "mcp" (arguments and result only when body logging is on).
package mcp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Protocol is the protocol name of MCP records in the request log.
const Protocol = "mcp"

// maxBody bounds a client message.
const maxBody = 4 << 20

// listTimeout bounds collecting one server's tools for a listing.
const listTimeout = 15 * time.Second

// Deps wires a Gateway. Registry and Egress are required.
type Deps struct {
	Registry *Registry
	Egress   *egress.Client
	// Secret signs session ids.
	Secret []byte
	// Access resolves what a caller may reach; nil grants nothing.
	Access func(c *plugin.Caller) Access
	// Limit runs the rate limiter for one tool call; done reports the
	// outcome. nil means no limit.
	Limit func(r *http.Request, c *plugin.Caller, tool string) (done func(status int), gw *ir.GwError)
	// Records receives one record per tool call; nil records nothing.
	Records  interface{ Enqueue(*plugin.Record) bool }
	NewID    func() string
	ClientIP func(*http.Request) string
	Version  string
	Logger   *slog.Logger
	Now      func() time.Time
}

// Gateway serves the MCP endpoints.
type Gateway struct {
	d     Deps
	mu    sync.Mutex
	conns map[string]*connEntry
	ids   atomic.Int64
}

type connEntry struct {
	c  *conn
	fp string
}

// New returns a Gateway.
func New(d Deps) *Gateway {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.ClientIP == nil {
		d.ClientIP = func(r *http.Request) string { return r.RemoteAddr }
	}
	if d.Access == nil {
		d.Access = func(*plugin.Caller) Access { return Access{} }
	}
	return &Gateway{d: d, conns: map[string]*connEntry{}}
}

// Close ends every upstream session.
func (g *Gateway) Close() {
	g.mu.Lock()
	conns := g.conns
	g.conns = map[string]*connEntry{}
	g.mu.Unlock()
	for _, e := range conns {
		e.c.close()
	}
}

// connFor returns the shared connection to s, replacing it when the
// server's configuration changed.
func (g *Gateway) connFor(s *Server) (*conn, error) {
	fp, _ := json.Marshal(s)
	g.mu.Lock()
	e := g.conns[s.ID]
	if e != nil && e.fp == string(fp) {
		g.mu.Unlock()
		return e.c, nil
	}
	g.mu.Unlock()
	c, err := g.newConn(s)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if cur := g.conns[s.ID]; cur != nil && cur.fp == string(fp) {
		return cur.c, nil
	} else if cur != nil {
		go cur.c.close()
	}
	g.conns[s.ID] = &connEntry{c: c, fp: string(fp)}
	return c, nil
}

func (g *Gateway) newConn(s *Server) (*conn, error) {
	h, err := g.d.Registry.open(s)
	if err != nil {
		return nil, err
	}
	client, err := g.d.Egress.HTTPClient(s.Proxy, s.AllowPrivate)
	if err != nil {
		return nil, err
	}
	return &conn{srv: *s, header: h, client: client, agent: g.d.Version}, nil
}

// ---------------------------------------------------------------- HTTP

// session is a client's view of the protocol for one request.
type session struct {
	caller  *plugin.Caller
	server  *Server // nil on the aggregated endpoint
	modern  bool
	version string
	access  Access
}

// ServeMCP serves one authenticated request to /mcp (serverID "") or
// /mcp/<serverID>.
func (g *Gateway) ServeMCP(w http.ResponseWriter, r *http.Request, caller *plugin.Caller, serverID string) {
	if caller == nil {
		caller = &plugin.Caller{}
	}
	if !originAllowed(r) {
		writeRPCError(w, http.StatusForbidden, nil, codeInvalidRequest, "origin not allowed")
		return
	}
	s := &session{caller: caller, access: g.d.Access(caller)}
	if serverID != "" {
		srv, err := g.d.Registry.Get(r.Context(), serverID)
		if err != nil || !srv.Enabled || !s.access.Server(serverID) {
			writeRPCError(w, http.StatusNotFound, nil, codeInvalidRequest, "unknown MCP server "+strconv.Quote(serverID))
			return
		}
		s.server = srv
	}
	switch r.Method {
	case http.MethodPost:
		g.post(w, r, s)
	case http.MethodDelete:
		if id := r.Header.Get("Mcp-Session-Id"); id != "" {
			if _, ok := g.verifySession(id, caller); !ok {
				writeRPCError(w, http.StatusNotFound, nil, codeInvalidRequest, "unknown session")
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		writeRPCError(w, http.StatusMethodNotAllowed, nil, codeInvalidRequest, "use POST; this server opens no event stream")
	}
}

// originAllowed rejects browsers on another origin (DNS rebinding): an
// Origin, when sent, must name the host the request was sent to.
func originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (g *Gateway) post(w http.ResponseWriter, r *http.Request, s *session) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeRPCError(w, http.StatusRequestEntityTooLarge, nil, codeInvalidRequest, "message too large")
		return
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		writeRPCError(w, http.StatusBadRequest, nil, codeInvalidRequest, "JSON-RPC batches are not supported")
		return
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil || m.JSONRPC != "2.0" {
		writeRPCError(w, http.StatusBadRequest, nil, codeParse, "invalid JSON-RPC message")
		return
	}
	if m.Method == "" || !m.hasID() {
		// A notification or a response to a request we never sent.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	header := r.Header.Get("MCP-Protocol-Version")
	metaVersion := gjson.GetBytes(m.Params, `_meta.io\.modelcontextprotocol/protocolVersion`).String()
	if m.Method == "initialize" {
		// A legacy handshake, whatever headers came with it.
	} else if header == VersionModern || metaVersion == VersionModern {
		if header != metaVersion {
			writeRPCError(w, http.StatusBadRequest, m.ID, codeHeaderMismatch, "MCP-Protocol-Version header and _meta protocolVersion differ")
			return
		}
		if msg := checkStandardHeaders(r.Header, &m); msg != "" {
			writeRPCError(w, http.StatusBadRequest, m.ID, codeHeaderMismatch, msg)
			return
		}
		s.modern, s.version = true, VersionModern
	} else {
		id := r.Header.Get("Mcp-Session-Id")
		if id == "" {
			writeRPCError(w, http.StatusBadRequest, m.ID, codeInvalidRequest, "missing Mcp-Session-Id; send initialize first")
			return
		}
		v, ok := g.verifySession(id, s.caller)
		if !ok {
			writeRPCError(w, http.StatusNotFound, m.ID, codeInvalidRequest, "unknown session; initialize again")
			return
		}
		if header != "" && header != v {
			writeRPCError(w, http.StatusBadRequest, m.ID, codeInvalidRequest, "unsupported MCP-Protocol-Version "+strconv.Quote(header))
			return
		}
		s.version = v
	}
	ctx := r.Context()
	switch m.Method {
	case "initialize":
		g.initialize(w, r, s, &m)
	case "ping":
		g.result(w, s, m.ID, json.RawMessage(`{}`))
	case "server/discover":
		g.result(w, s, m.ID, g.describe(ctx, s, true))
	case "tools/list":
		g.listTools(w, r, s, &m)
	case "tools/call":
		g.callTool(w, r, s, &m)
	case "resources/list", "resources/templates/list", "resources/read", "prompts/list", "prompts/get", "completion/complete":
		if s.server == nil {
			g.unknownMethod(w, s, &m)
			return
		}
		g.forward(w, r, s, &m)
	default:
		g.unknownMethod(w, s, &m)
	}
}

// checkStandardHeaders validates a modern request's Mcp-Method and
// Mcp-Name against its body, as servers must (Streamable HTTP 2026-07-28,
// "Standard Request Headers" and "Server Validation"): a missing,
// malformed or mismatched header is refused. It returns why, or "".
func checkStandardHeaders(h http.Header, m *message) string {
	method := h.Get("Mcp-Method")
	switch {
	case method == "":
		return "missing Mcp-Method header"
	case method != m.Method:
		return "Mcp-Method header " + strconv.Quote(method) + " does not match body method " + strconv.Quote(m.Method)
	}
	field := ""
	switch m.Method {
	case "tools/call", "prompts/get":
		field = "name"
	case "resources/read":
		field = "uri"
	default:
		return ""
	}
	raw := h.Get("Mcp-Name")
	if raw == "" {
		return "missing Mcp-Name header"
	}
	name, ok := decodeHeaderValue(raw)
	if !ok {
		return "malformed Mcp-Name header"
	}
	if body := gjson.GetBytes(m.Params, field); body.Type != gjson.String || body.Str != name {
		return "Mcp-Name header does not match params." + field
	}
	return ""
}

func (g *Gateway) unknownMethod(w http.ResponseWriter, s *session, m *message) {
	status := http.StatusOK
	if s.modern {
		status = http.StatusNotFound
	}
	writeRPCError(w, status, m.ID, codeMethodNotFound, "method not found: "+m.Method)
}

// initialize answers a legacy client's handshake and issues the session.
func (g *Gateway) initialize(w http.ResponseWriter, r *http.Request, s *session, m *message) {
	asked := gjson.GetBytes(m.Params, "protocolVersion").String()
	v := VersionLegacy
	if slices.Contains(legacyVersions, asked) {
		v = asked
	}
	s.version = v
	res := map[string]any{}
	_ = json.Unmarshal(g.describe(r.Context(), s, false), &res)
	res["protocolVersion"] = v
	b, _ := json.Marshal(res)
	w.Header().Set("Mcp-Session-Id", g.issueSession(s.caller, v))
	g.result(w, s, m.ID, b)
}

// describe is the relay's initialize or server/discover result.
func (g *Gateway) describe(ctx context.Context, s *session, discover bool) json.RawMessage {
	caps := map[string]any{"tools": map[string]any{}}
	info := map[string]any{"name": "plugrelay", "version": g.d.Version}
	instructions := ""
	if s.server != nil {
		info["name"] = "plugrelay/" + s.server.ID
		if s.server.Name != "" {
			info["title"] = s.server.Name
		}
		if c, err := g.connFor(s.server); err == nil {
			cctx, cancel := context.WithTimeout(ctx, listTimeout)
			if c.connect(cctx) == nil {
				ii, _, _ := c.info()
				for _, k := range []string{"resources", "prompts", "completions"} {
					if gjson.GetBytes(ii.Capabilities, k).Exists() {
						caps[k] = map[string]any{}
					}
				}
				instructions = ii.Instructions
			}
			cancel()
		}
	}
	res := map[string]any{"capabilities": caps}
	if instructions != "" {
		res["instructions"] = instructions
	}
	if discover {
		res["supportedVersions"] = append([]string{VersionModern}, legacyVersions...)
		res["_meta"] = map[string]any{"io.modelcontextprotocol/serverInfo": info}
		res["ttlMs"], res["cacheScope"] = 60000, "private"
	} else {
		res["serverInfo"] = info
	}
	b, _ := json.Marshal(res)
	return b
}

// result writes a JSON-RPC result; modern results carry resultType.
func (g *Gateway) result(w http.ResponseWriter, s *session, id, result json.RawMessage) {
	writeJSON(w, http.StatusOK, &message{JSONRPC: "2.0", ID: id, Result: g.shape(s, result)})
}

func (g *Gateway) shape(s *session, result json.RawMessage) json.RawMessage {
	if s.modern && gjson.ValidBytes(result) && !gjson.GetBytes(result, "resultType").Exists() {
		if out, err := sjson.SetBytes(result, "resultType", "complete"); err == nil {
			return out
		}
	}
	return result
}

// servers lists the enabled servers the caller may use, in id order.
func (g *Gateway) servers(ctx context.Context, s *session) ([]Server, error) {
	if s.server != nil {
		return []Server{*s.server}, nil
	}
	all, err := g.d.Registry.List(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, srv := range all {
		if srv.Enabled && s.access.Server(srv.ID) {
			out = append(out, srv)
		}
	}
	return out, nil
}

// exposed is one tool as a client sees it.
type exposed struct {
	name   string // as listed to the client
	server *Server
	tool   string // the upstream's name
	raw    json.RawMessage
}

// toolsOf lists one server's tools the caller may use, named for s.
func (g *Gateway) toolsOf(ctx context.Context, s *session, srv *Server) ([]exposed, error) {
	c, err := g.connFor(srv)
	if err != nil {
		return nil, err
	}
	raws, err := c.listTools(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(raws))
	byName := map[string]json.RawMessage{}
	for _, raw := range raws {
		n := gjson.GetBytes(raw, "name").String()
		if n == "" || !s.access.Tool(srv, n) {
			continue
		}
		if _, dup := byName[n]; dup {
			continue
		}
		names = append(names, n)
		byName[n] = raw
	}
	var out []exposed
	if s.server != nil {
		for _, n := range names {
			out = append(out, exposed{name: n, server: srv, tool: n, raw: byName[n]})
		}
		return out, nil
	}
	for alias, n := range aliases(srv.ID, names) {
		raw, _ := sjson.SetBytes(byName[n], "name", alias)
		out = append(out, exposed{name: alias, server: srv, tool: n, raw: raw})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func (g *Gateway) listTools(w http.ResponseWriter, r *http.Request, s *session, m *message) {
	srvs, err := g.servers(r.Context(), s)
	if err != nil {
		writeRPCError(w, http.StatusOK, m.ID, codeInternal, "could not read the server list")
		return
	}
	lists := make([][]exposed, len(srvs))
	errs := make([]error, len(srvs))
	var wg sync.WaitGroup
	for i := range srvs {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(r.Context(), listTimeout)
			defer cancel()
			lists[i], errs[i] = g.toolsOf(ctx, s, &srvs[i])
		})
	}
	wg.Wait()
	tools := []json.RawMessage{}
	for i, l := range lists {
		if errs[i] != nil {
			g.d.Logger.Warn("mcp: listing tools failed", "server", srvs[i].ID, "err", errs[i])
			if s.server != nil {
				writeRPCError(w, http.StatusOK, m.ID, codeInternal, "upstream MCP server unavailable: "+errs[i].Error())
				return
			}
			continue
		}
		for _, e := range l {
			tools = append(tools, e.raw)
		}
	}
	res := map[string]any{"tools": tools}
	if s.modern {
		res["ttlMs"], res["cacheScope"] = 60000, "private"
	}
	b, _ := json.Marshal(res)
	g.result(w, s, m.ID, b)
}

// resolve finds the tool a client named.
func (g *Gateway) resolve(ctx context.Context, s *session, name string) (*exposed, error) {
	var srv *Server
	if s.server != nil {
		srv = s.server
	} else {
		id, _, ok := strings.Cut(name, Sep)
		if !ok || !s.access.Server(id) {
			return nil, nil
		}
		got, err := g.d.Registry.Get(ctx, id)
		if errors.Is(err, ErrNotFound) || (got != nil && !got.Enabled) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		srv = got
	}
	tools, err := g.toolsOf(ctx, s, srv)
	if err != nil {
		return nil, err
	}
	for i := range tools {
		if tools[i].name == name {
			return &tools[i], nil
		}
	}
	return nil, nil
}

// callTool runs one tools/call.
func (g *Gateway) callTool(w http.ResponseWriter, r *http.Request, s *session, m *message) {
	start := g.d.Now()
	name := gjson.GetBytes(m.Params, "name").String()
	rec := g.newRecord(r, s, "tools/call", name, start)
	rec.Body = &plugin.RecordBody{Request: rawOr(gjson.GetBytes(m.Params, "arguments").Raw)}
	defer g.finishRecord(rec, start)

	t, err := g.resolve(r.Context(), s, name)
	if err != nil {
		g.fail(rec, http.StatusBadGateway, err.Error())
		writeRPCError(w, http.StatusOK, m.ID, codeInternal, "upstream MCP server unavailable: "+err.Error())
		return
	}
	if t == nil {
		g.fail(rec, http.StatusNotFound, "unknown or not allowed tool")
		writeRPCError(w, http.StatusOK, m.ID, codeInvalidParams, "unknown tool: "+name)
		return
	}
	rec.Model = t.server.ID + Sep + t.tool
	rec.Attempts[0].UpstreamID = "mcp:" + t.server.ID
	done, ok := g.limit(w, r, s, m, rec)
	if !ok {
		return
	}
	defer func() { done(rec.Status) }()
	c, err := g.connFor(t.server)
	if err != nil {
		g.fail(rec, http.StatusBadGateway, err.Error())
		writeRPCError(w, http.StatusOK, m.ID, codeInternal, "upstream MCP server unavailable")
		return
	}
	var params json.RawMessage
	params, _ = sjson.SetBytes(m.Params, "name", t.tool)
	params, _ = sjson.DeleteBytes(params, `_meta.io\.modelcontextprotocol/protocolVersion`)
	params, _ = sjson.DeleteBytes(params, `_meta.io\.modelcontextprotocol/clientCapabilities`)
	params, _ = sjson.DeleteBytes(params, `_meta.io\.modelcontextprotocol/clientInfo`)
	stream := g.progress(w, r, s, m, &params)
	ctx, cancel := context.WithTimeout(r.Context(), t.server.timeout())
	defer cancel()
	resp, err := c.call(ctx, "tools/call", params, stream.notify)
	switch {
	case err != nil:
		g.fail(rec, http.StatusBadGateway, err.Error())
		stream.finish(&message{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{Code: codeInternal, Message: "upstream MCP server failed: " + err.Error()}})
	case resp.Error != nil:
		status := http.StatusBadGateway
		if resp.Error.Code == codeInvalidParams || resp.Error.Code == codeInvalidRequest {
			status = http.StatusBadRequest
		}
		g.fail(rec, status, resp.Error.Message)
		stream.finish(&message{JSONRPC: "2.0", ID: m.ID, Error: resp.Error})
	default:
		rec.Body.Response = rawOr(string(resp.Result))
		if gjson.GetBytes(resp.Result, "isError").Bool() {
			rec.Error = "tool reported an error"
		}
		stream.finish(&message{JSONRPC: "2.0", ID: m.ID, Result: g.shape(s, resp.Result)})
	}
}

// limit runs the rate limiter for one recorded request; ok is false when
// it refused, and the refusal is already written.
func (g *Gateway) limit(w http.ResponseWriter, r *http.Request, s *session, m *message, rec *plugin.Record) (done func(int), ok bool) {
	if g.d.Limit == nil {
		return func(int) {}, true
	}
	done, gw := g.d.Limit(r, s.caller, rec.Model)
	if gw != nil {
		g.fail(rec, http.StatusTooManyRequests, gw.Message)
		if gw.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(gw.RetryAfter.Round(time.Second)/time.Second)))
		}
		writeRPCError(w, http.StatusTooManyRequests, m.ID, codeInternal, gw.Message)
		return nil, false
	}
	return done, true
}

// forward passes a resources, prompts or completion request to the one
// server of a single-server endpoint. The server grant was checked when
// the endpoint was resolved; tool rules do not apply. A resources/read or
// prompts/get, which return content, is rate limited and recorded like a
// tool call, under "<server>/<method>"; listings are not, like tools/list.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, s *session, m *message) {
	start := g.d.Now()
	rec := &plugin.Record{Attempts: make([]plugin.AttemptRecord, 1)} // discarded unless recorded
	if target, ok := map[string]string{"resources/read": "uri", "prompts/get": "name"}[m.Method]; ok {
		rec = g.newRecord(r, s, m.Method, gjson.GetBytes(m.Params, target).String(), start)
		rec.Model = s.server.ID + "/" + m.Method
		rec.Body = &plugin.RecordBody{Request: rawOr(string(m.Params))}
		defer g.finishRecord(rec, start)
		done, ok := g.limit(w, r, s, m, rec)
		if !ok {
			return
		}
		defer func() { done(rec.Status) }()
	}
	c, err := g.connFor(s.server)
	if err != nil {
		g.fail(rec, http.StatusBadGateway, err.Error())
		writeRPCError(w, http.StatusOK, m.ID, codeInternal, "upstream MCP server unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.server.timeout())
	defer cancel()
	resp, err := c.call(ctx, m.Method, m.Params, nil)
	if err != nil {
		g.fail(rec, http.StatusBadGateway, err.Error())
		writeRPCError(w, http.StatusOK, m.ID, codeInternal, "upstream MCP server failed: "+err.Error())
		return
	}
	if resp.Error != nil {
		g.fail(rec, http.StatusBadGateway, resp.Error.Message)
		writeJSON(w, http.StatusOK, &message{JSONRPC: "2.0", ID: m.ID, Error: resp.Error})
		return
	}
	if rec.Body != nil {
		rec.Body.Response = rawOr(string(resp.Result))
	}
	g.result(w, s, m.ID, resp.Result)
}

// progressStream relays a tool call's progress notifications on an SSE
// response, opened on the first one; without any the answer is JSON.
type progressStream struct {
	w      http.ResponseWriter
	token  json.RawMessage // the client's token
	ours   string          // the token sent upstream
	events chan *message
	done   chan struct{}
	opened bool
}

func (g *Gateway) progress(w http.ResponseWriter, r *http.Request, s *session, m *message, params *json.RawMessage) *progressStream {
	ps := &progressStream{w: w}
	tok := gjson.GetBytes(*params, "_meta.progressToken")
	if !tok.Exists() || !acceptsSSE(r) {
		if tok.Exists() {
			*params, _ = sjson.DeleteBytes(*params, "_meta.progressToken")
		}
		return ps
	}
	ps.token = json.RawMessage(tok.Raw)
	ps.ours = "pr-" + strconv.FormatInt(g.ids.Add(1), 10) + "-" + hash8(s.caller.KeyID+tok.Raw)
	*params, _ = sjson.SetBytes(*params, "_meta.progressToken", ps.ours)
	ps.events = make(chan *message, 64)
	ps.done = make(chan struct{})
	go ps.run()
	return ps
}

// notify is called from the upstream reader; it never blocks.
func (ps *progressStream) notify(m *message) {
	if ps.events == nil || m.Method != "notifications/progress" || gjson.GetBytes(m.Params, "progressToken").String() != ps.ours {
		return
	}
	params, _ := sjson.SetRawBytes(m.Params, "progressToken", ps.token)
	select {
	case ps.events <- &message{JSONRPC: "2.0", Method: m.Method, Params: params}:
	default:
	}
}

func (ps *progressStream) run() {
	defer close(ps.done)
	for ev := range ps.events {
		ps.write(ev)
	}
}

func (ps *progressStream) write(m *message) {
	if !ps.opened {
		ps.opened = true
		h := ps.w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		ps.w.WriteHeader(http.StatusOK)
	}
	b, _ := json.Marshal(m)
	_, _ = fmt.Fprintf(ps.w, "event: message\ndata: %s\n\n", b)
	_ = http.NewResponseController(ps.w).Flush()
}

// finish writes the response: on the stream when one is open, else JSON.
func (ps *progressStream) finish(m *message) {
	if ps.events != nil {
		close(ps.events)
		<-ps.done
	}
	if ps.opened {
		ps.write(m)
		return
	}
	writeJSON(ps.w, http.StatusOK, m)
}

func acceptsSSE(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		if mt, _, err := mime.ParseMediaType(strings.TrimSpace(part)); err == nil && mt == "text/event-stream" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- records

func (g *Gateway) newRecord(r *http.Request, s *session, method, name string, start time.Time) *plugin.Record {
	id := ""
	if g.d.NewID != nil {
		id = g.d.NewID()
	}
	if id == "" {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		id = "mcp_" + hex.EncodeToString(b)
	}
	up := "mcp"
	if s.server != nil {
		up = "mcp:" + s.server.ID
	}
	return &plugin.Record{ID: id, Time: start, Caller: *s.caller, Protocol: Protocol, Model: name,
		ClientUA: r.UserAgent(), ClientIP: g.d.ClientIP(r), Status: http.StatusOK,
		Attempts: []plugin.AttemptRecord{{N: 1, UpstreamID: up, Protocol: Protocol, Tier: plugin.TierPassthrough,
			Diagnostic: &ir.Diagnostic{Fidelity: ir.Lossless, Notes: []string{strings.TrimSpace("mcp: " + method + " " + name)}}}}}
}

func (g *Gateway) fail(rec *plugin.Record, status int, msg string) {
	rec.Status, rec.Error = status, msg
	rec.Attempts[0].Error = msg
	switch status {
	case http.StatusNotFound:
		rec.ErrorKind = ir.ErrNotFound
	case http.StatusTooManyRequests:
		rec.ErrorKind = ir.ErrRateLimited
	case http.StatusBadRequest:
		rec.ErrorKind = ir.ErrInvalidRequest
	default:
		rec.ErrorKind = ir.ErrUpstream
	}
}

func (g *Gateway) finishRecord(rec *plugin.Record, start time.Time) {
	rec.Duration = g.d.Now().Sub(start)
	a := &rec.Attempts[0]
	a.Status, a.Duration, a.Sent = rec.Status, rec.Duration, rec.Status != http.StatusNotFound && rec.Status != http.StatusTooManyRequests
	if g.d.Records != nil && !g.d.Records.Enqueue(rec) {
		g.d.Logger.Warn("mcp: record dropped by best-effort sinks", "request", rec.ID)
	}
}

func rawOr(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

// ---------------------------------------------------------------- sessions

const sessionPrefix = "mcs1."

// sessionTTL is how long a session id is accepted; past it the client gets
// 404 and, as the transport asks, initializes again.
const sessionTTL = 24 * time.Hour

// issueSession signs (key id, protocol version, time) into a session id.
func (g *Gateway) issueSession(c *plugin.Caller, version string) string {
	payload := c.KeyID + "\n" + version + "\n" + strconv.FormatInt(g.d.Now().Unix(), 10)
	p := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return sessionPrefix + p + "." + g.mac(p)
}

func (g *Gateway) mac(p string) string {
	h := hmac.New(sha256.New, g.d.Secret)
	h.Write([]byte("plugrelay mcp session\n" + p))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:18])
}

// verifySession returns the version of a session issued to c's key.
func (g *Gateway) verifySession(id string, c *plugin.Caller) (string, bool) {
	rest, ok := strings.CutPrefix(id, sessionPrefix)
	p, sig, ok2 := strings.Cut(rest, ".")
	if !ok || !ok2 || !hmac.Equal([]byte(sig), []byte(g.mac(p))) {
		return "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return "", false
	}
	parts := strings.Split(string(b), "\n")
	if len(parts) != 3 || parts[0] != c.KeyID || !slices.Contains(legacyVersions, parts[1]) {
		return "", false
	}
	issued, err := strconv.ParseInt(parts[2], 10, 64)
	if age := g.d.Now().Sub(time.Unix(issued, 0)); err != nil || age > sessionTTL || age < -time.Minute {
		return "", false
	}
	return parts[1], true
}

// ---------------------------------------------------------------- output

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	writeJSON(w, status, &message{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// WriteAuthError answers an MCP request whose platform key was refused.
func WriteAuthError(w http.ResponseWriter, gw *ir.GwError) {
	status := http.StatusUnauthorized
	if gw.Kind == ir.ErrPermission {
		status = http.StatusForbidden
	} else if gw.Kind != ir.ErrAuthentication {
		status = http.StatusInternalServerError
	}
	writeRPCError(w, status, nil, codeInvalidRequest, gw.Message)
}

// ---------------------------------------------------------------- admin test

// TestResult is what a connectivity test found.
type TestResult struct {
	OK              bool            `json:"ok"`
	Transport       string          `json:"transport,omitempty"`
	ProtocolVersion string          `json:"protocol_version,omitempty"`
	ServerInfo      json.RawMessage `json:"server_info,omitempty"`
	Tools           []string        `json:"tools"`
	LatencyMs       int64           `json:"latency_ms"`
	Error           string          `json:"error,omitempty"`
}

// Passed reports whether the test succeeded.
func (r TestResult) Passed() bool { return r.OK }

// Test connects to a server afresh, lists its tools and disconnects.
func (g *Gateway) Test(ctx context.Context, s *Server) TestResult {
	start := g.d.Now()
	res := TestResult{Tools: []string{}}
	c, err := g.newConn(s)
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, listTimeout)
		defer cancel()
		var tools []json.RawMessage
		if tools, err = c.listTools(ctx); err == nil {
			for _, t := range tools {
				res.Tools = append(res.Tools, gjson.GetBytes(t, "name").String())
			}
			sort.Strings(res.Tools)
			ii, v, mode := c.info()
			res.OK, res.Transport, res.ProtocolVersion, res.ServerInfo = true, mode, v, ii.ServerInfo
		}
		go c.close()
	}
	if err != nil {
		res.Error = plugin.RedactURLError(err).Error()
	}
	res.LatencyMs = g.d.Now().Sub(start).Milliseconds()
	return res
}

// Registry is the gateway's server list.
func (g *Gateway) Registry() *Registry { return g.d.Registry }
