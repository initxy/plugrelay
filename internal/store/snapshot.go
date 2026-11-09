package store

import (
	"context"
	"database/sql"
	"fmt"
)

// readTx runs fn in a transaction that sees one consistent state.
func (s *db) readTx(ctx context.Context, fn func(r run) error) error {
	var opts *sql.TxOptions
	if s.d != sqliteDialect {
		opts = &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	}
	tx, err := s.sql.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("store: begin read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(run{q: tx, d: s.d})
}

func (s *db) Snapshot(ctx context.Context) (*Snapshot, error) {
	snap := &Snapshot{}
	err := s.readTx(ctx, func(r run) error {
		if err := r.row(ctx, `SELECT v FROM meta WHERE k = 'version'`).Scan(&snap.Version); err != nil {
			return wrap("snapshot version", err)
		}
		var err error
		if snap.Groups, err = listGroups(ctx, r); err != nil {
			return err
		}
		if snap.Upstreams, err = listUpstreams(ctx, r); err != nil {
			return err
		}
		if snap.Credentials, err = listCredentials(ctx, r, ""); err != nil {
			return err
		}
		if snap.Models, err = listModels(ctx, r); err != nil {
			return err
		}
		if snap.Presets, err = listPresets(ctx, r); err != nil {
			return err
		}
		if snap.Routes, err = listRoutes(ctx, r, ""); err != nil {
			return err
		}
		if snap.Prices, err = listPrices(ctx, r); err != nil {
			return err
		}
		if snap.Plugins, err = listPluginConfigs(ctx, r); err != nil {
			return err
		}
		if snap.Features, err = listFeatures(ctx, r); err != nil {
			return err
		}
		if snap.Settings, err = listSettings(ctx, r, SettingFilter{}); err != nil {
			return err
		}
		snap.Manifests, err = listManifests(ctx, r)
		return err
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}
