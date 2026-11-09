package extplugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// MaxFrameSize bounds one JSON-RPC message in either direction.
const MaxFrameSize = 64 << 20

// JSON-RPC and plugrelay error codes; see sdk/PROTOCOL.md.
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

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

func readFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" && length < 0 {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("read frame header: %w", err)
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
			if err != nil || n < 0 {
				return nil, fmt.Errorf("bad Content-Length %q", value)
			}
			if n > MaxFrameSize {
				return nil, fmt.Errorf("frame of %d bytes exceeds limit %d", n, MaxFrameSize)
			}
			length = n
		}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("read frame body: %w", err)
	}
	return body, nil
}

func writeFrame(w io.Writer, body []byte) error {
	var b bytes.Buffer
	b.Grow(len(body) + 32)
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	b.Write(body)
	_, err := w.Write(b.Bytes())
	return err
}

// handlerFunc serves a request or notification the plugin sent to the host.
type handlerFunc func(ctx context.Context, method string, params json.RawMessage) (any, error)

// eventStream receives $/event notifications for one provider.send call.
type eventStream struct {
	ch     chan json.RawMessage
	closed chan struct{}
	once   sync.Once
}

func (s *eventStream) close() { s.once.Do(func() { close(s.closed) }) }

// conn is a bidirectional JSON-RPC 2.0 connection.
type conn struct {
	r      *bufio.Reader
	w      io.Writer
	closer io.Closer
	handle handlerFunc

	wmu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *message
	streams map[string]*eventStream
	err     error

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func newConn(r io.Reader, w io.Writer, closer io.Closer, handle handlerFunc) *conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &conn{
		r: bufio.NewReaderSize(r, 64<<10), w: w, closer: closer, handle: handle,
		pending: map[int64]chan *message{}, streams: map[string]*eventStream{},
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
}

// run reads until the connection fails, then fails every pending call.
func (c *conn) run() {
	err := c.readLoop()
	if err == nil || errors.Is(err, io.EOF) {
		err = errors.New("connection closed")
	}
	c.mu.Lock()
	c.err = err
	pending := c.pending
	c.pending = map[int64]chan *message{}
	for _, s := range c.streams {
		s.close()
	}
	c.streams = map[string]*eventStream{}
	c.mu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
	c.cancel()
	close(c.done)
}

func (c *conn) readLoop() error {
	for {
		body, err := readFrame(c.r)
		if err != nil {
			return err
		}
		var m message
		if err := json.Unmarshal(body, &m); err != nil {
			_ = c.send(&message{ID: json.RawMessage("null"), Error: &rpcError{Code: CodeParseError, Message: err.Error()}})
			continue
		}
		switch {
		case m.Method == "$/event":
			c.routeEvent(m.Params)
		case m.Method != "":
			go c.serve(m)
		default:
			c.deliver(&m)
		}
	}
}

func (c *conn) routeEvent(params json.RawMessage) {
	var p struct {
		Token string          `json:"token"`
		Event json.RawMessage `json:"event"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	c.mu.Lock()
	s := c.streams[p.Token]
	c.mu.Unlock()
	if s == nil {
		return
	}
	select {
	case s.ch <- p.Event:
	case <-s.closed:
	}
}

func (c *conn) deliver(m *message) {
	id, err := strconv.ParseInt(string(m.ID), 10, 64)
	if err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- m
	}
}

func (c *conn) serve(m message) {
	result, err := c.handle(c.ctx, m.Method, m.Params)
	if len(m.ID) == 0 {
		return
	}
	resp := &message{ID: m.ID}
	if err != nil {
		var re *rpcError
		if !errors.As(err, &re) {
			re = &rpcError{Code: CodeInternalError, Message: err.Error()}
		}
		resp.Error = re
	} else {
		raw, merr := json.Marshal(result)
		if merr != nil {
			resp.Error = &rpcError{Code: CodeInternalError, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	}
	_ = c.send(resp)
}

func (c *conn) send(m *message) error {
	m.JSONRPC = "2.0"
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(body) > MaxFrameSize {
		return fmt.Errorf("message of %d bytes exceeds limit %d", len(body), MaxFrameSize)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return writeFrame(c.w, body)
}

func (c *conn) notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.send(&message{Method: method, Params: raw})
}

// start sends a request and returns its id and the channel its response
// arrives on. The channel is closed without a value if the connection dies.
func (c *conn) start(method string, params any) (int64, chan *message, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return 0, nil, err
	}
	ch := make(chan *message, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return 0, nil, c.err
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.send(&message{ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: raw}); err != nil {
		c.forget(id)
		return 0, nil, err
	}
	return id, ch, nil
}

func (c *conn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *conn) cancelRequest(id int64) {
	c.forget(id)
	_ = c.notify("$/cancelRequest", map[string]int64{"id": id})
}

// errConnClosed is returned by call when the connection died mid-call.
var errConnClosed = errors.New("connection closed")

// errStoppedConn is returned by acquire when the process is stopped.
var errStoppedConn = errors.New("stopped")

// call sends a request and decodes the result into out (may be nil).
func (c *conn) call(ctx context.Context, method string, params, out any) error {
	id, ch, err := c.start(method, params)
	if err != nil {
		return fmt.Errorf("%w: %v", errConnClosed, err)
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return errConnClosed
		}
		return decodeResult(m, out)
	case <-ctx.Done():
		c.cancelRequest(id)
		return ctx.Err()
	}
}

func decodeResult(m *message, out any) error {
	if m.Error != nil {
		return m.Error
	}
	if out == nil || len(m.Result) == 0 || string(m.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(m.Result, out); err != nil {
		return &rpcError{Code: CodeInvalidParams, Message: "decode result: " + err.Error()}
	}
	return nil
}

func (c *conn) openStream(token string) *eventStream {
	s := &eventStream{ch: make(chan json.RawMessage, 64), closed: make(chan struct{})}
	c.mu.Lock()
	if c.err != nil {
		s.close()
	} else {
		c.streams[token] = s
	}
	c.mu.Unlock()
	return s
}

func (c *conn) closeStream(token string) {
	c.mu.Lock()
	s := c.streams[token]
	delete(c.streams, token)
	c.mu.Unlock()
	if s != nil {
		s.close()
	}
}

func (c *conn) close() error {
	if c.closer != nil {
		return c.closer.Close()
	}
	return nil
}
