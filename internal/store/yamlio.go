package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/initxy/plugrelay/internal/features"
	"github.com/initxy/plugrelay/internal/settings"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Document is the YAML shape of Export and Apply. Field names follow the
// JSON tags below. Users, keys, logs and usage are not part of it.
//
// Secret settings stay sealed under the master key, so a document carrying
// them only applies to a store with the same master key.
type Document struct {
	Groups      []DocGroup      `json:"groups,omitempty"`
	Upstreams   []DocUpstream   `json:"upstreams,omitempty"`
	Credentials []DocCredential `json:"credentials,omitempty"`
	Models      []DocModel      `json:"models,omitempty"`
	Presets     []DocPreset     `json:"presets,omitempty"`
	Prices      []PriceRule     `json:"prices,omitempty"`
	Features    []FeatureState  `json:"features,omitempty"`
	Plugins     []DocPlugin     `json:"plugins,omitempty"`
	Settings    []DocSetting    `json:"settings,omitempty"`
	Manifests   []DocManifest   `json:"manifests,omitempty"`
}

// DocGroup is a group in a Document.
type DocGroup struct {
	ID              string                     `json:"id"`
	Name            string                     `json:"name"`
	Settings        map[string]json.RawMessage `json:"settings,omitempty"`
	PriceMultiplier float64                    `json:"price_multiplier,omitempty"`
}

// DocUpstream is an upstream in a Document; Enabled defaults to true.
type DocUpstream struct {
	ID        string            `json:"id"`
	Provider  string            `json:"provider"`
	Manifest  string            `json:"manifest,omitempty"`
	Endpoints []plugin.Endpoint `json:"endpoints,omitempty"`
	Config    map[string]any    `json:"config,omitempty"`
	Enabled   *bool             `json:"enabled,omitempty"`
	Source    Source            `json:"source,omitempty"`
}

// DocCredential is a credential in a Document. Secret is plaintext; when
// absent on Apply the stored secret is kept. Status is runtime data and is
// neither exported nor applied.
type DocCredential struct {
	ID             string            `json:"id"`
	Upstream       string            `json:"upstream"`
	Kind           CredentialKind    `json:"kind"`
	Owner          string            `json:"owner,omitempty"`
	Secret         string            `json:"secret,omitempty"`
	Proxy          string            `json:"proxy,omitempty"`
	MaxConcurrency int               `json:"max_concurrency,omitempty"`
	Meta           map[string]string `json:"meta,omitempty"`
	ExpiresAt      time.Time         `json:"expires_at,omitzero"`
}

// DocModel is a model with its routes; Enabled defaults to true. Routes
// absent leaves stored routes alone; an empty list removes them.
type DocModel struct {
	Name           string        `json:"name"`
	Info           *ir.ModelInfo `json:"info,omitempty"`
	Enabled        *bool         `json:"enabled,omitempty"`
	ManualDisabled bool          `json:"manual_disabled,omitempty"`
	Routes         []DocRoute    `json:"routes"`
}

// DocPreset is a preset; Strategy defaults to in_order.
type DocPreset struct {
	Name     string         `json:"name"`
	Models   []string       `json:"models"`
	Strategy PresetStrategy `json:"strategy,omitempty"`
}

// DocRoute is one route of a DocModel.
type DocRoute struct {
	Upstream      string   `json:"upstream"`
	UpstreamModel string   `json:"upstream_model,omitempty"`
	Priority      int      `json:"priority,omitempty"`
	Weight        int      `json:"weight,omitempty"`
	Groups        []string `json:"groups,omitempty"`
}

// DocPlugin is an extension's config (a plugin outside the built-in
// features); Enabled defaults to true.
type DocPlugin struct {
	ID      string         `json:"id"`
	Enabled *bool          `json:"enabled,omitempty"`
	Order   int            `json:"order,omitempty"`
	Config  map[string]any `json:"config,omitempty"`
}

// DocSetting is one layered setting value.
type DocSetting struct {
	Scope   string          `json:"scope"`
	ScopeID string          `json:"scope_id,omitempty"`
	Plugin  string          `json:"plugin"`
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value"`
}

// DocManifest is a stored manifest.
type DocManifest struct {
	ID     string `json:"id"`
	YAML   string `json:"yaml"`
	Source Source `json:"source,omitempty"`
}

// ExportOptions tunes Export.
type ExportOptions struct {
	// Secrets includes decrypted credential secrets.
	Secrets bool
}

// ApplyResult summarizes what Apply did or, on a dry run, would do.
type ApplyResult struct {
	Changes   []Change `json:"changes"`
	Created   int      `json:"created"`
	Updated   int      `json:"updated"`
	Unchanged int      `json:"unchanged"`
}

// Change is one row Apply touched.
type Change struct {
	Kind string `json:"kind"` // group, upstream, credential, model, routes, preset, price, plugin, setting, manifest
	ID   string `json:"id"`
	Op   string `json:"op"` // create, update, unchanged
}

func (a *ApplyResult) add(kind, id, op string) {
	a.Changes = append(a.Changes, Change{Kind: kind, ID: id, Op: op})
	switch op {
	case "create":
		a.Created++
	case "update":
		a.Updated++
	default:
		a.Unchanged++
	}
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func ptr[T any](v T) *T { return &v }

func docGroup(g Group) DocGroup {
	return DocGroup{ID: g.ID, Name: g.Name, Settings: g.Settings, PriceMultiplier: g.PriceMultiplier}
}

func docUpstream(u Upstream) DocUpstream {
	return DocUpstream{ID: u.ID, Provider: u.Provider, Manifest: u.Manifest, Endpoints: u.Endpoints, Config: u.Config, Enabled: ptr(u.Enabled), Source: u.Source}
}

func docCredential(c Credential) DocCredential {
	return DocCredential{ID: c.ID, Upstream: c.UpstreamID, Kind: c.Kind, Owner: c.OwnerID, Proxy: c.Proxy,
		MaxConcurrency: c.MaxConcurrency, Meta: c.Meta, ExpiresAt: c.ExpiresAt}
}

func docModel(m Model, routes []Route) DocModel {
	d := DocModel{Name: m.Name, Enabled: ptr(m.Enabled), ManualDisabled: m.ManualDisabled,
		Routes: []DocRoute{}}
	if !reflect.ValueOf(m.Info).IsZero() {
		d.Info = ptr(m.Info)
	}
	for _, r := range routes {
		d.Routes = append(d.Routes, DocRoute{Upstream: r.UpstreamID, UpstreamModel: r.UpstreamModel, Priority: r.Priority, Weight: r.Weight, Groups: r.Groups})
	}
	return d
}

func docSetting(e SettingRow) DocSetting {
	return DocSetting{Scope: settings.ScopeName(e.Scope), ScopeID: e.ScopeID, Plugin: e.PluginID, Key: e.Key, Value: e.Value}
}

func (s *db) Export(ctx context.Context, opts ExportOptions) ([]byte, error) {
	if opts.Secrets && s.box == nil {
		return nil, ErrNoBox
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	var d Document
	for _, g := range snap.Groups {
		d.Groups = append(d.Groups, docGroup(g))
	}
	for _, u := range snap.Upstreams {
		d.Upstreams = append(d.Upstreams, docUpstream(u))
	}
	for _, c := range snap.Credentials {
		dc := docCredential(c)
		if opts.Secrets && c.Sealed != "" {
			pc, err := c.Open(s.box)
			if err != nil {
				return nil, err
			}
			dc.Secret = pc.Secret
		}
		d.Credentials = append(d.Credentials, dc)
	}
	byModel := map[string][]Route{}
	for _, r := range snap.Routes {
		byModel[r.Model] = append(byModel[r.Model], r)
	}
	for _, m := range snap.Models {
		d.Models = append(d.Models, docModel(m, byModel[m.Name]))
	}
	for _, p := range snap.Presets {
		d.Presets = append(d.Presets, DocPreset{Name: p.Name, Models: p.Models, Strategy: p.Strategy})
	}
	for _, p := range snap.Prices {
		p.UpdatedAt = time.Time{}
		if p.Source == PriceManual {
			p.Source = ""
		}
		d.Prices = append(d.Prices, p)
	}
	d.Features = snap.Features
	for _, p := range snap.Plugins {
		if features.Builtin(p.PluginID) {
			continue
		}
		d.Plugins = append(d.Plugins, DocPlugin{ID: p.PluginID, Enabled: ptr(p.Enabled), Order: p.Order, Config: p.Config})
	}
	for _, e := range snap.Settings {
		d.Settings = append(d.Settings, docSetting(e))
	}
	for _, m := range snap.Manifests {
		d.Manifests = append(d.Manifests, DocManifest{ID: m.ID, YAML: m.YAML, Source: m.Source})
	}
	return MarshalDocument(&d)
}

// MarshalDocument renders a Document as YAML.
func MarshalDocument(d *Document) ([]byte, error) {
	j, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("store: export: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, fmt.Errorf("store: export: %w", err)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(numbers(generic)); err != nil {
		return nil, fmt.Errorf("store: export: %w", err)
	}
	return buf.Bytes(), enc.Close()
}

// numbers turns json.Number into int64 or float64 so YAML prints them bare.
func numbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = numbers(x)
		}
	case []any:
		for i, x := range t {
			t[i] = numbers(x)
		}
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	}
	return v
}

// ParseDocument reads YAML into a Document; unknown fields are errors.
func ParseDocument(data []byte) (*Document, error) {
	var generic any
	if err := yaml.Unmarshal(data, &generic); err != nil {
		return nil, fmt.Errorf("store: parse document: %w", err)
	}
	j, err := json.Marshal(generic)
	if err != nil {
		return nil, fmt.Errorf("store: parse document: %w", err)
	}
	var d Document
	if string(j) == "null" {
		return &d, nil
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("store: parse document: %w", err)
	}
	return &d, nil
}

// canon renders v as canonical JSON (sorted keys) for comparisons.
func canon(v any) string {
	j, _ := json.Marshal(v)
	var g any
	if json.Unmarshal(j, &g) != nil {
		return string(j)
	}
	out, _ := json.Marshal(g)
	return string(out)
}

var errDryRun = errors.New("dry run")

func (s *db) Apply(ctx context.Context, data []byte, dryRun bool) (*ApplyResult, error) {
	d, err := ParseDocument(data)
	if err != nil {
		return nil, err
	}
	var res *ApplyResult
	err = s.write(ctx, func(r run) error {
		res = &ApplyResult{Changes: []Change{}}
		if err := s.apply(ctx, r, d, res); err != nil {
			return err
		}
		if dryRun {
			return errDryRun
		}
		return nil
	})
	if errors.Is(err, errDryRun) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

func op(found bool, before, after any) string {
	switch {
	case !found:
		return "create"
	case canon(before) == canon(after):
		return "unchanged"
	}
	return "update"
}

func (s *db) apply(ctx context.Context, r run, d *Document, res *ApplyResult) error {
	for _, dg := range d.Groups {
		g := Group{ID: dg.ID, Name: dg.Name, Settings: dg.Settings, PriceMultiplier: dg.PriceMultiplier}
		if g.ID == "" {
			return fmt.Errorf("store: apply: group without id")
		}
		if g.PriceMultiplier == 0 {
			g.PriceMultiplier = 1
		}
		cur, err := scanGroup(r.row(ctx, `SELECT `+groupCols+` FROM user_groups WHERE id = ?`, g.ID))
		if err != nil && !isNotFound(err) {
			return wrap("apply group", err)
		}
		o := op(cur != nil, docGroupOf(cur), docGroup(g))
		if o != "unchanged" {
			if err := putGroup(ctx, r, &g); err != nil {
				return err
			}
		}
		res.add("group", g.ID, o)
	}
	for _, du := range d.Upstreams {
		u := Upstream{ID: du.ID, Provider: du.Provider, Manifest: du.Manifest, Endpoints: du.Endpoints, Config: du.Config,
			Enabled: boolOr(du.Enabled, true), Source: du.Source}
		if u.Source == "" {
			u.Source = SourceCustom
		}
		cur, err := scanUpstream(r.row(ctx, `SELECT `+upstreamCols+` FROM upstreams WHERE id = ?`, u.ID))
		if err != nil && !isNotFound(err) {
			return wrap("apply upstream", err)
		}
		var before any
		if cur != nil {
			before = docUpstream(*cur)
		}
		o := op(cur != nil, before, docUpstream(u))
		if o != "unchanged" {
			if err := putUpstream(ctx, r, &u); err != nil {
				return err
			}
		}
		res.add("upstream", u.ID, o)
	}
	for _, dc := range d.Credentials {
		if dc.ID == "" {
			return fmt.Errorf("store: apply: credential without id")
		}
		c := Credential{ID: dc.ID, UpstreamID: dc.Upstream, Kind: dc.Kind, OwnerID: dc.Owner, Secret: dc.Secret, Proxy: dc.Proxy,
			MaxConcurrency: dc.MaxConcurrency, Meta: dc.Meta, ExpiresAt: dc.ExpiresAt, Status: CredActive}
		cur, err := scanCred(r.row(ctx, `SELECT `+credCols+` FROM credentials WHERE id = ?`, c.ID))
		if err != nil && !isNotFound(err) {
			return wrap("apply credential", err)
		}
		var before any
		secretChanged := false
		if cur != nil {
			before = docCredential(*cur)
			c.Status, c.StatusReason, c.StatusUntil = cur.Status, cur.StatusReason, cur.StatusUntil
			if c.Secret != "" {
				old, err := cur.Open(s.box)
				secretChanged = err != nil || old.Secret != c.Secret
			}
		}
		o := op(cur != nil, before, docCredential(c))
		if o == "unchanged" && secretChanged {
			o = "update"
		}
		if o != "unchanged" {
			if err := s.putCredential(ctx, r, &c); err != nil {
				return err
			}
		}
		res.add("credential", c.ID, o)
	}
	for _, dm := range d.Models {
		m := Model{Name: dm.Name, Enabled: boolOr(dm.Enabled, true), ManualDisabled: dm.ManualDisabled}
		if dm.Info != nil {
			m.Info = *dm.Info
		}
		cur, err := scanModel(r.row(ctx, `SELECT `+modelCols+` FROM models WHERE name = ?`, m.Name))
		if err != nil && !isNotFound(err) {
			return wrap("apply model", err)
		}
		var before any
		if cur != nil {
			b := docModel(*cur, nil)
			b.Routes = nil
			before = b
		}
		after := docModel(m, nil)
		after.Routes = nil
		o := op(cur != nil, before, after)
		if o != "unchanged" {
			if err := putModel(ctx, r, &m); err != nil {
				return err
			}
		}
		res.add("model", m.Name, o)
		if dm.Routes == nil {
			continue
		}
		var routes []Route
		for _, dr := range dm.Routes {
			rt := Route{Model: m.Name, UpstreamID: dr.Upstream, UpstreamModel: dr.UpstreamModel, Priority: dr.Priority, Weight: dr.Weight, Groups: dr.Groups}
			if rt.UpstreamModel == "" {
				rt.UpstreamModel = m.Name
			}
			if rt.Weight <= 0 {
				rt.Weight = 1
			}
			routes = append(routes, rt)
		}
		old, err := listRoutes(ctx, r, m.Name)
		if err != nil {
			return err
		}
		ro := op(len(old) > 0, docModel(Model{}, old).Routes, docModel(Model{}, sortRoutes(routes)).Routes)
		if ro != "unchanged" {
			if err := replaceRoutes(ctx, r, m.Name, routes); err != nil {
				return err
			}
		}
		res.add("routes", m.Name, ro)
	}
	for _, dp := range d.Presets {
		p := Preset{Name: dp.Name, Models: dp.Models, Strategy: dp.Strategy}
		if p.Strategy == "" {
			p.Strategy = PresetInOrder
		}
		cur, err := scanPreset(r.row(ctx, `SELECT `+presetCols+` FROM presets WHERE name = ?`, p.Name))
		if err != nil && !isNotFound(err) {
			return wrap("apply preset", err)
		}
		var before any
		if cur != nil {
			before = DocPreset{Name: cur.Name, Models: cur.Models, Strategy: cur.Strategy}
		}
		o := op(cur != nil, before, DocPreset{Name: p.Name, Models: p.Models, Strategy: p.Strategy})
		if o != "unchanged" {
			if err := putPreset(ctx, r, &p); err != nil {
				return err
			}
		}
		res.add("preset", p.Name, o)
	}
	for _, p := range d.Prices {
		if p.Currency == "" {
			p.Currency = "USD"
		}
		if err := checkBatchDiscount(p.BatchDiscount); err != nil {
			return fmt.Errorf("store: apply price %s|%s|%s: %w", p.Model, p.UpstreamID, p.UpstreamModel, err)
		}
		cur, err := scanPrice(r.row(ctx, `SELECT `+priceCols+` FROM price_rules WHERE model = ? AND upstream_id = ? AND upstream_model = ?`,
			p.Model, p.UpstreamID, p.UpstreamModel))
		if err != nil && !isNotFound(err) {
			return wrap("apply price", err)
		}
		found := err == nil
		if p.Source == "" {
			p.Source = PriceManual
		}
		if found {
			// When a price last changed is not part of what it is.
			p.UpdatedAt = cur.UpdatedAt
		}
		o := op(found, cur, p)
		if o != "unchanged" {
			if err := putPrice(ctx, r, &p); err != nil {
				return err
			}
		}
		res.add("price", p.Model+"|"+p.UpstreamID+"|"+p.UpstreamModel, o)
	}
	for _, f := range d.Features {
		def, ok := features.Get(f.ID)
		if !ok {
			return fmt.Errorf("store: apply feature %s: no such feature", f.ID)
		}
		if def.AlwaysOn && !f.Enabled {
			return fmt.Errorf("store: apply feature %s: it always runs and cannot be turned off", f.ID)
		}
		cur, err := scanFeature(r.row(ctx, `SELECT id, enabled FROM features WHERE id = ?`, f.ID))
		if err != nil && !isNotFound(err) {
			return wrap("apply feature", err)
		}
		found := err == nil
		var before any
		if found {
			before = *cur
		}
		o := op(found, before, f)
		if o != "unchanged" {
			if err := putFeature(ctx, r, f); err != nil {
				return err
			}
		}
		res.add("feature", f.ID, o)
	}
	for _, dp := range d.Plugins {
		if features.Builtin(dp.ID) {
			return fmt.Errorf("store: apply plugin %s: a built-in module runs with its feature; switch the feature under features: instead", dp.ID)
		}
		p := PluginConfig{PluginID: dp.ID, Enabled: boolOr(dp.Enabled, true), Order: dp.Order, Config: dp.Config}
		cur, err := scanPluginConfig(r.row(ctx, `SELECT `+pluginCols+` FROM plugin_configs WHERE plugin_id = ?`, p.PluginID))
		if err != nil && !isNotFound(err) {
			return wrap("apply plugin", err)
		}
		found := err == nil
		o := op(found, cur, p)
		if o != "unchanged" {
			if err := putPluginConfig(ctx, r, &p); err != nil {
				return err
			}
		}
		res.add("plugin", p.PluginID, o)
	}
	for _, ds := range d.Settings {
		scope, err := settings.ParseScope(ds.Scope)
		if err != nil {
			return fmt.Errorf("store: apply setting %s/%s: %w", ds.Plugin, ds.Key, err)
		}
		if field, owned := settings.RecordOwned(scope, ds.Plugin, ds.Key); owned {
			return fmt.Errorf("store: apply setting %s/%s at %s %s: that layer is the %s's own field %s; set it on the %s itself",
				ds.Plugin, ds.Key, ds.Scope, ds.ScopeID, ds.Scope, field, ds.Scope)
		}
		e := SettingRow{Scope: scope, ScopeID: ds.ScopeID, PluginID: ds.Plugin, Key: ds.Key, Value: ds.Value}
		cur, err := scanSetting(r.row(ctx, `SELECT scope, scope_id, plugin_id, setting_key, value FROM settings
WHERE scope = ? AND scope_id = ? AND plugin_id = ? AND setting_key = ?`, ds.Scope, ds.ScopeID, ds.Plugin, ds.Key))
		if err != nil && !isNotFound(err) {
			return wrap("apply setting", err)
		}
		found := err == nil
		var before any
		if found {
			before = docSetting(*cur)
		}
		o := op(found, before, docSetting(e))
		if o != "unchanged" {
			if err := putSetting(ctx, r, e); err != nil {
				return err
			}
		}
		res.add("setting", ds.Scope+":"+ds.ScopeID+":"+ds.Plugin+"/"+ds.Key, o)
	}
	for _, dm := range d.Manifests {
		m := Manifest{ID: dm.ID, YAML: dm.YAML, Source: dm.Source}
		if m.Source == "" {
			m.Source = SourceCustom
		}
		cur, err := scanManifest(r.row(ctx, `SELECT id, yaml, source, updated_at FROM manifests WHERE id = ?`, m.ID))
		if err != nil && !isNotFound(err) {
			return wrap("apply manifest", err)
		}
		found := err == nil
		var before any
		if found {
			before = DocManifest{ID: cur.ID, YAML: cur.YAML, Source: cur.Source}
		}
		o := op(found, before, DocManifest{ID: m.ID, YAML: m.YAML, Source: m.Source})
		if o != "unchanged" {
			if err := putManifest(ctx, r, &m); err != nil {
				return err
			}
		}
		res.add("manifest", m.ID, o)
	}
	return nil
}

func docGroupOf(g *Group) any {
	if g == nil {
		return nil
	}
	return docGroup(*g)
}

// sortRoutes orders routes as listRoutes returns them.
func sortRoutes(rs []Route) []Route {
	out := append([]Route(nil), rs...)
	less := func(a, b Route) bool {
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if a.Weight != b.Weight {
			return a.Weight > b.Weight
		}
		if a.UpstreamID != b.UpstreamID {
			return a.UpstreamID < b.UpstreamID
		}
		return a.UpstreamModel < b.UpstreamModel
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && less(out[j], out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
