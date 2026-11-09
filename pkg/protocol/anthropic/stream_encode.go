package anthropic

import (
	"encoding/json"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/sse"
)

// NewStreamEncoder returns an encoder writing the messages SSE event
// sequence: message_start, content_block_start/delta/stop per block,
// message_delta, message_stop.
func (Codec) NewStreamEncoder(w sse.Writer, req *ir.Request) protocol.StreamEncoder {
	return &streamEncoder{w: w, req: req, index: map[int]*outBlock{}}
}

// outBlock is a block as the client sees it. IR indexes map to dense client
// indexes because blocks this protocol cannot express are skipped.
type outBlock struct {
	index     int
	signature string // signature known before the block's end
	open      bool
}

type streamEncoder struct {
	w       sse.Writer
	req     *ir.Request
	started bool
	ended   bool
	next    int
	index   map[int]*outBlock // IR index → client block; nil entry = skipped
	order   []int             // IR indexes in start order
	usage   ir.Usage
}

func (s *streamEncoder) Usage() ir.Usage { return s.usage }

func (s *streamEncoder) Keepalive() error {
	if s.ended {
		return nil
	}
	return s.w.Event("ping", json.RawMessage(`{"type":"ping"}`))
}

func (s *streamEncoder) emit(event string, o *object) error {
	return s.w.Event(event, o.bytes())
}

func (s *streamEncoder) typed(t string) *object {
	o := newObject()
	o.setv("type", t)
	return o
}

func (s *streamEncoder) Encode(ev *ir.Event) error {
	if s.ended {
		return nil
	}
	if ev.Usage != nil {
		s.usage = *ev.Usage
	}
	switch ev.Kind {
	case ir.EvStart:
		return s.start(ev.ID, ev.Model)
	case ir.EvBlockStart:
		return s.blockStart(ev)
	case ir.EvDelta:
		return s.delta(ev)
	case ir.EvBlockStop:
		return s.blockStop(ev.Index, ev.Block)
	case ir.EvPing:
		return s.Keepalive()
	case ir.EvDone:
		return s.done(ev)
	case ir.EvError:
		s.ended = true
		e := ev.Err
		if e == nil {
			e = &ir.GwError{Kind: ir.ErrInternal, Message: "stream failed"}
		}
		_, typ := errorClass(e.Kind)
		return s.w.Event("error", errorBody(e, typ))
	}
	return nil
}

func (s *streamEncoder) start(id, model string) error {
	if s.started {
		return nil
	}
	s.started = true
	msg := newObject()
	msg.setv("id", messageID(id))
	msg.setv("type", "message")
	msg.setv("role", string(ir.RoleAssistant))
	msg.setv("model", modelOf(s.req, model))
	msg.set("content", json.RawMessage("[]"))
	msg.set("stop_reason", json.RawMessage(nullJSON))
	msg.set("stop_sequence", json.RawMessage(nullJSON))
	msg.set("usage", encodeUsage(s.usage, false))
	o := s.typed("message_start")
	o.set("message", msg.bytes())
	return s.emit("message_start", o)
}

// startForm renders a block as content_block_start carries it: the visible
// content empty, to be filled by deltas.
func startForm(b *ir.Block) (json.RawMessage, bool) {
	o := newObject()
	switch b.Type {
	case ir.BlockText:
		o.setv("type", wireText)
		o.setv("text", "")
		o.extra(b.Extra)
	case ir.BlockThinking:
		o.setv("type", wireThinking)
		o.setv("thinking", "")
		o.setv("signature", "")
	case ir.BlockToolUse:
		if b.Tool == nil {
			return nil, false
		}
		o.setv("type", wireToolUse)
		o.setv("id", b.Tool.ID)
		o.setv("name", b.Tool.Name)
		o.set("input", json.RawMessage(emptyObject))
	default:
		return encodeBlock(b)
	}
	return o.bytes(), true
}

func (s *streamEncoder) blockStart(ev *ir.Event) error {
	if err := s.start("", ""); err != nil {
		return err
	}
	if _, dup := s.index[ev.Index]; dup || ev.Block == nil {
		return nil
	}
	cb, ok := startForm(ev.Block)
	if !ok {
		s.index[ev.Index] = nil
		return nil
	}
	ob := &outBlock{index: s.next, open: true}
	s.next++
	s.index[ev.Index] = ob
	s.order = append(s.order, ev.Index)
	o := s.typed("content_block_start")
	o.setv("index", ob.index)
	o.set("content_block", cb)
	if err := s.emit("content_block_start", o); err != nil {
		return err
	}
	b := ev.Block
	switch b.Type {
	case ir.BlockText:
		if b.Text != "" {
			return s.writeDelta(ob, &ir.Delta{Kind: ir.DeltaText, Text: b.Text})
		}
	case ir.BlockThinking:
		if sig, ok := echoString(b.Opaque, kindSignature); ok {
			ob.signature = sig
		}
		if b.Text != "" {
			return s.writeDelta(ob, &ir.Delta{Kind: ir.DeltaThinking, Text: b.Text})
		}
	case ir.BlockToolUse:
		if len(b.Tool.Input) > 0 && string(b.Tool.Input) != emptyObject {
			return s.writeDelta(ob, &ir.Delta{Kind: ir.DeltaToolInput, Text: string(b.Tool.Input)})
		}
	}
	return nil
}

func (s *streamEncoder) delta(ev *ir.Event) error {
	ob := s.index[ev.Index]
	if ob == nil || !ob.open || ev.Delta == nil {
		return nil
	}
	return s.writeDelta(ob, ev.Delta)
}

func (s *streamEncoder) writeDelta(ob *outBlock, d *ir.Delta) error {
	var o *object
	switch d.Kind {
	case ir.DeltaText:
		o = s.typed("text_delta")
		o.setv("text", d.Text)
	case ir.DeltaThinking:
		o = s.typed("thinking_delta")
		o.setv("thinking", d.Text)
	case ir.DeltaToolInput:
		o = s.typed("input_json_delta")
		o.setv("partial_json", d.Text)
	case ir.DeltaOpaque:
		sig, ok := echoString(d.Opaque, kindSignature)
		if !ok {
			return nil
		}
		ob.signature = ""
		o = s.typed("signature_delta")
		o.setv("signature", sig)
	case ir.DeltaCitation:
		c, ok := d.Extra.Get(name, "citation")
		if !ok {
			return nil
		}
		o = s.typed("citations_delta")
		o.set("citation", c)
	default:
		return nil
	}
	ev := s.typed("content_block_delta")
	ev.setv("index", ob.index)
	ev.set("delta", o.bytes())
	return s.emit("content_block_delta", ev)
}

func (s *streamEncoder) blockStop(idx int, b *ir.Block) error {
	ob := s.index[idx]
	if ob == nil || !ob.open {
		return nil
	}
	if b != nil && b.Type == ir.BlockThinking {
		if sig, ok := echoString(b.Opaque, kindSignature); ok {
			ob.signature = sig
		}
	}
	if ob.signature != "" {
		if err := s.writeDelta(ob, &ir.Delta{Kind: ir.DeltaOpaque, Opaque: &ir.OpaqueState{
			Kind: kindSignature, Data: marshal(ob.signature),
		}}); err != nil {
			return err
		}
	}
	ob.open = false
	o := s.typed("content_block_stop")
	o.setv("index", ob.index)
	return s.emit("content_block_stop", o)
}

func (s *streamEncoder) done(ev *ir.Event) error {
	if err := s.start("", ""); err != nil {
		return err
	}
	for _, idx := range s.order {
		if err := s.blockStop(idx, nil); err != nil {
			return err
		}
	}
	s.ended = true
	d := newObject()
	d.setv("stop_reason", stopReason(ev.StopReason))
	if ev.StopSeq != "" {
		d.setv("stop_sequence", ev.StopSeq)
	} else {
		d.set("stop_sequence", json.RawMessage(nullJSON))
	}
	o := s.typed("message_delta")
	o.set("delta", d.bytes())
	o.set("usage", encodeUsage(s.usage, false))
	if err := s.emit("message_delta", o); err != nil {
		return err
	}
	return s.emit("message_stop", s.typed("message_stop"))
}
