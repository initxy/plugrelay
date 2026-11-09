package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/initxy/plugrelay/internal/app"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/plugins/job/pricesync"
)

// catalogPath is where the fake upstream serves a models.dev catalog.
const catalogPath = "/models.dev/api.json"

// catalogMakers maps a model owner to its models.dev provider id; owners
// not listed are not in the demo catalog.
var catalogMakers = map[string]string{"anthropic": "anthropic", "openai": "openai", "deepseek": "deepseek"}

// catalogDrift makes models.dev disagree with the demo's prices for a few
// models (input, output per million tokens in dollars), so suggestions show.
var catalogDrift = map[string][2]float64{
	"claude-haiku-4-5":  {1, 5},
	"gpt-5.4":           {2.5, 15},
	"deepseek-v4-flash": {0.28, 1.1},
}

// demoCatalog is a models.dev catalog of the demo models.
func demoCatalog() []byte {
	type cost struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read,omitempty"`
		CacheWrite float64 `json:"cache_write,omitempty"`
	}
	type entry struct {
		ID         string              `json:"id"`
		Name       string              `json:"name"`
		Reasoning  bool                `json:"reasoning"`
		ToolCall   bool                `json:"tool_call"`
		Modalities map[string][]string `json:"modalities"`
		Limit      map[string]int      `json:"limit"`
		Cost       cost                `json:"cost"`
	}
	type provider struct {
		ID     string           `json:"id"`
		Models map[string]entry `json:"models"`
	}
	cat := map[string]*provider{}
	for _, m := range modelSpecs {
		pid, ok := catalogMakers[m.owner]
		if !ok {
			continue
		}
		if cat[pid] == nil {
			cat[pid] = &provider{ID: pid, Models: map[string]entry{}}
		}
		l := listPrice(m.cost)
		c := cost{Input: l.in, Output: l.out, CacheRead: l.cacheRead, CacheWrite: l.cacheWrite5m}
		if d, ok := catalogDrift[m.name]; ok {
			c.Input, c.Output = d[0], d[1]
		}
		in := []string{"text"}
		if m.vision {
			in = append(in, "image")
		}
		cat[pid].Models[m.name] = entry{ID: m.name, Name: m.display, Reasoning: m.reasoning, ToolCall: true,
			Modalities: map[string][]string{"input": in, "output": {"text"}},
			Limit:      map[string]int{"context": m.context, "output": m.maxOutput}, Cost: c}
	}
	b, _ := json.Marshal(cat)
	return b
}

func serveCatalog(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(demoCatalog())
}

// seedPriceSync turns price sync on against the fake upstream's catalog and
// stores what a first check finds, so the prices tab has suggestions at
// once; the job's own checks find the same. One model follows models.dev.
func seedPriceSync(ctx context.Context, a *app.App, c *api, fakeURL string) error {
	if err := c.v1("PATCH", "/features/price_sync", map[string]any{"enabled": true,
		"settings": map[string]any{pricesync.ID: map[string]any{"url": fakeURL + catalogPath}}}, nil); err != nil {
		return err
	}
	cat, err := pricesync.Parse(bytes.NewReader(demoCatalog()))
	if err != nil {
		return err
	}
	var models []pricesync.Model
	for _, m := range modelSpecs {
		models = append(models, pricesync.Model{Name: m.name})
	}
	st := a.Store()
	now := time.Now().UTC()
	var rows []store.PriceSync
	for _, s := range pricesync.Match(cat, models, 1, "USD") {
		rows = append(rows, store.PriceSync{Model: s.Model, Ref: s.Ref, Input: s.Input, CacheRead: s.CacheRead,
			CacheWrite5m: s.CacheWrite5m, CacheWrite1h: s.CacheWrite1h, Output: s.Output, Currency: s.Currency, Info: s.Info,
			FetchedAt: now.Add(-2 * time.Hour)})
	}
	if err := st.SavePriceCatalog(ctx, rows); err != nil {
		return err
	}
	if err := st.AcceptPriceSuggestion(ctx, "claude-opus-4-8"); err != nil {
		return err
	}
	if err := st.SetPriceFollow(ctx, "claude-opus-4-8", true); err != nil {
		return err
	}
	b, _ := json.Marshal(pricesync.Status{RanAt: now.Add(-2 * time.Hour), Matched: len(rows), Models: len(models)})
	return st.KV(pricesync.ID).Put(ctx, pricesync.StatusKey, b, 0)
}
