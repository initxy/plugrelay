package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
)

func exact(t *testing.T, s store.Store, model string) *store.PriceRule {
	t.Helper()
	ps, err := s.ListPrices(context.Background())
	must(t, err)
	for i := range ps {
		if ps[i].Model == model && ps[i].UpstreamID == "" {
			return &ps[i]
		}
	}
	return nil
}

func syncRow(t *testing.T, s store.Store, model string) store.PriceSync {
	t.Helper()
	rows, err := s.ListPriceSync(context.Background())
	must(t, err)
	for _, r := range rows {
		if r.Model == model {
			return r
		}
	}
	t.Fatalf("no price sync row for %s", model)
	return store.PriceSync{}
}

// Manual prices win: a followed model gets catalog prices only while its
// price is missing or came from sync; rejecting keeps the same prices from
// coming back, new prices are suggested again.
func TestPriceSync(t *testing.T) {
	onEveryDB(t, func(t *testing.T, open opener) {
		s := open(t, newBox(t))
		ctx := context.Background()
		cat := func(model string, in, out int64) store.PriceSync {
			return store.PriceSync{Model: model, Ref: "anthropic/" + model, Input: in, Output: out, Currency: "USD",
				Info: ir.ModelInfo{ContextWindow: 200000}, FetchedAt: time.Now()}
		}
		must(t, s.PutPrice(ctx, &store.PriceRule{Model: "hand", Input: 1, Output: 2}))
		must(t, s.SetPriceFollow(ctx, "hand", true))
		must(t, s.SetPriceFollow(ctx, "auto", true))
		must(t, s.SavePriceCatalog(ctx, []store.PriceSync{cat("hand", 3_000_000, 15_000_000), cat("auto", 1_000_000, 5_000_000),
			cat("asked", 2_000_000, 8_000_000)}))

		if p := exact(t, s, "hand"); p == nil || p.Input != 1 || p.Source != store.PriceManual {
			t.Fatalf("manual price was replaced: %+v", p)
		}
		if p := exact(t, s, "auto"); p == nil || p.Input != 1_000_000 || p.Source != store.PriceSynced || p.UpdatedAt.IsZero() {
			t.Fatalf("followed model not applied: %+v", p)
		}
		if p := exact(t, s, "asked"); p != nil {
			t.Fatalf("unfollowed model got a price: %+v", p)
		}
		hand := syncRow(t, s, "hand")
		if !hand.Follow || !hand.Pending(exact(t, s, "hand")) || hand.Info.ContextWindow != 200000 {
			t.Fatalf("hand row %+v", hand)
		}

		must(t, s.RejectPriceSuggestion(ctx, "hand"))
		if r := syncRow(t, s, "hand"); r.Pending(exact(t, s, "hand")) {
			t.Fatal("rejected suggestion still pending")
		}
		must(t, s.SavePriceCatalog(ctx, []store.PriceSync{cat("hand", 3_000_000, 15_000_000)}))
		if r := syncRow(t, s, "hand"); r.Pending(exact(t, s, "hand")) || !r.Follow {
			t.Fatalf("same prices came back or follow lost: %+v", r)
		}
		must(t, s.SavePriceCatalog(ctx, []store.PriceSync{cat("hand", 4_000_000, 15_000_000)}))
		if r := syncRow(t, s, "hand"); !r.Pending(exact(t, s, "hand")) {
			t.Fatal("new prices not suggested")
		}

		must(t, s.AcceptPriceSuggestion(ctx, "asked"))
		if p := exact(t, s, "asked"); p == nil || p.Output != 8_000_000 || p.Source != store.PriceSynced {
			t.Fatalf("accepted %+v", p)
		}
		if r := syncRow(t, s, "asked"); r.Pending(exact(t, s, "asked")) {
			t.Fatal("accepted suggestion still pending")
		}
		if err := s.AcceptPriceSuggestion(ctx, "nothing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("accept unknown: %v", err)
		}
		// A followed synced price follows the next catalog change.
		must(t, s.SavePriceCatalog(ctx, []store.PriceSync{cat("auto", 1_500_000, 5_000_000)}))
		if p := exact(t, s, "auto"); p.Input != 1_500_000 {
			t.Fatalf("followed price not updated: %+v", p)
		}
		// Editing it by hand stops following.
		p := exact(t, s, "auto")
		p.Source, p.Input = store.PriceManual, 7
		must(t, s.PutPrice(ctx, p))
		must(t, s.SavePriceCatalog(ctx, []store.PriceSync{cat("auto", 2_000_000, 5_000_000)}))
		if p := exact(t, s, "auto"); p.Input != 7 {
			t.Fatalf("manual edit overwritten: %+v", p)
		}
	})
}
