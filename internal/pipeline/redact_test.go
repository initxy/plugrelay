package pipeline

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// Transport errors in request logs never carry the upstream URL's query,
// which may hold a credential.
func TestErrTextDropsQuery(t *testing.T) {
	ue := &url.Error{Op: "Post", URL: "https://h/v1/models:generate?key=AIzaSECRET&alt=sse", Err: errors.New("connection refused")}
	for _, err := range []error{ue, fmt.Errorf("first byte timeout: %w", ue)} {
		got := errText(err)
		if strings.Contains(got, "SECRET") || !strings.Contains(got, "connection refused") || !strings.Contains(got, "https://h/v1/models:generate") {
			t.Errorf("errText = %q", got)
		}
	}
	if got := errText(errors.New("plain")); got != "plain" {
		t.Errorf("plain error = %q", got)
	}
}
