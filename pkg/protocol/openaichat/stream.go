package openaichat

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/sse"
)

// NewStreamDecoder returns a decoder for chat.completion.chunk streams.
func (Codec) NewStreamDecoder() protocol.StreamDecoder {
	return &streamDecoder{cur: -1, tools: map[int]*toolSlot{}}
}

type toolSlot struct {
	index  int
	id     string
	closed bool
}

// streamDecoder keeps one block open at a time: a change of kind (reasoning,
// text, another tool call) closes the open block. A fragment for a tool call
// whose block was already closed cannot be delivered and is dropped.
type streamDecoder struct {
	started  bool
	done     bool
	finish   *string
	next     int
	cur      int
	curKind  ir.BlockType
	curTool  *toolSlot
	tools    map[int]*toolSlot
	toolUsed bool
	usage    ir.Usage
	hasUsage bool
}

type chunk struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Usage   json.RawMessage `json:"usage"`
	Error   json.RawMessage `json:"error"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			ReasoningContent *string         `json:"reasoning_content"`
			Reasoning        json.RawMessage `json:"reasoning"`
			ReasoningDetails json.RawMessage `json:"reasoning_details"`
			Content          *string         `json:"content"`
			Refusal          *string         `json:"refusal"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function *struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
				Custom *struct {
					Name  string `json:"name"`
					Input string `json:"input"`
				} `json:"custom"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

var doneMarker = []byte("[DONE]")

func (d *streamDecoder) Feed(ev sse.Event) ([]ir.Event, error) {
	if d.done || ev.Comment {
		return nil, nil
	}
	data := bytes.TrimSpace(ev.Data)
	if len(data) == 0 {
		return nil, nil
	}
	if bytes.Equal(data, doneMarker) {
		return d.end(), nil
	}
	var c chunk
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, ir.Errorf(ir.ErrUpstream, "invalid stream chunk: %s", err.Error())
	}
	if !isNull(c.Error) {
		d.done = true
		return []ir.Event{{Kind: ir.EvError, Err: Codec{}.DecodeError(0, nil, data)}}, nil
	}
	var out []ir.Event
	if !d.started {
		d.started = true
		out = append(out, ir.Event{Kind: ir.EvStart, ID: c.ID, Model: c.Model})
	}
	for _, ch := range c.Choices {
		if ch.Index != 0 {
			continue
		}
		dl := ch.Delta
		text := ""
		hasReasoning := false
		if dl.ReasoningContent != nil && *dl.ReasoningContent != "" {
			text, hasReasoning = *dl.ReasoningContent, true
		} else if s := str(dl.Reasoning); s != "" {
			text, hasReasoning = s, true
		}
		var extra ir.Extra
		if !isNull(dl.ReasoningDetails) && string(dl.ReasoningDetails) != "[]" {
			extra.Set(Name, "reasoning_details", dl.ReasoningDetails)
			if !hasReasoning {
				text = detailsText(dl.ReasoningDetails)
			}
			hasReasoning = true
		}
		if hasReasoning {
			out = d.open(out, ir.BlockThinking, nil)
			out = append(out, ir.Event{Kind: ir.EvDelta, Index: d.cur, Delta: &ir.Delta{Kind: ir.DeltaThinking, Text: text, Extra: extra}})
		}
		for _, s := range []*string{dl.Content, dl.Refusal} {
			if s != nil && *s != "" {
				out = d.open(out, ir.BlockText, nil)
				out = append(out, ir.Event{Kind: ir.EvDelta, Index: d.cur, Delta: &ir.Delta{Kind: ir.DeltaText, Text: *s}})
			}
		}
		for _, tc := range dl.ToolCalls {
			slot := d.tools[tc.Index]
			if slot == nil || (tc.ID != "" && slot.id != "" && tc.ID != slot.id) {
				call := &ir.ToolCall{ID: tc.ID, Kind: ir.ToolFunction}
				if call.ID == "" {
					call.ID = "call_" + uuid.NewString()
				}
				if tc.Function != nil {
					call.Name = tc.Function.Name
				}
				if tc.Type == "custom" || tc.Custom != nil {
					call.Kind = ir.ToolFreeform
					if tc.Custom != nil {
						call.Name = tc.Custom.Name
					}
				}
				slot = &toolSlot{id: tc.ID}
				d.tools[tc.Index] = slot
				out = d.open(out, ir.BlockToolUse, call)
				slot.index = d.cur
				d.curTool = slot
				d.toolUsed = true
			} else if slot.closed {
				continue
			}
			frag := ""
			if tc.Function != nil {
				frag = tc.Function.Arguments
			} else if tc.Custom != nil {
				frag = tc.Custom.Input
			}
			if frag != "" {
				out = append(out, ir.Event{Kind: ir.EvDelta, Index: slot.index, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: frag}})
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			f := *ch.FinishReason
			d.finish = &f
			out = d.close(out)
		}
	}
	if !isNull(c.Usage) {
		d.usage, d.hasUsage = decodeUsage(c.Usage), true
		u := d.usage
		out = append(out, ir.Event{Kind: ir.EvUsage, Usage: &u})
	}
	return out, nil
}

// open makes a block of kind the open one, closing any other. A non-nil
// call always starts a new block.
func (d *streamDecoder) open(out []ir.Event, kind ir.BlockType, call *ir.ToolCall) []ir.Event {
	if call == nil && d.cur >= 0 && d.curKind == kind {
		return out
	}
	out = d.close(out)
	d.cur, d.curKind = d.next, kind
	d.next++
	b := &ir.Block{Type: kind, Tool: call}
	return append(out, ir.Event{Kind: ir.EvBlockStart, Index: d.cur, Block: b})
}

func (d *streamDecoder) close(out []ir.Event) []ir.Event {
	if d.cur < 0 {
		return out
	}
	out = append(out, ir.Event{Kind: ir.EvBlockStop, Index: d.cur})
	if d.curTool != nil {
		d.curTool.closed = true
		d.curTool = nil
	}
	d.cur = -1
	return out
}

func (d *streamDecoder) end() []ir.Event {
	out := d.close(nil)
	finish := ""
	if d.finish != nil {
		finish = *d.finish
	}
	stop := stopReason(finish, d.toolUsed)
	if stop == "" {
		stop = ir.StopEndTurn
	}
	ev := ir.Event{Kind: ir.EvDone, StopReason: stop}
	if d.hasUsage {
		u := d.usage
		ev.Usage = &u
	}
	d.done = true
	return append(out, ev)
}

// Finish ends the stream. It is complete when [DONE] or a finish_reason was seen.
func (d *streamDecoder) Finish() ([]ir.Event, bool) {
	if d.done {
		return nil, true
	}
	if d.finish != nil {
		return d.end(), true
	}
	return nil, false
}

func (d *streamDecoder) Usage() ir.Usage { return d.usage }

// NewStreamEncoder renders IR events as chat.completion.chunk SSE frames.
func (Codec) NewStreamEncoder(w sse.Writer, req *ir.Request) protocol.StreamEncoder {
	e := &streamEncoder{w: w, created: time.Now().Unix(), blocks: map[int]encBlock{}, customOK: requestHasCustom(req)}
	if req != nil {
		e.model = req.Model
		if v, ok := req.Extra.Get(Name, "stream_options"); ok {
			var so struct {
				IncludeUsage bool `json:"include_usage"`
			}
			_ = json.Unmarshal(v, &so)
			e.includeUsage = so.IncludeUsage
		}
	}
	return e
}

type encBlock struct {
	kind     ir.BlockType
	wire     int
	freeform bool
}

type streamEncoder struct {
	w            sse.Writer
	id           string
	created      int64
	model        string
	includeUsage bool
	customOK     bool
	started      bool
	done         bool
	blocks       map[int]encBlock
	nextTool     int
	usage        ir.Usage
}

func (e *streamEncoder) Usage() ir.Usage { return e.usage }

func (e *streamEncoder) Keepalive() error { return e.w.Comment("keepalive") }

func (e *streamEncoder) write(choices any, extra ...field) error {
	o := object{
		{"id", e.id},
		{"object", "chat.completion.chunk"},
		{"created", e.created},
		{"model", e.model},
		{"choices", choices},
	}
	o = append(o, extra...)
	b, err := marshal(o)
	if err != nil {
		return err
	}
	return e.w.Event("", b)
}

func (e *streamEncoder) delta(d object, finish any) error {
	return e.write([]any{object{{"index", 0}, {"delta", d}, {"finish_reason", finish}}})
}

func (e *streamEncoder) start() error {
	if e.started {
		return nil
	}
	e.started = true
	if e.id == "" {
		e.id = newID()
	}
	return e.delta(object{{"role", "assistant"}, {"content", ""}}, nil)
}

func (e *streamEncoder) Encode(ev *ir.Event) error {
	if e.done {
		return nil
	}
	switch ev.Kind {
	case ir.EvStart:
		if ev.ID != "" && !e.started {
			e.id = ev.ID
		}
		if ev.Model != "" && !e.started {
			e.model = ev.Model
		}
		return e.start()
	case ir.EvPing:
		return e.Keepalive()
	case ir.EvStatus:
		return nil
	case ir.EvUsage:
		if ev.Usage != nil {
			e.usage = *ev.Usage
		}
		return nil
	case ir.EvError:
		e.done = true
		gw := ev.Err
		if gw == nil {
			gw = ir.Errorf(ir.ErrInternal, "stream failed")
		}
		b, err := marshal(errorBody(gw))
		if err != nil {
			return err
		}
		return e.w.Event("", b)
	}
	if err := e.start(); err != nil {
		return err
	}
	switch ev.Kind {
	case ir.EvBlockStart:
		return e.blockStart(ev)
	case ir.EvDelta:
		if ev.Delta == nil {
			return nil
		}
		return e.blockDelta(ev.Index, ev.Delta)
	case ir.EvDone:
		e.done = true
		if ev.Usage != nil {
			e.usage = *ev.Usage
		}
		if err := e.delta(object{}, finishReason(ev.StopReason)); err != nil {
			return err
		}
		if e.includeUsage {
			if err := e.write([]any{}, field{"usage", encodeUsage(e.usage)}); err != nil {
				return err
			}
		}
		return e.w.Event("", doneMarker)
	}
	return nil
}

func (e *streamEncoder) blockStart(ev *ir.Event) error {
	b := ev.Block
	if b == nil {
		return nil
	}
	eb := encBlock{kind: b.Type}
	switch b.Type {
	case ir.BlockToolUse:
		if b.Tool == nil {
			return nil
		}
		eb.wire = e.nextTool
		e.nextTool++
		eb.freeform = b.Tool.Kind == ir.ToolFreeform && e.customOK
		e.blocks[ev.Index] = eb
		call := object{{"index", eb.wire}, {"id", b.Tool.ID}}
		if eb.freeform {
			call = append(call, field{"type", "custom"}, field{"custom", object{{"name", b.Tool.Name}, {"input", b.Tool.Text}}})
		} else {
			args := ""
			if b.Tool.Kind == ir.ToolFreeform {
				args = b.Tool.Text
			} else if len(b.Tool.Input) > 0 {
				args = string(b.Tool.Input)
			}
			call = append(call, field{"type", "function"}, field{"function", object{{"name", b.Tool.Name}, {"arguments", args}}})
		}
		return e.delta(object{{"tool_calls", []any{call}}}, nil)
	case ir.BlockText, ir.BlockThinking:
		e.blocks[ev.Index] = eb
		if b.Text != "" {
			k := ir.DeltaText
			if b.Type == ir.BlockThinking {
				k = ir.DeltaThinking
			}
			return e.blockDelta(ev.Index, &ir.Delta{Kind: k, Text: b.Text})
		}
	}
	return nil
}

func (e *streamEncoder) blockDelta(index int, d *ir.Delta) error {
	switch d.Kind {
	case ir.DeltaText:
		if d.Text == "" {
			return nil
		}
		return e.delta(object{{"content", d.Text}}, nil)
	case ir.DeltaThinking:
		o := object{}
		if d.Text != "" {
			o.add("reasoning_content", d.Text)
		}
		if v, ok := d.Extra.Get(Name, "reasoning_details"); ok {
			o.add("reasoning_details", v)
		}
		if len(o) == 0 {
			return nil
		}
		return e.delta(o, nil)
	case ir.DeltaToolInput:
		eb, ok := e.blocks[index]
		if !ok || eb.kind != ir.BlockToolUse || d.Text == "" {
			return nil
		}
		call := object{{"index", eb.wire}}
		if eb.freeform {
			call.add("custom", object{{"input", d.Text}})
		} else {
			call.add("function", object{{"arguments", d.Text}})
		}
		return e.delta(object{{"tool_calls", []any{call}}}, nil)
	}
	return nil
}
