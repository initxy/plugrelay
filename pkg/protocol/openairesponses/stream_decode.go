package openairesponses

import (
	"encoding/json"
	"slices"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/sse"
	"github.com/tidwall/gjson"
)

// NewStreamDecoder parses an upstream's responses event stream into IR events.
func (Codec) NewStreamDecoder() protocol.StreamDecoder {
	return &streamDecoder{items: map[int64]*decItem{}}
}

type streamDecoder struct {
	next     int // next IR block index
	started  bool
	terminal bool
	sawTool  bool
	usage    ir.Usage
	items    map[int64]*decItem // by output_index
	open     []*decBlock        // in start order
}

type decItem struct {
	typ    string
	added  gjson.Result
	rest   json.RawMessage
	blk    *decBlock           // single-block items
	parts  map[int64]*decBlock // message content parts
	marked bool                // first message part carried the item marker
	// reasoning
	summaries int
	text      bool // thinking block has received text
	args      int  // tool input bytes forwarded
}

type decBlock struct {
	index  int
	closed bool
}

func (d *streamDecoder) Usage() ir.Usage { return d.usage }

func (d *streamDecoder) Finish() ([]ir.Event, bool) { return nil, d.terminal }

func (d *streamDecoder) startBlock(out []ir.Event, b ir.Block) ([]ir.Event, *decBlock) {
	out = d.ensureStart(out, gjson.Result{})
	db := &decBlock{index: d.next}
	d.next++
	d.open = append(d.open, db)
	if b.Type == ir.BlockToolUse {
		d.sawTool = true
	}
	return append(out, ir.Event{Kind: ir.EvBlockStart, Index: db.index, Block: &b}), db
}

func (d *streamDecoder) stopBlock(out []ir.Event, db *decBlock, b *ir.Block) []ir.Event {
	if db == nil || db.closed {
		return out
	}
	db.closed = true
	return append(out, ir.Event{Kind: ir.EvBlockStop, Index: db.index, Block: b})
}

func (d *streamDecoder) ensureStart(out []ir.Event, resp gjson.Result) []ir.Event {
	if d.started {
		return out
	}
	d.started = true
	return append(out, ir.Event{Kind: ir.EvStart, ID: resp.Get("id").Str, Model: resp.Get("model").Str})
}

func (d *streamDecoder) item(ev gjson.Result) *decItem {
	oi := ev.Get("output_index").Int()
	it := d.items[oi]
	if it == nil {
		it = &decItem{parts: map[int64]*decBlock{}}
		d.items[oi] = it
	}
	return it
}

// Feed parses one SSE event.
func (d *streamDecoder) Feed(sev sse.Event) ([]ir.Event, error) {
	if sev.Comment || len(sev.Data) == 0 || string(sev.Data) == "[DONE]" {
		return nil, nil
	}
	if !gjson.ValidBytes(sev.Data) {
		return nil, ir.Errorf(ir.ErrUpstream, "invalid stream event: %.200s", sev.Data)
	}
	ev := gjson.ParseBytes(sev.Data)
	typ := ev.Get("type").Str
	if typ == "" {
		typ = sev.Name
	}
	var out []ir.Event
	switch typ {
	case "response.created", "response.in_progress", "response.queued":
		out = d.ensureStart(out, ev.Get("response"))

	case "response.output_item.added":
		it := d.item(ev)
		item := ev.Get("item")
		it.typ, it.added = item.Get("type").Str, item
		switch it.typ {
		case itemFunctionCall, itemCustomCall:
			out = d.startTool(out, it, item)
		case itemMessage:
			it.rest = itemRest(item, "role", "content", "status")
		}

	case "response.content_part.added":
		out = d.ensurePart(out, ev, ev.Get("part").Get("type").Str)

	case "response.output_text.delta", "response.refusal.delta":
		ptype := "output_text"
		if typ == "response.refusal.delta" {
			ptype = "refusal"
		}
		var db *decBlock
		out, db = d.part(out, ev, ptype)
		out = append(out, ir.Event{Kind: ir.EvDelta, Index: db.index, Delta: &ir.Delta{Kind: ir.DeltaText, Text: ev.Get("delta").Str}})

	case "response.output_text.annotation.added":
		var db *decBlock
		out, db = d.part(out, ev, "output_text")
		dl := &ir.Delta{Kind: ir.DeltaCitation}
		xset(&dl.Extra, xAnnotation, json.RawMessage(ev.Get("annotation").Raw))
		out = append(out, ir.Event{Kind: ir.EvDelta, Index: db.index, Delta: dl})

	case "response.content_part.done":
		it := d.item(ev)
		db := it.parts[ev.Get("content_index").Int()]
		if db != nil {
			b := &ir.Block{Type: ir.BlockText}
			p := ev.Get("part")
			known := []string{"type", "text"}
			if p.Get("type").Str == "refusal" {
				known = []string{"type", "refusal"}
			}
			xset(&b.Extra, xPart, restObject(p.Raw, known...))
			out = d.stopBlock(out, db, b)
		}

	case "response.reasoning_summary_part.added":
		it := d.item(ev)
		out = d.ensureThinking(out, it)
		if it.summaries > 0 && it.text {
			out = append(out, ir.Event{Kind: ir.EvDelta, Index: it.blk.index, Delta: &ir.Delta{Kind: ir.DeltaThinking, Text: "\n\n"}})
		}
		it.summaries++

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		it := d.item(ev)
		out = d.ensureThinking(out, it)
		if t := ev.Get("delta").Str; t != "" {
			it.text = true
			out = append(out, ir.Event{Kind: ir.EvDelta, Index: it.blk.index, Delta: &ir.Delta{Kind: ir.DeltaThinking, Text: t}})
		}

	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		it := d.item(ev)
		if it.blk == nil {
			break
		}
		t := ev.Get("delta").Str
		it.args += len(t)
		out = append(out, ir.Event{Kind: ir.EvDelta, Index: it.blk.index, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: t}})

	case "response.output_item.done":
		out = d.itemDone(out, d.item(ev), ev.Get("item"))

	case "response.completed", "response.incomplete":
		resp := ev.Get("response")
		out = d.ensureStart(out, resp)
		out = d.closeAll(out)
		if u := resp.Get("usage"); u.IsObject() {
			d.usage = decodeUsage(u)
			usage := d.usage
			out = append(out, ir.Event{Kind: ir.EvUsage, Usage: &usage})
		}
		stop := ir.StopEndTurn
		if typ == "response.incomplete" || resp.Get("status").Str == "incomplete" {
			stop = stopReason("incomplete", resp.Get("incomplete_details.reason").Str, nil)
		} else if d.sawTool {
			stop = ir.StopToolUse
		}
		d.terminal = true
		out = append(out, ir.Event{Kind: ir.EvDone, StopReason: stop})

	case "response.failed":
		resp := ev.Get("response")
		if u := resp.Get("usage"); u.IsObject() {
			d.usage = decodeUsage(u)
		}
		d.terminal = true
		out = append(out, ir.Event{Kind: ir.EvError, Err: errorFromObject(resp.Get("error"))})

	case "error":
		e := ev.Get("error")
		if !e.IsObject() {
			e = ev
		}
		d.terminal = true
		out = append(out, ir.Event{Kind: ir.EvError, Err: errorFromObject(e)})
	}
	return out, nil
}

func (d *streamDecoder) startTool(out []ir.Event, it *decItem, item gjson.Result) []ir.Event {
	if it.blk != nil {
		return out
	}
	tc := &ir.ToolCall{ID: item.Get("call_id").Str, Name: item.Get("name").Str, Kind: ir.ToolFunction, Namespace: item.Get("namespace").Str}
	known := []string{"call_id", "name", "arguments", "namespace", "status"}
	if item.Get("type").Str == itemCustomCall {
		tc.Kind = ir.ToolFreeform
		known = []string{"call_id", "name", "input", "namespace", "status"}
	}
	b := ir.Block{Type: ir.BlockToolUse, Tool: tc}
	xset(&b.Extra, xItem, itemRest(item, known...))
	out, it.blk = d.startBlock(out, b)
	return out
}

// ensurePart starts the text block of a message content part.
func (d *streamDecoder) ensurePart(out []ir.Event, ev gjson.Result, ptype string) []ir.Event {
	out, _ = d.part(out, ev, ptype)
	return out
}

func (d *streamDecoder) part(out []ir.Event, ev gjson.Result, ptype string) ([]ir.Event, *decBlock) {
	it := d.item(ev)
	ci := ev.Get("content_index").Int()
	if db := it.parts[ci]; db != nil {
		return out, db
	}
	if it.typ == "" {
		it.typ = itemMessage
	}
	b := ir.Block{Type: ir.BlockText}
	if ptype != "" && ptype != "output_text" {
		xset(&b.Extra, xPartType, jstr(ptype))
	}
	if !it.marked {
		it.marked = true
		rest := it.rest
		if rest == nil {
			rest = json.RawMessage("{}")
		}
		xset(&b.Extra, xItem, rest)
		if r := it.added.Get("role").Str; r != "" && r != "assistant" {
			xset(&b.Extra, xRole, jstr(r))
		}
	}
	var db *decBlock
	out, db = d.startBlock(out, b)
	it.parts[ci] = db
	return out, db
}

func (d *streamDecoder) ensureThinking(out []ir.Event, it *decItem) []ir.Event {
	if it.blk != nil {
		return out
	}
	it.typ = itemReasoning
	b := ir.Block{Type: ir.BlockThinking}
	if it.added.Exists() {
		xset(&b.Extra, xItem, itemRest(it.added, "summary", "content", "encrypted_content", "status"))
	}
	out, it.blk = d.startBlock(out, b)
	return out
}

func (d *streamDecoder) itemDone(out []ir.Event, it *decItem, item gjson.Result) []ir.Event {
	typ := item.Get("type").Str
	var n ir.Needs
	switch typ {
	case itemMessage:
		if len(it.parts) == 0 {
			// A message delivered whole, without part events.
			for _, b := range itemBlocks(item, ir.RoleAssistant, &n) {
				dropStatus(&b)
				var db *decBlock
				text := b.Text
				b.Text = ""
				out, db = d.startBlock(out, b)
				if text != "" {
					out = append(out, ir.Event{Kind: ir.EvDelta, Index: db.index, Delta: &ir.Delta{Kind: ir.DeltaText, Text: text}})
				}
				out = d.stopBlock(out, db, nil)
			}
			return out
		}
		idx := make([]int64, 0, len(it.parts))
		for ci := range it.parts {
			idx = append(idx, ci)
		}
		slices.Sort(idx)
		for _, ci := range idx {
			out = d.stopBlock(out, it.parts[ci], nil)
		}
	case itemReasoning:
		blk := reasoningBlock(item, &n)
		dropStatus(&blk)
		if it.blk == nil {
			if blk.Type == ir.BlockThinking {
				text := blk.Text
				start := ir.Block{Type: ir.BlockThinking, Extra: blk.Extra}
				var db *decBlock
				out, db = d.startBlock(out, start)
				out = append(out, ir.Event{Kind: ir.EvDelta, Index: db.index, Delta: &ir.Delta{Kind: ir.DeltaThinking, Text: text}})
				blk.Text = ""
				return d.stopBlock(out, db, &blk)
			}
			var db *decBlock
			out, db = d.startBlock(out, blk)
			return d.stopBlock(out, db, &blk)
		}
		stop := ir.Block{Type: ir.BlockThinking, Opaque: blk.Opaque, Extra: blk.Extra}
		return d.stopBlock(out, it.blk, &stop)
	case itemFunctionCall, itemCustomCall:
		out = d.startTool(out, it, item)
		full := item.Get("arguments").Str
		if typ == itemCustomCall {
			full = item.Get("input").Str
		}
		if it.args == 0 && full != "" {
			out = append(out, ir.Event{Kind: ir.EvDelta, Index: it.blk.index, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: full}})
		}
		return d.stopBlock(out, it.blk, nil)
	default:
		if it.blk != nil {
			return d.stopBlock(out, it.blk, nil)
		}
		bs := itemBlocks(item, ir.RoleAssistant, &n)
		for i := range bs {
			b := bs[i]
			dropStatus(&b)
			var db *decBlock
			out, db = d.startBlock(out, b)
			out = d.stopBlock(out, db, &b)
		}
	}
	return out
}

func (d *streamDecoder) closeAll(out []ir.Event) []ir.Event {
	for _, db := range d.open {
		out = d.stopBlock(out, db, nil)
	}
	return out
}
