package pipeline

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/clientid"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// NativeProtocol is the protocol recorded for native passthrough requests.
const NativeProtocol = "native"

// DefaultNativeMaxBody bounds a native request body when Native.MaxBody is 0.
const DefaultNativeMaxBody = 512 << 20

// Native configures /native/<upstream>/<path> passthrough.
type Native struct {
	// Authorize decides whether caller may reach upstreamID; it returns
	// the error to answer with, or nil. A nil Authorize denies everyone.
	Authorize func(caller *plugin.Caller, r *http.Request, upstreamID string) *ir.GwError
	// MaxBody bounds the request body (DefaultNativeMaxBody when 0).
	MaxBody int64
}

// ServeNative forwards one authenticated request verbatim to an upstream:
// any method, path and query, appended to the base URL of the upstream's
// endpoint. ref is the upstream id, or "<upstream>:<endpoint>" to pick an
// endpoint other than the first; path is the escaped remainder of the URL
// path ("/files"). Bodies stream in both directions without buffering.
//
// Only the core's egress rules apply: client credentials and other
// dangerous headers are dropped, the provider sets its auth, and the
// credential's fixed proxy and the SSRF guard hold. Request plugins do not
// run, nothing is metered or billed (usage is unknown), and failures do not
// change the credential's health; the request log gets one record with
// protocol "native", tier passthrough, status, bytes and duration.
func (p *Pipeline) ServeNative(w http.ResponseWriter, r *http.Request, caller *plugin.Caller, ref, path string) {
	p.inflight.add()
	defer p.inflight.done()
	if caller == nil {
		caller = &plugin.Caller{}
	}
	n := &native{p: p, w: w, r: r, caller: caller, started: p.d.Now(), snap: p.snapshot()}
	n.rec = &plugin.Record{ID: p.d.NewID(), Time: n.started, Caller: *caller, Protocol: NativeProtocol,
		ClientUA: r.UserAgent(), ClientIP: p.d.ClientIP(r)}
	clientid.Stamp(n.rec, r.Header)
	defer n.finish()
	n.serve(ref, path)
}

// native is the state of one native passthrough request.
type native struct {
	p       *Pipeline
	w       http.ResponseWriter
	r       *http.Request
	caller  *plugin.Caller
	snap    *Snapshot
	started time.Time
	rec     *plugin.Record
	att     *plugin.AttemptRecord
}

func (n *native) serve(ref, path string) {
	upID, epID := n.resolve(ref)
	if az := n.p.d.Native.Authorize; az == nil {
		n.fail(ir.Errorf(ir.ErrPermission, "native passthrough is not allowed for this key"))
		return
	} else if gw := az(n.caller, n.r, upID); gw != nil {
		n.fail(gw)
		return
	}
	tu, known := router.Upstream{}, false
	if n.snap.Table != nil {
		tu, known = n.snap.Table.Upstreams[upID]
	}
	up := n.snap.Upstreams[upID]
	switch {
	case !known && up == nil:
		n.fail(ir.Errorf(ir.ErrNotFound, "upstream %q not found", upID))
		return
	case !tu.Enabled || up == nil || up.Provider == nil:
		n.fail(ir.Errorf(ir.ErrPermission, "upstream %q is disabled", upID))
		return
	}
	if epID == "" && len(tu.Endpoints) > 0 {
		epID = tu.Endpoints[0].ID
	}
	var ep *plugin.Endpoint
	for i := range tu.Endpoints {
		if tu.Endpoints[i].ID == epID {
			ep = &tu.Endpoints[i]
		}
	}
	if ep == nil {
		n.fail(ir.Errorf(ir.ErrNotFound, "upstream %q has no endpoint %q", upID, epID))
		return
	}
	if sender(up.Provider) != nil {
		n.fail(&ir.GwError{Kind: ir.ErrInvalidRequest, Status: http.StatusNotImplemented,
			Message: "upstream " + upID + " is not reached over HTTP; native passthrough is unavailable"})
		return
	}
	plan, err := n.p.d.Router.PlanUpstream(n.r.Context(), upID, ep.ID, *n.caller)
	if err != nil {
		n.fail(nativePlanError(err, upID))
		return
	}
	for _, cand := range plan.Candidates {
		release, ok, err := n.p.d.Router.Acquire(n.r.Context(), cand)
		if err != nil || !ok {
			continue
		}
		n.forward(cand, up, ep, path)
		release()
		return
	}
	e := ir.Errorf(ir.ErrOverloaded, "every credential of upstream %q is at capacity", upID)
	e.RetryAfter = time.Second
	n.fail(e)
}

// resolve splits "<upstream>:<endpoint>" unless the whole ref names an
// upstream.
func (n *native) resolve(ref string) (upstream, endpoint string) {
	if _, ok := n.snap.Upstreams[ref]; ok {
		return ref, ""
	}
	if n.snap.Table != nil {
		if _, ok := n.snap.Table.Upstreams[ref]; ok {
			return ref, ""
		}
	}
	if i := strings.LastIndexByte(ref, ':'); i > 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

// forward sends the request with one credential and streams the answer.
func (n *native) forward(cand plugin.Candidate, up *Upstream, ep *plugin.Endpoint, path string) {
	cred := up.credential(cand)
	if cred == nil {
		n.fail(ir.Errorf(ir.ErrInternal, "credential %s of upstream %s is not configured", cand.CredentialID, cand.UpstreamID))
		return
	}
	cred = withUpstreamProxy(up, cred)
	start := n.p.d.Now()
	n.att = &plugin.AttemptRecord{N: 1, UpstreamID: cand.UpstreamID, EndpointID: ep.ID, CredentialID: cred.ID,
		Protocol: ep.Protocol, Tier: plugin.TierPassthrough,
		Diagnostic: &ir.Diagnostic{Fidelity: ir.Lossless, Notes: []string{"native: " + n.r.Method + " " + path}}}
	defer func() { n.att.Duration = n.p.d.Now().Sub(start) }()

	target, err := nativeURL(ep.BaseURL, path, n.r.URL.RawQuery)
	if err != nil {
		n.fail(ir.Errorf(ir.ErrInvalidRequest, "invalid path: %v", err))
		return
	}
	watch := egress.Start(n.r.Context(), n.p.d.Timeouts.Override(up.Timeouts))
	defer watch.Stop()
	var wrote atomic.Bool
	ctx := httptrace.WithClientTrace(watch.Context(), &httptrace.ClientTrace{
		WroteHeaders: func() { wrote.Store(true) },
	})
	var body *countingReader
	var reqBody io.Reader
	if n.r.Body != nil && n.r.Body != http.NoBody && n.r.ContentLength != 0 {
		max := n.p.d.Native.MaxBody
		if max <= 0 {
			max = DefaultNativeMaxBody
		}
		if n.r.ContentLength > max {
			n.fail(&ir.GwError{Kind: ir.ErrInvalidRequest, Status: http.StatusRequestEntityTooLarge,
				Message: "request body exceeds " + strconv.FormatInt(max, 10) + " bytes"})
			return
		}
		body = &countingReader{r: http.MaxBytesReader(n.w, n.r.Body, max), touch: watch.Touch}
		reqBody = body
	}
	req, err := http.NewRequestWithContext(ctx, n.r.Method, target, reqBody)
	if err != nil {
		n.fail(ir.Errorf(ir.ErrInvalidRequest, "invalid request: %v", err))
		return
	}
	if reqBody != nil {
		req.ContentLength = n.r.ContentLength
	}
	owned := up.Provider.OwnedHeaders(up.Config)
	req.Header = egress.OutboundHeaders(n.r.Header, owned, true, nil)
	egress.SetExtraHeaders(req.Header, up.Headers, owned)
	out, client, failure, gw := n.p.prepareRaw(ctx, up, ep, cred, req, watch)
	if gw != nil {
		n.att.Error = failure
		n.fail(gw)
		return
	}
	resp, err := client.Do(out.Req)
	n.att.Sent = wrote.Load()
	if body != nil {
		n.att.BytesIn = body.n
	}
	if err != nil {
		err = watch.Err(err)
		n.att.Error = errText(err)
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			n.fail(&ir.GwError{Kind: ir.ErrInvalidRequest, Status: http.StatusRequestEntityTooLarge,
				Message: "request body exceeds " + strconv.FormatInt(mbe.Limit, 10) + " bytes"})
			return
		}
		n.fail(transportError(err))
		return
	}
	defer resp.Body.Close()
	n.att.Sent = true
	n.att.Status = resp.StatusCode
	n.att.TTFB = n.p.d.Now().Sub(start)
	n.rec.TTFB = n.att.TTFB
	n.rec.Status = resp.StatusCode
	n.rec.Stream = strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")

	h := n.w.Header()
	for k, vs := range egress.InboundResponseHeaders(resp.Header) {
		h[k] = vs
	}
	if resp.ContentLength >= 0 && !resp.Uncompressed {
		h.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	n.w.WriteHeader(resp.StatusCode)
	copied, err := copyFlushing(n.w, watch.Body(resp.Body))
	n.att.BytesOut = copied
	if body != nil {
		n.att.BytesIn = body.n
	}
	if err != nil {
		err = watch.Err(err)
		n.att.Error = errText(err)
		if n.r.Context().Err() != nil {
			n.rec.Status, n.rec.ErrorKind = statusClientClosed, ErrClientClosed
		} else {
			gw := transportError(err)
			n.rec.ErrorKind, n.rec.Error = gw.Kind, gw.Message
		}
	}
}

// fail answers with a relay error. The body carries both the OpenAI and the
// Anthropic error shape, since native clients are those vendors' SDKs.
func (n *native) fail(gw *ir.GwError) {
	status := gw.Status
	if status == 0 {
		switch gw.Kind {
		case ir.ErrUpstream:
			status = http.StatusBadGateway
		case ir.ErrOverloaded:
			status = http.StatusServiceUnavailable
		default:
			status = kindStatus(gw.Kind)
		}
	}
	n.rec.Status, n.rec.ErrorKind, n.rec.Error = status, gw.Kind, gw.Message
	if n.att != nil && n.att.Error == "" {
		n.att.Error = gw.Message
	}
	if n.r.Context().Err() != nil {
		n.rec.Status = statusClientClosed
		return
	}
	h := n.w.Header()
	h.Set("Content-Type", "application/json")
	if gw.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(math.Ceil(gw.RetryAfter.Seconds()))))
	}
	n.w.WriteHeader(status)
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": string(gw.Kind), "message": gw.Message}})
	_, _ = n.w.Write(b)
}

func (n *native) finish() {
	if n.att != nil {
		n.rec.Attempts = []plugin.AttemptRecord{*n.att}
	}
	n.rec.Duration = n.p.d.Now().Sub(n.started)
	if m := n.p.d.Metrics; m != nil {
		m.Request(n.rec)
	}
	if q := n.p.d.Records; q != nil && !q.Enqueue(n.rec) {
		n.p.d.Logger.Warn("request record dropped by best-effort sinks", "request", n.rec.ID)
	}
}

// nativePlanError maps a routing failure of an upstream to the client error.
func nativePlanError(err error, upstream string) *ir.GwError {
	var pe *router.PlanError
	if !errors.As(err, &pe) {
		return ir.Errorf(ir.ErrInternal, "routing failed")
	}
	switch pe.Kind {
	case router.ErrNoRoute:
		return ir.Errorf(ir.ErrPermission, "no credential of upstream %q is available to this key", upstream)
	case router.ErrCooling:
		e := ir.Errorf(ir.ErrRateLimited, "upstream %q is cooling down; retry later", upstream)
		if !pe.RetryAt.IsZero() {
			e.RetryAfter = max(time.Until(pe.RetryAt).Round(time.Second), time.Second)
		}
		return e
	case router.ErrBusy:
		e := ir.Errorf(ir.ErrOverloaded, "every credential of upstream %q is at capacity", upstream)
		e.RetryAfter = time.Second
		return e
	}
	return ir.Errorf(ir.ErrOverloaded, "no usable credential for upstream %q", upstream)
}

// credentialQueryParams are query parameters a client may authenticate
// with (see plugins/auth/apikey). They carry the platform key, so they are
// never forwarded upstream; the upstream gets the relay's credential.
var credentialQueryParams = []string{"key"}

// nativeURL appends an escaped path and the client's query, minus any
// credential parameter, to a base URL that may carry its own path. A path
// segment that decodes to "." or ".." is refused: an upstream that
// normalises paths would otherwise let it climb out of the base URL's
// prefix, which operators may use to scope access.
func nativeURL(base, path, rawQuery string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if err := checkDotSegments(path); err != nil {
		return "", err
	}
	u, err := url.Parse(strings.TrimRight(base, "/") + path)
	if err != nil {
		return "", err
	}
	if rawQuery = stripCredentialParams(rawQuery); rawQuery != "" {
		if u.RawQuery != "" {
			u.RawQuery += "&" + rawQuery
		} else {
			u.RawQuery = rawQuery
		}
	}
	return u.String(), nil
}

// checkDotSegments refuses a path with a "." or ".." segment in any form a
// server or gateway in front of the upstream might resolve: plain, percent-
// encoded, behind an encoded slash or backslash ("a%2f..%2fb"), or with
// Tomcat's ";" path parameters ("..;"). nginx and Envoy can decode %2F
// before resolving dot segments, so checking only whole raw segments is not
// enough.
func checkDotSegments(path string) error {
	for _, seg := range strings.Split(path, "/") {
		dec, err := url.PathUnescape(seg)
		if err != nil {
			return err
		}
		for _, part := range strings.FieldsFunc(dec, func(r rune) bool { return r == '/' || r == '\\' }) {
			part, _, _ = strings.Cut(part, ";")
			if part == "." || part == ".." {
				return errors.New("dot segments are not allowed")
			}
		}
	}
	return nil
}

// stripCredentialParams removes credentialQueryParams from a raw query,
// keeping every other parameter byte for byte and in order.
func stripCredentialParams(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	kept := make([]string, 0, 4)
	for _, part := range strings.Split(rawQuery, "&") {
		name, _, _ := strings.Cut(part, "=")
		if dec, err := url.QueryUnescape(name); err == nil && slices.Contains(credentialQueryParams, dec) {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, "&")
}

// countingReader counts request body bytes and reports progress, so a
// long upload does not trip the first-byte timer.
type countingReader struct {
	r     io.Reader
	n     int64
	touch func()
}

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	if k > 0 {
		c.n += int64(k)
		c.touch()
	}
	return k, err
}

// copyFlushing copies the upstream body to the client, flushing after
// every read so event streams arrive as they are produced.
func copyFlushing(w http.ResponseWriter, r io.Reader) (int64, error) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	var total int64
	for {
		k, err := r.Read(buf)
		if k > 0 {
			if _, werr := w.Write(buf[:k]); werr != nil {
				return total, werr
			}
			total += int64(k)
			_ = rc.Flush()
		}
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
