package pipeline

import (
	"context"
	"time"

	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/clientid"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// finish accounts for the request once it is answered: attempt costs,
// meter settlement, stickiness, finish hooks and the record.
//
// It runs before the handler returns, so the end of the HTTP body (the last
// chunk, or END_STREAM) waits for it; the client already has the protocol's
// own end event. It stays on the handler goroutine because the request
// context is cancelled on return (settlement and finish hooks read it
// through Call), net/http forbids using the Request afterwards, and a panic
// would escape the server's recovery.
func (x *exchange) finish() {
	if x.out != nil {
		x.out.close()
	}
	x.upload.close()
	end := x.p.d.Now()
	bg := context.WithoutCancel(x.ctx)

	rec := &plugin.Record{
		ID:        x.id,
		Time:      x.started,
		Caller:    *x.caller,
		Protocol:  x.codec.Name(),
		Model:     x.in.Model,
		Stream:    x.in.Stream,
		SessionID: x.session,
		ClientUA:  x.r.UserAgent(),
		Status:    x.status,
		Duration:  end.Sub(x.started),
	}
	clientid.Stamp(rec, x.r.Header)
	if x.call != nil {
		rec.ClientIP = x.call.ClientIP
	}
	for _, a := range x.attempts {
		x.settleUsage(a)
		ar := x.attemptRecord(a, end)
		rec.CostMicro += ar.CostMicro
		rec.Attempts = append(rec.Attempts, ar)
		if m := x.p.d.Metrics; m != nil {
			m.Attempt(x.codec.Name(), &ar)
		}
	}
	x.attachNotes(rec)

	delivered := x.delivered
	released := x.complete || x.committed
	if x.out != nil {
		_, released = x.out.state()
		released = released || x.complete
	}
	var charge int64
	var detail *plugin.Charge
	if delivered != nil {
		rec.Usage = delivered.usage
		charge, detail = x.charge(delivered)
		if !delivered.firstAt.IsZero() {
			rec.TTFB = delivered.firstAt.Sub(x.started)
		}
	} else if x.replyUsage != nil {
		rec.Usage = *x.replyUsage
	}

	// Charged: a complete response, or one the client walked away from
	// (it got what was delivered). A stream the upstream broke after
	// content is refunded; its operator cost is still recorded.
	settle := delivered != nil && released && (x.complete || x.disconnected)
	if settle {
		rec.ChargeMicro = charge
		for i := range rec.Attempts {
			if x.attempts[i] == delivered {
				rec.Attempts[i].Charge = detail
			}
		}
	}
	if m := x.chain.Meter; m != nil && x.reservation != nil {
		call := x.call.WithPlugin(m.Info().ID)
		var err error
		switch {
		case settle:
			if supports(m, "meter.settle") {
				if detail != nil {
					call.UnitChargeMicro = min(detail.Rates.UnitCharge(rec.Usage), charge)
				}
				err = m.Settle(call, x.reservation, rec.Usage, charge)
			}
		case supports(m, "meter.refund"):
			err = m.Refund(call, x.reservation)
		}
		if err != nil {
			x.p.d.Logger.Error("meter settlement failed", "request", x.id, "err", err)
		}
	}

	if delivered != nil && x.complete {
		turn := router.Turn{
			SessionKey:    x.session,
			Caller:        *x.caller,
			Candidate:     delivered.cand,
			SentSessionID: x.sentSessionOf(delivered),
			Usage:         delivered.usage,
		}
		if x.needs.Cache1h {
			turn.CacheTTL = time.Hour
		}
		if err := x.p.d.Router.Observe(bg, turn); err != nil {
			x.p.d.Logger.Warn("session binding failed", "request", x.id, "err", err)
		}
	}

	if x.err != nil {
		rec.ErrorKind, rec.Error = x.err.Kind, x.err.Message
	}
	res := &plugin.Result{Status: x.status, Err: x.err, Usage: rec.Usage, Attempts: len(x.attempts), Delivered: released}
	for i := len(x.entered) - 1; i >= 0; i-- {
		if h, ok := x.entered[i].(plugin.FinishHook); ok && supports(x.entered[i], "middleware.onFinish") {
			x.runFinish(x.entered[i].Info().ID, h, res)
		}
	}
	if m := x.p.d.Metrics; m != nil {
		m.Request(rec)
	}
	if q := x.p.d.Records; q != nil && !q.Enqueue(rec) {
		x.p.d.Logger.Warn("request record dropped by best-effort sinks", "request", x.id)
	}
}

func (x *exchange) runFinish(id string, h plugin.FinishHook, res *plugin.Result) {
	defer func() {
		if v := recover(); v != nil {
			x.p.d.Logger.Error("finish hook panicked", "plugin", id, "request", x.id, "panic", v)
		}
	}()
	h.OnFinish(x.call.WithPlugin(id), res)
}

func (x *exchange) attemptRecord(a *attempt, end time.Time) plugin.AttemptRecord {
	ar := plugin.AttemptRecord{
		N:             a.n,
		UpstreamID:    a.cand.UpstreamID,
		EndpointID:    a.cand.EndpointID,
		CredentialID:  a.cand.CredentialID,
		Protocol:      a.ep.Protocol,
		Model:         x.served[router.Key(a.cand)],
		UpstreamModel: a.cand.UpstreamModel,
		Tier:          a.tier,
		Status:        a.status,
		Cause:         a.cause,
		Sent:          a.wrote.Load(),
		Duration:      end.Sub(a.start),
		Usage:         a.usage,
		Diagnostic:    a.diag,
		Error:         a.errMsg,
	}
	if !a.firstAt.IsZero() {
		ar.TTFB = a.firstAt.Sub(a.start)
	} else if !a.headerAt.IsZero() {
		ar.TTFB = a.headerAt.Sub(a.start)
	}
	if ar.Sent {
		ar.CostMicro = x.cost(a)
	}
	return ar
}

// sentSessionOf is the session id the attempt carried upstream.
func (x *exchange) sentSessionOf(a *attempt) string {
	if a.cctx != nil && !a.cctx.SameProtocol {
		if a.sent != nil {
			return a.sent.CacheKey
		}
		return ""
	}
	h := x.in.Session
	if h.SessionID != "" {
		return h.SessionID
	}
	return h.CacheKey
}
