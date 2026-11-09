package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Config selects a backend.
type Config struct {
	// Driver is "memory", "redis" or "sql".
	Driver string
	// URL is the redis URL (redis://user:pass@host:6379/0); unused otherwise.
	URL string
	// Prefix namespaces redis keys; defaults to "plugrelay:".
	Prefix string
	// SweepEvery is how often memory and sql backends purge expired
	// entries; defaults to one minute.
	SweepEvery time.Duration
}

// Option supplies dependencies a driver needs.
type Option func(*options)

type options struct {
	db      *sql.DB
	dialect string
}

// WithSQL hands the sql driver its database handle and dialect ("sqlite",
// "postgres" or "mysql"). The store passes its own handle; see store.Store.State.
func WithSQL(db *sql.DB, dialect string) Option {
	return func(o *options) { o.db, o.dialect = db, dialect }
}

// Open connects to the configured backend.
func Open(ctx context.Context, cfg Config, opts ...Option) (State, error) {
	var o options
	for _, f := range opts {
		f(&o)
	}
	if cfg.SweepEvery <= 0 {
		cfg.SweepEvery = time.Minute
	}
	switch cfg.Driver {
	case "", "memory":
		return NewMemory(cfg.SweepEvery), nil
	case "redis":
		return OpenRedis(ctx, cfg.URL, cfg.Prefix)
	case "sql":
		if o.db == nil {
			return nil, errors.New("state: sql driver needs a database handle (WithSQL)")
		}
		return OpenSQL(ctx, o.db, o.dialect, cfg.SweepEvery)
	}
	return nil, fmt.Errorf("state: unknown driver %q", cfg.Driver)
}

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// releaseCtx detaches a release from its request so a cancelled request
// still frees its slot.
func releaseCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
