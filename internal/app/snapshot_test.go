package app

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// TestEndpointOverrideIssuer pins hard rule 7 for stored endpoint
// overrides: an endpoint that moved off the manifest's base URL, or was
// added without an issuer, gets the upstream's own issuer group, so it never
// shares opaque state with the official service or with another upstream.
func TestEndpointOverrideIssuer(t *testing.T) {
	ctx := context.Background()
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeAll)
	for _, tc := range []struct {
		name     string
		config   map[string]any
		override []plugin.Endpoint
		endpoint string
		want     string
	}{
		{"manifest as shipped", nil, nil, "messages", "anthropic"},
		{"same base URL", nil, []plugin.Endpoint{{ID: "messages", BaseURL: "https://api.anthropic.com/"}}, "messages", "anthropic"},
		{"base URL moved", nil, []plugin.Endpoint{{ID: "messages", BaseURL: "https://proxy.example.com"}}, "messages", "upstream:u"},
		{"base URL moved, endpoint issuer", nil,
			[]plugin.Endpoint{{ID: "messages", BaseURL: "https://proxy.example.com", Issuer: "bedrock"}}, "messages", "bedrock"},
		{"base URL moved, upstream issuer", map[string]any{"issuer": "vertex-anthropic"},
			[]plugin.Endpoint{{ID: "messages", BaseURL: "https://proxy.example.com"}}, "messages", "vertex-anthropic"},
		{"endpoint added without issuer", nil,
			[]plugin.Endpoint{{ID: "extra", Protocol: "anthropic", BaseURL: "https://other.example.com"}}, "extra", "upstream:u"},
		{"endpoint added with issuer", nil,
			[]plugin.Endpoint{{ID: "extra", Protocol: "anthropic", BaseURL: "https://other.example.com", Issuer: "anthropic"}}, "extra", "anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := &store.Upstream{ID: "u", Provider: "provider.anthropic", Enabled: true, Config: tc.config, Endpoints: tc.override}
			_, eps, err := a.endpoints(ctx, u)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range eps {
				if e.ID == tc.endpoint {
					if e.Issuer != tc.want {
						t.Fatalf("issuer %q, want %q", e.Issuer, tc.want)
					}
					return
				}
			}
			t.Fatalf("no endpoint %s in %+v", tc.endpoint, eps)
		})
	}
}

// TestSnapshotEgressAndPricing: an upstream's stored headers, proxy and
// timeouts reach the pipeline; groups carry their multiplier; manifest list
// prices back operator rules, and model rules are the list price.
func TestSnapshotEgressAndPricing(t *testing.T) {
	ctx := context.Background()
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeAll)
	st := a.Store()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// Stored as the admin API writes them, round-tripped through JSON.
	must(st.PutUpstream(ctx, &store.Upstream{ID: "u", Provider: "provider.anthropic", Enabled: true, Config: map[string]any{
		"_headers":  map[string]any{"X-Team": "ops"},
		"_proxy":    "http://proxy.internal:3128",
		"_timeouts": map[string]any{"connect_ms": 2000, "first_byte_ms": 1_800_000},
	}}))
	must(st.PutGroup(ctx, &store.Group{ID: "vip", Name: "VIP", PriceMultiplier: 1.5}))
	must(st.PutPrice(ctx, &store.PriceRule{Model: "fast", Input: 7, Output: 9}))
	must(a.Reload(ctx))
	snap := a.runtime().snap
	up := snap.Upstreams["u"]
	if up == nil {
		t.Fatal("upstream missing")
	}
	if up.Headers["X-Team"] != "ops" || up.Proxy != "http://proxy.internal:3128" ||
		up.Timeouts.Connect != 2*time.Second || up.Timeouts.FirstByte != 30*time.Minute || up.Timeouts.Idle != 0 {
		t.Errorf("egress overrides: %+v %q %+v", up.Headers, up.Proxy, up.Timeouts)
	}
	if snap.Multiplier("vip") != 1.5 || snap.Multiplier("none") != 0 {
		t.Errorf("multiplier vip=%v none=%v", snap.Multiplier("vip"), snap.Multiplier("none"))
	}
	if p, ok := snap.Price("u", "claude-opus-4-7"); !ok || p.Input != 5_000_000 || p.Output != 25_000_000 || p.CacheRead != 500_000 {
		t.Errorf("manifest list price: %+v %v", p, ok)
	}
	if _, ok := snap.Price("u", "not-listed"); ok {
		t.Error("unlisted model priced")
	}
	if p, ok := snap.ModelPrice("fast"); !ok || p.Input != 7 {
		t.Errorf("model rule: %+v %v", p, ok)
	}
	must(st.PutPrice(ctx, &store.PriceRule{UpstreamID: "u", UpstreamModel: "claude-opus-4-7", Input: 1, Output: 2}))
	must(a.Reload(ctx))
	if p, _ := a.runtime().snap.Price("u", "claude-opus-4-7"); p.Input != 1 {
		t.Errorf("operator rule must win over the manifest: %+v", p)
	}
}
