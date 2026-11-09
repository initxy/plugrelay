package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/initxy/plugrelay/internal/convert"
	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// crossKeep are the client headers forwarded to an upstream of another
// protocol.
var crossKeep = []string{"Content-Type", "Accept", "User-Agent"}

// attempt is one try against one candidate.
type attempt struct {
	n      int
	cand   plugin.Candidate
	up     *Upstream
	ep     *plugin.Endpoint
	cred   *plugin.Credential
	target protocol.Codec
	tier   plugin.Tier
	body   []byte
	path   string
	// cctx is the conversion context (convert tier); sent is the IR as sent
	// upstream, or the client request for the byte-level tiers.
	cctx *convert.Context
	sent *ir.Request
	diag *ir.Diagnostic
	pa   *plugin.Attempt
	// header is the outgoing request header, built before the body so
	// attempt hooks of the convert tier can run before encoding.
	header http.Header
	hooked bool // attempt hooks already ran
	fatal  bool // a closed-policy plugin failed: end the request

	start    time.Time
	headerAt time.Time
	firstAt  time.Time
	wrote    atomic.Bool // request bytes reached the connection
	status   int
	ok       bool // got a 2xx response
	cause    plugin.Cause
	errMsg   string
	usage    ir.Usage
	outChars int
	done     bool // the upstream stream reached its end event
	// hideUsage drops the usage-only chunk the relay asked for and the
	// client did not (stream_options.include_usage).
	hideUsage bool
	startEv   *ir.Event // first EvStart seen from upstream
	watch     *egress.Watch
}

// attempt runs one try and says what the loop does next.
func (x *exchange) attempt(cand plugin.Candidate, n int) result {
	a, gw := x.prepare(cand, n)
	if a != nil {
		x.attempts = append(x.attempts, a)
	}
	if gw != nil {
		if a != nil {
			a.errMsg = gw.Message
			if a.fatal {
				return result{next: nextStop, fail: &failure{err: gw}}
			}
		}
		return result{next: nextRetry, fail: &failure{err: gw}}
	}
	if snd := sender(a.up.Provider); snd != nil {
		return x.sendVia(snd, a)
	}
	resp, gw := x.send(a)
	if gw != nil {
		return result{next: nextStop, fail: &failure{err: gw}}
	}
	if resp == nil {
		return x.transportFailure(a)
	}
	a.status = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return x.upstreamFailure(a, resp)
	}
	a.ok = true
	x.p.d.Router.ObserveLatency(cand.UpstreamID, cand.EndpointID, a.headerAt.Sub(a.start))
	if x.in.Stream {
		return x.stream(a, resp)
	}
	return x.full(a, resp)
}

// prepare resolves the candidate and builds the upstream body for the
// lowest tier that serves it.
func (x *exchange) prepare(cand plugin.Candidate, n int) (*attempt, *ir.GwError) {
	up := x.snap.Upstreams[cand.UpstreamID]
	ep := x.endpoint(cand.UpstreamID, cand.EndpointID)
	if up == nil || up.Provider == nil || ep == nil {
		return nil, ir.Errorf(ir.ErrInternal, "upstream %s/%s is not configured", cand.UpstreamID, cand.EndpointID)
	}
	cred := up.credential(cand)
	if cred == nil {
		return nil, ir.Errorf(ir.ErrInternal, "credential %s of upstream %s is not configured", cand.CredentialID, cand.UpstreamID)
	}
	target, ok := protocol.Get(ep.Protocol)
	if !ok {
		return nil, ir.Errorf(ir.ErrInternal, "protocol %q is not available", ep.Protocol)
	}
	cred = withUpstreamProxy(up, x.p.CurrentSecret(cand.UpstreamID, cred))
	a := &attempt{n: n, cand: cand, up: up, ep: ep, cred: cred, target: target, start: x.p.d.Now()}
	owned := up.Provider.OwnedHeaders(up.Config)
	a.header = egress.OutboundHeaders(x.r.Header, owned, cand.SameProtocol, crossKeep)
	egress.SetExtraHeaders(a.header, up.Headers, owned)
	a.header.Set("Content-Type", "application/json")
	a.pa = &plugin.Attempt{N: n, Candidate: cand, UpstreamModel: cand.UpstreamModel, Header: a.header}
	var gw *ir.GwError
	if cand.SameProtocol && !x.sealed && sender(up.Provider) == nil && !x.dropsServerTools(ep) {
		gw = x.patchBody(a)
	} else {
		gw = x.convertBody(a)
	}
	a.pa.Tier = a.tier
	return a, gw
}

// withUpstreamProxy is cred with its fixed egress: its own proxy, else
// its upstream's.
func withUpstreamProxy(up *Upstream, cred *plugin.Credential) *plugin.Credential {
	if cred.Proxy != "" || up.Proxy == "" {
		return cred
	}
	c := *cred
	c.Proxy = up.Proxy
	return &c
}

// prepareRaw hands a request made outside an attempt (native, batch,
// WebSocket) to its provider's Prepare, with the client the provider's own
// calls use bound to watch. send is the credential's egress client for the
// request itself. On failure, failure is the attempt record's error text
// and gw the client error.
func (p *Pipeline) prepareRaw(ctx context.Context, up *Upstream, ep *plugin.Endpoint, cred *plugin.Credential, req *http.Request, watch *egress.Watch) (out *plugin.Outgoing, send *http.Client, failure string, gw *ir.GwError) {
	send, err := p.d.Egress.For(cred, up.AllowPrivate)
	if err != nil {
		return nil, nil, err.Error(), ir.Errorf(ir.ErrUpstream, "egress: %v", err)
	}
	own, err := p.d.Egress.ForAttempt(cred, up.AllowPrivate, watch)
	if err != nil {
		return nil, nil, err.Error(), ir.Errorf(ir.ErrUpstream, "egress: %v", err)
	}
	out = &plugin.Outgoing{Upstream: up.Config, Endpoint: ep, Credential: cred, Req: req, Client: own}
	if err := up.Provider.Prepare(ctx, out); err != nil {
		return nil, nil, "prepare: " + err.Error(), ir.Errorf(ir.ErrInternal, "prepare upstream request: %v", err)
	}
	if out.Client == nil {
		out.Client = own
	}
	return out, send, "", nil
}

// outgoing builds the upstream request, runs attempt hooks and the
// provider's Prepare. A nil request with a nil error means a failure
// recorded on a; a non-nil error is a hook rejection that ends the request.
func (x *exchange) outgoing(a *attempt) (*plugin.Outgoing, *ir.GwError) {
	a.watch = egress.Start(x.ctx, x.p.d.Timeouts.Override(a.up.Timeouts))
	u, err := upstreamURL(a.ep, a.path, a.cand.UpstreamModel)
	if err != nil {
		a.watch.Stop()
		a.errMsg = errText(err)
		return nil, nil
	}
	w := a.watch
	trace := &httptrace.ClientTrace{
		GotConn:      func(httptrace.GotConnInfo) { w.Connected() },
		WroteRequest: func(httptrace.WroteRequestInfo) { a.wrote.Store(true) },
	}
	ctx := httptrace.WithClientTrace(a.watch.Context(), trace)
	var body io.Reader = bytes.NewReader(a.body)
	if x.upload != nil {
		rc, ct, err := x.upload.body(a.cand.UpstreamModel, a.watch.Touch)
		if err != nil {
			a.watch.Stop()
			a.errMsg = errText(err)
			return nil, nil
		}
		a.header.Set("Content-Type", ct)
		body = rc
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		a.watch.Stop()
		a.errMsg = errText(err)
		return nil, nil
	}
	req.Header = a.header
	if gw := x.runAttemptHooks(a, nil); gw != nil {
		a.watch.Stop()
		return nil, gw
	}
	client, err := x.p.d.Egress.ForAttempt(a.cred, a.up.AllowPrivate, a.watch)
	if err != nil {
		a.watch.Stop()
		a.errMsg = errText(err)
		return nil, nil
	}
	out := &plugin.Outgoing{Upstream: a.up.Config, Endpoint: a.ep, Credential: a.cred, Model: a.cand.UpstreamModel, Req: req, Client: client}
	if err := a.up.Provider.Prepare(ctx, out); err != nil {
		a.watch.Stop()
		a.errMsg = "prepare: " + errText(err)
		return nil, nil
	}
	return out, nil
}

// send sends the prepared request through the credential's egress. A nil
// response with a nil error means a transport failure recorded on a.
func (x *exchange) send(a *attempt) (*http.Response, *ir.GwError) {
	out, gw := x.outgoing(a)
	if out == nil {
		return nil, gw
	}
	client, err := x.p.d.Egress.For(a.cred, a.up.AllowPrivate)
	if err != nil {
		a.watch.Stop()
		a.errMsg = errText(err)
		return nil, nil
	}
	resp, err := client.Do(out.Req)
	if err != nil {
		err = a.watch.Err(err)
		a.watch.Stop()
		a.errMsg = errText(err)
		return nil, nil
	}
	a.headerAt = x.p.d.Now()
	resp.Body = a.watch.Body(resp.Body)
	return resp, nil
}

// upstreamURL joins base URL, path (the endpoint's template or the codec's)
// and the endpoint's fixed query.
func upstreamURL(ep *plugin.Endpoint, path, model string) (string, error) {
	if ep.Path != "" {
		path = strings.ReplaceAll(ep.Path, "{model}", url.PathEscape(model))
	}
	p, rawQuery, _ := strings.Cut(path, "?")
	u, err := url.Parse(strings.TrimRight(ep.BaseURL, "/") + p)
	if err != nil {
		return "", err
	}
	if rawQuery == "" && len(ep.Query) == 0 {
		return u.String(), nil
	}
	q := u.Query()
	if extra, err := url.ParseQuery(rawQuery); err == nil {
		for k, vs := range extra {
			q[k] = vs
		}
	}
	for k, v := range ep.Query {
		if q.Get(k) == "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// runAttemptHooks runs OnAttempt once per attempt. In the convert tier it
// runs before encoding with Call.Request returning the attempt's IR (req),
// which hooks may edit.
func (x *exchange) runAttemptHooks(a *attempt, req *ir.Request) *ir.GwError {
	if a.hooked {
		return nil
	}
	a.hooked = true
	x.attemptReq = req
	defer func() { x.attemptReq = nil }()
	for _, pl := range x.entered {
		hk, ok := pl.(plugin.AttemptHook)
		if !ok || !supports(pl, "middleware.onAttempt") {
			continue
		}
		info := pl.Info()
		if err := hk.OnAttempt(x.call.WithPlugin(info.ID), a.pa); err != nil {
			if gw, ok := x.hookError(info, "attempt", err); ok {
				return gw
			}
		}
	}
	return nil
}

// transportFailure handles an attempt that got no response.
func (x *exchange) transportFailure(a *attempt) result {
	var err error = errors.New(a.errMsg)
	if f := a.watch.Fired(); f != nil {
		err = f
	} else if x.ctx.Err() != nil {
		err = context.Canceled
	}
	o := x.outcome(a)
	o.Err = err
	o.Sent = a.wrote.Load()
	return x.judge(a, o, nil)
}

// upstreamFailure handles a non-2xx response.
func (x *exchange) upstreamFailure(a *attempt, resp *http.Response) result {
	body, _ := readBounded(resp.Body, x.p.d.MaxErrorBody)
	resp.Body.Close()
	o := x.outcome(a)
	o.Status, o.Header, o.Body, o.Sent = resp.StatusCode, resp.Header, body, true
	o.Signal = a.target.DecodeError(resp.StatusCode, resp.Header, body)
	var verbatim *upstreamError
	if a.cand.SameProtocol {
		verbatim = &upstreamError{status: resp.StatusCode, header: egress.InboundResponseHeaders(resp.Header), body: body}
	}
	return x.judge(a, o, verbatim)
}

func (x *exchange) outcome(a *attempt) *plugin.Outcome {
	return &plugin.Outcome{Upstream: a.up.Config, Endpoint: a.ep, Credential: a.cred}
}

// judge classifies a failed attempt, reports it to the router and decides
// whether to retry. Nothing has reached the client when it is called,
// except possibly keepalives.
func (x *exchange) judge(a *attempt, o *plugin.Outcome, verbatim *upstreamError) result {
	if x.out != nil {
		x.out.discard()
	}
	x.held.reset()
	if x.upload != nil && x.upload.tooBig.Load() {
		// The client's body broke the limit mid-stream; not the upstream's fault.
		a.errMsg = "client body exceeds the limit"
		return result{next: nextStop, fail: &failure{err: tooLarge(x.upload.limit.Load())}}
	}
	v := a.up.Provider.Classify(o)
	_ = x.p.d.Router.Report(x.ctx, a.cand, v, a.cand.UpstreamModel)
	x.noteUnserved(a, o, v)
	a.cause = v.Cause
	if o.Sent || v.Sent {
		a.wrote.Store(true)
	}
	if a.status == 0 {
		a.status = o.Status
	}
	gw := v.ClientErr
	if gw == nil {
		gw = o.Signal
	}
	if gw == nil {
		gw = ir.Errorf(ir.ErrUpstream, "upstream request failed")
		if o.Err != nil {
			gw = transportError(o.Err)
		}
	}
	if a.errMsg == "" {
		a.errMsg = gw.Message
	}
	f := &failure{err: gw}
	switch {
	case !a.cand.SameProtocol:
		// The client codec picks the status native to its protocol.
		cp := *gw
		cp.Status = 0
		f.err = &cp
	case verbatim != nil && gw.Status == verbatim.status:
		f.verbatim = verbatim
	}
	if x.ctx.Err() != nil || (x.upload != nil && x.upload.spent()) {
		// A streamed upload cannot be sent again.
		return result{next: nextStop, fail: f}
	}
	if v.Credential == plugin.CredRefresh && x.refresh(a) {
		return result{next: nextRefreshed, fail: f}
	}
	switch v.Retry {
	case plugin.RetryNext:
		return result{next: nextRetry, fail: f}
	case plugin.RetryDowngrade:
		if len(v.Downgrade) > 0 {
			x.downgrade[router.Key(a.cand)] = v.Downgrade
			return result{next: nextDowngrade, fail: f}
		}
		return result{next: nextRetry, fail: f}
	}
	return result{next: nextStop, fail: f}
}

// readBounded reads at most limit bytes.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(b)) > limit {
		return b[:limit], errBodyTooLarge
	}
	return b, err
}

var errBodyTooLarge = errors.New("pipeline: upstream body exceeds limit")
