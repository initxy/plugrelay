package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/settings"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// ApplyDocument opens the relay described by cfg without serving it and
// applies a store document through App.Apply, so the enabled plugins vet
// its settings as they do a console save. It is `plugrelay apply`.
// External plugins are not loaded, so their settings checks do not run.
func ApplyDocument(ctx context.Context, cfg *config.Config, data []byte, dryRun bool) (*store.ApplyResult, error) {
	a, err := newApp(ctx, cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})), false)
	if err != nil {
		return nil, err
	}
	defer a.closeAll()
	return a.Apply(ctx, data, dryRun)
}

// Apply is store.Store.Apply after the enabled plugins' plugin.SettingsCheck
// accepts each plugin scope whose settings the document changes, with the
// values that scope would hold afterwards (secrets left out, as on the
// console). A plugin the document turns on also checks every scope.
func (a *App) Apply(ctx context.Context, data []byte, dryRun bool) (*store.ApplyResult, error) {
	d, err := store.ParseDocument(data)
	if err != nil {
		return nil, err
	}
	if err := a.checkDocSettings(ctx, d); err != nil {
		return nil, err
	}
	return a.store.Apply(ctx, data, dryRun)
}

// settingScope is one plugin's settings at one scope.
type settingScope struct {
	scope           plugin.Scope
	scopeID, plugin string
}

func (a *App) checkDocSettings(ctx context.Context, d *store.Document) error {
	var order []settingScope
	proposed := map[settingScope]map[string]json.RawMessage{}
	decls := a.settingDecls()
	for _, ds := range d.Settings {
		scope, err := settings.ParseScope(ds.Scope)
		if err != nil {
			return fmt.Errorf("apply setting %s/%s: %w", ds.Plugin, ds.Key, err)
		}
		// Values are checked as the console checks them. A plugin not
		// loaded here (an external one) cannot be checked.
		if keys, known := decls[ds.Plugin]; known {
			decl, ok := keys[ds.Key]
			switch {
			case !ok:
				return fmt.Errorf("apply setting %s/%s: %s declares no such setting", ds.Plugin, ds.Key, ds.Plugin)
			case decl.Scopes&scope == 0:
				return fmt.Errorf("apply setting %s/%s: not settable at scope %s", ds.Plugin, ds.Key, ds.Scope)
			}
			if err := settings.Validate(decl, ds.Value); err != nil {
				return fmt.Errorf("apply setting %s/%s at %s %s: %w", ds.Plugin, ds.Key, ds.Scope, ds.ScopeID, err)
			}
		}
		t := settingScope{scope, ds.ScopeID, ds.Plugin}
		if proposed[t] == nil {
			proposed[t] = map[string]json.RawMessage{}
			order = append(order, t)
		}
		proposed[t][ds.Key] = ds.Value
	}
	if len(d.Plugins) > 0 || len(d.Features) > 0 {
		stored, err := a.store.ListPluginConfigs(ctx)
		if err != nil {
			return err
		}
		for _, dp := range d.Plugins {
			stored = slices.DeleteFunc(stored, func(c store.PluginConfig) bool { return c.PluginID == dp.ID })
			enabled := dp.Enabled == nil || *dp.Enabled
			stored = append(stored, store.PluginConfig{PluginID: dp.ID, Enabled: enabled, Order: dp.Order, Config: dp.Config})
		}
		feats, err := a.store.ListFeatures(ctx)
		if err != nil {
			return err
		}
		for _, df := range d.Features {
			feats = slices.DeleteFunc(feats, func(c store.FeatureState) bool { return c.ID == df.ID })
			feats = append(feats, df)
		}
		if err := a.checkTurnedOn(ctx, stored, feats, proposed); err != nil {
			return fmt.Errorf("apply: %w: %w", store.ErrConflict, err)
		}
	}
	if len(order) == 0 {
		return nil
	}
	secret := a.secretSettings()
	rt := runtimeAPI{a}
	for _, t := range order {
		after, err := a.scopeSettings(ctx, t)
		if err != nil {
			return err
		}
		changed := false
		for k, v := range proposed[t] {
			if cur, ok := after[k]; !ok || !sameJSON(cur, v) {
				changed = true
			}
			after[k] = v
		}
		if !changed {
			continue
		}
		for k := range after {
			if secret[t.plugin+"/"+k] {
				delete(after, k)
			}
		}
		if err := rt.CheckSettings(ctx, t.plugin, t.scope, after); err != nil {
			return fmt.Errorf("apply settings of %s at %s: %w: %w", t.plugin, settings.ScopeName(t.scope), store.ErrConflict, err)
		}
	}
	return nil
}

// scopeSettings is what is stored for t.
func (a *App) scopeSettings(ctx context.Context, t settingScope) (map[string]json.RawMessage, error) {
	rows, err := a.store.ListSettings(ctx, store.SettingFilter{Scope: t.scope, ScopeID: t.scopeID, PluginID: t.plugin})
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for _, r := range rows {
		if r.ScopeID == t.scopeID && r.PluginID == t.plugin {
			out[r.Key] = r.Value
		}
	}
	return out, nil
}

// settingDecls are the declared settings of every plugin loaded here, by
// plugin id and key.
func (a *App) settingDecls() map[string]map[string]plugin.Setting {
	out := map[string]map[string]plugin.Setting{}
	for _, p := range a.plugins.known() {
		info := p.Info()
		keys := map[string]plugin.Setting{}
		for _, s := range info.Settings {
			keys[s.Key] = s
		}
		out[info.ID] = keys
	}
	return out
}

// secretSettings are the secret settings, as plugin + "/" + key.
func (a *App) secretSettings() map[string]bool {
	out := map[string]bool{}
	for _, p := range a.plugins.known() {
		for _, s := range p.Info().Settings {
			if s.Type == plugin.TypeSecret {
				out[p.Info().ID+"/"+s.Key] = true
			}
		}
	}
	return out
}

// checkTurnedOn runs the plugin.SettingsCheck of each plugin that the
// plugin configs and feature states stored would turn on (the running relay has it off) over
// every stored settings scope, with proposed values laid over them:
// settings stored while it was off must pass it before it runs.
func (a *App) checkTurnedOn(ctx context.Context, stored []store.PluginConfig, feats []store.FeatureState,
	proposed map[settingScope]map[string]json.RawMessage) error {
	states, err := a.states(a.plugins.known(), stored, feats, slog.New(slog.DiscardHandler))
	if err != nil {
		return err
	}
	cur := a.runtime().states
	var checks []plugin.SettingsCheck
	var ids []string
	for id, st := range states {
		if st.enabled && !cur[id].enabled {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		if p, ok := a.plugins.instance(id); !ok {
			continue
		} else if _, ok := p.(plugin.SettingsCheck); !ok {
			continue
		}
		// One whose Init fails does not run, so it has nothing to vet. Init
		// must not inherit the request's cancellation: a failed Init is
		// remembered, and a closed console tab would mark the plugin crashed.
		if p, err := a.plugins.ready(context.WithoutCancel(ctx), id); err == nil {
			checks = append(checks, p.(plugin.SettingsCheck))
		}
	}
	if len(checks) == 0 {
		return nil
	}
	rows, err := a.store.ListSettings(ctx, store.SettingFilter{})
	if err != nil {
		return err
	}
	all := map[settingScope]map[string]json.RawMessage{}
	var order []settingScope
	add := func(t settingScope, k string, v json.RawMessage) {
		if all[t] == nil {
			all[t] = map[string]json.RawMessage{}
			order = append(order, t)
		}
		all[t][k] = v
	}
	for _, r := range rows {
		add(settingScope{r.Scope, r.ScopeID, r.PluginID}, r.Key, r.Value)
	}
	for t, kv := range proposed {
		for k, v := range kv {
			add(t, k, v)
		}
	}
	secret := a.secretSettings()
	for _, t := range order {
		for k := range all[t] {
			if secret[t.plugin+"/"+k] {
				delete(all[t], k)
			}
		}
		for _, c := range checks {
			if err := c.CheckSettings(ctx, t.plugin, t.scope, all[t]); err != nil {
				return fmt.Errorf("%s refuses the settings of %s at %s: %w", c.Info().ID, t.plugin, settings.ScopeName(t.scope), err)
			}
		}
	}
	return nil
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}
