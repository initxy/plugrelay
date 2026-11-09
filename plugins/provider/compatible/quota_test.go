package compatible

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/initxy/plugrelay/manifests"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// rewrite sends every request to srv, keeping path and headers.
type rewrite struct{ srv *url.URL }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme, req.URL.Host = r.srv.Scheme, r.srv.Host
	return http.DefaultTransport.RoundTrip(req)
}

// The official OpenRouter manifest probes the key's own limit at GET
// /api/v1/key (response shape from its API reference) and derives the reset
// from limit_reset.
func TestOpenRouterQuotaProbe(t *testing.T) {
	var path, auth string
	body := `{"data":{"label":"k","limit":100,"limit_remaining":74.5,"limit_reset":"monthly","usage":25.5}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	su, _ := url.Parse(srv.URL)
	x, err := LoadDir(manifests.FS)
	if err != nil {
		t.Fatal(err)
	}
	p := New("openai-compatible", WithIndex(x), WithHTTPClient(&http.Client{Transport: rewrite{su}}))
	u := &plugin.Upstream{ID: "or", Config: map[string]any{"manifest": "openrouter"}}
	q, err := p.Quota(context.Background(), u, &plugin.Credential{ID: "c", Secret: "sk-or-1"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/key" || auth != "Bearer sk-or-1" {
		t.Errorf("probe went to %s with %q", path, auth)
	}
	if q.Remaining != 0.745 || q.Left != 74.5 || q.Limit != 100 || q.ResetAt.IsZero() || q.ResetAt.Day() != 1 || q.ResetAt.Hour() != 0 {
		t.Errorf("quota = %+v", q)
	}

	// An unlimited key reports null limits: remaining unknown, no reset.
	body = `{"data":{"limit":null,"limit_remaining":null,"limit_reset":null,"usage":3}}`
	if q, err := p.Quota(context.Background(), u, &plugin.Credential{Secret: "k"}); err != nil || q.Remaining != -1 || q.Limit != 0 || !q.ResetAt.IsZero() {
		t.Errorf("unlimited key: %+v %v", q, err)
	}
	// Spent past the limit clamps to used up.
	body = `{"data":{"limit":10,"limit_remaining":-0.2,"limit_reset":"daily"}}`
	if q, _ := p.Quota(context.Background(), u, &plugin.Credential{Secret: "k"}); q.Remaining != 0 || q.Left != 0 {
		t.Errorf("overspent key: %+v", q)
	}

	// A manifest without a probe says so in a way the job skips.
	_, err = p.Quota(context.Background(), &plugin.Upstream{ID: "g", Config: map[string]any{"manifest": "groq"}}, nil)
	if !errors.Is(err, plugin.ErrNoQuota) || !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("no probe: %v", err)
	}
}

func TestNextPeriod(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	now := at("2026-11-05T13:30:00+08:00") // Thursday 05:30 UTC
	for period, want := range map[string]string{
		"daily":   "2026-11-06T00:00:00Z",
		"weekly":  "2026-11-09T00:00:00Z",
		"monthly": "2026-12-01T00:00:00Z",
		"yearly":  "0001-01-01T00:00:00Z",
	} {
		if got := nextPeriod(period, now); !got.Equal(at(want)) {
			t.Errorf("%s: %s, want %s", period, got, want)
		}
	}
	if got := nextPeriod("weekly", at("2026-11-09T00:00:00Z")); !got.Equal(at("2026-11-16T00:00:00Z")) {
		t.Errorf("weekly on a Monday: %s", got)
	}
	if got := nextPeriod("monthly", at("2026-12-31T23:00:00Z")); !got.Equal(at("2027-01-01T00:00:00Z")) {
		t.Errorf("monthly in December: %s", got)
	}
}
