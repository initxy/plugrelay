package demo

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// The request history is drawn from a fixed seed, so every run produces
// the same requests, shifted to end at the current hour.
const seed = 20261107

type gen struct {
	rnd    *rand.Rand
	w      *world
	now    time.Time
	models map[string]modelSpec
	users  map[string]userSpec
	mult   map[string]float64 // group id -> price multiplier
}

func (g *gen) chance(p float64) bool { return g.rnd.Float64() < p }

// between draws an integer in [lo, hi], skewed towards lo.
func (g *gen) between(lo, hi int64) int64 {
	f := g.rnd.Float64()
	return lo + int64(f*f*float64(hi-lo))
}

func (g *gen) pick(ws []weighted) string {
	total := 0
	for _, w := range ws {
		total += w.weight
	}
	n := g.rnd.IntN(total)
	for _, w := range ws {
		if n < w.weight {
			return w.name
		}
		n -= w.weight
	}
	return ws[len(ws)-1].name
}

func (g *gen) hex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(g.rnd.UintN(256))
	}
	return hex.EncodeToString(b)
}

// hourWeight is the share of a day's traffic in one UTC hour: a working
// day in UTC+8 with a smaller evening bump, quiet weekends.
func hourWeight(t time.Time) float64 {
	local := t.Add(8 * time.Hour)
	h := float64(local.Hour()) + 0.5
	day := math.Exp(-math.Pow(h-11, 2)/8) + 0.9*math.Exp(-math.Pow(h-16, 2)/6) + 0.35*math.Exp(-math.Pow(h-21.5, 2)/3)
	if wd := local.Weekday(); wd == time.Saturday || wd == time.Sunday {
		day *= 0.3
	}
	return 0.03 + day
}

// genRecords draws the request log of the last days days.
func genRecords(w *world, days int, now time.Time) []*plugin.Record {
	g := &gen{rnd: rand.New(rand.NewPCG(seed, seed>>3)), w: w, now: now, models: map[string]modelSpec{},
		users: map[string]userSpec{}, mult: map[string]float64{}}
	for _, m := range modelSpecs {
		g.models[m.name] = m
	}
	for _, u := range userSpecs {
		g.users[u.key] = u
	}
	for _, gs := range groupSpecs {
		g.mult[w.groups[gs.key]] = gs.multiplier
	}
	var keys []weighted
	for i, k := range w.keys {
		keys = append(keys, weighted{name: fmt.Sprint(i), weight: k.spec.weight})
	}
	var out []*plugin.Record
	start := now.Add(-time.Duration(days) * 24 * time.Hour).Truncate(time.Hour)
	for h := start; h.Before(now); h = h.Add(time.Hour) {
		// Usage grows over the period.
		grow := 0.65 + 0.35*float64(h.Sub(start))/float64(now.Sub(start))
		n := int(math.Round(hourWeight(h) * 16 * grow * (0.75 + 0.5*g.rnd.Float64())))
		for range n {
			at := h.Add(time.Duration(g.rnd.Int64N(int64(time.Hour))))
			if !at.Before(now) {
				continue
			}
			var i int
			_, _ = fmt.Sscan(g.pick(keys), &i)
			k := w.keys[i]
			if k.spec.expiresInDays < 0 && at.After(now.Add(time.Duration(k.spec.expiresInDays)*24*time.Hour)) {
				continue
			}
			out = append(out, g.record(k, at))
		}
	}
	// A few recent requests carry their content, as with body logging on.
	for i := max(len(out)-25, 0); i < len(out); i++ {
		r := out[i]
		req, _ := json.Marshal(map[string]any{"model": r.Model, "stream": r.Stream, "messages": []any{
			map[string]any{"role": "user", "content": "Refactor the retry loop in sender.go to use a backoff helper."}}})
		resp, _ := json.Marshal(map[string]any{"role": "assistant", "content": reply})
		r.Body = &plugin.RecordBody{Request: req, Response: resp}
	}
	return out
}

type usageShape struct{ cacheRead, input, write5m, write1h, output [2]int64 }

var usageByClient = map[string]usageShape{
	"claude-code": {cacheRead: [2]int64{12_000, 150_000}, input: [2]int64{20, 2_500}, write5m: [2]int64{800, 14_000},
		write1h: [2]int64{2_000, 24_000}, output: [2]int64{60, 4_500}},
	"codex":      {cacheRead: [2]int64{8_000, 110_000}, input: [2]int64{400, 9_000}, output: [2]int64{150, 7_000}},
	"cursor":     {cacheRead: [2]int64{0, 24_000}, input: [2]int64{2_500, 28_000}, write5m: [2]int64{500, 6_000}, output: [2]int64{80, 2_800}},
	"gemini-cli": {cacheRead: [2]int64{0, 12_000}, input: [2]int64{1_500, 32_000}, output: [2]int64{80, 2_200}},
	"curl":       {input: [2]int64{20, 900}, output: [2]int64{15, 700}},
	"cline":      {cacheRead: [2]int64{4_000, 60_000}, input: [2]int64{1_500, 20_000}, write5m: [2]int64{500, 8_000}, output: [2]int64{100, 3_000}},
	"sdk-python": {input: [2]int64{200, 4_000}, output: [2]int64{50, 1_500}},
}

// failure is one way an attempt goes wrong.
type failure struct {
	status int
	cause  plugin.Cause
	kind   ir.ErrorKind
	msg    string
}

var (
	retryable = []failure{
		{429, plugin.CauseRateLimit, ir.ErrRateLimited, "rate limit exceeded: 40000 output tokens per minute"},
		{529, plugin.CauseRateLimit, ir.ErrOverloaded, "Overloaded"},
		{503, plugin.CauseTransient, ir.ErrUpstream, "upstream connect error or disconnect/reset before headers"},
	}
	final = []failure{
		{400, plugin.CauseRequest, ir.ErrContextTooLong, "prompt is too long: 1048576 tokens > 1000000 maximum"},
		{400, plugin.CauseRequest, ir.ErrInvalidRequest, "messages.3.content.0: tool_use ids must be unique"},
		{504, plugin.CauseTransient, ir.ErrTimeout, "no first byte from the upstream within 60s"},
		{429, plugin.CauseRateLimit, ir.ErrRateLimited, "every credential of this model is rate limited"},
	}
)

func (g *gen) record(k seededKey, at time.Time) *plugin.Record {
	client := g.pick(k.spec.clients)
	cp := clients[client]
	user := g.users[k.spec.user]
	groupID := g.w.groups[user.group]
	asked := g.pick(cp.models)
	served := asked
	for _, p := range presetSpecs {
		if p.name == asked {
			served = p.models[g.rnd.IntN(len(p.models))]
		}
	}
	m := g.models[served]
	if m.routes[0].groups != nil && user.group != "vip" {
		served, m = "claude-sonnet-4-6", g.models["claude-sonnet-4-6"]
		if asked != "budget" {
			asked = served
		}
	}
	rec := &plugin.Record{
		ID: g.hex(12), Time: at.UTC(), Protocol: cp.protocol, Model: asked, Stream: g.chance(cp.stream),
		Caller:   plugin.Caller{KeyID: k.id, KeyName: k.spec.name, UserID: g.w.users[user.key], GroupID: groupID},
		ClientUA: cp.ua, ClientIP: user.ip, Retried: g.chance(0.01),
	}
	if cp.sessions {
		// One session per key and three-hour block.
		block := at.Unix() / (3 * 3600)
		rec.SessionID = fmt.Sprintf("%s-%x", k.spec.name, block)
	}

	routes := m.routes
	pickRoute := func() routeSpec {
		r := routes[0]
		if len(routes) > 1 {
			// Bedrock took a quarter of the traffic until its breaker opened.
			if g.now.Sub(at) > 2*time.Hour && g.chance(0.25) {
				r = routes[1]
			}
		}
		return r
	}
	nAttempts := 1
	var failAt failure
	finalFail := false
	switch x := g.rnd.Float64(); {
	case x < 0.025:
		finalFail, nAttempts = true, 1+g.rnd.IntN(2)
		failAt = final[g.rnd.IntN(len(final))]
		if failAt.cause == plugin.CauseRequest {
			nAttempts = 1
		}
	case x < 0.085:
		nAttempts = 2
		failAt = retryable[g.rnd.IntN(len(retryable))]
	}
	ttfb := time.Duration(g.between(280, 2600)) * time.Millisecond
	var elapsed time.Duration
	for n := 1; n <= nAttempts; n++ {
		r := pickRoute()
		if n > 1 && len(routes) > 1 && r.up == routes[1].up {
			r = routes[0]
		}
		a := g.attempt(rec, cp, client, r, served, asked != served)
		last := n == nAttempts
		if !last || finalFail {
			f := failAt
			a.Status, a.Cause, a.Error = f.status, f.cause, f.msg
			a.TTFB, a.Duration = 0, time.Duration(g.between(80, 1500))*time.Millisecond
			if f.kind == ir.ErrTimeout {
				a.Duration = 60 * time.Second
			}
			a.Usage, a.CostMicro, a.Diagnostic = ir.Usage{}, 0, nil
			if last {
				rec.Status, rec.ErrorKind, rec.Error = f.status, f.kind, f.msg
			}
		} else {
			g.fillUsage(&a, client, r)
			a.Status = 200
			a.TTFB = ttfb
			a.Duration = ttfb + time.Duration(a.Usage.Output*1000/g.between(45, 140))*time.Millisecond
			a.Charge = g.charge(m, groupID)
			rec.Status, rec.Usage = 200, a.Usage
			rec.ChargeMicro = cost(a.Usage, a.Charge.Rates)
			rec.TTFB = elapsed + a.TTFB
		}
		elapsed += a.Duration
		rec.CostMicro += a.CostMicro
		rec.Attempts = append(rec.Attempts, a)
	}
	rec.Duration = elapsed
	return rec
}

// attempt fills what does not depend on the outcome: where it went and at
// which processing level.
func (g *gen) attempt(rec *plugin.Record, cp clientProfile, client string, r routeSpec, served string, preset bool) plugin.AttemptRecord {
	var spec upstreamSpec
	for _, s := range upstreamSpecs {
		if s.key == r.up {
			spec = s
		}
	}
	ep := spec.endpoints[0]
	for _, e := range spec.endpoints {
		if e.protocol == cp.protocol {
			ep = e
			break
		}
	}
	a := plugin.AttemptRecord{N: len(rec.Attempts) + 1, UpstreamID: g.w.upstreams[r.up], EndpointID: ep.id,
		CredentialID: g.credFor(r.up, rec), Protocol: ep.protocol, UpstreamModel: r.upstreamModel, Sent: true, Tier: plugin.TierPassthrough}
	if preset {
		a.Model = served
	}
	switch {
	case ep.protocol != cp.protocol:
		a.Tier = plugin.TierConvert
		steps := []string{"tools: namespaced tools flattened"}
		if ep.protocol == "anthropic" {
			steps = append(steps, "cache: placed breakpoints by policy")
		}
		a.Diagnostic = &ir.Diagnostic{Fidelity: ir.Lossless, Steps: steps}
		if client == "gemini-cli" && g.chance(0.3) {
			a.Diagnostic.Fidelity = ir.Lossy
			a.Diagnostic.Dropped = []string{"generationConfig.seed"}
		}
	case r.up == "bedrock" || (r.up == "deepseek" && ep.protocol == "anthropic") || g.chance(0.06):
		a.Tier = plugin.TierPatch
	}
	return a
}

// credFor picks the credential an attempt used: the backup key took some
// traffic until it cooled down, the old org key until it was quarantined
// three days ago, Bob's requests mostly use his own OpenAI key.
func (g *gen) credFor(up string, rec *plugin.Record) string {
	age := g.now.Sub(rec.Time)
	switch up {
	case "anthropic":
		switch x := g.rnd.Float64(); {
		case x < 0.12 && age > 3*24*time.Hour:
			return g.w.cred(up, "legacy")
		case x < 0.4 && age > 2*time.Hour:
			return g.w.cred(up, "backup")
		}
		return g.w.cred(up, "primary")
	case "openai":
		if rec.Caller.UserID == g.w.users["bob"] && g.chance(0.6) {
			return g.w.cred(up, "bob")
		}
		if g.chance(0.5) {
			return g.w.cred(up, "team-b")
		}
		return g.w.cred(up, "team-a")
	case "bedrock":
		return g.w.cred(up, "prod")
	}
	return g.w.cred(up, "main")
}

func (g *gen) fillUsage(a *plugin.AttemptRecord, client string, r routeSpec) {
	s := usageByClient[client]
	u := ir.Usage{Input: g.between(s.input[0], s.input[1]), Output: g.between(s.output[0], s.output[1])}
	if s.cacheRead[1] > 0 && g.chance(0.85) {
		u.CacheRead = g.between(s.cacheRead[0], s.cacheRead[1])
	}
	explicit := r.up == "anthropic" || r.up == "bedrock"
	if explicit && s.write5m[1] > 0 && g.chance(0.35) {
		u.CacheWrite5m = g.between(s.write5m[0], s.write5m[1])
	}
	if r.up == "anthropic" && s.write1h[1] > 0 && g.chance(0.06) {
		u.CacheWrite1h = g.between(s.write1h[0], s.write1h[1])
	}
	m := g.models[routeModel(r)]
	if m.reasoning && g.chance(0.6) {
		u.Reasoning = u.Output * g.between(15, 60) / 100
	}
	a.Usage = u
	a.CostMicro = cost(u, rates(m.cost, 1))
}

// routeModel is the client-facing model a route belongs to.
func routeModel(r routeSpec) string {
	for _, m := range modelSpecs {
		for _, mr := range m.routes {
			if mr.up == r.up && mr.upstreamModel == r.upstreamModel {
				return m.name
			}
		}
	}
	return ""
}

func (g *gen) charge(m modelSpec, groupID string) *plugin.Charge {
	mult, ok := g.mult[groupID]
	if !ok {
		mult = 1
	}
	return &plugin.Charge{Rates: rates(listPrice(m.cost), mult), Multiplier: mult, Source: "model"}
}

func rates(p price, mult float64) plugin.Rates {
	r := func(usd float64) int64 { return int64(math.Round(usd * 1e6 * mult)) }
	return plugin.Rates{Input: r(p.in), CacheRead: r(p.cacheRead), CacheWrite5m: r(p.cacheWrite5m), CacheWrite1h: r(p.cacheWrite1h), Output: r(p.out)}
}

// cost is tokens × rate per million, rounded half up.
func cost(u ir.Usage, r plugin.Rates) int64 {
	sum := u.Input*r.Input + u.CacheRead*r.CacheRead + u.CacheWrite5m*r.CacheWrite5m + u.CacheWrite1h*r.CacheWrite1h + u.Output*r.Output
	return (sum + 500_000) / 1_000_000
}

// insertRecords stores the history in batches and marks each key's last
// use.
func insertRecords(ctx context.Context, st store.Store, recs []*plugin.Record) error {
	last := map[string]time.Time{}
	for i := 0; i < len(recs); i += 500 {
		batch := recs[i:min(i+500, len(recs))]
		if err := st.InsertRecords(ctx, batch); err != nil {
			return err
		}
		for _, r := range batch {
			if r.Time.After(last[r.Caller.KeyID]) {
				last[r.Caller.KeyID] = r.Time
			}
		}
	}
	for id, t := range last {
		st.TouchKey(id, t)
	}
	return st.FlushTouches(ctx)
}
