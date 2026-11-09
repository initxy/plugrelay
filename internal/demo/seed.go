package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/initxy/plugrelay/internal/app"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// world is what seeding created, by the specs' keys.
type world struct {
	groups    map[string]string // group key -> id
	users     map[string]string // user key -> id
	upstreams map[string]string // upstream key -> id
	creds     map[string]string // "upstream/cred" key -> id
	keys      []seededKey
	demoKey   string // plaintext of the first key
}

type seededKey struct {
	spec keySpec
	id   string
}

func (w *world) cred(up, key string) string { return w.creds[up+"/"+key] }

// micro converts USD to micro-units.
func micro(usd float64) int64 { return int64(math.Round(usd * 1e6)) }

// seedConfig creates everything through the admin API, as an operator
// clicking through the console would; every call lands in the audit log.
func seedConfig(c *api, fakeURL string, now time.Time) (*world, error) {
	w := &world{groups: map[string]string{}, users: map[string]string{}, upstreams: map[string]string{}, creds: map[string]string{}}
	adm := userSpecs[0]
	if err := c.v1("POST", "/setup", map[string]any{"admin_username": adm.email, "admin_password": password, "usage": "team",
		"listen_public": false, "site_name": "plugrelay demo", "currency": "USD"}, nil); err != nil {
		return nil, err
	}
	var me struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := c.v1("GET", "/me", nil, &me); err != nil {
		return nil, err
	}
	w.users[adm.key] = me.User.ID
	if err := c.v1("PATCH", "/users/"+me.User.ID, map[string]any{"display_name": adm.display}, nil); err != nil {
		return nil, err
	}

	for _, g := range groupSpecs {
		var out struct {
			ID string `json:"id"`
		}
		if err := c.v1("POST", "/groups", map[string]any{"name": g.name, "description": g.description, "price_multiplier": g.multiplier}, &out); err != nil {
			return nil, err
		}
		w.groups[g.key] = out.ID
	}
	for _, u := range userSpecs[1:] {
		var out struct {
			ID string `json:"id"`
		}
		if err := c.v1("POST", "/users", map[string]any{"username": u.email, "display_name": u.display, "password": password,
			"role": "member", "group_id": w.groups[u.group]}, &out); err != nil {
			return nil, err
		}
		w.users[u.key] = out.ID
	}

	for _, s := range upstreamSpecs {
		var out struct {
			ID string `json:"id"`
		}
		cfg := map[string]any{allowPrivate: true}
		for k, v := range s.vars {
			cfg[k] = v
		}
		if err := c.v1("POST", "/upstreams", map[string]any{"manifest": s.manifest, "name": s.name, "tags": s.tags,
			"config": cfg, "model_sync": false, "quota_probe": "0"}, &out); err != nil {
			return nil, err
		}
		w.upstreams[s.key] = out.ID
		for _, cr := range s.creds {
			var co struct {
				ID string `json:"id"`
			}
			in := map[string]any{"label": cr.label, "kind": cr.kind, "secret": "demo-" + s.key + "-" + cr.key,
				"max_concurrency": cr.maxConcurrency}
			if cr.owner != "" {
				in["owner_id"] = w.users[cr.owner]
			}
			if err := c.v1("POST", "/upstreams/"+out.ID+"/credentials", in, &co); err != nil {
				return nil, err
			}
			w.creds[s.key+"/"+cr.key] = co.ID
		}
	}
	if err := pointUpstreams(c, w.upstreams, fakeURL); err != nil {
		return nil, err
	}

	for _, m := range modelSpecs {
		routes := []map[string]any{}
		for _, r := range m.routes {
			var groups []string
			for _, g := range r.groups {
				groups = append(groups, w.groups[g])
			}
			routes = append(routes, map[string]any{"upstream_id": w.upstreams[r.up], "upstream_model": r.upstreamModel,
				"priority": r.priority, "weight": r.weight, "groups": groups})
		}
		info := map[string]any{"display_name": m.display, "owned_by": m.owner, "context_window": m.context, "max_output": m.maxOutput,
			"tools": true, "vision": m.vision, "reasoning": m.reasoning}
		if err := c.v1("POST", "/models", map[string]any{"name": m.name, "enabled": true, "info": info, "routes": routes}, nil); err != nil {
			return nil, err
		}
		l := listPrice(m.cost)
		if err := c.v1("POST", "/pricing-rules", map[string]any{"match": m.name, "prices": map[string]any{"currency": "USD",
			"input": micro(l.in), "cache_read": micro(l.cacheRead), "cache_write_5m": micro(l.cacheWrite5m),
			"cache_write_1h": micro(l.cacheWrite1h), "output": micro(l.out)}}, nil); err != nil {
			return nil, err
		}
	}
	for _, p := range presetSpecs {
		if err := c.v1("POST", "/presets", map[string]any{"name": p.name, "models": p.models, "strategy": p.strategy}, nil); err != nil {
			return nil, err
		}
	}

	for i, k := range keySpecs {
		settings := map[string]any{}
		for n, v := range k.settings {
			settings[n] = v
		}
		if k.expiresInDays != 0 {
			settings["expires_at"] = now.Add(time.Duration(k.expiresInDays) * 24 * time.Hour).UTC().Truncate(time.Second)
		}
		var out struct {
			Key struct {
				ID string `json:"id"`
			} `json:"key"`
			Secret string `json:"secret"`
		}
		if err := c.v1("POST", "/keys", map[string]any{"name": k.name, "user_id": w.users[k.user], "settings": settings}, &out); err != nil {
			return nil, err
		}
		if k.disabled {
			if err := c.v1("PATCH", "/keys/"+out.Key.ID, map[string]any{"enabled": false}, nil); err != nil {
				return nil, err
			}
		}
		w.keys = append(w.keys, seededKey{spec: k, id: out.Key.ID})
		if i == 0 {
			w.demoKey = out.Secret
		}
	}
	return w, nil
}

// pointUpstreams points every endpoint of the demo upstreams at the fake
// upstream (its port changes between runs).
func pointUpstreams(c *api, ids map[string]string, fakeURL string) error {
	for _, s := range upstreamSpecs {
		eps := []map[string]any{}
		for _, e := range s.endpoints {
			eps = append(eps, map[string]any{"id": e.id, "base_url": fakeURL + e.path})
		}
		if err := c.v1("PATCH", "/upstreams/"+ids[s.key], map[string]any{"endpoints": eps}, nil); err != nil {
			return err
		}
	}
	return nil
}

func listPrice(p price) price {
	return price{p.in * markup, p.cacheRead * markup, p.cacheWrite5m * markup, p.cacheWrite1h * markup, p.out * markup}
}

// seedBilling turns the billing feature on and gives it balances, redeem
// codes (one redeemed by a member) and a plan.
func seedBilling(ctx context.Context, a *app.App, c *api, w *world) error {
	if err := c.v1("PATCH", "/features/billing", map[string]any{"enabled": true}, nil); err != nil {
		return err
	}
	if err := a.Reload(ctx); err != nil {
		return err
	}
	for _, u := range userSpecs {
		if err := c.plugin("meter.balance", "POST", "/adjust", map[string]any{"subject": "user:" + w.users[u.key],
			"amount_micro": micro(u.balance), "note": "Opening balance", "idempotency_key": "demo-open-" + u.key}, nil); err != nil {
			return err
		}
	}
	if err := c.plugin("meter.balance", "PUT", "/credit-limit", map[string]any{"subject": "user:" + w.users["alice"],
		"credit_limit_micro": micro(20)}, nil); err != nil {
		return err
	}
	var batch struct {
		Codes []string `json:"codes"`
	}
	if err := c.plugin("admin.redeem", "POST", "/batches", map[string]any{"count": 5, "amount_micro": micro(10), "max_uses": 1,
		"batch": "launch-week"}, &batch); err != nil {
		return err
	}
	if err := c.plugin("admin.redeem", "POST", "/batches", map[string]any{"count": 2, "amount_micro": micro(50), "max_uses": 20,
		"batch": "partners", "expires_at": time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)}, nil); err != nil {
		return err
	}
	bob := newAPI(c.base)
	if err := bob.login(userSpecs[2].email, password); err != nil {
		return err
	}
	if err := bob.plugin("admin.redeem", "POST", "/redeem", map[string]any{"code": batch.Codes[0]}, nil); err != nil {
		return err
	}
	for _, p := range []map[string]any{
		{"name": "Pro", "description": "USD 30 of usage a month, then pay as you go", "price_micro": micro(20), "period": "month",
			"allowance_unit": "money", "allowance": micro(30), "overage": "balance", "group_id": w.groups["vip"]},
		{"name": "Starter", "description": "5M tokens a week", "price_micro": micro(5), "period": "week",
			"allowance_unit": "tokens", "allowance": 5_000_000, "overage": "block"},
	} {
		if err := c.plugin("admin.subscriptions", "POST", "/plans", p, nil); err != nil {
			return err
		}
	}
	return nil
}

// seedLiveState puts credentials into the states the console shows: one
// cooling down after a rate limit, one quarantined as a suspected ban,
// the Bedrock upstream behind an open breaker, and quota readings.
func seedLiveState(ctx context.Context, a *app.App, w *world) error {
	r := a.Router()
	cand := func(up, cred string) plugin.Candidate {
		return plugin.Candidate{UpstreamID: w.upstreams[up], CredentialID: w.cred(up, cred)}
	}
	now := time.Now()
	if err := r.Report(ctx, cand("anthropic", "backup"), plugin.Verdict{Cause: plugin.CauseRateLimit, Credential: plugin.CredCooldown,
		Until: now.Add(95 * time.Minute)}, "claude-sonnet-4-6"); err != nil {
		return err
	}
	if err := r.Report(ctx, cand("anthropic", "legacy"), plugin.Verdict{Cause: plugin.CauseBanned, Credential: plugin.CredQuarantine},
		"claude-sonnet-4-6"); err != nil {
		return err
	}
	if err := tripBreaker(ctx, a, w); err != nil {
		return err
	}
	for _, q := range []struct {
		up, cred string
		q        plugin.Quota
	}{
		{"anthropic", "primary", plugin.Quota{Remaining: 0.62, Left: 62, Limit: 100, ResetAt: now.Add(3*time.Hour + 20*time.Minute)}},
		{"openai", "team-a", plugin.Quota{Remaining: 0.18, Left: 90, Limit: 500, ResetAt: now.Add(26 * time.Hour)}},
		{"kimi", "main", plugin.Quota{Remaining: 0.91, Left: 182, Limit: 200}},
	} {
		if err := r.ReportQuota(ctx, w.upstreams[q.up], w.cred(q.up, q.cred), q.q, nil, 24*time.Hour); err != nil {
			return err
		}
	}
	return nil
}

// tripBreaker reports enough upstream failures to open Bedrock's breaker.
func tripBreaker(ctx context.Context, a *app.App, w *world) error {
	c := plugin.Candidate{UpstreamID: w.upstreams["bedrock"], CredentialID: w.cred("bedrock", "prod")}
	for range 5 {
		if err := a.Router().Report(ctx, c, plugin.Verdict{Cause: plugin.CauseTransient}, "claude-sonnet-4-6"); err != nil {
			return err
		}
	}
	return nil
}

// seedAudit adds older audit entries, so the audit log has history
// before today's seeding.
func seedAudit(ctx context.Context, st store.Store, w *world, now time.Time) error {
	type change struct {
		Field string `json:"field"`
		From  any    `json:"from,omitempty"`
		To    any    `json:"to,omitempty"`
	}
	entries := []struct {
		ago                   time.Duration
		actor, action, target string
		name                  string
		changes               []change
	}{
		{26 * time.Hour, "admin", "key.rotate", "key:" + w.keys[1].id, "alice-claude-code", nil},
		{2*24*time.Hour + 3*time.Hour, "admin", "credential.update", "credential:" + w.cred("anthropic", "primary"), "Team plan key",
			[]change{{Field: "max_concurrency", From: 8, To: 16}}},
		{3*24*time.Hour + 5*time.Hour, "admin", "group.update", "group:" + w.groups["vip"], "vip",
			[]change{{Field: "price_multiplier", From: 0.9, To: 0.8}}},
		{4 * 24 * time.Hour, "admin", "user.update", "user:" + w.users["carol"], "Carol Wu",
			[]change{{Field: "group_id", From: "vip", To: "default"}}},
		{5*24*time.Hour + 7*time.Hour, "admin", "upstream.update", "upstream:" + w.upstreams["bedrock"], "Bedrock us-east-1",
			[]change{{Field: "enabled", From: false, To: true}}},
		{6 * 24 * time.Hour, "admin", "settings.update", "settings:site", "Site settings",
			[]change{{Field: "site.name", From: "plugrelay", To: "plugrelay demo"}}},
	}
	for _, e := range entries {
		diff, err := json.Marshal(map[string]any{"actor_name": userSpecs[0].display, "target_name": e.name, "ip": userSpecs[0].ip,
			"changes": e.changes})
		if err != nil {
			return err
		}
		if err := st.AddAudit(ctx, &store.Audit{Time: now.Add(-e.ago), Actor: w.users[e.actor], Action: e.action, Target: e.target,
			Diff: diff}); err != nil {
			return fmt.Errorf("audit: %w", err)
		}
	}
	return nil
}
