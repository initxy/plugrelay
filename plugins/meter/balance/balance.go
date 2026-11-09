// Package balance is the meter.balance plugin (sell kit): prepaid money per
// user (or per group or key) that requests draw on.
//
// Reserve holds the request's estimate (the core prices the input estimate
// and output limit at the caller's rate, list price × group multiplier),
// raised to min_reserve_micro; a request is refused when the available
// balance plus the payer's credit limit does not cover it. Settle charges
// the delivered attempt's charge, computed by the core the same way, in
// full and releases the hold; Refund releases it. Both are idempotent per
// request id in the core's ledger. A settle that fails is collected by the
// sweep from the request log record (written at-least-once after it), so
// a database blip never turns a delivered request into a free one; hence
// the requirement on sink.requestlog. Because the final charge is never cut
// short, a request whose output ran past its estimate can take the
// balance below zero once (by the uncovered part); the next one is
// refused. A credit limit on the payer allows going below zero on purpose.
//
// Refusals use the client protocol's own "out of credit" signal: 402
// billing_error for Anthropic clients, 429 insufficient_quota for OpenAI
// clients, 429 RESOURCE_EXHAUSTED for Gemini clients; all say not to retry.
//
// A user with a current plan subscription (admin.subscriptions) draws on
// the period's allowance first, in the same core store: the request holds
// its estimate (money, or tokens: Call.EstimateTokens) against what is
// left; once it is used up the plan either refuses (the same out-of-credit
// signal) or lets the balance pay as without a plan. A token plan covers
// token-priced requests only: a media operation that may be priced per
// unit (Call.UnitPriced: images, speech, transcription, rerank) pays from
// the balance as without a plan, whatever the plan's overage; so does the
// unit-priced part of any other request (Call.UnitChargeMicro: search
// units of a chat request, say). Batches are always billed to the balance.
package balance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

// ID is the plugin id.
const ID = "meter.balance"

func init() { plugin.Register(ID, func() plugin.Plugin { return New(nil) }) }

// Meter implements plugin.Meter, plugin.QuotaHeaders and plugin.Admin.
type Meter struct {
	l        ledger.Ledger
	subs     ledger.Subscriptions // nil without plan subscriptions
	settings plugin.Settings
	now      func() time.Time
	log      func(msg string, args ...any)

	lastSweep atomic.Int64 // unix nanoseconds
}

var (
	_ plugin.Meter         = (*Meter)(nil)
	_ plugin.DeferredMeter = (*Meter)(nil)
	_ plugin.QuotaHeaders  = (*Meter)(nil)
	_ plugin.Admin         = (*Meter)(nil)
)

// New returns the plugin; a nil ledger is taken from the Host in Init.
func New(l ledger.Ledger) *Meter {
	return &Meter{l: l, now: time.Now, log: func(string, ...any) {}}
}

// Info implements plugin.Plugin.
func (m *Meter) Info() plugin.Info {
	return plugin.Info{ID: ID, Kind: plugin.KindMeter, Version: "1.0.0", FailPolicy: plugin.FailClosed,
		Description: "Prepaid balance: hold the estimate, charge price × group multiplier, release on failure.",
		// A settle that fails is collected from the request log.
		Requires: []string{"sink.requestlog"},
		Settings: []plugin.Setting{
			{Key: "payer", Type: plugin.TypeString, Default: "user", Enum: []string{"user", "group", "key"},
				Scopes: plugin.Global, Merge: plugin.Override, Label: plugin.T("Whose balance pays", "从谁的余额扣费"),
				Help: plugin.T("The key's user (a key without a user pays from its own), the key's group, or the key itself.",
					"密钥所属用户（没有用户的密钥用自己的余额）、密钥所在分组，或者密钥自己。"),
				EnumLabels: map[string]plugin.Text{"user": plugin.T("The user", "用户"), "group": plugin.T("The group", "分组"),
					"key": plugin.T("The key", "密钥")}},
			{Key: "exempt", Type: plugin.TypeBool, Default: false, Scopes: plugin.Global | plugin.Group | plugin.User | plugin.Key,
				Merge: plugin.Override, Label: plugin.T("Free of charge", "不扣余额"),
				Help: plugin.T("Serve without drawing on any balance, e.g. for staff or internal keys.",
					"使用时不扣余额，适合员工或内部密钥。")},
			{Key: "min_reserve_micro", Type: plugin.TypeInt, Default: 0, Scopes: plugin.Global | plugin.Group, Merge: plugin.Override,
				Unit: plugin.UnitMoney, Label: plugin.T("Smallest hold per request", "每次请求至少预扣"),
				Help: plugin.T("Held from the balance while a request without an output limit runs; the actual price is charged at the end.",
					"请求没写输出上限时，先从余额里冻结这么多，结束后按实际价格结算。")},
			{Key: "balance_header", Type: plugin.TypeString, Default: "", Scopes: plugin.Global | plugin.Group, Merge: plugin.Override,
				Label: plugin.T("Response header with balance", "回报余额的响应头"),
				Help: plugin.T("Carries the available balance in the site currency; empty sends none.",
					"把可用余额（按站点币种）放进这个响应头；留空不发送。")},
			{Key: "low_balance_micro", Type: plugin.TypeInt, Default: 0, Scopes: plugin.Global | plugin.Group, Merge: plugin.Override,
				Unit: plugin.UnitMoney, Label: plugin.T("Send the balance header only below", "余额低于多少才发余额响应头"),
				Help: plugin.T("0 sends it on every response.", "0 表示每次都发。")},
			{Key: "hold_ttl", Type: plugin.TypeDuration, Default: "6h", Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Release stuck holds after", "冻结多久没结算就释放"),
				Help: plugin.T("A hold left by a node that stopped mid-request is released after this long; a request record that arrives later still charges.",
					"服务节点在请求中途停掉时会留下冻结金额，过了这段时间就释放；之后补到的请求记录照样扣费。")},
		}}
}

// Init implements plugin.Initializer.
func (m *Meter) Init(_ context.Context, h plugin.Host) error {
	if m.l == nil {
		l, err := ledger.From(h)
		if err != nil {
			return err
		}
		m.l = l
	}
	if m.subs == nil {
		m.subs = ledger.SubscriptionsFrom(h)
	}
	m.settings = h.Settings()
	if lg := h.Logger(); lg != nil {
		m.log = lg.Warn
	}
	return nil
}

// payer is the subject a call draws on.
func (m *Meter) payer(c *plugin.Call) string {
	cl := c.Caller
	if cl == nil {
		return ""
	}
	switch c.Setting("payer").String() {
	case "group":
		if cl.GroupID != "" {
			return ledger.GroupSubject(cl.GroupID)
		}
	case "key":
		return ledger.KeySubject(cl.KeyID)
	}
	if cl.UserID != "" {
		return ledger.UserSubject(cl.UserID)
	}
	return ledger.KeySubject(cl.KeyID)
}

type state struct {
	exempt    bool
	available int64
	// sub: the request draws on a plan's allowance.
	sub bool
}

// Reserve implements plugin.Meter.
func (m *Meter) Reserve(c *plugin.Call, estimate int64) (*plugin.Reservation, error) {
	res := &plugin.Reservation{ID: c.ID, AmountMicro: max(estimate, c.Setting("min_reserve_micro").Int(), 0)}
	subject := m.payer(c)
	if subject == "" || c.Setting("exempt").Bool() {
		c.Set("state", state{exempt: true})
		return res, nil
	}
	m.sweep(c)
	if m.subs != nil && c.Caller.UserID != "" {
		h, err := m.subs.SubscriptionHold(c.Context, c.Caller.UserID, subject, res.ID,
			ledger.SubEstimate{Money: res.AmountMicro, Tokens: c.EstimateTokens, UnitPriced: c.UnitPriced}, m.now())
		switch {
		case err == nil && h.Covered:
			c.Set("state", state{sub: true})
			return res, nil
		case errors.Is(err, ledger.ErrAllowanceExhausted):
			return nil, m.planRefusal(c, h.Sub)
		case err != nil && !errors.Is(err, ledger.ErrNotFound):
			return nil, fmt.Errorf("%w: subscription: %v", plugin.ErrPluginFailure, err)
		}
	}
	a, err := m.l.Hold(c.Context, subject, res.ID, res.AmountMicro)
	if errors.Is(err, ledger.ErrInsufficient) {
		return nil, m.refusal(c, a, res.AmountMicro)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: balance: %v", plugin.ErrPluginFailure, err)
	}
	c.Set("state", state{available: a.Available()})
	return res, nil
}

// refusal is the client-protocol error for an empty balance.
func (m *Meter) refusal(c *plugin.Call, a ledger.Account, need int64) *ir.GwError {
	cur := m.l.Currency(c.Context)
	msg := "Your credit balance is too low: available " + ledger.Format(a.Available()+a.CreditLimitMicro, cur)
	if need > 0 {
		msg += ", this request needs about " + ledger.Format(need, cur)
	}
	msg += ". Top up your balance in the console."
	return outOfCredit(c, msg)
}

// planRefusal is the client-protocol error for a used-up plan allowance.
func (m *Meter) planRefusal(c *plugin.Call, s ledger.Sub) *ir.GwError {
	msg := "Your plan's allowance for this period is used up"
	if !s.PeriodEnd.IsZero() {
		msg += "; it renews on " + s.PeriodEnd.UTC().Format("2006-01-02 15:04 UTC")
	}
	return outOfCredit(c, msg+". Change your plan in the console.")
}

// outOfCredit is the client protocol's own out-of-credit error.
func outOfCredit(c *plugin.Call, msg string) *ir.GwError {
	e := ir.Errorf(ir.ErrQuotaExhausted, "%s", msg)
	e.NoRetry = true
	e.Code = "insufficient_quota"
	if c.Protocol == "anthropic" {
		e.Status, e.Code = http.StatusPaymentRequired, "billing_error"
	} else if c.Protocol == "gemini" {
		e.Code = ""
	}
	return e
}

// settleGrace is how long a hold of a logged request may stay open before
// the sweep settles it from the record: past the in-process retries.
const settleGrace = time.Minute

// sweep runs at most once a minute per node: it settles holds whose
// request is logged but whose settle failed (from the record's charge),
// then releases holds older than hold_ttl that have no record.
func (m *Meter) sweep(c *plugin.Call) {
	now := m.now()
	last := m.lastSweep.Load()
	if now.UnixNano()-last < int64(time.Minute) || !m.lastSweep.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	ttl := c.Setting("hold_ttl").Duration()
	if ttl <= 0 {
		ttl = 6 * time.Hour
	}
	go m.runSweep(now, ttl)
}

func (m *Meter) runSweep(now time.Time, ttl time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := m.l.SettleRecorded(ctx, now.Add(-settleGrace)); err != nil {
		m.log("balance: settling logged requests failed", "err", err)
	}
	if _, err := m.l.ExpireHolds(ctx, now.Add(-ttl)); err != nil {
		m.log("balance: releasing stale holds failed", "err", err)
	}
	if m.subs != nil {
		if _, err := m.subs.RenewDue(ctx, now); err != nil {
			m.log("balance: renewing subscriptions failed", "err", err)
		}
	}
}

func (m *Meter) skip(c *plugin.Call) bool {
	v, ok := c.Get("state")
	return !ok || v.(state).exempt
}

func onPlan(c *plugin.Call) bool {
	v, ok := c.Get("state")
	return ok && v.(state).sub
}

// tokens is what a token plan counts: every input and output token.
func tokens(u ir.Usage) int64 {
	return u.Input + u.CacheRead + u.CacheWrite5m + u.CacheWrite1h + u.Output
}

// Settle implements plugin.Meter: the charge is the core's, already
// price × group multiplier.
func (m *Meter) Settle(c *plugin.Call, r *plugin.Reservation, u ir.Usage, charge int64) error {
	if m.skip(c) {
		return nil
	}
	if onPlan(c) {
		return retry(func(ctx context.Context) error {
			_, err := m.subs.SubscriptionSettle(ctx, reqID(c, r), charge, c.UnitChargeMicro, tokens(u), c.Model)
			return err
		})
	}
	return retry(func(ctx context.Context) error {
		_, err := m.l.Capture(ctx, reqID(c, r), charge, c.Model)
		return err
	})
}

// Refund implements plugin.Meter.
func (m *Meter) Refund(c *plugin.Call, r *plugin.Reservation) error {
	if m.skip(c) {
		return nil
	}
	if onPlan(c) {
		return retry(func(ctx context.Context) error {
			_, err := m.subs.SubscriptionRelease(ctx, reqID(c, r))
			return err
		})
	}
	return retry(func(ctx context.Context) error {
		_, err := m.l.Release(ctx, reqID(c, r))
		return err
	})
}

// Hold implements plugin.DeferredMeter: a batch's estimate, held under
// id until the batch is billed or ends. Refused like Reserve.
func (m *Meter) Hold(c *plugin.Call, id string, estimate int64) error {
	subject := m.payer(c)
	if subject == "" || c.Setting("exempt").Bool() {
		return nil
	}
	amount := max(estimate, c.Setting("min_reserve_micro").Int(), 0)
	a, err := m.l.Hold(c.Context, subject, id, amount)
	if errors.Is(err, ledger.ErrInsufficient) {
		return m.refusal(c, a, amount)
	}
	if err != nil {
		return fmt.Errorf("%w: balance: %v", plugin.ErrPluginFailure, err)
	}
	return nil
}

// Charge implements plugin.DeferredMeter: the charge is taken from the
// call's payer once per id, whether or not its hold is still there, and
// may take the balance below zero; an exempt payer only has the hold
// released.
func (m *Meter) Charge(c *plugin.Call, id string, _ ir.Usage, charge int64) error {
	subject := m.payer(c)
	if subject == "" {
		return nil
	}
	if c.Setting("exempt").Bool() {
		charge = 0
	}
	return retry(func(ctx context.Context) error {
		_, err := m.l.Charge(ctx, subject, id, charge, c.Model)
		return err
	})
}

func reqID(c *plugin.Call, r *plugin.Reservation) string {
	if r != nil && r.ID != "" {
		return r.ID
	}
	return c.ID
}

// retry runs a settlement up to three times; a hold left behind is
// settled by the sweep from the request log record, never charged twice.
func retry(f func(ctx context.Context) error) error {
	var err error
	for i := range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = f(ctx)
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(time.Duration(i+1) * 100 * time.Millisecond)
	}
	return err
}

// Headers implements plugin.QuotaHeaders.
func (m *Meter) Headers(c *plugin.Call) http.Header {
	name := c.Setting("balance_header").String()
	v, ok := c.Get("state")
	if name == "" || !ok || v.(state).exempt || v.(state).sub {
		return nil
	}
	avail := v.(state).available
	if low := c.Setting("low_balance_micro").Int(); low > 0 && avail >= low {
		return nil
	}
	h := http.Header{}
	h.Set(name, strconv.FormatFloat(float64(avail)/1e6, 'f', 6, 64))
	return h
}
