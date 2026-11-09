package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/sse"
)

var update = flag.Bool("update", false, "rewrite golden files")

func decodeStream(t *testing.T, raw []byte) ([]ir.Event, bool, ir.Usage) {
	t.Helper()
	d := c.NewStreamDecoder()
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
	return append(evs, tail...), complete, d.Usage()
}

func TestStreamDecode(t *testing.T) {
	evs, complete, usage := decodeStream(t, fixture(t, "stream_thinking_tool.sse"))
	if !complete {
		t.Fatal("stream not complete")
	}
	var kinds []string
	var thinking, text, input, sig strings.Builder
	for _, e := range evs {
		kinds = append(kinds, string(e.Kind))
		if e.Kind != ir.EvDelta {
			continue
		}
		switch e.Delta.Kind {
		case ir.DeltaThinking:
			thinking.WriteString(e.Delta.Text)
		case ir.DeltaText:
			text.WriteString(e.Delta.Text)
		case ir.DeltaToolInput:
			input.WriteString(e.Delta.Text)
		case ir.DeltaOpaque:
			if e.Delta.Opaque.Kind != "signature" || e.Index != 0 {
				t.Errorf("opaque delta = %+v", e)
			}
			sig.Write(e.Delta.Opaque.Data)
		}
	}
	want := "ping start usage block_start delta delta delta block_stop block_start delta block_stop ping " +
		"block_start delta delta delta block_stop usage done"
	if got := strings.Join(kinds, " "); got != want {
		t.Errorf("events\n got %s\nwant %s", got, want)
	}
	if thinking.String() != "Let me check the files." || text.String() != "Listing files." ||
		input.String() != `{"command": "ls pkg/ir"}` || sig.String() != `"EqQBCkYIBRgC"` {
		t.Errorf("content: %q %q %q %q", thinking.String(), text.String(), input.String(), sig.String())
	}
	if evs[1].ID != "msg_01XYZ" || evs[1].Model != "claude-opus-4-6" {
		t.Errorf("start = %+v", evs[1])
	}
	if b := evs[3].Block; b.Type != ir.BlockThinking || b.Opaque != nil {
		t.Errorf("thinking start = %+v", b)
	}
	if b := evs[12].Block; b.Type != ir.BlockToolUse || b.Tool.ID != "toolu_01A" || b.Tool.Input != nil {
		t.Errorf("tool start = %+v", b.Tool)
	}
	done := evs[len(evs)-1]
	if done.StopReason != ir.StopToolUse {
		t.Errorf("stop = %q", done.StopReason)
	}
	if usage.Input != 12 || usage.CacheRead != 30000 || usage.CacheWrite5m != 48 || usage.CacheWrite1h != 2000 || usage.Output != 87 {
		t.Errorf("usage = %+v", usage)
	}
	if !strings.Contains(string(usage.Raw), `"output_tokens":87`) || !strings.Contains(string(usage.Raw), `"service_tier"`) {
		t.Errorf("raw usage = %s", usage.Raw)
	}
}

func TestStreamDecodeIncomplete(t *testing.T) {
	raw := fixture(t, "stream_thinking_tool.sse")
	cut := raw[:bytes.Index(raw, []byte("event: message_delta"))]
	if _, complete, _ := decodeStream(t, cut); complete {
		t.Error("cut stream reported complete")
	}
}

func TestStreamDecodeError(t *testing.T) {
	evs, complete, _ := decodeStream(t, []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n"+
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"))
	last := evs[len(evs)-1]
	if complete || last.Kind != ir.EvError || last.Err.Kind != ir.ErrOverloaded || last.Err.Message != "Overloaded" {
		t.Errorf("error event = %+v complete=%v", last, complete)
	}
}

func encodeStream(t *testing.T, req *ir.Request, evs []ir.Event, keepaliveFirst bool) string {
	t.Helper()
	var buf bytes.Buffer
	enc := c.NewStreamEncoder(sse.NewWriter(&buf), req)
	if keepaliveFirst {
		if err := enc.Keepalive(); err != nil {
			t.Fatal(err)
		}
	}
	for i := range evs {
		if err := enc.Encode(&evs[i]); err != nil {
			t.Fatal(err)
		}
	}
	return buf.String()
}

// TestStreamEncodeGolden re-encodes the decoded fixture; the golden file is
// the exact client-facing byte sequence.
func TestStreamEncodeGolden(t *testing.T) {
	evs, _, _ := decodeStream(t, fixture(t, "stream_thinking_tool.sse"))
	got := encodeStream(t, &ir.Request{Model: "claude-opus-4-6"}, evs, false)
	path := filepath.Join("testdata", "stream_encode.golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := string(fixture(t, "stream_encode.golden")); got != want {
		t.Errorf("stream differs from golden\n got:\n%s\nwant:\n%s", got, want)
	}
	// The re-encoded stream decodes to the same events.
	again, complete, _ := decodeStream(t, []byte(got))
	if !complete || len(again) != len(evs) {
		t.Errorf("re-decode: %d events (want %d), complete=%v", len(again), len(evs), complete)
	}
}

func eventNames(stream string) []string {
	var names []string
	for _, line := range strings.Split(stream, "\n") {
		if n, ok := strings.CutPrefix(line, "event: "); ok {
			names = append(names, n)
		}
	}
	return names
}

func TestStreamEncodeSequences(t *testing.T) {
	sealed := &ir.OpaqueState{Kind: "encrypted_content", Data: json.RawMessage(`"prs1.s"`), Sealed: true}
	foreign := &ir.OpaqueState{Kind: "encrypted_content", Data: json.RawMessage(`"x"`)}
	cases := []struct {
		name      string
		keepalive bool
		evs       []ir.Event
		want      string
		contains  []string
	}{
		{
			name:      "ping before start, done closes open block",
			keepalive: true,
			evs: []ir.Event{
				{Kind: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText, Text: "hi"}},
				{Kind: ir.EvUsage, Usage: &ir.Usage{Input: 4, Output: 9}},
				{Kind: ir.EvDone, StopReason: ir.StopMaxTokens},
				{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaText, Text: "late"}},
			},
			want:     "ping message_start content_block_start content_block_delta content_block_stop message_delta message_stop",
			contains: []string{`"stop_reason":"max_tokens"`, `"output_tokens":9`, `"text":"hi"`},
		},
		{
			name: "foreign opaque skipped, sealed signature at stop, indexes dense",
			evs: []ir.Event{
				{Kind: ir.EvStart, ID: "resp_1", Model: "gpt-x"},
				{Kind: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockOpaque, Opaque: foreign}},
				{Kind: ir.EvBlockStop, Index: 0},
				{Kind: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockThinking}},
				{Kind: ir.EvDelta, Index: 1, Delta: &ir.Delta{Kind: ir.DeltaThinking, Text: "r"}},
				{Kind: ir.EvBlockStop, Index: 1, Block: &ir.Block{Type: ir.BlockThinking, Opaque: sealed}},
				{Kind: ir.EvBlockStart, Index: 2, Block: &ir.Block{Type: ir.BlockOpaque, Opaque: sealed}},
				{Kind: ir.EvBlockStop, Index: 2},
				{Kind: ir.EvDone},
			},
			want: "message_start content_block_start content_block_delta content_block_delta content_block_stop " +
				"content_block_start content_block_stop message_delta message_stop",
			contains: []string{`"id":"resp_1"`, `"index":0,"content_block":{"type":"thinking"`,
				`"signature_delta","signature":"prs1.s"`, `"index":1,"content_block":{"type":"redacted_thinking","data":"prs1.s"}`,
				`"stop_reason":"end_turn"`},
		},
		{
			name: "error ends the stream",
			evs: []ir.Event{
				{Kind: ir.EvStart},
				{Kind: ir.EvError, Err: &ir.GwError{Kind: ir.ErrOverloaded, Message: "busy"}},
				{Kind: ir.EvDone},
			},
			want:     "message_start error",
			contains: []string{`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`},
		},
		{
			name: "tool input known up front",
			evs: []ir.Event{
				{Kind: ir.EvBlockStart, Block: &ir.Block{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "c1", Name: "f", Input: json.RawMessage(`{"a":1}`)}}},
				{Kind: ir.EvBlockStop},
				{Kind: ir.EvDone, StopReason: ir.StopToolUse},
			},
			want:     "message_start content_block_start content_block_delta content_block_stop message_delta message_stop",
			contains: []string{`"input":{}`, `"partial_json":"{\"a\":1}"`, `"model":"req-model"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeStream(t, &ir.Request{Model: "req-model"}, tc.evs, tc.keepalive)
			if names := strings.Join(eventNames(got), " "); names != tc.want {
				t.Errorf("events\n got %s\nwant %s", names, tc.want)
			}
			for _, s := range tc.contains {
				if !strings.Contains(got, s) {
					t.Errorf("missing %s in\n%s", s, got)
				}
			}
		})
	}
}
