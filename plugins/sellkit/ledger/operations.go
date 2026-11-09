package ledger

import (
	"context"
	"errors"
	"time"
)

// Transaction kinds of rewards: a daily check-in, and an inviter's reward
// for the first top-up of someone they invited.
const (
	Checkin  Kind = "checkin"
	Referral Kind = "referral"
)

// Order kinds and statuses.
const (
	OrderTopup = "topup"
	OrderPlan  = "plan"

	OrderPending  = "pending"
	OrderPaid     = "paid"
	OrderCanceled = "canceled"
)

// CouponHold is how long an unpaid order keeps its coupon use counted,
// canceled or not (a provider's checkout page may still be paid). A
// provider's checkout must expire well inside it; an order paid later does
// not get its discount. internal/store counts uses with the same window.
const CouponHold = time.Hour

var (
	ErrCouponInvalid     = errors.New("coupon code is not valid")
	ErrCouponNotStarted  = errors.New("coupon is not valid yet")
	ErrCouponExpired     = errors.New("coupon has expired")
	ErrCouponUsedUp      = errors.New("coupon has been used up")
	ErrCouponAlreadyUsed = errors.New("you have used this coupon already")
	ErrCouponMinAmount   = errors.New("top up more to use this coupon")
	ErrSelfReferral      = errors.New("you cannot invite yourself")
	ErrConflict          = errors.New("already exists")
)

// Order is one payment at a provider: a top-up (AmountMicro credited) or
// a plan period (AmountMicro the price). PayMicro is what the provider
// charges: AmountMicro less a coupon's DiscountMicro.
type Order struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	UserID        string `json:"user_id"`
	Subject       string `json:"subject"`
	AmountMicro   int64  `json:"amount_micro"`
	PayMicro      int64  `json:"pay_micro"`
	DiscountMicro int64  `json:"discount_micro"`
	Currency      string `json:"currency"`
	Provider      string `json:"provider"`
	Method        string `json:"method,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	CouponID      string `json:"coupon_id,omitempty"`
	CouponCode    string `json:"coupon_code,omitempty"`
	// Ref is the subscription a plan order pays for.
	Ref       string    `json:"ref,omitempty"`
	Note      string    `json:"note,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	PaidAt    time.Time `json:"paid_at,omitzero"`
}

// OrderQuery pages orders newest first; empty fields match all.
type OrderQuery struct {
	UserID, Status, Provider, Kind string
	Cursor                         string
	Limit                          int
}

// OrderTotals sums the orders paid since a time and counts the pending ones.
type OrderTotals struct {
	PaidMicro    int64 `json:"paid_micro"`
	PaidCount    int64 `json:"paid_count"`
	PendingCount int64 `json:"pending_count"`
}

// Coupon is a discount on top-ups: Percent off, at most MaxDiscountMicro
// when set, or AmountMicro off. MaxUses and PerUser 0 mean no limit.
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
	Used             int       `json:"used"`
	Pending          int       `json:"pending"`
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

// RedeemUse is one account that redeemed a code.
type RedeemUse struct {
	Subject   string    `json:"subject"`
	CreatedAt time.Time `json:"created_at"`
}

// ReferralRow is who invited whom, and the reward once paid.
type ReferralRow struct {
	InviteeID   string    `json:"invitee_id"`
	InviterID   string    `json:"inviter_id"`
	CreatedAt   time.Time `json:"created_at"`
	RewardMicro int64     `json:"reward_micro"`
	RewardedAt  time.Time `json:"rewarded_at,omitzero"`
	OrderID     string    `json:"order_id,omitempty"`
}

// Operations is the store side of orders, coupons, redeem details,
// referrals and check-ins; every Ledger has it.
type Operations interface {
	// Timezone is the site's time zone, which days are counted in.
	Timezone(ctx context.Context) *time.Location

	RedeemUses(ctx context.Context, codeID string) ([]RedeemUse, error)
	SetBatchDisabled(ctx context.Context, batch string, disabled bool) error

	CreateOrder(ctx context.Context, o Order) error
	Order(ctx context.Context, id string) (Order, error)
	// PayOrder marks an order paid (and its coupon use); false when it
	// was before.
	PayOrder(ctx context.Context, id string, at time.Time) (bool, error)
	// CancelOrder cancels a pending order. release gives its coupon use
	// back now (the provider expired the checkout); otherwise the use stays
	// counted for CouponHold.
	CancelOrder(ctx context.Context, id string, release bool) (bool, error)
	Orders(ctx context.Context, q OrderQuery) ([]Order, string, error)
	OrderTotals(ctx context.Context, since time.Time) (OrderTotals, error)

	CreateCoupon(ctx context.Context, c Coupon) (Coupon, error)
	UpdateCoupon(ctx context.Context, c Coupon) (Coupon, error)
	Coupon(ctx context.Context, id string) (Coupon, error)
	Coupons(ctx context.Context) ([]Coupon, error)
	CouponUses(ctx context.Context, couponID string) ([]CouponUse, error)
	// CheckCoupon says what code takes off amount for userID (ErrCoupon*).
	CheckCoupon(ctx context.Context, code, userID string, amount int64, now time.Time) (Coupon, int64, error)
	// ReserveCoupon takes a use for orderID, never past the coupon's
	// limits under concurrency; ReleaseCoupon gives it back when the order
	// is not created after all.
	ReserveCoupon(ctx context.Context, code, userID, orderID string, amount int64, now time.Time) (Coupon, int64, error)
	ReleaseCoupon(ctx context.Context, orderID string) error

	// ReferralCode is the user's invite code, made the first time.
	ReferralCode(ctx context.Context, userID string) (string, error)
	// Refer records that the owner of code invited inviteeID, while
	// referral rewards are on; an unknown code is ErrNotFound.
	Refer(ctx context.Context, code, inviteeID string) error
	Referrals(ctx context.Context, inviterID string, limit int) ([]ReferralRow, error)
	// RewardReferral pays the inviter of userID for their first paid
	// top-up of paidMicro, once, by the referral settings; nil when
	// nothing was paid.
	RewardReferral(ctx context.Context, userID, orderID string, paidMicro int64) (*Txn, error)

	// CheckIn credits amount to subject for userID's check-in on day,
	// once per user and day; applied false with the earlier transaction
	// when the user checked in that day already.
	CheckIn(ctx context.Context, userID, subject, day string, amount int64) (t Txn, applied bool, err error)
	CheckIns(ctx context.Context, userID string, limit int) ([]Txn, error)
}
