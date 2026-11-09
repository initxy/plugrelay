// Package ledger is the contract between the sell-kit plugins and the
// core's prepaid balance ledger. The core implements Ledger over its store
// (balances, transactions, holds and redeem codes, one database transaction
// per change) and offers it through a Host that implements Host.
//
// Money is int64 micro-units of the site currency. A subject is
// "user:<id>", "group:<id>" or "key:<id>".
package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

var (
	ErrInsufficient    = errors.New("insufficient balance")
	ErrCodeInvalid     = errors.New("redeem code is not valid")
	ErrCodeExpired     = errors.New("redeem code has expired")
	ErrCodeUsedUp      = errors.New("redeem code has been used up")
	ErrCodeAlreadyUsed = errors.New("redeem code was already used by this account")
	ErrNotFound        = errors.New("not found")
	// ErrKeyRequired: keys pay (payer=key), so a console user must say
	// which of their keys a top-up, redeem or balance view is for.
	ErrKeyRequired = errors.New("choose one of your keys: each key has its own balance")
)

// Kind is a transaction kind.
type Kind string

const (
	Topup      Kind = "topup"
	Charge     Kind = "charge"
	Refund     Kind = "refund"
	Redeem     Kind = "redeem"
	Adjustment Kind = "adjustment"
)

// Account is a subject's balance.
type Account struct {
	Subject          string    `json:"subject"`
	BalanceMicro     int64     `json:"balance_micro"`
	HeldMicro        int64     `json:"held_micro"`
	CreditLimitMicro int64     `json:"credit_limit_micro"`
	UpdatedAt        time.Time `json:"updated_at,omitzero"`
}

// Available is the balance not held by requests in flight.
func (a Account) Available() int64 { return a.BalanceMicro - a.HeldMicro }

// Txn is one balance change. ID is its idempotency key.
type Txn struct {
	ID                string    `json:"id"`
	Subject           string    `json:"subject"`
	Kind              Kind      `json:"kind"`
	AmountMicro       int64     `json:"amount_micro"`
	BalanceAfterMicro int64     `json:"balance_after_micro"`
	RequestID         string    `json:"request_id,omitempty"`
	Ref               string    `json:"ref,omitempty"`
	Note              string    `json:"note,omitempty"`
	Actor             string    `json:"actor,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

// Payer is an account a console user can view and top up.
type Payer struct {
	Subject string `json:"subject"`
	KeyID   string `json:"key_id,omitempty"`
	KeyName string `json:"key_name,omitempty"`
}

// Query pages transactions newest first.
type Query struct {
	Subject string
	Kind    Kind
	Cursor  string
	Limit   int
}

// Code is a stored redeem code (the code itself is never stored).
type Code struct {
	ID          string    `json:"id"`
	Hash        string    `json:"-"`
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

// Ledger is the core's balance store, with the sell kit's operations
// (orders, coupons, referrals, check-ins) next to it. Every call is one
// database transaction; Hold, Capture, Release, Credit and Redeem are safe
// to retry.
type Ledger interface {
	Operations

	// Currency is the site currency all amounts are in.
	Currency(ctx context.Context) string
	// PayerOf is the subject requests by a console user's key pay from,
	// by meter.balance's payer setting, exactly as the meter picks it:
	// payer=user "user:<id>"; payer=group the key's group, else the user;
	// payer=key "key:<keyID>". keyID must be one of the user's keys
	// (ErrNotFound otherwise); empty means the user's own account (the
	// user's group when groups pay) and is ErrKeyRequired when keys pay.
	PayerOf(ctx context.Context, userID, keyID string) (string, error)
	// PayerMode is meter.balance's payer setting: "user", "group" or "key".
	PayerMode(ctx context.Context) string
	// Payers lists the accounts a console user's requests pay from: the
	// user's own account (KeyID empty; not when keys pay) and, when groups
	// or keys pay, each key whose account differs from it.
	Payers(ctx context.Context, userID string) ([]Payer, error)
	Account(ctx context.Context, subject string) (Account, error)
	Accounts(ctx context.Context) ([]Account, error)
	// Hold reserves amount for a request, or fails with ErrInsufficient
	// (returning the account as it stands).
	Hold(ctx context.Context, subject, requestID string, amount int64) (Account, error)
	// Capture charges a held request once; false if already settled.
	Capture(ctx context.Context, requestID string, charge int64, note string) (bool, error)
	// Charge ends a request's hold, if any is left, and charges subject
	// once per request id, in full: also without a hold and below zero
	// (work billed long after it was held, such as a batch). Zero only
	// releases. false when nothing changed.
	Charge(ctx context.Context, subject, requestID string, charge int64, note string) (bool, error)
	// Release drops a hold uncharged; false if already settled.
	Release(ctx context.Context, requestID string) (bool, error)
	// Credit applies t once per t.ID; negative amounts may fail with
	// ErrInsufficient.
	Credit(ctx context.Context, t Txn) (applied bool, acct Account, err error)
	Txn(ctx context.Context, id string) (Txn, error)
	SetCreditLimit(ctx context.Context, subject string, limitMicro int64) error
	Transactions(ctx context.Context, q Query) ([]Txn, string, error)
	ExpireHolds(ctx context.Context, before time.Time) (int64, error)
	// SettleRecorded settles holds made before `before` whose request is
	// already in the request log with the logged charge (zero releases):
	// a settle that failed after the request finished. Never charges a
	// request twice.
	SettleRecorded(ctx context.Context, before time.Time) (int64, error)

	CreateCodes(ctx context.Context, codes []Code) error
	Codes(ctx context.Context, batch string) ([]Code, error)
	SetCodeDisabled(ctx context.Context, id string, disabled bool) error
	// Redeem credits a code's amount to subject (ErrCode* on failure). A
	// code is used once per actor (the redeeming user), whichever subject
	// pays, so new keys of one account do not redeem it again.
	Redeem(ctx context.Context, codeHash, subject, actor string, now time.Time) (Txn, error)
}

// Host is implemented by a plugin.Host that offers the ledger.
type Host interface {
	Ledger() Ledger
}

// From returns the host's ledger.
func From(h plugin.Host) (Ledger, error) {
	if lh, ok := h.(Host); ok {
		if l := lh.Ledger(); l != nil {
			return l, nil
		}
	}
	return nil, errors.New("this relay offers no balance ledger")
}

// UserSubject and friends build subjects.
func UserSubject(id string) string  { return "user:" + id }
func GroupSubject(id string) string { return "group:" + id }
func KeySubject(id string) string   { return "key:" + id }
