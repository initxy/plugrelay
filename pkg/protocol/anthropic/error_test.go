package anthropic

import (
	"net/http"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
)

func TestEncodeError(t *testing.T) {
	reset := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		err    *ir.GwError
		status int
		body   string
		header map[string]string
	}{
		{"context too long", &ir.GwError{Kind: ir.ErrContextTooLong, Message: "x", TokensUsed: 210000, TokensLimit: 200000},
			400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`, nil},
		{"context too long, no numbers", &ir.GwError{Kind: ir.ErrContextTooLong, Message: "input exceeds window"},
			400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: input exceeds window"}}`, nil},
		{"rate limited", &ir.GwError{Kind: ir.ErrRateLimited, Message: "slow down", RetryAfter: 1500 * time.Millisecond},
			429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, map[string]string{"Retry-After": "2"}},
		{"overloaded", &ir.GwError{Kind: ir.ErrOverloaded, Message: "Overloaded"},
			529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, nil},
		{"quota", &ir.GwError{Kind: ir.ErrQuotaExhausted, Message: "daily quota used", ResetAt: reset},
			429, `{"type":"error","error":{"type":"rate_limit_error","message":"daily quota used (resets at 2026-11-06T00:00:00Z)"}}`,
			map[string]string{"X-Should-Retry": "false"}},
		{"content filtered", &ir.GwError{Kind: ir.ErrContentFiltered, Message: "blocked by policy"},
			400, `{"type":"error","error":{"type":"invalid_request_error","message":"blocked by policy"}}`, nil},
		{"capability", &ir.GwError{Kind: ir.ErrCapabilityMissing, Message: "server tool web_search_20250305"},
			400, `{"type":"error","error":{"type":"invalid_request_error","message":"capability_rejected: server tool web_search_20250305"}}`, nil},
		{"invalid", &ir.GwError{Kind: ir.ErrInvalidRequest, Message: "bad"},
			400, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`, nil},
		{"internal with request id", &ir.GwError{Kind: ir.ErrInternal, Message: "boom", Header: http.Header{"Request-Id": {"req_1"}, "X-Foo": {"bar"}}},
			500, `{"type":"error","error":{"type":"api_error","message":"boom"},"request_id":"req_1"}`, map[string]string{"X-Foo": "bar", "Request-Id": "req_1"}},
		{"auth", &ir.GwError{Kind: ir.ErrAuthentication, Message: "bad key"},
			401, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, nil},
		{"permission", &ir.GwError{Kind: ir.ErrPermission, Message: "no"},
			403, `{"type":"error","error":{"type":"permission_error","message":"no"}}`, nil},
		{"not found", &ir.GwError{Kind: ir.ErrNotFound, Message: "no model"},
			404, `{"type":"error","error":{"type":"not_found_error","message":"no model"}}`, nil},
		{"status override", &ir.GwError{Kind: ir.ErrUpstream, Message: "bad gateway", Status: 502},
			502, `{"type":"error","error":{"type":"api_error","message":"bad gateway"}}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, h, body := c.EncodeError(tc.err)
			if status != tc.status {
				t.Errorf("status = %d, want %d", status, tc.status)
			}
			if string(body) != tc.body {
				t.Errorf("body\n got %s\nwant %s", body, tc.body)
			}
			if h.Get("Content-Type") != "application/json" {
				t.Error("content type missing")
			}
			for k, v := range tc.header {
				if h.Get(k) != v {
					t.Errorf("header %s = %q, want %q", k, h.Get(k), v)
				}
			}
		})
	}
}

func TestDecodeError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header http.Header
		body   string
		want   ir.GwError
	}{
		{"prompt too long", 400, nil,
			`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 208310 tokens > 200000 maximum"}}`,
			ir.GwError{Kind: ir.ErrContextTooLong, Code: "invalid_request_error", TokensUsed: 208310, TokensLimit: 200000}},
		{"rate limited", 429, http.Header{"Retry-After": {"30"}, "Anthropic-Ratelimit-Unified-Status": {"rejected"}, "Set-Cookie": {"x"}},
			`{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"}}`,
			ir.GwError{Kind: ir.ErrRateLimited, Code: "rate_limit_error", RetryAfter: 30 * time.Second}},
		{"overloaded", 529, http.Header{"X-Should-Retry": {"false"}},
			`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			ir.GwError{Kind: ir.ErrOverloaded, Code: "overloaded_error", NoRetry: true}},
		{"api error", 500, nil, `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`,
			ir.GwError{Kind: ir.ErrUpstream, Code: "api_error"}},
		{"non-json 503", 503, nil, `upstream connect error`, ir.GwError{Kind: ir.ErrOverloaded}},
		{"billing", 400, nil, `{"type":"error","error":{"type":"billing_error","message":"credit balance too low"}}`,
			ir.GwError{Kind: ir.ErrQuotaExhausted, Code: "billing_error"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := c.DecodeError(tc.status, tc.header, []byte(tc.body))
			w := tc.want
			if e.Kind != w.Kind || e.Code != w.Code || e.TokensUsed != w.TokensUsed || e.TokensLimit != w.TokensLimit ||
				e.RetryAfter != w.RetryAfter || e.NoRetry != w.NoRetry {
				t.Errorf("got %+v, want %+v", e, w)
			}
			if string(e.Upstream) != tc.body || e.Message == "" {
				t.Errorf("upstream/message not kept: %+v", e)
			}
			if tc.header != nil && e.Header.Get("Set-Cookie") != "" {
				t.Error("unfiltered header passed through")
			}
		})
	}
	e := c.DecodeError(429, http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}}, nil)
	if e.Header.Get("Anthropic-Ratelimit-Unified-Status") != "rejected" {
		t.Error("ratelimit header dropped")
	}
}

// A decoded upstream error re-encodes to the same wording, so clients of this
// protocol see the original signal.
func TestErrorRoundTrip(t *testing.T) {
	body := `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 208310 tokens > 200000 maximum"}}`
	status, _, out := c.EncodeError(c.DecodeError(400, nil, []byte(body)))
	if status != 400 || string(out) != body {
		t.Errorf("%d %s", status, out)
	}
}
