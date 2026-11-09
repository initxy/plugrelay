package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Operations of the sell kit next to the balance ledger: payment orders,
// coupons, referrals and daily check-ins. Rewards (check-ins, referrals)
// are balance transactions credited inside the same database transaction
// that decides them, so a retry or a race never pays twice.

// Transaction kinds of rewards.
const (
	TxnCheckin  TxnKind = "checkin"
	TxnReferral TxnKind = "referral"
)

// Order kinds and statuses.
const (
	OrderTopup = "topup"
	OrderPlan  = "plan"

	OrderPending  = "pending"
	OrderPaid     = "paid"
	OrderCanceled = "canceled"
)

// PaymentOrder is one payment at a provider.
type PaymentOrder struct {
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	UserID        string    `json:"user_id"`
	Subject       string    `json:"subject"`
	AmountMicro   int64     `json:"amount_micro"`
	PayMicro      int64     `json:"pay_micro"`
	DiscountMicro int64     `json:"discount_micro"`
	Currency      string    `json:"currency"`
	Provider      string    `json:"provider"`
	Method        string    `json:"method,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	CouponID      string    `json:"coupon_id,omitempty"`
	CouponCode    string    `json:"coupon_code,omitempty"`
	Ref           string    `json:"ref,omitempty"`
	Note          string    `json:"note,omitempty"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	PaidAt        time.Time `json:"paid_at,omitzero"`
}

// OrderQuery pages orders newest first; empty fields match all.
type OrderQuery struct {
	UserID, Status, Provider, Kind string
	Cursor                         string
	Limit                          int
}

// OrderTotals sums paid orders since a time, and counts open ones.
type OrderTotals struct {
	PaidMicro    int64 `json:"paid_micro"`
	PaidCount    int64 `json:"paid_count"`
	PendingCount int64 `json:"pending_count"`
}

// Coupon is a discount on top-ups: Percent off (capped by
// MaxDiscountMicro when set) or AmountMicro off.
type Coupon struct {
	ID               string    `json:"id"`
	Code             string    `json:"code"`
	Percent          int       `json:"percent"`
	AmountMicro      int64     `json:"amount_micro"`
	MaxDiscountMicro int64     `json:"max_discount_micro"`
	MinTopupMicro    int64     `json:"min_topup_micro"`
	MaxUses          int       `json:"max_uses"`
	PerUser          int       `json:"per_user"`
	StartsAt         time.Time `json:"starts_at,omitzero"`
	EndsAt           time.Time `json:"ends_at,omitzero"`
	Disabled         bool      `json:"disabled"`
	Note             string    `json:"note,omitempty"`
	CreatedBy        string    `json:"created_by,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	// Used counts paid uses, Pending the uses of orders not paid yet that
	// still hold a place.
	Used    int `json:"used"`
	Pending int `json:"pending"`
}

// CouponUse is one order that applied a coupon.
type CouponUse struct {
	OrderID       string    `json:"order_id"`
	CouponID      string    `json:"coupon_id"`
	UserID        string    `json:"user_id"`
	DiscountMicro int64     `json:"discount_micro"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

// CouponHold is how long an unpaid use keeps its place, canceled or not;
// the same window as ledger.CouponHold.
const CouponHold = time.Hour

// Coupon failures.
var (
	ErrCouponInvalid     = errors.New("store: coupon invalid")
	ErrCouponNotStarted  = errors.New("store: coupon not valid yet")
	ErrCouponExpired     = errors.New("store: coupon expired")
	ErrCouponUsedUp      = errors.New("store: coupon used up")
	ErrCouponAlreadyUsed = errors.New("store: coupon already used by this user")
	ErrCouponMinAmount   = errors.New("store: amount below the coupon's minimum")
)

// discountStep rounds discounts down to whole cents, which every
// provider can charge.
const discountStep = 10_000

// Discount is what the coupon takes off amount, in whole cents.
func (c Coupon) Discount(amount int64) int64 {
	d := c.AmountMicro
	if c.Percent > 0 {
		d = amount * int64(c.Percent) / 100
		if c.MaxDiscountMicro > 0 {
			d = min(d, c.MaxDiscountMicro)
		}
	}
	return max(d-d%discountStep, 0)
}

// RedeemUse is one account that redeemed a code.
type RedeemUse struct {
	Subject   string    `json:"subject"`
	CreatedAt time.Time `json:"created_at"`
}

// Referral is who invited whom, and the reward once paid.
type Referral struct {
	InviteeID   string    `json:"invitee_id"`
	InviterID   string    `json:"inviter_id"`
	CreatedAt   time.Time `json:"created_at"`
	RewardMicro int64     `json:"reward_micro"`
	RewardedAt  time.Time `json:"rewarded_at,omitzero"`
	OrderID     string    `json:"order_id,omitempty"`
}

// ErrSelfReferral: a user cannot invite themselves.
var ErrSelfReferral = errors.New("store: self referral")

// ---------------------------------------------------------------- orders

const orderCols = `id, kind, user_id, subject, amount_micro, pay_micro, discount_micro, currency, provider, method,
session_id, coupon_id, coupon_code, ref, note, status, created_at, paid_at`

func scanOrder(sc interface{ Scan(...any) error }) (*PaymentOrder, error) {
	var o PaymentOrder
	var at, paid int64
	if err := sc.Scan(&o.ID, &o.Kind, &o.UserID, &o.Subject, &o.AmountMicro, &o.PayMicro, &o.DiscountMicro, &o.Currency,
		&o.Provider, &o.Method, &o.SessionID, &o.CouponID, &o.CouponCode, &o.Ref, &o.Note, &o.Status, &at, &paid); err != nil {
		return nil, err
	}
	o.CreatedAt, o.PaidAt = fromMs(at), fromMs(paid)
	return &o, nil
}

func (s *db) CreateOrder(ctx context.Context, o *PaymentOrder) error {
	if o.ID == "" || o.Kind == "" || o.UserID == "" || o.Provider == "" {
		return fmt.Errorf("store: order needs an id, kind, user and provider")
	}
	if o.Status == "" {
		o.Status = OrderPending
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC().Truncate(time.Millisecond)
	}
	_, err := s.r().exec(ctx, `INSERT INTO payment_orders (`+orderCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.Kind, o.UserID, o.Subject, o.AmountMicro, o.PayMicro, o.DiscountMicro, o.Currency, o.Provider, o.Method,
		o.SessionID, o.CouponID, o.CouponCode, o.Ref, o.Note, o.Status, ms(o.CreatedAt), ms(o.PaidAt))
	return wrap("create order", err)
}

func (s *db) GetOrder(ctx context.Context, id string) (*PaymentOrder, error) {
	o, err := scanOrder(s.r().row(ctx, `SELECT `+orderCols+` FROM payment_orders WHERE id = ?`, id))
	return o, wrap("get order", err)
}

// PayOrder marks an order paid, and its coupon use with it; false when it
// was paid before.
func (s *db) PayOrder(ctx context.Context, id string, at time.Time) (bool, error) {
	var applied bool
	err := s.tx(ctx, func(r run) error {
		res, err := r.exec(ctx, `UPDATE payment_orders SET status = ?, paid_at = ? WHERE id = ? AND status <> ?`,
			OrderPaid, ms(at), id, OrderPaid)
		if err != nil {
			return wrap("pay order", err)
		}
		n, _ := res.RowsAffected()
		if applied = n > 0; !applied {
			return nil
		}
		// A use released by a cancel comes back: the money was taken.
		var o *PaymentOrder
		if o, err = scanOrder(r.row(ctx, `SELECT `+orderCols+` FROM payment_orders WHERE id = ?`, id)); err != nil {
			return wrap("pay order", err)
		}
		if o.CouponID == "" {
			return nil
		}
		_, err = r.exec(ctx, `INSERT INTO coupon_uses (order_id, coupon_id, user_id, discount_micro, status, created_at)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (order_id) DO UPDATE SET status = excluded.status`,
			id, o.CouponID, o.UserID, o.DiscountMicro, OrderPaid, ms(o.CreatedAt))
		return wrap("pay order", err)
	})
	return applied, err
}

// CancelOrder cancels a pending order; false when it is not pending.
// release gives its coupon use back at once, for a checkout the provider
// has expired; otherwise the use stays counted until CouponHold passes,
// since the provider's page may still be paid.
func (s *db) CancelOrder(ctx context.Context, id string, release bool) (bool, error) {
	var applied bool
	err := s.tx(ctx, func(r run) error {
		res, err := r.exec(ctx, `UPDATE payment_orders SET status = ? WHERE id = ? AND status = ?`, OrderCanceled, id, OrderPending)
		if err != nil {
			return wrap("cancel order", err)
		}
		n, _ := res.RowsAffected()
		if applied = n > 0; !applied {
			return nil
		}
		if release {
			_, err = r.exec(ctx, `DELETE FROM coupon_uses WHERE order_id = ? AND status = ?`, id, OrderPending)
		} else {
			_, err = r.exec(ctx, `UPDATE coupon_uses SET status = ? WHERE order_id = ? AND status = ?`, OrderCanceled, id, OrderPending)
		}
		return wrap("cancel order", err)
	})
	return applied, err
}

func (s *db) ListOrders(ctx context.Context, q OrderQuery) ([]PaymentOrder, string, error) {
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	where := `WHERE (? = '' OR user_id = ?) AND (? = '' OR status = ?) AND (? = '' OR provider = ?) AND (? = '' OR kind = ?)`
	args := []any{q.UserID, q.UserID, q.Status, q.Status, q.Provider, q.Provider, q.Kind, q.Kind}
	if q.Cursor != "" {
		t, id, err := decodeCursor(q.Cursor)
		if err != nil {
			return nil, "", err
		}
		where += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, t, t, id)
	}
	args = append(args, limit+1)
	out, err := list(ctx, s.r(), "list orders", `SELECT `+orderCols+` FROM payment_orders `+where+
		` ORDER BY created_at DESC, id DESC LIMIT ?`, scanOrder, args...)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

func (s *db) OrderTotals(ctx context.Context, since time.Time) (OrderTotals, error) {
	var t OrderTotals
	err := s.r().row(ctx, `SELECT COALESCE(SUM(pay_micro), 0), COUNT(*) FROM payment_orders WHERE status = ? AND paid_at >= ?`,
		OrderPaid, ms(since)).Scan(&t.PaidMicro, &t.PaidCount)
	if err != nil {
		return t, wrap("order totals", err)
	}
	err = s.r().row(ctx, `SELECT COUNT(*) FROM payment_orders WHERE status = ? AND created_at >= ?`, OrderPending, ms(since)).
		Scan(&t.PendingCount)
	return t, wrap("order totals", err)
}

// ---------------------------------------------------------------- coupons

const couponCols = `id, code, percent, amount_micro, max_discount_micro, min_topup_micro, max_uses, per_user, starts_at, ends_at,
disabled, note, created_by, created_at, updated_at`

func scanCoupon(sc interface{ Scan(...any) error }) (*Coupon, error) {
	var c Coupon
	var st, end, at, up int64
	if err := sc.Scan(&c.ID, &c.Code, &c.Percent, &c.AmountMicro, &c.MaxDiscountMicro, &c.MinTopupMicro, &c.MaxUses, &c.PerUser,
		&st, &end, &c.Disabled, &c.Note, &c.CreatedBy, &at, &up); err != nil {
		return nil, err
	}
	c.StartsAt, c.EndsAt, c.CreatedAt, c.UpdatedAt = fromMs(st), fromMs(end), fromMs(at), fromMs(up)
	return &c, nil
}

// NormalizeCoupon is how a typed coupon code is compared.
func NormalizeCoupon(code string) string { return strings.ToUpper(strings.TrimSpace(code)) }

func validCoupon(c *Coupon) error {
	switch {
	case c.Code == "" || len(c.Code) > 64:
		return fmt.Errorf("store: coupon code must be 1 to 64 characters")
	case (c.Percent > 0) == (c.AmountMicro > 0) || c.Percent < 0 || c.Percent > 100 || c.AmountMicro < 0:
		return fmt.Errorf("store: a coupon takes either a percent (1-100) or a positive amount off")
	case c.MaxUses < 0 || c.PerUser < 0 || c.MinTopupMicro < 0 || c.MaxDiscountMicro < 0:
		return fmt.Errorf("store: coupon limits must not be negative")
	case !c.StartsAt.IsZero() && !c.EndsAt.IsZero() && !c.EndsAt.After(c.StartsAt):
		return fmt.Errorf("store: a coupon must end after it starts")
	}
	return nil
}

func (s *db) CreateCoupon(ctx context.Context, c *Coupon) error {
	c.Code = NormalizeCoupon(c.Code)
	if err := validCoupon(c); err != nil {
		return err
	}
	if c.ID == "" {
		c.ID = newID()
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	c.CreatedAt, c.UpdatedAt = now, now
	_, err := s.r().exec(ctx, `INSERT INTO coupons (`+couponCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Code, c.Percent, c.AmountMicro, c.MaxDiscountMicro, c.MinTopupMicro, c.MaxUses, c.PerUser, ms(c.StartsAt), ms(c.EndsAt),
		c.Disabled, c.Note, c.CreatedBy, ms(now), ms(now))
	return wrap("create coupon", err)
}

// UpdateCoupon changes everything but the code; uses already taken keep
// the discount they got.
func (s *db) UpdateCoupon(ctx context.Context, c *Coupon) error {
	old, err := s.GetCoupon(ctx, c.ID)
	if err != nil {
		return err
	}
	c.Code, c.CreatedAt, c.CreatedBy = old.Code, old.CreatedAt, old.CreatedBy
	if err := validCoupon(c); err != nil {
		return err
	}
	c.UpdatedAt = time.Now().UTC().Truncate(time.Millisecond)
	res, err := s.r().exec(ctx, `UPDATE coupons SET percent = ?, amount_micro = ?, max_discount_micro = ?, min_topup_micro = ?,
max_uses = ?, per_user = ?, starts_at = ?, ends_at = ?, disabled = ?, note = ?, updated_at = ? WHERE id = ?`,
		c.Percent, c.AmountMicro, c.MaxDiscountMicro, c.MinTopupMicro, c.MaxUses, c.PerUser, ms(c.StartsAt), ms(c.EndsAt), c.Disabled,
		c.Note, ms(c.UpdatedAt), c.ID)
	return mustAffect("update coupon", res, err)
}

func (s *db) GetCoupon(ctx context.Context, id string) (*Coupon, error) {
	c, err := scanCoupon(s.r().row(ctx, `SELECT `+couponCols+` FROM coupons WHERE id = ?`, id))
	if err != nil {
		return nil, wrap("get coupon", err)
	}
	return c, s.couponCounts(ctx, s.r(), c, time.Now())
}

func (s *db) couponCounts(ctx context.Context, r run, c *Coupon, now time.Time) error {
	err := r.row(ctx, `SELECT COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
COALESCE(SUM(CASE WHEN status <> ? AND created_at > ? THEN 1 ELSE 0 END), 0) FROM coupon_uses WHERE coupon_id = ?`,
		OrderPaid, OrderPaid, ms(now.Add(-CouponHold)), c.ID).Scan(&c.Used, &c.Pending)
	return wrap("coupon uses", err)
}

func (s *db) ListCoupons(ctx context.Context) ([]Coupon, error) {
	out, err := list(ctx, s.r(), "list coupons", `SELECT `+couponCols+` FROM coupons ORDER BY created_at DESC, id`, scanCoupon)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for i := range out {
		if err := s.couponCounts(ctx, s.r(), &out[i], now); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *db) CouponUses(ctx context.Context, couponID string) ([]CouponUse, error) {
	return list(ctx, s.r(), "coupon uses", `SELECT order_id, coupon_id, user_id, discount_micro, status, created_at FROM coupon_uses
WHERE coupon_id = ? ORDER BY created_at DESC, order_id`, func(sc interface{ Scan(...any) error }) (*CouponUse, error) {
		var u CouponUse
		var at int64
		if err := sc.Scan(&u.OrderID, &u.CouponID, &u.UserID, &u.DiscountMicro, &u.Status, &at); err != nil {
			return nil, err
		}
		u.CreatedAt = fromMs(at)
		return &u, nil
	}, couponID)
}

// checkCoupon applies a coupon's rules for userID topping up amount at
// now, counting the uses that hold a place; c's counts are filled in.
func (s *db) checkCoupon(ctx context.Context, r run, c *Coupon, userID string, amount int64, now time.Time) (int64, error) {
	switch {
	case c.Disabled:
		return 0, ErrCouponInvalid
	case !c.StartsAt.IsZero() && now.Before(c.StartsAt):
		return 0, ErrCouponNotStarted
	case !c.EndsAt.IsZero() && !now.Before(c.EndsAt):
		return 0, ErrCouponExpired
	case amount < c.MinTopupMicro:
		return 0, ErrCouponMinAmount
	}
	d := c.Discount(amount)
	if d <= 0 || d >= amount {
		return 0, ErrCouponMinAmount
	}
	if err := s.couponCounts(ctx, r, c, now); err != nil {
		return 0, err
	}
	if c.MaxUses > 0 && c.Used+c.Pending >= c.MaxUses {
		return 0, ErrCouponUsedUp
	}
	if c.PerUser > 0 {
		var mine int
		err := r.row(ctx, `SELECT COUNT(*) FROM coupon_uses WHERE coupon_id = ? AND user_id = ? AND (status = ? OR created_at > ?)`,
			c.ID, userID, OrderPaid, ms(now.Add(-CouponHold))).Scan(&mine)
		if err != nil {
			return 0, wrap("coupon uses", err)
		}
		if mine >= c.PerUser {
			return 0, ErrCouponAlreadyUsed
		}
	}
	return d, nil
}

// CheckCoupon says what code would take off amount for userID, without
// taking a use.
func (s *db) CheckCoupon(ctx context.Context, code, userID string, amount int64, now time.Time) (*Coupon, int64, error) {
	c, err := scanCoupon(s.r().row(ctx, `SELECT `+couponCols+` FROM coupons WHERE code = ?`, NormalizeCoupon(code)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrCouponInvalid
	}
	if err != nil {
		return nil, 0, wrap("check coupon", err)
	}
	d, err := s.checkCoupon(ctx, s.r(), c, userID, amount, now)
	return c, d, err
}

// ReserveCoupon takes a use of code for orderID in one transaction with
// the coupon row locked, so concurrent checkouts never pass max_uses or
// per_user. The order is created afterwards; ReleaseCoupon gives the use
// back when it cannot be.
func (s *db) ReserveCoupon(ctx context.Context, code, userID, orderID string, amount int64, now time.Time) (*Coupon, int64, error) {
	var out *Coupon
	var discount int64
	err := s.tx(ctx, func(r run) error {
		c, err := scanCoupon(r.row(ctx, `SELECT `+couponCols+` FROM coupons WHERE code = ?`+r.d.ForUpdate(), NormalizeCoupon(code)))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCouponInvalid
		}
		if err != nil {
			return wrap("reserve coupon", err)
		}
		d, err := s.checkCoupon(ctx, r, c, userID, amount, now)
		if err != nil {
			return err
		}
		if _, err := r.exec(ctx, `INSERT INTO coupon_uses (order_id, coupon_id, user_id, discount_micro, status, created_at)
VALUES (?, ?, ?, ?, ?, ?)`, orderID, c.ID, userID, d, OrderPending, ms(now)); err != nil {
			return wrap("reserve coupon", err)
		}
		out, discount = c, d
		return nil
	})
	return out, discount, err
}

// ReleaseCoupon drops the pending use of orderID.
func (s *db) ReleaseCoupon(ctx context.Context, orderID string) error {
	_, err := s.r().exec(ctx, `DELETE FROM coupon_uses WHERE order_id = ? AND status = ?`, orderID, OrderPending)
	return wrap("release coupon", err)
}

// ---------------------------------------------------------------- redeem code details

func (s *db) RedeemUses(ctx context.Context, codeID string) ([]RedeemUse, error) {
	return list(ctx, s.r(), "redeem uses", `SELECT subject, created_at FROM redeem_uses WHERE code_id = ? ORDER BY created_at DESC`,
		func(sc interface{ Scan(...any) error }) (*RedeemUse, error) {
			var u RedeemUse
			var at int64
			if err := sc.Scan(&u.Subject, &at); err != nil {
				return nil, err
			}
			u.CreatedAt = fromMs(at)
			return &u, nil
		}, codeID)
}

func (s *db) SetRedeemBatchDisabled(ctx context.Context, batch string, disabled bool) error {
	if batch == "" {
		return fmt.Errorf("store: batch is required")
	}
	res, err := s.r().exec(ctx, `UPDATE redeem_codes SET disabled = ? WHERE batch = ?`, disabled, batch)
	return mustAffect("disable redeem batch", res, err)
}

// ---------------------------------------------------------------- referrals

const referralCols = `invitee_id, inviter_id, created_at, reward_micro, rewarded_at, order_id`

func scanReferral(sc interface{ Scan(...any) error }) (*Referral, error) {
	var x Referral
	var at, rew int64
	if err := sc.Scan(&x.InviteeID, &x.InviterID, &at, &x.RewardMicro, &rew, &x.OrderID); err != nil {
		return nil, err
	}
	x.CreatedAt, x.RewardedAt = fromMs(at), fromMs(rew)
	return &x, nil
}

// ReferralCode is userID's invite code, made with newCode the first time.
func (s *db) ReferralCode(ctx context.Context, userID string, newCode func() string) (string, error) {
	for range 5 {
		var code string
		err := s.r().row(ctx, `SELECT code FROM referral_codes WHERE user_id = ?`, userID).Scan(&code)
		if err == nil {
			return code, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", wrap("referral code", err)
		}
		_, err = s.r().exec(ctx, `INSERT INTO referral_codes (user_id, code, created_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
			userID, newCode(), time.Now().UnixMilli())
		if err != nil {
			return "", wrap("referral code", err)
		}
	}
	return "", fmt.Errorf("store: referral code: no free code")
}

// Refer records that the owner of code invited inviteeID. An invitee
// keeps their first inviter; an unknown code is ErrNotFound.
func (s *db) Refer(ctx context.Context, code, inviteeID string, now time.Time) (string, error) {
	var inviter string
	err := s.r().row(ctx, `SELECT user_id FROM referral_codes WHERE code = ?`, strings.ToUpper(strings.TrimSpace(code))).Scan(&inviter)
	if err != nil {
		return "", wrap("refer", err)
	}
	if inviter == inviteeID {
		return "", ErrSelfReferral
	}
	_, err = s.r().exec(ctx, `INSERT INTO referrals (invitee_id, inviter_id, created_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		inviteeID, inviter, ms(now))
	return inviter, wrap("refer", err)
}

func (s *db) ReferralOf(ctx context.Context, inviteeID string) (*Referral, error) {
	x, err := scanReferral(s.r().row(ctx, `SELECT `+referralCols+` FROM referrals WHERE invitee_id = ?`, inviteeID))
	return x, wrap("referral", err)
}

// ListReferrals lists the people inviterID invited (everyone's when
// empty), newest first.
func (s *db) ListReferrals(ctx context.Context, inviterID string, limit int) ([]Referral, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	return list(ctx, s.r(), "list referrals", `SELECT `+referralCols+` FROM referrals WHERE (? = '' OR inviter_id = ?)
ORDER BY created_at DESC, invitee_id LIMIT ?`, scanReferral, inviterID, inviterID, limit)
}

// RewardReferral pays inviteeID's inviter for their first paid order,
// once: reward to subject, cut so that the inviter's rewards stay within
// limit (0 = no limit). The referral is marked rewarded even when the cut
// leaves nothing, so later orders pay nothing. nil when there is no
// referral or it was rewarded before.
func (s *db) RewardReferral(ctx context.Context, inviteeID, subject, orderID string, reward, limit int64, now time.Time) (*BalanceTxn, error) {
	var out *BalanceTxn
	err := s.tx(ctx, func(r run) error {
		out = nil
		if err := ensureAccount(ctx, r, subject); err != nil {
			return err
		}
		x, err := scanReferral(r.row(ctx, `SELECT `+referralCols+` FROM referrals WHERE invitee_id = ?`+r.d.ForUpdate(), inviteeID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return wrap("reward referral", err)
		}
		if !x.RewardedAt.IsZero() {
			return nil
		}
		amount := max(reward, 0)
		if limit > 0 {
			// The inviter's code row serialises their payouts, which may go
			// to different accounts: without it two could both read the sum
			// below before either is written and together pass the cap.
			// SQLite's write transactions are serial already.
			var held string
			err := r.row(ctx, `SELECT user_id FROM referral_codes WHERE user_id = ?`+r.d.ForUpdate(), x.InviterID).Scan(&held)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return wrap("reward referral", err)
			}
			var paid int64
			if err := r.row(ctx, `SELECT COALESCE(SUM(reward_micro), 0) FROM referrals WHERE inviter_id = ?`, x.InviterID).Scan(&paid); err != nil {
				return wrap("reward referral", err)
			}
			amount = max(min(amount, limit-paid), 0)
		}
		if _, err := r.exec(ctx, `UPDATE referrals SET reward_micro = ?, rewarded_at = ?, order_id = ? WHERE invitee_id = ?`,
			amount, ms(now), orderID, inviteeID); err != nil {
			return wrap("reward referral", err)
		}
		if amount == 0 {
			return nil
		}
		t := &BalanceTxn{ID: "referral:" + inviteeID, Subject: subject, Kind: TxnReferral, AmountMicro: amount, Ref: inviteeID,
			Note: orderID, Actor: x.InviterID, CreatedAt: now.UTC().Truncate(time.Millisecond)}
		ok, err := credit(ctx, r, t)
		if err != nil {
			return err
		}
		if ok {
			out = t
		}
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------- check-ins

func checkinID(userID, day string) string { return "checkin:" + userID + ":" + day }

// CheckIn credits amount to subject for userID's check-in on day (a date
// in the site's time zone), once per user and day; false with the
// earlier transaction when the user checked in that day already.
func (s *db) CheckIn(ctx context.Context, userID, subject, day string, amount int64, now time.Time) (*BalanceTxn, bool, error) {
	if amount <= 0 {
		return nil, false, fmt.Errorf("store: check-in reward must be positive")
	}
	var out *BalanceTxn
	var applied bool
	err := s.tx(ctx, func(r run) error {
		t := &BalanceTxn{ID: checkinID(userID, day), Subject: subject, Kind: TxnCheckin, AmountMicro: amount, Ref: day,
			Actor: userID, CreatedAt: now.UTC().Truncate(time.Millisecond)}
		ok, err := credit(ctx, r, t)
		if err != nil {
			return err
		}
		applied, out = ok, t
		if !ok {
			out, err = scanTxn(r.row(ctx, `SELECT `+txnCols+` FROM balance_txns WHERE id = ?`, txnID(t.ID)))
			return wrap("check in", err)
		}
		return nil
	})
	return out, applied, err
}

// CheckIns lists userID's check-ins, newest day first.
func (s *db) CheckIns(ctx context.Context, userID string, limit int) ([]BalanceTxn, error) {
	if limit <= 0 || limit > 400 {
		limit = 60
	}
	return list(ctx, s.r(), "check-ins", `SELECT `+txnCols+` FROM balance_txns WHERE id LIKE ? ESCAPE '\' AND kind = ?
ORDER BY id DESC LIMIT ?`, scanTxn, likePrefix(checkinID(userID, "")), TxnCheckin, limit)
}
