package store

import (
	"context"
	"strings"
	"time"
)

// Identity links an external sign-in account (an SSO provider's subject)
// to a console user.
type Identity struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Subject  string `json:"subject"`
	UserID   string `json:"user_id"`
	// Email is the address the provider reported at link time, for display.
	Email       string    `json:"email,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitzero"`
	LastLoginAt time.Time `json:"last_login_at,omitzero"`
}

const identityCols = `id, provider, subject, user_id, email, created_at, last_login_at`

func scanIdentity(sc interface{ Scan(...any) error }) (*Identity, error) {
	var i Identity
	var created, last int64
	if err := sc.Scan(&i.ID, &i.Provider, &i.Subject, &i.UserID, &i.Email, &created, &last); err != nil {
		return nil, err
	}
	i.CreatedAt, i.LastLoginAt = fromMs(created), fromMs(last)
	return &i, nil
}

func (s *db) LinkIdentity(ctx context.Context, i *Identity) error {
	if i.ID == "" {
		i.ID = newID()
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	i.CreatedAt = now
	_, err := s.r().exec(ctx, `INSERT INTO user_identities (`+identityCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.Provider, i.Subject, i.UserID, i.Email, ms(now), ms(i.LastLoginAt))
	return wrap("link identity", err)
}

func (s *db) GetIdentity(ctx context.Context, provider, subject string) (*Identity, error) {
	i, err := scanIdentity(s.r().row(ctx, `SELECT `+identityCols+` FROM user_identities WHERE provider = ? AND subject = ?`, provider, subject))
	return i, wrap("get identity", err)
}

func (s *db) ListIdentities(ctx context.Context, userID string) ([]Identity, error) {
	if userID == "" {
		return list(ctx, s.r(), "list identities", `SELECT `+identityCols+` FROM user_identities ORDER BY created_at, id`, scanIdentity)
	}
	return list(ctx, s.r(), "list identities", `SELECT `+identityCols+` FROM user_identities WHERE user_id = ? ORDER BY created_at, id`, scanIdentity, userID)
}

func (s *db) TouchIdentity(ctx context.Context, id string, at time.Time) error {
	res, err := s.r().exec(ctx, `UPDATE user_identities SET last_login_at = ? WHERE id = ?`, ms(at), id)
	return mustAffect("touch identity", res, err)
}

func (s *db) DeleteIdentity(ctx context.Context, id string) error {
	res, err := s.r().exec(ctx, `DELETE FROM user_identities WHERE id = ?`, id)
	return mustAffect("delete identity", res, err)
}

func (s *db) UserByEmail(ctx context.Context, email string) (*User, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, wrap("user by email", ErrNotFound)
	}
	u, err := scanUser(s.r().row(ctx, `SELECT `+userCols+` FROM users WHERE lower(email) = lower(?) AND email <> '' ORDER BY created_at LIMIT 1`, email))
	return u, wrap("user by email", err)
}
