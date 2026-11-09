package bedrock

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
	"github.com/initxy/plugrelay/plugins/provider/compatible"
	"github.com/initxy/plugrelay/plugins/provider/internal/cloudtest"
	"github.com/initxy/plugrelay/plugins/provider/internal/sigv4"
	"github.com/initxy/plugrelay/plugins/provider/manifest"
	"github.com/tidwall/gjson"
)

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

const keys = "AKIDEXAMPLE:wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"

func newProvider(t *testing.T, baseURL string) (*Provider, *plugin.Upstream) {
	t.Helper()
	p := New(compatible.WithClock(func() time.Time { return t0 }), compatible.WithHTTPClient(http.DefaultClient))
	u := &plugin.Upstream{ID: "bdrk", Provider: ID, Config: map[string]any{"manifest": "bedrock", "region": "eu-west-1"}}
	if baseURL != "" {
		m, err := p.Manifest(u)
		if err != nil {
			t.Fatal(err)
		}
		cp := *m
		cp.Endpoints = append([]manifest.Endpoint(nil), m.Endpoints...)
		cp.Endpoints[0].BaseURL = baseURL
		u.Config["manifest"] = &cp
	}
	return p, u
}

// outgoing builds the request the core would: Messages path and body,
// forwarded client headers.
func outgoing(t *testing.T, p *Provider, u *plugin.Upstream, secret, body string, hdr ...string) *plugin.Outgoing {
	t.Helper()
	eps, err := p.Endpoints(u)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, eps[0].BaseURL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	return &plugin.Outgoing{Upstream: u, Endpoint: &eps[0], Model: "anthropic.claude-sonnet-4-5-20250929-v1:0",
		Credential: &plugin.Credential{ID: "c1", UpstreamID: u.ID, Secret: secret}, Req: req}
}

func TestEndpointsFromManifest(t *testing.T) {
	p, u := newProvider(t, "")
	eps, err := p.Endpoints(u)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].BaseURL != "https://bedrock-runtime.eu-west-1.amazonaws.com" || eps[0].Issuer != "bedrock" || eps[0].Protocol != "anthropic" {
		t.Errorf("endpoints %+v", eps)
	}
	if eps[0].Caps.Cache1h || len(eps[0].Caps.ServerTools) != 0 {
		t.Errorf("caps %+v", eps[0].Caps)
	}
	if p.Info().ID != ID {
		t.Errorf("id %s", p.Info().ID)
	}
}

func TestPrepareInvoke(t *testing.T) {
	p, u := newProvider(t, "")
	o := outgoing(t, p, u, keys+":SESSIONTOKEN",
		`{"model":"claude-sonnet-4-5","stream":true,"max_tokens":16,"anthropic_beta":["a-1"],"messages":[{"role":"user","content":"hi"}]}`,
		"Anthropic-Beta", "interleaved-thinking-2025-05-14,claude-code-20250219", "Anthropic-Beta", "a-1",
		"Anthropic-Version", "2023-06-01", "X-Amz-Date", "planted", "Authorization", "Bearer client")
	if err := p.Prepare(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	r := o.Req
	if got := r.URL.EscapedPath(); got != "/model/anthropic.claude-sonnet-4-5-20250929-v1%3A0/invoke-with-response-stream" {
		t.Errorf("path %s", got)
	}
	body, _ := io.ReadAll(r.Body)
	if gjson.GetBytes(body, "model").Exists() || gjson.GetBytes(body, "stream").Exists() {
		t.Errorf("model or stream left in body: %s", body)
	}
	if v := gjson.GetBytes(body, "anthropic_version").String(); v != AnthropicVersion {
		t.Errorf("anthropic_version %q", v)
	}
	if got := gjson.GetBytes(body, "anthropic_beta").Raw; got != `["a-1","interleaved-thinking-2025-05-14"]` {
		t.Errorf("anthropic_beta %s (claude-code beta must be dropped)", got)
	}
	if r.ContentLength != int64(len(body)) {
		t.Errorf("content length %d for %d bytes", r.ContentLength, len(body))
	}
	h := r.Header
	if h.Get("Anthropic-Beta") != "" || h.Get("Anthropic-Version") != "" {
		t.Errorf("anthropic headers left: %v", h)
	}
	if h.Get("Accept") != contentStream || h.Get("X-Amzn-Bedrock-Accept") != contentJSON {
		t.Errorf("accept headers %v", h)
	}
	auth := h.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261105/eu-west-1/bedrock/aws4_request, SignedHeaders=") {
		t.Errorf("Authorization %s", auth)
	}
	if !strings.Contains(auth, "x-amz-security-token") || h.Get("X-Amz-Security-Token") != "SESSIONTOKEN" {
		t.Errorf("session token not signed: %s", auth)
	}
	if h.Get("X-Amz-Date") != "20261105T120000Z" {
		t.Errorf("X-Amz-Date %s", h.Get("X-Amz-Date"))
	}

	// Non-streaming requests invoke.
	o = outgoing(t, p, u, keys, `{"model":"m","max_tokens":16,"messages":[]}`)
	if err := p.Prepare(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(o.Req.URL.Path, "/invoke") || o.Req.Header.Get("Accept") != contentJSON {
		t.Errorf("invoke: %s %v", o.Req.URL, o.Req.Header)
	}

	// Token counting is answered by the core.
	o = outgoing(t, p, u, keys, `{"model":"m","messages":[]}`)
	o.Req.URL.Path = "/v1/messages/count_tokens"
	if err := p.Prepare(context.Background(), o); err == nil {
		t.Error("count_tokens forwarded")
	}
}

func TestPrepareAPIKey(t *testing.T) {
	p, u := newProvider(t, "")
	o := outgoing(t, p, u, " ABSKexampleKey== ", `{"model":"m","max_tokens":1,"messages":[]}`)
	o.Model = "arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-x"
	if err := p.Prepare(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if got := o.Req.Header.Get("Authorization"); got != "Bearer ABSKexampleKey==" {
		t.Errorf("Authorization %q", got)
	}
	if o.Req.Header.Get("X-Amz-Date") != "" {
		t.Error("API key requests are not signed")
	}
	want := "/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A123456789012%3Ainference-profile%2Fus.anthropic.claude-x/invoke"
	if got := o.Req.URL.EscapedPath(); got != want {
		t.Errorf("path\n got %s\nwant %s", got, want)
	}
}

func TestParseSecret(t *testing.T) {
	for in, want := range map[string]sigv4.Credentials{
		"AK:SK":     {AccessKeyID: "AK", SecretAccessKey: "SK"},
		"AK:SK:TOK": {AccessKeyID: "AK", SecretAccessKey: "SK", SessionToken: "TOK"},
		`{"access_key_id":"AK","secret_access_key":"SK","session_token":"TOK"}`:                             {AccessKeyID: "AK", SecretAccessKey: "SK", SessionToken: "TOK"},
		`{"Credentials":{"AccessKeyId":"AK","SecretAccessKey":"SK","SessionToken":"TOK","Expiration":"x"}}`: {AccessKeyID: "AK", SecretAccessKey: "SK", SessionToken: "TOK"},
		`{"aws_access_key_id":"AK","aws_secret_access_key":"SK"}`:                                           {AccessKeyID: "AK", SecretAccessKey: "SK"},
	} {
		k, key, err := ParseSecret(in)
		if err != nil || key != "" || k == nil || *k != want {
			t.Errorf("%s: %+v %q %v", in, k, key, err)
		}
	}
	if k, key, err := ParseSecret("ABSKbase64key"); err != nil || k != nil || key != "ABSKbase64key" {
		t.Errorf("API key: %v %q %v", k, key, err)
	}
	for _, bad := range []string{"", "AK:", ":SK", "a:b:c:d", "AK:SK:", `{"access_key_id":"AK"}`, `{broken`} {
		if _, _, err := ParseSecret(bad); err == nil {
			t.Errorf("%q accepted", bad)
		} else if strings.Contains(err.Error(), "SK") && bad != "" {
			t.Errorf("%q: error echoes the secret: %v", bad, err)
		}
	}
}

// capture is a fake Bedrock Runtime that records what it received.
type capture struct {
	mu   sync.Mutex
	reqs []*http.Request
	body [][]byte
}

func (c *capture) server(t *testing.T, h http.HandlerFunc) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.reqs, c.body = append(c.reqs, r.Clone(context.Background())), append(c.body, b)
		c.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The upstream can verify the signature: re-signing what it received with
// the same keys and time yields the same Authorization.
func TestSignatureVerifiesAtUpstream(t *testing.T) {
	var c capture
	srv := c.server(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, cloudtest.BedrockFixtures().Response)
	})
	p, u := newProvider(t, srv.URL)
	o := outgoing(t, p, u, keys, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, "X-Client", "kept")
	if err := p.Prepare(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	var evs []*ir.Event
	oc, err := p.Send(context.Background(), o, nil, func(ev *ir.Event) error { evs = append(evs, ev); return nil })
	if err != nil || oc != nil {
		t.Fatalf("Send: %+v %v", oc, err)
	}
	got := c.reqs[0]
	sent := got.Header.Get("Authorization")
	re, _ := http.NewRequest(got.Method, srv.URL+got.URL.RequestURI(), bytes.NewReader(c.body[0]))
	for k, vs := range got.Header {
		if k != "Accept-Encoding" { // added by the transport after signing
			re.Header[k] = vs
		}
	}
	keysOnly, _, _ := ParseSecret(keys)
	if err := sigv4.Sign(re, c.body[0], *keysOnly, "eu-west-1", "bedrock", t0); err != nil {
		t.Fatal(err)
	}
	if re.Header.Get("Authorization") != sent {
		t.Errorf("signature does not verify\n sent %s\n want %s", sent, re.Header.Get("Authorization"))
	}
	if len(evs) != 4 || evs[0].Kind != ir.EvStart || evs[1].Block.Text != "Hello" || evs[3].Kind != ir.EvDone || evs[3].StopReason != "end_turn" {
		t.Errorf("events %+v", evs)
	}
	if evs[3].Usage == nil || evs[3].Usage.Output != 2 {
		t.Errorf("usage %+v", evs[3].Usage)
	}
}

func eventStream(frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentStream)
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			w.(http.Flusher).Flush()
		}
	}
}

func send(t *testing.T, h http.HandlerFunc) (*Provider, *plugin.Outcome, []*ir.Event) {
	t.Helper()
	var c capture
	srv := c.server(t, h)
	p, u := newProvider(t, srv.URL)
	o := outgoing(t, p, u, keys, `{"model":"m","stream":true,"max_tokens":16,"messages":[]}`)
	if err := p.Prepare(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	var evs []*ir.Event
	oc, err := p.Send(context.Background(), o, nil, func(ev *ir.Event) error { evs = append(evs, ev); return nil })
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	return p, oc, evs
}

func TestStreamDecodesChunks(t *testing.T) {
	frames := cloudtest.BedrockFixtures().Stream
	_, oc, evs := send(t, eventStream(frames...))
	if oc != nil {
		t.Fatalf("outcome %+v", oc)
	}
	var text string
	for _, ev := range evs {
		if ev.Kind == ir.EvDelta {
			text += ev.Delta.Text
		}
	}
	last := evs[len(evs)-1]
	if text != "Hello" || last.Kind != ir.EvDone || last.StopReason != "end_turn" || last.Usage.Input != 3 || last.Usage.Output != 2 {
		t.Errorf("text %q, last %+v", text, last)
	}
}

// An exception before any content is a retryable outcome; after content
// it ends the stream in-band.
func TestStreamExceptions(t *testing.T) {
	p, oc, evs := send(t, eventStream(cloudtest.BedrockException("throttlingException", "Too many tokens, please wait.")))
	if oc == nil || len(evs) != 0 {
		t.Fatalf("outcome %+v events %d", oc, len(evs))
	}
	if oc.Status != 429 || oc.Signal.Code != "ThrottlingException" || oc.Signal.Kind != ir.ErrRateLimited || !oc.Sent {
		t.Errorf("outcome %+v signal %+v", oc, oc.Signal)
	}
	if v := p.Classify(oc); v.Cause != plugin.CauseRateLimit || v.Retry != plugin.RetryNext || !v.ModelScope {
		t.Errorf("verdict %+v", v)
	}

	frames := cloudtest.BedrockFixtures().Stream[:3]
	frames = append(frames, cloudtest.BedrockException("modelStreamErrorException", "The model stream failed."))
	_, oc, evs = send(t, eventStream(frames...))
	if oc != nil {
		t.Fatalf("outcome after content: %+v", oc)
	}
	last := evs[len(evs)-1]
	if last.Kind != ir.EvError || last.Err.Code != "ModelStreamErrorException" || last.Err.Message != "The model stream failed." {
		t.Errorf("last event %+v err %+v", last, last.Err)
	}

	// A stream that stops before message_stop is not a clean end.
	_, _, evs = send(t, eventStream(cloudtest.BedrockFixtures().Stream[:4]...))
	if last := evs[len(evs)-1]; last.Kind != ir.EvError {
		t.Errorf("truncated stream ended with %s", last.Kind)
	}
}

func TestErrorVerdicts(t *testing.T) {
	p, u := newProvider(t, "")
	if _, err := p.Endpoints(u); err != nil {
		t.Fatal(err)
	}
	cred := &plugin.Credential{ID: "c1", UpstreamID: u.ID}
	cases := []struct {
		status int
		typ    string
		msg    string
		cause  plugin.Cause
		cred   plugin.CredAction
		retry  plugin.RetryMode
		kind   ir.ErrorKind
	}{
		{429, "ThrottlingException", "Too many requests", plugin.CauseRateLimit, plugin.CredCooldown, plugin.RetryNext, ir.ErrRateLimited},
		{400, "ServiceQuotaExceededException", "quota", plugin.CauseRateLimit, plugin.CredCooldown, plugin.RetryNext, ir.ErrRateLimited},
		{403, "AccessDeniedException", "You don't have access to the model with the specified model ID.", plugin.CauseAuthExpired, plugin.CredCooldown, plugin.RetryNext, ir.ErrPermission},
		{403, "UnrecognizedClientException", "The security token included in the request is invalid.", plugin.CauseAuthExpired, plugin.CredCooldown, plugin.RetryNext, ir.ErrAuthentication},
		{403, "ExpiredTokenException", "The security token included in the request is expired", plugin.CauseAuthExpired, plugin.CredCooldown, plugin.RetryNext, ir.ErrAuthentication},
		{400, "ValidationException", "messages: field required", plugin.CauseRequest, plugin.CredKeep, plugin.RetryNone, ir.ErrInvalidRequest},
		{400, "ValidationException", "Input is too long for requested model.", plugin.CauseRequest, plugin.CredKeep, plugin.RetryNone, ir.ErrContextTooLong},
		{429, "ModelNotReadyException", "Model is not ready", plugin.CauseTransient, plugin.CredKeep, plugin.RetryNext, ir.ErrOverloaded},
		{424, "ModelErrorException", "model error", plugin.CauseTransient, plugin.CredKeep, plugin.RetryNext, ir.ErrUpstream},
		{503, "ServiceUnavailableException", "unavailable", plugin.CauseTransient, plugin.CredKeep, plugin.RetryNext, ir.ErrOverloaded},
		{404, "ResourceNotFoundException", "model not found", plugin.CauseRequest, plugin.CredKeep, plugin.RetryNext, ir.ErrNotFound},
		{403, "AccessDeniedException", "Your AWS account is suspended.", plugin.CauseBanned, plugin.CredQuarantine, plugin.RetryNone, ir.ErrPermission},
	}
	for _, c := range cases {
		h := codeHeader(c.typ + ":http://internal.amazon.com/coral/com.amazon.bedrock/")
		h.Set("X-Amzn-Requestid", "rid-1")
		body := []byte(`{"message":"` + c.msg + `"}`)
		v := p.Classify(&plugin.Outcome{Credential: cred, Status: c.status, Header: h, Body: body,
			Signal: DecodeError(c.status, h, body), Sent: true})
		if v.Cause != c.cause || v.Credential != c.cred || v.Retry != c.retry || v.ClientErr.Kind != c.kind {
			t.Errorf("%d %s %q: verdict %+v kind %s", c.status, c.typ, c.msg, v, v.ClientErr.Kind)
		}
		if v.ClientErr.Message != c.msg || v.ClientErr.Header.Get("X-Amzn-Requestid") != "rid-1" {
			t.Errorf("%s: client error %+v", c.typ, v.ClientErr)
		}
	}
}

// Send goes out through the attempt's egress client: the credential's
// proxy carries the request, and a dead proxy fails without a direct try.
func TestSendUsesAttemptEgress(t *testing.T) {
	var c capture
	srv := c.server(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, cloudtest.BedrockFixtures().Response)
	})
	_, u := newProvider(t, srv.URL)
	p := New(compatible.WithClock(func() time.Time { return t0 }))
	eg := egress.New(egress.Options{})
	send := func(proxy string, withClient bool) *plugin.Outcome {
		t.Helper()
		o := outgoing(t, p, u, keys, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		o.Credential.Proxy = proxy
		w := egress.Start(context.Background(), egress.Timeouts{Total: 10 * time.Second})
		defer w.Stop()
		if withClient {
			var err error
			if o.Client, err = eg.ForAttempt(o.Credential, true, w); err != nil {
				t.Fatal(err)
			}
		}
		if err := p.Prepare(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		oc, err := p.Send(w.Context(), o, nil, func(*ir.Event) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		return oc
	}
	proxy, via := testkit.ForwardProxy(t)
	if oc := send(proxy.URL, true); oc != nil || via.Load() != 1 || len(c.reqs) != 1 {
		t.Fatalf("via proxy: outcome %+v, proxied %d, upstream %d", oc, via.Load(), len(c.reqs))
	}
	oc := send(testkit.DeadProxy(t), true)
	var pe *egress.ProxyError
	if oc == nil || !errors.As(oc.Err, &pe) || oc.Sent || len(c.reqs) != 1 {
		t.Fatalf("dead proxy: outcome %+v, upstream %d (must not fall back to direct)", oc, len(c.reqs))
	}
	if oc := send("", false); oc == nil || oc.Err == nil || len(c.reqs) != 1 {
		t.Fatalf("no attempt client: outcome %+v, upstream %d", oc, len(c.reqs))
	}
}
