package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

func TestAddPeriodClampsToMonthEnd(t *testing.T) {
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 10, 30, 0, 0, time.UTC) }
	for _, c := range []struct {
		from   time.Time
		period string
		want   time.Time
	}{
		{d(2026, 1, 31), "month", d(2026, 2, 28)},
		{d(2028, 1, 31), "month", d(2028, 2, 29)},
		{d(2026, 3, 31), "month", d(2026, 4, 30)},
		{d(2026, 12, 31), "month", d(2027, 1, 31)},
		{d(2026, 1, 15), "month", d(2026, 2, 15)},
		{d(2028, 2, 29), "year", d(2029, 2, 28)},
		{d(2026, 1, 31), "week", d(2026, 2, 7)},
	} {
		if got := store.AddPeriod(c.from, c.period); !got.Equal(c.want) {
			t.Errorf("%v + %s = %v, want %v", c.from, c.period, got, c.want)
		}
	}
}

// TestSubscriptionsLive covers one live subscription per user and the
// allowance under concurrency, on every database.
func TestSubscriptionsLive(t *testing.T) { onEveryDB(t, subscriptionsLive) }

func subscriptionsLive(t *testing.T, open opener) {
	ctx := context.Background()
	type env struct {
		s  store.Store
		l  store.Ledger
		ss store.Subscriptions
	}
	setup := func(t *testing.T) env {
		s := open(t, newBox(t))
		must(t, s.CreateUser(ctx, &store.User{ID: "u1", Name: "Ann", Email: "ann@example.com", Role: store.RoleUser, GroupID: "free"}, "correct-horse-9"))
		return env{s: s, l: ledgerOf(t, s), ss: subsOf(t, s)}
	}
	plan := func(group, renewal, provider string) *store.Subscription {
		return &store.Subscription{UserID: "u1", Payer: "user:u1", PlanID: "p-" + group, PlanName: "Plan " + group, Period: "month",
			PriceMicro: 5_000_000, Unit: store.UnitMoney, Allowance: 1_000_000, Overage: store.OverageBlock, GroupID: group,
			Renewal: renewal, Provider: provider}
	}
	active := func(t *testing.T, e env, unit, overage string, allowance int64) *store.Subscription {
		t.Helper()
		sub := plan("", store.RenewProvider, "stripe")
		sub.Unit, sub.Overage, sub.Allowance = unit, overage, allowance
		must(t, e.ss.CreateSubscription(ctx, sub))
		now := time.Now()
		_, err := e.ss.StartPeriod(ctx, sub.ID, now.Add(-time.Minute), now.Add(time.Hour), &store.BalanceTxn{ID: "subscription:stripe:in_" + sub.ID})
		must(t, err)
		return sub
	}
	bal := func(t *testing.T, e env) int64 {
		t.Helper()
		a, err := e.l.Account(ctx, "user:u1")
		must(t, err)
		return a.BalanceMicro
	}
	topup := func(t *testing.T, e env, amount int64) {
		t.Helper()
		_, _, err := e.l.Credit(ctx, &store.BalanceTxn{ID: fmt.Sprintf("topup:%d", time.Now().UnixNano()), Subject: "user:u1",
			Kind: store.TxnTopup, AmountMicro: amount})
		must(t, err)
	}
	live := func(t *testing.T, e env) []store.Subscription {
		t.Helper()
		var out []store.Subscription
		for _, st := range []string{store.SubPending, store.SubActive} {
			ss, err := e.ss.ListSubscriptions(ctx, store.SubscriptionFilter{UserID: "u1", Status: st})
			must(t, err)
			out = append(out, ss...)
		}
		return out
	}

	t.Run("SubscribeAgainSupersedesPending", func(t *testing.T) {
		e := setup(t)
		topup(t, e, 20_000_000)
		a := plan("", store.RenewBalance, "epay")
		must(t, e.ss.CreateSubscription(ctx, a))
		b := plan("", store.RenewBalance, "balance")
		must(t, e.ss.CreateSubscription(ctx, b))
		if got, _ := e.ss.GetSubscription(ctx, a.ID); got.Status != store.SubEnded {
			t.Fatalf("superseded pending %+v", got)
		}
		// A superseded subscription is never paid from the balance.
		now := time.Now()
		_, err := e.ss.StartPeriod(ctx, a.ID, now, now.Add(time.Hour), &store.BalanceTxn{ID: "subscription:" + a.ID + ":first", AmountMicro: -5_000_000})
		if !errors.Is(err, store.ErrConflict) || bal(t, e) != 20_000_000 {
			t.Fatalf("balance start of an ended subscription: %v, balance %d", err, bal(t, e))
		}
		_, err = e.ss.StartPeriod(ctx, b.ID, now, now.Add(time.Hour), &store.BalanceTxn{ID: "subscription:" + b.ID + ":first", AmountMicro: -5_000_000})
		must(t, err)
		if err := e.ss.CreateSubscription(ctx, plan("", store.RenewBalance, "balance")); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("subscribing while active: %v", err)
		}
		if l := live(t, e); len(l) != 1 || l[0].ID != b.ID {
			t.Fatalf("live %+v", l)
		}
	})

	// Concurrent "subscribe from the balance" (create, then start): one
	// starts, the price is taken once.
	t.Run("ConcurrentBalanceSubscribe", func(t *testing.T) {
		for round := range 3 {
			e := setup(t)
			topup(t, e, 100_000_000)
			var wg sync.WaitGroup
			var started atomic.Int64
			for range 5 {
				wg.Go(func() {
					s := plan("pro", store.RenewBalance, "balance")
					if err := e.ss.CreateSubscription(ctx, s); err != nil {
						if !errors.Is(err, store.ErrConflict) {
							t.Error(err)
						}
						return
					}
					now := time.Now()
					_, err := e.ss.StartPeriod(ctx, s.ID, now, store.AddPeriod(now, "month"),
						&store.BalanceTxn{ID: "subscription:" + s.ID + ":first", AmountMicro: -5_000_000})
					if err == nil {
						started.Add(1)
					} else if !errors.Is(err, store.ErrConflict) {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			l := live(t, e)
			if started.Load() != 1 || len(l) != 1 || l[0].Status != store.SubActive || bal(t, e) != 95_000_000 {
				t.Fatalf("round %d: started %d, live %d, balance %d", round, started.Load(), len(l), bal(t, e))
			}
			if u, _ := e.s.GetUser(ctx, "u1"); u.GroupID != "pro" {
				t.Fatalf("group %q", u.GroupID)
			}
		}
	})

	// Two orders paid at a provider: the first starts, the second is
	// recorded and flagged for a refund; ending the first restores the
	// user's group.
	t.Run("SecondPaymentFlaggedForRefund", func(t *testing.T) {
		e := setup(t)
		a := plan("gold", store.RenewBalance, "epay")
		must(t, e.ss.CreateSubscription(ctx, a))
		b := plan("silver", store.RenewBalance, "epay")
		must(t, e.ss.CreateSubscription(ctx, b))
		now := time.Now()
		ok, err := e.ss.StartPeriod(ctx, a.ID, now, now.Add(time.Hour), &store.BalanceTxn{ID: "subscription:epay:A"})
		must(t, err)
		if !ok {
			t.Fatal("late payment of the superseded order not applied")
		}
		ok, err = e.ss.StartPeriod(ctx, b.ID, now, now.Add(time.Hour), &store.BalanceTxn{ID: "subscription:epay:B"})
		must(t, err)
		gotB, _ := e.ss.GetSubscription(ctx, b.ID)
		if !ok || gotB.Status != store.SubEnded || !gotB.RefundDue {
			t.Fatalf("second payment: %v %+v", ok, gotB)
		}
		if _, err := e.l.GetTxn(ctx, "subscription:epay:B"); err != nil {
			t.Fatalf("second payment not recorded: %v", err)
		}
		if l := live(t, e); len(l) != 1 || l[0].ID != a.ID {
			t.Fatalf("live %+v", l)
		}
		// The same notice again changes nothing.
		if ok, _ := e.ss.StartPeriod(ctx, b.ID, now, now.Add(time.Hour), &store.BalanceTxn{ID: "subscription:epay:B"}); ok {
			t.Fatal("replayed")
		}
		if u, _ := e.s.GetUser(ctx, "u1"); u.GroupID != "gold" {
			t.Fatalf("group while subscribed %q", u.GroupID)
		}
		_, err = e.ss.EndSubscription(ctx, a.ID)
		must(t, err)
		if u, _ := e.s.GetUser(ctx, "u1"); u.GroupID != "free" {
			t.Fatalf("group after the end %q", u.GroupID)
		}
		must(t, e.ss.SetRefundDue(ctx, b.ID, false))
		if got, _ := e.ss.GetSubscription(ctx, b.ID); got.RefundDue {
			t.Fatal("refund flag not cleared")
		}
	})

	t.Run("EndLeavesAdminMovedUserAlone", func(t *testing.T) {
		e := setup(t)
		key := &store.Key{Name: "k1", UserID: "u1", GroupID: "free"}
		_, err := e.s.CreateKey(ctx, key)
		must(t, err)
		a := plan("gold", store.RenewBalance, "epay")
		must(t, e.ss.CreateSubscription(ctx, a))
		_, err = e.ss.StartPeriod(ctx, a.ID, time.Now(), time.Now().Add(time.Hour), &store.BalanceTxn{ID: "subscription:epay:A"})
		must(t, err)
		u, _ := e.s.GetUser(ctx, "u1")
		u.GroupID = "vip"
		must(t, e.s.UpdateUser(ctx, u))
		_, err = e.ss.EndSubscription(ctx, a.ID)
		must(t, err)
		u, _ = e.s.GetUser(ctx, "u1")
		key, _ = e.s.GetKey(ctx, key.ID)
		if u.GroupID != "vip" || key.GroupID != "gold" {
			t.Fatalf("user %q key %q", u.GroupID, key.GroupID)
		}
	})

	// 50 concurrent requests on a 1000-token allowance, 100 tokens
	// estimated each: ten are covered, the rest refused (block) or sent to
	// the balance; the overrun is paid from the balance at the request's
	// rate.
	t.Run("TokenPlanConcurrent", func(t *testing.T) {
		for _, ov := range []string{store.OverageBlock, store.OverageBalance} {
			e := setup(t)
			topup(t, e, 10_000_000)
			sub := active(t, e, store.UnitTokens, ov, 1000)
			var covered, refused atomic.Int64
			var wg sync.WaitGroup
			cov := make([]bool, 50)
			for i := range 50 {
				wg.Go(func() {
					h, err := e.ss.SubscriptionHold(ctx, "u1", "user:u1", fmt.Sprintf("t%d", i), store.SubEstimate{Money: 1000, Tokens: 100}, time.Now())
					switch {
					case errors.Is(err, store.ErrAllowanceExhausted):
						refused.Add(1)
					case err != nil:
						t.Error(err)
					case h.Covered:
						covered.Add(1)
						cov[i] = true
					}
				})
			}
			wg.Wait()
			if covered.Load() != 10 || (ov == store.OverageBlock) != (refused.Load() == 40) {
				t.Fatalf("%s: covered %d refused %d", ov, covered.Load(), refused.Load())
			}
			for i := range 50 {
				if cov[i] {
					_, err := e.ss.SubscriptionSettle(ctx, fmt.Sprintf("t%d", i), 1500, 0, 150, "m")
					must(t, err)
				}
			}
			got, _ := e.ss.GetSubscription(ctx, sub.ID)
			// 1500 tokens used, 1000 by the allowance, 500 at 10 each.
			if got.Used != 1000 || got.Held != 0 || bal(t, e) != 10_000_000-5000 {
				t.Fatalf("%s: used %d held %d balance %d", ov, got.Used, got.Held, bal(t, e))
			}
		}
	})

	// With balance overage, a request larger than what is left is covered
	// only when the balance takes the rest.
	t.Run("OverageNeedsTheBalance", func(t *testing.T) {
		e := setup(t)
		active(t, e, store.UnitTokens, store.OverageBalance, 1000)
		h, err := e.ss.SubscriptionHold(ctx, "u1", "user:u1", "big", store.SubEstimate{Money: 3_000_000, Tokens: 3000}, time.Now())
		must(t, err)
		if h.Covered {
			t.Fatal("covered without a balance for the rest")
		}
		topup(t, e, 2_000_000)
		h, err = e.ss.SubscriptionHold(ctx, "u1", "user:u1", "big", store.SubEstimate{Money: 3_000_000, Tokens: 3000}, time.Now())
		must(t, err)
		if !h.Covered || h.Subscription.Held != 1000 {
			t.Fatalf("hold %+v", h)
		}
	})

	// Money plan, blocking, low estimates: every covered request is
	// charged in full, the allowance to its end and the rest as a debt on
	// the balance.
	t.Run("MoneyBlockLowEstimates", func(t *testing.T) {
		e := setup(t)
		sub := active(t, e, store.UnitMoney, store.OverageBlock, 1_000_000)
		var covered atomic.Int64
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Go(func() {
				h, err := e.ss.SubscriptionHold(ctx, "u1", "user:u1", fmt.Sprintf("m%d", i), store.SubEstimate{Money: 10_000}, time.Now())
				if err == nil && h.Covered {
					covered.Add(1)
				}
			})
		}
		wg.Wait()
		for i := range 50 {
			_, err := e.ss.SubscriptionSettle(ctx, fmt.Sprintf("m%d", i), 500_000, 0, 0, "m")
			must(t, err)
		}
		got, _ := e.ss.GetSubscription(ctx, sub.ID)
		total := covered.Load() * 500_000
		if got.Used != 1_000_000 || got.Held != 0 || bal(t, e) != -(total-1_000_000) {
			t.Fatalf("covered %d, used %d, balance %d", covered.Load(), got.Used, bal(t, e))
		}
	})

	// Concurrent holds, repeated settles and releases and sweeps on one
	// allowance: each request settles once, nothing stays held, the
	// allowance never goes past its amount and its ledger adds up.
	t.Run("Chaos", func(t *testing.T) {
		for round := range 3 {
			e := setup(t)
			sub := active(t, e, store.UnitMoney, store.OverageBalance, 1_000_000)
			topup(t, e, 10_000_000)
			const n = 40
			var wg sync.WaitGroup
			sem := make(chan struct{}, 16)
			cov := make([]bool, n)
			for i := range n {
				wg.Go(func() {
					sem <- struct{}{}
					defer func() { <-sem }()
					h, err := e.ss.SubscriptionHold(ctx, "u1", "user:u1", fmt.Sprintf("r%d", i), store.SubEstimate{Money: 50_000}, time.Now())
					if err != nil {
						t.Error(err)
						return
					}
					cov[i] = h.Covered
				})
			}
			wg.Wait()
			var recs []*plugin.Record
			settled := make([]atomic.Int64, n)
			for i := range n {
				if !cov[i] {
					continue
				}
				id := fmt.Sprintf("r%d", i)
				if i%4 == 0 {
					recs = append(recs, &plugin.Record{ID: id, Time: time.Now(), Model: "m", ChargeMicro: 60_000})
				}
				for k := range 5 {
					wg.Go(func() {
						sem <- struct{}{}
						defer func() { <-sem }()
						var ok bool
						var err error
						if k < 3 {
							ok, err = e.ss.SubscriptionSettle(ctx, id, 60_000, 0, 0, "m")
						} else {
							_, err = e.ss.SubscriptionRelease(ctx, id)
						}
						if err != nil {
							t.Error(err)
						}
						if ok {
							settled[i].Add(1)
						}
					})
				}
			}
			must(t, e.s.InsertRecords(ctx, recs))
			for range 4 {
				wg.Go(func() {
					if _, err := e.l.SettleRecorded(ctx, time.Now().Add(time.Hour)); err != nil {
						t.Error(err)
					}
					if _, err := e.l.ExpireHolds(ctx, time.Now().Add(time.Hour)); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			got, _ := e.ss.GetSubscription(ctx, sub.ID)
			for i := range n {
				if settled[i].Load() > 1 {
					t.Errorf("r%d settled %d times", i, settled[i].Load())
				}
			}
			txns, _, err := e.l.Transactions(ctx, store.TxnQuery{Subject: store.AllowanceSubject(sub.ID), Limit: 500})
			must(t, err)
			var drawn int64
			for _, x := range txns {
				if x.AmountMicro < 0 {
					drawn += -x.AmountMicro
				}
			}
			if got.Held != 0 || drawn != got.Used || got.Used > 1_000_000 {
				t.Fatalf("round %d: held %d used %d drawn %d", round, got.Held, got.Used, drawn)
			}
		}
	})

	// Several nodes renew the same subscription at once: charged once.
	t.Run("ConcurrentRenew", func(t *testing.T) {
		e := setup(t)
		topup(t, e, 100_000_000)
		s := plan("", store.RenewBalance, "balance")
		must(t, e.ss.CreateSubscription(ctx, s))
		start := time.Now().UTC().Add(-31 * 24 * time.Hour)
		_, err := e.ss.StartPeriod(ctx, s.ID, start, store.AddPeriod(start, "month"), &store.BalanceTxn{ID: "first"})
		must(t, err)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				if _, err := e.ss.RenewDue(ctx, time.Now()); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if b := bal(t, e); b != 95_000_000 {
			t.Fatalf("balance after concurrent renewals %d", b)
		}
	})

	// A renewal that finds no money and a provider payment for the next
	// period race: a period that was paid is never ended by the renewal.
	t.Run("RenewNeverEndsAPaidPeriod", func(t *testing.T) {
		for round := range 5 {
			e := setup(t)
			s := plan("gold", store.RenewBalance, "epay")
			must(t, e.ss.CreateSubscription(ctx, s))
			start := time.Now().UTC().Add(-31 * 24 * time.Hour)
			end := store.AddPeriod(start, "month")
			_, err := e.ss.StartPeriod(ctx, s.ID, start, end, &store.BalanceTxn{ID: "first"})
			must(t, err)
			var wg sync.WaitGroup
			var paid atomic.Bool
			wg.Go(func() {
				if _, err := e.ss.RenewDue(ctx, time.Now()); err != nil {
					t.Error(err)
				}
			})
			wg.Go(func() {
				now := time.Now()
				ok, err := e.ss.StartPeriod(ctx, s.ID, now, store.AddPeriod(now, "month"), &store.BalanceTxn{ID: "subscription:epay:next"})
				if err != nil {
					t.Error(err)
				}
				paid.Store(ok)
			})
			wg.Wait()
			got, _ := e.ss.GetSubscription(ctx, s.ID)
			if paid.Load() && (got.Status != store.SubActive || !got.PeriodEnd.After(time.Now())) {
				t.Fatalf("round %d: paid period ended: %+v", round, got)
			}
			u, _ := e.s.GetUser(ctx, "u1")
			if (got.Status == store.SubActive) != (u.GroupID == "gold") {
				t.Fatalf("round %d: status %s, group %q", round, got.Status, u.GroupID)
			}
		}
	})

	// One subscription that cannot be renewed does not stop the others.
	t.Run("RenewDueContinuesPastAFailure", func(t *testing.T) {
		e := setup(t)
		must(t, e.s.CreateUser(ctx, &store.User{ID: "u2", Name: "Bob", Email: "bob@example.com", Role: store.RoleUser}, "correct-horse-8"))
		start := time.Now().UTC().Add(-31 * 24 * time.Hour)
		mk := func(user string, cancel bool) *store.Subscription {
			s := plan("", store.RenewBalance, "balance")
			s.UserID, s.Payer = user, "user:"+user
			s.CancelAtPeriodEnd = cancel
			must(t, e.ss.CreateSubscription(ctx, s))
			_, err := e.ss.StartPeriod(ctx, s.ID, start, store.AddPeriod(start, "month"), &store.BalanceTxn{ID: "first:" + s.ID})
			must(t, err)
			return s
		}
		broken, fine := mk("u2", false), mk("u1", true)
		// A renewal charge with no payer fails.
		must(t, store.ExecForTest(e.s, `UPDATE subscriptions SET payer = '', price_micro = 0 WHERE id = ?`, broken.ID))
		n, err := e.ss.RenewDue(ctx, time.Now())
		if err == nil || n != 1 {
			t.Fatalf("renewed %d, err %v", n, err)
		}
		if got, _ := e.ss.GetSubscription(ctx, fine.ID); got.Status != store.SubEnded {
			t.Fatalf("the other subscription %+v", got)
		}
		if got, _ := e.ss.GetSubscription(ctx, broken.ID); got.Status != store.SubActive {
			t.Fatalf("the broken one %+v", got)
		}
	})
}
