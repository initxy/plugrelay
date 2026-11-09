package admin

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/plugins/job/pricesync"
)

const priceSyncFeature = "price_sync"

type priceHints struct {
	ContextWindow int  `json:"context_window,omitempty"`
	MaxOutput     int  `json:"max_output,omitempty"`
	Reasoning     bool `json:"reasoning"`
	Vision        bool `json:"vision"`
	Tools         bool `json:"tools"`
}

type priceSuggestionView struct {
	Model  string      `json:"model"`
	Ref    string      `json:"ref"`
	Prices tokenPrices `json:"prices"`
	// Current is what the model is charged now; absent when it has no price.
	Current *tokenPrices `json:"current,omitempty"`
	// CurrentRule is the rule charging it now (an exact rule or a pattern).
	CurrentRule string     `json:"current_rule,omitempty"`
	Follow      bool       `json:"follow"`
	FetchedAt   time.Time  `json:"fetched_at"`
	Hints       priceHints `json:"hints"`
}

type priceSyncStatus struct {
	Enabled bool       `json:"enabled"`
	RanAt   *time.Time `json:"ran_at,omitempty"`
	Matched int        `json:"matched"`
	Code    string     `json:"error_code,omitempty"`
	Error   string     `json:"error,omitempty"`
}

func hintsOf(i ir.ModelInfo) priceHints {
	return priceHints{ContextWindow: i.ContextWindow, MaxOutput: i.MaxOutput, Reasoning: i.Reasoning, Vision: i.Vision, Tools: i.Tools}
}

func syncPrices(p store.PriceSync) tokenPrices {
	return tokenPrices{Currency: p.Currency, Input: p.Input, CacheRead: p.CacheRead, CacheWrite5m: p.CacheWrite5m,
		CacheWrite1h: p.CacheWrite1h, Output: p.Output}
}

// priceSyncView is the price sync part of GET /pricing-rules: pending
// suggestions of existing models, the follow switch of every matched model
// by rule id, and the job's last run.
func (h *Handler) priceSyncView(c *call, rules []store.PriceRule) (suggestions []priceSuggestionView, follow map[string]bool, st priceSyncStatus, err error) {
	on, err := h.featureState(c)
	if err != nil {
		return nil, nil, st, err
	}
	st.Enabled = on.On(priceSyncFeature)
	if b, ok, err := h.d.Store.KV(pricesync.ID).Get(c.ctx, pricesync.StatusKey); err == nil && ok {
		var js pricesync.Status
		if json.Unmarshal(b, &js) == nil {
			st.RanAt, st.Matched, st.Code, st.Error = &js.RanAt, js.Matched, js.Code, js.Error
		}
	}
	rows, err := h.d.Store.ListPriceSync(c.ctx)
	if err != nil {
		return nil, nil, st, err
	}
	ms, err := h.d.Store.ListModels(c.ctx)
	if err != nil {
		return nil, nil, st, err
	}
	exists := map[string]bool{}
	for _, m := range ms {
		exists[m.Name] = true
	}
	cur := h.currency(c.ctx)
	suggestions, follow = []priceSuggestionView{}, map[string]bool{}
	for _, r := range rows {
		if !exists[r.Model] || !r.Matched() {
			continue
		}
		var now *store.PriceRule
		if p, ok := modelPrice(rules, r.Model); ok {
			now = &p
		}
		if now != nil && now.Model == r.Model {
			follow[priceID(*now)] = r.Follow
		}
		// A pattern's price is the model's price only until it has its own.
		var exact *store.PriceRule
		if now != nil && now.Model == r.Model {
			exact = now
		}
		if !r.Pending(exact) {
			continue
		}
		v := priceSuggestionView{Model: r.Model, Ref: r.Ref, Prices: syncPrices(r), Follow: r.Follow, FetchedAt: r.FetchedAt,
			Hints: hintsOf(r.Info)}
		if now != nil {
			p := pricesOf(*now, cur)
			v.Current, v.CurrentRule = &p, priceID(*now)
		}
		suggestions = append(suggestions, v)
	}
	return suggestions, follow, st, nil
}

func (h *Handler) priceSyncModel(c *call) (string, error) {
	name := c.r.PathValue("model")
	if _, err := h.d.Store.GetModel(c.ctx, name); err != nil {
		return "", storeErr(err, objModel)
	}
	return name, nil
}

func noMatch(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return errNotFound(eNoPriceMatch)
	}
	return err
}

func (h *Handler) acceptPriceSuggestion(c *call) error {
	model, err := h.priceSyncModel(c)
	if err != nil {
		return err
	}
	if err := h.d.Store.AcceptPriceSuggestion(c.ctx, model); err != nil {
		return noMatch(err)
	}
	p, ok := modelPrice(mustPrices(c.ctx, h.d.Store), model)
	if !ok {
		return errNotFound(eNoPriceMatch)
	}
	c.audit("pricing.accept", "pricing_rule", priceID(p), model, []change{{Field: "prices", To: pricesOf(p, "")}})
	return c.json(200, h.ruleView(c, p))
}

func (h *Handler) rejectPriceSuggestion(c *call) error {
	model, err := h.priceSyncModel(c)
	if err != nil {
		return err
	}
	if err := h.d.Store.RejectPriceSuggestion(c.ctx, model); err != nil {
		return noMatch(err)
	}
	c.audit("pricing.reject", "model", model, model, nil)
	return c.noContent()
}

func (h *Handler) updatePriceSync(c *call) error {
	model, err := h.priceSyncModel(c)
	if err != nil {
		return err
	}
	var in struct {
		Follow *bool `json:"follow"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if in.Follow == nil {
		return fieldInvalid("follow", eRequired)
	}
	if err := h.d.Store.SetPriceFollow(c.ctx, model, *in.Follow); err != nil {
		return err
	}
	c.audit("pricing.follow", "model", model, model, []change{{Field: "follow", To: *in.Follow}})
	return c.noContent()
}

func mustPrices(ctx context.Context, st store.Store) []store.PriceRule {
	ps, _ := st.ListPrices(ctx)
	return ps
}

// catalogHints fills the model information the catalog lacks from what
// price sync learnt.
func (h *Handler) catalogHints(ctx context.Context) map[string]ir.ModelInfo {
	rows, err := h.d.Store.ListPriceSync(ctx)
	if err != nil {
		return nil
	}
	out := map[string]ir.ModelInfo{}
	for _, r := range rows {
		if r.Matched() {
			out[r.Model] = r.Info
		}
	}
	return out
}

func withHints(i ir.ModelInfo, hint ir.ModelInfo) ir.ModelInfo {
	if i.ContextWindow == 0 {
		i.ContextWindow = hint.ContextWindow
	}
	if i.MaxOutput == 0 {
		i.MaxOutput = hint.MaxOutput
	}
	i.Reasoning = i.Reasoning || hint.Reasoning
	i.Vision = i.Vision || hint.Vision
	i.Tools = i.Tools || hint.Tools
	return i
}
