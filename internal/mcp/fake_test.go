package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMCP is an upstream MCP server. mode: "streamable" (legacy
// Streamable HTTP with sessions), "modern" (2026-07-28, stateless) or
// "sse" (the HTTP+SSE transport of 2024-11-05).
type fakeMCP struct {
	t     *testing.T
	mode  string
	tools []string
	srv   *httptest.Server

	mu       sync.Mutex
	headers  []http.Header
	methods  []string
	sessions map[string]bool
	inited   bool
	forget   bool // answer the next session request with 404
	sseOut   map[string]chan []byte
	sessionN int
}

func newFakeMCP(t *testing.T, mode string, tools ...string) *fakeMCP {
	f := &fakeMCP{t: t, mode: mode, tools: tools, sessions: map[string]bool{}, sseOut: map[string]chan []byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() { f.srv.CloseClientConnections(); f.srv.Close() })
	return f
}

func (f *fakeMCP) seen() ([]string, []http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...), append([]http.Header(nil), f.headers...)
}

func (f *fakeMCP) serve(w http.ResponseWriter, r *http.Request) {
	if f.mode == "sse" {
		f.serveSSE(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var m message
	_ = json.Unmarshal(body, &m)
	f.mu.Lock()
	f.headers = append(f.headers, r.Header.Clone())
	f.methods = append(f.methods, m.Method)
	f.mu.Unlock()
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") || !strings.Contains(r.Header.Get("Accept"), "application/json") {
		http.Error(w, "bad accept", http.StatusNotAcceptable)
		return
	}
	if f.mode == "modern" {
		if m.Method == "initialize" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(&message{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{Code: codeUnsupportedProto,
				Message: "Unsupported protocol version", Data: json.RawMessage(`{"supported":["2026-07-28"],"requested":"2025-11-25"}`)}})
			return
		}
		meta := jsonString(m.Params, `_meta.io\.modelcontextprotocol/protocolVersion`)
		if r.Header.Get("MCP-Protocol-Version") != VersionModern || meta != VersionModern || r.Header.Get("Mcp-Method") != m.Method {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(&message{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{Code: codeHeaderMismatch, Message: "headers"}})
			return
		}
		if m.Method == "server/discover" {
			f.reply(w, m.ID, `{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"modern","version":"1"}},"ttlMs":1000,"cacheScope":"public"}`)
			return
		}
		f.handle(w, r, &m)
		return
	}
	switch {
	case m.Method == "initialize":
		f.mu.Lock()
		f.sessionN++
		id := fmt.Sprintf("up-session-%d", f.sessionN)
		f.sessions[id] = true
		f.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", id)
		f.reply(w, m.ID, `{"protocolVersion":"2025-11-25","capabilities":{"tools":{"listChanged":true},"resources":{}},"serverInfo":{"name":"fake","version":"1"},"instructions":"use the tools"}`)
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")
	f.mu.Lock()
	known := f.sessions[sid]
	if f.forget && known {
		f.forget, known = false, false
		delete(f.sessions, sid)
	}
	f.mu.Unlock()
	if !known {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Header.Get("MCP-Protocol-Version") != "2025-11-25" {
		http.Error(w, "missing protocol version", http.StatusBadRequest)
		return
	}
	if m.Method == "" { // our answer to a request of ours
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if m.Method == "notifications/initialized" {
		f.mu.Lock()
		f.inited = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	}
	f.handle(w, r, &m)
}

func (f *fakeMCP) reply(w http.ResponseWriter, id json.RawMessage, result string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(&message{JSONRPC: "2.0", ID: id, Result: json.RawMessage(result)})
}

func toolJSON(name string) string {
	return fmt.Sprintf(`{"name":%q,"description":"d","inputSchema":{"type":"object"}}`, name)
}

// handle answers tools/list (two pages), tools/call, resources/list and
// resources/read.
func (f *fakeMCP) handle(w http.ResponseWriter, r *http.Request, m *message) {
	switch m.Method {
	case "tools/list":
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(m.Params, &p)
		half := len(f.tools) / 2
		page, next := f.tools[:half], `,"nextCursor":"p2"`
		if p.Cursor == "p2" {
			page, next = f.tools[half:], ""
		}
		var ts []string
		for _, t := range page {
			ts = append(ts, toolJSON(t))
		}
		f.reply(w, m.ID, `{"tools":[`+strings.Join(ts, ",")+`]`+next+`}`)
	case "tools/call":
		name := jsonString(m.Params, "name")
		args := jsonString(m.Params, "arguments")
		token := jsonString(m.Params, "_meta.progressToken")
		if name == "fail" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(&message{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{Code: codeInvalidParams, Message: "bad args"}})
			return
		}
		text, _ := json.Marshal("called " + name + " with " + args)
		result := fmt.Sprintf(`{"content":[{"type":"text","text":%s}],"isError":false}`, text)
		if token == "" {
			f.reply(w, m.ID, result)
			return
		}
		// Stream: a log message, a progress notification, a request we
		// must refuse, then the response.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\",\"data\":\"x\"}}\n\n")
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progressToken\":%q,\"progress\":1,\"total\":2}}\n\n", token)
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progressToken\":\"someone-else\",\"progress\":1}}\n\n")
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"srv-1\",\"method\":\"sampling/createMessage\",\"params\":{}}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(20 * time.Millisecond)
		b, _ := json.Marshal(&message{JSONRPC: "2.0", ID: m.ID, Result: json.RawMessage(result)})
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
	case "resources/list":
		f.reply(w, m.ID, `{"resources":[{"uri":"file:///a","name":"a"}]}`)
	case "resources/read":
		f.reply(w, m.ID, `{"contents":[{"uri":"file:///a","text":"secret a"}]}`)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&message{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{Code: codeMethodNotFound, Message: "no"}})
	}
}

// serveSSE is the 2024-11-05 transport: GET /sse streams, POST
// /messages?session=… accepts and answers on the stream.
func (f *fakeMCP) serveSSE(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/sse":
		f.mu.Lock()
		f.sessionN++
		sid := fmt.Sprintf("s%d", f.sessionN)
		out := make(chan []byte, 16)
		f.sseOut[sid] = out
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: endpoint\ndata: /messages?session=%s\n\n", sid)
		w.(http.Flusher).Flush()
		for {
			select {
			case b := <-out:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	case r.Method == http.MethodPost && r.URL.Path == "/messages" && f.out(r.URL.Query().Get("session")) != nil:
		body, _ := io.ReadAll(r.Body)
		var m message
		_ = json.Unmarshal(body, &m)
		f.mu.Lock()
		f.methods = append(f.methods, m.Method)
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		if !m.hasID() || m.Method == "" {
			return
		}
		rec := httptest.NewRecorder()
		switch m.Method {
		case "initialize":
			f.reply(rec, m.ID, `{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"old","version":"0"}}`)
		default:
			f.handle(rec, r, &m)
		}
		f.out(r.URL.Query().Get("session")) <- []byte(strings.TrimSpace(rec.Body.String()))
	case r.Method == http.MethodPost:
		w.WriteHeader(http.StatusMethodNotAllowed)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeMCP) out(sid string) chan []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sseOut[sid]
}

// memKV is a plugin.KV in memory.
type memKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (k *memKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok, nil
}

func (k *memKV) Put(_ context.Context, key string, v []byte, _ time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = map[string][]byte{}
	}
	k.m[key] = v
	return nil
}

func (k *memKV) Delete(_ context.Context, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, key)
	return nil
}

func (k *memKV) Incr(context.Context, string, int64, time.Duration) (int64, error) { return 0, nil }
