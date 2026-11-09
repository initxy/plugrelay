package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/initxy/plugrelay/internal/sqldialect"
)

// Balance ledger (sell kit): prepaid money per subject ("user:<id>",
// "group:<id>", "key:<id>") in micro-units of the site currency.
//
// A request first holds an amount (held_micro grows, the balance does
// not), then either captures its charge (one "charge" transaction, the
// hold released) or releases the hold. The hold row is the request's
// settle-or-refund marker, so whichever of the two runs first wins and
// the other is a no-op; the charge transaction id ("charge:<request>") is
// unique, so a request is never charged twice. Every other change is a
// transaction whose id is the caller's idempotency key.

var (
	// ErrInsufficientBalance: the change would take the available balance
	// below the subject's credit limit.
	ErrInsufficientBalance = errors.New("store: insufficient balance")
	// Redeem failures.
	ErrRedeemInvalid     = errors.New("store: redeem code invalid")
	ErrRedeemExpired     = errors.New("store: redeem code expired")
	ErrRedeemUsedUp      = errors.New("store: redeem code used up")
	ErrRedeemAlreadyUsed = errors.New("store: redeem code already used by this account")
)

// TxnKind is the kind of a balance transaction.
type TxnKind string

const (
	TxnTopup      TxnKind = "topup"
	TxnCharge     TxnKind = "charge"
	TxnRefund     TxnKind = "refund"
	TxnRedeem     TxnKind = "redeem"
	TxnAdjustment TxnKind = "adjustment"
)

// BalanceAccount is one subject's balance.
type BalanceAccount struct {
	Subject          string    `json:"subject"`
	BalanceMicro     int64     `json:"balance_micro"`
	HeldMicro        int64     `json:"held_micro"`
	CreditLimitMicro int64     `json:"credit_limit_micro"`
	UpdatedAt        time.Time `json:"updated_at,omitzero"`
}

// Available is what new requests may still reserve, before the credit limit.
func (a BalanceAccount) Available() int64 { return a.BalanceMicro - a.HeldMicro }

// BalanceTxn is one balance change.
type BalanceTxn struct {
	ID                string    `json:"id"`
	Subject           string    `json:"subject"`
	Kind              TxnKind   `json:"kind"`
	AmountMicro       int64     `json:"amount_micro"`
	BalanceAfterMicro int64     `json:"balance_after_micro"`
	RequestID         string    `json:"request_id,omitempty"`
	Ref               string    `json:"ref,omitempty"`
	Note              string    `json:"note,omitempty"`
	Actor             string    `json:"actor,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

// TxnQuery pages transactions newest first; empty fields match all.
type TxnQuery struct {
	Subject string
	Kind    TxnKind
	Cursor  string
	Limit   int
}

// RedeemCode is a stored code; only CodeHash identifies it.
type RedeemCode struct {
	ID          string    `json:"id"`
	CodeHash    string    `json:"-"`
	Last4       string    `json:"last4"`
	Batch       string    `json:"batch,omitempty"`
	AmountMicro int64     `json:"amount_micro"`
	MaxUses     int       `json:"max_uses"`
	Uses        int       `json:"uses"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
	Disabled    bool      `json:"disabled,omitempty"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Ledger is the balance and redeem-code store. The SQL store implements it;
// get it with LedgerOf.
type Ledger interface {
	// Account returns a subject's balance; a subject never credited reads
	// as zero.
	Account(ctx context.Context, subject string) (*BalanceAccount, error)
	ListAccounts(ctx context.Context) ([]BalanceAccount, error)
	// Hold reserves amount for requestID if the available balance covers
	// it within the credit limit (and is positive); else
	// ErrInsufficientBalance. A second hold for the same request changes
	// nothing.
	Hold(ctx context.Context, subject, requestID string, amount int64) (*BalanceAccount, error)
	// Capture ends the request's hold and charges it; false when the hold
	// was already captured or released.
	Capture(ctx context.Context, requestID string, charge int64, note string) (bool, error)
	// Charge ends the request's hold, if any is left, and charges subject
	// once per request id, in full: below zero and past the credit limit,
	// like an overrun. For work billed long after it was held (batches),
	// when the hold may have lapsed or never been granted. A zero charge
	// only releases. false when nothing changed (charged before, or no hold
	// to release).
	Charge(ctx context.Context, subject, requestID string, charge int64, note string) (bool, error)
	// Release ends the request's hold without a charge; false when it was
	// already captured or released.
	Release(ctx context.Context, requestID string) (bool, error)
	// Credit applies t (positive or negative) once per t.ID. A negative
	// amount that would pass the credit limit is ErrInsufficientBalance.
	Credit(ctx context.Context, t *BalanceTxn) (applied bool, acct *BalanceAccount, err error)
	GetTxn(ctx context.Context, id string) (*BalanceTxn, error)
	SetCreditLimit(ctx context.Context, subject string, limitMicro int64) error
	Transactions(ctx context.Context, q TxnQuery) (txns []BalanceTxn, next string, err error)
	// ExpireHolds releases the amounts of holds older than before (a node
	// died mid-request) but keeps their markers, so a late capture still
	// charges; markers older than a day past before are deleted. Allowance
	// draws of subscriptions expire alike.
	// Batch holds (request id "batch:…") are released BatchHoldGrace
	// later: a batch runs for up to a day and is billed after.
	ExpireHolds(ctx context.Context, before time.Time) (int64, error)
	// SettleRecorded settles holds made before `before` whose request is
	// already in the request log, with the logged charge (zero releases):
	// the request finished but its settle failed. The request log is an
	// at-least-once ledger (ADR-0012), so a charge whose settle hit a
	// database error is collected from it, once (charge:<request>).
	// Allowance draws of subscriptions are settled alike.
	SettleRecorded(ctx context.Context, before time.Time) (int64, error)

	CreateRedeemCodes(ctx context.Context, codes []RedeemCode) error
	ListRedeemCodes(ctx context.Context, batch string) ([]RedeemCode, error)
	SetRedeemCodeDisabled(ctx context.Context, id string, disabled bool) error
	// Redeem credits the code's amount to subject in one transaction; the
	// code is used once per actor (or per subject when actor is empty).
	Redeem(ctx context.Context, codeHash, subject, actor string, now time.Time) (*BalanceTxn, error)
	RedeemUses(ctx context.Context, codeID string) ([]RedeemUse, error)
	SetRedeemBatchDisabled(ctx context.Context, batch string, disabled bool) error

	// Payment orders, coupons, referrals and check-ins (operations.go).
	CreateOrder(ctx context.Context, o *PaymentOrder) error
	GetOrder(ctx context.Context, id string) (*PaymentOrder, error)
	PayOrder(ctx context.Context, id string, at time.Time) (bool, error)
	CancelOrder(ctx context.Context, id string, release bool) (bool, error)
	ListOrders(ctx context.Context, q OrderQuery) ([]PaymentOrder, string, error)
	OrderTotals(ctx context.Context, since time.Time) (OrderTotals, error)
	CreateCoupon(ctx context.Context, c *Coupon) error
	UpdateCoupon(ctx context.Context, c *Coupon) error
	GetCoupon(ctx context.Context, id string) (*Coupon, error)
	ListCoupons(ctx context.Context) ([]Coupon, error)
	CouponUses(ctx context.Context, couponID string) ([]CouponUse, error)
	CheckCoupon(ctx context.Context, code, userID string, amount int64, now time.Time) (*Coupon, int64, error)
	ReserveCoupon(ctx context.Context, code, userID, orderID string, amount int64, now time.Time) (*Coupon, int64, error)
	ReleaseCoupon(ctx context.Context, orderID string) error
	ReferralCode(ctx context.Context, userID string, newCode func() string) (string, error)
	Refer(ctx context.Context, code, inviteeID string, now time.Time) (string, error)
	ReferralOf(ctx context.Context, inviteeID string) (*Referral, error)
	ListReferrals(ctx context.Context, inviterID string, limit int) ([]Referral, error)
	RewardReferral(ctx context.Context, inviteeID, subject, orderID string, reward, limit int64, now time.Time) (*BalanceTxn, error)
	CheckIn(ctx context.Context, userID, subject, day string, amount int64, now time.Time) (*BalanceTxn, bool, error)
	CheckIns(ctx context.Context, userID string, limit int) ([]BalanceTxn, error)
}

// BatchHoldGrace is how much longer than other holds a batch hold
// ("batch:…") lasts before ExpireHolds releases it.
const BatchHoldGrace = 72 * time.Hour

// LedgerOf returns the store's ledger.
func LedgerOf(s Store) (Ledger, bool) {
	l, ok := s.(Ledger)
	return l, ok
}

var _ Ledger = (*db)(nil)

const accountCols = `subject, balance_micro, held_micro, credit_limit_micro, updated_at`

func scanAccount(sc interface{ Scan(...any) error }) (*BalanceAccount, error) {
	var a BalanceAccount
	var up int64
	if err := sc.Scan(&a.Subject, &a.BalanceMicro, &a.HeldMicro, &a.CreditLimitMicro, &up); err != nil {
		return nil, err
	}
	a.UpdatedAt = fromMs(up)
	return &a, nil
}

// ensureAccount creates subject's row and takes its write lock. A no-op
// update rather than DO NOTHING: on MySQL the duplicate check of DO NOTHING
// takes a shared lock, and two holds upgrading it for their UPDATE
// deadlock.
func ensureAccount(ctx context.Context, r run, subject string) error {
	_, err := r.exec(ctx, `INSERT INTO balances (subject, updated_at) VALUES (?, ?) ON CONFLICT (subject) DO UPDATE SET subject = excluded.subject`,
		subject, time.Now().UnixMilli())
	return wrap("ensure account", err)
}

func account(ctx context.Context, r run, subject string) (*BalanceAccount, error) {
	a, err := scanAccount(r.row(ctx, `SELECT `+accountCols+` FROM balances WHERE subject = ?`, subject))
	if errors.Is(err, sql.ErrNoRows) {
		return &BalanceAccount{Subject: subject}, nil
	}
	return a, wrap("account", err)
}

func (s *db) Account(ctx context.Context, subject string) (*BalanceAccount, error) {
	return account(ctx, s.r(), subject)
}

func (s *db) ListAccounts(ctx context.Context) ([]BalanceAccount, error) {
	return list(ctx, s.r(), "list accounts", `SELECT `+accountCols+` FROM balances ORDER BY subject`, scanAccount)
}

func (s *db) Hold(ctx context.Context, subject, requestID string, amount int64) (*BalanceAccount, error) {
	amount = max(amount, 0)
	requestID = holdID(requestID)
	var out *BalanceAccount
	err := s.tx(ctx, func(r run) error {
		if err := ensureAccount(ctx, r, subject); err != nil {
			return err
		}
		res, err := r.exec(ctx, `INSERT INTO balance_holds (request_id, subject, amount_micro, created_at) VALUES (?, ?, ?, ?)
ON CONFLICT (request_id) DO NOTHING`, requestID, subject, amount, time.Now().UnixMilli())
		if err != nil {
			return wrap("hold", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			out, err = account(ctx, r, subject)
			return err
		}
		res, err = r.exec(ctx, `UPDATE balances SET held_micro = held_micro + ?, updated_at = ?
WHERE subject = ? AND balance_micro - held_micro - ? >= -credit_limit_micro AND balance_micro - held_micro + credit_limit_micro > 0`,
			amount, time.Now().UnixMilli(), subject, amount)
		if err != nil {
			return wrap("hold", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrInsufficientBalance
		}
		out, err = account(ctx, r, subject)
		return err
	})
	if errors.Is(err, ErrInsufficientBalance) {
		a, aerr := s.Account(ctx, subject)
		if aerr != nil {
			return nil, aerr
		}
		return a, err
	}
	return out, err
}

// takeHold deletes a request's hold, returning its subject and amount. The
// locking read sees an amount a concurrent expiry just zeroed.
func takeHold(ctx context.Context, r run, requestID string) (subject string, amount int64, ok bool, err error) {
	requestID = holdID(requestID)
	err = r.row(ctx, `SELECT subject, amount_micro FROM balance_holds WHERE request_id = ?`+r.d.ForUpdate(), requestID).Scan(&subject, &amount)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, wrap("take hold", err)
	}
	res, err := r.exec(ctx, `DELETE FROM balance_holds WHERE request_id = ?`, requestID)
	if err != nil {
		return "", 0, false, wrap("take hold", err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return "", 0, false, wrap("take hold", err)
	}
	return subject, amount, true, nil
}

// balanceOf reads a balance inside the transaction that just updated it.
func balanceOf(ctx context.Context, r run, subject string) (int64, error) {
	var v int64
	err := r.row(ctx, `SELECT balance_micro FROM balances WHERE subject = ?`, subject).Scan(&v)
	return v, err
}

func (s *db) Capture(ctx context.Context, requestID string, charge int64, note string) (bool, error) {
	var applied bool
	err := s.tx(ctx, func(r run) error {
		applied = false
		subject, held, ok, err := takeHold(ctx, r, requestID)
		if err != nil || !ok {
			return err
		}
		applied = true
		_, err = settle(ctx, r, subject, held, subject, requestID, charge, note)
		return err
	})
	return applied, err
}

func (s *db) Charge(ctx context.Context, subject, requestID string, charge int64, note string) (bool, error) {
	var applied bool
	err := s.tx(ctx, func(r run) error {
		holder, held, ok, err := takeHold(ctx, r, requestID)
		if err != nil {
			return err
		}
		if !ok {
			holder, held = subject, 0
		}
		if charge > 0 {
			if err := ensureAccount(ctx, r, subject); err != nil {
				return err
			}
		}
		charged, err := settle(ctx, r, holder, held, subject, requestID, charge, note)
		applied = charged || (charge <= 0 && ok)
		return err
	})
	return applied, err
}

// settle ends a hold of held on holder and charges subject once per
// request (charge:<request>), reporting whether it charged; a request
// charged before is not charged again. The charge may take the balance
// below zero.
func settle(ctx context.Context, r run, holder string, held int64, subject, requestID string, charge int64, note string) (bool, error) {
	charge = max(charge, 0)
	now := time.Now().UnixMilli()
	if held != 0 {
		if _, err := r.exec(ctx, `UPDATE balances SET held_micro = held_micro - ?, updated_at = ? WHERE subject = ?`, held, now, holder); err != nil {
			return false, wrap("settle", err)
		}
	}
	if charge == 0 {
		return false, nil
	}
	id := txnID("charge:" + requestID)
	res, err := r.exec(ctx, `INSERT INTO balance_txns (id, subject, kind, amount_micro, request_id, note, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`, id, subject, TxnCharge, -charge, holdID(requestID), note, now)
	if err != nil {
		return false, wrap("settle", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // charged before under another hold of the same id
	}
	if _, err := r.exec(ctx, `UPDATE balances SET balance_micro = balance_micro - ?, updated_at = ? WHERE subject = ?`, charge, now, subject); err != nil {
		return false, wrap("settle", err)
	}
	after, err := balanceOf(ctx, r, subject)
	if err != nil {
		return false, wrap("settle", err)
	}
	_, err = r.exec(ctx, `UPDATE balance_txns SET balance_after_micro = ? WHERE id = ?`, after, id)
	return true, wrap("settle", err)
}

// Column widths of ledger ids (MySQL); longer ids are stored hashed
// (sqldialect.Fit), on every database alike.
const (
	holdIDWidth = 191 // balance_holds.request_id, balance_txns.request_id
	txnIDWidth  = 255 // balance_txns.id
)

func holdID(id string) string { return sqldialect.Fit(id, holdIDWidth) }
func txnID(id string) string  { return sqldialect.Fit(id, txnIDWidth) }

func (s *db) Release(ctx context.Context, requestID string) (bool, error) {
	var applied bool
	err := s.tx(ctx, func(r run) error {
		applied = false
		subject, held, ok, err := takeHold(ctx, r, requestID)
		if err != nil || !ok {
			return err
		}
		applied = true
		_, err = r.exec(ctx, `UPDATE balances SET held_micro = held_micro - ?, updated_at = ? WHERE subject = ?`,
			held, time.Now().UnixMilli(), subject)
		return wrap("release", err)
	})
	return applied, err
}

func (s *db) Credit(ctx context.Context, t *BalanceTxn) (bool, *BalanceAccount, error) {
	var applied bool
	var acct *BalanceAccount
	err := s.tx(ctx, func(r run) error {
		var err error
		applied, err = credit(ctx, r, t)
		if err != nil {
			return err
		}
		acct, err = account(ctx, r, t.Subject)
		return err
	})
	return applied, acct, err
}

// credit inserts t and applies it, once per t.ID.
func credit(ctx context.Context, r run, t *BalanceTxn) (bool, error) {
	if t.ID == "" || t.Subject == "" || t.Kind == "" {
		return false, fmt.Errorf("store: credit: id, subject and kind are required")
	}
	if err := ensureAccount(ctx, r, t.Subject); err != nil {
		return false, err
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC().Truncate(time.Millisecond)
	}
	t.ID, t.RequestID = txnID(t.ID), holdID(t.RequestID)
	res, err := r.exec(ctx, `INSERT INTO balance_txns (id, subject, kind, amount_micro, request_id, ref, note, actor, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		t.ID, t.Subject, t.Kind, t.AmountMicro, t.RequestID, t.Ref, t.Note, t.Actor, ms(t.CreatedAt))
	if err != nil {
		return false, wrap("credit", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	cond := ""
	if t.AmountMicro < 0 {
		cond = ` AND balance_micro - held_micro + ? >= -credit_limit_micro`
	}
	args := []any{t.AmountMicro, time.Now().UnixMilli(), t.Subject}
	if t.AmountMicro < 0 {
		args = append(args, t.AmountMicro)
	}
	res, err = r.exec(ctx, `UPDATE balances SET balance_micro = balance_micro + ?, updated_at = ? WHERE subject = ?`+cond, args...)
	if err != nil {
		return false, wrap("credit", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, ErrInsufficientBalance
	}
	if t.BalanceAfterMicro, err = balanceOf(ctx, r, t.Subject); err != nil {
		return false, wrap("credit", err)
	}
	_, err = r.exec(ctx, `UPDATE balance_txns SET balance_after_micro = ? WHERE id = ?`, t.BalanceAfterMicro, t.ID)
	return true, wrap("credit", err)
}

const txnCols = `id, subject, kind, amount_micro, balance_after_micro, request_id, ref, note, actor, created_at`

func scanTxn(sc interface{ Scan(...any) error }) (*BalanceTxn, error) {
	var t BalanceTxn
	var at int64
	if err := sc.Scan(&t.ID, &t.Subject, &t.Kind, &t.AmountMicro, &t.BalanceAfterMicro, &t.RequestID, &t.Ref, &t.Note, &t.Actor, &at); err != nil {
		return nil, err
	}
	t.CreatedAt = fromMs(at)
	return &t, nil
}

func (s *db) GetTxn(ctx context.Context, id string) (*BalanceTxn, error) {
	t, err := scanTxn(s.r().row(ctx, `SELECT `+txnCols+` FROM balance_txns WHERE id = ?`, txnID(id)))
	return t, wrap("get txn", err)
}

func (s *db) SetCreditLimit(ctx context.Context, subject string, limit int64) error {
	if limit < 0 {
		return fmt.Errorf("store: credit limit must not be negative")
	}
	return s.tx(ctx, func(r run) error {
		if err := ensureAccount(ctx, r, subject); err != nil {
			return err
		}
		_, err := r.exec(ctx, `UPDATE balances SET credit_limit_micro = ?, updated_at = ? WHERE subject = ?`, limit, time.Now().UnixMilli(), subject)
		return wrap("set credit limit", err)
	})
}

func (s *db) Transactions(ctx context.Context, q TxnQuery) ([]BalanceTxn, string, error) {
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	where := `WHERE (? = '' OR subject = ?) AND (? = '' OR kind = ?)`
	args := []any{q.Subject, q.Subject, q.Kind, q.Kind}
	if q.Cursor != "" {
		t, id, err := decodeCursor(q.Cursor)
		if err != nil {
			return nil, "", err
		}
		where += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, t, t, id)
	}
	args = append(args, limit+1)
	out, err := list(ctx, s.r(), "transactions", `SELECT `+txnCols+` FROM balance_txns `+where+
		` ORDER BY created_at DESC, id DESC LIMIT ?`, scanTxn, args...)
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

func (s *db) ExpireHolds(ctx context.Context, before time.Time) (int64, error) {
	type hold struct {
		id, subject string
		amount      int64
	}
	rows, err := s.r().query(ctx, `SELECT request_id, subject, amount_micro FROM balance_holds
WHERE amount_micro > 0 AND ((created_at < ? AND request_id NOT LIKE 'batch:%') OR created_at < ?)`, ms(before), ms(before.Add(-BatchHoldGrace)))
	if err != nil {
		return 0, wrap("expire holds", err)
	}
	var stale []hold
	for rows.Next() {
		var h hold
		if err := rows.Scan(&h.id, &h.subject, &h.amount); err != nil {
			rows.Close()
			return 0, wrap("expire holds", err)
		}
		stale = append(stale, h)
	}
	rows.Close()
	var n int64
	for _, h := range stale {
		var released bool
		err := s.tx(ctx, func(r run) error {
			res, err := r.exec(ctx, `UPDATE balance_holds SET amount_micro = 0 WHERE request_id = ? AND amount_micro = ?`, h.id, h.amount)
			if err != nil {
				return wrap("expire holds", err)
			}
			k, _ := res.RowsAffected()
			if released = k > 0; !released {
				return nil
			}
			_, err = r.exec(ctx, `UPDATE balances SET held_micro = held_micro - ? WHERE subject = ?`, h.amount, h.subject)
			return wrap("expire holds", err)
		})
		if err != nil {
			return n, err
		}
		if released {
			n++
		}
	}
	if err := s.deleteSpent(ctx, "balance_holds", "amount_micro", before.Add(-24*time.Hour)); err != nil {
		return n, wrap("expire holds", err)
	}
	k, err := s.expireSubHolds(ctx, before)
	return n + k, err
}

// deleteSpent deletes the zeroed holds (amount column 0) of table created
// before cutoff: found without a lock, then deleted one by one by primary
// key, so that it takes no lock out of the order settles take them in.
func (s *db) deleteSpent(ctx context.Context, table, amount string, cutoff time.Time) error {
	rows, err := s.r().query(ctx, `SELECT request_id FROM `+table+` WHERE created_at < ? AND `+amount+` = 0`, ms(cutoff))
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.tx(ctx, func(r run) error {
			_, err := r.exec(ctx, `DELETE FROM `+table+` WHERE request_id = ? AND `+amount+` = 0`, id)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *db) SettleRecorded(ctx context.Context, before time.Time) (int64, error) {
	type pending struct {
		id, model string
		charge    int64
	}
	rows, err := s.r().query(ctx, `SELECT h.request_id, r.charge_micro, r.model FROM balance_holds h
JOIN request_log r ON r.id = h.request_id WHERE h.created_at < ?`, ms(before))
	if err != nil {
		return 0, wrap("settle recorded", err)
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.charge, &p.model); err != nil {
			rows.Close()
			return 0, wrap("settle recorded", err)
		}
		todo = append(todo, p)
	}
	rows.Close()
	var n int64
	for _, p := range todo {
		ok, err := s.Capture(ctx, p.id, p.charge, p.model)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	k, err := s.settleRecordedSubs(ctx, before)
	return n + k, err
}

// ---------------------------------------------------------------- redeem codes

const redeemCols = `id, code_hash, last4, batch, amount_micro, max_uses, uses, expires_at, disabled, created_by, created_at`

func scanRedeem(sc interface{ Scan(...any) error }) (*RedeemCode, error) {
	var c RedeemCode
	var exp, at int64
	if err := sc.Scan(&c.ID, &c.CodeHash, &c.Last4, &c.Batch, &c.AmountMicro, &c.MaxUses, &c.Uses, &exp, &c.Disabled, &c.CreatedBy, &at); err != nil {
		return nil, err
	}
	c.ExpiresAt, c.CreatedAt = fromMs(exp), fromMs(at)
	return &c, nil
}

func (s *db) CreateRedeemCodes(ctx context.Context, codes []RedeemCode) error {
	return s.tx(ctx, func(r run) error {
		now := time.Now().UTC().Truncate(time.Millisecond)
		for i := range codes {
			c := &codes[i]
			if c.CodeHash == "" || c.AmountMicro <= 0 {
				return fmt.Errorf("store: redeem code needs a hash and a positive amount")
			}
			if c.ID == "" {
				c.ID = newID()
			}
			if c.MaxUses <= 0 {
				c.MaxUses = 1
			}
			c.CreatedAt = now
			if _, err := r.exec(ctx, `INSERT INTO redeem_codes (`+redeemCols+`) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
				c.ID, c.CodeHash, c.Last4, c.Batch, c.AmountMicro, c.MaxUses, ms(c.ExpiresAt), c.Disabled, c.CreatedBy, ms(now)); err != nil {
				return wrap("create redeem codes", err)
			}
		}
		return nil
	})
}

func (s *db) ListRedeemCodes(ctx context.Context, batch string) ([]RedeemCode, error) {
	return list(ctx, s.r(), "list redeem codes", `SELECT `+redeemCols+` FROM redeem_codes WHERE (? = '' OR batch = ?)
ORDER BY created_at DESC, id`, scanRedeem, batch, batch)
}

func (s *db) SetRedeemCodeDisabled(ctx context.Context, id string, disabled bool) error {
	res, err := s.r().exec(ctx, `UPDATE redeem_codes SET disabled = ? WHERE id = ?`, disabled, id)
	return mustAffect("disable redeem code", res, err)
}

func (s *db) Redeem(ctx context.Context, codeHash, subject, actor string, now time.Time) (*BalanceTxn, error) {
	var out *BalanceTxn
	err := s.tx(ctx, func(r run) error {
		// Locked first: on MySQL, racing duplicate inserts into redeem_uses
		// would otherwise deadlock on their shared next-key locks.
		c, err := scanRedeem(r.row(ctx, `SELECT `+redeemCols+` FROM redeem_codes WHERE code_hash = ?`+r.d.ForUpdate(), codeHash))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRedeemInvalid
		}
		if err != nil {
			return wrap("redeem", err)
		}
		switch {
		case c.Disabled:
			return ErrRedeemInvalid
		case !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt):
			return ErrRedeemExpired
		}
		by := subject
		if actor != "" {
			by = "user:" + actor // the ledger's subject for the user
		}
		res, err := r.exec(ctx, `INSERT INTO redeem_uses (code_id, subject, created_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
			c.ID, by, ms(now))
		if err != nil {
			return wrap("redeem", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrRedeemAlreadyUsed
		}
		res, err = r.exec(ctx, `UPDATE redeem_codes SET uses = uses + 1 WHERE id = ? AND uses < max_uses`, c.ID)
		if err != nil {
			return wrap("redeem", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrRedeemUsedUp
		}
		t := &BalanceTxn{ID: "redeem:" + c.ID + ":" + by, Subject: subject, Kind: TxnRedeem, AmountMicro: c.AmountMicro,
			Ref: c.ID, Note: "code …" + c.Last4, Actor: actor, CreatedAt: now.UTC().Truncate(time.Millisecond)}
		if _, err := credit(ctx, r, t); err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}
