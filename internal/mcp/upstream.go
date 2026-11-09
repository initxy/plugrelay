package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/sse"
)

// Protocol revisions. Legacy revisions open with initialize and may use
// sessions; the modern one (2026-07-28) is stateless and carries the
// version in every request.
const (
	VersionModern = "2026-07-28"
	VersionLegacy = "2025-11-25" // newest legacy revision
)

// legacyVersions are the legacy revisions spoken over Streamable HTTP,
// newest first.
var legacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// JSON-RPC and MCP error codes.
const (
	codeParse            = -32700
	codeInvalidRequest   = -32600
	codeMethodNotFound   = -32601
	codeInvalidParams    = -32602
	codeInternal         = -32603
	codeHeaderMismatch   = -32020
	codeUnsupportedProto = -32022
)

// message is one JSON-RPC 2.0 message.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (m *message) hasID() bool   { return len(m.ID) > 0 && string(m.ID) != "null" }
func (m *message) request() bool { return m.Method != "" && m.hasID() }

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return "mcp error " + strconv.Itoa(e.Code) + ": " + e.Message }

// errSessionGone means the upstream forgot our session (HTTP 404).
var errSessionGone = errors.New("upstream session expired")

// maxMessage bounds one message from an upstream server.
const maxMessage = 16 << 20

// toolsFor is how long a server's tool list is reused.
const toolsFor = time.Minute

// conn is the relay's connection to one upstream server, shared by every
// client on this node.
type conn struct {
	srv    Server
	header http.Header
	client *http.Client
	agent  string

	// hs serializes handshakes; mu guards the fields below.
	hs      sync.Mutex
	mu      sync.Mutex
	ready   bool
	mode    string // TransportStreamable, VersionModern or TransportSSE
	session string
	version string
	init    initInfo
	link    *sseLink
	tools   []json.RawMessage
	toolsAt time.Time

	nextID atomic.Int64
}

// initInfo is what the upstream said about itself.
type initInfo struct {
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	ServerInfo   json.RawMessage `json:"serverInfo,omitempty"`
	Instructions string          `json:"instructions,omitempty"`
}

func (c *conn) id() json.RawMessage {
	return json.RawMessage(strconv.FormatInt(c.nextID.Add(1), 10))
}

// connect runs the handshake unless a session is open.
func (c *conn) connect(ctx context.Context) error {
	c.hs.Lock()
	defer c.hs.Unlock()
	c.mu.Lock()
	ready := c.ready
	if !ready {
		c.session, c.version, c.mode = "", "", ""
	}
	c.mu.Unlock()
	if ready {
		return nil
	}
	if c.srv.Transport == TransportSSE {
		return c.connectSSE(ctx)
	}
	err := c.initialize(ctx)
	var he *httpError
	if err != nil && c.srv.Transport == TransportAuto && errors.As(err, &he) &&
		(he.status == 400 || he.status == 404 || he.status == 405) && !he.rpc {
		return c.connectSSE(ctx)
	}
	return err
}

func initParams(agent string, version string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"protocolVersion": version, "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "plugrelay", "version": agent}})
	return b
}

// initialize opens a Streamable HTTP session, or detects a modern server.
func (c *conn) initialize(ctx context.Context) error {
	c.set(func() { c.mode = TransportStreamable })
	msg := &message{JSONRPC: "2.0", ID: c.id(), Method: "initialize", Params: initParams(c.agent, VersionLegacy)}
	resp, hdr, err := c.post(ctx, msg, nil)
	if err != nil && (resp == nil || resp.Error == nil) {
		return err
	}
	if resp != nil && resp.Error != nil {
		if supported := modernSupported(resp.Error); slices.Contains(supported, VersionModern) {
			c.set(func() { c.mode, c.version = VersionModern, VersionModern })
			if err := c.discover(ctx); err != nil {
				return err
			}
			c.set(func() { c.ready = true })
			return nil
		}
		return resp.Error
	}
	if resp == nil || resp.Result == nil {
		return errors.New("upstream answered initialize without a result")
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		initInfo
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return fmt.Errorf("initialize result: %w", err)
	}
	if !slices.Contains(legacyVersions, res.ProtocolVersion) && res.ProtocolVersion != "2024-11-05" {
		return fmt.Errorf("upstream speaks unsupported protocol version %q", res.ProtocolVersion)
	}
	c.set(func() { c.version, c.init, c.session = res.ProtocolVersion, res.initInfo, hdr.Get("Mcp-Session-Id") })
	if _, _, err = c.post(ctx, &message{JSONRPC: "2.0", Method: "notifications/initialized"}, nil); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	c.set(func() { c.ready = true })
	return nil
}

func (c *conn) set(f func()) { c.mu.Lock(); f(); c.mu.Unlock() }

// discover reads a modern server's description.
func (c *conn) discover(ctx context.Context) error {
	resp, _, err := c.post(ctx, &message{JSONRPC: "2.0", ID: c.id(), Method: "server/discover", Params: json.RawMessage(`{}`)}, nil)
	if err != nil {
		return err
	}
	var res struct {
		Capabilities json.RawMessage `json:"capabilities"`
		Instructions string          `json:"instructions"`
		Meta         map[string]json.RawMessage
	}
	if resp.Result != nil {
		_ = json.Unmarshal(resp.Result, &res)
		var meta struct {
			M map[string]json.RawMessage `json:"_meta"`
		}
		_ = json.Unmarshal(resp.Result, &meta)
		c.set(func() {
			c.init = initInfo{Capabilities: res.Capabilities, Instructions: res.Instructions, ServerInfo: meta.M["io.modelcontextprotocol/serverInfo"]}
		})
	}
	return nil
}

// modernSupported returns data.supported of a modern version error.
func modernSupported(e *rpcError) []string {
	if e == nil || len(e.Data) == 0 {
		return nil
	}
	var d struct {
		Supported []string `json:"supported"`
	}
	_ = json.Unmarshal(e.Data, &d)
	return d.Supported
}

// modernParams adds the per-request _meta of the modern revision.
func modernParams(params json.RawMessage, agent string) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &m)
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := m["_meta"]; ok {
		_ = json.Unmarshal(raw, &meta)
	}
	meta["io.modelcontextprotocol/protocolVersion"], _ = json.Marshal(VersionModern)
	meta["io.modelcontextprotocol/clientCapabilities"] = json.RawMessage(`{}`)
	meta["io.modelcontextprotocol/clientInfo"], _ = json.Marshal(map[string]string{"name": "plugrelay", "version": agent})
	m["_meta"], _ = json.Marshal(meta)
	b, _ := json.Marshal(m)
	return b
}

// httpError is a non-2xx answer; rpc reports a JSON-RPC body came with it.
type httpError struct {
	status int
	rpc    bool
	body   string
}

func (e *httpError) Error() string {
	if e.body != "" {
		return fmt.Sprintf("upstream answered HTTP %d: %s", e.status, e.body)
	}
	return fmt.Sprintf("upstream answered HTTP %d", e.status)
}

// call sends one request and waits for its response, re-opening the
// session once if the upstream forgot it. notify receives notifications
// the upstream sends about this request.
func (c *conn) call(ctx context.Context, method string, params json.RawMessage, notify func(*message)) (*message, error) {
	for try := 0; ; try++ {
		if err := c.connect(ctx); err != nil {
			return nil, err
		}
		c.mu.Lock()
		mode, link := c.mode, c.link
		c.mu.Unlock()
		var err error
		msg := &message{JSONRPC: "2.0", ID: c.id(), Method: method, Params: params}
		var resp *message
		if mode == TransportSSE {
			resp, err = link.call(ctx, c, msg, notify)
		} else {
			resp, _, err = c.post(ctx, msg, notify)
		}
		if (errors.Is(err, errSessionGone) || errors.Is(err, errLinkClosed)) && try == 0 {
			c.reset()
			continue
		}
		if err != nil && resp == nil {
			return nil, err
		}
		return resp, nil
	}
}

// reset forgets the session so the next call opens a new one.
func (c *conn) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready, c.session = false, ""
	c.tools = nil
	if c.link != nil {
		c.link.close()
		c.link = nil
	}
}

// close ends the session (best effort) and stops the SSE link.
func (c *conn) close() {
	c.mu.Lock()
	session, mode := c.session, c.mode
	c.mu.Unlock()
	if session != "" && mode == TransportStreamable {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.srv.URL, nil); err == nil {
			c.headers(req, "", "")
			if resp, err := c.client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
		cancel()
	}
	c.reset()
}

func (c *conn) headers(req *http.Request, method, name string) {
	for k, vs := range c.header {
		req.Header[k] = vs
	}
	req.Header.Set("User-Agent", "plugrelay/"+c.agent)
	c.mu.Lock()
	session, version, mode := c.session, c.version, c.mode
	c.mu.Unlock()
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if mode == VersionModern && method != "" {
		req.Header.Set("Mcp-Method", method)
		if name != "" {
			req.Header.Set("Mcp-Name", headerValue(name))
		}
	}
}

// headerValue keeps plain ASCII and base64-encodes anything else, as the
// modern revision asks for Mcp-Name.
func headerValue(s string) string {
	if s != strings.TrimSpace(s) || (strings.HasPrefix(s, b64Open) && strings.HasSuffix(s, b64Close)) {
		return b64Open + b64(s) + b64Close
	}
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7e {
			return b64Open + b64(s) + b64Close
		}
	}
	return s
}

// Markers of a base64-encoded header value (Streamable HTTP 2026-07-28,
// "Value Encoding"); case-sensitive.
const (
	b64Open  = "=?base64?"
	b64Close = "?="
)

// decodeHeaderValue undoes headerValue; ok is false for a marked value
// that is not valid base64.
func decodeHeaderValue(v string) (string, bool) {
	if !strings.HasPrefix(v, b64Open) || !strings.HasSuffix(v, b64Close) || len(v) < len(b64Open)+len(b64Close) {
		return v, true
	}
	b, err := base64.StdEncoding.DecodeString(v[len(b64Open) : len(v)-len(b64Close)])
	if err != nil {
		return "", false
	}
	return string(b), true
}

// post sends one message over Streamable HTTP. For a request it returns
// the response, read from a JSON body or an SSE stream; for a notification
// it returns nil once the upstream accepted it.
func (c *conn) post(ctx context.Context, msg *message, notify func(*message)) (*message, http.Header, error) {
	c.mu.Lock()
	modern := c.mode == VersionModern
	c.mu.Unlock()
	if modern && msg.Method != "" {
		msg.Params = modernParams(msg.Params, c.agent)
	}
	body, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.srv.URL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	name := ""
	if msg.Method == "tools/call" || msg.Method == "prompts/get" {
		name = jsonString(msg.Params, "name")
	} else if msg.Method == "resources/read" {
		name = jsonString(msg.Params, "uri")
	}
	c.headers(req, msg.Method, name)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, nil, plugin.RedactURLError(err)
	}
	defer resp.Body.Close()
	c.mu.Lock()
	hadSession := c.session != ""
	c.mu.Unlock()
	switch {
	case resp.StatusCode == http.StatusNotFound && hadSession:
		return nil, resp.Header, errSessionGone
	case resp.StatusCode == http.StatusAccepted:
		return nil, resp.Header, nil
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var m message
		if json.Unmarshal(data, &m) == nil && m.Error != nil {
			return &m, resp.Header, &httpError{status: resp.StatusCode, rpc: true}
		}
		return nil, resp.Header, &httpError{status: resp.StatusCode, body: short(string(data))}
	}
	if !msg.hasID() {
		return nil, resp.Header, nil
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if ct == "text/event-stream" {
		m, err := c.readStream(ctx, resp.Body, msg.ID, notify)
		return m, resp.Header, err
	}
	var m message
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMessage)).Decode(&m); err != nil {
		return nil, resp.Header, fmt.Errorf("upstream response: %w", err)
	}
	return &m, resp.Header, nil
}

// readStream reads an SSE response until the response to id arrives.
func (c *conn) readStream(ctx context.Context, body io.Reader, id json.RawMessage, notify func(*message)) (*message, error) {
	rd := sse.NewReader(body, maxMessage)
	for {
		ev, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("upstream stream ended without a response")
			}
			return nil, err
		}
		if ev.Comment || len(ev.Data) == 0 {
			continue
		}
		var m message
		if json.Unmarshal(ev.Data, &m) != nil {
			continue
		}
		switch {
		case m.request():
			go c.refuse(context.WithoutCancel(ctx), &m)
		case m.Method != "":
			if notify != nil {
				notify(&m)
			}
		case bytes.Equal(m.ID, id):
			return &m, nil
		}
	}
}

// refuse answers a request the upstream sent us: the relay offers clients
// no sampling, elicitation or roots.
func (c *conn) refuse(ctx context.Context, m *message) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	reply := &message{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{Code: codeMethodNotFound, Message: "not supported by the relay"}}
	c.mu.Lock()
	mode, link := c.mode, c.link
	c.mu.Unlock()
	if mode == TransportSSE && link != nil {
		_ = link.send(ctx, c, reply)
		return
	}
	_, _, _ = c.post(ctx, reply, nil)
}

// listTools returns the upstream's tools, cached for a minute.
func (c *conn) listTools(ctx context.Context) ([]json.RawMessage, error) {
	c.mu.Lock()
	if c.tools != nil && time.Since(c.toolsAt) < toolsFor {
		t := c.tools
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()
	var all []json.RawMessage
	cursor := ""
	for page := 0; page < 50; page++ {
		params := json.RawMessage(`{}`)
		if cursor != "" {
			params, _ = json.Marshal(map[string]string{"cursor": cursor})
		}
		resp, err := c.call(ctx, "tools/list", params, nil)
		if err != nil {
			return nil, err
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		var res struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(resp.Result, &res); err != nil {
			return nil, fmt.Errorf("tools/list result: %w", err)
		}
		all = append(all, res.Tools...)
		if cursor = res.NextCursor; cursor == "" {
			break
		}
	}
	if all == nil {
		all = []json.RawMessage{}
	}
	c.mu.Lock()
	c.tools, c.toolsAt = all, time.Now()
	c.mu.Unlock()
	return all, nil
}

func (c *conn) info() (initInfo, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.init, c.version, c.mode
}

// ---------------------------------------------------------------- legacy SSE

var errLinkClosed = errors.New("upstream event stream closed")

// sseLink is the HTTP+SSE transport: one long GET carrying every message
// from the server, and POSTs to the endpoint it announced.
type sseLink struct {
	endpoint string
	cancel   context.CancelFunc
	done     chan struct{}

	mu      sync.Mutex
	pending map[string]chan *message
	notify  map[string]func(*message)
	err     error
}

// connectSSE opens the event stream, waits for the endpoint and then
// initializes over it (callers hold c.hs).
func (c *conn) connectSSE(ctx context.Context) error {
	lctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(lctx, http.MethodGet, c.srv.URL, nil)
	if err != nil {
		cancel()
		return err
	}
	for k, vs := range c.header {
		req.Header[k] = vs
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "plugrelay/"+c.agent)
	type opened struct {
		resp *http.Response
		err  error
	}
	got := make(chan opened, 1)
	go func() {
		resp, err := c.client.Do(req)
		got <- opened{resp, err}
	}()
	var resp *http.Response
	select {
	case o := <-got:
		if o.err != nil {
			cancel()
			return plugin.RedactURLError(o.err)
		}
		resp = o.resp
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return &httpError{status: resp.StatusCode}
	}
	l := &sseLink{cancel: cancel, done: make(chan struct{}), pending: map[string]chan *message{}, notify: map[string]func(*message){}}
	endpoint := make(chan string, 1)
	go l.read(resp, c.srv.URL, endpoint)
	select {
	case ep, ok := <-endpoint:
		if !ok {
			cancel()
			return errors.New("upstream event stream closed before announcing its endpoint")
		}
		l.endpoint = ep
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}
	c.set(func() { c.mode, c.link = TransportSSE, l })
	fail := func(err error) error {
		l.close()
		c.set(func() { c.link = nil })
		return err
	}
	resp2, err := l.call(ctx, c, &message{JSONRPC: "2.0", ID: c.id(), Method: "initialize", Params: initParams(c.agent, "2024-11-05")}, nil)
	if err != nil {
		return fail(err)
	}
	if resp2.Error != nil {
		return fail(resp2.Error)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		initInfo
	}
	_ = json.Unmarshal(resp2.Result, &res)
	if err := l.send(ctx, c, &message{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		return fail(err)
	}
	c.set(func() { c.version, c.init, c.ready = "", res.initInfo, true })
	return nil
}

// read dispatches the stream's messages until it ends.
func (l *sseLink) read(resp *http.Response, base string, endpoint chan<- string) {
	defer close(l.done)
	defer resp.Body.Close()
	announced := false
	rd := sse.NewReader(resp.Body, maxMessage)
	var err error
	for {
		var ev sse.Event
		ev, err = rd.Next()
		if err != nil {
			break
		}
		switch {
		case ev.Name == "endpoint" && !announced:
			u, perr := url.Parse(string(bytes.TrimSpace(ev.Data)))
			b, berr := url.Parse(base)
			if perr != nil || berr != nil {
				continue
			}
			ep := b.ResolveReference(u)
			if ep.Host != b.Host || ep.Scheme != b.Scheme {
				continue // the endpoint must stay on the configured server
			}
			announced = true
			endpoint <- ep.String()
			close(endpoint)
		case ev.Name == "" || ev.Name == "message":
			var m message
			if json.Unmarshal(ev.Data, &m) != nil {
				continue
			}
			l.dispatch(&m)
		}
	}
	if !announced {
		close(endpoint)
	}
	l.mu.Lock()
	l.err = errLinkClosed
	for id, ch := range l.pending {
		close(ch)
		delete(l.pending, id)
	}
	l.mu.Unlock()
}

func (l *sseLink) dispatch(m *message) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case m.request():
		// answered by the caller that sees it; nothing waits here
	case m.Method != "":
		// Each caller keeps only the notifications carrying its own
		// progress token.
		for _, fn := range l.notify {
			fn(m)
		}
	default:
		if ch, ok := l.pending[string(m.ID)]; ok {
			ch <- m
			delete(l.pending, string(m.ID))
		}
	}
}

// call posts a request and waits for its response on the stream.
func (l *sseLink) call(ctx context.Context, c *conn, m *message, notify func(*message)) (*message, error) {
	ch := make(chan *message, 1)
	key := string(m.ID)
	l.mu.Lock()
	if l.err != nil {
		l.mu.Unlock()
		return nil, l.err
	}
	l.pending[key] = ch
	if notify != nil {
		l.notify[key] = notify
	}
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.pending, key)
		delete(l.notify, key)
		l.mu.Unlock()
	}()
	if err := l.send(ctx, c, m); err != nil {
		return nil, err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, errLinkClosed
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// send posts one message to the announced endpoint.
func (l *sseLink) send(ctx context.Context, c *conn, m *message) error {
	body, _ := json.Marshal(m)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, vs := range c.header {
		req.Header[k] = vs
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "plugrelay/"+c.agent)
	resp, err := c.client.Do(req)
	if err != nil {
		return plugin.RedactURLError(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &httpError{status: resp.StatusCode}
	}
	return nil
}

func (l *sseLink) close() {
	l.cancel()
}
