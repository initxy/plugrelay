package pipeline

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/clientid"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/protocol/openairesponses"
)

// WebSocket session kinds handed over by the HTTP server.
const (
	WSRealtime  = "realtime"  // GET /v1/realtime?model=…
	WSResponses = "responses" // GET /v1/responses (Responses API WebSocket mode)
)

// Protocols recorded for WebSocket sessions and their responses.
const (
	RealtimeProtocol    = "openai-realtime"
	ResponsesWSProtocol = "openai-responses-ws"
)

// Defaults for zero WebSocket fields.
const (
	DefaultWSMaxSession      = 30 * time.Minute
	DefaultWSIdle            = 5 * time.Minute
	DefaultWSMaxMessage      = 16 << 20
	DefaultWSResponseReserve = 4096
	DefaultWSDrain           = 10 * time.Second
	DefaultWSReadyTimeout    = 10 * time.Second
)

// InsecureKeyProtocol prefixes the subprotocol browsers carry an API key in
// ("openai-insecure-api-key.<key>", matched ignoring case); it is never
// forwarded.
const InsecureKeyProtocol = "openai-insecure-api-key."

// WebSocket bounds WebSocket sessions.
type WebSocket struct {
	// MaxSession ends a session after this long (DefaultWSMaxSession).
	// Keep it within the router's in-flight slot lifetime (30m), or a long
	// session stops counting toward its credential's concurrency cap.
	MaxSession time.Duration
	// Idle ends a session when no message crossed in either direction for
	// this long (DefaultWSIdle).
	Idle time.Duration
	// MaxMessage bounds one message in either direction (DefaultWSMaxMessage);
	// a larger one closes the session with 1009.
	MaxMessage int64
	// ResponseReserve is the output tokens the meter holds for a response
	// whose session or request sets no cap (DefaultWSResponseReserve).
	ResponseReserve int64
	// Drain keeps the upstream open this long after the client left with
	// responses unfinished, to receive their usage (DefaultWSDrain); what is
	// still open then is billed from an estimate.
	Drain time.Duration
	// ReadyTimeout bounds the wait for a Realtime upstream's
	// session.created while client events wait for it
	// (DefaultWSReadyTimeout); past it the session is closed.
	ReadyTimeout time.Duration
	// Authorize checks a handshake before anything is dialed (the access
	// allowlists of the source address and client); nil admits all.
	Authorize func(caller *plugin.Caller, r *http.Request) *ir.GwError
}

func (w WebSocket) withDefaults() WebSocket {
	if w.MaxSession <= 0 {
		w.MaxSession = DefaultWSMaxSession
	}
	if w.Idle <= 0 {
		w.Idle = DefaultWSIdle
	}
	if w.MaxMessage <= 0 {
		w.MaxMessage = DefaultWSMaxMessage
	}
	if w.ResponseReserve <= 0 {
		w.ResponseReserve = DefaultWSResponseReserve
	}
	if w.Drain <= 0 {
		w.Drain = DefaultWSDrain
	}
	if w.ReadyTimeout <= 0 {
		w.ReadyTimeout = DefaultWSReadyTimeout
	}
	return w
}

// ServeWebSocket relays one authenticated WebSocket session to an upstream
// WebSocket: kind WSRealtime (OpenAI Realtime, routed by the model query
// parameter to an endpoint declaring realtime) or WSResponses (Responses
// API WebSocket mode, to an endpoint declaring responses_websocket).
//
// The upstream is dialed through the core egress with the provider's
// credentials before the client's upgrade is accepted, so routing and
// upstream handshake errors reach the client as HTTP errors. Messages are
// relayed in order, unchanged except the model name (set to the upstream
// model) and redacted user text. Request plugins run once for a Realtime session at
// the handshake (no body) and for each response.create of a Responses
// session; content checks also run on each Realtime event carrying text,
// and event hooks on output text (websocket_content.go). The
// meter holds for each response (its output cap at the model's price);
// each response's usage (from response.done, or response.completed /
// failed / incomplete) is settled and recorded as its own request, an
// unfinished one from an estimate, and the session gets one record with
// its duration and bytes. The credential's concurrency slot is held for
// the whole session.
//
// A Responses WebSocket handshake gets 426, so the client falls back to
// HTTP, unless one upstream with responses_websocket serves every model the
// caller can use: the model arrives only with the first response.create,
// after the upstream must already be chosen.
func (p *Pipeline) ServeWebSocket(w http.ResponseWriter, r *http.Request, caller *plugin.Caller, kind string) {
	p.inflight.add()
	defer p.inflight.done()
	if caller == nil {
		caller = &plugin.Caller{}
	}
	s := &wsSession{p: p, w: w, r: r, caller: caller, kind: kind, snap: p.snapshot(), lim: p.d.WebSocket.withDefaults(),
		id: p.d.NewID(), started: p.d.Now(), meter: p.chain().Meter, clientIP: p.d.ClientIP(r), routes: map[string]string{},
		settled: make(chan struct{}, 1)}
	s.proto = RealtimeProtocol
	if kind == WSResponses {
		s.proto = ResponsesWSProtocol
	}
	s.rec = &plugin.Record{ID: s.id, Time: s.started, Caller: *caller, Protocol: s.proto, Stream: true,
		SessionID: s.id, ClientUA: r.UserAgent(), ClientIP: s.clientIP}
	clientid.Stamp(s.rec, r.Header)
	defer s.finish()
	s.serve()
}

// wsSession is the state of one WebSocket session.
type wsSession struct {
	p        *Pipeline
	w        http.ResponseWriter
	r        *http.Request
	caller   *plugin.Caller
	snap     *Snapshot
	lim      WebSocket
	meter    plugin.Meter
	kind     string
	proto    string
	id       string
	started  time.Time
	clientIP string
	rec      *plugin.Record
	noRecord bool // a refused Responses upgrade (426) is not a request

	// served maps a Realtime candidate's key to the member of the
	// requested preset it serves; nil when a model was requested.
	served map[string]string
	// Upstream of the session, fixed once dialed.
	cand plugin.Candidate
	up   *Upstream
	ep   *plugin.Endpoint
	cred *plugin.Credential
	att  *plugin.AttemptRecord
	// routes maps the client models a Responses session may use to their
	// upstream model on this upstream.
	routes map[string]string

	bytesIn, bytesOut atomic.Int64

	// clientGone: the client left; upstream events are read but not
	// forwarded while open responses finish.
	clientGone atomic.Bool
	// settled is signalled whenever a response is settled or dropped.
	settled chan struct{}
	// hookCall and entered are the request plugins run for a Realtime
	// session, whose finish hooks run when it ends.
	hookCall *plugin.Call
	entered  []plugin.Plugin
	// bodyMutators are the body mutators, mutators those editing user text
	// (run on the text of Realtime events), eventHooks the event hooks run
	// over output text. Fixed at the handshake.
	bodyMutators, mutators, eventHooks []plugin.Plugin
	// uc is the upstream connection, once dialed.
	uc *websocket.Conn
	// Client pump only: Realtime events checked, the verdict on audio, and
	// the transcription models admitted.
	checks       int
	audioChecked bool
	audioRefusal *ir.GwError
	transcribeOK map[string]bool
	// clientTranscribe is the transcription model the client last named.
	clientTranscribe string
	// ready is closed once the upstream reported the session it created
	// and the relay's own session.update went out (or the upstream
	// ended); client events wait for it when the body mutators or the
	// transcription check need that session. readyOnce closes it; outCap
	// is the output cap the body mutators set (0 = none), fixed then.
	ready     chan struct{}
	readyOnce sync.Once
	outCap    int64
	// screen is the upstream pump's output checking.
	screen wsScreen

	mu sync.Mutex // guards the fields below
	// model is the client model: the realtime query parameter, or the last
	// response.create's model.
	model string
	// open are the responses not yet settled, oldest first.
	open []*wsResponse
	// next is the meter's hold for the next Realtime response.
	next *wsResponse
	// awaiting counts Realtime response.create events not yet answered.
	awaiting int
	// maxOut is the Realtime session's output cap (0 = none);
	// transcribeModel its input transcription model.
	maxOut          int64
	transcribeModel string
	// inTokens estimates the Realtime conversation's input so far (text,
	// audio at audioRate bytes per second, the last usage reported).
	inTokens       int64
	audioRate      int64
	holds          int
	responses      int
	transcriptions int
	usage          ir.Usage
	closeNote      string
	endKind        ir.ErrorKind

	// notes are the content checks' notes on Realtime client events, for
	// the session record.
	notes []string

	// refusal is the meter's answer when it refused the next response.
	refusal *ir.GwError
	// dialCancel ends the context the upstream connection was dialed with.
	dialCancel context.CancelFunc
}

func (s *wsSession) serve() {
	if gw := checkUpgrade(s.r); gw != nil {
		if gw.Status == http.StatusUpgradeRequired {
			s.w.Header().Set("Sec-WebSocket-Version", "13")
		}
		s.fail(gw)
		return
	}
	if a := s.lim.Authorize; a != nil {
		if gw := a(s.caller, s.r); gw != nil {
			s.fail(gw)
			return
		}
	}
	var cands []plugin.Candidate
	switch s.kind {
	case WSRealtime:
		model := s.r.URL.Query().Get("model")
		if model == "" {
			s.fail(ir.Errorf(ir.ErrInvalidRequest, "the model query parameter is required"))
			return
		}
		s.model, s.rec.Model = model, model
		if v := s.snap.Visible; v != nil && !v(s.caller, model) {
			s.fail(ir.Errorf(ir.ErrNotFound, "model %q not found", model))
			return
		}
		body := &wsBody{}
		s.hookCall = s.newCallWith(s.id, model, body)
		var gw *ir.GwError
		if s.entered, _, gw = s.admit(s.hookCall, body); gw != nil {
			s.fail(gw)
			return
		}
		plan, err := s.p.d.Router.Plan(s.r.Context(), router.Query{Model: model, Caller: *s.caller,
			ClientProtocol: "openai-responses",
			Allowed:        func(m string) bool { return s.snap.allows(s.caller, m) },
			Assess: func(ep plugin.Endpoint) (ir.Fidelity, []string) {
				if ep.Caps.Realtime {
					return ir.Lossless, nil
				}
				return ir.Impossible, []string{"realtime"}
			}})
		if err != nil {
			s.fail(planError(err))
			return
		}
		cands = plan.Candidates
		for _, c := range plan.Candidates {
			if sv := plan.Served(c); sv.Preset != "" {
				if s.served == nil {
					s.served = map[string]string{}
				}
				s.served[router.Key(c)] = sv.Model
			}
		}
	case WSResponses:
		upIDs := s.responsesUpstreams()
		if len(upIDs) == 0 {
			s.noRecord = true
			status, body := openairesponses.UpgradeRequired()
			s.w.Header().Set("Content-Type", "application/json")
			s.w.WriteHeader(status)
			_, _ = s.w.Write(body)
			return
		}
		var last error
		for _, u := range upIDs {
			plan, err := s.p.d.Router.PlanUpstream(s.r.Context(), u.upstream, u.endpoint, *s.caller)
			if err != nil {
				last = err
				continue
			}
			cands = append(cands, plan.Candidates...)
		}
		if len(cands) == 0 {
			if last == nil {
				s.fail(ir.Errorf(ir.ErrOverloaded, "no usable credential for upstream %q", upIDs[0].upstream))
			} else {
				s.fail(nativePlanError(last, upIDs[0].upstream))
			}
			return
		}
	default:
		s.fail(ir.Errorf(ir.ErrNotFound, "no such WebSocket endpoint"))
		return
	}
	s.contentPlugins()
	if s.kind == WSRealtime {
		if !s.reserveNext(true) {
			return
		}
	} else if s.meter != nil {
		// Each response.create is held for on its own; this only refuses a
		// caller who cannot pay for anything.
		call := s.newCall(s.nextHoldID(), "")
		res, gw := s.reserveOn(call, 0, 0)
		if gw != nil {
			s.fail(gw)
			return
		}
		s.refundHold(call, res)
	}
	cc, uc, release := s.connect(cands)
	if cc == nil {
		s.closeOpen("")
		return
	}
	defer release()
	s.relay(cc, uc)
	s.mu.Lock()
	note := s.closeNote
	s.mu.Unlock()
	s.guard("settlement", func() { s.closeOpen(note) })
}

// checkUpgrade refuses a malformed WebSocket handshake before anything is
// dialed (RFC 6455 section 4.2.1).
func checkUpgrade(r *http.Request) *ir.GwError {
	if r.Method != http.MethodGet || !headerHasToken(r.Header, "Connection", "upgrade") || !headerHasToken(r.Header, "Upgrade", "websocket") {
		return ir.Errorf(ir.ErrInvalidRequest, "a WebSocket upgrade must be a GET with Connection: Upgrade and Upgrade: websocket")
	}
	if v := r.Header.Get("Sec-WebSocket-Version"); v != "13" {
		gw := ir.Errorf(ir.ErrInvalidRequest, "unsupported Sec-WebSocket-Version %q; use 13", v)
		gw.Status = http.StatusUpgradeRequired
		return gw
	}
	if k, err := base64.StdEncoding.DecodeString(r.Header.Get("Sec-WebSocket-Key")); err != nil || len(k) != 16 {
		return ir.Errorf(ir.ErrInvalidRequest, "invalid Sec-WebSocket-Key")
	}
	return nil
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

type upstreamEndpoint struct{ upstream, endpoint string }

// responsesUpstreams lists, in id order, the upstreams (with their
// Responses WebSocket endpoint) that serve every model the caller may use.
func (s *wsSession) responsesUpstreams() []upstreamEndpoint {
	t := s.snap.Table
	if t == nil {
		return nil
	}
	var models []string
	for name, m := range t.Models {
		if !m.Enabled || (s.snap.Visible != nil && !s.snap.Visible(s.caller, name)) {
			continue
		}
		if slices.ContainsFunc(routesOf(t, name), func(rt router.Route) bool { return groupAllowed(rt.Groups, s.caller.GroupID) }) {
			models = append(models, name)
		}
	}
	if len(models) == 0 {
		return nil
	}
	ids := make([]string, 0, len(t.Upstreams))
	for id := range t.Upstreams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []upstreamEndpoint
	for _, id := range ids {
		u := t.Upstreams[id]
		pu := s.snap.Upstreams[id]
		if !u.Enabled || pu == nil || pu.Provider == nil || sender(pu.Provider) != nil {
			continue
		}
		epID := ""
		for _, ep := range u.Endpoints {
			if ep.Caps.ResponsesWebSocket && ep.Protocol == "openai-responses" {
				epID = ep.ID
				break
			}
		}
		if epID == "" {
			continue
		}
		all := true
		for _, m := range models {
			if _, ok := modelRoute(t, m, id, s.caller.GroupID); !ok {
				all = false
				break
			}
		}
		if all {
			out = append(out, upstreamEndpoint{id, epID})
		}
	}
	return out
}

// modelRoute returns the upstream model a client model maps to on one
// upstream, for a caller's group.
func modelRoute(t *router.Table, model, upstreamID, group string) (string, bool) {
	for _, rt := range routesOf(t, model) {
		if rt.UpstreamID == upstreamID && groupAllowed(rt.Groups, group) {
			return rt.UpstreamModel, true
		}
	}
	return "", false
}

func groupAllowed(groups []string, g string) bool {
	return len(groups) == 0 || (g != "" && slices.Contains(groups, g))
}

// connect takes a slot and dials the first candidate that answers, then
// accepts the client. On failure the client has had its HTTP error and cc
// is nil.
func (s *wsSession) connect(cands []plugin.Candidate) (cc, uc *websocket.Conn, release func()) {
	var fail *ir.GwError
	var verbatim *upstreamError
	busy := false
	for _, cand := range cands {
		rel, ok, err := s.p.d.Router.Acquire(s.r.Context(), cand)
		if err != nil || !ok {
			busy = true
			continue
		}
		conn, resp, gw, ue, retry := s.dial(cand)
		if conn != nil {
			if cc = s.accept(conn, resp); cc == nil {
				_ = conn.Close(websocket.StatusGoingAway, "client handshake failed")
				rel()
				return nil, nil, nil
			}
			return cc, conn, rel
		}
		rel()
		fail, verbatim = gw, ue
		if !retry {
			break
		}
	}
	switch {
	case verbatim != nil:
		s.failVerbatim(verbatim)
	case fail != nil:
		s.fail(fail)
	case busy:
		e := ir.Errorf(ir.ErrOverloaded, "every credential for model %q is at capacity", s.model)
		e.RetryAfter = time.Second
		s.fail(e)
	default:
		s.fail(ir.Errorf(ir.ErrOverloaded, "no upstream available"))
	}
	return nil, nil, nil
}

// dial opens the upstream WebSocket for one candidate. On failure it
// returns the client error (or the upstream's own answer) and whether
// another candidate may be tried.
func (s *wsSession) dial(cand plugin.Candidate) (*websocket.Conn, *http.Response, *ir.GwError, *upstreamError, bool) {
	up := s.snap.Upstreams[cand.UpstreamID]
	var ep *plugin.Endpoint
	if t := s.snap.Table; t != nil {
		for i, e := range t.Upstreams[cand.UpstreamID].Endpoints {
			if e.ID == cand.EndpointID {
				ep = &t.Upstreams[cand.UpstreamID].Endpoints[i]
			}
		}
	}
	if up == nil || up.Provider == nil || ep == nil {
		return nil, nil, ir.Errorf(ir.ErrInternal, "upstream %s is not configured", cand.UpstreamID), nil, true
	}
	cred := up.credential(cand)
	if cred == nil {
		return nil, nil, ir.Errorf(ir.ErrInternal, "credential %s of upstream %s is not configured", cand.CredentialID, cand.UpstreamID), nil, true
	}
	cred = withUpstreamProxy(up, cred)
	start := s.p.d.Now()
	att := &plugin.AttemptRecord{N: 1, UpstreamID: cand.UpstreamID, EndpointID: ep.ID, CredentialID: cred.ID,
		Protocol: ep.Protocol, UpstreamModel: cand.UpstreamModel, Tier: plugin.TierPassthrough,
		Diagnostic: &ir.Diagnostic{Fidelity: ir.Lossless}}
	s.att = att
	defer func() { att.TTFB = s.p.d.Now().Sub(start) }()

	path, query := "/responses", stripCredentialParams(s.r.URL.RawQuery)
	if s.kind == WSRealtime {
		path = "/realtime"
		query = setQueryParam(query, "model", cand.UpstreamModel)
	}
	target, err := nativeURL(ep.BaseURL, path, query)
	if err != nil {
		return nil, nil, ir.Errorf(ir.ErrInternal, "invalid upstream URL: %v", err), nil, true
	}
	owned := up.Provider.OwnedHeaders(up.Config)
	hdr := egress.OutboundHeaders(s.r.Header, owned, true, nil)
	for k := range hdr {
		if strings.HasPrefix(k, "Sec-Websocket-") || k == "Origin" {
			delete(hdr, k)
		}
	}
	egress.SetExtraHeaders(hdr, up.Headers, owned)

	// The handshake context lives as long as the session: the upgraded
	// connection must not depend on one that ends with the handshake.
	timers := s.p.d.Timeouts.Override(up.Timeouts)
	hsTimeout := timers.FirstByte
	if hsTimeout <= 0 {
		hsTimeout = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(s.r.Context()))
	timer := time.AfterFunc(hsTimeout, cancel)
	// The watch bounds the connect phase and classifies its errors; it
	// lives with the connection, since stopping it cancels its context.
	watch := egress.Start(ctx, egress.Timeouts{Connect: timers.Connect})
	ok := false
	defer func() {
		if timer.Stop() && ok {
			s.dialCancel = func() { watch.Stop(); cancel() }
		} else {
			watch.Stop()
			cancel()
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, ir.Errorf(ir.ErrInternal, "invalid upstream URL: %v", err), nil, true
	}
	req.Header = hdr
	out, _, failure, gw := s.p.prepareRaw(ctx, up, ep, cred, req, watch)
	if gw != nil {
		att.Error = failure
		return nil, nil, gw, nil, true
	}
	conn, resp, err := websocket.Dial(ctx, out.Req.URL.String(), &websocket.DialOptions{
		HTTPClient: out.Client, HTTPHeader: out.Req.Header, Subprotocols: clientSubprotocols(s.r.Header)})
	if err == nil {
		ok = true
		s.cand, s.up, s.ep, s.cred = cand, up, ep, cred
		att.Sent, att.Status = true, resp.StatusCode
		return conn, resp, nil, nil, false
	}
	att.Error = errText(watch.Err(err))
	if resp == nil || resp.StatusCode == http.StatusSwitchingProtocols {
		// No HTTP answer (refused, blocked address, timeout) or a broken 101.
		if errors.Is(err, egress.ErrBlocked) {
			return nil, nil, ir.Errorf(ir.ErrUpstream, "upstream connection failed"), nil, false
		}
		v := up.Provider.Classify(&plugin.Outcome{Upstream: up.Config, Endpoint: ep, Credential: cred, Err: err})
		_ = s.p.d.Router.Report(s.r.Context(), cand, v, cand.UpstreamModel)
		gw := transportError(watch.Err(err))
		if ctx.Err() != nil && s.r.Context().Err() == nil {
			gw = ir.Errorf(ir.ErrTimeout, "upstream WebSocket handshake timed out")
		}
		return nil, nil, gw, nil, v.Retry != plugin.RetryNone
	}
	att.Sent, att.Status = true, resp.StatusCode
	var body []byte
	if resp.Body != nil {
		body, _ = io.ReadAll(resp.Body)
	}
	v := up.Provider.Classify(&plugin.Outcome{Upstream: up.Config, Endpoint: ep, Credential: cred,
		Status: resp.StatusCode, Header: resp.Header, Body: body, Sent: true})
	_ = s.p.d.Router.Report(s.r.Context(), cand, v, cand.UpstreamModel)
	att.Cause = v.Cause
	retry := v.Retry != plugin.RetryNone
	if v.ClientErr != nil {
		return nil, nil, v.ClientErr, nil, retry
	}
	return nil, nil, nil, &upstreamError{status: resp.StatusCode, header: resp.Header, body: body}, retry
}

// clientSubprotocols are the subprotocols the client offered, minus the
// one carrying its API key.
func clientSubprotocols(h http.Header) []string {
	var out []string
	for _, v := range h.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" && !hasPrefixFold(p, InsecureKeyProtocol) {
				out = append(out, p)
			}
		}
	}
	return out
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// setQueryParam replaces (or adds) one parameter, keeping the others byte
// for byte and in order.
func setQueryParam(rawQuery, name, value string) string {
	kv := url.QueryEscape(name) + "=" + url.QueryEscape(value)
	parts := []string{}
	done := false
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		n, _, _ := strings.Cut(part, "=")
		if dec, err := url.QueryUnescape(n); err == nil && dec == name {
			if !done {
				parts = append(parts, kv)
				done = true
			}
			continue
		}
		parts = append(parts, part)
	}
	if !done {
		parts = append(parts, kv)
	}
	return strings.Join(parts, "&")
}

// accept upgrades the client connection with the upstream's handshake
// answer: its subprotocol and response headers.
func (s *wsSession) accept(uc *websocket.Conn, resp *http.Response) *websocket.Conn {
	h := s.w.Header()
	for k, vs := range egress.InboundResponseHeaders(resp.Header) {
		if strings.HasPrefix(k, "Sec-Websocket-") {
			continue
		}
		h[k] = vs
	}
	var subs []string
	if sp := uc.Subprotocol(); sp != "" {
		subs = []string{sp}
	}
	// Callers authenticate with an API key, never an ambient browser
	// credential, so the origin check guards nothing here.
	cc, err := websocket.Accept(s.w, s.r, &websocket.AcceptOptions{Subprotocols: subs, InsecureSkipVerify: true})
	if err != nil {
		s.rec.Status = http.StatusBadRequest
		s.rec.ErrorKind, s.rec.Error = ir.ErrInvalidRequest, err.Error()
		return nil
	}
	s.rec.Status = http.StatusSwitchingProtocols
	cc.SetReadLimit(s.lim.MaxMessage)
	uc.SetReadLimit(s.lim.MaxMessage)
	if s.kind == WSResponses {
		s.resolveRoutes()
	}
	return cc
}

// resolveRoutes maps every model the caller may use to its upstream model
// on the dialed upstream.
func (s *wsSession) resolveRoutes() {
	t := s.snap.Table
	for name, m := range t.Models {
		if !m.Enabled || (s.snap.Visible != nil && !s.snap.Visible(s.caller, name)) {
			continue
		}
		if um, ok := modelRoute(t, name, s.cand.UpstreamID, s.caller.GroupID); ok {
			s.routes[name] = um
		}
	}
}

// sessionEnd says why a session ended and how to close each side.
type sessionEnd struct {
	client, upstream websocket.StatusCode
	reason           string
	// event is an error event sent to the client before closing.
	event *ir.GwError
	kind  ir.ErrorKind
	note  string
	// drain: the client left, so open responses may still be awaited.
	drain bool
}

// relay copies messages both ways until one side closes or a limit ends
// the session, then closes both with fitting codes. When the client left
// with responses open, the upstream stays open up to the drain time so
// their usage still arrives.
func (s *wsSession) relay(cc, uc *websocket.Conn) {
	ends := make(chan sessionEnd, 8)
	end := func(e sessionEnd) {
		select {
		case ends <- e:
		default:
		}
	}
	var lastIO atomic.Int64
	lastIO.Store(time.Now().UnixNano())
	s.uc = uc
	s.ready = make(chan struct{})
	if s.kind != WSRealtime || (len(s.bodyMutators) == 0 && !s.transcriptionIntent()) {
		s.sessionReady()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.pump(cc, uc, &lastIO, true, end)
	}()
	go func() {
		defer wg.Done()
		s.pump(uc, cc, &lastIO, false, end)
	}()

	maxSession := time.NewTimer(s.lim.MaxSession)
	defer maxSession.Stop()
	tick := time.NewTicker(min(s.lim.Idle/4+time.Millisecond, time.Second))
	defer tick.Stop()
	readyTimer := time.NewTimer(s.lim.ReadyTimeout)
	defer readyTimer.Stop()
	readyC, readyWait := s.ready, readyTimer.C
	var e sessionEnd
wait:
	for {
		select {
		case e = <-ends:
			break wait
		case <-readyC:
			readyC, readyWait = nil, nil
		case <-readyWait:
			// Failing closed: client events must not pass without the
			// session the body mutators and transcription check bind to.
			e = sessionEnd{client: websocket.StatusTryAgainLater, upstream: websocket.StatusGoingAway,
				reason: "upstream session not ready", kind: ir.ErrUpstream,
				note: "upstream sent no session.created within " + s.lim.ReadyTimeout.String(),
				event: &ir.GwError{Kind: ir.ErrUpstream, Code: "session_not_ready",
					Message: "The upstream did not start the session within " + s.lim.ReadyTimeout.String() + "."}}
			break wait
		case <-maxSession.C:
			e = sessionEnd{client: websocket.StatusNormalClosure, upstream: websocket.StatusNormalClosure,
				reason: "session time limit reached", note: "session time limit reached",
				event: &ir.GwError{Kind: ir.ErrInvalidRequest, Code: "session_expired",
					Message: "The session reached its maximum duration of " + s.lim.MaxSession.String() + "."}}
			break wait
		case <-tick.C:
			if time.Since(time.Unix(0, lastIO.Load())) >= s.lim.Idle {
				e = sessionEnd{client: websocket.StatusNormalClosure, upstream: websocket.StatusNormalClosure,
					reason: "idle timeout", note: "idle timeout"}
				break wait
			}
		case <-s.r.Context().Done():
			e = sessionEnd{client: websocket.StatusGoingAway, upstream: websocket.StatusGoingAway,
				reason: "relay shutting down", note: "relay shutting down"}
			break wait
		}
	}
	if e.event != nil && !s.clientGone.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = cc.Write(ctx, websocket.MessageText, errorEvent(s.id, e.event))
		cancel()
	}
	var cw sync.WaitGroup
	cw.Add(2)
	go func() { defer cw.Done(); closeConn(cc, e.client, e.reason) }()
	if e.drain && s.openCount() > 0 {
		s.clientGone.Store(true)
		e.note += s.drain(ends)
	}
	go func() { defer cw.Done(); closeConn(uc, e.upstream, e.reason) }()
	cw.Wait()
	wg.Wait()
	if e.kind != "" {
		s.rec.ErrorKind, s.rec.Error = e.kind, e.note
	}
	s.mu.Lock()
	s.closeNote, s.endKind = e.note, e.kind
	s.mu.Unlock()
}

// drain waits, after the client left, until the open responses are
// settled, the upstream ends, the relay shuts down or the drain time runs
// out. It returns a note on how it ended.
func (s *wsSession) drain(ends <-chan sessionEnd) string {
	timer := time.NewTimer(s.lim.Drain)
	defer timer.Stop()
	for s.openCount() > 0 {
		select {
		case <-s.settled:
		case e := <-ends:
			if !e.drain {
				return ", upstream ended while draining"
			}
		case <-timer.C:
			return ", drain time ran out"
		case <-s.r.Context().Done():
			return ", relay shutting down while draining"
		}
	}
	return ", drained"
}

// closeConn closes with code (when it is one a peer may be sent) and falls
// back to dropping the connection.
func closeConn(c *websocket.Conn, code websocket.StatusCode, reason string) {
	if code == 0 || code == websocket.StatusNoStatusRcvd || code == websocket.StatusAbnormalClosure || code == websocket.StatusTLSHandshake {
		code = websocket.StatusNormalClosure
	}
	if len(reason) > 120 {
		reason = reason[:120]
	}
	if err := c.Close(code, reason); err != nil {
		_ = c.CloseNow()
	}
}

// pump copies messages from src to dst. fromClient tells the direction.
// After the client left, upstream messages are still read (and billed) but
// not forwarded.
func (s *wsSession) pump(src, dst *websocket.Conn, lastIO *atomic.Int64, fromClient bool, end func(sessionEnd)) {
	defer func() {
		if v := recover(); v != nil {
			s.p.d.Logger.Error("websocket relay panicked", "session", s.id, "panic", v, "stack", string(debug.Stack()))
			end(sessionEnd{client: websocket.StatusInternalError, upstream: websocket.StatusGoingAway,
				reason: "internal error", kind: ir.ErrInternal, note: "internal error"})
		}
	}()
	if !fromClient {
		defer s.sessionReady()
	}
	ctx := context.Background()
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			end(s.readEnd(err, fromClient))
			return
		}
		lastIO.Store(time.Now().UnixNano())
		if fromClient {
			<-s.ready
			s.bytesIn.Add(int64(len(data)))
			var gw *ir.GwError
			if data, gw = s.fromClient(typ, data); gw != nil {
				if err := s.writeClient(src, errorEvent(s.id, gw)); err != nil {
					end(sessionEnd{client: websocket.StatusNormalClosure, upstream: websocket.StatusNormalClosure,
						note: "client write failed", drain: true})
					return
				}
				continue
			}
		} else {
			s.bytesOut.Add(int64(len(data)))
		}
		if fromClient || !s.clientGone.Load() {
			if fromClient {
				err = s.writeTo(dst, typ, data)
			} else {
				err = s.toClient(src, dst, typ, data)
			}
			if err != nil {
				if fromClient {
					end(sessionEnd{client: websocket.StatusBadGateway, upstream: websocket.StatusGoingAway,
						reason: "upstream write failed", kind: ir.ErrUpstream, note: "upstream write failed: " + err.Error()})
					return
				}
				s.clientGone.Store(true)
				end(sessionEnd{client: websocket.StatusGoingAway, upstream: websocket.StatusGoingAway,
					reason: "client write failed", note: "client write failed", drain: true})
			}
		}
		if !fromClient && typ == websocket.MessageText {
			if gw := s.fromUpstream(data); gw != nil {
				if !s.clientGone.Load() {
					_ = s.writeClient(dst, errorEvent(s.id, gw))
				}
				code, reason := websocket.StatusPolicyViolation, "quota exhausted"
				if gw.Kind == ir.ErrInternal {
					code, reason = websocket.StatusInternalError, "metering unavailable"
				}
				end(sessionEnd{client: code, upstream: websocket.StatusNormalClosure,
					reason: reason, kind: gw.Kind, note: gw.Message})
				return
			}
		}
	}
}

func (s *wsSession) writeClient(cc *websocket.Conn, b []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.lim.Idle)
	defer cancel()
	return cc.Write(ctx, websocket.MessageText, b)
}

// readEnd maps a read error of one side to how the session ends.
func (s *wsSession) readEnd(err error, fromClient bool) sessionEnd {
	code := websocket.CloseStatus(err)
	var reason string
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		reason = ce.Reason
	}
	tooBig := strings.Contains(err.Error(), "read limited at")
	switch {
	case fromClient && tooBig:
		return sessionEnd{client: websocket.StatusMessageTooBig, upstream: websocket.StatusNormalClosure,
			reason: "message too big", kind: ir.ErrInvalidRequest,
			note: "client message exceeds " + strconv.FormatInt(s.lim.MaxMessage, 10) + " bytes"}
	case fromClient && code != -1:
		return sessionEnd{client: code, upstream: code, reason: reason, note: "closed by client (" + strconv.Itoa(int(code)) + ")", drain: true}
	case fromClient:
		return sessionEnd{client: websocket.StatusGoingAway, upstream: websocket.StatusGoingAway, reason: "client went away", note: "client went away", drain: true}
	case tooBig:
		return sessionEnd{client: websocket.StatusMessageTooBig, upstream: websocket.StatusMessageTooBig,
			reason: "upstream message too big", kind: ir.ErrUpstream,
			note: "upstream message exceeds " + strconv.FormatInt(s.lim.MaxMessage, 10) + " bytes"}
	case code != -1:
		return sessionEnd{client: code, upstream: code, reason: reason, note: "closed by upstream (" + strconv.Itoa(int(code)) + ")"}
	}
	return sessionEnd{client: websocket.StatusBadGateway, upstream: websocket.StatusGoingAway,
		reason: "upstream connection lost", kind: ir.ErrUpstream, note: "upstream connection lost: " + err.Error()}
}

// fromClient checks a client message before it is forwarded. A text
// message must be one JSON object without a repeated key, so the relay and
// the upstream read the same fields, and must not use the relay's event_id
// prefix. A Responses response.create is
// admitted per response (createResponse); a Realtime event carrying text or
// audio goes through the content checks (checkRealtime); a Realtime
// session.update may not switch the model, and once the meter refused the
// next response a response.create is answered instead of forwarded. A non-nil error is
// answered to the client as an error event instead of forwarding.
func (s *wsSession) fromClient(typ websocket.MessageType, data []byte) ([]byte, *ir.GwError) {
	if typ != websocket.MessageText {
		return data, nil
	}
	if gw := checkEvent(data); gw != nil {
		return nil, gw
	}
	if strings.HasPrefix(gjson.GetBytes(data, "event_id").String(), relayEventPrefix) {
		return nil, &ir.GwError{Kind: ir.ErrInvalidRequest, Code: "invalid_value",
			Message: "event_id may not start with " + quote(relayEventPrefix) + ", which the relay keeps for its own events"}
	}
	t := gjson.GetBytes(data, "type").String()
	if s.kind == WSResponses {
		if t != "response.create" {
			return data, nil
		}
		return s.createResponse(data)
	}
	var gw *ir.GwError
	if data, gw = s.checkRealtime(t, data); gw != nil {
		return nil, gw
	}
	switch t {
	case "session.update":
		m := gjson.GetBytes(data, "session.model")
		if !m.Exists() {
			break
		}
		switch m.String() {
		case s.cand.UpstreamModel:
		case s.model:
			b, err := sjson.SetBytes(data, "session.model", s.cand.UpstreamModel)
			if err != nil {
				return nil, ir.Errorf(ir.ErrInvalidRequest, "session.update: %v", err)
			}
			data = b
		default:
			return nil, &ir.GwError{Kind: ir.ErrInvalidRequest, Code: "invalid_value",
				Message: "the session model cannot be changed on this connection; open a new session for model " + quote(m.String())}
		}
	}
	if data, gw = s.capEvent(t, data); gw != nil {
		return nil, gw
	}
	switch t {
	case "session.update", "transcription_session.update":
		if gw := s.checkTranscription(gjson.GetBytes(data, "session")); gw != nil {
			return nil, gw
		}
	case "input_audio_buffer.append", "input_audio_buffer.commit":
		if gw := s.transcriptionReady(); gw != nil {
			return nil, gw
		}
	}
	if t == "response.create" {
		s.mu.Lock()
		if s.refusal != nil {
			gw := *s.refusal
			s.mu.Unlock()
			return nil, &gw
		}
		s.awaiting++
		s.mu.Unlock()
	}
	s.countInput(t, data)
	return data, nil
}

// maxEventDepth bounds the nesting of a client event, so checking it
// cannot exhaust the stack.
const maxEventDepth = 128

// checkEvent refuses a client text message that is not a JSON object, is
// nested deeper than maxEventDepth, or repeats a key in any object: the
// relay reads the first of two keys, an upstream may read the last.
func checkEvent(data []byte) *ir.GwError {
	if d := jsonDepth(data); d > maxEventDepth {
		return ir.Errorf(ir.ErrInvalidRequest, "event is nested deeper than %d levels", maxEventDepth)
	}
	if !gjson.ValidBytes(data) {
		return ir.Errorf(ir.ErrInvalidRequest, "event is not valid JSON")
	}
	v := gjson.ParseBytes(data)
	if !v.IsObject() {
		return ir.Errorf(ir.ErrInvalidRequest, "event must be a JSON object")
	}
	return duplicateKey(v, "event")
}

// duplicateKey rejects JSON that repeats a key in any object. The relay reads
// the first occurrence (gjson) while most upstreams keep the last, so a
// repeated "model" or "stream" would let a request be checked, routed and
// billed as one thing and served as another.
func duplicateKey(v gjson.Result, what string) *ir.GwError {
	if k := repeatedKey(v); k != "" {
		return &ir.GwError{Kind: ir.ErrInvalidRequest, Code: "duplicate_key", Message: what + " repeats the key " + quote(k)}
	}
	return nil
}

// jsonDepth is the deepest nesting of objects and arrays in data, ignoring
// brackets inside strings.
func jsonDepth(data []byte) int {
	depth, deepest, inStr, esc := 0, 0, false, false
	for _, c := range data {
		switch {
		case inStr:
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{' || c == '[':
			depth++
			deepest = max(deepest, depth)
		case c == '}' || c == ']':
			depth--
		}
	}
	return deepest
}

// repeatedKey returns the first key repeated in any object within v.
func repeatedKey(v gjson.Result) string {
	var found string
	switch {
	case v.IsObject():
		seen := map[string]bool{}
		v.ForEach(func(k, val gjson.Result) bool {
			if seen[k.String()] {
				found = k.String()
				return false
			}
			seen[k.String()] = true
			found = repeatedKey(val)
			return found == ""
		})
	case v.IsArray():
		v.ForEach(func(_, val gjson.Result) bool {
			found = repeatedKey(val)
			return found == ""
		})
	}
	return found
}

// errorEvent renders a relay error as a server error event, in the shape
// both the Realtime and the Responses WebSocket clients read.
func errorEvent(sessionID string, gw *ir.GwError) []byte {
	status, _, body := responsesCodec().EncodeError(gw)
	b := []byte(`{"type":"error"}`)
	b, _ = sjson.SetBytes(b, "event_id", "event_relay_"+sessionID)
	b, _ = sjson.SetBytes(b, "status", status)
	if e := gjson.GetBytes(body, "error"); e.Exists() {
		b, _ = sjson.SetRawBytes(b, "error", []byte(e.Raw))
	}
	return b
}

func responsesCodec() protocol.Codec {
	c, _ := protocol.Get("openai-responses")
	return c
}

// fail answers the handshake with an HTTP error in the OpenAI format.
func (s *wsSession) fail(gw *ir.GwError) {
	status, h, body := responsesCodec().EncodeError(gw)
	if gw.Status != 0 {
		status = gw.Status
	}
	s.rec.Status, s.rec.ErrorKind, s.rec.Error = status, gw.Kind, gw.Message
	if s.att != nil && s.att.Error == "" {
		s.att.Error = gw.Message
	}
	if s.r.Context().Err() != nil {
		s.rec.Status = statusClientClosed
		return
	}
	dst := s.w.Header()
	for k, vs := range h {
		dst[k] = vs
	}
	if dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "application/json")
	}
	if gw.RetryAfter > 0 {
		dst.Set("Retry-After", strconv.Itoa(max(int(gw.RetryAfter.Seconds()), 1)))
	}
	s.w.WriteHeader(status)
	_, _ = s.w.Write(body)
}

// failVerbatim passes the upstream's handshake answer through. A body that
// is not whole JSON (the WebSocket client keeps only its first KiB) is
// wrapped in a relay error instead.
func (s *wsSession) failVerbatim(e *upstreamError) {
	msg := "upstream answered " + strconv.Itoa(e.status) + " to the WebSocket handshake"
	s.rec.Status, s.rec.ErrorKind, s.rec.Error = e.status, ir.ErrUpstream, msg
	dst := s.w.Header()
	for k, vs := range egress.InboundResponseHeaders(e.header) {
		dst[k] = vs
	}
	body := e.body
	if !gjson.ValidBytes(body) {
		if text := strings.TrimSpace(strings.ToValidUTF8(string(body), "")); text != "" {
			msg += ": " + text
		}
		_, _, body = responsesCodec().EncodeError(&ir.GwError{Kind: ir.ErrUpstream, Code: "upstream_error", Message: msg})
		dst.Set("Content-Type", "application/json")
	}
	dst.Del("Content-Length")
	dst.Del("Content-Encoding")
	s.w.WriteHeader(e.status)
	_, _ = s.w.Write(body)
}

func (s *wsSession) emit(rec *plugin.Record, ar *plugin.AttemptRecord) {
	if m := s.p.d.Metrics; m != nil {
		if ar != nil {
			m.Attempt(s.proto, ar)
		}
		m.Request(rec)
	}
	if q := s.p.d.Records; q != nil && !q.Enqueue(rec) {
		s.p.d.Logger.Warn("request record dropped by best-effort sinks", "request", rec.ID)
	}
}

// finish runs the session's finish hooks and writes its record: duration,
// bytes each way, responses.
func (s *wsSession) finish() {
	if s.dialCancel != nil {
		s.dialCancel()
	}
	if s.noRecord {
		return
	}
	s.rec.Duration = s.p.d.Now().Sub(s.started)
	s.mu.Lock()
	usage := s.usage
	note := "websocket: " + strconv.Itoa(s.responses) + " responses"
	if s.transcriptions > 0 {
		note += ", " + strconv.Itoa(s.transcriptions) + " transcriptions"
	}
	if s.closeNote != "" {
		note += ", " + s.closeNote
	}
	notes := append([]string{note}, s.notes...)
	s.mu.Unlock()
	if s.hookCall != nil {
		var gw *ir.GwError
		if s.rec.ErrorKind != "" {
			gw = &ir.GwError{Kind: s.rec.ErrorKind, Message: s.rec.Error}
		}
		s.finishHooks(s.hookCall, s.entered, &plugin.Result{Status: s.rec.Status, Err: gw, Usage: usage,
			Attempts: len(s.rec.Attempts), Delivered: s.rec.Status == http.StatusSwitchingProtocols})
	}
	if s.att != nil {
		a := *s.att
		a.BytesIn, a.BytesOut = s.bytesIn.Load(), s.bytesOut.Load()
		a.Duration = s.rec.Duration
		if a.Diagnostic != nil {
			d := *a.Diagnostic
			d.Notes = append(slices.Clone(d.Notes), notes...)
			a.Diagnostic = &d
		}
		s.rec.Attempts = []plugin.AttemptRecord{a}
	}
	s.emit(s.rec, nil)
}
