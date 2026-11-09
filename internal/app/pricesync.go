package app

import (
	"context"
	"time"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/plugins/job/pricesync"
)

// priceSyncStore is the store side of job.pricesync.
type priceSyncStore struct{ st store.Store }

var (
	_ pricesync.Store     = priceSyncStore{}
	_ pricesync.StoreHost = (*host)(nil)
)

func (h *host) PriceSyncStore() pricesync.Store { return priceSyncStore{h.a.store} }

// Models lists every model with its routes; a route's provider is the
// upstream's manifest id, else its provider plugin id.
func (p priceSyncStore) Models(ctx context.Context) ([]pricesync.Model, error) {
	ms, err := p.st.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	routes, err := p.st.ListRoutes(ctx, "")
	if err != nil {
		return nil, err
	}
	ups, err := p.st.ListUpstreams(ctx)
	if err != nil {
		return nil, err
	}
	provider := map[string]string{}
	for _, u := range ups {
		provider[u.ID] = u.Manifest
		if u.Manifest == "" {
			provider[u.ID] = u.Provider
		}
	}
	out := make([]pricesync.Model, 0, len(ms))
	for _, m := range ms {
		pm := pricesync.Model{Name: m.Name}
		for _, r := range routes {
			if r.Model == m.Name {
				pm.Routes = append(pm.Routes, pricesync.Route{Provider: provider[r.UpstreamID], UpstreamModel: r.UpstreamModel})
			}
		}
		out = append(out, pm)
	}
	return out, nil
}

func (p priceSyncStore) Currency(ctx context.Context) string { return admin.Currency(ctx, p.st) }

func (p priceSyncStore) Save(ctx context.Context, s []pricesync.Suggestion, at time.Time) error {
	rows := make([]store.PriceSync, len(s))
	for i, x := range s {
		rows[i] = store.PriceSync{Model: x.Model, Ref: x.Ref, Input: x.Input, CacheRead: x.CacheRead, CacheWrite5m: x.CacheWrite5m,
			CacheWrite1h: x.CacheWrite1h, Output: x.Output, Currency: x.Currency, Info: x.Info, FetchedAt: at}
	}
	return p.st.SavePriceCatalog(ctx, rows)
}
