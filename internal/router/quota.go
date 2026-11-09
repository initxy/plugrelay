package router

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

func kQuota(up, cred string) string { return "rt:qt:" + credKey(up, cred) }
func kAttn(up, cred string) string  { return "rt:att:" + credKey(up, cred) }

// ReasonQuotaProbe marks cooldowns set from a quota probe; a later reading
// with quota left lifts only those, never one an upstream error set.
const ReasonQuotaProbe = "quota used up (quota probe)"

// QuotaReading is a credential's last probed upstream quota, shared by
// every node through the state backend.
type QuotaReading struct {
	Remaining float64   `json:"r"` // fraction 0..1, -1 unknown
	Left      float64   `json:"l,omitempty"`
	Limit     float64   `json:"m,omitempty"`
	ResetAt   time.Time `json:"z,omitzero"`
	At        time.Time `json:"a"`
	// Err is why the last probe failed; the figures are from the last
	// probe that worked.
	Err string `json:"e,omitempty"`
}

// ReportQuota records a probe result for ttl (how long it stays
// believable without a new probe). A used-up quota cools the credential
// until it resets, or for ttl when the upstream gives no reset time; a
// reading with quota left lifts a cooldown an earlier reading set. A
// failed probe (probeErr) keeps the previous figures and records why.
func (r *Router) ReportQuota(ctx context.Context, upstreamID, credID string, q plugin.Quota, probeErr error, ttl time.Duration) error {
	ctx = context.WithoutCancel(ctx)
	now := r.now()
	qr := QuotaReading{Remaining: q.Remaining, Left: q.Left, Limit: q.Limit, ResetAt: q.ResetAt, At: now}
	if probeErr != nil {
		prev, ok := r.QuotaOf(ctx, upstreamID, credID)
		if !ok {
			prev = QuotaReading{Remaining: -1}
		}
		qr = prev
		qr.At, qr.Err = now, probeErr.Error()
		b, _ := json.Marshal(qr)
		return r.st.Set(ctx, kQuota(upstreamID, credID), b, ttl)
	}
	b, _ := json.Marshal(qr)
	errs := []error{r.st.Set(ctx, kQuota(upstreamID, credID), b, ttl)}
	switch {
	case q.Remaining == 0:
		until := q.ResetAt
		if !until.After(now) {
			until = now.Add(ttl)
		}
		if m, ok := r.getMark(ctx, kCool(upstreamID, credID)); ok && !m.Until.Before(until) {
			break // already out at least that long
		}
		errs = append(errs, r.setMark(ctx, kCool(upstreamID, credID), mark{Reason: ReasonQuotaProbe, Until: until, At: now}, until.Sub(now)))
		if r.opt.OnCredential != nil {
			r.opt.OnCredential(CredEvent{UpstreamID: upstreamID, CredentialID: credID, Status: StatusCooling, Reason: ReasonQuotaProbe, Until: until})
		}
	case q.Remaining > 0:
		if m, ok := r.getMark(ctx, kCool(upstreamID, credID)); ok && m.Reason == ReasonQuotaProbe {
			errs = append(errs, r.st.Delete(ctx, kCool(upstreamID, credID)))
		}
	}
	return errors.Join(errs...)
}

// QuotaOf returns a credential's last quota reading.
func (r *Router) QuotaOf(ctx context.Context, upstreamID, credID string) (QuotaReading, bool) {
	b, ok, err := r.st.Get(ctx, kQuota(upstreamID, credID))
	if err != nil || !ok {
		return QuotaReading{}, false
	}
	var qr QuotaReading
	if json.Unmarshal(b, &qr) != nil {
		return QuotaReading{}, false
	}
	return qr, true
}

// MarkAttention flags a credential for an operator without taking it out
// of rotation (a refreshed secret that could not be saved). The flag stays
// until ClearAttention or Restore.
func (r *Router) MarkAttention(ctx context.Context, upstreamID, credID, reason string) error {
	return r.setMark(context.WithoutCancel(ctx), kAttn(upstreamID, credID), mark{Reason: reason, At: r.now()}, 0)
}

// ClearAttention removes a MarkAttention flag.
func (r *Router) ClearAttention(ctx context.Context, upstreamID, credID string) error {
	return r.st.Delete(context.WithoutCancel(ctx), kAttn(upstreamID, credID))
}
