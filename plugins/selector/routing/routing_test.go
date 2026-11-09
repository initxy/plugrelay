package routing_test

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
	"github.com/initxy/plugrelay/plugins/selector/routing"
)

var t0 = time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)

func cands() []plugin.Candidate {
	return []plugin.Candidate{
		{CredentialID: "a", Priority: 1, PriceInMicro: 3, PriceOutMicro: 10, LatencyMs: 500, QuotaReset: t0.Add(5 * time.Hour)},
		{CredentialID: "b", Priority: 2, PriceInMicro: 1, PriceOutMicro: 5, LatencyMs: 0},
		{CredentialID: "c", Priority: 2, PriceInMicro: 1, PriceOutMicro: 5, LatencyMs: 200, QuotaReset: t0.Add(time.Hour)},
		{CredentialID: "d", Priority: 0, PriceInMicro: 10, PriceOutMicro: 40, LatencyMs: 300},
		{CredentialID: "e", Priority: 1, Sticky: true, PriceInMicro: 50, PriceOutMicro: 50, LatencyMs: 900},
	}
}

func TestRank(t *testing.T) {
	tests := []struct {
		strategy string
		want     []string
	}{
		{routing.PriorityWeight, []string{"e", "b", "c", "a", "d"}},
		{routing.Cheapest, []string{"e", "b", "c", "a", "d"}},
		{routing.Fastest, []string{"e", "c", "d", "a", "b"}},
		{routing.QuotaReset, []string{"e", "c", "a", "b", "d"}},
		{"unknown", []string{"e", "b", "c", "a", "d"}},
	}
	for _, tt := range tests {
		t.Run(tt.strategy, func(t *testing.T) {
			in := cands()
			got := routing.Rank(tt.strategy, in)
			sameSet(t, got, cands())
			if order := ids(got); !slices.Equal(order, tt.want) {
				t.Errorf("got %v want %v", order, tt.want)
			}
			if !slices.EqualFunc(in, cands(), func(a, b plugin.Candidate) bool { return a.CredentialID == b.CredentialID }) {
				t.Error("input slice reordered in place")
			}
		})
	}
}

// The plugin ranks by the strategy the caller's settings name.
func TestSelectorReadsStrategy(t *testing.T) {
	if _, ok := plugin.Lookup(routing.ID); !ok {
		t.Fatal("not registered")
	}
	call := testkit.NewCall(routing.ID, testkit.CallOpts{Settings: testkit.Settings{routing.SettingStrategy: routing.Fastest}})
	if got := ids(routing.New().Rank(call, cands())); !slices.Equal(got, []string{"e", "c", "d", "a", "b"}) {
		t.Fatalf("got %v", got)
	}
	if got := ids(routing.New().Rank(nil, cands())); !slices.Equal(got, []string{"e", "b", "c", "a", "d"}) {
		t.Fatalf("default: %v", got)
	}
}

func TestNeverAddsOrDrops(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		n := r.IntN(8)
		in := make([]plugin.Candidate, n)
		for i := range in {
			in[i] = plugin.Candidate{CredentialID: string(rune('a' + i)), Priority: r.IntN(3), Sticky: r.IntN(4) == 0,
				PriceInMicro: int64(r.IntN(3)), LatencyMs: float64(r.IntN(3) * 100),
				QuotaReset: t0.Add(time.Duration(r.IntN(3)) * time.Hour)}
		}
		for _, s := range routing.Strategies {
			sameSet(t, routing.Rank(s, in), in)
		}
	}
	for _, s := range routing.Strategies {
		if got := routing.Rank(s, nil); len(got) != 0 {
			t.Fatal("empty input grew")
		}
	}
}

// sameSet fails t unless got holds exactly the candidates of want, by
// credential id, in any order.
func sameSet(t testing.TB, got, want []plugin.Candidate) {
	t.Helper()
	count := map[string]int{}
	for _, c := range want {
		count[c.CredentialID]++
	}
	for _, c := range got {
		count[c.CredentialID]--
	}
	for id, n := range count {
		if n != 0 {
			t.Fatalf("candidate set changed at %q (%d): got %v", id, n, ids(got))
		}
	}
	if len(got) != len(want) {
		t.Fatalf("len %d, want %d", len(got), len(want))
	}
}

// ids lists credential ids in order.
func ids(cs []plugin.Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.CredentialID
	}
	return out
}
