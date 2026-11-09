package plugin

import (
	"context"
	"time"
)

// SharedState is the cluster-wide runtime state a compiled-in plugin may use
// for counters, concurrency slots and locks that every relay node must agree
// on (rate-limit windows, quota reservations). It is backed by the relay's
// state driver (memory, Redis or SQL). Values may be lost when that backend
// restarts, so plugins must treat them as soft state.
//
// A Host that offers it implements StateHost.
type SharedState interface {
	// Get returns a value, or ok=false when missing or expired.
	Get(ctx context.Context, key string) (value []byte, ok bool, err error)
	// Set stores a value; ttl <= 0 means no expiry.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// SetNX stores a value only if the key is absent; reports whether it did.
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	// Delete removes a key; a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// Incr adds delta to an integer counter and returns the new value. The
	// ttl is applied when the key is created and left alone afterwards.
	Incr(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error)
	// Acquire takes one of limit slots under key. It returns a release func,
	// or ok=false when all slots are taken. Slots expire after ttl so a
	// crashed holder cannot leak them.
	Acquire(ctx context.Context, key string, limit int, ttl time.Duration) (release func(), ok bool, err error)
	// Lock takes an exclusive lock. It returns an unlock func, or ok=false
	// when another holder has it.
	Lock(ctx context.Context, key string, ttl time.Duration) (unlock func(), ok bool, err error)
}

// StateHost is implemented by a Host that shares the cluster state.
type StateHost interface {
	State() SharedState
}
