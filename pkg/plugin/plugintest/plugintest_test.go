package plugintest_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/plugin/plugintest"
	"github.com/initxy/plugrelay/pkg/sse"
)

// fakeProvider is a minimal provider following the verdict rules the suite
// checks; it stands in for a real one.
type fakeProvider struct {
	owned []string
	// brokenContent retries content violations; brokenAuth sets a header
	// it does not own and leaves the client's Authorization alone.
	brokenContent, brokenAuth bool
	// defaultVersion fills X-Proto-Version only when the client sent none.
	defaultVersion bool
}

func (p *fakeProvider) Info() plugin.Info {
	return plugin.Info{ID: "provider.fake", Kind: plugin.KindProvider}
}

func (p *fakeProvider) Endpoints(u *plugin.Upstream) ([]plugin.Endpoint, error) {
	return u.Endpoints, nil
}

func (p *fakeProvider) OwnedHeaders(*plugin.Upstream) []string { return p.owned }

func (p *fakeProvider) Prepare(_ context.Context, o *plugin.Outgoing) error {
	if p.brokenAuth {
		o.Req.Header.Set("X-Api-Key", o.Credential.Secret)
		return nil
	}
	o.Req.Header.Set("Authorization", "Bearer "+o.Credential.Secret)
	if p.defaultVersion && o.Req.Header.Get("X-Proto-Version") == "" {
		o.Req.Header.Set("X-Proto-Version", "1")
	}
	return nil
}

func (p *fakeProvider) Classify(o *plugin.Outcome) plugin.Verdict {
	v := plugin.Verdict{Sent: o.Sent, ClientErr: o.Signal, Retry: plugin.RetryNone, Credential: plugin.CredKeep}
	if v.ClientErr == nil {
		v.ClientErr = &ir.GwError{Kind: ir.ErrUpstream, Message: "upstream failed", Status: o.Status, Upstream: o.Body}
	}
	body := strings.ToLower(string(o.Body))
	content := o.Signal != nil && o.Signal.Kind == ir.ErrContentFiltered
	switch {
	case !o.Sent:
		v.Cause, v.Retry = plugin.CauseTransient, plugin.RetryNext
	case o.Status == 0 || o.Status < 400:
		v.Cause = plugin.CauseTransient
	case o.Status == 429:
		v.Cause, v.Retry, v.Credential = plugin.CauseRateLimit, plugin.RetryNext, plugin.CredCooldown
		if o.Signal != nil && o.Signal.RetryAfter > 0 {
			v.Until = time.Now().Add(o.Signal.RetryAfter)
		}
	case o.Status == 401:
		v.Cause, v.Retry, v.Credential = plugin.CauseAuthExpired, plugin.RetryNext, plugin.CredRefresh
	case o.Status == 403:
		v.Cause, v.Credential = plugin.CauseUnknown, plugin.CredQuarantine
		for _, w := range []string{"suspended", "deactivated", "disabled"} {
			if strings.Contains(body, w) {
				v.Cause = plugin.CauseBanned
			}
		}
	case content:
		v.Cause = plugin.CauseContent
		if p.brokenContent {
			v.Retry = plugin.RetryNext
		}
	case o.Status < 500:
		v.Cause = plugin.CauseRequest
	default:
		v.Cause, v.Retry = plugin.CauseTransient, plugin.RetryNext
	}
	return v
}

func newFake(p *fakeProvider, protocol string) func(string) (plugin.Provider, *plugin.Upstream, *plugin.Credential) {
	return func(baseURL string) (plugin.Provider, *plugin.Upstream, *plugin.Credential) {
		u := &plugin.Upstream{ID: "up", Provider: "provider.fake", Endpoints: []plugin.Endpoint{{ID: "e", Protocol: protocol, BaseURL: baseURL}}}
		return p, u, &plugin.Credential{ID: "c", UpstreamID: "up", Secret: "sk-upstream-1"}
	}
}

func TestFakeProviderAllProtocols(t *testing.T) {
	for _, proto := range []string{"openai-chat", "openai-responses", "anthropic", "gemini"} {
		t.Run(proto, func(t *testing.T) {
			plugintest.RunProvider(t, plugintest.ProviderSuite{
				Protocol:       proto,
				New:            newFake(&fakeProvider{owned: []string{"Authorization"}}, proto),
				FirstByteDelay: 100 * time.Millisecond,
			})
		})
	}
}

// TestDefaultedHeaderNeedNotBeOwned: a header Prepare only fills in when
// the client sent none cannot override a client value, so it passes the
// ownership check without being listed in OwnedHeaders.
func TestDefaultedHeaderNeedNotBeOwned(t *testing.T) {
	plugintest.RunProvider(t, plugintest.ProviderSuite{
		Protocol:       "anthropic",
		New:            newFake(&fakeProvider{owned: []string{"Authorization"}, defaultVersion: true}, "anthropic"),
		FirstByteDelay: 100 * time.Millisecond,
		Skip:           []string{plugintest.ScenarioSlowFirstByte, plugintest.ScenarioTimeoutBeforeSend},
	})
}

// fakeSender sends openai-chat requests itself and streams neutral events.
type fakeSender struct{ fakeProvider }

func (s *fakeSender) Send(ctx context.Context, o *plugin.Outgoing, _ *ir.Request, emit func(*ir.Event) error) (*plugin.Outcome, error) {
	var sent atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { sent.Store(true) }}
	resp, err := http.DefaultClient.Do(o.Req.WithContext(httptrace.WithClientTrace(ctx, trace)))
	if err != nil {
		return &plugin.Outcome{Endpoint: o.Endpoint, Credential: o.Credential, Err: err, Sent: sent.Load()}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		var e struct {
			Error struct {
				Message, Code string
			}
		}
		_ = json.Unmarshal(body, &e)
		kind := ir.ErrUpstream
		if e.Error.Code == "content_policy_violation" {
			kind = ir.ErrContentFiltered
		}
		sig := &ir.GwError{Kind: kind, Message: e.Error.Message, Upstream: body, Header: resp.Header}
		if resp.Header.Get("Retry-After") != "" {
			sig.RetryAfter = 7 * time.Second
		}
		return &plugin.Outcome{Endpoint: o.Endpoint, Credential: o.Credential, Status: resp.StatusCode, Header: resp.Header, Body: body, Signal: sig, Sent: true}, nil
	}
	if err := emit(&ir.Event{Kind: ir.EvStart, ID: "r", Model: o.Model}); err != nil {
		return nil, err
	}
	_ = emit(&ir.Event{Kind: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}})
	text := func(s string) {
		_ = emit(&ir.Event{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaText, Text: s}})
	}
	finish := func() error {
		_ = emit(&ir.Event{Kind: ir.EvBlockStop, Index: 0})
		return emit(&ir.Event{Kind: ir.EvDone, StopReason: ir.StopEndTurn})
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var r struct {
			Choices []struct {
				Message struct{ Content string }
			}
		}
		_ = json.NewDecoder(resp.Body).Decode(&r)
		text(r.Choices[0].Message.Content)
		return nil, finish()
	}
	rd := sse.NewReader(resp.Body, 0)
	for {
		ev, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, emit(&ir.Event{Kind: ir.EvError, Err: &ir.GwError{Kind: ir.ErrUpstream, Message: err.Error()}})
		}
		if string(ev.Data) == "[DONE]" {
			return nil, finish()
		}
		var c struct {
			Choices []struct {
				Delta struct{ Content string }
			}
		}
		if json.Unmarshal(ev.Data, &c) == nil && len(c.Choices) > 0 && c.Choices[0].Delta.Content != "" {
			text(c.Choices[0].Delta.Content)
		}
	}
}

func TestFakeSender(t *testing.T) {
	s := &fakeSender{fakeProvider{owned: []string{"Authorization"}}}
	plugintest.RunProvider(t, plugintest.ProviderSuite{
		Protocol: "openai-chat",
		UseSend:  true,
		New: func(baseURL string) (plugin.Provider, *plugin.Upstream, *plugin.Credential) {
			_, u, c := newFake(&s.fakeProvider, "openai-chat")(baseURL)
			return s, u, c
		},
		FirstByteDelay: 100 * time.Millisecond,
	})
}

func TestScenarioList(t *testing.T) {
	if len(plugintest.Scenarios()) != 14 {
		t.Fatalf("scenarios %v", plugintest.Scenarios())
	}
	for _, p := range []string{"openai-chat", "openai-responses", "openai-completions", "anthropic", "gemini"} {
		fx, ok := plugintest.FixturesFor(p)
		if !ok || len(fx.Errors) != 7 {
			t.Fatalf("%s fixtures incomplete", p)
		}
		for name, e := range fx.Errors {
			if !json.Valid([]byte(e.Body)) {
				t.Fatalf("%s %s: invalid JSON body", p, name)
			}
		}
	}
}

// TestBrokenProviderIsCaught runs the suite on providers that break the
// rules in a child process and expects it to fail with useful messages.
func TestBrokenProviderIsCaught(t *testing.T) {
	if os.Getenv("PLUGINTEST_BROKEN") != "" {
		plugintest.RunProvider(t, plugintest.ProviderSuite{
			Protocol: "openai-chat",
			New:      newFake(&fakeProvider{brokenContent: true, brokenAuth: true}, "openai-chat"),
			Skip:     []string{plugintest.ScenarioSlowFirstByte},
		})
		return
	}
	if testing.Short() {
		t.Skip("spawns a child test process")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestBrokenProviderIsCaught$", "-test.v")
	cmd.Env = append(os.Environ(), "PLUGINTEST_BROKEN=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("broken provider passed:\n%s", out)
	}
	for _, want := range []string{
		"client credential reached the upstream in Authorization",
		"Prepare sets X-Api-Key but OwnedHeaders does not list it",
		"content violations are never retried",
		"--- SKIP: TestBrokenProviderIsCaught/slow_first_byte",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
