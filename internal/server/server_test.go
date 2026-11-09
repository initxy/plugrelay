package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/initxy/plugrelay/internal/httpx"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
	_ "github.com/initxy/plugrelay/pkg/protocol/anthropic"
	_ "github.com/initxy/plugrelay/pkg/protocol/coherererank"
	_ "github.com/initxy/plugrelay/pkg/protocol/gemini"
	_ "github.com/initxy/plugrelay/pkg/protocol/geminiembeddings"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaiaudio"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaichat"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaicompletions"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaiembeddings"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaiimages"
	_ "github.com/initxy/plugrelay/pkg/protocol/openairesponses"
	_ "github.com/initxy/plugrelay/pkg/protocol/rerank"
	_ "github.com/initxy/plugrelay/pkg/protocol/teirerank"
	"github.com/initxy/plugrelay/plugins/auth/apikey"
)

// served records what reached the pipeline.
type served struct {
	codec  string
	route  protocol.Route
	caller *plugin.Caller
	body   string
}

type fakePipeline struct{ last *served }

func (f *fakePipeline) ServeProtocol(w http.ResponseWriter, r *http.Request, c protocol.Codec, rt protocol.Route, caller *plugin.Caller, body []byte) {
	f.last = &served{codec: c.Name(), route: rt, caller: caller, body: string(body)}
	w.WriteHeader(http.StatusTeapot)
}

// keyAuth accepts the key "pr_good" from any of the usual headers.
type keyAuth struct{}

func (keyAuth) Info() plugin.Info { return plugin.Info{ID: "auth.test", Kind: plugin.KindAuth} }

func (keyAuth) Authenticate(r *http.Request) (*plugin.Caller, error) {
	switch apikey.ExtractKey(r) {
	case "":
		return nil, nil
	case "pr_good":
		return &plugin.Caller{KeyID: "k1"}, nil
	}
	return nil, ir.Errorf(ir.ErrAuthentication, "invalid API key")
}

func newTestServer(console bool) (*Server, *fakePipeline) {
	fp := &fakePipeline{}
	o := Options{
		Pipeline: fp,
		Auth:     func() []plugin.Authenticator { return []plugin.Authenticator{keyAuth{}} },
		Limits:   httpx.Limits{MaxCompressed: 1 << 10, MaxDecompressed: 1 << 12},
		API: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "api")
		}),
		Logger: slog.New(slog.DiscardHandler),
	}
	if console {
		o.Console = fstest.MapFS{
			"index.html":         {Data: []byte("<html>console</html>")},
			"assets/app-1234.js": {Data: []byte("js")},
		}
	}
	return New(o), fp
}

func do(s *Server, method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestProtocolPathVariants(t *testing.T) {
	cases := []struct {
		method, path, codec string
		op                  protocol.Op
	}{
		{"POST", "/v1/messages", "anthropic", protocol.OpGenerate},
		{"POST", "/messages", "anthropic", protocol.OpGenerate},
		{"POST", "/v1/messages/", "anthropic", protocol.OpGenerate},
		{"POST", "/v1/messages?beta=true", "anthropic", protocol.OpGenerate},
		{"POST", "/v1/messages/count_tokens", "anthropic", protocol.OpCountTokens},
		{"POST", "/v1/chat/completions", "openai-chat", protocol.OpGenerate},
		{"POST", "/chat/completions", "openai-chat", protocol.OpGenerate},
		{"POST", "/v1/v1/chat/completions", "openai-chat", protocol.OpGenerate},
		{"POST", "/chat/completions?api-version=2024-10-21", "openai-chat", protocol.OpGenerate},
		{"POST", "/v1/completions", "openai-completions", protocol.OpGenerate},
		{"POST", "/completions", "openai-completions", protocol.OpGenerate},
		{"POST", "/v1/completions/", "openai-completions", protocol.OpGenerate},
		{"POST", "/v1/responses", "openai-responses", protocol.OpGenerate},
		{"POST", "/responses/compact", "openai-responses", protocol.OpGenerate},
		{"POST", "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", "gemini", protocol.OpGenerate},
		{"POST", "/models/gemini-2.5-pro:generateContent", "gemini", protocol.OpGenerate},
		{"POST", "/v1beta/models/gemini-embedding-001:embedContent", "gemini-embeddings", protocol.OpEmbed},
		{"POST", "/models/gemini-embedding-001:batchEmbedContents", "gemini-embeddings", protocol.OpEmbed},
		{"POST", "/v1/embeddings", "openai-embeddings", protocol.OpEmbed},
		{"POST", "/embeddings", "openai-embeddings", protocol.OpEmbed},
		{"POST", "/v1/images/generations", "openai-images", protocol.OpImage},
		{"POST", "/v1/images/edits", "openai-images", protocol.OpImageEdit},
		{"POST", "/v1/audio/transcriptions", "openai-audio", protocol.OpTranscribe},
		{"POST", "/audio/translations", "openai-audio", protocol.OpTranslate},
		{"POST", "/v1/audio/speech", "openai-audio", protocol.OpSpeech},
	}
	s, fp := newTestServer(false)
	for _, c := range cases {
		fp.last = nil
		w := do(s, c.method, c.path, `{"model":"m"}`, "Authorization", "Bearer pr_good")
		if w.Code != http.StatusTeapot || fp.last == nil {
			t.Errorf("%s %s: status %d, not served", c.method, c.path, w.Code)
			continue
		}
		if fp.last.codec != c.codec || fp.last.route.Op != c.op {
			t.Errorf("%s %s: got %s/%s, want %s/%s", c.method, c.path, fp.last.codec, fp.last.route.Op, c.codec, c.op)
		}
		want := `{"model":"m"}`
		if fp.last.route.Upload {
			want = "" // streamed by the pipeline
		}
		if fp.last.body != want || fp.last.caller == nil || fp.last.caller.KeyID != "k1" {
			t.Errorf("%s %s: body %q caller %+v", c.method, c.path, fp.last.body, fp.last.caller)
		}
	}
	if w := do(s, "POST", "/v1beta/models/gemini-2.5-pro", "{}", "x-goog-api-key", "pr_good"); w.Code != http.StatusNotFound {
		t.Errorf("gemini path without action: %d", w.Code)
	}
}

func TestAuthHeaders(t *testing.T) {
	s, fp := newTestServer(false)
	for _, h := range [][]string{
		{"Authorization", "Bearer pr_good"},
		{"x-api-key", "pr_good"},
		{"x-goog-api-key", "pr_good"},
	} {
		fp.last = nil
		if w := do(s, "POST", "/v1/messages", "{}", h...); w.Code != http.StatusTeapot {
			t.Errorf("%s: %d", h[0], w.Code)
		}
	}
	// Missing and wrong keys are answered in the client's protocol.
	w := do(s, "POST", "/v1/messages", "{}")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "authentication_error") {
		t.Errorf("anthropic without key: %d %s", w.Code, w.Body)
	}
	w = do(s, "POST", "/v1/chat/completions", "{}", "Authorization", "Bearer pr_bad")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("chat with bad key: %d %s", w.Code, w.Body)
	}
	w = do(s, "POST", "/v1beta/models/g:generateContent", "{}")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "UNAUTHENTICATED") {
		t.Errorf("gemini without key: %d %s", w.Code, w.Body)
	}
}

func TestModelsDispatch(t *testing.T) {
	s, fp := newTestServer(false)
	cases := []struct {
		target string
		hdr    []string
		codec  string
	}{
		{"/v1/models", []string{"Authorization", "Bearer pr_good"}, "openai-chat"},
		{"/models", []string{"Authorization", "Bearer pr_good"}, "openai-chat"},
		{"/v1/models?client_version=0.99.0", []string{"Authorization", "Bearer pr_good"}, "openai-responses"},
		{"/v1/models", []string{"x-api-key", "pr_good", "anthropic-version", "2023-06-01"}, "anthropic"},
		{"/v1beta/models", []string{"x-goog-api-key", "pr_good"}, "gemini"},
	}
	for _, c := range cases {
		fp.last = nil
		w := do(s, "GET", c.target, "", c.hdr...)
		if fp.last == nil || fp.last.codec != c.codec || fp.last.route.Op != protocol.OpModels {
			t.Errorf("%s %v: status %d served %+v, want %s", c.target, c.hdr, w.Code, fp.last, c.codec)
		}
	}
}

func TestHelloAndWebSocket(t *testing.T) {
	s, fp := newTestServer(false)
	for _, m := range []string{"HEAD", "GET"} {
		if w := do(s, m, "/api/hello", ""); w.Code != http.StatusOK || fp.last != nil {
			t.Errorf("%s /api/hello: %d", m, w.Code)
		}
	}
	w := do(s, "GET", "/v1/responses", "", "Upgrade", "websocket", "Connection", "Upgrade", "Authorization", "Bearer pr_good")
	if w.Code != http.StatusUpgradeRequired || fp.last != nil {
		t.Fatalf("websocket: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "websocket") {
		t.Errorf("426 body: %s", w.Body)
	}
}

func TestBodyLimit(t *testing.T) {
	s, fp := newTestServer(false)
	w := do(s, "POST", "/v1/chat/completions", `{"x":"`+strings.Repeat("a", 2000)+`"}`, "Authorization", "Bearer pr_good")
	if w.Code != http.StatusRequestEntityTooLarge || fp.last != nil {
		t.Fatalf("oversized body: %d", w.Code)
	}
}

func TestConsoleAndOtherRoutes(t *testing.T) {
	s, _ := newTestServer(true)
	w := do(s, "GET", "/keys/abc", "", "Accept", "text/html")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "console") {
		t.Fatalf("SPA fallback: %d %s", w.Code, w.Body)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || w.Header().Get("X-Frame-Options") != "DENY" ||
		!strings.Contains(csp, "frame-src 'none';") || strings.Contains(csp, "cloudflare") || strings.Contains(csp, "hcaptcha") {
		t.Errorf("security headers without a captcha: %v", w.Header())
	}
	s.o.CaptchaProviders = func(context.Context) []string { return []string{"turnstile"} }
	csp := do(s, "GET", "/", "", "Accept", "text/html").Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' https://challenges.cloudflare.com;") || !strings.Contains(csp, "frame-src https://challenges.cloudflare.com;") ||
		strings.Contains(csp, "hcaptcha") {
		t.Errorf("CSP with turnstile on: %s", csp)
	}
	s.o.CaptchaProviders = nil
	// A browser loading /models gets the console, an API client the listing.
	if w := do(s, "GET", "/models", "", "Accept", "text/html,application/xhtml+xml"); !strings.Contains(w.Body.String(), "console") {
		t.Errorf("browser /models: %d %s", w.Code, w.Body)
	}
	w = do(s, "GET", "/assets/app-1234.js", "")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("asset caching: %d %v", w.Code, w.Header())
	}
	if w := do(s, "GET", "/assets/missing.js", ""); w.Code != 404 {
		t.Errorf("missing asset: %d", w.Code)
	}
	if w := do(s, "GET", "/api/v1/me", ""); w.Body.String() != "api" {
		t.Errorf("api mount: %s", w.Body)
	}
	if w := do(s, "GET", "/healthz", ""); w.Code != 200 {
		t.Errorf("healthz: %d", w.Code)
	}
	if w := do(s, "GET", "/readyz", ""); w.Code != 200 {
		t.Errorf("readyz: %d", w.Code)
	}
	if w := do(s, "GET", "/metrics", ""); w.Code != 404 {
		t.Errorf("metrics without handler: %d", w.Code)
	}
	// Native passthrough is off unless a handler is wired.
	if w := do(s, "GET", "/native/x/y", "", "Authorization", "Bearer pr_good"); w.Code != 404 {
		t.Errorf("native without handler: %d", w.Code)
	}
	if id := w.Header().Get(RequestIDHeader); id == "" {
		t.Error("no request id")
	}
}

func TestPanicRecovery(t *testing.T) {
	s := New(Options{Pipeline: &fakePipeline{}, Auth: func() []plugin.Authenticator { return nil },
		Logger: slog.New(slog.DiscardHandler),
		API:    http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })})
	w := do(s, "GET", "/api/v1/x", "")
	if w.Code != 500 || !bytes.Contains(w.Body.Bytes(), []byte("internal")) {
		t.Fatalf("panic: %d %s", w.Code, w.Body)
	}
}

type nativeCall struct {
	method, ref, path, query string
	caller                   *plugin.Caller
}

type fakeNative struct{ last *nativeCall }

func (f *fakeNative) ServeNative(w http.ResponseWriter, r *http.Request, caller *plugin.Caller, ref, path string) {
	f.last = &nativeCall{method: r.Method, ref: ref, path: path, query: r.URL.RawQuery, caller: caller}
	w.WriteHeader(http.StatusAccepted)
}

// /native/ requires a platform key, then hands the unescaped upstream ref
// and the escaped rest of the path to the native handler.
func TestNativeDispatch(t *testing.T) {
	fn := &fakeNative{}
	s := New(Options{Pipeline: &fakePipeline{}, Native: fn, Logger: slog.New(slog.DiscardHandler),
		Auth: func() []plugin.Authenticator { return []plugin.Authenticator{keyAuth{}} }})
	if w := do(s, "POST", "/native/openai/files", "x"); w.Code != 401 || fn.last != nil || !strings.Contains(w.Body.String(), "missing or invalid API key") {
		t.Fatalf("no key: %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/native/openai/files", "x", "Authorization", "Bearer pr_bad"); w.Code != 401 || fn.last != nil {
		t.Fatalf("bad key: %d %s", w.Code, w.Body)
	}
	w := do(s, "DELETE", "/native/my%20up:ep/files/a%2Fb?x=1", "", "x-api-key", "pr_good")
	if w.Code != http.StatusAccepted || fn.last == nil {
		t.Fatalf("dispatch: %d %s", w.Code, w.Body)
	}
	if c := fn.last; c.method != "DELETE" || c.ref != "my up:ep" || c.path != "/files/a%2Fb" || c.query != "x=1" || c.caller.KeyID != "k1" {
		t.Errorf("native call %+v", c)
	}
	// A protocol path under /native/ is still native, never a model call.
	fn.last = nil
	if w := do(s, "POST", "/native/openai/v1/chat/completions", "{}", "Authorization", "Bearer pr_good"); w.Code != http.StatusAccepted || fn.last == nil || fn.last.path != "/v1/chat/completions" {
		t.Errorf("protocol path: %d %+v", w.Code, fn.last)
	}
	for _, p := range []string{"/native", "/native/", "/native//files"} {
		if w := do(s, "GET", p, "", "Authorization", "Bearer pr_good"); w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
}

// Upload routes are not read by the server: the body reaches the pipeline
// unread, bounded by the body limit; a declared oversize length is a 413
// and a compressed upload a 415, both before the pipeline runs.
func TestUploadRoutesStreamBody(t *testing.T) {
	s, fp := newTestServer(false)
	big := strings.Repeat("x", 2<<10) // over the 1 KiB test limit
	w := do(s, "POST", "/v1/audio/transcriptions", big, "Authorization", "Bearer pr_good", "Content-Type", "multipart/form-data; boundary=b")
	if w.Code != http.StatusRequestEntityTooLarge || fp.last != nil {
		t.Fatalf("declared oversize: %d %s", w.Code, w.Body)
	}
	w = do(s, "POST", "/v1/images/edits", "x", "Authorization", "Bearer pr_good", "Content-Encoding", "gzip")
	if w.Code != http.StatusUnsupportedMediaType || fp.last != nil {
		t.Fatalf("compressed: %d %s", w.Code, w.Body)
	}
	w = do(s, "POST", "/v1/audio/transcriptions", "small", "Authorization", "Bearer pr_good")
	if w.Code != http.StatusTeapot || fp.last == nil || fp.last.body != "" || !fp.last.route.Upload {
		t.Fatalf("upload: %d %+v", w.Code, fp.last)
	}
}
