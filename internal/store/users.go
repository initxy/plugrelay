package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/initxy/plugrelay/internal/secret"
)

// dummyHash keeps Authenticate's timing the same for unknown emails.
const dummyHash = "$2a$10$GhvpdkbCvM9XhJt6FfDRwOKusIwaOTsc//PsWlqUZwutlZfCTpoNW"

const userCols = `id, name, email, password_hash, role, group_id, disabled, created_at, updated_at, email_unverified`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created, updated int64
	if err := sc.Scan(&u.ID, &u.Name, &u.Email, &u.passwordHash, &u.Role, &u.GroupID, &u.Disabled, &created, &updated, &u.EmailUnverified); err != nil {
		return nil, err
	}
	u.CreatedAt, u.UpdatedAt = fromMs(created), fromMs(updated)
	return &u, nil
}

func validRole(r Role) bool { return r == RoleAdmin || r == RoleMember || r == RoleUser }

func (s *db) CreateUser(ctx context.Context, u *User, password string) error {
	if !validRole(u.Role) {
		return fmt.Errorf("store: create user: invalid role %q", u.Role)
	}
	if u.ID == "" {
		u.ID = newID()
	}
	hash := ""
	if password != "" {
		var err error
		if hash, err = secret.HashPassword(password); err != nil {
			return err
		}
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	u.CreatedAt, u.UpdatedAt, u.passwordHash = now, now, hash
	_, err := s.r().exec(ctx, `INSERT INTO users (`+userCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Name, u.Email, hash, u.Role, u.GroupID, u.Disabled, ms(now), ms(now), u.EmailUnverified)
	return wrap("create user", err)
}

func (s *db) UpdateUser(ctx context.Context, u *User) error {
	if !validRole(u.Role) {
		return fmt.Errorf("store: update user: invalid role %q", u.Role)
	}
	u.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	res, err := s.r().exec(ctx, `UPDATE users SET name = ?, email = ?, role = ?, group_id = ?, disabled = ?, updated_at = ?, email_unverified = ? WHERE id = ?`,
		u.Name, u.Email, u.Role, u.GroupID, u.Disabled, ms(u.UpdatedAt), u.EmailUnverified, u.ID)
	return mustAffect("update user", res, err)
}

func (s *db) SetPassword(ctx context.Context, userID, password string) error {
	hash, err := secret.HashPassword(password)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(r run) error {
		res, err := r.exec(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, hash, time.Now().UnixMilli(), userID)
		if err := mustAffect("set password", res, err); err != nil {
			return err
		}
		// A password change ends every console session of the user.
		_, err = r.exec(ctx, `DELETE FROM admin_sessions WHERE user_id = ?`, userID)
		return wrap("set password", err)
	})
}

func (s *db) GetUser(ctx context.Context, id string) (*User, error) {
	u, err := scanUser(s.r().row(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	return u, wrap("get user", err)
}

func (s *db) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.r().query(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at, id`)
	if err != nil {
		return nil, wrap("list users", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, wrap("list users", err)
		}
		out = append(out, *u)
	}
	return out, wrap("list users", rows.Err())
}

func (s *db) DeleteUser(ctx context.Context, id string) error {
	return s.tx(ctx, func(r run) error {
		res, err := r.exec(ctx, `DELETE FROM users WHERE id = ?`, id)
		if err := mustAffect("delete user", res, err); err != nil {
			return err
		}
		// The user's live subscriptions end with the user (no group to
		// restore); a provider billing one is the caller's to stop.
		for _, st := range []string{SubPending, SubActive} {
			ids, err := liveIDs(ctx, r, id, st, "")
			if err != nil {
				return wrap("delete user", err)
			}
			for _, sid := range ids {
				if _, err := r.exec(ctx, `UPDATE subscriptions SET status = ?, updated_at = ? WHERE id = ?`, SubEnded, time.Now().UnixMilli(), sid); err != nil {
					return wrap("delete user", err)
				}
			}
		}
		if _, err = r.exec(ctx, `DELETE FROM admin_sessions WHERE user_id = ?`, id); err != nil {
			return wrap("delete user", err)
		}
		if _, err = r.exec(ctx, `DELETE FROM user_identities WHERE user_id = ?`, id); err != nil {
			return wrap("delete user", err)
		}
		return resetMFA(ctx, r, id)
	})
}

func (s *db) Authenticate(ctx context.Context, email, password string) (*User, error) {
	u, err := scanUser(s.r().row(ctx, `SELECT `+userCols+` FROM users WHERE email = ? AND email <> ''`, email))
	if errors.Is(err, sql.ErrNoRows) {
		_ = secret.CheckPassword(dummyHash, password)
		return nil, ErrInvalidLogin
	}
	if err != nil {
		return nil, wrap("authenticate", err)
	}
	if err := secret.CheckPassword(u.passwordHash, password); err != nil || u.Disabled {
		if err != nil && !errors.Is(err, secret.ErrPasswordMismatch) {
			return nil, err
		}
		return nil, ErrInvalidLogin
	}
	return u, nil
}

// HasPassword reports whether the user can sign in with a password.
func (u *User) HasPassword() bool { return u.passwordHash != "" }

// CheckPassword reports whether password is the user's; the user must
// have been read from the store.
func (u *User) CheckPassword(password string) bool {
	return u.passwordHash != "" && secret.CheckPassword(u.passwordHash, password) == nil
}

func (s *db) CreateSession(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	tok, err := secret.GenerateToken()
	if err != nil {
		return "", err
	}
	now := time.Now()
	_, err = s.r().exec(ctx, `INSERT INTO admin_sessions (token_hash, user_id, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		secret.HashKey(tok), userID, ms(now.Add(ttl)), ms(now))
	if err != nil {
		return "", wrap("create session", err)
	}
	return tok, nil
}

func (s *db) SessionUser(ctx context.Context, token string) (*User, error) {
	u, err := scanUser(s.r().row(ctx, `SELECT u.id, u.name, u.email, u.password_hash, u.role, u.group_id, u.disabled, u.created_at, u.updated_at, u.email_unverified
FROM admin_sessions s JOIN users u ON u.id = s.user_id
WHERE s.token_hash = ? AND s.expires_at > ? AND NOT u.disabled`, secret.HashKey(token), time.Now().UnixMilli()))
	return u, wrap("session", err)
}

func (s *db) DeleteSession(ctx context.Context, token string) error {
	_, err := s.r().exec(ctx, `DELETE FROM admin_sessions WHERE token_hash = ?`, secret.HashKey(token))
	return wrap("delete session", err)
}

// ---------------------------------------------------------------- groups

const groupCols = `id, name, settings, price_multiplier, created_at, updated_at`

func scanGroup(sc interface{ Scan(...any) error }) (*Group, error) {
	var g Group
	var set string
	var created, updated int64
	if err := sc.Scan(&g.ID, &g.Name, &set, &g.PriceMultiplier, &created, &updated); err != nil {
		return nil, err
	}
	g.CreatedAt, g.UpdatedAt = fromMs(created), fromMs(updated)
	return &g, unjs(set, &g.Settings)
}

func (s *db) PutGroup(ctx context.Context, g *Group) error {
	return s.write(ctx, func(r run) error { return putGroup(ctx, r, g) })
}

func putGroup(ctx context.Context, r run, g *Group) error {
	if g.ID == "" {
		g.ID = newID()
	}
	if g.PriceMultiplier == 0 {
		g.PriceMultiplier = 1
	}
	now := time.Now().UnixMilli()
	_, err := r.exec(ctx, `INSERT INTO user_groups (`+groupCols+`) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET name = excluded.name, settings = excluded.settings, price_multiplier = excluded.price_multiplier, updated_at = excluded.updated_at`,
		g.ID, g.Name, js(g.Settings, "{}"), g.PriceMultiplier, now, now)
	return wrap("put group", err)
}

func (s *db) GetGroup(ctx context.Context, id string) (*Group, error) {
	g, err := scanGroup(s.r().row(ctx, `SELECT `+groupCols+` FROM user_groups WHERE id = ?`, id))
	return g, wrap("get group", err)
}

func (s *db) ListGroups(ctx context.Context) ([]Group, error) { return listGroups(ctx, s.r()) }

func listGroups(ctx context.Context, r run) ([]Group, error) {
	return list(ctx, r, "list groups", `SELECT `+groupCols+` FROM user_groups ORDER BY id`, scanGroup)
}

func (s *db) DeleteGroup(ctx context.Context, id string) error {
	return s.write(ctx, func(r run) error {
		res, err := r.exec(ctx, `DELETE FROM user_groups WHERE id = ?`, id)
		return mustAffect("delete group", res, err)
	})
}

// list runs a query and scans every row.
func list[T any](ctx context.Context, r run, op, q string, scan func(interface{ Scan(...any) error }) (*T, error), args ...any) ([]T, error) {
	rows, err := r.query(ctx, q, args...)
	if err != nil {
		return nil, wrap(op, err)
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, wrap(op, err)
		}
		out = append(out, *v)
	}
	return out, wrap(op, rows.Err())
}
