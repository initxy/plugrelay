package pipeline

import (
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/selector/routing"
)

// 1000 input and 333 output tokens.
const chatUsageResp = `{"id":"c1","object":"chat.completion","model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":333,"total_tokens":1333}}`

type estimateMeter struct {
	fakeMeter
	estimate int64
}

func (m *estimateMeter) Reserve(c *plugin.Call, estimate int64) (*plugin.Reservation, error) {
	m.estimate = estimate
	return m.fakeMeter.Reserve(c, estimate)
}

// The caller is charged the model's list price times its group's
// multiplier, rounded half up; operator cost stays at the upstream rate.
func TestChargeAppliesGroupMultiplier(t *testing.T) {
	list := Price{Input: 2_000_000, Output: 10_000_000}
	cases := []struct {
		name      string
		group     string
		listed    bool
		charge    int64
		mult      float64
		source    string
		rateIn    int64
		rateOut   int64
		estimated int64 // reservation for 100 output tokens and no input estimate
	}{
		// 1000×2.5 + 333×12.5 = 6662.5 → 6663
		{"group multiplier", "vip", true, 6663, 1.25, "model", 2_500_000, 12_500_000, 1250},
		{"no group", "", true, 5330, 1, "model", 2_000_000, 10_000_000, 1000},
		{"unknown group", "gone", true, 5330, 1, "model", 2_000_000, 10_000_000, 1000},
		{"group at 1", "std", true, 5330, 1, "model", 2_000_000, 10_000_000, 1000},
		// No list price: the upstream's cost rate (3/15) × 1.25 =
		// 3750 + 333×18.75 = 9993.75 → 9994
		{"no list price", "vip", false, 9994, 1.25, "upstream", 3_750_000, 18_750_000, 1875},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFake(t, jsonReply(200, chatUsageResp))
			e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}})
			e.caller.GroupID = tc.group
			e.snap.Multiplier = func(g string) float64 { return map[string]float64{"vip": 1.25, "std": 1}[g] }
			e.snap.ModelPrice = func(m string) (Price, bool) { return list, tc.listed && m == "gpt-x" }
			m := &estimateMeter{}
			e.chain.Meter = m
			resp, body := e.call("openai-chat", "/v1/chat/completions",
				`{"model":"gpt-x","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode != 200 {
				t.Fatalf("%d %s", resp.StatusCode, body)
			}
			rec := e.records.last(t)
			if rec.ChargeMicro != tc.charge || m.charged != tc.charge {
				t.Errorf("charge %d, settled %d, want %d", rec.ChargeMicro, m.charged, tc.charge)
			}
			if rec.CostMicro != 1000*3+333*15 {
				t.Errorf("operator cost %d must stay at the upstream rate", rec.CostMicro)
			}
			ch := rec.Attempts[len(rec.Attempts)-1].Charge
			if ch == nil || ch.Multiplier != tc.mult || ch.Source != tc.source || ch.Rates.Input != tc.rateIn || ch.Rates.Output != tc.rateOut {
				t.Errorf("charge detail %+v", ch)
			}
			if m.estimate < tc.estimated {
				t.Errorf("reservation %d, want at least %d (output limit at the charged rate)", m.estimate, tc.estimated)
			}
		})
	}
}

func TestChargeRefundedHasNoDetail(t *testing.T) {
	up := newFake(t, jsonReply(500, `{"error":{"message":"boom","type":"server_error"}}`))
	e := newEnv(t, "gpt-x", []upSpec{{id: "o", protocol: "openai-chat", issuer: "i", fake: up}})
	e.caller.GroupID = "vip"
	e.snap.Multiplier = func(string) float64 { return 2 }
	e.call("openai-chat", "/v1/chat/completions", chatReq)
	rec := e.records.last(t)
	for _, a := range rec.Attempts {
		if a.Charge != nil {
			t.Errorf("failed attempt carries a charge: %+v", a)
		}
	}
	if rec.ChargeMicro != 0 {
		t.Errorf("charged %d", rec.ChargeMicro)
	}
}

func TestPriceScaledRounding(t *testing.T) {
	p := Price{Input: 3, Output: 1_000_001, CacheRead: 1}.scaled(1.1)
	// 3×1.1 = 3.3 → 3; 1_000_001×1.1 = 1_100_001.1 → 1_100_001; 1×1.1 → 1
	if p.Input != 3 || p.Output != 1_100_001 || p.CacheRead != 1 {
		t.Errorf("%+v", p)
	}
	if q := (Price{Input: 5}).scaled(0.5); q.Input != 3 { // 2.5 → 3
		t.Errorf("half up: %+v", q)
	}
}

// strategy ranks by one fixed routing strategy.
type strategy string

func (s strategy) Info() plugin.Info {
	return plugin.Info{ID: "selector.test", Kind: plugin.KindSelector}
}
func (s strategy) Rank(_ *plugin.Call, c []plugin.Candidate) []plugin.Candidate {
	return routing.Rank(string(s), c)
}

// The cheapest strategy sees each candidate's cost rate: the cheaper upstream is
// tried first although the router would put the other one first.
func TestCheapestSeesUpstreamPrices(t *testing.T) {
	dear := newFake(t, jsonReply(200, chatUsageResp))
	cheap := newFake(t, jsonReply(200, chatUsageResp))
	e := newEnv(t, "gpt-x", []upSpec{
		{id: "dear", protocol: "openai-chat", issuer: "i", fake: dear, priority: 1},
		{id: "cheap", protocol: "openai-chat", issuer: "i", fake: cheap},
	})
	e.snap.Price = func(up, _ string) (Price, bool) {
		if up == "cheap" {
			return Price{Input: 1_000_000, Output: 2_000_000}, true
		}
		return Price{Input: 3_000_000, Output: 15_000_000}, true
	}
	e.chain.Selector = strategy(routing.Cheapest)
	for i := range 3 {
		if resp, body := e.call("openai-chat", "/v1/chat/completions", chatReq); resp.StatusCode != 200 {
			t.Fatalf("%d: %d %s", i, resp.StatusCode, body)
		}
	}
	if cheap.calls() != 3 || dear.calls() != 0 {
		t.Fatalf("cheap %d, dear %d", cheap.calls(), dear.calls())
	}
}

// A preset serves its name with one of its models, picked by its strategy;
// a model asked for by name is never swapped. The record keeps the preset
// as the requested model and the served upstream model on the attempt.
func TestPresetPicksModel(t *testing.T) {
	dear := newFake(t, jsonReply(200, chatUsageResp))
	cheap := newFake(t, jsonReply(200, chatUsageResp))
	e := newEnv(t, "gpt-x", []upSpec{
		{id: "dear", protocol: "openai-chat", issuer: "i", fake: dear, model: "big-1"},
		{id: "cheap", protocol: "openai-chat", issuer: "i", fake: cheap, model: "small-1"},
	})
	tb := e.snap.Table
	routes := tb.Models["gpt-x"].Routes
	tb.Models["big"] = router.Model{Name: "big", Enabled: true, Routes: routes[:1]}
	tb.Models["small"] = router.Model{Name: "small", Enabled: true, Routes: routes[1:]}
	e.snap.Price = func(up, _ string) (Price, bool) {
		if up == "cheap" {
			return Price{Input: 1_000_000, Output: 2_000_000}, true
		}
		return Price{Input: 3_000_000, Output: 15_000_000}, true
	}
	e.chain.Pick = func(s string, c []plugin.Candidate) []plugin.Candidate {
		if s == "in_order" {
			return c
		}
		return routing.Rank(s, c)
	}
	// The routing strategy never applies to a preset.
	e.chain.Selector = strategy(routing.Fastest)
	ask := func(model string) {
		t.Helper()
		req, _ := sjson.Set(chatReq, "model", model)
		if resp, body := e.call("openai-chat", "/v1/chat/completions", req); resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", model, resp.StatusCode, body)
		}
	}
	tb.Presets = map[string]router.Preset{"auto-coding": {Name: "auto-coding", Models: []string{"big", "small"}, Strategy: "cheapest"}}
	ask("auto-coding")
	if cheap.calls() != 1 || dear.calls() != 0 {
		t.Fatalf("cheapest: cheap %d, dear %d", cheap.calls(), dear.calls())
	}
	if r := e.records.last(t); r.Model != "auto-coding" || r.Attempts[0].Model != "small" || r.Attempts[0].UpstreamModel != "small-1" {
		t.Fatalf("record %s -> %s (%s)", r.Model, r.Attempts[0].Model, r.Attempts[0].UpstreamModel)
	}
	tb.Presets["auto-coding"] = router.Preset{Name: "auto-coding", Models: []string{"big", "small"}, Strategy: "in_order"}
	ask("auto-coding")
	if dear.calls() != 1 {
		t.Fatalf("in order: dear %d", dear.calls())
	}
	ask("big")
	if dear.calls() != 2 || cheap.calls() != 1 {
		t.Fatalf("model asked by name: cheap %d, dear %d", cheap.calls(), dear.calls())
	}
	if r := e.records.last(t); r.Attempts[0].Model != "" {
		t.Fatalf("model asked by name recorded as served by %q", r.Attempts[0].Model)
	}
	// The caller pays the list price of the model that served.
	e.snap.ModelPrice = func(m string) (Price, bool) {
		if m == "small" {
			return Price{Input: 5_000_000}, true
		}
		return Price{}, false
	}
	tb.Presets["auto-coding"] = router.Preset{Name: "auto-coding", Models: []string{"big", "small"}, Strategy: "cheapest"}
	ask("auto-coding")
	if ch := e.records.last(t).Attempts[0].Charge; ch == nil || ch.Source != "model" || ch.Rates.Input != 5_000_000 {
		t.Fatalf("charge %+v", ch)
	}
	// A preset never reaches a model the caller may not use.
	e.snap.Visible = func(_ *plugin.Caller, m string) bool { return m != "big" }
	tb.Presets["auto-coding"] = router.Preset{Name: "auto-coding", Models: []string{"big", "small"}, Strategy: "in_order"}
	ask("auto-coding")
	if dear.calls() != 2 || cheap.calls() != 3 {
		t.Fatalf("hidden model used: cheap %d, dear %d", cheap.calls(), dear.calls())
	}
}

// presetSplit turns e's two routes into gpt-5 (the first) and gpt-mini
// (the second), behind the preset auto [gpt-5, gpt-mini] in order, for a
// caller whose group allows gpt-mini only.
func presetSplit(e *env, model string) {
	tb := e.snap.Table
	routes := tb.Models[model].Routes
	tb.Models["gpt-5"] = router.Model{Name: "gpt-5", Enabled: true, Routes: routes[:1]}
	tb.Models["gpt-mini"] = router.Model{Name: "gpt-mini", Enabled: true, Routes: routes[1:]}
	tb.Presets = map[string]router.Preset{"auto": {Name: "auto", Models: []string{"gpt-5", "gpt-mini"}, Strategy: "in_order"}}
	e.chain.Pick = func(_ string, c []plugin.Candidate) []plugin.Candidate { return c }
	e.snap.Visible = func(_ *plugin.Caller, m string) bool { return m != "gpt-5" && m != model }
}

// A batch for a preset is placed only on a member the caller may use.
func TestBatchPresetSkipsHiddenModel(t *testing.T) {
	big, mini := newFake(t, anthropicBatchFake), newFake(t, anthropicBatchFake)
	e, _ := batchEnv(t, "claude", []upSpec{
		{id: "big", protocol: "anthropic", issuer: "i", fake: big, model: "big-up", caps: "{batches: true}"},
		{id: "mini", protocol: "anthropic", issuer: "i", fake: mini, model: "mini-up", caps: "{batches: true}"},
	})
	presetSplit(e, "claude")
	create := `{"requests":[{"custom_id":"a","params":{"model":"auto","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}}]}`
	if resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(create)); resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, out)
	}
	if big.calls() != 0 || mini.calls() != 1 {
		t.Fatalf("hidden model used: big %d, mini %d", big.calls(), mini.calls())
	}
	// With no member allowed the batch is refused and nothing is sent.
	e.snap.Visible = func(_ *plugin.Caller, m string) bool { return m == "auto" }
	if resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(create)); resp.StatusCode < 400 || big.calls() != 0 || mini.calls() != 1 {
		t.Fatalf("no member allowed: %d %s (big %d, mini %d)", resp.StatusCode, out, big.calls(), mini.calls())
	}
}

// A Realtime session for a preset dials only a member the caller may use,
// and its responses are charged at that member's list price.
func TestRealtimePresetSkipsHiddenModel(t *testing.T) {
	done := `{"type":"response.done","response":{"status":"completed","usage":` + realtimeUsage + `}}`
	big, mini := newWSUpstream(t, done), newWSUpstream(t, done)
	e := newEnv(t, "m", []upSpec{
		{id: "big", protocol: "openai-responses", issuer: "i", fake: big.fakeUpstream, model: "big-up", caps: "{realtime: true}"},
		{id: "mini", protocol: "openai-responses", issuer: "i", fake: mini.fakeUpstream, model: "mini-up", caps: "{realtime: true}"},
	})
	presetSplit(e, "m")
	e.snap.ModelPrice = func(m string) (Price, bool) {
		switch m {
		case "gpt-mini":
			return Price{Input: 1_000_000}, true
		case "auto":
			return Price{Input: 9_000_000}, true
		}
		return Price{}, false
	}
	srv := e.wsServer(WSRealtime)
	c, resp, err := wsDial(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime?model=auto", nil, "realtime")
	if err != nil {
		t.Fatalf("dial: %v %+v", err, resp)
	}
	if _, err := wsRead(t, c); err != nil {
		t.Fatal(err)
	}
	wsSend(t, c, `{"type":"response.create"}`)
	for range 2 {
		if _, err := wsRead(t, c); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Close(websocket.StatusNormalClosure, "bye")
	if big.calls() != 0 || mini.calls() != 1 {
		t.Fatalf("hidden model used: big %d, mini %d", big.calls(), mini.calls())
	}
	r := e.waitRecordsOf(2)[0]
	a := r.Attempts[0]
	// 68 input tokens at gpt-mini's 1/M; cache reads and output unpriced.
	if r.Model != "auto" || a.Model != "gpt-mini" || a.UpstreamModel != "mini-up" || a.Charge == nil || a.Charge.Rates.Input != 1_000_000 || r.ChargeMicro != 68 {
		t.Fatalf("record %s charge %d, attempt %+v", r.Model, r.ChargeMicro, a)
	}
	e.snap.Visible = func(_ *plugin.Caller, m string) bool { return m == "auto" }
	if _, resp, err := wsDial(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime?model=auto", nil, "realtime"); err == nil || resp == nil || resp.StatusCode < 400 || big.calls() != 0 {
		t.Fatalf("no member allowed: %v %+v (big %d)", err, resp, big.calls())
	}
}

// A Realtime response is held for and charged at the caller's rate: the
// list price, else the upstream's cost rate, times the group multiplier.
func TestRealtimeChargeAppliesGroupMultiplier(t *testing.T) {
	cases := []struct {
		name    string
		listed  bool
		charge  int64
		hold    int64 // DefaultWSResponseReserve output tokens at the charged rate
		source  string
		rateOut int64
	}{
		// 68 × 2.5 + 121 × 12.5 = 1682.5 → 1683
		{"list price", true, 1683, 4096 * 12_500_000 / 1_000_000, "model", 12_500_000},
		// 68 × 3.75 + 64 × 0.375 + 121 × 18.75 = 2547.75 → 2548
		{"upstream rate", false, 2548, 4096 * 18_750_000 / 1_000_000, "upstream", 18_750_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newWSUpstream(t, `{"type":"response.done","response":{"status":"completed","usage":`+realtimeUsage+`}}`)
			meter := newWSMeter(10)
			e := realtimeEnv(t, up, "{realtime: true}")
			e.chain.Meter = meter
			e.caller.GroupID = "vip"
			e.snap.Multiplier = func(g string) float64 { return map[string]float64{"vip": 1.25}[g] }
			e.snap.ModelPrice = func(m string) (Price, bool) {
				return Price{Input: 2_000_000, Output: 10_000_000}, tc.listed && m == "m"
			}
			srv := e.wsServer(WSRealtime)
			c, resp, err := wsDial(t, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime?model=m", nil, "realtime")
			if err != nil {
				t.Fatalf("dial: %v %+v", err, resp)
			}
			if _, err := wsRead(t, c); err != nil {
				t.Fatal(err)
			}
			wsSend(t, c, `{"type":"response.create"}`)
			for range 2 {
				if _, err := wsRead(t, c); err != nil {
					t.Fatal(err)
				}
			}
			_ = c.Close(websocket.StatusNormalClosure, "bye")
			r := e.waitRecordsOf(2)[0]
			if r.ChargeMicro != tc.charge || r.CostMicro != 2038 {
				t.Errorf("charge %d cost %d, want %d and 2038", r.ChargeMicro, r.CostMicro, tc.charge)
			}
			if got := meter.amounts[r.ID]; got != tc.hold {
				t.Errorf("held %d, want %d", got, tc.hold)
			}
			ch := r.Attempts[0].Charge
			if ch == nil || ch.Multiplier != 1.25 || ch.Source != tc.source || ch.Rates.Output != tc.rateOut {
				t.Errorf("charge detail %+v", ch)
			}
		})
	}
}
