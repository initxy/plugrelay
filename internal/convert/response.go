package convert

import (
	"encoding/json"
	"slices"

	"github.com/initxy/plugrelay/pkg/ir"
)

// Response rewrites one upstream response for the client: renamed and
// wrapped tool calls are restored, and upstream-private state is tagged with the
// target's issuer and sealed for (or dropped from) clients of another
// protocol. A Response serves one stream or one full response and is not
// safe for concurrent use.
type Response struct {
	c *Context
	r recorder

	next    int                      // next client block index
	index   map[int]int              // upstream index → client index
	skip    map[int]bool             // upstream blocks not shown to the client
	types   map[int]ir.BlockType     // block type by upstream index
	wrapped map[int][]byte           // buffered input of wrapped tool calls
	frags   map[int][]ir.OpaqueState // buffered opaque deltas
}

// NewResponse returns a converter for responses to a request converted with
// c. It reads c.Tools as filled by Request.
func NewResponse(c *Context) *Response {
	return &Response{
		c:       c,
		index:   map[int]int{},
		skip:    map[int]bool{},
		types:   map[int]ir.BlockType{},
		wrapped: map[int][]byte{},
		frags:   map[int][]ir.OpaqueState{},
	}
}

// Diagnostic reports what the response side did so far.
func (r *Response) Diagnostic() ir.Diagnostic { return r.r.diagnostic() }

// Event converts one upstream event into zero or more client events. Block
// indexes stay dense: dropped blocks shift later ones down.
//
// Opaque deltas bound for a client of another protocol are buffered and
// delivered, sealed as a whole, on the block's stop event; wrapped tool
// input is buffered and delivered as one freeform delta before the stop.
func (r *Response) Event(ev *ir.Event) []ir.Event {
	switch ev.Kind {
	case ir.EvBlockStart:
		return r.blockStart(ev)
	case ir.EvDelta:
		return r.delta(ev)
	case ir.EvBlockStop:
		return r.blockStop(ev)
	}
	return []ir.Event{*ev}
}

func (r *Response) blockStart(ev *ir.Event) []ir.Event {
	i := ev.Index
	if ev.Block == nil {
		r.index[i] = r.alloc()
		out := *ev
		out.Index = r.index[i]
		return []ir.Event{out}
	}
	b := *ev.Block
	r.types[i] = b.Type
	if b.Type == ir.BlockOpaque && !r.c.canEcho() {
		r.skip[i] = true
		r.lostState()
		return nil
	}
	if b.Type == ir.BlockToolUse && b.Tool != nil {
		if o, ok := r.c.Tools[b.Tool.Name]; ok {
			tc := *b.Tool
			r.renamed(&tc, o)
			if o.Kind == ir.ToolFreeform {
				r.wrapped[i] = append([]byte(nil), tc.Input...)
				tc.Kind, tc.Input = ir.ToolFreeform, nil
				r.unwrapped()
			}
			b.Tool = &tc
		}
	}
	if b.Opaque != nil {
		b.Opaque = r.outbound(b.Opaque)
	}
	r.index[i] = r.alloc()
	out := *ev
	out.Index, out.Block = r.index[i], &b
	return []ir.Event{out}
}

func (r *Response) delta(ev *ir.Event) []ir.Event {
	i := ev.Index
	if r.skip[i] {
		return nil
	}
	if buf, ok := r.wrapped[i]; ok && ev.Delta != nil && ev.Delta.Kind == ir.DeltaToolInput {
		r.wrapped[i] = append(buf, ev.Delta.Text...)
		return nil
	}
	out := *ev
	out.Index = r.mapped(i)
	if d := ev.Delta; d != nil && d.Kind == ir.DeltaOpaque && d.Opaque != nil {
		if !r.c.SameProtocol {
			r.frags[i] = append(r.frags[i], *d.Opaque)
			return nil
		}
		cp := *d
		cp.Opaque = r.outbound(d.Opaque)
		out.Delta = &cp
	}
	return []ir.Event{out}
}

func (r *Response) blockStop(ev *ir.Event) []ir.Event {
	i := ev.Index
	defer r.forget(i)
	if r.skip[i] {
		return nil
	}
	idx := r.mapped(i)
	var events []ir.Event
	if buf, ok := r.wrapped[i]; ok {
		var tc ir.ToolCall
		unwrapCall(&tc, buf)
		events = append(events, ir.Event{Kind: ir.EvDelta, Index: idx,
			Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: tc.Text}})
	}
	out := *ev
	out.Index = idx
	var b ir.Block
	if ev.Block != nil {
		b = *ev.Block
	} else {
		b.Type = r.types[i]
	}
	st := b.Opaque
	if frags := r.frags[i]; len(frags) > 0 {
		if st != nil {
			frags = append(frags, *st)
		}
		merged := mergeFragments(frags)
		st = &merged
	}
	if st != nil {
		b.Opaque = r.outbound(st)
		out.Block = &b
	} else if ev.Block != nil {
		out.Block = &b
	}
	if out.Block != nil && out.Block.Tool != nil {
		tc := *out.Block.Tool
		if o, ok := r.c.Tools[tc.Name]; ok {
			r.renamed(&tc, o)
		}
		if _, ok := r.wrapped[i]; ok {
			unwrapCall(&tc, r.wrapped[i])
		}
		out.Block.Tool = &tc
	}
	return append(events, out)
}

// Full converts a complete response in place.
func (r *Response) Full(resp *ir.Response) {
	out := resp.Content[:0:0]
	for _, b := range resp.Content {
		if b.Type == ir.BlockToolUse && b.Tool != nil {
			if o, ok := r.c.Tools[b.Tool.Name]; ok {
				tc := *b.Tool
				r.renamed(&tc, o)
				if o.Kind == ir.ToolFreeform {
					unwrapCall(&tc, tc.Input)
					r.unwrapped()
				}
				b.Tool = &tc
			}
		}
		if b.Opaque != nil {
			b.Opaque = r.outbound(b.Opaque)
			if b.Opaque == nil && b.Type == ir.BlockOpaque {
				continue
			}
		}
		out = append(out, b)
	}
	resp.Content = out
}

func (r *Response) outbound(st *ir.OpaqueState) *ir.OpaqueState {
	o := r.c.outbound(st)
	if o == nil {
		r.lostState()
	}
	return o
}

func (r *Response) lostState() {
	if !slices.Contains(r.r.d.Dropped, "opaque_state") {
		r.r.note("client protocol cannot echo upstream state; dropped")
	}
	r.r.drop("opaque_state")
}

// renamed restores the client's name and namespace of a call.
func (r *Response) renamed(tc *ir.ToolCall, o ToolOrigin) {
	if tc.Name == o.Name && tc.Namespace == o.Namespace {
		return
	}
	tc.Name, tc.Namespace = o.Name, o.Namespace
	if !slices.Contains(r.r.d.Steps, "tools: flattened calls restored to their namespace") {
		r.r.step("tools: flattened calls restored to their namespace")
	}
}

func (r *Response) unwrapped() {
	if !slices.Contains(r.r.d.Steps, "tools: wrapped calls restored as freeform") {
		r.r.step("tools: wrapped calls restored as freeform")
	}
}

func (r *Response) alloc() int {
	n := r.next
	r.next++
	return n
}

// mapped returns the client index of an upstream block; events for blocks
// never started pass through with the next free index as a best effort.
func (r *Response) mapped(i int) int {
	if n, ok := r.index[i]; ok {
		return n
	}
	r.index[i] = r.alloc()
	return r.index[i]
}

func (r *Response) forget(i int) {
	delete(r.skip, i)
	delete(r.wrapped, i)
	delete(r.frags, i)
	delete(r.types, i)
}

// mergeFragments joins streamed pieces of one state. JSON string pieces are
// concatenated as strings; anything else byte-wise.
func mergeFragments(frags []ir.OpaqueState) ir.OpaqueState {
	st := ir.OpaqueState{Issuer: frags[0].Issuer, Kind: frags[0].Kind}
	var sb []byte
	allStrings := true
	var text string
	for _, f := range frags {
		if st.Kind == "" {
			st.Kind = f.Kind
		}
		sb = append(sb, f.Data...)
		var s string
		if allStrings && json.Unmarshal(f.Data, &s) == nil {
			text += s
		} else {
			allStrings = false
		}
	}
	if allStrings {
		st.Data, _ = json.Marshal(text)
	} else {
		st.Data = sb
	}
	return st
}
