package openairesponses

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/sse"
	"github.com/tidwall/gjson"
)

// decodeFile runs a recorded stream through the stream decoder.
func decodeFile(t *testing.T, path string) ([]ir.Event, bool, ir.Usage) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return decodeStream(t, f)
}

func decodeStream(t *testing.T, r io.Reader) ([]ir.Event, bool, ir.Usage) {
	t.Helper()
	d := Codec{}.NewStreamDecoder()
	rd := sse.NewReader(r, 0)
	var evs []ir.Event
	for {
		ev, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out, err := d.Feed(ev)
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, out...)
	}
	tail, complete := d.Finish()
	return append(evs, tail...), complete, d.Usage()
}

// assembled is a block rebuilt from its events.
type assembled struct {
	start ir.Block
	text  string
	stop  *ir.Block
}

func assemble(t *testing.T, evs []ir.Event) []*assembled {
	t.Helper()
	var bs []*assembled
	for _, ev := range evs {
		switch ev.Kind {
		case ir.EvBlockStart:
			if ev.Index != len(bs) {
				t.Fatalf("block index %d not dense (have %d)", ev.Index, len(bs))
			}
			bs = append(bs, &assembled{start: *ev.Block})
		case ir.EvDelta:
			bs[ev.Index].text += ev.Delta.Text
		case ir.EvBlockStop:
			if bs[ev.Index].stop != nil {
				t.Fatalf("block %d stopped twice", ev.Index)
			}
			if ev.Block == nil {
				ev.Block = &ir.Block{}
			}
			bs[ev.Index].stop = ev.Block
		}
	}
	for i, b := range bs {
		if b.stop == nil {
			t.Fatalf("block %d never stopped", i)
		}
	}
	return bs
}

func lastDone(t *testing.T, evs []ir.Event) ir.Event {
	t.Helper()
	ev := evs[len(evs)-1]
	if ev.Kind != ir.EvDone {
		t.Fatalf("last event %s, want done", ev.Kind)
	}
	return ev
}

func doneItems(t *testing.T, path, typ string) []gjson.Result {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []gjson.Result
	for _, line := range strings.Split(string(raw), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if ok && gjson.Get(data, "type").Str == "response.output_item.done" && gjson.Get(data, "item.type").Str == typ {
			out = append(out, gjson.Get(data, "item"))
		}
	}
	return out
}

func TestStreamDecodeReasoningFunction(t *testing.T) {
	const path = "testdata/stream_reasoning_function.sse"
	evs, complete, usage := decodeFile(t, path)
	if !complete {
		t.Fatal("stream not complete")
	}
	if evs[0].Kind != ir.EvStart || !strings.HasPrefix(evs[0].ID, "resp_") {
		t.Fatalf("first event %+v", evs[0])
	}
	bs := assemble(t, evs)
	if len(bs) != 3 {
		t.Fatalf("got %d blocks", len(bs))
	}
	rs := doneItems(t, path, "reasoning")[0]
	th := bs[0]
	if th.start.Type != ir.BlockThinking || !strings.HasPrefix(th.text, "**Planning tool calls**") {
		t.Fatalf("thinking block %+v %q", th.start, th.text)
	}
	if th.stop.Opaque == nil || th.stop.Opaque.Kind != kindEncrypted || string(th.stop.Opaque.Data) != rs.Get("encrypted_content").Raw || th.stop.Opaque.Sealed {
		t.Fatalf("reasoning opaque not carried on stop: %+v", th.stop.Opaque)
	}
	if got := jsonField(mustX(t, &th.stop.Extra, xItem), "id"); got != rs.Get("id").Str {
		t.Fatalf("reasoning id %q", got)
	}
	for i, city := range []string{"Paris", "Tokyo"} {
		b := bs[i+1]
		if b.start.Type != ir.BlockToolUse || b.start.Tool.Name != "get_weather" || b.start.Tool.Kind != ir.ToolFunction ||
			!strings.HasPrefix(b.start.Tool.ID, "call_") {
			t.Fatalf("tool block %d: %+v", i, b.start.Tool)
		}
		if b.text != `{"city":"`+city+`"}` {
			t.Fatalf("tool args %q", b.text)
		}
	}
	if d := lastDone(t, evs); d.StopReason != ir.StopToolUse {
		t.Fatalf("stop reason %s", d.StopReason)
	}
	if usage.Input != 76 || usage.Output != 86 || usage.Reasoning != 36 || usage.CacheRead != 0 || len(usage.Raw) == 0 {
		t.Fatalf("usage %+v", usage)
	}
}

func mustX(t *testing.T, e *ir.Extra, k string) []byte {
	t.Helper()
	v, ok := xget(e, k)
	if !ok {
		t.Fatalf("extra %q missing in %+v", k, e)
	}
	return v
}

func TestStreamDecodeCustomTool(t *testing.T) {
	evs, complete, usage := decodeFile(t, "testdata/stream_custom_tool.sse")
	if !complete {
		t.Fatal("stream not complete")
	}
	bs := assemble(t, evs)
	if len(bs) != 2 {
		t.Fatalf("got %d blocks", len(bs))
	}
	if bs[0].start.Type != ir.BlockText || bs[0].text != "I will add the requested hello.txt file." {
		t.Fatalf("text block %+v %q", bs[0].start, bs[0].text)
	}
	if got := jsonField(mustX(t, &bs[0].start.Extra, xItem), "phase"); got != "commentary" {
		t.Fatalf("phase not kept: %q", got)
	}
	tool := bs[1]
	if tool.start.Tool.Kind != ir.ToolFreeform || tool.start.Tool.Name != "apply_patch" {
		t.Fatalf("tool %+v", tool.start.Tool)
	}
	if tool.text != "*** Begin Patch\n*** Add File: hello.txt\n+hi there\n*** End Patch\n" {
		t.Fatalf("patch %q", tool.text)
	}
	if lastDone(t, evs).StopReason != ir.StopToolUse || usage.Output == 0 {
		t.Fatalf("done/usage wrong: %+v", usage)
	}
}

func TestStreamDecodeIncomplete(t *testing.T) {
	evs, complete, usage := decodeFile(t, "testdata/stream_incomplete.sse")
	if !complete || lastDone(t, evs).StopReason != ir.StopMaxTokens || usage.Output != 16 {
		t.Fatalf("complete=%v usage=%+v", complete, usage)
	}
	bs := assemble(t, evs)
	if bs[0].start.Type != ir.BlockOpaque && bs[0].start.Type != ir.BlockRaw {
		t.Fatalf("reasoning without summary should be opaque or raw, got %s", bs[0].start.Type)
	}
}

func TestStreamDecodeFailure(t *testing.T) {
	in := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\"}}\n\n" +
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"status\":\"failed\",\"error\":{\"code\":\"context_length_exceeded\",\"message\":\"too long\"}}}\n\n"
	evs, complete, _ := decodeStream(t, strings.NewReader(in))
	last := evs[len(evs)-1]
	if !complete || last.Kind != ir.EvError || last.Err.Kind != ir.ErrContextTooLong {
		t.Fatalf("got %+v", last)
	}
	_, complete, _ = decodeStream(t, strings.NewReader(in[:strings.Index(in, "event: response.failed")]))
	if complete {
		t.Fatal("truncated stream reported complete")
	}
}

// encodeEvents renders events through the stream encoder.
func encodeEvents(t *testing.T, req *ir.Request, evs []ir.Event) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := Codec{}.NewStreamEncoder(sse.NewWriter(&buf), req)
	for i := range evs {
		if err := enc.Encode(&evs[i]); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

type wireEvent struct {
	name string
	data gjson.Result
}

func parseWire(t *testing.T, b []byte) []wireEvent {
	t.Helper()
	rd := sse.NewReader(bytes.NewReader(b), 0)
	var out []wireEvent
	for {
		ev, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Comment {
			out = append(out, wireEvent{name: ":"})
			continue
		}
		d := gjson.ParseBytes(ev.Data)
		if d.Get("type").Str != ev.Name {
			t.Fatalf("event name %q vs type %q", ev.Name, d.Get("type").Str)
		}
		out = append(out, wireEvent{ev.Name, d})
	}
}

func TestStreamReencodeRecorded(t *testing.T) {
	for _, path := range []string{"testdata/stream_reasoning_function.sse", "testdata/stream_custom_tool.sse"} {
		t.Run(path, func(t *testing.T) {
			evs, _, _ := decodeFile(t, path)
			out := encodeEvents(t, &ir.Request{Model: "client-model"}, evs)
			wire := parseWire(t, out)
			for i, w := range wire {
				if w.data.Get("sequence_number").Int() != int64(i) {
					t.Fatalf("sequence_number %d at %d", w.data.Get("sequence_number").Int(), i)
				}
			}
			if wire[0].name != "response.created" || wire[1].name != "response.in_progress" {
				t.Fatalf("stream opens with %s, %s", wire[0].name, wire[1].name)
			}
			last := wire[len(wire)-1]
			if last.name != "response.completed" {
				t.Fatalf("stream ends with %s", last.name)
			}
			resp := last.data.Get("response")
			if resp.Get("usage.output_tokens").Int() == 0 || !resp.Get("usage.input_tokens_details.cached_tokens").Exists() {
				t.Fatalf("completed without usage: %s", resp.Get("usage").Raw)
			}
			if resp.Get("model").Str != "client-model" {
				t.Fatalf("model %q", resp.Get("model").Str)
			}
			// Same-protocol re-encode keeps the upstream item ids and payloads.
			upstream := map[string]gjson.Result{}
			for _, typ := range []string{"reasoning", "function_call", "custom_tool_call", "message"} {
				for _, it := range doneItems(t, path, typ) {
					upstream[it.Get("id").Str] = it
				}
			}
			outItems := resp.Get("output").Array()
			if len(outItems) != len(upstream) {
				t.Fatalf("output has %d items, upstream %d", len(outItems), len(upstream))
			}
			for _, it := range outItems {
				up, ok := upstream[it.Get("id").Str]
				if !ok {
					t.Fatalf("item id %q not preserved", it.Get("id").Str)
				}
				for _, k := range []string{"type", "call_id", "name", "arguments", "input", "encrypted_content", "phase", "summary.0.text", "content.0.text"} {
					if up.Get(k).Raw != it.Get(k).Raw && up.Get(k).String() != it.Get(k).String() {
						t.Fatalf("%s field %s: %s vs %s", it.Get("id").Str, k, up.Get(k).Raw, it.Get(k).Raw)
					}
				}
			}
			// The encoder's output decodes back to the same blocks.
			again, complete, _ := decodeStream(t, bytes.NewReader(out))
			if !complete {
				t.Fatal("re-encoded stream not complete")
			}
			a, b := assemble(t, evs), assemble(t, again)
			if len(a) != len(b) {
				t.Fatalf("%d blocks vs %d", len(a), len(b))
			}
			for i := range a {
				if a[i].start.Type != b[i].start.Type || a[i].text != b[i].text {
					t.Fatalf("block %d differs: %s %q vs %s %q", i, a[i].start.Type, a[i].text, b[i].start.Type, b[i].text)
				}
			}
		})
	}
}

func TestStreamEncodeForeignIR(t *testing.T) {
	seal := `"` + ir.SealPrefix + `abc"`
	evs := []ir.Event{
		{Kind: ir.EvPing},
		{Kind: ir.EvStart, ID: "msg_upstream", Model: "upstream-model"},
		{Kind: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking}},
		{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaThinking, Text: "think"}},
		{Kind: ir.EvBlockStop, Index: 0, Block: &ir.Block{Type: ir.BlockThinking, Opaque: &ir.OpaqueState{Kind: "signature", Data: []byte(seal), Sealed: true}}},
		{Kind: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockOpaque, Opaque: &ir.OpaqueState{Kind: "redacted_thinking", Data: []byte(seal), Sealed: true}}},
		{Kind: ir.EvBlockStop, Index: 1},
		{Kind: ir.EvBlockStart, Index: 2, Block: &ir.Block{Type: ir.BlockText}},
		{Kind: ir.EvDelta, Index: 2, Delta: &ir.Delta{Kind: ir.DeltaText, Text: "Hel"}},
		{Kind: ir.EvDelta, Index: 2, Delta: &ir.Delta{Kind: ir.DeltaText, Text: "lo"}},
		{Kind: ir.EvBlockStop, Index: 2},
		{Kind: ir.EvBlockStart, Index: 3, Block: &ir.Block{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "toolu_1", Name: "shell", Kind: ir.ToolFunction}}},
		{Kind: ir.EvDelta, Index: 3, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: `{"cmd":`}},
		{Kind: ir.EvDelta, Index: 3, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: `"ls"}`}},
		{Kind: ir.EvBlockStop, Index: 3},
		{Kind: ir.EvBlockStart, Index: 4, Block: &ir.Block{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "toolu_2", Name: "apply_patch", Kind: ir.ToolFreeform, Text: "*** Begin Patch\n"}}},
		{Kind: ir.EvBlockStop, Index: 4},
		{Kind: ir.EvUsage, Usage: &ir.Usage{Input: 10, CacheRead: 90, CacheWrite5m: 5, Output: 20, Reasoning: 7}},
		{Kind: ir.EvDone, StopReason: ir.StopToolUse},
		{Kind: ir.EvBlockStart, Index: 5, Block: &ir.Block{Type: ir.BlockText}},
	}
	out := encodeEvents(t, &ir.Request{Model: "alias"}, evs)
	wire := parseWire(t, out)
	if wire[0].name != "response.created" {
		t.Fatalf("ping before start must not be written, got %q", wire[0].name)
	}
	var names []string
	for _, w := range wire {
		names = append(names, w.name)
	}
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done",
		"response.output_item.added", "response.output_item.done",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.output_item.added", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", "response.output_item.done",
		"response.completed",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}
	resp := wire[len(wire)-1].data.Get("response")
	out0 := resp.Get("output").Array()
	if len(out0) != 5 {
		t.Fatalf("output %s", resp.Get("output").Raw)
	}
	prefixes := []string{"rs_", "rs_", "msg_", "fc_", "ctc_"}
	for i, it := range out0 {
		if !strings.HasPrefix(it.Get("id").Str, prefixes[i]) {
			t.Fatalf("item %d id %q", i, it.Get("id").Str)
		}
	}
	if out0[0].Get("encrypted_content").Raw != seal || out0[0].Get("summary.0.text").Str != "think" {
		t.Fatalf("sealed thinking: %s", out0[0].Raw)
	}
	if out0[1].Get("encrypted_content").Raw != seal || len(out0[1].Get("summary").Array()) != 0 {
		t.Fatalf("sealed opaque: %s", out0[1].Raw)
	}
	if out0[2].Get("content.0.text").Str != "Hello" || out0[3].Get("arguments").Str != `{"cmd":"ls"}` ||
		out0[3].Get("call_id").Str != "toolu_1" || out0[4].Get("input").Str != "*** Begin Patch\n" {
		t.Fatalf("output %s", resp.Get("output").Raw)
	}
	u := resp.Get("usage")
	if u.Get("input_tokens").Int() != 105 || u.Get("input_tokens_details.cached_tokens").Int() != 90 ||
		u.Get("output_tokens").Int() != 20 || u.Get("output_tokens_details.reasoning_tokens").Int() != 7 || u.Get("total_tokens").Int() != 125 {
		t.Fatalf("usage %s", u.Raw)
	}
	if resp.Get("model").Str != "alias" || resp.Get("status").Str != "completed" {
		t.Fatalf("response %s", resp.Raw)
	}
	// Stable ids: the same item id appears in added and done.
	if wire[2].data.Get("item.id").Str != out0[0].Get("id").Str {
		t.Fatal("reasoning id changed between added and done")
	}
}

func TestStreamEncodeIncompleteAndError(t *testing.T) {
	out := encodeEvents(t, nil, []ir.Event{
		{Kind: ir.EvStart, ID: "resp_x"},
		{Kind: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaText, Text: "cut"}},
		{Kind: ir.EvDone, StopReason: ir.StopMaxTokens, Usage: &ir.Usage{Input: 1, Output: 2}},
	})
	wire := parseWire(t, out)
	last := wire[len(wire)-1]
	if last.name != "response.incomplete" || last.data.Get("response.incomplete_details.reason").Str != "max_output_tokens" ||
		last.data.Get("response.usage.output_tokens").Int() != 2 || last.data.Get("response.output.0.content.0.text").Str != "cut" {
		t.Fatalf("got %s %s", last.name, last.data.Raw)
	}

	var buf bytes.Buffer
	enc := Codec{}.NewStreamEncoder(sse.NewWriter(&buf), &ir.Request{Model: "m"})
	_ = enc.Encode(&ir.Event{Kind: ir.EvError, Err: &ir.GwError{Kind: ir.ErrQuotaExhausted, Message: "quota"}})
	_ = enc.Encode(&ir.Event{Kind: ir.EvDone})
	if err := enc.Keepalive(); err != nil {
		t.Fatal(err)
	}
	wire = parseWire(t, buf.Bytes())
	names := []string{}
	for _, w := range wire {
		names = append(names, w.name)
	}
	if strings.Join(names, ",") != "response.created,response.in_progress,response.failed,:" {
		t.Fatalf("events %v", names)
	}
	f := wire[2].data.Get("response")
	if f.Get("status").Str != "failed" || f.Get("error.code").Str != codeQuota || f.Get("error.message").Str != "quota" {
		t.Fatalf("failed %s", f.Raw)
	}
}
