package openaichat

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
)

var codec Codec

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
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got: %v\n%s", err, got)
	}
	if !reflect.DeepEqual(w, g) {
		wi, _ := json.MarshalIndent(w, "", " ")
		gi, _ := json.MarshalIndent(g, "", " ")
		t.Fatalf("JSON differs\nwant:\n%s\ngot:\n%s", wi, gi)
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
	if !ok || c.Name() != "openai-chat" {
		t.Fatal("codec not registered")
	}
}

func TestRequestRoundTrip(t *testing.T) {
	body := readFixture(t, "request_full.json")
	req, _ := decode(t, body)
	out, err := codec.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, body, out)
	if !bytes.Contains(out, []byte("<b>&</b>")) {
		t.Error("HTML characters were escaped")
	}
}

func TestDecodeRequestShape(t *testing.T) {
	req, needs := decode(t, readFixture(t, "request_full.json"))

	if len(req.System) != 3 {
		t.Fatalf("system blocks = %d, want 3", len(req.System))
	}
	if r := getStr(&req.System[1].Extra, xRole); r != "developer" {
		t.Errorf("developer role not kept: %q", r)
	}
	if req.System[2].Cache == nil {
		t.Error("cache_control on system part lost")
	}
	if *req.MaxOutputTokens != 512 || !has(&req.Extra, xMaxTokens) {
		t.Error("max_tokens not decoded or not remembered")
	}
	if req.Reasoning == nil || req.Reasoning.Effort != "high" || !req.Reasoning.Enabled {
		t.Errorf("reasoning = %+v", req.Reasoning)
	}
	if req.CacheKey != "sess-1" || req.ServiceTier != "auto" || !req.Stream {
		t.Error("top-level scalars lost")
	}
	if req.ToolChoice == nil || req.ToolChoice.Mode != "tool" || req.ToolChoice.Name != "get_weather" {
		t.Errorf("tool_choice = %+v", req.ToolChoice)
	}
	if req.Tools[1].Kind != ir.ToolFreeform || len(req.Tools[1].Grammar) == 0 {
		t.Error("custom tool not decoded as freeform with grammar")
	}

	// user, assistant, merged tool results, user, relocated system, assistant, user
	if len(req.Messages) != 7 {
		t.Fatalf("messages = %d, want 7", len(req.Messages))
	}
	user := req.Messages[0]
	if user.Content[1].Type != ir.BlockImage || user.Content[1].Media.Data != "iVBORw0KGgo=" ||
		user.Content[1].Media.MediaType != "image/png" || user.Content[1].Media.Detail != "high" {
		t.Errorf("data URL image = %+v", user.Content[1].Media)
	}
	if user.Content[2].Media.URL != "https://example.com/cat.jpg" {
		t.Error("image URL lost")
	}
	if user.Content[3].Type != ir.BlockRaw {
		t.Error("input_audio should be raw")
	}

	asst := req.Messages[1]
	if asst.Content[0].Type != ir.BlockThinking || asst.Content[0].Text != "Need weather for both." {
		t.Errorf("thinking = %+v", asst.Content[0])
	}
	if len(asst.Content) != 4 || asst.Content[3].Tool.Kind != ir.ToolFreeform {
		t.Fatalf("assistant blocks = %+v", asst.Content)
	}

	tools := req.Messages[2]
	if tools.Role != ir.RoleUser || len(tools.Content) != 3 {
		t.Fatalf("tool messages not merged: %+v", tools)
	}
	for i, id := range []string{"call_1", "call_2", "call_3"} {
		if tools.Content[i].Result.ToolUseID != id {
			t.Errorf("result %d id = %q", i, tools.Content[i].Result.ToolUseID)
		}
	}
	if !tools.Content[2].Result.Freeform {
		t.Error("result of custom call should be Freeform")
	}
	if c := tools.Content[2].Cache; c == nil || c.TTL != "1h" {
		t.Errorf("tool message cache_control = %+v", c)
	}
	if req.Messages[3].Content[0].Cache == nil {
		t.Error("message-level cache_control on user lost")
	}
	if getStr(&req.Messages[4].Extra, xRole) != "system" {
		t.Error("mid-conversation system message not marked")
	}
	if _, ok := req.Messages[5].Content[0].Extra.Get(Name, "reasoning_details"); !ok {
		t.Error("reasoning_details not kept")
	}
	if req.Messages[6].Content[0].Type != ir.BlockDocument || req.Messages[6].Content[0].Media.Title != "a.pdf" {
		t.Error("file part not decoded as document")
	}

	want := ir.Needs{
		ExplicitCache: true, Cache1h: true, FunctionTools: true, FreeformTools: true,
		Reasoning: true, Images: true, Documents: true, OpaqueState: true,
		InputTokensEst: needs.InputTokensEst,
	}
	if !reflect.DeepEqual(needs, want) {
		t.Errorf("needs = %+v\nwant  %+v", needs, want)
	}
	if needs.InputTokensEst <= 0 {
		t.Error("no input estimate")
	}
}

func TestNeedsMinimal(t *testing.T) {
	_, n := decode(t, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`))
	if n.ExplicitCache || n.Reasoning || n.FunctionTools || n.Images {
		t.Errorf("unexpected needs %+v", n)
	}
	_, n = decode(t, []byte(`{"model":"m","messages":[{"role":"tool","tool_call_id":"a","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]}`))
	if !n.ToolResultImgs || n.Images {
		t.Errorf("tool result image needs %+v", n)
	}
}

func TestDecodeRequestErrors(t *testing.T) {
	for _, body := range []string{`[]`, `{"messages":"x"}`, `{"messages":[{"role":"wizard","content":"x"}]}`, `{"messages":[],"stop":5}`} {
		_, _, err := codec.DecodeRequest(nil, protocol.Inbound{}, []byte(body))
		var gw *ir.GwError
		if err == nil || !asGw(err, &gw) || gw.Kind != ir.ErrInvalidRequest {
			t.Errorf("%s: err = %v", body, err)
		}
	}
}

func asGw(err error, out **ir.GwError) bool {
	g, ok := err.(*ir.GwError)
	*out = g
	return ok
}

func TestCacheControlInChat(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"system","content":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}]},
		{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b","cache_control":{"type":"ephemeral","ttl":"1h"}}]},
		{"role":"assistant","content":"x","cache_control":{"type":"ephemeral"}}]}`)
	req, n := decode(t, body)
	if req.System[0].Cache == nil || req.Messages[0].Content[1].Cache.TTL != "1h" || req.Messages[1].Content[0].Cache == nil {
		t.Fatal("cache marks not decoded")
	}
	if !n.ExplicitCache || !n.Cache1h {
		t.Errorf("needs %+v", n)
	}
	out, _ := codec.EncodeRequest(req)
	jsonEqual(t, body, out)

	// Marks removed by a conversion step leave plain content behind.
	req.System[0].Cache = nil
	req.Messages[0].Content[1].Cache = nil
	req.Messages[1].Content[0].Cache = nil
	out, _ = codec.EncodeRequest(req)
	if bytes.Contains(out, []byte("cache_control")) {
		t.Errorf("cache_control still present: %s", out)
	}
}

func TestToolResultSplitting(t *testing.T) {
	img := ir.Block{Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: "AA"}}
	req := &ir.Request{Model: "m", Messages: []ir.Message{
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockThinking, Text: "think"},
			{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "t1", Name: "f", Kind: ir.ToolFunction, Input: json.RawMessage(`{"a":1}`)}},
			{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "t2", Name: "f", Kind: ir.ToolFunction}},
		}},
		{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockToolResult, Result: &ir.ToolResult{ToolUseID: "t1", Content: []ir.Block{{Type: ir.BlockText, Text: "r1"}}}},
			{Type: ir.BlockToolResult, Result: &ir.ToolResult{ToolUseID: "t2", Content: []ir.Block{{Type: ir.BlockText, Text: "r2"}, img}}},
			{Type: ir.BlockText, Text: "next"},
		}},
	}}
	out, err := codec.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","messages":[
		{"role":"assistant","content":null,"reasoning_content":"think","tool_calls":[
			{"id":"t1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}},
			{"id":"t2","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"t1","content":"r1"},
		{"role":"tool","tool_call_id":"t2","content":[{"type":"text","text":"r2"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]},
		{"role":"user","content":"next"}]}`
	jsonEqual(t, []byte(want), out)

	back, _ := decode(t, out)
	if len(back.Messages) != 3 || len(back.Messages[1].Content) != 2 || back.Messages[2].Content[0].Text != "next" {
		t.Errorf("re-decode merged wrongly: %+v", back.Messages)
	}
}

func TestEncodeRequestForeign(t *testing.T) {
	max := 100
	req := &ir.Request{
		Model:  "m",
		System: []ir.Block{{Type: ir.BlockText, Text: "sys"}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "<context/>"}, {Type: ir.BlockText, Text: "task"}}},
		},
		MaxOutputTokens: &max,
		Stop:            []string{"x"},
		Stream:          true,
		Reasoning:       &ir.Reasoning{Enabled: true, Effort: "low"},
		Tools:           []ir.Tool{{Kind: ir.ToolFreeform, Name: "patch"}},
		ToolChoice:      &ir.ToolChoice{Mode: "tool", Name: "patch"},
		Extra:           ir.Extra{Origin: "anthropic", Fields: ir.RawFields{"metadata": json.RawMessage(`{}`)}},
	}
	out, err := codec.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"},
		{"role":"assistant","content":"ok"},{"role":"user","content":"<context/>\n\ntask"}],
		"tools":[{"type":"custom","custom":{"name":"patch"}}],
		"tool_choice":{"type":"custom","custom":{"name":"patch"}},
		"max_completion_tokens":100,"stop":["x"],"reasoning_effort":"low",
		"stream":true,"stream_options":{"include_usage":true}}`
	jsonEqual(t, []byte(want), out)
}

func TestSniff(t *testing.T) {
	body := []byte(`{"model":"m1","stream":true,"prompt_cache_key":"ck","safety_identifier":"sid",
		"messages":[{"role":"system","content":"S"},{"role":"user","content":"U"},{"role":"assistant","content":"A"}]}`)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("x-session-id", "sess")
	route := codec.Routes()[0]
	in, err := codec.Sniff(r, route, body)
	if err != nil {
		t.Fatal(err)
	}
	if in.Model != "m1" || !in.Stream || in.Op != protocol.OpGenerate {
		t.Errorf("inbound = %+v", in)
	}
	s := in.Session
	if s.SessionID != "sess" || s.CacheKey != "ck" || s.UserTag != "sid" || s.PrefixHash == "" {
		t.Errorf("session = %+v", s)
	}

	// Same prompt with parts, cache markers and a different later turn hashes alike.
	alt := []byte(`{"model":"m2","messages":[{"role":"system","content":[{"type":"text","text":"S","cache_control":{"type":"ephemeral"}}]},
		{"role":"user","content":[{"type":"text","text":"U"}]},{"role":"assistant","content":"B"}]}`)
	in2, _ := codec.Sniff(r, route, alt)
	if in2.Session.PrefixHash != s.PrefixHash {
		t.Error("prefix hash depends on cache_control or content form")
	}
	other := []byte(`{"messages":[{"role":"system","content":"S"},{"role":"user","content":"V"}]}`)
	in3, _ := codec.Sniff(r, route, other)
	if in3.Session.PrefixHash == s.PrefixHash {
		t.Error("prefix hash ignores first user message")
	}
	if _, err := codec.Sniff(r, route, []byte(`{`)); err == nil {
		t.Error("invalid JSON accepted")
	}
	if in, err := codec.Sniff(r, codec.Routes()[1], nil); err != nil || in.Op != protocol.OpModels {
		t.Errorf("models sniff = %+v, %v", in, err)
	}
}

func TestSetModelByteExact(t *testing.T) {
	body := []byte("{\n  \"messages\" : [ {\"role\":\"user\",\"content\":\"hi\"} ],\n  \"model\":   \"old\",\n  \"z\":1.50\n}")
	out, err := codec.SetModel(body, "new/model")
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"messages\" : [ {\"role\":\"user\",\"content\":\"hi\"} ],\n  \"model\":   \"new/model\",\n  \"z\":1.50\n}"
	if string(out) != want {
		t.Errorf("got %q", out)
	}
}

func TestStripOpaque(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"x","reasoning_details":[]},` +
		`{"role":"assistant","content":"a","reasoning_content":"r","reasoning_details":[{"type":"reasoning.encrypted","data":"e"}]},` +
		`{"role":"assistant","content":"b","reasoning_details":[{"type":"reasoning.text","text":"t"}]}],"model":"m"}`)
	out, n, err := codec.StripOpaque(body)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	want := `{"messages":[{"role":"user","content":"x","reasoning_details":[]},` +
		`{"role":"assistant","content":"a","reasoning_content":"r"},` +
		`{"role":"assistant","content":"b"}],"model":"m"}`
	if string(out) != want {
		t.Errorf("got %s", out)
	}
}

func TestEncodeModels(t *testing.T) {
	ct, b := codec.EncodeModels(nil, []ir.ModelInfo{{ID: "a", Created: 5, OwnedBy: "x"}, {ID: "b"}})
	if ct != "application/json" {
		t.Error(ct)
	}
	jsonEqual(t, []byte(`{"object":"list","data":[{"id":"a","object":"model","created":5,"owned_by":"x"},{"id":"b","object":"model","created":0,"owned_by":"system"}]}`), b)
}

func TestCountTokens(t *testing.T) {
	req, _ := decode(t, []byte(`{"messages":[{"role":"user","content":"hello world, this is a test"}]}`))
	b, err := codec.CountTokens(req)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		InputTokens int `json:"input_tokens"`
	}
	if json.Unmarshal(b, &v) != nil || v.InputTokens <= 0 {
		t.Errorf("count = %s", b)
	}
	if codec.UpstreamPath(req, protocol.Inbound{}) != "/chat/completions" {
		t.Error("upstream path")
	}
}
