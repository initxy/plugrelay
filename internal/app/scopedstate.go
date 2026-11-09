package app

import (
	"context"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// scopedState gives one plugin its own key space in the shared state
// backend, so a plugin cannot read or change another plugin's counters (for
// example rate-limit windows touching quota reservations) or the router's
// keys. Every key is stored as "plugin:<id>:<key>".
type scopedState struct {
	st     plugin.SharedState
	prefix string
}

var _ plugin.SharedState = scopedState{}

func newScopedState(st plugin.SharedState, pluginID string) scopedState {
	return scopedState{st: st, prefix: "plugin:" + pluginID + ":"}
}

func (s scopedState) Get(ctx context.Context, key string) ([]byte, bool, error) {
	return s.st.Get(ctx, s.prefix+key)
}

func (s scopedState) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return s.st.Set(ctx, s.prefix+key, value, ttl)
}

func (s scopedState) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	return s.st.SetNX(ctx, s.prefix+key, value, ttl)
}

func (s scopedState) Delete(ctx context.Context, key string) error {
	return s.st.Delete(ctx, s.prefix+key)
}

func (s scopedState) Incr(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	return s.st.Incr(ctx, s.prefix+key, delta, ttl)
}

func (s scopedState) Acquire(ctx context.Context, key string, limit int, ttl time.Duration) (func(), bool, error) {
	return s.st.Acquire(ctx, s.prefix+key, limit, ttl)
}

func (s scopedState) Lock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	return s.st.Lock(ctx, s.prefix+key, ttl)
}
