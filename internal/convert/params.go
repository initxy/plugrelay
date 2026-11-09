package convert

import (
	"strconv"

	"github.com/initxy/plugrelay/pkg/ir"
)

// Reasoning styles a capability declaration may name.
const (
	StyleEffort = "effort"
	StyleBudget = "budget"
)

// minBudget is the smallest thinking budget the table produces.
const minBudget = 1024

// efforts is the effort ⇄ budget table, ordered by budget.
var efforts = []struct {
	name   string
	budget int
}{
	{"minimal", 1024},
	{"low", 4096},
	{"medium", 10000},
	{"high", 24000},
	{"xhigh", 32000},
	{"max", 64000},
}

// EffortBudget returns the thinking budget for a named effort.
func EffortBudget(effort string) (int, bool) {
	for _, e := range efforts {
		if e.name == effort {
			return e.budget, true
		}
	}
	return 0, false
}

// BudgetEffort returns the effort whose budget is nearest to budget; ties go
// to the lower effort.
func BudgetEffort(budget int) string {
	best := efforts[0]
	for _, e := range efforts[1:] {
		if abs(e.budget-budget) < abs(best.budget-budget) {
			best = e
		}
	}
	return best.name
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// params maps an UnsupportedParams name to a function clearing it; it
// reports whether the field was set.
var params = map[string]func(*ir.Request) bool{
	"temperature":    func(q *ir.Request) bool { return unset(&q.Temperature) },
	"top_p":          func(q *ir.Request) bool { return unset(&q.TopP) },
	"top_k":          func(q *ir.Request) bool { return unset(&q.TopK) },
	"parallel_tools": func(q *ir.Request) bool { return unset(&q.ParallelTools) },
	"store":          func(q *ir.Request) bool { return unset(&q.Store) },
	"max_output_tokens": func(q *ir.Request) bool {
		return unset(&q.MaxOutputTokens)
	},
	"stop": func(q *ir.Request) bool {
		set := len(q.Stop) > 0
		q.Stop = nil
		return set
	},
	"service_tier": func(q *ir.Request) bool { return clearStr(&q.ServiceTier) },
	"verbosity":    func(q *ir.Request) bool { return clearStr(&q.Verbosity) },
	"cache_key":    func(q *ir.Request) bool { return clearStr(&q.CacheKey) },
	"tool_choice":  func(q *ir.Request) bool { return unset(&q.ToolChoice) },
	"reasoning":    func(q *ir.Request) bool { return unset(&q.Reasoning) },
}

func unset[T any](p **T) bool {
	set := *p != nil
	*p = nil
	return set
}

func clearStr(p *string) bool {
	set := *p != ""
	*p = ""
	return set
}

// stepParams adapts generation parameters: reasoning controls are mapped to
// the target's style or dropped, and parameters the target rejects are
// removed. The output limit is kept; the target codec writes it under the
// target's MaxTokensField.
func stepParams(req *ir.Request, c *Context, r *recorder) {
	t := c.Target
	if req.MaxOutputTokens != nil && t.MaxTokensField != "" && t.MaxTokensField != c.Source.MaxTokensField {
		r.step("params: output limit sent as " + t.MaxTokensField)
	}
	if rs := req.Reasoning; rs != nil {
		switch {
		case !t.Reasoning:
			req.Reasoning = nil
			if rs.Enabled {
				r.step("params: reasoning removed")
				r.drop("reasoning")
				r.major(lossReasoning)
			}
		case !rs.Enabled:
		case t.ReasoningStyle == StyleBudget && rs.Budget == nil && rs.Effort != "":
			if b, ok := EffortBudget(rs.Effort); ok {
				cp := *rs
				cp.Budget = &b
				req.Reasoning = &cp
				r.step("params: effort " + rs.Effort + " mapped to budget " + strconv.Itoa(b))
			}
		case t.ReasoningStyle == StyleEffort && rs.Effort == "" && rs.Budget != nil:
			cp := *rs
			cp.Effort = BudgetEffort(*rs.Budget)
			req.Reasoning = &cp
			r.step("params: budget " + strconv.Itoa(*rs.Budget) + " mapped to effort " + cp.Effort)
		}
		clampBudget(req, c, r)
	}
	for _, name := range t.UnsupportedParams {
		if clearFn, ok := params[name]; ok && clearFn(req) {
			r.step("params: " + name + " removed")
			r.drop("param:" + name)
		}
	}
}

// clampBudget keeps a derived budget below the output limit, which budget
// style targets require.
func clampBudget(req *ir.Request, c *Context, r *recorder) {
	rs := req.Reasoning
	if rs == nil || rs.Budget == nil || req.MaxOutputTokens == nil || c.Target.ReasoningStyle != StyleBudget {
		return
	}
	limit := *req.MaxOutputTokens
	if *rs.Budget < limit {
		return
	}
	b := max(limit-1, minBudget)
	if b >= limit {
		return // limit too small to fit any budget; leave it to the upstream
	}
	cp := *rs
	cp.Budget = &b
	req.Reasoning = &cp
	r.note("reasoning budget clamped to " + strconv.Itoa(b) + " below the output limit")
}
