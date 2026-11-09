package pipeline

import (
	"net/http"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/sse"
)

// reply sends a plugin's short-circuit answer in the client's protocol.
func (x *exchange) reply(r *plugin.Reply) {
	if r.Error != nil || r.Response == nil {
		gw := r.Error
		if gw == nil {
			gw = ir.Errorf(ir.ErrInternal, "empty plugin reply")
		}
		x.fail(gw)
		return
	}
	resp := r.Response
	u := resp.Usage
	x.replyUsage = &u
	if x.in.Stream {
		x.out = newClientOut(x)
		enc := x.codec.NewStreamEncoder(sse.NewWriter(liveWriter{x.out}), x.clientReq())
		for _, ev := range responseEvents(resp) {
			if err := enc.Encode(&ev); err != nil {
				x.status = statusClientClosed
				return
			}
		}
		x.status, x.complete = http.StatusOK, true
		return
	}
	body, err := x.codec.EncodeResponse(x.clientReq(), resp)
	if err != nil {
		x.fail(ir.Errorf(ir.ErrInternal, "could not render the plugin reply"))
		return
	}
	h := http.Header{"Content-Type": {"application/json"}}
	x.addHeaders(h, nil)
	writePlain(x.w, http.StatusOK, h, body)
	x.status, x.complete = http.StatusOK, true
}

// responseEvents replays a complete response as a stream.
func responseEvents(resp *ir.Response) []ir.Event {
	evs := []ir.Event{{Kind: ir.EvStart, ID: resp.ID, Model: resp.Model}}
	for i, b := range resp.Content {
		start := b
		var delta *ir.Delta
		switch b.Type {
		case ir.BlockText:
			start.Text = ""
			delta = &ir.Delta{Kind: ir.DeltaText, Text: b.Text}
		case ir.BlockThinking:
			start.Text, start.Opaque = "", nil
			delta = &ir.Delta{Kind: ir.DeltaThinking, Text: b.Text}
		case ir.BlockToolUse:
			if b.Tool != nil {
				tc := *b.Tool
				text := string(tc.Input)
				if tc.Kind == ir.ToolFreeform {
					text = tc.Text
				}
				tc.Input, tc.Text = nil, ""
				start.Tool = &tc
				delta = &ir.Delta{Kind: ir.DeltaToolInput, Text: text}
			}
		}
		evs = append(evs, ir.Event{Kind: ir.EvBlockStart, Index: i, Block: &start})
		if delta != nil && delta.Text != "" {
			evs = append(evs, ir.Event{Kind: ir.EvDelta, Index: i, Delta: delta})
		}
		stop := ir.Event{Kind: ir.EvBlockStop, Index: i}
		if b.Type == ir.BlockThinking && b.Opaque != nil {
			stop.Block = &ir.Block{Type: b.Type, Opaque: b.Opaque}
		}
		evs = append(evs, stop)
	}
	u := resp.Usage
	evs = append(evs,
		ir.Event{Kind: ir.EvUsage, Usage: &u},
		ir.Event{Kind: ir.EvDone, StopReason: resp.StopReason, StopSeq: resp.StopSeq, Usage: &u})
	return evs
}
