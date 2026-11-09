package pricesync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/plugins/internal/testkit"
)

// testdata/models.dev.json is a slice of the real catalog recorded in
// November 2026.
func fixture(t *testing.T) Catalog {
	t.Helper()
	f, err := os.Open("testdata/models.dev.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	c, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func byModel(s []Suggestion) map[string]Suggestion {
	out := map[string]Suggestion{}
	for _, x := range s {
		out[x.Model] = x
	}
	return out
}

func TestMatch(t *testing.T) {
	c := fixture(t)
	models := []Model{
		// The route's own provider lists the upstream name.
		{Name: "sonnet", Routes: []Route{{Provider: "anthropic", UpstreamModel: "claude-sonnet-4-6"}}},
		// A cloud's region-prefixed name: its provider, under the catalog's id, lists it.
		{Name: "haiku-bedrock", Routes: []Route{{Provider: "bedrock", UpstreamModel: "us.anthropic.claude-haiku-4-5-20251001-v1:0"}}},
		// A provider the catalog does not know: the maker's entry by name.
		{Name: "gpt-5.4", Routes: []Route{{Provider: "my-proxy", UpstreamModel: "gpt-5.4"}}},
		// A vendor path is taken off before the makers are tried.
		{Name: "codex", Routes: []Route{{Provider: "new-api", UpstreamModel: "openai/gpt-5.3-codex"}}},
		// No route: our own name.
		{Name: "deepseek-v4-flash"},
		// Not in the catalog.
		{Name: "kimi-k2.5", Routes: []Route{{Provider: "moonshot-kimi-intl", UpstreamModel: "kimi-k2.5"}}},
	}
	got := byModel(Match(c, models, 1, "USD"))
	if len(got) != 5 {
		t.Fatalf("matched %d: %+v", len(got), got)
	}
	s := got["sonnet"]
	if s.Ref != "anthropic/claude-sonnet-4-6" || s.Input != 3_000_000 || s.Output != 15_000_000 || s.CacheRead != 300_000 ||
		s.CacheWrite5m != 3_750_000 || s.CacheWrite1h != 6_000_000 || s.Currency != "USD" {
		t.Fatalf("sonnet %+v", s)
	}
	if s.Info.ContextWindow != 1_000_000 || s.Info.MaxOutput != 128_000 || !s.Info.Reasoning || !s.Info.Vision || !s.Info.Tools {
		t.Fatalf("sonnet hints %+v", s.Info)
	}
	if h := got["haiku-bedrock"]; h.Ref != "amazon-bedrock/us.anthropic.claude-haiku-4-5-20251001-v1:0" || h.Input != 1_100_000 {
		t.Fatalf("bedrock %+v", h)
	}
	if g := got["gpt-5.4"]; g.Ref != "openai/gpt-5.4" || g.CacheWrite5m != 0 || g.CacheWrite1h != 0 {
		t.Fatalf("gpt %+v", g)
	}
	if g := got["codex"]; g.Ref != "openai/gpt-5.3-codex" {
		t.Fatalf("codex %+v", g)
	}
	if g := got["deepseek-v4-flash"]; g.Ref != "deepseek/deepseek-v4-flash" {
		t.Fatalf("deepseek %+v", g)
	}
}

func TestMatchConvertsCurrency(t *testing.T) {
	got := byModel(Match(fixture(t), []Model{{Name: "claude-haiku-4-5"}}, 7.1, "CNY"))
	if h := got["claude-haiku-4-5"]; h.Input != 7_100_000 || h.Output != 35_500_000 || h.Currency != "CNY" {
		t.Fatalf("%+v", h)
	}
}

func TestBare(t *testing.T) {
	for in, want := range map[string]string{
		"us.anthropic.claude-sonnet-4-6": "claude-sonnet-4-6", "openai/gpt-5.4": "gpt-5.4", "gpt-5.4": "gpt-5.4",
		"global.anthropic.claude-opus-4-8": "claude-opus-4-8",
	} {
		if got := bare(in); got != want {
			t.Errorf("bare(%q) = %q, want %q", in, got, want)
		}
	}
}

type memStore struct {
	models []Model
	cur    string
	saved  []Suggestion
}

func (m *memStore) Models(context.Context) ([]Model, error) { return m.models, nil }
func (m *memStore) Currency(context.Context) string         { return m.cur }
func (m *memStore) Save(_ context.Context, s []Suggestion, _ time.Time) error {
	m.saved = s
	return nil
}

func TestRun(t *testing.T) {
	body, err := os.ReadFile("testdata/models.dev.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	st := &memStore{models: []Model{{Name: "claude-opus-4-8"}}, cur: "USD"}
	h := &testkit.Host{Set: testkit.Settings{"url": srv.URL}, Store: testkit.NewState(time.Now)}
	j := New(st)
	if err := j.Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if err := j.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st.saved) != 1 || st.saved[0].Output != 25_000_000 {
		t.Fatalf("saved %+v", st.saved)
	}
	b, ok, _ := h.KV().Get(context.Background(), StatusKey)
	if !ok || !strings.Contains(string(b), `"matched":1`) {
		t.Fatalf("status %s", b)
	}

	// Another currency without a rate stops before downloading anything.
	st2 := &memStore{models: st.models, cur: "CNY"}
	j2 := New(st2)
	if err := j2.Init(context.Background(), &testkit.Host{Set: testkit.Settings{"url": srv.URL}, Store: testkit.NewState(time.Now)}); err != nil {
		t.Fatal(err)
	}
	if err := j2.Run(context.Background()); err == nil || st2.saved != nil {
		t.Fatalf("ran without a rate: %v", err)
	}
}
