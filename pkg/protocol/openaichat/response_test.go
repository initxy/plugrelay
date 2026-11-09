package openaichat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
)

func TestDecodeResponseFixtures(t *testing.T) {
	r, err := codec.DecodeResponse(readFixture(t, "response_reasoning.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Content) != 2 || r.Content[0].Type != ir.BlockThinking || r.Content[1].Text != "Hi there friend." {
		t.Fatalf("content %+v", r.Content)
	}
	if r.StopReason != ir.StopEndTurn || r.Usage.Input != 37 || r.Usage.Output != 74 || r.Usage.Reasoning != 68 {
		t.Errorf("stop %s usage %+v", r.StopReason, r.Usage)
	}

	r, err = codec.DecodeResponse(readFixture(t, "response_tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Content) != 1 || r.Content[0].Tool.Name != "get_weather" || string(r.Content[0].Tool.Input) != `{"city":"Rome"}` {
		t.Fatalf("content %+v", r.Content)
	}
	if r.StopReason != ir.StopToolUse || r.ID == "" || r.Model != "gpt-5.4" {
		t.Errorf("resp %+v", r)
	}
}

func TestResponseRoundTrip(t *testing.T) {
	for _, f := range []string{"response_reasoning.json", "response_tools.json"} {
		t.Run(f, func(t *testing.T) {
			body := readFixture(t, f)
			r, err := codec.DecodeResponse(body)
			if err != nil {
				t.Fatal(err)
			}
			out, err := codec.EncodeResponse(nil, r)
			if err != nil {
				t.Fatal(err)
			}
			want := body
			// The encoder always reports total_tokens and both details objects.
			var w, g map[string]any
			_ = json.Unmarshal(want, &w)
			_ = json.Unmarshal(out, &g)
			for _, k := range []string{"id", "model", "created", "object", "system_fingerprint"} {
				if w[k] != nil && w[k] != g[k] {
					t.Errorf("%s: %v != %v", k, w[k], g[k])
				}
			}
			wc := w["choices"].([]any)[0].(map[string]any)
			gc := g["choices"].([]any)[0].(map[string]any)
			wb, _ := json.Marshal(wc["message"])
			gb, _ := json.Marshal(gc["message"])
			jsonEqual(t, wb, gb)
			if wc["finish_reason"] != gc["finish_reason"] {
				t.Errorf("finish %v != %v", wc["finish_reason"], gc["finish_reason"])
			}
		})
	}
}

func TestUsageMapping(t *testing.T) {
	u := decodeUsage(json.RawMessage(`{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":800,"cache_write_tokens":100},"completion_tokens_details":{"reasoning_tokens":20}}`))
	if u.Input != 100 || u.CacheRead != 800 || u.CacheWrite5m != 100 || u.Output != 50 || u.Reasoning != 20 || u.TotalInput() != 1000 {
		t.Errorf("usage %+v", u)
	}
	b, _ := marshal(encodeUsage(u))
	jsonEqual(t, []byte(`{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050,"prompt_tokens_details":{"cached_tokens":800,"cache_write_tokens":100},"completion_tokens_details":{"reasoning_tokens":20}}`), b)
}

func TestEncodeResponseForeign(t *testing.T) {
	resp := &ir.Response{
		ID: "msg_1", Model: "m",
		Content: []ir.Block{
			{Type: ir.BlockThinking, Text: "t", Opaque: &ir.OpaqueState{Kind: "signature", Data: json.RawMessage(`"sig"`)}},
			{Type: ir.BlockText, Text: "a"},
			{Type: ir.BlockText, Text: "b"},
			{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "x", Name: "p", Kind: ir.ToolFreeform, Text: "in"}},
		},
		StopReason: ir.StopToolUse,
		Usage:      ir.Usage{Input: 1, CacheRead: 2, Output: 3},
	}
	out, err := codec.EncodeResponse(&ir.Request{}, resp)
	if err != nil {
		t.Fatal(err)
	}
	var g map[string]any
	_ = json.Unmarshal(out, &g)
	msg, _ := json.Marshal(g["choices"].([]any)[0].(map[string]any)["message"])
	jsonEqual(t, []byte(`{"role":"assistant","content":"ab","reasoning_content":"t","tool_calls":[{"id":"x","type":"function","function":{"name":"p","arguments":"in"}}]}`), msg)
	if strings.Contains(string(out), "sig") {
		t.Error("opaque state leaked into chat response")
	}
	out, _ = codec.EncodeResponse(&ir.Request{Tools: []ir.Tool{{Kind: ir.ToolFreeform, Name: "p"}}}, resp)
	if !strings.Contains(string(out), `"type":"custom","custom":{"name":"p","input":"in"}`) {
		t.Errorf("custom shape missing: %s", out)
	}
}

func TestEncodeError(t *testing.T) {
	tests := []struct {
		err    *ir.GwError
		status int
		body   string
		header map[string]string
	}{
		{&ir.GwError{Kind: ir.ErrContextTooLong, TokensUsed: 9000, TokensLimit: 8192}, 400,
			`{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","type":"invalid_request_error","param":null,"code":"context_length_exceeded"}}`, nil},
		{&ir.GwError{Kind: ir.ErrRateLimited, Message: "slow down", RetryAfter: 1500 * time.Millisecond}, 429,
			`{"error":{"message":"slow down","type":"rate_limit_error","param":null,"code":"rate_limit_exceeded"}}`, map[string]string{"Retry-After": "2"}},
		{&ir.GwError{Kind: ir.ErrOverloaded, Message: "busy"}, 503,
			`{"error":{"message":"busy","type":"server_error","param":null,"code":"server_is_overloaded"}}`, nil},
		{&ir.GwError{Kind: ir.ErrQuotaExhausted, Message: "quota"}, 429,
			`{"error":{"message":"quota","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`, map[string]string{"X-Should-Retry": "false"}},
		{&ir.GwError{Kind: ir.ErrContentFiltered, Message: "no"}, 400,
			`{"error":{"message":"no","type":"invalid_request_error","param":null,"code":"content_filter"}}`, nil},
		{&ir.GwError{Kind: ir.ErrCapabilityMissing, Message: "needs images"}, 400,
			`{"error":{"message":"needs images","type":"invalid_request_error","param":null,"code":null}}`, nil},
		{&ir.GwError{Kind: ir.ErrInvalidRequest, Message: "bad"}, 400,
			`{"error":{"message":"bad","type":"invalid_request_error","param":null,"code":null}}`, nil},
		{&ir.GwError{Kind: ir.ErrInternal, Message: "oops"}, 500,
			`{"error":{"message":"oops","type":"server_error","param":null,"code":null}}`, nil},
		{&ir.GwError{Kind: ir.ErrAuthentication, Message: "key"}, 401,
			`{"error":{"message":"key","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`, nil},
		{&ir.GwError{Kind: ir.ErrPermission, Message: "denied"}, 403,
			`{"error":{"message":"denied","type":"permission_error","param":null,"code":"permission_denied"}}`, nil},
		{&ir.GwError{Kind: ir.ErrNotFound, Message: "no model"}, 404,
			`{"error":{"message":"no model","type":"invalid_request_error","param":null,"code":"model_not_found"}}`, nil},
		{&ir.GwError{Kind: ir.ErrQuotaExhausted, Message: "plan", Code: "usage_limit_reached", Status: 402}, 402,
			`{"error":{"message":"plan","type":"insufficient_quota","param":null,"code":"usage_limit_reached"}}`, nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.err.Kind), func(t *testing.T) {
			status, h, body := codec.EncodeError(tt.err)
			if status != tt.status {
				t.Errorf("status %d want %d", status, tt.status)
			}
			jsonEqual(t, []byte(tt.body), body)
			if h.Get("Content-Type") != "application/json" {
				t.Error("content type")
			}
			for k, v := range tt.header {
				if h.Get(k) != v {
					t.Errorf("%s = %q want %q", k, h.Get(k), v)
				}
			}
		})
	}
}

func TestDecodeError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		header http.Header
		body   string
		kind   ir.ErrorKind
		check  func(*testing.T, *ir.GwError)
	}{
		{"context by code", 400, nil,
			`{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130021 tokens.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`,
			ir.ErrContextTooLong, func(t *testing.T, e *ir.GwError) {
				if e.TokensLimit != 128000 || e.TokensUsed != 130021 || e.Code != "context_length_exceeded" {
					t.Errorf("%+v", e)
				}
			}},
		{"context by message", 400, nil, `{"error":{"message":"maximum context length is 4096 tokens, you requested 5000 tokens","code":null}}`, ir.ErrContextTooLong, nil},
		{"quota", 429, nil, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`,
			ir.ErrQuotaExhausted, func(t *testing.T, e *ir.GwError) {
				if !e.NoRetry {
					t.Error("quota should be NoRetry")
				}
			}},
		{"rate limit", 429, http.Header{"Retry-After": {"7"}, "X-Ratelimit-Remaining-Requests": {"0"}, "Set-Cookie": {"x"}},
			`{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`,
			ir.ErrRateLimited, func(t *testing.T, e *ir.GwError) {
				if e.RetryAfter != 7*time.Second || e.Header.Get("X-Ratelimit-Remaining-Requests") != "0" || e.Header.Get("Set-Cookie") != "" {
					t.Errorf("%+v", e)
				}
			}},
		{"rate limit ms", 429, http.Header{"Retry-After-Ms": {"250"}}, `{"error":{"message":"x"}}`, ir.ErrRateLimited, func(t *testing.T, e *ir.GwError) {
			if e.RetryAfter != 250*time.Millisecond {
				t.Error(e.RetryAfter)
			}
		}},
		{"auth", 401, nil, `{"error":{"message":"Incorrect API key","code":"invalid_api_key"}}`, ir.ErrAuthentication, nil},
		{"forbidden", 403, nil, `{"error":{"message":"no"}}`, ir.ErrPermission, nil},
		{"not found", 404, nil, `{"error":{"message":"The model does not exist","code":"model_not_found"}}`, ir.ErrNotFound, nil},
		{"overloaded", 503, nil, `{"error":{"message":"busy"}}`, ir.ErrOverloaded, nil},
		{"content filter", 400, nil, `{"error":{"message":"flagged","code":"content_policy_violation"}}`, ir.ErrContentFiltered, nil},
		{"bad gateway text", 502, nil, `<html>bad gateway</html>`, ir.ErrUpstream, func(t *testing.T, e *ir.GwError) {
			if e.Message != "<html>bad gateway</html>" || string(e.Upstream) != "<html>bad gateway</html>" {
				t.Errorf("%+v", e)
			}
		}},
		{"string error", 400, nil, `{"error":"bad thing"}`, ir.ErrInvalidRequest, func(t *testing.T, e *ir.GwError) {
			if e.Message != "bad thing" {
				t.Error(e.Message)
			}
		}},
		{"numeric code", 500, nil, `{"error":{"message":"x","code":500}}`, ir.ErrUpstream, func(t *testing.T, e *ir.GwError) {
			if e.Code != "500" {
				t.Error(e.Code)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := codec.DecodeError(tt.status, tt.header, []byte(tt.body))
			if e.Kind != tt.kind {
				t.Fatalf("kind %s want %s (%+v)", e.Kind, tt.kind, e)
			}
			if e.Status != 0 {
				t.Error("decoded errors leave Status to the client codec")
			}
			if tt.check != nil {
				tt.check(t, e)
			}
		})
	}
}

func TestDecodeResponseError(t *testing.T) {
	_, err := codec.DecodeResponse([]byte(`{"error":{"message":"maximum context length is 10 tokens","code":"context_length_exceeded"}}`))
	gw, ok := err.(*ir.GwError)
	if !ok || gw.Kind != ir.ErrContextTooLong {
		t.Errorf("err %v", err)
	}
}

func TestCapabilities(t *testing.T) {
	c := codec.Capabilities()
	if c.Cache != ir.CacheKeyed || !c.FreeformTools || !c.FunctionTools || c.ReasoningStyle != "effort" ||
		!c.Images || c.MaxTokensField != "max_completion_tokens" || len(c.EchoFields) != 0 {
		t.Errorf("%+v", c)
	}
}
