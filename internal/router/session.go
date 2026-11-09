package router

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Binding ties a session to the candidate that holds its prompt cache.
type Binding struct {
	Key string `json:"k"` // candidate Key
	// SentSessionID is the session id actually sent upstream on that turn.
	SentSessionID string    `json:"s,omitempty"`
	Cached        bool      `json:"c"`
	ValidUntil    time.Time `json:"v"`
	At            time.Time `json:"a"`
}

// Valid reports whether the binding should still be honored: the last turn
// used the cache and the cache has not expired.
func (b Binding) Valid(now time.Time) bool { return b.Cached && now.Before(b.ValidUntil) }

// bindingKeep is how long a non-cached binding is kept for its SentSessionID.
const bindingKeep = time.Hour

// Turn is one finished upstream turn, for Bind and Observe.
type Turn struct {
	SessionKey    string
	Caller        plugin.Caller
	Candidate     plugin.Candidate
	SentSessionID string
	Usage         ir.Usage
	// CacheTTL overrides the binding lifetime; 0 picks the default TTL, or the
	// long TTL when the turn wrote the 1h cache.
	CacheTTL time.Duration
}

// Bind binds the session to the turn's candidate unconditionally for the
// cache TTL, and remembers the credential for the caller.
func (r *Router) Bind(ctx context.Context, t Turn) error {
	return r.store(ctx, t, true)
}

// Observe records a finished turn: the session stays bound only if the turn
// read or wrote the prompt cache; otherwise the next turn may be re-routed.
func (r *Router) Observe(ctx context.Context, t Turn) error {
	u := t.Usage
	return r.store(ctx, t, u.CacheRead+u.CacheWrite5m+u.CacheWrite1h > 0)
}

func (r *Router) store(ctx context.Context, t Turn, cached bool) error {
	if err := r.remember(ctx, callerKey(t.Caller), t.Candidate); err != nil {
		return err
	}
	if t.SessionKey == "" {
		return nil
	}
	now := r.now()
	ttl := t.CacheTTL
	if ttl <= 0 {
		ttl = r.opt.DefaultCacheTTL
		if t.Usage.CacheWrite1h > 0 {
			ttl = r.opt.LongCacheTTL
		}
	}
	b := Binding{Key: Key(t.Candidate), SentSessionID: t.SentSessionID, Cached: cached, At: now}
	keep := bindingKeep
	if cached {
		b.ValidUntil = now.Add(ttl)
		keep = max(ttl, bindingKeep)
	}
	raw, _ := json.Marshal(b)
	return r.st.Set(ctx, kBinding(t.SessionKey), raw, keep)
}

// Binding returns the stored binding of a session, valid or not.
func (r *Router) Binding(ctx context.Context, sessionKey string) (Binding, bool) {
	return r.loadBinding(ctx, sessionKey)
}

func (r *Router) loadBinding(ctx context.Context, sessionKey string) (Binding, bool) {
	raw, ok, err := r.st.Get(ctx, kBinding(sessionKey))
	if err != nil || !ok {
		return Binding{}, false
	}
	var b Binding
	if json.Unmarshal(raw, &b) != nil || b.Key == "" {
		return Binding{}, false
	}
	return b, true
}

func (r *Router) loadRecent(ctx context.Context, user string) []string {
	raw, ok, err := r.st.Get(ctx, kRecent(user))
	if err != nil || !ok {
		return nil
	}
	var keys []string
	_ = json.Unmarshal(raw, &keys)
	return keys
}

// remember puts the credential at the front of the caller's recent list.
// Concurrent turns of one user may race; the loser's entry is merely dropped.
func (r *Router) remember(ctx context.Context, user string, c plugin.Candidate) error {
	if user == "" || c.CredentialID == "" {
		return nil
	}
	k := credKey(c.UpstreamID, c.CredentialID)
	keys := r.loadRecent(ctx, user)
	keys = slices.DeleteFunc(keys, func(s string) bool { return s == k })
	keys = append([]string{k}, keys...)
	if len(keys) > r.opt.RecentPerUser {
		keys = keys[:r.opt.RecentPerUser]
	}
	raw, _ := json.Marshal(keys)
	return r.st.Set(ctx, kRecent(user), raw, r.opt.RecentTTL)
}
