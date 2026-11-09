package redact

import (
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
)

func TestDetectors(t *testing.T) {
	rules, err := New().rules(testkit.NewCall(ID, testkit.CallOpts{}))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ in, want string }{
		{"key sk-abcdefghijklmnopqrstuv end", "key [REDACTED:secret] end"},
		{"aws AKIAABCDEFGHIJKLMNOP", "aws [REDACTED:secret]"},
		{"Authorization: Bearer abcdefghijklmnopqrstuvwxyz123", "Authorization: [REDACTED:secret]"},
		{"jwt eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0.SflKxwRJSMeKKF2QT4", "jwt [REDACTED:secret]"},
		{"mail me at a.b+c@example.co.uk now", "mail me at [REDACTED:email] now"},
		{"call +1 415-555-0123 or 13812345678", "call [REDACTED:phone] or [REDACTED:phone]"},
		{"call +86 138 1234 5678", "call [REDACTED:phone]"},
		{"plain text with 2026 numbers and sk-short", "plain text with 2026 numbers and sk-short"},
		{"hash 0123456789abcdef0123456789abcdef", "hash 0123456789abcdef0123456789abcdef"},
		// The default detectors are textscan's: Google and GitLab keys and
		// North American numbers written with separators are masked too.
		{"google AIzaSyA1234567890abcdefghijklmnopqrstuv", "google [REDACTED:secret]"},
		{"gitlab glpat-abcdefghijklmnopqrst", "gitlab [REDACTED:secret]"},
		{"call (415) 555-0123 or 415-555-0123", "call [REDACTED:phone] or [REDACTED:phone]"},
		// Cards, IBANs and the rest only when listed in "detect".
		{"card 4111 1111 1111 1111", "card 4111 1111 1111 1111"},
	}
	for _, tt := range tests {
		if got := mask(rules, tt.in); got != tt.want {
			t.Errorf("mask(%q)\n got %q\nwant %q", tt.in, got, tt.want)
		}
	}
}

// Every textscan detector can be switched on, checksums included.
func TestMoreDetectors(t *testing.T) {
	c := testkit.NewCall(ID, testkit.CallOpts{Settings: testkit.Settings{"detect": []string{"credit_card", "iban", "private_key"}}})
	rules, err := New().rules(c)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ in, want string }{
		{"card 4111 1111 1111 1111", "card [REDACTED:credit_card]"},
		{"not a card 4111 1111 1111 1112", "not a card 4111 1111 1111 1112"},
		{"iban GB82 WEST 1234 5698 7654 32", "iban [REDACTED:iban]"},
		{"-----BEGIN PRIVATE KEY-----\nMIIB\n-----END PRIVATE KEY-----", "[REDACTED:private_key]"},
		{"mail x@y.io", "mail x@y.io"},
	}
	for _, tt := range tests {
		if got := mask(rules, tt.in); got != tt.want {
			t.Errorf("mask(%q)\n got %q\nwant %q", tt.in, got, tt.want)
		}
	}
}

func TestMutateBody(t *testing.T) {
	tests := []struct{ name, proto, in, want string }{
		{"anthropic string and parts; assistant untouched", "anthropic",
			`{"messages":[{"role":"user","content":"hi x@y.io"},{"role":"assistant","content":"x@y.io"},{"role":"user","content":[{"type":"text","text":"x@y.io","cache_control":{"type":"ephemeral"}},{"type":"image"}]}]}`,
			`{"messages":[{"role":"user","content":"hi [REDACTED:email]"},{"role":"assistant","content":"x@y.io"},{"role":"user","content":[{"type":"text","text":"[REDACTED:email]","cache_control":{"type":"ephemeral"}},{"type":"image"}]}]}`},
		{"chat system untouched", "openai-chat",
			`{"messages":[{"role":"system","content":"x@y.io"},{"role":"user","content":"x@y.io"}]}`,
			`{"messages":[{"role":"system","content":"x@y.io"},{"role":"user","content":"[REDACTED:email]"}]}`},
		{"responses string input", "openai-responses", `{"input":"x@y.io"}`, `{"input":"[REDACTED:email]"}`},
		{"responses items", "openai-responses",
			`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"x@y.io"}]},{"type":"function_call_output","output":"x@y.io"}]}`,
			`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"[REDACTED:email]"}]},{"type":"function_call_output","output":"x@y.io"}]}`},
		{"gemini", "gemini",
			`{"contents":[{"role":"user","parts":[{"text":"x@y.io"}]},{"role":"model","parts":[{"text":"x@y.io"}]}]}`,
			`{"contents":[{"role":"user","parts":[{"text":"[REDACTED:email]"}]},{"role":"model","parts":[{"text":"x@y.io"}]}]}`},
		{"text completions", "openai-completions",
			`{"prompt":["mail x@y.io"],"suffix":"x@y.io","stop":"x@y.io"}`,
			`{"prompt":["mail [REDACTED:email]"],"suffix":"[REDACTED:email]","stop":"x@y.io"}`},
		{"no match keeps bytes", "anthropic", `{"messages":[{"role":"user","content":"hello"}]}`, `{"messages":[{"role":"user","content":"hello"}]}`},
	}
	r := New()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testkit.NewCall(ID, testkit.CallOpts{Protocol: tt.proto})
			got, err := r.MutateBody(c, &plugin.Attempt{Tier: plugin.TierPatch}, []byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestSettings(t *testing.T) {
	r := New()
	body := []byte(`{"messages":[{"role":"user","content":"x@y.io order-12345"}]}`)
	c := testkit.NewCall(ID, testkit.CallOpts{Protocol: "anthropic",
		Settings: testkit.Settings{"detect": []string{}, "patterns": []string{`order-\d+`}}})
	got, err := r.MutateBody(c, &plugin.Attempt{}, body)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"messages":[{"role":"user","content":"x@y.io [REDACTED:custom]"}]}`; string(got) != want {
		t.Fatalf("got %s", got)
	}
	bad := testkit.NewCall(ID, testkit.CallOpts{Protocol: "anthropic", Settings: testkit.Settings{"patterns": []string{`(`}}})
	if _, err := r.MutateBody(bad, &plugin.Attempt{}, body); err == nil {
		t.Fatal("invalid pattern accepted")
	}
}

func TestConvertTier(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "x@y.io"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "x@y.io"}}},
	}}
	c := testkit.NewCall(ID, testkit.CallOpts{Request: req})
	if err := New().OnAttempt(c, &plugin.Attempt{Tier: plugin.TierConvert}); err != nil {
		t.Fatal(err)
	}
	if req.Messages[0].Content[0].Text != "[REDACTED:email]" || req.Messages[1].Content[0].Text != "x@y.io" {
		t.Fatalf("%+v", req.Messages)
	}
}
