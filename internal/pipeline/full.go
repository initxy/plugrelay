package pipeline

import (
	"errors"
	"io"
	"net/http"

	"github.com/initxy/plugrelay/internal/convert"
	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// full relays a 2xx non-streaming response.
func (x *exchange) full(a *attempt, resp *http.Response) result {
	if bs, ok := x.codec.(protocol.BodyStreamer); ok && a.tier != plugin.TierConvert &&
		bs.StreamsBody(x.in, resp.Header.Get("Content-Type")) {
		return x.copyBody(a, resp)
	}
	data, err := readBounded(resp.Body, x.p.d.MaxResponseBody)
	resp.Body.Close()
	if err != nil {
		o := x.outcome(a)
		o.Err, o.Sent = a.watch.Err(err), true
		return x.judge(a, o, nil)
	}
	a.touch(x)
	if a.tier != plugin.TierConvert {
		r, err := a.target.DecodeResponse(data)
		if err == nil {
			a.usage = r.Usage
			a.outChars = contentChars(r.Content)
			edited, gw := x.checkResponse(r)
			if gw != nil {
				a.usage = x.mediaUsage(a.usage, true)
				return x.refuseResponse(a, gw)
			}
			if edited {
				if out, eerr := x.codec.EncodeResponse(x.clientReq(), r); eerr == nil {
					data = out
				}
			}
		}
		a.usage = x.mediaUsage(a.usage, err == nil)
		h := egress.InboundResponseHeaders(resp.Header)
		x.addHeaders(h, a)
		writePlain(x.w, resp.StatusCode, h, data)
		x.status = resp.StatusCode
		x.succeed(a)
		return result{next: nextDone}
	}
	r, err := a.target.DecodeResponse(data)
	if err != nil {
		o := x.outcome(a)
		o.Status, o.Header, o.Body, o.Sent = http.StatusBadGateway, resp.Header, data, true
		o.Signal = ir.Errorf(ir.ErrUpstream, "upstream returned an unreadable response: %v", err)
		return x.judge(a, o, nil)
	}
	return x.deliverFull(a, r)
}

// deliverFull converts a complete upstream response and sends it.
func (x *exchange) deliverFull(a *attempt, r *ir.Response) result {
	a.usage = r.Usage
	a.outChars = contentChars(r.Content)
	conv := convert.NewResponse(a.cctx)
	conv.Full(r)
	a.mergeDiag(conv.Diagnostic())
	if _, gw := x.checkResponse(r); gw != nil {
		return x.refuseResponse(a, gw)
	}
	out, err := x.codec.EncodeResponse(x.clientReq(), r)
	if err != nil {
		a.errMsg = "encode response: " + err.Error()
		return result{next: nextStop, fail: &failure{err: ir.Errorf(ir.ErrInternal, "could not render the response")}}
	}
	h := http.Header{"Content-Type": {"application/json"}}
	x.addHeaders(h, a)
	writePlain(x.w, http.StatusOK, h, out)
	x.status = http.StatusOK
	x.succeed(a)
	return result{next: nextDone}
}

func contentChars(bs []ir.Block) int {
	n := 0
	for _, b := range bs {
		n += len(b.Text)
		if b.Tool != nil {
			n += len(b.Tool.Input) + len(b.Tool.Text)
		}
	}
	return n
}

// addHeaders adds the meter's quota headers and the sticky turn header.
func (x *exchange) addHeaders(h http.Header, a *attempt) {
	for k, vs := range x.baseHeader() {
		h[k] = vs
	}
	if a != nil {
		for k, vs := range x.stickyHeader(a.cand) {
			h[k] = vs
		}
	}
}

// copyBody relays a same-protocol body the client codec does not buffer
// (audio, an event stream it does not interpret): bytes are written as
// they arrive. Once the status is sent nothing can be retried; a body cut
// by the upstream is not charged.
func (x *exchange) copyBody(a *attempt, resp *http.Response) result {
	defer resp.Body.Close()
	h := egress.InboundResponseHeaders(resp.Header)
	x.addHeaders(h, a)
	dst := x.w.Header()
	for k, vs := range h {
		dst[k] = vs
	}
	x.w.WriteHeader(resp.StatusCode)
	x.status, x.committed = resp.StatusCode, true
	a.usage = x.mediaUsage(ir.Usage{}, false)
	rc := http.NewResponseController(x.w)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			a.touch(x)
			if _, werr := x.w.Write(buf[:n]); werr != nil {
				x.disconnected = true
				x.finishDelivered(a, false, &ir.GwError{Kind: ErrClientClosed, Message: "client went away"})
				return result{next: nextDone}
			}
			_ = rc.Flush()
		}
		if errors.Is(err, io.EOF) {
			x.succeed(a)
			return result{next: nextDone}
		}
		if err != nil {
			if x.ctx.Err() != nil {
				x.disconnected = true
			}
			x.finishDelivered(a, false, transportError(a.watch.Err(err)))
			return result{next: nextDone}
		}
	}
}

// mediaUsage completes an upstream's usage with the request's metered
// facts: size and quality of images, characters to speak. decoded reports
// that the response itself was read; when it was not (a text transcript,
// an event stream), images asked for and the uploaded audio's length
// stand in, marked Estimated, so no response format goes unbilled.
func (x *exchange) mediaUsage(u ir.Usage, decoded bool) ir.Usage {
	if x.upload != nil && (x.in.Op == protocol.OpTranscribe || x.in.Op == protocol.OpTranslate) &&
		u.AudioMs == 0 && u.Input == 0 && u.Output == 0 {
		if ms := x.upload.audioMs(); ms > 0 {
			u.AudioMs, u.Estimated = ms, true
		}
	}
	req := x.req
	if req == nil || req.Units == nil {
		return u
	}
	m := req.Units
	if !decoded && u.Images == 0 && m.Images > 0 {
		u.Images, u.Estimated = int64(m.Images), true
	}
	if u.Images > 0 {
		if u.ImageSize == "" {
			u.ImageSize = m.ImageSize
		}
		if u.ImageQuality == "" {
			u.ImageQuality = m.ImageQuality
		}
	}
	if u.Chars == 0 {
		u.Chars = m.Chars
	}
	return u
}
