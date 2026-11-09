package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/initxy/plugrelay/internal/settings"
	"github.com/initxy/plugrelay/pkg/plugin"
)

const pluginCols = `plugin_id, enabled, ord, config`

func scanPluginConfig(sc interface{ Scan(...any) error }) (*PluginConfig, error) {
	var p PluginConfig
	var cfg string
	if err := sc.Scan(&p.PluginID, &p.Enabled, &p.Order, &cfg); err != nil {
		return nil, err
	}
	return &p, unjs(cfg, &p.Config)
}

func (s *db) PutPluginConfig(ctx context.Context, p *PluginConfig) error {
	return s.write(ctx, func(r run) error { return putPluginConfig(ctx, r, p) })
}

func putPluginConfig(ctx context.Context, r run, p *PluginConfig) error {
	if p.PluginID == "" {
		return fmt.Errorf("store: put plugin config: empty plugin id")
	}
	_, err := r.exec(ctx, `INSERT INTO plugin_configs (`+pluginCols+`) VALUES (?, ?, ?, ?)
ON CONFLICT (plugin_id) DO UPDATE SET enabled = excluded.enabled, ord = excluded.ord, config = excluded.config`,
		p.PluginID, p.Enabled, p.Order, js(p.Config, "{}"))
	return wrap("put plugin config", err)
}

func (s *db) ListPluginConfigs(ctx context.Context) ([]PluginConfig, error) {
	return listPluginConfigs(ctx, s.r())
}

func listPluginConfigs(ctx context.Context, r run) ([]PluginConfig, error) {
	return list(ctx, r, "list plugin configs", `SELECT `+pluginCols+` FROM plugin_configs ORDER BY ord, plugin_id`, scanPluginConfig)
}

func (s *db) DeletePluginConfig(ctx context.Context, pluginID string) error {
	return s.write(ctx, func(r run) error {
		res, err := r.exec(ctx, `DELETE FROM plugin_configs WHERE plugin_id = ?`, pluginID)
		return mustAffect("delete plugin config", res, err)
	})
}

// ---------------------------------------------------------------- features

func scanFeature(sc interface{ Scan(...any) error }) (*FeatureState, error) {
	var f FeatureState
	return &f, sc.Scan(&f.ID, &f.Enabled)
}

func (s *db) PutFeature(ctx context.Context, f FeatureState) error {
	return s.write(ctx, func(r run) error { return putFeature(ctx, r, f) })
}

func putFeature(ctx context.Context, r run, f FeatureState) error {
	if f.ID == "" {
		return fmt.Errorf("store: put feature: empty id")
	}
	_, err := r.exec(ctx, `INSERT INTO features (id, enabled) VALUES (?, ?)
ON CONFLICT (id) DO UPDATE SET enabled = excluded.enabled`, f.ID, f.Enabled)
	return wrap("put feature", err)
}

func (s *db) ListFeatures(ctx context.Context) ([]FeatureState, error) {
	return listFeatures(ctx, s.r())
}

func listFeatures(ctx context.Context, r run) ([]FeatureState, error) {
	return list(ctx, r, "list features", `SELECT id, enabled FROM features ORDER BY id`, scanFeature)
}

// ---------------------------------------------------------------- settings

func scanSetting(sc interface{ Scan(...any) error }) (*SettingRow, error) {
	var e SettingRow
	var scope, value string
	if err := sc.Scan(&scope, &e.ScopeID, &e.PluginID, &e.Key, &value); err != nil {
		return nil, err
	}
	sc2, err := settings.ParseScope(scope)
	if err != nil {
		return nil, err
	}
	e.Scope, e.Value = sc2, json.RawMessage(value)
	return &e, nil
}

func (s *db) PutSetting(ctx context.Context, e SettingRow) error {
	return s.write(ctx, func(r run) error { return putSetting(ctx, r, e) })
}

func putSetting(ctx context.Context, r run, e SettingRow) error {
	name := settings.ScopeName(e.Scope)
	if name == "" {
		return fmt.Errorf("store: put setting %s: invalid scope %d", e.Key, e.Scope)
	}
	if e.PluginID == "" || e.Key == "" {
		return fmt.Errorf("store: put setting: plugin id and key are required")
	}
	if (e.Scope == plugin.Global) != (e.ScopeID == "") {
		return fmt.Errorf("store: put setting %s: scope id must be empty only for the global layer", e.Key)
	}
	if !json.Valid(e.Value) {
		return fmt.Errorf("store: put setting %s: value is not JSON", e.Key)
	}
	_, err := r.exec(ctx, `INSERT INTO settings (scope, scope_id, plugin_id, setting_key, value, updated_at) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (scope, scope_id, plugin_id, setting_key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		name, e.ScopeID, e.PluginID, e.Key, string(e.Value), time.Now().UnixMilli())
	return wrap("put setting", err)
}

func (s *db) DeleteSetting(ctx context.Context, scope plugin.Scope, scopeID, pluginID, key string) error {
	return s.write(ctx, func(r run) error {
		res, err := r.exec(ctx, `DELETE FROM settings WHERE scope = ? AND scope_id = ? AND plugin_id = ? AND setting_key = ?`,
			settings.ScopeName(scope), scopeID, pluginID, key)
		return mustAffect("delete setting", res, err)
	})
}

func (s *db) ListSettings(ctx context.Context, f SettingFilter) ([]SettingRow, error) {
	return listSettings(ctx, s.r(), f)
}

func listSettings(ctx context.Context, r run, f SettingFilter) ([]SettingRow, error) {
	scope := settings.ScopeName(f.Scope)
	return list(ctx, r, "list settings", `SELECT scope, scope_id, plugin_id, setting_key, value FROM settings
WHERE (? = '' OR scope = ?) AND (? = '' OR scope_id = ?) AND (? = '' OR plugin_id = ?)
ORDER BY plugin_id, setting_key, scope, scope_id`, scanSetting, scope, scope, f.ScopeID, f.ScopeID, f.PluginID, f.PluginID)
}

// ---------------------------------------------------------------- manifests

func scanManifest(sc interface{ Scan(...any) error }) (*Manifest, error) {
	var m Manifest
	var updated int64
	if err := sc.Scan(&m.ID, &m.YAML, &m.Source, &updated); err != nil {
		return nil, err
	}
	m.UpdatedAt = fromMs(updated)
	return &m, nil
}

func (s *db) PutManifest(ctx context.Context, m *Manifest) error {
	return s.write(ctx, func(r run) error { return putManifest(ctx, r, m) })
}

func putManifest(ctx context.Context, r run, m *Manifest) error {
	if m.ID == "" {
		return fmt.Errorf("store: put manifest: empty id")
	}
	if m.Source == "" {
		m.Source = SourceCustom
	}
	_, err := r.exec(ctx, `INSERT INTO manifests (id, yaml, source, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET yaml = excluded.yaml, source = excluded.source, updated_at = excluded.updated_at`,
		m.ID, m.YAML, m.Source, time.Now().UnixMilli())
	return wrap("put manifest", err)
}

func (s *db) GetManifest(ctx context.Context, id string) (*Manifest, error) {
	m, err := scanManifest(s.r().row(ctx, `SELECT id, yaml, source, updated_at FROM manifests WHERE id = ?`, id))
	return m, wrap("get manifest", err)
}

func (s *db) ListManifests(ctx context.Context) ([]Manifest, error) { return listManifests(ctx, s.r()) }

func listManifests(ctx context.Context, r run) ([]Manifest, error) {
	return list(ctx, r, "list manifests", `SELECT id, yaml, source, updated_at FROM manifests ORDER BY id`, scanManifest)
}

func (s *db) DeleteManifest(ctx context.Context, id string) error {
	return s.write(ctx, func(r run) error {
		res, err := r.exec(ctx, `DELETE FROM manifests WHERE id = ?`, id)
		return mustAffect("delete manifest", res, err)
	})
}

// ---------------------------------------------------------------- audit

func (s *db) AddAudit(ctx context.Context, a *Audit) error {
	if a.ID == "" {
		a.ID = newID()
	}
	if a.Time.IsZero() {
		a.Time = time.Now().UTC().Truncate(time.Millisecond)
	}
	diff := "null"
	if len(a.Diff) > 0 {
		if !json.Valid(a.Diff) {
			return fmt.Errorf("store: add audit: diff is not JSON")
		}
		diff = string(a.Diff)
	}
	_, err := s.r().exec(ctx, `INSERT INTO audit_log (id, time, actor, action, target, diff) VALUES (?, ?, ?, ?, ?, ?)`,
		a.ID, ms(a.Time), a.Actor, a.Action, a.Target, diff)
	return wrap("add audit", err)
}

func (s *db) ListAudit(ctx context.Context, q AuditQuery) ([]Audit, error) {
	before := q.Before
	if before.IsZero() {
		before = time.Now().Add(time.Millisecond)
	}
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	return list(ctx, s.r(), "list audit", `SELECT id, time, actor, action, target, diff FROM audit_log
WHERE time < ? AND (? = '' OR actor = ?) AND (? = '' OR target = ?) ORDER BY time DESC, id DESC LIMIT ?`,
		func(sc interface{ Scan(...any) error }) (*Audit, error) {
			var a Audit
			var t int64
			var diff string
			if err := sc.Scan(&a.ID, &t, &a.Actor, &a.Action, &a.Target, &diff); err != nil {
				return nil, err
			}
			a.Time = fromMs(t)
			if diff != "null" {
				a.Diff = json.RawMessage(diff)
			}
			return &a, nil
		}, ms(before), q.Actor, q.Actor, q.Target, q.Target, limit)
}

// ---------------------------------------------------------------- plugin KV

func (s *db) KV(pluginID string) plugin.KV { return kv{s: s, id: pluginID} }

type kv struct {
	s  *db
	id string
}

func (k kv) Get(ctx context.Context, key string) ([]byte, bool, error) {
	var v []byte
	var n sql.NullInt64
	err := k.s.r().row(ctx, `SELECT value, num FROM plugin_kv WHERE plugin_id = ? AND k = ? AND (expires_at = 0 OR expires_at > ?)`,
		k.id, key, time.Now().UnixMilli()).Scan(&v, &n)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, wrap("kv get", err)
	}
	if v == nil && n.Valid {
		v = strconv.AppendInt(nil, n.Int64, 10)
	}
	if v == nil {
		v = []byte{}
	}
	return v, true, nil
}

func kvExpiry(ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	return time.Now().Add(ttl).UnixMilli()
}

func (k kv) Put(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if value == nil {
		value = []byte{}
	}
	var num sql.NullInt64
	if n, err := strconv.ParseInt(string(value), 10, 64); err == nil {
		num = sql.NullInt64{Int64: n, Valid: true}
	}
	_, err := k.s.r().exec(ctx, `INSERT INTO plugin_kv (plugin_id, k, value, num, expires_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (plugin_id, k) DO UPDATE SET value = excluded.value, num = excluded.num, expires_at = excluded.expires_at`,
		k.id, key, value, num, kvExpiry(ttl))
	return wrap("kv put", err)
}

func (k kv) Delete(ctx context.Context, key string) error {
	_, err := k.s.r().exec(ctx, `DELETE FROM plugin_kv WHERE plugin_id = ? AND k = ?`, k.id, key)
	return wrap("kv delete", err)
}

// Incr is a single upsert, so concurrent increments never lose updates;
// the read back runs in the same transaction, under the upsert's row lock.
// num is assigned before expires_at, which its expiry test reads.
func (k kv) Incr(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	now := time.Now().UnixMilli()
	var n int64
	err := k.s.tx(ctx, func(r run) error {
		_, err := r.exec(ctx, `INSERT INTO plugin_kv (plugin_id, k, value, num, expires_at) VALUES (?, ?, NULL, ?, ?)
ON CONFLICT (plugin_id, k) DO UPDATE SET
  num = CASE WHEN plugin_kv.expires_at > 0 AND plugin_kv.expires_at <= ? THEN excluded.num ELSE COALESCE(plugin_kv.num, 0) + excluded.num END,
  expires_at = CASE WHEN plugin_kv.expires_at > 0 AND plugin_kv.expires_at <= ? THEN excluded.expires_at ELSE plugin_kv.expires_at END,
  value = NULL`, k.id, key, delta, kvExpiry(ttl), now, now)
		if err != nil {
			return err
		}
		return r.row(ctx, `SELECT num FROM plugin_kv WHERE plugin_id = ? AND k = ?`, k.id, key).Scan(&n)
	})
	if err != nil {
		return 0, wrap("kv incr", err)
	}
	return n, nil
}

func (s *db) PurgeExpired(ctx context.Context) error {
	now := time.Now().UnixMilli()
	if _, err := s.r().exec(ctx, `DELETE FROM admin_sessions WHERE expires_at <= ?`, now); err != nil {
		return wrap("purge sessions", err)
	}
	_, err := s.r().exec(ctx, `DELETE FROM plugin_kv WHERE expires_at > 0 AND expires_at <= ?`, now)
	return wrap("purge kv", err)
}
