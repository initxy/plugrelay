// Package metrics is the sink.metrics plugin: Prometheus metrics built
// from finished request records.
//
// All series live in the plugin's own registry (no global state), served by
// Handler. Metrics, all prefixed plugrelay_:
//
//	requests_total{protocol,model,status_class,tier}   finished requests
//	ttfb_seconds{protocol}                              time to first byte
//	request_duration_seconds{protocol}                  whole request
//	tokens_total{bucket}                                input, cache_read, cache_write_5m,
//	                                                    cache_write_1h, output, reasoning
//	prompt_tokens_total / prompt_cached_tokens_total    all input vs. cache reads;
//	                                                    hit ratio = rate(cached) / rate(prompt)
//	attempts_total{upstream,cause}                      upstream tries ("ok" when no cause)
//	sink_dropped_records_total{sink}                    best-effort records the sink queue dropped
//	sink_ledger_records_total{outcome}                  ledger records that waited for a full
//	                                                    queue, were written by the request
//	                                                    itself ("inline"), failed for good, or
//	                                                    were lost at the shutdown deadline
//
// The model label is the name the client asked for, which the router only
// accepts for configured models, so its cardinality is bounded by config.
package metrics

import (
	"context"
	"net/http"
	"strconv"
	"sync"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ID is the plugin id.
const ID = "sink.metrics"

func init() { plugin.Register(ID, func() plugin.Plugin { return New() }) }

// Sink implements plugin.Sink and serves the collected metrics.
type Sink struct {
	reg          *prometheus.Registry
	requests     *prometheus.CounterVec
	ttfb         *prometheus.HistogramVec
	duration     *prometheus.HistogramVec
	tokens       *prometheus.CounterVec
	prompt       prometheus.Counter
	promptCached prometheus.Counter
	attempts     *prometheus.CounterVec
	dropped      *prometheus.CounterVec
	ledger       *prometheus.CounterVec
	secretFail   prometheus.Counter
	secretUnsave prometheus.Gauge

	// mu guards the queue totals last reported, so the counters advance
	// by the difference.
	mu          sync.Mutex
	lastDropped map[string]int64
	lastLedger  map[string]int64
	lastSecret  map[string]int64
}

// New returns the sink with a fresh registry.
func New() *Sink {
	latency := []float64{.1, .25, .5, 1, 2, 4, 8, 15, 30, 60}
	s := &Sink{
		reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "plugrelay_requests_total",
			Help: "Finished requests."}, []string{"protocol", "model", "status_class", "tier"}),
		ttfb: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "plugrelay_ttfb_seconds",
			Help: "Time from request start to first byte sent to the client.", Buckets: latency}, []string{"protocol"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "plugrelay_request_duration_seconds",
			Help: "Request duration.", Buckets: append(latency, 120, 300, 600)}, []string{"protocol"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "plugrelay_tokens_total",
			Help: "Tokens of delivered attempts by usage bucket."}, []string{"bucket"}),
		prompt: prometheus.NewCounter(prometheus.CounterOpts{Name: "plugrelay_prompt_tokens_total",
			Help: "All input tokens, cached or not."}),
		promptCached: prometheus.NewCounter(prometheus.CounterOpts{Name: "plugrelay_prompt_cached_tokens_total",
			Help: "Input tokens served from the prompt cache."}),
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "plugrelay_attempts_total",
			Help: "Upstream attempts by failure cause."}, []string{"upstream", "cause"}),
		dropped: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "plugrelay_sink_dropped_records_total",
			Help: "Records the sink queue dropped for best-effort sinks (queue full or shut down)."}, []string{"sink"}),
		ledger: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "plugrelay_sink_ledger_records_total",
			Help: "Ledger (FailClosed sink) deliveries that waited for a full queue, were delivered inline by the request, failed for good, or were lost at the shutdown deadline."},
			[]string{"outcome"}),
		secretFail: prometheus.NewCounter(prometheus.CounterOpts{Name: "plugrelay_credential_secret_save_failures_total",
			Help: "Refreshed credential secrets the store refused after retries; each is held in memory only until saved."}),
		secretUnsave: prometheus.NewGauge(prometheus.GaugeOpts{Name: "plugrelay_credential_secrets_unsaved",
			Help: "Refreshed credential secrets on this node not yet saved; lost if it restarts."}),
		lastDropped: map[string]int64{},
		lastLedger:  map[string]int64{},
		lastSecret:  map[string]int64{},
	}
	s.reg.MustRegister(s.requests, s.ttfb, s.duration, s.tokens, s.prompt, s.promptCached, s.attempts, s.dropped, s.ledger,
		s.secretFail, s.secretUnsave)
	return s
}

// Info implements plugin.Plugin.
func (s *Sink) Info() plugin.Info {
	return plugin.Info{ID: ID, Kind: plugin.KindSink, Version: "1.0.0", FailPolicy: plugin.FailOpen,
		Description: "Prometheus metrics: success rate, latency, tokens, cache hit ratio, attempts."}
}

// Handler serves the registry in the Prometheus exposition format; mount
// it at /metrics.
func (s *Sink) Handler() http.Handler {
	return promhttp.HandlerFor(s.reg, promhttp.HandlerOpts{Registry: s.reg})
}

// Registry exposes the registry so the core can add its own collectors
// (credential states, queue depth) to the same endpoint.
func (s *Sink) Registry() *prometheus.Registry { return s.reg }

// SetSinkDropped reports the drop total of a sink queue since start; the
// counter advances by the increase since the last report.
func (s *Sink) SetSinkDropped(sinkID string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	advance(s.dropped.WithLabelValues(sinkID), s.lastDropped, sinkID, n)
}

// SetSinkLedger reports the ledger totals of the sink queue since start.
func (s *Sink) SetSinkLedger(waited, inline, failed, lost int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range []struct {
		name string
		n    int64
	}{{"waited", waited}, {"inline", inline}, {"failed", failed}, {"lost", lost}} {
		advance(s.ledger.WithLabelValues(o.name), s.lastLedger, o.name, o.n)
	}
}

// SetCredentialSecrets reports refreshed secrets the store refused since
// start and how many are still unsaved.
func (s *Sink) SetCredentialSecrets(failures int64, unsaved int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	advance(s.secretFail, s.lastSecret, "failures", failures)
	s.secretUnsave.Set(float64(unsaved))
}

func advance(c prometheus.Counter, last map[string]int64, key string, total int64) {
	if d := total - last[key]; d > 0 {
		c.Add(float64(d))
		last[key] = total
	}
}

// Consume implements plugin.Sink.
func (s *Sink) Consume(_ context.Context, r *plugin.Record) error {
	tier := "none"
	if n := len(r.Attempts); n > 0 {
		tier = string(r.Attempts[n-1].Tier)
	}
	s.requests.WithLabelValues(label(r.Protocol), label(r.Model), statusClass(r.Status), tier).Inc()
	if r.TTFB > 0 {
		s.ttfb.WithLabelValues(label(r.Protocol)).Observe(r.TTFB.Seconds())
	}
	s.duration.WithLabelValues(label(r.Protocol)).Observe(r.Duration.Seconds())

	u := r.Usage
	for _, b := range []struct {
		name string
		n    int64
	}{
		{"input", u.Input}, {"cache_read", u.CacheRead}, {"cache_write_5m", u.CacheWrite5m},
		{"cache_write_1h", u.CacheWrite1h}, {"output", u.Output}, {"reasoning", u.Reasoning},
	} {
		if b.n > 0 {
			s.tokens.WithLabelValues(b.name).Add(float64(b.n))
		}
	}
	if in := u.TotalInput(); in > 0 {
		s.prompt.Add(float64(in))
		s.promptCached.Add(float64(u.CacheRead))
	}
	for _, a := range r.Attempts {
		cause := string(a.Cause)
		if cause == "" {
			cause = "ok"
		}
		s.attempts.WithLabelValues(label(a.UpstreamID), cause).Inc()
	}
	return nil
}

func statusClass(code int) string {
	if code < 100 || code > 599 {
		return "none"
	}
	return strconv.Itoa(code/100) + "xx"
}

func label(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
