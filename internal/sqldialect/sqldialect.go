// Package sqldialect adapts the SQL that the store and the SQL state
// backend write once to the database they run on.
//
// Queries are written in the dialect SQLite and PostgreSQL share: ?
// placeholders, INSERT … ON CONFLICT (cols) DO UPDATE SET c = excluded.c
// or DO NOTHING, CAST(… AS BIGINT), double-quoted identifiers for the
// MySQL reserved words "groups" and "usage", and no RETURNING (MySQL has
// none). Rebind turns that into each driver's form; MySQL connections must
// come from OpenMySQL, whose session settings those forms rely on. A DO
// NOTHING insert must run through Exec, which on MySQL turns the duplicate
// key error of the plain INSERT it becomes into zero rows affected.
package sqldialect

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
)

// Dialect names a supported SQL database.
type Dialect string

const (
	SQLite   Dialect = "sqlite"
	Postgres Dialect = "postgres"
	MySQL    Dialect = "mysql"
)

// Rebind rewrites q for d. Queries never contain a literal '?'.
func (d Dialect) Rebind(q string) string {
	switch d {
	case Postgres:
		return numbered(q)
	case MySQL:
		if v, ok := mysqlCache.Load(q); ok {
			return v.(string)
		}
		v := toMySQL(q)
		mysqlCache.Store(q, v)
		return v
	}
	return q
}

// Execer is a *sql.DB, *sql.Tx or *sql.Conn.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Exec rebinds and runs q. On MySQL, INSERT … ON CONFLICT DO NOTHING runs
// as a plain INSERT whose duplicate key error (1062, on any unique key)
// reports zero rows affected; every other error, such as a value too long
// for its column, is returned. A failed statement does not end a MySQL
// transaction.
func (d Dialect) Exec(ctx context.Context, ex Execer, q string, args ...any) (sql.Result, error) {
	res, err := ex.ExecContext(ctx, d.Rebind(q), args...)
	if err != nil && d == MySQL && IsDuplicate(err) && reDoNothing.MatchString(q) {
		return noRows{}, nil
	}
	return res, err
}

type noRows struct{}

func (noRows) LastInsertId() (int64, error) { return 0, nil }
func (noRows) RowsAffected() (int64, error) { return 0, nil }

// IsDuplicate reports a MySQL duplicate key error.
func IsDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

// Fit returns s if it is at most n characters, else a deterministic
// stand-in of n characters: a prefix of s cut at a character boundary,
// "#" and the hex SHA-256 of s. n must exceed 65. MySQL VARCHAR widths
// count characters; s is cut by bytes, which is never shorter.
func Fit(s string, n int) string {
	if len(s) <= n {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	keep := n - 1 - 2*len(sum)
	for keep > 0 && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + "#" + hex.EncodeToString(sum[:])
}

// Truncate cuts s to at most n characters (an indexed column's width).
func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i, c := 0, 0
	for i < len(s) && c < n {
		_, w := utf8.DecodeRuneInString(s[i:])
		i += w
		c++
	}
	return s[:i]
}

// LockName is a MySQL GET_LOCK name for what in database: named locks are
// server-wide, so two databases on one server must not share them, and a
// name is at most 64 characters.
func LockName(database, what string) string {
	sum := sha256.Sum256([]byte(database + "\x00" + what))
	return "plugrelay_" + hex.EncodeToString(sum[:24])
}

// ForUpdate is the locking-read suffix for a SELECT; SQLite has none and
// needs none, its write transactions being serialized.
func (d Dialect) ForUpdate() string {
	if d == SQLite {
		return ""
	}
	return " FOR UPDATE"
}

func numbered(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 16)
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

// mysqlSession runs on every new MySQL connection. ANSI_QUOTES and
// NO_BACKSLASH_ESCAPES make identifiers and string literals behave as in
// SQLite and PostgreSQL; READ COMMITTED matches PostgreSQL's default and
// keeps InnoDB from taking gap locks that the shared queries never expect.
var mysqlSession = []string{
	`SET SESSION sql_mode = 'ANSI_QUOTES,PIPES_AS_CONCAT,NO_BACKSLASH_ESCAPES,STRICT_ALL_TABLES,ONLY_FULL_GROUP_BY,NO_ZERO_IN_DATE,NO_ZERO_DATE,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'`,
	`SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED`,
}

var (
	mysqlCache sync.Map

	reDoNothing = regexp.MustCompile(`(?s)\s*ON CONFLICT\s*(\([^)]*\))?\s*DO NOTHING`)
	reDoUpdate  = regexp.MustCompile(`(?s)ON CONFLICT\s*\([^)]*\)\s*DO UPDATE SET`)
	reExcluded  = regexp.MustCompile(`\bexcluded\.(\w+)`)
	reCastInt   = regexp.MustCompile(`\bAS BIGINT\)`)
)

// toMySQL maps the shared upsert forms onto a plain INSERT (DO NOTHING,
// see Exec; never INSERT IGNORE, which also turns a value too long for its
// column into a warning) and ON DUPLICATE KEY UPDATE. MySQL matches a
// duplicate on any unique key, not just the named conflict columns, so an
// upsert's target must be the table's only unique key. ON DUPLICATE KEY UPDATE assigns left to right: a later
// assignment that reads a column assigned earlier sees the new value.
func toMySQL(q string) string {
	q = reDoNothing.ReplaceAllString(q, "")
	if reDoUpdate.MatchString(q) {
		q = reDoUpdate.ReplaceAllString(q, "ON DUPLICATE KEY UPDATE")
		q = reExcluded.ReplaceAllString(q, "VALUES($1)")
	}
	return reCastInt.ReplaceAllString(q, "AS SIGNED)")
}
