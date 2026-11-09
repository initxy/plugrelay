package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
)

func TestOrders(t *testing.T) {
	onEveryDB(t, func(t *testing.T, open opener) {
		ctx := context.Background()
		l := ledgerOf(t, open(t, newBox(t)))
		base := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
		for i, o := range []store.PaymentOrder{
			{ID: "o1", Kind: store.OrderTopup, UserID: "u1", Subject: "user:u1", AmountMicro: 10e6, PayMicro: 10e6, Currency: "USD", Provider: "stripe"},
			{ID: "o2", Kind: store.OrderTopup, UserID: "u2", Subject: "user:u2", AmountMicro: 20e6, PayMicro: 18e6, DiscountMicro: 2e6, Currency: "USD", Provider: "epay", Method: "alipay"},
			{ID: "o3", Kind: store.OrderPlan, UserID: "u1", Subject: "user:u1", AmountMicro: 5e6, PayMicro: 5e6, Currency: "USD", Provider: "epay", Ref: "sub1"},
		} {
			o.CreatedAt = base.Add(time.Duration(i) * time.Minute)
			must(t, l.CreateOrder(ctx, &o))
		}
		ok, err := l.PayOrder(ctx, "o2", base.Add(time.Hour))
		must(t, err)
		again, err := l.PayOrder(ctx, "o2", base.Add(time.Hour))
		must(t, err)
		if !ok || again {
			t.Fatalf("pay %v, again %v", ok, again)
		}
		if ok, err := l.CancelOrder(ctx, "o2", true); err != nil || ok {
			t.Fatalf("cancel of a paid order: %v %v", ok, err)
		}
		if ok, err := l.CancelOrder(ctx, "o1", true); err != nil || !ok {
			t.Fatalf("cancel: %v %v", ok, err)
		}
		all, next, err := l.ListOrders(ctx, store.OrderQuery{Limit: 2})
		must(t, err)
		if len(all) != 2 || all[0].ID != "o3" || next == "" {
			t.Fatalf("%+v %q", all, next)
		}
		rest, _, err := l.ListOrders(ctx, store.OrderQuery{Cursor: next})
		must(t, err)
		if len(rest) != 1 || rest[0].ID != "o1" || rest[0].Status != store.OrderCanceled {
			t.Fatalf("%+v", rest)
		}
		mine, _, err := l.ListOrders(ctx, store.OrderQuery{UserID: "u1", Kind: store.OrderPlan})
		must(t, err)
		if len(mine) != 1 || mine[0].Ref != "sub1" {
			t.Fatalf("%+v", mine)
		}
		paid, _, err := l.ListOrders(ctx, store.OrderQuery{Status: store.OrderPaid, Provider: "epay"})
		must(t, err)
		if len(paid) != 1 || paid[0].PaidAt.IsZero() || paid[0].Method != "alipay" {
			t.Fatalf("%+v", paid)
		}
		tot, err := l.OrderTotals(ctx, base.Add(-time.Hour))
		must(t, err)
		if tot.PaidMicro != 18e6 || tot.PaidCount != 1 || tot.PendingCount != 1 {
			t.Fatalf("%+v", tot)
		}
	})
}

func TestCoupons(t *testing.T) {
	onEveryDB(t, func(t *testing.T, open opener) {
		ctx := context.Background()
		l := ledgerOf(t, open(t, newBox(t)))
		now := time.Now()
		if err := l.CreateCoupon(ctx, &store.Coupon{Code: "bad", Percent: 10, AmountMicro: 1e6}); err == nil {
			t.Fatal("a coupon with both a percent and an amount was accepted")
		}
		c := &store.Coupon{Code: " spring10 ", Percent: 10, MaxDiscountMicro: 3e6, MinTopupMicro: 5e6, MaxUses: 3, PerUser: 1}
		must(t, l.CreateCoupon(ctx, c))
		if c.Code != "SPRING10" {
			t.Fatal(c.Code)
		}
		if err := l.CreateCoupon(ctx, &store.Coupon{Code: "Spring10", AmountMicro: 1e6}); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate code: %v", err)
		}
		if _, _, err := l.CheckCoupon(ctx, "spring10", "u1", 4e6, now); !errors.Is(err, store.ErrCouponMinAmount) {
			t.Fatal(err)
		}
		_, d, err := l.CheckCoupon(ctx, "spring10", "u1", 12_345_678, now)
		must(t, err)
		if d != 1_230_000 { // 10 %, in whole cents
			t.Fatal(d)
		}
		if _, d, _ := l.CheckCoupon(ctx, "spring10", "u1", 100e6, now); d != 3e6 {
			t.Fatalf("cap: %d", d)
		}
		if _, _, err := l.CheckCoupon(ctx, "nope", "u1", 10e6, now); !errors.Is(err, store.ErrCouponInvalid) {
			t.Fatal(err)
		}

		// One use per user, three in all, under concurrency.
		var wg sync.WaitGroup
		var mu sync.Mutex
		won := 0
		for i := range 8 {
			wg.Go(func() {
				_, _, err := l.ReserveCoupon(ctx, "SPRING10", fmt.Sprintf("u%d", i), fmt.Sprintf("ord%d", i), 10e6, now)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					won++
				case !errors.Is(err, store.ErrCouponUsedUp):
					t.Errorf("reserve %d: %v", i, err)
				}
			})
		}
		wg.Wait()
		if won != 3 {
			t.Fatalf("%d checkouts got the coupon, want 3", won)
		}
		got, err := l.GetCoupon(ctx, c.ID)
		must(t, err)
		if got.Used != 0 || got.Pending != 3 {
			t.Fatalf("used %d pending %d", got.Used, got.Pending)
		}

		c2 := &store.Coupon{Code: "FIVE", AmountMicro: 5e6, PerUser: 1}
		must(t, l.CreateCoupon(ctx, c2))
		if _, _, err := l.CheckCoupon(ctx, "five", "u1", 5e6, now); !errors.Is(err, store.ErrCouponMinAmount) {
			t.Fatalf("a discount of the whole amount: %v", err)
		}
		must(t, l.CreateOrder(ctx, &store.PaymentOrder{ID: "p1", Kind: store.OrderTopup, UserID: "u1", Subject: "user:u1",
			AmountMicro: 20e6, PayMicro: 15e6, DiscountMicro: 5e6, CouponID: c2.ID, CouponCode: "FIVE", Currency: "USD", Provider: "stripe"}))
		if _, _, err := l.ReserveCoupon(ctx, "five", "u1", "p1", 20e6, now); err != nil {
			t.Fatal(err)
		}
		if _, _, err := l.ReserveCoupon(ctx, "five", "u1", "p2", 20e6, now); !errors.Is(err, store.ErrCouponAlreadyUsed) {
			t.Fatalf("second use by the same user: %v", err)
		}
		// A cancel whose checkout may still be paid keeps the use for the hold.
		_, err = l.CancelOrder(ctx, "p1", false)
		must(t, err)
		if _, _, err := l.CheckCoupon(ctx, "five", "u1", 20e6, now); !errors.Is(err, store.ErrCouponAlreadyUsed) {
			t.Fatalf("after a held cancel: %v", err)
		}
		if _, _, err := l.CheckCoupon(ctx, "five", "u1", 20e6, now.Add(store.CouponHold+time.Minute)); err != nil {
			t.Fatalf("after the hold: %v", err)
		}
		_, err = l.PayOrder(ctx, "p1", now)
		must(t, err)
		uses, err := l.CouponUses(ctx, c2.ID)
		must(t, err)
		if len(uses) != 1 || uses[0].Status != store.OrderPaid || uses[0].DiscountMicro != 5e6 {
			t.Fatalf("%+v", uses)
		}
		// Pending uses stop counting after the hold.
		late := now.Add(store.CouponHold + time.Minute)
		if _, _, err := l.CheckCoupon(ctx, "spring10", "u9", 10e6, late); err != nil {
			t.Fatalf("pending uses still counted: %v", err)
		}

		got.Disabled = true
		must(t, l.UpdateCoupon(ctx, got))
		if _, _, err := l.CheckCoupon(ctx, "spring10", "u9", 10e6, late); !errors.Is(err, store.ErrCouponInvalid) {
			t.Fatal(err)
		}
		win := &store.Coupon{Code: "LATER", Percent: 50, StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour)}
		must(t, l.CreateCoupon(ctx, win))
		if _, _, err := l.CheckCoupon(ctx, "later", "u1", 10e6, now); !errors.Is(err, store.ErrCouponNotStarted) {
			t.Fatal(err)
		}
		if _, _, err := l.CheckCoupon(ctx, "later", "u1", 10e6, now.Add(3*time.Hour)); !errors.Is(err, store.ErrCouponExpired) {
			t.Fatal(err)
		}
		list, err := l.ListCoupons(ctx)
		must(t, err)
		if len(list) != 3 {
			t.Fatal(len(list))
		}
	})
}

func TestCheckIn(t *testing.T) {
	onEveryDB(t, func(t *testing.T, open opener) {
		ctx := context.Background()
		l := ledgerOf(t, open(t, newBox(t)))
		now := time.Now()
		var wg sync.WaitGroup
		var mu sync.Mutex
		applied := 0
		for range 6 {
			wg.Go(func() {
				_, ok, err := l.CheckIn(ctx, "u1", "user:u1", "2026-11-07", 1e6, now)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					t.Error(err)
				}
				if ok {
					applied++
				}
			})
		}
		wg.Wait()
		if applied != 1 {
			t.Fatalf("checked in %d times on one day", applied)
		}
		txn, ok, err := l.CheckIn(ctx, "u1", "user:u1", "2026-11-07", 9e6, now)
		must(t, err)
		if ok || txn.AmountMicro != 1e6 {
			t.Fatalf("second check-in: %v %+v", ok, txn)
		}
		_, ok, err = l.CheckIn(ctx, "u1", "user:u1", "2026-11-08", 2e6, now)
		must(t, err)
		if !ok {
			t.Fatal("next day refused")
		}
		a, err := l.Account(ctx, "user:u1")
		must(t, err)
		if a.BalanceMicro != 3e6 {
			t.Fatalf("balance %d", a.BalanceMicro)
		}
		list, err := l.CheckIns(ctx, "u1", 0)
		must(t, err)
		if len(list) != 2 || list[0].Ref != "2026-11-08" || list[0].Kind != store.TxnCheckin {
			t.Fatalf("%+v", list)
		}
		if other, _ := l.CheckIns(ctx, "u", 0); len(other) != 0 {
			t.Fatalf("prefix match leaked: %+v", other)
		}
	})
}

func TestReferrals(t *testing.T) {
	onEveryDB(t, func(t *testing.T, open opener) {
		ctx := context.Background()
		l := ledgerOf(t, open(t, newBox(t)))
		now := time.Now()
		n := 0
		gen := func() string { n++; return fmt.Sprintf("CODE%d", n) }
		code, err := l.ReferralCode(ctx, "inviter", gen)
		must(t, err)
		if again, _ := l.ReferralCode(ctx, "inviter", gen); again != code {
			t.Fatalf("code changed: %s %s", code, again)
		}
		if _, err := l.Refer(ctx, code, "inviter", now); !errors.Is(err, store.ErrSelfReferral) {
			t.Fatal(err)
		}
		if _, err := l.Refer(ctx, "nope", "a", now); !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		for _, who := range []string{"a", "b", "c"} {
			if inv, err := l.Refer(ctx, code, who, now); err != nil || inv != "inviter" {
				t.Fatal(inv, err)
			}
		}
		// The first referral sticks.
		other, _ := l.ReferralCode(ctx, "other", gen)
		_, err = l.Refer(ctx, other, "a", now)
		must(t, err)
		if x, _ := l.ReferralOf(ctx, "a"); x.InviterID != "inviter" {
			t.Fatalf("%+v", x)
		}

		// Rewarded once per invitee, and never past the cap of 10.
		txn, err := l.RewardReferral(ctx, "a", "user:inviter", "o1", 6e6, 10e6, now)
		must(t, err)
		if txn == nil || txn.AmountMicro != 6e6 {
			t.Fatalf("%+v", txn)
		}
		if again, err := l.RewardReferral(ctx, "a", "user:inviter", "o2", 6e6, 10e6, now); err != nil || again != nil {
			t.Fatalf("second reward: %+v %v", again, err)
		}
		var wg sync.WaitGroup
		for _, who := range []string{"b", "c"} {
			wg.Go(func() {
				if _, err := l.RewardReferral(ctx, who, "user:inviter", "o-"+who, 6e6, 10e6, now); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		acct, err := l.Account(ctx, "user:inviter")
		must(t, err)
		if acct.BalanceMicro != 10e6 {
			t.Fatalf("inviter got %d, cap is 10e6", acct.BalanceMicro)
		}
		if none, err := l.RewardReferral(ctx, "stranger", "user:inviter", "o9", 6e6, 0, now); err != nil || none != nil {
			t.Fatalf("no referral: %+v %v", none, err)
		}
		list, err := l.ListReferrals(ctx, "inviter", 0)
		must(t, err)
		var sum int64
		for _, x := range list {
			if x.RewardedAt.IsZero() {
				t.Fatalf("not marked: %+v", x)
			}
			sum += x.RewardMicro
		}
		if len(list) != 3 || sum != 10e6 {
			t.Fatalf("%d referrals, %d paid", len(list), sum)
		}

		// Many first orders paid at once still stay within the cap, even
		// when the inviter's payer account changes between them (the
		// account's row lock cannot serialise them then).
		busy, _ := l.ReferralCode(ctx, "busy", gen)
		var invitees []string
		for i := range 12 {
			who := fmt.Sprintf("busy-%d", i)
			_, err := l.Refer(ctx, busy, who, now)
			must(t, err)
			invitees = append(invitees, who)
		}
		for _, who := range invitees {
			wg.Go(func() {
				if _, err := l.RewardReferral(ctx, who, "user:"+who, "o-"+who, 1e6, 5e6, now); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		var got int64
		for _, who := range invitees {
			acct, err := l.Account(ctx, "user:"+who)
			must(t, err)
			got += acct.BalanceMicro
		}
		if got != 5e6 {
			t.Fatalf("busy inviter got %d, cap is 5e6", got)
		}
	})
}

func TestRedeemDetails(t *testing.T) {
	onEveryDB(t, func(t *testing.T, open opener) {
		ctx := context.Background()
		l := ledgerOf(t, open(t, newBox(t)))
		must(t, l.CreateRedeemCodes(ctx, []store.RedeemCode{{CodeHash: "h1", Last4: "AAAA", Batch: "b", AmountMicro: 1e6, MaxUses: 2},
			{CodeHash: "h2", Last4: "BBBB", Batch: "b", AmountMicro: 1e6}}))
		_, err := l.Redeem(ctx, "h1", "user:x", "x", time.Now())
		must(t, err)
		codes, err := l.ListRedeemCodes(ctx, "b")
		must(t, err)
		var id string
		for _, c := range codes {
			if c.Last4 == "AAAA" {
				id = c.ID
			}
		}
		uses, err := l.RedeemUses(ctx, id)
		must(t, err)
		if len(uses) != 1 || uses[0].Subject != "user:x" {
			t.Fatalf("%+v", uses)
		}
		must(t, l.SetRedeemBatchDisabled(ctx, "b", true))
		if _, err := l.Redeem(ctx, "h2", "user:y", "y", time.Now()); !errors.Is(err, store.ErrRedeemInvalid) {
			t.Fatal(err)
		}
		if err := l.SetRedeemBatchDisabled(ctx, "none", true); !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
	})
}
