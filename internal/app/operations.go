package app

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

// rewardsPlugin holds the referral settings the ledger reads when a
// top-up is paid.
const rewardsPlugin = "admin.rewards"

func opsErr(err error) error {
	switch {
	case errors.Is(err, store.ErrCouponInvalid):
		return ledger.ErrCouponInvalid
	case errors.Is(err, store.ErrCouponNotStarted):
		return ledger.ErrCouponNotStarted
	case errors.Is(err, store.ErrCouponExpired):
		return ledger.ErrCouponExpired
	case errors.Is(err, store.ErrCouponUsedUp):
		return ledger.ErrCouponUsedUp
	case errors.Is(err, store.ErrCouponAlreadyUsed):
		return ledger.ErrCouponAlreadyUsed
	case errors.Is(err, store.ErrCouponMinAmount):
		return ledger.ErrCouponMinAmount
	case errors.Is(err, store.ErrSelfReferral):
		return ledger.ErrSelfReferral
	case errors.Is(err, store.ErrConflict):
		return ledger.ErrConflict
	}
	return ledgerErr(err)
}

func (s storeLedger) Timezone(ctx context.Context) *time.Location {
	return admin.Timezone(ctx, s.a.store)
}

func (s storeLedger) RedeemUses(ctx context.Context, codeID string) ([]ledger.RedeemUse, error) {
	rows, err := s.l.RedeemUses(ctx, codeID)
	out := make([]ledger.RedeemUse, 0, len(rows))
	for _, u := range rows {
		out = append(out, ledger.RedeemUse(u))
	}
	return out, opsErr(err)
}

func (s storeLedger) SetBatchDisabled(ctx context.Context, batch string, disabled bool) error {
	return opsErr(s.l.SetRedeemBatchDisabled(ctx, batch, disabled))
}

func (s storeLedger) CreateOrder(ctx context.Context, o ledger.Order) error {
	so := store.PaymentOrder(o)
	return opsErr(s.l.CreateOrder(ctx, &so))
}

func (s storeLedger) Order(ctx context.Context, id string) (ledger.Order, error) {
	o, err := s.l.GetOrder(ctx, id)
	if err != nil {
		return ledger.Order{}, opsErr(err)
	}
	return ledger.Order(*o), nil
}

func (s storeLedger) PayOrder(ctx context.Context, id string, at time.Time) (bool, error) {
	ok, err := s.l.PayOrder(ctx, id, at)
	return ok, opsErr(err)
}

func (s storeLedger) CancelOrder(ctx context.Context, id string, release bool) (bool, error) {
	ok, err := s.l.CancelOrder(ctx, id, release)
	return ok, opsErr(err)
}

func (s storeLedger) Orders(ctx context.Context, q ledger.OrderQuery) ([]ledger.Order, string, error) {
	rows, next, err := s.l.ListOrders(ctx, store.OrderQuery(q))
	out := make([]ledger.Order, 0, len(rows))
	for _, o := range rows {
		out = append(out, ledger.Order(o))
	}
	return out, next, opsErr(err)
}

func (s storeLedger) OrderTotals(ctx context.Context, since time.Time) (ledger.OrderTotals, error) {
	t, err := s.l.OrderTotals(ctx, since)
	return ledger.OrderTotals(t), opsErr(err)
}

func (s storeLedger) CreateCoupon(ctx context.Context, c ledger.Coupon) (ledger.Coupon, error) {
	sc := store.Coupon(c)
	err := s.l.CreateCoupon(ctx, &sc)
	return ledger.Coupon(sc), opsErr(err)
}

func (s storeLedger) UpdateCoupon(ctx context.Context, c ledger.Coupon) (ledger.Coupon, error) {
	sc := store.Coupon(c)
	if err := s.l.UpdateCoupon(ctx, &sc); err != nil {
		return ledger.Coupon{}, opsErr(err)
	}
	return s.Coupon(ctx, c.ID)
}

func (s storeLedger) Coupon(ctx context.Context, id string) (ledger.Coupon, error) {
	c, err := s.l.GetCoupon(ctx, id)
	if err != nil {
		return ledger.Coupon{}, opsErr(err)
	}
	return ledger.Coupon(*c), nil
}

func (s storeLedger) Coupons(ctx context.Context) ([]ledger.Coupon, error) {
	rows, err := s.l.ListCoupons(ctx)
	out := make([]ledger.Coupon, 0, len(rows))
	for _, c := range rows {
		out = append(out, ledger.Coupon(c))
	}
	return out, opsErr(err)
}

func (s storeLedger) CouponUses(ctx context.Context, couponID string) ([]ledger.CouponUse, error) {
	rows, err := s.l.CouponUses(ctx, couponID)
	out := make([]ledger.CouponUse, 0, len(rows))
	for _, u := range rows {
		out = append(out, ledger.CouponUse(u))
	}
	return out, opsErr(err)
}

func (s storeLedger) CheckCoupon(ctx context.Context, code, userID string, amount int64, now time.Time) (ledger.Coupon, int64, error) {
	c, d, err := s.l.CheckCoupon(ctx, code, userID, amount, now)
	if err != nil {
		return ledger.Coupon{}, 0, opsErr(err)
	}
	return ledger.Coupon(*c), d, nil
}

func (s storeLedger) ReserveCoupon(ctx context.Context, code, userID, orderID string, amount int64, now time.Time) (ledger.Coupon, int64, error) {
	c, d, err := s.l.ReserveCoupon(ctx, code, userID, orderID, amount, now)
	if err != nil {
		return ledger.Coupon{}, 0, opsErr(err)
	}
	return ledger.Coupon(*c), d, nil
}

func (s storeLedger) ReleaseCoupon(ctx context.Context, orderID string) error {
	return opsErr(s.l.ReleaseCoupon(ctx, orderID))
}

// referralAlphabet leaves out 0/O and 1/I/L, which people misread.
const referralAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

func newReferralCode() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	for i, c := range b {
		b[i] = referralAlphabet[int(c)%len(referralAlphabet)]
	}
	return string(b)
}

func (s storeLedger) ReferralCode(ctx context.Context, userID string) (string, error) {
	c, err := s.l.ReferralCode(ctx, userID, newReferralCode)
	return c, opsErr(err)
}

func (s storeLedger) referralOn() bool {
	return s.a.settings().Get(nil, nil, rewardsPlugin, "referral_enabled").Bool()
}

func (s storeLedger) Refer(ctx context.Context, code, inviteeID string) error {
	if !s.referralOn() || code == "" {
		return nil
	}
	_, err := s.l.Refer(ctx, code, inviteeID, time.Now())
	return opsErr(err)
}

func (s storeLedger) Referrals(ctx context.Context, inviterID string, limit int) ([]ledger.ReferralRow, error) {
	rows, err := s.l.ListReferrals(ctx, inviterID, limit)
	out := make([]ledger.ReferralRow, 0, len(rows))
	for _, x := range rows {
		out = append(out, ledger.ReferralRow(x))
	}
	return out, opsErr(err)
}

// RewardReferral pays by the admin.rewards settings: a percent of what
// the invitee paid, or a fixed amount, to the account the inviter's own
// requests pay from, never past the cap per inviter. With payer=key the
// inviter has no one account, and nothing is paid.
func (s storeLedger) RewardReferral(ctx context.Context, userID, orderID string, paid int64) (*ledger.Txn, error) {
	if !s.referralOn() || paid <= 0 {
		return nil, nil
	}
	x, err := s.l.ReferralOf(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, opsErr(err)
	}
	if !x.RewardedAt.IsZero() {
		return nil, nil
	}
	subject, err := s.PayerOf(ctx, x.InviterID, "")
	if errors.Is(err, ledger.ErrKeyRequired) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	get := func(key string) float64 { return s.a.settings().Get(nil, nil, rewardsPlugin, key).Float() }
	reward := int64(get("referral_fixed_micro"))
	if s.a.settings().Get(nil, nil, rewardsPlugin, "referral_mode").String() != "fixed" {
		reward = int64(float64(paid) * get("referral_percent") / 100)
	}
	reward -= reward % 10_000 // whole cents
	t, err := s.l.RewardReferral(ctx, userID, subject, orderID, reward, int64(get("referral_cap_micro")), time.Now())
	if err != nil || t == nil {
		return nil, opsErr(err)
	}
	out := toTxn(t)
	return &out, nil
}

func (s storeLedger) CheckIn(ctx context.Context, userID, subject, day string, amount int64) (ledger.Txn, bool, error) {
	t, ok, err := s.l.CheckIn(ctx, userID, subject, day, amount, time.Now())
	if err != nil {
		return ledger.Txn{}, false, opsErr(err)
	}
	return toTxn(t), ok, nil
}

func (s storeLedger) CheckIns(ctx context.Context, userID string, limit int) ([]ledger.Txn, error) {
	rows, err := s.l.CheckIns(ctx, userID, limit)
	out := make([]ledger.Txn, 0, len(rows))
	for i := range rows {
		out = append(out, toTxn(&rows[i]))
	}
	return out, opsErr(err)
}
