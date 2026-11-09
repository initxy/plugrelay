package store

import (
	"context"
	"time"
)

// Passkey is a WebAuthn credential registered by a console user.
type Passkey struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`
	// CredentialID is the authenticator's credential id, base64url.
	CredentialID string `json:"credential_id"`
	// PublicKey is the COSE public key as registered, base64url.
	PublicKey  string    `json:"-"`
	Alg        int64     `json:"alg"`
	SignCount  uint32    `json:"sign_count"`
	Name       string    `json:"name"`
	Transports []string  `json:"transports,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitzero"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
}

// MFA stores second sign-in factors. The SQL store implements it; get it
// with MFAOf.
type MFA interface {
	// EnableTOTP stores a verified TOTP secret (sealed under the master
	// key), marks step as used and replaces the recovery codes, in one
	// transaction.
	EnableTOTP(ctx context.Context, userID string, secret []byte, step int64, codeHashes []string) error
	// TOTPSecret opens a user's TOTP secret; ErrNotFound when TOTP is off.
	TOTPSecret(ctx context.Context, userID string) ([]byte, error)
	// UseTOTPStep records a time step as used; only the first call for a
	// step newer than every earlier one reports true.
	UseTOTPStep(ctx context.Context, userID string, step int64) (bool, error)
	DeleteTOTP(ctx context.Context, userID string) error

	// ReplaceRecoveryCodes swaps every recovery code of a user.
	ReplaceRecoveryCodes(ctx context.Context, userID string, codeHashes []string) error
	// UseRecoveryCode deletes a code; true when it existed.
	UseRecoveryCode(ctx context.Context, userID, codeHash string) (bool, error)
	CountRecoveryCodes(ctx context.Context, userID string) (int, error)

	AddPasskey(ctx context.Context, p *Passkey) error
	ListPasskeys(ctx context.Context, userID string) ([]Passkey, error)
	PasskeyByCredential(ctx context.Context, credentialID string) (*Passkey, error)
	RenamePasskey(ctx context.Context, userID, id, name string) error
	DeletePasskey(ctx context.Context, userID, id string) error
	// UsePasskey stores a new signature counter; it reports false when the
	// counter did not grow (a cloned authenticator or a replay), except
	// for authenticators that keep it at zero.
	UsePasskey(ctx context.Context, id string, signCount uint32, at time.Time) (bool, error)

	// MFAUsers lists the users with TOTP or a passkey.
	MFAUsers(ctx context.Context) (map[string]bool, error)
	// ResetMFA removes every factor and recovery code of a user.
	ResetMFA(ctx context.Context, userID string) error
}

// MFAOf returns the store's second-factor storage.
func MFAOf(s Store) (MFA, bool) {
	m, ok := s.(MFA)
	return m, ok
}

var _ MFA = (*db)(nil)

func totpAD(userID string) string { return "totp:" + userID }

func (s *db) EnableTOTP(ctx context.Context, userID string, secret []byte, step int64, codeHashes []string) error {
	if s.box == nil {
		return ErrNoBox
	}
	sealed, err := s.box.Seal(secret, totpAD(userID))
	if err != nil {
		return err
	}
	return s.tx(ctx, func(r run) error {
		now := time.Now().UnixMilli()
		_, err := r.exec(ctx, `INSERT INTO user_totp (user_id, secret, last_step, created_at) VALUES (?, ?, ?, ?)
ON CONFLICT (user_id) DO UPDATE SET secret = excluded.secret, last_step = excluded.last_step, created_at = excluded.created_at`,
			userID, sealed, step, now)
		if err != nil {
			return wrap("enable totp", err)
		}
		return replaceCodes(ctx, r, userID, codeHashes)
	})
}

func (s *db) TOTPSecret(ctx context.Context, userID string) ([]byte, error) {
	var sealed string
	if err := s.r().row(ctx, `SELECT secret FROM user_totp WHERE user_id = ?`, userID).Scan(&sealed); err != nil {
		return nil, wrap("totp secret", err)
	}
	if s.box == nil {
		return nil, ErrNoBox
	}
	return s.box.Open(sealed, totpAD(userID))
}

func (s *db) UseTOTPStep(ctx context.Context, userID string, step int64) (bool, error) {
	res, err := s.r().exec(ctx, `UPDATE user_totp SET last_step = ? WHERE user_id = ? AND last_step < ?`, step, userID, step)
	return affected("use totp step", res, err)
}

func (s *db) DeleteTOTP(ctx context.Context, userID string) error {
	return s.tx(ctx, func(r run) error {
		res, err := r.exec(ctx, `DELETE FROM user_totp WHERE user_id = ?`, userID)
		if err := mustAffect("delete totp", res, err); err != nil {
			return err
		}
		// Recovery codes stay while a passkey remains.
		var n int
		if err := r.row(ctx, `SELECT COUNT(*) FROM user_passkeys WHERE user_id = ?`, userID).Scan(&n); err != nil {
			return wrap("delete totp", err)
		}
		if n == 0 {
			_, err = r.exec(ctx, `DELETE FROM user_recovery_codes WHERE user_id = ?`, userID)
		}
		return wrap("delete totp", err)
	})
}

func replaceCodes(ctx context.Context, r run, userID string, hashes []string) error {
	if _, err := r.exec(ctx, `DELETE FROM user_recovery_codes WHERE user_id = ?`, userID); err != nil {
		return wrap("recovery codes", err)
	}
	now := time.Now().UnixMilli()
	for _, h := range hashes {
		if _, err := r.exec(ctx, `INSERT INTO user_recovery_codes (user_id, code_hash, created_at) VALUES (?, ?, ?)`, userID, h, now); err != nil {
			return wrap("recovery codes", err)
		}
	}
	return nil
}

func (s *db) ReplaceRecoveryCodes(ctx context.Context, userID string, codeHashes []string) error {
	return s.tx(ctx, func(r run) error { return replaceCodes(ctx, r, userID, codeHashes) })
}

func (s *db) UseRecoveryCode(ctx context.Context, userID, codeHash string) (bool, error) {
	res, err := s.r().exec(ctx, `DELETE FROM user_recovery_codes WHERE user_id = ? AND code_hash = ?`, userID, codeHash)
	return affected("use recovery code", res, err)
}

func (s *db) CountRecoveryCodes(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.r().row(ctx, `SELECT COUNT(*) FROM user_recovery_codes WHERE user_id = ?`, userID).Scan(&n)
	return n, wrap("count recovery codes", err)
}

const passkeyCols = `id, user_id, credential_id, public_key, alg, sign_count, name, transports, created_at, last_used_at`

func scanPasskey(sc interface{ Scan(...any) error }) (*Passkey, error) {
	var p Passkey
	var transports string
	var count, created, used int64
	if err := sc.Scan(&p.ID, &p.UserID, &p.CredentialID, &p.PublicKey, &p.Alg, &count, &p.Name, &transports, &created, &used); err != nil {
		return nil, err
	}
	p.SignCount = uint32(count)
	if err := unjs(transports, &p.Transports); err != nil {
		return nil, err
	}
	p.CreatedAt, p.LastUsedAt = fromMs(created), fromMs(used)
	return &p, nil
}

func (s *db) AddPasskey(ctx context.Context, p *Passkey) error {
	if p.ID == "" {
		p.ID = newID()
	}
	p.CreatedAt = time.Now().UTC().Truncate(time.Millisecond)
	_, err := s.r().exec(ctx, `INSERT INTO user_passkeys (`+passkeyCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.UserID, p.CredentialID, p.PublicKey, p.Alg, int64(p.SignCount), p.Name, js(p.Transports, "[]"), ms(p.CreatedAt), ms(p.LastUsedAt))
	return wrap("add passkey", err)
}

func (s *db) ListPasskeys(ctx context.Context, userID string) ([]Passkey, error) {
	return list(ctx, s.r(), "list passkeys", `SELECT `+passkeyCols+` FROM user_passkeys WHERE user_id = ? ORDER BY created_at, id`, scanPasskey, userID)
}

func (s *db) PasskeyByCredential(ctx context.Context, credentialID string) (*Passkey, error) {
	p, err := scanPasskey(s.r().row(ctx, `SELECT `+passkeyCols+` FROM user_passkeys WHERE credential_id = ?`, credentialID))
	return p, wrap("passkey", err)
}

func (s *db) RenamePasskey(ctx context.Context, userID, id, name string) error {
	res, err := s.r().exec(ctx, `UPDATE user_passkeys SET name = ? WHERE id = ? AND user_id = ?`, name, id, userID)
	return mustAffect("rename passkey", res, err)
}

func (s *db) DeletePasskey(ctx context.Context, userID, id string) error {
	return s.tx(ctx, func(r run) error {
		res, err := r.exec(ctx, `DELETE FROM user_passkeys WHERE id = ? AND user_id = ?`, id, userID)
		if err := mustAffect("delete passkey", res, err); err != nil {
			return err
		}
		var n int
		if err := r.row(ctx, `SELECT (SELECT COUNT(*) FROM user_passkeys WHERE user_id = ?) + (SELECT COUNT(*) FROM user_totp WHERE user_id = ?)`,
			userID, userID).Scan(&n); err != nil {
			return wrap("delete passkey", err)
		}
		if n == 0 {
			_, err = r.exec(ctx, `DELETE FROM user_recovery_codes WHERE user_id = ?`, userID)
		}
		return wrap("delete passkey", err)
	})
}

func (s *db) UsePasskey(ctx context.Context, id string, signCount uint32, at time.Time) (bool, error) {
	n := int64(signCount)
	res, err := s.r().exec(ctx, `UPDATE user_passkeys SET sign_count = ?, last_used_at = ? WHERE id = ? AND (sign_count < ? OR (sign_count = 0 AND ? = 0))`,
		n, ms(at), id, n, n)
	return affected("use passkey", res, err)
}

func (s *db) MFAUsers(ctx context.Context) (map[string]bool, error) {
	rows, err := s.r().query(ctx, `SELECT user_id FROM user_totp UNION SELECT user_id FROM user_passkeys`)
	if err != nil {
		return nil, wrap("mfa users", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, wrap("mfa users", err)
		}
		out[id] = true
	}
	return out, wrap("mfa users", rows.Err())
}

func (s *db) ResetMFA(ctx context.Context, userID string) error {
	return s.tx(ctx, func(r run) error { return resetMFA(ctx, r, userID) })
}

func resetMFA(ctx context.Context, r run, userID string) error {
	for _, q := range []string{`DELETE FROM user_totp WHERE user_id = ?`, `DELETE FROM user_recovery_codes WHERE user_id = ?`,
		`DELETE FROM user_passkeys WHERE user_id = ?`} {
		if _, err := r.exec(ctx, q, userID); err != nil {
			return wrap("reset mfa", err)
		}
	}
	return nil
}

// affected reports whether a conditional write touched a row.
func affected(op string, res interface{ RowsAffected() (int64, error) }, err error) (bool, error) {
	if err != nil {
		return false, wrap(op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, wrap(op, err)
	}
	return n > 0, nil
}
