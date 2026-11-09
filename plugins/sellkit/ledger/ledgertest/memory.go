// Package ledgertest is an in-memory ledger.Ledger for plugin tests, with
// the same rules as the core's SQL ledger.
package ledgertest

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

// Memory implements ledger.Ledger.
type Memory struct {
	mu    sync.Mutex
	accts map[string]*ledger.Account
	txns  map[string]ledger.Txn
	holds map[string]hold
	codes map[string]*ledger.Code // by hash
	uses  map[string]bool         // code id + "|" + subject
	// Groups maps user ids to group ids and Keys key ids to their owner
	// and group; Payer is meter.balance's payer setting ("" = user).
	Groups map[string]string
	Keys   map[string]Key
	Payer  string
	// records are logged requests' charges by request id, for
	// SettleRecorded; set them with Record.
	records map[string]int64
	// Cur is the site currency; empty = USD.
	Cur string

	ops
}

// Key is a platform key as PayerOf sees it.
type Key struct {
	UserID, GroupID, Name string
}

type hold struct {
	subject string
	amount  int64
	at      time.Time
}

var _ ledger.Ledger = (*Memory)(nil)

// New returns an empty ledger.
func New() *Memory {
	return &Memory{accts: map[string]*ledger.Account{}, txns: map[string]ledger.Txn{}, holds: map[string]hold{},
		codes: map[string]*ledger.Code{}, uses: map[string]bool{}, Groups: map[string]string{}, Keys: map[string]Key{},
		records: map[string]int64{}}
}

func (m *Memory) acct(s string) *ledger.Account {
	a, ok := m.accts[s]
	if !ok {
		a = &ledger.Account{Subject: s}
		m.accts[s] = a
	}
	return a
}

func (m *Memory) Currency(context.Context) string {
	if m.Cur == "" {
		return "USD"
	}
	return m.Cur
}

func (m *Memory) PayerMode(context.Context) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Payer == "" {
		return "user"
	}
	return m.Payer
}

func (m *Memory) PayerOf(_ context.Context, userID, keyID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.payerOf(userID, keyID)
}

func (m *Memory) payerOf(userID, keyID string) (string, error) {
	if keyID != "" {
		k, ok := m.Keys[keyID]
		if !ok || k.UserID != userID {
			return "", ledger.ErrNotFound
		}
		switch m.Payer {
		case "key":
			return ledger.KeySubject(keyID), nil
		case "group":
			if k.GroupID != "" {
				return ledger.GroupSubject(k.GroupID), nil
			}
		}
		return ledger.UserSubject(userID), nil
	}
	switch m.Payer {
	case "key":
		return "", ledger.ErrKeyRequired
	case "group":
		if g := m.Groups[userID]; g != "" {
			return ledger.GroupSubject(g), nil
		}
	}
	return ledger.UserSubject(userID), nil
}

func (m *Memory) Payers(_ context.Context, userID string) ([]ledger.Payer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ledger.Payer
	own, err := m.payerOf(userID, "")
	if err == nil {
		out = append(out, ledger.Payer{Subject: own})
	}
	if m.Payer != "key" && m.Payer != "group" {
		return out, nil
	}
	ids := make([]string, 0, len(m.Keys))
	for id := range m.Keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if m.Keys[id].UserID != userID {
			continue
		}
		if s, _ := m.payerOf(userID, id); s != own {
			out = append(out, ledger.Payer{Subject: s, KeyID: id, KeyName: m.Keys[id].Name})
		}
	}
	return out, nil
}

func (m *Memory) Account(_ context.Context, s string) (ledger.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.acct(s), nil
}

func (m *Memory) Accounts(context.Context) ([]ledger.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ledger.Account
	for _, a := range m.accts {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out, nil
}

func (m *Memory) Hold(_ context.Context, s, req string, amount int64) (ledger.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.acct(s)
	if _, ok := m.holds[req]; ok {
		return *a, nil
	}
	amount = max(amount, 0)
	if a.Available()-amount < -a.CreditLimitMicro || a.Available()+a.CreditLimitMicro <= 0 {
		return *a, ledger.ErrInsufficient
	}
	a.HeldMicro += amount
	m.holds[req] = hold{s, amount, time.Now()}
	return *a, nil
}

func (m *Memory) Capture(_ context.Context, req string, charge int64, note string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.capture(req, charge, note), nil
}

func (m *Memory) capture(req string, charge int64, note string) bool {
	h, ok := m.holds[req]
	if !ok {
		return false
	}
	delete(m.holds, req)
	a := m.acct(h.subject)
	a.HeldMicro -= h.amount
	if id := "charge:" + req; charge > 0 {
		if _, dup := m.txns[id]; !dup {
			a.BalanceMicro -= charge
			m.txns[id] = ledger.Txn{ID: id, Subject: h.subject, Kind: ledger.Charge, AmountMicro: -charge,
				BalanceAfterMicro: a.BalanceMicro, RequestID: req, Note: note, CreatedAt: time.Now()}
		}
	}
	return true
}

func (m *Memory) Charge(_ context.Context, s, req string, charge int64, note string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, held := m.holds[req]
	if held {
		delete(m.holds, req)
		m.acct(h.subject).HeldMicro -= h.amount
	}
	id := "charge:" + req
	if _, dup := m.txns[id]; charge <= 0 || dup {
		return held && charge <= 0, nil
	}
	a := m.acct(s)
	a.BalanceMicro -= charge
	m.txns[id] = ledger.Txn{ID: id, Subject: s, Kind: ledger.Charge, AmountMicro: -charge,
		BalanceAfterMicro: a.BalanceMicro, RequestID: req, Note: note, CreatedAt: time.Now()}
	return true, nil
}

func (m *Memory) Release(_ context.Context, req string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.holds[req]
	if !ok {
		return false, nil
	}
	delete(m.holds, req)
	m.acct(h.subject).HeldMicro -= h.amount
	return true, nil
}

func (m *Memory) Credit(_ context.Context, t ledger.Txn) (bool, ledger.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.acct(t.Subject)
	if _, dup := m.txns[t.ID]; dup {
		return false, *a, nil
	}
	if t.AmountMicro < 0 && a.Available()+t.AmountMicro < -a.CreditLimitMicro {
		return false, *a, ledger.ErrInsufficient
	}
	a.BalanceMicro += t.AmountMicro
	t.BalanceAfterMicro = a.BalanceMicro
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	m.txns[t.ID] = t
	return true, *a, nil
}

func (m *Memory) Txn(_ context.Context, id string) (ledger.Txn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.txns[id]
	if !ok {
		return t, ledger.ErrNotFound
	}
	return t, nil
}

func (m *Memory) SetCreditLimit(_ context.Context, s string, limit int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.acct(s).CreditLimitMicro = limit
	return nil
}

func (m *Memory) Transactions(_ context.Context, q ledger.Query) ([]ledger.Txn, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ledger.Txn
	for _, t := range m.txns {
		if (q.Subject == "" || t.Subject == q.Subject) && (q.Kind == "" || t.Kind == q.Kind) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, "", nil
}

func (m *Memory) ExpireHolds(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, h := range m.holds {
		cut := before
		if strings.HasPrefix(id, "batch:") {
			cut = before.Add(-72 * time.Hour) // as the store: batch holds last longer
		}
		if h.at.Before(cut) && h.amount > 0 {
			m.acct(h.subject).HeldMicro -= h.amount
			h.amount = 0
			m.holds[id] = h
			n++
		}
	}
	return n, nil
}

// Record logs a finished request with its charge, as the request log does.
func (m *Memory) Record(requestID string, charge int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[requestID] = charge
}

func (m *Memory) SettleRecorded(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, h := range m.holds {
		if charge, ok := m.records[id]; ok && h.at.Before(before) && m.capture(id, charge, "") {
			n++
		}
	}
	return n, nil
}

func (m *Memory) CreateCodes(_ context.Context, codes []ledger.Code) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range codes {
		c := c
		if c.MaxUses <= 0 {
			c.MaxUses = 1
		}
		if c.ID == "" {
			c.ID = c.Hash
		}
		c.CreatedAt = time.Now()
		m.codes[c.Hash] = &c
	}
	return nil
}

func (m *Memory) Codes(_ context.Context, batch string) ([]ledger.Code, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ledger.Code
	for _, c := range m.codes {
		if batch == "" || c.Batch == batch {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) SetCodeDisabled(_ context.Context, id string, disabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.codes {
		if c.ID == id {
			c.Disabled = disabled
			return nil
		}
	}
	return ledger.ErrNotFound
}

func (m *Memory) Redeem(ctx context.Context, hash, subject, actor string, now time.Time) (ledger.Txn, error) {
	by := subject
	if actor != "" {
		by = ledger.UserSubject(actor)
	}
	m.mu.Lock()
	c, ok := m.codes[hash]
	switch {
	case !ok || c.Disabled:
		m.mu.Unlock()
		return ledger.Txn{}, ledger.ErrCodeInvalid
	case !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt):
		m.mu.Unlock()
		return ledger.Txn{}, ledger.ErrCodeExpired
	case m.uses[c.ID+"|"+by]:
		m.mu.Unlock()
		return ledger.Txn{}, ledger.ErrCodeAlreadyUsed
	case c.Uses >= c.MaxUses:
		m.mu.Unlock()
		return ledger.Txn{}, ledger.ErrCodeUsedUp
	}
	m.uses[c.ID+"|"+by] = true
	c.Uses++
	t := ledger.Txn{ID: "redeem:" + c.ID + ":" + by, Subject: subject, Kind: ledger.Redeem, AmountMicro: c.AmountMicro,
		Ref: c.ID, Actor: actor, CreatedAt: now}
	m.mu.Unlock()
	_, a, err := m.Credit(ctx, t)
	t.BalanceAfterMicro = a.BalanceMicro
	return t, err
}
