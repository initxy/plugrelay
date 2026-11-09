package ratelimit

import (
	"fmt"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
)

// t0 sits exactly on a minute boundary.
var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func newCall(id string, set testkit.Settings, caller *plugin.Caller) *plugin.Call {
	return testkit.NewCall(ID, testkit.CallOpts{ID: id, Settings: set, Caller: caller})
}

var alice = &plugin.Caller{KeyID: "k1", UserID: "u1", GroupID: "g1"}

func TestRPM(t *testing.T) {
	clk := testkit.NewClock(t0)
	l := New(testkit.NewState(clk.Now), WithClock(clk.Now))
	set := testkit.Settings{"rpm": 3}
	for i := range 3 {
		if r, err := l.OnRequest(newCall(fmt.Sprint(i), set, alice)); r != nil || err != nil {
			t.Fatalf("request %d limited: %v %v", i, r, err)
		}
	}
	r, _ := l.OnRequest(newCall("x", set, alice))
	if r == nil || r.Error.Kind != ir.ErrRateLimited || r.Error.RetryAfter != time.Minute {
		t.Fatalf("want rate_limited with retry 60s, got %+v", r)
	}
	// Another key is counted separately.
	if r, _ := l.OnRequest(newCall("y", set, &plugin.Caller{KeyID: "k2"})); r != nil {
		t.Fatal("other key limited")
	}
	// Halfway into the next minute the previous bucket counts half: 3*0.5=1 → 2 more allowed.
	clk.Advance(90 * time.Second)
	allowed := 0
	for i := range 5 {
		if r, _ := l.OnRequest(newCall(fmt.Sprint("n", i), set, alice)); r == nil {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("allowed %d after window slid, want 2", allowed)
	}
	// Two full minutes later everything has drained.
	clk.Advance(2 * time.Minute)
	if r, _ := l.OnRequest(newCall("z", set, alice)); r != nil {
		t.Fatal("still limited after the window drained")
	}
}

func TestTPMCountedAtFinish(t *testing.T) {
	clk := testkit.NewClock(t0)
	l := New(testkit.NewState(clk.Now), WithClock(clk.Now))
	set := testkit.Settings{"tpm": 1000}
	c := newCall("a", set, alice)
	if r, _ := l.OnRequest(c); r != nil {
		t.Fatal("first request limited")
	}
	l.OnFinish(c, &plugin.Result{Usage: ir.Usage{Input: 200, CacheRead: 700, Output: 150}})
	r, _ := l.OnRequest(newCall("b", set, alice))
	if r == nil || r.Error.Kind != ir.ErrRateLimited {
		t.Fatalf("expected tpm limit, got %+v", r)
	}
}

func TestConcurrency(t *testing.T) {
	clk := testkit.NewClock(t0)
	st := testkit.NewState(clk.Now)
	l := New(st, WithClock(clk.Now))
	set := testkit.Settings{"concurrency": 2, "rpm": 100}
	a, b := newCall("a", set, alice), newCall("b", set, alice)
	for _, c := range []*plugin.Call{a, b} {
		if r, _ := l.OnRequest(c); r != nil {
			t.Fatal("limited early")
		}
	}
	third := newCall("c", set, alice)
	if r, _ := l.OnRequest(third); r == nil || r.Error.Kind != ir.ErrRateLimited {
		t.Fatal("third request not limited")
	}
	l.OnFinish(third, &plugin.Result{}) // limited request holds no slot
	if st.Held("rl:conc:key:k1") != 2 {
		t.Fatalf("held %d", st.Held("rl:conc:key:k1"))
	}
	l.OnFinish(a, &plugin.Result{})
	if r, _ := l.OnRequest(newCall("d", set, alice)); r != nil {
		t.Fatal("slot not released")
	}
	// The refused request's rpm count was rolled back: a, b, d counted.
	v, _, _ := st.Get(t.Context(), key("rpm", "key:k1", t0.Unix()/60))
	if string(v) != "3" {
		t.Fatalf("rpm counter = %s, want 3", v)
	}
}

func TestSubject(t *testing.T) {
	tests := []struct {
		setting string
		caller  *plugin.Caller
		want    string
	}{
		{"", alice, "key:k1"},
		{"user", alice, "user:u1"},
		{"group", alice, "group:g1"},
		{"group", &plugin.Caller{KeyID: "k9"}, "key:k9"},
		{"key", nil, "ip:1.2.3.4"},
	}
	for _, tt := range tests {
		c := testkit.NewCall(ID, testkit.CallOpts{Caller: tt.caller, ClientIP: "1.2.3.4", Settings: testkit.Settings{"subject": tt.setting}})
		if got := subject(c); got != tt.want {
			t.Errorf("%s: got %s want %s", tt.setting, got, tt.want)
		}
	}
}

func TestUnlimitedTouchesNothing(t *testing.T) {
	l := New(testkit.NewState(nil))
	c := newCall("a", nil, alice)
	if r, err := l.OnRequest(c); r != nil || err != nil {
		t.Fatal(r, err)
	}
	l.OnFinish(c, &plugin.Result{Usage: ir.Usage{Input: 5}})
}
