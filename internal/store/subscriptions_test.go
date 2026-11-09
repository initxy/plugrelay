package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

func subsOf(t *testing.T, s store.Store) store.Subscriptions {
	t.Helper()
	ss, ok := store.SubscriptionsOf(s)
	if !ok {
		t.Fatal("store has no subscriptions")
	}
	return ss
}

func TestSubscriptions(t *testing.T) {
	onEveryDB(t, subscriptionConformance)
}

func subscriptionConformance(t *testing.T, open opener) {
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
	// newSub creates and activates a subscription for u1 for the hour
	// around now.
	newSub := func(t *testing.T, e env, unit, overage string, allowance int64) *store.Subscription {
		t.Helper()
		sub := &store.Subscription{UserID: "u1", Payer: "user:u1", PlanID: "p1", PlanName: "Pro", Period: "month",
			PriceMicro: 5_000_000, Unit: unit, Allowance: allowance, Overage: overage, Provider: "stripe", Renewal: store.RenewProvider}
		must(t, e.ss.CreateSubscription(ctx, sub))
		now := time.Now()
		ok, err := e.ss.StartPeriod(ctx, sub.ID, now.Add(-time.Minute), now.Add(time.Hour),
			&store.BalanceTxn{ID: "subscription:stripe:in_" + sub.ID})
		must(t, err)
		if !ok {
			t.Fatal("first period not applied")
		}
		got, err := e.ss.GetSubscription(ctx, sub.ID)
		must(t, err)
		return got
	}
	balance := func(t *testing.T, e env, subject string) int64 {
		t.Helper()
		a, err := e.l.Account(ctx, subject)
		must(t, err)
		return a.BalanceMicro
	}
	topup := func(t *testing.T, e env, subject string, amount int64) {
		t.Helper()
		_, _, err := e.l.Credit(ctx, &store.BalanceTxn{ID: fmt.Sprintf("topup:%d", time.Now().UnixNano()), Subject: subject,
			Kind: store.TxnTopup, AmountMicro: amount})
		must(t, err)
	}

	t.Run("CreateAndStartOnce", func(t *testing.T) {
		e := setup(t)
		sub := newSub(t, e, store.UnitMoney, store.OverageBlock, 1_000_000)
		if sub.Status != store.SubActive || !sub.Current(time.Now()) || sub.Used != 0 {
			t.Fatalf("subscription %+v", sub)
		}
		// The same invoice again (a replayed webhook) changes nothing.
		ok, err := e.ss.StartPeriod(ctx, sub.ID, time.Now(), time.Now().Add(48*time.Hour), &store.BalanceTxn{ID: "subscription:stripe:in_" + sub.ID})
		must(t, err)
		again, _ := e.ss.GetSubscription(ctx, sub.ID)
		if ok || !again.PeriodEnd.Equal(sub.PeriodEnd) {
			t.Fatalf("replayed period applied: %v %+v", ok, again)
		}
		// A second subscription while one is current is a conflict.
		err = e.ss.CreateSubscription(ctx, &store.Subscription{UserID: "u1", Payer: "user:u1", PlanID: "p2", Period: "month",
			Unit: store.UnitMoney, Overage: store.OverageBlock})
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("second subscription: %v", err)
		}
		// The period's allowance is in the ledger.
		txns, _, err := e.l.Transactions(ctx, store.TxnQuery{Subject: store.AllowanceSubject(sub.ID)})
		must(t, err)
		if len(txns) != 1 || txns[0].Kind != store.TxnAllowance || txns[0].AmountMicro != 1_000_000 {
			t.Fatalf("grant %+v", txns)
		}
	})

	t.Run("MoneyPlanBlocksOverage", func(t *testing.T) {
		e := setup(t)
		sub := newSub(t, e, store.UnitMoney, store.OverageBlock, 1_000_000)
		now := time.Now()
		h, err := e.ss.SubscriptionHold(ctx, "u1", "user:u1", "r1", store.SubEstimate{Money: 300_000}, now)
		must(t, err)
		if !h.Covered || h.Subscription.Held != 300_000 {
			t.Fatalf("hold %+v", h)
		}
		// Holding the same request again changes nothing.
		h, err = e.ss.SubscriptionHold(ctx, "u1", "user:u1", "r1", store.SubEstimate{Money: 300_000}, now)
		must(t, err)
		if h.Subscription.Held != 300_000 {
			t.Fatalf("second hold %+v", h)
		}
		ok, err := e.ss.SubscriptionSettle(ctx, "r1", 400_000, 0, 0, "m")
		must(t, err)
		if !ok {
			t.Fatal("settle not applied")
		}
		if ok, _ := e.ss.SubscriptionSettle(ctx, "r1", 400_000, 0, 0, "m"); ok {
			t.Fatal("settled twice")
		}
		if ok, _ := e.ss.SubscriptionRelease(ctx, "r1"); ok {
			t.Fatal("released after settle")
		}
		got, _ := e.ss.GetSubscription(ctx, sub.ID)
		if got.Used != 400_000 || got.Held != 0 {
			t.Fatalf("after settle %+v", got)
		}
		tx, err := e.l.GetTxn(ctx, "allowance:r1")
		must(t, err)
		if tx.AmountMicro != -400_000 || tx.BalanceAfterMicro != 600_000 || tx.Subject != store.AllowanceSubject(sub.ID) {
			t.Fatalf("allowance txn %+v", tx)
		}
		// A request that starts with allowance left holds what is left and
		// may run past it: the allowance pays to zero, the rest is a debt
		// on the balance. The next is refused.
		h, err = e.ss.SubscriptionHold(ctx, "u1", "user:u1", "r2", store.SubEstimate{Money: 2_000_000}, now)
		must(t, err)
		if !h.Covered || h.Subscription.Held != 600_000 {
			t.Fatalf("hold of what is left %+v", h)
		}
		_, err = e.ss.SubscriptionSettle(ctx, "r2", 900_000, 0, 0, "m")
		must(t, err)
		h, err = e.ss.SubscriptionHold(ctx, "u1", "user:u1", "r3", store.SubEstimate{Money: 1}, now)
		if !errors.Is(err, store.ErrAllowanceExhausted) || h == nil || h.Subscription.ID != sub.ID {
			t.Fatalf("exhausted hold: %+v %v", h, err)
		}
		got, _ = e.ss.GetSubscription(ctx, sub.ID)
		if b := balance(t, e, "user:u1"); b != -300_000 || got.Used != 1_000_000 {
			t.Fatalf("debt %d, used %d", b, got.Used)
		}
	})

	t.Run("MoneyPlanBillsOverage", func(t *testing.T) {
		e := setup(t)
		sub := newSub(t, e, store.UnitMoney, store.OverageBalance, 1_000_000)
		now := time.Now()
		_, err := e.ss.SubscriptionHold(ctx, "u1", "user:u1", "r1", store.SubEstimate{Money: 800_000}, now)
		must(t, err)
		_, err = e.ss.SubscriptionSettle(ctx, "r1", 1_300_000, 0, 0, "m")
		must(t, err)
		got, _ := e.ss.GetSubscription(ctx, sub.ID)
		if got.Used != 1_000_000 || balance(t, e, "user:u1") != -300_000 {
			t.Fatalf("used %d, balance %d", got.Used, balance(t, e, "user:u1"))
		}
		c, err := e.l.GetTxn(ctx, "charge:r1")
		must(t, err)
		if c.Kind != store.TxnCharge || c.AmountMicro != -300_000 {
			t.Fatalf("overage txn %+v", c)
		}
		// Used up: requests pay from the balance as without a plan.
		h, err := e.ss.SubscriptionHold(ctx, "u1", "user:u1", "r2", store.SubEstimate{Money: 1}, now)
		must(t, err)
		if h.Covered {
			t.Fatal("covered past the allowance")
		}
	})

	t.Run("TokenPlan", func(t *testing.T) {
		e := setup(t)
		sub := newSub(t, e, store.UnitTokens, store.OverageBlock, 1000)
		now := time.Now()
		h, err := e.ss.SubscriptionHold(ctx, "u1", "user:u1", "r1", store.SubEstimate{Money: 5_000_000, Tokens: 600}, now)
		must(t, err)
		if !h.Covered || h.Subscription.Held != 600 {
			t.Fatalf("token hold %+v", h)
		}
		// 1200 tokens for 6.0: 1000 from the allowance, 200 at the
		// request's rate (1.0) from the balance.
		_, err = e.ss.SubscriptionSettle(ctx, "r1", 6_000_000, 0, 1200, "m")
		must(t, err)
		got, _ := e.ss.GetSubscription(ctx, sub.ID)
		if got.Used != 1000 || got.Held != 0 || balance(t, e, "user:u1") != -1_000_000 {
			t.Fatalf("token settle %+v, balance %d", got, balance(t, e, "user:u1"))
		}
		if _, err := e.ss.SubscriptionHold(ctx, "u1", "user:u1", "r2", store.SubEstimate{Money: 1}, now); !errors.Is(err, store.ErrAllowanceExhausted) {
			t.Fatalf("exhausted token plan: %v", err)
		}
	})

	t.Run("TokenPlanUnitFees", func(t *testing.T) {
		e := setup(t)
		sub := newSub(t, e, store.UnitTokens, store.OverageBlock, 1000)
		now := time.Now()
		_, err := e.ss.SubscriptionHold(ctx, "u1", "user:u1", "r1", store.SubEstimate{Money: 3_000_000, Tokens: 600}, now)
		must(t, err)
		// 500 tokens, all within the allowance, plus 1.0 of search units:
		// the units are paid from the balance, not covered by the tokens.
		_, err = e.ss.SubscriptionSettle(ctx, "r1", 3_000_000, 1_000_000, 500, "m")
		must(t, err)
		got, _ := e.ss.GetSubscription(ctx, sub.ID)
		if got.Used != 500 || balance(t, e, "user:u1") != -1_000_000 {
			t.Fatalf("settle %+v, balance %d", got, balance(t, e, "user:u1"))
		}
		// The sweep finds the unit part in the request log.
		_, err = e.ss.SubscriptionHold(ctx, "u1", "user:u1", "r2", store.SubEstimate{Money: 1_000_000, Tokens: 100}, now)
		must(t, err)
		rates := plugin.Rates{Input: 1_000_000, Units: map[string]int64{plugin.UnitSearch: 500_000}}
		must(t, e.s.InsertRecords(ctx, []*plugin.Record{{ID: "r2", Time: now, Model: "m", ChargeMicro: 1_100_000,
			Usage:    ir.Usage{Input: 100, SearchUnits: 2},
			Attempts: []plugin.AttemptRecord{{N: 1, Charge: &plugin.Charge{Rates: rates, Multiplier: 1}}}}}))
		_, err = e.l.SettleRecorded(ctx, now.Add(time.Minute))
		must(t, err)
		got, _ = e.ss.GetSubscription(ctx, sub.ID)
		if got.Used != 600 || balance(t, e, "user:u1") != -2_000_000 {
			t.Fatalf("sweep %+v, balance %d", got, balance(t, e, "user:u1"))
		}
	})

	t.Run("ReleaseAndSweep", func(t *testing.T) {
		e := setup(t)
		sub := newSub(t, e, store.UnitMoney, store.OverageBlock, 1_000_000)
		now := time.Now()
		for _, id := range []string{"a", "b", "c"} {
			_, err := e.ss.SubscriptionHold(ctx, "u1", "user:u1", id, store.SubEstimate{Money: 100_000}, now)
			must(t, err)
		}
		ok, err := e.ss.SubscriptionRelease(ctx, "a")
		must(t, err)
		if !ok {
			t.Fatal("release not applied")
		}
		// "b" finished but its settle failed: the sweep settles it from the
		// request log; "c" has no record and is released by the TTL sweep.
		must(t, e.s.InsertRecords(ctx, []*plugin.Record{{ID: "b", Time: now, Model: "m", ChargeMicro: 70_000, Usage: ir.Usage{Input: 10, Output: 5}}}))
		n, err := e.l.SettleRecorded(ctx, now.Add(time.Minute))
		must(t, err)
		if n != 1 {
			t.Fatalf("settled %d", n)
		}
		_, err = e.l.ExpireHolds(ctx, now.Add(time.Minute))
		must(t, err)
		got, _ := e.ss.GetSubscription(ctx, sub.ID)
		if got.Used != 70_000 || got.Held != 0 {
			t.Fatalf("after sweep %+v", got)
		}
		// A record of "c" landing after the expiry still draws, once.
		must(t, e.s.InsertRecords(ctx, []*plugin.Record{{ID: "c", Time: now, Model: "m", ChargeMicro: 30_000}}))
		_, err = e.l.SettleRecorded(ctx, now.Add(time.Minute))
		must(t, err)
		_, err = e.l.SettleRecorded(ctx, now.Add(time.Minute))
		must(t, err)
		got, _ = e.ss.GetSubscription(ctx, sub.ID)
		if got.Used != 100_000 || got.Held != 0 {
			t.Fatalf("late record %+v", got)
		}
	})

	t.Run("PlanGroup", func(t *testing.T) {
		e := setup(t)
		_, err := e.s.CreateKey(ctx, &store.Key{Name: "k1", UserID: "u1", GroupID: "free"})
		must(t, err)
		_, err = e.s.CreateKey(ctx, &store.Key{Name: "k2", UserID: "u1", GroupID: "team"})
		must(t, err)
		sub := &store.Subscription{UserID: "u1", Payer: "user:u1", PlanID: "p1", Period: "month", Unit: store.UnitMoney,
			Allowance: 1, Overage: store.OverageBlock, GroupID: "pro", Renewal: store.RenewBalance, Provider: "balance"}
		must(t, e.ss.CreateSubscription(ctx, sub))
		_, err = e.ss.StartPeriod(ctx, sub.ID, time.Now(), time.Now().Add(time.Hour), &store.BalanceTxn{ID: "subscription:x:" + sub.ID})
		must(t, err)
		groups := func() (string, map[string]string) {
			u, err := e.s.GetUser(ctx, "u1")
			must(t, err)
			keys, err := e.s.ListKeys(ctx, store.KeyFilter{UserID: "u1"})
			must(t, err)
			m := map[string]string{}
			for _, k := range keys {
				m[k.Name] = k.GroupID
			}
			return u.GroupID, m
		}
		if g, k := groups(); g != "pro" || k["k1"] != "pro" || k["k2"] != "team" {
			t.Fatalf("while subscribed: %s %v", g, k)
		}
		ended, err := e.ss.EndSubscription(ctx, sub.ID)
		must(t, err)
		if g, k := groups(); !ended || g != "free" || k["k1"] != "free" || k["k2"] != "team" {
			t.Fatalf("after end: %v %s %v", ended, g, k)
		}
		if again, _ := e.ss.EndSubscription(ctx, sub.ID); again {
			t.Fatal("ended twice")
		}
	})

	t.Run("RenewDue", func(t *testing.T) {
		e := setup(t)
		must(t, e.s.CreateUser(ctx, &store.User{ID: "u2", Name: "Bob", Email: "bob@example.com", Role: store.RoleUser}, "correct-horse-8"))
		must(t, e.s.CreateUser(ctx, &store.User{ID: "u3", Name: "Cy", Email: "cy@example.com", Role: store.RoleUser}, "correct-horse-7"))
		must(t, e.s.CreateUser(ctx, &store.User{ID: "u4", Name: "Di", Email: "di@example.com", Role: store.RoleUser}, "correct-horse-6"))
		start := time.Now().Add(-31 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
		mk := func(user, renewal string) *store.Subscription {
			sub := &store.Subscription{UserID: user, Payer: "user:" + user, PlanID: "p", PlanName: "Basic", Period: "month",
				PriceMicro: 2_000_000, Unit: store.UnitMoney, Allowance: 1_000_000, Overage: store.OverageBlock, Renewal: renewal, Provider: "balance"}
			must(t, e.ss.CreateSubscription(ctx, sub))
			_, err := e.ss.StartPeriod(ctx, sub.ID, start, store.AddPeriod(start, "month"), &store.BalanceTxn{ID: "subscription:first:" + sub.ID})
			must(t, err)
			return sub
		}
		paid := mk("u1", store.RenewBalance)
		topup(t, e, "user:u1", 3_000_000)
		broke := mk("u2", store.RenewBalance)
		canceled := mk("u3", store.RenewBalance)
		must(t, e.ss.SetCancelAtPeriodEnd(ctx, canceled.ID, true))
		stripe := mk("u4", store.RenewProvider)
		now := time.Now()
		n, err := e.ss.RenewDue(ctx, now)
		must(t, err)
		if n != 3 {
			t.Fatalf("changed %d", n)
		}
		p, _ := e.ss.GetSubscription(ctx, paid.ID)
		end := store.AddPeriod(start, "month")
		if p.Status != store.SubActive || !p.PeriodStart.Equal(end) || !p.PeriodEnd.Equal(store.AddPeriod(end, "month")) || balance(t, e, "user:u1") != 1_000_000 {
			t.Fatalf("renewed %+v, balance %d", p, balance(t, e, "user:u1"))
		}
		for _, id := range []string{broke.ID, canceled.ID} {
			if s, _ := e.ss.GetSubscription(ctx, id); s.Status != store.SubEnded {
				t.Fatalf("%s not ended: %+v", id, s)
			}
		}
		if s, _ := e.ss.GetSubscription(ctx, stripe.ID); s.Status != store.SubActive {
			t.Fatalf("provider subscription ended inside the grace: %+v", s)
		}
		// Running again renews nothing twice; past the grace the provider
		// subscription ends.
		n, err = e.ss.RenewDue(ctx, now)
		must(t, err)
		if n != 0 || balance(t, e, "user:u1") != 1_000_000 {
			t.Fatalf("second run changed %d", n)
		}
		_, err = e.ss.RenewDue(ctx, now.Add(store.ProviderGrace+time.Hour))
		must(t, err)
		if s, _ := e.ss.GetSubscription(ctx, stripe.ID); s.Status != store.SubEnded {
			t.Fatalf("provider subscription past the grace: %+v", s)
		}
	})

	// Concurrent requests on one allowance: every charge lands once, the
	// allowance pays exactly its amount and the balance the rest.
	t.Run("Concurrent", func(t *testing.T) {
		e := setup(t)
		sub := newSub(t, e, store.UnitMoney, store.OverageBalance, 1_000_000)
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := fmt.Sprintf("c%d", i)
				h, err := e.ss.SubscriptionHold(ctx, "u1", "user:u1", id, store.SubEstimate{Money: 100_000}, time.Now())
				if err != nil {
					t.Error(err)
					return
				}
				if !h.Covered {
					if _, err := e.l.Charge(ctx, "user:u1", id, 100_000, "m"); err != nil {
						t.Error(err)
					}
					return
				}
				if _, err := e.ss.SubscriptionSettle(ctx, id, 100_000, 0, 0, "m"); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		got, _ := e.ss.GetSubscription(ctx, sub.ID)
		if got.Used != 1_000_000 || got.Held != 0 || balance(t, e, "user:u1") != -1_000_000 {
			t.Fatalf("used %d held %d balance %d", got.Used, got.Held, balance(t, e, "user:u1"))
		}
	})
}
