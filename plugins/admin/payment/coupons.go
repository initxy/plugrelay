package payment

import (
	"net/http"
	"strings"
	"time"

	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

// couponInput is a coupon as admins write it. Times are RFC 3339, empty
// for none.
type couponInput struct {
	Code             string `json:"code"`
	Percent          int    `json:"percent"`
	AmountMicro      int64  `json:"amount_micro"`
	MaxDiscountMicro int64  `json:"max_discount_micro"`
	MinTopupMicro    int64  `json:"min_topup_micro"`
	MaxUses          int    `json:"max_uses"`
	PerUser          *int   `json:"per_user"`
	StartsAt         string `json:"starts_at"`
	EndsAt           string `json:"ends_at"`
	Disabled         bool   `json:"disabled"`
	Note             string `json:"note"`
}

// CouponView is a coupon as the console shows it.
type CouponView struct {
	ledger.Coupon
	Amount      ledger.Money `json:"amount"`
	MaxDiscount ledger.Money `json:"max_discount"`
	MinTopup    ledger.Money `json:"min_topup"`
	// State is active, scheduled, ended, used_up or disabled.
	State string `json:"state"`
}

func viewCoupon(c ledger.Coupon, cur string, now time.Time) CouponView {
	state := "active"
	switch {
	case c.Disabled:
		state = "disabled"
	case !c.EndsAt.IsZero() && !now.Before(c.EndsAt):
		state = "ended"
	case !c.StartsAt.IsZero() && now.Before(c.StartsAt):
		state = "scheduled"
	case c.MaxUses > 0 && c.Used >= c.MaxUses:
		state = "used_up"
	}
	return CouponView{Coupon: c, Amount: ledger.Money{Micro: c.AmountMicro, Currency: cur},
		MaxDiscount: ledger.Money{Micro: c.MaxDiscountMicro, Currency: cur}, MinTopup: ledger.Money{Micro: c.MinTopupMicro, Currency: cur},
		State: state}
}

func (in couponInput) coupon(w http.ResponseWriter) (ledger.Coupon, bool) {
	c := ledger.Coupon{Code: in.Code, Percent: in.Percent, AmountMicro: in.AmountMicro, MaxDiscountMicro: in.MaxDiscountMicro,
		MinTopupMicro: in.MinTopupMicro, MaxUses: in.MaxUses, PerUser: 1, Disabled: in.Disabled, Note: strings.TrimSpace(in.Note)}
	if in.PerUser != nil {
		c.PerUser = *in.PerUser
	}
	for _, f := range []struct {
		raw string
		dst *time.Time
	}{{in.StartsAt, &c.StartsAt}, {in.EndsAt, &c.EndsAt}} {
		if f.raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, f.raw)
		if err != nil {
			invalidCoupon(w, "times are RFC 3339")
			return c, false
		}
		*f.dst = t.UTC()
	}
	switch {
	case (c.Percent > 0) == (c.AmountMicro > 0) || c.Percent < 0 || c.Percent > 100 || c.AmountMicro < 0:
		invalidCoupon(w, "a coupon takes either a percent from 1 to 100 or a positive amount off")
	case c.MaxUses < 0 || c.PerUser < 0 || c.MinTopupMicro < 0 || c.MaxDiscountMicro < 0:
		invalidCoupon(w, "limits must not be negative")
	case !c.StartsAt.IsZero() && !c.EndsAt.IsZero() && !c.EndsAt.After(c.StartsAt):
		invalidCoupon(w, "a coupon must end after it starts")
	default:
		return c, true
	}
	return c, false
}

func invalidCoupon(w http.ResponseWriter, msg string) {
	ledger.WriteError(w, http.StatusBadRequest, "coupon_rules", msg)
}

func (p *Plugin) getCoupons(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cs, err := p.l.Coupons(ctx)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	cur, now := p.l.Currency(ctx), p.now()
	items := make([]CouponView, 0, len(cs))
	for _, c := range cs {
		items = append(items, viewCoupon(c, cur, now))
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "currency": cur, "enabled": p.couponsOn()})
}

func (p *Plugin) postCoupon(w http.ResponseWriter, r *http.Request) {
	var in couponInput
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	code := strings.ToUpper(strings.TrimSpace(in.Code))
	if code == "" || len(code) > 64 || strings.ContainsAny(code, " \t") {
		ledger.WriteError(w, http.StatusBadRequest, "coupon_code", "the code must be 1 to 64 characters without spaces")
		return
	}
	in.Code = code
	c, ok := in.coupon(w)
	if !ok {
		return
	}
	c.CreatedBy, _ = ledger.Who(r)
	ctx := r.Context()
	c, err := p.l.CreateCoupon(ctx, c)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	ledger.WriteJSON(w, http.StatusCreated, viewCoupon(c, p.l.Currency(ctx), p.now()))
}

func (p *Plugin) putCoupon(w http.ResponseWriter, r *http.Request) {
	var in couponInput
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	c, ok := in.coupon(w)
	if !ok {
		return
	}
	c.ID = pathID(r)
	ctx := r.Context()
	c, err := p.l.UpdateCoupon(ctx, c)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	ledger.WriteJSON(w, http.StatusOK, viewCoupon(c, p.l.Currency(ctx), p.now()))
}

func (p *Plugin) getCouponUses(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uses, err := p.l.CouponUses(ctx, pathID(r))
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	cur := p.l.Currency(ctx)
	type useView struct {
		ledger.CouponUse
		Discount ledger.Money `json:"discount"`
	}
	items := make([]useView, 0, len(uses))
	for _, u := range uses {
		items = append(items, useView{CouponUse: u, Discount: ledger.Money{Micro: u.DiscountMicro, Currency: cur}})
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// postCouponCheck tells a user what a coupon takes off a top-up, before
// paying; the use is taken at checkout.
func (p *Plugin) postCouponCheck(w http.ResponseWriter, r *http.Request) {
	uid, _ := ledger.Who(r)
	var in struct {
		Code        string `json:"code"`
		AmountMicro int64  `json:"amount_micro"`
	}
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	if !p.couponsOn() {
		ledger.Fail(w, ledger.ErrCouponInvalid)
		return
	}
	ctx := r.Context()
	c, d, err := p.l.CheckCoupon(ctx, in.Code, uid, in.AmountMicro, p.now())
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	cur := p.l.Currency(ctx)
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"code": c.Code, "percent": c.Percent,
		"discount": ledger.Money{Micro: d, Currency: cur}, "pay": ledger.Money{Micro: in.AmountMicro - d, Currency: cur}})
}
