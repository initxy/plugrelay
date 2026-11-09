package pluginsdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/initxy/plugrelay/pkg/ir"
)

// ProtocolVersion is the plugin protocol version this SDK speaks.
const ProtocolVersion = 1

// MaxFrameSize bounds one message.
const MaxFrameSize = 64 << 20

// Error codes; see sdk/PROTOCOL.md.
const (
	CodeParseError       = -32700
	CodeInvalidRequest   = -32600
	CodeMethodNotFound   = -32601
	CodeInvalidParams    = -32602
	CodeInternalError    = -32603
	CodePluginError      = -32000
	CodeRejected         = -32001
	CodePermissionDenied = -32002
	CodeNotInitialized   = -32003
	CodeHostUnavailable  = -32004
	CodeRequestCancelled = -32800
)

// RPCError is a JSON-RPC error. Return one from a handler to choose the
// code; any other error becomes CodePluginError.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Reject returns a deliberate refusal: the host sends it to the client
// instead of applying the plugin's fail policy (auth.authenticate,
// meter.reserve, ...).
func Reject(kind ir.ErrorKind, message string) error {
	data, _ := json.Marshal(Error{Kind: kind, Message: message})
	return &RPCError{Code: CodeRejected, Message: message, Data: data}
}

// HandlerFunc serves one method. The result is JSON-encoded.
type HandlerFunc func(ctx context.Context, params json.RawMessage) (any, error)

// Plugin is a plugin server: register handlers, then call Serve.
type Plugin struct {
	// ID must equal the manifest id.
	ID      string
	Version string
	// OnInit runs during initialize with the global-layer settings.
	OnInit func(ctx context.Context, settings map[string]json.RawMessage) error
	// OnShutdown runs when the host asks the plugin to shut down.
	OnShutdown func(ctx context.Context) error

	handlers map[string]HandlerFunc
	host     *Host

	wmu     sync.Mutex
	w       io.Writer
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *message
	running map[string]context.CancelFunc
	inited  bool
}

// New returns a Plugin for the manifest id.
func New(id, version string) *Plugin {
	p := &Plugin{ID: id, Version: version, handlers: map[string]HandlerFunc{}, pending: map[int64]chan *message{}, running: map[string]context.CancelFunc{}}
	p.host = &Host{p: p}
	return p
}

// Handle registers a raw handler for a protocol method.
func (p *Plugin) Handle(method string, h HandlerFunc) { p.handlers[method] = h }

// Host returns the client for host.* calls.
func (p *Plugin) Host() *Host { return p.host }

// Points lists the registered protocol methods.
func (p *Plugin) Points() []string {
	out := make([]string, 0, len(p.handlers))
	for m := range p.handlers {
		out = append(out, m)
	}
	return out
}

// Serve speaks the protocol on stdin/stdout, or on the unix socket named
// by PLUGRELAY_SOCKET. It returns after the host's exit notification or
// when the input closes.
func (p *Plugin) Serve() error {
	if path := os.Getenv("PLUGRELAY_SOCKET"); path != "" {
		c, err := net.Dial("unix", path)
		if err != nil {
			return err
		}
		defer c.Close()
		return p.ServeStreams(c, c)
	}
	return p.ServeStreams(os.Stdin, os.Stdout)
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// ServeStreams speaks the protocol on r and w.
func (p *Plugin) ServeStreams(r io.Reader, w io.Writer) error {
	p.w = w
	br := bufio.NewReaderSize(r, 64<<10)
	var wg sync.WaitGroup
	defer func() {
		p.mu.Lock()
		for _, cancel := range p.running {
			cancel()
		}
		for id, ch := range p.pending {
			close(ch)
			delete(p.pending, id)
		}
		p.mu.Unlock()
		wg.Wait()
	}()
	for {
		body, err := readFrame(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var m message
		if err := json.Unmarshal(body, &m); err != nil {
			p.send(&message{ID: json.RawMessage("null"), Error: &RPCError{Code: CodeParseError, Message: err.Error()}})
			continue
		}
		switch {
		case m.Method == "exit":
			return nil
		case m.Method == "$/cancelRequest":
			var c struct {
				ID json.RawMessage `json:"id"`
			}
			if json.Unmarshal(m.Params, &c) == nil {
				p.mu.Lock()
				if cancel := p.running[string(c.ID)]; cancel != nil {
					cancel()
				}
				p.mu.Unlock()
			}
		case m.Method != "":
			// Register before the goroutine starts so a $/cancelRequest
			// read right after the request is never lost.
			ctx, cancel := p.track(m)
			wg.Go(func() { p.serve(ctx, cancel, m) })
		default:
			p.deliver(&m)
		}
	}
}

// track gives a request its context and makes it cancellable by id.
func (p *Plugin) track(m message) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	if len(m.ID) > 0 {
		p.mu.Lock()
		p.running[string(m.ID)] = cancel
		p.mu.Unlock()
	}
	return ctx, cancel
}

func (p *Plugin) serve(ctx context.Context, cancel context.CancelFunc, m message) {
	defer cancel()
	if len(m.ID) > 0 {
		defer func() {
			p.mu.Lock()
			delete(p.running, string(m.ID))
			p.mu.Unlock()
		}()
	}
	result, err := p.dispatch(ctx, m.Method, m.Params)
	if len(m.ID) == 0 {
		return
	}
	resp := &message{ID: m.ID}
	if err != nil {
		var re *RPCError
		if !errors.As(err, &re) {
			code := CodePluginError
			if ctx.Err() != nil {
				code = CodeRequestCancelled
			}
			re = &RPCError{Code: code, Message: err.Error()}
		}
		resp.Error = re
	} else if raw, merr := json.Marshal(result); merr != nil {
		resp.Error = &RPCError{Code: CodeInternalError, Message: merr.Error()}
	} else {
		resp.Result = raw
	}
	p.send(resp)
}

func (p *Plugin) dispatch(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var ip struct {
			ProtocolVersion int                        `json:"protocol_version"`
			Settings        map[string]json.RawMessage `json:"settings"`
		}
		if err := json.Unmarshal(params, &ip); err != nil {
			return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
		}
		if ip.ProtocolVersion != ProtocolVersion {
			return nil, &RPCError{Code: CodeInvalidRequest, Message: fmt.Sprintf("unsupported protocol version %d", ip.ProtocolVersion)}
		}
		if p.OnInit != nil {
			if err := p.OnInit(ctx, ip.Settings); err != nil {
				return nil, err
			}
		}
		p.mu.Lock()
		p.inited = true
		p.mu.Unlock()
		res := map[string]any{
			"protocol_version": ProtocolVersion,
			"manifest":         map[string]string{"id": p.ID, "version": p.Version},
			"capabilities":     map[string]any{"points": p.Points()},
		}
		return res, nil
	case "shutdown":
		if p.OnShutdown != nil {
			return struct{}{}, p.OnShutdown(ctx)
		}
		return struct{}{}, nil
	}
	p.mu.Lock()
	inited := p.inited
	p.mu.Unlock()
	if !inited {
		return nil, &RPCError{Code: CodeNotInitialized, Message: "initialize first"}
	}
	h := p.handlers[method]
	if h == nil {
		return nil, &RPCError{Code: CodeMethodNotFound, Message: "method not found: " + method}
	}
	return h(ctx, params)
}

func (p *Plugin) deliver(m *message) {
	id, err := strconv.ParseInt(string(m.ID), 10, 64)
	if err != nil {
		return
	}
	p.mu.Lock()
	ch := p.pending[id]
	delete(p.pending, id)
	p.mu.Unlock()
	if ch != nil {
		ch <- m
	}
}

func (p *Plugin) send(m *message) error {
	m.JSONRPC = "2.0"
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if p.w == nil {
		return errors.New("not serving")
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	b.Write(body)
	_, err = p.w.Write(b.Bytes())
	return err
}

func (p *Plugin) notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return p.send(&message{Method: method, Params: raw})
}

// request calls a host method and decodes its result.
func (p *Plugin) request(ctx context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ch := make(chan *message, 1)
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	p.pending[id] = ch
	p.mu.Unlock()
	if err := p.send(&message{ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: raw}); err != nil {
		return err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return errors.New("connection closed")
		}
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return ctx.Err()
	}
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" && length < 0 {
				return nil, io.EOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if length < 0 {
				return nil, errors.New("frame without Content-Length")
			}
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("malformed frame header %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 0 || n > MaxFrameSize {
				return nil, fmt.Errorf("bad Content-Length %q", value)
			}
			length = n
		}
	}
	body := make([]byte, length)
	_, err := io.ReadFull(r, body)
	return body, err
}
