package modelsync

import (
	"context"
	"errors"
	"slices"
	"sort"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// catalog is an in-memory ModelCatalog.
type catalog struct {
	models map[string]map[string]CatalogModel
	calls  int
}

func newCatalog(up string, ms ...CatalogModel) *catalog {
	c := &catalog{models: map[string]map[string]CatalogModel{up: {}}}
	for _, m := range ms {
		c.models[up][m.Name] = m
	}
	return c
}

func (c *catalog) Models(_ context.Context, up string) ([]CatalogModel, error) {
	var out []CatalogModel
	for _, m := range c.models[up] {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *catalog) Upsert(_ context.Context, up string, ms []ir.ModelInfo) error {
	c.calls++
	for _, m := range ms {
		c.models[up][m.ID] = CatalogModel{Name: m.ID}
	}
	return nil
}

func (c *catalog) Disable(_ context.Context, up string, names []string) error {
	c.calls++
	for _, n := range names {
		m := c.models[up][n]
		m.Disabled = true
		c.models[up][n] = m
	}
	return nil
}

type lister struct {
	models []ir.ModelInfo
	err    error
}

func (l lister) ListModels(context.Context, *plugin.Upstream, *plugin.Credential) ([]ir.ModelInfo, error) {
	return l.models, l.err
}

type targets []Target

func (t targets) SyncTargets(context.Context) ([]Target, error) { return t, nil }

func ids(names ...string) []ir.ModelInfo {
	var out []ir.ModelInfo
	for _, n := range names {
		out = append(out, ir.ModelInfo{ID: n})
	}
	return out
}

func snapshot(c *catalog, up string) []CatalogModel {
	ms, _ := c.Models(context.Background(), up)
	return ms
}

func TestRule21(t *testing.T) {
	start := []CatalogModel{
		{Name: "a"},
		{Name: "b"},
		{Name: "gone-manual", Manual: true},
		{Name: "manual-off", Manual: true, Disabled: true},
		{Name: "old", Disabled: true},
	}
	tests := []struct {
		name  string
		list  lister
		want  []CatalogModel // nil = unchanged
		calls int
	}{
		{name: "fetch error changes nothing", list: lister{err: errors.New("503")}},
		{name: "empty list changes nothing", list: lister{models: nil}},
		{name: "gone models disabled, never deleted; manual untouched; reappearing re-enabled",
			list: lister{models: ids("a", "new", "manual-off", "old", "a")},
			want: []CatalogModel{
				{Name: "a"},
				{Name: "b", Disabled: true},
				{Name: "gone-manual", Manual: true},
				{Name: "manual-off", Manual: true, Disabled: true},
				{Name: "new"},
				{Name: "old"},
			}, calls: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat := newCatalog("u", start...)
			j := New(cat, targets{{Upstream: &plugin.Upstream{ID: "u"}, Lister: tt.list}})
			err := j.Run(context.Background())
			if (err != nil) != (tt.list.err != nil) {
				t.Fatalf("err = %v", err)
			}
			want := tt.want
			if want == nil {
				want = start
			}
			if got := snapshot(cat, "u"); !slices.Equal(got, want) {
				t.Errorf("\n got %+v\nwant %+v", got, want)
			}
			if cat.calls != tt.calls {
				t.Errorf("catalog writes = %d, want %d", cat.calls, tt.calls)
			}
		})
	}
}

func TestRunContinuesPastFailures(t *testing.T) {
	cat := &catalog{models: map[string]map[string]CatalogModel{"bad": {}, "good": {}, "nolist": {}}}
	j := New(cat, targets{
		{Upstream: &plugin.Upstream{ID: "bad"}, Lister: lister{err: errors.New("x")}},
		{Upstream: &plugin.Upstream{ID: "nolist"}},
		{Upstream: &plugin.Upstream{ID: "good"}, Lister: lister{models: ids("m")}},
	})
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("failure not reported")
	}
	if len(cat.models["good"]) != 1 {
		t.Fatal("good upstream not synced")
	}
}

func TestPlanNoopWhenInSync(t *testing.T) {
	up, dis := Plan([]CatalogModel{{Name: "a"}, {Name: "x", Disabled: true}}, ids("a"))
	if len(dis) != 0 || len(up) != 1 {
		t.Fatal(up, dis)
	}
}
