package admin

import (
	"encoding/json"
	"slices"

	"github.com/initxy/plugrelay/internal/features"
	"github.com/initxy/plugrelay/internal/settings"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// The settings of one group, user or key: every setting of a running
// module that may be placed on that layer, except the ones that are fields
// of the record itself (settings.RecordOwned).

// settingsCaller is the identity whose merged settings a layer shows.
func (h *Handler) settingsCaller(c *call, scope plugin.Scope, scopeID string) (plugin.Caller, error) {
	switch scope {
	case plugin.Key:
		k, err := h.d.Store.GetKey(c.ctx, scopeID)
		if err != nil {
			return plugin.Caller{}, storeErr(err, objKey)
		}
		return k.Caller(), nil
	case plugin.User:
		u, err := h.d.Store.GetUser(c.ctx, scopeID)
		if err != nil {
			return plugin.Caller{}, storeErr(err, objUser)
		}
		return plugin.Caller{UserID: u.ID, GroupID: u.GroupID}, nil
	case plugin.Group:
		if _, err := h.d.Store.GetGroup(c.ctx, scopeID); err != nil {
			return plugin.Caller{}, storeErr(err, objGroup)
		}
		return plugin.Caller{GroupID: scopeID}, nil
	}
	return plugin.Caller{}, nil
}

type effectiveValue struct {
	Value any `json:"value"`
	// Source is the layer the value comes from: default, global, group,
	// user or key.
	Source    string `json:"source"`
	SecretSet bool   `json:"secret_set,omitempty"`
	// Layers lists each layer's own list of a strictest list set on more
	// than one layer, every one of which must allow an entry; DeniedBy is
	// the layer after which no entry is left in all of them.
	Layers   []effectiveLayer `json:"layers,omitempty"`
	DeniedBy string           `json:"denied_by,omitempty"`
}

type effectiveLayer struct {
	Source string   `json:"source"`
	Value  []string `json:"value"`
}

// strictLayers names the layers of a strictest list merged from more
// than one layer, in merge order, and the layer that leaves no entry
// common to all.
func strictLayers(d plugin.Setting, v plugin.Value, at func(plugin.Scope) (json.RawMessage, bool)) ([]effectiveLayer, string) {
	lists := v.ListLayers()
	if d.Type != plugin.TypeList || d.Merge != plugin.Strictest || len(lists) < 2 {
		return nil, ""
	}
	var out []effectiveLayer
	for _, s := range []plugin.Scope{plugin.Global, plugin.Group, plugin.User, plugin.Key} {
		raw, ok := at(s)
		if !ok || d.Scopes&s == 0 {
			continue
		}
		if raw = settings.LayerValue(d, raw); raw == nil {
			continue
		}
		if l := plugin.NewValue(raw).List(); len(l) > 0 {
			// An invalid layer allows nothing; shown as an empty list.
			l = slices.DeleteFunc(l, func(e string) bool { return e == plugin.DenyAll })
			out = append(out, effectiveLayer{Source: settings.ScopeName(s), Value: l})
		}
	}
	if len(out) != len(lists) {
		return nil, ""
	}
	common := out[0].Value
	if len(common) == 0 {
		return out, out[0].Source
	}
	for _, l := range out[1:] {
		common = slices.DeleteFunc(slices.Clone(common), func(e string) bool { return !slices.Contains(l.Value, e) })
		if len(common) == 0 {
			return out, l.Source
		}
	}
	return out, ""
}

// layerSettings lists what the layer may hold: settings of core modules,
// of the modules of features that are on, and of running extensions.
func (h *Handler) layerSettings(c *call, scope plugin.Scope) ([]settingView, map[string]plugin.Setting, error) {
	st, err := h.featureState(c)
	if err != nil {
		return nil, nil, err
	}
	var views []settingView
	decls := map[string]plugin.Setting{}
	for _, e := range h.d.Runtime.Plugins() {
		id := e.Info.ID
		f, inFeature := features.Of(id)
		switch {
		case features.IsCore(id):
		case inFeature:
			if !st.On(f.ID) {
				continue
			}
		case !isExtension(&e) || !e.Enabled:
			continue
		}
		for _, d := range e.Info.Settings {
			if d.Scopes&scope == 0 {
				continue
			}
			if _, owned := settings.RecordOwned(scope, id, d.Key); owned {
				continue
			}
			v := newSettingView(id, d)
			v.Feature = featureOfSetting(id, d.Key)
			views = append(views, v)
			decls[id+"/"+d.Key] = d
		}
	}
	return views, decls, nil
}

// featureOfSetting is the feature a setting is shown under: its module's,
// else the one whose form shows it.
func featureOfSetting(module, key string) string {
	if f, ok := features.Of(module); ok {
		return f.ID
	}
	for _, f := range features.All() {
		if slices.Contains(f.Settings, features.Ref{Module: module, Key: key}) {
			return f.ID
		}
	}
	return ""
}

func (h *Handler) objectSettings(c *call, scope plugin.Scope, scopeID string) (map[string]any, error) {
	caller, err := h.settingsCaller(c, scope, scopeID)
	if err != nil {
		return nil, err
	}
	views, decls, err := h.layerSettings(c, scope)
	if err != nil {
		return nil, err
	}
	rows, err := h.d.Store.ListSettings(c.ctx, store.SettingFilter{})
	if err != nil {
		return nil, err
	}
	at := func(s plugin.Scope, id, module, key string) (json.RawMessage, bool) {
		for _, r := range rows {
			if r.Scope == s && r.ScopeID == id && r.PluginID == module && r.Key == key {
				return r.Value, true
			}
		}
		return nil, false
	}
	ids := map[plugin.Scope]string{plugin.Group: caller.GroupID, plugin.User: caller.UserID, plugin.Key: caller.KeyID}
	stored := layerValues{}
	for _, r := range rows {
		if r.Scope == scope && r.ScopeID == scopeID {
			if stored[r.PluginID] == nil {
				stored[r.PluginID] = map[string]json.RawMessage{}
			}
			stored[r.PluginID][r.Key] = r.Value
		}
	}
	items := []settingView{}
	for _, v := range views {
		d := decls[v.Module+"/"+v.Key]
		v = v.withValue(d, stored)
		ev := effectiveValue{Source: "default", Value: v.Default}
		for _, s := range []plugin.Scope{plugin.Key, plugin.User, plugin.Group, plugin.Global} {
			if s != plugin.Global && ids[s] == "" {
				continue
			}
			if _, ok := at(s, ids[s], v.Module, v.Key); ok {
				ev.Source = settings.ScopeName(s)
				break
			}
		}
		val := h.d.Runtime.SettingsFor(v.Module, caller).Get(v.Key)
		if val.IsSet() {
			ev.Value = settings.ToDisplay(d, val.Raw())
		}
		ev.Layers, ev.DeniedBy = strictLayers(d, val, func(s plugin.Scope) (json.RawMessage, bool) {
			if s != plugin.Global && ids[s] == "" {
				return nil, false
			}
			return at(s, ids[s], v.Module, v.Key)
		})
		if d.Type == plugin.TypeList && slices.Contains(val.List(), plugin.DenyAll) {
			ev.Value = []string{}
		}
		if d.Type == plugin.TypeSecret {
			ev.SecretSet, ev.Value = val.IsSet() && ev.Source != "default", nil
		}
		v.Effective = &ev
		items = append(items, v)
	}
	return map[string]any{"items": items}, nil
}

func (h *Handler) getObjectSettings(scope plugin.Scope) func(c *call) error {
	return func(c *call) error {
		out, err := h.objectSettings(c, scope, c.r.PathValue("id"))
		if err != nil {
			return err
		}
		return c.json(200, out)
	}
}

func (h *Handler) putObjectSettings(scope plugin.Scope) func(c *call) error {
	return func(c *call) error {
		id := c.r.PathValue("id")
		if _, err := h.settingsCaller(c, scope, id); err != nil {
			return err
		}
		var in struct {
			Settings layerValues `json:"settings"`
		}
		if err := c.decode(&in); err != nil {
			return err
		}
		if in.Settings == nil {
			return fieldInvalid("settings", eRequired)
		}
		_, decls, err := h.layerSettings(c, scope)
		if err != nil {
			return err
		}
		writes, err := h.prepareSettings(c, scope, id, in.Settings, func(module, key string) (plugin.Setting, bool) {
			d, ok := decls[module+"/"+key]
			return d, ok
		})
		if err != nil {
			return err
		}
		changes, err := h.writeSettings(c, scope, id, writes)
		if err != nil {
			return err
		}
		if len(changes) > 0 {
			c.audit("settings.layer", settings.ScopeName(scope), id, id, changes)
		}
		out, err := h.objectSettings(c, scope, id)
		if err != nil {
			return err
		}
		return c.json(200, out)
	}
}
