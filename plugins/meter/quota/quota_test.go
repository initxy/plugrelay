package quota

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
)

var t0 = time.Date(2026, 11, 5, 15, 30, 0, 0, time.UTC)

// spent is a fake UsageReader: cost per subject, counted from a fixed time.
type spent struct {
	mu   sync.Mutex
	cost map[string]int64
	at   time.Time // all recorded cost happened at this instant
}

func (s *spent) Spent(_ context.Context, subj string, since time.Time) (int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at.Before(since) {
		return 0, 0, nil
	}
	return s.cost[subj], 1, nil
}

var caller = &plugin.Caller{KeyID: "k1", UserID: "u1", GroupID: "g1"}

func setup(cost map[string]int64) (*Meter, *testkit.State, *testkit.Clock) {
	clk := testkit.NewClock(t0)
	st := testkit.NewState(clk.Now)
	m := New(Deps{State: st, KV: testkit.NewState(clk.Now), Usage: &spent{cost: cost, at: t0.Add(-time.Hour)}}, WithClock(clk.Now))
	return m, st, clk
}

func call(id string, set testkit.Settings) *plugin.Call {
	return testkit.NewCall(ID, testkit.CallOpts{ID: id, Caller: caller, Settings: set})
}

func kindOf(err error) ir.ErrorKind {
	var gw *ir.GwError
	if errors.As(err, &gw) {
		return gw.Kind
	}
	return ""
}

func TestReserveBudgets(t *testing.T) {
	tests := []struct {
		name      string
		set       testkit.Settings
		cost      map[string]int64
		estimate  int64
		wantErr   bool
		wantReset time.Time
	}{
		{name: "no budgets", estimate: 1e9},
		{name: "key daily ok", set: testkit.Settings{"key_daily_micro": 1000}, cost: map[string]int64{"key:k1": 500}, estimate: 400},
		{name: "key daily exceeded", set: testkit.Settings{"key_daily_micro": 1000}, cost: map[string]int64{"key:k1": 700}, estimate: 400,
			wantErr: true, wantReset: time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)},
		{name: "group monthly exceeded", set: testkit.Settings{"group_monthly_micro": 1000}, cost: map[string]int64{"group:g1": 1000}, estimate: 1,
			wantErr: true, wantReset: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
		{name: "user total exceeded has no reset", set: testkit.Settings{"user_total_micro": 10}, cost: map[string]int64{"user:u1": 11},
			wantErr: true},
		{name: "other subject spend ignored", set: testkit.Settings{"key_daily_micro": 1000}, cost: map[string]int64{"key:k2": 5000}, estimate: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, _ := setup(tt.cost)
			_, err := m.Reserve(call("r", tt.set), tt.estimate)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if err != nil {
				gw := err.(*ir.GwError)
				if gw.Kind != ir.ErrQuotaExhausted || !gw.NoRetry || !gw.ResetAt.Equal(tt.wantReset) {
					t.Fatalf("got %+v", gw)
				}
			}
		})
	}
}

func TestOutstandingReservationsCount(t *testing.T) {
	m, st, _ := setup(nil)
	set := testkit.Settings{"key_daily_micro": 1000}
	a := call("a", set)
	ra, err := m.Reserve(a, 600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reserve(call("b", set), 600); kindOf(err) != ir.ErrQuotaExhausted {
		t.Fatalf("second reservation should not fit: %v", err)
	}
	// The refused reservation released its hold.
	if v, _, _ := st.Get(t.Context(), "quota:held:key:k1"); string(v) != "600" {
		t.Fatalf("held = %s", v)
	}
	if err := m.Refund(a, ra); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reserve(call("c", set), 600); err != nil {
		t.Fatalf("after refund: %v", err)
	}
}

func TestSettleRefundIdempotent(t *testing.T) {
	m, st, _ := setup(nil)
	set := testkit.Settings{"key_daily_micro": 1000, "key_daily_tokens": 100}
	c := call("a", set)
	r, err := m.Reserve(c, 300)
	if err != nil {
		t.Fatal(err)
	}
	u := ir.Usage{Input: 40, CacheRead: 10, Output: 20}
	for range 3 {
		if err := m.Settle(c, r, u, 250); err != nil {
			t.Fatal(err)
		}
		if err := m.Refund(c, r); err != nil {
			t.Fatal(err)
		}
	}
	if v, _, _ := st.Get(t.Context(), "quota:held:key:k1"); string(v) != "0" {
		t.Fatalf("held = %s, want 0", v)
	}
	if n, _ := m.kvInt(t.Context(), tokenKey(subject{"key", "key:k1"}, daily, time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC))); n != 70 {
		t.Fatalf("tokens = %d, want 70 (counted once)", n)
	}
	// 70 < 100: still allowed; a second request pushes it over.
	c2 := call("b", set)
	r2, err := m.Reserve(c2, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Settle(c2, r2, u, 0)
	if _, err := m.Reserve(call("c", set), 0); kindOf(err) != ir.ErrQuotaExhausted {
		t.Fatalf("token budget not enforced: %v", err)
	}
}

func TestDayRollover(t *testing.T) {
	m, _, clk := setup(nil)
	set := testkit.Settings{"key_daily_tokens": 10}
	c := call("a", set)
	r, _ := m.Reserve(c, 0)
	_ = m.Settle(c, r, ir.Usage{Output: 50}, 0)
	if _, err := m.Reserve(call("b", set), 0); err == nil {
		t.Fatal("expected exhausted")
	}
	clk.Advance(9 * time.Hour) // past midnight UTC
	if _, err := m.Reserve(call("c", set), 0); err != nil {
		t.Fatalf("new day still exhausted: %v", err)
	}
}

func TestTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("no tzdata")
	}
	start, reset := bounds(daily, t0, loc) // 23:30 in Shanghai
	if start.Day() != 5 || !reset.Equal(time.Date(2026, 11, 6, 0, 0, 0, 0, loc)) {
		t.Fatal(start, reset)
	}
}

// The daily window follows the site time zone: spend at 17:00 UTC on the
// 4th is outside the UTC day of t0 but inside Shanghai's (01:00 on the 5th).
func TestSiteTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("no tzdata")
	}
	set := testkit.Settings{"key_daily_micro": 100}
	usage := &spent{cost: map[string]int64{"key:k1": 101}, at: time.Date(2026, 11, 4, 17, 0, 0, 0, time.UTC)}
	m, _, _ := setup(nil)
	m.d.Usage = usage
	if _, err := m.Reserve(call("a", set), 0); err != nil {
		t.Fatalf("UTC day: %v", err)
	}
	m, _, _ = setup(nil)
	m.d.Usage = usage
	m.d.Timezone = func(context.Context) *time.Location { return loc }
	if _, err := m.Reserve(call("b", set), 0); kindOf(err) != ir.ErrQuotaExhausted {
		t.Fatalf("Shanghai day: %v, want exhausted", err)
	}
}

func TestHeaders(t *testing.T) {
	m, _, _ := setup(map[string]int64{"key:k1": 250})
	c := call("a", testkit.Settings{"key_daily_micro": 1000, "key_monthly_micro": 10000})
	if _, err := m.Reserve(c, 100); err != nil {
		t.Fatal(err)
	}
	h := m.Headers(c)
	if got := h.Get(DefaultUsageHeader); got != "25.0" {
		t.Fatalf("percent = %q", got)
	}
	c2 := call("b", testkit.Settings{"key_daily_micro": 1000, "usage_header": "x-codex-primary-used-percent", "reset_header": "x-reset"})
	_, _ = m.Reserve(c2, 0)
	h = m.Headers(c2)
	if h.Get("x-codex-primary-used-percent") != "35.0" /* a still holds 100 */ || h.Get(DefaultUsageHeader) != "" || h.Get("x-reset") != "30600" {
		t.Fatalf("headers %v", h)
	}
	// No budget configured → no header.
	c3 := call("c", nil)
	_, _ = m.Reserve(c3, 0)
	if m.Headers(c3) != nil {
		t.Fatal("header without budget")
	}
}

func TestAnonymousCallPasses(t *testing.T) {
	m, _, _ := setup(nil)
	c := testkit.NewCall(ID, testkit.CallOpts{Settings: testkit.Settings{"key_daily_micro": 1}})
	r, err := m.Reserve(c, 100)
	if err != nil || r == nil {
		t.Fatal(r, err)
	}
	if err := m.Settle(c, r, ir.Usage{}, 0); err != nil {
		t.Fatal(err)
	}
}

// From NoticeFrom percent up, a reservation raises a budget notice naming
// the subject, the window and the period; below it, none.
func TestBudgetNotice(t *testing.T) {
	var got []plugin.Notice
	m, _, _ := setup(map[string]int64{"key:k1": 900, "user:u1": 100})
	m.d.Notify = func(_ context.Context, n plugin.Notice) { got = append(got, n) }
	set := testkit.Settings{"key_monthly_micro": 1000, "user_total_micro": 1000}
	if _, err := m.Reserve(call("r1", set), 0); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("%+v", got)
	}
	n := got[0]
	if n.Event != "budget_reached" || n.Object != (plugin.NoticeObject{Type: "key", ID: "k1"}) || n.Detail["percent"] != "90" ||
		n.Detail["window"] != "monthly" || n.Detail["kind"] != "spend" || n.Key != "spend:monthly:2026-11-01" {
		t.Fatalf("%+v", n)
	}
}
