package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
)

// Price sources: typed in by an operator, or taken from the public model
// catalog (price sync).
const (
	PriceManual = "manual"
	PriceSynced = "sync"
)

// PriceSync is what the public model catalog says about one of our models,
// plus the operator's decisions about it. Prices are micro-units of
// Currency per million tokens, already in the site currency.
type PriceSync struct {
	Model string `json:"model"`
	// Follow applies new catalog prices by itself, unless the model's own
	// price was entered by hand: manual prices always win.
	Follow bool `json:"follow"`
	// Ref is the catalog entry the model matched ("<provider>/<model>");
	// empty until a sync matched it.
	Ref          string `json:"ref,omitempty"`
	Input        int64  `json:"input"`
	CacheRead    int64  `json:"cache_read"`
	CacheWrite5m int64  `json:"cache_write_5m"`
	CacheWrite1h int64  `json:"cache_write_1h"`
	Output       int64  `json:"output"`
	Currency     string `json:"currency,omitempty"`
	// Info carries the catalog's context window and capability hints.
	Info      ir.ModelInfo `json:"info"`
	FetchedAt time.Time    `json:"fetched_at,omitzero"`
	// Rejected is the Fingerprint of the prices last rejected; the same
	// prices are not suggested again.
	Rejected string `json:"rejected,omitempty"`
}

// Fingerprint identifies the suggested prices.
func (p PriceSync) Fingerprint() string {
	return strings.Join([]string{strconv.FormatInt(p.Input, 10), strconv.FormatInt(p.CacheRead, 10),
		strconv.FormatInt(p.CacheWrite5m, 10), strconv.FormatInt(p.CacheWrite1h, 10), strconv.FormatInt(p.Output, 10),
		strings.ToUpper(p.Currency)}, "/")
}

// Matched reports whether a sync found the model in the catalog.
func (p PriceSync) Matched() bool { return p.Ref != "" }

// Same reports whether rule r already charges the suggested prices.
func (p PriceSync) Same(r PriceRule) bool {
	return r.Input == p.Input && r.CacheRead == p.CacheRead && r.CacheWrite5m == p.CacheWrite5m &&
		r.CacheWrite1h == p.CacheWrite1h && r.Output == p.Output && strings.EqualFold(r.Currency, p.Currency)
}

// Pending reports whether the suggestion should be shown against cur, the
// model's current price (nil when it has none): it was matched, differs
// and was not rejected.
func (p PriceSync) Pending(cur *PriceRule) bool {
	if !p.Matched() || p.Rejected == p.Fingerprint() {
		return false
	}
	return cur == nil || !p.Same(*cur)
}

// rule is the exact price rule the suggestion becomes, keeping the media
// unit prices and batch discount of the rule it replaces.
func (p PriceSync) rule(old *PriceRule) PriceRule {
	r := PriceRule{Model: p.Model, Input: p.Input, CacheRead: p.CacheRead, CacheWrite5m: p.CacheWrite5m,
		CacheWrite1h: p.CacheWrite1h, Output: p.Output, Currency: p.Currency, Source: PriceSynced}
	if old != nil {
		r.Units, r.BatchDiscount = old.Units, old.BatchDiscount
	}
	return r
}

const priceSyncCols = `model, follow, ref, input, cache_read, cache_write_5m, cache_write_1h, output, currency, info, fetched_at, rejected`

func scanPriceSync(sc interface{ Scan(...any) error }) (*PriceSync, error) {
	var p PriceSync
	var info string
	var fetched int64
	if err := sc.Scan(&p.Model, &p.Follow, &p.Ref, &p.Input, &p.CacheRead, &p.CacheWrite5m, &p.CacheWrite1h, &p.Output,
		&p.Currency, &info, &fetched, &p.Rejected); err != nil {
		return nil, err
	}
	p.FetchedAt = fromMs(fetched)
	return &p, unjs(info, &p.Info)
}

func (s *db) ListPriceSync(ctx context.Context) ([]PriceSync, error) {
	return list(ctx, s.r(), "list price sync", `SELECT `+priceSyncCols+` FROM price_sync ORDER BY model`, scanPriceSync)
}

func getPriceSync(ctx context.Context, r run, model string) (*PriceSync, error) {
	p, err := scanPriceSync(r.row(ctx, `SELECT `+priceSyncCols+` FROM price_sync WHERE model = ?`, model))
	if err != nil {
		return nil, wrap("get price sync", err)
	}
	return p, nil
}

// exactPrice is the model's own client-facing rule, or nil.
func exactPrice(ctx context.Context, r run, model string) (*PriceRule, error) {
	p, err := scanPrice(r.row(ctx, `SELECT `+priceCols+` FROM price_rules WHERE model = ? AND upstream_id = '' AND upstream_model = ''`, model))
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrap("get price", err)
	}
	return p, nil
}

// follow applies a followed suggestion unless the model's price was
// entered by hand.
func follow(ctx context.Context, r run, p *PriceSync) error {
	if !p.Follow || !p.Matched() {
		return nil
	}
	cur, err := exactPrice(ctx, r, p.Model)
	if err != nil {
		return err
	}
	if cur != nil && (cur.Source != PriceSynced || p.Same(*cur)) {
		return nil
	}
	rule := p.rule(cur)
	return putPrice(ctx, r, &rule)
}

func (s *db) SavePriceCatalog(ctx context.Context, rows []PriceSync) error {
	return s.write(ctx, func(r run) error {
		for i := range rows {
			p := &rows[i]
			if p.Model == "" || p.Ref == "" {
				return fmt.Errorf("store: save price catalog: needs a model and a catalog entry")
			}
			if p.Currency == "" {
				p.Currency = "USD"
			}
			if _, err := r.exec(ctx, `INSERT INTO price_sync (`+priceSyncCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (model) DO UPDATE SET ref = excluded.ref, input = excluded.input, cache_read = excluded.cache_read,
  cache_write_5m = excluded.cache_write_5m, cache_write_1h = excluded.cache_write_1h, output = excluded.output,
  currency = excluded.currency, info = excluded.info, fetched_at = excluded.fetched_at`,
				p.Model, false, p.Ref, p.Input, p.CacheRead, p.CacheWrite5m, p.CacheWrite1h, p.Output, p.Currency,
				js(p.Info, "{}"), ms(p.FetchedAt), ""); err != nil {
				return wrap("save price catalog", err)
			}
			stored, err := getPriceSync(ctx, r, p.Model)
			if err != nil {
				return err
			}
			if err := follow(ctx, r, stored); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *db) SetPriceFollow(ctx context.Context, model string, on bool) error {
	if model == "" {
		return fmt.Errorf("store: set price follow: empty model")
	}
	return s.write(ctx, func(r run) error {
		if _, err := r.exec(ctx, `INSERT INTO price_sync (model, follow) VALUES (?, ?)
ON CONFLICT (model) DO UPDATE SET follow = excluded.follow`, model, on); err != nil {
			return wrap("set price follow", err)
		}
		p, err := getPriceSync(ctx, r, model)
		if err != nil {
			return err
		}
		return follow(ctx, r, p)
	})
}

func (s *db) AcceptPriceSuggestion(ctx context.Context, model string) error {
	return s.write(ctx, func(r run) error {
		p, err := getPriceSync(ctx, r, model)
		if err != nil {
			return err
		}
		if !p.Matched() {
			return fmt.Errorf("store: accept price suggestion: %w", ErrNotFound)
		}
		cur, err := exactPrice(ctx, r, model)
		if err != nil {
			return err
		}
		rule := p.rule(cur)
		return putPrice(ctx, r, &rule)
	})
}

func (s *db) RejectPriceSuggestion(ctx context.Context, model string) error {
	return s.write(ctx, func(r run) error {
		p, err := getPriceSync(ctx, r, model)
		if err != nil {
			return err
		}
		if !p.Matched() {
			return fmt.Errorf("store: reject price suggestion: %w", ErrNotFound)
		}
		_, err = r.exec(ctx, `UPDATE price_sync SET rejected = ? WHERE model = ?`, p.Fingerprint(), model)
		return wrap("reject price suggestion", err)
	})
}
