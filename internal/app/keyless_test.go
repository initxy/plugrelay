package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// keyOptionalChat is chatOnlyManifest of a server that may run without a key.
func keyOptionalChat() map[string]any {
	m := chatOnlyManifest()
	m["auth"] = map[string]any{"header": "Authorization", "scheme": "Bearer", "key_optional": true}
	return m
}

// Only an upstream whose manifest marks the key optional (auth.key_optional:
// a self-hosted server, the LiteLLM gateway) with no credentials is routed
// keyless, as `plugrelay upstream add` without --credential-env leaves it.
// A cloud upstream whose last key is deleted has no route, and an
// unopenable credential is not mistaken for keyless.
func TestKeylessUpstreamRoutes(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"up-model","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()
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
	for id, conf := range map[string]map[string]any{
		"local":   {"manifest": keyOptionalChat(), "base_url": srv.URL + "/v1"},
		"cloud":   {"manifest": chatOnlyManifest(), "base_url": srv.URL + "/v1"},
		"litellm": {"manifest": "litellm", "server_url": srv.URL},
	} {
		conf[configAllowPrivate] = true
		must(st.PutUpstream(ctx, &store.Upstream{ID: id, Provider: "provider.openai-compatible", Enabled: true, Config: conf}))
		must(st.PutModel(ctx, &store.Model{Name: id, Enabled: true}))
		must(st.ReplaceRoutes(ctx, id, []store.Route{{Model: id, UpstreamID: id, UpstreamModel: "up-model", Weight: 1}}))
	}
	must(st.PutCredential(ctx, &store.Credential{ID: "ck", UpstreamID: "cloud", Kind: store.CredShared, Secret: "sk-cloud"}))
	must(a.Reload(ctx))
	call := func(model string) admin.TestResult {
		res, _ := send(ctx, a.pipe, "openai-chat", model, &plugin.Caller{KeyName: "t"})
		return res
	}

	if res := call("local"); !res.OK || calls.Load() != 1 {
		t.Fatalf("self-hosted upstream without credentials not served: %+v", res)
	}
	if got := auth.Load().(string); got != "" {
		t.Fatalf("keyless upstream sent auth %q", got)
	}
	if res := call("cloud"); !res.OK || calls.Load() != 2 || auth.Load().(string) != "Bearer sk-cloud" {
		t.Fatalf("cloud upstream with its key: %+v auth %q", res, auth.Load())
	}
	if res := call("litellm"); !res.OK || calls.Load() != 3 || auth.Load().(string) != "" {
		t.Fatalf("LiteLLM gateway without credentials not served keyless: %+v auth %q", res, auth.Load())
	}

	// The cloud upstream's last key is deleted: no route, nothing sent.
	must(st.DeleteCredential(ctx, "ck"))
	must(a.Reload(ctx))
	if res := call("cloud"); res.OK || calls.Load() != 3 {
		t.Fatalf("cloud upstream without credentials was routed keyless: %+v", res)
	}
	if res := a.testUpstream(ctx, "cloud", admin.TestRequest{Model: "up-model"}); len(res) != 1 || res[0].OK || calls.Load() != 3 {
		t.Fatalf("tester probed a cloud upstream without credentials: %+v", res)
	}

	// A credential sealed under another master key cannot be opened: it
	// stays in the table switched off, and nothing is sent without its key.
	must(st.PutCredential(ctx, &store.Credential{ID: "k", UpstreamID: "local", Kind: store.CredShared, Secret: "sk-x"}))
	other := make([]byte, secret.MasterKeySize)
	other[0] = 1
	if a.box, err = secret.NewBox(other); err != nil {
		t.Fatal(err)
	}
	must(a.Reload(ctx))
	tu := a.runtime().snap.Table.Upstreams["local"]
	if len(tu.Credentials) != 1 || tu.Credentials[0].Status != router.StatusDisabled {
		t.Fatalf("unopenable credential: %+v", tu.Credentials)
	}
	if res := call("local"); res.OK || calls.Load() != 3 {
		t.Fatalf("an upstream with an unopenable credential was routed keyless: %+v", res)
	}
}

// Model sync lists a keyless upstream with no auth but never calls an
// upstream that needs a key and has no usable credential.
func TestModelSyncSkipsUpstreamWithoutKey(t *testing.T) {
	ctx := context.Background()
	var listed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		listed.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"m1","object":"model"}]}`)
	}))
	defer srv.Close()
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeAll)
	for id, man := range map[string]map[string]any{"local": keyOptionalChat(), "cloud": chatOnlyManifest()} {
		man["models"] = map[string]any{"list": "/models"}
		mustOK(t, a.Store().PutUpstream(ctx, &store.Upstream{ID: id, Provider: "provider.openai-compatible", Enabled: true,
			Config: map[string]any{"manifest": man, "base_url": srv.URL + "/v1", configAllowPrivate: true}}))
	}
	mustOK(t, a.Reload(ctx))

	ts, err := targets{a: a}.SyncTargets(ctx)
	mustOK(t, err)
	if len(ts) != 1 || ts[0].Upstream.ID != "local" {
		t.Fatalf("sync targets %+v, want only the keyless upstream", ts)
	}
	if _, err := (runtimeAPI{a}).RemoteModels(ctx, "cloud"); !errors.Is(err, errNoKey) {
		t.Fatalf("listing a cloud upstream without a key: %v", err)
	}
	res := runtimeAPI{a}.SyncModels(ctx, []string{"cloud"})
	if len(res.Failed) != 1 || res.Failed[0].UpstreamID != "cloud" || listed.Load() != 0 {
		t.Fatalf("syncing a cloud upstream without a key: %+v, %d listings", res, listed.Load())
	}
	if ms, err := (runtimeAPI{a}).RemoteModels(ctx, "local"); err != nil || len(ms) != 1 || listed.Load() != 1 {
		t.Fatalf("keyless listing: %v %v", ms, err)
	}
}
