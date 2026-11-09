package ledgertest

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

// ops is the in-memory side of ledger.Operations.
type ops struct {
	orders    map[string]ledger.Order
	coupons   map[string]*ledger.Coupon // by id
	cuses     map[string]ledger.CouponUse
	refCodes  map[string]string // user id -> code
	referrals map[string]*ledger.ReferralRow
	// Referral is the reward RewardReferral pays: a fixed amount when
	// set, else ReferralPercent of the top-up, within ReferralCap.
	Referral, ReferralCap int64
	ReferralPercent       float64
}

func (m *Memory) opsInit() {
	if m.orders == nil {
		m.orders, m.coupons, m.cuses = map[string]ledger.Order{}, map[string]*ledger.Coupon{}, map[string]ledger.CouponUse{}
		m.refCodes, m.referrals = map[string]string{}, map[string]*ledger.ReferralRow{}
	}
}

func (m *Memory) Timezone(context.Context) *time.Location { return time.UTC }

func (m *Memory) RedeemUses(_ context.Context, codeID string) ([]ledger.RedeemUse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ledger.RedeemUse
	for k := range m.uses {
		if id, subject, _ := strings.Cut(k, "|"); id == codeID {
			out = append(out, ledger.RedeemUse{Subject: subject})
		}
	}
	return out, nil
}

func (m *Memory) SetBatchDisabled(_ context.Context, batch string, disabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.codes {
		if c.Batch == batch {
			c.Disabled = disabled
			n++
		}
	}
	if n == 0 {
		return ledger.ErrNotFound
	}
	return nil
}

func (m *Memory) CreateOrder(_ context.Context, o ledger.Order) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	if _, dup := m.orders[o.ID]; dup {
		return ledger.ErrConflict
	}
	m.orders[o.ID] = o
	return nil
}

func (m *Memory) Order(_ context.Context, id string) (ledger.Order, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	o, ok := m.orders[id]
	if !ok {
		return o, ledger.ErrNotFound
	}
	return o, nil
}

func (m *Memory) PayOrder(_ context.Context, id string, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	o, ok := m.orders[id]
	if !ok || o.Status == ledger.OrderPaid {
		return false, nil
	}
	o.Status, o.PaidAt = ledger.OrderPaid, at
	m.orders[id] = o
	if o.CouponID != "" {
		m.cuses[id] = ledger.CouponUse{OrderID: id, CouponID: o.CouponID, UserID: o.UserID, DiscountMicro: o.DiscountMicro,
			Status: ledger.OrderPaid, CreatedAt: o.CreatedAt}
	}
	return true, nil
}

func (m *Memory) CancelOrder(_ context.Context, id string, release bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	o, ok := m.orders[id]
	if !ok || o.Status != ledger.OrderPending {
		return false, nil
	}
	o.Status = ledger.OrderCanceled
	m.orders[id] = o
	if u, ok := m.cuses[id]; ok && u.Status == ledger.OrderPending {
		u.Status = ledger.OrderCanceled
		m.cuses[id] = u
		if release {
			delete(m.cuses, id)
		}
	}
	return true, nil
}

func (m *Memory) Orders(_ context.Context, q ledger.OrderQuery) ([]ledger.Order, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	var out []ledger.Order
	for _, o := range m.orders {
		if (q.UserID == "" || o.UserID == q.UserID) && (q.Status == "" || o.Status == q.Status) &&
			(q.Provider == "" || o.Provider == q.Provider) && (q.Kind == "" || o.Kind == q.Kind) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, "", nil
}

func (m *Memory) OrderTotals(_ context.Context, since time.Time) (ledger.OrderTotals, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	var t ledger.OrderTotals
	for _, o := range m.orders {
		switch {
		case o.Status == ledger.OrderPaid && !o.PaidAt.Before(since):
			t.PaidMicro += o.PayMicro
			t.PaidCount++
		case o.Status == ledger.OrderPending && !o.CreatedAt.Before(since):
			t.PendingCount++
		}
	}
	return t, nil
}

func (m *Memory) CreateCoupon(_ context.Context, c ledger.Coupon) (ledger.Coupon, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	c.Code = strings.ToUpper(strings.TrimSpace(c.Code))
	for _, o := range m.coupons {
		if o.Code == c.Code {
			return ledger.Coupon{}, ledger.ErrConflict
		}
	}
	if c.ID == "" {
		c.ID = "cp" + c.Code
	}
	c.CreatedAt, c.UpdatedAt = time.Now(), time.Now()
	m.coupons[c.ID] = &c
	return c, nil
}

func (m *Memory) UpdateCoupon(_ context.Context, c ledger.Coupon) (ledger.Coupon, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	old, ok := m.coupons[c.ID]
	if !ok {
		return ledger.Coupon{}, ledger.ErrNotFound
	}
	c.Code, c.CreatedAt, c.UpdatedAt = old.Code, old.CreatedAt, time.Now()
	m.coupons[c.ID] = &c
	return m.counted(c, time.Now()), nil
}

// counted fills in c's use counts.
func (m *Memory) counted(c ledger.Coupon, now time.Time) ledger.Coupon {
	c.Used, c.Pending = 0, 0
	for _, u := range m.cuses {
		switch {
		case u.CouponID != c.ID:
		case u.Status == ledger.OrderPaid:
			c.Used++
		case u.CreatedAt.After(now.Add(-ledger.CouponHold)):
			c.Pending++
		}
	}
	return c
}

func (m *Memory) Coupon(_ context.Context, id string) (ledger.Coupon, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	c, ok := m.coupons[id]
	if !ok {
		return ledger.Coupon{}, ledger.ErrNotFound
	}
	return m.counted(*c, time.Now()), nil
}

func (m *Memory) Coupons(context.Context) ([]ledger.Coupon, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	out := []ledger.Coupon{}
	for _, c := range m.coupons {
		out = append(out, m.counted(*c, time.Now()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out, nil
}

func (m *Memory) CouponUses(_ context.Context, couponID string) ([]ledger.CouponUse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	var out []ledger.CouponUse
	for _, u := range m.cuses {
		if u.CouponID == couponID {
			out = append(out, u)
		}
	}
	return out, nil
}

func (m *Memory) check(code, userID string, amount int64, now time.Time) (ledger.Coupon, int64, error) {
	m.opsInit()
	var c *ledger.Coupon
	for _, x := range m.coupons {
		if x.Code == strings.ToUpper(strings.TrimSpace(code)) {
			c = x
		}
	}
	switch {
	case c == nil || c.Disabled:
		return ledger.Coupon{}, 0, ledger.ErrCouponInvalid
	case !c.StartsAt.IsZero() && now.Before(c.StartsAt):
		return ledger.Coupon{}, 0, ledger.ErrCouponNotStarted
	case !c.EndsAt.IsZero() && !now.Before(c.EndsAt):
		return ledger.Coupon{}, 0, ledger.ErrCouponExpired
	case amount < c.MinTopupMicro:
		return ledger.Coupon{}, 0, ledger.ErrCouponMinAmount
	}
	d := c.AmountMicro
	if c.Percent > 0 {
		d = amount * int64(c.Percent) / 100
		if c.MaxDiscountMicro > 0 {
			d = min(d, c.MaxDiscountMicro)
		}
	}
	d -= d % 10_000
	if d <= 0 || d >= amount {
		return ledger.Coupon{}, 0, ledger.ErrCouponMinAmount
	}
	cc := m.counted(*c, now)
	if cc.MaxUses > 0 && cc.Used+cc.Pending >= cc.MaxUses {
		return ledger.Coupon{}, 0, ledger.ErrCouponUsedUp
	}
	mine := 0
	for _, u := range m.cuses {
		if u.CouponID == c.ID && u.UserID == userID && (u.Status == ledger.OrderPaid || u.CreatedAt.After(now.Add(-ledger.CouponHold))) {
			mine++
		}
	}
	if cc.PerUser > 0 && mine >= cc.PerUser {
		return ledger.Coupon{}, 0, ledger.ErrCouponAlreadyUsed
	}
	return cc, d, nil
}

func (m *Memory) CheckCoupon(_ context.Context, code, userID string, amount int64, now time.Time) (ledger.Coupon, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.check(code, userID, amount, now)
}

func (m *Memory) ReserveCoupon(_ context.Context, code, userID, orderID string, amount int64, now time.Time) (ledger.Coupon, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, d, err := m.check(code, userID, amount, now)
	if err != nil {
		return c, 0, err
	}
	m.cuses[orderID] = ledger.CouponUse{OrderID: orderID, CouponID: c.ID, UserID: userID, DiscountMicro: d,
		Status: ledger.OrderPending, CreatedAt: now}
	return c, d, nil
}

func (m *Memory) ReleaseCoupon(_ context.Context, orderID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	if u, ok := m.cuses[orderID]; ok && u.Status == ledger.OrderPending {
		delete(m.cuses, orderID)
	}
	return nil
}

func (m *Memory) ReferralCode(_ context.Context, userID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	if c, ok := m.refCodes[userID]; ok {
		return c, nil
	}
	c := "REF" + strings.ToUpper(userID)
	m.refCodes[userID] = c
	return c, nil
}

func (m *Memory) Refer(_ context.Context, code, inviteeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	for uid, c := range m.refCodes {
		if c != strings.ToUpper(code) {
			continue
		}
		if uid == inviteeID {
			return ledger.ErrSelfReferral
		}
		if _, ok := m.referrals[inviteeID]; !ok {
			m.referrals[inviteeID] = &ledger.ReferralRow{InviteeID: inviteeID, InviterID: uid, CreatedAt: time.Now()}
		}
		return nil
	}
	return ledger.ErrNotFound
}

func (m *Memory) Referrals(_ context.Context, inviterID string, _ int) ([]ledger.ReferralRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opsInit()
	var out []ledger.ReferralRow
	for _, x := range m.referrals {
		if inviterID == "" || x.InviterID == inviterID {
			out = append(out, *x)
		}
	}
	return out, nil
}

func (m *Memory) RewardReferral(ctx context.Context, userID, orderID string, paid int64) (*ledger.Txn, error) {
	m.mu.Lock()
	m.opsInit()
	x, ok := m.referrals[userID]
	if !ok || !x.RewardedAt.IsZero() {
		m.mu.Unlock()
		return nil, nil
	}
	reward := m.Referral
	if reward == 0 {
		reward = int64(float64(paid) * m.ReferralPercent / 100)
	}
	if m.ReferralCap > 0 {
		var sum int64
		for _, y := range m.referrals {
			if y.InviterID == x.InviterID {
				sum += y.RewardMicro
			}
		}
		reward = max(min(reward, m.ReferralCap-sum), 0)
	}
	x.RewardMicro, x.RewardedAt, x.OrderID = reward, time.Now(), orderID
	m.mu.Unlock()
	if reward == 0 {
		return nil, nil
	}
	t := ledger.Txn{ID: "referral:" + userID, Subject: ledger.UserSubject(x.InviterID), Kind: ledger.Referral, AmountMicro: reward,
		Ref: userID, Note: orderID}
	ok, a, err := m.Credit(ctx, t)
	if err != nil || !ok {
		return nil, err
	}
	t.BalanceAfterMicro = a.BalanceMicro
	return &t, nil
}

func (m *Memory) CheckIn(ctx context.Context, userID, subject, day string, amount int64) (ledger.Txn, bool, error) {
	t := ledger.Txn{ID: "checkin:" + userID + ":" + day, Subject: subject, Kind: ledger.Checkin, AmountMicro: amount, Ref: day}
	ok, a, err := m.Credit(ctx, t)
	if err != nil {
		return ledger.Txn{}, false, err
	}
	if !ok {
		t, err = m.Txn(ctx, t.ID)
		return t, false, err
	}
	t.BalanceAfterMicro = a.BalanceMicro
	return t, true, nil
}

func (m *Memory) CheckIns(_ context.Context, userID string, limit int) ([]ledger.Txn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ledger.Txn
	for id, t := range m.txns {
		if strings.HasPrefix(id, "checkin:"+userID+":") {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref > out[j].Ref })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
