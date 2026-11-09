package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/features"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/selector/routing"
)

func newTestApp(t *testing.T) (*App, store.Store) {
	t.Helper()
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	a, err := New(context.Background(), &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeAll)
	return a, a.Store()
}

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Core modules always run and feature modules run with their feature,
// whatever the stored plugin configs say.
func TestFeatureStates(t *testing.T) {
	a, st := newTestApp(t)
	ctx := context.Background()
	for _, id := range append(slices.Clone(features.Core), "middleware.ratelimit") {
		mustOK(t, st.PutPluginConfig(ctx, &store.PluginConfig{PluginID: id, Enabled: false}))
	}
	mustOK(t, st.PutPluginConfig(ctx, &store.PluginConfig{PluginID: "meter.balance", Enabled: true}))
	mustOK(t, a.Reload(ctx))
	on := func(id string) bool { return a.runtime().states[id].enabled }
	rt := a.runtime()
	if sel := rt.chain.Selector; sel == nil || sel.Info().ID != routing.ID {
		t.Fatalf("selector %v", sel)
	}
	if rt.job("job.modelsync") == nil || rt.job("job.quotaprobe") == nil {
		t.Fatal("a job was switched off")
	}
	for _, id := range append(slices.Clone(features.Core), "middleware.ratelimit", "meter.quota", "sink.metrics") {
		if !on(id) {
			t.Errorf("%s is off", id)
		}
	}
	for _, id := range []string{"meter.balance", "admin.payment", "middleware.guardrails", "admin.signup"} {
		if on(id) {
			t.Errorf("%s is on", id)
		}
	}
	mustOK(t, st.PutFeature(ctx, store.FeatureState{ID: "limits", Enabled: false}))
	mustOK(t, st.PutFeature(ctx, store.FeatureState{ID: "billing", Enabled: true}))
	mustOK(t, a.Reload(ctx))
	for id, want := range map[string]bool{"middleware.ratelimit": false, "meter.quota": false, "middleware.param-policy": false,
		"meter.balance": true, "admin.payment": true, "admin.redeem": true, "admin.subscriptions": true, "admin.import-newapi": true} {
		if on(id) != want {
			t.Errorf("%s on = %v, want %v", id, on(id), want)
		}
	}
}

// Key limits are read from the key record and a group's body logging from
// the group record; plugin settings rows on those layers are ignored.
func TestRecordSettings(t *testing.T) {
	a, st := newTestApp(t)
	ctx := context.Background()
	mustOK(t, st.PutGroup(ctx, &store.Group{ID: "g", Name: "g", Settings: map[string]json.RawMessage{"log_bodies": json.RawMessage("true")}}))
	k := &store.Key{Name: "k", GroupID: "g", Settings: map[string]json.RawMessage{
		"rpm": json.RawMessage("30"), "max_concurrency": json.RawMessage("2"),
		"budget_monthly": json.RawMessage(`{"micro":5000000,"currency":"USD"}`)}}
	_, err := st.CreateKey(ctx, k)
	mustOK(t, err)
	row := func(scope plugin.Scope, id, pluginID, key, v string) {
		mustOK(t, st.PutSetting(ctx, store.SettingRow{Scope: scope, ScopeID: id, PluginID: pluginID, Key: key, Value: json.RawMessage(v)}))
	}
	row(plugin.Key, k.ID, "middleware.ratelimit", "rpm", "999")
	row(plugin.Key, k.ID, "middleware.ratelimit", "tpm", "1000")
	row(plugin.Group, "g", "sink.requestlog", "body_logging", "false")
	row(plugin.Global, "", "middleware.ratelimit", "rpm", "60")
	mustOK(t, a.Reload(ctx))
	c := k.Caller()
	for _, tc := range []struct {
		plugin, key string
		want        int64
	}{
		{"middleware.ratelimit", "rpm", 30},
		{"middleware.ratelimit", "concurrency", 2},
		{"middleware.ratelimit", "tpm", 1000},
		{"meter.quota", "key_monthly_micro", 5_000_000},
	} {
		if got := a.settingFor(&c, tc.plugin, tc.key).Int(); got != tc.want {
			t.Errorf("%s/%s = %d, want %d", tc.plugin, tc.key, got, tc.want)
		}
	}
	if !a.settingFor(&c, "sink.requestlog", "body_logging").Bool() {
		t.Error("group log_bodies not applied")
	}
	// Strictest still holds over the record: a lower global rpm wins.
	row(plugin.Global, "", "middleware.ratelimit", "rpm", "10")
	mustOK(t, a.Reload(ctx))
	if got := a.settingFor(&c, "middleware.ratelimit", "rpm").Int(); got != 10 {
		t.Errorf("rpm = %d, want the stricter global 10", got)
	}
	// plugrelay apply refuses rows on those layers instead of storing them.
	for _, doc := range []string{
		"settings:\n  - {scope: key, scope_id: " + k.ID + ", plugin: middleware.ratelimit, key: rpm, value: 5}\n",
		"settings:\n  - {scope: group, scope_id: g, plugin: sink.requestlog, key: body_logging, value: true}\n",
	} {
		if _, err := a.Apply(ctx, []byte(doc), true); err == nil || !strings.Contains(err.Error(), "itself") {
			t.Errorf("apply %q: %v", doc, err)
		}
	}
	// A built-in module is switched by its feature, not as a plugin.
	if _, err := a.Apply(ctx, []byte("plugins:\n  - {id: meter.balance, enabled: true}\n"), true); err == nil {
		t.Error("apply switched a built-in module")
	}
	if _, err := a.Apply(ctx, []byte("features:\n  - {id: billing, enabled: true}\n"), false); err != nil {
		t.Fatal(err)
	}
	if fs, _ := st.ListFeatures(ctx); len(fs) != 1 || !fs[0].Enabled {
		t.Errorf("features after apply: %v", fs)
	}
}

// Presets are listed like models to callers who may use one of their
// models; a caller sees nothing a preset lists outside its group's models.
func TestPresetSnapshot(t *testing.T) {
	a, st := newTestApp(t)
	ctx := context.Background()
	mustOK(t, st.PutUpstream(ctx, &store.Upstream{ID: "u", Provider: "provider.anthropic", Enabled: true}))
	for _, m := range []string{"claude-sonnet-4-5", "gpt-5.5"} {
		mustOK(t, st.PutModel(ctx, &store.Model{Name: m, Enabled: true, Info: ir.ModelInfo{ID: m, ContextWindow: 200000}}))
		mustOK(t, st.ReplaceRoutes(ctx, m, []store.Route{{UpstreamID: "u"}}))
	}
	mustOK(t, st.PutPreset(ctx, &store.Preset{Name: "auto-coding", Models: []string{"claude-sonnet-4-5", "gpt-5.5"}, Strategy: store.PresetFastest}))
	mustOK(t, st.PutGroup(ctx, &store.Group{ID: "gpt", Name: "gpt", Settings: map[string]json.RawMessage{"models": json.RawMessage(`["gpt-*"]`)}}))
	mustOK(t, st.PutGroup(ctx, &store.Group{ID: "none", Name: "none", Settings: map[string]json.RawMessage{"models": json.RawMessage(`["o3"]`)}}))
	mustOK(t, a.Reload(ctx))
	snap := a.runtime().snap
	i := slices.IndexFunc(snap.Models, func(m ir.ModelInfo) bool { return m.ID == "auto-coding" })
	if i < 0 || snap.Models[i].ContextWindow != 200000 {
		t.Fatalf("preset not listed with its first model's info: %+v", snap.Models)
	}
	if p := snap.Table.Presets["auto-coding"]; p.Strategy != "fastest" || len(p.Models) != 2 {
		t.Fatalf("table preset %+v", p)
	}
	if !snap.Visible(&plugin.Caller{GroupID: "gpt"}, "auto-coding") || snap.Visible(&plugin.Caller{GroupID: "none"}, "auto-coding") {
		t.Error("preset visibility does not follow its models")
	}
}

// Model sync runs per upstream: on by default where the manifest lists
// models, off by default where it lists none, and switchable either way.
func TestModelSyncPerUpstream(t *testing.T) {
	a, st := newTestApp(t)
	ctx := context.Background()
	up := func(id, manifest string, cfg map[string]any) {
		if cfg == nil {
			cfg = map[string]any{}
		}
		cfg["manifest"] = manifest
		mustOK(t, st.PutUpstream(ctx, &store.Upstream{ID: id, Provider: "provider.openai-compatible", Manifest: manifest, Enabled: true, Config: cfg}))
		// Without a usable credential an upstream that needs a key is never listed.
		mustOK(t, st.PutCredential(ctx, &store.Credential{ID: id + "-k", UpstreamID: id, Kind: store.CredShared, Secret: "sk"}))
	}
	up("lists", "anthropic", nil)
	up("lists-off", "anthropic", map[string]any{"_model_sync": false})
	up("silent", "baichuan", nil)
	up("silent-on", "baichuan", map[string]any{"_model_sync": true})
	mustOK(t, a.Reload(ctx))
	ts, err := targets{a, nil}.SyncTargets(ctx)
	mustOK(t, err)
	var got []string
	for _, tg := range ts {
		got = append(got, tg.Upstream.ID)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"lists", "silent-on"}) {
		t.Fatalf("synced %v", got)
	}
	// Asking for an upstream by id syncs it whatever its switch.
	if ts, _ := (targets{a, []string{"lists-off"}}).SyncTargets(ctx); len(ts) != 1 {
		t.Fatalf("explicit sync: %d targets", len(ts))
	}
}
