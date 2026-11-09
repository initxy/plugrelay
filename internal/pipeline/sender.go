package pipeline

import (
	"encoding/json"
	"errors"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// sender returns the provider's own transport, if it has one.
func sender(p plugin.Provider) plugin.Sender {
	s, ok := p.(plugin.Sender)
	if !ok || !supports(p, "provider.send") {
		return nil
	}
	return s
}

// sendVia lets a provider perform the upstream call itself. Its events
// take the convert tier's path to the client; an Outcome returned before
// any event is classified like an HTTP failure.
func (x *exchange) sendVia(s plugin.Sender, a *attempt) result {
	out, gw := x.outgoing(a)
	if out == nil {
		if gw != nil {
			return result{next: nextStop, fail: &failure{err: gw}}
		}
		return x.transportFailure(a)
	}
	defer a.watch.Stop()
	a.wrote.Store(true)
	ctx := a.watch.Context()
	if x.in.Stream {
		return x.sendStream(s, a, out)
	}
	acc := &accumulator{}
	var signal *ir.GwError
	o, err := s.Send(ctx, out, a.sent, func(ev *ir.Event) error {
		a.touch(x)
		a.watch.Touch() // the provider reads the body itself, so its events are the progress
		a.observe(ev)
		if ev.Kind == ir.EvError && signal == nil {
			signal = ev.Err
		}
		acc.add(ev)
		return nil
	})
	a.status = 200
	switch {
	case o != nil:
		return x.judge(a, x.fillOutcome(a, o), nil)
	case signal != nil:
		return x.streamSignal(a, nil, signal, nil)
	case err != nil || !acc.done:
		oc := x.outcome(a)
		oc.Err, oc.Sent = plugin.RedactURLError(a.watch.Err(orIncomplete(err))), true
		return x.judge(a, oc, nil)
	}
	a.ok = true
	return x.deliverFull(a, acc.resp)
}

func (x *exchange) sendStream(s plugin.Sender, a *attempt, out *plugin.Outgoing) result {
	x.out.setSticky(x.stickyHeader(a.cand))
	cs := x.newConvStream(a, nil)
	defer cs.close()
	var res result
	stopped := false
	done := false
	o, err := s.Send(a.watch.Context(), out, a.sent, func(ev *ir.Event) error {
		if stopped {
			return errStopEmit
		}
		a.touch(x)
		a.watch.Touch() // the provider reads the body itself, so its events are the progress
		if ev.Kind == ir.EvDone {
			done = true
		}
		if r, stop := cs.emit([]ir.Event{*ev}); stop {
			res, stopped = r, true
			return errStopEmit
		}
		return nil
	})
	if stopped {
		return res
	}
	a.status = 200
	if o != nil {
		if _, released := x.out.state(); !released {
			return x.judge(a, x.fillOutcome(a, o), nil)
		}
		err = orIncomplete(o.Err)
	}
	if err != nil && !errors.Is(err, errStopEmit) {
		return x.cut(a, a.watch.Err(err), cs.enc)
	}
	a.ok = true
	return cs.end(done)
}

var errStopEmit = errors.New("pipeline: stream ended")

func orIncomplete(err error) error {
	if err == nil {
		return errIncomplete
	}
	return err
}

// fillOutcome completes a provider-built outcome with the attempt's facts.
func (x *exchange) fillOutcome(a *attempt, o *plugin.Outcome) *plugin.Outcome {
	if o.Upstream == nil {
		o.Upstream = a.up.Config
	}
	if o.Endpoint == nil {
		o.Endpoint = a.ep
	}
	if o.Credential == nil {
		o.Credential = a.cred
	}
	// The one place every provider-made outcome passes: redact here so a
	// provider that forgot to does not leak a query credential.
	o.Err = plugin.RedactURLError(o.Err)
	a.status = o.Status
	if !o.Sent {
		a.wrote.Store(false)
	}
	return o
}

// accumulator rebuilds a complete response from IR events.
type accumulator struct {
	resp   *ir.Response
	blocks map[int]*ir.Block
	order  []int
	input  map[int][]byte
	done   bool
}

func (c *accumulator) add(ev *ir.Event) {
	if c.resp == nil {
		c.resp = &ir.Response{}
		c.blocks = map[int]*ir.Block{}
		c.input = map[int][]byte{}
	}
	switch ev.Kind {
	case ir.EvStart:
		c.resp.ID, c.resp.Model = ev.ID, ev.Model
	case ir.EvBlockStart:
		b := ir.Block{}
		if ev.Block != nil {
			b = *ev.Block
		}
		c.blocks[ev.Index] = &b
		c.order = append(c.order, ev.Index)
	case ir.EvDelta:
		b := c.blocks[ev.Index]
		if b == nil || ev.Delta == nil {
			return
		}
		switch ev.Delta.Kind {
		case ir.DeltaText, ir.DeltaThinking:
			b.Text += ev.Delta.Text
		case ir.DeltaToolInput:
			c.input[ev.Index] = append(c.input[ev.Index], ev.Delta.Text...)
		case ir.DeltaOpaque:
			if ev.Delta.Opaque != nil {
				st := *ev.Delta.Opaque
				b.Opaque = &st
			}
		}
	case ir.EvBlockStop:
		b := c.blocks[ev.Index]
		if b == nil {
			return
		}
		if ev.Block != nil && ev.Block.Opaque != nil {
			b.Opaque = ev.Block.Opaque
		}
		if in, ok := c.input[ev.Index]; ok && b.Tool != nil {
			tc := *b.Tool
			if tc.Kind == ir.ToolFreeform {
				tc.Text = string(in)
			} else {
				tc.Input = json.RawMessage(in)
			}
			b.Tool = &tc
		}
	case ir.EvDone:
		c.done = true
		c.resp.StopReason, c.resp.StopSeq = ev.StopReason, ev.StopSeq
		if ev.Usage != nil {
			c.resp.Usage = *ev.Usage
		}
	}
	if ev.Kind == ir.EvUsage && ev.Usage != nil {
		c.resp.Usage = *ev.Usage
	}
	if ev.Kind == ir.EvDone {
		for _, i := range c.order {
			c.resp.Content = append(c.resp.Content, *c.blocks[i])
		}
	}
}
