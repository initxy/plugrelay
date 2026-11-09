package sqldialect

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRebind(t *testing.T) {
	for _, c := range []struct {
		d       Dialect
		in, out string
	}{
		{SQLite, `SELECT a FROM t WHERE b = ?`, `SELECT a FROM t WHERE b = ?`},
		{Postgres, `UPDATE t SET a = ? WHERE b = ?`, `UPDATE t SET a = $1 WHERE b = $2`},
		{MySQL, `INSERT INTO t (a, b) VALUES (?, ?) ON CONFLICT (a) DO NOTHING`, `INSERT INTO t (a, b) VALUES (?, ?)`},
		{MySQL, `INSERT INTO t (a, b) VALUES (?, ?)
ON CONFLICT DO NOTHING`, `INSERT INTO t (a, b) VALUES (?, ?)`},
		{MySQL, `INSERT INTO t (a, b, c) VALUES (?, ?, ?)
ON CONFLICT (a) DO UPDATE SET b = excluded.b, c = t.c + excluded.c`, `INSERT INTO t (a, b, c) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE b = VALUES(b), c = t.c + VALUES(c)`},
		{MySQL, `SELECT CAST(COALESCE(SUM(x), 0) AS BIGINT) FROM t`, `SELECT CAST(COALESCE(SUM(x), 0) AS SIGNED) FROM t`},
	} {
		if got := c.d.Rebind(c.in); got != c.out {
			t.Errorf("%s %q:\n got %q\nwant %q", c.d, c.in, got, c.out)
		}
	}
}

func TestFitAndTruncate(t *testing.T) {
	if Fit("short", 100) != "short" {
		t.Fatal("short changed")
	}
	long := strings.Repeat("键", 300)
	a, b := Fit(long+"a", 191), Fit(long+"b", 191)
	if a == b || a != Fit(long+"a", 191) || len(a) > 191 || !utf8.ValidString(a) {
		t.Fatalf("fit %q %q", a, b)
	}
	if got := Truncate(long, 191); utf8.RuneCountInString(got) != 191 || !utf8.ValidString(got) {
		t.Fatalf("truncate %d runes", utf8.RuneCountInString(got))
	}
	if l := LockName("db1", "k"); l == LockName("db2", "k") || len(l) > 64 {
		t.Fatalf("lock name %q", l)
	}
}
