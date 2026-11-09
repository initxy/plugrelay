package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
	_ "modernc.org/sqlite"             // registers "sqlite"

	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/sqldialect"
	"github.com/initxy/plugrelay/internal/state"
)

//go:embed migrations
var migrations embed.FS

type dialect = sqldialect.Dialect

const (
	sqliteDialect   = sqldialect.SQLite
	postgresDialect = sqldialect.Postgres
	mysqlDialect    = sqldialect.MySQL
)

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// run executes rebound queries on a DB or a transaction.
type run struct {
	q querier
	d dialect
}

func (r run) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return r.d.Exec(ctx, r.q, q, args...)
}

func (r run) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return r.q.QueryContext(ctx, r.d.Rebind(q), args...)
}

func (r run) row(ctx context.Context, q string, args ...any) *sql.Row {
	return r.q.QueryRowContext(ctx, r.d.Rebind(q), args...)
}

// db implements Store.
type db struct {
	sql *sql.DB
	d   dialect
	box *secret.Box

	touchMu sync.Mutex
	touches map[string]time.Time
	stop    chan struct{}
	done    chan struct{}
	closed  sync.Once
}

// Open connects, applies pending migrations and starts the key-touch
// flusher.
func Open(ctx context.Context, cfg Config) (Store, error) {
	var (
		d      dialect
		driver string
		dsn    = cfg.DSN
	)
	switch cfg.Driver {
	case "sqlite":
		d, driver, dsn = sqliteDialect, "sqlite", sqliteDSN(cfg.DSN)
	case "postgres":
		d, driver = postgresDialect, "pgx"
	case "mysql":
		d = mysqlDialect
	default:
		return nil, fmt.Errorf("store: unknown driver %q", cfg.Driver)
	}
	if cfg.DSN == "" {
		return nil, errors.New("store: empty DSN")
	}
	var (
		sdb *sql.DB
		err error
	)
	if d == mysqlDialect {
		sdb, err = sqldialect.OpenMySQL(dsn)
	} else {
		sdb, err = sql.Open(driver, dsn)
	}
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", cfg.Driver, err)
	}
	conns := cfg.MaxConns
	if conns <= 0 {
		conns = 20
		if d == sqliteDialect {
			conns = 8
		}
	}
	sdb.SetMaxOpenConns(conns)
	sdb.SetMaxIdleConns(min(conns, 8))
	sdb.SetConnMaxIdleTime(5 * time.Minute)
	if err := sdb.PingContext(ctx); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("store: connect %s: %w", cfg.Driver, err)
	}
	s := &db{sql: sdb, d: d, box: cfg.Secrets, touches: map[string]time.Time{}, stop: make(chan struct{}), done: make(chan struct{})}
	if err := s.migrate(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	every := cfg.TouchFlushEvery
	if every <= 0 {
		every = 30 * time.Second
	}
	go s.flusher(every)
	return s, nil
}

func sqliteDSN(dsn string) string {
	if strings.Contains(dsn, "?") {
		return dsn
	}
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}
	v := url.Values{}
	v.Add("_pragma", "busy_timeout(10000)")
	v.Add("_pragma", "journal_mode(WAL)")
	v.Add("_pragma", "synchronous(NORMAL)")
	v.Add("_pragma", "foreign_keys(ON)")
	v.Set("_txlock", "immediate")
	return dsn + "?" + v.Encode()
}

func (s *db) r() run { return run{q: s.sql, d: s.d} }

// txAttempts is how many times tx runs a transaction the database aborted
// to break a deadlock.
const txAttempts = 4

// tx runs fn in a transaction, and again when the database aborted it to
// break a deadlock (PostgreSQL 40P01, MySQL 1213), up to txAttempts times
// in all. fn may therefore run more than once: it sets its results from
// scratch on every run.
func (s *db) tx(ctx context.Context, fn func(r run) error) error {
	for attempt := 1; ; attempt++ {
		err := s.tx1(ctx, fn)
		if err == nil || attempt == txAttempts || !isDeadlock(err) {
			return err
		}
		t := time.NewTimer(time.Duration(attempt)*5*time.Millisecond + time.Duration(rand.Int64N(int64(10*time.Millisecond))))
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}

// isDeadlock reports a transaction the database chose as a deadlock
// victim and rolled back.
func isDeadlock(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "40P01"
	}
	var my *mysql.MySQLError
	if errors.As(err, &my) {
		return my.Number == 1213 // ER_LOCK_DEADLOCK
	}
	return false
}

func (s *db) tx1(ctx context.Context, fn func(r run) error) error {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	if err := fn(run{q: tx, d: s.d}); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// write runs fn in a transaction that also bumps the routing version.
func (s *db) write(ctx context.Context, fn func(r run) error) error {
	return s.tx(ctx, func(r run) error {
		if err := fn(r); err != nil {
			return err
		}
		return bump(ctx, r)
	})
}

func bump(ctx context.Context, r run) error {
	if _, err := r.exec(ctx, `UPDATE meta SET v = v + 1 WHERE k = 'version'`); err != nil {
		return fmt.Errorf("store: bump version: %w", err)
	}
	return nil
}

func (s *db) Version(ctx context.Context) (int64, error) {
	var v int64
	if err := s.r().row(ctx, `SELECT v FROM meta WHERE k = 'version'`).Scan(&v); err != nil {
		return 0, fmt.Errorf("store: version: %w", err)
	}
	return v, nil
}

func (s *db) migrate(ctx context.Context) error {
	dir := "migrations/" + string(s.d)
	entries, err := fs.ReadDir(migrations, dir)
	if err != nil {
		return fmt.Errorf("store: migrations: %w", err)
	}
	conn, err := s.sql.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	defer conn.Close()
	r := run{q: conn, d: s.d}
	// Nodes starting together must not migrate concurrently.
	switch s.d {
	case postgresDialect:
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(7072657261790001)`); err != nil {
			return fmt.Errorf("store: migrate lock: %w", err)
		}
		defer func() {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(7072657261790001)`)
		}()
	case mysqlDialect:
		var got sql.NullInt64
		var database string
		if err := conn.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&database); err != nil {
			return fmt.Errorf("store: migrate lock: %w", err)
		}
		name := sqldialect.LockName(database, "migrate")
		if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 300)`, name).Scan(&got); err != nil {
			return fmt.Errorf("store: migrate lock: %w", err)
		}
		if got.Int64 != 1 {
			return errors.New("store: migrate lock: GET_LOCK timed out")
		}
		defer func() {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT RELEASE_LOCK(?)`, name)
		}()
	}
	if _, err := r.exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version BIGINT PRIMARY KEY, name TEXT NOT NULL, applied_at BIGINT NOT NULL)`); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	applied := map[int64]bool{}
	rows, err := r.query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("store: migrate: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		name := e.Name()
		num, _, ok := strings.Cut(name, "_")
		v, err := strconv.ParseInt(num, 10, 64)
		if !ok || err != nil || !strings.HasSuffix(name, ".sql") {
			return fmt.Errorf("store: migration file %s: want NNNN_name.sql", name)
		}
		if applied[v] {
			continue
		}
		body, err := migrations.ReadFile(dir + "/" + name)
		if err != nil {
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		for _, stmt := range splitSQL(string(body)) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("store: migration %s: %w", name, err)
			}
		}
		if _, err := tx.ExecContext(ctx, s.d.Rebind(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`), v, name, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
	}
	return nil
}

// splitSQL splits a migration on statement-ending semicolons. Migrations
// must not contain semicolons inside strings, bodies or comments.
func splitSQL(body string) []string {
	var out []string
	for _, part := range strings.Split(body, ";") {
		var lines []string
		for _, l := range strings.Split(part, "\n") {
			if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "--") {
				lines = append(lines, l)
			}
		}
		if len(lines) > 0 {
			out = append(out, strings.Join(lines, "\n"))
		}
	}
	return out
}

func (s *db) State(ctx context.Context) (state.State, error) {
	return state.Open(ctx, state.Config{Driver: "sql"}, state.WithSQL(s.sql, string(s.d)))
}

func (s *db) Close() error {
	var err error
	s.closed.Do(func() {
		close(s.stop)
		<-s.done
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = errors.Join(s.FlushTouches(ctx), s.sql.Close())
	})
	return err
}

// ---------------------------------------------------------------- helpers

func newID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMs(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

// js encodes a JSON column; nil maps and slices become empty, never null.
func js(v any, empty string) string {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return empty
	}
	return string(b)
}

func unjs(src string, dst any) error {
	if src == "" || src == "null" {
		return nil
	}
	if err := json.Unmarshal([]byte(src), dst); err != nil {
		return fmt.Errorf("store: decode json column: %w", err)
	}
	return nil
}

// wrap turns driver errors into package errors with context.
func wrap(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("store: %s: %w", op, ErrNotFound)
	case isUnique(err):
		return fmt.Errorf("store: %s: %w: %v", op, ErrConflict, err)
	}
	return fmt.Errorf("store: %s: %w", op, err)
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "23505"
	}
	var my *mysql.MySQLError
	if errors.As(err, &my) {
		return my.Number == 1062 // ER_DUP_ENTRY
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// mustAffect turns a write that touched no row into ErrNotFound.
func mustAffect(op string, res sql.Result, err error) error {
	if err != nil {
		return wrap(op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return wrap(op, err)
	}
	if n == 0 {
		return fmt.Errorf("store: %s: %w", op, ErrNotFound)
	}
	return nil
}

func isNotFound(err error) bool { return errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) }
