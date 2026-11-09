package admin

import (
	"context"
	"encoding/base64"
	"path"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

type tokenPrices struct {
	Currency     string           `json:"currency"`
	Input        int64            `json:"input"`
	CacheRead    int64            `json:"cache_read"`
	CacheWrite5m int64            `json:"cache_write_5m"`
	CacheWrite1h int64            `json:"cache_write_1h"`
	Output       int64            `json:"output"`
	Units        map[string]int64 `json:"units,omitempty"`
	// BatchDiscount is the fraction taken off batch API usage.
	BatchDiscount float64 `json:"batch_discount,omitempty"`
}

type pricingRuleView struct {
	ID     string      `json:"id"`
	Match  string      `json:"match"`
	Prices tokenPrices `json:"prices"`
	// Source is manual or pricesync.
	Source    string     `json:"source"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// Follow is set on a model's own rule when price sync found the model:
	// whether new catalog prices apply by themselves.
	Follow *bool `json:"follow,omitempty"`
}

func sourceOf(p store.PriceRule) string {
	if p.Source == store.PriceSynced {
		return "pricesync"
	}
	return "manual"
}

func (h *Handler) ruleView(c *call, p store.PriceRule) pricingRuleView {
	v := pricingRuleView{ID: priceID(p), Match: priceMatch(p), Prices: pricesOf(p, h.currency(c.ctx)), Source: sourceOf(p)}
	if !p.UpdatedAt.IsZero() {
		t := p.UpdatedAt
		v.UpdatedAt = &t
	}
	return v
}

// A rule's id encodes its natural key: model, upstream and upstream model.
func priceID(p store.PriceRule) string {
	return base64.RawURLEncoding.EncodeToString([]byte(p.Model + "\x00" + p.UpstreamID + "\x00" + p.UpstreamModel))
}

func parsePriceID(id string) (model, upstream, upstreamModel string, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", "", "", false
	}
	parts := strings.Split(string(b), "\x00")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// match is the rule's display form: the model pattern, or
// "<upstream>/<upstream model>" for operator-cost rules.
func priceMatch(p store.PriceRule) string {
	if p.Model != "" {
		return p.Model
	}
	if p.UpstreamModel == "" {
		return p.UpstreamID + "/*"
	}
	return p.UpstreamID + "/" + p.UpstreamModel
}

func pricesOf(p store.PriceRule, cur string) tokenPrices {
	return tokenPrices{Currency: orDefault(p.Currency, cur), Input: p.Input, CacheRead: p.CacheRead,
		CacheWrite5m: p.CacheWrite5m, CacheWrite1h: p.CacheWrite1h, Output: p.Output, Units: p.Units, BatchDiscount: p.BatchDiscount}
}

func (h *Handler) listPricingRules(c *call) error {
	ps, err := h.d.Store.ListPrices(c.ctx)
	if err != nil {
		return err
	}
	suggestions, follow, st, err := h.priceSyncView(c, ps)
	if err != nil {
		return err
	}
	items := []pricingRuleView{}
	for _, p := range ps {
		v := h.ruleView(c, p)
		if f, ok := follow[v.ID]; ok {
			v.Follow = &f
		}
		items = append(items, v)
	}
	return c.json(200, map[string]any{"items": items, "suggestions": suggestions, "sync": st})
}

type pricingInput struct {
	Match  *string      `json:"match"`
	Prices *tokenPrices `json:"prices"`
}

func validatePrices(p *tokenPrices, bad *invalid) {
	for _, f := range []struct {
		name string
		v    int64
	}{{"input", p.Input}, {"cache_read", p.CacheRead}, {"cache_write_5m", p.CacheWrite5m},
		{"cache_write_1h", p.CacheWrite1h}, {"output", p.Output}} {
		if f.v < 0 {
			bad.add("prices."+f.name, eNotNegative)
		}
	}
	if p.BatchDiscount < 0 || p.BatchDiscount >= 1 {
		bad.add("prices.batch_discount", eBatchDiscount)
	}
	if p.Currency != "" && len(p.Currency) != 3 {
		bad.add("prices.currency", eCurrencyCode)
	}
	for k, v := range p.Units {
		switch {
		case !plugin.ValidUnit(k):
			bad.add("prices.units."+k, ePriceUnit)
		case v < 0:
			bad.add("prices.units."+k, eNotNegative)
		}
	}
}

func (h *Handler) createPricingRule(c *call) error {
	var in pricingInput
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	if in.Match == nil || strings.TrimSpace(*in.Match) == "" {
		bad.add("match", eNotEmpty)
	} else if _, err := path.Match(*in.Match, ""); err != nil {
		bad.add("match", eGlob)
	}
	if in.Prices == nil {
		bad.add("prices", eRequired)
	} else {
		validatePrices(in.Prices, &bad)
	}
	if err := bad.err(); err != nil {
		return err
	}
	p := store.PriceRule{Model: strings.TrimSpace(*in.Match)}
	setPrices(&p, *in.Prices, h.currency(c.ctx))
	if err := h.d.Store.PutPrice(c.ctx, &p); err != nil {
		return storeErr(err, objPriceRule)
	}
	c.audit("pricing.create", "pricing_rule", priceID(p), p.Model, []change{{Field: "prices", To: in.Prices}})
	return c.json(201, h.ruleView(c, p))
}

func setPrices(p *store.PriceRule, t tokenPrices, cur string) {
	p.Input, p.CacheRead, p.CacheWrite5m, p.CacheWrite1h, p.Output = t.Input, t.CacheRead, t.CacheWrite5m, t.CacheWrite1h, t.Output
	p.Units = t.Units
	p.BatchDiscount = t.BatchDiscount
	p.Currency = strings.ToUpper(orDefault(t.Currency, cur))
	// Prices typed in are the operator's: price sync never replaces them.
	p.Source = store.PriceManual
}

func (h *Handler) findPrice(c *call) (store.PriceRule, error) {
	model, up, upModel, ok := parsePriceID(c.r.PathValue("id"))
	if !ok {
		return store.PriceRule{}, errNotFound(eNotFound.with("object", objPriceRule))
	}
	ps, err := h.d.Store.ListPrices(c.ctx)
	if err != nil {
		return store.PriceRule{}, err
	}
	for _, p := range ps {
		if p.Model == model && p.UpstreamID == up && p.UpstreamModel == upModel {
			return p, nil
		}
	}
	return store.PriceRule{}, errNotFound(eNotFound.with("object", objPriceRule))
}

func (h *Handler) updatePricingRule(c *call) error {
	p, err := h.findPrice(c)
	if err != nil {
		return err
	}
	var in pricingInput
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	if in.Match != nil && *in.Match != priceMatch(p) {
		bad.add("match", eImmutable)
	}
	if in.Prices != nil {
		validatePrices(in.Prices, &bad)
	}
	if err := bad.err(); err != nil {
		return err
	}
	before := pricesOf(p, "")
	if in.Prices != nil {
		setPrices(&p, *in.Prices, h.currency(c.ctx))
		if err := h.d.Store.PutPrice(c.ctx, &p); err != nil {
			return storeErr(err, objPriceRule)
		}
	}
	c.audit("pricing.update", "pricing_rule", priceID(p), priceMatch(p), []change{{Field: "prices", From: before, To: pricesOf(p, "")}})
	return c.json(200, h.ruleView(c, p))
}

func (h *Handler) deletePricingRule(c *call) error {
	p, err := h.findPrice(c)
	if err != nil {
		return err
	}
	if err := h.d.Store.DeletePrice(c.ctx, p.Model, p.UpstreamID, p.UpstreamModel); err != nil {
		return storeErr(err, objPriceRule)
	}
	c.audit("pricing.delete", "pricing_rule", priceID(p), priceMatch(p), nil)
	return c.noContent()
}

// ModelPrice is the client-facing price rule of a model, or nil.
func ModelPrice(ps []store.PriceRule, model string) *store.PriceRule {
	if p, ok := modelPrice(ps, model); ok {
		return &p
	}
	return nil
}

// modelPrice finds the client-facing price of a model: an exact rule, then
// the first matching glob.
func modelPrice(ps []store.PriceRule, model string) (store.PriceRule, bool) {
	for _, p := range ps {
		if p.Model == model && p.UpstreamID == "" {
			return p, true
		}
	}
	for _, p := range ps {
		if p.Model != "" && p.UpstreamID == "" {
			if ok, _ := path.Match(p.Model, model); ok {
				return p, true
			}
		}
	}
	return store.PriceRule{}, false
}

type catalogEntry struct {
	Model        string       `json:"model"`
	Kind         string       `json:"kind"`
	Info         ir.ModelInfo `json:"info,omitzero"`
	Price        tokenPrices  `json:"price"`
	PresetModels []string     `json:"preset_models,omitempty"`
}

func (h *Handler) getCatalog(c *call) error {
	allowed := h.callerModelFilter(c)
	if c.who.isAdmin() {
		allowed = func(string) bool { return true }
	}
	// Prices are shown as the caller pays them: list price times the
	// caller's group multiplier, never the multiplier itself.
	mult := 1.0
	if u := c.who.user; u != nil && u.GroupID != "" {
		if g, err := h.d.Store.GetGroup(c.ctx, u.GroupID); err == nil {
			mult = g.PriceMultiplier
		}
	}
	items, err := h.catalogItems(c.ctx, allowed, mult)
	if err != nil {
		return err
	}
	return c.json(200, map[string]any{"items": items})
}

// catalogItems lists the enabled models allowed passes and the presets
// serving at least one of them, at list price times mult.
func (h *Handler) catalogItems(ctx context.Context, allowed func(string) bool, mult float64) ([]catalogEntry, error) {
	ms, err := h.d.Store.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	ps, err := h.d.Store.ListPrices(ctx)
	if err != nil {
		return nil, err
	}
	cur := h.currency(ctx)
	hints := h.catalogHints(ctx)
	items := []catalogEntry{}
	for i := range ms {
		m := &ms[i]
		if !m.Enabled || !allowed(m.Name) {
			continue
		}
		v := modelViewOf(m, nil)
		e := catalogEntry{Model: m.Name, Kind: "model", Info: withHints(v.Info, hints[m.Name]), Price: tokenPrices{Currency: cur}}
		if p, ok := modelPrice(ps, m.Name); ok {
			r := plugin.Rates{Input: p.Input, CacheRead: p.CacheRead, CacheWrite5m: p.CacheWrite5m,
				CacheWrite1h: p.CacheWrite1h, Output: p.Output, Units: p.Units}.Scale(mult)
			p.Input, p.CacheRead, p.CacheWrite5m, p.CacheWrite1h, p.Output = r.Input, r.CacheRead, r.CacheWrite5m, r.CacheWrite1h, r.Output
			p.Units = r.Units
			e.Price = pricesOf(p, cur)
		}
		items = append(items, e)
	}
	presets, err := h.d.Store.ListPresets(ctx)
	if err != nil {
		return nil, err
	}
	enabled := map[string]bool{}
	for _, m := range ms {
		enabled[m.Name] = m.Enabled
	}
	for _, p := range presets {
		e := catalogEntry{Model: p.Name, Kind: "preset", Price: tokenPrices{Currency: cur}}
		for _, m := range p.Models {
			if enabled[m] && allowed(m) {
				e.PresetModels = append(e.PresetModels, m)
			}
		}
		if len(e.PresetModels) > 0 {
			items = append(items, e)
		}
	}
	return items, nil
}
