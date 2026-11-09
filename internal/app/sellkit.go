package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/admin"
	newapi "github.com/initxy/plugrelay/internal/importnewapi"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/plugins/admin/importnewapi"
	"github.com/initxy/plugrelay/plugins/admin/signup"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

// The sell kit's Host interfaces: the balance ledger, account creation for
// sign-up and the new-api importer.
var (
	_ ledger.Host               = (*host)(nil)
	_ signup.AccountsHost       = (*host)(nil)
	_ importnewapi.ImporterHost = (*host)(nil)
)

func (h *host) Ledger() ledger.Ledger {
	l, ok := store.LedgerOf(h.a.store)
	if !ok {
		return nil
	}
	return storeLedger{a: h.a, l: l}
}

func (h *host) SignupAccounts() signup.Accounts { return signupAccounts{h.a.store} }

func (h *host) NewAPIImporter() importnewapi.Importer {
	return newapi.Importer{Store: h.a.store, Opts: func() newapi.Options {
		ms := runtimeAPI{h.a}.Manifests(context.Background())
		return newapi.Options{Currency: admin.Currency(context.Background(), h.a.store), Apply: h.a.Apply,
			Manifests: func(name string) (string, bool, bool) {
				for _, m := range ms {
					if m.Name == name {
						return m.Provider, m.Kind == "official", true
					}
				}
				return "", false, false
			}}
	}}
}

// storeLedger adapts store.Ledger to the plugins' ledger.Ledger.
type storeLedger struct {
	a *App
	l store.Ledger
}

var _ ledger.Ledger = storeLedger{}

func ledgerErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrInsufficientBalance):
		return ledger.ErrInsufficient
	case errors.Is(err, store.ErrNotFound):
		return ledger.ErrNotFound
	case errors.Is(err, store.ErrRedeemInvalid):
		return ledger.ErrCodeInvalid
	case errors.Is(err, store.ErrRedeemExpired):
		return ledger.ErrCodeExpired
	case errors.Is(err, store.ErrRedeemUsedUp):
		return ledger.ErrCodeUsedUp
	case errors.Is(err, store.ErrRedeemAlreadyUsed):
		return ledger.ErrCodeAlreadyUsed
	}
	return err
}

func toAccount(a *store.BalanceAccount) ledger.Account {
	if a == nil {
		return ledger.Account{}
	}
	return ledger.Account{Subject: a.Subject, BalanceMicro: a.BalanceMicro, HeldMicro: a.HeldMicro,
		CreditLimitMicro: a.CreditLimitMicro, UpdatedAt: a.UpdatedAt}
}

func toTxn(t *store.BalanceTxn) ledger.Txn {
	if t == nil {
		return ledger.Txn{}
	}
	return ledger.Txn{ID: t.ID, Subject: t.Subject, Kind: ledger.Kind(t.Kind), AmountMicro: t.AmountMicro,
		BalanceAfterMicro: t.BalanceAfterMicro, RequestID: t.RequestID, Ref: t.Ref, Note: t.Note, Actor: t.Actor, CreatedAt: t.CreatedAt}
}

func (s storeLedger) Currency(ctx context.Context) string { return admin.Currency(ctx, s.a.store) }

func (s storeLedger) payerMode() string {
	if m := s.a.settings().Get(nil, nil, "meter.balance", "payer").String(); m != "" {
		return m
	}
	return "user"
}

func (s storeLedger) PayerMode(context.Context) string { return s.payerMode() }

// PayerOf mirrors meter.balance's choice for a request by keyID (the
// Caller carries the key's group, not the user's).
func (s storeLedger) PayerOf(ctx context.Context, userID, keyID string) (string, error) {
	mode := s.payerMode()
	if keyID != "" {
		k, err := s.a.store.GetKey(ctx, keyID)
		if err != nil {
			return "", ledgerErr(err)
		}
		if k.UserID != userID {
			return "", ledger.ErrNotFound
		}
		return keyPayer(mode, userID, k), nil
	}
	switch mode {
	case "key":
		return "", ledger.ErrKeyRequired
	case "group":
		u, err := s.a.store.GetUser(ctx, userID)
		if err != nil {
			return "", ledgerErr(err)
		}
		if u.GroupID != "" {
			return ledger.GroupSubject(u.GroupID), nil
		}
	}
	return ledger.UserSubject(userID), nil
}

func keyPayer(mode, userID string, k *store.Key) string {
	switch mode {
	case "key":
		return ledger.KeySubject(k.ID)
	case "group":
		if k.GroupID != "" {
			return ledger.GroupSubject(k.GroupID)
		}
	}
	return ledger.UserSubject(userID)
}

func (s storeLedger) Payers(ctx context.Context, userID string) ([]ledger.Payer, error) {
	mode := s.payerMode()
	var out []ledger.Payer
	own, err := s.PayerOf(ctx, userID, "")
	if err != nil && !errors.Is(err, ledger.ErrKeyRequired) {
		return nil, err
	}
	if own != "" {
		out = append(out, ledger.Payer{Subject: own})
	}
	if mode != "key" && mode != "group" {
		return out, nil
	}
	keys, err := s.a.store.ListKeys(ctx, store.KeyFilter{UserID: userID})
	if err != nil {
		return nil, ledgerErr(err)
	}
	for i := range keys {
		if sub := keyPayer(mode, userID, &keys[i]); sub != own {
			out = append(out, ledger.Payer{Subject: sub, KeyID: keys[i].ID, KeyName: keys[i].Name})
		}
	}
	return out, nil
}

func (s storeLedger) Account(ctx context.Context, subject string) (ledger.Account, error) {
	a, err := s.l.Account(ctx, subject)
	return toAccount(a), ledgerErr(err)
}

func (s storeLedger) Accounts(ctx context.Context) ([]ledger.Account, error) {
	as, err := s.l.ListAccounts(ctx)
	out := make([]ledger.Account, 0, len(as))
	for i := range as {
		out = append(out, toAccount(&as[i]))
	}
	return out, ledgerErr(err)
}

func (s storeLedger) Hold(ctx context.Context, subject, requestID string, amount int64) (ledger.Account, error) {
	a, err := s.l.Hold(ctx, subject, requestID, amount)
	return toAccount(a), ledgerErr(err)
}

func (s storeLedger) Capture(ctx context.Context, requestID string, charge int64, note string) (bool, error) {
	ok, err := s.l.Capture(ctx, requestID, charge, note)
	return ok, ledgerErr(err)
}

func (s storeLedger) Charge(ctx context.Context, subject, requestID string, charge int64, note string) (bool, error) {
	ok, err := s.l.Charge(ctx, subject, requestID, charge, note)
	return ok, ledgerErr(err)
}

func (s storeLedger) Release(ctx context.Context, requestID string) (bool, error) {
	ok, err := s.l.Release(ctx, requestID)
	return ok, ledgerErr(err)
}

func (s storeLedger) Credit(ctx context.Context, t ledger.Txn) (bool, ledger.Account, error) {
	st := &store.BalanceTxn{ID: t.ID, Subject: t.Subject, Kind: store.TxnKind(t.Kind), AmountMicro: t.AmountMicro,
		RequestID: t.RequestID, Ref: t.Ref, Note: t.Note, Actor: t.Actor, CreatedAt: t.CreatedAt}
	ok, a, err := s.l.Credit(ctx, st)
	return ok, toAccount(a), ledgerErr(err)
}

func (s storeLedger) Txn(ctx context.Context, id string) (ledger.Txn, error) {
	t, err := s.l.GetTxn(ctx, id)
	return toTxn(t), ledgerErr(err)
}

func (s storeLedger) SetCreditLimit(ctx context.Context, subject string, limit int64) error {
	return ledgerErr(s.l.SetCreditLimit(ctx, subject, limit))
}

func (s storeLedger) Transactions(ctx context.Context, q ledger.Query) ([]ledger.Txn, string, error) {
	ts, next, err := s.l.Transactions(ctx, store.TxnQuery{Subject: q.Subject, Kind: store.TxnKind(q.Kind), Cursor: q.Cursor, Limit: q.Limit})
	out := make([]ledger.Txn, 0, len(ts))
	for i := range ts {
		out = append(out, toTxn(&ts[i]))
	}
	return out, next, ledgerErr(err)
}

func (s storeLedger) ExpireHolds(ctx context.Context, before time.Time) (int64, error) {
	n, err := s.l.ExpireHolds(ctx, before)
	return n, ledgerErr(err)
}

func (s storeLedger) SettleRecorded(ctx context.Context, before time.Time) (int64, error) {
	n, err := s.l.SettleRecorded(ctx, before)
	return n, ledgerErr(err)
}

func (s storeLedger) CreateCodes(ctx context.Context, codes []ledger.Code) error {
	rows := make([]store.RedeemCode, len(codes))
	for i, c := range codes {
		rows[i] = store.RedeemCode{CodeHash: c.Hash, Last4: c.Last4, Batch: c.Batch, AmountMicro: c.AmountMicro,
			MaxUses: c.MaxUses, ExpiresAt: c.ExpiresAt, Disabled: c.Disabled, CreatedBy: c.CreatedBy}
	}
	return ledgerErr(s.l.CreateRedeemCodes(ctx, rows))
}

func (s storeLedger) Codes(ctx context.Context, batch string) ([]ledger.Code, error) {
	rows, err := s.l.ListRedeemCodes(ctx, batch)
	out := make([]ledger.Code, 0, len(rows))
	for _, c := range rows {
		out = append(out, ledger.Code{ID: c.ID, Last4: c.Last4, Batch: c.Batch, AmountMicro: c.AmountMicro, MaxUses: c.MaxUses,
			Uses: c.Uses, ExpiresAt: c.ExpiresAt, Disabled: c.Disabled, CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt})
	}
	return out, ledgerErr(err)
}

func (s storeLedger) SetCodeDisabled(ctx context.Context, id string, disabled bool) error {
	return ledgerErr(s.l.SetRedeemCodeDisabled(ctx, id, disabled))
}

func (s storeLedger) Redeem(ctx context.Context, hash, subject, actor string, now time.Time) (ledger.Txn, error) {
	t, err := s.l.Redeem(ctx, hash, subject, actor, now)
	return toTxn(t), ledgerErr(err)
}

// signupAccounts creates end-user accounts for admin.signup.
type signupAccounts struct{ st store.Store }

func (s signupAccounts) CreateUser(ctx context.Context, email, name, password, groupID string) (string, error) {
	users, err := s.st.ListUsers(ctx)
	if err != nil {
		return "", err
	}
	for _, u := range users {
		if strings.EqualFold(u.Email, email) {
			return "", signup.ErrExists
		}
	}
	// Nobody checked the address: SSO link_by_email must not trust it.
	u := &store.User{Name: name, Email: email, Role: store.RoleUser, GroupID: groupID, EmailUnverified: true}
	if err := s.st.CreateUser(ctx, u, password); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return "", signup.ErrExists
		}
		return "", err
	}
	return u.ID, nil
}
