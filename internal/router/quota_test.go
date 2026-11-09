package router

import (
	"errors"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

func quotaTable() *Table {
	return &Table{
		Models: map[string]Model{"m": {Name: "m", Enabled: true, Routes: []Route{{UpstreamID: "a", UpstreamModel: "m"}}}},
		Upstreams: map[string]Upstream{
			"a": {ID: "a", Enabled: true, Endpoints: []plugin.Endpoint{ep("chat", "openai_chat")}, Credentials: shared("x", "y", "z")},
		},
	}
}

func resets(p *Plan) map[string]time.Time {
	out := map[string]time.Time{}
	for _, c := range p.Candidates {
		out[c.CredentialID] = c.QuotaReset
	}
	return out
}

// A probe reading fills Candidate.QuotaReset on every node (it lives in
// state); a used-up quota keeps the credential out until the reset, a later
// reading with quota left lets it back, and a failed probe keeps the last
// figures.
func TestQuotaReadingFeedsPlan(t *testing.T) {
	r, clk, _ := newRouter(t, quotaTable())
	var events []CredEvent
	r.opt.OnCredential = func(ev CredEvent) { events = append(events, ev) }
	now := clk.Now()
	ttl := 30 * time.Minute
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.ReportQuota(ctx, "a", "x", plugin.Quota{Remaining: 0.5, Left: 5, Limit: 10, ResetAt: now.Add(5 * time.Hour)}, nil, ttl))
	must(r.ReportQuota(ctx, "a", "y", plugin.Quota{Remaining: 0.9, ResetAt: now.Add(time.Hour)}, nil, ttl))
	// A reset already past is not a reset time to rank by.
	must(r.ReportQuota(ctx, "a", "z", plugin.Quota{Remaining: -1, ResetAt: now.Add(-time.Minute)}, nil, ttl))

	got := resets(mustPlan(t, r, Query{Model: "m"}))
	if !got["x"].Equal(now.Add(5*time.Hour)) || !got["y"].Equal(now.Add(time.Hour)) || !got["z"].IsZero() || len(got) != 3 {
		t.Fatalf("quota resets on candidates: %v", got)
	}

	// Used up: out until the reset, with an event for the audit trail.
	resetAt := now.Add(2 * time.Hour)
	must(r.ReportQuota(ctx, "a", "x", plugin.Quota{Remaining: 0, Left: 0, Limit: 10, ResetAt: resetAt}, nil, ttl))
	if got := resets(mustPlan(t, r, Query{Model: "m"})); len(got) != 2 {
		t.Fatalf("exhausted credential still planned: %v", got)
	}
	if len(events) != 1 || events[0].Status != StatusCooling || !events[0].Until.Equal(resetAt) || events[0].Reason != ReasonQuotaProbe {
		t.Fatalf("events = %+v", events)
	}
	cl := liveCred(t, r, "a", "x")
	if cl.Status != StatusCooling || !cl.Until.Equal(resetAt) || cl.Quota == nil || cl.Quota.Remaining != 0 || cl.Quota.Limit != 10 {
		t.Fatalf("live = %+v quota %+v", cl, cl.Quota)
	}
	// The same reading again changes nothing.
	must(r.ReportQuota(ctx, "a", "x", plugin.Quota{Remaining: 0, Limit: 10, ResetAt: resetAt}, nil, ttl))
	if len(events) != 1 {
		t.Fatalf("repeat reading fired %d events", len(events))
	}

	// A failed probe keeps the figures and says why.
	clk.Advance(time.Minute)
	must(r.ReportQuota(ctx, "a", "x", plugin.Quota{}, errors.New("502 from /quota"), ttl))
	cl = liveCred(t, r, "a", "x")
	if cl.Quota.Err != "502 from /quota" || cl.Quota.Limit != 10 || !cl.Quota.At.Equal(clk.Now()) || cl.Status != StatusCooling {
		t.Fatalf("after failed probe: %+v %+v", cl, cl.Quota)
	}

	// The upstream reset early: quota is back, so is the credential.
	must(r.ReportQuota(ctx, "a", "x", plugin.Quota{Remaining: 1, ResetAt: now.Add(26 * time.Hour)}, nil, ttl))
	if _, in := resets(mustPlan(t, r, Query{Model: "m"}))["x"]; !in {
		t.Fatal("credential with quota back not planned")
	}

	// Used up with no reset time: out for the reading's lifetime.
	must(r.ReportQuota(ctx, "a", "y", plugin.Quota{Remaining: 0}, nil, ttl))
	if cl := liveCred(t, r, "a", "y"); cl.Status != StatusCooling || !cl.Until.Equal(clk.Now().Add(ttl)) {
		t.Fatalf("no-reset exhaustion: %+v", cl)
	}
	clk.Advance(ttl + time.Second)
	if _, in := resets(mustPlan(t, r, Query{Model: "m"}))["y"]; !in {
		t.Fatal("credential not back after the reading expired")
	}
	if _, ok := r.QuotaOf(ctx, "a", "y"); ok {
		t.Fatal("reading outlived its ttl")
	}
}

// A probe never lifts or shortens a cooldown an upstream error set.
func TestQuotaReadingLeavesErrorCooldowns(t *testing.T) {
	r, clk, _ := newRouter(t, quotaTable())
	until := clk.Now().Add(3 * time.Hour)
	if err := r.Report(ctx, plugin.Candidate{UpstreamID: "a", CredentialID: "x"},
		plugin.Verdict{Cause: plugin.CauseQuota, Credential: plugin.CredCooldown, Until: until}, "m"); err != nil {
		t.Fatal(err)
	}
	_ = r.ReportQuota(ctx, "a", "x", plugin.Quota{Remaining: 0.7}, nil, time.Hour)
	_ = r.ReportQuota(ctx, "a", "x", plugin.Quota{Remaining: 0, ResetAt: clk.Now().Add(time.Hour)}, nil, time.Hour)
	if cl := liveCred(t, r, "a", "x"); cl.Status != StatusCooling || !cl.Until.Equal(until) || cl.Reason != string(plugin.CauseQuota) {
		t.Fatalf("error cooldown changed by a probe: %+v", cl)
	}
}

// Attention flags a credential without taking it out of rotation, until
// cleared or restored.
func TestAttention(t *testing.T) {
	r, clk, _ := newRouter(t, quotaTable())
	if err := r.MarkAttention(ctx, "a", "x", "secret not saved"); err != nil {
		t.Fatal(err)
	}
	if cl := liveCred(t, r, "a", "x"); cl.Attention != "secret not saved" || !cl.AttentionSince.Equal(clk.Now()) || cl.Status != StatusActive {
		t.Fatalf("live = %+v", cl)
	}
	if len(mustPlan(t, r, Query{Model: "m"}).Candidates) != 3 {
		t.Fatal("attention took the credential out of rotation")
	}
	_ = r.ClearAttention(ctx, "a", "x")
	if cl := liveCred(t, r, "a", "x"); cl.Attention != "" {
		t.Fatalf("not cleared: %+v", cl)
	}
	_ = r.MarkAttention(ctx, "a", "y", "again")
	_ = r.Restore(ctx, "a", "y")
	if cl := liveCred(t, r, "a", "y"); cl.Attention != "" {
		t.Fatalf("restore kept attention: %+v", cl)
	}
}

func liveCred(t *testing.T, r *Router, up, id string) CredLive {
	t.Helper()
	for _, ul := range r.Live(ctx).Upstreams {
		for _, cl := range ul.Credentials {
			if ul.ID == up && cl.ID == id {
				return cl
			}
		}
	}
	t.Fatalf("no credential %s/%s", up, id)
	return CredLive{}
}
