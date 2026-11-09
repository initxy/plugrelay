// Package rewards is the admin.rewards plugin (sell kit): a daily
// check-in that credits a small amount once per user and day, and
// referral rewards that pay an inviter when someone they invited first
// tops up. Both are off until switched on in the billing feature's form.
//
// Rewards are balance transactions decided inside one store transaction
// (check-in: one per user and day by its id; referral: one per invitee,
// within the inviter's cap), so a double click, a retried callback or a
// race never pays twice. The referral reward itself is paid by
// admin.payment when the invitee's top-up is credited; sign-up records
// who invited whom.
package rewards

import (
	"context"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

// ID is the plugin id.
const ID = "admin.rewards"

func init() { plugin.Register(ID, func() plugin.Plugin { return New(nil) }) }

// Plugin implements plugin.Admin.
type Plugin struct {
	l        ledger.Ledger
	settings plugin.Settings
	now      func() time.Time
}

var _ plugin.Admin = (*Plugin)(nil)

// New returns the plugin; a nil ledger is taken from the Host in Init.
func New(l ledger.Ledger) *Plugin { return &Plugin{l: l, now: time.Now} }

// Info implements plugin.Plugin.
func (p *Plugin) Info() plugin.Info {
	T := plugin.T
	g := func(s plugin.Setting) plugin.Setting {
		s.Scopes, s.Merge = plugin.Global, plugin.Override
		return s
	}
	return plugin.Info{ID: ID, Kind: plugin.KindAdmin, Version: "1.0.0",
		Description: "Daily check-in rewards and referral rewards, paid into the balance once.",
		Requires:    []string{"meter.balance"},
		Settings: []plugin.Setting{
			g(plugin.Setting{Key: "checkin_enabled", Type: plugin.TypeBool, Default: false,
				Label: T("Daily check-in", "每日签到"),
				Help:  T("Members check in once a day on their wallet page and get a small credit.", "成员每天在钱包页签到一次，领一小笔余额。")}),
			g(plugin.Setting{Key: "checkin_min_micro", Type: plugin.TypeInt, Default: 100_000, Unit: plugin.UnitMoney,
				Label: T("Check-in reward, lowest", "签到奖励下限"),
				Help:  T("Each check-in pays a random amount between the lowest and highest; make them equal for a fixed reward.", "每次签到在下限和上限之间随机给；两个设成一样就是固定金额。")}),
			g(plugin.Setting{Key: "checkin_max_micro", Type: plugin.TypeInt, Default: 500_000, Unit: plugin.UnitMoney,
				Label: T("Check-in reward, highest", "签到奖励上限"),
				Help:  T("In the site currency.", "按站点币种。")}),
			g(plugin.Setting{Key: "referral_enabled", Type: plugin.TypeBool, Default: false,
				Label: T("Referral rewards", "邀请返利"),
				Help: T("Each member gets an invite link; when someone who signed up through it first tops up, the inviter is rewarded once.",
					"每个成员有一个邀请链接；通过它注册的人第一次充值后，邀请人得到一次奖励。")}),
			g(plugin.Setting{Key: "referral_mode", Type: plugin.TypeString, Default: "percent", Enum: []string{"percent", "fixed"},
				EnumLabels: map[string]plugin.Text{"percent": T("A share of the first top-up", "首次充值的一定比例"),
					"fixed": T("A fixed amount", "固定金额")},
				Label: T("Referral reward", "返利方式"),
				Help:  T("A share of what the invitee first paid, or a fixed amount.", "按被邀请人首次支付金额的比例，或者给固定金额。")}),
			g(plugin.Setting{Key: "referral_percent", Type: plugin.TypeFloat, Default: 10.0,
				Label: T("Share of the first top-up (%)", "首充返利比例（%）"),
				Help:  T("Of what the invitee paid, after any coupon.", "按被邀请人实际支付的金额算（扣掉优惠券之后）。")}),
			g(plugin.Setting{Key: "referral_fixed_micro", Type: plugin.TypeInt, Default: 1_000_000, Unit: plugin.UnitMoney,
				Label: T("Fixed referral reward", "固定返利金额"),
				Help:  T("Used when the reward is a fixed amount.", "返利方式选固定金额时用。")}),
			g(plugin.Setting{Key: "referral_cap_micro", Type: plugin.TypeInt, Default: 0, Unit: plugin.UnitMoney,
				Label: T("Most one inviter can earn", "每人最多返利"),
				Help:  T("In total, over all their invitees; 0 means no limit.", "一个邀请人从所有被邀请人那里累计最多拿多少；0 表示不限。")}),
		}}
}

// Init implements plugin.Initializer.
func (p *Plugin) Init(_ context.Context, h plugin.Host) error {
	if p.l == nil {
		l, err := ledger.From(h)
		if err != nil {
			return err
		}
		p.l = l
	}
	p.settings = h.Settings()
	return nil
}

// Routes implements plugin.Admin.
func (p *Plugin) Routes() []plugin.AdminRoute {
	return []plugin.AdminRoute{
		{Method: http.MethodGet, Path: "/me", Handler: p.getMe},
		{Method: http.MethodPost, Path: "/checkin", Handler: p.postCheckin},
		{Method: http.MethodGet, Path: "/me/referrals", Handler: p.getMyReferrals},
		{Method: http.MethodGet, Path: "/referrals", Handler: p.getReferrals, Role: "admin"},
	}
}

func (p *Plugin) get(key string) plugin.Value {
	if p.settings == nil {
		return plugin.Value{}
	}
	return p.settings.Get(key)
}

func (p *Plugin) money(key string, def int64) int64 {
	if v := p.get(key); v.IsSet() {
		return v.Int()
	}
	return def
}

// rewardRange is the check-in reward's lowest and highest amount.
func (p *Plugin) rewardRange() (lo, hi int64) {
	lo, hi = max(p.money("checkin_min_micro", 100_000), 0), max(p.money("checkin_max_micro", 500_000), 0)
	if hi < lo {
		lo, hi = hi, lo
	}
	return lo, hi
}

// draw picks a check-in reward in whole cents.
func draw(lo, hi int64) int64 {
	const cent = 10_000
	steps := (hi - lo) / cent
	if steps <= 0 {
		return lo
	}
	return lo + rand.Int64N(steps+1)*cent
}

// today is the check-in day in the site's time zone.
func (p *Plugin) today(ctx context.Context) (string, *time.Location) {
	loc := p.l.Timezone(ctx)
	return p.now().In(loc).Format(time.DateOnly), loc
}

// streak counts the consecutive days up to today (or up to yesterday
// when today is not checked in yet) among days, newest first.
func streak(days []string, today string, loc *time.Location) int {
	d, err := time.ParseInLocation(time.DateOnly, today, loc)
	if err != nil || len(days) == 0 {
		return 0
	}
	if days[0] != today {
		d = d.AddDate(0, 0, -1)
	}
	n := 0
	for _, day := range days {
		if day != d.Format(time.DateOnly) {
			break
		}
		n++
		d = d.AddDate(0, 0, -1)
	}
	return n
}

func user(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid, _ := ledger.Who(r)
	if uid == "" {
		ledger.WriteError(w, http.StatusForbidden, "forbidden", "a console user is required")
	}
	return uid, uid != ""
}

func (p *Plugin) getMe(w http.ResponseWriter, r *http.Request) {
	uid, ok := user(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	cur := p.l.Currency(ctx)
	m := func(v int64) ledger.Money { return ledger.Money{Micro: v, Currency: cur} }
	out := map[string]any{"currency": cur}

	ci := map[string]any{"enabled": p.get("checkin_enabled").Bool()}
	lo, hi := p.rewardRange()
	ci["min"], ci["max"] = m(lo), m(hi)
	checks, err := p.l.CheckIns(ctx, uid, 60)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	today, loc := p.today(ctx)
	days := make([]string, 0, len(checks))
	var total int64
	for _, t := range checks {
		days = append(days, t.Ref)
		total += t.AmountMicro
	}
	ci["today"] = today
	ci["done_today"] = len(days) > 0 && days[0] == today
	if len(days) > 0 && days[0] == today {
		ci["today_amount"] = m(checks[0].AmountMicro)
	}
	ci["streak"] = streak(days, today, loc)
	ci["days"] = days
	ci["recent_total"] = m(total)
	out["checkin"] = ci

	ref := map[string]any{"enabled": p.get("referral_enabled").Bool()}
	if ref["enabled"] == true {
		code, err := p.l.ReferralCode(ctx, uid)
		if err != nil {
			ledger.Fail(w, err)
			return
		}
		rows, err := p.l.Referrals(ctx, uid, 1000)
		if err != nil {
			ledger.Fail(w, err)
			return
		}
		var earned int64
		rewarded := 0
		for _, x := range rows {
			earned += x.RewardMicro
			if !x.RewardedAt.IsZero() {
				rewarded++
			}
		}
		mode := p.get("referral_mode").String()
		if mode == "" {
			mode = "percent"
		}
		ref["code"], ref["invited"], ref["rewarded"], ref["earned"] = code, len(rows), rewarded, m(earned)
		ref["mode"], ref["fixed"], ref["cap"] = mode, m(p.money("referral_fixed_micro", 1_000_000)), m(p.money("referral_cap_micro", 0))
		pct := 10.0
		if v := p.get("referral_percent"); v.IsSet() {
			pct = v.Float()
		}
		ref["percent"] = pct
	}
	out["referral"] = ref
	ledger.WriteJSON(w, http.StatusOK, out)
}

func (p *Plugin) postCheckin(w http.ResponseWriter, r *http.Request) {
	uid, ok := user(w, r)
	if !ok {
		return
	}
	var in struct {
		// KeyID picks the account when keys or groups pay (meter.balance payer).
		KeyID string `json:"key_id"`
	}
	if r.ContentLength != 0 && !ledger.ReadJSON(w, r, &in) {
		return
	}
	if !p.get("checkin_enabled").Bool() {
		ledger.WriteError(w, http.StatusNotFound, "checkin_off", "daily check-in is not offered")
		return
	}
	ctx := r.Context()
	subject, err := p.l.PayerOf(ctx, uid, in.KeyID)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	lo, hi := p.rewardRange()
	if hi <= 0 {
		ledger.WriteError(w, http.StatusNotFound, "checkin_off", "daily check-in is not offered")
		return
	}
	today, _ := p.today(ctx)
	t, applied, err := p.l.CheckIn(ctx, uid, subject, today, draw(max(lo, 10_000), hi))
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	cur := p.l.Currency(ctx)
	status := http.StatusOK
	if !applied {
		status = http.StatusConflict
		ledger.WriteJSON(w, status, map[string]any{"error": map[string]any{"code": "checked_in",
			"message": "you have checked in today already"}, "amount": ledger.Money{Micro: t.AmountMicro, Currency: cur}})
		return
	}
	ledger.WriteJSON(w, status, map[string]any{"day": today, "amount": ledger.Money{Micro: t.AmountMicro, Currency: cur},
		"balance": ledger.Money{Micro: t.BalanceAfterMicro, Currency: cur}})
}

type referralView struct {
	ledger.ReferralRow
	Reward ledger.Money `json:"reward"`
}

func (p *Plugin) referrals(w http.ResponseWriter, r *http.Request, inviter string) {
	ctx := r.Context()
	rows, err := p.l.Referrals(ctx, inviter, 500)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	cur := p.l.Currency(ctx)
	items := make([]referralView, 0, len(rows))
	for _, x := range rows {
		if inviter != "" {
			x.InviteeID, x.OrderID = "", "" // members see when and how much, not who
		}
		items = append(items, referralView{ReferralRow: x, Reward: ledger.Money{Micro: x.RewardMicro, Currency: cur}})
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "currency": cur})
}

func (p *Plugin) getMyReferrals(w http.ResponseWriter, r *http.Request) {
	if uid, ok := user(w, r); ok {
		p.referrals(w, r, uid)
	}
}

func (p *Plugin) getReferrals(w http.ResponseWriter, r *http.Request) {
	p.referrals(w, r, r.URL.Query().Get("inviter_id"))
}
