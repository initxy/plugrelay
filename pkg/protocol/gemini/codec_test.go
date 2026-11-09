package gemini

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/sse"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func jsonEqual(t *testing.T, want, got []byte) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(want, &a); err != nil {
		t.Fatalf("want: %v", err)
	}
	if err := json.Unmarshal(got, &b); err != nil {
		t.Fatalf("got: %v\n%s", err, got)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("JSON differs\nwant: %s\ngot:  %s", compactJSON(want), got)
	}
}

func generatePost(path string, body []byte) *http.Request {
	return httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
}

func decode(t *testing.T, path string, body []byte) (*ir.Request, ir.Needs) {
	t.Helper()
	c := Codec{}
	in, err := c.Sniff(generatePost(path, body), c.Routes()[0], body)
	if err != nil {
		t.Fatal(err)
	}
	req, needs, err := c.DecodeRequest(nil, in, body)
	if err != nil {
		t.Fatal(err)
	}
	return req, needs
}

func TestRegistered(t *testing.T) {
	if c, ok := protocol.Get(Name); !ok || c.Name() != Name {
		t.Fatal("codec not registered")
	}
}

func TestSniff(t *testing.T) {
	tests := []struct {
		path, model, action string
		op                  protocol.Op
		stream, wantErr     bool
	}{
		{path: "/v1beta/models/gemini-2.5-pro:generateContent", model: "gemini-2.5-pro", action: ActionGenerate, op: protocol.OpGenerate},
		{path: "/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse", model: "gemini-2.5-flash", action: ActionStream, op: protocol.OpGenerate, stream: true},
		{path: "/v1/models/gemini-1.5-pro-002:countTokens", model: "gemini-1.5-pro-002", action: ActionCountTokens, op: protocol.OpCountTokens},
		{path: "/v1beta/models/vendor/model-v1.5:generateContent", model: "vendor/model-v1.5", action: ActionGenerate, op: protocol.OpGenerate},
		{path: "/gw/v1beta/models/gemini-3-pro-preview:streamGenerateContent", model: "gemini-3-pro-preview", action: ActionStream, op: protocol.OpGenerate, stream: true},
		{path: "/models/x%2Fy:generateContent", model: "x/y", action: ActionGenerate, op: protocol.OpGenerate},
		{path: "/v1beta/models/text-embedding-004:embedContent", wantErr: true},
		{path: "/v1beta/models/gemini-2.5-pro", wantErr: true},
		{path: "/v1/messages", wantErr: true},
	}
	body := readFixture(t, "request_agent.json")
	c := Codec{}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			in, err := c.Sniff(generatePost(tt.path, body), c.Routes()[0], body)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", in)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if in.Model != tt.model || in.Action != tt.action || in.Op != tt.op || in.Stream != tt.stream {
				t.Fatalf("got %+v", in)
			}
			if in.Session.PrefixHash == "" {
				t.Fatal("no prefix hash")
			}
		})
	}
	in, err := c.Sniff(httptest.NewRequest(http.MethodGet, "/v1beta/models", nil), c.Routes()[1], nil)
	if err != nil || in.Op != protocol.OpModels {
		t.Fatalf("models: %+v %v", in, err)
	}
}

func TestPrefixHashStable(t *testing.T) {
	a := []byte(`{"systemInstruction":{"parts":[{"text":"S"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	b := []byte(`{"systemInstruction":{"parts":[{"text":"S"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"yo"}]}]}`)
	c := []byte(`{"systemInstruction":{"parts":[{"text":"S"}]},"contents":[{"role":"user","parts":[{"text":"other"}]}]}`)
	if prefixHash(a) != prefixHash(b) || prefixHash(a) == prefixHash(c) {
		t.Fatal("prefix hash must depend only on system + first user content")
	}
}

func TestDecodeRequest(t *testing.T) {
	req, needs := decode(t, "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", readFixture(t, "request_agent.json"))
	if req.Model != "gemini-2.5-pro" || !req.Stream {
		t.Fatalf("model/stream: %q %v", req.Model, req.Stream)
	}
	if len(req.System) != 2 || req.System[1].Text != "Be concise." {
		t.Fatalf("system: %+v", req.System)
	}
	if len(req.Messages) != 5 || req.Messages[1].Role != ir.RoleAssistant {
		t.Fatalf("messages: %+v", req.Messages)
	}
	model := req.Messages[1].Content
	if model[0].Type != ir.BlockThinking || model[1].Type != ir.BlockToolUse {
		t.Fatalf("model blocks: %+v", model)
	}
	call := model[1]
	if call.Tool.ID != "call_list_directory_0" || string(call.Tool.Input) != `{"path":"src"}` {
		t.Fatalf("call: %+v", call.Tool)
	}
	if call.Opaque == nil || call.Opaque.Kind != "thought_signature" || call.Opaque.Sealed ||
		string(call.Opaque.Data) != `"CiQBVKhc7mVhc2lnbmF0dXJlLW9uZQ=="` {
		t.Fatalf("signature: %+v", call.Opaque)
	}
	results := req.Messages[2].Content
	if results[0].Result.ToolUseID != "call_list_directory_0" || results[1].Result.ToolUseID != "call_read_file_1" {
		t.Fatalf("result ids: %s %s", results[0].Result.ToolUseID, results[1].Result.ToolUseID)
	}
	if results[0].Result.Content[0].Text != "main.go\nutil.go" {
		t.Fatalf("result text: %+v", results[0].Result.Content)
	}
	answer := req.Messages[3].Content
	if answer[0].Type != ir.BlockText || answer[0].Opaque == nil || answer[1].Type != ir.BlockOpaque {
		t.Fatalf("answer: %+v", answer)
	}
	if len(req.Tools) != 4 || req.Tools[0].Kind != ir.ToolFunction || req.Tools[2].ServerType != "google_search" || req.Tools[3].ServerType != "url_context" {
		t.Fatalf("tools: %+v", req.Tools)
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != "auto" {
		t.Fatalf("tool choice: %+v", req.ToolChoice)
	}
	if *req.MaxOutputTokens != 8192 || *req.Temperature != 0.7 || *req.TopK != 40 || req.Stop[0] != "END" {
		t.Fatal("generation config")
	}
	if r := req.Reasoning; r == nil || !r.Enabled || r.Budget == nil || *r.Budget != 8192 {
		t.Fatalf("reasoning: %+v", req.Reasoning)
	}
	if _, ok := req.Extra.Get(Name, "safetySettings"); !ok {
		t.Fatal("safetySettings not kept")
	}
	want := ir.Needs{
		FunctionTools: true, ServerTools: []string{"google_search", "url_context"},
		Reasoning: true, Images: true, Documents: true, OpaqueState: true,
		InputTokensEst: needs.InputTokensEst,
	}
	if !reflect.DeepEqual(needs, want) || needs.InputTokensEst <= 0 {
		t.Fatalf("needs:\n got %+v\nwant %+v", needs, want)
	}
}

func TestRequestRoundTrip(t *testing.T) {
	body := readFixture(t, "request_agent.json")
	req, _ := decode(t, "/v1beta/models/gemini-2.5-pro:generateContent", body)
	out, err := Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, body, out)
}

func TestThinkingConfig(t *testing.T) {
	tests := []struct {
		name, in  string
		enabled   bool
		budget    *int
		effort    string
		roundTrip string
	}{
		{name: "level", in: `{"thinkingLevel":"HIGH"}`, enabled: true, effort: "high", roundTrip: `{"thinkingLevel":"high","includeThoughts":true}`},
		{name: "off", in: `{"thinkingBudget":0}`, budget: new(0), roundTrip: `{"thinkingBudget":0}`},
		{name: "dynamic", in: `{"thinkingBudget":-1}`, enabled: true, roundTrip: `{"thinkingBudget":-1,"includeThoughts":true}`},
		{name: "include only", in: `{"includeThoughts":true}`, enabled: true, roundTrip: `{"includeThoughts":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"contents":[],"generationConfig":{"thinkingConfig":` + tt.in + `}}`)
			req, needs := decode(t, "/v1beta/models/m:generateContent", body)
			r := req.Reasoning
			if r.Enabled != tt.enabled || r.Effort != tt.effort || !reflect.DeepEqual(r.Budget, tt.budget) || needs.Reasoning != tt.enabled {
				t.Fatalf("reasoning %+v", r)
			}
			out, _ := Codec{}.EncodeRequest(req)
			jsonEqual(t, []byte(tt.roundTrip), []byte(gjson.GetBytes(out, "generationConfig.thinkingConfig").Raw))
		})
	}
}

func TestToolChoice(t *testing.T) {
	tests := []struct{ in, mode, name string }{
		{`{"mode":"NONE"}`, "none", ""},
		{`{"mode":"ANY"}`, "any", ""},
		{`{"mode":"ANY","allowedFunctionNames":["f"]}`, "tool", "f"},
		{`{"mode":"AUTO"}`, "auto", ""},
	}
	for _, tt := range tests {
		body := []byte(`{"contents":[],"toolConfig":{"functionCallingConfig":` + tt.in + `}}`)
		req, _ := decode(t, "/v1beta/models/m:generateContent", body)
		if req.ToolChoice.Mode != tt.mode || req.ToolChoice.Name != tt.name {
			t.Fatalf("%s: %+v", tt.in, req.ToolChoice)
		}
		req.Extra = ir.Extra{}
		out, _ := Codec{}.EncodeRequest(req)
		jsonEqual(t, []byte(tt.in), []byte(gjson.GetBytes(out, "toolConfig.functionCallingConfig").Raw))
	}
}

func TestSealedSignature(t *testing.T) {
	seal := ir.SealPrefix + "abc.def"
	body := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + seal + `"},{"text":"","thoughtSignature":"` + seal + `"}]}]}`)
	req, _ := decode(t, "/v1beta/models/m:generateContent", body)
	for _, b := range req.Messages[0].Content {
		if b.Opaque == nil || !b.Opaque.Sealed {
			t.Fatalf("not sealed: %+v", b)
		}
	}
	// A sealed state from another issuer reaches the client in thoughtSignature.
	resp := &ir.Response{Content: []ir.Block{
		{Type: ir.BlockThinking, Text: "t", Opaque: &ir.OpaqueState{Kind: "signature", Data: json.RawMessage(`"` + seal + `"`), Sealed: true}},
		{Type: ir.BlockOpaque, Opaque: &ir.OpaqueState{Kind: "redacted_thinking", Data: json.RawMessage(`"` + seal + `"`), Sealed: true}},
		{Type: ir.BlockOpaque, Opaque: &ir.OpaqueState{Kind: "encrypted_content", Data: json.RawMessage(`"raw"`)}},
	}, StopReason: ir.StopEndTurn}
	out, err := Codec{}.EncodeResponse(nil, resp)
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(out, "candidates.0.content.parts")
	jsonEqual(t, []byte(`[{"text":"t","thought":true,"thoughtSignature":"`+seal+`"},{"text":"","thoughtSignature":"`+seal+`"}]`), []byte(parts.Raw))
	// Upstreams never receive sealed state.
	req.Messages[0].Content[0].Opaque.Sealed = true
	up, _ := Codec{}.EncodeRequest(req)
	if bytes.Contains(up, []byte(ir.SealPrefix)) {
		t.Fatalf("seal leaked upstream: %s", up)
	}
}

func TestToolIDs(t *testing.T) {
	body := []byte(`{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}},{"functionCall":{"id":"x1","name":"f","args":{}}},{"functionCall":{"name":"f","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"id":"x1","name":"f","response":{"output":"b"}}},{"functionResponse":{"name":"f","response":{"output":"a"}}},{"functionResponse":{"name":"f","response":{"output":"c"}}}]}]}`)
	req, _ := decode(t, "/v1beta/models/m:generateContent", body)
	var got []string
	for _, b := range req.Messages[1].Content {
		got = append(got, b.Result.ToolUseID)
	}
	if want := []string{"x1", "call_f_0", "call_f_1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids %v want %v", got, want)
	}
	out, _ := Codec{}.EncodeRequest(req)
	jsonEqual(t, []byte(gjson.GetBytes(body, "contents").Raw), []byte(gjson.GetBytes(out, "contents").Raw))

	// A tool result from another protocol gets its name from history.
	foreign := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "toolu_1", Name: "grep", Kind: ir.ToolFunction, Input: json.RawMessage(`{"q":"x"}`)}}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, Result: &ir.ToolResult{ToolUseID: "toolu_1", IsError: true, Content: []ir.Block{{Type: ir.BlockText, Text: "boom"}}}}}},
	}}
	out, _ = Codec{}.EncodeRequest(foreign)
	jsonEqual(t, []byte(`{"id":"toolu_1","name":"grep","response":{"error":"boom"}}`), []byte(gjson.GetBytes(out, "contents.1.parts.0.functionResponse").Raw))
	if n := nameFromSyntheticID("call_read_file_ab12cd34_3"); n != "read_file" {
		t.Fatalf("name from salted id: %q", n)
	}
}

func TestResponseRoundTrip(t *testing.T) {
	body := readFixture(t, "response_tool.json")
	resp, err := Codec{}.DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "resp-abc" || resp.Model != "gemini-2.5-pro" || resp.StopReason != ir.StopToolUse {
		t.Fatalf("resp: %+v", resp)
	}
	want := ir.Usage{Input: 200, CacheRead: 1000, Output: 130, Reasoning: 100}
	got := resp.Usage
	got.Raw = nil
	if !reflect.DeepEqual(got, want) || len(resp.Usage.Raw) == 0 {
		t.Fatalf("usage %+v", resp.Usage)
	}
	call := resp.Content[1]
	if !strings.HasPrefix(call.Tool.ID, "call_get_weather_") || call.Opaque == nil {
		t.Fatalf("call: %+v", call)
	}
	out, err := Codec{}.EncodeResponse(nil, resp)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, body, out)
}

func TestFinishReasons(t *testing.T) {
	tests := []struct {
		finish string
		call   bool
		want   ir.StopReason
	}{
		{"STOP", false, ir.StopEndTurn},
		{"STOP", true, ir.StopToolUse},
		{"MAX_TOKENS", false, ir.StopMaxTokens},
		{"SAFETY", false, ir.StopContentFilter},
		{"RECITATION", false, ir.StopContentFilter},
		{"PROHIBITED_CONTENT", false, ir.StopContentFilter},
		{"MALFORMED_FUNCTION_CALL", false, ir.StopEndTurn},
	}
	for _, tt := range tests {
		if got := stopReason(tt.finish, tt.call); got != tt.want {
			t.Errorf("%s/%v: %s", tt.finish, tt.call, got)
		}
	}
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"MALFORMED_FUNCTION_CALL","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`)
	resp, _ := Codec{}.DecodeResponse(body)
	out, _ := Codec{}.EncodeResponse(nil, resp)
	if f := gjson.GetBytes(out, "candidates.0.finishReason").String(); f != "MALFORMED_FUNCTION_CALL" {
		t.Fatalf("finish reason not preserved: %s", f)
	}
	blocked, _ := Codec{}.DecodeResponse([]byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`))
	if blocked.StopReason != ir.StopContentFilter {
		t.Fatalf("blocked prompt: %s", blocked.StopReason)
	}
}

func feedStream(t *testing.T, data []byte) ([]ir.Event, bool, protocol.StreamDecoder) {
	t.Helper()
	d := Codec{}.NewStreamDecoder()
	rd := sse.NewReader(bytes.NewReader(data), 0)
	var evs []ir.Event
	for {
		ev, err := rd.Next()
		if err != nil {
			break
		}
		out, err := d.Feed(ev)
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, out...)
	}
	tail, complete := d.Finish()
	return append(evs, tail...), complete, d
}

func eventShape(evs []ir.Event) []string {
	var s []string
	for _, e := range evs {
		x := string(e.Kind)
		switch e.Kind {
		case ir.EvBlockStart:
			x += ":" + string(e.Block.Type)
		case ir.EvDelta:
			x += ":" + string(e.Delta.Kind) + ":" + e.Delta.Text
		case ir.EvBlockStop:
			if e.Block != nil && e.Block.Opaque != nil {
				x += ":sig"
			}
		case ir.EvDone:
			x += ":" + string(e.StopReason)
		}
		if e.Kind != ir.EvStart && e.Kind != ir.EvUsage && e.Kind != ir.EvDone {
			x = string(rune('0'+e.Index)) + " " + x
		}
		s = append(s, x)
	}
	return s
}

func TestStreamDecode(t *testing.T) {
	evs, complete, d := feedStream(t, readFixture(t, "stream_tool.sse"))
	if !complete {
		t.Fatal("stream not complete")
	}
	want := []string{
		"start", "0 block_start:thinking", "0 delta:thinking:**Planning** the", "usage",
		"0 delta:thinking: lookup.",
		"0 block_stop", "1 block_start:text", "1 delta:text:Let me check", "1 delta:text: the weather.",
		"1 block_stop", "2 block_start:tool_use", `2 delta:tool_input:{"city":"Paris"}`, "2 block_stop:sig",
		"usage", "done:tool_use",
	}
	if got := eventShape(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}
	if evs[0].ID != "r-1" || evs[0].Model != "gemini-2.5-flash" {
		t.Fatalf("start: %+v", evs[0])
	}
	u := d.Usage()
	if u.Input != 300 || u.CacheRead != 200 || u.Output != 60 || u.Reasoning != 20 {
		t.Fatalf("usage %+v", u)
	}
}

func TestStreamDecodeSignatureAndErrors(t *testing.T) {
	chunk := func(parts string) string {
		return `data: {"candidates":[{"content":{"role":"model","parts":[` + parts + `]}}]}` + "\n\n"
	}
	// A signature-only part attaches to the open text block.
	evs, complete, _ := feedStream(t, []byte(chunk(`{"text":"hi"}`)+chunk(`{"text":"","thoughtSignature":"s1"}`)))
	if complete {
		t.Fatal("no finishReason must be incomplete")
	}
	if got := eventShape(evs); !reflect.DeepEqual(got, []string{"start", "0 block_start:text", "0 delta:text:hi", "0 block_stop:sig"}) {
		t.Fatalf("got %q", got)
	}
	// With no open block it becomes an opaque block.
	evs, _, _ = feedStream(t, []byte(chunk(`{"thoughtSignature":"s2"}`)))
	if got := eventShape(evs); !reflect.DeepEqual(got, []string{"start", "0 block_start:opaque", "0 block_stop:sig"}) {
		t.Fatalf("got %q", got)
	}
	// An error chunk ends the stream.
	evs, complete, _ = feedStream(t, []byte(chunk(`{"text":"a"}`)+`data: {"error":{"code":503,"message":"The model is overloaded.","status":"UNAVAILABLE"}}`+"\n\n"))
	last := evs[len(evs)-1]
	if !complete || last.Kind != ir.EvError || last.Err.Kind != ir.ErrOverloaded {
		t.Fatalf("error: %+v", last)
	}
}

type recWriter struct {
	events   [][]byte
	comments []string
}

func (w *recWriter) Event(_ string, data []byte) error {
	w.events = append(w.events, append([]byte(nil), data...))
	return nil
}
func (w *recWriter) Comment(text string) error { w.comments = append(w.comments, text); return nil }
func (w *recWriter) Raw([]byte) error          { return nil }

func (w *recWriter) sse() []byte {
	var b bytes.Buffer
	for _, e := range w.events {
		b.WriteString("data: ")
		b.Write(e)
		b.WriteString("\n\n")
	}
	return b.Bytes()
}

func TestStreamEncode(t *testing.T) {
	w := &recWriter{}
	enc := Codec{}.NewStreamEncoder(w, &ir.Request{Model: "m"})
	sig := &ir.OpaqueState{Kind: "thought_signature", Data: json.RawMessage(`"sig-fc"`)}
	usage := &ir.Usage{Input: 10, CacheRead: 5, Output: 7, Reasoning: 2}
	evs := []ir.Event{
		{Kind: ir.EvStart, ID: "r-9", Model: "gemini-x"},
		{Kind: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking}},
		{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaThinking, Text: "hmm"}},
		{Kind: ir.EvBlockStop, Index: 0},
		{Kind: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockText}},
		{Kind: ir.EvDelta, Index: 1, Delta: &ir.Delta{Kind: ir.DeltaText, Text: "Hello"}},
		{Kind: ir.EvBlockStop, Index: 1},
		{Kind: ir.EvBlockStart, Index: 2, Block: &ir.Block{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "toolu_7", Name: "f", Kind: ir.ToolFunction}}},
		{Kind: ir.EvDelta, Index: 2, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: `{"a":`}},
		{Kind: ir.EvDelta, Index: 2, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: `1}`}},
		{Kind: ir.EvBlockStop, Index: 2, Block: &ir.Block{Type: ir.BlockToolUse, Opaque: sig}},
		{Kind: ir.EvUsage, Usage: usage},
		{Kind: ir.EvDone, StopReason: ir.StopToolUse},
		{Kind: ir.EvDelta, Index: 1, Delta: &ir.Delta{Kind: ir.DeltaText, Text: "late"}},
	}
	for i := range evs {
		if err := enc.Encode(&evs[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Keepalive(); err != nil || len(w.comments) != 1 {
		t.Fatal("keepalive")
	}
	if len(w.events) != 4 {
		t.Fatalf("chunks: %d\n%s", len(w.events), w.sse())
	}
	jsonEqual(t, []byte(`{"functionCall":{"id":"toolu_7","name":"f","args":{"a":1}},"thoughtSignature":"sig-fc"}`),
		[]byte(gjson.GetBytes(w.events[2], "candidates.0.content.parts.0").Raw))
	last := w.events[3]
	if gjson.GetBytes(last, "candidates.0.finishReason").String() != "STOP" {
		t.Fatalf("last chunk: %s", last)
	}
	jsonEqual(t, []byte(`{"promptTokenCount":15,"cachedContentTokenCount":5,"candidatesTokenCount":5,"thoughtsTokenCount":2,"totalTokenCount":22}`),
		[]byte(gjson.GetBytes(last, "usageMetadata").Raw))
	if gjson.GetBytes(last, "responseId").String() != "r-9" || gjson.GetBytes(last, "modelVersion").String() != "gemini-x" {
		t.Fatalf("ids: %s", last)
	}
	if !reflect.DeepEqual(enc.Usage(), *usage) {
		t.Fatal("encoder usage")
	}

	// What the encoder wrote decodes back to the same blocks.
	back, complete, _ := feedStream(t, w.sse())
	want := []string{"start", "0 block_start:thinking", "0 delta:thinking:hmm", "0 block_stop", "1 block_start:text",
		"1 delta:text:Hello", "1 block_stop", "2 block_start:tool_use", `2 delta:tool_input:{"a":1}`, "2 block_stop:sig", "usage", "done:tool_use"}
	if got := eventShape(back); !complete || !reflect.DeepEqual(got, want) {
		t.Fatalf("re-decode:\n got %q\nwant %q", got, want)
	}
}

func TestStreamEncodeError(t *testing.T) {
	w := &recWriter{}
	enc := Codec{}.NewStreamEncoder(w, nil)
	_ = enc.Encode(&ir.Event{Kind: ir.EvError, Err: &ir.GwError{Kind: ir.ErrOverloaded, Message: "busy"}})
	_ = enc.Encode(&ir.Event{Kind: ir.EvDone})
	if len(w.events) != 1 {
		t.Fatalf("events: %d", len(w.events))
	}
	jsonEqual(t, []byte(`{"error":{"code":503,"message":"busy","status":"UNAVAILABLE"}}`), w.events[0])
}

func TestEncodeError(t *testing.T) {
	tests := []struct {
		name   string
		err    ir.GwError
		status int
		body   string
		retry  string
	}{
		{"rate limited", ir.GwError{Kind: ir.ErrRateLimited, Message: "slow down", RetryAfter: 12 * time.Second}, 429,
			`{"error":{"code":429,"message":"slow down","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"12s"}]}}`, "12"},
		{"quota per day", ir.GwError{Kind: ir.ErrQuotaExhausted, Message: "quota used", NoRetry: true}, 429,
			`{"error":{"code":429,"message":"quota used","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaMetric":"generate_requests","quotaId":"GenerateRequestsPerDayPerKey","description":"quota used"}]}]}}`, ""},
		{"quota retryable", ir.GwError{Kind: ir.ErrQuotaExhausted, Message: "q", RetryAfter: 1500 * time.Millisecond}, 429,
			`{"error":{"code":429,"message":"q","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaMetric":"generate_requests","quotaId":"GenerateRequestsPerMinutePerKey","description":"q"}]},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"1.5s"}]}}`, "2"},
		{"context too long", ir.GwError{Kind: ir.ErrContextTooLong, Message: "too long", TokensUsed: 1200000, TokensLimit: 1048576}, 400,
			`{"error":{"code":400,"message":"The input token count (1200000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`, ""},
		{"overloaded", ir.GwError{Kind: ir.ErrOverloaded, Message: "busy"}, 503, `{"error":{"code":503,"message":"busy","status":"UNAVAILABLE"}}`, ""},
		{"filtered", ir.GwError{Kind: ir.ErrContentFiltered, Message: "blocked"}, 400, `{"error":{"code":400,"message":"blocked","status":"INVALID_ARGUMENT"}}`, ""},
		{"capability", ir.GwError{Kind: ir.ErrCapabilityMissing, Message: "no tools"}, 400, `{"error":{"code":400,"message":"no tools","status":"INVALID_ARGUMENT"}}`, ""},
		{"internal", ir.GwError{Kind: ir.ErrInternal, Message: "oops"}, 500, `{"error":{"code":500,"message":"oops","status":"INTERNAL"}}`, ""},
		{"auth", ir.GwError{Kind: ir.ErrAuthentication, Message: "bad key"}, 401, `{"error":{"code":401,"message":"bad key","status":"UNAUTHENTICATED"}}`, ""},
		{"permission", ir.GwError{Kind: ir.ErrPermission, Message: "no"}, 403, `{"error":{"code":403,"message":"no","status":"PERMISSION_DENIED"}}`, ""},
		{"not found", ir.GwError{Kind: ir.ErrNotFound, Message: "nope"}, 404, `{"error":{"code":404,"message":"nope","status":"NOT_FOUND"}}`, ""},
		{"status override", ir.GwError{Kind: ir.ErrUpstream, Message: "x", Status: 503}, 503, `{"error":{"code":503,"message":"x","status":"UNAVAILABLE"}}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, h, body := Codec{}.EncodeError(&tt.err)
			if status != tt.status || h.Get("Retry-After") != tt.retry || !strings.HasPrefix(h.Get("Content-Type"), "application/json") {
				t.Fatalf("status %d headers %v", status, h)
			}
			jsonEqual(t, []byte(tt.body), body)
		})
	}
}

func TestDecodeError(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		header  http.Header
		body    string
		kind    ir.ErrorKind
		retry   time.Duration
		noRetry bool
		used    int
	}{
		{name: "retry info", status: 429, body: `{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerMinutePerProjectPerModel"}]},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"37.5s"}]}}`,
			kind: ir.ErrRateLimited, retry: 37500 * time.Millisecond},
		{name: "per day", status: 429, body: `{"error":{"code":429,"message":"You exceeded your current quota","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerDayPerProjectPerModel-FreeTier"}]}]}}`,
			kind: ir.ErrQuotaExhausted, noRetry: true},
		{name: "retry-after header", status: 429, header: http.Header{"Retry-After": {"5"}}, body: `{"error":{"code":429,"message":"x","status":"RESOURCE_EXHAUSTED"}}`,
			kind: ir.ErrRateLimited, retry: 5 * time.Second},
		{name: "context", status: 400, body: `{"error":{"code":400,"message":"The input token count (1200000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}`,
			kind: ir.ErrContextTooLong, used: 1200000},
		{name: "bad key", status: 400, body: `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID","domain":"googleapis.com"}]}}`,
			kind: ir.ErrAuthentication},
		{name: "invalid", status: 400, body: `{"error":{"code":400,"message":"Invalid JSON payload","status":"INVALID_ARGUMENT"}}`, kind: ir.ErrInvalidRequest},
		{name: "permission", status: 403, body: `{"error":{"code":403,"message":"denied","status":"PERMISSION_DENIED"}}`, kind: ir.ErrPermission},
		{name: "not found", status: 404, body: `{"error":{"code":404,"message":"models/x is not found","status":"NOT_FOUND"}}`, kind: ir.ErrNotFound},
		{name: "unavailable", status: 503, body: `{"error":{"code":503,"message":"The model is overloaded.","status":"UNAVAILABLE"}}`, kind: ir.ErrOverloaded},
		{name: "deadline", status: 504, body: `{"error":{"code":504,"message":"Deadline exceeded","status":"DEADLINE_EXCEEDED"}}`, kind: ir.ErrTimeout},
		{name: "internal", status: 500, body: `{"error":{"code":500,"message":"An internal error has occurred.","status":"INTERNAL"}}`, kind: ir.ErrUpstream},
		{name: "array body", status: 429, body: `[{"error":{"code":429,"message":"x","status":"RESOURCE_EXHAUSTED"}}]`, kind: ir.ErrRateLimited},
		{name: "not json", status: 502, body: `bad gateway`, kind: ir.ErrUpstream},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.header
			if h == nil {
				h = http.Header{}
			}
			e := Codec{}.DecodeError(tt.status, h, []byte(tt.body))
			if e.Kind != tt.kind || e.RetryAfter != tt.retry || e.NoRetry != tt.noRetry || e.TokensUsed != tt.used || e.Message == "" {
				t.Fatalf("got %+v", e)
			}
		})
	}
	// Encoding what was decoded keeps the client signal.
	e := Codec{}.DecodeError(429, http.Header{}, []byte(tests[1].body))
	_, _, body := Codec{}.EncodeError(e)
	if !strings.Contains(gjson.GetBytes(body, "error.details.0.violations.0.quotaId").String(), "PerDay") {
		t.Fatalf("per-day lost: %s", body)
	}
}

func TestStripOpaque(t *testing.T) {
	body := readFixture(t, "request_agent.json")
	out, n, err := Codec{}.StripOpaque(body)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || bytes.Contains(out, []byte("thoughtSignature")) {
		t.Fatalf("removed %d: %s", n, out)
	}
	if got := len(gjson.GetBytes(out, "contents.3.parts").Array()); got != 1 {
		t.Fatalf("signature-only part kept: %d parts", got)
	}
	wrapped := []byte(`{"generateContentRequest":{"model":"models/m","contents":[{"role":"model","parts":[{"thoughtSignature":"s"}]},{"role":"user","parts":[{"text":"q"}]}]}}`)
	out, n, _ = Codec{}.StripOpaque(wrapped)
	jsonEqual(t, []byte(`{"generateContentRequest":{"model":"models/m","contents":[{"role":"user","parts":[{"text":"q"}]}]}}`), out)
	if n != 1 {
		t.Fatalf("removed %d", n)
	}
}

func TestUpstreamPathAndModel(t *testing.T) {
	c := Codec{}
	if p := c.UpstreamPath(&ir.Request{Model: "gemini-2.5-pro", Stream: true}, protocol.Inbound{Op: protocol.OpGenerate}); p != "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse" {
		t.Fatal(p)
	}
	if p := c.UpstreamPath(&ir.Request{Model: "models/m"}, protocol.Inbound{Op: protocol.OpGenerate}); p != "/v1beta/models/m:generateContent" {
		t.Fatal(p)
	}
	if p := c.UpstreamPath(&ir.Request{Model: "m"}, protocol.Inbound{Op: protocol.OpCountTokens}); p != "/v1beta/models/m:countTokens" {
		t.Fatal(p)
	}
	body := []byte(`{"contents":[]}`)
	if out, _ := c.SetModel(body, "x"); !bytes.Equal(out, body) {
		t.Fatal("SetModel must not touch the body")
	}
}

func TestModelsAndCountTokens(t *testing.T) {
	ct, body := Codec{}.EncodeModels(nil, []ir.ModelInfo{{ID: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro", ContextWindow: 1048576, MaxOutput: 65536, Reasoning: true}})
	if ct != "application/json" {
		t.Fatal(ct)
	}
	m := gjson.GetBytes(body, "models.0")
	if m.Get("name").String() != "models/gemini-2.5-pro" || m.Get("inputTokenLimit").Int() != 1048576 ||
		m.Get("outputTokenLimit").Int() != 65536 || !m.Get("thinking").Bool() ||
		m.Get("supportedGenerationMethods.#").Int() != 3 || m.Get("displayName").String() != "Gemini 2.5 Pro" {
		t.Fatalf("models: %s", body)
	}
	wrapped := []byte(`{"generateContentRequest":{"model":"models/m","contents":[{"role":"user","parts":[{"text":"` + strings.Repeat("a", 400) + `"}]}]}}`)
	req, _ := decode(t, "/v1beta/models/m:countTokens", wrapped)
	out, err := Codec{}.CountTokens(req)
	if err != nil || gjson.GetBytes(out, "totalTokens").Int() != 100 {
		t.Fatalf("count: %s %v", out, err)
	}
}
