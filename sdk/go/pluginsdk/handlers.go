package pluginsdk

import (
	"context"
	"encoding/json"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

func decode[T any](params json.RawMessage) (*T, error) {
	var v T
	if err := json.Unmarshal(params, &v); err != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: err.Error()}
	}
	return &v, nil
}

type callParams struct {
	Call *Call `json:"call"`
}

func (p callParams) call() *Call {
	if p.Call == nil {
		return &Call{}
	}
	return p.Call
}

// Authenticate registers auth.authenticate. Return (nil, nil) to let the
// next authenticator try and Reject(ir.ErrAuthentication, ...) to refuse.
func (p *Plugin) Authenticate(h func(ctx context.Context, r *HTTPRequest) (*plugin.Caller, error)) {
	p.Handle("auth.authenticate", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[struct {
			Request HTTPRequest `json:"request"`
		}](raw)
		if err != nil {
			return nil, err
		}
		c, err := h(ctx, &in.Request)
		return map[string]any{"caller": c}, err
	})
}

// OnRequest registers middleware.onRequest. Return a Reply to stop the
// request (see Deny), nil to let it continue.
func (p *Plugin) OnRequest(h func(ctx context.Context, c *Call) (*Reply, error)) {
	p.Handle("middleware.onRequest", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[callParams](raw)
		if err != nil {
			return nil, err
		}
		c := in.call()
		reply, err := h(ctx, c)
		return map[string]any{"reply": reply, "state": c.newState}, err
	})
}

// OnAttempt registers middleware.onAttempt. The returned headers are set on
// the outgoing request (provider-owned and credential headers excepted).
func (p *Plugin) OnAttempt(h func(ctx context.Context, c *Call, a *Attempt) (map[string]string, error)) {
	p.Handle("middleware.onAttempt", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[struct {
			callParams
			Attempt Attempt `json:"attempt"`
		}](raw)
		if err != nil {
			return nil, err
		}
		c := in.call()
		set, err := h(ctx, c, &in.Attempt)
		return map[string]any{"set_header": set, "state": c.newState}, err
	})
}

// OnFinish registers middleware.onFinish.
func (p *Plugin) OnFinish(h func(ctx context.Context, c *Call, r *Result) error) {
	p.Handle("middleware.onFinish", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[struct {
			callParams
			Result Result `json:"result"`
		}](raw)
		if err != nil {
			return nil, err
		}
		return struct{}{}, h(ctx, in.call(), &in.Result)
	})
}

// Rank registers selector.rank. Return candidate indexes in the preferred
// order; omitted candidates are appended by the host.
func (p *Plugin) Rank(h func(ctx context.Context, c *Call, cands []Candidate) ([]int, error)) {
	p.Handle("selector.rank", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[struct {
			callParams
			Candidates []Candidate `json:"candidates"`
		}](raw)
		if err != nil {
			return nil, err
		}
		order, err := h(ctx, in.call(), in.Candidates)
		return map[string]any{"order": order}, err
	})
}

// Meter registers meter.reserve, meter.settle and meter.refund. Settle and
// refund must be idempotent per call id.
func (p *Plugin) Meter(
	reserve func(ctx context.Context, c *Call, estimateMicro int64) (*Reservation, error),
	settle func(ctx context.Context, c *Call, r *Reservation, usage ir.Usage, costMicro int64) error,
	refund func(ctx context.Context, c *Call, r *Reservation) error,
) {
	type params struct {
		callParams
		EstimateMicro int64        `json:"estimate_micro"`
		Reservation   *Reservation `json:"reservation"`
		Usage         ir.Usage     `json:"usage"`
		CostMicro     int64        `json:"cost_micro"`
	}
	p.Handle("meter.reserve", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[params](raw)
		if err != nil {
			return nil, err
		}
		r, err := reserve(ctx, in.call(), in.EstimateMicro)
		return map[string]any{"reservation": r}, err
	})
	p.Handle("meter.settle", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[params](raw)
		if err != nil {
			return nil, err
		}
		return struct{}{}, settle(ctx, in.call(), in.Reservation, in.Usage, in.CostMicro)
	})
	p.Handle("meter.refund", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[params](raw)
		if err != nil {
			return nil, err
		}
		return struct{}{}, refund(ctx, in.call(), in.Reservation)
	})
}

// Consume registers sink.consume.
func (p *Plugin) Consume(h func(ctx context.Context, r *plugin.Record) error) {
	p.Handle("sink.consume", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[struct {
			Record plugin.Record `json:"record"`
		}](raw)
		if err != nil {
			return nil, err
		}
		return struct{}{}, h(ctx, &in.Record)
	})
}

// RunJob registers job.run.
func (p *Plugin) RunJob(h func(ctx context.Context) error) {
	p.Handle("job.run", func(ctx context.Context, _ json.RawMessage) (any, error) {
		return struct{}{}, h(ctx)
	})
}

// AdminHandle registers admin.handle; route is the manifest path matched.
func (p *Plugin) AdminHandle(h func(ctx context.Context, route string, r *HTTPRequest) (*HTTPResponse, error)) {
	p.Handle("admin.handle", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[struct {
			Route   string      `json:"route"`
			Request HTTPRequest `json:"request"`
		}](raw)
		if err != nil {
			return nil, err
		}
		return h(ctx, in.Route, &in.Request)
	})
}

// Endpoints registers provider.endpoints.
func (p *Plugin) Endpoints(h func(ctx context.Context, u *Upstream) (eps []plugin.Endpoint, ownedHeaders []string, err error)) {
	p.Handle("provider.endpoints", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[struct {
			Upstream Upstream `json:"upstream"`
		}](raw)
		if err != nil {
			return nil, err
		}
		eps, owned, err := h(ctx, &in.Upstream)
		return map[string]any{"endpoints": eps, "owned_headers": owned}, err
	})
}

// Prepare registers provider.prepare. Edit o.Header and o.URL in place.
func (p *Plugin) Prepare(h func(ctx context.Context, o *Outgoing) error) {
	p.Handle("provider.prepare", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[struct {
			Outgoing Outgoing `json:"outgoing"`
		}](raw)
		if err != nil {
			return nil, err
		}
		o := &in.Outgoing
		if err := h(ctx, o); err != nil {
			return nil, err
		}
		return map[string]any{"header": o.Header, "url": o.URL}, nil
	})
}

// Classify registers provider.classify.
func (p *Plugin) Classify(h func(ctx context.Context, o *Outcome) (*Verdict, error)) {
	p.Handle("provider.classify", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[struct {
			Outcome Outcome `json:"outcome"`
		}](raw)
		if err != nil {
			return nil, err
		}
		v, err := h(ctx, &in.Outcome)
		return map[string]any{"verdict": v}, err
	})
}

// Emit sends one stream event to the host.
type Emit func(ev *Event) error

// Send registers provider.send. Emit events in order and end with EvDone
// or EvError; or, before any content, return a non-nil Outcome describing
// the failed attempt so the host can classify and retry it.
func (p *Plugin) Send(h func(ctx context.Context, o *Outgoing, req *ir.Request, emit Emit) (*Outcome, error)) {
	p.Handle("provider.send", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[struct {
			Token    string      `json:"token"`
			Outgoing Outgoing    `json:"outgoing"`
			Request  *ir.Request `json:"request"`
		}](raw)
		if err != nil {
			return nil, err
		}
		emit := func(ev *Event) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return p.notify("$/event", map[string]any{"token": in.Token, "event": ev})
		}
		out, err := h(ctx, &in.Outgoing, in.Request, emit)
		return map[string]any{"outcome": out}, err
	})
}

// ListModels registers provider.listModels.
func (p *Plugin) ListModels(h func(ctx context.Context, u *Upstream, c *Credential) ([]ir.ModelInfo, error)) {
	p.Handle("provider.listModels", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[struct {
			Upstream   Upstream    `json:"upstream"`
			Credential *Credential `json:"credential"`
		}](raw)
		if err != nil {
			return nil, err
		}
		ms, err := h(ctx, &in.Upstream, in.Credential)
		return map[string]any{"models": ms}, err
	})
}

// Refresh registers provider.refresh.
func (p *Plugin) Refresh(h func(ctx context.Context, u *Upstream, c *Credential) (secret string, expires time.Time, err error)) {
	p.Handle("provider.refresh", func(ctx context.Context, raw json.RawMessage) (any, error) {
		in, err := decode[struct {
			Upstream   Upstream    `json:"upstream"`
			Credential *Credential `json:"credential"`
		}](raw)
		if err != nil {
			return nil, err
		}
		s, exp, err := h(ctx, &in.Upstream, in.Credential)
		return map[string]any{"secret": s, "expires": exp}, err
	})
}

// Stream helpers for providers.

// StartEvent opens a response.
func StartEvent(id, model string) *Event {
	return &Event{Event: ir.Event{Kind: ir.EvStart, ID: id, Model: model}}
}

// TextBlockEvents returns block_start, one delta and block_stop for a text block.
func TextBlockEvents(index int, text string) []*Event {
	return []*Event{
		{Event: ir.Event{Kind: ir.EvBlockStart, Index: index, Block: &ir.Block{Type: ir.BlockText}}},
		{Event: ir.Event{Kind: ir.EvDelta, Index: index, Delta: &ir.Delta{Kind: ir.DeltaText, Text: text}}},
		{Event: ir.Event{Kind: ir.EvBlockStop, Index: index}},
	}
}

// DoneEvent ends a response normally.
func DoneEvent(stop ir.StopReason, usage *ir.Usage) *Event {
	return &Event{Event: ir.Event{Kind: ir.EvDone, StopReason: stop, Usage: usage}}
}

// ErrorEvent ends a response abnormally.
func ErrorEvent(kind ir.ErrorKind, message string) *Event {
	return &Event{Event: ir.Event{Kind: ir.EvError}, Err: &Error{Kind: kind, Message: message}}
}
