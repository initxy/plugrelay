package app

import (
	"context"
	"math"
	"slices"
	"strings"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/pipeline"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/match"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/provider/compatible"
	"github.com/initxy/plugrelay/plugins/provider/manifest"
)

// configAllowPrivate is the upstream config flag that lets an upstream
// reach private addresses (a gateway on the local network).
const configAllowPrivate = "allow_private"

// loadManifests adds stored manifests to the provider index; official
// manifests of the same id stay as shipped.
func (a *App) loadManifests(ss *store.Snapshot) {
	idx := compatible.Default()
	for _, sm := range ss.Manifests {
		if _, official := officialManifest(sm.ID); official {
			continue
		}
		m, err := manifest.Parse([]byte(sm.YAML))
		if err != nil {
			a.log.Warn("stored manifest is invalid; skipped", "manifest", sm.ID, "err", err)
			continue
		}
		if err := idx.Put(m); err != nil {
			a.log.Warn("stored manifest rejected", "manifest", sm.ID, "err", err)
		}
	}
}

// endpoints resolves an upstream's effective endpoints: the provider's,
// with stored per-endpoint overrides applied on top.
//
// Opaque state is only restored for the issuer that produced it, so an
// endpoint must not claim an issuer it cannot vouch for: one added without
// an issuer, or one whose base URL was moved off the manifest's without an
// explicit issuer, gets the upstream's own group instead of an empty or
// official one.
func (a *App) endpoints(ctx context.Context, u *store.Upstream) (plugin.Provider, []plugin.Endpoint, error) {
	prov, err := a.plugins.provider(ctx, u.Provider)
	if err != nil {
		return nil, nil, err
	}
	pu := u.Plugin()
	pu.Endpoints = nil
	eps, err := prov.Endpoints(&pu)
	if err != nil {
		return prov, nil, err
	}
	own := "upstream:" + u.ID
	configIssuer, _ := u.Config["issuer"].(string)
	for _, o := range u.Endpoints {
		i := slices.IndexFunc(eps, func(e plugin.Endpoint) bool { return e.ID == o.ID })
		if i < 0 {
			if o.Issuer == "" {
				o.Issuer = own
			}
			eps = append(eps, o)
			continue
		}
		e := &eps[i]
		if o.BaseURL != "" {
			if o.Issuer == "" && configIssuer == "" && strings.TrimRight(o.BaseURL, "/") != strings.TrimRight(e.BaseURL, "/") {
				e.Issuer = own
			}
			e.BaseURL = o.BaseURL
		}
		if o.Protocol != "" {
			e.Protocol = o.Protocol
		}
		if o.Issuer != "" {
			e.Issuer = o.Issuer
		}
		if o.Path != "" {
			e.Path = o.Path
		}
		if o.Query != nil {
			e.Query = o.Query
		}
		if o.DropBetas != nil {
			e.DropBetas = o.DropBetas
		}
	}
	return prov, eps, nil
}

func (a *App) buildSnapshot(ctx context.Context, ss *store.Snapshot, rt *runtime) *pipeline.Snapshot {
	table := &router.Table{Models: map[string]router.Model{}, Presets: map[string]router.Preset{}, Upstreams: map[string]router.Upstream{}}
	snap := &pipeline.Snapshot{Table: table, Upstreams: map[string]*pipeline.Upstream{}}
	creds := map[string][]store.Credential{}
	currency := admin.Currency(ctx, a.store)
	listed := map[string]map[string]pipeline.Price{}
	for _, c := range ss.Credentials {
		creds[c.UpstreamID] = append(creds[c.UpstreamID], c)
	}
	for i := range ss.Upstreams {
		u := &ss.Upstreams[i]
		tu := router.Upstream{ID: u.ID, Enabled: u.Enabled}
		prov, eps, err := a.endpoints(ctx, u)
		if err != nil {
			a.log.Warn("upstream unusable", "upstream", u.ID, "provider", u.Provider, "err", err)
			tu.Enabled = false
			table.Upstreams[u.ID] = tu
			continue
		}
		tu.Endpoints = eps
		pu := u.Plugin()
		pu.Endpoints = eps
		allow, _ := u.Config[configAllowPrivate].(bool)
		up := &pipeline.Upstream{Config: &pu, Provider: prov, Credentials: map[string]*plugin.Credential{}, AllowPrivate: allow}
		up.Headers, up.Proxy, up.Timeouts = admin.UpstreamEgress(u.Config)
		if lp := manifestPrices(prov, &pu, currency); lp != nil {
			listed[u.ID] = lp
		}
		for j := range creds[u.ID] {
			c := &creds[u.ID][j]
			rc := router.Cred{ID: c.ID, Kind: string(c.Kind), OwnerID: c.OwnerID,
				MaxConcurrency: c.MaxConcurrency, Status: router.CredStatus(c.Status), StatusUntil: c.StatusUntil,
				Reason: c.StatusReason}
			pc, err := c.Open(a.box)
			if err != nil {
				// Kept in the table, switched off: dropping it could leave a
				// key-optional upstream with no credentials, routed keyless.
				a.log.Warn("credential cannot be opened; skipped", "upstream", u.ID, "credential", c.ID, "err", err)
				rc.Status, rc.Reason = router.StatusDisabled, "cannot be opened"
			} else {
				up.Credentials[c.ID] = &pc
			}
			tu.Credentials = append(tu.Credentials, rc)
		}
		tu.KeyOptional = keyOptional(prov, &pu)
		up.Keyless = tu.Keyless()
		table.Upstreams[u.ID] = tu
		snap.Upstreams[u.ID] = up
	}
	routes := map[string][]router.Route{}
	for _, r := range ss.Routes {
		routes[r.Model] = append(routes[r.Model], router.Route{UpstreamID: r.UpstreamID, UpstreamModel: r.UpstreamModel,
			Priority: r.Priority, Weight: r.Weight, Groups: r.Groups})
	}
	enabled := map[string]ir.ModelInfo{}
	for _, m := range ss.Models {
		table.Models[m.Name] = router.Model{Name: m.Name, Enabled: m.Enabled, Routes: routes[m.Name]}
		if !m.Enabled {
			continue
		}
		info := m.Info
		info.ID = m.Name
		enabled[m.Name] = info
		snap.Models = append(snap.Models, info)
	}
	// A preset is listed like a model, described by its first enabled model.
	for _, p := range ss.Presets {
		table.Presets[p.Name] = router.Preset{Name: p.Name, Models: p.Models, Strategy: string(p.Strategy)}
		for _, m := range p.Models {
			if info, ok := enabled[m]; ok {
				info.ID, info.DisplayName = p.Name, p.Name
				snap.Models = append(snap.Models, info)
				break
			}
		}
	}
	prices := ss.Prices
	snap.Price = func(upstreamID, upstreamModel string) (pipeline.Price, bool) {
		if p := admin.UpstreamPrice(prices, upstreamID, upstreamModel); p != nil {
			return priceOf(p), true
		}
		p, ok := listed[upstreamID][upstreamModel]
		return p, ok
	}
	snap.ModelPrice = func(model string) (pipeline.Price, bool) {
		if p := admin.ModelPrice(prices, model); p != nil {
			return priceOf(p), true
		}
		return pipeline.Price{}, false
	}
	multipliers := map[string]float64{}
	for _, g := range ss.Groups {
		multipliers[g.ID] = g.PriceMultiplier
	}
	snap.Multiplier = func(groupID string) float64 { return multipliers[groupID] }
	groups := rt.groupModels
	visible := func(c *plugin.Caller, model string) bool {
		if c == nil {
			return true
		}
		if c.KeyID != "" && !modelAllowed(model, a.keys.metaOf(c.KeyID).models()) {
			return false
		}
		return c.GroupID == "" || modelAllowed(model, groups[c.GroupID])
	}
	// A preset grants nothing: the caller sees it while it may use one of
	// its enabled models, and the router skips the others.
	snap.Visible = func(c *plugin.Caller, model string) bool {
		p, ok := table.Presets[model]
		if !ok {
			return visible(c, model)
		}
		return slices.ContainsFunc(p.Models, func(m string) bool { _, on := enabled[m]; return on && visible(c, m) })
	}
	return snap
}

func priceOf(p *store.PriceRule) pipeline.Price {
	return pipeline.Price{Input: p.Input, Output: p.Output, CacheRead: p.CacheRead,
		CacheWrite5m: p.CacheWrite5m, CacheWrite1h: p.CacheWrite1h, Units: p.Units, BatchDiscount: p.BatchDiscount}
}

// keyOptional reports whether an upstream's manifest says the server may
// run without a key (auth.key_optional).
func keyOptional(prov plugin.Provider, u *plugin.Upstream) bool {
	mp, ok := prov.(interface {
		Manifest(*plugin.Upstream) (*manifest.Manifest, error)
	})
	if !ok {
		return false
	}
	m, err := mp.Manifest(u)
	return err == nil && m.Auth.KeyOptional
}

// manifestPrices is the upstream manifest's list prices in micro-units, for
// models priced in the site currency; operator price rules take precedence.
func manifestPrices(prov plugin.Provider, u *plugin.Upstream, currency string) map[string]pipeline.Price {
	mp, ok := prov.(interface {
		Manifest(*plugin.Upstream) (*manifest.Manifest, error)
	})
	if !ok {
		return nil
	}
	m, err := mp.Manifest(u)
	if err != nil || m.Pricing == nil || len(m.Pricing.Models) == 0 {
		return nil
	}
	micro := func(v float64) int64 { return int64(math.Round(v * 1e6)) }
	out := map[string]pipeline.Price{}
	for name, p := range m.Pricing.Models {
		cur := p.Currency
		if cur == "" {
			cur = m.Pricing.Currency
		}
		if !strings.EqualFold(cur, currency) {
			continue
		}
		out[name] = pipeline.Price{Input: micro(p.Input), Output: micro(p.Output), CacheRead: micro(p.CacheRead),
			CacheWrite5m: micro(p.CacheWrite5m), CacheWrite1h: micro(p.CacheWrite1h)}
	}
	return out
}

func modelAllowed(model string, allow []string) bool {
	return len(allow) == 0 || match.Any(model, allow)
}
