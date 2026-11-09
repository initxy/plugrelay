package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/extplugin"
	"github.com/initxy/plugrelay/internal/features"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/internal/version"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/builtin"
	"github.com/initxy/plugrelay/plugins/meter/quota"
	"github.com/initxy/plugrelay/plugins/sink/metrics"
)

// pluginSet owns plugin instances. Every known plugin has one instance,
// created from its factory (or the external plugin manager) when first
// listed; Init runs once, when it is first enabled or used as a provider.
type pluginSet struct {
	a  *App
	mu sync.Mutex
	// inst holds one instance per plugin id; inited marks those whose Init
	// ran, failed holds Init errors.
	inst   map[string]plugin.Plugin
	inited map[string]bool
	failed map[string]string
	// extOf maps an external adapter id to its manifest id.
	extOf map[string]string
	jobs  map[string]bool
}

func newPluginSet(a *App) *pluginSet {
	return &pluginSet{a: a, inst: map[string]plugin.Plugin{}, inited: map[string]bool{}, failed: map[string]string{},
		extOf: map[string]string{}, jobs: map[string]bool{}}
}

// known returns every installed plugin instance, sorted by id.
func (s *pluginSet) known() []plugin.Plugin {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range plugin.Registered() {
		if _, ok := s.inst[id]; !ok {
			if f, ok := plugin.Lookup(id); ok {
				s.inst[id] = f()
			}
		}
	}
	if m := s.a.ext; m != nil {
		for _, st := range m.List() {
			for _, p := range m.Plugins(st.ID) {
				id := p.Info().ID
				if _, ok := s.inst[id]; !ok {
					s.inst[id] = p
					s.extOf[id] = st.ID
					s.inited[id] = true // the manager runs the handshake
				}
			}
		}
	}
	out := make([]plugin.Plugin, 0, len(s.inst))
	for _, p := range s.inst {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Info().ID < out[j].Info().ID })
	return out
}

// ready returns an initialized instance of id.
func (s *pluginSet) ready(ctx context.Context, id string) (plugin.Plugin, error) {
	s.mu.Lock()
	p, ok := s.inst[id]
	if !ok {
		f, found := plugin.Lookup(id)
		if !found {
			s.mu.Unlock()
			return nil, fmt.Errorf("plugin %s is not installed", id)
		}
		p = f()
		s.inst[id] = p
	}
	if s.inited[id] {
		s.mu.Unlock()
		return p, nil
	}
	if msg, bad := s.failed[id]; bad {
		s.mu.Unlock()
		return nil, fmt.Errorf("plugin %s: %s", id, msg)
	}
	s.mu.Unlock()
	if in, ok := p.(plugin.Initializer); ok {
		ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := in.Init(ictx, s.host(id))
		cancel()
		if err != nil {
			s.mu.Lock()
			s.failed[id] = err.Error()
			s.mu.Unlock()
			s.a.log.Error("plugin init failed", "plugin", id, "err", err)
			return nil, err
		}
	}
	s.mu.Lock()
	s.inited[id] = true
	s.mu.Unlock()
	return p, nil
}

// provider returns an initialized provider plugin.
func (s *pluginSet) provider(ctx context.Context, id string) (plugin.Provider, error) {
	p, err := s.ready(ctx, id)
	if err != nil {
		return nil, err
	}
	prov, ok := p.(plugin.Provider)
	if !ok {
		return nil, fmt.Errorf("plugin %s is not a provider", id)
	}
	return prov, nil
}

// metrics returns the metrics sink when it is enabled.
func (s *pluginSet) metrics() *metrics.Sink {
	rt := s.a.cur.Load()
	if rt == nil {
		return nil
	}
	for _, sk := range rt.sinks {
		if m, ok := sk.(*metrics.Sink); ok {
			return m
		}
	}
	return nil
}

func (s *pluginSet) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, p := range s.inst {
		if c, ok := p.(plugin.Closer); ok && s.inited[id] && s.extOf[id] == "" {
			if err := c.Close(); err != nil {
				s.a.log.Warn("close plugin", "plugin", id, "err", err)
			}
		}
	}
}

// pluginState is a plugin's effective switch and order.
type pluginState struct {
	enabled  bool
	order    int
	approved []string
}

// states resolves which plugins run: core modules always; a feature's
// modules while the feature is on (features.State over stored); other
// plugins (extensions) as their stored config says, off until configured.
// Built-in plugins run in builtin.Order, then by id; extensions in their
// stored order. It fails when a FailClosed plugin cannot get a plugin it
// requires.
func (a *App) states(known []plugin.Plugin, stored []store.PluginConfig, feats []store.FeatureState, log *slog.Logger) (map[string]pluginState, error) {
	on := features.State{}
	for _, f := range feats {
		on[f.ID] = f.Enabled
	}
	out := map[string]pluginState{}
	for _, p := range known {
		id := p.Info().ID
		st := pluginState{order: 1000}
		if f, ok := features.Of(id); ok {
			st.enabled = on.On(f.ID)
		}
		st.enabled = st.enabled || features.IsCore(id)
		out[id] = st
	}
	for i, id := range builtin.Order() {
		if st, ok := out[id]; ok {
			st.order = (i + 1) * 10
			out[id] = st
		}
	}
	for _, pc := range stored {
		if features.Builtin(pc.PluginID) {
			continue
		}
		st := out[pc.PluginID]
		st.enabled, st.order = pc.Enabled, pc.Order
		st.approved = toStrings(pc.Config["approved_permissions"])
		out[pc.PluginID] = st
	}
	if err := resolveRequires(known, out, log); err != nil {
		return nil, err
	}
	if a.testMode {
		var on []string
		for id, st := range out {
			if st.enabled {
				on = append(on, id)
			}
		}
		// Drop every plugin that reports off the box, external ones included.
		var reporting []string
		for _, p := range known {
			if p.Info().ReportsOffBox {
				reporting = append(reporting, p.Info().ID)
			}
		}
		keep := builtin.ForTestMode(on, reporting...)
		for id, st := range out {
			if st.enabled && !slices.Contains(keep, id) {
				st.enabled = false
				out[id] = st
			}
		}
	}
	return out, nil
}

// resolveRequires applies plugin.Info.Requires: a required plugin is turned
// on with its dependent. When the requirement is not installed, a
// FailClosed dependent (a meter that bills from it) is an
// error, since running it without its requirement or silently dropping it
// would both break billing; any other dependent is turned off and the
// conflict logged. It repeats until stable so chains of requirements
// resolve.
func resolveRequires(known []plugin.Plugin, out map[string]pluginState, log *slog.Logger) error {
	infos := map[string]plugin.Info{}
	for _, p := range known {
		infos[p.Info().ID] = p.Info()
	}
	// blocked are plugins turned off for a missing requirement; they count
	// as unavailable, so a dependent cannot turn them back on.
	blocked := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for id, st := range out {
			if !st.enabled {
				continue
			}
			for _, req := range infos[id].Requires {
				rs, ok := out[req]
				switch {
				case ok && rs.enabled:
				case !ok || blocked[req]:
					if infos[id].EffectiveFailPolicy() == plugin.FailClosed {
						why := "not installed"
						if ok {
							why = "missing a plugin it requires itself"
						}
						return fmt.Errorf("plugin %s requires %s, which is %s; %s fails closed and cannot run without it: enable %s or turn %s off",
							id, req, why, id, req, id)
					}
					log.Error("plugin disabled: a plugin it requires is unavailable",
						"plugin", id, "requires", req)
					st.enabled = false
					out[id] = st
					blocked[id] = true
					changed = true
				default:
					log.Info("plugin enabled because another plugin requires it", "plugin", req, "required_by", id)
					rs.enabled = true
					out[req] = rs
					changed = true
				}
				if !st.enabled {
					break
				}
			}
		}
	}
	return nil
}

// checkPluginConfig resolves the plugin states as they would be with pc
// stored, without logging or changing anything, and runs the settings
// check of each plugin it would turn on (checkTurnedOn).
func (a *App) checkPluginConfig(ctx context.Context, pc *store.PluginConfig) error {
	stored, err := a.store.ListPluginConfigs(ctx)
	if err != nil {
		return err
	}
	feats, err := a.store.ListFeatures(ctx)
	if err != nil {
		return err
	}
	stored = slices.DeleteFunc(stored, func(c store.PluginConfig) bool { return c.PluginID == pc.PluginID })
	return a.checkTurnedOn(ctx, append(stored, *pc), feats, nil)
}

// checkFeature is checkPluginConfig for storing f.
func (a *App) checkFeature(ctx context.Context, f store.FeatureState) error {
	stored, err := a.store.ListPluginConfigs(ctx)
	if err != nil {
		return err
	}
	feats, err := a.store.ListFeatures(ctx)
	if err != nil {
		return err
	}
	feats = slices.DeleteFunc(feats, func(c store.FeatureState) bool { return c.ID == f.ID })
	return a.checkTurnedOn(ctx, stored, append(feats, f), nil)
}

// instance is the plugin instance of id, when known.
func (s *pluginSet) instance(id string) (plugin.Plugin, bool) {
	s.known()
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.inst[id]
	return p, ok
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// entries lists plugins for the admin API.
func (a *App) entries() []admin.PluginEntry {
	rt := a.runtime()
	known := a.plugins.known()
	var out []admin.PluginEntry
	var statuses map[string]extplugin.Status
	if a.ext != nil {
		statuses = map[string]extplugin.Status{}
		for _, st := range a.ext.List() {
			statuses[st.ID] = st
		}
	}
	for _, p := range known {
		info := p.Info()
		st := rt.states[info.ID]
		e := admin.PluginEntry{Info: info, Source: admin.SourceBuiltin, Enabled: st.enabled, Order: st.order,
			Approved: st.approved}
		if info.Kind == plugin.KindProvider {
			e.Enabled = true
		}
		a.plugins.mu.Lock()
		manID, ext := a.plugins.extOf[info.ID]
		failed, bad := a.plugins.failed[info.ID]
		a.plugins.mu.Unlock()
		blocked, isBlocked := rt.blocked[info.ID]
		switch {
		case bad:
			e.Status, e.StatusDetail = "crashed", failed
		case e.Enabled && isBlocked:
			e.Status, e.StatusDetail = "crashed", blocked
		case e.Enabled:
			e.Status = "running"
		default:
			e.Status = "stopped"
		}
		if ad, ok := p.(plugin.Admin); ok {
			e.Routes = ad.Routes()
		}
		if ext {
			e.Source = admin.SourceExternal
			es := statuses[manID]
			if primaryExt(es.Manifest) == info.ID {
				e.Manifest = es.Manifest
			}
			switch {
			case !e.Enabled && len(e.Approved) == 0:
				e.Status = "needs_approval"
			case e.Enabled && es.State != extplugin.StateRunning:
				e.Status, e.StatusDetail = "crashed", es.LastError
			}
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Info.ID < out[j].Info.ID
	})
	return out
}

// primaryExt is the core-facing id that carries an external plugin's
// manifest and admin routes: its admin kind when it has one, else its first kind.
func primaryExt(m *extplugin.Manifest) string {
	if m == nil || len(m.Kinds) == 0 {
		return ""
	}
	if slices.Contains(m.Kinds, plugin.KindAdmin) {
		return m.PluginID(plugin.KindAdmin)
	}
	return m.PluginID(m.Kinds[0])
}

// syncExternal starts enabled external plugins and stops disabled ones, in
// the background; closeAll cancels and waits for those runs.
func (a *App) syncExternal(states map[string]pluginState) {
	if a.ext == nil {
		return
	}
	a.extMu.Lock()
	defer a.extMu.Unlock()
	if a.extClosed {
		return
	}
	want := map[string]bool{}
	a.plugins.mu.Lock()
	for id, man := range a.plugins.extOf {
		if states[id].enabled {
			want[man] = true
		} else if _, ok := want[man]; !ok {
			want[man] = false
		}
	}
	a.plugins.mu.Unlock()
	for man, on := range want {
		st, _ := a.ext.Status(man)
		running := st.State == extplugin.StateRunning || st.State == extplugin.StateStarting || st.State == extplugin.StateBackoff
		switch {
		case on && !running:
			a.extWG.Go(func() {
				sctx, cancel := context.WithTimeout(a.extCtx, time.Minute)
				defer cancel()
				if err := a.ext.Start(sctx, man); err != nil && a.extCtx.Err() == nil {
					a.log.Warn("start external plugin", "plugin", man, "err", err)
				}
			})
		case !on && running:
			a.extWG.Go(func() {
				sctx, cancel := context.WithTimeout(a.extCtx, 30*time.Second)
				defer cancel()
				_ = a.ext.Stop(sctx, man)
			})
		}
	}
}

// ---------------------------------------------------------------- jobs

// startJobs runs a scheduler loop for every job that is or becomes
// enabled; it is called again after each rebuild.
func (s *pluginSet) startJobs(ctx context.Context) {
	rt := s.a.cur.Load()
	if rt == nil {
		return
	}
	for _, j := range rt.jobs {
		id := j.Info().ID
		s.mu.Lock()
		running := s.jobs[id]
		s.jobs[id] = true
		s.mu.Unlock()
		if running {
			continue
		}
		s.a.wg.Add(1)
		go func() {
			defer s.a.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.jobs, id)
				s.mu.Unlock()
			}()
			s.runJob(ctx, id)
		}()
	}
}

// runJob ticks one job with jitter while it stays enabled. A cluster-wide
// lock makes each run happen on one node only.
func (s *pluginSet) runJob(ctx context.Context, id string) {
	log := s.a.log.With("job", id)
	first := true
	for {
		j := s.a.runtime().job(id)
		if j == nil {
			return
		}
		every := j.Every()
		if every <= 0 {
			every = time.Hour
		}
		wait := every + time.Duration(rand.Int64N(int64(every/10)+1))
		if first {
			wait = 30*time.Second + time.Duration(rand.Int64N(int64(30*time.Second)))
			first = false
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if j = s.a.runtime().job(id); j == nil {
			return
		}
		unlock, ok, err := s.a.state.Lock(ctx, "job:"+id, every)
		if err != nil || !ok {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, every)
		start := time.Now()
		err = safeRun(rctx, j)
		cancel()
		unlock()
		if err != nil && ctx.Err() == nil {
			log.Warn("job failed", "err", err, "duration", time.Since(start))
		} else {
			log.Debug("job done", "duration", time.Since(start))
		}
	}
}

func safeRun(ctx context.Context, j plugin.Job) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("job panicked: %v", v)
		}
	}()
	return j.Run(ctx)
}

// ---------------------------------------------------------------- host

// host is what one plugin sees of the core. Besides plugin.Host it offers
// the optional interfaces built-in plugins look for (key lookup, shared
// state, request log, model catalog, per-caller settings).
type host struct {
	a   *App
	id  string
	log *slog.Logger
}

func (s *pluginSet) host(id string) plugin.Host {
	return &host{a: s.a, id: id, log: s.a.log.With("plugin", id)}
}

func (h *host) Logger() *slog.Logger { return h.log }

func (h *host) HTTPClient() *http.Client {
	c, err := h.a.egress.HTTPClient("", false)
	if err != nil {
		return &http.Client{Transport: errTransport{err}}
	}
	return c
}

func (h *host) ClientFor(u *plugin.Upstream, c *plugin.Credential) (*http.Client, error) {
	return h.a.clientFor(u, c)
}

// clientFor is the egress of a plugin's call on behalf of an upstream
// credential: the credential's proxy, else the upstream's, under the
// upstream's timeouts. Proxies come from the current snapshot, so a plugin
// cannot opt out of one by passing a credential without it.
func (a *App) clientFor(u *plugin.Upstream, c *plugin.Credential) (*http.Client, error) {
	if u == nil {
		return nil, errors.New("egress: no upstream given")
	}
	up := a.runtime().snap.Upstreams[u.ID]
	if up == nil {
		return nil, fmt.Errorf("egress: upstream %q is not configured", u.ID)
	}
	proxy := up.Proxy
	if c != nil {
		if c.Proxy != "" {
			proxy = c.Proxy
		}
		if sc := up.Credentials[c.ID]; sc != nil && sc.Proxy != "" {
			proxy = sc.Proxy
		}
	}
	return a.egress.Timed(proxy, up.AllowPrivate, a.defaultTimeouts().Override(up.Timeouts))
}

type errTransport struct{ err error }

func (t errTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

func (h *host) KV() plugin.KV                   { return h.a.store.KV(h.id) }
func (h *host) Settings() plugin.Settings       { return globalSettings{a: h.a, id: h.id} }
func (h *host) Usage() plugin.UsageReader       { return h.a.store }
func (h *host) Version() string                 { return version.Version }
func (h *host) KeyLookup() apikeyLookup         { return h.a.keys }
func (h *host) ClientIP(r *http.Request) string { return h.a.clientIP(r) }
func (h *host) State() plugin.SharedState       { return newScopedState(h.a.state, h.id) }
func (h *host) RecordWriter() recordWriterAPI   { return recordWriter{h.a.store} }
func (h *host) RecordPurger() purgerAPI         { return recordWriter{h.a.store} }
func (h *host) ModelCatalog() catalogAPI        { return catalog{h.a, nil} }
func (h *host) ModelSyncTargets() targetsAPI    { return targets{h.a, nil} }

var _ quota.TimezoneHost = (*host)(nil)

// Timezone is the site time zone, for meter.quota's daily and monthly windows.
func (h *host) Timezone(ctx context.Context) *time.Location {
	return admin.Timezone(ctx, h.a.store)
}

func (h *host) SettingsFor(pluginID string, c plugin.Caller) plugin.Settings {
	return callerSettings{a: h.a, id: pluginID, c: c}
}
