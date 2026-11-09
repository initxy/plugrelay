package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestRedactURLError(t *testing.T) {
	ue := &url.Error{Op: "Get", URL: "https://h/v1/models?key=SECRET&alt=sse", Err: context.DeadlineExceeded}
	for _, err := range []error{ue, fmt.Errorf("first byte timeout: %w", ue)} {
		got := RedactURLError(err)
		if strings.Contains(got.Error(), "SECRET") || !strings.Contains(got.Error(), "https://h/v1/models") {
			t.Errorf("redacted = %q", got)
		}
		if !errors.Is(got, context.DeadlineExceeded) {
			t.Errorf("%q lost its chain", got)
		}
	}
	// Every URL error of a joined chain is redacted.
	joined := errors.Join(&url.Error{Op: "Get", URL: "https://a/?k=A1", Err: errors.New("x")},
		&url.Error{Op: "Get", URL: "https://b/?k=B2", Err: errors.New("y")})
	if got := RedactURLError(joined).Error(); strings.Contains(got, "A1") || strings.Contains(got, "B2") {
		t.Errorf("joined = %q", got)
	}
	// A typed nil *url.Error in the chain must not panic.
	var nilUE *url.Error
	_ = RedactURLError(fmt.Errorf("wrapped: %w", nilUE))
	_ = RedactURLError(error(nilUE))
	plain := errors.New("plain")
	if RedactURLError(plain) != plain || RedactURLError(nil) != nil {
		t.Error("errors without a URL query must pass unchanged")
	}
}
