package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/sse"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NewStreamDecoder returns a decoder for a messages SSE stream.
func (Codec) NewStreamDecoder() protocol.StreamDecoder { return &streamDecoder{} }

type streamDecoder struct {
	usageRaw   []byte // message_start usage with message_delta usage merged in
	usage      ir.Usage
	stop       ir.StopReason
	stopSeq    string
	complete   bool
	terminated bool // after message_stop or error
}

func (d *streamDecoder) Usage() ir.Usage { return d.usage }

func (d *streamDecoder) Finish() ([]ir.Event, bool) { return nil, d.complete }

func (d *streamDecoder) Feed(ev sse.Event) ([]ir.Event, error) {
	if ev.Comment || len(ev.Data) == 0 || d.terminated {
		return nil, nil
	}
	if !gjson.ValidBytes(ev.Data) {
		return nil, fmt.Errorf("anthropic stream: invalid JSON in %q event", ev.Name)
	}
	data := gjson.ParseBytes(ev.Data)
	typ := ev.Name
	if typ == "" {
		typ = data.Get("type").String()
	}
	idx := int(data.Get("index").Int())
	switch typ {
	case "message_start":
		msg := data.Get("message")
		out := []ir.Event{{Kind: ir.EvStart, ID: msg.Get("id").String(), Model: msg.Get("model").String()}}
		if u := msg.Get("usage"); u.IsObject() {
			// Carried on the start event too, so an encoder can fill its
			// own message_start without waiting for the usage event.
			d.mergeUsage(u)
			u := d.usage
			out[0].Usage = &u
			out = append(out, d.usageEvent())
		}
		return out, nil
	case "content_block_start":
		b := decodeBlock(data.Get("content_block"))
		if b.Tool != nil && string(b.Tool.Input) == emptyObject {
			// Input arrives as input_json_delta fragments.
			b.Tool.Input = nil
		}
		if b.Opaque != nil && b.Type == ir.BlockThinking && string(b.Opaque.Data) == `""` {
			// The signature arrives as a signature_delta.
			b.Opaque = nil
		}
		return []ir.Event{{Kind: ir.EvBlockStart, Index: idx, Block: &b}}, nil
	case "content_block_delta":
		delta := decodeDelta(data.Get("delta"))
		if delta == nil {
			return nil, nil
		}
		return []ir.Event{{Kind: ir.EvDelta, Index: idx, Delta: delta}}, nil
	case "content_block_stop":
		return []ir.Event{{Kind: ir.EvBlockStop, Index: idx}}, nil
	case "message_delta":
		if s := data.Get("delta.stop_reason"); s.Exists() {
			d.stop = ir.StopReason(s.String())
		}
		if s := data.Get("delta.stop_sequence"); s.Exists() {
			d.stopSeq = s.String()
		}
		if u := data.Get("usage"); u.IsObject() {
			d.mergeUsage(u)
			return []ir.Event{d.usageEvent()}, nil
		}
		return nil, nil
	case "message_stop":
		d.complete, d.terminated = true, true
		u := d.usage
		return []ir.Event{{Kind: ir.EvDone, StopReason: d.stop, StopSeq: d.stopSeq, Usage: &u}}, nil
	case "ping":
		return []ir.Event{{Kind: ir.EvPing}}, nil
	case "error":
		d.terminated = true
		return []ir.Event{{Kind: ir.EvError, Err: Codec{}.DecodeError(0, nil, ev.Data)}}, nil
	}
	return nil, nil
}

func (d *streamDecoder) usageEvent() ir.Event {
	u := d.usage
	return ir.Event{Kind: ir.EvUsage, Usage: &u}
}

// mergeUsage overlays a usage report on the ones seen so far: message_delta
// repeats cumulative counts for the members it carries.
func (d *streamDecoder) mergeUsage(u gjson.Result) {
	if d.usageRaw == nil {
		d.usageRaw = []byte(u.Raw)
	} else {
		u.ForEach(func(k, v gjson.Result) bool {
			if v.Type == gjson.Null {
				return true
			}
			if out, err := sjson.SetRawBytes(d.usageRaw, k.String(), []byte(v.Raw)); err == nil {
				d.usageRaw = out
			}
			return true
		})
	}
	d.usage = decodeUsage(gjson.ParseBytes(d.usageRaw))
}

func decodeDelta(v gjson.Result) *ir.Delta {
	switch v.Get("type").String() {
	case "text_delta":
		return &ir.Delta{Kind: ir.DeltaText, Text: v.Get("text").String()}
	case "thinking_delta":
		return &ir.Delta{Kind: ir.DeltaThinking, Text: v.Get("thinking").String()}
	case "input_json_delta":
		return &ir.Delta{Kind: ir.DeltaToolInput, Text: v.Get("partial_json").String()}
	case "signature_delta":
		return &ir.Delta{Kind: ir.DeltaOpaque, Opaque: opaqueOf(kindSignature, v.Get("signature"))}
	case "citations_delta":
		d := &ir.Delta{Kind: ir.DeltaCitation}
		d.Extra.Set(name, "citation", json.RawMessage(v.Get("citation").Raw))
		return d
	}
	return nil
}
