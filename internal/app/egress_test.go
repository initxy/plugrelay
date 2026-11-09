package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

func countingProxy(t *testing.T) (*httptest.Server, *atomic.Int32) {
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
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// Model sync (remote listing) goes through the credential's proxy, else the
// upstream's; a dead proxy fails the sync with no direct try (hard rule 15).
// A plugin passing a credential without its proxy still gets the proxy.
func TestBackgroundCallsHonourProxy(t *testing.T) {
	ctx := context.Background()
	var direct atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		direct.Add(1)
		_, _ = io.WriteString(w, `{"data":[{"id":"m1"}]}`)
	}))
	t.Cleanup(up.Close)
	upProxy, viaUp := countingProxy(t)
	credProxy, viaCred := countingProxy(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + l.Addr().String()
	l.Close()

	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeAll)
	st := a.Store()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	putUpstream := func(proxy string) {
		must(st.PutUpstream(ctx, &store.Upstream{ID: "u", Provider: "provider.openai-compatible", Enabled: true,
			Config: map[string]any{"manifest": "vllm", "base_url": up.URL + "/v1", configAllowPrivate: true, "_proxy": proxy}}))
	}
	putCred := func(proxy string) {
		must(st.PutCredential(ctx, &store.Credential{ID: "c1", UpstreamID: "u", Kind: store.CredShared, Secret: "sk",
			Status: store.CredActive, Proxy: proxy}))
	}
	putUpstream(upProxy.URL)
	putCred("")
	must(a.Reload(ctx))
	api := runtimeAPI{a}
	if ms, err := api.RemoteModels(ctx, "u"); err != nil || len(ms) != 1 || viaUp.Load() != 1 || direct.Load() != 1 {
		t.Fatalf("upstream proxy: %v %v, proxied %d, upstream %d", ms, err, viaUp.Load(), direct.Load())
	}

	putCred(credProxy.URL)
	must(a.Reload(ctx))
	if res := api.SyncModels(ctx, []string{"u"}); len(res.Failed) != 0 || viaCred.Load() != 1 || viaUp.Load() != 1 {
		t.Fatalf("credential proxy: %+v, credential proxy %d, upstream proxy %d", res, viaCred.Load(), viaUp.Load())
	}
	// A plugin cannot drop the proxy by passing a bare credential.
	hc, err := a.clientFor(&plugin.Upstream{ID: "u"}, &plugin.Credential{ID: "c1"})
	must(err)
	resp, err := hc.Get(up.URL + "/v1/models")
	must(err)
	resp.Body.Close()
	if viaCred.Load() != 2 {
		t.Fatalf("bare credential bypassed its proxy (%d)", viaCred.Load())
	}
	if _, err := a.clientFor(&plugin.Upstream{ID: "nope"}, nil); err == nil {
		t.Fatal("unknown upstream got a client")
	}

	putCred(dead)
	must(a.Reload(ctx))
	before := direct.Load()
	_, err = api.RemoteModels(ctx, "u")
	var pe *egress.ProxyError
	if !errors.As(err, &pe) || direct.Load() != before {
		t.Fatalf("dead proxy: %v, upstream %d → %d", err, before, direct.Load())
	}
}
