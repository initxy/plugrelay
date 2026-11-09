package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/version"
	"github.com/initxy/plugrelay/pkg/plugin"
)

func TestOutboundHeaders(t *testing.T) {
	client := http.Header{
		"Authorization":            {"Bearer user"},
		"X-Api-Key":                {"k"},
		"X-Goog-Api-Key":           {"k"},
		"Openai-Organization":      {"org"},
		"Openai-Project":           {"p"},
		"Cookie":                   {"c"},
		"Host":                     {"h"},
		"Content-Length":           {"5"},
		"Accept-Encoding":          {"gzip"},
		"Connection":               {"keep-alive, X-Custom-Hop"},
		"X-Custom-Hop":             {"1"},
		"Keep-Alive":               {"timeout=5"},
		"Transfer-Encoding":        {"chunked"},
		"X-Forwarded-For":          {"1.2.3.4"},
		"X-Forwarded-Proto":        {"https"},
		"Forwarded":                {"for=1.2.3.4"},
		"Via":                      {"1.1 x"},
		"X-Real-Ip":                {"1.2.3.4"},
		"Anthropic-Beta":           {"a", "b"},
		"Anthropic-Version":        {"2023-06-01"},
		"User-Agent":               {"claude-cli/2.0"},
		"X-Claude-Code-Session-Id": {"s1"},
		"X-Owned":                  {"client value"},
	}
	dropped := []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Openai-Organization", "Openai-Project",
		"Cookie", "Host", "Content-Length", "Accept-Encoding", "Connection", "X-Custom-Hop", "Keep-Alive",
		"Transfer-Encoding", "X-Forwarded-For", "X-Forwarded-Proto", "Forwarded", "Via", "X-Real-Ip", "X-Owned"}

	tests := []struct {
		name  string
		same  bool
		keep  []string
		want  []string
		agent string
	}{
		{"same protocol forwards the rest verbatim", true, nil,
			[]string{"Anthropic-Beta", "Anthropic-Version", "User-Agent", "X-Claude-Code-Session-Id"}, "claude-cli/2.0"},
		{"cross protocol keeps only listed", false, []string{"x-claude-code-session-id", "authorization"},
			[]string{"X-Claude-Code-Session-Id", "User-Agent"}, "plugrelay/" + version.Version},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := OutboundHeaders(client, []string{"x-owned"}, tc.same, tc.keep)
			for _, d := range dropped {
				if out.Get(d) != "" {
					t.Errorf("%s forwarded", d)
				}
			}
			var got []string
			for k := range out {
				got = append(got, k)
			}
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
			if out.Get("User-Agent") != tc.agent {
				t.Fatalf("UA %q", out.Get("User-Agent"))
			}
		})
	}
	if v := OutboundHeaders(client, nil, true, nil)["Anthropic-Beta"]; !slices.Equal(v, []string{"a", "b"}) {
		t.Fatalf("multi-value header: %v", v)
	}
	out := OutboundHeaders(client, nil, true, nil)
	out["Anthropic-Beta"][0] = "mutated"
	if client["Anthropic-Beta"][0] != "a" {
		t.Fatal("output aliases input")
	}
}

func TestInboundResponseHeaders(t *testing.T) {
	up := http.Header{
		"Connection":                             {"close, X-Hop"},
		"X-Hop":                                  {"1"},
		"Set-Cookie":                             {"a=b"},
		"Content-Length":                         {"10"},
		"Content-Encoding":                       {"gzip"},
		"Transfer-Encoding":                      {"chunked"},
		"Retry-After":                            {"30"},
		"X-Should-Retry":                         {"false"},
		"Anthropic-Ratelimit-Requests-Remaining": {"5"},
		"X-Ratelimit-Remaining-Tokens":           {"100"},
		"Request-Id":                             {"req_1"},
		"Content-Type":                           {"text/event-stream"},
	}
	out := InboundResponseHeaders(up)
	for _, d := range []string{"Connection", "X-Hop", "Set-Cookie", "Content-Length", "Content-Encoding", "Transfer-Encoding"} {
		if out.Get(d) != "" {
			t.Errorf("%s kept", d)
		}
	}
	for _, k := range []string{"Retry-After", "X-Should-Retry", "Anthropic-Ratelimit-Requests-Remaining", "X-Ratelimit-Remaining-Tokens", "Request-Id", "Content-Type"} {
		if out.Get(k) != up.Get(k) {
			t.Errorf("%s not passed", k)
		}
	}
}

func allowedAddr(ip netip.Addr, allow []netip.Prefix) bool {
	return (&guard{allow: allow}).allowed(ip)
}

func TestGuardRanges(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fe80::1", "fd00:ec2::254", "::ffff:127.0.0.1"} {
		if allowedAddr(netip.MustParseAddr(s), nil) {
			t.Errorf("%s allowed", s)
		}
	}
	// IPv4 embedded in NAT64 and 6to4 addresses is checked like the IPv4.
	for _, s := range []string{"64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b::10.1.2.3", "2002:7f00:1::", "2002:c0a8:101::1", "2002:a9fe:a9fe:1::5"} {
		if allowedAddr(netip.MustParseAddr(s), nil) {
			t.Errorf("%s allowed", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111", "64:ff9b::808:808", "2002:808:808::1"} {
		if !allowedAddr(netip.MustParseAddr(s), nil) {
			t.Errorf("%s blocked", s)
		}
	}
	if !allowedAddr(netip.MustParseAddr("64:ff9b::a00:5"), []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}) {
		t.Error("allow CIDR ignored for a NAT64 address")
	}
	if !allowedAddr(netip.MustParseAddr("10.0.0.5"), []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}) {
		t.Error("allow CIDR ignored")
	}
}

func get(c *http.Client, u string) (*http.Response, error) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	return c.Do(req)
}

func TestSSRFDirect(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()

	c := New(Options{})
	hc, _ := c.HTTPClient("", false)
	_, err := get(hc, srv.URL)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("loopback must be blocked: %v", err)
	}
	// Hostname resolving to loopback is caught after resolution too.
	_, err = get(hc, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1))
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("localhost must be blocked: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("blocked request reached the server")
	}

	hc, _ = c.HTTPClient("", true)
	if resp, err := get(hc, srv.URL); err != nil {
		t.Fatalf("allow_private: %v", err)
	} else {
		resp.Body.Close()
	}
	c2 := New(Options{AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	hc, _ = c2.HTTPClient("", false)
	if resp, err := get(hc, srv.URL); err != nil {
		t.Fatalf("allow CIDR: %v", err)
	} else {
		resp.Body.Close()
	}
}

// Dial (SMTP and other non-HTTP traffic) has the same guard.
func TestDialGuarded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ctx := context.Background()
	if _, err := New(Options{}).Dial(ctx, "tcp", ln.Addr().String()); !errors.Is(err, ErrBlocked) {
		t.Fatalf("loopback must be blocked: %v", err)
	}
	conn, err := New(Options{AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}).Dial(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("allow CIDR: %v", err)
	}
	conn.Close()
}

func TestProxyFixedNoFallback(t *testing.T) {
	var direct atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { direct.Add(1) }))
	defer target.Close()

	// A dead proxy: grab a port and close it.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://user:secret@" + l.Addr().String()
	l.Close()

	c := New(Options{})
	hc, err := c.HTTPClient(dead, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = get(hc, target.URL)
	var pe *ProxyError
	if !errors.As(err, &pe) {
		t.Fatalf("want ProxyError, got %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("proxy password leaked into error")
	}
	if direct.Load() != 0 {
		t.Fatal("fell back to direct")
	}

	var via atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		via.Add(1)
		if r.Header.Get("X-Forwarded-For") != "" {
			t.Error("proxy headers added")
		}
		w.Header().Set("X-Via-Proxy", "1")
	}))
	defer proxy.Close()
	hc, _ = c.HTTPClient(proxy.URL, true)
	resp, err := get(hc, target.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if via.Load() != 1 || direct.Load() != 0 || resp.Header.Get("X-Via-Proxy") != "1" {
		t.Fatal("request did not go through the proxy")
	}

	// Through a proxy the target is still checked.
	hc, _ = c.HTTPClient(proxy.URL, false)
	if _, err := get(hc, target.URL); !errors.Is(err, ErrBlocked) {
		t.Fatalf("proxied private target: %v", err)
	}

	if _, err := c.HTTPClient("ftp://x", false); err == nil {
		t.Fatal("bad scheme accepted")
	}
	for _, s := range []string{"https://p:1", "socks5://p:1", "socks5h://p:1"} {
		if _, err := c.HTTPClient(s, false); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	a, _ := c.transport(proxy.URL, true)
	b, _ := c.transport(proxy.URL, true)
	if a != b {
		t.Fatal("transport not cached")
	}
}

func TestNoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
	}))
	defer srv.Close()
	hc, _ := New(Options{}).HTTPClient("", true)
	resp, err := get(hc, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("redirect followed: %d", resp.StatusCode)
	}
}

type slowBody struct {
	chunks chan string
}

func (s *slowBody) Read(p []byte) (int, error) {
	c, ok := <-s.chunks
	if !ok {
		return 0, io.EOF
	}
	return copy(p, c), nil
}
func (s *slowBody) Close() error { return nil }

// ctxBody blocks until its context ends, like a real response body.
type ctxBody struct{ ctx context.Context }

func (b ctxBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b ctxBody) Close() error             { return nil }

func TestTimers(t *testing.T) {
	w := Start(context.Background(), Timeouts{FirstByte: 20 * time.Millisecond})
	_, err := w.Body(ctxBody{w.Context()}).Read(make([]byte, 1))
	if !errors.Is(err, ErrFirstByteTimeout) {
		t.Fatalf("first byte: %v", err)
	}

	w = Start(context.Background(), Timeouts{FirstByte: 50 * time.Millisecond, Idle: 30 * time.Millisecond})
	sb := &slowBody{chunks: make(chan string, 4)}
	sb.chunks <- "a"
	body := w.Body(struct {
		io.Reader
		io.Closer
	}{io.MultiReader(sb, ctxBody{w.Context()}), sb})
	// Close sb channel after first chunk so MultiReader moves to the blocking body.
	close(sb.chunks)
	buf := make([]byte, 8)
	if n, err := body.Read(buf); n != 1 || err != nil {
		t.Fatalf("first read: %d %v", n, err)
	}
	time.Sleep(60 * time.Millisecond) // past first-byte: it must not fire after data arrived
	_, err = io.ReadAll(body)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("idle: %v", err)
	}
	body.Close()

	w = Start(context.Background(), Timeouts{Idle: time.Second, Total: 40 * time.Millisecond})
	_, err = w.Body(ctxBody{w.Context()}).Read(buf)
	if !errors.Is(err, ErrTotalTimeout) {
		t.Fatalf("total: %v", err)
	}
	if errors.Is(err, ErrIdleTimeout) {
		t.Fatal("kinds must be distinguishable")
	}

	w = Start(context.Background(), Timeouts{Total: time.Hour})
	if err := w.Err(context.Canceled); err != context.Canceled {
		t.Fatal("unrelated error rewritten")
	}
	w.Stop()
}

func TestRedact(t *testing.T) {
	u, _ := url.Parse(redact("socks5://a:b@h:1"))
	if u.User != nil {
		t.Fatal("redact")
	}
}

func TestConnectTimer(t *testing.T) {
	w := Start(context.Background(), Timeouts{Connect: 20 * time.Millisecond, FirstByte: time.Hour})
	<-w.Context().Done()
	if !errors.Is(w.Fired(), ErrConnectTimeout) {
		t.Fatalf("connect: %v", w.Fired())
	}
	w = Start(context.Background(), Timeouts{Connect: 20 * time.Millisecond})
	w.Connected()
	time.Sleep(50 * time.Millisecond)
	if w.Fired() != nil {
		t.Fatalf("fired after the connection was obtained: %v", w.Fired())
	}
	w.Stop()
}

func TestTimeoutsOverride(t *testing.T) {
	def := Timeouts{FirstByte: 2 * time.Minute, Idle: 5 * time.Minute, Total: 30 * time.Minute}
	got := def.Override(Timeouts{Connect: time.Second, Idle: time.Minute})
	want := Timeouts{Connect: time.Second, FirstByte: 2 * time.Minute, Idle: time.Minute, Total: 30 * time.Minute}
	if got != want {
		t.Fatalf("got %+v", got)
	}
	if def.Override(Timeouts{}) != def {
		t.Fatal("zero override changed the defaults")
	}
}

func TestSetExtraHeaders(t *testing.T) {
	h := http.Header{"X-Team": {"from-client"}}
	skipped := SetExtraHeaders(h, map[string]string{
		"x-team": "ops", "X-Extra": "1",
		"authorization": "Bearer stolen", "X-Api-Key": "k", "X-Forwarded-For": "1.2.3.4",
		"Connection": "close", "X-Owned": "mine",
	}, []string{"x-owned"})
	if h.Get("X-Team") != "ops" || h.Get("X-Extra") != "1" {
		t.Fatalf("extra headers not set: %v", h)
	}
	for _, n := range []string{"Authorization", "X-Api-Key", "X-Forwarded-For", "Connection", "X-Owned"} {
		if h.Get(n) != "" || !slices.Contains(skipped, n) {
			t.Errorf("%s must be skipped: %v / %v", n, h, skipped)
		}
	}
	if ExtraHeaderAllowed("Cookie") || ExtraHeaderAllowed("") || !ExtraHeaderAllowed("anthropic-beta") {
		t.Error("ExtraHeaderAllowed")
	}
}

func TestCheckProxy(t *testing.T) {
	for _, ok := range []string{"", "http://p:3128", "socks5h://u:p@p:1080"} {
		if err := CheckProxy(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"p:3128", "ftp://p", "http://"} {
		if CheckProxy(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Plugin-facing clients keep the fixed egress and add timers: Timed runs
// each request under its own, ForAttempt binds requests to the attempt's.
func TestPluginClients(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + l.Addr().String()
	l.Close()
	c := New(Options{})

	hc, err := c.Timed("", true, Timeouts{FirstByte: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := get(hc, slow.URL); !errors.Is(err, ErrFirstByteTimeout) {
		t.Fatalf("timed: %v", err)
	}
	hc, _ = c.Timed(dead, true, Timeouts{})
	var pe *ProxyError
	if _, err := get(hc, slow.URL); !errors.As(err, &pe) {
		t.Fatalf("timed dead proxy: %v", err)
	}

	w := Start(context.Background(), Timeouts{Total: 50 * time.Millisecond})
	defer w.Stop()
	hc, err = c.ForAttempt(&plugin.Credential{}, true, w)
	if err != nil {
		t.Fatal(err)
	}
	// The request carries its own context; the attempt's total timer still ends it.
	if _, err := get(hc, slow.URL); !errors.Is(err, ErrTotalTimeout) {
		t.Fatalf("attempt timer: %v", err)
	}
	hc, _ = c.ForAttempt(&plugin.Credential{Proxy: dead}, true, Start(context.Background(), Timeouts{}))
	if _, err := get(hc, slow.URL); !errors.As(err, &pe) {
		t.Fatalf("attempt dead proxy: %v", err)
	}
}
