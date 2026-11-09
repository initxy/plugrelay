// Package modelsync is the job.modelsync plugin: it pulls each upstream's
// model list and reconciles the model catalog with it.
//
// Reconciliation follows hard rule 21:
//
//   - If listing fails or returns no models, nothing changes for that
//     upstream. An empty list is far more often an outage than a real
//     catalog wipe.
//   - Models that disappeared upstream are disabled, never deleted, so
//     routes, prices and history that point at them survive.
//   - Models an admin has taken over by hand (Manual) are never touched:
//     they are neither re-enabled, updated nor disabled.
//
// Upserting a listed model that the job disabled earlier re-enables it.
package modelsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// ID is the plugin id.
const ID = "job.modelsync"

func init() { plugin.Register(ID, func() plugin.Plugin { return New(nil, nil) }) }

// CatalogModel is one model the catalog holds for an upstream.
type CatalogModel struct {
	Name     string
	Disabled bool
	// Manual marks a model an admin added, edited or disabled by hand;
	// the job leaves it alone.
	Manual bool
}

// ModelCatalog is the core's per-upstream model table.
type ModelCatalog interface {
	Models(ctx context.Context, upstreamID string) ([]CatalogModel, error)
	// Upsert creates or updates models and enables them.
	Upsert(ctx context.Context, upstreamID string, models []ir.ModelInfo) error
	// Disable marks models disabled without deleting them.
	Disable(ctx context.Context, upstreamID string, names []string) error
}

// Target is one upstream to sync, with the credential to list models with
// and its provider's lister (nil when the provider cannot list).
type Target struct {
	Upstream   *plugin.Upstream
	Credential *plugin.Credential
	Lister     plugin.ModelLister
}

// Targets enumerates the upstreams to sync.
type Targets interface {
	SyncTargets(ctx context.Context) ([]Target, error)
}

// CatalogHost is implemented by a Host that hands the job its catalog and
// upstream list.
type CatalogHost interface {
	ModelCatalog() ModelCatalog
	ModelSyncTargets() Targets
}

// Job implements plugin.Job.
type Job struct {
	cat     ModelCatalog
	targets Targets
	log     *slog.Logger
	every   time.Duration
}

// DefaultInterval applies when the interval setting is unset.
const DefaultInterval = 6 * time.Hour

// New returns the job. Nil arguments are taken from the Host in Init.
func New(cat ModelCatalog, targets Targets) *Job {
	return &Job{cat: cat, targets: targets, log: slog.New(slog.DiscardHandler), every: DefaultInterval}
}

// Info implements plugin.Plugin.
func (j *Job) Info() plugin.Info {
	return plugin.Info{
		ID:          ID,
		Kind:        plugin.KindJob,
		Version:     "1.0.0",
		Description: "Pulls upstream model lists; failures and empty lists change nothing, removed models are disabled, manual edits are kept.",
		FailPolicy:  plugin.FailOpen,
		Settings: []plugin.Setting{
			{Key: "interval", Type: plugin.TypeDuration, Default: DefaultInterval.String(), Scopes: plugin.Global,
				Merge: plugin.Override, Restart: true, Label: plugin.T("Sync model lists every", "多久同步一次模型列表"),
				Help: plugin.T("For upstreams whose model sync is on. Takes effect after a restart.", "对开了模型同步的上游生效；重启后生效。")},
		},
	}
}

// Init implements plugin.Initializer.
func (j *Job) Init(_ context.Context, h plugin.Host) error {
	if ch, ok := h.(CatalogHost); ok {
		if j.cat == nil {
			j.cat = ch.ModelCatalog()
		}
		if j.targets == nil {
			j.targets = ch.ModelSyncTargets()
		}
	}
	if j.cat == nil || j.targets == nil {
		return errors.New("modelsync: no catalog: construct with New(catalog, targets) or provide a Host implementing CatalogHost")
	}
	j.log = h.Logger()
	if d := h.Settings().Get("interval").Duration(); d > 0 {
		j.every = d
	}
	return nil
}

// Every implements plugin.Job.
func (j *Job) Every() time.Duration { return j.every }

// Run implements plugin.Job. One upstream failing does not stop the others;
// the returned error joins every failure.
func (j *Job) Run(ctx context.Context) error {
	ts, err := j.targets.SyncTargets(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range ts {
		if t.Lister == nil || t.Upstream == nil {
			continue
		}
		if err := j.Sync(ctx, t); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Upstream.ID, err))
		}
	}
	return errors.Join(errs...)
}

// Sync reconciles one upstream.
func (j *Job) Sync(ctx context.Context, t Target) error {
	id := t.Upstream.ID
	listed, err := t.Lister.ListModels(ctx, t.Upstream, t.Credential)
	if err != nil {
		return fmt.Errorf("list models (catalog unchanged): %w", err)
	}
	if len(listed) == 0 {
		j.log.Warn("upstream listed no models; catalog unchanged", "upstream", id)
		return nil
	}
	have, err := j.cat.Models(ctx, id)
	if err != nil {
		return fmt.Errorf("read catalog: %w", err)
	}
	upsert, disable := Plan(have, listed)
	if len(upsert) > 0 {
		if err := j.cat.Upsert(ctx, id, upsert); err != nil {
			return fmt.Errorf("upsert: %w", err)
		}
	}
	if len(disable) > 0 {
		if err := j.cat.Disable(ctx, id, disable); err != nil {
			return fmt.Errorf("disable: %w", err)
		}
	}
	j.log.Info("models synced", "upstream", id, "listed", len(listed), "upserted", len(upsert), "disabled", len(disable))
	return nil
}

// Plan computes the catalog changes for a non-empty upstream list: listed
// models to upsert (minus manual ones) and enabled, non-manual catalog
// models missing upstream to disable. Duplicate listed ids keep the first.
func Plan(have []CatalogModel, listed []ir.ModelInfo) (upsert []ir.ModelInfo, disable []string) {
	manual := map[string]bool{}
	for _, m := range have {
		if m.Manual {
			manual[m.Name] = true
		}
	}
	seen := map[string]bool{}
	for _, m := range listed {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if !manual[m.ID] {
			upsert = append(upsert, m)
		}
	}
	for _, m := range have {
		if !m.Manual && !m.Disabled && !seen[m.Name] {
			disable = append(disable, m.Name)
		}
	}
	return upsert, disable
}
