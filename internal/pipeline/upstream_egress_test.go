package pipeline

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/pkg/plugin"
)

const slowChatReq = `{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// An upstream's own timeouts replace the relay defaults field by field.
func TestUpstreamTimeoutsOverrideDefaults(t *testing.T) {
	firstChunk := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"a\"},\"finish_reason\":null}]}\n\n"
	stallAfterFirst := func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, firstChunk)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
	// Accepts TCP but never answers the TLS handshake.
	blackhole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blackhole.Close() })
	go func() {
		for {
			c, err := blackhole.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()

	cases := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request, []byte)
		tls     bool
		over    egress.Timeouts
		want    string
	}{
		{"first byte", sseReply(3*time.Second, "data: [DONE]\n\n"), false, egress.Timeouts{FirstByte: 100 * time.Millisecond}, "first byte timeout"},
		{"idle", stallAfterFirst, false, egress.Timeouts{Idle: 100 * time.Millisecond}, "idle timeout"},
		{"total", stallAfterFirst, false, egress.Timeouts{Total: 150 * time.Millisecond}, "total timeout"},
		{"connect", nil, true, egress.Timeouts{Connect: 100 * time.Millisecond}, "connect timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.handler
			if h == nil {
				h = jsonReply(200, chatResp)
			}
			up := newFake(t, h)
			e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}},
				func(d *Deps) {
					d.Timeouts = egress.Timeouts{FirstByte: 5 * time.Second, Idle: 5 * time.Second, Total: 10 * time.Second}
					d.KeepaliveDelay = 5 * time.Second
				})
			u := e.snap.Upstreams["o"]
			u.Timeouts = tc.over
			if tc.tls {
				u.Config.Endpoints[0].BaseURL = "https://" + blackhole.Addr().String() + "/v1"
			}
			start := time.Now()
			_, body := e.call("openai-chat", "/v1/chat/completions", slowChatReq)
			if took := time.Since(start); took > 2*time.Second {
				t.Errorf("took %v: the upstream's timeout did not apply", took)
			}
			rec := e.waitRecords(1)
			if got := rec.Attempts[0].Error + rec.Error + string(body); !strings.Contains(got, tc.want) {
				t.Errorf("want %q in %q", tc.want, got)
			}
		})
	}
}

// forwardProxy is a plain HTTP forward proxy that counts requests.
func forwardProxy(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		out := r.Clone(r.Context())
		out.RequestURI = ""
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			w.Header()[k] = vs
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestUpstreamProxy(t *testing.T) {
	upProxy, viaUp := forwardProxy(t)
	credProxy, viaCred := forwardProxy(t)

	t.Run("upstream proxy for credentials without one", func(t *testing.T) {
		up := newFake(t, jsonReply(200, chatResp))
		e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}})
		e.snap.Upstreams["o"].Proxy = upProxy.URL
		if resp, body := e.call("openai-chat", "/v1/chat/completions", chatReq); resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
		if viaUp.Load() != 1 || up.calls() != 1 {
			t.Errorf("via upstream proxy %d, upstream calls %d", viaUp.Load(), up.calls())
		}
	})
	t.Run("credential proxy wins", func(t *testing.T) {
		before := viaUp.Load()
		up := newFake(t, jsonReply(200, chatResp))
		e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}})
		e.snap.Upstreams["o"].Proxy = upProxy.URL
		e.snap.Upstreams["o"].Credentials["c1"].Proxy = credProxy.URL
		if resp, body := e.call("openai-chat", "/v1/chat/completions", chatReq); resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
		if viaCred.Load() != 1 || viaUp.Load() != before {
			t.Errorf("credential proxy %d, upstream proxy %d", viaCred.Load(), viaUp.Load()-before)
		}
		if e.snap.Upstreams["o"].Credentials["c1"].Proxy != credProxy.URL {
			t.Error("snapshot credential mutated")
		}
	})
	t.Run("dead upstream proxy fails without direct fallback", func(t *testing.T) {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		dead := "http://" + l.Addr().String()
		l.Close()
		up := newFake(t, jsonReply(200, chatResp))
		e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}})
		e.snap.Upstreams["o"].Proxy = dead
		resp, _ := e.call("openai-chat", "/v1/chat/completions", chatReq)
		if resp.StatusCode < 500 || up.calls() != 0 {
			t.Errorf("status %d, direct calls %d", resp.StatusCode, up.calls())
		}
	})
}

// Extra headers arrive, replace a client's copy, and never override the
// provider's auth header or add dangerous headers.
func TestUpstreamExtraHeaders(t *testing.T) {
	up := newFake(t, jsonReply(200, chatResp))
	e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}})
	e.snap.Upstreams["o"].Headers = map[string]string{
		"X-Team": "ops", "x-extra": "1",
		"Authorization": "Bearer stolen", "X-Forwarded-For": "6.6.6.6", "Cookie": "a=b",
	}
	resp, body := e.call("openai-chat", "/v1/chat/completions", chatReq, "X-Team", "from-client")
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	h := up.headers[0]
	if h.Get("X-Team") != "ops" || h.Get("X-Extra") != "1" {
		t.Errorf("extra headers missing: %v", h)
	}
	if h.Get("Authorization") != "Bearer sk-o" {
		t.Errorf("provider-owned auth header overridden: %q", h.Get("Authorization"))
	}
	if h.Get("X-Forwarded-For") != "" || h.Get("Cookie") != "" {
		t.Errorf("dangerous header sent: %v", h)
	}
}

// tokenProvider fetches a token through Outgoing.Client in Prepare, like a
// provider exchanging a key for an access token.
type tokenProvider struct {
	plugin.Provider
	tokenURL string
}

func (p tokenProvider) Prepare(ctx context.Context, o *plugin.Outgoing) error {
	if o.Client == nil {
		return errors.New("no attempt client")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.tokenURL, nil)
	resp, err := o.Client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return p.Provider.Prepare(ctx, o)
}

// Calls a provider makes itself during an attempt take the attempt's
// egress: the upstream's proxy here, and a dead one fails the attempt
// without a direct try.
func TestProviderCallsUseAttemptEgress(t *testing.T) {
	var tokens atomic.Int32
	tok := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { tokens.Add(1) }))
	t.Cleanup(tok.Close)
	proxy, via := forwardProxy(t)
	run := func(proxyURL string) (int, *fakeUpstream) {
		up := newFake(t, jsonReply(200, chatResp))
		e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}})
		u := e.snap.Upstreams["o"]
		u.Provider = tokenProvider{Provider: u.Provider, tokenURL: tok.URL}
		u.Proxy = proxyURL
		resp, _ := e.call("openai-chat", "/v1/chat/completions", chatReq)
		return resp.StatusCode, up
	}
	if status, up := run(proxy.URL); status != 200 || tokens.Load() != 1 || via.Load() != 2 || up.calls() != 1 {
		t.Fatalf("status %d, tokens %d, proxied %d (token + request), upstream %d", status, tokens.Load(), via.Load(), up.calls())
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + l.Addr().String()
	l.Close()
	if status, up := run(dead); status < 500 || tokens.Load() != 1 || up.calls() != 0 {
		t.Fatalf("dead proxy: status %d, tokens %d, upstream %d", status, tokens.Load(), up.calls())
	}
}
