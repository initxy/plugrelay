package admin

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
)

type routeView struct {
	UpstreamID    string   `json:"upstream_id"`
	UpstreamModel string   `json:"upstream_model"`
	Priority      int      `json:"priority"`
	Weight        int      `json:"weight"`
	Groups        []string `json:"groups,omitempty"`
}

type modelView struct {
	ID             string       `json:"id"`
	Name           string       `json:"name"`
	Enabled        bool         `json:"enabled"`
	DisabledReason string       `json:"disabled_reason,omitempty"`
	Info           ir.ModelInfo `json:"info"`
	Routes         []routeView  `json:"routes"`
	SyncedAt       *time.Time   `json:"synced_at"`
}

type modelInput struct {
	Name    *string       `json:"name"`
	Enabled *bool         `json:"enabled"`
	Info    *ir.ModelInfo `json:"info"`
	Routes  *[]routeView  `json:"routes"`
}

func modelViewOf(m *store.Model, routes []store.Route) modelView {
	v := modelView{ID: m.Name, Name: m.Name, Enabled: m.Enabled, Info: m.Info, Routes: []routeView{}}
	if v.Info.ID == "" {
		v.Info.ID = m.Name
	}
	if !m.Enabled {
		v.DisabledReason = "upstream_gone"
		if m.ManualDisabled {
			v.DisabledReason = "manual"
		}
	}
	for _, r := range routes {
		if r.Model == m.Name {
			v.Routes = append(v.Routes, routeView{UpstreamID: r.UpstreamID, UpstreamModel: r.UpstreamModel,
				Priority: r.Priority, Weight: r.Weight, Groups: r.Groups})
		}
	}
	return v
}

func (h *Handler) listModels(c *call) error {
	ms, err := h.d.Store.ListModels(c.ctx)
	if err != nil {
		return err
	}
	routes, err := h.d.Store.ListRoutes(c.ctx, "")
	if err != nil {
		return err
	}
	allowed := func(string) bool { return true }
	if !c.who.isAdmin() {
		allowed = h.callerModelFilter(c)
		routes = nil // routing details are for admins
	}
	items := []modelView{}
	for i := range ms {
		if !c.who.isAdmin() && (!ms[i].Enabled || !allowed(ms[i].Name)) {
			continue
		}
		items = append(items, modelViewOf(&ms[i], routes))
	}
	return c.json(200, map[string]any{"items": items})
}

func (h *Handler) writeModel(c *call, status int, name string) error {
	m, err := h.d.Store.GetModel(c.ctx, name)
	if err != nil {
		return storeErr(err, objModel)
	}
	routes, err := h.d.Store.ListRoutes(c.ctx, name)
	if err != nil {
		return err
	}
	return c.json(status, modelViewOf(m, routes))
}

func (h *Handler) applyModelInput(c *call, m *store.Model, in modelInput, bad *invalid) []store.Route {
	if in.Enabled != nil {
		m.Enabled = *in.Enabled
		m.ManualDisabled = !*in.Enabled
	}
	if in.Info != nil {
		m.Info = *in.Info
		m.Info.ID = m.Name
	}
	if in.Routes == nil {
		return nil
	}
	out := []store.Route{}
	for i, r := range *in.Routes {
		if _, err := h.d.Store.GetUpstream(c.ctx, r.UpstreamID); err != nil {
			bad.add(fmt.Sprintf("routes.%d.upstream_id", i), eNoSuch.with("object", objUpstream))
		}
		if r.UpstreamModel == "" {
			r.UpstreamModel = m.Name
		}
		if r.Weight < 0 {
			bad.add(fmt.Sprintf("routes.%d.weight", i), eNotNegative)
		}
		out = append(out, store.Route{Model: m.Name, UpstreamID: r.UpstreamID, UpstreamModel: r.UpstreamModel,
			Priority: r.Priority, Weight: r.Weight, Groups: r.Groups})
	}
	return out
}

func (h *Handler) createModel(c *call) error {
	var in modelInput
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	name := ""
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
	}
	if name == "" {
		bad.add("name", eNotEmpty)
	} else if _, err := h.d.Store.GetModel(c.ctx, name); err == nil {
		bad.add("name", eNameTaken.with("object", objModel))
	} else if _, err := h.d.Store.GetPreset(c.ctx, name); err == nil {
		bad.add("name", eNameTaken.with("object", objPreset))
	}
	m := &store.Model{Name: name, Enabled: true}
	m.Info.ID = name
	routes := h.applyModelInput(c, m, in, &bad)
	if err := bad.err(); err != nil {
		return err
	}
	if err := h.d.Store.PutModel(c.ctx, m); err != nil {
		return storeErr(err, objModel)
	}
	if routes != nil {
		if err := h.d.Store.ReplaceRoutes(c.ctx, m.Name, routes); err != nil {
			return storeErr(err, objRoute)
		}
	}
	c.audit("model.create", "model", m.Name, m.Name, nil)
	return h.writeModel(c, 201, m.Name)
}

func (h *Handler) updateModel(c *call) error {
	m, err := h.d.Store.GetModel(c.ctx, c.r.PathValue("id"))
	if err != nil {
		return storeErr(err, objModel)
	}
	var in modelInput
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	if in.Name != nil && *in.Name != m.Name {
		bad.add("name", eModelRename)
	}
	before := modelViewOf(m, nil)
	routes := h.applyModelInput(c, m, in, &bad)
	if err := bad.err(); err != nil {
		return err
	}
	if err := h.d.Store.PutModel(c.ctx, m); err != nil {
		return storeErr(err, objModel)
	}
	if routes != nil {
		if err := h.d.Store.ReplaceRoutes(c.ctx, m.Name, routes); err != nil {
			return storeErr(err, objRoute)
		}
	}
	c.audit("model.update", "model", m.Name, m.Name, []change{{Field: "model", From: before, To: modelViewOf(m, nil)}})
	return h.writeModel(c, 200, m.Name)
}

func (h *Handler) deleteModel(c *call) error {
	name := c.r.PathValue("id")
	if _, err := h.d.Store.GetModel(c.ctx, name); err != nil {
		return storeErr(err, objModel)
	}
	if err := h.d.Store.DeleteModel(c.ctx, name); err != nil {
		return storeErr(err, objModel)
	}
	c.audit("model.delete", "model", name, name, nil)
	return c.noContent()
}

func (h *Handler) syncModels(c *call) error {
	var in struct {
		UpstreamIDs []string `json:"upstream_ids"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	res := h.d.Runtime.SyncModels(c.ctx, in.UpstreamIDs)
	res.Added, res.Disabled, res.Failed = nonNil(res.Added), nonNil(res.Disabled), nonNil(res.Failed)
	c.audit("model.sync", "model", "*", "", []change{{Field: "added", To: res.Added}, {Field: "disabled", To: res.Disabled}})
	return c.json(200, res)
}

// callerModelFilter is the caller's own model allowlist (their group's).
func (h *Handler) callerModelFilter(c *call) func(string) bool {
	var allow []string
	if u := c.who.user; u != nil && u.GroupID != "" {
		if g, err := h.d.Store.GetGroup(c.ctx, u.GroupID); err == nil {
			allow = groupViewOf(g, 0).Models
		}
	}
	return func(name string) bool { return globMatch(name, allow) }
}

// ---------------------------------------------------------------- presets

type presetView struct {
	Name     string   `json:"name"`
	Models   []string `json:"models"`
	Strategy string   `json:"strategy"`
}

type presetInput struct {
	Name     *string   `json:"name"`
	Models   *[]string `json:"models"`
	Strategy *string   `json:"strategy"`
}

func presetViewOf(p *store.Preset) presetView {
	return presetView{Name: p.Name, Models: nonNil(p.Models), Strategy: string(p.Strategy)}
}

func (h *Handler) listPresets(c *call) error {
	ps, err := h.d.Store.ListPresets(c.ctx)
	if err != nil {
		return err
	}
	allowed := func(string) bool { return true }
	if !c.who.isAdmin() {
		allowed = h.callerModelFilter(c)
	}
	items := []presetView{}
	for i := range ps {
		v := presetViewOf(&ps[i])
		if !c.who.isAdmin() {
			v.Models = slices.DeleteFunc(v.Models, func(m string) bool { return !allowed(m) })
			if len(v.Models) == 0 {
				continue
			}
		}
		items = append(items, v)
	}
	return c.json(200, map[string]any{"items": items})
}

func (h *Handler) applyPresetInput(c *call, p *store.Preset, in presetInput, bad *invalid) {
	if in.Strategy != nil {
		p.Strategy = store.PresetStrategy(*in.Strategy)
		if !slices.Contains(store.PresetStrategies, p.Strategy) {
			bad.add("strategy", oneOf("in_order", "cheapest", "fastest"))
		}
	}
	if in.Models != nil {
		p.Models = nil
		for i, m := range *in.Models {
			m = strings.TrimSpace(m)
			switch {
			case m == p.Name:
				bad.add(fmt.Sprintf("models.%d", i), ePresetSelf)
			case slices.Contains(p.Models, m):
				bad.add(fmt.Sprintf("models.%d", i), eListedTwice)
			default:
				if _, err := h.d.Store.GetModel(c.ctx, m); err != nil {
					bad.add(fmt.Sprintf("models.%d", i), eNoSuch.with("object", objModel))
				}
			}
			p.Models = append(p.Models, m)
		}
	}
	if len(p.Models) == 0 {
		bad.add("models", ePresetEmpty)
	}
}

func (h *Handler) createPreset(c *call) error {
	var in presetInput
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	p := &store.Preset{Strategy: store.PresetInOrder}
	if in.Name != nil {
		p.Name = strings.TrimSpace(*in.Name)
	}
	if p.Name == "" {
		bad.add("name", eNotEmpty)
	} else if _, err := h.d.Store.GetModel(c.ctx, p.Name); err == nil {
		bad.add("name", eNameTaken.with("object", objModel))
	} else if _, err := h.d.Store.GetPreset(c.ctx, p.Name); err == nil {
		bad.add("name", eNameTaken.with("object", objPreset))
	}
	h.applyPresetInput(c, p, in, &bad)
	if err := bad.err(); err != nil {
		return err
	}
	if err := h.d.Store.PutPreset(c.ctx, p); err != nil {
		return storeErr(err, objPreset)
	}
	c.audit("preset.create", "preset", p.Name, p.Name, nil)
	return c.json(201, presetViewOf(p))
}

func (h *Handler) updatePreset(c *call) error {
	p, err := h.d.Store.GetPreset(c.ctx, c.r.PathValue("name"))
	if err != nil {
		return storeErr(err, objPreset)
	}
	var in presetInput
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	if in.Name != nil && *in.Name != p.Name {
		bad.add("name", eImmutable)
	}
	before := presetViewOf(p)
	h.applyPresetInput(c, p, in, &bad)
	if err := bad.err(); err != nil {
		return err
	}
	if err := h.d.Store.PutPreset(c.ctx, p); err != nil {
		return storeErr(err, objPreset)
	}
	after := presetViewOf(p)
	c.audit("preset.update", "preset", p.Name, p.Name, []change{{Field: "preset", From: before, To: after}})
	return c.json(200, after)
}

func (h *Handler) deletePreset(c *call) error {
	name := c.r.PathValue("name")
	if err := h.d.Store.DeletePreset(c.ctx, name); err != nil {
		return storeErr(err, objPreset)
	}
	c.audit("preset.delete", "preset", name, name, nil)
	return c.noContent()
}
