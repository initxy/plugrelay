package openairesponses

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/sse"
)

// NewStreamEncoder renders IR events as a responses event stream. A normal
// end always emits response.completed (or response.incomplete) carrying the
// full response with usage.
func (Codec) NewStreamEncoder(w sse.Writer, req *ir.Request) protocol.StreamEncoder {
	return &streamEncoder{w: w, req: req, blocks: map[int]*encBlock{}}
}

type streamEncoder struct {
	w   sse.Writer
	req *ir.Request

	seq     int64
	started bool
	ended   bool
	shell   responseShell
	gen     func(prefix string, n int) string
	usage   ir.Usage

	blocks  map[int]*encBlock
	order   []*encBlock
	outputs []json.RawMessage // by output index; nil until done
	msg     *encMsg           // open message item
}

type encMsg struct {
	out   int
	id    string
	role  string
	rest  json.RawMessage
	parts []json.RawMessage
	live  int // parts not yet done
}

type encBlock struct {
	start   ir.Block
	out     int
	id      string
	content int
	ptype   string
	text    strings.Builder
	notes   []json.RawMessage
	opaque  *ir.OpaqueState
	summary bool // summary part opened
	deltas  bool
	skip    bool
	closed  bool
	msg     *encMsg
}

func (s *streamEncoder) Usage() ir.Usage { return s.usage }

// Keepalive writes an SSE comment.
func (s *streamEncoder) Keepalive() error { return s.w.Comment("keepalive") }

func (s *streamEncoder) emit(name string, fields func(o *object)) error {
	o := newObject()
	o.str("type", name)
	fields(o)
	o.num("sequence_number", s.seq)
	s.seq++
	return s.w.Event(name, o.bytes())
}

func (s *streamEncoder) begin(id, model string) error {
	if s.started {
		return nil
	}
	s.started = true
	if id == "" {
		id = newResponseID()
	}
	s.shell = responseShell{id: id, model: clientModel(s.req, model), created: time.Now().Unix(), status: "in_progress", req: s.req}
	s.gen = idGen(id)
	resp := s.shell.object(nil, nil).raw()
	if err := s.emit("response.created", func(o *object) { o.set("response", resp) }); err != nil {
		return err
	}
	return s.emit("response.in_progress", func(o *object) { o.set("response", resp) })
}

func (s *streamEncoder) newOutput() int {
	s.outputs = append(s.outputs, nil)
	return len(s.outputs) - 1
}

func (s *streamEncoder) itemID(b *ir.Block, prefix string, out int) string {
	if rest, ok := xget(&b.Extra, xItem); ok {
		if id := jsonField(rest, "id"); id != "" {
			return id
		}
	}
	return s.gen(prefix, out)
}

// Encode writes one event.
func (s *streamEncoder) Encode(ev *ir.Event) error {
	if s.ended || ev == nil {
		return nil
	}
	switch ev.Kind {
	case ir.EvStart:
		return s.begin(ev.ID, ev.Model)
	case ir.EvPing:
		if !s.started {
			return nil
		}
		return s.Keepalive()
	case ir.EvStatus:
		return nil
	case ir.EvUsage:
		if ev.Usage != nil {
			s.usage = *ev.Usage
		}
		return nil
	case ir.EvError:
		return s.fail(ev.Err)
	}
	if err := s.begin("", ""); err != nil {
		return err
	}
	switch ev.Kind {
	case ir.EvBlockStart:
		if ev.Block == nil {
			return nil
		}
		return s.blockStart(ev.Index, *ev.Block)
	case ir.EvDelta:
		b := s.blocks[ev.Index]
		if b == nil || b.skip || b.closed || ev.Delta == nil {
			return nil
		}
		return s.delta(b, ev.Delta)
	case ir.EvBlockStop:
		b := s.blocks[ev.Index]
		if b == nil || b.closed {
			return nil
		}
		return s.blockStop(b, ev.Block)
	case ir.EvDone:
		if ev.Usage != nil {
			s.usage = *ev.Usage
		}
		return s.done(ev.StopReason)
	}
	return nil
}

func (s *streamEncoder) blockStart(index int, b ir.Block) error {
	eb := &encBlock{start: b, out: -1, opaque: b.Opaque}
	s.blocks[index] = eb
	s.order = append(s.order, eb)
	_, hasItem := xget(&b.Extra, xItem)
	if b.Type != ir.BlockText || hasItem || s.msg == nil || s.msg.role != roleOf(b) {
		if err := s.closeMsg(); err != nil {
			return err
		}
	}
	switch b.Type {
	case ir.BlockText:
		return s.startText(eb)
	case ir.BlockThinking:
		eb.out = s.newOutput()
		eb.id = s.itemID(&b, "rs_", eb.out)
		item := s.reasoningItem(eb, "", nil)
		if err := s.added(eb.out, item); err != nil {
			return err
		}
		if b.Text != "" {
			return s.delta(eb, &ir.Delta{Kind: ir.DeltaThinking, Text: b.Text})
		}
	case ir.BlockToolUse:
		if b.Tool == nil {
			eb.skip = true
			return nil
		}
		eb.out = s.newOutput()
		prefix := "fc_"
		if b.Tool.Kind == ir.ToolFreeform {
			prefix = "ctc_"
		}
		eb.id = s.itemID(&b, prefix, eb.out)
		if err := s.added(eb.out, s.toolItem(eb, "in_progress", "")); err != nil {
			return err
		}
		initial := b.Tool.Text
		if b.Tool.Kind != ir.ToolFreeform {
			initial = string(b.Tool.Input)
		}
		if initial != "" {
			return s.delta(eb, &ir.Delta{Kind: ir.DeltaToolInput, Text: initial})
		}
	default:
		// Opaque and raw items are emitted whole at block stop, when the
		// final state is known.
	}
	return nil
}

func roleOf(b ir.Block) string {
	if r := xstring(&b.Extra, xRole); r != "" {
		return r
	}
	return "assistant"
}

func (s *streamEncoder) added(out int, item json.RawMessage) error {
	return s.emit("response.output_item.added", func(o *object) {
		o.num("output_index", int64(out))
		o.set("item", item)
	})
}

func (s *streamEncoder) itemDone(out int, item json.RawMessage) error {
	s.outputs[out] = item
	return s.emit("response.output_item.done", func(o *object) {
		o.num("output_index", int64(out))
		o.set("item", item)
	})
}

func (s *streamEncoder) messageItem(m *encMsg, status string, parts []json.RawMessage) json.RawMessage {
	o := newObject()
	o.str("id", m.id)
	o.str("type", itemMessage)
	o.str("status", status)
	if parts == nil {
		parts = []json.RawMessage{}
	}
	o.set("content", jarray(parts))
	o.str("role", m.role)
	o.merge(m.rest)
	return o.raw()
}

func (s *streamEncoder) startText(eb *encBlock) error {
	b := eb.start
	if s.msg == nil {
		m := &encMsg{out: s.newOutput(), role: roleOf(b)}
		rest, _ := xget(&b.Extra, xItem)
		m.rest = restObject(string(rest), "id", "status")
		m.id = s.itemID(&b, "msg_", m.out)
		s.msg = m
		if err := s.added(m.out, s.messageItem(m, "in_progress", nil)); err != nil {
			return err
		}
	}
	m := s.msg
	eb.msg, eb.out, eb.id = m, m.out, m.id
	eb.content = len(m.parts)
	m.parts = append(m.parts, nil)
	m.live++
	eb.ptype = xstring(&b.Extra, xPartType)
	if eb.ptype == "" {
		eb.ptype = "output_text"
	}
	if err := s.emit("response.content_part.added", func(o *object) {
		o.str("item_id", eb.id)
		o.num("output_index", int64(eb.out))
		o.num("content_index", int64(eb.content))
		o.set("part", s.partJSON(eb, nil))
	}); err != nil {
		return err
	}
	if b.Text != "" {
		return s.delta(eb, &ir.Delta{Kind: ir.DeltaText, Text: b.Text})
	}
	return nil
}

func (s *streamEncoder) partJSON(eb *encBlock, stop *ir.Block) json.RawMessage {
	o := newObject()
	o.str("type", eb.ptype)
	if eb.ptype == "refusal" {
		o.str("refusal", eb.text.String())
		return o.raw()
	}
	var rest json.RawMessage
	if stop != nil {
		rest, _ = xget(&stop.Extra, xPart)
	}
	if rest == nil {
		rest, _ = xget(&eb.start.Extra, xPart)
	}
	switch {
	case len(eb.notes) > 0:
		o.set("annotations", jarray(eb.notes))
	case jsonField(rest, "annotations") == "" && !hasField(rest, "annotations"):
		o.set("annotations", json.RawMessage("[]"))
	}
	o.str("text", eb.text.String())
	o.merge(rest)
	return o.raw()
}

func (s *streamEncoder) delta(eb *encBlock, d *ir.Delta) error {
	t := eb.start.Type
	switch {
	case t == ir.BlockText && d.Kind == ir.DeltaText:
		eb.text.WriteString(d.Text)
		name := "response.output_text.delta"
		if eb.ptype == "refusal" {
			name = "response.refusal.delta"
		}
		return s.emit(name, func(o *object) {
			o.str("item_id", eb.id)
			o.num("output_index", int64(eb.out))
			o.num("content_index", int64(eb.content))
			o.str("delta", d.Text)
			if eb.ptype != "refusal" {
				o.set("logprobs", json.RawMessage("[]"))
			}
		})
	case t == ir.BlockText && d.Kind == ir.DeltaCitation:
		note, ok := xget(&d.Extra, xAnnotation)
		if !ok {
			return nil
		}
		eb.notes = append(eb.notes, note)
		return s.emit("response.output_text.annotation.added", func(o *object) {
			o.str("item_id", eb.id)
			o.num("output_index", int64(eb.out))
			o.num("content_index", int64(eb.content))
			o.num("annotation_index", int64(len(eb.notes)-1))
			o.set("annotation", note)
		})
	case t == ir.BlockThinking && d.Kind == ir.DeltaThinking:
		if !eb.summary {
			eb.summary = true
			if err := s.emit("response.reasoning_summary_part.added", func(o *object) {
				o.str("item_id", eb.id)
				o.num("output_index", int64(eb.out))
				o.num("summary_index", 0)
				o.set("part", summaryPart(""))
			}); err != nil {
				return err
			}
		}
		eb.text.WriteString(d.Text)
		return s.emit("response.reasoning_summary_text.delta", func(o *object) {
			o.str("item_id", eb.id)
			o.num("output_index", int64(eb.out))
			o.num("summary_index", 0)
			o.str("delta", d.Text)
		})
	case t == ir.BlockToolUse && d.Kind == ir.DeltaToolInput:
		eb.text.WriteString(d.Text)
		eb.deltas = true
		name := "response.function_call_arguments.delta"
		if eb.start.Tool.Kind == ir.ToolFreeform {
			name = "response.custom_tool_call_input.delta"
		}
		return s.emit(name, func(o *object) {
			o.str("item_id", eb.id)
			o.num("output_index", int64(eb.out))
			o.str("delta", d.Text)
		})
	case d.Kind == ir.DeltaOpaque && d.Opaque != nil:
		eb.opaque = d.Opaque
	}
	return nil
}

func summaryPart(text string) json.RawMessage {
	o := newObject()
	o.str("type", partSummaryText)
	o.str("text", text)
	return o.raw()
}

func (s *streamEncoder) reasoningItem(eb *encBlock, text string, stop *ir.Block) json.RawMessage {
	o := newObject()
	o.str("id", eb.id)
	o.str("type", itemReasoning)
	if text != "" {
		o.set("summary", jarray([]json.RawMessage{summaryPart(text)}))
	} else {
		o.set("summary", json.RawMessage("[]"))
	}
	if op := eb.opaque; op != nil && (op.Sealed || op.Kind == kindEncrypted) && len(op.Data) > 0 {
		o.set("encrypted_content", op.Data)
	}
	rest, _ := xget(&eb.start.Extra, xItem)
	if stop != nil {
		if r, ok := xget(&stop.Extra, xItem); ok {
			rest = r
		}
	}
	o.merge(restObject(string(rest), "status"))
	return o.raw()
}

func (s *streamEncoder) toolItem(eb *encBlock, status, input string) json.RawMessage {
	tc := eb.start.Tool
	o := newObject()
	o.str("id", eb.id)
	if tc.Kind == ir.ToolFreeform {
		o.str("type", itemCustomCall)
		o.str("status", status)
		o.str("call_id", tc.ID)
		o.str("input", input)
	} else {
		o.str("type", itemFunctionCall)
		o.str("status", status)
		o.str("arguments", input)
		o.str("call_id", tc.ID)
	}
	o.str("name", tc.Name)
	if tc.Namespace != "" {
		o.str("namespace", tc.Namespace)
	}
	rest, _ := xget(&eb.start.Extra, xItem)
	o.merge(restObject(string(rest), "status"))
	return o.raw()
}

func (s *streamEncoder) blockStop(eb *encBlock, stop *ir.Block) error {
	eb.closed = true
	if stop != nil && stop.Opaque != nil {
		eb.opaque = stop.Opaque
	}
	if eb.skip {
		return nil
	}
	b := eb.start
	switch b.Type {
	case ir.BlockText:
		return s.stopText(eb, stop)
	case ir.BlockThinking:
		text := eb.text.String()
		if eb.summary {
			if err := s.emit("response.reasoning_summary_text.done", func(o *object) {
				o.str("item_id", eb.id)
				o.num("output_index", int64(eb.out))
				o.num("summary_index", 0)
				o.str("text", text)
			}); err != nil {
				return err
			}
			if err := s.emit("response.reasoning_summary_part.done", func(o *object) {
				o.str("item_id", eb.id)
				o.num("output_index", int64(eb.out))
				o.num("summary_index", 0)
				o.set("part", summaryPart(text))
			}); err != nil {
				return err
			}
		}
		return s.itemDone(eb.out, s.reasoningItem(eb, text, stop))
	case ir.BlockToolUse:
		input := eb.text.String()
		if !eb.deltas && b.Tool.Kind != ir.ToolFreeform && input == "" {
			input = "{}"
		}
		name, field := "response.function_call_arguments.done", "arguments"
		if b.Tool.Kind == ir.ToolFreeform {
			name, field = "response.custom_tool_call_input.done", "input"
		}
		if err := s.emit(name, func(o *object) {
			o.str("item_id", eb.id)
			o.num("output_index", int64(eb.out))
			o.str(field, input)
		}); err != nil {
			return err
		}
		return s.itemDone(eb.out, s.toolItem(eb, "completed", input))
	case ir.BlockOpaque, ir.BlockRaw:
		final := b
		if eb.opaque != nil {
			final.Opaque = eb.opaque
		}
		base := len(s.outputs)
		w := &itemWriter{output: true}
		w.newID = func(prefix string) string { return s.gen(prefix, base+len(w.items)) }
		w.add(final, "assistant")
		w.flush()
		for _, item := range w.items {
			out := s.newOutput()
			if err := s.added(out, item); err != nil {
				return err
			}
			if err := s.itemDone(out, item); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *streamEncoder) stopText(eb *encBlock, stop *ir.Block) error {
	text := eb.text.String()
	name := "response.output_text.done"
	if eb.ptype == "refusal" {
		name = "response.refusal.done"
	}
	if err := s.emit(name, func(o *object) {
		o.str("item_id", eb.id)
		o.num("output_index", int64(eb.out))
		o.num("content_index", int64(eb.content))
		if eb.ptype == "refusal" {
			o.str("refusal", text)
		} else {
			o.str("text", text)
			o.set("logprobs", json.RawMessage("[]"))
		}
	}); err != nil {
		return err
	}
	part := s.partJSON(eb, stop)
	eb.msg.parts[eb.content] = part
	eb.msg.live--
	if err := s.emit("response.content_part.done", func(o *object) {
		o.str("item_id", eb.id)
		o.num("output_index", int64(eb.out))
		o.num("content_index", int64(eb.content))
		o.set("part", part)
	}); err != nil {
		return err
	}
	if s.msg != eb.msg && eb.msg.live == 0 {
		return s.finishMsg(eb.msg)
	}
	return nil
}

// closeMsg ends the open message item once its parts are done.
func (s *streamEncoder) closeMsg() error {
	m := s.msg
	if m == nil {
		return nil
	}
	s.msg = nil
	if m.live > 0 {
		return nil
	}
	return s.finishMsg(m)
}

func (s *streamEncoder) finishMsg(m *encMsg) error {
	parts := make([]json.RawMessage, 0, len(m.parts))
	for _, p := range m.parts {
		if p != nil {
			parts = append(parts, p)
		}
	}
	return s.itemDone(m.out, s.messageItem(m, "completed", parts))
}

// closeAll stops every open block, in start order.
func (s *streamEncoder) closeAll() error {
	for _, eb := range s.order {
		if !eb.closed {
			if err := s.blockStop(eb, nil); err != nil {
				return err
			}
		}
	}
	return s.closeMsg()
}

func (s *streamEncoder) finalOutput() []json.RawMessage {
	out := make([]json.RawMessage, 0, len(s.outputs))
	for _, o := range s.outputs {
		if o != nil {
			out = append(out, o)
		}
	}
	return out
}

func (s *streamEncoder) done(reason ir.StopReason) error {
	if err := s.closeAll(); err != nil {
		return err
	}
	s.ended = true
	status, incomplete := statusOf(reason)
	sh := s.shell
	sh.status, sh.incomplete = status, incomplete
	usage := s.usage
	resp := sh.object(s.finalOutput(), &usage).raw()
	name := "response.completed"
	if status == "incomplete" {
		name = "response.incomplete"
	}
	return s.emit(name, func(o *object) { o.set("response", resp) })
}

func (s *streamEncoder) fail(e *ir.GwError) error {
	if e == nil {
		e = ir.Errorf(ir.ErrInternal, "stream failed")
	}
	if err := s.begin("", ""); err != nil {
		return err
	}
	s.ended = true
	sh := s.shell
	sh.status = "failed"
	sh.err = streamError(e)
	usage := s.usage
	resp := sh.object(s.finalOutput(), &usage).raw()
	return s.emit("response.failed", func(o *object) { o.set("response", resp) })
}
