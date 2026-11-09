package state_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/initxy/plugrelay/internal/sqldialect"
	"github.com/initxy/plugrelay/internal/state"
)

type backend struct {
	st   state.State
	wait func(time.Duration) // advances the backend's clock past d
}

func TestMemory(t *testing.T) {
	conformance(t, func(t *testing.T) backend {
		return backend{st: state.NewMemory(10 * time.Millisecond), wait: sleep}
	})
}

func TestRedis(t *testing.T) {
	conformance(t, func(t *testing.T) backend {
		mr := miniredis.RunT(t)
		var mu sync.Mutex
		now := time.Now()
		mr.SetTime(now)
		st, err := state.Open(context.Background(), state.Config{Driver: "redis", URL: "redis://" + mr.Addr()})
		if err != nil {
			t.Fatal(err)
		}
		return backend{st: st, wait: func(d time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(d + time.Millisecond)
			mr.SetTime(now)
			mr.FastForward(d + time.Millisecond)
		}}
	})
}

func TestSQLite(t *testing.T) {
	conformance(t, func(t *testing.T) backend {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "s.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		st, err := state.Open(context.Background(), state.Config{Driver: "sql", SweepEvery: 10 * time.Millisecond}, state.WithSQL(db, "sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		return backend{st: st, wait: sleep}
	})
}

func TestPostgres(t *testing.T) {
	dsn := os.Getenv("PLUGRELAY_TEST_PG")
	if dsn == "" {
		t.Skip("PLUGRELAY_TEST_PG not set")
	}
	conformance(t, func(t *testing.T) backend {
		ctx := context.Background()
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		schema := fmt.Sprintf("state_test_%d", time.Now().UnixNano())
		if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE")
			admin.Close()
		})
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		db, err := sql.Open("pgx", dsn+sep+"search_path="+schema)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		st, err := state.Open(ctx, state.Config{Driver: "sql", SweepEvery: 10 * time.Millisecond}, state.WithSQL(db, "postgres"))
		if err != nil {
			t.Fatal(err)
		}
		return backend{st: st, wait: sleep}
	})
}

func TestMySQL(t *testing.T) {
	dsn := os.Getenv("PLUGRELAY_TEST_MYSQL")
	if dsn == "" {
		t.Skip("PLUGRELAY_TEST_MYSQL not set")
	}
	conformance(t, func(t *testing.T) backend {
		ctx := context.Background()
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			t.Fatal(err)
		}
		admin, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.DBName = fmt.Sprintf("state_test_%d", time.Now().UnixNano())
		if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+cfg.DBName); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = admin.ExecContext(ctx, "DROP DATABASE "+cfg.DBName)
			admin.Close()
		})
		db, err := sqldialect.OpenMySQL(cfg.FormatDSN())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		st, err := state.Open(ctx, state.Config{Driver: "sql", SweepEvery: 10 * time.Millisecond}, state.WithSQL(db, "mysql"))
		if err != nil {
			t.Fatal(err)
		}
		return backend{st: st, wait: sleep}
	})
}

func sleep(d time.Duration) { time.Sleep(d + 20*time.Millisecond) }

func conformance(t *testing.T, open func(t *testing.T) backend) {
	ctx := context.Background()
	run := func(name string, f func(t *testing.T, b backend)) {
		t.Run(name, func(t *testing.T) {
			b := open(t)
			t.Cleanup(func() { b.st.Close() })
			f(t, b)
		})
	}
	must := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	get := func(t *testing.T, st state.State, k string) (string, bool) {
		t.Helper()
		v, ok, err := st.Get(ctx, k)
		must(t, err)
		return string(v), ok
	}

	run("GetSetDelete", func(t *testing.T, b backend) {
		if _, ok := get(t, b.st, "a"); ok {
			t.Fatal("missing key found")
		}
		must(t, b.st.Set(ctx, "a", []byte("1"), 0))
		must(t, b.st.Set(ctx, "a", []byte("2"), 0))
		if v, ok := get(t, b.st, "a"); !ok || v != "2" {
			t.Fatalf("got %q %v", v, ok)
		}
		must(t, b.st.Delete(ctx, "a"))
		if _, ok := get(t, b.st, "a"); ok {
			t.Fatal("deleted key found")
		}
		must(t, b.st.Delete(ctx, "a"))
	})

	// Keys past the SQL key column (512 characters) are stored hashed:
	// two keys sharing a long prefix stay apart and a lock releases.
	run("LongKeys", func(t *testing.T, b backend) {
		long := strings.Repeat("L", 600)
		must(t, b.st.Set(ctx, long+"a", []byte("1"), time.Minute))
		must(t, b.st.Set(ctx, long+"b", []byte("2"), time.Minute))
		if v, ok := get(t, b.st, long+"a"); !ok || v != "1" {
			t.Fatalf("long key a: %q %v", v, ok)
		}
		if n, err := b.st.Incr(ctx, long+"n", 3, time.Minute); err != nil || n != 3 {
			t.Fatalf("incr: %d %v", n, err)
		}
		relA, okA, err := b.st.Lock(ctx, long+"one", time.Minute)
		must(t, err)
		_, okB, err := b.st.Lock(ctx, long+"two", time.Minute)
		must(t, err)
		if !okA || !okB {
			t.Fatalf("locks on keys with one long prefix: %v %v", okA, okB)
		}
		relA()
		rel, ok, err := b.st.Lock(ctx, long+"one", time.Minute)
		must(t, err)
		if !ok {
			t.Fatal("long lock not released")
		}
		rel()
		relS, ok, err := b.st.Acquire(ctx, long+"slot", 1, time.Minute)
		must(t, err)
		if !ok {
			t.Fatal("long slot not acquired")
		}
		relS()
		must(t, b.st.Delete(ctx, long+"a"))
		if _, ok := get(t, b.st, long+"a"); ok {
			t.Fatal("long key not deleted")
		}
	})

	run("TTL", func(t *testing.T, b backend) {
		must(t, b.st.Set(ctx, "t", []byte("x"), 100*time.Millisecond))
		must(t, b.st.Set(ctx, "p", []byte("y"), 0))
		if _, ok := get(t, b.st, "t"); !ok {
			t.Fatal("expired too early")
		}
		b.wait(100 * time.Millisecond)
		if _, ok := get(t, b.st, "t"); ok {
			t.Fatal("not expired")
		}
		if _, ok := get(t, b.st, "p"); !ok {
			t.Fatal("persistent key expired")
		}
	})

	run("SetNX", func(t *testing.T, b backend) {
		ok, err := b.st.SetNX(ctx, "n", []byte("1"), 100*time.Millisecond)
		must(t, err)
		if !ok {
			t.Fatal("first SetNX failed")
		}
		ok, err = b.st.SetNX(ctx, "n", []byte("2"), 0)
		must(t, err)
		if ok {
			t.Fatal("second SetNX succeeded")
		}
		if v, _ := get(t, b.st, "n"); v != "1" {
			t.Fatalf("value overwritten: %q", v)
		}
		b.wait(100 * time.Millisecond)
		ok, err = b.st.SetNX(ctx, "n", []byte("3"), 0)
		must(t, err)
		if !ok {
			t.Fatal("SetNX over expired key failed")
		}
	})

	run("Incr", func(t *testing.T, b backend) {
		// A ttl long enough that the next two calls land inside it on a
		// loaded machine.
		n, err := b.st.Incr(ctx, "c", 5, time.Second)
		must(t, err)
		if n != 5 {
			t.Fatalf("got %d", n)
		}
		n, err = b.st.Incr(ctx, "c", -2, time.Hour)
		must(t, err)
		if n != 3 {
			t.Fatalf("got %d", n)
		}
		if v, _ := get(t, b.st, "c"); v != "3" {
			t.Fatalf("Get counter = %q", v)
		}
		// The ttl from creation still holds; the second call did not extend it.
		b.wait(time.Second)
		n, err = b.st.Incr(ctx, "c", 1, 0)
		must(t, err)
		if n != 1 {
			t.Fatalf("after expiry got %d", n)
		}
		must(t, b.st.Set(ctx, "s", []byte("41"), 0))
		n, err = b.st.Incr(ctx, "s", 1, 0)
		must(t, err)
		if n != 42 {
			t.Fatalf("incr over Set value got %d", n)
		}
	})

	run("IncrConcurrent", func(t *testing.T, b backend) {
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				if _, err := b.st.Incr(ctx, "cc", 1, time.Minute); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if v, _ := get(t, b.st, "cc"); v != "20" {
			t.Fatalf("got %q", v)
		}
	})

	run("Acquire", func(t *testing.T, b backend) {
		r1, ok, err := b.st.Acquire(ctx, "slots", 2, time.Minute)
		must(t, err)
		if !ok {
			t.Fatal("1st")
		}
		// A short lease that must still be held for the next few calls even
		// on a loaded machine (the full check runs every suite at once).
		_, ok, err = b.st.Acquire(ctx, "slots", 2, 2*time.Second)
		must(t, err)
		if !ok {
			t.Fatal("2nd")
		}
		if _, ok, _ = b.st.Acquire(ctx, "slots", 2, time.Minute); ok {
			t.Fatal("3rd over limit")
		}
		r1()
		r1() // idempotent
		r3, ok, err := b.st.Acquire(ctx, "slots", 2, time.Minute)
		must(t, err)
		if !ok {
			t.Fatal("after release")
		}
		if _, ok, _ = b.st.Acquire(ctx, "slots", 2, time.Minute); ok {
			t.Fatal("double release freed two slots")
		}
		b.wait(2 * time.Second) // 2nd lease expires
		if _, ok, _ = b.st.Acquire(ctx, "slots", 2, time.Minute); !ok {
			t.Fatal("expired lease not reclaimed")
		}
		r3()
		if _, ok, _ = b.st.Acquire(ctx, "other", 1, time.Minute); !ok {
			t.Fatal("keys not independent")
		}
	})

	run("AcquireConcurrent", func(t *testing.T, b backend) {
		var got atomic.Int32
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				_, ok, err := b.st.Acquire(ctx, "race", 5, time.Minute)
				if err != nil {
					t.Error(err)
				}
				if ok {
					got.Add(1)
				}
			})
		}
		wg.Wait()
		if got.Load() != 5 {
			t.Fatalf("acquired %d of limit 5", got.Load())
		}
	})

	run("Lock", func(t *testing.T, b backend) {
		unlock, ok, err := b.st.Lock(ctx, "l", 100*time.Millisecond)
		must(t, err)
		if !ok {
			t.Fatal("lock")
		}
		if _, ok, _ := b.st.Lock(ctx, "l", time.Minute); ok {
			t.Fatal("lock held twice")
		}
		b.wait(100 * time.Millisecond)
		unlock2, ok, err := b.st.Lock(ctx, "l", time.Minute)
		must(t, err)
		if !ok {
			t.Fatal("expired lock not reclaimable")
		}
		unlock() // stale holder must not release the new holder's lock
		if _, ok, _ := b.st.Lock(ctx, "l", time.Minute); ok {
			t.Fatal("stale unlock released another holder")
		}
		unlock2()
		if _, ok, _ := b.st.Lock(ctx, "l", time.Minute); !ok {
			t.Fatal("unlock did not release")
		}
	})
}
