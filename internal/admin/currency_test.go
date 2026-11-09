package admin

import (
	"context"
	"testing"

	"github.com/initxy/plugrelay/internal/store"
)

// Changing site.currency while balances exist needs an explicit confirm:
// the amounts are not converted.
func TestCurrencyChangeWithBalances(t *testing.T) {
	for name, open := range stores(t) {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, open, false)
			c := e.setup()
			set := func(v any, confirm bool) int {
				code, _ := c.do("PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"site.currency": v},
					"confirm_currency_change": confirm})
				return code
			}
			if code := set("EUR", false); code != 200 {
				t.Fatalf("change without balances: %d", code)
			}
			l, _ := store.LedgerOf(e.st)
			if _, _, err := l.Credit(context.Background(), &store.BalanceTxn{ID: "t1", Subject: "user:x", Kind: store.TxnTopup, AmountMicro: 5}); err != nil {
				t.Fatal(err)
			}
			if code := set("eur", false); code != 200 {
				t.Fatalf("same currency: %d", code)
			}
			for _, v := range []any{"JPY", nil} {
				if code := set(v, false); code != 409 {
					t.Fatalf("change to %v with balances: %d", v, code)
				}
			}
			if code := set("JPY", true); code != 200 {
				t.Fatalf("confirmed change: %d", code)
			}
		})
	}
}
