package anthropic

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
)

var c Codec

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
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
		t.Errorf("JSON differs\nwant: %s\ngot:  %s", want, got)
	}
}

func decode(t *testing.T, body []byte) (*ir.Request, ir.Needs) {
	t.Helper()
	req, needs, err := c.DecodeRequest(nil, protocol.Inbound{}, body)
	if err != nil {
		t.Fatal(err)
	}
	return req, needs
}

func TestRegistered(t *testing.T) {
	got, ok := protocol.Get("anthropic")
	if !ok || got.Name() != "anthropic" {
		t.Fatal("codec not registered")
	}
}

func TestRequestRoundTrip(t *testing.T) {
	cases := map[string]string{
		"agent fixture":       string(fixture(t, "request_agent.json")),
		"string system":       `{"model":"m","max_tokens":10,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`,
		"empty system array":  `{"model":"m","system":[],"messages":[]}`,
		"empty content array": `{"model":"m","messages":[{"role":"user","content":[]}]}`,
		"adaptive thinking":   `{"model":"m","messages":[],"thinking":{"type":"adaptive"},"output_config":{"effort":"max","format":{"type":"json_schema"}}}`,
		"disabled thinking":   `{"model":"m","messages":[],"thinking":{"type":"disabled"}}`,
		"effort only":         `{"model":"m","messages":[],"output_config":{"effort":"low"}}`,
		"output_config only":  `{"model":"m","messages":[],"output_config":{"format":{"type":"json_schema"}}}`,
		"future thinking":     `{"model":"m","messages":[],"thinking":{"type":"interleaved","budget_tokens":5,"display":"omitted"}}`,
		"stream false":        `{"model":"m","messages":[],"stream":false,"stop_sequences":[],"top_k":5,"top_p":0.9}`,
		"tool choice":         `{"model":"m","messages":[],"tool_choice":{"type":"tool","name":"Read"}}`,
		"custom tool":         `{"model":"m","messages":[],"tools":[{"type":"custom","name":"x","input_schema":{"type":"object"},"strict":true,"defer_loading":false}]}`,
		"unknown block":       `{"model":"m","messages":[{"role":"user","content":[{"type":"search_result","source":"s","title":"t","content":[]}]}]}`,
		"image url":           `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://x/y.png"}},{"type":"document","source":{"type":"file","file_id":"file_1"}}]}]}`,
		"claude code fixture": string(fixture(t, "claude_code_request.json")),
		"empty signature":     `{"model":"m","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":""}]}]}`,
		"sealed opaque":       `{"model":"m","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"prs1.abc"},{"type":"redacted_thinking","data":"prs1.def"}]}]}`,
		"empty tool result":   `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[],"is_error":false}]}]}`,
		"odd cache_control":   `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"persistent","scope":"org"}}]}]}`,
		"message extras":      `{"model":"m","messages":[{"role":"user","content":"x","name":"bob"}],"mcp_servers":[{"type":"url","url":"https://m"}],"cache_control":{"type":"ephemeral"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			req, _ := decode(t, []byte(body))
			out, err := c.EncodeRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			jsonEqual(t, []byte(body), out)
		})
	}
}

func TestDecodeRequestFields(t *testing.T) {
	req, _ := decode(t, fixture(t, "request_agent.json"))
	if len(req.System) != 3 || !strings.HasPrefix(req.System[0].Text, "x-anthropic-billing-header") {
		t.Fatalf("system order lost: %+v", req.System)
	}
	if req.System[1].Cache == nil || req.System[1].Cache.TTL != "1h" {
		t.Errorf("system[1] cache = %+v", req.System[1].Cache)
	}
	r := req.Reasoning
	if r == nil || !r.Enabled || r.Budget == nil || *r.Budget != 31999 || r.Effort != "high" {
		t.Errorf("reasoning = %+v", r)
	}
	if req.ParallelTools == nil || !*req.ParallelTools {
		t.Errorf("parallel tools = %v", req.ParallelTools)
	}
	asst := req.Messages[1].Content
	if asst[0].Opaque == nil || asst[0].Opaque.Kind != "signature" || string(asst[0].Opaque.Data) != `"EqQBCkYIBRgCKkB2c2lnbmF0dXJlLWJ5dGVz"` {
		t.Errorf("signature = %+v", asst[0].Opaque)
	}
	if asst[1].Type != ir.BlockOpaque || asst[1].Opaque.Kind != "redacted_thinking" {
		t.Errorf("redacted = %+v", asst[1])
	}
	srv := req.Messages[3].Content
	if srv[0].Type != ir.BlockServerTool || srv[0].Tool.Name != "web_search" || srv[1].Type != ir.BlockServerTool {
		t.Errorf("server blocks = %+v", srv[:2])
	}
	if web := req.Tools[2]; web.Kind != ir.ToolServer || web.ServerType != "web_search_20250305" || web.Cache.TTL != "1h" {
		t.Errorf("web tool = %+v", web)
	}
	if !req.Tools[1].DeferLoading {
		t.Error("defer_loading lost")
	}
	if _, ok := req.Extra.Get("anthropic", "context_management"); !ok {
		t.Error("context_management not kept")
	}
}

func TestSealed(t *testing.T) {
	req, needs := decode(t, []byte(`{"model":"m","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"prs1.abc"},{"type":"redacted_thinking","data":"prs1.def"},{"type":"thinking","thinking":"u","signature":"native"}]}]}`))
	bs := req.Messages[0].Content
	if !bs[0].Opaque.Sealed || !bs[1].Opaque.Sealed || bs[2].Opaque.Sealed {
		t.Errorf("sealed flags = %v %v %v", bs[0].Opaque.Sealed, bs[1].Opaque.Sealed, bs[2].Opaque.Sealed)
	}
	if !needs.OpaqueState {
		t.Error("OpaqueState need not set")
	}
}

func TestEncodeForeignOpaque(t *testing.T) {
	seal := json.RawMessage(`"prs1.xyz"`)
	req := &ir.Request{Model: "m", Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{
		{Type: ir.BlockThinking, Text: "a", Opaque: &ir.OpaqueState{Kind: "encrypted_content", Data: seal, Sealed: true}},
		{Type: ir.BlockThinking, Text: "b", Opaque: &ir.OpaqueState{Kind: "thought_signature", Data: json.RawMessage(`"g"`)}},
		{Type: ir.BlockOpaque, Opaque: &ir.OpaqueState{Kind: "encrypted_content", Data: seal, Sealed: true}},
		{Type: ir.BlockOpaque, Opaque: &ir.OpaqueState{Kind: "compaction", Data: json.RawMessage(`"c"`)}},
		{Type: ir.BlockRaw, Extra: ir.Extra{Origin: "openai-responses", Fields: ir.RawFields{"type": json.RawMessage(`"x"`)}}},
		{Type: ir.BlockText, Text: "ok"},
	}}}}
	out, _ := c.EncodeRequest(req)
	want := `{"model":"m","messages":[{"role":"assistant","content":[
		{"type":"thinking","thinking":"a","signature":"prs1.xyz"},
		{"type":"thinking","thinking":"b"},
		{"type":"redacted_thinking","data":"prs1.xyz"},
		{"type":"text","text":"ok"}]}]}`
	jsonEqual(t, []byte(want), out)
}

func TestNeeds(t *testing.T) {
	_, n := decode(t, fixture(t, "request_agent.json"))
	want := ir.Needs{
		ExplicitCache: true, Cache1h: true, FunctionTools: true,
		ServerTools: []string{"web_search_20250305"}, DeferredTools: true,
		Reasoning: true, Images: true, Documents: true, ToolResultImgs: true,
		OpaqueState: true, InputTokensEst: n.InputTokensEst,
	}
	if !reflect.DeepEqual(n, want) {
		t.Errorf("needs\n got %+v\nwant %+v", n, want)
	}
	if n.InputTokensEst < 200 || n.InputTokensEst > 4000 {
		t.Errorf("InputTokensEst = %d", n.InputTokensEst)
	}
	_, n = decode(t, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if n.ExplicitCache || n.Reasoning || n.FunctionTools || n.OpaqueState || n.InputTokensEst != 1 {
		t.Errorf("plain needs = %+v", n)
	}
}

func TestSniff(t *testing.T) {
	route := protocol.Route{Method: "POST", Path: "/v1/messages", Op: protocol.OpGenerate}
	body := fixture(t, "request_agent.json")
	r, _ := http.NewRequest("POST", "/v1/messages?beta=true", nil)
	r.Header.Set("x-claude-code-agent-id", "agent-7")
	in, err := c.Sniff(r, route, body)
	if err != nil {
		t.Fatal(err)
	}
	if in.Model != "claude-opus-4-6" || !in.Stream || in.Op != protocol.OpGenerate {
		t.Errorf("inbound = %+v", in)
	}
	if in.Session.SessionID != "8b0c2a6e-5d3f-4c1a-9e7b-2f4d6a8c0e12" || in.Session.AgentID != "agent-7" {
		t.Errorf("session = %+v", in.Session)
	}
	r.Header.Set("X-Claude-Code-Session-Id", "hdr")
	in, _ = c.Sniff(r, route, body)
	if in.Session.SessionID != "hdr" || in.Session.UserTag == "" {
		t.Errorf("header session = %+v", in.Session)
	}
	in, _ = c.Sniff(nil, route, []byte(`{"metadata":{"user_id":"user_ab_account_cd_session_ef12"}}`))
	if in.Session.SessionID != "ef12" {
		t.Errorf("legacy session = %q", in.Session.SessionID)
	}
}

func TestPrefixHash(t *testing.T) {
	route := protocol.Route{Op: protocol.OpGenerate}
	hash := func(body string) string {
		in, err := c.Sniff(nil, route, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return in.Session.PrefixHash
	}
	base := hash(`{"system":[{"type":"text","text":"S"}],"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>date A</system-reminder>"},{"type":"text","text":"fix it"}]},{"role":"assistant","content":"ok"}]}`)
	same := []string{
		`{"system":[{"type":"text","text":"S","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>date B</system-reminder>"},{"type":"text","text":"fix it"}]}]}`,
		`{"system":[{"type":"text","text":"S"}],"messages":[{"role":"user","content":[{"type":"text","text":"fix it","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"later"}]}`,
	}
	for _, b := range same {
		if h := hash(b); h != base {
			t.Errorf("hash changed for %s", b)
		}
	}
	if hash(`{"system":[{"type":"text","text":"S"}],"messages":[{"role":"user","content":"fix that"}]}`) == base {
		t.Error("hash ignores user text")
	}
	if base == "" || hash(`{"messages":[]}`) != "" {
		t.Error("empty prefix should hash to empty")
	}
}

func TestSetModel(t *testing.T) {
	body := fixture(t, "request_agent.json")
	out, err := c.SetModel(body, "claude-sonnet-4-5-20250929")
	if err != nil {
		t.Fatal(err)
	}
	old, repl := []byte(`"claude-opus-4-6"`), []byte(`"claude-sonnet-4-5-20250929"`)
	if want := bytes.Replace(body, old, repl, 1); !bytes.Equal(out, want) {
		t.Errorf("SetModel changed more than the model value")
	}
}

func TestStripOpaque(t *testing.T) {
	body := []byte(`{"model":"m",
  "messages": [
    {"role":"user","content":[{"type":"text","text":"thinking about it"}]},
    {"role":"assistant","content":[{"type":"thinking","thinking":"a","signature":"s"},  {"type":"redacted_thinking","data":"d"}, {"type":"text","text":"x"}]},
    {"role":"assistant","content":"plain"},
    {"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n","input":{}},{"type":"thinking","thinking":"b","signature":"s2"}]}
  ]}`)
	out, n, err := c.StripOpaque(body)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("removed = %d", n)
	}
	want := []byte(`{"model":"m",
  "messages": [
    {"role":"user","content":[{"type":"text","text":"thinking about it"}]},
    {"role":"assistant","content":[{"type":"text","text":"x"}]},
    {"role":"assistant","content":"plain"},
    {"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n","input":{}}]}
  ]}`)
	jsonEqual(t, want, out)
	if !bytes.Contains(out, []byte(`{"type":"tool_use","id":"t","name":"n","input":{}}`)) ||
		!bytes.HasPrefix(out, []byte("{\"model\":\"m\",\n  \"messages\": [\n")) {
		t.Errorf("untouched bytes changed:\n%s", out)
	}
}

func TestResponseRoundTrip(t *testing.T) {
	body := fixture(t, "response.json")
	resp, err := c.DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	u := resp.Usage
	if u.Input != 10 || u.CacheRead != 5000 || u.CacheWrite5m != 100 || u.CacheWrite1h != 200 || u.Output != 42 || len(u.Raw) == 0 {
		t.Errorf("usage = %+v", u)
	}
	if resp.StopReason != ir.StopToolUse || resp.Content[1].Type != ir.BlockOpaque {
		t.Errorf("resp = %+v", resp)
	}
	out, err := c.EncodeResponse(nil, resp)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, body, out)
}

func TestUsageFallback(t *testing.T) {
	resp, _ := c.DecodeResponse([]byte(`{"usage":{"input_tokens":3,"cache_creation_input_tokens":70,"output_tokens":1}}`))
	if resp.Usage.CacheWrite5m != 70 || resp.Usage.CacheWrite1h != 0 || resp.Usage.TotalInput() != 73 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestEncodeForeignResponse(t *testing.T) {
	resp := &ir.Response{
		Content:    []ir.Block{{Type: ir.BlockText, Text: "hi"}},
		StopReason: ir.StopContentFilter,
		Usage:      ir.Usage{Input: 5, CacheRead: 7, Output: 2, Raw: json.RawMessage(`{"prompt_tokens":12}`)},
	}
	out, _ := c.EncodeResponse(&ir.Request{Model: "m"}, resp)
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if !strings.HasPrefix(got["id"].(string), "msg_") || got["model"] != "m" || got["stop_reason"] != "refusal" {
		t.Errorf("response = %s", out)
	}
	jsonEqual(t, []byte(`{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens":2}`), marshal(got["usage"]))
}

func TestCountTokens(t *testing.T) {
	req, _ := decode(t, []byte(`{"model":"m","messages":[{"role":"user","content":"12345678"}]}`))
	out, _ := c.CountTokens(req)
	jsonEqual(t, []byte(`{"input_tokens":2}`), out)
}

func TestEncodeModels(t *testing.T) {
	ct, body := c.EncodeModels(nil, []ir.ModelInfo{
		{ID: "claude-a", DisplayName: "Claude A", Created: 1700000000},
		{ID: "claude-b"},
	})
	if ct != "application/json" {
		t.Error(ct)
	}
	jsonEqual(t, []byte(`{"data":[
		{"type":"model","id":"claude-a","display_name":"Claude A","created_at":"2023-11-14T22:13:20Z"},
		{"type":"model","id":"claude-b","display_name":"claude-b","created_at":"1970-01-01T00:00:00Z"}],
		"has_more":false,"first_id":"claude-a","last_id":"claude-b"}`), body)
	_, body = c.EncodeModels(nil, nil)
	jsonEqual(t, []byte(`{"data":[],"has_more":false,"first_id":null,"last_id":null}`), body)
}

func TestUpstreamPath(t *testing.T) {
	if p := c.UpstreamPath(nil, protocol.Inbound{Op: protocol.OpGenerate}); p != "/v1/messages" {
		t.Error(p)
	}
	if p := c.UpstreamPath(nil, protocol.Inbound{Op: protocol.OpCountTokens}); p != "/v1/messages/count_tokens" {
		t.Error(p)
	}
}

func TestTypedTools(t *testing.T) {
	body := `{"model":"m","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":[{"type":"server_tool_use","id":"s1","name":"tool_search","input":{}}]}],
	"tools":[
		{"type":"bash_20250124","name":"bash"},
		{"type":"text_editor_20250728","name":"str_replace_based_edit_tool","max_characters":10000},
		{"type":"computer_20250124","name":"computer","display_width_px":1024,"display_height_px":768},
		{"type":"memory_20250818","name":"memory"},
		{"type":"web_fetch_20250910","name":"web_fetch"},
		{"type":"code_execution_20250825","name":"code_execution"},
		{"type":"tool_search_tool_regex_20251119","name":"tool_search"}]}`
	req, needs := decode(t, []byte(body))
	out, _ := c.EncodeRequest(req)
	jsonEqual(t, []byte(body), out)

	wantSchema := map[string]string{
		"bash":                        `"restart"`,
		"str_replace_based_edit_tool": `"old_str"`,
		"computer":                    `{"type":"object"}`,
		"memory":                      `{"type":"object"}`,
	}
	for _, tool := range req.Tools[:4] {
		if tool.Kind != ir.ToolFunction || tool.ServerType != "" ||
			!strings.Contains(string(tool.InputSchema), wantSchema[tool.Name]) {
			t.Errorf("client tool %s = %+v", tool.Name, tool)
		}
	}
	for _, tool := range req.Tools[4:] {
		if tool.Kind != ir.ToolServer {
			t.Errorf("server tool %s kind = %s", tool.Name, tool.Kind)
		}
	}
	want := []string{"web_fetch_20250910", "code_execution_20250825", "tool_search_tool_regex_20251119"}
	if !reflect.DeepEqual(needs.ServerTools, want) || !needs.FunctionTools {
		t.Errorf("needs = %+v", needs)
	}
	// The trailing server_tool_use has no result yet: the turn continues it.
	if !reflect.DeepEqual(needs.ServerToolsRequired, []string{"tool_search_tool_regex_20251119"}) {
		t.Errorf("required = %v", needs.ServerToolsRequired)
	}

	// Without its origin Extra (as another codec would see it) the tool is a
	// plain function tool with the synthesised schema.
	foreign := req.Tools[0]
	foreign.Extra = ir.Extra{}
	enc, _ := encodeTool(&foreign)
	jsonEqual(t, []byte(`{"name":"bash","input_schema":`+string(bashSchema)+`}`), enc)
}

func TestStripOpaqueEmptied(t *testing.T) {
	think := `{"role":"assistant","content":[{"type":"thinking","thinking":"a","signature":"s"}]}`
	cases := []struct{ name, in, want string }{
		{"trailing assistant dropped",
			`{"messages":[{"role":"user","content":"q"},` + think + `]}`,
			`{"messages":[{"role":"user","content":"q"}]}`},
		{"between users gets placeholder",
			`{"messages":[{"role":"user","content":"q"},` + think + `,{"role":"user","content":"more"}]}`,
			`{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":[{"type":"text","text":"(no content)"}]},{"role":"user","content":"more"}]}`},
		{"between differing roles dropped",
			`{"messages":[{"role":"user","content":"q"},` + think + `,{"role":"assistant","content":"a"}]}`,
			`{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":"a"}]}`},
		{"only message gets placeholder",
			`{"messages":[` + think + `]}`,
			`{"messages":[{"role":"assistant","content":[{"type":"text","text":"(no content)"}]}]}`},
		{"already empty untouched",
			`{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":[]}]}`,
			`{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":[]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := c.StripOpaque([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			jsonEqual(t, []byte(tc.want), out)
		})
	}
}

// A real Claude Code request: custom tools only, one deferred, cache
// breakpoints, adaptive thinking, and history thinking with an empty
// signature, which is no upstream state.
func TestClaudeCodeRequest(t *testing.T) {
	req, needs := decode(t, fixture(t, "claude_code_request.json"))
	if !needs.FunctionTools || !needs.DeferredTools || !needs.ExplicitCache || !needs.Reasoning {
		t.Errorf("needs = %+v", needs)
	}
	if needs.OpaqueState || len(needs.ServerTools) != 0 || len(needs.ServerToolsRequired) != 0 {
		t.Errorf("needs = %+v", needs)
	}
	thinking := 0
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockThinking {
				thinking++
				if b.Opaque != nil {
					t.Errorf("empty signature decoded as state: %+v", b.Opaque)
				}
			}
		}
	}
	if thinking == 0 {
		t.Fatal("fixture has no thinking block")
	}
}
