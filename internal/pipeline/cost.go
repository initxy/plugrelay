package pipeline

import (
	"math/big"

	"github.com/initxy/plugrelay/internal/convert"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Cost prices usage: each bucket times its rate per million tokens, plus
// media units times their unit rate. Reasoning tokens are part of Output
// and not priced again.
func (p Price) Cost(u ir.Usage) int64 {
	n, d := p.amount(u)
	return n.Quo(n, d).Int64()
}

// amount is the price of u as numerator and denominator.
func (p Price) amount(u ir.Usage) (*big.Int, *big.Int) {
	sum := new(big.Int)
	add := func(n, rate, scale int64) {
		sum.Add(sum, new(big.Int).Mul(new(big.Int).Mul(big.NewInt(n), big.NewInt(rate)), big.NewInt(scale)))
	}
	add(u.Input, p.Input, 1)
	add(u.CacheRead, p.CacheRead, 1)
	add(u.CacheWrite5m, p.CacheWrite5m, 1)
	add(u.CacheWrite1h, p.CacheWrite1h, 1)
	add(u.Output, p.Output, 1)
	if len(p.Units) > 0 {
		// Unit rates are per unit; scaled onto the per-million denominator.
		add(u.Images, plugin.ImageRate(p.Units, u.ImageSize, u.ImageQuality), 1_000_000)
		add(u.AudioMs, p.Units[plugin.UnitAudioSecond], 1_000) // ms / 1000 s
		add(u.Chars, p.Units[plugin.UnitKChars], 1_000)        // chars / 1000
		add(u.SearchUnits, p.Units[plugin.UnitSearch], 1_000_000)
	}
	return sum, big.NewInt(1_000_000)
}

// settleUsage fills the usage of every attempt that reached an upstream
// and may have been billed there: one that got a 2xx, or that was sent and
// then lost (timeout, reset). Missing usage is estimated from the request
// as sent and the output received, and marked Estimated.
func (x *exchange) settleUsage(a *attempt) {
	if !a.wrote.Load() || !(a.ok || a.status == 0) {
		return
	}
	req := a.sent
	if req == nil {
		req = &ir.Request{}
	}
	if a.usage.IsZero() {
		a.usage = convert.EstimateUsage(req, a.outChars)
		return
	}
	// A stream cut before its end event carries only the usage reported so
	// far (Anthropic's message_start says output_tokens: 1), so the output
	// actually received sets a floor.
	if x.in.Stream && !a.done {
		if est := convert.EstimateUsage(req, a.outChars).Output; est > a.usage.Output {
			a.usage.Output, a.usage.Estimated = est, true
		}
	}
}

// cost prices an attempt at its upstream model's rate.
func (x *exchange) cost(a *attempt) int64 {
	pr, ok := x.snap.costRate(a.cand.UpstreamID, a.cand.UpstreamModel)
	if !ok {
		return 0
	}
	return pr.Cost(a.usage)
}

// charge is what the caller pays for attempt a: the list price of the
// model it served (for a preset, that model's, else the preset's), or a's
// cost rate when there is none, times the caller's group multiplier. A nil
// Charge means nothing is priced.
func (x *exchange) charge(a *attempt) (micro int64, ch *plugin.Charge) {
	pr, src, ok := x.snap.chargeRate(x.served[router.Key(a.cand)], x.in.Model, a.cand.UpstreamID, a.cand.UpstreamModel)
	if !ok {
		return 0, nil
	}
	r, ch := chargeAt(pr, x.multiplier(), src, 0)
	return r.charge(a.usage), ch
}

// costRate is the operator's rate for an upstream model.
func (s *Snapshot) costRate(upstreamID, upModel string) (Price, bool) {
	if s.Price == nil {
		return Price{}, false
	}
	return s.Price(upstreamID, upModel)
}

// listPrice is a client-facing model's list price.
func (s *Snapshot) listPrice(model string) (Price, bool) {
	if s.ModelPrice == nil {
		return Price{}, false
	}
	return s.ModelPrice(model)
}

// chargeRate is the unscaled rate a caller is charged for model: the list
// price of served (the preset member that served it; "" for none), else
// of model, else the upstream model's cost rate. src says which: "model"
// or "upstream".
func (s *Snapshot) chargeRate(served, model, upstreamID, upModel string) (pr Price, src string, ok bool) {
	if served != "" {
		if pr, ok := s.listPrice(served); ok {
			return pr, "model", true
		}
	}
	if pr, ok := s.listPrice(model); ok {
		return pr, "model", true
	}
	if pr, ok := s.costRate(upstreamID, upModel); ok {
		return pr, "upstream", true
	}
	return Price{}, "", false
}

// chargeAt is pr at the caller's rate: times the group multiplier mult,
// less a batch discount, with the attempt's charge detail.
func chargeAt(pr Price, mult float64, src string, discount float64) (Price, *plugin.Charge) {
	r := pr.scaled(mult * (1 - discount))
	return r, &plugin.Charge{Multiplier: mult, Source: src, Rates: r.rates(), BatchDiscount: discount}
}

// tokenEstimate prices in input and out output tokens at pr, truncated.
func (p Price) tokenEstimate(in, out int64) int64 {
	return (in*p.Input + out*p.Output) / 1_000_000
}

// multiplier is the caller's group price multiplier.
func (x *exchange) multiplier() float64 {
	if x.caller == nil {
		return 1
	}
	return x.snap.multiplier(x.caller.GroupID)
}

// multiplier is a group's price multiplier; 1 when it sets none.
func (s *Snapshot) multiplier(groupID string) float64 {
	if s.Multiplier == nil || groupID == "" {
		return 1
	}
	if m := s.Multiplier(groupID); m > 0 {
		return m
	}
	return 1
}

// scaled multiplies every rate by m (plugin.Rates.Scale).
func (p Price) scaled(m float64) Price {
	r := p.rates().Scale(m)
	return Price{Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, CacheWrite5m: r.CacheWrite5m,
		CacheWrite1h: r.CacheWrite1h, Units: r.Units}
}

func (p Price) rates() plugin.Rates {
	return plugin.Rates{Input: p.Input, CacheRead: p.CacheRead, CacheWrite5m: p.CacheWrite5m,
		CacheWrite1h: p.CacheWrite1h, Output: p.Output, Units: p.Units}
}

// charge is like Cost but rounds half up instead of truncating.
func (p Price) charge(u ir.Usage) int64 {
	n, d := p.amount(u)
	return plugin.RoundMicro(new(big.Rat).SetFrac(n, d))
}
