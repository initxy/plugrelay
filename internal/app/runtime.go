package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/initxy/plugrelay/internal/features"
	"github.com/initxy/plugrelay/internal/pipeline"
	"github.com/initxy/plugrelay/internal/settings"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/auth/apikey"
	"github.com/initxy/plugrelay/plugins/job/modelsync"
	"github.com/initxy/plugrelay/plugins/selector/routing"
	"github.com/initxy/plugrelay/plugins/sink/metrics"
	"github.com/initxy/plugrelay/plugins/sink/requestlog"
	"github.com/prometheus/client_golang/prometheus"
)

// Names for the optional Host interfaces built-in plugins type-assert.
type (
	apikeyLookup    = apikey.KeyLookup
	recordWriterAPI = requestlog.RecordWriter
	purgerAPI       = requestlog.Purger
	catalogAPI      = modelsync.ModelCatalog
	targetsAPI      = modelsync.Targets
)

var (
	_ apikey.LookupHost         = (*host)(nil)
	_ apikey.ClientIPHost       = (*host)(nil)
	_ requestlog.WriterHost     = (*host)(nil)
	_ requestlog.PurgerHost     = (*host)(nil)
	_ requestlog.CallerSettings = (*host)(nil)
	_ modelsync.CatalogHost     = (*host)(nil)
	_ plugin.StateHost          = (*host)(nil)
)

// runtime is one immutable configuration generation.
type runtime struct {
	version  int64
	store    *store.Snapshot
	snap     *pipeline.Snapshot
	chain    *pipeline.Chain
	auth     []plugin.Authenticator
	sinks    []plugin.Sink
	jobs     []plugin.Job
	resolver *settings.Resolver
	states   map[string]pluginState
	// blocked holds enabled plugins skipped because a plugin they require
	// is not running, with the reason.
	blocked map[string]string
	// groupModels is each group's model allowlist.
	groupModels map[string][]string
}

var emptyRuntime = &runtime{snap: &pipeline.Snapshot{}, chain: &pipeline.Chain{},
	resolver: settings.NewResolver(nil, nil, nil), states: map[string]pluginState{}, store: &store.Snapshot{}}

func (a *App) runtime() *runtime {
	if rt := a.cur.Load(); rt != nil {
		return rt
	}
	return emptyRuntime
}

func (rt *runtime) job(id string) plugin.Job {
	for _, j := range rt.jobs {
		if j.Info().ID == id {
			return j
		}
	}
	return nil
}

// poll rebuilds the runtime whenever the store version moves.
func (a *App) poll(ctx context.Context) {
	t := time.NewTicker(PollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.rebuild(ctx, false); err != nil && ctx.Err() == nil {
				a.log.Error("configuration reload failed; keeping the previous one", "err", err)
			}
		}
	}
}

// Reload rebuilds the runtime now if the store changed.
func (a *App) Reload(ctx context.Context) error { return a.rebuild(ctx, false) }

func (a *App) rebuild(ctx context.Context, force bool) (err error) {
	a.rebuildMu.Lock()
	defer a.rebuildMu.Unlock()
	defer func() {
		switch {
		case err == nil:
			a.reloadErr.Store(nil)
		case ctx.Err() == nil:
			msg := err.Error()
			a.reloadErr.Store(&msg)
		}
	}()
	v, err := a.store.Version(ctx)
	if err != nil {
		return err
	}
	if cur := a.cur.Load(); cur != nil && !force && cur.version == v {
		return nil
	}
	ss, err := a.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	rt, err := a.build(ctx, ss)
	if err != nil {
		return err
	}
	a.cur.Store(rt)
	// The console reads router.Live; without this the router keeps the
	// previous table until the next client request.
	a.router.SetTable(rt.snap.Table)
	a.syncExternal(rt.states)
	if a.started.Load() {
		a.plugins.startJobs(a.ctx)
	}
	a.log.Debug("configuration loaded", "version", ss.Version, "upstreams", len(ss.Upstreams), "models", len(ss.Models))
	return nil
}

// build turns a store snapshot into a runtime generation. It fails, before
// anything goes live, when the plugin configuration cannot run.
func (a *App) build(ctx context.Context, ss *store.Snapshot) (*runtime, error) {
	rt := &runtime{version: ss.Version, store: ss, chain: &pipeline.Chain{}, groupModels: map[string][]string{},
		blocked: map[string]string{}}
	a.loadManifests(ss)
	known := a.plugins.known()
	states, err := a.states(known, ss.Plugins, ss.Features, a.log)
	if err != nil {
		return nil, fmt.Errorf("plugins: %w", err)
	}
	rt.states = states
	decls := map[string][]plugin.Setting{}
	for _, p := range known {
		if s := p.Info().Settings; len(s) > 0 {
			decls[p.Info().ID] = s
		}
	}
	groups := map[string]map[string]json.RawMessage{}
	for _, g := range ss.Groups {
		var ms []string
		_ = json.Unmarshal(g.Settings["models"], &ms)
		rt.groupModels[g.ID] = ms
		groups[g.ID] = g.Settings
	}
	rt.resolver = settings.NewResolver(decls, ss.Settings, a.box).WithRecords(
		func(scope plugin.Scope, id, pluginID, key string) (json.RawMessage, bool) {
			switch scope {
			case plugin.Group:
				return settings.FromRecord(scope, pluginID, key, groups[id])
			case plugin.Key:
				if _, owned := settings.RecordOwned(scope, pluginID, key); owned {
					return settings.FromRecord(scope, pluginID, key, a.keys.metaOf(id).settings)
				}
			}
			return nil, false
		}).WarnInvalid(a.log)
	// Plugins initialize against the new settings, so they go live first.
	a.resolver.Store(rt.resolver)
	a.buildChain(ctx, rt, known)
	rt.snap = a.buildSnapshot(ctx, ss, rt)
	return rt, nil
}

// featureOn reports whether feature id is on in the running configuration.
func (a *App) featureOn(id string) bool {
	on := features.State{}
	if rt := a.runtime(); rt != nil && rt.store != nil {
		for _, f := range rt.store.Features {
			on[f.ID] = f.Enabled
		}
	}
	return on.On(id)
}

// enabledPlugins are the running relay's enabled plugins that started,
// providers aside, sorted by id.
func (a *App) enabledPlugins(ctx context.Context) []plugin.Plugin {
	rt := a.runtime()
	var out []plugin.Plugin
	for _, p := range a.plugins.known() {
		id := p.Info().ID
		if st := rt.states[id]; !st.enabled || p.Info().Kind == plugin.KindProvider || rt.blocked[id] != "" {
			continue
		}
		if p, err := a.plugins.ready(ctx, id); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func (a *App) buildChain(ctx context.Context, rt *runtime, known []plugin.Plugin) {
	type entry struct {
		id    string
		order int
	}
	var on []entry
	for _, p := range known {
		id := p.Info().ID
		if st := rt.states[id]; st.enabled && p.Info().Kind != plugin.KindProvider {
			on = append(on, entry{id, st.order})
		}
	}
	sort.SliceStable(on, func(i, j int) bool {
		if on[i].order != on[j].order {
			return on[i].order < on[j].order
		}
		return on[i].id < on[j].id
	})
	ready := map[string]plugin.Plugin{}
	down := map[string]bool{}
	for _, e := range on {
		if p, err := a.plugins.ready(ctx, e.id); err == nil {
			ready[e.id] = p
		} else {
			down[e.id] = true
		}
	}
	// A plugin whose requirement failed to start does not run either; it
	// would work against a sink or meter that is not there.
	for changed := true; changed; {
		changed = false
		for id, p := range ready {
			for _, req := range p.Info().Requires {
				if !down[req] {
					continue
				}
				why := fmt.Sprintf("requires %s, which is not running", req)
				a.log.Error("plugin not started: a plugin it requires is not running", "plugin", id, "requires", req,
					"fail_policy", p.Info().EffectiveFailPolicy())
				delete(ready, id)
				down[id], rt.blocked[id] = true, why
				changed = true
				break
			}
		}
	}
	for _, e := range on {
		p, ok := ready[e.id]
		if !ok {
			continue
		}
		switch p.Info().Kind {
		case plugin.KindAuth:
			if au, ok := p.(plugin.Authenticator); ok {
				rt.auth = append(rt.auth, au)
			}
		case plugin.KindMiddleware:
			rt.chain.Plugins = append(rt.chain.Plugins, p)
		case plugin.KindSelector:
			// The routing strategy ranks unless an enabled external
			// selector takes its place.
			if sel, ok := p.(plugin.Selector); ok && (rt.chain.Selector == nil || rt.chain.Selector.Info().ID == routing.ID) {
				rt.chain.Selector = sel
			}
		case plugin.KindMeter:
			if m, ok := p.(plugin.Meter); ok {
				rt.chain.Meter = chainMeters(rt.chain.Meter, m)
			}
		case plugin.KindSink:
			if s, ok := p.(plugin.Sink); ok {
				rt.sinks = append(rt.sinks, s)
			}
		case plugin.KindJob:
			if j, ok := p.(plugin.Job); ok {
				rt.jobs = append(rt.jobs, j)
			}
		}
	}
	rt.chain.Pick = pickPreset
	registerPluginMetrics(rt)
}

// pickPreset orders a requested preset's candidates by its pick strategy.
func pickPreset(strategy string, cands []plugin.Candidate) []plugin.Candidate {
	switch store.PresetStrategy(strategy) {
	case store.PresetCheapest:
		return routing.Rank(routing.Cheapest, cands)
	case store.PresetFastest:
		return routing.Rank(routing.Fastest, cands)
	}
	return cands
}

// registerPluginMetrics adds the series of running plugins that collect
// their own (guardrail verdicts) to the metrics sink's registry. A plugin
// keeps one instance for the relay's lifetime, so registering it again on
// a reload is a no-op.
func registerPluginMetrics(rt *runtime) {
	for _, sk := range rt.sinks {
		m, ok := sk.(*metrics.Sink)
		if !ok {
			continue
		}
		for _, p := range rt.chain.Plugins {
			if c, ok := p.(prometheus.Collector); ok {
				_ = m.Registry().Register(c)
			}
		}
	}
}

// settingFor resolves a plugin setting for a request's caller.
func (a *App) settingFor(c *plugin.Caller, pluginID, key string) plugin.Value {
	var exempt settings.Exemptions
	if c != nil && c.KeyID != "" {
		exempt = a.keys.metaOf(c.KeyID).exempt
	}
	return a.settings().Get(c, exempt, pluginID, key)
}

type globalSettings struct {
	a  *App
	id string
}

func (g globalSettings) Get(key string) plugin.Value { return g.a.settings().Get(nil, nil, g.id, key) }

func (a *App) settings() *settings.Resolver {
	if r := a.resolver.Load(); r != nil {
		return r
	}
	return emptyRuntime.resolver
}

type callerSettings struct {
	a  *App
	id string
	c  plugin.Caller
}

func (s callerSettings) Get(key string) plugin.Value { return s.a.settingFor(&s.c, s.id, key) }
