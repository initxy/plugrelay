package anthropic

import (
	"strings"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
)

// An empty prepaid balance is Anthropic's 402 billing_error, not a rate limit.
func TestEncodeBillingError(t *testing.T) {
	status, h, body := Codec{}.EncodeError(&ir.GwError{Kind: ir.ErrQuotaExhausted, Status: 402, Code: "billing_error",
		Message: "Your credit balance is too low", NoRetry: true})
	if status != 402 || !strings.Contains(string(body), `"type":"billing_error"`) || h.Get("X-Should-Retry") != "false" {
		t.Fatalf("%d %s %v", status, body, h)
	}
	if _, _, body := (Codec{}).EncodeError(&ir.GwError{Kind: ir.ErrQuotaExhausted, Message: "daily budget"}); !strings.Contains(string(body), "rate_limit_error") {
		t.Fatalf("budget errors stay rate limits: %s", body)
	}
}
