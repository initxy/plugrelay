package metrics

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

func scrape(t *testing.T, s *Sink) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func TestConsume(t *testing.T) {
	s := New()
	recs := []*plugin.Record{
		{Protocol: "anthropic", Model: "m1", Status: 200, TTFB: 300 * time.Millisecond, Duration: 2 * time.Second,
			Usage: ir.Usage{Input: 100, CacheRead: 900, Output: 50, Reasoning: 10},
			Attempts: []plugin.AttemptRecord{
				{UpstreamID: "u1", Cause: plugin.CauseRateLimit, Tier: plugin.TierPassthrough},
				{UpstreamID: "u2", Tier: plugin.TierPassthrough},
			}},
		{Protocol: "anthropic", Model: "m1", Status: 429, Duration: time.Second},
		{Protocol: "", Model: "", Status: 0},
	}
	for _, r := range recs {
		if err := s.Consume(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	s.SetSinkDropped("queue", 1)
	s.SetSinkDropped("queue", 3)
	s.SetSinkLedger(2, 1, 0, 0)
	s.SetSinkLedger(4, 1, 0, 2)
	out := scrape(t, s)
	for _, want := range []string{
		`plugrelay_requests_total{model="m1",protocol="anthropic",status_class="2xx",tier="passthrough"} 1`,
		`plugrelay_requests_total{model="m1",protocol="anthropic",status_class="4xx",tier="none"} 1`,
		`plugrelay_requests_total{model="unknown",protocol="unknown",status_class="none",tier="none"} 1`,
		`plugrelay_tokens_total{bucket="cache_read"} 900`,
		`plugrelay_tokens_total{bucket="reasoning"} 10`,
		`plugrelay_prompt_tokens_total 1000`,
		`plugrelay_prompt_cached_tokens_total 900`,
		`plugrelay_attempts_total{cause="rate_limit",upstream="u1"} 1`,
		`plugrelay_attempts_total{cause="ok",upstream="u2"} 1`,
		`plugrelay_ttfb_seconds_count{protocol="anthropic"} 1`,
		`plugrelay_request_duration_seconds_count{protocol="anthropic"} 2`,
		"# TYPE plugrelay_sink_ledger_records_total counter",
		`plugrelay_sink_dropped_records_total{sink="queue"} 3`,
		`plugrelay_sink_ledger_records_total{outcome="waited"} 4`,
		`plugrelay_sink_ledger_records_total{outcome="inline"} 1`,
		`plugrelay_sink_ledger_records_total{outcome="lost"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(out, "go_goroutines") {
		t.Error("default collectors leaked into the private registry")
	}
}

func TestInstancesIsolated(t *testing.T) {
	a, b := New(), New()
	_ = a.Consume(context.Background(), &plugin.Record{Protocol: "p", Model: "m", Status: 200})
	if strings.Contains(scrape(t, b), "plugrelay_requests_total{") {
		t.Fatal("registries share state")
	}
}
