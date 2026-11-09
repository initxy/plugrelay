package store_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/initxy/plugrelay/internal/store"
)

// Every subscription and balance operation at once, on three users with
// keys: none fails (a deadlock is not retried away silently here: it would
// surface after the retries), and the books add up afterwards.
func TestSubscriptionLocking(t *testing.T) { onEveryDB(t, subscriptionLocking) }

func subscriptionLocking(t *testing.T, open opener) {
	ctx := context.Background()
	s := open(t, newBox(t))
	l, ss := ledgerOf(t, s), subsOf(t, s)
	users := []string{"u1", "u2", "u3"}
	keys := map[string][]string{}
	for _, u := range users {
		must(t, s.CreateUser(ctx, &store.User{ID: u, Name: u, Email: u + "@example.com", Role: store.RoleUser, GroupID: "free"}, "correct-horse-9"))
		for k := range 2 {
			kk := &store.Key{Name: fmt.Sprintf("%s-k%d", u, k), UserID: u, GroupID: "free"}
			_, err := s.CreateKey(ctx, kk)
			must(t, err)
			keys[u] = append(keys[u], kk.ID)
		}
		_, _, err := l.Credit(ctx, &store.BalanceTxn{ID: "topup:init:" + u, Subject: "user:" + u, Kind: store.TxnTopup, AmountMicro: 50_000_000})
		must(t, err)
	}
	plan := func(u, renewal, provider string) *store.Subscription {
		return &store.Subscription{UserID: u, Payer: "user:" + u, PlanID: "p", PlanName: "Pro", Period: "month",
			PriceMicro: 1_000_000, Unit: store.UnitMoney, Allowance: 500_000, Overage: store.OverageBalance, GroupID: "pro",
			Renewal: renewal, Provider: provider}
	}
	var mu sync.Mutex
	ids := map[string][]string{}
	failures := map[string]error{}
	note := func(op string, err error) {
		if err == nil || errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) ||
			errors.Is(err, store.ErrInsufficientBalance) || errors.Is(err, store.ErrAllowanceExhausted) {
			return
		}
		mu.Lock()
		failures[op] = err
		mu.Unlock()
	}
	pick := func(u string) string {
		mu.Lock()
		defer mu.Unlock()
		if len(ids[u]) == 0 {
			return ""
		}
		return ids[u][rand.IntN(len(ids[u]))]
	}
	var seq sync.Mutex
	n := 0
	next := func() int {
		seq.Lock()
		defer seq.Unlock()
		n++
		return n
	}
	var wg sync.WaitGroup
	deadline := time.Now().Add(3 * time.Second)
	for w := range 18 {
		wg.Go(func() {
			u := users[w%len(users)]
			for time.Now().Before(deadline) {
				now := time.Now()
				switch rand.IntN(10) {
				case 0: // subscribe from the balance
					sub := plan(u, store.RenewBalance, "balance")
					if err := ss.CreateSubscription(ctx, sub); err != nil {
						note("create", err)
						continue
					}
					mu.Lock()
					ids[u] = append(ids[u], sub.ID)
					mu.Unlock()
					_, err := ss.StartPeriod(ctx, sub.ID, now, now.Add(time.Hour), &store.BalanceTxn{ID: "subscription:" + sub.ID + ":first", AmountMicro: -1_000_000})
					note("start from balance", err)
				case 1: // subscribe at a provider
					sub := plan(u, store.RenewProvider, "stripe")
					if err := ss.CreateSubscription(ctx, sub); err != nil {
						note("create", err)
						continue
					}
					mu.Lock()
					ids[u] = append(ids[u], sub.ID)
					mu.Unlock()
				case 2: // the provider pays, maybe late or twice
					if id := pick(u); id != "" {
						_, err := ss.StartPeriod(ctx, id, now, now.Add(time.Hour), &store.BalanceTxn{ID: fmt.Sprintf("subscription:stripe:in_%s_%d", id, rand.IntN(3))})
						note("start at provider", err)
					}
				case 3:
					if id := pick(u); id != "" {
						_, err := ss.EndSubscription(ctx, id)
						note("end", err)
					}
				case 4:
					_, err := ss.RenewDue(ctx, now.Add(time.Duration(rand.IntN(3))*time.Hour))
					note("renew", err)
				case 5, 6: // a request on the plan, else on the balance
					rid := fmt.Sprintf("r%d", next())
					h, err := ss.SubscriptionHold(ctx, u, "user:"+u, rid, store.SubEstimate{Money: 100_000}, now)
					note("subscription hold", err)
					switch {
					case err == nil && h.Covered && rand.IntN(4) == 0:
						_, err = ss.SubscriptionRelease(ctx, rid)
						note("subscription release", err)
					case err == nil && h.Covered:
						_, err = ss.SubscriptionSettle(ctx, rid, int64(50_000+rand.IntN(200_000)), 0, 0, "m")
						note("subscription settle", err)
					case err == nil:
						if _, err = l.Hold(ctx, "user:"+u, rid, 100_000); err == nil {
							_, err = l.Capture(ctx, rid, 80_000, "m")
						}
						note("hold and capture", err)
					}
				case 7:
					rid := fmt.Sprintf("b%d", next())
					_, err := l.Hold(ctx, "user:"+u, rid, 100_000)
					if err == nil {
						_, err = l.Capture(ctx, rid, 90_000, "m")
					}
					note("hold and capture", err)
				case 8:
					for _, k := range keys[u] {
						s.TouchKey(k, now)
					}
					note("flush touches", s.FlushTouches(ctx))
					_, err := l.ExpireHolds(ctx, now.Add(-time.Second))
					note("expire holds", err)
				case 9:
					_, _, err := l.Credit(ctx, &store.BalanceTxn{ID: fmt.Sprintf("topup:%d", next()), Subject: "user:" + u, Kind: store.TxnTopup, AmountMicro: 1_000_000})
					note("top-up", err)
				}
			}
		})
	}
	wg.Wait()
	for op, err := range failures {
		t.Errorf("%s: %v", op, err)
	}
	_, err := l.ExpireHolds(ctx, time.Now().Add(time.Hour))
	must(t, err)
	for _, u := range users {
		var live []store.Subscription
		for _, st := range []string{store.SubPending, store.SubActive} {
			x, err := ss.ListSubscriptions(ctx, store.SubscriptionFilter{UserID: u, Status: st})
			must(t, err)
			live = append(live, x...)
		}
		if len(live) > 1 {
			t.Errorf("%s: %d live subscriptions", u, len(live))
		}
		want := "free"
		for _, x := range live {
			if x.Status == store.SubActive {
				want = "pro"
				if x.Held != 0 || x.Used > x.Allowance {
					t.Errorf("%s: subscription held %d used %d", u, x.Held, x.Used)
				}
			}
		}
		usr, err := s.GetUser(ctx, u)
		must(t, err)
		if usr.GroupID != want {
			t.Errorf("%s: group %q, want %q", u, usr.GroupID, want)
		}
		for _, k := range keys[u] {
			kk, err := s.GetKey(ctx, k)
			must(t, err)
			if kk.GroupID != usr.GroupID {
				t.Errorf("%s: key group %q, user's %q", u, kk.GroupID, usr.GroupID)
			}
		}
		a, err := l.Account(ctx, "user:"+u)
		must(t, err)
		var sum int64
		cur := ""
		for {
			txns, next, err := l.Transactions(ctx, store.TxnQuery{Subject: "user:" + u, Limit: 500, Cursor: cur})
			must(t, err)
			for _, x := range txns {
				sum += x.AmountMicro
			}
			if cur = next; cur == "" {
				break
			}
		}
		if a.HeldMicro != 0 || a.BalanceMicro != sum {
			t.Errorf("%s: balance %d held %d, transactions sum %d", u, a.BalanceMicro, a.HeldMicro, sum)
		}
	}
}

// A token plan covers tokens only: unit-priced requests (images) pay from
// the balance, refused there like without a plan, whatever the overage;
// a request settled with a charge and no tokens pays all of it.
func TestSubscriptionTokenPlanUnitPriced(t *testing.T) {
	onEveryDB(t, func(t *testing.T, open opener) {
		ctx := context.Background()
		for _, ov := range []string{store.OverageBlock, store.OverageBalance} {
			t.Run(ov, func(t *testing.T) {
				s := open(t, newBox(t))
				l, ss := ledgerOf(t, s), subsOf(t, s)
				must(t, s.CreateUser(ctx, &store.User{ID: "u1", Name: "u", Email: "u@example.com", Role: store.RoleUser}, "correct-horse-9"))
				_, _, err := l.Credit(ctx, &store.BalanceTxn{ID: "topup:1", Subject: "user:u1", Kind: store.TxnTopup, AmountMicro: 10_000_000})
				must(t, err)
				sub := &store.Subscription{UserID: "u1", Payer: "user:u1", PlanID: "p", PlanName: "Pro", Period: "month",
					PriceMicro: 1, Unit: store.UnitTokens, Allowance: 100, Overage: ov, Renewal: store.RenewProvider, Provider: "stripe"}
				must(t, ss.CreateSubscription(ctx, sub))
				now := time.Now()
				_, err = ss.StartPeriod(ctx, sub.ID, now.Add(-time.Minute), now.Add(time.Hour), &store.BalanceTxn{ID: "subscription:stripe:in"})
				must(t, err)
				// 20 images at 0.04 on a 100-token plan.
				for i := range 20 {
					id := fmt.Sprintf("img%d", i)
					h, err := ss.SubscriptionHold(ctx, "u1", "user:u1", id, store.SubEstimate{Money: 40_000, Tokens: 1, UnitPriced: true}, time.Now())
					must(t, err)
					if h.Covered {
						t.Fatalf("image %d covered by the token plan", i)
					}
					_, err = l.Hold(ctx, "user:u1", id, 40_000)
					must(t, err)
					_, err = l.Capture(ctx, id, 40_000, "dall-e-3")
					must(t, err)
				}
				// Held as a token request but settled with no tokens.
				h, err := ss.SubscriptionHold(ctx, "u1", "user:u1", "odd", store.SubEstimate{Money: 40_000, Tokens: 1}, time.Now())
				must(t, err)
				if !h.Covered {
					t.Fatal("a token request was not covered")
				}
				_, err = ss.SubscriptionSettle(ctx, "odd", 40_000, 0, 0, "m")
				must(t, err)
				a, err := l.Account(ctx, "user:u1")
				must(t, err)
				got, err := ss.GetSubscription(ctx, sub.ID)
				must(t, err)
				if a.BalanceMicro != 10_000_000-21*40_000 || a.HeldMicro != 0 || got.Used != 0 || got.Held != 0 {
					t.Fatalf("balance %d held %d, plan used %d held %d", a.BalanceMicro, a.HeldMicro, got.Used, got.Held)
				}
				// A used-up token plan, blocking or not, still lets images
				// pay from the balance.
				_, err = ss.SubscriptionHold(ctx, "u1", "user:u1", "t", store.SubEstimate{Money: 1000, Tokens: 100}, time.Now())
				must(t, err)
				_, err = ss.SubscriptionSettle(ctx, "t", 1000, 0, 100, "m")
				must(t, err)
				h, err = ss.SubscriptionHold(ctx, "u1", "user:u1", "img-late", store.SubEstimate{Money: 40_000, UnitPriced: true}, time.Now())
				if err != nil || h.Covered {
					t.Fatalf("image past the tokens: %+v %v", h, err)
				}
			})
		}
	})
}

// Deleting a user ends the user's live subscriptions; one left live by an
// earlier deletion can still be ended, and renewing it ends it.
func TestSubscriptionUserDeleted(t *testing.T) {
	onEveryDB(t, func(t *testing.T, open opener) {
		ctx := context.Background()
		s := open(t, newBox(t))
		l, ss := ledgerOf(t, s), subsOf(t, s)
		start := func(u string) *store.Subscription {
			must(t, s.CreateUser(ctx, &store.User{ID: u, Name: u, Email: u + "@example.com", Role: store.RoleUser}, "correct-horse-9"))
			_, _, err := l.Credit(ctx, &store.BalanceTxn{ID: "topup:" + u, Subject: "user:" + u, Kind: store.TxnTopup, AmountMicro: 50_000_000})
			must(t, err)
			sub := &store.Subscription{UserID: u, Payer: "user:" + u, PlanID: "p", PlanName: "Pro", Period: "month",
				PriceMicro: 1_000_000, Unit: store.UnitMoney, Allowance: 500_000, Overage: store.OverageBalance, Renewal: store.RenewBalance, Provider: "balance"}
			must(t, ss.CreateSubscription(ctx, sub))
			now := time.Now()
			_, err = ss.StartPeriod(ctx, sub.ID, now.Add(-2*time.Hour), now.Add(-time.Hour), &store.BalanceTxn{ID: "subscription:" + sub.ID + ":first", AmountMicro: -1_000_000})
			must(t, err)
			return sub
		}
		status := func(id string) string {
			got, err := ss.GetSubscription(ctx, id)
			must(t, err)
			return got.Status
		}

		a := start("u1")
		must(t, s.DeleteUser(ctx, "u1"))
		if st := status(a.ID); st != store.SubEnded {
			t.Fatalf("after deleting the user: %s", st)
		}

		// A subscription still live although its user is gone (deleted
		// before deletion ended them): ending it works, renewal ends it.
		for i, end := range []func(id string) error{
			func(id string) error { _, err := ss.EndSubscription(ctx, id); return err },
			func(string) error { _, err := ss.RenewDue(ctx, time.Now()); return err },
		} {
			u := fmt.Sprintf("orphan%d", i)
			b := start(u)
			must(t, s.DeleteUser(ctx, u))
			reviveSub(t, s, b.ID)
			must(t, end(b.ID))
			if st := status(b.ID); st != store.SubEnded {
				t.Fatalf("orphan %d: %s", i, st)
			}
		}
	})
}

// reviveSub makes a subscription active again behind the store's back.
func reviveSub(t *testing.T, s store.Store, id string) {
	t.Helper()
	must(t, store.ExecForTest(s, `UPDATE subscriptions SET status = ? WHERE id = ?`, store.SubActive, id))
}

func TestDeadlockErrors(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&pgconn.PgError{Code: "40P01"}, true},
		{fmt.Errorf("store: move group: %w", &pgconn.PgError{Code: "40P01"}), true},
		{&pgconn.PgError{Code: "23505"}, false},
		{&mysql.MySQLError{Number: 1213}, true},
		{&mysql.MySQLError{Number: 1205}, false}, // a lock wait timeout is not retried
		{errors.New("deadlock"), false},
	} {
		if got := store.IsDeadlockForTest(c.err); got != c.want {
			t.Errorf("%v: %v", c.err, got)
		}
	}
}
