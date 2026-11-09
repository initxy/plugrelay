package admin

import (
	"context"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// balanceMeter is the sell kit's prepaid balance plugin.
const balanceMeter = "meter.balance"

// overviewBalance is the caller's available balance for the overview, when
// meter.balance runs: from the account the caller's requests pay from,
// with days left at the window's spend rate when the window is the
// caller's own spend.
func (h *Handler) overviewBalance(c *call, spend int64, window time.Duration, own bool) map[string]any {
	e, ok := h.plugin(balanceMeter)
	l, lok := store.LedgerOf(h.d.Store)
	if !ok || !e.Enabled || !lok || c.who == nil || c.who.user == nil {
		return nil
	}
	subject := h.payerSubject(c.who.user)
	if subject == "" {
		return nil
	}
	a, err := l.Account(c.ctx, subject)
	if err != nil {
		return nil
	}
	out := map[string]any{"amount": money{Micro: a.Available(), Currency: h.currency(c.ctx)}}
	if days := window.Hours() / 24; own && spend > 0 && days > 0 {
		out["days_left"] = float64(max(a.Available(), 0)) / (float64(spend) / days)
	}
	return out
}

// payerSubject is the ledger account u's requests pay from; empty when
// each key pays from its own (payer=key), as the user has no one account.
func (h *Handler) payerSubject(u *store.User) string {
	switch h.d.Runtime.SettingsFor(balanceMeter, plugin.Caller{UserID: u.ID, GroupID: u.GroupID}).Get("payer").String() {
	case "key":
		return ""
	case "group":
		if u.GroupID != "" {
			return "group:" + u.GroupID
		}
	}
	return "user:" + u.ID
}

// userBalances is every ledger account's available balance, read once per
// user list.
type userBalances struct {
	h     *Handler
	avail map[string]int64
	cur   string
}

// loadUserBalances is nil when meter.balance does not run.
func (h *Handler) loadUserBalances(ctx context.Context, cur string) (*userBalances, error) {
	e, ok := h.plugin(balanceMeter)
	l, lok := store.LedgerOf(h.d.Store)
	if !ok || !e.Enabled || !lok {
		return nil, nil
	}
	accts, err := l.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	b := &userBalances{h: h, avail: make(map[string]int64, len(accts)), cur: cur}
	for _, a := range accts {
		b.avail[a.Subject] = a.Available()
	}
	return b, nil
}

// of is the available balance u pays from; a subject never credited is zero.
func (b *userBalances) of(u *store.User) *money {
	if b == nil {
		return nil
	}
	s := b.h.payerSubject(u)
	if s == "" {
		return nil
	}
	return &money{Micro: b.avail[s], Currency: b.cur}
}
