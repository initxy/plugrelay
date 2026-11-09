package quotaprobe

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

type prober struct {
	mu    sync.Mutex
	calls []string
	q     plugin.Quota
	err   error
}

func (p *prober) Quota(_ context.Context, _ *plugin.Upstream, c *plugin.Credential) (plugin.Quota, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, c.ID)
	return p.q, p.err
}

type reading struct {
	q   plugin.Quota
	err error
	ttl time.Duration
}

type recorder struct {
	mu   sync.Mutex
	got  map[string]reading
	fail error
}

func (r *recorder) RecordQuota(_ context.Context, up, cred string, q plugin.Quota, err error, ttl time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got[up+"/"+cred] = reading{q, err, ttl}
	return r.fail
}

type targets []Target

func (t targets) QuotaTargets(context.Context) ([]Target, error) { return t, nil }

func target(up, cred string, p plugin.QuotaProber, every time.Duration, last time.Time) Target {
	return Target{Upstream: &plugin.Upstream{ID: up}, Credential: &plugin.Credential{ID: cred}, Prober: p, Every: every, Last: last}
}

func TestRunProbesDueCredentials(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ok := &prober{q: plugin.Quota{Remaining: 0.4, ResetAt: now.Add(time.Hour)}}
	bad := &prober{err: errors.New("503")}
	none := &prober{err: plugin.ErrNoQuota}
	ts := targets{
		target("u", "never", ok, 0, time.Time{}),                       // never probed: due
		target("u", "stale", ok, 0, now.Add(-11*time.Minute)),          // job interval 10m passed
		target("u", "fresh", ok, 0, now.Add(-2*time.Minute)),           // not due
		target("v", "own", ok, 2*time.Minute, now.Add(-3*time.Minute)), // upstream's 2m passed
		target("v", "off", ok, -1, time.Time{}),                        // upstream turned probing off
		target("w", "failing", bad, 0, time.Time{}),
		target("x", "noprobe", none, 0, time.Time{}),
		{Upstream: &plugin.Upstream{ID: "y"}, Credential: &plugin.Credential{ID: "nil"}},
	}
	rec := &recorder{got: map[string]reading{}}
	j := New(ts, rec)
	j.now = func() time.Time { return now }
	j.jitter = func(time.Duration) time.Duration { return 0 }

	err := j.Run(context.Background())
	if err == nil || !errors.Is(err, bad.err) {
		t.Fatalf("Run error = %v, want the failing probe", err)
	}
	if len(ok.calls) != 3 {
		t.Errorf("probed %v, want never, stale, own", ok.calls)
	}
	for k, want := range map[string]time.Duration{"u/never": 30 * time.Minute, "u/stale": 30 * time.Minute, "v/own": 6 * time.Minute} {
		r, in := rec.got[k]
		if !in || r.q.Remaining != 0.4 || r.ttl != want || r.err != nil {
			t.Errorf("%s recorded %+v (in=%v), want ttl %s", k, r, in, want)
		}
	}
	if r := rec.got["w/failing"]; r.err == nil {
		t.Error("failed probe not recorded, so it would be retried every tick")
	}
	for _, k := range []string{"u/fresh", "v/off", "x/noprobe", "y/nil"} {
		if _, in := rec.got[k]; in {
			t.Errorf("%s recorded", k)
		}
	}

	// Jitter lets a credential go a little early, never late.
	j.jitter = func(m time.Duration) time.Duration { return m }
	if _, due := j.due(target("u", "c", ok, 0, now.Add(-9*time.Minute-time.Second))); !due {
		t.Error("jitter did not pull the probe forward")
	}
	if _, due := j.due(target("u", "c", ok, 0, now.Add(-8*time.Minute))); due {
		t.Error("jitter beyond a tenth of the interval")
	}
}

func TestRecordFailureReported(t *testing.T) {
	rec := &recorder{got: map[string]reading{}, fail: errors.New("state down")}
	j := New(targets{target("u", "c", &prober{q: plugin.Quota{Remaining: 1}}, 0, time.Time{})}, rec)
	if err := j.Run(context.Background()); err == nil || !errors.Is(err, rec.fail) {
		t.Fatalf("Run = %v", err)
	}
}

func TestEveryTicksAtMostAMinute(t *testing.T) {
	j := New(targets{}, &recorder{})
	if j.Every() != time.Minute {
		t.Fatal(j.Every())
	}
	j.every = 30 * time.Second
	if j.Every() != 30*time.Second {
		t.Fatal(j.Every())
	}
}
