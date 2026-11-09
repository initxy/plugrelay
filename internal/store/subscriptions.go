package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Subscriptions (sell kit): a user pays for a plan per period and gets an
// allowance for that period, in money (micro-units) or tokens. Requests
// draw on the allowance first; past it, a plan either refuses them or lets
// the balance pay (overage). The plan's terms are copied into the row when
// the subscription is created, so editing a plan never changes what a
// subscriber already bought.
//
// A request's draw is held like a balance hold: a subscription_holds row
// is its settle/release marker, so whichever runs first wins. Draws are
// logged as "allowance" transactions on the subject "sub:<id>"
// (balance_after = what is left of the period's allowance); overage is an
// ordinary "charge:<request>" on the payer; a period paid from the balance
// is a "subscription" transaction on the payer, and one paid at a provider
// the same with amount zero, whose id makes the period start once.
//
// A user has at most one live (pending or active) subscription, enforced
// by a unique index and by locking the user's row: subscribing again ends
// a pending one, and a payment for a subscription that cannot start
// because another is active is recorded and flagged refund_due instead.
//
// Locks are taken by primary key, in one order: the user's row, then
// subscriptions, then balances (and the user's keys, in id order). Rows
// found through another index are read without a lock first and locked
// by id afterwards; a deadlock the database breaks anyway is retried (tx).
// A subscription whose user was deleted can still be ended.

// Subscription statuses, units, overage policies and renewals.
const (
	SubPending = "pending"
	SubActive  = "active"
	SubEnded   = "ended"

	UnitMoney  = "money"
	UnitTokens = "tokens"

	OverageBlock   = "block"
	OverageBalance = "balance"

	// RenewBalance: the period is renewed by charging the payer's balance
	// at its end (plans paid from the balance or by a one-off provider).
	RenewBalance = "balance"
	// RenewProvider: the provider bills each period and reports it
	// (Stripe subscriptions).
	RenewProvider = "provider"
)

// Transaction kinds of subscriptions.
const (
	TxnSubscription TxnKind = "subscription"
	TxnAllowance    TxnKind = "allowance"
)

// ErrAllowanceExhausted: the period's allowance is used up and the plan
// does not let the balance pay for more.
var ErrAllowanceExhausted = errors.New("store: plan allowance used up")

// SubEstimate is what a request is expected to cost, in money (micro-units,
// at the request's rate) and in tokens (input estimate plus output limit).
// UnitPriced marks a media operation that may be priced per unit (image,
// speech, transcription, rerank): token plans do not cover it.
type SubEstimate struct {
	Money      int64
	Tokens     int64
	UnitPriced bool
}

// ProviderGrace is how long after its period end a provider-renewed
// subscription stays active waiting for the provider's renewal before it
// is ended. A renewal that arrives later starts it again.
const ProviderGrace = 72 * time.Hour

// pendingTTL is how long a subscription may wait for its first payment.
const pendingTTL = 48 * time.Hour

// Subscription is one user's subscription to a plan.
type Subscription struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Payer       string    `json:"payer"`
	PlanID      string    `json:"plan_id"`
	PlanName    string    `json:"plan_name"`
	Status      string    `json:"status"`
	Period      string    `json:"period"` // week, month, year
	PriceMicro  int64     `json:"price_micro"`
	Unit        string    `json:"unit"`
	Allowance   int64     `json:"allowance"`
	Used        int64     `json:"used"`
	Held        int64     `json:"held"`
	Overage     string    `json:"overage"`
	GroupID     string    `json:"group_id,omitempty"`
	PrevGroupID string    `json:"prev_group_id,omitempty"`
	PeriodStart time.Time `json:"period_start,omitzero"`
	PeriodEnd   time.Time `json:"period_end,omitzero"`
	// CancelAtPeriodEnd: the subscription ends instead of renewing.
	CancelAtPeriodEnd bool   `json:"cancel_at_period_end"`
	Provider          string `json:"provider"`
	Renewal           string `json:"renewal"`
	ProviderRef       string `json:"provider_ref,omitempty"`
	// RefundDue: a payment for it arrived while another subscription of
	// the user was active, so it was recorded but not started.
	RefundDue bool      `json:"refund_due,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Current reports whether s is active and inside its period at now.
func (s *Subscription) Current(now time.Time) bool {
	return s.Status == SubActive && !now.Before(s.PeriodStart) && now.Before(s.PeriodEnd)
}

// SubscriptionFilter narrows ListSubscriptions; empty fields match all.
type SubscriptionFilter struct {
	UserID string
	Status string
}

// SubscriptionHold is the outcome of SubscriptionHold: Covered when the
// allowance takes the request; otherwise the request pays from the
// balance as without a plan.
type SubscriptionHold struct {
	Subscription Subscription
	Covered      bool
}

// Subscriptions is the subscription store. The SQL store implements it;
// get it with SubscriptionsOf.
type Subscriptions interface {
	// CreateSubscription stores a pending subscription, ending the user's
	// pending ones; ErrConflict when the user has an active one.
	CreateSubscription(ctx context.Context, s *Subscription) error
	GetSubscription(ctx context.Context, id string) (*Subscription, error)
	// SubscriptionByRef finds a subscription by its provider's id.
	SubscriptionByRef(ctx context.Context, provider, ref string) (*Subscription, error)
	ListSubscriptions(ctx context.Context, f SubscriptionFilter) ([]Subscription, error)
	// StartPeriod records t (its id makes it apply once: a provider's
	// invoice or order, or a balance renewal) and, when it applied, makes
	// the subscription active for [start, end) with a fresh allowance. A
	// period that ends no later than the current one only records t. A
	// negative t.AmountMicro is paid from t.Subject's balance and fails
	// with ErrInsufficientBalance. The first activation moves the user
	// (and the user's keys in the user's group) into the plan's group and
	// ends the user's other pending subscription. When another
	// subscription of the user is active, or the user was deleted, a
	// period paid at a provider is recorded and the subscription ended and
	// flagged RefundDue instead of started;
	// one paid from the balance is refused (ErrConflict) uncharged, as is
	// one paid from the balance for a subscription no longer pending.
	StartPeriod(ctx context.Context, id string, start, end time.Time, t *BalanceTxn) (bool, error)
	SetSubscriptionRef(ctx context.Context, id, ref string) error
	// SetRefundDue sets or clears the RefundDue flag (an admin refunded
	// the payment).
	SetRefundDue(ctx context.Context, id string, due bool) error
	SetCancelAtPeriodEnd(ctx context.Context, id string, cancel bool) error
	// EndSubscription ends it now and moves the user back out of the plan's
	// group; false when it had ended already.
	EndSubscription(ctx context.Context, id string) (bool, error)
	// RenewDue renews or ends active subscriptions whose period is over:
	// cancelled ones end; balance-renewed ones charge the price to the
	// payer for the next period, or end when the balance does not cover
	// it; provider-renewed ones end ProviderGrace after the period without
	// a renewal. Pending ones never paid for end after two days. One
	// subscription failing does not stop the others; the errors are
	// joined.
	RenewDue(ctx context.Context, now time.Time) (int64, error)
	// SubscriptionHold reserves a request's draw on the user's current
	// subscription: ErrNotFound without one, ErrAllowanceExhausted (with
	// the hold describing the subscription) when its allowance is used up
	// and overage is blocked, Covered=false when it is used up and the
	// balance pays, and Covered=false on a token plan for a unit-priced
	// request (est.UnitPriced), with either overage policy. It holds the estimate in the plan's unit (at least 1)
	// up to what is left, so concurrent requests see the allowance taken.
	// With balance overage, a request whose estimate is larger than what
	// is left is covered only when the payer's available balance (plus
	// credit limit) covers the rest, else Covered=false.
	SubscriptionHold(ctx context.Context, userID, payer, requestID string, est SubEstimate, now time.Time) (*SubscriptionHold, error)
	// SubscriptionSettle ends a request's draw: the request's units
	// (money: charge; tokens: tokens) come from what is left of the
	// allowance, never past it, and the rest, as money (tokens converted at
	// the request's own rate, charge/tokens; on a token plan a request with
	// a charge but no tokens pays all of it, and unitCharge, the part of
	// charge priced per media unit, is never covered), is charged to the payer's
	// balance (charge:<request>), with either overage policy and below
	// zero if need be: past a blocking plan's allowance it is a debt the
	// next top-up or balance renewal pays. false when the request was
	// settled or released before.
	SubscriptionSettle(ctx context.Context, requestID string, charge, unitCharge, tokens int64, note string) (bool, error)
	// SubscriptionRelease drops a request's draw uncharged.
	SubscriptionRelease(ctx context.Context, requestID string) (bool, error)
}

// SubscriptionsOf returns the store's subscriptions.
func SubscriptionsOf(s Store) (Subscriptions, bool) {
	l, ok := s.(Subscriptions)
	return l, ok
}

var _ Subscriptions = (*db)(nil)

// AddPeriod returns t plus one period ("week", "month" or "year"). A day
// past the end of the target month is clamped to its last day: Jan 31 plus
// a month is Feb 28 (29), Feb 29 plus a year Feb 28.
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
	last := first.AddDate(0, 1, -1).Day()
	return first.AddDate(0, 0, min(d, last)-1)
}

// AllowanceSubject is the ledger subject of a subscription's allowance.
func AllowanceSubject(id string) string { return "sub:" + id }

const subCols = `id, user_id, payer, plan_id, plan_name, status, period, price_micro, unit, allowance, used, held, overage,
group_id, prev_group_id, period_start, period_end, cancel_at_period_end, provider, renewal, provider_ref, refund_due, created_at, updated_at`

func scanSub(sc interface{ Scan(...any) error }) (*Subscription, error) {
	var s Subscription
	var start, end, created, updated int64
	if err := sc.Scan(&s.ID, &s.UserID, &s.Payer, &s.PlanID, &s.PlanName, &s.Status, &s.Period, &s.PriceMicro, &s.Unit,
		&s.Allowance, &s.Used, &s.Held, &s.Overage, &s.GroupID, &s.PrevGroupID, &start, &end, &s.CancelAtPeriodEnd,
		&s.Provider, &s.Renewal, &s.ProviderRef, &s.RefundDue, &created, &updated); err != nil {
		return nil, err
	}
	s.PeriodStart, s.PeriodEnd, s.CreatedAt, s.UpdatedAt = fromMs(start), fromMs(end), fromMs(created), fromMs(updated)
	return &s, nil
}

func lockSub(ctx context.Context, r run, id string) (*Subscription, error) {
	s, err := scanSub(r.row(ctx, `SELECT `+subCols+` FROM subscriptions WHERE id = ?`+r.d.ForUpdate(), id))
	return s, wrap("subscription", err)
}

// lockUser takes the user's row lock, which serializes changing which
// subscription of the user is live.
func lockUser(ctx context.Context, r run, userID string) error {
	var uid string
	return wrap("lock user", r.row(ctx, `SELECT id FROM users WHERE id = ?`+r.d.ForUpdate(), userID).Scan(&uid))
}

// lockUserSub locks a subscription's user, then the subscription. A user
// that was deleted is not an error: userGone reports it, and the
// subscription is locked all the same so that it can be ended.
func lockUserSub(ctx context.Context, r run, id string) (sub *Subscription, userGone bool, err error) {
	var uid string
	if err := r.row(ctx, `SELECT user_id FROM subscriptions WHERE id = ?`, id).Scan(&uid); err != nil {
		return nil, false, wrap("subscription", err)
	}
	if err := lockUser(ctx, r, uid); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return nil, false, err
		}
		userGone = true
	}
	sub, err = lockSub(ctx, r, id)
	return sub, userGone, err
}

// liveIDs are the ids of the user's subscriptions in status, other than
// except, in id order; read without a lock, to be locked by id.
func liveIDs(ctx context.Context, r run, userID, status, except string) ([]string, error) {
	rows, err := r.query(ctx, `SELECT id FROM subscriptions WHERE user_id = ? AND status = ? AND id <> ? ORDER BY id`, userID, status, except)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// activeOther is the id of the user's active subscription other than id,
// or "".
func activeOther(ctx context.Context, r run, userID, id string) (string, error) {
	var other string
	err := r.row(ctx, `SELECT id FROM subscriptions WHERE user_id = ? AND status = ? AND id <> ? LIMIT 1`, userID, SubActive, id).Scan(&other)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return other, wrap("subscription", err)
}

// endPending ends the user's pending subscriptions other than id; the
// caller holds the user's row lock.
func endPending(ctx context.Context, r run, userID, id string) error {
	ids, err := liveIDs(ctx, r, userID, SubPending, id)
	if err != nil {
		return wrap("end pending subscriptions", err)
	}
	for _, other := range ids {
		if _, err := r.exec(ctx, `UPDATE subscriptions SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
			SubEnded, time.Now().UnixMilli(), other, SubPending); err != nil {
			return wrap("end pending subscriptions", err)
		}
	}
	return nil
}

func (s *db) CreateSubscription(ctx context.Context, sub *Subscription) error {
	if sub.UserID == "" || sub.Payer == "" || sub.PlanID == "" {
		return fmt.Errorf("store: create subscription: user, payer and plan are required")
	}
	if sub.ID == "" {
		sub.ID = newID()
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	sub.Status, sub.CreatedAt, sub.UpdatedAt = SubPending, now, now
	sub.Used, sub.Held, sub.PeriodStart, sub.PeriodEnd, sub.PrevGroupID, sub.RefundDue = 0, 0, time.Time{}, time.Time{}, "", false
	return s.tx(ctx, func(r run) error {
		if err := lockUser(ctx, r, sub.UserID); err != nil {
			return wrap("create subscription", err)
		}
		other, err := activeOther(ctx, r, sub.UserID, sub.ID)
		if err != nil {
			return err
		}
		if other != "" {
			return fmt.Errorf("store: create subscription: %w: the user has an active subscription", ErrConflict)
		}
		if err := endPending(ctx, r, sub.UserID, sub.ID); err != nil {
			return err
		}
		_, err = r.exec(ctx, `INSERT INTO subscriptions (`+subCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?, '', 0, 0, ?, ?, ?, ?, ?, ?, ?)`,
			sub.ID, sub.UserID, sub.Payer, sub.PlanID, sub.PlanName, sub.Status, sub.Period, sub.PriceMicro, sub.Unit,
			sub.Allowance, sub.Overage, sub.GroupID, sub.CancelAtPeriodEnd, sub.Provider, sub.Renewal, sub.ProviderRef,
			false, ms(now), ms(now))
		return wrap("create subscription", err)
	})
}

func (s *db) GetSubscription(ctx context.Context, id string) (*Subscription, error) {
	sub, err := scanSub(s.r().row(ctx, `SELECT `+subCols+` FROM subscriptions WHERE id = ?`, id))
	return sub, wrap("get subscription", err)
}

func (s *db) SubscriptionByRef(ctx context.Context, provider, ref string) (*Subscription, error) {
	if ref == "" {
		return nil, fmt.Errorf("store: subscription by ref: %w", ErrNotFound)
	}
	sub, err := scanSub(s.r().row(ctx, `SELECT `+subCols+` FROM subscriptions WHERE provider = ? AND provider_ref = ?
ORDER BY created_at DESC, id DESC LIMIT 1`, provider, ref))
	return sub, wrap("subscription by ref", err)
}

func (s *db) ListSubscriptions(ctx context.Context, f SubscriptionFilter) ([]Subscription, error) {
	return list(ctx, s.r(), "list subscriptions", `SELECT `+subCols+` FROM subscriptions
WHERE (? = '' OR user_id = ?) AND (? = '' OR status = ?) ORDER BY created_at DESC, id DESC LIMIT 500`, scanSub,
		f.UserID, f.UserID, f.Status, f.Status)
}

func (s *db) SetSubscriptionRef(ctx context.Context, id, ref string) error {
	res, err := s.r().exec(ctx, `UPDATE subscriptions SET provider_ref = ?, updated_at = ? WHERE id = ?`, ref, time.Now().UnixMilli(), id)
	return mustAffect("set subscription ref", res, err)
}

func (s *db) SetRefundDue(ctx context.Context, id string, due bool) error {
	res, err := s.r().exec(ctx, `UPDATE subscriptions SET refund_due = ?, updated_at = ? WHERE id = ?`, due, time.Now().UnixMilli(), id)
	return mustAffect("set subscription refund", res, err)
}

func (s *db) SetCancelAtPeriodEnd(ctx context.Context, id string, cancel bool) error {
	res, err := s.r().exec(ctx, `UPDATE subscriptions SET cancel_at_period_end = ?, updated_at = ? WHERE id = ?`, cancel, time.Now().UnixMilli(), id)
	return mustAffect("set subscription cancel", res, err)
}

func (s *db) StartPeriod(ctx context.Context, id string, start, end time.Time, t *BalanceTxn) (bool, error) {
	if !end.After(start) {
		return false, fmt.Errorf("store: start period: the period ends before it starts")
	}
	var applied bool
	err := s.tx(ctx, func(r run) error {
		applied = false
		sub, userGone, err := lockUserSub(ctx, r, id)
		if err != nil {
			return err
		}
		if t != nil {
			if t.Subject == "" {
				t.Subject = sub.Payer
			}
			if t.Kind == "" {
				t.Kind = TxnSubscription
			}
			if t.Ref == "" {
				t.Ref = sub.ID
			}
		}
		if userGone {
			if t == nil || t.AmountMicro < 0 {
				return fmt.Errorf("store: start period: user: %w", ErrNotFound)
			}
			// Paid at the provider after the user was deleted: keep the
			// money on record and flag it for a refund.
			ok, err := credit(ctx, r, t)
			if err != nil || !ok {
				return err
			}
			applied = true
			_, err = r.exec(ctx, `UPDATE subscriptions SET status = ?, refund_due = ?, updated_at = ? WHERE id = ?`,
				SubEnded, true, time.Now().UnixMilli(), sub.ID)
			return wrap("start period", err)
		}
		activating := sub.Status != SubActive
		var other string
		if activating {
			if other, err = activeOther(ctx, r, sub.UserID, sub.ID); err != nil {
				return err
			}
			fromBalance := t != nil && t.AmountMicro < 0
			if (other != "" && (t == nil || fromBalance)) || (fromBalance && sub.Status != SubPending) {
				return fmt.Errorf("store: start period: %w: the user has another subscription", ErrConflict)
			}
		}
		if t != nil {
			ok, err := credit(ctx, r, t)
			if err != nil || !ok {
				return err
			}
		}
		applied = true
		if other != "" {
			// Paid, but another subscription is active: keep the money
			// on record and flag it for a refund.
			_, err := r.exec(ctx, `UPDATE subscriptions SET status = ?, refund_due = ?, updated_at = ? WHERE id = ?`,
				SubEnded, true, time.Now().UnixMilli(), sub.ID)
			return wrap("start period", err)
		}
		if activating {
			if err := endPending(ctx, r, sub.UserID, sub.ID); err != nil {
				return err
			}
		}
		return startPeriod(ctx, r, sub, start, end)
	})
	return applied, err
}

// startPeriod activates sub for [start, end); a period no later than the
// current one changes nothing but the status.
func startPeriod(ctx context.Context, r run, sub *Subscription, start, end time.Time) error {
	now := time.Now().UnixMilli()
	if sub.Status != SubActive && sub.GroupID != "" {
		var prev string
		if err := r.row(ctx, `SELECT group_id FROM users WHERE id = ?`, sub.UserID).Scan(&prev); err != nil {
			return wrap("start period", err)
		}
		if err := moveGroup(ctx, r, sub.UserID, prev, sub.GroupID); err != nil {
			return err
		}
		sub.PrevGroupID = prev
	}
	if end.After(sub.PeriodEnd) {
		sub.PeriodStart, sub.PeriodEnd, sub.Used, sub.Held = start.UTC().Truncate(time.Millisecond), end.UTC().Truncate(time.Millisecond), 0, 0
		if _, err := r.exec(ctx, `INSERT INTO balance_txns (id, subject, kind, amount_micro, balance_after_micro, ref, note, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`, txnID("allowance:grant:"+sub.ID+":"+strconv.FormatInt(ms(sub.PeriodStart), 10)),
			AllowanceSubject(sub.ID), TxnAllowance, sub.Allowance, sub.Allowance, sub.ID, sub.PlanName, now); err != nil {
			return wrap("start period", err)
		}
	}
	_, err := r.exec(ctx, `UPDATE subscriptions SET status = ?, period_start = ?, period_end = ?, used = ?, held = ?, prev_group_id = ?,
updated_at = ? WHERE id = ?`, SubActive, ms(sub.PeriodStart), ms(sub.PeriodEnd), sub.Used, sub.Held, sub.PrevGroupID, now, sub.ID)
	if err == nil {
		sub.Status = SubActive
	}
	return wrap("start period", err)
}

// moveGroup moves the user from group `from` to `to`, with the user's keys
// that were in `from` (keys copy the user's group when created). A user an
// admin moved elsewhere meanwhile is left alone, keys included. The keys
// are locked one by one in id order, as FlushTouches locks them.
func moveGroup(ctx context.Context, r run, userID, from, to string) error {
	if from == to {
		return nil
	}
	res, err := r.exec(ctx, `UPDATE users SET group_id = ?, updated_at = ? WHERE id = ? AND group_id = ?`,
		to, time.Now().UnixMilli(), userID, from)
	if err != nil {
		return wrap("move group", err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return wrap("move group", err)
	}
	rows, err := r.query(ctx, `SELECT id FROM api_keys WHERE user_id = ? AND group_id = ? ORDER BY id`, userID, from)
	if err != nil {
		return wrap("move group", err)
	}
	var keys []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return wrap("move group", err)
		}
		keys = append(keys, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return wrap("move group", err)
	}
	for _, id := range keys {
		if _, err := r.exec(ctx, `UPDATE api_keys SET group_id = ? WHERE id = ? AND group_id = ?`, to, id, from); err != nil {
			return wrap("move group", err)
		}
	}
	return nil
}

func (s *db) EndSubscription(ctx context.Context, id string) (bool, error) {
	var ended bool
	err := s.tx(ctx, func(r run) error {
		sub, _, err := lockUserSub(ctx, r, id)
		if err != nil {
			return err
		}
		ended, err = endSub(ctx, r, sub)
		return err
	})
	return ended, err
}

func endSub(ctx context.Context, r run, sub *Subscription) (bool, error) {
	if sub.Status == SubEnded {
		return false, nil
	}
	if sub.Status == SubActive && sub.GroupID != "" {
		if err := moveGroup(ctx, r, sub.UserID, sub.GroupID, sub.PrevGroupID); err != nil {
			return false, err
		}
	}
	_, err := r.exec(ctx, `UPDATE subscriptions SET status = ?, updated_at = ? WHERE id = ?`, SubEnded, time.Now().UnixMilli(), sub.ID)
	return err == nil, wrap("end subscription", err)
}

func (s *db) RenewDue(ctx context.Context, now time.Time) (int64, error) {
	stale, err := s.subIDs(ctx, `SELECT id FROM subscriptions WHERE status = ? AND created_at < ?`, SubPending, ms(now.Add(-pendingTTL)))
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, id := range stale {
		if err := s.expirePending(ctx, id, now); err != nil {
			errs = append(errs, fmt.Errorf("subscription %s: %w", id, err))
		}
	}
	due, err := s.subIDs(ctx, `SELECT id FROM subscriptions WHERE status = ? AND period_end <= ?`, SubActive, ms(now))
	if err != nil {
		return 0, errors.Join(append(errs, err)...)
	}
	var n int64
	for _, id := range due {
		changed, err := s.renew(ctx, id, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("subscription %s: %w", id, err))
			continue
		}
		if changed {
			n++
		}
	}
	return n, errors.Join(errs...)
}

// subIDs lists subscription ids, read without a lock.
func (s *db) subIDs(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.r().query(ctx, q, args...)
	if err != nil {
		return nil, wrap("renew subscriptions", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, wrap("renew subscriptions", err)
		}
		ids = append(ids, id)
	}
	return ids, wrap("renew subscriptions", rows.Err())
}

// expirePending ends a subscription still pending pendingTTL after it was
// created.
func (s *db) expirePending(ctx context.Context, id string, now time.Time) error {
	return s.tx(ctx, func(r run) error {
		sub, _, err := lockUserSub(ctx, r, id)
		if err != nil {
			return err
		}
		if sub.Status != SubPending || !sub.CreatedAt.Before(now.Add(-pendingTTL)) {
			return nil
		}
		_, err = endSub(ctx, r, sub)
		return err
	})
}

// renew renews or ends one subscription whose period is over: a payer
// whose balance does not cover the next period ends it, decided in the
// same transaction as the renewal. A deleted user's subscription ends.
func (s *db) renew(ctx context.Context, id string, now time.Time) (bool, error) {
	var changed bool
	err := s.tx(ctx, func(r run) error {
		changed = false
		sub, userGone, err := lockUserSub(ctx, r, id)
		if err != nil {
			return err
		}
		if sub.Status != SubActive || sub.PeriodEnd.After(now) {
			return nil
		}
		switch {
		case userGone:
			changed, err = endSub(ctx, r, sub)
			return err
		case sub.Renewal == RenewProvider:
			if now.Before(sub.PeriodEnd.Add(ProviderGrace)) {
				return nil
			}
			changed, err = endSub(ctx, r, sub)
			return err
		case sub.CancelAtPeriodEnd:
			changed, err = endSub(ctx, r, sub)
			return err
		}
		start := sub.PeriodEnd
		if !AddPeriod(start, sub.Period).After(now) {
			start = now // missed whole periods (the relay was down) are not billed
		}
		end := AddPeriod(start, sub.Period)
		if err := ensureAccount(ctx, r, sub.Payer); err != nil {
			return err
		}
		a, err := account(ctx, r, sub.Payer)
		if err != nil {
			return err
		}
		if a.BalanceMicro-a.HeldMicro-sub.PriceMicro < -a.CreditLimitMicro {
			changed, err = endSub(ctx, r, sub)
			return err
		}
		t := &BalanceTxn{ID: "subscription:" + sub.ID + ":" + strconv.FormatInt(ms(start), 10), Subject: sub.Payer,
			Kind: TxnSubscription, AmountMicro: -sub.PriceMicro, Ref: sub.ID, Note: sub.PlanName + " renewal"}
		ok, err := credit(ctx, r, t)
		if err != nil || !ok {
			return err
		}
		changed = true
		return startPeriod(ctx, r, sub, start, end)
	})
	return changed, err
}

type subHold struct {
	subID, payer string
	periodStart  int64
	amount       int64
}

func (s *db) SubscriptionHold(ctx context.Context, userID, payer, requestID string, est SubEstimate, now time.Time) (*SubscriptionHold, error) {
	requestID = holdID(requestID)
	var out *SubscriptionHold
	err := s.tx(ctx, func(r run) error {
		out = nil
		sub, err := lockCurrent(ctx, r, userID, now)
		if err != nil {
			return err
		}
		out = &SubscriptionHold{Subscription: *sub}
		if sub.Unit == UnitTokens && est.UnitPriced {
			return nil // the balance pays it as without a plan
		}
		left := sub.Allowance - sub.Used - sub.Held
		if left <= 0 {
			if sub.Overage == OverageBalance {
				return nil
			}
			return ErrAllowanceExhausted
		}
		need := est.Money
		if sub.Unit == UnitTokens {
			need = est.Tokens
		}
		need = max(need, 1)
		if need > left && sub.Overage == OverageBalance {
			rest := need - left
			if sub.Unit == UnitTokens {
				rest = mulDiv(max(est.Money, 0), rest, need)
			}
			a, err := account(ctx, r, payer)
			if err != nil {
				return err
			}
			if a.BalanceMicro-a.HeldMicro+a.CreditLimitMicro < rest {
				return nil // the balance pays all of it, or refuses
			}
		}
		amount := min(need, left)
		res, err := r.exec(ctx, `INSERT INTO subscription_holds (request_id, subscription_id, payer, period_start, amount, created_at)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (request_id) DO NOTHING`, requestID, sub.ID, payer, ms(sub.PeriodStart), amount, now.UnixMilli())
		if err != nil {
			return wrap("subscription hold", err)
		}
		out.Covered = true
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // held before
		}
		if _, err := r.exec(ctx, `UPDATE subscriptions SET held = held + ?, updated_at = ? WHERE id = ?`, amount, now.UnixMilli(), sub.ID); err != nil {
			return wrap("subscription hold", err)
		}
		out.Subscription.Held += amount
		return nil
	})
	if err != nil && !errors.Is(err, ErrAllowanceExhausted) {
		return nil, err
	}
	return out, err
}

// lockCurrent locks the user's current subscription: found without a lock
// (through the user's index), then locked by id and checked again.
func lockCurrent(ctx context.Context, r run, userID string, now time.Time) (*Subscription, error) {
	for range 3 {
		var id string
		err := r.row(ctx, `SELECT id FROM subscriptions WHERE user_id = ? AND status = ? AND period_start <= ? AND period_end > ?
ORDER BY period_end DESC, id LIMIT 1`, userID, SubActive, ms(now), ms(now)).Scan(&id)
		if err != nil {
			return nil, wrap("subscription hold", err)
		}
		sub, err := lockSub(ctx, r, id)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if err == nil && sub.UserID == userID && sub.Current(now) {
			return sub, nil
		}
	}
	return nil, fmt.Errorf("store: subscription hold: %w", ErrNotFound)
}

// mulDiv is a*b/c for non-negative a and 0 <= b <= c, without overflow.
func mulDiv(a, b, c int64) int64 {
	if c <= 0 || a <= 0 || b <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	q, _ := bits.Div64(hi, lo, uint64(c))
	return int64(q)
}

// takeSubHold deletes a request's draw, returning it.
func takeSubHold(ctx context.Context, r run, requestID string) (subHold, bool, error) {
	requestID = holdID(requestID)
	var h subHold
	err := r.row(ctx, `SELECT subscription_id, payer, period_start, amount FROM subscription_holds WHERE request_id = ?`+r.d.ForUpdate(),
		requestID).Scan(&h.subID, &h.payer, &h.periodStart, &h.amount)
	if errors.Is(err, sql.ErrNoRows) {
		return h, false, nil
	}
	if err != nil {
		return h, false, wrap("take subscription hold", err)
	}
	res, err := r.exec(ctx, `DELETE FROM subscription_holds WHERE request_id = ?`, requestID)
	if err != nil {
		return h, false, wrap("take subscription hold", err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return h, false, wrap("take subscription hold", err)
	}
	return h, true, nil
}

// heldAfter is sub's held amount once h's draw is dropped: a draw of an
// earlier period was reset with it.
func heldAfter(sub *Subscription, h subHold) int64 {
	if h.periodStart != ms(sub.PeriodStart) {
		return sub.Held
	}
	return max(sub.Held-h.amount, 0)
}

func (s *db) SubscriptionSettle(ctx context.Context, requestID string, charge, unitCharge, tokens int64, note string) (bool, error) {
	var applied bool
	err := s.tx(ctx, func(r run) error {
		applied = false
		h, ok, err := takeSubHold(ctx, r, requestID)
		if err != nil || !ok {
			return err
		}
		applied = true
		sub, err := lockSub(ctx, r, h.subID)
		if err != nil {
			return err
		}
		held := heldAfter(sub, h)
		charge, tokens = max(charge, 0), max(tokens, 0)
		unitCharge = min(max(unitCharge, 0), charge)
		units := charge
		if sub.Unit == UnitTokens {
			units = tokens
		}
		draw := min(units, max(sub.Allowance-sub.Used-held, 0))
		excess := units - draw
		if sub.Unit == UnitTokens {
			if units == 0 {
				excess = charge // priced per unit (an image, a second of audio): no token covers it
			} else {
				// Tokens cover their own price only; units are paid apart.
				excess = unitCharge + mulDiv(charge-unitCharge, excess, units)
			}
		}
		used := sub.Used + draw
		now := time.Now().UnixMilli()
		if _, err := r.exec(ctx, `UPDATE subscriptions SET used = ?, held = ?, updated_at = ? WHERE id = ?`, used, held, now, sub.ID); err != nil {
			return wrap("subscription settle", err)
		}
		if draw > 0 {
			if _, err := r.exec(ctx, `INSERT INTO balance_txns (id, subject, kind, amount_micro, balance_after_micro, request_id, ref, note, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`, txnID("allowance:"+requestID), AllowanceSubject(sub.ID), TxnAllowance,
				-draw, sub.Allowance-used, holdID(requestID), sub.ID, note, now); err != nil {
				return wrap("subscription settle", err)
			}
		}
		if excess > 0 {
			if err := ensureAccount(ctx, r, h.payer); err != nil {
				return err
			}
			_, err = settle(ctx, r, h.payer, 0, h.payer, requestID, excess, note)
			return err
		}
		return nil
	})
	return applied, err
}

func (s *db) SubscriptionRelease(ctx context.Context, requestID string) (bool, error) {
	var applied bool
	err := s.tx(ctx, func(r run) error {
		applied = false
		h, ok, err := takeSubHold(ctx, r, requestID)
		if err != nil || !ok {
			return err
		}
		applied = true
		return releaseSubDraw(ctx, r, h)
	})
	return applied, err
}

func releaseSubDraw(ctx context.Context, r run, h subHold) error {
	if h.amount == 0 {
		return nil
	}
	_, err := r.exec(ctx, `UPDATE subscriptions SET held = CASE WHEN held > ? THEN held - ? ELSE 0 END WHERE id = ? AND period_start = ?`,
		h.amount, h.amount, h.subID, h.periodStart)
	return wrap("release subscription draw", err)
}

// expireSubHolds is ExpireHolds for allowance draws.
func (s *db) expireSubHolds(ctx context.Context, before time.Time) (int64, error) {
	rows, err := s.r().query(ctx, `SELECT request_id, subscription_id, period_start, amount FROM subscription_holds
WHERE amount > 0 AND created_at < ?`, ms(before))
	if err != nil {
		return 0, wrap("expire subscription holds", err)
	}
	type stale struct {
		id string
		h  subHold
	}
	var todo []stale
	for rows.Next() {
		var x stale
		if err := rows.Scan(&x.id, &x.h.subID, &x.h.periodStart, &x.h.amount); err != nil {
			rows.Close()
			return 0, wrap("expire subscription holds", err)
		}
		todo = append(todo, x)
	}
	rows.Close()
	var n int64
	for _, x := range todo {
		var released bool
		err := s.tx(ctx, func(r run) error {
			res, err := r.exec(ctx, `UPDATE subscription_holds SET amount = 0 WHERE request_id = ? AND amount = ?`, x.id, x.h.amount)
			if err != nil {
				return wrap("expire subscription holds", err)
			}
			k, _ := res.RowsAffected()
			if released = k > 0; !released {
				return nil
			}
			return releaseSubDraw(ctx, r, x.h)
		})
		if err != nil {
			return n, err
		}
		if released {
			n++
		}
	}
	return n, wrap("expire subscription holds", s.deleteSpent(ctx, "subscription_holds", "amount", before.Add(-24*time.Hour)))
}

// settleRecordedSubs is SettleRecorded for allowance draws.
func (s *db) settleRecordedSubs(ctx context.Context, before time.Time) (int64, error) {
	type pending struct {
		id, model                  string
		charge, unitCharge, tokens int64
	}
	rows, err := s.r().query(ctx, `SELECT h.request_id, r.charge_micro, r.model,
r.input_tokens + r.cache_read_tokens + r.cache_write_5m_tokens + r.cache_write_1h_tokens + r.output_tokens, r.usage, r.attempts
FROM subscription_holds h JOIN request_log r ON r.id = h.request_id WHERE h.created_at < ?`, ms(before))
	if err != nil {
		return 0, wrap("settle recorded subscriptions", err)
	}
	var todo []pending
	for rows.Next() {
		var p pending
		var usage, attempts []byte
		if err := rows.Scan(&p.id, &p.charge, &p.model, &p.tokens, &usage, &attempts); err != nil {
			rows.Close()
			return 0, wrap("settle recorded subscriptions", err)
		}
		p.unitCharge = recordedUnitCharge(usage, attempts)
		todo = append(todo, p)
	}
	rows.Close()
	var n int64
	for _, p := range todo {
		ok, err := s.SubscriptionSettle(ctx, p.id, p.charge, p.unitCharge, p.tokens, p.model)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// recordedUnitCharge is the unit-priced part of a logged request's charge,
// from its usage and the charged attempt's rates.
func recordedUnitCharge(usage, attempts []byte) int64 {
	var u ir.Usage
	var as []plugin.AttemptRecord
	if json.Unmarshal(usage, &u) != nil || json.Unmarshal(attempts, &as) != nil {
		return 0
	}
	for _, a := range as {
		if a.Charge != nil {
			return a.Charge.Rates.UnitCharge(u)
		}
	}
	return 0
}
