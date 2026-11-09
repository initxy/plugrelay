// Package routing is the selector.routing plugin: the routing strategy, one
// setting that picks how candidates are ranked after the router planned
// them.
//
//	priority_weight  keep the router's order: priority, then the weighted
//	                 split of new sessions (the default)
//	cheapest         lowest upstream price (input + output per million
//	                 tokens, as the core costs the attempt) first; a price
//	                 of 0 is taken at face value
//	fastest          lowest recent time to first byte first; candidates
//	                 never measured go last
//	quota_reset      credentials whose upstream quota resets soonest first,
//	                 so allowance that would expire unused gets spent;
//	                 reset times come from job.quotaprobe
//
// Every strategy only reorders: sticky candidates (a pinned turn or a bound
// session, whose prompt cache lives there) stay first, ties keep router
// order, and the result holds exactly the planned candidates.
package routing

import (
	"slices"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// ID is the plugin id.
const ID = "selector.routing"

func init() { plugin.Register(ID, func() plugin.Plugin { return New() }) }

// SettingStrategy is the setting naming the strategy.
const SettingStrategy = "strategy"

// Strategies.
const (
	PriorityWeight = "priority_weight"
	Cheapest       = "cheapest"
	Fastest        = "fastest"
	QuotaReset     = "quota_reset"
)

// Strategies lists every strategy, the default first.
var Strategies = []string{PriorityWeight, Cheapest, Fastest, QuotaReset}

// Selector implements plugin.Selector.
type Selector struct{}

// New returns the plugin.
func New() *Selector { return &Selector{} }

// Info implements plugin.Plugin.
func (*Selector) Info() plugin.Info {
	return plugin.Info{ID: ID, Kind: plugin.KindSelector, Version: "1.0.0",
		Description: "Routing strategy: priority and weight, cheapest, fastest, or soonest quota reset first.",
		Settings: []plugin.Setting{
			{Key: SettingStrategy, Type: plugin.TypeString, Default: PriorityWeight, Enum: Strategies,
				Scopes: plugin.Global | plugin.Group, Merge: plugin.Override,
				Label:      plugin.T("Routing strategy", "路由策略"),
				Help:       plugin.T("Which upstream a request tries first. A conversation stays where its prompt cache is warm whatever the strategy.", "请求先发给哪个上游。不管选哪种，一段对话在提示词缓存还有效时都会留在原来的上游。"),
				EnumLabels: map[string]plugin.Text{"priority_weight": plugin.T("By priority and weight", "按优先级和权重"), "cheapest": plugin.T("Cheapest first", "便宜的优先"), "fastest": plugin.T("Fastest first", "响应快的优先"), "quota_reset": plugin.T("Soonest quota reset first", "额度最快重置的优先")}},
		}}
}

// Rank implements plugin.Selector with the strategy set for the caller.
func (*Selector) Rank(c *plugin.Call, cands []plugin.Candidate) []plugin.Candidate {
	s := PriorityWeight
	if c != nil {
		if v := c.Setting(SettingStrategy).String(); v != "" {
			s = v
		}
	}
	return Rank(s, cands)
}

// Rank orders a copy of cands by strategy; an unknown strategy keeps
// priority_weight.
func Rank(strategy string, cands []plugin.Candidate) []plugin.Candidate {
	if !slices.Contains(Strategies, strategy) {
		strategy = PriorityWeight
	}
	return stable(cands, less[strategy])
}

// stable returns a reordered copy of cands. Sticky candidates stay ahead of
// the rest (a session bound to a credential keeps its prompt cache); within
// each part, less orders the candidates and ties keep router order.
func stable(cands []plugin.Candidate, less func(a, b *plugin.Candidate) bool) []plugin.Candidate {
	out := slices.Clone(cands)
	slices.SortStableFunc(out, func(a, b plugin.Candidate) int {
		switch {
		case a.Sticky != b.Sticky:
			if a.Sticky {
				return -1
			}
			return 1
		case less(&a, &b):
			return -1
		case less(&b, &a):
			return 1
		}
		return 0
	})
	return out
}

var less = map[string]func(a, b *plugin.Candidate) bool{
	// The router already orders by priority and splits new sessions by
	// weight, stably per session; re-sorting by priority alone is a no-op
	// on its output and never reshuffles a session's weighted pick.
	PriorityWeight: func(a, b *plugin.Candidate) bool { return a.Priority > b.Priority },
	Cheapest: func(a, b *plugin.Candidate) bool {
		return a.PriceInMicro+a.PriceOutMicro < b.PriceInMicro+b.PriceOutMicro
	},
	Fastest: func(a, b *plugin.Candidate) bool {
		ka, kb := a.LatencyMs > 0, b.LatencyMs > 0
		if ka != kb {
			return ka
		}
		return ka && a.LatencyMs < b.LatencyMs
	},
	QuotaReset: func(a, b *plugin.Candidate) bool {
		ka, kb := !a.QuotaReset.IsZero(), !b.QuotaReset.IsZero()
		if ka != kb {
			return ka
		}
		return ka && a.QuotaReset.Before(b.QuotaReset)
	},
}
