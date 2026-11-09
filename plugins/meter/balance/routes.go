package balance

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

// Routes implements plugin.Admin.
func (m *Meter) Routes() []plugin.AdminRoute {
	return []plugin.AdminRoute{
		{Method: http.MethodGet, Path: "/me", Handler: m.getMe},
		{Method: http.MethodGet, Path: "/me/transactions", Handler: m.getMyTxns},
		{Method: http.MethodGet, Path: "/me/payers", Handler: m.getMyPayers},
		{Method: http.MethodGet, Path: "/accounts", Handler: m.getAccounts, Role: "admin"},
		{Method: http.MethodGet, Path: "/transactions", Handler: m.getTxns, Role: "admin"},
		{Method: http.MethodPost, Path: "/adjust", Handler: m.postAdjust, Role: "admin"},
		{Method: http.MethodPut, Path: "/credit-limit", Handler: m.putCreditLimit, Role: "admin"},
		{Method: http.MethodPost, Path: "/refund", Handler: m.postRefund, Role: "admin"},
	}
}

func (m *Meter) payerSetting() string {
	if m.settings == nil {
		return "user"
	}
	return m.settings.Get("payer").String()
}

type accountView struct {
	Subject     string       `json:"subject"`
	Balance     ledger.Money `json:"balance"`
	Available   ledger.Money `json:"available"`
	Held        ledger.Money `json:"held"`
	CreditLimit ledger.Money `json:"credit_limit"`
	UpdatedAt   string       `json:"updated_at,omitempty"`
}

func view(a ledger.Account, cur string) accountView {
	v := accountView{Subject: a.Subject, Balance: ledger.Money{Micro: a.BalanceMicro, Currency: cur},
		Available: ledger.Money{Micro: a.Available(), Currency: cur}, Held: ledger.Money{Micro: a.HeldMicro, Currency: cur},
		CreditLimit: ledger.Money{Micro: a.CreditLimitMicro, Currency: cur}}
	if !a.UpdatedAt.IsZero() {
		v.UpdatedAt = a.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return v
}

func (m *Meter) me(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid, _ := ledger.Who(r)
	if uid == "" {
		ledger.WriteError(w, http.StatusForbidden, "forbidden", "a console user is required")
		return "", false
	}
	s, err := m.l.PayerOf(r.Context(), uid, r.URL.Query().Get("key_id"))
	if err != nil {
		ledger.Fail(w, err)
		return "", false
	}
	return s, true
}

// getMyPayers lists the accounts the caller's requests pay from, for the
// console's account picker (payer=group or key).
func (m *Meter) getMyPayers(w http.ResponseWriter, r *http.Request) {
	uid, _ := ledger.Who(r)
	if uid == "" {
		ledger.WriteError(w, http.StatusForbidden, "forbidden", "a console user is required")
		return
	}
	ps, err := m.l.Payers(r.Context(), uid)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	if ps == nil {
		ps = []ledger.Payer{}
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"payer": m.payerSetting(), "items": ps})
}

func (m *Meter) getMe(w http.ResponseWriter, r *http.Request) {
	s, ok := m.me(w, r)
	if !ok {
		return
	}
	a, err := m.l.Account(r.Context(), s)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	ledger.WriteJSON(w, http.StatusOK, view(a, m.l.Currency(r.Context())))
}

type txnView struct {
	ledger.Txn
	Amount       ledger.Money `json:"amount"`
	BalanceAfter ledger.Money `json:"balance_after"`
}

func (m *Meter) txns(w http.ResponseWriter, r *http.Request, subject string) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	txns, next, err := m.l.Transactions(r.Context(), ledger.Query{Subject: subject, Kind: ledger.Kind(q.Get("kind")),
		Cursor: q.Get("cursor"), Limit: limit})
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	cur := m.l.Currency(r.Context())
	items := make([]txnView, 0, len(txns))
	for _, t := range txns {
		items = append(items, txnView{Txn: t, Amount: ledger.Money{Micro: t.AmountMicro, Currency: cur},
			BalanceAfter: ledger.Money{Micro: t.BalanceAfterMicro, Currency: cur}})
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (m *Meter) getMyTxns(w http.ResponseWriter, r *http.Request) {
	if s, ok := m.me(w, r); ok {
		m.txns(w, r, s)
	}
}

func (m *Meter) getTxns(w http.ResponseWriter, r *http.Request) {
	m.txns(w, r, r.URL.Query().Get("subject"))
}

func (m *Meter) getAccounts(w http.ResponseWriter, r *http.Request) {
	accts, err := m.l.Accounts(r.Context())
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	cur := m.l.Currency(r.Context())
	items := make([]accountView, 0, len(accts))
	for _, a := range accts {
		items = append(items, view(a, cur))
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "currency": cur, "payer": m.payerSetting()})
}

func (m *Meter) postAdjust(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Subject     string `json:"subject"`
		AmountMicro int64  `json:"amount_micro"`
		Note        string `json:"note"`
		// IdempotencyKey makes a resubmitted form apply once.
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	if !ledger.ValidSubject(in.Subject) || in.AmountMicro == 0 || strings.TrimSpace(in.IdempotencyKey) == "" {
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "subject, a non-zero amount_micro and idempotency_key are required")
		return
	}
	actor, _ := ledger.Who(r)
	applied, a, err := m.l.Credit(r.Context(), ledger.Txn{ID: "adjust:" + in.IdempotencyKey, Subject: in.Subject,
		Kind: ledger.Adjustment, AmountMicro: in.AmountMicro, Note: in.Note, Actor: actor})
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"applied": applied, "account": view(a, m.l.Currency(r.Context()))})
}

func (m *Meter) putCreditLimit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Subject          string `json:"subject"`
		CreditLimitMicro int64  `json:"credit_limit_micro"`
	}
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	if !ledger.ValidSubject(in.Subject) || in.CreditLimitMicro < 0 {
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "subject and a non-negative credit_limit_micro are required")
		return
	}
	if err := m.l.SetCreditLimit(r.Context(), in.Subject, in.CreditLimitMicro); err != nil {
		ledger.Fail(w, err)
		return
	}
	a, err := m.l.Account(r.Context(), in.Subject)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	ledger.WriteJSON(w, http.StatusOK, view(a, m.l.Currency(r.Context())))
}

// postRefund gives back all or part of one request's charge, once.
func (m *Meter) postRefund(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RequestID   string `json:"request_id"`
		AmountMicro int64  `json:"amount_micro"`
		Note        string `json:"note"`
	}
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	charge, err := m.l.Txn(r.Context(), "charge:"+in.RequestID)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	amount := -charge.AmountMicro
	if in.AmountMicro > 0 && in.AmountMicro < amount {
		amount = in.AmountMicro
	}
	actor, _ := ledger.Who(r)
	applied, a, err := m.l.Credit(r.Context(), ledger.Txn{ID: "refund:" + in.RequestID, Subject: charge.Subject,
		Kind: ledger.Refund, AmountMicro: amount, RequestID: in.RequestID, Note: in.Note, Actor: actor})
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"applied": applied, "account": view(a, m.l.Currency(r.Context()))})
}
