package store_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
)

// Concurrent captures, releases and sweeps on the same holds never charge
// a request twice or leave an amount held.
func TestLedgerSettleChaosSQLite(t *testing.T) { settleChaos(t, sqliteOpener) }
func TestLedgerSettleChaosMySQL(t *testing.T)  { settleChaos(t, mysqlFromEnv(t)) }
func TestLedgerSettleChaosPostgres(t *testing.T) {
	dsn := os.Getenv("PLUGRELAY_TEST_PG")
	if dsn == "" {
		t.Skip()
	}
	settleChaos(t, pgOpener(dsn))
}

func settleChaos(t *testing.T, open opener) {
	ctx := context.Background()
	l := ledgerOf(t, open(t, newBox(t)))
	if _, _, err := l.Credit(ctx, &store.BalanceTxn{ID: "t1", Subject: "user:x", Kind: store.TxnTopup, AmountMicro: 1000}); err != nil {
		t.Fatal(err)
	}
	const n = 40
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	held := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			_, err := l.Hold(ctx, "user:x", fmt.Sprintf("r%d", i), 50)
			<-sem
			held[i] = err == nil
		}()
	}
	wg.Wait()
	ok := 0
	for _, h := range held {
		if h {
			ok++
		}
	}
	if ok != 20 {
		t.Fatalf("holds ok=%d want 20", ok)
	}
	// Chaos: for each held request: 3 captures, 2 releases, and sweeps, concurrently.
	captured := make([]int, n)
	var mu sync.Mutex
	for i := range n {
		if !held[i] {
			continue
		}
		id := fmt.Sprintf("r%d", i)
		for range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				a, err := l.Capture(ctx, id, 30, "")
				<-sem
				if err != nil {
					t.Error(err)
				}
				if a {
					mu.Lock()
					captured[i]++
					mu.Unlock()
				}
			}()
		}
		for range 2 {
			wg.Add(1)
			go func() { defer wg.Done(); sem <- struct{}{}; _, _ = l.Release(ctx, id); <-sem }()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			_, _ = l.ExpireHolds(ctx, time.Now().Add(time.Hour))
			<-sem
		}()
	}
	wg.Wait()
	a, _ := l.Account(ctx, "user:x")
	total := 0
	for _, c := range captured {
		if c > 1 {
			t.Fatalf("captured twice")
		}
		total += c
	}
	if a.HeldMicro != 0 {
		t.Fatalf("held left %d", a.HeldMicro)
	}
	if a.BalanceMicro != 1000-int64(total)*30 {
		t.Fatalf("balance %d, captures %d", a.BalanceMicro, total)
	}
	t.Logf("captures=%d balance=%d", total, a.BalanceMicro)
	// Re-hold a captured id: it is accepted and a second capture charges 0.
	for i := range n {
		if captured[i] == 1 {
			id := fmt.Sprintf("r%d", i)
			if _, err := l.Hold(ctx, "user:x", id, 50); err != nil {
				t.Fatal(err)
			}
			_, _ = l.Capture(ctx, id, 30, "")
			b, _ := l.Account(ctx, "user:x")
			if b.BalanceMicro != a.BalanceMicro || b.HeldMicro != 0 {
				t.Fatalf("re-hold charged again: %+v", b)
			}
			break
		}
	}
}
