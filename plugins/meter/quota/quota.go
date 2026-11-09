// Package quota is the meter.quota plugin: spending and token budgets per
// key, user and group, with optional usage headers for clients.
//
// Budgets are settings, all merged Strictest (0 = unlimited):
//
//	<level>_total_micro     lifetime spend, micro-units of the site currency
//	<level>_daily_micro     spend since the start of the day
//	<level>_monthly_micro   spend since the start of the month
//	<level>_daily_tokens    tokens since the start of the day
//	<level>_monthly_tokens  tokens since the start of the month
//
// where level is key, user or group. A budget on a higher layer is the
// default for each subject below it: key_daily_micro set on a group gives
// every key of that group its own daily budget, while group_daily_micro is
// one budget the whole group shares. Days and months start at midnight in
// the site time zone.
//
// Spend comes from the core's recorded usage (UsageReader.Spent) plus the
// reservations still outstanding, which live in plugin.SharedState so all nodes
// see them. Token counts are kept by this plugin in its durable KV. Settle
// and Refund are idempotent per request id through a SetNX marker: whichever
// runs first applies, later calls are no-ops.
//
// Known lag: between Settle and the usage row becoming visible to
// UsageReader, a request's cost is counted in neither place.
package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// ID is the plugin id.
const ID = "meter.quota"

func init() { plugin.Register(ID, func() plugin.Plugin { return New(Deps{}) }) }

// DefaultUsageHeader is the default name of the usage-percent header.
const DefaultUsageHeader = "x-ratelimit-budget-used-percent"

const (
	heldTTL = time.Hour      // bounds a reservation leaked by a crashed node
	doneTTL = 48 * time.Hour // idempotency markers
)

// StateHost is implemented by a Host that shares the cluster state.
type StateHost = plugin.StateHost

// TimezoneHost is implemented by a Host that knows the site time zone.
type TimezoneHost interface {
	Timezone(ctx context.Context) *time.Location
}

// Deps are the plugin's backends. Zero fields are taken from the Host in Init.
type Deps struct {
	State plugin.SharedState
	Usage plugin.UsageReader
	KV    plugin.KV
	// Notify hears about budgets used from NoticeFrom percent up; nil
	// tells no one.
	Notify func(context.Context, plugin.Notice)
	// Timezone is the site time zone days and months start in; nil is UTC.
	Timezone func(context.Context) *time.Location
}

// NoticeFrom is the share of a budget, in percent, from which the meter
// raises a "budget_reached" notice on each reservation; the notifications
// feature picks its own threshold above it and drops repeats.
const NoticeFrom = 50

func (m *Meter) notice(ctx context.Context, s subject, w window, kind string, start time.Time, pct float64) {
	if m.d.Notify == nil || pct < NoticeFrom {
		return
	}
	typ, id, _ := strings.Cut(s.id, ":")
	m.d.Notify(ctx, plugin.Notice{Event: "budget_reached", Object: plugin.NoticeObject{Type: typ, ID: id},
		Key:    kind + ":" + string(w) + ":" + start.Format("2006-01-02"),
		Detail: map[string]string{"percent": strconv.Itoa(int(pct)), "window": string(w), "kind": kind}})
}

// Option configures the plugin.
type Option func(*Meter)

// WithClock overrides time.Now.
func WithClock(now func() time.Time) Option { return func(m *Meter) { m.now = now } }

// Meter implements plugin.Meter and plugin.QuotaHeaders.
type Meter struct {
	d   Deps
	now func() time.Time
}

// New returns the plugin.
func New(d Deps, opts ...Option) *Meter {
	m := &Meter{d: d, now: time.Now}
	for _, o := range opts {
		o(m)
	}
	return m
}

type level struct {
	name   string
	scopes plugin.Scope
	id     func(*plugin.Caller) string
	// who names the level for people ("each key", "每个密钥").
	who plugin.Text
}

var levels = []level{
	{"key", plugin.Global | plugin.Group | plugin.User | plugin.Key, func(c *plugin.Caller) string { return c.KeyID },
		plugin.T("each key", "每个密钥")},
	{"user", plugin.Global | plugin.Group | plugin.User, func(c *plugin.Caller) string { return c.UserID },
		plugin.T("each user", "每个用户")},
	{"group", plugin.Global | plugin.Group, func(c *plugin.Caller) string { return c.GroupID },
		plugin.T("each group", "每个分组")},
}

type window string

const (
	total   window = "total"
	daily   window = "daily"
	monthly window = "monthly"
)

// windowText names a budget window for people.
var windowText = map[window]plugin.Text{
	total:   plugin.T("in total", "累计"),
	daily:   plugin.T("per day", "每天"),
	monthly: plugin.T("per month", "每月"),
}

// Info implements plugin.Plugin.
func (m *Meter) Info() plugin.Info {
	var set []plugin.Setting
	unlimited := plugin.T("0 means no limit.", "0 表示不限。")
	for _, l := range levels {
		for _, w := range []window{total, daily, monthly} {
			set = append(set, plugin.Setting{Key: l.name + "_" + string(w) + "_micro", Type: plugin.TypeInt, Default: 0,
				Scopes: l.scopes, Merge: plugin.Strictest, Unit: plugin.UnitMoney,
				Label: plugin.T(fmt.Sprintf("Spend budget for %s, %s", l.who.EN, windowText[w].EN),
					fmt.Sprintf("%s%s花费上限", l.who.ZH, windowText[w].ZH)),
				Help: unlimited})
		}
		for _, w := range []window{daily, monthly} {
			set = append(set, plugin.Setting{Key: l.name + "_" + string(w) + "_tokens", Type: plugin.TypeInt, Default: 0,
				Scopes: l.scopes, Merge: plugin.Strictest,
				Label: plugin.T(fmt.Sprintf("Token budget for %s, %s", l.who.EN, windowText[w].EN),
					fmt.Sprintf("%s%s token 上限", l.who.ZH, windowText[w].ZH)),
				Help: unlimited})
		}
	}
	set = append(set,
		plugin.Setting{Key: "usage_header", Type: plugin.TypeString, Default: DefaultUsageHeader,
			Scopes: plugin.Global | plugin.Group, Merge: plugin.Override,
			Label: plugin.T("Response header with budget used", "回报已用额度的响应头"),
			Help: plugin.T("Carries the highest percentage of any budget used so far; empty sends none. "+
				"Use a name a client already reads (e.g. x-codex-primary-used-percent) to show usage there.",
				"告诉客户端已经用掉的额度比例（取各项上限里最高的一个）；留空不发送。"+
					"填客户端本来就会读的名字（例如 x-codex-primary-used-percent），客户端里就能看到用量。")},
		plugin.Setting{Key: "reset_header", Type: plugin.TypeString, Scopes: plugin.Global | plugin.Group, Merge: plugin.Override,
			Label: plugin.T("Response header with time to reset", "回报额度重置时间的响应头"),
			Help: plugin.T("Carries the seconds until that budget starts over; empty sends none.",
				"告诉客户端还有多少秒额度重新计算；留空不发送。")},
	)
	return plugin.Info{ID: ID, Kind: plugin.KindMeter, Version: "1.0.0", FailPolicy: plugin.FailClosed,
		Description: "Total, daily and monthly budgets by spend or tokens per key, user and group.", Settings: set,
		// Spend is summed from request records; without them every
		// budget would read as unspent.
		Requires: []string{"sink.requestlog"}}
}

// Init implements plugin.Initializer.
func (m *Meter) Init(_ context.Context, h plugin.Host) error {
	if m.d.State == nil {
		if sh, ok := h.(StateHost); ok {
			m.d.State = sh.State()
		}
	}
	if m.d.Usage == nil {
		m.d.Usage = h.Usage()
	}
	if m.d.KV == nil {
		m.d.KV = h.KV()
	}
	if nh, ok := h.(plugin.NoticeHost); ok && m.d.Notify == nil {
		m.d.Notify = nh.Notify
	}
	if th, ok := h.(TimezoneHost); ok && m.d.Timezone == nil {
		m.d.Timezone = th.Timezone
	}
	if m.d.State == nil || m.d.Usage == nil || m.d.KV == nil {
		return errors.New("quota: state, usage reader and KV are required")
	}
	return nil
}

// subject is one budget owner of a call.
type subject struct {
	level string
	id    string // "key:<id>" etc., as UsageReader expects
}

func (m *Meter) subjects(c *plugin.Call) []subject {
	if c.Caller == nil {
		return nil
	}
	var out []subject
	for _, l := range levels {
		if id := l.id(c.Caller); id != "" {
			out = append(out, subject{l.name, l.name + ":" + id})
		}
	}
	return out
}

// zone returns the site time zone, looked up on first use: most calls have
// no daily or monthly budget.
func (m *Meter) zone(ctx context.Context) func() *time.Location {
	var loc *time.Location
	return func() *time.Location {
		if loc == nil {
			loc = time.UTC
			if m.d.Timezone != nil {
				if l := m.d.Timezone(ctx); l != nil {
					loc = l
				}
			}
		}
		return loc
	}
}

// bounds returns the start of the window containing now and the next reset
// (zero for total).
func bounds(w window, now time.Time, loc *time.Location) (start, reset time.Time) {
	t := now.In(loc)
	switch w {
	case daily:
		start = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		return start, start.AddDate(0, 0, 1)
	case monthly:
		start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
		return start, start.AddDate(0, 1, 0)
	}
	return time.Time{}, time.Time{}
}

func heldKey(s subject) string { return "quota:held:" + s.id }

func tokenKey(s subject, w window, start time.Time) string {
	return "tok:" + s.id + ":" + string(w) + ":" + start.Format("2006-01-02")
}

// usage is the furthest-used budget, for headers.
type usage struct {
	pct   float64
	reset time.Time
}

type held struct {
	subjects []subject
}

// Reserve implements plugin.Meter.
func (m *Meter) Reserve(c *plugin.Call, estimateMicro int64) (*plugin.Reservation, error) {
	ctx := c.Context
	now := m.now()
	loc := m.zone(ctx)
	res := &plugin.Reservation{ID: c.ID, AmountMicro: max(estimateMicro, 0)}
	var h held
	var worst usage
	budgeted := false
	note := func(s subject, w window, kind string, start time.Time, used, budget int64, reset time.Time) {
		budgeted = true
		p := float64(used) * 100 / float64(budget)
		if p >= worst.pct {
			worst = usage{min(p, 100), reset}
		}
		m.notice(ctx, s, w, kind, start, min(p, 100))
	}
	fail := func(err error) (*plugin.Reservation, error) {
		m.release(ctx, h.subjects, res.AmountMicro)
		return nil, err
	}

	for _, s := range m.subjects(c) {
		for _, w := range []window{daily, monthly} {
			budget := c.Setting(s.level + "_" + string(w) + "_tokens").Int()
			if budget <= 0 {
				continue
			}
			start, reset := bounds(w, now, loc())
			used, err := m.kvInt(ctx, tokenKey(s, w, start))
			if err != nil {
				return fail(err)
			}
			note(s, w, "token", start, used, budget, reset)
			if used >= budget {
				return fail(exhausted(s, w, "token", reset))
			}
		}

		var money []window
		for _, w := range []window{total, daily, monthly} {
			if c.Setting(s.level+"_"+string(w)+"_micro").Int() > 0 {
				money = append(money, w)
			}
		}
		if len(money) == 0 {
			continue
		}
		// Hold first, then check, so concurrent reservations see each other.
		outstanding, err := m.d.State.Incr(ctx, heldKey(s), res.AmountMicro, heldTTL)
		if err != nil {
			return fail(err)
		}
		h.subjects = append(h.subjects, s)
		for _, w := range money {
			budget := c.Setting(s.level + "_" + string(w) + "_micro").Int()
			start, reset := bounds(w, now, loc())
			spent, _, err := m.d.Usage.Spent(ctx, s.id, start)
			if err != nil {
				return fail(err)
			}
			note(s, w, "spend", start, spent+outstanding-res.AmountMicro, budget, reset)
			if spent+outstanding > budget {
				return fail(exhausted(s, w, "spend", reset))
			}
		}
	}
	c.Set("held", h)
	if budgeted {
		c.Set("usage", worst)
	}
	return res, nil
}

// Settle implements plugin.Meter. Cost itself is recorded by the core; this
// releases the hold and counts tokens.
func (m *Meter) Settle(c *plugin.Call, r *plugin.Reservation, u ir.Usage, _ int64) error {
	ctx := context.WithoutCancel(c.Context)
	if first, err := m.once(ctx, c, r); err != nil || !first {
		return err
	}
	m.releaseCall(ctx, c, r)
	tokens := u.TotalInput() + u.Output
	if tokens <= 0 {
		return nil
	}
	now, loc := m.now(), m.zone(ctx)
	var errs []error
	for _, s := range m.subjects(c) {
		for _, w := range []window{daily, monthly} {
			if c.Setting(s.level+"_"+string(w)+"_tokens").Int() <= 0 {
				continue
			}
			start, reset := bounds(w, now, loc())
			if _, err := m.d.KV.Incr(ctx, tokenKey(s, w, start), tokens, reset.Sub(now)+24*time.Hour); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Refund implements plugin.Meter.
func (m *Meter) Refund(c *plugin.Call, r *plugin.Reservation) error {
	ctx := context.WithoutCancel(c.Context)
	if first, err := m.once(ctx, c, r); err != nil || !first {
		return err
	}
	m.releaseCall(ctx, c, r)
	return nil
}

// Headers implements plugin.QuotaHeaders.
func (m *Meter) Headers(c *plugin.Call) http.Header {
	v, ok := c.Get("usage")
	if !ok {
		return nil
	}
	u := v.(usage)
	h := http.Header{}
	name := DefaultUsageHeader
	if v := c.Setting("usage_header"); v.IsSet() {
		name = v.String()
	}
	if name != "" {
		h.Set(name, strconv.FormatFloat(u.pct, 'f', 1, 64))
	}
	if name := c.Setting("reset_header").String(); name != "" && !u.reset.IsZero() {
		h.Set(name, strconv.FormatInt(int64(u.reset.Sub(m.now()).Seconds()), 10))
	}
	return h
}

func (m *Meter) once(ctx context.Context, c *plugin.Call, r *plugin.Reservation) (bool, error) {
	id := c.ID
	if r != nil && r.ID != "" {
		id = r.ID
	}
	return m.d.State.SetNX(ctx, "quota:done:"+id, []byte("1"), doneTTL)
}

func (m *Meter) releaseCall(ctx context.Context, c *plugin.Call, r *plugin.Reservation) {
	v, ok := c.Get("held")
	if !ok || r == nil {
		return
	}
	m.release(ctx, v.(held).subjects, r.AmountMicro)
}

func (m *Meter) release(ctx context.Context, subs []subject, amount int64) {
	if amount == 0 {
		return
	}
	for _, s := range subs {
		_, _ = m.d.State.Incr(ctx, heldKey(s), -amount, heldTTL)
	}
}

func (m *Meter) kvInt(ctx context.Context, key string) (int64, error) {
	b, ok, err := m.d.KV.Get(ctx, key)
	if err != nil || !ok {
		return 0, err
	}
	n, _ := strconv.ParseInt(string(b), 10, 64)
	return n, nil
}

func exhausted(s subject, w window, what string, reset time.Time) *ir.GwError {
	e := ir.Errorf(ir.ErrQuotaExhausted, "%s %s budget of this %s is exhausted", w, what, s.level)
	e.ResetAt = reset
	e.NoRetry = true
	return e
}
