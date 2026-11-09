package router

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

var ctx = context.Background()

func ep(id, proto string) plugin.Endpoint { return plugin.Endpoint{ID: id, Protocol: proto} }

func shared(ids ...string) []Cred {
	var out []Cred
	for _, id := range ids {
		out = append(out, Cred{ID: id, Kind: KindShared})
	}
	return out
}

func baseTable() *Table {
	return &Table{
		Models: map[string]Model{
			"m": {Name: "m", Enabled: true, Routes: []Route{
				{UpstreamID: "a", UpstreamModel: "m-a", Priority: 10, Weight: 3},
				{UpstreamID: "b", UpstreamModel: "m-b", Priority: 10, Weight: 1},
				{UpstreamID: "c", UpstreamModel: "m-c", Priority: 5, Groups: []string{"vip"}},
			}},
		},
		Upstreams: map[string]Upstream{
			"a": {ID: "a", Enabled: true, Endpoints: []plugin.Endpoint{ep("a-chat", "openai_chat"), ep("a-msg", "anthropic")}, Credentials: shared("a1")},
			"b": {ID: "b", Enabled: true, Endpoints: []plugin.Endpoint{ep("b-chat", "openai_chat")}, Credentials: shared("b1")},
			"c": {ID: "c", Enabled: true, Endpoints: []plugin.Endpoint{ep("c-msg", "anthropic")}, Credentials: shared("c1")},
		},
	}
}

func newRouter(t *testing.T, tb *Table) (*Router, *clock, *memState) {
	t.Helper()
	clk := newClock()
	st := newMemState(clk)
	r := New(st, Options{Secret: []byte("k"), Now: clk.Now, Rand: rand.New(rand.NewPCG(1, 2))})
	r.SetTable(tb)
	return r, clk, st
}

func ups(p *Plan) []string {
	var out []string
	for _, c := range p.Candidates {
		out = append(out, c.UpstreamID+"/"+c.EndpointID+"/"+c.CredentialID)
	}
	return out
}

func mustPlan(t *testing.T, r *Router, q Query) *Plan {
	t.Helper()
	p, err := r.Plan(ctx, q)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p
}

func TestGroupFilterAndPriority(t *testing.T) {
	r, _, _ := newRouter(t, baseTable())
	p := mustPlan(t, r, Query{Model: "m", ClientProtocol: "anthropic", Caller: plugin.Caller{UserID: "u"}})
	for _, c := range p.Candidates {
		if c.UpstreamID == "c" {
			t.Fatalf("non-vip caller got vip route: %v", ups(p))
		}
	}
	p = mustPlan(t, r, Query{Model: "m", ClientProtocol: "anthropic", Caller: plugin.Caller{UserID: "u", GroupID: "vip"}})
	if last := p.Candidates[len(p.Candidates)-1]; last.UpstreamID != "c" {
		t.Fatalf("lower priority route not last: %v", ups(p))
	}
}

func TestSameProtocolPreferred(t *testing.T) {
	tb := baseTable()
	tb.Models["m"] = Model{Name: "m", Enabled: true, Routes: []Route{{UpstreamID: "a", UpstreamModel: "x"}}}
	tb.Upstreams["a"] = Upstream{ID: "a", Enabled: true, Endpoints: []plugin.Endpoint{ep("a-chat", "openai_chat"), ep("a-msg", "anthropic")}, Credentials: shared("a1", "a2")}
	r, _, _ := newRouter(t, tb)
	p := mustPlan(t, r, Query{Model: "m", ClientProtocol: "anthropic"})
	got := ups(p)
	if len(got) != 4 || !p.Candidates[0].SameProtocol || !p.Candidates[1].SameProtocol || p.Candidates[2].SameProtocol {
		t.Fatalf("same-protocol endpoint must come first for every credential: %v", got)
	}
}

// Routes of one priority are tried by how well they fit before weights
// apply: a lossy cross-protocol route never precedes a same-protocol one.
func TestRoutesOrderedByFit(t *testing.T) {
	r, _, _ := newRouter(t, baseTable())
	assess := func(e plugin.Endpoint) (ir.Fidelity, []string) {
		if e.Protocol == "anthropic" {
			return ir.Lossless, nil
		}
		return ir.Lossy, nil
	}
	for i := 0; i < 20; i++ {
		p := mustPlan(t, r, Query{Model: "m", ClientProtocol: "anthropic", Assess: assess})
		if c := p.Candidates[0]; c.UpstreamID != "a" || c.EndpointID != "a-msg" {
			t.Fatalf("best-fitting route not first: %v", ups(p))
		}
	}
}

func TestCapabilityFiltering(t *testing.T) {
	r, _, _ := newRouter(t, baseTable())
	assess := func(e plugin.Endpoint) (ir.Fidelity, []string) {
		switch e.Protocol {
		case "anthropic":
			return ir.Lossless, nil
		default:
			return ir.Impossible, []string{"server_tools"}
		}
	}
	p := mustPlan(t, r, Query{Model: "m", ClientProtocol: "openai_chat", Assess: assess})
	for _, c := range p.Candidates {
		if c.Protocol != "anthropic" {
			t.Fatalf("impossible endpoint kept: %v", ups(p))
		}
	}

	_, err := r.Plan(ctx, Query{Model: "m", Assess: func(plugin.Endpoint) (ir.Fidelity, []string) { return ir.Impossible, []string{"images"} }})
	var pe *PlanError
	if !errors.As(err, &pe) || pe.Kind != ErrImpossible || !slices.Equal(pe.Missing, []string{"images"}) {
		t.Fatalf("want impossible error with missing list, got %v", err)
	}

	disc := func(plugin.Endpoint) (ir.Fidelity, []string) { return ir.Discouraged, []string{"opaque_state"} }
	_, err = r.Plan(ctx, Query{Model: "m", Assess: disc})
	if !errors.As(err, &pe) || pe.Kind != ErrImpossible || !pe.Discouraged {
		t.Fatalf("discouraged must be excluded by default: %v", err)
	}
	p = mustPlan(t, r, Query{Model: "m", Assess: disc, AllowDiscouraged: true})
	if len(p.Candidates) == 0 || p.Candidates[0].Fidelity != ir.Discouraged {
		t.Fatal("discouraged endpoints must be usable when allowed")
	}
}

func TestPersonalCredentialIsolation(t *testing.T) {
	tb := &Table{
		Models: map[string]Model{"m": {Name: "m", Enabled: true, Routes: []Route{{UpstreamID: "p", UpstreamModel: "x"}}}},
		Upstreams: map[string]Upstream{"p": {ID: "p", Enabled: true, Endpoints: []plugin.Endpoint{ep("e", "anthropic")}, Credentials: []Cred{
			{ID: "alice", Kind: KindPersonal, OwnerID: "alice"},
			{ID: "orphan", Kind: KindPersonal},
			{ID: "pool", Kind: KindShared},
		}}},
	}
	r, _, _ := newRouter(t, tb)
	p := mustPlan(t, r, Query{Model: "m", Caller: plugin.Caller{UserID: "bob"}})
	if got := ups(p); !slices.Equal(got, []string{"p/e/pool"}) {
		t.Fatalf("bob saw someone else's personal credential: %v", got)
	}
	p = mustPlan(t, r, Query{Model: "m", Caller: plugin.Caller{UserID: "alice"}, PersonalOnly: true})
	if got := ups(p); !slices.Equal(got, []string{"p/e/alice"}) {
		t.Fatalf("personal-only: %v", got)
	}
	_, err := r.Plan(ctx, Query{Model: "m", Caller: plugin.Caller{UserID: "bob"}, PersonalOnly: true})
	if !errors.Is(err, &PlanError{Kind: ErrNoRoute}) {
		t.Fatalf("want no route, got %v", err)
	}
	// Even a cooling personal credential of another user must not leak into
	// the error (no cooling hint).
	tb.Upstreams["p"].Credentials[2].Status = StatusDisabled
	_, err = r.Plan(ctx, Query{Model: "m", Caller: plugin.Caller{UserID: "bob"}})
	if errors.Is(err, &PlanError{Kind: ErrCooling}) {
		t.Fatalf("unexpected cooling: %v", err)
	}
}

func TestWeightedSplitOnlyForNewSessions(t *testing.T) {
	r, _, _ := newRouter(t, baseTable())
	firstA := 0
	const n = 4000
	for i := range n {
		p := mustPlan(t, r, Query{Model: "m", ClientProtocol: "openai_chat", Caller: plugin.Caller{UserID: "u" + string(rune(i))}})
		if p.Candidates[0].UpstreamID == "a" {
			firstA++
		}
	}
	frac := float64(firstA) / n
	if frac < 0.71 || frac > 0.79 {
		t.Fatalf("weight 3:1 split gave %.3f for a", frac)
	}

	// An existing (bound) session always starts on its binding.
	for range 50 {
		_ = r.Bind(ctx, Turn{SessionKey: "s", Candidate: plugin.Candidate{UpstreamID: "b", EndpointID: "b-chat", CredentialID: "b1", UpstreamModel: "m-b"}})
		p := mustPlan(t, r, Query{Model: "m", ClientProtocol: "openai_chat", SessionKey: "s"})
		if p.Candidates[0].UpstreamID != "b" || !p.Candidates[0].Sticky {
			t.Fatalf("bound session moved: %v", ups(p))
		}
		// Remaining same-priority routes in weight order, not shuffled.
		if p.Candidates[1].UpstreamID != "a" {
			t.Fatalf("existing session order: %v", ups(p))
		}
	}
}

func TestStickinessWindow(t *testing.T) {
	r, clk, _ := newRouter(t, baseTable())
	b := plugin.Candidate{UpstreamID: "b", EndpointID: "b-chat", CredentialID: "b1", UpstreamModel: "m-b"}
	q := Query{Model: "m", ClientProtocol: "openai_chat", SessionKey: "s"}

	// No cache use: binding recorded but not honored.
	_ = r.Observe(ctx, Turn{SessionKey: "s", Candidate: b, SentSessionID: "up-1", Usage: ir.Usage{Input: 10}})
	p := mustPlan(t, r, q)
	if p.Binding == nil || p.Binding.Valid(clk.Now()) || p.Binding.SentSessionID != "up-1" {
		t.Fatalf("binding: %+v", p.Binding)
	}
	for _, c := range p.Candidates {
		if c.Sticky {
			t.Fatal("non-cached turn must not pin")
		}
	}

	_ = r.Observe(ctx, Turn{SessionKey: "s", Candidate: b, Usage: ir.Usage{CacheRead: 100}})
	clk.Advance(4 * time.Minute)
	if p := mustPlan(t, r, q); p.Candidates[0].UpstreamID != "b" || !p.Candidates[0].Sticky {
		t.Fatalf("within 5m must stick: %v", ups(p))
	}
	clk.Advance(2 * time.Minute)
	for _, c := range mustPlan(t, r, q).Candidates {
		if c.Sticky {
			t.Fatal("expired 5m cache still sticky")
		}
	}

	_ = r.Observe(ctx, Turn{SessionKey: "s", Candidate: b, Usage: ir.Usage{CacheWrite1h: 100}})
	clk.Advance(50 * time.Minute)
	if p := mustPlan(t, r, q); !p.Candidates[0].Sticky {
		t.Fatal("1h cache must stick for an hour")
	}

	// Sticky beats priority.
	tb := baseTable()
	tb.Models["m"].Routes[1].Priority = 1
	r.SetTable(tb)
	if p := mustPlan(t, r, q); p.Candidates[0].UpstreamID != "b" {
		t.Fatalf("binding must win over priority: %v", ups(p))
	}

	// A binding to a credential that is no longer usable falls through.
	_ = r.Report(ctx, b, plugin.Verdict{Cause: plugin.CauseQuota, Credential: plugin.CredCooldown}, "m-b")
	if p := mustPlan(t, r, q); p.Candidates[0].UpstreamID == "b" {
		t.Fatalf("cooling bound credential used: %v", ups(p))
	}
}

func TestTurnToken(t *testing.T) {
	r, clk, _ := newRouter(t, baseTable())
	b := plugin.Candidate{UpstreamID: "b", EndpointID: "b-chat", CredentialID: "b1", UpstreamModel: "m-b"}
	tok := r.IssueTurnToken(b)
	if k, ok := r.ParseTurnToken(tok); !ok || k != Key(b) {
		t.Fatalf("parse: %q %v", k, ok)
	}
	body, sig, _ := strings.Cut(tok, ".")
	for _, part := range []struct {
		s     string
		build func(string) string
	}{
		{body, func(m string) string { return m + "." + sig }},
		{sig, func(m string) string { return body + "." + m }},
	} {
		raw, err := base64.RawURLEncoding.DecodeString(part.s)
		if err != nil {
			t.Fatal(err)
		}
		for i := range raw {
			mut := bytes.Clone(raw)
			mut[i] ^= 0x01
			if _, ok := r.ParseTurnToken(part.build(base64.RawURLEncoding.EncodeToString(mut))); ok {
				t.Fatalf("tampered byte %d accepted", i)
			}
		}
	}
	other := New(newMemState(clk), Options{Secret: []byte("other"), Now: clk.Now})
	if _, ok := other.ParseTurnToken(tok); ok {
		t.Fatal("token from another key accepted")
	}
	for range 20 {
		p := mustPlan(t, r, Query{Model: "m", ClientProtocol: "openai_chat", TurnToken: tok})
		if Key(p.Candidates[0]) != Key(b) || !p.Candidates[0].Sticky {
			t.Fatalf("token must pin: %v", ups(p))
		}
	}
	clk.Advance(7 * time.Hour)
	if _, ok := r.ParseTurnToken(tok); ok {
		t.Fatal("expired token accepted")
	}
}

func TestUserRecentCredential(t *testing.T) {
	tb := baseTable()
	tb.Upstreams["a"] = Upstream{ID: "a", Enabled: true, Endpoints: []plugin.Endpoint{ep("a-chat", "openai_chat")}, Credentials: shared("a1", "a2", "a3", "a4")}
	r, _, _ := newRouter(t, tb)
	u := plugin.Caller{UserID: "carol"}
	_ = r.Observe(ctx, Turn{Caller: u, Candidate: plugin.Candidate{UpstreamID: "b", EndpointID: "b-chat", CredentialID: "b1", UpstreamModel: "m-b"}})
	for i := range 30 {
		p := mustPlan(t, r, Query{Model: "m", ClientProtocol: "openai_chat", Caller: u, SessionKey: "new" + string(rune(i))})
		if p.Candidates[0].CredentialID != "b1" {
			t.Fatalf("new session of carol must reuse b1: %v", ups(p))
		}
	}
	_ = r.Observe(ctx, Turn{Caller: u, Candidate: plugin.Candidate{UpstreamID: "a", EndpointID: "a-chat", CredentialID: "a3", UpstreamModel: "m-a"}})
	p := mustPlan(t, r, Query{Model: "m", ClientProtocol: "openai_chat", Caller: u})
	var aCreds []string
	for _, c := range p.Candidates {
		if c.UpstreamID == "a" {
			aCreds = append(aCreds, c.CredentialID)
		}
	}
	if aCreds[0] != "a3" {
		t.Fatalf("within upstream a, carol's credential first: %v", aCreds)
	}
}

func TestCooldownAndQuarantine(t *testing.T) {
	tb := baseTable()
	tb.Models["m"] = Model{Name: "m", Enabled: true, Routes: []Route{{UpstreamID: "a", UpstreamModel: "m-a"}}}
	tb.Upstreams["a"] = Upstream{ID: "a", Enabled: true, Endpoints: []plugin.Endpoint{ep("e", "x")}, Credentials: shared("a1", "a2")}
	var events []CredEvent
	clk := newClock()
	r := New(newMemState(clk), Options{Now: clk.Now, OnCredential: func(e CredEvent) { events = append(events, e) }})
	r.SetTable(tb)
	a1 := plugin.Candidate{UpstreamID: "a", EndpointID: "e", CredentialID: "a1", UpstreamModel: "m-a"}
	a2 := a1
	a2.CredentialID = "a2"
	q := Query{Model: "m"}

	// Not-the-credential's-fault causes never change it.
	for _, c := range []plugin.Cause{plugin.CauseRequest, plugin.CauseTransient, plugin.CauseContent} {
		_ = r.Report(ctx, a1, plugin.Verdict{Cause: c, Credential: plugin.CredQuarantine}, "m-a")
	}
	if len(mustPlan(t, r, q).Candidates) != 2 || len(events) != 0 {
		t.Fatal("hard rule 17 violated")
	}

	// Model-scoped cooldown affects only that model.
	_ = r.Report(ctx, a1, plugin.Verdict{Cause: plugin.CauseRateLimit, Credential: plugin.CredCooldown, ModelScope: true, Until: clk.Now().Add(time.Minute)}, "other")
	if len(mustPlan(t, r, q).Candidates) != 2 {
		t.Fatal("cooldown of another model leaked")
	}
	_ = r.Report(ctx, a1, plugin.Verdict{Cause: plugin.CauseRateLimit, Credential: plugin.CredCooldown, ModelScope: true, Until: clk.Now().Add(time.Minute)}, "m-a")
	if got := ups(mustPlan(t, r, q)); !slices.Equal(got, []string{"a/e/a2"}) {
		t.Fatalf("model cooldown: %v", got)
	}

	_ = r.Report(ctx, a2, plugin.Verdict{Cause: plugin.CauseQuota, Credential: plugin.CredCooldown}, "m-a")
	_, err := r.Plan(ctx, q)
	var pe *PlanError
	if !errors.As(err, &pe) || pe.Kind != ErrCooling || !pe.RetryAt.Equal(clk.Now().Add(30*time.Second)) {
		t.Fatalf("want cooling until default backoff, got %v", err)
	}
	clk.Advance(2 * time.Minute)
	if len(mustPlan(t, r, q).Candidates) != 2 {
		t.Fatal("cooldown must auto-recover")
	}
	// Default backoff grows with repeated strikes.
	_ = r.Report(ctx, a2, plugin.Verdict{Cause: plugin.CauseRateLimit, Credential: plugin.CredCooldown}, "m-a")
	if m, _ := r.getMark(ctx, kCool("a", "a2")); !m.Until.Equal(clk.Now().Add(time.Minute)) {
		t.Fatalf("second strike backoff: %v", m.Until.Sub(clk.Now()))
	}

	_ = r.Report(ctx, a1, plugin.Verdict{Cause: plugin.CauseBanned, Credential: plugin.CredQuarantine}, "m-a")
	clk.Advance(1000 * time.Hour)
	if got := ups(mustPlan(t, r, q)); !slices.Equal(got, []string{"a/e/a2"}) {
		t.Fatalf("quarantine must never auto-recover: %v", got)
	}
	live := r.Live(ctx)
	if c := live.Upstreams[0].Credentials[0]; c.Status != StatusQuarantined || c.Reason != "banned" {
		t.Fatalf("live: %+v", c)
	}
	if last := events[len(events)-1]; last.Status != StatusQuarantined || last.CredentialID != "a1" {
		t.Fatalf("event: %+v", last)
	}
	_ = r.Restore(ctx, "a", "a1")
	if len(mustPlan(t, r, q).Candidates) != 2 {
		t.Fatal("restore")
	}
}

func TestBreaker(t *testing.T) {
	tb := baseTable()
	tb.Models["m"] = Model{Name: "m", Enabled: true, Routes: []Route{{UpstreamID: "a", UpstreamModel: "m-a"}}}
	clk := newClock()
	r := New(newMemState(clk), Options{Now: clk.Now, BreakerThreshold: 3})
	r.SetTable(tb)
	a1 := plugin.Candidate{UpstreamID: "a", EndpointID: "a-chat", CredentialID: "a1", UpstreamModel: "m-a"}
	q := Query{Model: "m"}
	fail := plugin.Verdict{Cause: plugin.CauseTransient, Retry: plugin.RetryNext}

	_ = r.Report(ctx, a1, fail, "m-a")
	_ = r.Report(ctx, a1, fail, "m-a")
	_ = r.Report(ctx, a1, plugin.Verdict{}, "m-a") // success resets the streak
	_ = r.Report(ctx, a1, fail, "m-a")
	_ = r.Report(ctx, a1, fail, "m-a")
	// Rate limits are the credential's business, not the upstream's.
	_ = r.Report(ctx, a1, plugin.Verdict{Cause: plugin.CauseRateLimit}, "m-a")
	mustPlan(t, r, q)
	_ = r.Report(ctx, a1, fail, "m-a")
	_, err := r.Plan(ctx, q)
	if !errors.Is(err, &PlanError{Kind: ErrCooling}) {
		t.Fatalf("breaker should be open: %v", err)
	}
	if r.Live(ctx).Upstreams[0].Breaker != BreakerOpen {
		t.Fatal("live breaker")
	}

	clk.Advance(31 * time.Second)
	mustPlan(t, r, q) // the probe
	if _, err := r.Plan(ctx, q); err == nil {
		t.Fatal("half-open must admit exactly one probe")
	}
	_ = r.Report(ctx, a1, fail, "m-a") // probe failed: open again
	if r.Live(ctx).Upstreams[0].Breaker != BreakerOpen {
		t.Fatal("failed probe must reopen")
	}
	clk.Advance(31 * time.Second)
	mustPlan(t, r, q)
	_ = r.Report(ctx, a1, plugin.Verdict{}, "m-a")
	if r.Live(ctx).Upstreams[0].Breaker != BreakerClosed {
		t.Fatal("successful probe must close")
	}
	mustPlan(t, r, q)
	mustPlan(t, r, q)
}

func TestConcurrencyCaps(t *testing.T) {
	tb := &Table{
		Models: map[string]Model{"m": {Name: "m", Enabled: true, Routes: []Route{{UpstreamID: "a", UpstreamModel: "x"}}}},
		Upstreams: map[string]Upstream{"a": {ID: "a", Enabled: true, Endpoints: []plugin.Endpoint{ep("e", "x")}, MaxActiveCredentials: 2, Credentials: []Cred{
			{ID: "c1", Kind: KindShared, MaxConcurrency: 2},
			{ID: "c2", Kind: KindShared, MaxConcurrency: 1},
			{ID: "c3", Kind: KindShared},
		}}},
	}
	r, _, _ := newRouter(t, tb)
	cand := func(id string) plugin.Candidate {
		return plugin.Candidate{UpstreamID: "a", EndpointID: "e", CredentialID: id, UpstreamModel: "x"}
	}
	q := Query{Model: "m"}
	if n := len(mustPlan(t, r, q).Candidates); n != 2 {
		t.Fatalf("max active credentials: %d candidates", n)
	}
	rel1, ok, _ := r.Acquire(ctx, cand("c2"))
	if !ok {
		t.Fatal("acquire c2")
	}
	if _, ok, _ := r.Acquire(ctx, cand("c2")); ok {
		t.Fatal("c2 over its cap")
	}
	p := mustPlan(t, r, q)
	for _, c := range p.Candidates {
		if c.CredentialID == "c2" {
			t.Fatal("full credential planned")
		}
	}
	relA, _, _ := r.Acquire(ctx, cand("c1"))
	// c1 active, c2 full and active → upstream at 2 active; c3 must wait.
	p = mustPlan(t, r, q)
	if got := ups(p); !slices.Equal(got, []string{"a/e/c1"}) || p.Candidates[0].InFlight != 1 {
		t.Fatalf("active cap: %v", got)
	}
	relB, _, _ := r.Acquire(ctx, cand("c1"))
	_, err := r.Plan(ctx, q)
	if !errors.Is(err, &PlanError{Kind: ErrBusy}) {
		t.Fatalf("want busy, got %v", err)
	}
	rel1()
	rel1()
	relA()
	relB()
	if n := r.Live(ctx).Upstreams[0].Credentials[0].InFlight; n != 0 {
		t.Fatalf("inflight leak: %d", n)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	got := 0
	for range 20 {
		wg.Go(func() {
			if _, ok, _ := r.Acquire(ctx, cand("c1")); ok {
				mu.Lock()
				got++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if got != 2 {
		t.Fatalf("concurrent acquire granted %d of cap 2", got)
	}
}

func TestPreset(t *testing.T) {
	tb := baseTable()
	tb.Models["m2"] = Model{Name: "m2", Enabled: true, Routes: []Route{{UpstreamID: "c", UpstreamModel: "m2-c", Priority: 1}}}
	tb.Models["off"] = Model{Name: "off", Routes: tb.Models["m"].Routes}
	tb.Presets = map[string]Preset{
		"auto":  {Name: "auto", Models: []string{"m2", "off", "nope", "m"}, Strategy: "cheapest"},
		"empty": {Name: "empty", Models: []string{"off", "nope"}},
	}
	r, _, _ := newRouter(t, tb)
	p := mustPlan(t, r, Query{Model: "auto"})
	if p.Candidates[0].UpstreamID != "c" || p.Served(p.Candidates[0]) != (Served{Model: "m2", Preset: "auto"}) {
		t.Fatalf("first level: %v %+v", ups(p), p.Served(p.Candidates[0]))
	}
	last := p.Candidates[len(p.Candidates)-1]
	if s := p.Served(last); s.Preset != "auto" || s.Model != "m" {
		t.Fatalf("second level: %+v", s)
	}
	if p.Strategy != "cheapest" {
		t.Fatalf("strategy %q", p.Strategy)
	}
	// A model the caller may not use is skipped, as if it had no route.
	p = mustPlan(t, r, Query{Model: "auto", Allowed: func(m string) bool { return m != "m2" }})
	if s := p.Served(p.Candidates[0]); s.Model != "m" {
		t.Fatalf("disallowed model planned: %+v", s)
	}
	// A model is never swapped for another: its own routes or nothing.
	if p := mustPlan(t, r, Query{Model: "m"}); p.Strategy != "" || p.Served(p.Candidates[0]) != (Served{Model: "m"}) {
		t.Fatalf("model plan: %+v", p.Served(p.Candidates[0]))
	}
	for _, m := range []string{"empty", "off", "nope"} {
		if _, err := r.Plan(ctx, Query{Model: m}); !errors.Is(err, &PlanError{Kind: ErrNoRoute}) {
			t.Fatalf("%s: %v", m, err)
		}
	}
}

func TestExcludeAndErrors(t *testing.T) {
	r, _, _ := newRouter(t, baseTable())
	q := Query{Model: "m", Exclude: map[string]bool{}}
	for {
		p, err := r.Plan(ctx, q)
		if err != nil {
			if !errors.Is(err, &PlanError{Kind: ErrExhausted}) {
				t.Fatalf("want exhausted, got %v", err)
			}
			break
		}
		if q.Exclude[Key(p.Candidates[0])] {
			t.Fatal("excluded candidate returned")
		}
		q.Exclude[Key(p.Candidates[0])] = true
	}
	if len(q.Exclude) != 3 {
		t.Fatalf("tried %d candidates", len(q.Exclude))
	}

	tb := baseTable()
	for id, u := range tb.Upstreams {
		u.Enabled = false
		tb.Upstreams[id] = u
	}
	r.SetTable(tb)
	if _, err := r.Plan(ctx, Query{Model: "m"}); !errors.Is(err, &PlanError{Kind: ErrNoRoute}) {
		t.Fatalf("disabled upstreams: %v", err)
	}
}

func TestStoreStatus(t *testing.T) {
	tb := baseTable()
	tb.Models["m"] = Model{Name: "m", Enabled: true, Routes: []Route{{UpstreamID: "a", UpstreamModel: "m-a"}}}
	r, clk, _ := newRouter(t, tb)
	u := tb.Upstreams["a"]
	u.Credentials = []Cred{{ID: "a1", Kind: KindShared, Status: StatusCooling, StatusUntil: clk.Now().Add(time.Minute)}}
	tb.Upstreams["a"] = u
	_, err := r.Plan(ctx, Query{Model: "m"})
	if !errors.Is(err, &PlanError{Kind: ErrCooling}) {
		t.Fatalf("stored cooling: %v", err)
	}
	clk.Advance(time.Minute)
	mustPlan(t, r, Query{Model: "m"})
	if s := r.Live(ctx).Upstreams[0].Credentials[0].Status; s != StatusActive {
		t.Fatalf("expired stored cooldown shown as %s", s)
	}
}

// A key-optional upstream with no credentials routes keyless; any other
// upstream without a usable credential has no route.
func TestKeylessUpstream(t *testing.T) {
	tb := &Table{
		Models: map[string]Model{"m": {Name: "m", Enabled: true, Routes: []Route{{UpstreamID: "local", UpstreamModel: "x"}}}},
		Upstreams: map[string]Upstream{"local": {ID: "local", Enabled: true, Endpoints: []plugin.Endpoint{ep("e", "openai_chat")},
			KeyOptional: true}},
	}
	var events []CredEvent
	clk := newClock()
	r := New(newMemState(clk), Options{Now: clk.Now, OnCredential: func(e CredEvent) { events = append(events, e) }})
	r.SetTable(tb)
	q := Query{Model: "m", Caller: plugin.Caller{UserID: "bob"}}
	if got := ups(mustPlan(t, r, q)); !slices.Equal(got, []string{"local/e/"}) {
		t.Fatalf("an upstream with no credentials routes keyless: %v", got)
	}
	if _, err := r.Plan(ctx, Query{Model: "m", Caller: plugin.Caller{UserID: "bob"}, PersonalOnly: true}); !errors.Is(err, &PlanError{Kind: ErrNoRoute}) {
		t.Fatalf("keyless is shared, not personal: %v", err)
	}

	// No credential an operator could restore: a ban only cools it down.
	cand := plugin.Candidate{UpstreamID: "local", EndpointID: "e", UpstreamModel: "x"}
	_ = r.Report(ctx, cand, plugin.Verdict{Cause: plugin.CauseBanned, Credential: plugin.CredQuarantine}, "x")
	if _, err := r.Plan(ctx, q); !errors.Is(err, &PlanError{Kind: ErrCooling}) {
		t.Fatalf("want cooling, got %v", err)
	}
	clk.Advance(time.Hour)
	if len(mustPlan(t, r, q).Candidates) != 1 {
		t.Fatal("keyless cooldown must auto-recover")
	}
	if len(events) != 0 {
		t.Fatalf("keyless state is not persisted: %+v", events)
	}

	for name, up := range map[string]Upstream{
		// A cloud upstream whose last key was deleted.
		"key required, no credentials": {},
		"disabled credential":          {KeyOptional: true, Credentials: []Cred{{ID: "k", Kind: KindShared, Status: StatusDisabled}}},
		"only someone else's personal": {KeyOptional: true, Credentials: []Cred{{ID: "a", Kind: KindPersonal, OwnerID: "alice"}}},
	} {
		up.ID, up.Enabled, up.Endpoints = "local", true, []plugin.Endpoint{ep("e", "openai_chat")}
		tb.Upstreams["local"] = up
		r.SetTable(tb)
		if p, err := r.Plan(ctx, q); err == nil {
			t.Errorf("%s: routed %v", name, ups(p))
		}
	}
}
