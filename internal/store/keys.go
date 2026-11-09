package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/initxy/plugrelay/internal/secret"
)

const keyCols = `id, name, hash, last4, user_id, group_id, settings, exceptions, disabled, expires_at, created_at, last_used_at`

func scanKey(sc interface{ Scan(...any) error }) (*Key, error) {
	var k Key
	var set, exc string
	var exp, created, used int64
	if err := sc.Scan(&k.ID, &k.Name, &k.Hash, &k.Last4, &k.UserID, &k.GroupID, &set, &exc, &k.Disabled, &exp, &created, &used); err != nil {
		return nil, err
	}
	k.ExpiresAt, k.CreatedAt, k.LastUsedAt = fromMs(exp), fromMs(created), fromMs(used)
	if err := unjs(set, &k.Settings); err != nil {
		return nil, err
	}
	return &k, unjs(exc, &k.Exceptions)
}

func (s *db) CreateKey(ctx context.Context, k *Key) (string, error) {
	plain, err := secret.GenerateKey()
	if err != nil {
		return "", err
	}
	if k.ID == "" {
		k.ID = newID()
	}
	k.Hash, k.Last4 = secret.HashKey(plain), secret.Last4(plain)
	k.CreatedAt, k.LastUsedAt = time.Now().UTC().Truncate(time.Millisecond), time.Time{}
	_, err = s.r().exec(ctx, `INSERT INTO api_keys (`+keyCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.Name, k.Hash, k.Last4, k.UserID, k.GroupID, js(k.Settings, "{}"), js(k.Exceptions, "[]"), k.Disabled, ms(k.ExpiresAt), ms(k.CreatedAt), 0)
	if err != nil {
		return "", wrap("create key", err)
	}
	return plain, nil
}

func (s *db) UpdateKey(ctx context.Context, k *Key) error {
	res, err := s.r().exec(ctx, `UPDATE api_keys SET name = ?, user_id = ?, group_id = ?, settings = ?, exceptions = ?, disabled = ?, expires_at = ? WHERE id = ?`,
		k.Name, k.UserID, k.GroupID, js(k.Settings, "{}"), js(k.Exceptions, "[]"), k.Disabled, ms(k.ExpiresAt), k.ID)
	return mustAffect("update key", res, err)
}

func (s *db) GetKey(ctx context.Context, id string) (*Key, error) {
	k, err := scanKey(s.r().row(ctx, `SELECT `+keyCols+` FROM api_keys WHERE id = ?`, id))
	return k, wrap("get key", err)
}

func (s *db) KeyByToken(ctx context.Context, plaintext string) (*Key, error) {
	if !secret.ValidKeyFormat(plaintext) && !secret.ValidImportedKeyFormat(plaintext) {
		return nil, fmt.Errorf("store: key by token: %w", ErrNotFound)
	}
	h := secret.HashKey(plaintext)
	k, err := scanKey(s.r().row(ctx, `SELECT `+keyCols+` FROM api_keys WHERE hash = ?`, h))
	if err != nil {
		return nil, wrap("key by token", err)
	}
	if !secret.EqualHash(k.Hash, h) {
		return nil, fmt.Errorf("store: key by token: %w", ErrNotFound)
	}
	return k, nil
}

func (s *db) ListKeys(ctx context.Context, f KeyFilter) ([]Key, error) {
	return list(ctx, s.r(), "list keys", `SELECT `+keyCols+` FROM api_keys
WHERE (? = '' OR user_id = ?) AND (? = '' OR group_id = ?) ORDER BY created_at, id`, scanKey, f.UserID, f.UserID, f.GroupID, f.GroupID)
}

func (s *db) DeleteKey(ctx context.Context, id string) error {
	res, err := s.r().exec(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	return mustAffect("delete key", res, err)
}

func (s *db) TouchKey(id string, at time.Time) {
	s.touchMu.Lock()
	if at.After(s.touches[id]) {
		s.touches[id] = at
	}
	s.touchMu.Unlock()
}

func (s *db) FlushTouches(ctx context.Context) error {
	s.touchMu.Lock()
	batch := s.touches
	s.touches = map[string]time.Time{}
	s.touchMu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	// Keys are updated in id order, the order every transaction that
	// updates several keys takes their locks in.
	ids := make([]string, 0, len(batch))
	for id := range batch {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	err := s.tx(ctx, func(r run) error {
		for _, id := range ids {
			at := batch[id]
			if _, err := r.exec(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ? AND last_used_at < ?`, ms(at), id, ms(at)); err != nil {
				return wrap("touch keys", err)
			}
		}
		return nil
	})
	if err != nil {
		// Keep the batch for the next flush unless newer touches arrived.
		s.touchMu.Lock()
		for id, at := range batch {
			if at.After(s.touches[id]) {
				s.touches[id] = at
			}
		}
		s.touchMu.Unlock()
	}
	return err
}

func (s *db) flusher(every time.Duration) {
	defer close(s.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), every)
			_ = s.FlushTouches(ctx)
			cancel()
		}
	}
}
