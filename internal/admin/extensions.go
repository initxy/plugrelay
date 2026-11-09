package admin

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/initxy/plugrelay/internal/features"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// approvedKey is where an extension's approved permissions live in its
// stored config.
const approvedKey = "approved_permissions"

type pluginPermission struct {
	ID     string `json:"id"`
	API    string `json:"api"`
	Reason string `json:"reason,omitempty"`
}

// extensionView is a plugin outside the built-in features: an external
// process plugin, or a Go plugin linked into a custom build.
type extensionView struct {
	ID           string             `json:"id"`
	Kind         plugin.Kind        `json:"kind"`
	Version      string             `json:"version"`
	Title        string             `json:"title"`
	Description  string             `json:"description,omitempty"`
	Source       string             `json:"source"`
	Enabled      bool               `json:"enabled"`
	Order        int                `json:"order"`
	Status       string             `json:"status"`
	StatusDetail string             `json:"status_detail,omitempty"`
	Permissions  []pluginPermission `json:"permissions"`
	Approved     []string           `json:"approved_permissions,omitempty"`
	Settings     []settingView      `json:"settings"`
}

// isExtension reports whether e is switched on its own rather than by a
// feature. Compiled-in providers always run and are neither.
func isExtension(e *PluginEntry) bool {
	if features.Builtin(e.Info.ID) {
		return false
	}
	return e.Source != SourceBuiltin || e.Info.Kind != plugin.KindProvider
}

func permissionsOf(e *PluginEntry) []pluginPermission {
	out := []pluginPermission{}
	if e.Manifest == nil {
		return out
	}
	reason := strings.TrimSpace(strings.Join([]string{e.Manifest.Permissions.Network, e.Manifest.Permissions.Files}, " "))
	for _, p := range e.Manifest.Permissions.Host {
		out = append(out, pluginPermission{ID: "host." + p, API: "host." + p, Reason: reason})
	}
	return out
}

// required lists every permission an admin must approve.
func required(e *PluginEntry) []string {
	var ids []string
	for _, p := range permissionsOf(e) {
		ids = append(ids, p.ID)
	}
	return ids
}

func extensionViewOf(e *PluginEntry, global layerValues) extensionView {
	v := extensionView{ID: e.Info.ID, Kind: e.Info.Kind, Version: e.Info.Version, Title: e.Info.ID,
		Description: e.Info.Description, Source: e.Source, Enabled: e.Enabled, Order: e.Order, Status: pluginStatus(e),
		StatusDetail: e.StatusDetail, Permissions: permissionsOf(e), Approved: e.Approved, Settings: []settingView{}}
	if e.Manifest != nil && e.Manifest.Name != "" {
		v.Title = e.Manifest.Name
	}
	if v.Version == "" {
		v.Version = "0.0.0"
	}
	for _, d := range e.Info.Settings {
		if d.Scopes&plugin.Global != 0 {
			v.Settings = append(v.Settings, newSettingView(e.Info.ID, d).withValue(d, global))
		}
	}
	return v
}

func (h *Handler) listExtensions(c *call) error {
	global, err := h.layer(c, plugin.Global, "")
	if err != nil {
		return err
	}
	items := []extensionView{}
	for _, e := range h.d.Runtime.Plugins() {
		if isExtension(&e) {
			items = append(items, extensionViewOf(&e, global))
		}
	}
	out := map[string]any{"items": items}
	if msg := h.d.Runtime.ReloadError(); msg != "" {
		out["reload_error"] = msg
	}
	return c.json(200, out)
}

func (h *Handler) pluginConfig(c *call, id string) (*store.PluginConfig, error) {
	cfgs, err := h.d.Store.ListPluginConfigs(c.ctx)
	if err != nil {
		return nil, err
	}
	for _, pc := range cfgs {
		if pc.PluginID == id {
			return &pc, nil
		}
	}
	return nil, nil
}

func (h *Handler) updateExtension(c *call) error {
	id := c.r.PathValue("id")
	e := h.entries()[id]
	if e == nil || !isExtension(e) {
		return errNotFound(eNotFound.with("object", objExtension))
	}
	var in struct {
		Enabled  *bool                      `json:"enabled"`
		Order    *int                       `json:"order"`
		Approved *[]string                  `json:"approved_permissions"`
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	writes, err := h.prepareSettings(c, plugin.Global, "", layerValues{id: in.Settings}, func(module, key string) (plugin.Setting, bool) {
		return declOf(e, key)
	})
	if err != nil {
		return err
	}
	pc, err := h.pluginConfig(c, id)
	if err != nil {
		return err
	}
	if pc == nil {
		pc = &store.PluginConfig{PluginID: id, Enabled: e.Enabled, Order: e.Order}
	}
	if pc.Config == nil {
		pc.Config = map[string]any{}
	}
	approved := e.Approved
	var changes []change
	if in.Approved != nil {
		approved = *in.Approved
		pc.Config[approvedKey] = approved
		changes = append(changes, change{Field: approvedKey, From: e.Approved, To: approved})
	}
	if in.Order != nil {
		changes = append(changes, change{Field: "order", From: pc.Order, To: *in.Order})
		pc.Order = *in.Order
	}
	if in.Enabled != nil {
		if *in.Enabled {
			var missing []string
			for _, r := range required(e) {
				if !slices.Contains(approved, r) {
					missing = append(missing, r)
				}
			}
			if len(missing) > 0 {
				return errConflict(eApproveFirst.with("permissions", strings.Join(missing, ", ")))
			}
		}
		changes = append(changes, change{Field: "enabled", From: e.Enabled, To: *in.Enabled})
		pc.Enabled = *in.Enabled
	}
	if in.Enabled != nil || in.Order != nil || in.Approved != nil {
		// Stored, a conflicting change would fail every reload (later
		// changes would silently not apply) and the next start.
		if err := h.d.Runtime.CheckPlugin(c.ctx, pc); err != nil {
			return errConflict(eChangeCannotRun.with("reason", err.Error()))
		}
		if err := h.d.Store.PutPluginConfig(c.ctx, pc); err != nil {
			return storeErr(err, objExtension)
		}
	}
	set, err := h.writeSettings(c, plugin.Global, "", writes)
	if err != nil {
		return err
	}
	action := "extension.update"
	if in.Enabled != nil {
		action = map[bool]string{true: "extension.enable", false: "extension.disable"}[*in.Enabled]
	}
	if changes = append(changes, set...); len(changes) > 0 {
		c.audit(action, "extension", id, id, changes)
	}
	e.Enabled, e.Approved, e.Order = pc.Enabled, approved, pc.Order
	if e.Enabled && e.Status == "stopped" {
		e.Status = "running"
	}
	global, err := h.layer(c, plugin.Global, "")
	if err != nil {
		return err
	}
	return c.json(200, extensionViewOf(e, global))
}

func (h *Handler) plugin(id string) (*PluginEntry, bool) {
	e := h.entries()[id]
	return e, e != nil
}
