package httpx

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func zl(b []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func rawDeflate(b []byte) []byte {
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func zs(b []byte) []byte {
	w, _ := zstd.NewWriter(nil)
	defer w.Close()
	return w.EncodeAll(b, nil)
}

func req(body []byte, enc string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	if enc != "" {
		r.Header.Set("Content-Encoding", enc)
	}
	return r
}

func TestReadBody(t *testing.T) {
	plain := []byte(`{"model":"x","messages":[]}`)
	big := bytes.Repeat([]byte("a"), 1<<20)
	tests := []struct {
		name   string
		body   []byte
		enc    string
		lim    Limits
		want   []byte
		status int
		decomp bool
	}{
		{"identity", plain, "", Limits{}, plain, 200, false},
		{"gzip", gz(plain), "gzip", Limits{}, plain, 200, false},
		{"x-gzip", gz(plain), "X-GZIP", Limits{}, plain, 200, false},
		{"zlib deflate", zl(plain), "deflate", Limits{}, plain, 200, false},
		{"raw deflate", rawDeflate(plain), "deflate", Limits{}, plain, 200, false},
		{"zstd", zs(plain), "zstd", Limits{}, plain, 200, false},
		{"stacked", gz(zs(plain)), "zstd, gzip", Limits{}, plain, 200, false},
		{"identity listed", plain, "identity", Limits{}, plain, 200, false},
		{"exact limit", plain, "", Limits{MaxCompressed: int64(len(plain))}, plain, 200, false},
		{"wire too large", plain, "", Limits{MaxCompressed: 5}, nil, 413, false},
		{"gzip bomb", gz(big), "gzip", Limits{MaxCompressed: 1 << 20, MaxDecompressed: 4096}, nil, 413, true},
		{"zstd bomb", zs(big), "zstd", Limits{MaxDecompressed: 4096}, nil, 413, true},
		{"compressed wire limit", gz(big), "gzip", Limits{MaxCompressed: 100}, nil, 413, false},
		{"bad gzip", plain, "gzip", Limits{}, nil, 400, false},
		{"bad zstd", plain, "zstd", Limits{}, nil, 400, false},
		{"unknown", plain, "br", Limits{}, nil, 415, false},
		{"too many codings", plain, "gzip, gzip, gzip", Limits{}, nil, 415, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := req(tc.body, tc.enc)
			got, err := ReadBody(r, tc.lim)
			if s := Status(err); s != tc.status {
				t.Fatalf("status %d (%v), want %d", s, err, tc.status)
			}
			if err != nil {
				var tl *TooLargeError
				if errors.As(err, &tl) && tl.Decompressed != tc.decomp {
					t.Fatalf("decompressed flag %v", tl.Decompressed)
				}
				return
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("got %q", got)
			}
			if r.Header.Get("Content-Encoding") != "" {
				t.Fatal("content-encoding left on request")
			}
		})
	}
	r := req(plain, "")
	r.ContentLength = 1 << 30
	if _, err := ReadBody(r, Limits{MaxCompressed: 10}); Status(err) != 413 {
		t.Fatal("declared length not checked")
	}
}

func TestPathMatcher(t *testing.T) {
	m := NewPathMatcher(
		"/v1/messages",
		"/v1/chat/completions",
		"/v1/models",
		"/v1beta/models",
		"/v1beta/models/{model}:generateContent",
		"/v1beta/models/{model}:streamGenerateContent",
		"/openai/deployments/{deployment}/chat/completions",
	)
	tests := []struct {
		path   string
		want   string
		params map[string]string
	}{
		{"/v1/messages", "/v1/messages", nil},
		{"/messages", "/v1/messages", nil},
		{"/v1/messages/", "/v1/messages", nil},
		{"//v1//messages", "/v1/messages", nil},
		{"/v1/v1/messages", "/v1/messages", nil},
		{"/v1/messages?beta=true", "/v1/messages", nil},
		{"/chat/completions", "/v1/chat/completions", nil},
		{"/v1/models", "/v1/models", nil},
		{"/v1beta/models", "/v1beta/models", nil},
		{"/models", "/v1/models", nil},
		{"/v1beta/models/gemini-2.5-pro:generateContent", "/v1beta/models/{model}:generateContent", map[string]string{"model": "gemini-2.5-pro"}},
		{"/models/g:streamGenerateContent", "/v1beta/models/{model}:streamGenerateContent", map[string]string{"model": "g"}},
		{"/openai/deployments/gpt55/chat/completions?api-version=2025-01-01", "/openai/deployments/{deployment}/chat/completions", map[string]string{"deployment": "gpt55"}},
		{"/v1/nope", "", nil},
		{"/v1beta/models/:generateContent", "", nil},
	}
	for _, tc := range tests {
		got, params, ok := m.Match(tc.path)
		if got != tc.want || ok != (tc.want != "") || !maps.Equal(params, tc.params) {
			t.Errorf("%s → %q %v %v, want %q %v", tc.path, got, params, ok, tc.want, tc.params)
		}
	}
	if CleanPath("") != "/" || CleanPath("a//b/") != "/a/b" || CleanPath("/") != "/" {
		t.Fatal("CleanPath")
	}
}

func TestClientIP(t *testing.T) {
	x := IPResolver{Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	tests := []struct {
		remote, xff, real, want string
	}{
		{"1.2.3.4:5", "9.9.9.9", "", "1.2.3.4"},                     // untrusted peer: headers ignored
		{"10.0.0.1:5", "9.9.9.9", "", "9.9.9.9"},                    // trusted proxy
		{"10.0.0.1:5", "6.6.6.6, 9.9.9.9, 10.0.0.2", "", "9.9.9.9"}, // spoofed leftmost ignored
		{"10.0.0.1:5", "", "8.8.8.8", "8.8.8.8"},
		{"10.0.0.1:5", "10.0.0.3", "", "10.0.0.1"}, // only trusted hops
		{"10.0.0.1:5", "garbage", "", "10.0.0.1"},
		{"[::ffff:1.2.3.4]:5", "", "", "1.2.3.4"},
		{"[2001:db8::1]:443", "", "", "2001:db8::1"},
	}
	for _, tc := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tc.remote
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if tc.real != "" {
			r.Header.Set("X-Real-IP", tc.real)
		}
		if got := x.ClientIP(r); got != tc.want {
			t.Errorf("%+v → %s", tc, got)
		}
	}
}
