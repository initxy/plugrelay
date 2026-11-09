package extplugin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// stateKey is where a middleware's opaque state lives between its hooks.
const stateKey = "extplugin.state"

// maxAdminBody bounds an admin request body proxied to a plugin.
const maxAdminBody = 16 << 20

// credentialHeaders never reach plugins other than authenticators.
var credentialHeaders = []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Cookie", "Proxy-Authorization"}

var (
	_ plugin.Authenticator = (*Authenticator)(nil)
	_ plugin.RequestHook   = (*Middleware)(nil)
	_ plugin.AttemptHook   = (*Middleware)(nil)
	_ plugin.FinishHook    = (*Middleware)(nil)
	_ plugin.Selector      = (*Selector)(nil)
	_ plugin.Meter         = (*Meter)(nil)
	_ plugin.Sink          = (*Sink)(nil)
	_ plugin.Job           = (*Job)(nil)
	_ plugin.Admin         = (*Admin)(nil)
	_ plugin.Supporter     = (*Middleware)(nil)
)

// base is shared by every adapter.
type base struct {
	in   *instance
	kind plugin.Kind
}

// Info implements plugin.Plugin.
func (b *base) Info() plugin.Info {
	m := b.in.man
	return plugin.Info{
		ID: m.PluginID(b.kind), Kind: b.kind, Version: m.Version, Description: m.Description,
		Settings: m.PluginSettings(), FailPolicy: m.FailPolicyFor(b.kind),
		ReportsOffBox: m.ReportsOffBox, Requires: m.Requires,
	}
}

// Supports implements plugin.Supporter: whether the plugin implements a
// protocol method. The core checks it before using optional hooks.
func (b *base) Supports(method string) bool { return b.in.man.Implements(method) }

func (b *base) call(ctx context.Context, method string, params, out any) error {
	if !b.in.man.Implements(method) {
		return &CallError{Plugin: b.in.man.ID, Method: method, Err: ErrNotImplemented}
	}
	return b.in.call(ctx, method, params, out)
}

func (b *base) wireCall(c *plugin.Call, withRequest bool) wireCall {
	w := wireCall{
		ID: c.ID, Caller: c.Caller, Protocol: c.Protocol, Model: c.Model, Stream: c.Stream,
		SessionID: c.SessionID, ClientIP: c.ClientIP, Header: scrubHeader(c.Header),
	}
	if len(b.in.man.Settings) > 0 {
		w.Settings = map[string]json.RawMessage{}
		for _, s := range b.in.man.Settings {
			v := c.Setting(s.Key)
			if v.IsSet() {
				w.Settings[s.Key] = v.Raw()
			}
			if l := v.ListLayers(); len(l) > 1 {
				if w.SettingLayers == nil {
					w.SettingLayers = map[string][][]string{}
				}
				w.SettingLayers[s.Key] = l
			}
		}
	}
	if v, ok := c.Get(stateKey); ok {
		w.State, _ = v.(json.RawMessage)
	}
	if withRequest && b.in.man.RequestBody {
		if r, err := c.Request(); err == nil {
			w.Request = r
		}
	}
	return w
}

func scrubHeader(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := h.Clone()
	for _, k := range credentialHeaders {
		out.Del(k)
	}
	return out
}

func ctxOf(c *plugin.Call) context.Context {
	if c != nil && c.Context != nil {
		return c.Context
	}
	return context.Background()
}

// ---------------------------------------------------------------- auth

// Authenticator adapts auth.authenticate.
type Authenticator struct{ base }

// Authenticate implements plugin.Authenticator.
func (a *Authenticator) Authenticate(r *http.Request) (*plugin.Caller, error) {
	p := map[string]any{"request": wireHTTPRequest{
		Method: r.Method, URL: r.URL.String(), Host: r.Host, Path: r.URL.Path,
		Query: r.URL.RawQuery, Header: r.Header, RemoteAddr: r.RemoteAddr,
	}}
	var res struct {
		Caller *plugin.Caller `json:"caller"`
	}
	if err := a.call(r.Context(), "auth.authenticate", p, &res); err != nil {
		return nil, err
	}
	return res.Caller, nil
}

// ---------------------------------------------------------------- middleware

// Middleware adapts middleware.onRequest/onAttempt/onFinish. Hooks the
// manifest does not list are no-ops.
type Middleware struct{ base }

// OnRequest implements plugin.RequestHook.
func (m *Middleware) OnRequest(c *plugin.Call) (*plugin.Reply, error) {
	if !m.Supports("middleware.onRequest") {
		return nil, nil
	}
	var res struct {
		Reply *wireReply      `json:"reply"`
		State json.RawMessage `json:"state"`
	}
	if err := m.call(ctxOf(c), "middleware.onRequest", map[string]any{"call": m.wireCall(c, true)}, &res); err != nil {
		return nil, err
	}
	m.keepState(c, res.State)
	if res.Reply == nil || (res.Reply.Error == nil && res.Reply.Response == nil) {
		return nil, nil
	}
	return &plugin.Reply{Error: res.Reply.Error.gw(), Response: res.Reply.Response}, nil
}

// OnAttempt implements plugin.AttemptHook.
func (m *Middleware) OnAttempt(c *plugin.Call, a *plugin.Attempt) error {
	if !m.Supports("middleware.onAttempt") {
		return nil
	}
	p := map[string]any{"call": m.wireCall(c, true), "attempt": wireAttempt{
		N: a.N, Candidate: toWireCandidate(a.Candidate), Tier: a.Tier,
		UpstreamModel: a.UpstreamModel, Header: scrubHeader(a.Header),
	}}
	var res struct {
		SetHeader map[string]string `json:"set_header"`
		State     json.RawMessage   `json:"state"`
	}
	if err := m.call(ctxOf(c), "middleware.onAttempt", p, &res); err != nil {
		return err
	}
	m.keepState(c, res.State)
	if a.Header != nil {
		for k, v := range res.SetHeader {
			if slices.ContainsFunc(credentialHeaders, func(h string) bool { return strings.EqualFold(h, k) }) {
				continue
			}
			a.Header.Set(k, v)
		}
	}
	return nil
}

// OnFinish implements plugin.FinishHook. Errors are logged, never returned.
func (m *Middleware) OnFinish(c *plugin.Call, r *plugin.Result) {
	if !m.Supports("middleware.onFinish") {
		return
	}
	p := map[string]any{"call": m.wireCall(c, false), "result": wireResult{
		Status: r.Status, Error: toWireError(r.Err), Usage: r.Usage, Attempts: r.Attempts, Delivered: r.Delivered,
	}}
	if err := m.call(context.WithoutCancel(ctxOf(c)), "middleware.onFinish", p, nil); err != nil {
		m.in.log.Warn("middleware.onFinish failed", "err", err, "request", c.ID)
	}
}

func (m *Middleware) keepState(c *plugin.Call, state json.RawMessage) {
	if len(state) > 0 && string(state) != "null" {
		c.Set(stateKey, state)
	}
}

// ---------------------------------------------------------------- selector

// Selector adapts selector.rank. On failure it keeps the router's order.
type Selector struct{ base }

// Rank implements plugin.Selector. The result is always a permutation of
// cands: unknown or repeated indexes are ignored and omitted candidates
// keep their relative order at the end.
func (s *Selector) Rank(c *plugin.Call, cands []plugin.Candidate) []plugin.Candidate {
	wc := make([]wireCandidate, len(cands))
	for i, cand := range cands {
		wc[i] = toWireCandidate(cand)
	}
	var res struct {
		Order []int `json:"order"`
	}
	if err := s.call(ctxOf(c), "selector.rank", map[string]any{"call": s.wireCall(c, false), "candidates": wc}, &res); err != nil {
		s.in.log.Warn("selector.rank failed; keeping router order", "err", err)
		return cands
	}
	return permute(cands, res.Order)
}

func permute(cands []plugin.Candidate, order []int) []plugin.Candidate {
	out := make([]plugin.Candidate, 0, len(cands))
	used := make([]bool, len(cands))
	for _, i := range order {
		if i >= 0 && i < len(cands) && !used[i] {
			used[i] = true
			out = append(out, cands[i])
		}
	}
	for i, c := range cands {
		if !used[i] {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------- meter

// Meter adapts meter.reserve/settle/refund.
type Meter struct{ base }

// Reserve implements plugin.Meter.
func (m *Meter) Reserve(c *plugin.Call, estimateMicro int64) (*plugin.Reservation, error) {
	var res struct {
		Reservation *wireReservation `json:"reservation"`
	}
	p := map[string]any{"call": m.wireCall(c, false), "estimate_micro": estimateMicro}
	if err := m.call(ctxOf(c), "meter.reserve", p, &res); err != nil {
		return nil, err
	}
	if res.Reservation == nil {
		return nil, nil
	}
	return &plugin.Reservation{ID: res.Reservation.ID, AmountMicro: res.Reservation.AmountMicro}, nil
}

// Settle implements plugin.Meter.
func (m *Meter) Settle(c *plugin.Call, r *plugin.Reservation, usage ir.Usage, costMicro int64) error {
	p := map[string]any{"call": m.wireCall(c, false), "reservation": wireRes(r), "usage": usage, "cost_micro": costMicro}
	return m.call(context.WithoutCancel(ctxOf(c)), "meter.settle", p, nil)
}

// Refund implements plugin.Meter.
func (m *Meter) Refund(c *plugin.Call, r *plugin.Reservation) error {
	p := map[string]any{"call": m.wireCall(c, false), "reservation": wireRes(r)}
	return m.call(context.WithoutCancel(ctxOf(c)), "meter.refund", p, nil)
}

func wireRes(r *plugin.Reservation) *wireReservation {
	if r == nil {
		return nil
	}
	return &wireReservation{ID: r.ID, AmountMicro: r.AmountMicro}
}

// ---------------------------------------------------------------- sink

// Sink adapts sink.consume.
type Sink struct{ base }

// Consume implements plugin.Sink.
func (s *Sink) Consume(ctx context.Context, r *plugin.Record) error {
	return s.call(ctx, "sink.consume", map[string]any{"record": r}, nil)
}

// ---------------------------------------------------------------- job

// Job adapts job.run.
type Job struct{ base }

// Every implements plugin.Job.
func (j *Job) Every() time.Duration { return j.in.man.Job.Every.D() }

// Run implements plugin.Job.
func (j *Job) Run(ctx context.Context) error { return j.call(ctx, "job.run", struct{}{}, nil) }

// ---------------------------------------------------------------- admin

// Admin adapts admin.handle: each declared route proxies the HTTP request.
type Admin struct{ base }

// Routes implements plugin.Admin.
func (a *Admin) Routes() []plugin.AdminRoute {
	var out []plugin.AdminRoute
	for _, r := range a.in.man.Admin.Routes {
		out = append(out, plugin.AdminRoute{Method: r.Method, Path: r.Path, Role: r.Role, Handler: a.handler(r)})
	}
	return out
}

func (a *Admin) handler(route RouteSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, maxAdminBody+1))
		if err != nil || len(data) > maxAdminBody {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		req := wireHTTPRequest{
			Method: r.Method, URL: r.URL.String(), Path: r.URL.Path, Query: r.URL.RawQuery,
			Header: scrubHeader(r.Header), RemoteAddr: r.RemoteAddr,
		}
		setBody(data, &req.Body, &req.BodyBase64)
		var res wireHTTPResponse
		if err := a.call(r.Context(), "admin.handle", map[string]any{"route": route.Path, "request": req}, &res); err != nil {
			status := http.StatusBadGateway
			if gw, ok := err.(*ir.GwError); ok && gw.Status != 0 {
				status = gw.Status
			}
			http.Error(w, err.Error(), status)
			return
		}
		body, err := decodeBody(res.Body, res.BodyBase64)
		if err != nil {
			http.Error(w, "plugin returned a malformed body", http.StatusBadGateway)
			return
		}
		for k, vs := range res.Header {
			if strings.EqualFold(k, "Set-Cookie") {
				continue
			}
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if res.Status == 0 {
			res.Status = http.StatusOK
		}
		w.WriteHeader(res.Status)
		_, _ = w.Write(body)
	}
}
