package app

import (
	"context"
	"errors"
	"time"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/admin/payment"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

var (
	_ ledger.SubscriptionsHost = (*host)(nil)
	_ payment.SettingsHost     = (*host)(nil)
	_ payment.PublicURLHost    = (*host)(nil)
	_ ledger.Subscriptions     = storeSubs{}
)

func (h *host) Subscriptions() ledger.Subscriptions {
	ss, ok := store.SubscriptionsOf(h.a.store)
	if !ok {
		return nil
	}
	return storeSubs{ss}
}

// PaymentSettings are admin.payment's settings (provider keys), which
// admin.subscriptions pays through.
func (h *host) PaymentSettings() plugin.Settings { return globalSettings{a: h.a, id: payment.ID} }

// PublicURL is the relay's configured public URL, for payment return and
// callback URLs.
func (h *host) PublicURL(ctx context.Context) string { return admin.PublicURL(ctx, h.a.store) }

// storeSubs adapts store.Subscriptions to the plugins' ledger.Subscriptions.
type storeSubs struct{ s store.Subscriptions }

func subErr(err error) error {
	switch {
	case errors.Is(err, store.ErrConflict):
		return ledger.ErrSubscribed
	case errors.Is(err, store.ErrAllowanceExhausted):
		return ledger.ErrAllowanceExhausted
	}
	return ledgerErr(err)
}

func toSub(s *store.Subscription) ledger.Sub {
	if s == nil {
		return ledger.Sub{}
	}
	return ledger.Sub{ID: s.ID, UserID: s.UserID, Payer: s.Payer, PlanID: s.PlanID, PlanName: s.PlanName, Status: s.Status,
		Period: s.Period, PriceMicro: s.PriceMicro, Unit: s.Unit, Allowance: s.Allowance, Used: s.Used, Held: s.Held,
		Overage: s.Overage, GroupID: s.GroupID, PeriodStart: s.PeriodStart, PeriodEnd: s.PeriodEnd,
		CancelAtPeriodEnd: s.CancelAtPeriodEnd, Provider: s.Provider, Renewal: s.Renewal, ProviderRef: s.ProviderRef,
		RefundDue: s.RefundDue, CreatedAt: s.CreatedAt}
}

func (x storeSubs) CreateSubscription(ctx context.Context, s ledger.Sub) (ledger.Sub, error) {
	row := &store.Subscription{ID: s.ID, UserID: s.UserID, Payer: s.Payer, PlanID: s.PlanID, PlanName: s.PlanName,
		Period: s.Period, PriceMicro: s.PriceMicro, Unit: s.Unit, Allowance: s.Allowance, Overage: s.Overage,
		GroupID: s.GroupID, CancelAtPeriodEnd: s.CancelAtPeriodEnd, Provider: s.Provider, Renewal: s.Renewal,
		ProviderRef: s.ProviderRef}
	if err := x.s.CreateSubscription(ctx, row); err != nil {
		return ledger.Sub{}, subErr(err)
	}
	return toSub(row), nil
}

func (x storeSubs) Subscription(ctx context.Context, id string) (ledger.Sub, error) {
	s, err := x.s.GetSubscription(ctx, id)
	return toSub(s), subErr(err)
}

func (x storeSubs) SubscriptionByRef(ctx context.Context, provider, ref string) (ledger.Sub, error) {
	s, err := x.s.SubscriptionByRef(ctx, provider, ref)
	return toSub(s), subErr(err)
}

func (x storeSubs) ListSubscriptions(ctx context.Context, userID, status string) ([]ledger.Sub, error) {
	rows, err := x.s.ListSubscriptions(ctx, store.SubscriptionFilter{UserID: userID, Status: status})
	out := make([]ledger.Sub, 0, len(rows))
	for i := range rows {
		out = append(out, toSub(&rows[i]))
	}
	return out, subErr(err)
}

func (x storeSubs) StartPeriod(ctx context.Context, id string, start, end time.Time, t ledger.Txn) (bool, error) {
	ok, err := x.s.StartPeriod(ctx, id, start, end, &store.BalanceTxn{ID: t.ID, Subject: t.Subject, Kind: store.TxnKind(t.Kind),
		AmountMicro: t.AmountMicro, Ref: t.Ref, Note: t.Note, Actor: t.Actor})
	return ok, subErr(err)
}

func (x storeSubs) SetSubscriptionRef(ctx context.Context, id, ref string) error {
	return subErr(x.s.SetSubscriptionRef(ctx, id, ref))
}

func (x storeSubs) SetRefundDue(ctx context.Context, id string, due bool) error {
	return subErr(x.s.SetRefundDue(ctx, id, due))
}

func (x storeSubs) SetCancelAtPeriodEnd(ctx context.Context, id string, cancel bool) error {
	return subErr(x.s.SetCancelAtPeriodEnd(ctx, id, cancel))
}

func (x storeSubs) EndSubscription(ctx context.Context, id string) (bool, error) {
	ok, err := x.s.EndSubscription(ctx, id)
	return ok, subErr(err)
}

func (x storeSubs) RenewDue(ctx context.Context, now time.Time) (int64, error) {
	n, err := x.s.RenewDue(ctx, now)
	return n, subErr(err)
}

func (x storeSubs) SubscriptionHold(ctx context.Context, userID, payer, requestID string, est ledger.SubEstimate, now time.Time) (ledger.SubHold, error) {
	h, err := x.s.SubscriptionHold(ctx, userID, payer, requestID, store.SubEstimate{Money: est.Money, Tokens: est.Tokens, UnitPriced: est.UnitPriced}, now)
	if h == nil {
		return ledger.SubHold{}, subErr(err)
	}
	return ledger.SubHold{Sub: toSub(&h.Subscription), Covered: h.Covered}, subErr(err)
}

func (x storeSubs) SubscriptionSettle(ctx context.Context, requestID string, charge, unitCharge, tokens int64, note string) (bool, error) {
	ok, err := x.s.SubscriptionSettle(ctx, requestID, charge, unitCharge, tokens, note)
	return ok, subErr(err)
}

func (x storeSubs) SubscriptionRelease(ctx context.Context, requestID string) (bool, error) {
	ok, err := x.s.SubscriptionRelease(ctx, requestID)
	return ok, subErr(err)
}
