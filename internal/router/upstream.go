package router

import (
	"context"
	"sort"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// PlanUpstream returns candidates for a request addressed to one endpoint
// of one upstream rather than to a model (native passthrough): one per
// credential the caller may use that is not cooling, held or at its cap,
// credentials recently used by the caller first, then fewest in flight.
// Candidates carry no upstream model. An open breaker blocks the upstream;
// a half-open one is not probed, since these requests report no verdict.
//
// Errors are *PlanError with Model set to the upstream id: ErrNoRoute when
// the upstream or endpoint is unknown or disabled, or the caller may use
// none of its credentials; ErrCooling, ErrBusy or ErrExhausted otherwise.
func (r *Router) PlanUpstream(ctx context.Context, upstreamID, endpointID string, caller plugin.Caller) (*Plan, error) {
	t := r.table.Load()
	now := r.now()
	up, ok := t.Upstreams[upstreamID]
	var ep *plugin.Endpoint
	if ok {
		for i := range up.Endpoints {
			if up.Endpoints[i].ID == endpointID {
				ep = &up.Endpoints[i]
				break
			}
		}
	}
	if !ok || !up.Enabled || ep == nil {
		return nil, &PlanError{Kind: ErrNoRoute, Model: upstreamID}
	}
	d := drops{routes: 1, reachable: 1, capable: 1}
	if m, open := r.getMark(ctx, kBrOpen(up.ID)); open {
		d.cool(m.Until)
		return nil, d.err(upstreamID)
	}
	creds := r.usableCreds(ctx, Query{Caller: caller}, up, "", now, &d)
	recent := map[string]bool{}
	if k := callerKey(caller); k != "" {
		for _, c := range r.loadRecent(ctx, k) {
			recent[c] = true
		}
	}
	r.randMu.Lock()
	r.opt.Rand.Shuffle(len(creds), func(i, j int) { creds[i], creds[j] = creds[j], creds[i] })
	r.randMu.Unlock()
	sort.SliceStable(creds, func(i, j int) bool {
		ui, uj := recent[credKey(up.ID, creds[i].c.ID)], recent[credKey(up.ID, creds[j].c.ID)]
		if ui != uj {
			return ui
		}
		return creds[i].inflight < creds[j].inflight
	})
	if len(creds) == 0 {
		return nil, d.err(upstreamID)
	}
	plan := &Plan{served: map[string]Served{}}
	for _, cr := range creds {
		plan.Candidates = append(plan.Candidates, plugin.Candidate{
			UpstreamID:   up.ID,
			EndpointID:   ep.ID,
			CredentialID: cr.c.ID,
			Protocol:     ep.Protocol,
			SameProtocol: true,
			Fidelity:     ir.Lossless,
			InFlight:     cr.inflight,
		})
	}
	return plan, nil
}
