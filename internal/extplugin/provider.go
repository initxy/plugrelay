package extplugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// classifyTimeout bounds provider.classify, which has no caller context.
const classifyTimeout = 5 * time.Second

// Provider adapts the provider.* methods. It implements plugin.Provider,
// plugin.ModelLister, plugin.Refresher, plugin.QuotaProber and
// plugin.Sender; the optional ones return ErrNotImplemented unless Supports
// reports the method.
type Provider struct {
	base
	mu    sync.Mutex
	owned map[string][]string
}

var (
	_ plugin.Provider    = (*Provider)(nil)
	_ plugin.ModelLister = (*Provider)(nil)
	_ plugin.Refresher   = (*Provider)(nil)
	_ plugin.QuotaProber = (*Provider)(nil)
	_ plugin.Sender      = (*Provider)(nil)
	_ plugin.Supporter   = (*Provider)(nil)
)

// Endpoints implements plugin.Provider. Without provider.endpoints the
// upstream's configured endpoints are used as is.
func (p *Provider) Endpoints(u *plugin.Upstream) ([]plugin.Endpoint, error) {
	if !p.Supports("provider.endpoints") {
		eps := slices.Clone(u.Endpoints)
		if p.in.man.Provider != nil {
			for i := range eps {
				if eps[i].Issuer == "" {
					eps[i].Issuer = p.in.man.Provider.Issuer
				}
			}
		}
		return eps, nil
	}
	var res struct {
		Endpoints    []plugin.Endpoint `json:"endpoints"`
		OwnedHeaders []string          `json:"owned_headers"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.in.man.CallTimeout("provider.endpoints"))
	defer cancel()
	if err := p.call(ctx, "provider.endpoints", map[string]any{"upstream": toWireUpstream(u)}, &res); err != nil {
		return nil, err
	}
	if res.OwnedHeaders != nil {
		p.mu.Lock()
		if p.owned == nil {
			p.owned = map[string][]string{}
		}
		p.owned[u.ID] = res.OwnedHeaders
		p.mu.Unlock()
	}
	return res.Endpoints, nil
}

// OwnedHeaders implements plugin.Provider: the list reported by the last
// provider.endpoints call for u, else the manifest's.
func (p *Provider) OwnedHeaders(u *plugin.Upstream) []string {
	p.mu.Lock()
	h, ok := p.owned[u.ID]
	p.mu.Unlock()
	if ok {
		return slices.Clone(h)
	}
	if p.in.man.Provider != nil {
		return slices.Clone(p.in.man.Provider.OwnedHeaders)
	}
	return nil
}

func (p *Provider) outgoing(o *plugin.Outgoing, body []byte) *wireOutgoing {
	w := &wireOutgoing{
		Upstream: toWireUpstream(o.Upstream), Endpoint: o.Endpoint,
		Credential: toWireCredential(o.Credential), Model: o.Model,
	}
	if o.Req != nil {
		w.Method, w.URL, w.Header = o.Req.Method, o.Req.URL.String(), o.Req.Header
	}
	if len(body) > 0 {
		if json.Valid(body) {
			w.Body = body
		} else {
			w.BodyBase64 = base64.StdEncoding.EncodeToString(body)
		}
	}
	return w
}

// Prepare implements plugin.Provider. The header the plugin returns
// replaces the outgoing header; a returned url replaces the request URL.
func (p *Provider) Prepare(ctx context.Context, o *plugin.Outgoing) error {
	if !p.Supports("provider.prepare") {
		return nil
	}
	var res struct {
		Header http.Header `json:"header"`
		URL    string      `json:"url"`
	}
	if err := p.call(ctx, "provider.prepare", map[string]any{"outgoing": p.outgoing(o, nil)}, &res); err != nil {
		return err
	}
	if o.Req == nil {
		return nil
	}
	if res.Header != nil {
		h := http.Header{}
		for k, vs := range res.Header {
			for _, v := range vs {
				h.Add(k, v)
			}
		}
		o.Req.Header = h
	}
	if res.URL != "" {
		u, err := url.Parse(res.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return &CallError{Plugin: p.in.man.ID, Method: "provider.prepare", Message: "invalid url " + res.URL, Err: ErrPlugin}
		}
		o.Req.URL = u
		o.Req.Host = ""
	}
	return nil
}

// Classify implements plugin.Provider. If the plugin cannot answer, the
// verdict leaves the credential alone: a plugin failure says nothing about
// the credential.
func (p *Provider) Classify(o *plugin.Outcome) plugin.Verdict {
	w := wireOutcome{
		Status: o.Status, Header: o.Header, Body: string(o.Body), Signal: toWireError(o.Signal), Sent: o.Sent,
	}
	if o.Endpoint != nil {
		w.EndpointID = o.Endpoint.ID
	}
	if o.Credential != nil {
		w.CredentialID = o.Credential.ID
	}
	if o.Err != nil {
		w.Error = o.Err.Error()
	}
	if p.Supports("provider.classify") {
		ctx, cancel := context.WithTimeout(context.Background(), min(classifyTimeout, p.in.man.CallTimeout("provider.classify")))
		defer cancel()
		var res struct {
			Verdict *wireVerdict `json:"verdict"`
		}
		err := p.call(ctx, "provider.classify", map[string]any{"outcome": w}, &res)
		if err == nil && res.Verdict != nil {
			return res.Verdict.verdict()
		}
		p.in.log.Warn("provider.classify failed; using fallback verdict", "err", err)
	}
	return fallbackVerdict(o)
}

func fallbackVerdict(o *plugin.Outcome) plugin.Verdict {
	v := plugin.Verdict{Cause: plugin.CauseUnknown, Retry: plugin.RetryNone, Credential: plugin.CredKeep, Sent: o.Sent}
	if !o.Sent {
		v.Retry = plugin.RetryNext
	}
	switch {
	case o.Signal != nil:
		v.ClientErr = o.Signal
	default:
		v.ClientErr = &ir.GwError{Kind: ir.ErrUpstream, Message: "upstream request failed", Status: o.Status, Upstream: o.Body}
	}
	return v
}

// ListModels implements plugin.ModelLister.
func (p *Provider) ListModels(ctx context.Context, u *plugin.Upstream, c *plugin.Credential) ([]ir.ModelInfo, error) {
	var res struct {
		Models []ir.ModelInfo `json:"models"`
	}
	err := p.call(ctx, "provider.listModels", map[string]any{"upstream": toWireUpstream(u), "credential": toWireCredential(c)}, &res)
	return res.Models, err
}

// Refresh implements plugin.Refresher.
func (p *Provider) Refresh(ctx context.Context, u *plugin.Upstream, c *plugin.Credential) (string, time.Time, error) {
	var res struct {
		Secret  string    `json:"secret"`
		Expires time.Time `json:"expires"`
	}
	err := p.call(ctx, "provider.refresh", map[string]any{"upstream": toWireUpstream(u), "credential": toWireCredential(c)}, &res)
	return res.Secret, res.Expires, err
}

// Quota implements plugin.QuotaProber.
func (p *Provider) Quota(ctx context.Context, u *plugin.Upstream, c *plugin.Credential) (plugin.Quota, error) {
	if !p.Supports("provider.quota") {
		return plugin.Quota{Remaining: -1}, plugin.ErrNoQuota
	}
	var res struct {
		Quota plugin.Quota `json:"quota"`
	}
	err := p.call(ctx, "provider.quota", map[string]any{"upstream": toWireUpstream(u), "credential": toWireCredential(c)}, &res)
	return res.Quota, err
}

// Send implements plugin.Sender over provider.send and $/event notifications.
//
//   - (nil, nil): the stream ended and its last emitted event was EvDone or EvError.
//   - (outcome, nil): the attempt failed before any content; pass it to Classify.
//   - (nil, err): the plugin failed (plugin.IsFailure) or emit returned err.
//
// Once content was emitted, failures end the stream in-band with EvError
// and Send returns (nil, nil): the attempt can no longer be retried.
func (p *Provider) Send(ctx context.Context, o *plugin.Outgoing, req *ir.Request, emit func(*ir.Event) error) (*plugin.Outcome, error) {
	const method = "provider.send"
	if !p.Supports(method) {
		return nil, &CallError{Plugin: p.in.man.ID, Method: method, Err: ErrNotImplemented}
	}
	ctx, cancel := context.WithTimeoutCause(ctx, p.in.man.CallTimeout(method), errCallTimeout)
	defer cancel()
	c, release, err := p.in.acquire(ctx)
	if err != nil {
		return nil, p.in.wrap(ctx, method, err)
	}
	defer release()
	body, err := readBody(o.Req)
	if err != nil {
		return nil, err
	}

	token := uuid.NewString()
	stream := c.openStream(token)
	defer c.closeStream(token)
	id, respCh, err := c.start(method, map[string]any{"token": token, "outgoing": p.outgoing(o, body), "request": req})
	if err != nil {
		return nil, p.in.wrap(ctx, method, errors.Join(errConnClosed, err))
	}

	var content, terminal bool
	var emitErr error
	handle := func(raw json.RawMessage) {
		if terminal || emitErr != nil {
			return
		}
		var we wireEvent
		if json.Unmarshal(raw, &we) != nil {
			return
		}
		ev := we.event()
		if ev.IsContent() {
			content = true
		}
		if ev.Kind == ir.EvDone || ev.Kind == ir.EvError {
			terminal = true
		}
		emitErr = emit(ev)
	}
	// abort ends a failed call; after content it closes the stream in-band.
	abort := func(cause error, msg string) (*plugin.Outcome, error) {
		c.cancelRequest(id)
		if content && !terminal {
			if err := emit(&ir.Event{Kind: ir.EvError, Err: &ir.GwError{Kind: ir.ErrUpstream, Message: msg}}); err != nil {
				return nil, err
			}
			return nil, nil
		}
		return nil, p.in.wrap(ctx, method, cause)
	}

	for {
		select {
		case raw := <-stream.ch:
			handle(raw)
			if emitErr != nil {
				c.cancelRequest(id)
				return nil, emitErr
			}
		case m, ok := <-respCh:
			for drained := false; !drained; {
				select {
				case raw := <-stream.ch:
					handle(raw)
				default:
					drained = true
				}
			}
			if emitErr != nil {
				return nil, emitErr
			}
			if !ok {
				return abort(errConnClosed, "plugin exited during the stream")
			}
			var res struct {
				Outcome *wireOutcome `json:"outcome"`
			}
			if err := decodeResult(m, &res); err != nil {
				return abort(err, "plugin failed during the stream")
			}
			if res.Outcome != nil && !content && !terminal {
				return p.outcome(o, res.Outcome), nil
			}
			if !terminal {
				if err := emit(&ir.Event{Kind: ir.EvError, Err: &ir.GwError{Kind: ir.ErrUpstream, Message: "plugin ended the stream without a terminal event"}}); err != nil {
					return nil, err
				}
			}
			return nil, nil
		case <-ctx.Done():
			return abort(ctx.Err(), "plugin stream timed out")
		}
	}
}

func (p *Provider) outcome(o *plugin.Outgoing, w *wireOutcome) *plugin.Outcome {
	out := &plugin.Outcome{
		Endpoint: o.Endpoint, Credential: o.Credential, Status: w.Status, Header: w.Header,
		Signal: w.Signal.gw(), Sent: w.Sent,
	}
	if w.Body != "" {
		out.Body = []byte(w.Body)
	}
	if w.Error != "" {
		out.Err = errors.New(w.Error)
	}
	return out
}

func readBody(r *http.Request) ([]byte, error) {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	if r.GetBody != nil {
		rc, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}
