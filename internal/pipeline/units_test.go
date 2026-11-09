package pipeline

import (
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Each media unit is priced per unit: images per image, audio per second
// (from milliseconds), speech per 1000 characters; tokens stay per million.
func TestPriceMediaUnits(t *testing.T) {
	p := Price{Input: 2_000_000, Units: map[string]int64{plugin.UnitImage: 40_000, plugin.UnitAudioSecond: 100, plugin.UnitKChars: 15_000}}
	cases := []struct {
		name   string
		u      ir.Usage
		cost   int64
		charge int64
	}{
		{"images", ir.Usage{Images: 3}, 120_000, 120_000},
		{"audio seconds", ir.Usage{AudioMs: 12_345}, 1234, 1235}, // 1234.5
		{"characters", ir.Usage{Chars: 1501}, 22_515, 22_515},
		{"tokens and units", ir.Usage{Input: 1000, Images: 1}, 42_000, 42_000},
		{"no units priced", ir.Usage{Images: 2}, 0, 0},
	}
	for _, tc := range cases {
		pr := p
		if tc.name == "no units priced" {
			pr.Units = nil
		}
		if got := pr.Cost(tc.u); got != tc.cost {
			t.Errorf("%s: cost %d, want %d", tc.name, got, tc.cost)
		}
		if got := pr.charge(tc.u); got != tc.charge {
			t.Errorf("%s: charge %d, want %d", tc.name, got, tc.charge)
		}
	}
}
