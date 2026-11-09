package openaichat

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/sse"
)

// assembled is a stream folded back into a response, checked against the
// event contract on the way.
type assembled struct {
	id, model string
	blocks    []ir.Block
	stop      ir.StopReason
	usage     *ir.Usage
	err       *ir.GwError
	complete  bool
}

func assemble(t *testing.T, evs []ir.Event) assembled {
	t.Helper()
	var a assembled
	open := map[int]bool{}
	stopped := map[int]bool{}
	for _, ev := range evs {
		switch ev.Kind {
		case ir.EvStart:
			a.id, a.model = ev.ID, ev.Model
		case ir.EvBlockStart:
			if ev.Index != len(a.blocks) {
				t.Fatalf("block index %d not dense (have %d)", ev.Index, len(a.blocks))
			}
			a.blocks = append(a.blocks, *ev.Block)
			if ev.Block.Tool != nil {
				tc := *ev.Block.Tool
				a.blocks[ev.Index].Tool = &tc
			}
			open[ev.Index] = true
		case ir.EvDelta:
			if !open[ev.Index] {
				t.Fatalf("delta for block %d that is not open", ev.Index)
			}
			b := &a.blocks[ev.Index]
			switch ev.Delta.Kind {
			case ir.DeltaToolInput:
				if b.Tool.Kind == ir.ToolFreeform {
					b.Tool.Text += ev.Delta.Text
				} else {
					b.Tool.Input = append(b.Tool.Input, ev.Delta.Text...)
				}
			default:
				b.Text += ev.Delta.Text
			}
		case ir.EvBlockStop:
			if !open[ev.Index] || stopped[ev.Index] {
				t.Fatalf("bad stop for block %d", ev.Index)
			}
			open[ev.Index], stopped[ev.Index] = false, true
		case ir.EvUsage:
			u := *ev.Usage
			a.usage = &u
		case ir.EvDone:
			a.stop = ev.StopReason
			if ev.Usage != nil {
				u := *ev.Usage
				a.usage = &u
			}
		case ir.EvError:
			a.err = ev.Err
		}
	}
	for i, o := range open {
		if o && a.err == nil {
			t.Fatalf("block %d never stopped", i)
		}
	}
	return a
}

func decodeStream(t *testing.T, raw []byte) assembled {
	t.Helper()
	d := codec.NewStreamDecoder()
	rd := sse.NewReader(bytes.NewReader(raw), 0)
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
	evs = append(evs, tail...)
	a := assemble(t, evs)
	a.complete = complete
	return a
}

type toolWant struct{ name, args string }

func TestStreamDecodeFixtures(t *testing.T) {
	tests := []struct {
		file      string
		types     []ir.BlockType
		text      string
		tools     []toolWant
		stop      ir.StopReason
		input     int64
		output    int64
		reasoning int64
	}{
		{"stream_reasoning_usage.sse", []ir.BlockType{ir.BlockThinking, ir.BlockText}, "391", nil, ir.StopEndTurn, 40, 17, 14},
		{"stream_parallel_tools.sse", []ir.BlockType{ir.BlockToolUse, ir.BlockToolUse}, "", []toolWant{
			{"get_weather", `{"city":"Paris"}`}, {"get_weather", `{"city":"Tokyo"}`}}, ir.StopToolUse, 57, 48, 0},
		{"stream_reasoning_tools.sse", []ir.BlockType{ir.BlockThinking, ir.BlockToolUse}, "", []toolWant{{"get_weather", `{"city": "Berlin"}`}}, ir.StopToolUse, 390, 135, 102},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			a := decodeStream(t, readFixture(t, tt.file))
			if !a.complete || a.stop != tt.stop {
				t.Errorf("complete=%v stop=%s", a.complete, a.stop)
			}
			if a.id == "" || a.model == "" {
				t.Error("EvStart without id/model")
			}
			if tt.types != nil {
				var got []ir.BlockType
				for _, b := range a.blocks {
					got = append(got, b.Type)
				}
				if !equalTypes(got, tt.types) {
					t.Errorf("blocks %v want %v", got, tt.types)
				}
			}
			var text string
			var tools []toolWant
			for _, b := range a.blocks {
				switch b.Type {
				case ir.BlockText:
					text += b.Text
				case ir.BlockThinking:
					if b.Text == "" {
						t.Error("empty thinking block")
					}
				case ir.BlockToolUse:
					if b.Tool.ID == "" {
						t.Error("tool call without id")
					}
					tools = append(tools, toolWant{b.Tool.Name, string(b.Tool.Input)})
				}
			}
			if text != tt.text {
				t.Errorf("text %q want %q", text, tt.text)
			}
			if len(tools) != len(tt.tools) {
				t.Fatalf("tools %v want %v", tools, tt.tools)
			}
			for i := range tools {
				if tools[i] != tt.tools[i] {
					t.Errorf("tool %d = %v want %v", i, tools[i], tt.tools[i])
				}
			}
			if a.usage == nil || a.usage.Input != tt.input || a.usage.Output != tt.output || a.usage.Reasoning != tt.reasoning || len(a.usage.Raw) == 0 {
				t.Errorf("usage = %+v", a.usage)
			}
		})
	}
}

func equalTypes(a, b []ir.BlockType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func frames(chunks ...string) []byte {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	return []byte(b.String())
}

func TestStreamParallelFragments(t *testing.T) {
	raw := frames(
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"plan"}}]}`,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"f","arguments":"{\"x\""}}]}}]}`,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"g","arguments":""}}]}}]}`,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]}}]}`,
		// Same wire index, new id: a separate call, as some upstreams send.
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c3","type":"custom","custom":{"name":"p","input":"raw"}}]}}]}`,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	)
	a := decodeStream(t, raw)
	if len(a.blocks) != 4 || a.blocks[0].Type != ir.BlockThinking {
		t.Fatalf("blocks = %+v", a.blocks)
	}
	if string(a.blocks[1].Tool.Input) != `{"x":1}` || a.blocks[1].Tool.ID != "a" {
		t.Errorf("tool 0 = %+v", a.blocks[1].Tool)
	}
	if string(a.blocks[2].Tool.Input) != `{}` || a.blocks[2].Tool.Name != "g" {
		t.Errorf("tool 1 = %+v", a.blocks[2].Tool)
	}
	if a.blocks[3].Tool.Kind != ir.ToolFreeform || a.blocks[3].Tool.Text != "raw" {
		t.Errorf("custom = %+v", a.blocks[3].Tool)
	}
}

func TestStreamUsageAfterFinish(t *testing.T) {
	finish := `{"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"length"}]}`
	usage := `{"id":"c","model":"m","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":60}}}`
	tests := []struct {
		name     string
		raw      []byte
		complete bool
		usage    bool
	}{
		{"usage then DONE", frames(finish, usage, "[DONE]"), true, true},
		{"usage, no DONE", frames(finish, usage), true, true},
		{"finish only", frames(finish), true, false},
		{"cut before finish", frames(`{"id":"c","choices":[{"index":0,"delta":{"content":"hi"}}]}`), false, false},
		{"DONE without finish", frames(`{"id":"c","choices":[{"index":0,"delta":{"content":"hi"}}]}`, "[DONE]"), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := codec.NewStreamDecoder()
			rd := sse.NewReader(bytes.NewReader(tt.raw), 0)
			var evs []ir.Event
			for {
				ev, err := rd.Next()
				if err != nil {
					break
				}
				out, _ := d.Feed(ev)
				evs = append(evs, out...)
			}
			tail, complete := d.Finish()
			evs = append(evs, tail...)
			if complete != tt.complete {
				t.Fatalf("complete = %v", complete)
			}
			var done *ir.Event
			for i := range evs {
				if evs[i].Kind == ir.EvDone {
					if done != nil {
						t.Fatal("two EvDone")
					}
					done = &evs[i]
				}
			}
			if tt.complete != (done != nil) {
				t.Fatalf("EvDone present = %v", done != nil)
			}
			if tt.usage {
				if done.Usage == nil || done.Usage.Input != 40 || done.Usage.CacheRead != 60 || done.Usage.Output != 5 {
					t.Errorf("done usage = %+v", done.Usage)
				}
				if d.Usage().TotalInput() != 100 {
					t.Error("decoder Usage()")
				}
			}
			if !tt.complete {
				// Blocks may stay open on a cut stream; the pipeline ends it with an error.
				return
			}
			assemble(t, evs)
		})
	}
}

func TestStreamDecodeError(t *testing.T) {
	a := decodeStream(t, frames(
		`{"id":"c","choices":[{"index":0,"delta":{"content":"x"}}]}`,
		`{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`,
		`{"id":"c","choices":[{"index":0,"delta":{"content":"ignored"}}]}`,
	))
	if a.err == nil || a.err.Kind != ir.ErrRateLimited || a.complete == false {
		t.Errorf("err = %+v complete=%v", a.err, a.complete)
	}
}

// recorder is an sse.Writer that keeps frames.
type recorder struct {
	data     []string
	comments []string
}

func (r *recorder) Event(name string, data []byte) error {
	r.data = append(r.data, string(data))
	return nil
}
func (r *recorder) Comment(text string) error { r.comments = append(r.comments, text); return nil }
func (r *recorder) Raw(p []byte) error        { return nil }

func (r *recorder) bytes() []byte {
	var b strings.Builder
	for _, d := range r.data {
		b.WriteString("data: " + d + "\n\n")
	}
	return []byte(b.String())
}

func streamEvents() []ir.Event {
	u := ir.Usage{Input: 30, CacheRead: 70, Output: 12, Reasoning: 4}
	return []ir.Event{
		{Kind: ir.EvStart, ID: "msg_1", Model: "up-model"},
		{Kind: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking}},
		{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaThinking, Text: "hmm"}},
		{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaOpaque, Opaque: &ir.OpaqueState{Kind: "signature"}}},
		{Kind: ir.EvBlockStop, Index: 0},
		{Kind: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockText}},
		{Kind: ir.EvDelta, Index: 1, Delta: &ir.Delta{Kind: ir.DeltaText, Text: "Hello"}},
		{Kind: ir.EvBlockStop, Index: 1},
		{Kind: ir.EvBlockStart, Index: 2, Block: &ir.Block{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "t1", Name: "f", Kind: ir.ToolFunction}}},
		{Kind: ir.EvDelta, Index: 2, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: `{"a"`}},
		{Kind: ir.EvDelta, Index: 2, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: `:1}`}},
		{Kind: ir.EvBlockStop, Index: 2},
		{Kind: ir.EvBlockStart, Index: 3, Block: &ir.Block{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "t2", Name: "p", Kind: ir.ToolFreeform}}},
		{Kind: ir.EvDelta, Index: 3, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: "patch"}},
		{Kind: ir.EvBlockStop, Index: 3},
		{Kind: ir.EvUsage, Usage: &u},
		{Kind: ir.EvDone, StopReason: ir.StopToolUse},
	}
}

func TestStreamEncode(t *testing.T) {
	tests := []struct {
		name         string
		req          string
		includeUsage bool
		customShape  bool
	}{
		{"include_usage + custom tools", `{"model":"alias","stream":true,"stream_options":{"include_usage":true},"messages":[],"tools":[{"type":"custom","custom":{"name":"p"}}]}`, true, true},
		{"no usage, no custom tools", `{"model":"alias","stream":true,"messages":[]}`, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := decode(t, []byte(tt.req))
			rec := &recorder{}
			enc := codec.NewStreamEncoder(rec, req)
			for _, ev := range streamEvents() {
				if err := enc.Encode(&ev); err != nil {
					t.Fatal(err)
				}
			}
			if err := enc.Encode(&ir.Event{Kind: ir.EvDelta, Index: 1, Delta: &ir.Delta{Kind: ir.DeltaText, Text: "late"}}); err != nil {
				t.Fatal(err)
			}
			if enc.Usage().CacheRead != 70 {
				t.Error("encoder Usage()")
			}
			if rec.data[len(rec.data)-1] != "[DONE]" {
				t.Fatalf("last frame %q", rec.data[len(rec.data)-1])
			}
			ids := map[string]bool{}
			var created []float64
			for _, d := range rec.data[:len(rec.data)-1] {
				var c map[string]any
				if err := json.Unmarshal([]byte(d), &c); err != nil {
					t.Fatal(err)
				}
				ids[c["id"].(string)] = true
				created = append(created, c["created"].(float64))
				if c["object"] != "chat.completion.chunk" || c["model"] != "up-model" {
					t.Errorf("chunk header %v", c)
				}
			}
			if len(ids) != 1 || !ids["msg_1"] || created[0] != created[len(created)-1] {
				t.Errorf("inconsistent ids %v", ids)
			}
			first := rec.data[0]
			if !strings.Contains(first, `"delta":{"role":"assistant","content":""}`) {
				t.Errorf("first chunk %s", first)
			}
			last := rec.data[len(rec.data)-2]
			if tt.includeUsage {
				want := `"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":12,"total_tokens":112,"prompt_tokens_details":{"cached_tokens":70},"completion_tokens_details":{"reasoning_tokens":4}}`
				if !strings.Contains(last, want) {
					t.Errorf("usage chunk %s", last)
				}
				last = rec.data[len(rec.data)-3]
			} else if strings.Contains(strings.Join(rec.data, ""), `"usage"`) {
				t.Error("usage chunk sent without include_usage")
			}
			if !strings.Contains(last, `"finish_reason":"tool_calls"`) {
				t.Errorf("finish chunk %s", last)
			}
			all := strings.Join(rec.data, "\n")
			if !strings.Contains(all, `"reasoning_content":"hmm"`) || strings.Contains(all, "late") {
				t.Error("reasoning missing or event after done written")
			}
			if tt.customShape != strings.Contains(all, `"type":"custom"`) {
				t.Errorf("custom shape = %v: %s", !tt.customShape, all)
			}

			// What we wrote decodes back to the same content.
			a := decodeStream(t, rec.bytes())
			if len(a.blocks) != 4 || a.blocks[1].Text != "Hello" || string(a.blocks[2].Tool.Input) != `{"a":1}` || a.stop != ir.StopToolUse {
				t.Errorf("re-decoded %+v", a)
			}
			if tt.customShape && a.blocks[3].Tool.Text != "patch" {
				t.Errorf("custom input %+v", a.blocks[3].Tool)
			}
			if tt.includeUsage && (a.usage == nil || a.usage.CacheRead != 70 || a.usage.Input != 30) {
				t.Errorf("re-decoded usage %+v", a.usage)
			}
		})
	}
}

func TestStreamEncodeKeepaliveAndError(t *testing.T) {
	rec := &recorder{}
	enc := codec.NewStreamEncoder(rec, &ir.Request{Model: "m"})
	if err := enc.Keepalive(); err != nil {
		t.Fatal(err)
	}
	_ = enc.Encode(&ir.Event{Kind: ir.EvPing})
	_ = enc.Encode(&ir.Event{Kind: ir.EvStatus, Status: "queued"})
	if len(rec.comments) != 2 || rec.comments[0] != "keepalive" || len(rec.data) != 0 {
		t.Fatalf("comments %v data %v", rec.comments, rec.data)
	}
	_ = enc.Encode(&ir.Event{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaText, Text: "x"}})
	_ = enc.Encode(&ir.Event{Kind: ir.EvError, Err: ir.Errorf(ir.ErrOverloaded, "busy")})
	_ = enc.Encode(&ir.Event{Kind: ir.EvDone})
	if len(rec.data) != 3 {
		t.Fatalf("frames %v", rec.data)
	}
	jsonEqual(t, []byte(`{"error":{"message":"busy","type":"server_error","param":null,"code":"server_is_overloaded"}}`), []byte(rec.data[2]))
}

func TestStreamReasoningDetailsPassThrough(t *testing.T) {
	a := frames(`{"id":"c","model":"m","choices":[{"index":0,"delta":{"reasoning_details":[{"type":"reasoning.text","text":"abc"}]}}]}`, `[DONE]`)
	d := codec.NewStreamDecoder()
	rd := sse.NewReader(bytes.NewReader(a), 0)
	rec := &recorder{}
	enc := codec.NewStreamEncoder(rec, &ir.Request{})
	for {
		ev, err := rd.Next()
		if err != nil {
			break
		}
		evs, _ := d.Feed(ev)
		for i := range evs {
			_ = enc.Encode(&evs[i])
		}
	}
	all := strings.Join(rec.data, "\n")
	if !strings.Contains(all, `"reasoning_content":"abc","reasoning_details":[{"type":"reasoning.text","text":"abc"}]`) {
		t.Errorf("got %s", all)
	}
}
