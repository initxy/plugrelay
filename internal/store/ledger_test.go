package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

func TestLedgerSQLite(t *testing.T) { ledgerConformance(t, sqliteOpener) }
func TestLedgerMySQL(t *testing.T)  { ledgerConformance(t, mysqlFromEnv(t)) }

func TestLedgerPostgres(t *testing.T) {
	dsn := os.Getenv("PLUGRELAY_TEST_PG")
	if dsn == "" {
		t.Skip("PLUGRELAY_TEST_PG not set")
	}
	ledgerConformance(t, pgOpener(dsn))
}

func ledgerOf(t *testing.T, s store.Store) store.Ledger {
	t.Helper()
	l, ok := store.LedgerOf(s)
	if !ok {
		t.Fatal("store has no ledger")
	}
	return l
}

func ledgerConformance(t *testing.T, open opener) {
	ctx := context.Background()
	newLedger := func(t *testing.T) store.Ledger { return ledgerOf(t, open(t, newBox(t))) }
	topup := func(t *testing.T, l store.Ledger, subject string, amount int64) {
		t.Helper()
		if _, _, err := l.Credit(ctx, &store.BalanceTxn{ID: fmt.Sprintf("topup:%s:%d", subject, time.Now().UnixNano()),
			Subject: subject, Kind: store.TxnTopup, AmountMicro: amount}); err != nil {
			t.Fatal(err)
		}
	}
	balance := func(t *testing.T, l store.Ledger, subject string) store.BalanceAccount {
		t.Helper()
		a, err := l.Account(ctx, subject)
		if err != nil {
			t.Fatal(err)
		}
		return *a
	}

	t.Run("HoldCaptureRelease", func(t *testing.T) {
		l := newLedger(t)
		if _, err := l.Hold(ctx, "user:u", "r0", 1); !errors.Is(err, store.ErrInsufficientBalance) {
			t.Fatalf("empty account hold: %v", err)
		}
		topup(t, l, "user:u", 1000)
		a, err := l.Hold(ctx, "user:u", "r1", 300)
		must(t, err)
		if a.HeldMicro != 300 || a.Available() != 700 {
			t.Fatalf("after hold: %+v", a)
		}
		// A second hold for the same request changes nothing.
		a, err = l.Hold(ctx, "user:u", "r1", 300)
		must(t, err)
		if a.HeldMicro != 300 {
			t.Fatalf("double hold: %+v", a)
		}
		if _, err := l.Hold(ctx, "user:u", "r2", 701); !errors.Is(err, store.ErrInsufficientBalance) {
			t.Fatalf("over-hold: %v", err)
		}
		ok, err := l.Capture(ctx, "r1", 250, "gpt")
		must(t, err)
		if !ok {
			t.Fatal("first capture not applied")
		}
		for _, again := range []func() (bool, error){
			func() (bool, error) { return l.Capture(ctx, "r1", 250, "gpt") },
			func() (bool, error) { return l.Release(ctx, "r1") },
		} {
			if ok, err := again(); err != nil || ok {
				t.Fatalf("settled twice: %v %v", ok, err)
			}
		}
		if a := balance(t, l, "user:u"); a.BalanceMicro != 750 || a.HeldMicro != 0 {
			t.Fatalf("after capture: %+v", a)
		}
		tx, err := l.GetTxn(ctx, "charge:r1")
		must(t, err)
		if tx.AmountMicro != -250 || tx.BalanceAfterMicro != 750 || tx.Kind != store.TxnCharge || tx.RequestID != "r1" {
			t.Fatalf("charge txn: %+v", tx)
		}
		// Refund path: the hold goes back, nothing is charged.
		_, err = l.Hold(ctx, "user:u", "r3", 500)
		must(t, err)
		ok, err = l.Release(ctx, "r3")
		must(t, err)
		if !ok {
			t.Fatal("release not applied")
		}
		if ok, _ := l.Capture(ctx, "r3", 500, ""); ok {
			t.Fatal("capture after release applied")
		}
		if a := balance(t, l, "user:u"); a.BalanceMicro != 750 || a.HeldMicro != 0 {
			t.Fatalf("after release: %+v", a)
		}
	})

	t.Run("CreditLimitAndNegativeCredit", func(t *testing.T) {
		l := newLedger(t)
		must(t, l.SetCreditLimit(ctx, "group:g", 100))
		_, err := l.Hold(ctx, "group:g", "r1", 100)
		must(t, err)
		if _, err := l.Hold(ctx, "group:g", "r2", 1); !errors.Is(err, store.ErrInsufficientBalance) {
			t.Fatalf("past credit limit: %v", err)
		}
		_, err = l.Capture(ctx, "r1", 100, "")
		must(t, err)
		if a := balance(t, l, "group:g"); a.BalanceMicro != -100 {
			t.Fatalf("credit used: %+v", a)
		}
		applied, _, err := l.Credit(ctx, &store.BalanceTxn{ID: "adj1", Subject: "group:g", Kind: store.TxnAdjustment, AmountMicro: -1})
		if applied || !errors.Is(err, store.ErrInsufficientBalance) {
			t.Fatalf("negative credit past limit: %v %v", applied, err)
		}
		if _, err := l.GetTxn(ctx, "adj1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("refused credit left a row: %v", err)
		}
		applied, a, err := l.Credit(ctx, &store.BalanceTxn{ID: "adj2", Subject: "group:g", Kind: store.TxnAdjustment, AmountMicro: 500, Actor: "admin"})
		must(t, err)
		if !applied || a.BalanceMicro != 400 {
			t.Fatalf("credit: %v %+v", applied, a)
		}
		applied, a, err = l.Credit(ctx, &store.BalanceTxn{ID: "adj2", Subject: "group:g", Kind: store.TxnAdjustment, AmountMicro: 500})
		must(t, err)
		if applied || a.BalanceMicro != 400 {
			t.Fatalf("replayed credit: %v %+v", applied, a)
		}
		txns, next, err := l.Transactions(ctx, store.TxnQuery{Subject: "group:g", Limit: 1})
		must(t, err)
		if len(txns) != 1 || next == "" {
			t.Fatalf("page 1: %+v %q", txns, next)
		}
		more, _, err := l.Transactions(ctx, store.TxnQuery{Subject: "group:g", Cursor: next})
		must(t, err)
		if len(more) != 1 || more[0].ID == txns[0].ID {
			t.Fatalf("page 2: %+v", more)
		}
	})

	// Work billed long after its hold (a batch) is charged without one,
	// below zero, once per id; a zero charge only releases.
	t.Run("ChargeWithoutHold", func(t *testing.T) {
		l := newLedger(t)
		topup(t, l, "user:b", 100)
		_, err := l.Hold(ctx, "user:b", "batch:h1", 80)
		must(t, err)
		ok, err := l.Charge(ctx, "user:b", "batch:h1", 0, "")
		must(t, err)
		if a := balance(t, l, "user:b"); !ok || a.HeldMicro != 0 || a.BalanceMicro != 100 {
			t.Fatalf("release: %v %+v", ok, a)
		}
		for i := range 2 {
			ok, err := l.Charge(ctx, "user:b", "batch:h1", 250, "claude")
			must(t, err)
			if ok != (i == 0) {
				t.Fatalf("charge %d applied %v", i, ok)
			}
		}
		if a := balance(t, l, "user:b"); a.BalanceMicro != -150 || a.HeldMicro != 0 {
			t.Fatalf("after charge: %+v", a)
		}
		// With a hold on another subject: the hold is released there.
		topup(t, l, "user:c", 100)
		_, err = l.Hold(ctx, "user:c", "batch:h2", 50)
		must(t, err)
		_, err = l.Charge(ctx, "user:b", "batch:h2", 10, "")
		must(t, err)
		if c, b := balance(t, l, "user:c"), balance(t, l, "user:b"); c.HeldMicro != 0 || c.BalanceMicro != 100 || b.BalanceMicro != -160 {
			t.Fatalf("holder %+v payer %+v", c, b)
		}
	})

	// Ids longer than their columns (MySQL) are stored hashed: they stay
	// distinct and idempotent.
	t.Run("LongIDs", func(t *testing.T) {
		l := newLedger(t)
		long := strings.Repeat("x", 400)
		topup(t, l, "user:l", 1000)
		for _, id := range []string{long + "a", long + "b"} {
			_, err := l.Hold(ctx, "user:l", id, 100)
			must(t, err)
		}
		if a := balance(t, l, "user:l"); a.HeldMicro != 200 {
			t.Fatalf("two long holds: %+v", a)
		}
		ok, err := l.Capture(ctx, long+"a", 30, "")
		must(t, err)
		if !ok {
			t.Fatal("long capture not applied")
		}
		if ok, _ := l.Capture(ctx, long+"a", 30, ""); ok {
			t.Fatal("long capture applied twice")
		}
		if _, err := l.GetTxn(ctx, "charge:"+long+"a"); err != nil {
			t.Fatalf("long charge txn: %v", err)
		}
		for range 2 {
			_, _, err := l.Credit(ctx, &store.BalanceTxn{ID: "topup:" + long, Subject: "user:l", Kind: store.TxnTopup, AmountMicro: 5})
			must(t, err)
		}
		if a := balance(t, l, "user:l"); a.BalanceMicro != 975 || a.HeldMicro != 100 {
			t.Fatalf("after long ids: %+v", a)
		}
	})

	// Batch holds outlast hold_ttl: a batch runs a day and is billed after.
	t.Run("BatchHoldsLastLonger", func(t *testing.T) {
		l := newLedger(t)
		topup(t, l, "user:y", 100)
		_, err := l.Hold(ctx, "user:y", "batch:z", 60)
		must(t, err)
		if n, err := l.ExpireHolds(ctx, time.Now().Add(time.Minute)); err != nil || n != 0 {
			t.Fatalf("batch hold expired early: %d %v", n, err)
		}
		if n, err := l.ExpireHolds(ctx, time.Now().Add(store.BatchHoldGrace+time.Minute)); err != nil || n != 1 {
			t.Fatalf("batch hold kept: %d %v", n, err)
		}
	})

	t.Run("ExpireHolds", func(t *testing.T) {
		l := newLedger(t)
		topup(t, l, "user:x", 100)
		_, err := l.Hold(ctx, "user:x", "r1", 60)
		must(t, err)
		n, err := l.ExpireHolds(ctx, time.Now().Add(time.Minute))
		must(t, err)
		if n != 1 {
			t.Fatalf("expired %d", n)
		}
		if a := balance(t, l, "user:x"); a.HeldMicro != 0 {
			t.Fatalf("held after expiry: %+v", a)
		}
		// A late settle still charges.
		ok, err := l.Capture(ctx, "r1", 40, "")
		must(t, err)
		if a := balance(t, l, "user:x"); !ok || a.BalanceMicro != 60 || a.HeldMicro != 0 {
			t.Fatalf("late capture: %v %+v", ok, a)
		}
	})

	// A settle that failed (database blip) is collected from the request
	// log by the sweep, once; a refunded request's hold is released.
	t.Run("SettleRecorded", func(t *testing.T) {
		s := open(t, newBox(t))
		l := ledgerOf(t, s)
		topup(t, l, "key:k", 1000)
		for _, id := range []string{"paid", "refunded", "running"} {
			_, err := l.Hold(ctx, "key:k", id, 300)
			must(t, err)
		}
		// Settle of "paid" failed; its record (charge 120) was written.
		must(t, s.InsertRecords(ctx, []*plugin.Record{
			{ID: "paid", Time: time.Now(), Model: "m1", ChargeMicro: 120},
			{ID: "refunded", Time: time.Now(), Model: "m1"},
		}))
		n, err := l.SettleRecorded(ctx, time.Now().Add(time.Minute))
		must(t, err)
		if a := balance(t, l, "key:k"); n != 2 || a.BalanceMicro != 880 || a.HeldMicro != 300 {
			t.Fatalf("settled %d: %+v", n, a)
		}
		c, err := l.GetTxn(ctx, "charge:paid")
		must(t, err)
		if c.AmountMicro != -120 || c.Subject != "key:k" || c.Note != "m1" {
			t.Fatalf("charge txn %+v", c)
		}
		// The late in-process retry and a second sweep change nothing.
		ok, err := l.Capture(ctx, "paid", 120, "m1")
		must(t, err)
		n, err = l.SettleRecorded(ctx, time.Now().Add(time.Minute))
		must(t, err)
		if a := balance(t, l, "key:k"); ok || n != 0 || a.BalanceMicro != 880 {
			t.Fatalf("second settle: %v %d %+v", ok, n, a)
		}
		// A hold released by the TTL sweep before its record arrived is
		// still charged when the record lands.
		_, err = l.ExpireHolds(ctx, time.Now().Add(time.Minute))
		must(t, err)
		must(t, s.InsertRecords(ctx, []*plugin.Record{{ID: "running", Time: time.Now(), Model: "m2", ChargeMicro: 50}}))
		_, err = l.SettleRecorded(ctx, time.Now().Add(time.Minute))
		must(t, err)
		if a := balance(t, l, "key:k"); a.BalanceMicro != 830 || a.HeldMicro != 0 {
			t.Fatalf("after expiry: %+v", a)
		}
	})

	// Concurrent requests never overdraw and every charge lands once.
	t.Run("ConcurrentHolds", func(t *testing.T) {
		l := newLedger(t)
		topup(t, l, "user:c", 1000)
		var wg sync.WaitGroup
		var admitted, refused atomic.Int64
		for i := range 40 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := fmt.Sprintf("c%d", i)
				if _, err := l.Hold(ctx, "user:c", id, 100); err != nil {
					if !errors.Is(err, store.ErrInsufficientBalance) {
						t.Error(err)
					}
					refused.Add(1)
					return
				}
				admitted.Add(1)
				// Settle and a duplicate settle race.
				var inner sync.WaitGroup
				for range 2 {
					inner.Add(1)
					go func() {
						defer inner.Done()
						if _, err := l.Capture(ctx, id, 100, ""); err != nil {
							t.Error(err)
						}
					}()
				}
				inner.Wait()
			}()
		}
		wg.Wait()
		a := balance(t, l, "user:c")
		if a.BalanceMicro < 0 || a.HeldMicro != 0 || a.BalanceMicro != 1000-100*admitted.Load() || admitted.Load() > 10 {
			t.Fatalf("balance %+v after %d admitted, %d refused", a, admitted.Load(), refused.Load())
		}
		txns, _, err := l.Transactions(ctx, store.TxnQuery{Subject: "user:c", Kind: store.TxnCharge, Limit: 100})
		must(t, err)
		if int64(len(txns)) != admitted.Load() {
			t.Fatalf("%d charge rows for %d requests", len(txns), admitted.Load())
		}
	})

	t.Run("Redeem", func(t *testing.T) {
		l := newLedger(t)
		now := time.Now()
		must(t, l.CreateRedeemCodes(ctx, []store.RedeemCode{
			{CodeHash: "h-once", Last4: "once", AmountMicro: 5_000_000},
			{CodeHash: "h-old", AmountMicro: 1, ExpiresAt: now.Add(-time.Second)},
			{CodeHash: "h-two", AmountMicro: 7, MaxUses: 2},
		}))
		tx, err := l.Redeem(ctx, "h-once", "user:a", "a", now)
		must(t, err)
		if tx.AmountMicro != 5_000_000 || tx.BalanceAfterMicro != 5_000_000 || tx.Kind != store.TxnRedeem {
			t.Fatalf("redeem txn: %+v", tx)
		}
		for _, c := range []struct {
			hash, subject string
			want          error
		}{
			{"h-once", "user:a", store.ErrRedeemAlreadyUsed},
			{"h-once", "user:b", store.ErrRedeemUsedUp},
			{"h-old", "user:a", store.ErrRedeemExpired},
			{"nope", "user:a", store.ErrRedeemInvalid},
		} {
			if _, err := l.Redeem(ctx, c.hash, c.subject, "", now); !errors.Is(err, c.want) {
				t.Errorf("%s by %s: %v, want %v", c.hash, c.subject, err, c.want)
			}
		}
		codes, err := l.ListRedeemCodes(ctx, "")
		must(t, err)
		var two string
		for _, c := range codes {
			if c.CodeHash == "h-two" {
				two = c.ID
			}
		}
		must(t, l.SetRedeemCodeDisabled(ctx, two, true))
		if _, err := l.Redeem(ctx, "h-two", "user:a", "", now); !errors.Is(err, store.ErrRedeemInvalid) {
			t.Fatalf("disabled code: %v", err)
		}
		must(t, l.SetRedeemCodeDisabled(ctx, two, false))

		// Races: many accounts on a two-use code, one account twice at once.
		var wg sync.WaitGroup
		var ok atomic.Int64
		for i := range 10 {
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := l.Redeem(ctx, "h-two", fmt.Sprintf("user:r%d", i), "", now)
					switch {
					case err == nil:
						ok.Add(1)
					case errors.Is(err, store.ErrRedeemUsedUp), errors.Is(err, store.ErrRedeemAlreadyUsed):
					default:
						t.Error(err)
					}
				}()
			}
		}
		wg.Wait()
		if ok.Load() != 2 {
			t.Fatalf("two-use code redeemed %d times", ok.Load())
		}
		codes, _ = l.ListRedeemCodes(ctx, "")
		for _, c := range codes {
			if c.ID == two && c.Uses != 2 {
				t.Fatalf("uses = %d", c.Uses)
			}
		}
		var total int64
		accts, err := l.ListAccounts(ctx)
		must(t, err)
		for _, a := range accts {
			if a.Subject != "user:a" {
				total += a.BalanceMicro
			}
		}
		if total != 14 {
			t.Fatalf("credited %d, want 14", total)
		}
	})
}
