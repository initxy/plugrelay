package compatible

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/initxy/plugrelay/manifests"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/provider/manifest"
)

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func clock() time.Time { return t0 }

func TestOfficialManifestsValidate(t *testing.T) {
	x, err := LoadDir(manifests.FS)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := fs.Glob(manifests.FS, "*.yaml")
	if len(x.IDs()) != len(files) {
		t.Errorf("%d manifests from %d files", len(x.IDs()), len(files))
	}
	for _, id := range []string{"openai", "azure-openai", "anthropic", "gemini", "deepseek", "moonshot-kimi",
		"zhipu-glm", "minimax", "qwen-dashscope", "volcengine-ark", "openrouter", "groq", "together",
		"mistral", "xai", "vllm", "ollama", "lmstudio"} {
		m, ok := x.Get(id)
		if !ok {
			t.Errorf("missing official manifest %s", id)
			continue
		}
		if _, ok := plugin.Lookup(m.ProviderID()); !ok {
			for _, e := range m.Endpoints {
				if _, ok := plugin.Lookup("provider." + m.EndpointInterpreter(&e)); !ok {
					t.Errorf("%s: interpreter of %s not registered", id, e.ID)
				}
			}
		}
		// Every official manifest yields endpoints with only defaults.
		p := New("openai-compatible", WithIndex(x))
		cfg := map[string]any{"manifest": id}
		if id == "azure-openai" {
			cfg["resource"] = "res"
		}
		if _, err := p.Endpoints(&plugin.Upstream{ID: id, Config: cfg}); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
	if Default().IDs()[0] == "" {
		t.Error("default index empty")
	}
}

// The console groups official manifests by category and shows variable
// help in both of its languages.
func TestOfficialManifestsConsoleFields(t *testing.T) {
	x, err := LoadDir(manifests.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range x.IDs() {
		m, _ := x.Get(id)
		if m.Category == "" {
			t.Errorf("%s: no category", id)
		}
		for name, v := range m.Variables {
			if (v.Description == "") != (v.DescriptionZh == "") {
				t.Errorf("%s: variable %s needs description and description_zh together", id, name)
			}
		}
	}
	bad := &manifest.Manifest{ID: "x", Kind: manifest.Kind, Category: "other",
		Auth:      manifest.Auth{Header: "Authorization"},
		Endpoints: []manifest.Endpoint{{Protocol: "openai-chat", BaseURL: "https://x.example", Interpreter: "openai-compatible"}}}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "category") {
		t.Errorf("unknown category accepted: %v", err)
	}
}

// Official manifests leave out what the loader defaults: an endpoint id
// equal to its protocol, and models.endpoint naming the first endpoint.
func TestOfficialManifestsOmitDefaults(t *testing.T) {
	files, _ := fs.Glob(manifests.FS, "*.yaml")
	for _, f := range files {
		raw, _ := fs.ReadFile(manifests.FS, f)
		m, err := manifest.Decode(raw)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		for _, e := range m.Endpoints {
			if e.ID != "" && e.ID == e.Protocol {
				t.Errorf("%s: endpoint id %q is its protocol, the default", f, e.ID)
			}
		}
		if len(m.Endpoints) > 0 && m.Models.Endpoint != "" {
			first := m.Endpoints[0].ID
			if first == "" {
				first = m.Endpoints[0].Protocol
			}
			if m.Models.Endpoint == first {
				t.Errorf("%s: models.endpoint %q is the first endpoint, the default", f, first)
			}
		}
	}
}

func TestRegistrations(t *testing.T) {
	for _, id := range []string{IDOpenAICompatible, IDAnthropicCompatible, IDGemini, IDOpenAI, IDAnthropic} {
		f, ok := plugin.Lookup(id)
		if !ok {
			t.Fatalf("%s not registered", id)
		}
		pl := f()
		if pl.Info().ID != id || pl.Info().Kind != plugin.KindProvider {
			t.Errorf("%s: info %+v", id, pl.Info())
		}
		if _, ok := pl.(plugin.Provider); !ok {
			t.Errorf("%s is not a Provider", id)
		}
		if _, ok := pl.(plugin.ModelLister); !ok {
			t.Errorf("%s is not a ModelLister", id)
		}
		if _, ok := pl.(plugin.Refresher); ok {
			t.Errorf("%s claims to refresh credentials", id)
		}
	}
}

func TestEndpointsAzure(t *testing.T) {
	p := New("openai", WithDefaultManifest("openai"), WithIssuers(OpenAIIssuers...))
	u := &plugin.Upstream{ID: "az", Config: map[string]any{"manifest": "azure-openai", "resource": "acme"}}
	eps, err := p.Endpoints(u)
	if err != nil {
		t.Fatal(err)
	}
	chat := eps[0]
	if chat.BaseURL != "https://acme.openai.azure.com" || chat.Path != "/openai/deployments/{model}/chat/completions" ||
		chat.Query["api-version"] != "2024-10-21" || chat.Issuer != "azure-openai" {
		t.Errorf("chat endpoint = %+v", chat)
	}
	if got := p.OwnedHeaders(u); !contains(got, "Api-Key") || !contains(got, "Authorization") {
		t.Errorf("owned = %v", got)
	}
	u.Config["api_version"] = "2025-04-01-preview"
	eps, _ = p.Endpoints(u)
	if eps[0].Query["api-version"] != "2025-04-01-preview" {
		t.Errorf("api_version override ignored: %v", eps[0].Query)
	}
	delete(u.Config, "resource")
	if _, err := p.Endpoints(u); err == nil || !strings.Contains(err.Error(), "config.resource") {
		t.Errorf("missing resource: %v", err)
	}
	u.Config["resource"] = "acme"
	u.Config["issuer"] = "anthropic"
	if _, err := p.Endpoints(u); err == nil {
		t.Error("foreign issuer accepted")
	}
	// Default manifest when the config names none.
	eps, err = p.Endpoints(&plugin.Upstream{ID: "oa"})
	if err != nil || eps[0].BaseURL != "https://api.openai.com/v1" || eps[0].Issuer != "openai" {
		t.Errorf("default manifest: %+v %v", eps, err)
	}
}

func TestEndpointsCapsAndBetas(t *testing.T) {
	p := New("anthropic-compatible")
	eps, err := p.Endpoints(&plugin.Upstream{ID: "ds", Config: map[string]any{"manifest": "deepseek"}})
	if err != nil {
		t.Fatal(err)
	}
	if eps[0].Caps.Cache != ir.CacheImplicit || eps[0].Caps.ReasoningStyle != "reasoning_content" {
		t.Errorf("chat caps = %+v", eps[0].Caps)
	}
	if eps[1].Protocol != "anthropic" || eps[1].DropBetas[0] != "context-1m-2025-08-07" || eps[1].Issuer != "deepseek" {
		t.Errorf("anthropic endpoint = %+v", eps[1])
	}
	vl, err := p.Endpoints(&plugin.Upstream{ID: "v", Config: map[string]any{"manifest": "vllm", "base_url": "http://gpu:9000/v1"}})
	if err != nil || vl[0].BaseURL != "http://gpu:9000/v1" {
		t.Errorf("vllm: %+v %v", vl, err)
	}
	if _, err := p.Endpoints(&plugin.Upstream{ID: "v", Config: map[string]any{"manifest": "vllm", "base_url": "file:///etc"}}); err == nil {
		t.Error("non-http base URL accepted")
	}
	if _, err := p.Endpoints(&plugin.Upstream{ID: "x", Config: map[string]any{"manifest": "nope"}}); err == nil {
		t.Error("unknown manifest accepted")
	}
}

func TestInlineManifestConfig(t *testing.T) {
	p := New("openai-compatible")
	cfg := map[string]any{"manifest": map[string]any{
		"id": "inline", "kind": "provider", "interpreter": "openai-compatible",
		"auth":      map[string]any{"header": "Authorization"},
		"endpoints": []any{map[string]any{"protocol": "openai-chat", "base_url": "https://llm.example.org/v1"}},
	}}
	eps, err := p.Endpoints(&plugin.Upstream{ID: "i", Config: cfg})
	if err != nil || eps[0].ID != "openai-chat" {
		t.Fatalf("%+v %v", eps, err)
	}
}

func outgoing(t *testing.T, p *Provider, u *plugin.Upstream, epID string, secret string, hdr http.Header) *plugin.Outgoing {
	t.Helper()
	eps, err := p.Endpoints(u)
	if err != nil {
		t.Fatal(err)
	}
	var ep *plugin.Endpoint
	for i := range eps {
		if eps[i].ID == epID {
			ep = &eps[i]
		}
	}
	if ep == nil {
		t.Fatalf("no endpoint %s", epID)
	}
	req := httptest.NewRequest(http.MethodPost, ep.BaseURL+"/v1/messages", nil)
	req.Header = hdr
	return &plugin.Outgoing{Upstream: u, Endpoint: ep, Credential: &plugin.Credential{ID: "c1", UpstreamID: u.ID, Secret: secret,
		Meta: map[string]string{"organization": "org-mine"}}, Model: "m", Req: req}
}

// Hard rule 14 at the provider: owned headers carry only provider values.
func TestPrepareOwnsHeaders(t *testing.T) {
	p := New("anthropic")
	u := &plugin.Upstream{ID: "an", Config: map[string]any{"manifest": "anthropic"}}
	o := outgoing(t, p, u, "messages", "sk-relay", http.Header{
		"Authorization":  {"Bearer client-token"},
		"X-Api-Key":      {"client-key"},
		"Anthropic-Beta": {"context-1m-2025-08-07, files-api-2025-04-14"},
		"User-Agent":     {"claude-cli/2"},
	})
	if err := p.Prepare(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	h := o.Req.Header
	if h.Get("Authorization") != "" || h.Get("X-Api-Key") != "sk-relay" || len(h.Values("X-Api-Key")) != 1 {
		t.Errorf("auth headers = %v", h)
	}
	if h.Get("Anthropic-Version") != "2023-06-01" || h.Get("User-Agent") != "claude-cli/2" {
		t.Errorf("headers = %v", h)
	}
	if h.Get("Anthropic-Beta") != "context-1m-2025-08-07, files-api-2025-04-14" {
		t.Errorf("betas changed without unsupported_betas: %q", h.Get("Anthropic-Beta"))
	}

	// Client's anthropic-version is kept; unsupported betas are dropped
	// across repeated header lines.
	ds := &plugin.Upstream{ID: "ds", Config: map[string]any{"manifest": "deepseek"}}
	o = outgoing(t, p, ds, "anthropic", "sk-ds", http.Header{
		"Authorization":     {"Bearer client"},
		"Anthropic-Version": {"2024-01-01"},
		"Anthropic-Beta":    {"context-1m-2025-08-07,interleaved-thinking-2025-05-14", "context-1m-2025-08-07"},
	})
	if err := p.Prepare(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	h = o.Req.Header
	if h.Get("Authorization") != "Bearer sk-ds" || len(h.Values("Authorization")) != 1 {
		t.Errorf("authorization = %v", h.Values("Authorization"))
	}
	if h.Get("Anthropic-Version") != "2024-01-01" {
		t.Errorf("client version overwritten: %q", h.Get("Anthropic-Version"))
	}
	if got := h.Values("Anthropic-Beta"); len(got) != 1 || got[0] != "interleaved-thinking-2025-05-14" {
		t.Errorf("betas = %v", got)
	}
	o = outgoing(t, p, ds, "anthropic", "sk-ds", http.Header{"Anthropic-Beta": {"context-1m-2025-08-07"}})
	_ = p.Prepare(context.Background(), o)
	if _, ok := o.Req.Header["Anthropic-Beta"]; ok {
		t.Error("empty beta header left behind")
	}

	// openai-chat endpoint gets no anthropic headers.
	o = outgoing(t, p, ds, "chat", "sk-ds", http.Header{})
	_ = p.Prepare(context.Background(), o)
	if o.Req.Header.Get("Anthropic-Version") != "" {
		t.Error("anthropic-version on an openai-chat endpoint")
	}
}

func TestPrepareCredentialAndQuery(t *testing.T) {
	p := New("openai")
	u := &plugin.Upstream{ID: "oa", Config: map[string]any{"manifest": "openai"}}
	o := outgoing(t, p, u, "chat", "sk", http.Header{"Openai-Organization": {"org-victim"}, "Openai-Project": {"proj-victim"}})
	_ = p.Prepare(context.Background(), o)
	if o.Req.Header.Get("OpenAI-Organization") != "org-mine" || o.Req.Header.Get("OpenAI-Project") != "" {
		t.Errorf("org headers = %v", o.Req.Header)
	}

	az := &plugin.Upstream{ID: "az", Config: map[string]any{"manifest": "azure-openai", "resource": "r"}}
	o = outgoing(t, p, az, "chat", "azkey", http.Header{"Authorization": {"Bearer x"}})
	_ = p.Prepare(context.Background(), o)
	if o.Req.Header.Get("Api-Key") != "azkey" || o.Req.Header.Get("Authorization") != "" ||
		o.Req.URL.Query().Get("api-version") != "2024-10-21" {
		t.Errorf("azure request = %v %v", o.Req.Header, o.Req.URL)
	}

	// Query-param auth replaces any client value; empty secrets send nothing.
	m, err := manifest.Parse([]byte(`
id: q
kind: provider
interpreter: gemini
auth: {query_param: key}
extra_headers: {X-Goog-Api-Client: relay}
endpoints: [{protocol: gemini, base_url: "https://g.example.com"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	gp := New("gemini", WithIndex(NewIndex(m)))
	qu := &plugin.Upstream{ID: "q", Config: map[string]any{"manifest": "q"}}
	o = outgoing(t, gp, qu, "gemini", "gk", http.Header{"X-Goog-Api-Client": {"spoof"}})
	o.Req.URL.RawQuery = "key=client&alt=sse"
	_ = gp.Prepare(context.Background(), o)
	if q := o.Req.URL.Query(); q.Get("key") != "gk" || q.Get("alt") != "sse" || len(q["key"]) != 1 {
		t.Errorf("query = %v", q)
	}
	if o.Req.Header.Get("X-Goog-Api-Client") != "relay" {
		t.Errorf("extra header = %q", o.Req.Header.Get("X-Goog-Api-Client"))
	}

	vl := &plugin.Upstream{ID: "v", Config: map[string]any{"manifest": "vllm"}}
	o = outgoing(t, p, vl, "chat", "", http.Header{"Authorization": {"Bearer client"}})
	_ = p.Prepare(context.Background(), o)
	if o.Req.Header.Get("Authorization") != "" {
		t.Error("client Authorization survived an empty secret")
	}
}

// ---------------------------------------------------------------- classify

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

func body(msg string) []byte { return []byte(`{"error":{"message":"` + msg + `","type":"x"}}`) }

func TestClassifyDefaults(t *testing.T) {
	p := New("openai-compatible", WithClock(clock))
	cred := &plugin.Credential{ID: "c", UpstreamID: "unbound"}
	cases := []struct {
		name  string
		o     plugin.Outcome
		cause plugin.Cause
		retry plugin.RetryMode
		cred  plugin.CredAction
		until time.Duration
		kind  ir.ErrorKind
		scope bool
	}{
		{"dial failed", plugin.Outcome{Err: errors.New("connection refused")}, plugin.CauseTransient, plugin.RetryNext, plugin.CredKeep, 0, ir.ErrUpstream, false},
		{"timeout after send", plugin.Outcome{Err: timeoutErr{}, Sent: true}, plugin.CauseTransient, plugin.RetryNext, plugin.CredKeep, 0, ir.ErrTimeout, false},
		{"deadline after send", plugin.Outcome{Err: context.DeadlineExceeded, Sent: true}, plugin.CauseTransient, plugin.RetryNext, plugin.CredKeep, 0, ir.ErrTimeout, false},
		{"caller canceled", plugin.Outcome{Err: context.Canceled, Sent: true}, plugin.CauseTransient, plugin.RetryNone, plugin.CredKeep, 0, ir.ErrUpstream, false},
		{"400 context signal", plugin.Outcome{Status: 400, Signal: &ir.GwError{Kind: ir.ErrContextTooLong, Message: "prompt is too long: 210000 tokens > 200000 maximum"}},
			plugin.CauseRequest, plugin.RetryNone, plugin.CredKeep, 0, ir.ErrContextTooLong, false},
		{"400 context wording", plugin.Outcome{Status: 400, Body: body("This model's maximum context length is 128000 tokens")},
			plugin.CauseRequest, plugin.RetryNone, plugin.CredKeep, 0, ir.ErrContextTooLong, false},
		{"400 invalid", plugin.Outcome{Status: 400, Body: body("tools.0.name: invalid")}, plugin.CauseRequest, plugin.RetryNone, plugin.CredKeep, 0, ir.ErrInvalidRequest, false},
		{"400 org disabled", plugin.Outcome{Status: 400, Body: body("This organization has been disabled.")}, plugin.CauseBanned, plugin.RetryNone, plugin.CredQuarantine, 0, ir.ErrPermission, false},
		{"401", plugin.Outcome{Status: 401, Body: body("invalid x-api-key")}, plugin.CauseAuthExpired, plugin.RetryNext, plugin.CredCooldown, 10 * time.Minute, ir.ErrUpstream, false},
		{"401 deactivated", plugin.Outcome{Status: 401, Body: body("Your account has been deactivated")}, plugin.CauseBanned, plugin.RetryNone, plugin.CredQuarantine, 0, ir.ErrPermission, false},
		{"403 suspended", plugin.Outcome{Status: 403, Body: body("Your account is suspended")}, plugin.CauseBanned, plugin.RetryNone, plugin.CredQuarantine, 0, ir.ErrPermission, false},
		{"403 uncertain", plugin.Outcome{Status: 403, Body: body("Request not allowed")}, plugin.CauseUnknown, plugin.RetryNone, plugin.CredQuarantine, 0, ir.ErrPermission, false},
		{"403 policy", plugin.Outcome{Status: 403, Body: body("This request may violate our usage policy")}, plugin.CauseUnknown, plugin.RetryNone, plugin.CredQuarantine, 0, ir.ErrPermission, false},
		{"403 region", plugin.Outcome{Status: 403, Body: body("unsupported_country_region_territory")}, plugin.CauseTransient, plugin.RetryNext, plugin.CredKeep, 0, ir.ErrPermission, false},
		{"402", plugin.Outcome{Status: 402, Body: body("Insufficient Balance")}, plugin.CauseQuota, plugin.RetryNext, plugin.CredCooldown, time.Hour, ir.ErrQuotaExhausted, false},
		{"429 insufficient_quota", plugin.Outcome{Status: 429, Body: []byte(`{"error":{"message":"You exceeded your current quota","code":"insufficient_quota"}}`),
			Header: http.Header{"X-Ratelimit-Reset-Tokens": {"2h"}}}, plugin.CauseQuota, plugin.RetryNext, plugin.CredCooldown, 2 * time.Hour, ir.ErrQuotaExhausted, false},
		{"429 retry-after", plugin.Outcome{Status: 429, Header: http.Header{"Retry-After": {"7"}}}, plugin.CauseRateLimit, plugin.RetryNext, plugin.CredCooldown, 7 * time.Second, ir.ErrRateLimited, true},
		{"429 reset header", plugin.Outcome{Status: 429, Header: http.Header{"Anthropic-Ratelimit-Unified-Reset": {"1793966460"}}}, plugin.CauseRateLimit, plugin.RetryNext, plugin.CredCooldown,
			time.Unix(1793966460, 0).Sub(t0), ir.ErrRateLimited, true},
		{"429 bare", plugin.Outcome{Status: 429}, plugin.CauseRateLimit, plugin.RetryNext, plugin.CredCooldown, 30 * time.Second, ir.ErrRateLimited, true},
		{"529", plugin.Outcome{Status: 529, Body: []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)},
			plugin.CauseRateLimit, plugin.RetryNext, plugin.CredCooldown, 10 * time.Second, ir.ErrOverloaded, true},
		{"503 overloaded", plugin.Outcome{Status: 503, Signal: &ir.GwError{Kind: ir.ErrOverloaded}}, plugin.CauseRateLimit, plugin.RetryNext, plugin.CredCooldown, 10 * time.Second, ir.ErrOverloaded, true},
		{"503 plain", plugin.Outcome{Status: 503, Body: []byte("upstream connect error")}, plugin.CauseTransient, plugin.RetryNext, plugin.CredKeep, 0, ir.ErrUpstream, false},
		{"500", plugin.Outcome{Status: 500, Body: body("Internal error")}, plugin.CauseTransient, plugin.RetryNext, plugin.CredKeep, 0, ir.ErrUpstream, false},
		{"504", plugin.Outcome{Status: 504}, plugin.CauseTransient, plugin.RetryNext, plugin.CredKeep, 0, ir.ErrTimeout, false},
		{"content signal", plugin.Outcome{Status: 400, Signal: &ir.GwError{Kind: ir.ErrContentFiltered, Message: "blocked by content filter"}},
			plugin.CauseContent, plugin.RetryNone, plugin.CredKeep, 0, ir.ErrContentFiltered, false},
		{"content code", plugin.Outcome{Status: 400, Body: []byte(`{"error":{"message":"The response was filtered","code":"content_filter"}}`)},
			plugin.CauseContent, plugin.RetryNone, plugin.CredKeep, 0, ir.ErrContentFiltered, false},
		{"404", plugin.Outcome{Status: 404, Body: body("model not found")}, plugin.CauseRequest, plugin.RetryNext, plugin.CredKeep, 0, ir.ErrNotFound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.o
			o.Credential = cred
			if o.Status != 0 {
				o.Sent = true
			}
			v := p.Classify(&o)
			if v.Cause != tc.cause || v.Retry != tc.retry || v.Credential != tc.cred {
				t.Fatalf("verdict = %s/%s/%s, want %s/%s/%s", v.Cause, v.Retry, v.Credential, tc.cause, tc.retry, tc.cred)
			}
			if tc.until > 0 && !v.Until.Equal(t0.Add(tc.until)) {
				t.Errorf("until = %v, want +%v", v.Until.Sub(t0), tc.until)
			}
			if tc.until == 0 && !v.Until.IsZero() {
				t.Errorf("until set: %v", v.Until)
			}
			if v.ModelScope != tc.scope {
				t.Errorf("model scope = %v", v.ModelScope)
			}
			if v.Sent != o.Sent {
				t.Errorf("sent = %v", v.Sent)
			}
			if v.ClientErr == nil || v.ClientErr.Kind != tc.kind || v.ClientErr.Message == "" {
				t.Errorf("client err = %+v, want kind %s", v.ClientErr, tc.kind)
			}
		})
	}
}

func TestClassifyRefresher(t *testing.T) {
	p := NewRefreshing("openai-compatible", refresher{}, WithClock(clock))
	v := p.Classify(&plugin.Outcome{Status: 401, Sent: true})
	if v.Credential != plugin.CredRefresh || v.Retry != plugin.RetryNext || !v.Until.IsZero() {
		t.Errorf("verdict = %+v", v)
	}
	var _ plugin.Refresher = p
}

type refresher struct{}

func (refresher) Refresh(context.Context, *plugin.Upstream, *plugin.Credential) (string, time.Time, error) {
	return "new", t0.Add(time.Hour), nil
}

func TestClassifyManifestRulesFirst(t *testing.T) {
	m, err := manifest.Parse([]byte(`
id: r
kind: provider
interpreter: openai-compatible
auth: {header: Authorization}
endpoints: [{protocol: openai-chat, base_url: "https://r.example.com/v1"}]
errors:
  - when: {status: 500, message_contains: "BUSY"}
    verdict: {cause: rate_limit, cooldown: 2m, model_scope: true, signal: overloaded}
  - when: {status: 400, code: reasoning_effort_unsupported}
    verdict: {cause: request, retry: downgrade, downgrade: {reasoning.effort: high}}
  - when: {status: 429, header_present: x-quota-exhausted}
    verdict: {cause: quota, credential: cool_until_reset}
  - when: {status: 403}
    verdict: {cause: banned, credential: manual}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := New("openai-compatible", WithIndex(NewIndex(m)), WithClock(clock))
	u := &plugin.Upstream{ID: "r1", Config: map[string]any{"manifest": "r"}}
	if _, err := p.Endpoints(u); err != nil {
		t.Fatal(err)
	}
	cred := &plugin.Credential{ID: "c", UpstreamID: "r1"}

	v := p.Classify(&plugin.Outcome{Status: 500, Sent: true, Credential: cred, Body: body("server busy")})
	if v.Cause != plugin.CauseRateLimit || v.Credential != plugin.CredCooldown || !v.Until.Equal(t0.Add(2*time.Minute)) ||
		!v.ModelScope || v.ClientErr.Kind != ir.ErrOverloaded {
		t.Errorf("rule 1: %+v", v)
	}
	v = p.Classify(&plugin.Outcome{Status: 500, Sent: true, Credential: cred, Body: body("boom")})
	if v.Cause != plugin.CauseTransient {
		t.Errorf("fallthrough to defaults: %+v", v)
	}
	v = p.Classify(&plugin.Outcome{Status: 400, Sent: true, Credential: cred,
		Body: []byte(`{"error":{"message":"effort max unsupported","code":"reasoning_effort_unsupported"}}`)})
	if v.Retry != plugin.RetryDowngrade || string(v.Downgrade["reasoning.effort"]) != `"high"` || v.Credential != plugin.CredKeep {
		t.Errorf("downgrade: %+v", v)
	}
	v = p.Classify(&plugin.Outcome{Status: 429, Sent: true, Credential: cred,
		Header: http.Header{"X-Quota-Exhausted": {"1"}, "X-Ratelimit-Reset-Requests": {"3h"}}})
	if v.Cause != plugin.CauseQuota || !v.Until.Equal(t0.Add(3*time.Hour)) || v.Retry != plugin.RetryNext {
		t.Errorf("quota rule: %+v", v)
	}
	v = p.Classify(&plugin.Outcome{Status: 403, Sent: true, Credential: cred, Body: body("region")})
	if v.Cause != plugin.CauseBanned || v.Credential != plugin.CredManual || v.Retry != plugin.RetryNone {
		t.Errorf("manifest 403 rule must win over the built-in region rule: %+v", v)
	}
}

func TestClassifyOfficialRule(t *testing.T) {
	p := New("openai-compatible", WithClock(clock))
	u := &plugin.Upstream{ID: "ds", Config: map[string]any{"manifest": "deepseek"}}
	_, _ = p.Endpoints(u)
	v := p.Classify(&plugin.Outcome{Status: 400, Sent: true, Credential: &plugin.Credential{UpstreamID: "ds"},
		Body: body("This model's maximum context length is 131072 tokens")})
	if v.Cause != plugin.CauseRequest || v.ClientErr.Kind != ir.ErrContextTooLong || v.Retry != plugin.RetryNone {
		t.Errorf("deepseek context rule: %+v", v)
	}
}

// Hard rule 9: the client sees the upstream's message, status, retry hint
// and rate-limit/request-id headers, and nothing else of the response.
func TestClientErrPassthrough(t *testing.T) {
	p := New("anthropic-compatible", WithClock(clock))
	resp := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"}}`)
	v := p.Classify(&plugin.Outcome{Status: 429, Sent: true, Body: resp, Header: http.Header{
		"Retry-After":                        {"12"},
		"X-Should-Retry":                     {"false"},
		"Anthropic-Ratelimit-Tokens-Reset":   {"2026-11-05T12:00:12Z"},
		"X-Ratelimit-Remaining-Requests":     {"0"},
		"Request-Id":                         {"req_123"},
		"X-Request-Id":                       {"abc"},
		"Set-Cookie":                         {"s=1"},
		"Content-Type":                       {"application/json"},
		"Anthropic-Organization-Id":          {"org"},
		"Openai-Processing-Ms":               {"5"},
		"Cf-Ray":                             {"x"},
		"Anthropic-Ratelimit-Unified-Status": {"rejected"},
	}})
	e := v.ClientErr
	if e.Message != "Number of request tokens has exceeded your per-minute rate limit" || e.Status != 429 ||
		e.RetryAfter != 12*time.Second || !e.NoRetry || e.Code != "rate_limit_error" || string(e.Upstream) != string(resp) {
		t.Errorf("client err = %+v", e)
	}
	want := []string{"Retry-After", "X-Should-Retry", "Anthropic-Ratelimit-Tokens-Reset", "X-Ratelimit-Remaining-Requests",
		"Request-Id", "X-Request-Id", "Anthropic-Ratelimit-Unified-Status"}
	if len(e.Header) != len(want) {
		t.Errorf("headers = %v", e.Header)
	}
	for _, k := range want {
		if e.Header.Get(k) == "" {
			t.Errorf("header %s not passed through", k)
		}
	}
	// Credential failures do not leak the upstream status to the client.
	v = p.Classify(&plugin.Outcome{Status: 401, Sent: true, Body: body("invalid api key"), Header: http.Header{"Request-Id": {"r"}}})
	if v.ClientErr.Status != 0 || v.ClientErr.Message != "invalid api key" || v.ClientErr.Header.Get("Request-Id") != "r" {
		t.Errorf("401 client err = %+v", v.ClientErr)
	}
	// Signal fields from the codec are preserved.
	v = p.Classify(&plugin.Outcome{Status: 400, Sent: true, Signal: &ir.GwError{Kind: ir.ErrContextTooLong, Message: "too long", TokensUsed: 9, TokensLimit: 8, Code: "invalid_request_error"}})
	if v.ClientErr.TokensUsed != 9 || v.ClientErr.TokensLimit != 8 || v.ClientErr.Code != "invalid_request_error" {
		t.Errorf("signal lost: %+v", v.ClientErr)
	}
}

// Hard rules 16 and 17 over every built-in path.
func TestHardRules16And17(t *testing.T) {
	p := New("openai-compatible", WithClock(clock))
	var outcomes []plugin.Outcome
	for _, st := range []int{0, 400, 401, 402, 403, 404, 408, 409, 413, 422, 429, 500, 502, 503, 504, 529} {
		for _, b := range [][]byte{nil, body("Your account has been suspended"), body("violates our usage policy"),
			body("content_filter triggered"), body("unsupported country"), body("maximum context length exceeded"), []byte("<html>bad gateway</html>")} {
			outcomes = append(outcomes, plugin.Outcome{Status: st, Body: b, Sent: st != 0})
			outcomes = append(outcomes, plugin.Outcome{Status: st, Body: b, Sent: st != 0, Signal: &ir.GwError{Kind: ir.ErrContentFiltered}})
		}
	}
	outcomes = append(outcomes, plugin.Outcome{Err: errors.New("dial tcp: no route to host")},
		plugin.Outcome{Err: timeoutErr{}, Sent: true})
	for _, o := range outcomes {
		v := p.Classify(&o)
		switch v.Cause {
		case plugin.CauseContent:
			if v.Retry != plugin.RetryNone {
				t.Errorf("rule 16: content retried for %d %s", o.Status, o.Body)
			}
		case plugin.CauseBanned, plugin.CauseUnknown:
			if v.Retry != plugin.RetryNone || (v.Credential != plugin.CredQuarantine && v.Credential != plugin.CredManual) || !v.Until.IsZero() {
				t.Errorf("rule 16: suspected ban %+v for %d %s", v, o.Status, o.Body)
			}
		case plugin.CauseRequest, plugin.CauseTransient:
			if v.Credential != plugin.CredKeep {
				t.Errorf("rule 17: credential changed for %d %s: %s", o.Status, o.Body, v.Credential)
			}
		}
		if o.Signal != nil && o.Signal.Kind == ir.ErrContentFiltered && o.Status != 0 && v.Cause != plugin.CauseContent {
			t.Errorf("content signal at %d classified %s", o.Status, v.Cause)
		}
		if o.Status == 0 && !o.Sent && (v.Retry != plugin.RetryNext || v.Sent) {
			t.Errorf("unsent request must be retried: %+v", v)
		}
	}
}

// ---------------------------------------------------------------- models & quota

func TestListModels(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("X-Api-Key")+"|"+r.Header.Get("Anthropic-Version"))
		switch {
		case r.URL.Path != "/v1/models":
			http.NotFound(w, r)
		case r.URL.Query().Get("after_id") == "":
			_, _ = w.Write([]byte(`{"data":[{"id":"a","display_name":"A","created_at":"2026-01-01T00:00:00Z","max_input_tokens":1000}],"has_more":true,"last_id":"a"}`))
		default:
			_, _ = w.Write([]byte(`{"data":[{"id":"b"}],"has_more":false}`))
		}
	}))
	defer srv.Close()
	m, err := manifest.Parse([]byte(`
id: l
kind: provider
interpreter: anthropic-compatible
variables: {base: {default: "` + srv.URL + `"}}
auth: {header: x-api-key}
endpoints: [{protocol: anthropic, base_url: "{base}"}]
models: {list: /v1/models}
quota: {path: /v1/quota, remaining_fraction: left, reset: reset}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := New("anthropic-compatible", WithIndex(NewIndex(m)), WithHTTPClient(srv.Client()))
	u := &plugin.Upstream{ID: "l", Config: map[string]any{"manifest": "l"}}
	got, err := p.ListModels(context.Background(), u, &plugin.Credential{Secret: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[0].DisplayName != "A" || got[0].ContextWindow != 1000 || got[0].Created == 0 || got[1].ID != "b" {
		t.Errorf("models = %+v", got)
	}
	if auth[0] != "k|2023-06-01" {
		t.Errorf("listing auth = %v", auth)
	}
	if _, err := p.Quota(context.Background(), u, &plugin.Credential{Secret: "k"}); err == nil {
		t.Error("quota 404 not reported")
	}
	if _, err := New("x", WithIndex(NewIndex(m))).ListModels(context.Background(), u, nil); err == nil {
		t.Error("listing without egress client must fail, not fall back to a default client")
	}
}

func TestListModelsShapesAndQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`[{"id":"x","context_length":4096,"created":1700000000}]`))
		case "/v1beta/models":
			_, _ = w.Write([]byte(`{"models":[{"name":"models/g","displayName":"G","inputTokenLimit":1048576,"outputTokenLimit":65536}]}`))
		case "/v1/balance":
			_, _ = w.Write([]byte(`{"data":{"left":25,"total":100,"reset":1800000000}}`))
		}
	}))
	defer srv.Close()
	m, err := manifest.Parse([]byte(`
id: s
kind: provider
interpreter: openai-compatible
variables: {base: {default: "` + srv.URL + `"}}
auth: {header: Authorization}
endpoints: [{protocol: openai-chat, base_url: "{base}/v1"}]
models: {list: /models}
quota: {path: /balance, remaining: data.left, limit: data.total, reset: data.reset}
`))
	if err != nil {
		t.Fatal(err)
	}
	g, err := manifest.Parse([]byte(`
id: g
kind: provider
interpreter: gemini
variables: {base: {default: "` + srv.URL + `"}}
auth: {header: x-goog-api-key}
endpoints: [{protocol: gemini, base_url: "{base}"}]
models: {list: /v1beta/models}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := New("openai-compatible", WithIndex(NewIndex(m, g)), WithHTTPClient(srv.Client()))
	got, err := p.ListModels(context.Background(), &plugin.Upstream{ID: "s", Config: map[string]any{"manifest": "s"}}, nil)
	if err != nil || len(got) != 1 || got[0].ContextWindow != 4096 || got[0].Created != 1700000000 {
		t.Errorf("array shape: %+v %v", got, err)
	}
	got, err = p.ListModels(context.Background(), &plugin.Upstream{ID: "g", Config: map[string]any{"manifest": "g"}}, nil)
	if err != nil || len(got) != 1 || got[0].ID != "g" || got[0].DisplayName != "G" || got[0].MaxOutput != 65536 {
		t.Errorf("models shape: %+v %v", got, err)
	}
	q, err := p.Quota(context.Background(), &plugin.Upstream{ID: "s", Config: map[string]any{"manifest": "s"}}, nil)
	if err != nil || q.Remaining != 0.25 || q.ResetAt.Unix() != 1800000000 {
		t.Errorf("quota = %+v %v", q, err)
	}
	st, err := manifest.Parse([]byte(`
id: st
kind: provider
interpreter: openai-compatible
auth: {header: Authorization}
endpoints: [{protocol: openai-chat, base_url: "https://st.example.com/v1"}]
models: {static: [{id: only, context_window: 8192, tools: true}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	got, err = New("openai-compatible", WithIndex(NewIndex(st))).ListModels(context.Background(), &plugin.Upstream{ID: "st", Config: map[string]any{"manifest": "st"}}, nil)
	if err != nil || len(got) != 1 || got[0].ID != "only" || !got[0].Tools {
		t.Errorf("static: %+v %v", got, err)
	}
}

func TestLoadDirErrors(t *testing.T) {
	x, err := LoadDir(manifests.FS)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.Put(&manifest.Manifest{ID: "bad", Kind: "provider"}); err == nil {
		t.Error("invalid manifest put")
	}
	x.Delete("openai")
	if _, ok := x.Get("openai"); ok {
		t.Error("delete failed")
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestLoadDirReportsEveryFile(t *testing.T) {
	ok := "id: a\nkind: provider\ninterpreter: openai-compatible\nauth: {header: Authorization}\nendpoints: [{protocol: openai-chat, base_url: \"https://a.example.com/v1\"}]\n"
	fsys := fstest.MapFS{
		"a.yaml":        {Data: []byte(ok)},
		"sub/dup.yml":   {Data: []byte(ok)},
		"bad.yaml":      {Data: []byte(strings.Replace(ok, "id: a", "id: b", 1) + "models: {list: nope}\n")},
		"a.prices.yaml": {Data: []byte("models: {}\n")},
		"README.md":     {Data: []byte("x")},
	}
	_, err := LoadDir(fsys)
	if err == nil || !strings.Contains(err.Error(), "bad.yaml: models.list") || !strings.Contains(err.Error(), "sub/dup.yml: id \"a\" already defined in a.yaml") {
		t.Errorf("err = %v", err)
	}
	delete(fsys, "bad.yaml")
	delete(fsys, "sub/dup.yml")
	x, err := LoadDir(fsys)
	if err != nil || len(x.IDs()) != 1 {
		t.Errorf("%v %v", x, err)
	}
}

func TestEndpointsDefaultIssuerIsPerUpstream(t *testing.T) {
	m, err := manifest.Parse([]byte(`id: x
kind: provider
interpreter: openai-compatible
auth: {header: Authorization, scheme: Bearer}
endpoints:
  - {id: chat, protocol: openai-chat, base_url: "{base_url}"}
variables: {base_url: {default: "https://a.example/v1"}}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := New("openai-compatible")
	issuers := map[string]bool{}
	for _, id := range []string{"one", "two"} {
		eps, err := p.Endpoints(&plugin.Upstream{ID: id, Config: map[string]any{"manifest": m}})
		if err != nil {
			t.Fatal(err)
		}
		issuers[eps[0].Issuer] = true
	}
	if len(issuers) != 2 || issuers[""] {
		t.Fatalf("issuers must be distinct and non-empty, got %v", issuers)
	}
}
