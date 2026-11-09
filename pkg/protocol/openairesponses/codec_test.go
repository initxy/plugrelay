package openairesponses

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

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/tidwall/gjson"
)

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("%v: %s", err, a)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return reflect.DeepEqual(x, y)
}

func generateRoute() protocol.Route {
	return protocol.Route{Method: http.MethodPost, Path: "/v1/responses", Op: protocol.OpGenerate}
}

func TestRegistered(t *testing.T) {
	c, ok := protocol.Get(Name)
	if !ok {
		t.Fatal("codec not registered")
	}
	if c.(protocol.StickyHeader).StickyHeaderName() != TurnStateHeader {
		t.Fatal("sticky header")
	}
	caps := c.Capabilities()
	if caps.Cache != ir.CacheKeyed || !caps.ServerState || !caps.Compaction || caps.MaxTokensField != "max_output_tokens" ||
		len(caps.EchoFields) != 1 || caps.EchoFields[0] != "encrypted_content" || !caps.FreeformTools || !caps.DeferredTools || !caps.Namespaces {
		t.Fatalf("caps %+v", caps)
	}
}

func TestAgentRequestRoundTrip(t *testing.T) {
	body := readFile(t, "testdata/request_agent.json")
	c := Codec{}
	hr := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	in, err := c.Sniff(hr, generateRoute(), body)
	if err != nil {
		t.Fatal(err)
	}
	req, needs, err := c.DecodeRequest(hr, in, body)
	if err != nil {
		t.Fatal(err)
	}

	// System: instructions plus the leading developer message.
	if len(req.System) != 2 || req.System[0].Text != "You are a coding agent running in a terminal." ||
		!strings.HasPrefix(req.System[1].Text, "<permissions instructions>") {
		t.Fatalf("system %+v", req.System)
	}
	roles := ""
	for _, m := range req.Messages {
		roles += string(m.Role[0])
	}
	if roles != "uauauauau" {
		t.Fatalf("message sides %q", roles)
	}
	asst := req.Messages[1].Content
	if asst[0].Type != ir.BlockThinking || asst[0].Text != "**Planning**\n\nI will patch.\n\nThen call the tool." ||
		asst[0].Opaque == nil || asst[0].Opaque.Kind != kindEncrypted || asst[0].Opaque.Sealed {
		t.Fatalf("reasoning %+v", asst[0])
	}
	if asst[2].Type != ir.BlockToolUse || asst[2].Tool.Kind != ir.ToolFreeform || !strings.HasPrefix(asst[2].Tool.Text, "*** Begin Patch") {
		t.Fatalf("custom tool call %+v", asst[2])
	}
	user := req.Messages[2].Content
	if user[0].Type != ir.BlockToolResult || !user[0].Result.Freeform {
		t.Fatalf("custom tool output %+v", user[0])
	}
	var sealed, compaction, raws int
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Opaque != nil && b.Opaque.Sealed {
				sealed++
			}
			if b.Type == ir.BlockOpaque && b.Opaque.Kind == kindCompaction {
				compaction++
			}
			if b.Type == ir.BlockRaw {
				raws++
			}
		}
	}
	if sealed != 1 || compaction != 1 || raws != 4 {
		t.Fatalf("sealed=%d compaction=%d raw=%d", sealed, compaction, raws)
	}
	if req.Reasoning == nil || req.Reasoning.Effort != "high" || !req.Reasoning.IncludeEncrypted || req.Verbosity != "low" ||
		req.CacheKey != "019a-thread" || req.ServiceTier != "priority" || req.Store == nil || *req.Store {
		t.Fatalf("params %+v %+v", req, req.Reasoning)
	}
	if !needs.FunctionTools || !needs.FreeformTools || !needs.DeferredTools || !needs.Reasoning || !needs.Images ||
		!needs.ToolResultImgs || !needs.OpaqueState || !needs.Compaction || needs.ServerState || needs.InputTokensEst == 0 {
		t.Fatalf("needs %+v", needs)
	}
	if strings.Join(needs.ServerTools, ",") != "web_search,tool_search" || len(needs.ServerToolsRequired) != 0 {
		t.Fatalf("server tools %v required %v", needs.ServerTools, needs.ServerToolsRequired)
	}
	var names []string
	for _, tool := range req.Tools {
		names = append(names, tool.Namespace+"/"+tool.Name)
	}
	if strings.Join(names, ",") != "/get_weather,/apply_patch,mcp__weather/get_weather,/web_search,/tool_search,/cal_list,/late_tool" {
		t.Fatalf("tools %v", names)
	}
	if call := req.Messages[3].Content[0].Tool; call == nil || call.Namespace != "mcp__weather" {
		t.Fatalf("namespaced call %+v", req.Messages[3].Content)
	}

	out, err := c.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, body, out) {
		t.Fatalf("round trip differs:\n%s\n---\n%s", body, out)
	}
	// Unknown items survive byte for byte.
	for _, it := range gjson.GetBytes(body, "input").Array() {
		switch it.Get("type").Str {
		case "tool_search_call", "tool_search_output", "local_shell_call", "additional_tools":
			if !bytes.Contains(out, []byte(it.Raw)) {
				t.Fatalf("raw item not verbatim: %s", it.Raw)
			}
		}
	}
}

func TestDecodeRequestVariants(t *testing.T) {
	c := Codec{}
	cases := []string{
		`{"model":"m","input":"hello","stream":false}`,
		`{"model":"m","input":[{"role":"user","content":"hi"}],"reasoning":{"effort":3},"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[]},"include":[],"metadata":{"a":"b"},"truncation":"auto"}`,
		`{"model":"m","input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"sys"}]},{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}],"tool_choice":{"type":"custom","name":"apply_patch"},"tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}}],"previous_response_id":"resp_prev","max_output_tokens":100,"temperature":0.5,"top_p":1}`,
		`{"model":"m","input":[{"type":"function_call","call_id":"c","name":"f","arguments":"not json"},{"type":"function_call_output","call_id":"c","output":"ok"},{"type":"message","role":"user","content":[{"type":"input_file","file_data":"data:application/pdf;base64,JVBERi0=","filename":"a.pdf"}]}],"include":["reasoning.encrypted_content","web_search_call.action.sources"]}`,
	}
	for _, body := range cases {
		req, n, err := c.DecodeRequest(nil, protocol.Inbound{Op: protocol.OpGenerate}, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		out, err := c.EncodeRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		if !jsonEqual(t, []byte(body), out) {
			t.Errorf("round trip differs:\n%s\n%s", body, out)
		}
		_ = n
	}
	req, n, _ := c.DecodeRequest(nil, protocol.Inbound{}, []byte(cases[2]))
	if !n.ServerState || req.Previous != "resp_prev" || req.ToolChoice.Mode != "tool" || req.Messages[0].Content[0].Text != "no" {
		t.Fatalf("variant 2: %+v %+v", n, req)
	}
	if _, _, err := c.DecodeRequest(nil, protocol.Inbound{}, []byte(`{"input":5}`)); err == nil {
		t.Fatal("want error for numeric input")
	}
}

func TestEncodeForeignRequest(t *testing.T) {
	max := 512
	req := &ir.Request{
		Model:  "gpt",
		System: []ir.Block{{Type: ir.BlockText, Text: "a"}, {Type: ir.BlockText, Text: "b"}},
		Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "q"}, {Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: "AAA"}}}},
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockThinking, Text: "hmm", Opaque: &ir.OpaqueState{Kind: "signature", Data: []byte(`"sig"`)}},
				{Type: ir.BlockText, Text: "calling"},
				{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "t1", Name: "f", Kind: ir.ToolFunction, Input: []byte(`{"x":1}`)}},
			}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, Result: &ir.ToolResult{ToolUseID: "t1", Content: []ir.Block{{Type: ir.BlockText, Text: "r"}}}}}},
		},
		Tools:           []ir.Tool{{Kind: ir.ToolFunction, Name: "f", InputSchema: []byte(`{"type":"object"}`)}},
		ToolChoice:      &ir.ToolChoice{Mode: "any"},
		MaxOutputTokens: &max,
		Reasoning:       &ir.Reasoning{Enabled: true, Effort: "low", IncludeEncrypted: true},
		Stream:          true,
	}
	out, err := Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"gpt","instructions":"a\n\nb","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"q"},{"type":"input_image","image_url":"data:image/png;base64,AAA"}]},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"hmm"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"calling"}]},` +
		`{"type":"function_call","call_id":"t1","name":"f","arguments":"{\"x\":1}"},` +
		`{"type":"function_call_output","call_id":"t1","output":"r"}],` +
		`"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"tool_choice":"required",` +
		`"reasoning":{"effort":"low"},"stream":true,"include":["reasoning.encrypted_content"],"max_output_tokens":512}`
	if string(out) != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
}

func TestSetModelAndStripOpaque(t *testing.T) {
	c := Codec{}
	body := readFile(t, "testdata/request_agent.json")
	out, err := c.SetModel(body, "other-model")
	if err != nil {
		t.Fatal(err)
	}
	if want := bytes.Replace(body, []byte(`"model": "gpt-5.5"`), []byte(`"model":"other-model"`), 1); !bytes.Equal(out, want) &&
		!bytes.Equal(out, bytes.Replace(body, []byte(`"gpt-5.5"`), []byte(`"other-model"`), 1)) {
		t.Fatalf("SetModel touched other bytes:\n%s", out)
	}

	stripped, n, err := c.StripOpaque(body)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("removed %d, want 3", n)
	}
	// Expected: the original bytes with each removed element (and one
	// separator) cut out.
	want := body
	items := gjson.GetBytes(body, "input").Array()
	for i := len(items) - 1; i >= 0; i-- {
		typ := items[i].Get("type").Str
		if typ != "reasoning" && typ != "compaction" {
			continue
		}
		start := items[i].Index
		end := start + len(items[i].Raw)
		// Remove the preceding ",\n    " separator.
		sep := bytes.LastIndexByte(want[:start], ',')
		want = append(append([]byte{}, want[:sep]...), want[end:]...)
	}
	if !bytes.Equal(stripped, want) {
		t.Fatalf("StripOpaque output not byte-exact:\n%s", stripped)
	}
	if !gjson.ValidBytes(stripped) || len(gjson.GetBytes(stripped, "input").Array()) != len(items)-3 {
		t.Fatal("stripped body invalid")
	}
	if again, n, _ := c.StripOpaque(stripped); n != 0 || !bytes.Equal(again, stripped) {
		t.Fatal("second strip changed body")
	}
	first := []byte(`{"input":[{"type":"reasoning","summary":[]},{"type":"message","role":"user","content":"x"}],"model":"m"}`)
	got, n, _ := c.StripOpaque(first)
	if n != 1 || string(got) != `{"input":[{"type":"message","role":"user","content":"x"}],"model":"m"}` {
		t.Fatalf("leading strip: %s", got)
	}
}

func TestSniff(t *testing.T) {
	body := readFile(t, "testdata/request_agent.json")
	hr := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	hr.Header.Set("session_id", "sess-1")
	hr.Header.Set("x-openai-subagent", "review")
	hr.Header.Set(TurnStateHeader, "tok")
	in, err := Codec{}.Sniff(hr, generateRoute(), body)
	if err != nil {
		t.Fatal(err)
	}
	s := in.Session
	if in.Model != "gpt-5.5" || !in.Stream || s.SessionID != "sess-1" || s.AgentID != "review" || s.TurnState != "tok" ||
		s.CacheKey != "019a-thread" || len(s.PrefixHash) != 32 {
		t.Fatalf("inbound %+v", in)
	}
	// The environment block and later turns do not affect the prefix hash.
	changed := bytes.Replace(body, []byte("<cwd>/repo</cwd>"), []byte("<cwd>/elsewhere</cwd>"), 1)
	changed = bytes.Replace(changed, []byte(`"thanks"`), []byte(`"thank you"`), 1)
	in2, _ := Codec{}.Sniff(hr, generateRoute(), changed)
	if in2.Session.PrefixHash != s.PrefixHash {
		t.Fatal("prefix hash depends on environment context or later turns")
	}
	in3, _ := Codec{}.Sniff(hr, generateRoute(), bytes.Replace(body, []byte("Use gofmt."), []byte("Use tabs."), 1))
	if in3.Session.PrefixHash == s.PrefixHash {
		t.Fatal("prefix hash ignores first user message")
	}
	hr2 := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	hr2.Header.Set("thread-id", "th")
	in4, _ := Codec{}.Sniff(hr2, protocol.Route{Path: "/v1/responses/compact", Op: protocol.OpGenerate}, []byte(`{"model":"m"}`))
	if in4.Session.SessionID != "th" || in4.Action != "compact" || Codec.UpstreamPath(Codec{}, nil, in4) != "/responses/compact" {
		t.Fatalf("inbound %+v", in4)
	}
	if Codec.UpstreamPath(Codec{}, nil, in) != "/responses" {
		t.Fatal("upstream path")
	}
}

func TestResponseRoundTrip(t *testing.T) {
	c := Codec{}
	body := readFile(t, "testdata/response_text.json")
	resp, err := c.DecodeResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "Bonjour" || resp.StopReason != ir.StopEndTurn ||
		resp.Usage.Input != 14 || resp.Usage.Output != 5 {
		t.Fatalf("resp %+v", resp)
	}
	out, err := c.EncodeResponse(&ir.Request{Model: "alias"}, resp)
	if err != nil {
		t.Fatal(err)
	}
	o := gjson.ParseBytes(out)
	item := o.Get("output.0")
	if o.Get("id").Str != resp.ID || o.Get("model").Str != "alias" || o.Get("status").Str != "completed" ||
		item.Get("id").Str != "msg_0e5e8f782e0f29fc006aec842bbf008194887bb1b2d09d47f1" || item.Get("phase").Str != "final_answer" ||
		item.Get("status").Str != "completed" || item.Get("content.0.text").Str != "Bonjour" ||
		o.Get("usage.input_tokens").Int() != 14 || o.Get("created_at").Int() != 1793885227 {
		t.Fatalf("encoded %s", out)
	}

	tool := &ir.Response{ID: "x", Content: []ir.Block{
		{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "c1", Name: "f", Kind: ir.ToolFunction, Input: []byte(`{}`)}},
	}, StopReason: ir.StopToolUse}
	out, _ = c.EncodeResponse(nil, tool)
	if fc := gjson.GetBytes(out, "output.0"); !strings.HasPrefix(fc.Get("id").Str, "fc_") || fc.Get("arguments").Str != "{}" {
		t.Fatalf("tool output %s", out)
	}

	inc, _ := c.DecodeResponse([]byte(`{"id":"r","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":4},"output_tokens":1}}`))
	if inc.StopReason != ir.StopContentFilter || inc.Usage.Input != 6 || inc.Usage.CacheRead != 4 {
		t.Fatalf("incomplete %+v", inc)
	}
	_, err = c.DecodeResponse([]byte(`{"id":"r","status":"failed","error":{"code":"rate_limit_exceeded","message":"slow. Please try again in 2s."}}`))
	var ge *ir.GwError
	if e, ok := err.(*ir.GwError); ok {
		ge = e
	}
	if ge == nil || ge.Kind != ir.ErrRateLimited || ge.RetryAfter != 2*time.Second {
		t.Fatalf("failed response err %v", err)
	}
}

func TestEncodeError(t *testing.T) {
	c := Codec{}
	cases := []struct {
		e      *ir.GwError
		status int
		typ    string
		code   string
	}{
		{&ir.GwError{Kind: ir.ErrContextTooLong, Message: "too long", TokensUsed: 300, TokensLimit: 200}, 400, "invalid_request_error", "context_length_exceeded"},
		{&ir.GwError{Kind: ir.ErrRateLimited, Message: "slow down", RetryAfter: 1500 * time.Millisecond}, 429, "rate_limit_error", "rate_limit_exceeded"},
		{&ir.GwError{Kind: ir.ErrQuotaExhausted, Message: "out"}, 429, "insufficient_quota", "insufficient_quota"},
		{&ir.GwError{Kind: ir.ErrQuotaExhausted, Message: "out", Code: "usage_limit_reached", ResetAt: time.Now().Add(time.Hour)}, 429, "usage_limit_reached", "usage_limit_reached"},
		{&ir.GwError{Kind: ir.ErrOverloaded, Message: "busy"}, 503, "server_error", "server_is_overloaded"},
		{&ir.GwError{Kind: ir.ErrContentFiltered, Message: "blocked"}, 400, "invalid_request_error", "content_filter"},
		{&ir.GwError{Kind: ir.ErrCapabilityMissing, Message: "no freeform tools"}, 400, "invalid_request_error", ""},
		{&ir.GwError{Kind: ir.ErrInternal, Message: "boom"}, 500, "server_error", "server_error"},
		{&ir.GwError{Kind: ir.ErrAuthentication, Message: "bad key"}, 401, "invalid_request_error", "invalid_api_key"},
		{&ir.GwError{Kind: ir.ErrNotFound, Message: "no model"}, 404, "invalid_request_error", "model_not_found"},
		{&ir.GwError{Kind: ir.ErrInvalidRequest, Message: "bad", Status: 422}, 422, "invalid_request_error", ""},
	}
	for _, tc := range cases {
		status, h, body := c.EncodeError(tc.e)
		e := gjson.GetBytes(body, "error")
		if status != tc.status || e.Get("type").Str != tc.typ || e.Get("code").Str != tc.code || e.Get("message").Str == "" {
			t.Errorf("%s: %d %s", tc.e.Kind, status, body)
		}
		if h.Get("Content-Type") != "application/json" {
			t.Errorf("%s: content type", tc.e.Kind)
		}
		switch tc.e.Kind {
		case ir.ErrRateLimited:
			if h.Get("Retry-After") != "2" || !strings.Contains(e.Get("message").Str, "try again in 1.500s") {
				t.Errorf("rate limit hints: %v %s", h, body)
			}
		case ir.ErrContextTooLong:
			if !strings.Contains(e.Get("message").Str, "300") {
				t.Errorf("context message %s", body)
			}
		}
		if tc.e.Code == "usage_limit_reached" && !e.Get("resets_at").Exists() {
			t.Errorf("usage limit without resets_at: %s", body)
		}
	}
	status, body := UpgradeRequired()
	if status != http.StatusUpgradeRequired || gjson.GetBytes(body, "error.message").Str == "" {
		t.Fatal("upgrade required")
	}
}

func TestDecodeError(t *testing.T) {
	c := Codec{}
	cases := []struct {
		status int
		header http.Header
		body   string
		kind   ir.ErrorKind
	}{
		{400, nil, `{"error":{"message":"Your input exceeds the context window of this model.","type":"invalid_request_error","code":"context_length_exceeded"}}`, ir.ErrContextTooLong},
		{429, http.Header{"Retry-After": {"3"}}, `{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`, ir.ErrRateLimited},
		{429, nil, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`, ir.ErrQuotaExhausted},
		{429, nil, `{"error":{"type":"usage_limit_reached","message":"limit","resets_at":1893456000}}`, ir.ErrQuotaExhausted},
		{503, nil, `{"error":{"message":"overloaded","code":"server_is_overloaded"}}`, ir.ErrOverloaded},
		{401, nil, `{"error":{"message":"无效的 API key。","type":"authentication_error","code":"invalid_api_key","param":null}}`, ir.ErrAuthentication},
		{404, nil, `{"error":{"message":"未知模型 nope/nope。","type":"not_found_error","code":"unknown_model","param":null}}`, ir.ErrNotFound},
		{400, nil, `{"error":{"message":"bad param","type":"invalid_request_error"}}`, ir.ErrInvalidRequest},
		{502, nil, `<html>bad gateway</html>`, ir.ErrUpstream},
	}
	for _, tc := range cases {
		e := c.DecodeError(tc.status, tc.header, []byte(tc.body))
		if e.Kind != tc.kind || e.Message == "" {
			t.Errorf("%s: got %s %q", tc.body, e.Kind, e.Message)
		}
	}
	e := c.DecodeError(429, http.Header{"Retry-After": {"3"}, "X-Request-Id": {"r"}, "Set-Cookie": {"x"}}, []byte(cases[1].body))
	if e.RetryAfter != 3*time.Second || e.Header.Get("X-Request-Id") != "r" || e.Header.Get("Set-Cookie") != "" || e.Code != "rate_limit_exceeded" {
		t.Fatalf("headers %+v", e)
	}
	e = c.DecodeError(429, nil, []byte(cases[3].body))
	if e.Code != "usage_limit_reached" || e.ResetAt.Unix() != 1893456000 || !e.NoRetry {
		t.Fatalf("usage limit %+v", e)
	}
}

func TestEncodeModels(t *testing.T) {
	models := []ir.ModelInfo{
		{ID: "gpt-5.5", DisplayName: "GPT-5.5", ContextWindow: 272000, Tools: true, Vision: true, Reasoning: true, Efforts: []string{"low", "medium", "high", "xhigh"}},
		{ID: "plain", Tools: false},
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.99.0", nil)
	ct, body := Codec{}.EncodeModels(r, models)
	if ct != "application/json" {
		t.Fatal(ct)
	}
	m := gjson.GetBytes(body, "models.0")
	if m.Get("slug").Str != "gpt-5.5" || m.Get("display_name").Str != "GPT-5.5" || m.Get("default_reasoning_level").Str != "medium" ||
		len(m.Get("supported_reasoning_levels").Array()) != 4 || m.Get("supported_reasoning_levels.0.description").Str == "" ||
		m.Get("context_window").Int() != 272000 || m.Get("truncation_policy.mode").Str != "tokens" ||
		m.Get("truncation_policy.limit").Int() == 0 || !m.Get("supports_parallel_tool_calls").Bool() ||
		m.Get("input_modalities.1").Str != "image" || m.Get("visibility").Str != "list" || m.Get("priority").Int() != 1 ||
		m.Get("shell_type").Str == "" || m.Get("base_instructions").Str == "" || !m.Get("support_verbosity").Exists() {
		t.Fatalf("catalog %s", body)
	}
	p := gjson.GetBytes(body, "models.1")
	if p.Get("default_reasoning_level").Exists() || len(p.Get("supported_reasoning_levels").Array()) != 0 || p.Get("context_window").Int() == 0 {
		t.Fatalf("plain model %s", p.Raw)
	}

	_, body = Codec{}.EncodeModels(httptest.NewRequest(http.MethodGet, "/v1/models", nil), models)
	if gjson.GetBytes(body, "object").Str != "list" || gjson.GetBytes(body, "data.0.id").Str != "gpt-5.5" ||
		gjson.GetBytes(body, "data.0.object").Str != "model" {
		t.Fatalf("list %s", body)
	}
}

func TestCountTokens(t *testing.T) {
	body := readFile(t, "testdata/request_agent.json")
	req, _, err := Codec{}.DecodeRequest(nil, protocol.Inbound{Op: protocol.OpCountTokens}, body)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := Codec{}.CountTokens(req)
	if gjson.GetBytes(out, "object").Str != "response.input_tokens" || gjson.GetBytes(out, "input_tokens").Int() < 100 {
		t.Fatalf("count %s", out)
	}
	if Codec.UpstreamPath(Codec{}, req, protocol.Inbound{Op: protocol.OpCountTokens}) != "/responses/input_tokens" {
		t.Fatal("count path")
	}
}

func TestNamespaceTools(t *testing.T) {
	c := Codec{}
	body := `{"model":"m","tools":[{"type":"function","name":"a","parameters":{}},` +
		`{"type":"namespace","name":"mcp__x","description":"X tools","tools":[{"type":"function","name":"f","parameters":{}},{"type":"custom","name":"g"}]},` +
		`{"type":"function","name":"b","parameters":{}},{"type":"namespace","name":"empty","tools":[]}],` +
		`"input":[{"type":"function_call","call_id":"c","name":"f","namespace":"mcp__x","arguments":"{}"},` +
		`{"type":"additional_tools","tools":[{"type":"function","name":"late","parameters":{}},{"type":"namespace","name":"mcp__x","tools":[{"type":"function","name":"f","parameters":{}},{"type":"function","name":"h","parameters":{}}]}]}]}`
	req, n, err := c.DecodeRequest(nil, protocol.Inbound{Op: protocol.OpGenerate}, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tool := range req.Tools {
		got = append(got, tool.Namespace+"/"+tool.Name+":"+string(tool.Kind))
	}
	want := "/a:function,mcp__x/f:function,mcp__x/g:freeform,/b:function,/empty:server,/late:function,mcp__x/h:function"
	if strings.Join(got, ",") != want {
		t.Fatalf("tools %v", got)
	}
	if strings.Join(n.ServerTools, ",") != "namespace" || !n.FreeformTools {
		t.Fatalf("needs %+v", n)
	}
	out, err := c.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, []byte(body), out) {
		t.Fatalf("round trip differs:\n%s\n%s", body, out)
	}
}

func TestForcedServerTool(t *testing.T) {
	c := Codec{}
	_, n, err := c.DecodeRequest(nil, protocol.Inbound{}, []byte(`{"model":"m","tools":[{"type":"web_search"},{"type":"tool_search"}],"tool_choice":{"type":"web_search"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(n.ServerTools, ",") != "web_search,tool_search" || strings.Join(n.ServerToolsRequired, ",") != "web_search" {
		t.Fatalf("needs %+v", n)
	}
}

// A real Codex request to a model without built-in metadata: MCP tools in
// namespaces, a freeform apply_patch, an offered web_search, and history
// with namespaced and freeform calls.
func TestCodexRequest(t *testing.T) {
	body := readFile(t, "testdata/codex_request.json")
	c := Codec{}
	req, n, err := c.DecodeRequest(nil, protocol.Inbound{Op: protocol.OpGenerate}, body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(n.ServerTools, ",") != "web_search" || len(n.ServerToolsRequired) != 0 || !n.FreeformTools || !n.FunctionTools {
		t.Fatalf("needs %+v", n)
	}
	ns := map[string]int{}
	for _, tool := range req.Tools {
		if tool.Namespace != "" {
			ns[tool.Namespace]++
		}
	}
	if ns["mcp__notes"] != 2 || ns["multi_agent_v1"] == 0 {
		t.Fatalf("namespaces %v", ns)
	}
	var nsCall, freeform bool
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Tool != nil && b.Tool.Namespace == "mcp__notes" && b.Tool.Name == "add_note" {
				nsCall = true
			}
			if b.Tool != nil && b.Tool.Kind == ir.ToolFreeform && b.Tool.Name == "apply_patch" {
				freeform = true
			}
		}
	}
	if !nsCall || !freeform {
		t.Fatalf("history calls: namespaced=%v freeform=%v", nsCall, freeform)
	}
	out, err := c.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, body, out) {
		t.Fatalf("round trip differs:\n%s", out)
	}
}
