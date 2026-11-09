package admin

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
)

func TestPriceSuggestions(t *testing.T) {
	e := newEnv(t, sqliteStore, false)
	c := e.setup()
	ctx := context.Background()
	for _, m := range []string{"claude-x", "vendor/gpt-y"} {
		c.must(201, "POST", "/api/v1/models", map[string]any{"name": m})
	}
	c.must(201, "POST", "/api/v1/pricing-rules", map[string]any{"match": "claude-*", "prices": map[string]any{"input": 1, "output": 2}})
	if err := e.st.SavePriceCatalog(ctx, []store.PriceSync{
		{Model: "claude-x", Ref: "anthropic/claude-x", Input: 3_000_000, Output: 15_000_000, Currency: "CNY",
			Info: ir.ModelInfo{ContextWindow: 200_000, Reasoning: true}, FetchedAt: time.Now()},
		{Model: "vendor/gpt-y", Ref: "openai/gpt-y", Input: 1_000_000, Output: 4_000_000, Currency: "CNY", FetchedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	out := c.must(200, "GET", "/api/v1/pricing-rules", nil)
	sugg := out["suggestions"].([]any)
	if len(sugg) != 2 {
		t.Fatalf("suggestions %v", sugg)
	}
	first := sugg[0].(map[string]any)
	if first["model"] != "claude-x" || first["current_rule"] == nil || first["hints"].(map[string]any)["context_window"] != 200000.0 {
		t.Fatalf("claude suggestion %v", first)
	}
	if sync := out["sync"].(map[string]any); sync["enabled"] != false {
		t.Fatalf("sync %v", sync)
	}

	// Rejecting hides it; accepting gives the model its own synced price.
	c.must(204, "POST", "/api/v1/price-sync/claude-x/reject", nil)
	path := "/api/v1/price-sync/" + url.PathEscape("vendor/gpt-y")
	rule := c.must(200, "POST", path+"/accept", nil)
	if rule["source"] != "pricesync" || rule["match"] != "vendor/gpt-y" {
		t.Fatalf("accepted %v", rule)
	}
	c.must(204, "PATCH", path, map[string]any{"follow": true})
	out = c.must(200, "GET", "/api/v1/pricing-rules", nil)
	if len(out["suggestions"].([]any)) != 0 {
		t.Fatalf("still suggested %v", out["suggestions"])
	}
	var followed bool
	for _, it := range out["items"].([]any) {
		r := it.(map[string]any)
		if r["match"] == "vendor/gpt-y" {
			followed = r["follow"] == true
		}
	}
	if !followed {
		t.Fatalf("follow not shown: %v", out["items"])
	}
	// Editing the price by hand makes it manual.
	var id string
	for _, it := range out["items"].([]any) {
		if r := it.(map[string]any); r["match"] == "vendor/gpt-y" {
			id = r["id"].(string)
		}
	}
	if r := c.must(200, "PATCH", "/api/v1/pricing-rules/"+id, map[string]any{"prices": map[string]any{"input": 5, "output": 6}}); r["source"] != "manual" {
		t.Fatalf("edited %v", r)
	}

	if code, out := c.do("POST", "/api/v1/price-sync/nope/accept", nil); code != 404 || errCode(out) != "not_found" {
		t.Fatalf("unknown model %d %v", code, out)
	}
	c.must(201, "POST", "/api/v1/models", map[string]any{"name": "unmatched"})
	if code, out := c.do("POST", "/api/v1/price-sync/unmatched/accept", nil); code != 404 || errCode(out) != "no_price_match" {
		t.Fatalf("unmatched model %d %v", code, out)
	}

	// The catalog shows the context window the model lacks.
	cat := c.must(200, "GET", "/api/v1/catalog", nil)
	for _, it := range cat["items"].([]any) {
		if m := it.(map[string]any); m["model"] == "claude-x" {
			if m["info"].(map[string]any)["context_window"] != 200000.0 {
				t.Fatalf("catalog hints %v", m)
			}
		}
	}
}
