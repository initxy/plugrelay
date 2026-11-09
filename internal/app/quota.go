package app

import (
	"context"
	"time"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/job/quotaprobe"
)

// quotaTargets lists every credential of an enabled upstream whose provider
// can probe quota, except those an operator or the router took out for
// good (disabled, quarantined, manual hold): probing a suspended account
// helps no one.
type quotaTargets struct{ a *App }

var _ quotaprobe.Targets = quotaTargets{}

func (t quotaTargets) QuotaTargets(ctx context.Context) ([]quotaprobe.Target, error) {
	rt := t.a.runtime()
	live := map[string]router.CredLive{}
	for _, ul := range t.a.router.Live(ctx).Upstreams {
		for _, cl := range ul.Credentials {
			live[ul.ID+"|"+cl.ID] = cl
		}
	}
	var out []quotaprobe.Target
	for _, u := range rt.store.Upstreams {
		up := rt.snap.Upstreams[u.ID]
		if !u.Enabled || up == nil {
			continue
		}
		prober, ok := up.Provider.(plugin.QuotaProber)
		if !ok {
			continue
		}
		every := admin.QuotaProbeInterval(u.Config)
		for _, c := range rt.snap.Table.Upstreams[u.ID].Credentials {
			pc := up.Credentials[c.ID]
			cl := live[u.ID+"|"+c.ID]
			if pc == nil {
				continue
			}
			switch cl.Status {
			case router.StatusDisabled, router.StatusQuarantined, router.StatusManual:
				continue
			}
			tg := quotaprobe.Target{Upstream: up.Config, Credential: t.a.pipe.CurrentSecret(u.ID, pc), Prober: prober, Every: every}
			if cl.Quota != nil {
				tg.Last = cl.Quota.At
			}
			out = append(out, tg)
		}
	}
	return out, nil
}

// quotaRecorder stores readings as router live state.
type quotaRecorder struct{ a *App }

func (r quotaRecorder) RecordQuota(ctx context.Context, upstreamID, credID string, q plugin.Quota, err error, ttl time.Duration) error {
	return r.a.router.ReportQuota(ctx, upstreamID, credID, q, err, ttl)
}

func (h *host) QuotaTargets() quotaprobe.Targets   { return quotaTargets{h.a} }
func (h *host) QuotaRecorder() quotaprobe.Recorder { return quotaRecorder{h.a} }

var _ quotaprobe.QuotaHost = (*host)(nil)
