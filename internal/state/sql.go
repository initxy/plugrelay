package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/initxy/plugrelay/internal/sqldialect"
)

// SQL is a durable backend on the store's database, for single-node
// deployments that want session bindings and cooldowns to survive a
// restart. It creates its own tables on open.
type SQL struct {
	db       *sql.DB
	dialect  sqldialect.Dialect
	database string // MySQL: the database, part of named lock names
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
}

var sqlSchema = map[sqldialect.Dialect][]string{
	"sqlite": {
		`CREATE TABLE IF NOT EXISTS state_kv (k TEXT PRIMARY KEY, v BLOB, n INTEGER, expires_at INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS state_slots (k TEXT NOT NULL, token TEXT NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY (k, token))`,
		`CREATE INDEX IF NOT EXISTS state_kv_expires ON state_kv (expires_at) WHERE expires_at > 0`,
	},
	"postgres": {
		`CREATE TABLE IF NOT EXISTS state_kv (k TEXT PRIMARY KEY, v BYTEA, n BIGINT, expires_at BIGINT NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS state_slots (k TEXT NOT NULL, token TEXT NOT NULL, expires_at BIGINT NOT NULL, PRIMARY KEY (k, token))`,
		`CREATE INDEX IF NOT EXISTS state_kv_expires ON state_kv (expires_at) WHERE expires_at > 0`,
	},
	// MySQL has neither partial indexes nor CREATE INDEX IF NOT EXISTS;
	// the index is declared with the table.
	"mysql": {
		`CREATE TABLE IF NOT EXISTS state_kv (k VARCHAR(512) NOT NULL PRIMARY KEY, v LONGBLOB, n BIGINT, expires_at BIGINT NOT NULL DEFAULT 0,
  INDEX state_kv_expires (expires_at)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
		`CREATE TABLE IF NOT EXISTS state_slots (k VARCHAR(512) NOT NULL, token VARCHAR(64) NOT NULL, expires_at BIGINT NOT NULL,
  PRIMARY KEY (k, token)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	},
}

// OpenSQL prepares the state tables on db and starts the expiry sweeper.
func OpenSQL(ctx context.Context, db *sql.DB, dialect string, sweepEvery time.Duration) (*SQL, error) {
	stmts, ok := sqlSchema[sqldialect.Dialect(dialect)]
	if !ok {
		return nil, fmt.Errorf("state: sql dialect %q unsupported", dialect)
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return nil, fmt.Errorf("state: create tables: %w", err)
		}
	}
	if sweepEvery <= 0 {
		sweepEvery = time.Minute
	}
	s := &SQL{db: db, dialect: sqldialect.Dialect(dialect), stop: make(chan struct{}), done: make(chan struct{})}
	if s.dialect == sqldialect.MySQL {
		if err := db.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&s.database); err != nil {
			return nil, fmt.Errorf("state: database name: %w", err)
		}
	}
	go s.janitor(sweepEvery)
	return s, nil
}

func (s *SQL) q(query string) string { return s.dialect.Rebind(query) }

// keyWidth is the width of the key columns (MySQL VARCHAR(512)).
const keyWidth = 512

// dbKey is the stored form of key: longer keys are cut and suffixed with
// their hash (sqldialect.Fit), on every database alike.
func dbKey(key string) string { return sqldialect.Fit(key, keyWidth) }

func nowMs() int64 { return time.Now().UnixMilli() }

func expMs(ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	return time.Now().Add(ttl).UnixMilli()
}

// Get implements State.
func (s *SQL) Get(ctx context.Context, key string) ([]byte, bool, error) {
	key = dbKey(key)
	var v []byte
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx, s.q(`SELECT v, n FROM state_kv WHERE k = ? AND (expires_at = 0 OR expires_at > ?)`), key, nowMs()).Scan(&v, &n)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("state: sql get: %w", err)
	}
	if v == nil && n.Valid {
		v = strconv.AppendInt(nil, n.Int64, 10)
	}
	if v == nil {
		v = []byte{}
	}
	return v, true, nil
}

// Set implements State.
func (s *SQL) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	key = dbKey(key)
	if value == nil {
		value = []byte{}
	}
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO state_kv (k, v, n, expires_at) VALUES (?, ?, ?, ?)
ON CONFLICT (k) DO UPDATE SET v = excluded.v, n = excluded.n, expires_at = excluded.expires_at`), key, value, intOf(value), expMs(ttl))
	if err != nil {
		return fmt.Errorf("state: sql set: %w", err)
	}
	return nil
}

func intOf(v []byte) sql.NullInt64 {
	n, err := strconv.ParseInt(string(v), 10, 64)
	return sql.NullInt64{Int64: n, Valid: err == nil}
}

// SetNX implements State. An expired entry is dropped first, so the insert
// that follows decides the race on its own.
func (s *SQL) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	key = dbKey(key)
	if value == nil {
		value = []byte{}
	}
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM state_kv WHERE k = ? AND expires_at > 0 AND expires_at <= ?`), key, nowMs()); err != nil {
		return false, fmt.Errorf("state: sql setnx: %w", err)
	}
	res, err := s.dialect.Exec(ctx, s.db, `INSERT INTO state_kv (k, v, n, expires_at) VALUES (?, ?, ?, ?)
ON CONFLICT (k) DO NOTHING`, key, value, intOf(value), expMs(ttl))
	if err != nil {
		return false, fmt.Errorf("state: sql setnx: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("state: sql setnx: %w", err)
	}
	return n > 0, nil
}

// Delete implements State.
func (s *SQL) Delete(ctx context.Context, key string) error {
	key = dbKey(key)
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM state_kv WHERE k = ?`), key); err != nil {
		return fmt.Errorf("state: sql delete: %w", err)
	}
	return nil
}

// Incr implements State. A value that is not an integer counts as 0. The
// read back runs in the upsert's transaction, under its row lock; n is
// assigned before expires_at, which its expiry test reads.
func (s *SQL) Incr(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	key = dbKey(key)
	now := nowMs()
	var n int64
	err := func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO state_kv (k, v, n, expires_at) VALUES (?, NULL, ?, ?)
ON CONFLICT (k) DO UPDATE SET
  n = CASE WHEN state_kv.expires_at > 0 AND state_kv.expires_at <= ? THEN excluded.n ELSE COALESCE(state_kv.n, 0) + excluded.n END,
  expires_at = CASE WHEN state_kv.expires_at > 0 AND state_kv.expires_at <= ? THEN excluded.expires_at ELSE state_kv.expires_at END,
  v = NULL`), key, delta, expMs(ttl), now, now); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, s.q(`SELECT n FROM state_kv WHERE k = ?`), key).Scan(&n); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		return 0, fmt.Errorf("state: sql incr: %w", err)
	}
	return n, nil
}

// Acquire implements State.
func (s *SQL) Acquire(ctx context.Context, key string, limit int, ttl time.Duration) (func(), bool, error) {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	key = dbKey(key)
	tok := newToken()
	ok, err := s.acquire(ctx, key, tok, limit, ttl)
	if err != nil || !ok {
		return nil, false, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			c, cancel := releaseCtx(ctx)
			defer cancel()
			_, _ = s.db.ExecContext(c, s.q(`DELETE FROM state_slots WHERE k = ? AND token = ?`), key, tok)
		})
	}, true, nil
}

func (s *SQL) acquire(ctx context.Context, key, tok string, limit int, ttl time.Duration) (bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("state: sql acquire: %w", err)
	}
	defer conn.Close()
	// Serializes acquirers of one key; sqlite transactions already are.
	if s.dialect == sqldialect.MySQL {
		// MySQL's named locks belong to the session, not the transaction.
		name := sqldialect.LockName(s.database, "state_slots:"+key)
		var got sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 10)`, name).Scan(&got); err != nil {
			return false, fmt.Errorf("state: sql acquire lock: %w", err)
		}
		if got.Int64 != 1 {
			return false, errors.New("state: sql acquire lock: GET_LOCK timed out")
		}
		defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT RELEASE_LOCK(?)`, name) }()
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("state: sql acquire: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.dialect == sqldialect.Postgres {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "state_slots:"+key); err != nil {
			return false, fmt.Errorf("state: sql acquire lock: %w", err)
		}
	}
	now := nowMs()
	if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM state_slots WHERE k = ? AND expires_at <= ?`), key, now); err != nil {
		return false, fmt.Errorf("state: sql acquire: %w", err)
	}
	var held int
	if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM state_slots WHERE k = ?`), key).Scan(&held); err != nil {
		return false, fmt.Errorf("state: sql acquire: %w", err)
	}
	if held >= limit {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO state_slots (k, token, expires_at) VALUES (?, ?, ?)`), key, tok, now+ttl.Milliseconds()); err != nil {
		return false, fmt.Errorf("state: sql acquire: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("state: sql acquire: %w", err)
	}
	return true, nil
}

// Lock implements State.
func (s *SQL) Lock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	key = dbKey(key)
	tok := []byte(newToken())
	ok, err := s.SetNX(ctx, key, tok, ttl)
	if err != nil || !ok {
		return nil, false, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			c, cancel := releaseCtx(ctx)
			defer cancel()
			_, _ = s.db.ExecContext(c, s.q(`DELETE FROM state_kv WHERE k = ? AND v = ?`), key, tok)
		})
	}, true, nil
}

func (s *SQL) janitor(every time.Duration) {
	defer close(s.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			now := nowMs()
			_, _ = s.db.ExecContext(ctx, s.q(`DELETE FROM state_kv WHERE expires_at > 0 AND expires_at <= ?`), now)
			_, _ = s.db.ExecContext(ctx, s.q(`DELETE FROM state_slots WHERE expires_at <= ?`), now)
			cancel()
		}
	}
}

// Close stops the sweeper. The database handle belongs to the store and
// stays open.
func (s *SQL) Close() error {
	s.once.Do(func() { close(s.stop) })
	<-s.done
	return nil
}
