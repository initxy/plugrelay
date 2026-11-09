package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

var (
	// ErrAllowanceExhausted: the plan's allowance for the period is used up
	// and the plan does not let the balance pay for more.
	ErrAllowanceExhausted = errors.New("the plan allowance for this period is used up")
	// ErrSubscribed: the user already has a current subscription.
	ErrSubscribed = errors.New("you already have an active subscription")
)

// Transaction kinds of subscriptions: a period paid ("subscription", on
// the payer: negative when paid from the balance, zero when paid at a
// provider) and allowance grants and draws ("allowance", on
// AllowanceSubject, in the plan's unit).
const (
	Subscription Kind = "subscription"
	Allowance    Kind = "allowance"
)

// Subscription statuses, plan units, overage policies and renewals.
const (
	SubPending = "pending"
	SubActive  = "active"
	SubEnded   = "ended"

	UnitMoney  = "money"  // allowance in micro-units of the site currency
	UnitTokens = "tokens" // allowance in tokens (input, cached and output)

	OverageBlock   = "block"   // refuse past the allowance
	OverageBalance = "balance" // the prepaid balance pays past it

	RenewBalance  = "balance"  // the next period is charged to the balance
	RenewProvider = "provider" // the provider bills each period
)

// Sub is one user's subscription. Allowance, Used and Held are in Unit.
type Sub struct {
	ID                string    `json:"id"`
	UserID            string    `json:"user_id"`
	Payer             string    `json:"payer"`
	PlanID            string    `json:"plan_id"`
	PlanName          string    `json:"plan_name"`
	Status            string    `json:"status"`
	Period            string    `json:"period"`
	PriceMicro        int64     `json:"price_micro"`
	Unit              string    `json:"unit"`
	Allowance         int64     `json:"allowance"`
	Used              int64     `json:"used"`
	Held              int64     `json:"held"`
	Overage           string    `json:"overage"`
	GroupID           string    `json:"group_id,omitempty"`
	PeriodStart       time.Time `json:"period_start,omitzero"`
	PeriodEnd         time.Time `json:"period_end,omitzero"`
	CancelAtPeriodEnd bool      `json:"cancel_at_period_end"`
	Provider          string    `json:"provider"`
	Renewal           string    `json:"renewal"`
	ProviderRef       string    `json:"provider_ref,omitempty"`
	// RefundDue: paid while another subscription of the user was active,
	// so recorded but not started; an admin refunds it.
	RefundDue bool      `json:"refund_due,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SubEstimate is a request's expected cost: money at its rate and tokens
// (input estimate plus output limit). UnitPriced marks a media operation
// (plugin.Call.UnitPriced), which token plans do not cover.
type SubEstimate struct {
	Money      int64
	Tokens     int64
	UnitPriced bool
}

// Current reports whether s is active and inside its period at now.
func (s Sub) Current(now time.Time) bool {
	return s.Status == SubActive && !now.Before(s.PeriodStart) && now.Before(s.PeriodEnd)
}

// Left is what remains of the period's allowance.
func (s Sub) Left() int64 { return s.Allowance - s.Used - s.Held }

// SubHold is a request's draw on a subscription: Covered when the
// allowance takes it, else the request pays from the balance.
type SubHold struct {
	Sub     Sub
	Covered bool
}

// Subscriptions is the core's subscription store; see store.Subscriptions
// for the rules. Every call is one database transaction.
type Subscriptions interface {
	// CreateSubscription stores s as pending, ending the user's pending
	// one (ErrSubscribed when the user has an active one), and returns it
	// with its id.
	CreateSubscription(ctx context.Context, s Sub) (Sub, error)
	Subscription(ctx context.Context, id string) (Sub, error)
	SubscriptionByRef(ctx context.Context, provider, ref string) (Sub, error)
	// ListSubscriptions lists subscriptions newest first, narrowed to a
	// user and a status when given.
	ListSubscriptions(ctx context.Context, userID, status string) ([]Sub, error)
	// StartPeriod records t once per t.ID and then makes the subscription
	// active for [start, end) with a fresh allowance; false when t was
	// recorded before. A negative t.AmountMicro is paid from the payer's
	// balance (ErrInsufficient). A payment for a subscription that cannot
	// start because another is active is recorded and flags it RefundDue
	// (from the balance: ErrSubscribed, uncharged).
	StartPeriod(ctx context.Context, id string, start, end time.Time, t Txn) (bool, error)
	SetSubscriptionRef(ctx context.Context, id, ref string) error
	SetRefundDue(ctx context.Context, id string, due bool) error
	SetCancelAtPeriodEnd(ctx context.Context, id string, cancel bool) error
	EndSubscription(ctx context.Context, id string) (bool, error)
	// RenewDue renews or ends subscriptions whose period is over.
	RenewDue(ctx context.Context, now time.Time) (int64, error)
	// SubscriptionHold reserves a request's draw on the user's current
	// subscription: ErrNotFound without one, ErrAllowanceExhausted (with
	// the hold) when used up and overage is blocked.
	SubscriptionHold(ctx context.Context, userID, payer, requestID string, est SubEstimate, now time.Time) (SubHold, error)
	// SubscriptionSettle charges a held request (money from the allowance,
	// overage from the payer; tokens for token plans, whose payer pays
	// unitCharge, the part of charge priced per media unit) once.
	SubscriptionSettle(ctx context.Context, requestID string, charge, unitCharge, tokens int64, note string) (bool, error)
	SubscriptionRelease(ctx context.Context, requestID string) (bool, error)
}

// SubscriptionsHost is implemented by a plugin.Host that offers
// subscriptions.
type SubscriptionsHost interface {
	Subscriptions() Subscriptions
}

// SubscriptionsFrom returns the host's subscriptions, or nil when it
// offers none.
func SubscriptionsFrom(h plugin.Host) Subscriptions {
	if sh, ok := h.(SubscriptionsHost); ok {
		return sh.Subscriptions()
	}
	return nil
}

// AllowanceSubject is the ledger subject of a subscription's allowance.
func AllowanceSubject(id string) string { return "sub:" + id }

// AddPeriod returns t plus one period ("week", "month" or "year"), a day
// past the end of the target month clamped to its last day (Jan 31 plus a
// month is Feb 28 or 29).
func AddPeriod(t time.Time, period string) time.Time {
	switch period {
	case "week":
		return t.AddDate(0, 0, 7)
	case "year":
		return addMonths(t, 12)
	}
	return addMonths(t, 1)
}

func addMonths(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	first := time.Date(y, m+time.Month(n), 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
	return first.AddDate(0, 0, min(d, first.AddDate(0, 1, -1).Day())-1)
}
