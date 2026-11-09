package store

import (
	"context"
	"fmt"
	"slices"
	"time"
)

const modelCols = `name, info, enabled, manual_disabled, created_at, updated_at`

func scanModel(sc interface{ Scan(...any) error }) (*Model, error) {
	var m Model
	var info string
	var created, updated int64
	if err := sc.Scan(&m.Name, &info, &m.Enabled, &m.ManualDisabled, &created, &updated); err != nil {
		return nil, err
	}
	m.CreatedAt, m.UpdatedAt = fromMs(created), fromMs(updated)
	return &m, unjs(info, &m.Info)
}

func (s *db) PutModel(ctx context.Context, m *Model) error {
	return s.write(ctx, func(r run) error { return putModel(ctx, r, m) })
}

func putModel(ctx context.Context, r run, m *Model) error {
	if m.Name == "" {
		return fmt.Errorf("store: put model: empty name")
	}
	if err := nameFree(ctx, r, "presets", m.Name); err != nil {
		return fmt.Errorf("store: put model %s: a preset has this name: %w", m.Name, err)
	}
	now := time.Now().UnixMilli()
	_, err := r.exec(ctx, `INSERT INTO models (`+modelCols+`) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET info = excluded.info, enabled = excluded.enabled,
  manual_disabled = excluded.manual_disabled, updated_at = excluded.updated_at`,
		m.Name, js(m.Info, "{}"), m.Enabled, m.ManualDisabled, now, now)
	return wrap("put model", err)
}

// nameFree fails with ErrConflict when table has a row named name: models
// and presets share one namespace.
func nameFree(ctx context.Context, r run, table, name string) error {
	var n int
	if err := r.row(ctx, `SELECT COUNT(*) FROM `+table+` WHERE name = ?`, name).Scan(&n); err != nil {
		return wrap("check name", err)
	}
	if n > 0 {
		return ErrConflict
	}
	return nil
}

const presetCols = `name, models, strategy, created_at, updated_at`

func scanPreset(sc interface{ Scan(...any) error }) (*Preset, error) {
	var p Preset
	var models, strategy string
	var created, updated int64
	if err := sc.Scan(&p.Name, &models, &strategy, &created, &updated); err != nil {
		return nil, err
	}
	p.Strategy = PresetStrategy(strategy)
	p.CreatedAt, p.UpdatedAt = fromMs(created), fromMs(updated)
	return &p, unjs(models, &p.Models)
}

func (s *db) PutPreset(ctx context.Context, p *Preset) error {
	return s.write(ctx, func(r run) error { return putPreset(ctx, r, p) })
}

func putPreset(ctx context.Context, r run, p *Preset) error {
	if p.Name == "" {
		return fmt.Errorf("store: put preset: empty name")
	}
	if p.Strategy == "" {
		p.Strategy = PresetInOrder
	}
	if !slices.Contains(PresetStrategies, p.Strategy) {
		return fmt.Errorf("store: put preset %s: unknown strategy %q", p.Name, p.Strategy)
	}
	if len(p.Models) == 0 {
		return fmt.Errorf("store: put preset %s: no models", p.Name)
	}
	if slices.Contains(p.Models, p.Name) {
		return fmt.Errorf("store: put preset %s: lists itself", p.Name)
	}
	if err := nameFree(ctx, r, "models", p.Name); err != nil {
		return fmt.Errorf("store: put preset %s: a model has this name: %w", p.Name, err)
	}
	now := time.Now().UnixMilli()
	_, err := r.exec(ctx, `INSERT INTO presets (`+presetCols+`) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET models = excluded.models, strategy = excluded.strategy, updated_at = excluded.updated_at`,
		p.Name, js(p.Models, "[]"), string(p.Strategy), now, now)
	return wrap("put preset", err)
}

func (s *db) GetPreset(ctx context.Context, name string) (*Preset, error) {
	p, err := scanPreset(s.r().row(ctx, `SELECT `+presetCols+` FROM presets WHERE name = ?`, name))
	return p, wrap("get preset", err)
}

func (s *db) ListPresets(ctx context.Context) ([]Preset, error) { return listPresets(ctx, s.r()) }

func listPresets(ctx context.Context, r run) ([]Preset, error) {
	return list(ctx, r, "list presets", `SELECT `+presetCols+` FROM presets ORDER BY name`, scanPreset)
}

func (s *db) DeletePreset(ctx context.Context, name string) error {
	return s.write(ctx, func(r run) error {
		res, err := r.exec(ctx, `DELETE FROM presets WHERE name = ?`, name)
		return mustAffect("delete preset", res, err)
	})
}

func (s *db) GetModel(ctx context.Context, name string) (*Model, error) {
	m, err := scanModel(s.r().row(ctx, `SELECT `+modelCols+` FROM models WHERE name = ?`, name))
	return m, wrap("get model", err)
}

func (s *db) ListModels(ctx context.Context) ([]Model, error) { return listModels(ctx, s.r()) }

func listModels(ctx context.Context, r run) ([]Model, error) {
	return list(ctx, r, "list models", `SELECT `+modelCols+` FROM models ORDER BY name`, scanModel)
}

func (s *db) DeleteModel(ctx context.Context, name string) error {
	return s.write(ctx, func(r run) error {
		res, err := r.exec(ctx, `DELETE FROM models WHERE name = ?`, name)
		if err := mustAffect("delete model", res, err); err != nil {
			return err
		}
		_, err = r.exec(ctx, `DELETE FROM routes WHERE model = ?`, name)
		return wrap("delete model routes", err)
	})
}

const routeCols = `model, upstream_id, upstream_model, priority, weight, "groups"`

func scanRoute(sc interface{ Scan(...any) error }) (*Route, error) {
	var rt Route
	var groups string
	if err := sc.Scan(&rt.Model, &rt.UpstreamID, &rt.UpstreamModel, &rt.Priority, &rt.Weight, &groups); err != nil {
		return nil, err
	}
	return &rt, unjs(groups, &rt.Groups)
}

func (s *db) ReplaceRoutes(ctx context.Context, model string, routes []Route) error {
	return s.write(ctx, func(r run) error { return replaceRoutes(ctx, r, model, routes) })
}

func replaceRoutes(ctx context.Context, r run, model string, routes []Route) error {
	if _, err := r.exec(ctx, `DELETE FROM routes WHERE model = ?`, model); err != nil {
		return wrap("replace routes", err)
	}
	for i := range routes {
		rt := &routes[i]
		rt.Model = model
		if rt.UpstreamID == "" {
			return fmt.Errorf("store: replace routes %s: route %d has no upstream", model, i)
		}
		if rt.UpstreamModel == "" {
			rt.UpstreamModel = model
		}
		if rt.Weight <= 0 {
			rt.Weight = 1
		}
		if _, err := r.exec(ctx, `INSERT INTO routes (`+routeCols+`) VALUES (?, ?, ?, ?, ?, ?)`,
			rt.Model, rt.UpstreamID, rt.UpstreamModel, rt.Priority, rt.Weight, js(rt.Groups, "[]")); err != nil {
			return wrap("replace routes", err)
		}
	}
	return nil
}

func (s *db) ListRoutes(ctx context.Context, model string) ([]Route, error) {
	return listRoutes(ctx, s.r(), model)
}

func listRoutes(ctx context.Context, r run, model string) ([]Route, error) {
	return list(ctx, r, "list routes", `SELECT `+routeCols+` FROM routes WHERE (? = '' OR model = ?)
ORDER BY model, priority DESC, weight DESC, upstream_id, upstream_model`, scanRoute, model, model)
}

const priceCols = `model, upstream_id, upstream_model, input, cache_read, cache_write_5m, cache_write_1h, output, currency, units, batch_discount, source, updated_at`

func scanPrice(sc interface{ Scan(...any) error }) (*PriceRule, error) {
	var p PriceRule
	var units string
	var updated int64
	err := sc.Scan(&p.Model, &p.UpstreamID, &p.UpstreamModel, &p.Input, &p.CacheRead, &p.CacheWrite5m, &p.CacheWrite1h, &p.Output, &p.Currency, &units, &p.BatchDiscount,
		&p.Source, &updated)
	if err == nil {
		p.UpdatedAt = fromMs(updated)
		err = unjs(units, &p.Units)
		if len(p.Units) == 0 {
			p.Units = nil
		}
	}
	return &p, err
}

func (s *db) PutPrice(ctx context.Context, p *PriceRule) error {
	return s.write(ctx, func(r run) error { return putPrice(ctx, r, p) })
}

// checkBatchDiscount accepts a fraction in [0, 1): 1 would make batches
// free, a negative one a surcharge, NaN breaks every charge.
func checkBatchDiscount(v float64) error {
	if !(v >= 0 && v < 1) {
		return fmt.Errorf("batch_discount must be at least 0 and below 1 (got %v)", v)
	}
	return nil
}

func putPrice(ctx context.Context, r run, p *PriceRule) error {
	if p.Model == "" && p.UpstreamModel == "" {
		return fmt.Errorf("store: put price: needs a model or an upstream model")
	}
	if p.Currency == "" {
		p.Currency = "USD"
	}
	if p.Source == "" {
		p.Source = PriceManual
	}
	if p.Source != PriceManual && p.Source != PriceSynced {
		return fmt.Errorf("store: put price: unknown source %q", p.Source)
	}
	if err := checkBatchDiscount(p.BatchDiscount); err != nil {
		return fmt.Errorf("store: put price: %w", err)
	}
	p.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	_, err := r.exec(ctx, `INSERT INTO price_rules (`+priceCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (model, upstream_id, upstream_model) DO UPDATE SET input = excluded.input, cache_read = excluded.cache_read,
  cache_write_5m = excluded.cache_write_5m, cache_write_1h = excluded.cache_write_1h, output = excluded.output, currency = excluded.currency,
  units = excluded.units, batch_discount = excluded.batch_discount, source = excluded.source, updated_at = excluded.updated_at`,
		p.Model, p.UpstreamID, p.UpstreamModel, p.Input, p.CacheRead, p.CacheWrite5m, p.CacheWrite1h, p.Output, p.Currency, js(p.Units, "{}"), p.BatchDiscount,
		p.Source, ms(p.UpdatedAt))
	return wrap("put price", err)
}

func (s *db) ListPrices(ctx context.Context) ([]PriceRule, error) { return listPrices(ctx, s.r()) }

func listPrices(ctx context.Context, r run) ([]PriceRule, error) {
	return list(ctx, r, "list prices", `SELECT `+priceCols+` FROM price_rules ORDER BY model, upstream_id, upstream_model`, scanPrice)
}

func (s *db) DeletePrice(ctx context.Context, model, upstreamID, upstreamModel string) error {
	return s.write(ctx, func(r run) error {
		res, err := r.exec(ctx, `DELETE FROM price_rules WHERE model = ? AND upstream_id = ? AND upstream_model = ?`, model, upstreamID, upstreamModel)
		return mustAffect("delete price", res, err)
	})
}
