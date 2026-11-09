package openaicompletions

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/sse"
	"github.com/tidwall/gjson"
)

var codec = Codec{}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(b)
}

func jsonEqual(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got %s: %v", got, err)
	}
	if !reflect.DeepEqual(w, g) {
		t.Errorf("JSON differs\nwant %s\ngot  %s", want, got)
	}
}

func decode(t *testing.T, body []byte) (*ir.Request, ir.Needs) {
	t.Helper()
	req, needs, err := codec.DecodeRequest(nil, protocol.Inbound{}, body)
	if err != nil {
		t.Fatal(err)
	}
	return req, needs
}

func TestRegistered(t *testing.T) {
	c, ok := protocol.Get(Name)
	if !ok || c.Name() != Name {
		t.Fatal("codec not registered")
	}
	caps := c.Capabilities()
	if !caps.PromptOnly || !caps.FillInMiddle || caps.FunctionTools || caps.MaxTokensField != "max_tokens" {
		t.Errorf("capabilities %+v", caps)
	}
	if rs := c.Routes(); len(rs) != 1 || rs[0].Path != "/v1/completions" || rs[0].Op != protocol.OpGenerate {
		t.Errorf("routes %+v", rs)
	}
	if p := c.UpstreamPath(nil, protocol.Inbound{}); p != "/completions" {
		t.Errorf("upstream path %q", p)
	}
}

func TestSniff(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/completions", nil)
	r.Header.Set("x-session-id", "s1")
	in, err := codec.Sniff(r, codec.Routes()[0], readFixture(t, "request_fim.json"))
	if err != nil {
		t.Fatal(err)
	}
	if in.Model != "deepseek-chat" || !in.Stream || in.Session.SessionID != "s1" || in.Session.UserTag != "dev-1" {
		t.Errorf("inbound %+v", in)
	}
	if _, err := codec.Sniff(r, codec.Routes()[0], []byte("{")); err == nil {
		t.Error("invalid JSON accepted")
	}
}

// A Continue-style FIM request decodes to one user prompt with the suffix
// as a need, and encodes back to the same JSON.
func TestDecodeFIMRoundTrip(t *testing.T) {
	body := readFixture(t, "request_fim.json")
	req, needs := decode(t, body)
	if len(req.Messages) != 1 || req.Messages[0].Role != ir.RoleUser || req.Messages[0].Content[0].Text != "def fib(n):\n    " {
		t.Fatalf("messages %+v", req.Messages)
	}
	if !needs.FillInMiddle || needs.InputTokensEst == 0 {
		t.Errorf("needs %+v", needs)
	}
	if *req.MaxOutputTokens != 256 || *req.Temperature != 0.01 || !req.Stream || len(req.Stop) != 2 {
		t.Errorf("params %+v", req)
	}
	out, err := codec.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, body, out)
}

func TestDecodePromptShapes(t *testing.T) {
	req, needs := decode(t, []byte(`{"model":"m","prompt":["hi"],"stop":"\n","suffix":""}`))
	if req.Messages[0].Content[0].Text != "hi" || needs.FillInMiddle {
		t.Errorf("one-element prompt list: %+v %+v", req.Messages, needs)
	}
	out, err := codec.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, []byte(`{"model":"m","prompt":["hi"],"stop":"\n","suffix":""}`), out)

	req, _ = decode(t, []byte(`{"model":"m"}`))
	if len(req.Messages) != 1 || req.Messages[0].Content[0].Text != "" {
		t.Errorf("missing prompt: %+v", req.Messages)
	}
}

// Requests with no faithful IR form fail to decode, which leaves them to
// same-protocol passthrough.
func TestDecodeSameProtocolOnly(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","prompt":["a","b"]}`,
		`{"model":"m","prompt":[1,2,3]}`,
		`{"model":"m","prompt":[[1,2]]}`,
		`{"model":"m","prompt":"a","n":2}`,
		`{"model":"m","prompt":"a","best_of":3}`,
		`{"model":"m","prompt":"a","echo":true}`,
	} {
		_, _, err := codec.DecodeRequest(nil, protocol.Inbound{}, []byte(body))
		var gw *ir.GwError
		if !errors.As(err, &gw) || gw.Kind != ir.ErrInvalidRequest || !strings.Contains(gw.Message, "openai-completions") {
			t.Errorf("%s: err %v", body, err)
		}
	}
	for _, body := range []string{`{"model":"m","prompt":"a","n":1,"echo":false}`, `{"model":"m","prompt":"a","n":null}`} {
		if _, _, err := codec.DecodeRequest(nil, protocol.Inbound{}, []byte(body)); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
	if _, _, err := codec.DecodeRequest(nil, protocol.Inbound{}, []byte(`{"model":"m","prompt":{}}`)); err == nil {
		t.Error("object prompt accepted")
	}
}

// IR from another protocol encodes as a prompt only when it is one user
// text message; anything else is refused, never flattened.
func TestEncodeFromIR(t *testing.T) {
	n := 64
	req := &ir.Request{Model: "m", Stream: true, MaxOutputTokens: &n,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "a"}, {Type: ir.BlockText, Text: "b"}}}}}
	out, err := codec.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, []byte(`{"model":"m","prompt":"ab","max_tokens":64,"stream":true,"stream_options":{"include_usage":true}}`), out)

	for name, r := range map[string]*ir.Request{
		"system":    {Model: "m", System: []ir.Block{{Type: ir.BlockText, Text: "s"}}, Messages: req.Messages},
		"two turns": {Model: "m", Messages: append(append([]ir.Message{}, req.Messages...), ir.Message{Role: ir.RoleAssistant})},
		"image":     {Model: "m", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockImage}}}}},
		"tools":     {Model: "m", Messages: req.Messages, Tools: []ir.Tool{{Name: "t"}}},
	} {
		if _, err := codec.EncodeRequest(r); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}
}

func TestSetModelByteExact(t *testing.T) {
	body := readFixture(t, "request_fim.json")
	out, err := codec.SetModel(body, "deepseek-v4")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(string(body), `"deepseek-chat"`, `"deepseek-v4"`, 1)
	if string(out) != want {
		t.Errorf("SetModel changed more than the model:\n%s", out)
	}
	if o, n, err := codec.StripOpaque(body); err != nil || n != 0 || !bytes.Equal(o, body) {
		t.Error("StripOpaque changed the body")
	}
}

// logprobs and unknown fields survive a same-protocol response round trip.
func TestResponseRoundTrip(t *testing.T) {
	body := readFixture(t, "response_logprobs.json")
	resp, err := codec.DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "cmpl-9f2" || resp.StopReason != ir.StopMaxTokens || len(resp.Content) != 1 || resp.Content[0].Text != " world" {
		t.Fatalf("resp %+v", resp)
	}
	if resp.Usage.Input != 5 || resp.Usage.Output != 1 {
		t.Errorf("usage %+v", resp.Usage)
	}
	out, err := codec.EncodeResponse(nil, resp)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, body, out)
}

// A response from another protocol renders as a text_completion: visible
// text only, reasoning dropped, tool-free.
func TestEncodeResponseFromIR(t *testing.T) {
	out, err := codec.EncodeResponse(nil, &ir.Response{ID: "msg_1", Model: "claude", StopReason: ir.StopEndTurn,
		Content: []ir.Block{{Type: ir.BlockThinking, Text: "hmm"}, {Type: ir.BlockText, Text: "return n"}},
		Usage:   ir.Usage{Input: 3, CacheRead: 2, Output: 4}})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"object": "text_completion", "id": "msg_1", "choices.0.text": "return n", "choices.0.finish_reason": "stop",
		"choices.0.logprobs": "", "usage.prompt_tokens": "5", "usage.prompt_tokens_details.cached_tokens": "2", "usage.total_tokens": "9",
	} {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestDecodeResponseError(t *testing.T) {
	_, err := codec.DecodeResponse([]byte(`{"error":{"message":"bad","type":"invalid_request_error","code":null}}`))
	var gw *ir.GwError
	if !errors.As(err, &gw) || gw.Message != "bad" {
		t.Errorf("err %v", err)
	}
}

func decodeStream(t *testing.T, raw []byte) ([]ir.Event, bool) {
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
	return append(evs, tail...), complete
}

func TestStreamDecode(t *testing.T) {
	evs, complete := decodeStream(t, readFixture(t, "stream_usage.sse"))
	if !complete {
		t.Fatal("stream not complete")
	}
	var kinds []ir.EventKind
	text := ""
	var done *ir.Event
	for i, ev := range evs {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == ir.EvDelta {
			text += ev.Delta.Text
		}
		if ev.Kind == ir.EvDone {
			done = &evs[i]
		}
	}
	want := []ir.EventKind{ir.EvStart, ir.EvBlockStart, ir.EvDelta, ir.EvDelta, ir.EvUsage, ir.EvBlockStop, ir.EvDone}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("events %v", kinds)
	}
	if evs[0].ID != "cmpl-7a" || evs[0].Model != "deepseek-chat" || text != "if n < 2:\n        return n" {
		t.Errorf("start %+v text %q", evs[0], text)
	}
	if done == nil || done.StopReason != ir.StopEndTurn || done.Usage == nil || done.Usage.Input != 4 || done.Usage.CacheRead != 16 || done.Usage.Output != 9 {
		t.Errorf("done %+v", done)
	}
}

func TestStreamDecodeIncomplete(t *testing.T) {
	raw := `data: {"id":"c","choices":[{"text":"a","index":0,"finish_reason":null}]}` + "\n\n"
	if _, complete := decodeStream(t, []byte(raw)); complete {
		t.Error("cut stream reported complete")
	}
	raw = `data: {"error":{"message":"overloaded","type":"server_error"}}` + "\n\n"
	evs, _ := decodeStream(t, []byte(raw))
	if len(evs) != 1 || evs[0].Kind != ir.EvError || evs[0].Err.Message != "overloaded" {
		t.Errorf("error chunk: %+v", evs)
	}
}

func encodeStream(t *testing.T, req *ir.Request, evs []ir.Event) string {
	t.Helper()
	var buf bytes.Buffer
	e := codec.NewStreamEncoder(sse.NewWriter(&buf), req)
	for i := range evs {
		if err := e.Encode(&evs[i]); err != nil {
			t.Fatal(err)
		}
	}
	return buf.String()
}

// The encoder replays a decoded stream as the client's chunks, with the
// usage chunk only when stream_options.include_usage asked for it.
func TestStreamEncode(t *testing.T) {
	evs, _ := decodeStream(t, readFixture(t, "stream_usage.sse"))
	req, _ := decode(t, readFixture(t, "request_fim.json"))
	out := encodeStream(t, req, evs)
	var chunks []string
	for _, f := range strings.Split(strings.TrimSpace(out), "\n\n") {
		chunks = append(chunks, strings.TrimPrefix(f, "data: "))
	}
	if len(chunks) != 5 || chunks[4] != "[DONE]" {
		t.Fatalf("chunks:\n%s", out)
	}
	for i, c := range chunks[:4] {
		if gjson.Get(c, "object").String() != "text_completion" || gjson.Get(c, "id").String() != "cmpl-7a" || gjson.Get(c, "model").String() != "deepseek-chat" {
			t.Errorf("chunk %d: %s", i, c)
		}
	}
	if gjson.Get(chunks[0], "choices.0.text").String() != "if n < 2:" || gjson.Get(chunks[0], "choices.0.finish_reason").Raw != "null" {
		t.Errorf("first chunk %s", chunks[0])
	}
	if gjson.Get(chunks[2], "choices.0.finish_reason").String() != "stop" {
		t.Errorf("finish chunk %s", chunks[2])
	}
	if gjson.Get(chunks[3], "choices.#").Int() != 0 || gjson.Get(chunks[3], "usage.prompt_tokens").Int() != 20 || gjson.Get(chunks[3], "usage.completion_tokens").Int() != 9 {
		t.Errorf("usage chunk %s", chunks[3])
	}

	req.Extra.Fields["stream_options"] = json.RawMessage(`{"include_usage":false}`)
	out = encodeStream(t, req, evs)
	if strings.Contains(out, `"usage"`) || !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Errorf("usage chunk without include_usage:\n%s", out)
	}
}

func TestStreamEncodeError(t *testing.T) {
	out := encodeStream(t, nil, []ir.Event{
		{Kind: ir.EvStart, ID: "x", Model: "m"},
		{Kind: ir.EvBlockStart, Block: &ir.Block{Type: ir.BlockText}},
		{Kind: ir.EvDelta, Delta: &ir.Delta{Kind: ir.DeltaText, Text: "a"}},
		{Kind: ir.EvError, Err: ir.Errorf(ir.ErrOverloaded, "upstream overloaded")},
		{Kind: ir.EvDone},
	})
	if !strings.Contains(out, `"error":{"message":"upstream overloaded"`) || strings.Contains(out, "[DONE]") {
		t.Errorf("stream:\n%s", out)
	}
}

func TestEncodeError(t *testing.T) {
	status, h, body := codec.EncodeError(ir.Errorf(ir.ErrCapabilityMissing, "no endpoint supports fill_in_middle"))
	if status != 400 || h.Get("Content-Type") != "application/json" || gjson.GetBytes(body, "error.message").String() != "no endpoint supports fill_in_middle" {
		t.Errorf("%d %v %s", status, h, body)
	}
}
