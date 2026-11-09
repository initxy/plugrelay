package store

import (
	"context"
	"fmt"
	"time"
)

const upstreamCols = `id, provider, manifest, endpoints, config, enabled, source, created_at, updated_at`

func scanUpstream(sc interface{ Scan(...any) error }) (*Upstream, error) {
	var u Upstream
	var eps, cfg string
	var created, updated int64
	if err := sc.Scan(&u.ID, &u.Provider, &u.Manifest, &eps, &cfg, &u.Enabled, &u.Source, &created, &updated); err != nil {
		return nil, err
	}
	u.CreatedAt, u.UpdatedAt = fromMs(created), fromMs(updated)
	if err := unjs(eps, &u.Endpoints); err != nil {
		return nil, err
	}
	return &u, unjs(cfg, &u.Config)
}

func (s *db) PutUpstream(ctx context.Context, u *Upstream) error {
	return s.write(ctx, func(r run) error { return putUpstream(ctx, r, u) })
}

func putUpstream(ctx context.Context, r run, u *Upstream) error {
	if u.ID == "" {
		return fmt.Errorf("store: put upstream: empty id")
	}
	if u.Provider == "" {
		return fmt.Errorf("store: put upstream %s: empty provider", u.ID)
	}
	if u.Source == "" {
		u.Source = SourceCustom
	}
	now := time.Now().UnixMilli()
	_, err := r.exec(ctx, `INSERT INTO upstreams (`+upstreamCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET provider = excluded.provider, manifest = excluded.manifest, endpoints = excluded.endpoints,
  config = excluded.config, enabled = excluded.enabled, source = excluded.source, updated_at = excluded.updated_at`,
		u.ID, u.Provider, u.Manifest, js(u.Endpoints, "[]"), js(u.Config, "{}"), u.Enabled, u.Source, now, now)
	return wrap("put upstream", err)
}

func (s *db) GetUpstream(ctx context.Context, id string) (*Upstream, error) {
	u, err := scanUpstream(s.r().row(ctx, `SELECT `+upstreamCols+` FROM upstreams WHERE id = ?`, id))
	return u, wrap("get upstream", err)
}

func (s *db) ListUpstreams(ctx context.Context) ([]Upstream, error) { return listUpstreams(ctx, s.r()) }

func listUpstreams(ctx context.Context, r run) ([]Upstream, error) {
	return list(ctx, r, "list upstreams", `SELECT `+upstreamCols+` FROM upstreams ORDER BY id`, scanUpstream)
}

func (s *db) DeleteUpstream(ctx context.Context, id string) error {
	return s.write(ctx, func(r run) error {
		res, err := r.exec(ctx, `DELETE FROM upstreams WHERE id = ?`, id)
		if err := mustAffect("delete upstream", res, err); err != nil {
			return err
		}
		if _, err := r.exec(ctx, `DELETE FROM credentials WHERE upstream_id = ?`, id); err != nil {
			return wrap("delete upstream credentials", err)
		}
		_, err = r.exec(ctx, `DELETE FROM routes WHERE upstream_id = ?`, id)
		return wrap("delete upstream routes", err)
	})
}

// ---------------------------------------------------------------- credentials

const credCols = `id, upstream_id, kind, owner_id, secret, proxy, max_concurrency, status, status_reason, status_until, meta, expires_at, created_at, updated_at`

func scanCred(sc interface{ Scan(...any) error }) (*Credential, error) {
	var c Credential
	var meta string
	var until, exp, created, updated int64
	if err := sc.Scan(&c.ID, &c.UpstreamID, &c.Kind, &c.OwnerID, &c.Sealed, &c.Proxy, &c.MaxConcurrency, &c.Status, &c.StatusReason, &until, &meta, &exp, &created, &updated); err != nil {
		return nil, err
	}
	c.StatusUntil, c.ExpiresAt, c.CreatedAt, c.UpdatedAt = fromMs(until), fromMs(exp), fromMs(created), fromMs(updated)
	return &c, unjs(meta, &c.Meta)
}

func (s *db) seal(id, plaintext string) (string, error) {
	if s.box == nil {
		return "", ErrNoBox
	}
	sealed, err := s.box.Seal([]byte(plaintext), CredentialAD(id))
	if err != nil {
		return "", fmt.Errorf("store: seal credential %s: %w", id, err)
	}
	return sealed, nil
}

func (s *db) PutCredential(ctx context.Context, c *Credential) error {
	return s.write(ctx, func(r run) error { return s.putCredential(ctx, r, c) })
}

func (s *db) putCredential(ctx context.Context, r run, c *Credential) error {
	if c.UpstreamID == "" {
		return fmt.Errorf("store: put credential: empty upstream id")
	}
	switch c.Kind {
	case CredShared, CredService:
	case CredPersonal:
		if c.OwnerID == "" {
			return fmt.Errorf("store: put credential: a personal credential needs an owner")
		}
	default:
		return fmt.Errorf("store: put credential: invalid kind %q", c.Kind)
	}
	if c.ID == "" {
		c.ID = newID()
	}
	if c.Status == "" {
		c.Status = CredActive
	}
	if c.Secret != "" {
		sealed, err := s.seal(c.ID, c.Secret)
		if err != nil {
			return err
		}
		c.Sealed, c.Secret = sealed, ""
	} else {
		// Keep the stored secret.
		var cur string
		err := r.row(ctx, `SELECT secret FROM credentials WHERE id = ?`, c.ID).Scan(&cur)
		if err != nil && !isNotFound(err) {
			return wrap("put credential", err)
		}
		c.Sealed = cur
	}
	now := time.Now().UnixMilli()
	_, err := r.exec(ctx, `INSERT INTO credentials (`+credCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET upstream_id = excluded.upstream_id, kind = excluded.kind, owner_id = excluded.owner_id,
  secret = excluded.secret, proxy = excluded.proxy, max_concurrency = excluded.max_concurrency, status = excluded.status,
  status_reason = excluded.status_reason, status_until = excluded.status_until, meta = excluded.meta,
  expires_at = excluded.expires_at, updated_at = excluded.updated_at`,
		c.ID, c.UpstreamID, c.Kind, c.OwnerID, c.Sealed, c.Proxy, c.MaxConcurrency, c.Status, c.StatusReason, ms(c.StatusUntil),
		js(c.Meta, "{}"), ms(c.ExpiresAt), now, now)
	return wrap("put credential", err)
}

func (s *db) GetCredential(ctx context.Context, id string) (*Credential, error) {
	c, err := scanCred(s.r().row(ctx, `SELECT `+credCols+` FROM credentials WHERE id = ?`, id))
	return c, wrap("get credential", err)
}

func (s *db) ListCredentials(ctx context.Context, upstreamID string) ([]Credential, error) {
	return listCredentials(ctx, s.r(), upstreamID)
}

func listCredentials(ctx context.Context, r run, upstreamID string) ([]Credential, error) {
	return list(ctx, r, "list credentials", `SELECT `+credCols+` FROM credentials WHERE (? = '' OR upstream_id = ?) ORDER BY upstream_id, id`,
		scanCred, upstreamID, upstreamID)
}

func (s *db) DeleteCredential(ctx context.Context, id string) error {
	return s.write(ctx, func(r run) error {
		res, err := r.exec(ctx, `DELETE FROM credentials WHERE id = ?`, id)
		return mustAffect("delete credential", res, err)
	})
}

func (s *db) SetCredentialStatus(ctx context.Context, id string, status CredentialStatus, reason string, until time.Time) error {
	return s.write(ctx, func(r run) error {
		res, err := r.exec(ctx, `UPDATE credentials SET status = ?, status_reason = ?, status_until = ?, updated_at = ? WHERE id = ?`,
			status, reason, ms(until), time.Now().UnixMilli(), id)
		return mustAffect("set credential status", res, err)
	})
}

func (s *db) SetCredentialSecret(ctx context.Context, id, plaintext string, expiresAt time.Time) error {
	sealed, err := s.seal(id, plaintext)
	if err != nil {
		return err
	}
	return s.write(ctx, func(r run) error {
		res, err := r.exec(ctx, `UPDATE credentials SET secret = ?, expires_at = ?, updated_at = ? WHERE id = ?`,
			sealed, ms(expiresAt), time.Now().UnixMilli(), id)
		return mustAffect("set credential secret", res, err)
	})
}
