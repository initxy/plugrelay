package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

type records struct {
	mu   sync.Mutex
	recs []*plugin.Record
}

func (q *records) Enqueue(r *plugin.Record) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.recs = append(q.recs, r)
	return true
}

func (q *records) last(t *testing.T) *plugin.Record {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.recs) == 0 {
		t.Fatal("no record")
	}
	return q.recs[len(q.recs)-1]
}

type env struct {
	t      *testing.T
	g      *Gateway
	reg    *Registry
	srv    *httptest.Server
	recs   *records
	access Access
	limit  *ir.GwError
	caller plugin.Caller
}

func newEnv(t *testing.T) *env {
	t.Helper()
	box, err := secret.NewBox(bytes.Repeat([]byte{3}, secret.MasterKeySize))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, reg: NewRegistry(&memKV{}, box), recs: &records{}, access: Access{Servers: []string{"*"}},
		caller: plugin.Caller{KeyID: "k1", UserID: "u1"}}
	e.g = New(Deps{Registry: e.reg, Egress: egress.New(egress.Options{}), Secret: []byte("s"), Records: e.recs, Version: "test",
		Access: func(*plugin.Caller) Access { return e.access },
		Limit: func(*http.Request, *plugin.Caller, string) (func(int), *ir.GwError) {
			if e.limit != nil {
				return nil, e.limit
			}
			return func(int) {}, nil
		}})
	t.Cleanup(e.g.Close)
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/mcp"), "/")
		c := e.caller
		e.g.ServeMCP(w, r, &c, id)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func ptr[T any](v T) *T { return &v }

func (e *env) add(id, url string, mod ...func(*Input)) {
	e.t.Helper()
	in := Input{ID: ptr(id), URL: ptr(url), AllowPrivate: ptr(true),
		SecretHeaders: map[string]string{"Authorization": "Bearer upstream-" + id}, Headers: map[string]string{"X-Team": "eng"}}
	for _, f := range mod {
		f(&in)
	}
	if _, err := e.reg.Create(context.Background(), in); err != nil {
		e.t.Fatal(err)
	}
}

// post sends one JSON-RPC message; hdr are header name/value pairs.
func (e *env) post(path, body string, hdr ...string) (*http.Response, string) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer pr_platform_key")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// open initializes a legacy session on path and returns its id.
func (e *env) open(path string) string {
	e.t.Helper()
	resp, body := e.post(path, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	sid := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || sid == "" || gjson.Get(body, "result.protocolVersion").Str != "2025-06-18" {
		e.t.Fatalf("initialize: %d %s %v", resp.StatusCode, body, resp.Header)
	}
	if resp, _ := e.post(path, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "Mcp-Session-Id", sid); resp.StatusCode != 202 {
		e.t.Fatalf("initialized: %d", resp.StatusCode)
	}
	return sid
}

func toolNames(body string) []string {
	var out []string
	for _, n := range gjson.Get(body, "result.tools.#.name").Array() {
		out = append(out, n.Str)
	}
	return out
}

func TestStreamableServer(t *testing.T) {
	e := newEnv(t)
	up := newFakeMCP(t, "streamable", "read", "write", "search", "admin_drop")
	e.add("files", up.srv.URL, func(in *Input) { in.ToolsDeny = []string{"admin_*"} })

	_, body := e.post("/mcp/files", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if gjson.Get(body, "result.capabilities.resources").Exists() == false || gjson.Get(body, "result.instructions").Str != "use the tools" ||
		gjson.Get(body, "result.capabilities.tools.listChanged").Exists() {
		t.Fatalf("initialize result %s", body)
	}
	sid := e.open("/mcp/files")

	// Sessions are required and bound to the key.
	if resp, body := e.post("/mcp/files", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); resp.StatusCode != 400 {
		t.Fatalf("no session: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.post("/mcp/files", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, "Mcp-Session-Id", sid+"x"); resp.StatusCode != 404 {
		t.Fatalf("forged session: %d", resp.StatusCode)
	}
	other := e.caller
	e.caller.KeyID = "k2"
	if resp, _ := e.post("/mcp/files", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, "Mcp-Session-Id", sid); resp.StatusCode != 404 {
		t.Fatalf("other key's session: %d", resp.StatusCode)
	}
	e.caller = other
	if resp, _ := e.post("/mcp/files", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, "Mcp-Session-Id", sid, "MCP-Protocol-Version", "2025-11-25"); resp.StatusCode != 400 {
		t.Fatalf("version differs from the session: %d", resp.StatusCode)
	}

	resp, body := e.post("/mcp/files", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, "Mcp-Session-Id", sid, "MCP-Protocol-Version", "2025-06-18")
	if resp.StatusCode != 200 || !slices.Equal(toolNames(body), []string{"read", "write", "search"}) || gjson.Get(body, "id").Int() != 2 {
		t.Fatalf("tools/list (two upstream pages, server deny list): %s", body)
	}
	_, body = e.post("/mcp/files", `{"jsonrpc":"2.0","id":"c","method":"tools/call","params":{"name":"read","arguments":{"path":"/x"}}}`, "Mcp-Session-Id", sid)
	if gjson.Get(body, "result.content.0.text").Str != `called read with {"path":"/x"}` || gjson.Get(body, "id").Str != "c" {
		t.Fatalf("tools/call: %s", body)
	}
	_, body = e.post("/mcp/files", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"admin_drop"}}`, "Mcp-Session-Id", sid)
	if gjson.Get(body, "error.code").Int() != codeInvalidParams {
		t.Fatalf("denied tool callable: %s", body)
	}
	_, body = e.post("/mcp/files", `{"jsonrpc":"2.0","id":4,"method":"resources/list"}`, "Mcp-Session-Id", sid)
	if gjson.Get(body, "result.resources.0.uri").Str != "file:///a" {
		t.Fatalf("resources/list passthrough: %s", body)
	}
	_, body = e.post("/mcp/files", `{"jsonrpc":"2.0","id":5,"method":"ping"}`, "Mcp-Session-Id", sid)
	if body != "{\"jsonrpc\":\"2.0\",\"id\":5,\"result\":{}}\n" {
		t.Fatalf("ping: %q", body)
	}
	_, body = e.post("/mcp/files", `{"jsonrpc":"2.0","id":6,"method":"sampling/whatever"}`, "Mcp-Session-Id", sid)
	if gjson.Get(body, "error.code").Int() != codeMethodNotFound {
		t.Fatalf("unknown method: %s", body)
	}

	// Credential injection: the server got its own secret and headers,
	// never the platform key, plus its session and protocol version.
	methods, headers := up.seen()
	if !slices.Equal(methods[:2], []string{"initialize", "notifications/initialized"}) {
		t.Fatalf("upstream methods %v", methods)
	}
	for i, h := range headers {
		if h.Get("Authorization") != "Bearer upstream-files" || h.Get("X-Team") != "eng" {
			t.Fatalf("request %d (%s) headers %v", i, methods[i], h)
		}
		if i > 0 && (h.Get("Mcp-Session-Id") != "up-session-1" || h.Get("MCP-Protocol-Version") != "2025-11-25") {
			t.Fatalf("request %d (%s) session headers %v", i, methods[i], h)
		}
	}

	// One record per tool call; arguments ride in the body (the request
	// log sink keeps them only with body logging on).
	rec := e.recs.last(t)
	if rec.Protocol != Protocol || rec.Model != "files__admin_drop" && rec.Model != "admin_drop" || rec.Status != 404 {
		t.Fatalf("denied call record %+v", rec)
	}
	e.recs.mu.Lock()
	first := e.recs.recs[0]
	e.recs.mu.Unlock()
	if first.Model != "files__read" || first.Status != 200 || string(first.Body.Request) != `{"path":"/x"}` ||
		first.Attempts[0].UpstreamID != "mcp:files" || first.Caller.KeyID != "k1" {
		t.Fatalf("call record %+v %s", first, first.Body.Request)
	}

	// The upstream forgot the session: one re-initialize, then the call works.
	up.mu.Lock()
	up.forget = true
	up.mu.Unlock()
	_, body = e.post("/mcp/files", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"write","arguments":{}}}`, "Mcp-Session-Id", sid)
	if gjson.Get(body, "result.content.0.text").Str != "called write with {}" {
		t.Fatalf("after upstream session loss: %s", body)
	}
	if methods, _ := up.seen(); slices.Index(methods[2:], "initialize") < 0 {
		t.Fatalf("no re-initialize: %v", methods)
	}

	// Upstream JSON-RPC errors pass through.
	_, body = e.post("/mcp/files", `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"fail"}}`, "Mcp-Session-Id", sid)
	if gjson.Get(body, "error.code").Int() != codeUnknownTool() {
		t.Fatalf("unknown upstream tool: %s", body)
	}

	// GET opens no stream; DELETE ends the session.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/mcp/files", nil)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 405 || resp.Header.Get("Allow") == "" {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodDelete, e.srv.URL+"/mcp/files", nil)
	req.Header.Set("Mcp-Session-Id", sid)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 204 {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
}

func codeUnknownTool() int64 { return codeInvalidParams }

func TestProgressStream(t *testing.T) {
	e := newEnv(t)
	up := newFakeMCP(t, "streamable", "slow")
	e.add("work", up.srv.URL)
	sid := e.open("/mcp/work")
	resp, body := e.post("/mcp/work", `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"slow","arguments":{"n":1},"_meta":{"progressToken":"tok-7"}}}`, "Mcp-Session-Id", sid)
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("not a stream: %v %s", resp.Header, body)
	}
	events := strings.Split(strings.TrimSpace(body), "\n\n")
	if len(events) != 2 {
		t.Fatalf("want one progress event and the response, got:\n%s", body)
	}
	if !strings.Contains(events[0], `"method":"notifications/progress"`) || !strings.Contains(events[0], `"progressToken":"tok-7"`) {
		t.Fatalf("progress event %s", events[0])
	}
	if !strings.Contains(events[1], `"id":9`) || !strings.Contains(events[1], `called slow`) {
		t.Fatalf("response event %s", events[1])
	}
	// The upstream got a token of the relay's, not the client's.
	if _, hs := up.seen(); len(hs) == 0 {
		t.Fatal("no upstream call")
	}
	// The server's sampling request was refused with an error POST.
	deadline := time.Now().Add(2 * time.Second)
	for {
		methods, _ := up.seen()
		if slices.Contains(methods, "") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server request never answered: %v", methods)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A client that accepts only JSON gets the response alone.
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/mcp/work", strings.NewReader(`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"slow","_meta":{"progressToken":1}}}`))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Mcp-Session-Id", sid)
	r2, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(r2.Body)
	if r2.Header.Get("Content-Type") != "application/json" || gjson.GetBytes(b, "result.content.0.text").Str == "" {
		t.Fatalf("JSON-only client: %v %s", r2.Header, b)
	}
}

func TestAggregatedNamesAndAccess(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("x", 70)
	a := newFakeMCP(t, "streamable", "a.b", "a_b", "secret_read", long, "ok")
	b := newFakeMCP(t, "streamable", "search", "ok")
	e.add("alpha", a.srv.URL)
	e.add("beta", b.srv.URL)
	e.add("off", b.srv.URL, func(in *Input) { in.Enabled = ptr(false) })
	sid := e.open("/mcp")
	_, body := e.post("/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "Mcp-Session-Id", sid)
	names := toolNames(body)
	hashed := "alpha__" + strings.Repeat("x", 48) + "_" + hash8("alpha/"+long)
	want := []string{"alpha__a_b", "alpha__a_b_2", "alpha__ok", "alpha__secret_read", hashed, "beta__ok", "beta__search"}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("names\n got %v\nwant %v", names, want)
	}
	for _, n := range names {
		if len(n) > 64 {
			t.Fatalf("%s longer than 64", n)
		}
	}
	// Each alias reaches its own tool: "a.b" sorts first, so it keeps the plain name.
	for _, c := range [][2]string{{"alpha__a_b", "a.b"}, {"alpha__a_b_2", "a_b"}, {hashed, long}, {"beta__ok", "ok"}} {
		alias, tool := c[0], c[1]
		_, body = e.post("/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+alias+`"}}`, "Mcp-Session-Id", sid)
		if got := gjson.Get(body, "result.content.0.text").Str; got != "called "+tool+" with " {
			t.Errorf("%s → %q (%s)", alias, got, body)
		}
	}
	if rec := e.recs.last(t); rec.Model != "beta__ok" || rec.Attempts[0].UpstreamID != "mcp:beta" {
		t.Fatalf("aggregated record %+v", rec)
	}

	// Grants: only alpha, and no secret_* tools.
	e.access = Access{Servers: []string{"alpha"}, ToolsDeny: []string{"alpha__secret*"}}
	_, body = e.post("/mcp", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`, "Mcp-Session-Id", sid)
	if names := toolNames(body); slices.Contains(names, "beta__ok") || slices.Contains(names, "alpha__secret_read") || !slices.Contains(names, "alpha__ok") {
		t.Fatalf("filtered names %v", names)
	}
	for _, n := range []string{"beta__ok", "alpha__secret_read", "off__ok", "nosuch__x", "plain"} {
		_, body = e.post("/mcp", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"`+n+`"}}`, "Mcp-Session-Id", sid)
		if gjson.Get(body, "error.code").Int() != codeInvalidParams {
			t.Errorf("%s callable: %s", n, body)
		}
	}
	if resp, _ := e.post("/mcp/beta", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`); resp.StatusCode != 404 {
		t.Fatalf("ungranted server endpoint: %d", resp.StatusCode)
	}
	e.access = Access{Servers: []string{"*"}, ToolsAllow: [][]string{{"beta__*"}}}
	_, body = e.post("/mcp", `{"jsonrpc":"2.0","id":5,"method":"tools/list"}`, "Mcp-Session-Id", sid)
	if names := toolNames(body); !slices.Equal(names, []string{"beta__ok", "beta__search"}) {
		t.Fatalf("allow list %v", names)
	}
	e.access = Access{}
	_, body = e.post("/mcp", `{"jsonrpc":"2.0","id":6,"method":"tools/list"}`, "Mcp-Session-Id", sid)
	if names := toolNames(body); len(names) != 0 || !gjson.Get(body, "result.tools").IsArray() {
		t.Fatalf("no grant still lists %s", body)
	}

	// The aggregated endpoint offers tools only.
	e.access = Access{Servers: []string{"*"}}
	_, body = e.post("/mcp", `{"jsonrpc":"2.0","id":7,"method":"resources/list"}`, "Mcp-Session-Id", sid)
	if gjson.Get(body, "error.code").Int() != codeMethodNotFound {
		t.Fatalf("aggregated resources: %s", body)
	}
}

func TestRateLimitAndRequestChecks(t *testing.T) {
	e := newEnv(t)
	up := newFakeMCP(t, "streamable", "t")
	e.add("s", up.srv.URL)
	sid := e.open("/mcp/s")
	e.limit = &ir.GwError{Kind: ir.ErrRateLimited, Message: "request rate limit of 1 per minute reached", RetryAfter: 30 * time.Second}
	resp, body := e.post("/mcp/s", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t"}}`, "Mcp-Session-Id", sid)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "30" || !strings.Contains(body, "rate limit") {
		t.Fatalf("limited: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	if rec := e.recs.last(t); rec.Status != 429 || rec.ErrorKind != ir.ErrRateLimited {
		t.Fatalf("limited record %+v", rec)
	}
	if resp, _ := e.post("/mcp/s", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, "Mcp-Session-Id", sid); resp.StatusCode != 400 {
		t.Fatalf("batch: %d", resp.StatusCode)
	}
	if resp, _ := e.post("/mcp/s", `{nope`); resp.StatusCode != 400 {
		t.Fatalf("parse error: %d", resp.StatusCode)
	}
	if resp, _ := e.post("/mcp/s", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Mcp-Session-Id", sid, "Origin", "https://evil.example"); resp.StatusCode != 403 {
		t.Fatalf("foreign origin: %d", resp.StatusCode)
	}
	if resp, _ := e.post("/mcp/nosuch", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`); resp.StatusCode != 404 {
		t.Fatalf("unknown server: %d", resp.StatusCode)
	}
}

func TestLegacySSEUpstream(t *testing.T) {
	e := newEnv(t)
	up := newFakeMCP(t, "sse", "old_tool")
	e.add("old", up.srv.URL+"/sse")
	sid := e.open("/mcp/old")
	_, body := e.post("/mcp/old", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "Mcp-Session-Id", sid)
	if !slices.Equal(toolNames(body), []string{"old_tool"}) {
		t.Fatalf("tools over SSE transport: %s", body)
	}
	_, body = e.post("/mcp/old", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"old_tool","arguments":{"a":1}}}`, "Mcp-Session-Id", sid)
	if gjson.Get(body, "result.content.0.text").Str != `called old_tool with {"a":1}` {
		t.Fatalf("call over SSE transport: %s", body)
	}
	res := e.g.Test(context.Background(), &Server{ID: "old", URL: up.srv.URL + "/sse", Transport: TransportSSE, AllowPrivate: true})
	if !res.OK || res.Transport != TransportSSE || !slices.Equal(res.Tools, []string{"old_tool"}) {
		t.Fatalf("test: %+v", res)
	}
	_, headers := up.seen()
	for _, h := range headers {
		if h.Get("Authorization") == "Bearer pr_platform_key" {
			t.Fatal("platform key reached the server")
		}
	}
}

func TestModernClientAndServer(t *testing.T) {
	e := newEnv(t)
	up := newFakeMCP(t, "modern", "now")
	e.add("m", up.srv.URL)
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	hdr := func(method string, more ...string) []string {
		return append([]string{"MCP-Protocol-Version", VersionModern, "Mcp-Method", method}, more...)
	}

	resp, body := e.post("/mcp/m", `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+meta+`}}`, hdr("server/discover")...)
	if resp.StatusCode != 200 || resp.Header.Get("Mcp-Session-Id") != "" || gjson.Get(body, "result.resultType").Str != "complete" ||
		!slices.Contains(toStrings(gjson.Get(body, "result.supportedVersions").Array()), VersionModern) {
		t.Fatalf("discover: %d %s", resp.StatusCode, body)
	}
	_, body = e.post("/mcp/m", `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{`+meta+`}}`, hdr("tools/list")...)
	if !slices.Equal(toolNames(body), []string{"now"}) || gjson.Get(body, "result.cacheScope").Str != "private" || gjson.Get(body, "result.resultType").Str != "complete" {
		t.Fatalf("modern tools/list: %s", body)
	}
	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"now","arguments":{},` + meta + `}}`
	_, body = e.post("/mcp/m", call, hdr("tools/call", "Mcp-Name", "now")...)
	if gjson.Get(body, "result.resultType").Str != "complete" || gjson.Get(body, "result.content.0.text").Str != "called now with {}" {
		t.Fatalf("modern call: %s", body)
	}
	// Mcp-Name may come base64-encoded.
	_, body = e.post("/mcp/m", call, hdr("tools/call", "Mcp-Name", "=?base64?"+b64("now")+"?=")...)
	if gjson.Get(body, "result.content.0.text").Str != "called now with {}" {
		t.Fatalf("modern call, encoded name: %s", body)
	}
	if resp, _ := e.post("/mcp/m", `{"jsonrpc":"2.0","id":4,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25"}}}`, hdr("tools/list")...); resp.StatusCode != 400 {
		t.Fatalf("header mismatch: %d", resp.StatusCode)
	}
	// Mcp-Method and Mcp-Name must be present and match the body.
	for _, tc := range []struct {
		name string
		hdr  []string
	}{
		{"no Mcp-Method", []string{"MCP-Protocol-Version", VersionModern, "Mcp-Name", "now"}},
		{"other Mcp-Method", hdr("tools/list", "Mcp-Name", "now")},
		{"no Mcp-Name", hdr("tools/call")},
		{"other Mcp-Name", hdr("tools/call", "Mcp-Name", "delete_all")},
		{"bad base64", hdr("tools/call", "Mcp-Name", "=?base64?!!?=")},
	} {
		resp, body := e.post("/mcp/m", call, tc.hdr...)
		if resp.StatusCode != 400 || gjson.Get(body, "error.code").Int() != codeHeaderMismatch || gjson.Get(body, "id").Int() != 3 {
			t.Fatalf("%s: %d %s", tc.name, resp.StatusCode, body)
		}
	}
	read := `{"jsonrpc":"2.0","id":6,"method":"resources/read","params":{"uri":"file:///a b",` + meta + `}}`
	if resp, body := e.post("/mcp/m", read, hdr("resources/read", "Mcp-Name", "file:///other")...); resp.StatusCode != 400 ||
		gjson.Get(body, "error.code").Int() != codeHeaderMismatch {
		t.Fatalf("resources/read uri mismatch: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.post("/mcp/m", `{"jsonrpc":"2.0","id":5,"method":"nope","params":{`+meta+`}}`, hdr("nope")...); resp.StatusCode != 404 {
		t.Fatalf("modern unknown method: %d", resp.StatusCode)
	}
	// The upstream was spoken to statelessly with the modern headers.
	methods, headers := up.seen()
	for i, h := range headers {
		if methods[i] == "initialize" {
			continue
		}
		if h.Get("Mcp-Session-Id") != "" || h.Get("Mcp-Method") != methods[i] {
			t.Fatalf("%s headers %v", methods[i], h)
		}
		if methods[i] == "tools/call" && h.Get("Mcp-Name") != "now" {
			t.Fatalf("Mcp-Name %q", h.Get("Mcp-Name"))
		}
	}
	res := e.g.Test(context.Background(), &Server{ID: "m", URL: up.srv.URL, AllowPrivate: true})
	if !res.OK || res.Transport != VersionModern || gjson.GetBytes(res.ServerInfo, "name").Str != "modern" {
		t.Fatalf("test: %+v %s", res, res.ServerInfo)
	}
}

func toStrings(rs []gjson.Result) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Str)
	}
	return out
}

func TestEgressGuard(t *testing.T) {
	e := newEnv(t)
	up := newFakeMCP(t, "streamable", "t")
	e.add("private", up.srv.URL, func(in *Input) { in.AllowPrivate = ptr(false) })
	res := e.g.Test(context.Background(), mustGet(t, e.reg, "private"))
	if res.OK || !strings.Contains(res.Error, "non-public address") {
		t.Fatalf("loopback server reached without allow_private: %+v", res)
	}
}

func mustGet(t *testing.T, r *Registry, id string) *Server {
	t.Helper()
	s, err := r.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRegistry(t *testing.T) {
	ctx := context.Background()
	box, _ := secret.NewBox(bytes.Repeat([]byte{5}, secret.MasterKeySize))
	kv := &memKV{}
	r := NewRegistry(kv, box)
	for _, in := range []Input{
		{ID: ptr("Bad_ID"), URL: ptr("https://x")},
		{ID: ptr("a"), URL: ptr("ftp://x")},
		{ID: ptr("a"), URL: ptr("https://u:p@x")},
		{ID: ptr("a"), URL: ptr("https://x"), Transport: ptr("ws")},
		{ID: ptr("a"), URL: ptr("https://x"), Headers: map[string]string{"Mcp-Session-Id": "x"}},
		{ID: ptr("a"), URL: ptr("https://x"), SecretHeaders: map[string]string{"X-Key": "a\nb"}},
		{ID: ptr("a"), URL: ptr("https://x"), Proxy: ptr("::bad")},
	} {
		if _, err := r.Create(ctx, in); err == nil {
			t.Errorf("accepted %+v", in)
		}
	}
	s, err := r.Create(ctx, Input{ID: ptr("gh"), URL: ptr("https://api.example/mcp"), SecretHeaders: map[string]string{"authorization": "Bearer ghp_x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create(ctx, Input{ID: ptr("gh"), URL: ptr("https://x")}); err != ErrExists {
		t.Fatalf("duplicate: %v", err)
	}
	raw, _, _ := kv.Get(ctx, serversKey)
	if strings.Contains(string(raw), "ghp_x") {
		t.Fatal("secret stored in clear")
	}
	v := ViewOf(*s)
	if b, _ := json.Marshal(v); strings.Contains(string(b), "ghp_x") || !slices.Equal(v.SecretHeaders, []string{"Authorization"}) {
		t.Fatalf("view %s", b)
	}
	h, err := r.open(s)
	if err != nil || h.Get("Authorization") != "Bearer ghp_x" {
		t.Fatalf("open %v %v", h, err)
	}
	// Another server cannot open this one's sealed value.
	s2 := *s
	s2.ID = "other"
	if _, err := r.open(&s2); err == nil {
		t.Fatal("sealed header opened under another id")
	}
	s, err = r.Update(ctx, "gh", Input{Name: ptr("GitHub"), SecretHeaders: map[string]string{"Authorization": ""}})
	if err != nil || s.Name != "GitHub" || len(s.SecretHeaders) != 0 || s.URL != "https://api.example/mcp" {
		t.Fatalf("update %+v %v", s, err)
	}
	if _, err := r.Update(ctx, "gh", Input{ID: ptr("x")}); err == nil {
		t.Fatal("id changed")
	}
	if err := r.Delete(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "gh"); err != ErrNotFound {
		t.Fatalf("after delete: %v", err)
	}
	noBox := NewRegistry(&memKV{}, nil)
	if _, err := noBox.Create(ctx, Input{ID: ptr("a"), URL: ptr("https://x"), SecretHeaders: map[string]string{"X-Key": "k"}}); err == nil {
		t.Fatal("secret stored without a master key")
	}
}

func TestAliasesDeterministic(t *testing.T) {
	a := aliases("s", []string{"x y", "x_y", "x-y"})
	b := aliases("s", []string{"x-y", "x_y", "x y"})
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("%v", a)
	}
	for k, v := range a {
		if b[k] != v {
			t.Fatalf("order changed names: %v vs %v", a, b)
		}
	}
}

// TestForwardedReadsLimitedAndRecorded: resources/read on a single-server
// endpoint goes through the rate limiter and the request log; listings do
// not.
func TestForwardedReadsLimitedAndRecorded(t *testing.T) {
	e := newEnv(t)
	up := newFakeMCP(t, "streamable", "read")
	e.add("files", up.srv.URL)
	sid := e.open("/mcp/files")
	read := `{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"file:///a"}}`
	_, body := e.post("/mcp/files", read, "Mcp-Session-Id", sid)
	if gjson.Get(body, "result.contents.0.text").Str != "secret a" {
		t.Fatalf("resources/read: %s", body)
	}
	rec := e.recs.last(t)
	if rec.Model != "files/resources/read" || rec.Status != 200 || !strings.Contains(string(rec.Body.Response), "secret a") ||
		!slices.Contains(rec.Attempts[0].Diagnostic.Notes, "mcp: resources/read file:///a") {
		t.Fatalf("read record %+v", rec)
	}
	n := len(e.recs.recs)
	e.post("/mcp/files", `{"jsonrpc":"2.0","id":8,"method":"resources/list"}`, "Mcp-Session-Id", sid)
	if len(e.recs.recs) != n {
		t.Fatal("listing recorded")
	}
	e.limit = &ir.GwError{Kind: ir.ErrRateLimited, Message: "slow down"}
	if resp, body := e.post("/mcp/files", read, "Mcp-Session-Id", sid); resp.StatusCode != 429 || gjson.Get(body, "error.message").Str != "slow down" {
		t.Fatalf("limited read: %d %s", resp.StatusCode, body)
	}
	if rec := e.recs.last(t); rec.Status != 429 {
		t.Fatalf("limited read record status %d", rec.Status)
	}
}

// TestSessionExpires: a session id is refused once older than sessionTTL.
func TestSessionExpires(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.g.d.Now = func() time.Time { return now }
	up := newFakeMCP(t, "streamable", "read")
	e.add("files", up.srv.URL)
	sid := e.open("/mcp/files")
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	if resp, _ := e.post("/mcp/files", list, "Mcp-Session-Id", sid); resp.StatusCode != 200 {
		t.Fatalf("fresh session: %d", resp.StatusCode)
	}
	now = now.Add(sessionTTL + time.Minute)
	if resp, _ := e.post("/mcp/files", list, "Mcp-Session-Id", sid); resp.StatusCode != 404 {
		t.Fatalf("expired session: %d", resp.StatusCode)
	}
}

// TestURLChangeNeedsSecretsAgain: moving a server to another origin keeps
// its sealed headers only when they are entered again (or cleared).
func TestURLChangeNeedsSecretsAgain(t *testing.T) {
	e := newEnv(t)
	e.add("gh", "https://mcp.example.com/mcp")
	ctx := context.Background()
	if _, err := e.reg.Update(ctx, "gh", Input{URL: ptr("https://MCP.example.com:443/v2")}); err != nil {
		t.Fatalf("same origin, new path: %v", err)
	}
	var fe *FieldError
	if _, err := e.reg.Update(ctx, "gh", Input{URL: ptr("https://evil.example.net/mcp")}); !errors.As(err, &fe) || fe.Field != "secret_headers.Authorization" {
		t.Fatalf("new host without secrets: %v", err)
	}
	if s, _ := e.reg.Get(ctx, "gh"); s.URL != "https://MCP.example.com:443/v2" {
		t.Fatalf("refused update applied: %s", s.URL)
	}
	if _, err := e.reg.Update(ctx, "gh", Input{URL: ptr("https://other.example.net/mcp"), SecretHeaders: map[string]string{"authorization": "Bearer new"}}); err != nil {
		t.Fatalf("new host with the secret again: %v", err)
	}
	if _, err := e.reg.Update(ctx, "gh", Input{URL: ptr("http://third.example.net/mcp"), SecretHeaders: map[string]string{"Authorization": ""}}); err != nil {
		t.Fatalf("new host, secret cleared: %v", err)
	}
}
