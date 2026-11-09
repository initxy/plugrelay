package router

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// Report applies an attempt's verdict. A zero Verdict (CauseNone) records a
// success. model is the upstream model name, used by model-scoped cooldowns.
//
// Failures that are not the credential's fault (CauseRequest, CauseTransient,
// CauseContent) never change the credential, whatever action the verdict
// asks for. Only CauseTransient counts toward the upstream breaker.
func (r *Router) Report(ctx context.Context, c plugin.Candidate, v plugin.Verdict, model string) error {
	ctx = context.WithoutCancel(ctx)
	if v.Cause == plugin.CauseNone {
		return errors.Join(r.breakerSuccess(ctx, c.UpstreamID), r.st.Delete(ctx, kStrikes(c.UpstreamID, c.CredentialID)))
	}
	var errs []error
	if v.Cause == plugin.CauseTransient {
		errs = append(errs, r.breakerFailure(ctx, c.UpstreamID))
	}
	switch v.Cause {
	case plugin.CauseRequest, plugin.CauseTransient, plugin.CauseContent:
		return errors.Join(errs...)
	}
	up, cred := c.UpstreamID, c.CredentialID
	now := r.now()
	ev := CredEvent{UpstreamID: up, CredentialID: cred, Reason: string(v.Cause)}
	action := v.Credential
	if cred == "" && (action == plugin.CredQuarantine || action == plugin.CredManual) {
		// A keyless upstream has no credential an operator could restore.
		action = plugin.CredCooldown
	}
	switch action {
	case plugin.CredCooldown:
		until := v.Until
		if !until.After(now) {
			until = now.Add(r.backoff(ctx, up, cred))
		}
		key := kCool(up, cred)
		if v.ModelScope && model != "" {
			key = kCoolModel(up, cred, model)
			ev.Model = model
		}
		ev.Status, ev.Until = StatusCooling, until
		errs = append(errs, r.setMark(ctx, key, mark{Reason: ev.Reason, Until: until, At: now}, until.Sub(now)))
	case plugin.CredRefresh:
		ev.Status, ev.Until = StatusRefreshing, now.Add(r.opt.ProbeTimeout)
		errs = append(errs, r.setMark(ctx, kRefresh(up, cred), mark{Reason: ev.Reason, Until: ev.Until, At: now}, r.opt.ProbeTimeout))
	case plugin.CredQuarantine:
		ev.Status = StatusQuarantined
		errs = append(errs, r.setMark(ctx, kQuar(up, cred), mark{Reason: ev.Reason, At: now}, 0))
	case plugin.CredManual:
		ev.Status = StatusManual
		errs = append(errs, r.setMark(ctx, kManual(up, cred), mark{Reason: ev.Reason, At: now}, 0))
	default:
		return errors.Join(errs...)
	}
	if r.opt.OnCredential != nil && cred != "" {
		r.opt.OnCredential(ev)
	}
	return errors.Join(errs...)
}

// RefreshDone ends a refresh started by a CredRefresh verdict.
func (r *Router) RefreshDone(ctx context.Context, upstreamID, credID string) error {
	return r.st.Delete(ctx, kRefresh(upstreamID, credID))
}

// Restore clears every live restriction on a credential (an operator's
// decision after a quarantine or manual hold).
func (r *Router) Restore(ctx context.Context, upstreamID, credID string) error {
	errs := []error{
		r.st.Delete(ctx, kQuar(upstreamID, credID)),
		r.st.Delete(ctx, kManual(upstreamID, credID)),
		r.st.Delete(ctx, kCool(upstreamID, credID)),
		r.st.Delete(ctx, kRefresh(upstreamID, credID)),
		r.st.Delete(ctx, kStrikes(upstreamID, credID)),
		r.st.Delete(ctx, kAttn(upstreamID, credID)),
	}
	for _, m := range r.modelsOf(upstreamID) {
		errs = append(errs, r.st.Delete(ctx, kCoolModel(upstreamID, credID, m)))
	}
	if r.opt.OnCredential != nil {
		r.opt.OnCredential(CredEvent{UpstreamID: upstreamID, CredentialID: credID, Status: StatusActive, Reason: "restored"})
	}
	return errors.Join(errs...)
}

func (r *Router) backoff(ctx context.Context, up, cred string) time.Duration {
	n, err := r.st.Incr(ctx, kStrikes(up, cred), 1, r.opt.BackoffMax*4)
	if err != nil || n < 1 {
		n = 1
	}
	d := r.opt.BackoffBase
	for i := int64(1); i < n && d < r.opt.BackoffMax; i++ {
		d *= 2
	}
	return min(d, r.opt.BackoffMax)
}

// breakerBlocks reports whether the upstream is unavailable. In half-open
// state exactly one plan cluster-wide gets the upstream as its probe.
func (r *Router) breakerBlocks(ctx context.Context, up string) (until time.Time, blocked bool) {
	if m, ok := r.getMark(ctx, kBrOpen(up)); ok {
		return m.Until, true
	}
	if _, half := r.getMark(ctx, kBrHalf(up)); !half {
		return time.Time{}, false
	}
	got, err := r.st.SetNX(ctx, kBrProbe(up), []byte("1"), r.opt.ProbeTimeout)
	if err != nil {
		return time.Time{}, false
	}
	if !got {
		return r.now().Add(r.opt.BreakerCooldown), true
	}
	return time.Time{}, false
}

func (r *Router) breakerFailure(ctx context.Context, up string) error {
	now := r.now()
	open := func() error {
		until := now.Add(r.opt.BreakerCooldown)
		return errors.Join(
			r.setMark(ctx, kBrOpen(up), mark{Reason: "failures", Until: until, At: now}, r.opt.BreakerCooldown),
			r.setMark(ctx, kBrHalf(up), mark{At: now}, 24*time.Hour),
			r.st.Delete(ctx, kBrProbe(up)),
			r.st.Delete(ctx, kBrFail(up)),
		)
	}
	if _, half := r.getMark(ctx, kBrHalf(up)); half {
		return open()
	}
	n, err := r.st.Incr(ctx, kBrFail(up), 1, r.opt.BreakerWindow)
	if err != nil {
		return err
	}
	if int(n) >= r.opt.BreakerThreshold {
		return open()
	}
	return nil
}

func (r *Router) breakerSuccess(ctx context.Context, up string) error {
	errs := []error{r.st.Delete(ctx, kBrFail(up))}
	if _, half := r.getMark(ctx, kBrHalf(up)); half {
		errs = append(errs, r.st.Delete(ctx, kBrHalf(up)), r.st.Delete(ctx, kBrProbe(up)))
	}
	return errors.Join(errs...)
}

func (r *Router) modelsOf(upstreamID string) []string {
	var out []string
	for _, m := range r.table.Load().Models {
		for _, rt := range m.Routes {
			if rt.UpstreamID == upstreamID {
				out = appendUniq(out, rt.UpstreamModel)
			}
		}
	}
	return out
}

// Breaker states.
const (
	BreakerClosed   = "closed"
	BreakerOpen     = "open"
	BreakerHalfOpen = "half_open"
)

// Live is the router's current view, for the console. It is read from the
// same state Plan uses; nothing else should hold a copy.
type Live struct {
	Upstreams []UpstreamLive
}

// UpstreamLive is one upstream's live state.
type UpstreamLive struct {
	ID          string
	Enabled     bool
	Breaker     string
	Failures    int       // consecutive failures in the current window
	BreakerOpen time.Time // when an open breaker allows a probe
	Credentials []CredLive
}

// CredLive is one credential's live state.
type CredLive struct {
	ID       string
	Kind     string
	OwnerID  string
	Status   CredStatus
	Reason   string
	Since    time.Time
	Until    time.Time
	InFlight int
	// ModelCooldowns lists model-scoped cooldowns; the credential itself may
	// still be active.
	ModelCooldowns []ModelCooldown
	// Quota is the last quota probe reading, nil when never probed.
	Quota *QuotaReading
	// Attention is why an operator should look at the credential although
	// it stays in rotation; "" when nothing is wrong.
	Attention      string
	AttentionSince time.Time
}

// ModelCooldown is a cooldown limited to one upstream model.
type ModelCooldown struct {
	Model  string
	Reason string
	Until  time.Time
}

// Live returns the live state of every upstream in the current table.
func (r *Router) Live(ctx context.Context) Live {
	t := r.table.Load()
	now := r.now()
	var out Live
	for _, up := range sortedUpstreams(t) {
		ul := UpstreamLive{ID: up.ID, Enabled: up.Enabled, Breaker: BreakerClosed}
		if m, ok := r.getMark(ctx, kBrOpen(up.ID)); ok {
			ul.Breaker, ul.BreakerOpen = BreakerOpen, m.Until
		} else if _, ok := r.getMark(ctx, kBrHalf(up.ID)); ok {
			ul.Breaker = BreakerHalfOpen
		}
		ul.Failures = r.counter(ctx, kBrFail(up.ID))
		models := r.modelsOf(up.ID)
		for _, c := range up.Credentials {
			cl := CredLive{ID: c.ID, Kind: c.Kind, OwnerID: c.OwnerID, Status: c.Status, Reason: c.Reason, Until: c.StatusUntil}
			if cl.Status == "" || ((cl.Status == StatusCooling || cl.Status == StatusRefreshing) && !c.StatusUntil.IsZero() && !now.Before(c.StatusUntil)) {
				cl.Status, cl.Reason, cl.Until = StatusActive, "", time.Time{}
			}
			if cl.Status == StatusActive {
				for _, probe := range []struct {
					key string
					st  CredStatus
				}{
					{kQuar(up.ID, c.ID), StatusQuarantined},
					{kManual(up.ID, c.ID), StatusManual},
					{kRefresh(up.ID, c.ID), StatusRefreshing},
					{kCool(up.ID, c.ID), StatusCooling},
				} {
					if m, ok := r.getMark(ctx, probe.key); ok && (probe.st != StatusCooling || now.Before(m.Until)) {
						cl.Status, cl.Reason, cl.Since, cl.Until = probe.st, m.Reason, m.At, m.Until
						break
					}
				}
			}
			for _, m := range models {
				if mk, ok := r.getMark(ctx, kCoolModel(up.ID, c.ID, m)); ok && now.Before(mk.Until) {
					cl.ModelCooldowns = append(cl.ModelCooldowns, ModelCooldown{Model: m, Reason: mk.Reason, Until: mk.Until})
				}
			}
			cl.InFlight = r.counter(ctx, kInflight(up.ID, c.ID))
			if qr, ok := r.QuotaOf(ctx, up.ID, c.ID); ok {
				cl.Quota = &qr
			}
			if m, ok := r.getMark(ctx, kAttn(up.ID, c.ID)); ok {
				cl.Attention, cl.AttentionSince = m.Reason, m.At
			}
			ul.Credentials = append(ul.Credentials, cl)
		}
		out.Upstreams = append(out.Upstreams, ul)
	}
	return out
}

func sortedUpstreams(t *Table) []Upstream {
	ids := make([]string, 0, len(t.Upstreams))
	for id := range t.Upstreams {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]Upstream, len(ids))
	for i, id := range ids {
		out[i] = t.Upstreams[id]
	}
	return out
}
