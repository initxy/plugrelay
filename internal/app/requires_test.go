package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

type infoOnly plugin.Info

func (p infoOnly) Info() plugin.Info { return plugin.Info(p) }

// seedPlugins stores plugin configs in the store cfg names before the
// relay opens it.
func seedPlugins(t *testing.T, cfg *config.Config, cfgs ...store.PluginConfig) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{Driver: cfg.Store.Driver, DSN: cfg.Store.DSN})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := range cfgs {
		if err := st.PutPluginConfig(ctx, &cfgs[i]); err != nil {
			t.Fatal(err)
		}
	}
}

// seedFeature stores a feature state the same way.
func seedFeature(t *testing.T, cfg *config.Config, f store.FeatureState) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Config{Driver: cfg.Store.Driver, DSN: cfg.Store.DSN})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.PutFeature(ctx, f); err != nil {
		t.Fatal(err)
	}
}

func on(ids ...string) []store.PluginConfig {
	var out []store.PluginConfig
	for _, id := range ids {
		out = append(out, store.PluginConfig{PluginID: id, Enabled: true})
	}
	return out
}

// orphanMeterID requires a plugin that is not installed.
const orphanMeterID = "meter.orphan-test"

type orphanMeter struct{ plugin.Meter }

func (orphanMeter) Info() plugin.Info {
	return plugin.Info{ID: orphanMeterID, Kind: plugin.KindMeter, Version: "0", Requires: []string{"sink.not-installed"}}
}

// TestRequiresResolution checks plugin.Info.Requires: a required plugin is
// turned on with its dependent. A requirement that is not installed turns
// a fail-open dependent off, and is an error for a fail-closed one, which
// must neither run without it nor vanish silently.
func TestRequiresResolution(t *testing.T) {
	known := []plugin.Plugin{
		infoOnly{ID: "meter.budget", Kind: plugin.KindMeter, Requires: []string{"sink.ledger"}},
		infoOnly{ID: "sink.ledger", Kind: plugin.KindSink},
		infoOnly{ID: "meter.orphan", Kind: plugin.KindMeter, Requires: []string{"sink.missing"}},
		infoOnly{ID: "middleware.notes", Kind: plugin.KindMiddleware, FailPolicy: plugin.FailOpen, Requires: []string{"sink.ledger"}},
		infoOnly{ID: "middleware.chain", Kind: plugin.KindMiddleware, FailPolicy: plugin.FailOpen, Requires: []string{"sink.missing"}},
		infoOnly{ID: "meter.top", Kind: plugin.KindMeter, Requires: []string{"middleware.chain"}},
	}
	ledgerOff := store.PluginConfig{PluginID: "sink.ledger"}
	for _, tc := range []struct {
		name   string
		stored []store.PluginConfig
		on     []string
		err    string
	}{
		{"requirement turned on", on("meter.budget"), []string{"meter.budget", "sink.ledger"}, ""},
		{"stored-off requirement turned on", append(on("meter.budget"), ledgerOff), []string{"meter.budget", "sink.ledger"}, ""},
		{"fail-open dependent turned off", on("middleware.chain"), nil, ""},
		{"fail-closed requirement not installed", on("meter.orphan"), nil,
			"meter.orphan requires sink.missing, which is not installed"},
		{"fail-closed requirement blocked by its own", on("meter.top"), nil,
			"meter.top requires middleware.chain, which is missing a plugin it requires itself"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			a := &App{cfg: &cfg, log: slog.New(slog.DiscardHandler)}
			st, err := a.states(known, tc.stored, nil, a.log)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range known {
				id := p.Info().ID
				want := false
				for _, o := range tc.on {
					want = want || o == id
				}
				if st[id].enabled != want {
					t.Errorf("%s enabled=%v, want %v", id, st[id].enabled, want)
				}
			}
		})
	}
}

func init() { plugin.Register(orphanMeterID, func() plugin.Plugin { return orphanMeter{} }) }

func requiresConfig(t *testing.T) *config.Config {
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	return &cfg
}

// TestStartupRefusesMissingRequirement: a fail-closed plugin whose
// requirement is not installed stops startup. A stored-off requirement is
// turned on instead: meter.quota bills from sink.requestlog.
func TestStartupRefusesMissingRequirement(t *testing.T) {
	cfg := requiresConfig(t)
	seedPlugins(t, cfg, on(orphanMeterID)...)
	_, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), orphanMeterID+" requires sink.not-installed") {
		t.Fatalf("err %v", err)
	}

	cfg = requiresConfig(t)
	seedPlugins(t, cfg, store.PluginConfig{PluginID: "sink.requestlog"})
	a, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeAll)
	if !a.cur.Load().states["sink.requestlog"].enabled {
		t.Fatal("requirement of meter.quota not turned on")
	}
}

// TestReloadKeepsRuntimeOnRequiresConflict: a reload that hits the same
// conflict is rejected and the running configuration stays.
func TestReloadKeepsRuntimeOnRequiresConflict(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx, requiresConfig(t), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeAll)
	before := a.cur.Load()
	if err := a.store.PutPluginConfig(ctx, &store.PluginConfig{PluginID: orphanMeterID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.rebuild(ctx, true); err == nil || !strings.Contains(err.Error(), orphanMeterID+" requires sink.not-installed") {
		t.Fatalf("err %v", err)
	}
	if a.cur.Load() != before {
		t.Fatal("runtime replaced by a rejected reload")
	}
}

const (
	brokenSinkID = "sink.broken-init-test"
	dependentID  = "meter.dependent-test"
	optionalID   = "middleware.dependent-test"
)

// brokenInitSink fails its Init, so it never runs.
type brokenInitSink struct{}

func (brokenInitSink) Info() plugin.Info {
	return plugin.Info{ID: brokenSinkID, Kind: plugin.KindSink, Version: "0", FailPolicy: plugin.FailClosed}
}
func (brokenInitSink) Init(context.Context, plugin.Host) error {
	return errors.New("cannot open the ledger")
}
func (brokenInitSink) Consume(context.Context, *plugin.Record) error { return nil }

type dependentMeter struct{ plugin.Meter }

func (dependentMeter) Info() plugin.Info {
	return plugin.Info{ID: dependentID, Kind: plugin.KindMeter, Version: "0", Requires: []string{brokenSinkID}}
}

type dependentMiddleware struct{}

func (dependentMiddleware) Info() plugin.Info {
	return plugin.Info{ID: optionalID, Kind: plugin.KindMiddleware, Version: "0", FailPolicy: plugin.FailOpen,
		Requires: []string{dependentID}}
}

func init() {
	plugin.Register(brokenSinkID, func() plugin.Plugin { return brokenInitSink{} })
	plugin.Register(dependentID, func() plugin.Plugin { return dependentMeter{} })
	plugin.Register(optionalID, func() plugin.Plugin { return dependentMiddleware{} })
}

// TestDependentsOfFailedPluginSkipped: when a required plugin fails to
// initialize, the plugins that require it (directly or through another)
// do not run either, and the console shows why.
func TestDependentsOfFailedPluginSkipped(t *testing.T) {
	cfg := requiresConfig(t)
	seedPlugins(t, cfg, on(optionalID, dependentID)...)
	seedFeature(t, cfg, store.FeatureState{ID: "limits"})
	a, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeAll)
	rt := a.runtime()
	if rt.chain.Meter != nil {
		t.Errorf("meter %s runs without the sink it requires", rt.chain.Meter.Info().ID)
	}
	for _, p := range rt.chain.Plugins {
		if p.Info().ID == optionalID {
			t.Errorf("%s runs although its requirement %s does not", optionalID, dependentID)
		}
	}
	for _, s := range rt.sinks {
		if s.Info().ID == brokenSinkID {
			t.Error("sink with a failed Init is running")
		}
	}
	want := map[string]string{
		brokenSinkID: "cannot open the ledger",
		dependentID:  "requires " + brokenSinkID + ", which is not running",
		optionalID:   "requires " + dependentID + ", which is not running",
	}
	for _, e := range a.entries() {
		if detail, ok := want[e.Info.ID]; ok && (e.Status != "crashed" || !strings.Contains(e.StatusDetail, detail)) {
			t.Errorf("%s: status %q (%s), want crashed (%s)", e.Info.ID, e.Status, e.StatusDetail, detail)
		}
	}
}

// TestConsoleRefusesRequiresConflict: turning a fail-closed plugin on in the
// console while its requirement is not installed is refused before it is
// stored; a conflict that reached the store anyway shows as the reload
// error.
func TestConsoleRefusesRequiresConflict(t *testing.T) {
	const token = "admin-test-token"
	t.Setenv(EnvAdminToken, token)
	ctx := context.Background()
	a, err := New(ctx, requiresConfig(t), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeAll)
	if err := a.store.CreateUser(ctx, &store.User{Name: "admin", Email: "admin", Role: store.RoleAdmin}, "correct-horse-1"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler().Handler())
	t.Cleanup(srv.Close)
	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, out := call("PATCH", "/api/v1/extensions/"+orphanMeterID, `{"enabled":true}`)
	if code != 409 || !strings.Contains(fmt.Sprint(out), orphanMeterID+" requires sink.not-installed, which is not installed") {
		t.Fatalf("PATCH: %d %v", code, out)
	}
	if cfgs, _ := a.store.ListPluginConfigs(ctx); len(cfgs) != 0 {
		t.Fatalf("refused change stored: %+v", cfgs)
	}
	if code, out := call("PATCH", "/api/v1/features/metrics", `{"enabled":false}`); code != 200 {
		t.Fatalf("unrelated change refused: %d %v", code, out)
	}

	if err := a.store.PutPluginConfig(ctx, &store.PluginConfig{PluginID: orphanMeterID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(ctx); err == nil {
		t.Fatal("conflicting reload applied")
	}
	if _, out := call("GET", "/api/v1/extensions", ""); !strings.Contains(fmt.Sprint(out["reload_error"]), orphanMeterID+" requires sink.not-installed") {
		t.Fatalf("reload_error %v", out["reload_error"])
	}
	if err := a.store.DeletePluginConfig(ctx, orphanMeterID); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, out := call("GET", "/api/v1/extensions", ""); out["reload_error"] != nil {
		t.Fatalf("reload_error after a good reload: %v", out["reload_error"])
	}
}
