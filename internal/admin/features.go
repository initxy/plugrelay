package admin

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"

	"github.com/initxy/plugrelay/internal/features"
	"github.com/initxy/plugrelay/internal/settings"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// settingView is one setting as the console shows it. Money settings
// (unit "money") carry currency units in default, value and effective.
type settingView struct {
	Module     string                 `json:"module"`
	Key        string                 `json:"key"`
	Type       plugin.SettingType     `json:"type"`
	Label      plugin.Text            `json:"label"`
	Help       plugin.Text            `json:"help"`
	Unit       plugin.Unit            `json:"unit,omitempty"`
	Enum       []string               `json:"enum,omitempty"`
	EnumLabels map[string]plugin.Text `json:"enum_labels,omitempty"`
	Default    any                    `json:"default,omitempty"`
	Scopes     []string               `json:"scopes"`
	Merge      plugin.Merge           `json:"merge"`
	Restart    bool                   `json:"restart,omitempty"`
	// Value is what the layer shown stores; nil when unset, and always for
	// a secret (SecretSet says whether one is stored).
	Value     any  `json:"value"`
	SecretSet bool `json:"secret_set,omitempty"`
	// Invalid: the stored value fails validation, so the merge reads it as
	// settings.LayerValue says (an allowlist that allows nothing, a deny
	// list of its valid entries, or ignored).
	Invalid bool `json:"invalid,omitempty"`
	// Feature is the feature the setting belongs to; "" for core modules
	// and extensions. Effective is the merged value the layer's callers
	// get. Both only on an object's settings.
	Feature   string          `json:"feature,omitempty"`
	Effective *effectiveValue `json:"effective,omitempty"`
}

func newSettingView(module string, d plugin.Setting) settingView {
	v := settingView{Module: module, Key: d.Key, Type: d.Type, Label: d.Label, Help: d.Help, Unit: d.Unit, Enum: d.Enum,
		EnumLabels: d.EnumLabels, Scopes: settings.ScopeNames(d.Scopes), Merge: d.Merge, Restart: d.Restart}
	if v.Label.EN == "" {
		v.Label.EN = d.Key
	}
	if v.Merge == "" {
		v.Merge = plugin.Override
	}
	if d.Default != nil && d.Type != plugin.TypeSecret {
		raw, _ := json.Marshal(d.Default)
		v.Default = settings.ToDisplay(d, raw)
	}
	return v
}

// layerValues is what one layer stores, by module and key.
type layerValues map[string]map[string]json.RawMessage

func (h *Handler) layer(c *call, scope plugin.Scope, scopeID string) (layerValues, error) {
	rows, err := h.d.Store.ListSettings(c.ctx, store.SettingFilter{Scope: scope, ScopeID: scopeID})
	if err != nil {
		return nil, err
	}
	out := layerValues{}
	for _, r := range rows {
		if r.Scope != scope || r.ScopeID != scopeID {
			continue
		}
		if out[r.PluginID] == nil {
			out[r.PluginID] = map[string]json.RawMessage{}
		}
		out[r.PluginID][r.Key] = r.Value
	}
	return out, nil
}

// withValue fills v.Value from what the layer stores.
func (v settingView) withValue(d plugin.Setting, stored layerValues) settingView {
	raw, ok := stored[v.Module][v.Key]
	if !ok {
		return v
	}
	if d.Type == plugin.TypeSecret {
		v.SecretSet = string(raw) != "null"
		return v
	}
	v.Value = settings.ToDisplay(d, raw)
	v.Invalid = string(raw) != "null" && settings.Validate(d, raw) != nil
	return v
}

// entries are the installed plugins by id.
func (h *Handler) entries() map[string]*PluginEntry {
	out := map[string]*PluginEntry{}
	for _, e := range h.d.Runtime.Plugins() {
		out[e.Info.ID] = &e
	}
	return out
}

func declOf(e *PluginEntry, key string) (plugin.Setting, bool) {
	if e == nil {
		return plugin.Setting{}, false
	}
	for _, d := range e.Info.Settings {
		if d.Key == key {
			return d, true
		}
	}
	return plugin.Setting{}, false
}

// featureState is the stored on/off of every feature.
func (h *Handler) featureState(c *call) (features.State, error) {
	fs, err := h.d.Store.ListFeatures(c.ctx)
	if err != nil {
		return nil, err
	}
	out := features.State{}
	for _, f := range fs {
		out[f.ID] = f.Enabled
	}
	return out, nil
}

// ---------------------------------------------------------------- features

type moduleView struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	StatusDetail string `json:"status_detail,omitempty"`
}

type featureView struct {
	ID          string        `json:"id"`
	Name        plugin.Text   `json:"name"`
	Description plugin.Text   `json:"description"`
	Enabled     bool          `json:"enabled"`
	Default     bool          `json:"default"`
	AlwaysOn    bool          `json:"always_on,omitempty"`
	Modules     []moduleView  `json:"modules"`
	Settings    []settingView `json:"settings"`
	Pages       []string      `json:"pages"`
}

func (h *Handler) featureView(f features.Feature, st features.State, global layerValues, entries map[string]*PluginEntry) featureView {
	v := featureView{ID: f.ID, Name: f.Name, Description: f.Description, Enabled: st.On(f.ID), Default: f.Default,
		AlwaysOn: f.AlwaysOn, Modules: []moduleView{}, Settings: []settingView{}, Pages: []string{}}
	for _, m := range f.Modules {
		mv := moduleView{ID: m, Status: "missing"}
		if e := entries[m]; e != nil {
			mv.Status, mv.StatusDetail = pluginStatus(e), e.StatusDetail
		}
		v.Modules = append(v.Modules, mv)
	}
	for _, r := range f.Settings {
		if d, ok := declOf(entries[r.Module], r.Key); ok {
			v.Settings = append(v.Settings, newSettingView(r.Module, d).withValue(d, global))
		}
	}
	for _, p := range f.Pages {
		v.Pages = append(v.Pages, p.ID)
	}
	return v
}

func pluginStatus(e *PluginEntry) string {
	if e.Status != "" {
		return e.Status
	}
	if e.Enabled {
		return "running"
	}
	return "stopped"
}

func (h *Handler) listFeatures(c *call) error {
	st, err := h.featureState(c)
	if err != nil {
		return err
	}
	global, err := h.layer(c, plugin.Global, "")
	if err != nil {
		return err
	}
	entries := h.entries()
	items := []featureView{}
	for _, f := range features.All() {
		items = append(items, h.featureView(f, st, global, entries))
	}
	out := map[string]any{"items": items}
	if msg := h.d.Runtime.ReloadError(); msg != "" {
		out["reload_error"] = msg
	}
	return c.json(200, out)
}

func (h *Handler) updateFeature(c *call) error {
	f, ok := features.Get(c.r.PathValue("id"))
	if !ok {
		return errNotFound(eNotFound.with("object", objFeature))
	}
	var in struct {
		Enabled  *bool       `json:"enabled"`
		Settings layerValues `json:"settings"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	st, err := h.featureState(c)
	if err != nil {
		return err
	}
	entries := h.entries()
	shown := func(module, key string) (plugin.Setting, bool) {
		if !slices.Contains(f.Settings, features.Ref{Module: module, Key: key}) {
			return plugin.Setting{}, false
		}
		return declOf(entries[module], key)
	}
	writes, err := h.prepareSettings(c, plugin.Global, "", in.Settings, shown)
	if err != nil {
		return err
	}
	var changes []change
	if in.Enabled != nil && *in.Enabled != st.On(f.ID) {
		if f.AlwaysOn {
			return errConflict(eFeatureAlwaysOn)
		}
		fs := store.FeatureState{ID: f.ID, Enabled: *in.Enabled}
		// Stored, a conflicting change would fail every reload (later
		// changes would silently not apply) and the next start.
		if err := h.d.Runtime.CheckFeature(c.ctx, fs); err != nil {
			return errConflict(eChangeCannotRun.with("reason", err.Error()))
		}
		if err := h.d.Store.PutFeature(c.ctx, fs); err != nil {
			return storeErr(err, objFeature)
		}
		changes = append(changes, change{Field: "enabled", From: !fs.Enabled, To: fs.Enabled})
		st[f.ID] = fs.Enabled
	}
	set, err := h.writeSettings(c, plugin.Global, "", writes)
	if err != nil {
		return err
	}
	if changes = append(changes, set...); len(changes) > 0 {
		action := "feature.update"
		if in.Enabled != nil && len(changes) > len(set) {
			action = map[bool]string{true: "feature.enable", false: "feature.disable"}[*in.Enabled]
		}
		c.audit(action, "feature", f.ID, f.Name.EN, changes)
	}
	global, err := h.layer(c, plugin.Global, "")
	if err != nil {
		return err
	}
	return c.json(200, h.featureView(f, st, global, h.entries()))
}

// ---------------------------------------------------------------- settings writes

// settingWrite is one validated change to a layer.
type settingWrite struct {
	module, key string
	decl        plugin.Setting
	raw         json.RawMessage // stored form; nil deletes
}

// prepareSettings validates changes to one layer: values come in display
// form (currency units for money) and null removes one; a null secret is
// left as it is, since secrets are never shown. allowed says which
// settings may be written here.
func (h *Handler) prepareSettings(c *call, scope plugin.Scope, scopeID string, in layerValues,
	allowed func(module, key string) (plugin.Setting, bool)) ([]settingWrite, error) {
	var bad invalid
	var out []settingWrite
	for _, module := range slices.Sorted(maps.Keys(in)) {
		for _, key := range slices.Sorted(maps.Keys(in[module])) {
			raw, path := in[module][key], "settings."+module+"."+key
			d, ok := allowed(module, key)
			if _, owned := settings.RecordOwned(scope, module, key); owned {
				bad.add(path, eRecordOwned.with("scope", scopeValue(scope)))
				continue
			}
			if !ok || d.Scopes&scope == 0 {
				bad.add(path, eNotASetting)
				continue
			}
			if string(raw) == "null" {
				if d.Type != plugin.TypeSecret {
					out = append(out, settingWrite{module: module, key: key, decl: d})
				}
				continue
			}
			raw, err := settings.FromDisplay(d, raw)
			if err == nil {
				err = settings.Validate(d, raw)
			}
			if err == nil && d.Type == plugin.TypeSecret {
				raw, err = settings.SealSecret(h.d.Box, module, key, raw)
			}
			if err != nil {
				bad.addErr(path, err)
				continue
			}
			out = append(out, settingWrite{module: module, key: key, decl: d, raw: raw})
		}
	}
	if err := bad.err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	// The enabled plugins vet each module's layer as it would be, secrets
	// left out.
	stored, err := h.layer(c, scope, scopeID)
	if err != nil {
		return nil, err
	}
	after := layerValues{}
	for _, w := range out {
		if after[w.module] == nil {
			after[w.module] = map[string]json.RawMessage{}
			for k, v := range stored[w.module] {
				after[w.module][k] = v
			}
		}
		if w.raw == nil {
			delete(after[w.module], w.key)
		} else {
			after[w.module][w.key] = w.raw
		}
	}
	entries := h.entries()
	for _, module := range slices.Sorted(maps.Keys(after)) {
		vals := after[module]
		for k := range vals {
			if d, ok := declOf(entries[module], k); !ok || d.Type == plugin.TypeSecret {
				delete(vals, k)
			}
		}
		if err := h.d.Runtime.CheckSettings(c.ctx, module, scope, vals); err != nil {
			return nil, errConflict(eChangeCannotRun.with("reason", err.Error()))
		}
	}
	return out, nil
}

// writeSettings stores prepared changes and returns them for the audit log.
func (h *Handler) writeSettings(c *call, scope plugin.Scope, scopeID string, writes []settingWrite) ([]change, error) {
	var changes []change
	for _, w := range writes {
		field := w.module + "." + w.key
		if w.raw == nil {
			err := h.d.Store.DeleteSetting(c.ctx, scope, scopeID, w.module, w.key)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
			if err == nil {
				changes = append(changes, change{Field: field})
			}
			continue
		}
		row := store.SettingRow{Scope: scope, ScopeID: scopeID, PluginID: w.module, Key: w.key, Value: w.raw}
		if err := h.d.Store.PutSetting(c.ctx, row); err != nil {
			return nil, storeErr(err, objSetting)
		}
		changes = append(changes, change{Field: field, To: json.RawMessage(settings.Redact(w.decl, w.raw))})
	}
	return changes, nil
}
