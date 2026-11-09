package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/clientid"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// BatchProtocol is the protocol recorded for batch API requests.
const BatchProtocol = "batch"

// DefaultBatchMaxBody bounds a batch request body (an uploaded file, a
// message batch) when Deps.BatchMaxBody is 0.
const DefaultBatchMaxBody = 512 << 20

// Batch object kinds, as stored.
const (
	BatchKindFile         = "file"
	BatchKindBatch        = "batch"
	BatchKindMessageBatch = "message_batch"
)

// File roles.
const (
	roleInput  = "batch"
	roleOutput = "batch_output"
	roleError  = "batch_error"
)

// BatchObject records where a stateful upstream object lives and who owns
// it; see store.BatchObject.
type BatchObject struct {
	Kind, ID                             string
	UpstreamID, EndpointID, CredentialID string
	Protocol                             string // of the requests inside
	Model, UpstreamModel                 string
	// ServedModel is the member of the requested preset (Model) the
	// object was placed on; "" when a model was requested.
	ServedModel   string
	UserID, KeyID string
	// GroupID is the creator's group: its usage is billed to the creator.
	GroupID string
	// HoldID is the id a batch's money is held and charged under.
	HoldID string
	// InputTokens and OutputTokens estimate an input file's lines.
	InputTokens, OutputTokens int64
	Role, BatchID             string
	Object                    json.RawMessage
	CreatedAt, BilledAt       time.Time
}

// BatchQuery pages through one owner's objects of one kind, newest first.
type BatchQuery struct {
	Kind, UserID, KeyID string
	After, Before       string
	Limit               int
}

// BatchStore persists batch objects. GetBatch returns nil, nil for an
// unknown object.
type BatchStore interface {
	PutBatch(ctx context.Context, o *BatchObject) error
	GetBatch(ctx context.Context, kind, id string) (*BatchObject, error)
	ListBatches(ctx context.Context, q BatchQuery) ([]BatchObject, bool, error)
	SetBatchObject(ctx context.Context, kind, id string, object json.RawMessage) error
	// ClaimBatchBilling reports true to the first caller only.
	ClaimBatchBilling(ctx context.Context, kind, id string, at time.Time) (bool, error)
	// UnbilledBatches returns up to limit batches and message batches not
	// billed and not checked since checkedBefore, least recently checked
	// first, marking them checked at now.
	UnbilledBatches(ctx context.Context, checkedBefore, now time.Time, limit int) ([]BatchObject, error)
	DeleteBatch(ctx context.Context, kind, id string) error
}

// BatchFamily reports whether path is a batch API path and which codec
// renders its errors: "anthropic" for message batches, "openai-chat" for
// OpenAI files and batches. A "/v1" prefix is optional.
func (p *Pipeline) BatchFamily(path string) (string, bool) {
	r, ok := parseBatchPath(path)
	if !ok {
		return "", false
	}
	if r.anthropic {
		return "anthropic", true
	}
	return "openai-chat", true
}

// batchRoute is a parsed batch API path.
type batchRoute struct {
	anthropic bool
	resource  string // "files", "batches", "messages/batches"
	id        string
	action    string // "", "content", "cancel", "results"
}

func parseBatchPath(path string) (batchRoute, bool) {
	p := strings.TrimSuffix(path, "/")
	p = strings.TrimPrefix(p, "/v1")
	var r batchRoute
	switch {
	case p == "/messages/batches" || strings.HasPrefix(p, "/messages/batches/"):
		r.anthropic, r.resource, p = true, "messages/batches", strings.TrimPrefix(p, "/messages/batches")
	case p == "/files" || strings.HasPrefix(p, "/files/"):
		r.resource, p = "files", strings.TrimPrefix(p, "/files")
	case p == "/batches" || strings.HasPrefix(p, "/batches/"):
		r.resource, p = "batches", strings.TrimPrefix(p, "/batches")
	default:
		return r, false
	}
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if p == "" {
		return r, true
	}
	if len(segs) > 2 || segs[0] == "" {
		return r, false
	}
	r.id = segs[0]
	if len(segs) == 2 {
		r.action = segs[1]
		allowed := map[string]string{"files": "content", "batches": "cancel"}
		if r.anthropic {
			if r.action != "cancel" && r.action != "results" {
				return r, false
			}
		} else if allowed[r.resource] != r.action {
			return r, false
		}
	}
	return r, true
}

// ServeBatch serves the batch APIs for an authenticated caller: OpenAI
// files (purpose "batch") and batches, Anthropic message batches. Objects
// are created on one credential of an endpoint declaring `batches`,
// routed by the model of the requests inside; the relay records the
// object's credential and owner, so later calls reach the same
// credential and other callers get 404. Usage is billed to the creator
// when the results are read, through the relay or by the relay once the
// batch ended (SettleBatches), at the price rule's batch discount.
func (p *Pipeline) ServeBatch(w http.ResponseWriter, r *http.Request, caller *plugin.Caller) {
	p.inflight.add()
	defer p.inflight.done()
	if caller == nil {
		caller = &plugin.Caller{}
	}
	b := p.newBatchReq(w, r, caller)
	b.rec.ClientUA, b.rec.ClientIP = r.UserAgent(), p.d.ClientIP(r)
	clientid.Stamp(b.rec, r.Header)
	defer b.finish()
	route, ok := parseBatchPath(r.URL.Path)
	b.route = route
	if route.anthropic {
		b.codec, _ = protocol.Get("anthropic")
	}
	switch {
	case !ok:
		b.fail(ir.Errorf(ir.ErrNotFound, "no such endpoint"))
	case b.store == nil:
		b.fail(ir.Errorf(ir.ErrNotFound, "the batch API is not available on this relay"))
	case route.anthropic:
		b.serveAnthropic()
	default:
		b.serveOpenAI()
	}
}

func (p *Pipeline) newBatchReq(w http.ResponseWriter, r *http.Request, caller *plugin.Caller) *batchReq {
	b := &batchReq{p: p, w: w, r: r, caller: caller, started: p.d.Now(), snap: p.snapshot(), store: p.d.Batches}
	b.rec = &plugin.Record{ID: p.d.NewID(), Time: b.started, Caller: *caller, Protocol: BatchProtocol}
	b.codec, _ = protocol.Get("openai-chat")
	return b
}

// batchReq is the state of one batch API request.
type batchReq struct {
	p       *Pipeline
	w       http.ResponseWriter
	r       *http.Request
	caller  *plugin.Caller
	snap    *Snapshot
	store   BatchStore
	route   batchRoute
	codec   protocol.Codec // renders relay errors
	started time.Time
	rec     *plugin.Record
	att     *plugin.AttemptRecord
	// detach keeps upstream calls running after the client left, so a
	// results download is metered to its end.
	detach bool
	// quiet: calls made for a list are not the request's attempt.
	quiet bool
	// hooks is the call the request plugins ran on (admit), entered the
	// plugins whose finish hooks are owed.
	hooks   *plugin.Call
	entered []plugin.Plugin
	// background: made by the relay itself (SettleBatches); its record is
	// written only when it billed something.
	background, billed bool
}

// batchTarget is the credential a batch object lives on.
type batchTarget struct {
	cand plugin.Candidate
	// served is the preset member the candidate serves; "" for a model.
	served string
	up     *Upstream
	ep     *plugin.Endpoint
	cred   *plugin.Credential
}

func (b *batchReq) ctx() context.Context { return b.r.Context() }

// owns reports whether the caller owns o: the same user, or for keys
// without a user, the same key.
func (b *batchReq) owns(o *BatchObject) bool {
	if o == nil {
		return false
	}
	if b.caller.UserID != "" {
		return o.UserID == b.caller.UserID
	}
	return o.UserID == "" && o.KeyID == b.caller.KeyID
}

// lookup returns the caller's object, or answers 404.
func (b *batchReq) lookup(kind, id, what string) (*BatchObject, bool) {
	o, err := b.store.GetBatch(b.ctx(), kind, id)
	if err != nil {
		b.fail(ir.Errorf(ir.ErrInternal, "batch store: %v", err))
		return nil, false
	}
	if !b.owns(o) {
		b.fail(ir.Errorf(ir.ErrNotFound, "no %s found with id '%s'", what, id))
		return nil, false
	}
	b.rec.Model = o.Model
	return o, true
}

// targetOf resolves the credential a stored object lives on.
func (b *batchReq) targetOf(o *BatchObject) (*batchTarget, *ir.GwError) {
	t := b.target(plugin.Candidate{UpstreamID: o.UpstreamID, EndpointID: o.EndpointID, CredentialID: o.CredentialID,
		Protocol: o.Protocol, UpstreamModel: o.UpstreamModel})
	if t == nil {
		return nil, &ir.GwError{Kind: ir.ErrOverloaded, Status: http.StatusServiceUnavailable,
			Message: "the upstream credential holding this object is no longer configured"}
	}
	t.served = o.ServedModel
	return t, nil
}

func (b *batchReq) target(c plugin.Candidate) *batchTarget {
	up := b.snap.Upstreams[c.UpstreamID]
	if up == nil || up.Provider == nil || up.Config == nil || sender(up.Provider) != nil {
		return nil
	}
	cred := up.credential(c)
	if cred == nil {
		return nil
	}
	cred = withUpstreamProxy(up, cred)
	for i := range up.Config.Endpoints {
		if ep := &up.Config.Endpoints[i]; ep.ID == c.EndpointID {
			return &batchTarget{cand: c, up: up, ep: ep, cred: cred}
		}
	}
	return nil
}

// place picks the credential a new object is created on: an endpoint of
// the inner protocol declaring batches, for the model, as the router
// orders them.
func (b *batchReq) place(model, proto string) (*batchTarget, *ir.GwError) {
	b.rec.Model = model
	if model == "" {
		return nil, ir.Errorf(ir.ErrInvalidRequest, "the requests of a batch must name a model")
	}
	if v := b.snap.Visible; v != nil && !v(b.caller, model) {
		return nil, ir.Errorf(ir.ErrNotFound, "model %q not found", model)
	}
	q := router.Query{Model: model, Caller: *b.caller, ClientProtocol: proto,
		Needs:   ir.Needs{Modality: batchModality[proto]},
		Allowed: func(m string) bool { return b.snap.allows(b.caller, m) },
		Assess: func(ep plugin.Endpoint) (ir.Fidelity, []string) {
			if ep.Protocol == proto && ep.Caps.Batches {
				return ir.Lossless, nil
			}
			return ir.Impossible, []string{"batches"}
		}}
	plan, err := b.p.d.Router.Plan(b.ctx(), q)
	if err != nil {
		return nil, planError(err)
	}
	for _, c := range plan.Candidates {
		if t := b.target(c); t != nil {
			if sv := plan.Served(c); sv.Preset != "" {
				t.served = sv.Model
			}
			return t, nil
		}
	}
	return nil, ir.Errorf(ir.ErrOverloaded, "no upstream available for model %q", model)
}

var batchModality = map[string]ir.Modality{"openai-embeddings": ir.ModalityEmbeddings}

// gate asks the meter whether the caller may start paid work: a zero
// reservation, released at once.
func (b *batchReq) gate() bool {
	m := b.p.chain().Meter
	if m == nil || !supports(m, "meter.reserve") {
		return true
	}
	call := b.call()
	res, err := m.Reserve(call.WithPlugin(m.Info().ID), 0)
	if err != nil {
		b.fail(meterRefusal(err))
		return false
	}
	if res != nil && supports(m, "meter.refund") {
		if err := m.Refund(call.WithPlugin(m.Info().ID), res); err != nil {
			b.p.d.Logger.Warn("meter refund failed", "request", b.rec.ID, "err", err)
		}
	}
	return true
}

func (b *batchReq) call() *plugin.Call { return b.callAs(b.caller, b.rec.ID) }

// callAs is a plugin call for caller under id; it has no body.
func (b *batchReq) callAs(caller *plugin.Caller, id string) *plugin.Call {
	return b.callWithRequest(caller, id, b.rec.Model, func() (*ir.Request, error) { return nil, errBatchBody })
}

// send makes one upstream call on t's credential. path is appended to
// the endpoint's base URL. The returned body releases the credential's
// slot and the timers when closed.
func (b *batchReq) send(t *batchTarget, method, path string, body io.Reader, header http.Header) (*http.Response, error) {
	base := b.ctx()
	if b.detach {
		base = context.WithoutCancel(base)
	}
	release, ok, err := b.p.d.Router.Acquire(base, t.cand)
	if err != nil || !ok {
		return nil, &ir.GwError{Kind: ir.ErrOverloaded, Status: http.StatusServiceUnavailable, RetryAfter: time.Second,
			Message: "the upstream credential holding this batch is at capacity"}
	}
	start := b.p.d.Now()
	att := &plugin.AttemptRecord{N: 1, UpstreamID: t.cand.UpstreamID, EndpointID: t.ep.ID, CredentialID: t.cred.ID,
		Protocol: t.ep.Protocol, Model: t.served, UpstreamModel: t.cand.UpstreamModel, Tier: plugin.TierPassthrough,
		Diagnostic: &ir.Diagnostic{Fidelity: ir.Lossless, Notes: []string{"batch: " + method + " " + path}}}
	if b.att == nil && !b.quiet {
		b.att = att
	}
	watch := egress.Start(base, b.p.d.Timeouts.Override(t.up.Timeouts))
	done := func() {
		watch.Stop()
		release()
		att.Duration = b.p.d.Now().Sub(start)
	}
	var wrote atomic.Bool
	ctx := httptrace.WithClientTrace(watch.Context(), &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }})
	if body != nil {
		body = &countingReader{r: body, touch: watch.Touch}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(t.ep.BaseURL, "/")+path, body)
	if err != nil {
		done()
		return nil, err
	}
	owned := t.up.Provider.OwnedHeaders(t.up.Config)
	req.Header = egress.OutboundHeaders(b.r.Header, owned, true, nil)
	// A partial or conditional download would be metered as the whole
	// results; the relay always fetches all of it.
	for _, h := range []string{"Range", "If-Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
		req.Header.Del(h)
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	if body == nil {
		req.Header.Del("Content-Type")
	}
	egress.SetExtraHeaders(req.Header, t.up.Headers, owned)
	out, client, failure, gw := b.p.prepareRaw(ctx, t.up, t.ep, t.cred, req, watch)
	if gw != nil {
		done()
		att.Error = failure
		return nil, gw
	}
	resp, err := client.Do(out.Req)
	att.Sent = wrote.Load()
	if err != nil {
		err = watch.Err(err)
		done()
		att.Error = errText(err)
		return nil, err
	}
	att.Sent, att.Status, att.TTFB = true, resp.StatusCode, b.p.d.Now().Sub(start)
	resp.Body = &doneBody{r: watch.Body(resp.Body), c: resp.Body, done: done}
	return resp, nil
}

type doneBody struct {
	r    io.Reader
	c    io.Closer
	done func()
	once atomic.Bool
}

func (d *doneBody) Read(p []byte) (int, error) { return d.r.Read(p) }

func (d *doneBody) Close() error {
	err := d.c.Close()
	if d.once.CompareAndSwap(false, true) {
		d.done()
	}
	return err
}

// sendJSON makes a call with a JSON body (or none) and reads the answer.
func (b *batchReq) sendJSON(t *batchTarget, method, path string, body []byte) (*http.Response, []byte, error) {
	var rd io.Reader
	h := http.Header{}
	if body != nil {
		rd = strings.NewReader(string(body))
		h.Set("Content-Type", "application/json")
	}
	resp, err := b.send(t, method, path, rd, h)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := readBounded(resp.Body, b.p.d.MaxResponseBody)
	return resp, data, err
}

// reply writes an upstream answer to the client as is, body replaced.
func (b *batchReq) reply(resp *http.Response, body []byte) {
	h := b.w.Header()
	for k, vs := range egress.InboundResponseHeaders(resp.Header) {
		h[k] = vs
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	b.w.WriteHeader(resp.StatusCode)
	_, _ = b.w.Write(body)
	b.rec.Status = resp.StatusCode
	if b.att != nil {
		b.att.BytesOut = int64(len(body))
	}
}

// replyJSON writes a relay-built answer.
func (b *batchReq) replyJSON(v any) {
	data, _ := json.Marshal(v)
	b.w.Header().Set("Content-Type", "application/json")
	b.w.WriteHeader(http.StatusOK)
	_, _ = b.w.Write(data)
	b.rec.Status = http.StatusOK
}

// upstreamFailed answers a failed upstream call.
func (b *batchReq) upstreamFailed(err error) {
	if gw, ok := err.(*ir.GwError); ok {
		b.fail(gw)
		return
	}
	b.fail(transportError(err))
}

// fail answers with a relay error in the family's format.
func (b *batchReq) fail(gw *ir.GwError) {
	b.rec.ErrorKind, b.rec.Error = gw.Kind, gw.Message
	if b.att != nil && b.att.Error == "" {
		b.att.Error = gw.Message
	}
	status, h, body := b.codec.EncodeError(gw)
	if gw.Status != 0 {
		status = gw.Status
	}
	b.rec.Status = status
	if b.ctx().Err() != nil {
		b.rec.Status = statusClientClosed
		return
	}
	dst := b.w.Header()
	for k, vs := range h {
		dst[k] = vs
	}
	if dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "application/json")
	}
	if gw.RetryAfter > 0 {
		dst.Set("Retry-After", strconv.Itoa(max(int(gw.RetryAfter.Seconds()), 1)))
	}
	b.w.WriteHeader(status)
	_, _ = b.w.Write(body)
}

func (b *batchReq) finish() {
	if b.hooks != nil {
		res := &plugin.Result{Status: b.rec.Status, Usage: b.rec.Usage}
		for i := len(b.entered) - 1; i >= 0; i-- {
			if h, ok := b.entered[i].(plugin.FinishHook); ok && supports(b.entered[i], "middleware.onFinish") {
				h.OnFinish(b.hooks.WithPlugin(b.entered[i].Info().ID), res)
			}
		}
	}
	if b.background && !b.billed {
		return
	}
	if b.att != nil {
		b.rec.Attempts = append([]plugin.AttemptRecord{*b.att}, b.rec.Attempts...)
		b.rec.TTFB = b.att.TTFB
	}
	for _, a := range b.rec.Attempts {
		b.rec.CostMicro += a.CostMicro
	}
	b.rec.Duration = b.p.d.Now().Sub(b.started)
	if m := b.p.d.Metrics; m != nil {
		m.Request(b.rec)
	}
	if q := b.p.d.Records; q != nil && !q.Enqueue(b.rec) {
		b.p.d.Logger.Warn("request record dropped by best-effort sinks", "request", b.rec.ID)
	}
}

// owner fills o's owner and placement from the caller and t.
func (b *batchReq) owner(o *BatchObject, t *batchTarget) *BatchObject {
	o.UpstreamID, o.EndpointID, o.CredentialID = t.cand.UpstreamID, t.ep.ID, t.cred.ID
	o.UpstreamModel, o.ServedModel = t.cand.UpstreamModel, t.served
	o.UserID, o.KeyID, o.GroupID = b.caller.UserID, b.caller.KeyID, b.caller.GroupID
	o.CreatedAt = b.p.d.Now()
	return o
}

// save stores an object, logging (not failing) on error: the client
// already has the upstream's answer.
func (b *batchReq) save(o *BatchObject) {
	if err := b.store.PutBatch(context.WithoutCancel(b.ctx()), o); err != nil {
		b.p.d.Logger.Error("batch object not recorded", "kind", o.Kind, "id", o.ID, "err", err)
	}
}

func (b *batchReq) refresh(o *BatchObject, body []byte) {
	if err := b.store.SetBatchObject(context.WithoutCancel(b.ctx()), o.Kind, o.ID, body); err != nil {
		b.p.d.Logger.Warn("batch object not updated", "kind", o.Kind, "id", o.ID, "err", err)
	}
}

// listLimit reads a page size: def when absent, clamped to [1, max].
func listLimit(r *http.Request, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, max)
}

func ok2xx(status int) bool { return status >= 200 && status < 300 }
