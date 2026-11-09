package compatible

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
	"github.com/initxy/plugrelay/plugins/provider/manifest"
)

// proxyHost resolves a credential's egress like the core: its own proxy,
// else the upstream's.
type proxyHost struct {
	*testkit.Host
	eg            *egress.Client
	upstreamProxy string
	asked         atomic.Int32
}

func (h *proxyHost) ClientFor(_ *plugin.Upstream, c *plugin.Credential) (*http.Client, error) {
	h.asked.Add(1)
	proxy := h.upstreamProxy
	if c != nil && c.Proxy != "" {
		proxy = c.Proxy
	}
	return h.eg.Timed(proxy, true, egress.Timeouts{Total: 10 * time.Second})
}

// Model listing and quota probes go out through Host.ClientFor: via the
// credential's or the upstream's proxy, failing when it is dead.
func TestBackgroundCallsUseCredentialEgress(t *testing.T) {
	var direct atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		direct.Add(1)
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"a"}]}`))
		case "/v1/quota":
			_, _ = w.Write([]byte(`{"left":0.5}`))
		}
	}))
	defer srv.Close()
	m, err := manifest.Parse([]byte(`
id: l
kind: provider
interpreter: openai-compatible
variables: {base: {default: "` + srv.URL + `"}}
auth: {header: Authorization, scheme: Bearer}
endpoints: [{protocol: openai-chat, base_url: "{base}"}]
models: {list: /v1/models}
quota: {path: /v1/quota, remaining_fraction: left}
`))
	if err != nil {
		t.Fatal(err)
	}
	credProxy, viaCred := testkit.ForwardProxy(t)
	upProxy, viaUp := testkit.ForwardProxy(t)
	h := &proxyHost{Host: &testkit.Host{}, eg: egress.New(egress.Options{}), upstreamProxy: upProxy.URL}
	p := New("openai-compatible", WithIndex(NewIndex(m)))
	if err := p.Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	u := &plugin.Upstream{ID: "l", Config: map[string]any{"manifest": "l"}}
	ctx := context.Background()

	c := &plugin.Credential{ID: "c1", UpstreamID: "l", Secret: "k", Proxy: credProxy.URL}
	if got, err := p.ListModels(ctx, u, c); err != nil || len(got) != 1 {
		t.Fatalf("list: %v %v", got, err)
	}
	if q, err := p.Quota(ctx, u, c); err != nil || q.Remaining != 0.5 {
		t.Fatalf("quota: %+v %v", q, err)
	}
	if viaCred.Load() != 2 || viaUp.Load() != 0 || direct.Load() != 2 {
		t.Fatalf("credential proxy %d, upstream proxy %d, upstream hits %d", viaCred.Load(), viaUp.Load(), direct.Load())
	}
	if _, err := p.ListModels(ctx, u, &plugin.Credential{ID: "c2", UpstreamID: "l", Secret: "k"}); err != nil || viaUp.Load() != 1 {
		t.Fatalf("upstream proxy: %v, proxied %d", err, viaUp.Load())
	}

	dead := &plugin.Credential{ID: "c3", UpstreamID: "l", Secret: "k", Proxy: testkit.DeadProxy(t)}
	before := direct.Load()
	_, err = p.ListModels(ctx, u, dead)
	var pe *egress.ProxyError
	if !errors.As(err, &pe) || direct.Load() != before {
		t.Fatalf("dead proxy: %v, upstream hits %d → %d (no direct fallback)", err, before, direct.Load())
	}
	if _, err := p.Quota(ctx, u, dead); !errors.As(err, &pe) || direct.Load() != before {
		t.Fatalf("dead proxy quota: %v", err)
	}
}

// A credential sent as a query parameter (auth.query_param) never shows up
// in the error of a failed background call, which reaches the console
// (model sync failures, quota probe errors).
func TestBackgroundCallErrorHidesQueryCredential(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close() // connection refused from here on
	m, err := manifest.Parse([]byte(`
id: q
kind: provider
interpreter: openai-compatible
variables: {base: {default: "` + dead + `"}}
auth: {query_param: key}
endpoints: [{protocol: openai-chat, base_url: "{base}"}]
models: {list: /v1/models}
`))
	if err != nil {
		t.Fatal(err)
	}
	h := &proxyHost{Host: &testkit.Host{}, eg: egress.New(egress.Options{})}
	p := New("openai-compatible", WithIndex(NewIndex(m)))
	if err := p.Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	u := &plugin.Upstream{ID: "q", Config: map[string]any{"manifest": "q"}}
	_, err = p.ListModels(context.Background(), u, &plugin.Credential{ID: "c", UpstreamID: "q", Secret: "SECRETKEY123"})
	if err == nil || strings.Contains(err.Error(), "SECRETKEY123") {
		t.Fatalf("error %v exposes the credential", err)
	}
}
