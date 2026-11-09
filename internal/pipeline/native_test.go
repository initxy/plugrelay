package pipeline

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

func allowNative(*plugin.Caller, *http.Request, string) *ir.GwError { return nil }

func withNative(az func(*plugin.Caller, *http.Request, string) *ir.GwError, max int64) func(*Deps) {
	return func(d *Deps) { d.Native = Native{Authorize: az, MaxBody: max} }
}

// nativeServer serves /native/<ref>/<path> through ServeNative the way the
// HTTP server does after authentication.
func (e *env) nativeServer() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.EscapedPath(), "/native/")
		ref, path, _ := strings.Cut(rest, "/")
		caller := e.caller
		e.p.ServeNative(w, r, &caller, ref, "/"+path)
	}))
	e.t.Cleanup(srv.Close)
	return srv
}

func nativeEnv(t *testing.T, h func(http.ResponseWriter, *http.Request, []byte), mod ...func(*Deps)) (*env, *fakeUpstream, *httptest.Server) {
	t.Helper()
	up := newFake(t, h)
	if len(mod) == 0 {
		mod = []func(*Deps){withNative(allowNative, 0)}
	}
	e := newEnv(t, "m", []upSpec{{id: "o", protocol: "openai-chat", issuer: "iss-o", fake: up}}, mod...)
	return e, up, e.nativeServer()
}

// Method, escaped path, query, body and ordinary headers reach the
// upstream's base URL unchanged; the client's credentials do not, the
// credential's do. The answer comes back verbatim.
func TestNativeForwards(t *testing.T) {
	e, up, srv := nativeEnv(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Request-Id", "req_up")
		w.Header().Set("Set-Cookie", "s=1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("\x00\x01binary"))
	})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/native/o/files/a%2Fb/content?purpose=batch&key=pr_client&x=1", strings.NewReader("payload"))
	req.Header.Set("Authorization", "Bearer pr_client")
	req.Header.Set("X-Api-Key", "client-secret")
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	req.Header.Set("OpenAI-Beta", "assistants=v2")
	req.Header.Set("X-Forwarded-For", "10.9.9.9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || string(body) != "\x00\x01binary" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Request-Id") != "req_up" || resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("response headers %v", resp.Header)
	}
	if up.paths[0] != "/v1/files/a%2Fb/content?purpose=batch&x=1" || string(up.body(0)) != "payload" {
		t.Errorf("upstream got %s %q", up.paths[0], up.body(0))
	}
	h := up.headers[0]
	if h.Get("Authorization") != "Bearer sk-o" || h.Get("X-Api-Key") != "" || h.Get("X-Forwarded-For") != "" {
		t.Errorf("credential headers %v", h)
	}
	if h.Get("Content-Type") != "multipart/form-data; boundary=x" || h.Get("Openai-Beta") != "assistants=v2" {
		t.Errorf("forwarded headers %v", h)
	}
	rec := e.waitRecords(1)
	if rec.Protocol != NativeProtocol || rec.Status != 201 || len(rec.Attempts) != 1 || rec.CostMicro != 0 || rec.ChargeMicro != 0 {
		t.Fatalf("record %+v", rec)
	}
	a := rec.Attempts[0]
	if a.Tier != plugin.TierPassthrough || a.UpstreamID != "o" || a.CredentialID != "c1" || !a.Sent || a.BytesIn != 7 || a.BytesOut != 8 {
		t.Errorf("attempt %+v", a)
	}
	if a.Diagnostic == nil || len(a.Diagnostic.Notes) != 1 || a.Diagnostic.Notes[0] != "native: PUT /files/a%2Fb/content" {
		t.Errorf("diagnostic %+v", a.Diagnostic)
	}
}

// A path segment that decodes to ".." is refused before anything is sent.
func TestNativeRefusesDotSegments(t *testing.T) {
	_, up, srv := nativeEnv(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusOK)
	})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/native/o/%2e%2e/admin", nil)
	req.Header.Set("Authorization", "Bearer pr_client")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || len(up.paths) != 0 {
		t.Fatalf("status %d, upstream saw %v", resp.StatusCode, up.paths)
	}
}

// Response bytes reach the client as the upstream produces them, and the
// request body reaches the upstream while the client is still sending.
func TestNativeStreamsBothWays(t *testing.T) {
	gotFirst := make(chan struct{})
	release := make(chan struct{})
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 5)
		if _, err := io.ReadFull(r.Body, buf); err != nil || string(buf) != "part1" {
			t.Errorf("first upload chunk %q %v", buf, err)
		}
		close(gotFirst)
		rest, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+string(rest)+"\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upSrv.Close()
	e, _, srv := nativeEnv(t, nil)
	e.snap.Upstreams["o"].Config.Endpoints[0].BaseURL = upSrv.URL
	tu := e.snap.Table.Upstreams["o"]
	tu.Endpoints[0].BaseURL = upSrv.URL
	e.snap.Table.Upstreams["o"] = tu

	pr, pw := io.Pipe()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/native/o/stream", pr)
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			close(done)
			return
		}
		done <- resp
	}()
	_, _ = pw.Write([]byte("part1"))
	select {
	case <-gotFirst:
	case <-time.After(5 * time.Second):
		t.Fatal("upload buffered: upstream never saw the first chunk")
	}
	_, _ = pw.Write([]byte("part2"))
	pw.Close()
	resp := <-done
	if resp == nil {
		return
	}
	defer resp.Body.Close()
	first := make([]byte, len("data: part2\n\n"))
	if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != "data: part2\n\n" {
		t.Fatalf("first event %q %v", first, err)
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "data: [DONE]\n\n" {
		t.Errorf("rest %q", rest)
	}
	if rec := e.waitRecords(1); !rec.Stream || rec.Attempts[0].BytesIn != 10 {
		t.Errorf("record %+v attempts %+v", rec, rec.Attempts)
	}
}

func nativeDo(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestNativeRefusals(t *testing.T) {
	deny := func(*plugin.Caller, *http.Request, string) *ir.GwError {
		return ir.Errorf(ir.ErrPermission, "native passthrough to upstream \"o\" is not allowed for this key")
	}
	t.Run("no authorizer", func(t *testing.T) {
		_, up, srv := nativeEnv(t, jsonReply(200, `{}`), withNative(nil, 0))
		if st, body := nativeDo(t, srv, "GET", "/native/o/models", ""); st != 403 || up.calls() != 0 {
			t.Errorf("%d %s", st, body)
		}
	})
	t.Run("not granted", func(t *testing.T) {
		e, up, srv := nativeEnv(t, jsonReply(200, `{}`), withNative(deny, 0))
		st, body := nativeDo(t, srv, "GET", "/native/o/models", "")
		if st != 403 || !strings.Contains(body, `"message":"native passthrough to upstream \"o\" is not allowed`) || up.calls() != 0 {
			t.Errorf("%d %s", st, body)
		}
		if rec := e.waitRecords(1); rec.Status != 403 || rec.Protocol != NativeProtocol || len(rec.Attempts) != 0 {
			t.Errorf("record %+v", rec)
		}
	})
	t.Run("unknown upstream", func(t *testing.T) {
		_, _, srv := nativeEnv(t, jsonReply(200, `{}`))
		if st, body := nativeDo(t, srv, "GET", "/native/nope/models", ""); st != 404 {
			t.Errorf("%d %s", st, body)
		}
	})
	t.Run("unknown endpoint", func(t *testing.T) {
		_, _, srv := nativeEnv(t, jsonReply(200, `{}`))
		if st, body := nativeDo(t, srv, "GET", "/native/o:nope/models", ""); st != 404 {
			t.Errorf("%d %s", st, body)
		}
	})
	t.Run("named endpoint", func(t *testing.T) {
		_, up, srv := nativeEnv(t, jsonReply(200, `{}`))
		if st, body := nativeDo(t, srv, "GET", "/native/o:main/models", ""); st != 200 || up.paths[0] != "/v1/models" {
			t.Errorf("%d %s", st, body)
		}
	})
	t.Run("disabled upstream", func(t *testing.T) {
		e, up, srv := nativeEnv(t, jsonReply(200, `{}`))
		tu := e.snap.Table.Upstreams["o"]
		tu.Enabled = false
		e.snap.Table.Upstreams["o"] = tu
		if st, body := nativeDo(t, srv, "GET", "/native/o/models", ""); st != 403 || up.calls() != 0 {
			t.Errorf("%d %s", st, body)
		}
	})
	t.Run("credential cooling", func(t *testing.T) {
		e, up, srv := nativeEnv(t, jsonReply(200, `{}`))
		tu := e.snap.Table.Upstreams["o"]
		tu.Credentials = []router.Cred{{ID: "c1", Kind: router.KindShared, Status: router.StatusCooling, StatusUntil: time.Now().Add(time.Hour)}}
		e.snap.Table.Upstreams["o"] = tu
		e.snap.Table = &router.Table{Models: e.snap.Table.Models, Upstreams: e.snap.Table.Upstreams}
		st, body := nativeDo(t, srv, "GET", "/native/o/models", "")
		if st != 429 || up.calls() != 0 {
			t.Errorf("%d %s", st, body)
		}
	})
	t.Run("body too large", func(t *testing.T) {
		_, up, srv := nativeEnv(t, jsonReply(200, `{}`), withNative(allowNative, 4))
		if st, body := nativeDo(t, srv, "POST", "/native/o/files", "too long"); st != 413 || up.calls() != 0 {
			t.Errorf("%d %s", st, body)
		}
	})
	t.Run("upstream error verbatim", func(t *testing.T) {
		e, _, srv := nativeEnv(t, jsonReply(404, `{"error":{"message":"No such file"}}`))
		st, body := nativeDo(t, srv, "GET", "/native/o/files/x", "")
		if st != 404 || body != `{"error":{"message":"No such file"}}` {
			t.Errorf("%d %s", st, body)
		}
		if rec := e.waitRecords(1); rec.Status != 404 || rec.ErrorKind != "" {
			t.Errorf("record %+v", rec)
		}
	})
}

// The SSRF guard holds: an upstream not allowed to reach private addresses
// cannot be used to reach the loopback fake.
func TestNativeSSRFGuard(t *testing.T) {
	e, up, srv := nativeEnv(t, jsonReply(200, `{}`))
	e.snap.Upstreams["o"].AllowPrivate = false
	st, body := nativeDo(t, srv, "GET", "/native/o/models", "")
	if st != http.StatusBadGateway || up.calls() != 0 {
		t.Fatalf("%d %s (calls %d)", st, body, up.calls())
	}
	if rec := e.waitRecords(1); len(rec.Attempts) != 1 || rec.Attempts[0].Sent || rec.Attempts[0].Error == "" {
		t.Errorf("record %+v", rec.Attempts)
	}
}

func TestNativeURL(t *testing.T) {
	for _, tt := range []struct{ base, path, query, want string }{
		{"https://api.x.com/v1", "/files", "", "https://api.x.com/v1/files"},
		{"https://api.x.com/v1/", "/files/a%2Fb", "limit=2", "https://api.x.com/v1/files/a%2Fb?limit=2"},
		{"https://h", "/", "", "https://h/"},
		// The platform key a client may authenticate with in ?key= never
		// reaches the upstream; other parameters keep their bytes and order.
		{"https://h/v1", "/models", "key=pr_client_secret", "https://h/v1/models"},
		{"https://h/v1", "/models", "a=1&key=pr_x&b=%20&%6Bey=pr_y", "https://h/v1/models?a=1&b=%20"},
		{"https://h/v1", "/models", "keys=1&monkey=2", "https://h/v1/models?keys=1&monkey=2"},
	} {
		got, err := nativeURL(tt.base, tt.path, tt.query)
		if err != nil || got != tt.want {
			t.Errorf("nativeURL(%q, %q, %q) = %q, %v; want %q", tt.base, tt.path, tt.query, got, err, tt.want)
		}
	}
	// Dot segments, plain or encoded, could climb out of the base path.
	for _, p := range []string{"/../admin", "/%2e%2e/admin", "/a/%2E/b", "/x/.%2e/y", "/..",
		"/%2e%2e%2fadmin", "/a%2f..%2fadmin", "/a/..%2f", "/a%5c..%5cb", "/..;/admin", "/a/..;x=1/b"} {
		if got, err := nativeURL("https://h/tenant-a/v1", p, ""); err == nil {
			t.Errorf("nativeURL(%q) = %q, want an error", p, got)
		}
	}
	// Dots inside a name are fine.
	for _, p := range []string{"/files/a..b", "/v1/models/gpt-5.5", "/x/.well-known/y", "/a%2fb"} {
		if _, err := nativeURL("https://h/v1", p, ""); err != nil {
			t.Errorf("nativeURL(%q): %v, want accepted", p, err)
		}
	}
}
