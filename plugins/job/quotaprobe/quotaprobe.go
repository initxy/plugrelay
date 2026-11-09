// Package quotaprobe is the job.quotaprobe plugin: it asks each credential's
// provider for the remaining upstream quota (plugin.QuotaProber) and hands
// the reading to the core, which shares it with every node, fills
// plugin.Candidate.QuotaReset from it and keeps a credential whose quota is
// used up out of rotation until the reset.
//
// The job ticks every minute; a credential is due when its upstream's
// interval (else the job's interval setting) has passed since its last
// probe on any node, minus up to a tenth of jitter so credentials added
// together drift apart. Providers that cannot probe an upstream return
// plugin.ErrNoQuota and are skipped without a record.
package quotaprobe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// ID is the plugin id.
const ID = "job.quotaprobe"

func init() { plugin.Register(ID, func() plugin.Plugin { return New(nil, nil) }) }

// Target is one credential to probe.
type Target struct {
	Upstream   *plugin.Upstream
	Credential *plugin.Credential
	Prober     plugin.QuotaProber
	// Every is the upstream's own probe interval; 0 takes the job's,
	// negative turns probing off for the upstream.
	Every time.Duration
	// Last is when any node last probed this credential; zero if never.
	Last time.Time
}

// Targets enumerates the credentials that may be probed.
type Targets interface {
	QuotaTargets(ctx context.Context) ([]Target, error)
}

// Recorder stores a probe result. err is the probe's failure, with q then
// meaningless; ttl is how long the reading stays believable.
type Recorder interface {
	RecordQuota(ctx context.Context, upstreamID, credentialID string, q plugin.Quota, err error, ttl time.Duration) error
}

// QuotaHost is implemented by a Host that hands the job its targets and
// where readings go.
type QuotaHost interface {
	QuotaTargets() Targets
	QuotaRecorder() Recorder
}

// Defaults.
const (
	DefaultInterval = 10 * time.Minute
	tick            = time.Minute
	probeTimeout    = 20 * time.Second
	parallel        = 4
)

// Job implements plugin.Job.
type Job struct {
	targets Targets
	rec     Recorder
	log     *slog.Logger
	every   time.Duration
	now     func() time.Time
	jitter  func(max time.Duration) time.Duration
}

// New returns the job. Nil arguments are taken from the Host in Init.
func New(targets Targets, rec Recorder) *Job {
	return &Job{targets: targets, rec: rec, log: slog.New(slog.DiscardHandler), every: DefaultInterval, now: time.Now,
		jitter: func(m time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(m) + 1)) }}
}

// Info implements plugin.Plugin.
func (j *Job) Info() plugin.Info {
	return plugin.Info{
		ID:          ID,
		Kind:        plugin.KindJob,
		Version:     "1.0.0",
		Description: "Probes upstream credential quota where the provider can; feeds the quota_reset routing strategy and keeps used-up credentials out until their reset.",
		FailPolicy:  plugin.FailOpen,
		Settings: []plugin.Setting{
			{Key: "interval", Type: plugin.TypeDuration, Default: DefaultInterval.String(), Scopes: plugin.Global,
				Merge: plugin.Override, Label: plugin.T("Check remaining quota every", "多久查一次剩余额度"),
				Help: plugin.T("For each credential of upstreams that can report it, unless the upstream sets its own interval.", "对能查询额度的上游的每个凭证生效；上游自己设了间隔的以上游为准。")},
		},
	}
}

// Init implements plugin.Initializer.
func (j *Job) Init(_ context.Context, h plugin.Host) error {
	if qh, ok := h.(QuotaHost); ok {
		if j.targets == nil {
			j.targets = qh.QuotaTargets()
		}
		if j.rec == nil {
			j.rec = qh.QuotaRecorder()
		}
	}
	if j.targets == nil || j.rec == nil {
		return errors.New("quotaprobe: no targets: construct with New(targets, recorder) or provide a Host implementing QuotaHost")
	}
	j.log = h.Logger()
	if d := h.Settings().Get("interval").Duration(); d > 0 {
		j.every = d
	}
	return nil
}

// Every implements plugin.Job.
func (j *Job) Every() time.Duration { return min(tick, j.every) }

// Run implements plugin.Job: probes every due credential, a few at a time.
// A failed probe is recorded (so the console shows it and the credential is
// not retried before its interval) and joined into the returned error.
func (j *Job) Run(ctx context.Context) error {
	ts, err := j.targets.QuotaTargets(ctx)
	if err != nil {
		return err
	}
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
		sem  = make(chan struct{}, parallel)
	)
	for _, t := range ts {
		every, ok := j.due(t)
		if !ok {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return errors.Join(append(errs, ctx.Err())...)
		}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := j.Probe(ctx, t, every); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (j *Job) due(t Target) (time.Duration, bool) {
	if t.Prober == nil || t.Upstream == nil || t.Credential == nil || t.Every < 0 {
		return 0, false
	}
	every := t.Every
	if every == 0 {
		every = j.every
	}
	if t.Last.IsZero() {
		return every, true
	}
	return every, j.now().Sub(t.Last) >= every-j.jitter(every/10)
}

// Probe asks one credential's quota and records the reading for three
// intervals.
func (j *Job) Probe(ctx context.Context, t Target, every time.Duration) error {
	up, cred := t.Upstream.ID, t.Credential.ID
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	q, err := t.Prober.Quota(pctx, t.Upstream, t.Credential)
	cancel()
	if errors.Is(err, errors.ErrUnsupported) {
		return nil
	}
	if rerr := j.rec.RecordQuota(ctx, up, cred, q, err, 3*every); rerr != nil {
		return fmt.Errorf("%s/%s: record quota: %w", up, cred, rerr)
	}
	if err != nil {
		return fmt.Errorf("%s/%s: probe quota: %w", up, cred, err)
	}
	j.log.Debug("quota probed", "upstream", up, "credential", cred, "remaining", q.Remaining, "reset", q.ResetAt)
	return nil
}
