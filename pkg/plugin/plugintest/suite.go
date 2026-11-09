package plugintest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/sse"
)

// Scenario names, usable in ProviderSuite.Skip and ProviderSuite.Expect.
const (
	ScenarioNormal            = "normal"
	ScenarioStream            = "stream"
	ScenarioHeaders           = "header_ownership"
	ScenarioCutStream         = "cut_stream"
	ScenarioRateLimit         = "rate_limit"
	ScenarioUnauthorized      = "unauthorized"
	ScenarioSuspended         = "suspended"
	ScenarioContent           = "content_violation"
	ScenarioUncertain         = "uncertain"
	ScenarioTimeoutBeforeSend = "timeout_before_send"
	ScenarioSlowFirstByte     = "slow_first_byte"
	ScenarioOversizedEvent    = "oversized_event"
	ScenarioErrorPassthrough  = "error_passthrough"
	ScenarioServerError       = "server_error"
)

var scenarios = []string{
	ScenarioNormal, ScenarioStream, ScenarioHeaders, ScenarioCutStream, ScenarioRateLimit,
	ScenarioUnauthorized, ScenarioSuspended, ScenarioContent, ScenarioUncertain,
	ScenarioTimeoutBeforeSend, ScenarioSlowFirstByte, ScenarioOversizedEvent,
	ScenarioErrorPassthrough, ScenarioServerError,
}

// Scenarios lists every scenario in run order.
func Scenarios() []string { return slices.Clone(scenarios) }

// ClientSecret is planted in the client's Authorization header and in
// every provider-owned header before Prepare runs; it must never reach the
// upstream.
const ClientSecret = "client-secret-must-not-leak"

// ProviderSuite configures RunProvider.
type ProviderSuite struct {
	// New builds the provider under test pointed at baseURL, plus the
	// upstream instance and credential to use. It is called once per
	// scenario. The upstream must have an endpoint speaking Protocol whose
	// base URL is baseURL (directly or via Provider.Endpoints).
	New func(baseURL string) (plugin.Provider, *plugin.Upstream, *plugin.Credential)
	// Protocol is the endpoint protocol (codec name) to exercise.
	Protocol string
	// Model is the upstream model name (default "test-model").
	Model string
	// Fixtures overrides the built-in fixtures for Protocol.
	Fixtures *Fixtures
	// UseSend drives the provider's Send instead of the suite's own HTTP
	// client. Send must end every stream with EvDone or EvError, report a
	// failure before content as an Outcome (Sent=false when the request
	// never left), and report a failure after content in-band as EvError.
	UseSend bool
	// MaxEventSize is the SSE event limit. The suite reads with it in
	// client mode (default 1 MiB) and expects Send to enforce
	// sse.DefaultMaxEventSize in send mode unless set.
	MaxEventSize int
	// FirstByteDelay is how long the slow_first_byte upstream waits
	// before answering (default 500ms).
	FirstByteDelay time.Duration
	// SkipSecretCheck disables the check that the credential's secret
	// reaches the upstream verbatim (for signing schemes).
	SkipSecretCheck bool
	// Skip lists scenarios not to run.
	Skip []string
	// Expect replaces the default verdict assertions of a scenario.
	Expect map[string]func(t *testing.T, v plugin.Verdict)
}

const scenarioTimeout = 15 * time.Second

// RunProvider runs every scenario as a subtest.
func RunProvider(t *testing.T, s ProviderSuite) {
	t.Helper()
	if s.New == nil {
		t.Fatal("plugintest: ProviderSuite.New is required")
	}
	fx, ok := FixturesFor(s.Protocol)
	if s.Fixtures != nil {
		fx, ok = *s.Fixtures, true
	}
	if !ok {
		t.Fatalf("plugintest: no fixtures for protocol %q; set ProviderSuite.Fixtures", s.Protocol)
	}
	if s.Model == "" {
		s.Model = "test-model"
	}
	if s.FirstByteDelay == 0 {
		s.FirstByteDelay = 500 * time.Millisecond
	}
	if s.MaxEventSize == 0 {
		s.MaxEventSize = 1 << 20
		if s.UseSend {
			s.MaxEventSize = sse.DefaultMaxEventSize
		}
	}
	h := &harness{s: &s, fx: fx}
	for _, name := range scenarios {
		t.Run(name, func(t *testing.T) {
			if slices.Contains(s.Skip, name) {
				t.Skip("skipped by ProviderSuite.Skip")
			}
			if len(fx.Stream) == 0 && isStreamScenario(name) {
				t.Skip("the protocol has no event stream")
			}
			h.run(t, name)
		})
	}
}

type harness struct {
	s  *ProviderSuite
	fx Fixtures
}

// recorded is what the upstream received.
type recorded struct {
	Method string
	URL    *url.URL
	Header http.Header
	Body   []byte
}

type upstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recorded
}

func (u *upstream) requests() []recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.reqs)
}

func newUpstream(handler http.HandlerFunc) *upstream {
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, recorded{Method: r.Method, URL: r.URL, Header: r.Header.Clone(), Body: body})
		u.mu.Unlock()
		handler(w, r)
	}))
	return u
}

// result is one finished attempt.
type result struct {
	outcome  *plugin.Outcome
	sendErr  error
	events   []*ir.Event
	frames   int
	before   http.Header
	prepared http.Header
	owned    []string
	// defaulted are headers Prepare sets only when the client sent none.
	defaulted []string
	cred      *plugin.Credential
	up        *upstream
	provider  plugin.Provider
}

func (h *harness) handler(name string) http.HandlerFunc {
	fx := h.fx
	writeStream := func(w http.ResponseWriter, frames []string) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		for _, fr := range frames {
			_, _ = io.WriteString(w, fr)
			if f != nil {
				f.Flush()
			}
		}
	}
	switch name {
	case ScenarioNormal, ScenarioHeaders:
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, fx.Response)
		}
	case ScenarioStream:
		return func(w http.ResponseWriter, r *http.Request) { writeStream(w, fx.Stream) }
	case ScenarioSlowFirstByte:
		return func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(h.s.FirstByteDelay):
			case <-r.Context().Done():
				return
			}
			writeStream(w, fx.Stream)
		}
	case ScenarioCutStream:
		return func(w http.ResponseWriter, r *http.Request) {
			n := max(2, len(fx.Stream)/2)
			writeStream(w, fx.Stream[:min(n, len(fx.Stream)-1)])
			_, _ = io.WriteString(w, "data: {\"trunc")
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
	case ScenarioOversizedEvent:
		return func(w http.ResponseWriter, r *http.Request) {
			writeStream(w, fx.Stream[:1])
			big := "data: \"" + strings.Repeat("a", h.s.MaxEventSize+1024) + "\"\n\n"
			_, _ = io.WriteString(w, big)
			_, _ = io.WriteString(w, fx.Stream[len(fx.Stream)-1])
		}
	}
	e, ok := fx.Errors[name]
	if !ok {
		return nil
	}
	return func(w http.ResponseWriter, r *http.Request) {
		for k, vs := range e.Header {
			w.Header()[k] = vs
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(e.Status)
		_, _ = io.WriteString(w, e.Body)
	}
}

func isStreamScenario(name string) bool {
	return name == ScenarioStream || name == ScenarioCutStream || name == ScenarioSlowFirstByte || name == ScenarioOversizedEvent
}

func (h *harness) run(t *testing.T, name string) {
	stream := isStreamScenario(name)
	var up *upstream
	baseURL := ""
	if name == ScenarioTimeoutBeforeSend {
		baseURL = deadAddress(t)
	} else {
		hf := h.handler(name)
		if hf == nil {
			t.Skipf("fixtures have no %q error", name)
		}
		up = newUpstream(hf)
		defer up.srv.Close()
		baseURL = up.srv.URL
	}
	res := h.attempt(t, name, baseURL, stream)
	res.up = up
	h.check(t, name, res)
}

// deadAddress returns a URL whose connection attempts never complete in
// client mode and are refused in send mode.
func deadAddress(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr
}

func (h *harness) attempt(t *testing.T, name, baseURL string, stream bool) *result {
	t.Helper()
	s := h.s
	p, u, cred := s.New(baseURL)
	if p == nil || u == nil {
		t.Fatal("ProviderSuite.New returned a nil provider or upstream")
	}
	eps, err := p.Endpoints(u)
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	i := slices.IndexFunc(eps, func(e plugin.Endpoint) bool { return e.Protocol == s.Protocol })
	if i < 0 {
		t.Fatalf("Endpoints returned no %q endpoint: %+v", s.Protocol, eps)
	}
	ep := eps[i]
	owned := p.OwnedHeaders(u)

	req := h.request(t, &ep, stream, owned)
	o := &plugin.Outgoing{Upstream: u, Endpoint: &ep, Credential: cred, Model: s.Model, Req: req}
	res := &result{before: req.Header.Clone(), owned: owned, cred: cred, provider: p}

	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	if err := p.Prepare(ctx, o); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	res.prepared = o.Req.Header.Clone()
	res.defaulted = h.defaulted(ctx, t, p, o, stream, res)

	if s.UseSend {
		sender, ok := p.(plugin.Sender)
		if !ok {
			t.Fatal("UseSend is set but the provider does not implement plugin.Sender")
		}
		irReq := &ir.Request{Model: s.Model, Stream: stream, Messages: []ir.Message{{
			Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hello"}},
		}}}
		res.outcome, res.sendErr = sender.Send(ctx, o, irReq, func(ev *ir.Event) error {
			res.events = append(res.events, ev)
			return nil
		})
		return res
	}

	client := &http.Client{}
	if name == ScenarioTimeoutBeforeSend {
		client.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		var c2 context.CancelFunc
		ctx, c2 = context.WithTimeout(ctx, 200*time.Millisecond)
		defer c2()
	}
	var sent atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { sent.Store(true) }}
	resp, err := client.Do(o.Req.WithContext(httptrace.WithClientTrace(ctx, trace)))
	if err != nil {
		res.outcome = &plugin.Outcome{Endpoint: &ep, Credential: cred, Err: err, Sent: sent.Load()}
		return res
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		res.outcome = &plugin.Outcome{
			Endpoint: &ep, Credential: cred, Status: resp.StatusCode, Header: resp.Header,
			Body: body, Signal: h.signal(name, resp.Header, body), Sent: true,
		}
		return res
	}
	if !stream {
		_, _ = io.Copy(io.Discard, resp.Body)
		return res
	}
	rd := sse.NewReader(resp.Body, s.MaxEventSize)
	for {
		_, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return res
		}
		if err != nil {
			res.outcome = &plugin.Outcome{Endpoint: &ep, Credential: cred, Status: resp.StatusCode, Header: resp.Header, Err: err, Sent: true}
			return res
		}
		res.frames++
	}
}

// clientValue is planted by defaulted to see whether Prepare keeps it.
const clientValue = "plugintest-client-value"

// defaulted returns the unowned headers Prepare set that it leaves alone
// when the client sends its own value (protocol version headers, for
// example). Such a header fills a default and cannot override a client
// value, so it need not be listed in OwnedHeaders. Each candidate is
// checked by running Prepare again on a fresh request carrying a client
// value for it.
func (h *harness) defaulted(ctx context.Context, t *testing.T, p plugin.Provider, o *plugin.Outgoing, stream bool, res *result) []string {
	t.Helper()
	var out []string
	for k := range unionKeys(res.before, res.prepared) {
		if len(res.before.Values(k)) > 0 || len(res.prepared.Values(k)) == 0 ||
			slices.ContainsFunc(res.owned, func(s string) bool { return strings.EqualFold(s, k) }) {
			continue
		}
		req := h.request(t, o.Endpoint, stream, res.owned)
		req.Header.Set(k, clientValue)
		again := *o
		again.Req = req
		if err := p.Prepare(ctx, &again); err != nil {
			t.Fatalf("Prepare with a client %s: %v", k, err)
		}
		if slices.Equal(req.Header.Values(k), []string{clientValue}) {
			out = append(out, k)
		}
	}
	return out
}

// request builds the outgoing request the way the core does, then plants
// the client secret in every credential-bearing and provider-owned header.
func (h *harness) request(t *testing.T, ep *plugin.Endpoint, stream bool, owned []string) *http.Request {
	t.Helper()
	fx := h.fx
	path := fx.Path
	if stream && fx.StreamPath != "" {
		path = fx.StreamPath
	}
	if ep.Path != "" {
		path = ep.Path
	}
	path = strings.ReplaceAll(path, "{model}", h.s.Model)
	u, err := url.Parse(strings.TrimRight(ep.BaseURL, "/") + path)
	if err != nil {
		t.Fatalf("endpoint base URL %q: %v", ep.BaseURL, err)
	}
	q := u.Query()
	for k, v := range ep.Query {
		q.Set(k, v)
	}
	if stream && fx.StreamQuery != "" {
		extra, _ := url.ParseQuery(fx.StreamQuery)
		for k, vs := range extra {
			q[k] = vs
		}
	}
	u.RawQuery = q.Encode()

	body := strings.ReplaceAll(fx.Request, "{model}", h.s.Model)
	if stream && fx.StreamPath == "" {
		body = `{"stream":true,` + strings.TrimPrefix(body, "{")
	}
	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "plugintest/1")
	req.Header.Set("X-Client-Session", "s-1")
	req.Header.Set("Authorization", "Bearer "+ClientSecret)
	for _, k := range owned {
		req.Header.Set(k, ClientSecret)
	}
	return req
}

func (h *harness) signal(name string, header http.Header, body []byte) *ir.GwError {
	e, ok := h.fx.Errors[name]
	if !ok {
		return &ir.GwError{Kind: ir.ErrUpstream, Message: "upstream error", Upstream: body}
	}
	sig := e.Signal
	sig.Upstream = body
	sig.Header = http.Header{}
	for k, vs := range header {
		lk := strings.ToLower(k)
		if lk == "retry-after" || lk == "x-should-retry" || strings.HasSuffix(lk, "request-id") || strings.Contains(lk, "ratelimit") {
			sig.Header[k] = vs
		}
	}
	if ra, err := strconv.Atoi(header.Get("Retry-After")); err == nil {
		sig.RetryAfter = time.Duration(ra) * time.Second
	}
	return &sig
}

// ---------------------------------------------------------------- checks

func (h *harness) check(t *testing.T, name string, r *result) {
	switch name {
	case ScenarioNormal, ScenarioStream, ScenarioSlowFirstByte:
		h.checkSuccess(t, r)
		return
	case ScenarioHeaders:
		h.checkSuccess(t, r)
		h.checkHeaders(t, r)
		return
	}

	if h.s.UseSend && (name == ScenarioCutStream || name == ScenarioOversizedEvent) {
		if r.sendErr != nil || r.outcome != nil {
			t.Fatalf("Send after content must end in-band with EvError, got outcome=%+v err=%v", r.outcome, r.sendErr)
		}
		checkEvents(t, r.events, ir.EvError)
		return
	}
	if r.sendErr != nil {
		t.Fatalf("Send must report a failed attempt as an Outcome, got error: %v", r.sendErr)
	}
	if r.outcome == nil {
		t.Fatalf("attempt succeeded, want failure (events %d, frames %d)", len(r.events), r.frames)
	}
	if h.s.UseSend && len(r.events) > 0 {
		t.Errorf("Send returned an Outcome after emitting %d events", len(r.events))
	}
	v := r.provider.Classify(r.outcome)
	if v.Sent != r.outcome.Sent {
		t.Errorf("Verdict.Sent = %v, Outcome.Sent = %v", v.Sent, r.outcome.Sent)
	}
	if v.Retry != plugin.RetryNext && v.ClientErr == nil {
		t.Errorf("a verdict that may reach the client needs ClientErr: %+v", v)
	}
	if exp, ok := h.s.Expect[name]; ok {
		exp(t, v)
		return
	}
	expectVerdict(t, name, v, r)
}

func (h *harness) checkSuccess(t *testing.T, r *result) {
	t.Helper()
	if r.sendErr != nil {
		t.Fatalf("Send: %v", r.sendErr)
	}
	if r.outcome != nil {
		t.Fatalf("attempt failed: status %d err %v", r.outcome.Status, r.outcome.Err)
	}
	if n := len(r.up.requests()); n != 1 {
		t.Fatalf("upstream received %d requests, want 1", n)
	}
	if h.s.UseSend {
		checkEvents(t, r.events, ir.EvDone)
	}
}

func checkEvents(t *testing.T, evs []*ir.Event, terminal ir.EventKind) {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	open := map[int]bool{}
	stopped := map[int]bool{}
	next := 0
	content := false
	for i, ev := range evs {
		last := i == len(evs)-1
		switch ev.Kind {
		case ir.EvBlockStart:
			if ev.Index != next {
				t.Errorf("event %d: block index %d, want dense %d", i, ev.Index, next)
			}
			next++
			open[ev.Index] = true
		case ir.EvDelta:
			if !open[ev.Index] || stopped[ev.Index] {
				t.Errorf("event %d: delta for block %d that is not open", i, ev.Index)
			}
		case ir.EvBlockStop:
			if !open[ev.Index] || stopped[ev.Index] {
				t.Errorf("event %d: block %d stopped twice or never started", i, ev.Index)
			}
			stopped[ev.Index] = true
		case ir.EvDone, ir.EvError:
			if !last {
				t.Errorf("event %d: %s before the end of the stream", i, ev.Kind)
			}
			if ev.Kind == ir.EvError && ev.Err == nil {
				t.Errorf("event %d: EvError without Err", i)
			}
		}
		if ev.Kind != ir.EvPing && ev.Kind != ir.EvStatus && ev.Kind != ir.EvStart && ev.Kind != ir.EvUsage {
			content = true
		}
	}
	if got := evs[len(evs)-1].Kind; got != terminal {
		t.Errorf("stream ended with %s, want %s", got, terminal)
	}
	if terminal == ir.EvDone {
		if !content {
			t.Error("stream carried no content")
		}
		for idx := range open {
			if !stopped[idx] {
				t.Errorf("block %d never stopped", idx)
			}
		}
	}
}

func (h *harness) checkHeaders(t *testing.T, r *result) {
	t.Helper()
	reqs := r.up.requests()
	if len(reqs) == 0 {
		return
	}
	got := reqs[0]
	for k, vs := range got.Header {
		for _, v := range vs {
			if strings.Contains(v, ClientSecret) {
				t.Errorf("client credential reached the upstream in %s; Prepare must set or delete it and list it in OwnedHeaders", k)
			}
		}
	}
	if strings.Contains(got.URL.RawQuery, ClientSecret) {
		t.Error("client credential reached the upstream in the query")
	}
	// A sender may rebuild the request itself, so header ownership is only
	// observable from Prepare in client mode.
	if !h.s.UseSend {
		ignored := []string{"Content-Type", "Accept", "Accept-Encoding", "Content-Length"}
		for k := range unionKeys(r.before, r.prepared) {
			if slices.Equal(r.before.Values(k), r.prepared.Values(k)) || len(r.prepared.Values(k)) == 0 {
				continue
			}
			if slices.ContainsFunc(ignored, func(s string) bool { return strings.EqualFold(s, k) }) ||
				slices.Contains(r.defaulted, k) {
				continue
			}
			if !slices.ContainsFunc(r.owned, func(s string) bool { return strings.EqualFold(s, k) }) {
				t.Errorf("Prepare sets %s but OwnedHeaders does not list it; a client value would collide", k)
			}
		}
	}
	if !h.s.SkipSecretCheck && r.cred != nil && r.cred.Secret != "" {
		found := strings.Contains(got.URL.RawQuery, url.QueryEscape(r.cred.Secret)) || strings.Contains(got.URL.RawQuery, r.cred.Secret)
		for _, vs := range got.Header {
			for _, v := range vs {
				found = found || strings.Contains(v, r.cred.Secret)
			}
		}
		if !found {
			t.Error("the credential's secret did not reach the upstream (set SkipSecretCheck for signing schemes)")
		}
	}
}

func unionKeys(a, b http.Header) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[http.CanonicalHeaderKey(k)] = true
	}
	for k := range b {
		out[http.CanonicalHeaderKey(k)] = true
	}
	return out
}

func expectVerdict(t *testing.T, name string, v plugin.Verdict, r *result) {
	t.Helper()
	fail := func(format string, a ...any) {
		t.Errorf("%s: "+format+"; verdict %s", append([]any{name}, append(a, fmtVerdict(v))...)...)
	}
	keep := func() {
		if v.Credential != plugin.CredKeep {
			fail("not the credential's fault, credential must be kept, got %s", v.Credential)
		}
	}
	switch name {
	case ScenarioRateLimit:
		if v.Cause != plugin.CauseRateLimit && v.Cause != plugin.CauseQuota {
			fail("cause %q, want rate_limit or quota", v.Cause)
		}
		if v.Retry != plugin.RetryNext {
			fail("retry %q, want next", v.Retry)
		}
		if v.Credential != plugin.CredCooldown {
			fail("credential %q, want cooldown", v.Credential)
		}
		if !v.Until.IsZero() && v.Until.Before(time.Now().Add(5*time.Second)) {
			fail("cooldown ends at %s, before the upstream's retry-after of 7s", v.Until.Format(time.RFC3339))
		}
		if v.ClientErr == nil || (v.ClientErr.RetryAfter <= 0 && v.ClientErr.Header.Get("Retry-After") == "") {
			fail("retry-after must reach the client")
		}
	case ScenarioUnauthorized:
		if v.Cause != plugin.CauseAuthExpired {
			fail("cause %q, want auth_expired", v.Cause)
		}
		if v.Credential == plugin.CredKeep {
			fail("a rejected credential cannot stay in rotation")
		}
	case ScenarioSuspended:
		if v.Cause != plugin.CauseBanned {
			fail("cause %q, want banned", v.Cause)
		}
		if v.Credential != plugin.CredQuarantine {
			fail("credential %q, want quarantine", v.Credential)
		}
		if v.Retry != plugin.RetryNone {
			fail("retry %q, want none", v.Retry)
		}
	case ScenarioContent:
		if v.Cause != plugin.CauseContent {
			fail("cause %q, want content", v.Cause)
		}
		if v.Retry != plugin.RetryNone {
			fail("content violations are never retried on any credential, got retry %q", v.Retry)
		}
		keep()
	case ScenarioUncertain:
		if v.Credential != plugin.CredQuarantine {
			fail("when unsure between ban and content, quarantine; got %q", v.Credential)
		}
		if v.Retry != plugin.RetryNone {
			fail("when unsure, do not retry; got %q", v.Retry)
		}
	case ScenarioTimeoutBeforeSend:
		if v.Sent || r.outcome.Sent {
			fail("request never left, Sent must be false")
		}
		if v.Retry != plugin.RetryNext {
			fail("an unsent request is safe to retry, got %q", v.Retry)
		}
		keep()
	case ScenarioErrorPassthrough:
		if v.Cause != plugin.CauseRequest {
			fail("cause %q, want request", v.Cause)
		}
		if v.Retry != plugin.RetryNone {
			fail("a bad request fails everywhere, got retry %q", v.Retry)
		}
		keep()
		if ce := v.ClientErr; ce != nil {
			body := string(r.outcome.Body)
			if !bytes.Equal(ce.Upstream, r.outcome.Body) && (ce.Message == "" || !strings.Contains(body, ce.Message)) {
				fail("ClientErr must carry the upstream's message or body")
			}
			for k := range r.outcome.Header {
				if strings.HasSuffix(strings.ToLower(k), "request-id") && ce.Header.Get(k) == "" {
					fail("upstream header %s must pass through to the client", k)
				}
			}
		}
	case ScenarioServerError:
		if v.Cause != plugin.CauseTransient && v.Cause != plugin.CauseRateLimit {
			fail("cause %q, want transient", v.Cause)
		}
		if v.Retry != plugin.RetryNext {
			fail("retry %q, want next", v.Retry)
		}
		if v.Credential == plugin.CredQuarantine || v.Credential == plugin.CredManual || v.Credential == plugin.CredRefresh {
			fail("a server error is not the credential's fault")
		}
	case ScenarioCutStream, ScenarioOversizedEvent:
		if !v.Sent {
			fail("the request reached the upstream, Sent must be true")
		}
		keep()
	}
}

func fmtVerdict(v plugin.Verdict) string {
	ce := "<nil>"
	if v.ClientErr != nil {
		ce = fmt.Sprintf("%s %q", v.ClientErr.Kind, v.ClientErr.Message)
	}
	return fmt.Sprintf("{cause=%s retry=%s credential=%s sent=%v client_err=%s}", v.Cause, v.Retry, v.Credential, v.Sent, ce)
}
