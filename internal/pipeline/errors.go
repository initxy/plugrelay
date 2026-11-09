package pipeline

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/sse"
)

// ErrClientClosed is the record's error kind when the client went away
// mid-response; such a request is settled for what was delivered.
const ErrClientClosed ir.ErrorKind = "client_closed"

// statusClientClosed is recorded when the client went away (nginx's 499).
const statusClientClosed = 499

// fail answers the request with a relay error in the client's protocol.
func (x *exchange) fail(gw *ir.GwError) { x.failWith(&failure{err: gw}) }

// failWith answers with a failure: the upstream's own response when it is
// verbatim, else the client codec's rendering. Once the stream is
// committed only the protocol's error event is possible.
func (x *exchange) failWith(f *failure) {
	x.err = f.err
	if x.ctx.Err() != nil {
		x.status = statusClientClosed
		return
	}
	status, h, body := 0, http.Header(nil), []byte(nil)
	if v := f.verbatim; v != nil {
		status, h, body = v.status, v.header.Clone(), v.body
	} else {
		status, h, body = x.codec.EncodeError(f.err)
	}
	if h == nil {
		h = http.Header{}
	}
	x.addHeaders(h, nil)
	if x.out == nil {
		writePlain(x.w, status, h, body)
		x.status = status
		return
	}
	if x.out.httpError(status, h, body) {
		x.status = status
		x.out.close()
		return
	}
	enc := x.codec.NewStreamEncoder(sse.NewWriter(liveWriter{x.out}), x.clientReq())
	_ = enc.Encode(&ir.Event{Kind: ir.EvError, Err: f.err})
	x.status = http.StatusOK
}

// planError maps a routing failure to the client error it stands for.
func planError(err error) *ir.GwError {
	var pe *router.PlanError
	if !errors.As(err, &pe) {
		return ir.Errorf(ir.ErrInternal, "routing failed")
	}
	switch pe.Kind {
	case router.ErrNoRoute:
		return ir.Errorf(ir.ErrNotFound, "model %q not found", pe.Model)
	case router.ErrImpossible:
		if len(pe.Missing) == 0 {
			return ir.Errorf(ir.ErrCapabilityMissing, "no endpoint of model %q can serve this request", pe.Model)
		}
		return ir.Errorf(ir.ErrCapabilityMissing, "no endpoint of model %q supports: %s", pe.Model, strings.Join(pe.Missing, ", "))
	case router.ErrCooling:
		e := ir.Errorf(ir.ErrRateLimited, "model %q is rate limited; retry later", pe.Model)
		if !pe.RetryAt.IsZero() {
			e.RetryAfter = max(time.Until(pe.RetryAt).Round(time.Second), time.Second)
		}
		return e
	case router.ErrBusy:
		e := ir.Errorf(ir.ErrOverloaded, "model %q is at capacity", pe.Model)
		e.RetryAfter = time.Second
		return e
	}
	return ir.Errorf(ir.ErrOverloaded, "no upstream available for model %q", pe.Model)
}

// transportError is the client error for an attempt without a usable
// response.
func transportError(err error) *ir.GwError {
	var te *egress.TimeoutError
	switch {
	case errors.As(err, &te):
		return ir.Errorf(ir.ErrTimeout, "upstream %s", te.Error()[len("egress: "):])
	case errors.Is(err, sse.ErrEventTooLarge):
		return ir.Errorf(ir.ErrUpstream, "upstream sent an event larger than the relay accepts")
	case errors.Is(err, context.Canceled):
		return ir.Errorf(ir.ErrUpstream, "request canceled")
	}
	return ir.Errorf(ir.ErrUpstream, "upstream connection failed")
}

// kindStatus is the HTTP status an in-stream upstream error stands for,
// so providers classify it like the equivalent HTTP response.
func kindStatus(k ir.ErrorKind) int {
	switch k {
	case ir.ErrInvalidRequest, ir.ErrContextTooLong, ir.ErrContentFiltered, ir.ErrCapabilityMissing:
		return http.StatusBadRequest
	case ir.ErrAuthentication:
		return http.StatusUnauthorized
	case ir.ErrPermission:
		return http.StatusForbidden
	case ir.ErrNotFound:
		return http.StatusNotFound
	case ir.ErrRateLimited:
		return http.StatusTooManyRequests
	case ir.ErrQuotaExhausted:
		return http.StatusPaymentRequired
	case ir.ErrOverloaded:
		return 529
	case ir.ErrTimeout:
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

// baseHeader is the meter's quota headers, read once per request.
func (x *exchange) baseHeader() http.Header {
	if x.base != nil {
		return x.base
	}
	x.base = http.Header{}
	if m := x.chain.Meter; m != nil && x.call != nil {
		if q, ok := m.(plugin.QuotaHeaders); ok && supports(m, "meter.headers") {
			for k, vs := range q.Headers(x.call.WithPlugin(m.Info().ID)) {
				x.base[k] = vs
			}
		}
	}
	return x.base
}

// stickyHeader is the relay-signed turn token for clients that echo one.
func (x *exchange) stickyHeader(c plugin.Candidate) http.Header {
	s, ok := x.codec.(protocol.StickyHeader)
	if !ok {
		return nil
	}
	return http.Header{http.CanonicalHeaderKey(s.StickyHeaderName()): {x.p.d.Router.IssueTurnToken(c)}}
}

// succeed records a fully delivered response.
func (x *exchange) succeed(a *attempt) {
	_ = x.p.d.Router.Report(x.ctx, a.cand, plugin.Verdict{}, a.cand.UpstreamModel)
	if x.status == 0 {
		x.status = http.StatusOK
	}
	x.delivered, x.complete = a, true
}

// finishDelivered records a response that reached the client but did not
// end normally.
func (x *exchange) finishDelivered(a *attempt, complete bool, gw *ir.GwError) {
	if x.status == 0 {
		x.status = http.StatusOK
	}
	x.delivered, x.complete = a, complete
	if !complete {
		x.err = gw
	}
}

// meterRefusal is the client error for a meter that would not reserve or
// hold: the meter's own rejection as it is, else "metering unavailable"
// (billing fails closed: an unknown balance never serves). An exhausted
// balance is never worth retrying.
func meterRefusal(err error) *ir.GwError {
	var gw *ir.GwError
	if !errors.As(err, &gw) || plugin.IsFailure(err) {
		return ir.Errorf(ir.ErrInternal, "metering unavailable")
	}
	if gw.Kind == ir.ErrQuotaExhausted && !gw.NoRetry {
		c := *gw
		c.NoRetry = true
		gw = &c
	}
	return gw
}
