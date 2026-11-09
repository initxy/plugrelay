package gemini

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/sse"
)

// streamDecoder turns alt=sse chunks (each a GenerateContentResponse) into
// IR events. Consecutive text parts merge into one text block and thought
// parts into one thinking block; every functionCall is a complete tool_use
// block. A thoughtSignature rides on EvBlockStop.Block.Opaque of the block it
// arrived on; a signature-only part attaches to the open text or thinking
// block, or becomes its own opaque block.
type streamDecoder struct {
	ids     *idState
	started bool
	next    int
	open    *openBlock
	finish  string
	sawCall bool
	errored bool
	usage   ir.Usage
}

type openBlock struct {
	index int
	typ   ir.BlockType
	sig   *ir.OpaqueState
}

func newStreamDecoder() *streamDecoder { return &streamDecoder{} }

// Feed implements protocol.StreamDecoder.
func (d *streamDecoder) Feed(ev sse.Event) ([]ir.Event, error) {
	data := bytes.TrimSpace(ev.Data)
	if ev.Comment || len(data) == 0 || d.errored {
		return nil, nil
	}
	g := gjson.ParseBytes(data)
	if !g.IsObject() && !g.IsArray() {
		return nil, ir.Errorf(ir.ErrUpstream, "invalid stream chunk: %.200s", data)
	}
	var out []ir.Event
	chunks := []gjson.Result{g}
	if g.IsArray() {
		chunks = g.Array()
	}
	for _, c := range chunks {
		evs, err := d.chunk(c)
		if err != nil {
			return out, err
		}
		out = append(out, evs...)
		if d.errored {
			break
		}
	}
	return out, nil
}

func (d *streamDecoder) chunk(c gjson.Result) ([]ir.Event, error) {
	var out []ir.Event
	if c.Get("error").Exists() {
		out = append(out, d.closeOpen()...)
		d.errored = true
		e := Codec{}.DecodeError(0, nil, []byte(c.Raw))
		return append(out, ir.Event{Kind: ir.EvError, Err: e}), nil
	}
	if !d.started {
		d.started = true
		id := c.Get("responseId").String()
		d.ids = newIDState(responseSalt(id))
		out = append(out, ir.Event{Kind: ir.EvStart, ID: id, Model: c.Get("modelVersion").String()})
	}
	cand := c.Get("candidates.0")
	for _, p := range cand.Get("content.parts").Array() {
		evs, err := d.part(json.RawMessage(p.Raw))
		if err != nil {
			return out, err
		}
		out = append(out, evs...)
	}
	if f := cand.Get("finishReason").String(); f != "" {
		d.finish = f
	} else if !cand.Exists() && c.Get("promptFeedback.blockReason").Exists() {
		d.finish = "SAFETY"
	}
	if u, ok := decodeUsage(json.RawMessage(c.Get("usageMetadata").Raw)); ok {
		d.usage = u
		uc := u
		out = append(out, ir.Event{Kind: ir.EvUsage, Usage: &uc})
	}
	return out, nil
}

func (d *streamDecoder) part(raw json.RawMessage) ([]ir.Event, error) {
	b, err := decodePart(raw, d.ids, false, nil)
	if err != nil || b == nil {
		return nil, err
	}
	var out []ir.Event
	switch b.Type {
	case ir.BlockText, ir.BlockThinking:
		if d.open == nil || d.open.typ != b.Type || (b.Opaque != nil && d.open.sig != nil) {
			out = append(out, d.closeOpen()...)
			d.open = &openBlock{index: d.next, typ: b.Type}
			d.next++
			out = append(out, ir.Event{Kind: ir.EvBlockStart, Index: d.open.index, Block: &ir.Block{Type: b.Type}})
		}
		kind := ir.DeltaText
		if b.Type == ir.BlockThinking {
			kind = ir.DeltaThinking
		}
		out = append(out, ir.Event{Kind: ir.EvDelta, Index: d.open.index, Delta: &ir.Delta{Kind: kind, Text: b.Text}})
		if b.Opaque != nil {
			d.open.sig = b.Opaque
		}
		return out, nil
	case ir.BlockOpaque:
		if d.open != nil && d.open.sig == nil {
			d.open.sig = b.Opaque
			return nil, nil
		}
	case ir.BlockToolUse:
		d.sawCall = true
	}
	out = append(out, d.closeOpen()...)
	idx := d.next
	d.next++
	start := &ir.Block{Type: b.Type, Media: b.Media, Extra: b.Extra}
	if b.Type == ir.BlockOpaque {
		start.Opaque = b.Opaque
	}
	if b.Tool != nil {
		start.Tool = &ir.ToolCall{ID: b.Tool.ID, Name: b.Tool.Name, Kind: b.Tool.Kind}
	}
	out = append(out, ir.Event{Kind: ir.EvBlockStart, Index: idx, Block: start})
	if b.Tool != nil {
		out = append(out, ir.Event{Kind: ir.EvDelta, Index: idx, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: string(b.Tool.Input)}})
	}
	stop := ir.Event{Kind: ir.EvBlockStop, Index: idx}
	if b.Opaque != nil {
		stop.Block = &ir.Block{Type: b.Type, Opaque: b.Opaque}
	}
	return append(out, stop), nil
}

func (d *streamDecoder) closeOpen() []ir.Event {
	if d.open == nil {
		return nil
	}
	ev := ir.Event{Kind: ir.EvBlockStop, Index: d.open.index}
	if d.open.sig != nil {
		ev.Block = &ir.Block{Type: d.open.typ, Opaque: d.open.sig}
	}
	d.open = nil
	return []ir.Event{ev}
}

// Finish implements protocol.StreamDecoder. The stream is complete when a
// finishReason was seen or it ended with an error chunk.
func (d *streamDecoder) Finish() ([]ir.Event, bool) {
	if d.errored {
		return nil, true
	}
	out := d.closeOpen()
	if d.finish == "" {
		return out, false
	}
	return append(out, ir.Event{Kind: ir.EvDone, StopReason: stopReason(d.finish, d.sawCall)}), true
}

// Usage implements protocol.StreamDecoder.
func (d *streamDecoder) Usage() ir.Usage { return d.usage }

// streamEncoder writes IR events as alt=sse chunks. Text and thinking deltas
// go out as they arrive; a tool_use is sent as one functionCall part when its
// block stops; the final chunk carries finishReason and usageMetadata.
type streamEncoder struct {
	w      sse.Writer
	req    *ir.Request
	names  map[string]string
	id     string
	model  string
	blocks map[int]*encBlock
	usage  ir.Usage
	done   bool
}

type encBlock struct {
	block ir.Block
	args  strings.Builder
}

func newStreamEncoder(w sse.Writer, req *ir.Request) *streamEncoder {
	e := &streamEncoder{w: w, req: req, blocks: map[int]*encBlock{}}
	if req != nil {
		e.model = req.Model
		e.names = toolNames(req.Messages)
	}
	return e
}

// Encode implements protocol.StreamEncoder.
func (e *streamEncoder) Encode(ev *ir.Event) error {
	if e.done {
		return nil
	}
	switch ev.Kind {
	case ir.EvStart:
		e.id = ev.ID
		if ev.Model != "" {
			e.model = ev.Model
		}
	case ir.EvBlockStart:
		if ev.Block == nil {
			return nil
		}
		eb := &encBlock{block: *ev.Block}
		if ev.Block.Tool != nil {
			t := *ev.Block.Tool
			eb.block.Tool = &t
		}
		e.blocks[ev.Index] = eb
		switch eb.block.Type {
		case ir.BlockImage, ir.BlockDocument, ir.BlockRaw:
			return e.emitBlock(&eb.block)
		}
	case ir.EvDelta:
		eb := e.blocks[ev.Index]
		if eb == nil || ev.Delta == nil {
			return nil
		}
		switch ev.Delta.Kind {
		case ir.DeltaText, ir.DeltaThinking:
			if ev.Delta.Text == "" {
				return nil
			}
			b := ir.Block{Type: ir.BlockText, Text: ev.Delta.Text}
			if eb.block.Type == ir.BlockThinking || ev.Delta.Kind == ir.DeltaThinking {
				b.Type = ir.BlockThinking
			}
			return e.emitBlock(&b)
		case ir.DeltaToolInput:
			eb.args.WriteString(ev.Delta.Text)
		case ir.DeltaOpaque:
			if ev.Delta.Opaque != nil {
				eb.block.Opaque = ev.Delta.Opaque
			}
		}
	case ir.EvBlockStop:
		eb := e.blocks[ev.Index]
		if eb == nil {
			return nil
		}
		delete(e.blocks, ev.Index)
		if ev.Block != nil && ev.Block.Opaque != nil {
			eb.block.Opaque = ev.Block.Opaque
		}
		return e.stopBlock(eb)
	case ir.EvUsage:
		if ev.Usage != nil {
			e.usage = *ev.Usage
		}
	case ir.EvPing:
		return e.Keepalive()
	case ir.EvDone:
		if ev.Usage != nil {
			e.usage = *ev.Usage
		}
		if err := e.flushOpen(); err != nil {
			return err
		}
		e.done = true
		return e.writeChunk([]json.RawMessage{json.RawMessage(`{"text":""}`)}, finishReason(ev.StopReason), true)
	case ir.EvError:
		e.done = true
		ge := ev.Err
		if ge == nil {
			ge = ir.Errorf(ir.ErrInternal, "stream failed")
		}
		code := kindStatus(ge.Kind)
		if ge.Status != 0 {
			code = ge.Status
		}
		return e.w.Event("", errorBody(ge, code))
	}
	return nil
}

// flushOpen stops blocks the producer left open, in index order.
func (e *streamEncoder) flushOpen() error {
	idx := make([]int, 0, len(e.blocks))
	for i := range e.blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		eb := e.blocks[i]
		delete(e.blocks, i)
		if err := e.stopBlock(eb); err != nil {
			return err
		}
	}
	return nil
}

func (e *streamEncoder) stopBlock(eb *encBlock) error {
	b := &eb.block
	switch b.Type {
	case ir.BlockToolUse:
		if b.Tool == nil {
			return nil
		}
		if eb.args.Len() > 0 {
			b.Tool.Input = json.RawMessage(eb.args.String())
		}
		return e.emitBlock(b)
	case ir.BlockText, ir.BlockThinking:
		if signatureFor(b.Opaque, toClient) == "" {
			return nil
		}
		p := encodeBlock(&ir.Block{Type: ir.BlockOpaque, Opaque: b.Opaque}, e.names, toClient)
		if b.Type == ir.BlockThinking {
			p = setJSON(p, "thought", true)
		}
		return e.writeChunk([]json.RawMessage{p}, "", false)
	case ir.BlockOpaque:
		return e.emitBlock(b)
	}
	return nil
}

func (e *streamEncoder) emitBlock(b *ir.Block) error {
	p := encodeBlock(b, e.names, toClient)
	if p == nil {
		return nil
	}
	return e.writeChunk([]json.RawMessage{p}, "", false)
}

func (e *streamEncoder) writeChunk(parts []json.RawMessage, finish string, final bool) error {
	content := setJSON([]byte("{}"), "role", "model")
	content = setRaw(content, "parts", rawArray(parts))
	cand := setRaw([]byte("{}"), "content", content)
	if finish != "" {
		cand = setJSON(cand, "finishReason", finish)
	}
	cand = setJSON(cand, "index", 0)
	out := setRaw([]byte("{}"), "candidates", rawArray([]json.RawMessage{cand}))
	if final {
		out = setRaw(out, "usageMetadata", encodeUsage(e.usage))
	}
	if e.model != "" {
		out = setJSON(out, "modelVersion", e.model)
	}
	if e.id != "" {
		out = setJSON(out, "responseId", e.id)
	}
	return e.w.Event("", out)
}

// Keepalive implements protocol.StreamEncoder with an SSE comment.
func (e *streamEncoder) Keepalive() error { return e.w.Comment("keepalive") }

// Usage implements protocol.StreamEncoder.
func (e *streamEncoder) Usage() ir.Usage { return e.usage }
