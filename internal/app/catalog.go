package app

import (
	"context"
	"errors"
	"slices"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/pipeline"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/job/modelsync"
	"github.com/initxy/plugrelay/plugins/provider/manifest"
)

// syncRecord collects what one sync changed.
type syncRecord struct {
	added, disabled []string
	unchanged       int
}

// catalog is the store-backed model table job.modelsync reconciles. An
// upstream's catalog models are the upstream model names its routes
// point at; the public model behind a route carries the enabled flag.
type catalog struct {
	a   *App
	rec *syncRecord
}

var _ modelsync.ModelCatalog = catalog{}

// routesAndModels lists every route and every model by name.
func (c catalog) routesAndModels(ctx context.Context) ([]store.Route, map[string]*store.Model, error) {
	routes, err := c.a.store.ListRoutes(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	ms, err := c.a.store.ListModels(ctx)
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]*store.Model{}
	for i := range ms {
		byName[ms[i].Name] = &ms[i]
	}
	return routes, byName, nil
}

func (c catalog) Models(ctx context.Context, upstreamID string) ([]modelsync.CatalogModel, error) {
	routes, models, err := c.routesAndModels(ctx)
	if err != nil {
		return nil, err
	}
	var out []modelsync.CatalogModel
	for _, r := range routes {
		if r.UpstreamID != upstreamID {
			continue
		}
		m := models[r.Model]
		if m == nil {
			continue
		}
		out = append(out, modelsync.CatalogModel{Name: r.UpstreamModel, Disabled: !m.Enabled, Manual: m.ManualDisabled})
	}
	return out, nil
}

func (c catalog) Upsert(ctx context.Context, upstreamID string, infos []ir.ModelInfo) error {
	routes, models, err := c.routesAndModels(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, info := range infos {
		i := slices.IndexFunc(routes, func(r store.Route) bool { return r.UpstreamID == upstreamID && r.UpstreamModel == info.ID })
		if i >= 0 {
			m := models[routes[i].Model]
			if m != nil && !m.Enabled && !m.ManualDisabled {
				m.Enabled = true
				errs = append(errs, c.a.store.PutModel(ctx, m))
			}
			if c.rec != nil {
				c.rec.unchanged++
			}
			continue
		}
		m := models[info.ID]
		if m == nil {
			m = &store.Model{Name: info.ID, Enabled: true, Info: info}
			if err := c.a.store.PutModel(ctx, m); err != nil {
				errs = append(errs, err)
				continue
			}
			models[info.ID] = m
			if c.rec != nil {
				c.rec.added = append(c.rec.added, info.ID)
			}
		} else if c.rec != nil {
			c.rec.unchanged++
		}
		cur, err := c.a.store.ListRoutes(ctx, info.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cur = append(cur, store.Route{Model: info.ID, UpstreamID: upstreamID, UpstreamModel: info.ID, Weight: 1})
		errs = append(errs, c.a.store.ReplaceRoutes(ctx, info.ID, cur))
	}
	return errors.Join(errs...)
}

// Disable turns off public models served only by this upstream; models
// other upstreams also serve just lose this upstream's route.
func (c catalog) Disable(ctx context.Context, upstreamID string, names []string) error {
	routes, models, err := c.routesAndModels(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range routes {
		if r.UpstreamID != upstreamID || !slices.Contains(names, r.UpstreamModel) {
			continue
		}
		m := models[r.Model]
		if m == nil || m.ManualDisabled {
			continue
		}
		var others []store.Route
		for _, o := range routes {
			if o.Model == r.Model && o.UpstreamID != upstreamID {
				others = append(others, o)
			}
		}
		if len(others) > 0 {
			errs = append(errs, c.a.store.ReplaceRoutes(ctx, r.Model, others))
		} else if m.Enabled {
			m.Enabled = false
			errs = append(errs, c.a.store.PutModel(ctx, m))
		}
		if c.rec != nil {
			c.rec.disabled = append(c.rec.disabled, r.Model)
		}
	}
	return errors.Join(errs...)
}

// targets lists upstreams to sync with a credential to list models with:
// the ids given, or every enabled upstream whose model sync is on. An
// upstream with no usable credential is left out unless it is keyless:
// listing it would call a server that needs a key with no auth.
type targets struct {
	a   *App
	ids []string
}

var _ modelsync.Targets = targets{}

func (t targets) SyncTargets(ctx context.Context) ([]modelsync.Target, error) {
	rt := t.a.runtime()
	var out []modelsync.Target
	for _, u := range rt.store.Upstreams {
		if len(t.ids) > 0 && !slices.Contains(t.ids, u.ID) {
			continue
		}
		if len(t.ids) == 0 && !u.Enabled {
			continue
		}
		up := rt.snap.Upstreams[u.ID]
		if up == nil {
			continue
		}
		if len(t.ids) == 0 && !modelSyncOn(up) {
			continue
		}
		tu := rt.snap.Table.Upstreams[u.ID]
		tg := modelsync.Target{Upstream: up.Config}
		tg.Lister, _ = up.Provider.(plugin.ModelLister)
		if tg.Credential = pickCredential(up, tu); tg.Credential == nil && !tu.Keyless() {
			continue
		}
		out = append(out, tg)
	}
	return out, nil
}

// errNoKey is why an upstream that needs a key cannot be listed.
var errNoKey = errors.New("the upstream has no usable credential: add its API key")

// lacksKey reports whether an upstream is left out of targets for want of
// a usable credential.
func lacksKey(rt *runtime, id string) bool {
	up, tu := rt.snap.Upstreams[id], rt.snap.Table.Upstreams[id]
	return up != nil && pickCredential(up, tu) == nil && !tu.Keyless()
}

// pickCredential prefers an active shared credential, then any active one.
func pickCredential(up *pipeline.Upstream, tu router.Upstream) *plugin.Credential {
	var fallback *plugin.Credential
	for _, c := range tu.Credentials {
		pc := up.Credentials[c.ID]
		if pc == nil || (c.Status != "" && c.Status != router.StatusActive) {
			continue
		}
		if c.Kind == router.KindShared {
			return pc
		}
		if fallback == nil {
			fallback = pc
		}
	}
	return fallback
}

// modelSyncOn is the upstream's model sync switch; unset, it is on when
// the provider lists models for the upstream (a manifest declaring a
// listing path or a static list, or a lister that is not manifest-driven).
func modelSyncOn(up *pipeline.Upstream) bool {
	if on, set := admin.ModelSync(up.Config.Config); set {
		return on
	}
	if _, ok := up.Provider.(plugin.ModelLister); !ok {
		return false
	}
	mp, ok := up.Provider.(interface {
		Manifest(*plugin.Upstream) (*manifest.Manifest, error)
	})
	if !ok {
		return true
	}
	m, err := mp.Manifest(up.Config)
	return err == nil && (m.Models.List != "" || len(m.Models.Static) > 0)
}
