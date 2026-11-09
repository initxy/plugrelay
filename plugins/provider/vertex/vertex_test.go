package vertex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
	"github.com/initxy/plugrelay/plugins/provider/compatible"
	"github.com/initxy/plugrelay/plugins/provider/internal/cloudtest"
	"github.com/tidwall/gjson"
)

type fixture struct {
	p   *Provider
	ts  *cloudtest.TokenServer
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{ts: cloudtest.NewTokenServer(t), now: time.Now()}
	f.p = New(compatible.WithClock(func() time.Time { return f.now }), compatible.WithHTTPClient(http.DefaultClient))
	return f
}

func (f *fixture) upstream(manifest string, cfg ...string) *plugin.Upstream {
	c := map[string]any{"manifest": manifest}
	for i := 0; i+1 < len(cfg); i += 2 {
		c[cfg[i]] = cfg[i+1]
	}
	return &plugin.Upstream{ID: "vx-" + manifest, Provider: ID, Config: c}
}

func (f *fixture) cred(u *plugin.Upstream) *plugin.Credential {
	return &plugin.Credential{ID: "c1", UpstreamID: u.ID, Secret: f.ts.ServiceAccount}
}

func (f *fixture) prepare(t *testing.T, u *plugin.Upstream, c *plugin.Credential, model, path, body string) *http.Request {
	t.Helper()
	eps, err := f.p.Endpoints(u)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, eps[0].BaseURL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219,context-1m-2025-08-07")
	o := &plugin.Outgoing{Upstream: u, Endpoint: &eps[0], Credential: c, Model: model, Req: req}
	if err := f.p.Prepare(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	return o.Req
}

func TestEndpointsAndHosts(t *testing.T) {
	f := newFixture(t)
	eps, err := f.p.Endpoints(f.upstream("vertex-anthropic"))
	if err != nil {
		t.Fatal(err)
	}
	if eps[0].BaseURL != "https://aiplatform.googleapis.com" || eps[0].Issuer != "vertex-anthropic" {
		t.Errorf("global: %+v", eps[0])
	}
	eps, _ = f.p.Endpoints(f.upstream("vertex-gemini", "location", "us-central1"))
	if eps[0].BaseURL != "https://us-central1-aiplatform.googleapis.com" || eps[0].Issuer != "vertex-gemini" || eps[0].Protocol != "gemini" {
		t.Errorf("regional gemini: %+v", eps[0])
	}
}

func TestAnthropicRequests(t *testing.T) {
	f := newFixture(t)
	u := f.upstream("vertex-anthropic", "location", "us-east5")
	c := f.cred(u)
	model := "claude-sonnet-4-5@20250929"

	r := f.prepare(t, u, c, model, "/v1/messages", `{"model":"claude-sonnet-4-5","stream":true,"max_tokens":8,"messages":[]}`)
	if r.URL.Host != "us-east5-aiplatform.googleapis.com" ||
		r.URL.EscapedPath() != "/v1/projects/conformance-project/locations/us-east5/publishers/anthropic/models/claude-sonnet-4-5@20250929:streamRawPredict" {
		t.Errorf("stream URL %s", r.URL)
	}
	body, _ := io.ReadAll(r.Body)
	if gjson.GetBytes(body, "model").Exists() || !gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "anthropic_version").String() != AnthropicVersion {
		t.Errorf("body %s", body)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+f.ts.Issued()[0] {
		t.Errorf("Authorization %q", got)
	}
	if got := r.Header.Get("Anthropic-Beta"); got != "context-1m-2025-08-07" {
		t.Errorf("betas %q", got)
	}

	r = f.prepare(t, u, c, model, "/v1/messages", `{"model":"x","max_tokens":8,"messages":[]}`)
	if !strings.HasSuffix(r.URL.Path, "/models/claude-sonnet-4-5@20250929:rawPredict") {
		t.Errorf("invoke URL %s", r.URL)
	}

	r = f.prepare(t, u, c, model, "/v1/messages/count_tokens", `{"model":"x","messages":[]}`)
	body, _ = io.ReadAll(r.Body)
	if !strings.HasSuffix(r.URL.Path, "/publishers/anthropic/models/count-tokens:rawPredict") || gjson.GetBytes(body, "model").String() != model {
		t.Errorf("count tokens %s %s", r.URL, body)
	}
	if n := f.ts.Exchanges.Load(); n != 1 {
		t.Errorf("%d token exchanges for three requests, want 1", n)
	}

	// The upstream's project variable wins over the key file's.
	u2 := f.upstream("vertex-anthropic", "project", "other-proj")
	r = f.prepare(t, u2, &plugin.Credential{ID: "c1", UpstreamID: u2.ID, Secret: f.ts.ServiceAccount}, model, "/v1/messages", `{"max_tokens":1,"messages":[]}`)
	if !strings.HasPrefix(r.URL.Path, "/v1/projects/other-proj/locations/global/") {
		t.Errorf("project override: %s", r.URL)
	}
}

func TestGeminiRequests(t *testing.T) {
	f := newFixture(t)
	u := f.upstream("vertex-gemini", "project", "p1", "location", "europe-west4")
	r := f.prepare(t, u, f.cred(u), "gemini-2.5-pro", "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", `{"contents":[]}`)
	if r.URL.String() != "https://europe-west4-aiplatform.googleapis.com/v1/projects/p1/locations/europe-west4/publishers/google/models/gemini-2.5-pro:streamGenerateContent?alt=sse" {
		t.Errorf("URL %s", r.URL)
	}
	body, _ := io.ReadAll(r.Body)
	if string(body) != `{"contents":[]}` {
		t.Errorf("body changed: %s", body)
	}
	r = f.prepare(t, u, f.cred(u), "gemini-2.5-pro", "/v1beta/models/gemini-2.5-pro:countTokens", `{"contents":[]}`)
	if !strings.HasSuffix(r.URL.Path, "/publishers/google/models/gemini-2.5-pro:countTokens") {
		t.Errorf("countTokens URL %s", r.URL)
	}
}

func TestTokenLifecycle(t *testing.T) {
	f := newFixture(t)
	u := f.upstream("vertex-anthropic")
	c := f.cred(u)
	body := `{"max_tokens":1,"messages":[]}`
	auth := func() string {
		return f.prepare(t, u, c, "m", "/v1/messages", body).Header.Get("Authorization")
	}
	first := auth()
	f.now = f.now.Add(50 * time.Minute)
	if auth() != first {
		t.Error("token not reused while valid")
	}
	f.now = f.now.Add(6 * time.Minute) // within five minutes of expiry
	second := auth()
	if second == first || f.ts.Exchanges.Load() != 2 {
		t.Errorf("token not renewed before expiry (%d exchanges)", f.ts.Exchanges.Load())
	}

	// A 401 drops the token and asks the core to refresh.
	v := f.p.Classify(&plugin.Outcome{Credential: c, Status: 401, Sent: true,
		Body: []byte(`{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`)})
	if v.Cause != plugin.CauseAuthExpired || v.Credential != plugin.CredRefresh || v.Retry != plugin.RetryNext {
		t.Errorf("401 verdict %+v", v)
	}
	if third := auth(); third == second {
		t.Error("rejected token reused")
	}

	secret, exp, err := f.p.Refresh(context.Background(), u, c)
	if err != nil || secret != c.Secret || !exp.Equal(f.now.Add(time.Hour)) {
		t.Errorf("Refresh = %q… %v %v", secret[:10], exp, err)
	}

	// Replacing the key file never reuses the old key's token.
	n := f.ts.Exchanges.Load()
	var sa map[string]string
	_ = json.Unmarshal([]byte(c.Secret), &sa)
	sa["client_id"] = "2"
	b, _ := json.Marshal(sa)
	c.Secret = string(b)
	auth()
	if f.ts.Exchanges.Load() != n+1 {
		t.Error("new key file reused the old token")
	}
}

func TestTokenFailures(t *testing.T) {
	f := newFixture(t)
	u := f.upstream("vertex-anthropic")
	eps, _ := f.p.Endpoints(u)
	try := func(secret string) error {
		req, _ := http.NewRequest(http.MethodPost, eps[0].BaseURL+"/v1/messages", strings.NewReader(`{"max_tokens":1,"messages":[]}`))
		return f.p.Prepare(context.Background(), &plugin.Outgoing{Upstream: u, Endpoint: &eps[0], Model: "m", Req: req,
			Credential: &plugin.Credential{ID: "c", UpstreamID: u.ID, Secret: secret}})
	}
	f.ts.Fail("invalid_grant")
	err := try(f.ts.ServiceAccount)
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Errorf("refused grant: %v", err)
	}
	if err := try(`{"type":"authorized_user","client_id":"x"}`); err == nil {
		t.Error("user credentials accepted")
	}
	if err := try("ya29.static"); err == nil || !strings.Contains(err.Error(), "project") {
		t.Errorf("static token without a project: %v", err)
	}
	u.Config["project"] = "p1"
	if err := try("ya29.static"); err != nil {
		t.Errorf("static token: %v", err)
	}
	if _, _, err := f.p.Refresh(context.Background(), u, &plugin.Credential{ID: "c", Secret: "ya29.static"}); err == nil {
		t.Error("static token refreshed")
	}
}

// egressHost hands out the core's egress clients, proxies included.
type egressHost struct {
	*testkit.Host
	eg *egress.Client
}

func (h egressHost) ClientFor(_ *plugin.Upstream, c *plugin.Credential) (*http.Client, error) {
	return h.eg.Timed(c.Proxy, true, egress.Timeouts{Total: 10 * time.Second})
}

// The token exchange, in Prepare and in Refresh, takes the credential's
// egress: through its proxy, and a dead proxy fails with no direct try.
func TestTokenExchangeUsesCredentialEgress(t *testing.T) {
	ts := cloudtest.NewTokenServer(t)
	eg := egress.New(egress.Options{})
	p := New()
	if err := p.Init(context.Background(), egressHost{&testkit.Host{}, eg}); err != nil {
		t.Fatal(err)
	}
	u := &plugin.Upstream{ID: "vx", Provider: ID, Config: map[string]any{"manifest": "vertex-anthropic"}}
	eps, _ := p.Endpoints(u)
	prepare := func(c *plugin.Credential, withClient bool) error {
		req, _ := http.NewRequest(http.MethodPost, eps[0].BaseURL+"/v1/messages", strings.NewReader(`{"max_tokens":1,"messages":[]}`))
		o := &plugin.Outgoing{Upstream: u, Endpoint: &eps[0], Model: "m", Req: req, Credential: c}
		w := egress.Start(context.Background(), egress.Timeouts{Total: 10 * time.Second})
		defer w.Stop()
		if withClient {
			o.Client, _ = eg.ForAttempt(c, true, w)
		}
		return p.Prepare(w.Context(), o)
	}
	proxy, via := testkit.ForwardProxy(t)
	c := &plugin.Credential{ID: "c1", UpstreamID: u.ID, Secret: ts.ServiceAccount, Proxy: proxy.URL}
	if err := prepare(c, true); err != nil || via.Load() != 1 || ts.Exchanges.Load() != 1 {
		t.Fatalf("prepare via proxy: %v, proxied %d, exchanges %d", err, via.Load(), ts.Exchanges.Load())
	}
	if _, _, err := p.Refresh(context.Background(), u, c); err != nil || via.Load() != 2 || ts.Exchanges.Load() != 2 {
		t.Fatalf("refresh via proxy: %v, proxied %d, exchanges %d", err, via.Load(), ts.Exchanges.Load())
	}
	dead := &plugin.Credential{ID: "c2", UpstreamID: u.ID, Secret: ts.ServiceAccount, Proxy: testkit.DeadProxy(t)}
	if err := prepare(dead, true); err == nil || ts.Exchanges.Load() != 2 {
		t.Fatalf("dead proxy in prepare: %v, exchanges %d", err, ts.Exchanges.Load())
	}
	if _, _, err := p.Refresh(context.Background(), u, dead); err == nil || ts.Exchanges.Load() != 2 {
		t.Fatalf("dead proxy in refresh: %v, exchanges %d", err, ts.Exchanges.Load())
	}
	other := &plugin.Credential{ID: "c3", UpstreamID: u.ID, Secret: ts.ServiceAccount}
	if err := prepare(other, false); err == nil || ts.Exchanges.Load() != 2 {
		t.Fatalf("no attempt client: %v, exchanges %d", err, ts.Exchanges.Load())
	}
}
