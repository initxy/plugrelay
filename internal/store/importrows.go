package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RowImporter writes users and keys carried over from another relay with
// their credentials already hashed. The SQL store implements it; get it
// with RowImporterOf.
type RowImporter interface {
	// ImportUser inserts u with a bcrypt passwordHash ("" = no password)
	// unless a user with u.ID exists; created reports which.
	ImportUser(ctx context.Context, u *User, passwordHash string) (created bool, err error)
	// ImportKey inserts k with its precomputed Hash and Last4 unless a key
	// with k.ID exists.
	ImportKey(ctx context.Context, k *Key) (created bool, err error)
}

// RowImporterOf returns the store's RowImporter.
func RowImporterOf(s Store) (RowImporter, bool) {
	ri, ok := s.(RowImporter)
	return ri, ok
}

var _ RowImporter = (*db)(nil)

func (s *db) ImportUser(ctx context.Context, u *User, passwordHash string) (bool, error) {
	if !validRole(u.Role) || u.ID == "" {
		return false, fmt.Errorf("store: import user: id and a valid role are required")
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	res, err := s.r().exec(ctx, `INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		u.ID, u.Name, u.Email, passwordHash, u.Role, u.GroupID, u.Disabled, ms(u.CreatedAt), ms(now), u.EmailUnverified)
	if err != nil {
		return false, wrap("import user", err)
	}
	return s.imported(ctx, res, "users", u.ID, "import user")
}

// imported reads an import's outcome. Zero rows is a row with that id
// already there, unless no such row exists: then another unique key
// (an email, a key hash) was taken, which MySQL reports as a duplicate
// like the id and the other databases as an error.
func (s *db) imported(ctx context.Context, res sql.Result, table, id, op string) (bool, error) {
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	var exists int
	if err := s.r().row(ctx, `SELECT COUNT(*) FROM `+table+` WHERE id = ?`, id).Scan(&exists); err != nil {
		return false, wrap(op, err)
	}
	if exists == 0 {
		return false, fmt.Errorf("store: %s: %w: another unique field of %s is taken", op, ErrConflict, id)
	}
	return false, nil
}

func (s *db) ImportKey(ctx context.Context, k *Key) (bool, error) {
	if k.ID == "" || k.Hash == "" {
		return false, fmt.Errorf("store: import key: id and hash are required")
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC().Truncate(time.Millisecond)
	}
	res, err := s.r().exec(ctx, `INSERT INTO api_keys (`+keyCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		k.ID, k.Name, k.Hash, k.Last4, k.UserID, k.GroupID, js(k.Settings, "{}"), js(k.Exceptions, "[]"), k.Disabled,
		ms(k.ExpiresAt), ms(k.CreatedAt), ms(k.LastUsedAt))
	if err != nil {
		return false, wrap("import key", err)
	}
	return s.imported(ctx, res, "api_keys", k.ID, "import key")
}
