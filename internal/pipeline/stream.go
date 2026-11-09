package pipeline

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/internal/convert"
	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/sse"
)

// errIncomplete means the upstream stream ended without its terminal event.
var errIncomplete = errors.New("upstream stream ended before its terminal event")

// stream relays a 2xx streaming response.
func (x *exchange) stream(a *attempt, resp *http.Response) result {
	defer resp.Body.Close()
	x.out.setSticky(x.stickyHeader(a.cand))
	if a.tier == plugin.TierConvert {
		return x.streamConvert(a, resp)
	}
	return x.streamPassthrough(a, resp)
}

// streamPassthrough copies the upstream's frames verbatim while a
// read-only decoder of the same protocol watches usage and the end.
func (x *exchange) streamPassthrough(a *attempt, resp *http.Response) result {
	dec := a.target.NewStreamDecoder()
	header := egress.InboundResponseHeaders(resp.Header)
	fr := &framer{r: bufio.NewReaderSize(resp.Body, 64<<10), max: sse.DefaultMaxEventSize}
	feed := &frameFeed{}
	rd := sse.NewReader(feed, 0)
	upstreamEnded := false // the upstream sent its own error event
	for {
		frame, rerr := fr.next()
		if len(frame) > 0 {
			a.touch(x)
			feed.b = frame
			var events []ir.Event
			for {
				ev, err := rd.Next()
				if err != nil {
					break
				}
				evs, _ := dec.Feed(ev)
				events = append(events, evs...)
			}
			content, failed := false, false
			for i := range events {
				e := &events[i]
				a.observe(e)
				if e.Kind == ir.EvError {
					if _, released := x.out.state(); !released {
						return x.streamSignal(a, resp.Header, e.Err, frame)
					}
					failed = true
				}
				if gw := x.watchEvent(e); gw != nil {
					a.setUsage(dec.Usage())
					return x.endStream(a, nil, gw, nil)
				}
				content = content || e.IsContent()
			}
			switch {
			case upstreamEnded:
				// The error was the client's last frame: what follows
				// is not judged.
			case failed:
				// Held content was never judged and never leaves; the
				// error goes straight out.
				x.held.reset()
				x.held.off = true
				upstreamEnded = true
				if err := x.writeFrame(frame, false, header); err != nil {
					return x.clientGone(a)
				}
			case a.hideUsage && usageOnlyChunk(frame):
			default:
				if err := x.sendFrame(frame, content, header); err != nil {
					return x.clientGone(a)
				}
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				a.setUsage(dec.Usage())
				return x.cut(a, a.watch.Err(rerr), nil)
			}
			break
		}
	}
	trailing, complete := dec.Finish()
	for i := range trailing {
		a.observe(&trailing[i])
	}
	a.setUsage(dec.Usage())
	if upstreamEnded {
		x.finishDelivered(a, false, ir.Errorf(ir.ErrUpstream, "upstream stream failed"))
		return result{next: nextDone}
	}
	if !complete {
		return x.cut(a, errIncomplete, nil)
	}
	if err := x.flushHeld(true); err != nil {
		return x.clientGone(a)
	}
	if err := x.out.release(header); err != nil {
		return x.clientGone(a)
	}
	x.succeed(a)
	return result{next: nextDone}
}

// streamConvert decodes the upstream's stream, converts each event and
// renders it in the client's protocol.
func (x *exchange) streamConvert(a *attempt, resp *http.Response) result {
	dec := a.target.NewStreamDecoder()
	cs := x.newConvStream(a, resp.Header)
	defer cs.close()
	rd := sse.NewReader(resp.Body, 0)
	// Once the client has its terminal event the response is complete;
	// the upstream's trailer (e.g. chat's [DONE]) is not waited for, since
	// the client may already have hung up.
	for !cs.ended && !cs.done {
		ev, err := rd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			a.setUsage(dec.Usage())
			return x.cut(a, a.watch.Err(err), cs.enc)
		}
		a.touch(x)
		events, ferr := dec.Feed(ev)
		if res, stop := cs.emit(events); stop {
			return res
		}
		if ferr != nil {
			a.setUsage(dec.Usage())
			return x.cut(a, ferr, cs.enc)
		}
	}
	trailing, complete := dec.Finish()
	if !cs.ended && !cs.done {
		if res, stop := cs.emit(trailing); stop {
			return res
		}
	}
	a.setUsage(dec.Usage())
	return cs.end(complete || cs.done)
}

// convStream renders upstream IR events for the client: response
// conversion, event hooks, then the client's stream encoder.
type convStream struct {
	x      *exchange
	a      *attempt
	header http.Header // upstream response headers, for classification
	conv   *convert.Response
	enc    protocol.StreamEncoder
	ended  bool // an error event reached the client
	// done is set once the client's terminal event (EvDone) was written.
	// Clients such as Codex hang up as soon as they read it, while the
	// upstream stream still has its own trailer to send; a write failing
	// after that point is a finished response, not a disconnect.
	done bool
	// content marks the event being encoded as content, for heldWriter.
	content bool
}

func (x *exchange) newConvStream(a *attempt, header http.Header) *convStream {
	cs := &convStream{x: x, a: a, header: header, conv: convert.NewResponse(a.cctx)}
	cs.enc = x.codec.NewStreamEncoder(sse.NewWriter(heldWriter{x: x, content: &cs.content}), x.clientReq())
	return cs
}

func (cs *convStream) close() { cs.a.mergeDiag(cs.conv.Diagnostic()) }

// emit renders events; stop=true ends the attempt with res.
func (cs *convStream) emit(events []ir.Event) (res result, stop bool) {
	x, a := cs.x, cs.a
	for i := range events {
		e := &events[i]
		a.observe(e)
		if e.Kind == ir.EvError {
			if _, released := x.out.state(); !released {
				return x.streamSignal(a, cs.header, e.Err, nil), true
			}
		}
		for _, ce := range cs.conv.Event(e) {
			drop, gw := x.filterEvent(&ce)
			if gw != nil {
				return x.endStream(a, cs.enc, gw, nil), true
			}
			if drop {
				continue
			}
			cs.content = ce.IsContent() || ce.Kind == ir.EvError
			if ce.Kind == ir.EvError {
				// Held content was never judged and never leaves; the
				// error goes straight out.
				x.held.reset()
				x.held.off = true
			}
			if err := cs.enc.Encode(&ce); err != nil {
				return cs.gone(), true
			}
			switch ce.Kind {
			case ir.EvError:
				cs.ended = true
			case ir.EvDone:
				cs.done = true
			}
			if cs.content && !x.held.holding() {
				if err := x.out.release(nil); err != nil {
					return cs.gone(), true
				}
			}
			if cs.ended {
				// The client has its error event; nothing after it in this
				// batch may follow.
				return result{}, false
			}
		}
	}
	return result{}, false
}

// end finishes a stream whose input ended; complete reports whether the
// upstream sent its terminal event.
func (cs *convStream) end(complete bool) result {
	x, a := cs.x, cs.a
	if cs.ended {
		x.finishDelivered(a, false, ir.Errorf(ir.ErrUpstream, "upstream stream failed"))
		return result{next: nextDone}
	}
	if !complete {
		return x.cut(a, errIncomplete, cs.enc)
	}
	if err := x.flushHeld(true); err != nil {
		return cs.gone()
	}
	if err := x.out.release(nil); err != nil {
		return cs.gone()
	}
	x.succeed(a)
	return result{next: nextDone}
}

// gone handles a failed write to the client: after the terminal event the
// response is complete and counts as a success; before it, the client left.
func (cs *convStream) gone() result {
	if cs.done && !cs.ended {
		cs.x.succeed(cs.a)
		return result{next: nextDone}
	}
	return cs.x.clientGone(cs.a)
}

// streamSignal handles an error event the upstream sent before any
// content: the attempt failed like an HTTP error would have.
func (x *exchange) streamSignal(a *attempt, header http.Header, sig *ir.GwError, raw []byte) result {
	if sig == nil {
		sig = ir.Errorf(ir.ErrUpstream, "upstream stream failed")
	}
	o := x.outcome(a)
	o.Status = kindStatus(sig.Kind)
	o.Header, o.Signal, o.Sent = header, sig, true
	o.Body = raw
	if raw == nil {
		o.Body = sig.Upstream
	}
	a.status = o.Status
	return x.judge(a, o, nil)
}

// cut handles a stream that broke. Before any content it is an ordinary
// failed attempt; after content the client gets its protocol's error event
// and the request ends.
func (x *exchange) cut(a *attempt, err error, enc protocol.StreamEncoder) result {
	o := x.outcome(a)
	o.Err, o.Sent = err, true
	if _, released := x.out.state(); !released {
		return x.judge(a, o, nil)
	}
	if x.ctx.Err() != nil {
		return x.clientGone(a)
	}
	v := a.up.Provider.Classify(o)
	_ = x.p.d.Router.Report(x.ctx, a.cand, v, a.cand.UpstreamModel)
	a.cause = v.Cause
	gw := transportError(err)
	if errors.Is(err, errIncomplete) {
		gw = ir.Errorf(ir.ErrUpstream, "upstream stream ended unexpectedly")
	}
	return x.endStream(a, enc, gw, err)
}

// endStream ends a delivered stream with the client protocol's error
// event. enc is the attempt's encoder (convert tier); nil builds one primed
// with the upstream's start event so only the error is written.
func (x *exchange) endStream(a *attempt, enc protocol.StreamEncoder, gw *ir.GwError, cause error) result {
	// What hooks held back never leaves; the error goes straight out.
	x.held.reset()
	x.held.off = true
	if cause != nil {
		a.errMsg = errText(cause)
	} else {
		a.errMsg = gw.Message
	}
	if enc == nil {
		sw := &switchWriter{o: x.out}
		enc = x.codec.NewStreamEncoder(sse.NewWriter(sw), x.clientReq())
		if a.startEv != nil {
			_ = enc.Encode(a.startEv)
		}
		sw.live = true
	}
	_ = x.out.release(nil)
	_ = enc.Encode(&ir.Event{Kind: ir.EvError, Err: gw})
	x.finishDelivered(a, false, gw)
	return result{next: nextDone}
}

// clientGone ends the request after the client stopped reading.
func (x *exchange) clientGone(a *attempt) result {
	a.errMsg = "client disconnected"
	x.finishDelivered(a, false, &ir.GwError{Kind: ErrClientClosed, Message: "client disconnected"})
	x.disconnected = true
	x.status = statusClientClosed
	return result{next: nextDone}
}

// watchEvent runs the event hooks over a passthrough event read-only;
// only an abort is honored, since the bytes go out unchanged.
func (x *exchange) watchEvent(e *ir.Event) *ir.GwError {
	cp := *e
	_, gw := x.filterEvent(&cp)
	return gw
}

// filterEvent runs the event hooks and records what holders hold back
// (see heldFrames). It reports whether to drop the event, or the error to
// end the stream with. Each hook works on a copy that is
// kept only when the hook returned within the event budget; an overrun is
// a plugin failure under its FailPolicy.
func (x *exchange) filterEvent(e *ir.Event) (bool, *ir.GwError) {
	x.held.seq++
	for _, pl := range x.entered {
		h, ok := pl.(plugin.EventHook)
		if !ok || !supports(pl, "middleware.onEvent") {
			continue
		}
		info := pl.Info()
		cp := copyEvent(e)
		start := time.Now()
		act, err := h.OnEvent(x.call.WithPlugin(info.ID), cp)
		if took := time.Since(start); err == nil && took > x.p.d.EventHookBudget {
			err = fmt.Errorf("%w: event hook took %v (budget %v)", plugin.ErrPluginFailure, took, x.p.d.EventHookBudget)
		}
		holder, holds := h.(plugin.EventHolder)
		if err != nil {
			if gw, stop := x.hookError(info, "event", err); stop {
				return false, gw
			}
			if holds {
				x.held.hold(info.ID, 0)
			}
			continue
		}
		if holds {
			x.held.hold(info.ID, holder.Held(x.call.WithPlugin(info.ID)))
		}
		*e = *cp
		switch act {
		case plugin.EventDrop:
			return true, nil
		case plugin.EventAbort:
			return false, ir.Errorf(ir.ErrPermission, "response stopped by %s", info.ID)
		}
	}
	return false, nil
}

// copyEvent copies an event and the structs it points to one level deep,
// so a hook's edits can be discarded.
func copyEvent(e *ir.Event) *ir.Event {
	cp := *e
	if e.Block != nil {
		b := *e.Block
		cp.Block = &b
	}
	if e.Delta != nil {
		d := *e.Delta
		cp.Delta = &d
	}
	if e.Usage != nil {
		u := *e.Usage
		cp.Usage = &u
	}
	return &cp
}

// touch records the first byte of the response body.
func (a *attempt) touch(x *exchange) {
	if a.firstAt.IsZero() {
		a.firstAt = x.p.d.Now()
	}
}

// observe tracks what an upstream event delivered.
func (a *attempt) observe(e *ir.Event) {
	switch e.Kind {
	case ir.EvStart:
		if a.startEv == nil {
			cp := *e
			a.startEv = &cp
		}
	case ir.EvDelta:
		if e.Delta != nil {
			a.outChars += len(e.Delta.Text)
		}
	case ir.EvDone:
		a.done = true
	}
	if e.Usage != nil {
		a.usage = *e.Usage
	}
}

// setUsage keeps the decoder's usage unless it reported nothing.
func (a *attempt) setUsage(u ir.Usage) {
	if !u.IsZero() || len(u.Raw) > 0 {
		a.usage = u
	}
}

func (a *attempt) mergeDiag(d ir.Diagnostic) {
	if len(d.Steps)+len(d.Dropped)+len(d.Notes) == 0 {
		return
	}
	if a.diag == nil {
		a.diag = &ir.Diagnostic{Fidelity: ir.Lossless}
	}
	a.diag.Steps = append(a.diag.Steps, d.Steps...)
	a.diag.Dropped = append(a.diag.Dropped, d.Dropped...)
	a.diag.Notes = append(a.diag.Notes, d.Notes...)
	a.diag.Fidelity = ir.Worse(a.diag.Fidelity, d.Fidelity)
}

// framer splits an SSE byte stream into complete frames (an event and its
// terminating blank line) without altering a byte.
type framer struct {
	r   *bufio.Reader
	max int
}

func (f *framer) next() ([]byte, error) {
	var frame []byte
	for {
		line, err := f.r.ReadSlice('\n')
		frame = append(frame, line...)
		if len(frame) > f.max {
			return nil, sse.ErrEventTooLarge
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return frame, err
		}
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			return frame, nil
		}
	}
}

// frameFeed hands one frame at a time to the shared sse.Reader; it reports
// EOF when the frame is consumed so the reader never blocks.
type frameFeed struct{ b []byte }

func (f *frameFeed) Read(p []byte) (int, error) {
	if len(f.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, f.b)
	f.b = f.b[n:]
	return n, nil
}

// switchWriter discards frames until live, then writes them to the client.
type switchWriter struct {
	o    *clientOut
	live bool
}

func (s *switchWriter) Write(p []byte) (int, error) {
	if !s.live {
		return len(p), nil
	}
	return liveWriter{s.o}.Write(p)
}

// usageOnlyChunk reports whether an OpenAI chat or completions frame is the
// final chunk that include_usage adds: no choices, a usage object.
func usageOnlyChunk(frame []byte) bool {
	for _, line := range bytes.Split(frame, []byte("\n")) {
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		v := gjson.ParseBytes(bytes.TrimSpace(data))
		ch := v.Get("choices")
		return v.Get("usage").IsObject() && (!ch.Exists() || (ch.IsArray() && len(ch.Array()) == 0))
	}
	return false
}
