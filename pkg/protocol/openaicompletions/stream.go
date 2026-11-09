package openaicompletions

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/sse"
)

var doneMarker = []byte("[DONE]")

// NewStreamDecoder returns a decoder for text_completion chunk streams.
func (Codec) NewStreamDecoder() protocol.StreamDecoder { return &streamDecoder{} }

// streamDecoder turns choice 0's text into one text block.
type streamDecoder struct {
	started  bool
	open     bool
	done     bool
	finish   *string
	usage    ir.Usage
	hasUsage bool
}

type chunk struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Usage   json.RawMessage `json:"usage"`
	Error   json.RawMessage `json:"error"`
	Choices []struct {
		Index        int     `json:"index"`
		Text         string  `json:"text"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

func (d *streamDecoder) Feed(ev sse.Event) ([]ir.Event, error) {
	if d.done || ev.Comment {
		return nil, nil
	}
	data := bytes.TrimSpace(ev.Data)
	if len(data) == 0 {
		return nil, nil
	}
	if bytes.Equal(data, doneMarker) {
		return d.end(nil), nil
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
		if ch.Text != "" {
			if !d.open {
				d.open = true
				out = append(out, ir.Event{Kind: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}})
			}
			out = append(out, ir.Event{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaText, Text: ch.Text}})
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			f := *ch.FinishReason
			d.finish = &f
		}
	}
	if !isNull(c.Usage) {
		d.usage, d.hasUsage = decodeUsage(c.Usage), true
		u := d.usage
		out = append(out, ir.Event{Kind: ir.EvUsage, Usage: &u})
	}
	return out, nil
}

// end closes the text block and reports the stop reason.
func (d *streamDecoder) end(out []ir.Event) []ir.Event {
	if d.open {
		out = append(out, ir.Event{Kind: ir.EvBlockStop, Index: 0})
		d.open = false
	}
	stop := ir.StopEndTurn
	if d.finish != nil {
		if s := stopReason(*d.finish); s != "" {
			stop = s
		}
	}
	ev := ir.Event{Kind: ir.EvDone, StopReason: stop}
	if d.hasUsage {
		u := d.usage
		ev.Usage = &u
	}
	d.done = true
	return append(out, ev)
}

// Finish ends the stream. It is complete when [DONE] or a finish_reason
// was seen.
func (d *streamDecoder) Finish() ([]ir.Event, bool) {
	if d.done {
		return nil, true
	}
	if d.finish != nil {
		return d.end(nil), true
	}
	return nil, false
}

func (d *streamDecoder) Usage() ir.Usage { return d.usage }

// NewStreamEncoder renders IR events as text_completion SSE chunks. A
// usage chunk is sent before [DONE] when the client asked for it with
// stream_options.include_usage.
func (Codec) NewStreamEncoder(w sse.Writer, req *ir.Request) protocol.StreamEncoder {
	e := &streamEncoder{w: w, created: time.Now().Unix()}
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

type streamEncoder struct {
	w            sse.Writer
	id           string
	created      int64
	model        string
	includeUsage bool
	started      bool
	done         bool
	usage        ir.Usage
}

func (e *streamEncoder) Usage() ir.Usage { return e.usage }

func (e *streamEncoder) Keepalive() error { return e.w.Comment("keepalive") }

func (e *streamEncoder) write(choices []any, extra ...field) error {
	o := object{
		{"id", e.id},
		{"object", "text_completion"},
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

func (e *streamEncoder) text(s string, finish any) error {
	return e.write([]any{object{{"text", s}, {"index", 0}, {"logprobs", nil}, {"finish_reason", finish}}})
}

func (e *streamEncoder) start() {
	if !e.started {
		e.started = true
		if e.id == "" {
			e.id = newID()
		}
	}
}

func (e *streamEncoder) Encode(ev *ir.Event) error {
	if e.done {
		return nil
	}
	switch ev.Kind {
	case ir.EvStart:
		if !e.started {
			if ev.ID != "" {
				e.id = ev.ID
			}
			if ev.Model != "" {
				e.model = ev.Model
			}
		}
		e.start()
	case ir.EvPing:
		return e.Keepalive()
	case ir.EvUsage:
		if ev.Usage != nil {
			e.usage = *ev.Usage
		}
	case ir.EvError:
		e.done = true
		gw := ev.Err
		if gw == nil {
			gw = ir.Errorf(ir.ErrInternal, "stream failed")
		}
		_, _, body := Codec{}.EncodeError(gw)
		return e.w.Event("", body)
	case ir.EvBlockStart:
		e.start()
		if b := ev.Block; b != nil && b.Type == ir.BlockText && b.Text != "" {
			return e.text(b.Text, nil)
		}
	case ir.EvDelta:
		e.start()
		if d := ev.Delta; d != nil && d.Kind == ir.DeltaText && d.Text != "" {
			return e.text(d.Text, nil)
		}
	case ir.EvDone:
		e.start()
		e.done = true
		if ev.Usage != nil {
			e.usage = *ev.Usage
		}
		if err := e.text("", finishReason(ev.StopReason)); err != nil {
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
