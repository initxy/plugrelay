package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

type opener func(t *testing.T, box *secret.Box) store.Store

func newBox(t *testing.T) *secret.Box {
	t.Helper()
	k, _ := secret.GenerateMasterKey()
	mk, _ := secret.ParseMasterKey(k)
	b, err := secret.NewBox(mk)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sqliteOpener(t *testing.T, box *secret.Box) store.Store {
	s, err := store.Open(context.Background(), store.Config{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "p.db"), Secrets: box})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func pgOpener(dsn string) opener {
	return func(t *testing.T, box *secret.Box) store.Store {
		ctx := context.Background()
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		schema := fmt.Sprintf("store_test_%d", time.Now().UnixNano())
		if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		s, err := store.Open(ctx, store.Config{Driver: "postgres", DSN: dsn + sep + "search_path=" + schema, Secrets: box})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			s.Close()
			_, _ = admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE")
			admin.Close()
		})
		return s
	}
}

// mysqlOpener gives each test its own database on the server dsn names.
func mysqlOpener(dsn string) opener {
	return func(t *testing.T, box *secret.Box) store.Store {
		ctx := context.Background()
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			t.Fatal(err)
		}
		admin, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.DBName = fmt.Sprintf("store_test_%d", time.Now().UnixNano())
		if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+cfg.DBName); err != nil {
			t.Fatal(err)
		}
		s, err := store.Open(ctx, store.Config{Driver: "mysql", DSN: cfg.FormatDSN(), Secrets: box})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			s.Close()
			_, _ = admin.ExecContext(ctx, "DROP DATABASE "+cfg.DBName)
			admin.Close()
		})
		return s
	}
}

// mysqlFromEnv skips unless PLUGRELAY_TEST_MYSQL holds a go-sql-driver DSN.
func mysqlFromEnv(t *testing.T) opener {
	dsn := os.Getenv("PLUGRELAY_TEST_MYSQL")
	if dsn == "" {
		t.Skip("PLUGRELAY_TEST_MYSQL not set")
	}
	return mysqlOpener(dsn)
}

func TestSQLite(t *testing.T) { conformance(t, sqliteOpener) }

func TestMySQL(t *testing.T) { conformance(t, mysqlFromEnv(t)) }

func TestPostgres(t *testing.T) {
	dsn := os.Getenv("PLUGRELAY_TEST_PG")
	if dsn == "" {
		t.Skip("PLUGRELAY_TEST_PG not set")
	}
	conformance(t, pgOpener(dsn))
}

func TestSQLiteReopen(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "r.db")
	s, err := store.Open(ctx, store.Config{Driver: "sqlite", DSN: p})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutGroup(ctx, &store.Group{ID: "g", Name: "G"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = store.Open(ctx, store.Config{Driver: "sqlite", DSN: p})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.GetGroup(ctx, "g"); err != nil {
		t.Fatal(err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func conformance(t *testing.T, open opener) {
	ctx := context.Background()
	run := func(name string, f func(t *testing.T, s store.Store, box *secret.Box)) {
		t.Run(name, func(t *testing.T) {
			box := newBox(t)
			f(t, open(t, box), box)
		})
	}

	run("Users", func(t *testing.T, s store.Store, _ *secret.Box) {
		u := &store.User{Name: "Ann", Email: "ann@example.com", Role: store.RoleAdmin}
		must(t, s.CreateUser(ctx, u, "pw1"))
		if u.ID == "" {
			t.Fatal("no id")
		}
		err := s.CreateUser(ctx, &store.User{Name: "Dup", Email: "ann@example.com", Role: store.RoleUser}, "")
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate email: %v", err)
		}
		must(t, s.CreateUser(ctx, &store.User{Name: "NoMail1", Role: store.RoleUser}, ""))
		must(t, s.CreateUser(ctx, &store.User{Name: "NoMail2", Role: store.RoleUser}, ""))
		if err := s.CreateUser(ctx, &store.User{Name: "x", Role: "root"}, ""); err == nil {
			t.Fatal("bad role accepted")
		}
		got, err := s.Authenticate(ctx, "ann@example.com", "pw1")
		must(t, err)
		if got.ID != u.ID || got.Role != store.RoleAdmin {
			t.Fatalf("%+v", got)
		}
		for _, c := range [][2]string{{"ann@example.com", "bad"}, {"nobody@example.com", "pw1"}, {"", ""}} {
			if _, err := s.Authenticate(ctx, c[0], c[1]); !errors.Is(err, store.ErrInvalidLogin) {
				t.Fatalf("%v: %v", c, err)
			}
		}
		tok, err := s.CreateSession(ctx, u.ID, time.Hour)
		must(t, err)
		if su, err := s.SessionUser(ctx, tok); err != nil || su.ID != u.ID {
			t.Fatalf("session: %v", err)
		}
		if _, err := s.SessionUser(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		expired, _ := s.CreateSession(ctx, u.ID, -time.Second)
		if _, err := s.SessionUser(ctx, expired); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("expired session resolved")
		}
		must(t, s.SetPassword(ctx, u.ID, "pw2"))
		if _, err := s.SessionUser(ctx, tok); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("session survived password change")
		}
		if _, err := s.Authenticate(ctx, "ann@example.com", "pw2"); err != nil {
			t.Fatal(err)
		}
		u.Disabled = true
		must(t, s.UpdateUser(ctx, u))
		if _, err := s.Authenticate(ctx, "ann@example.com", "pw2"); !errors.Is(err, store.ErrInvalidLogin) {
			t.Fatal("disabled user logged in")
		}
		tok2, _ := s.CreateSession(ctx, u.ID, time.Hour)
		if _, err := s.SessionUser(ctx, tok2); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("disabled user session resolved")
		}
		users, err := s.ListUsers(ctx)
		must(t, err)
		if len(users) != 3 {
			t.Fatalf("users: %d", len(users))
		}
		must(t, s.DeleteUser(ctx, u.ID))
		if _, err := s.GetUser(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		if err := s.UpdateUser(ctx, u); !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		must(t, s.DeleteSession(ctx, tok2))
	})

	run("GroupsAndVersion", func(t *testing.T, s store.Store, _ *secret.Box) {
		v0, err := s.Version(ctx)
		must(t, err)
		g := &store.Group{Name: "vip", Settings: map[string]json.RawMessage{"models": json.RawMessage(`["a"]`)}, PriceMultiplier: 1.5}
		must(t, s.PutGroup(ctx, g))
		v1, _ := s.Version(ctx)
		if v1 <= v0 {
			t.Fatal("version not bumped")
		}
		got, err := s.GetGroup(ctx, g.ID)
		must(t, err)
		if got.PriceMultiplier != 1.5 || string(got.Settings["models"]) != `["a"]` {
			t.Fatalf("%+v", got)
		}
		g.Name = "VIP"
		must(t, s.PutGroup(ctx, g))
		gs, _ := s.ListGroups(ctx)
		if len(gs) != 1 || gs[0].Name != "VIP" {
			t.Fatalf("%+v", gs)
		}
		must(t, s.DeleteGroup(ctx, g.ID))
		if err := s.DeleteGroup(ctx, g.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
	})

	run("Keys", func(t *testing.T, s store.Store, _ *secret.Box) {
		k := &store.Key{Name: "laptop", UserID: "u1", GroupID: "g1", Exceptions: []string{"middleware.ratelimit/rpm"},
			Settings: map[string]json.RawMessage{"ip_allowlist": json.RawMessage(`["10.0.0.0/8"]`)}}
		plain, err := s.CreateKey(ctx, k)
		must(t, err)
		if !secret.ValidKeyFormat(plain) || k.Last4 != plain[len(plain)-4:] {
			t.Fatalf("plain %q last4 %q", plain, k.Last4)
		}
		got, err := s.KeyByToken(ctx, plain)
		must(t, err)
		if got.ID != k.ID || got.Exceptions[0] != "middleware.ratelimit/rpm" || got.Caller().GroupID != "g1" {
			t.Fatalf("%+v", got)
		}
		if _, err := s.KeyByToken(ctx, "pr_"+strings.Repeat("a", 32)); !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		if _, err := s.KeyByToken(ctx, "garbage"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		k.ExpiresAt = time.Now().Add(-time.Minute)
		must(t, s.UpdateKey(ctx, k))
		got, _ = s.GetKey(ctx, k.ID)
		if got.Usable(time.Now()) {
			t.Fatal("expired key usable")
		}
		_, err = s.CreateKey(ctx, &store.Key{Name: "other", UserID: "u2"})
		must(t, err)
		ks, _ := s.ListKeys(ctx, store.KeyFilter{UserID: "u1"})
		if len(ks) != 1 {
			t.Fatalf("filter: %d", len(ks))
		}
		ks, _ = s.ListKeys(ctx, store.KeyFilter{})
		if len(ks) != 2 {
			t.Fatalf("all: %d", len(ks))
		}
		at := time.Now().Truncate(time.Millisecond)
		s.TouchKey(k.ID, at.Add(-time.Minute))
		s.TouchKey(k.ID, at)
		must(t, s.FlushTouches(ctx))
		got, _ = s.GetKey(ctx, k.ID)
		if !got.LastUsedAt.Equal(at) {
			t.Fatalf("last used %v want %v", got.LastUsedAt, at)
		}
		must(t, s.DeleteKey(ctx, k.ID))
		if _, err := s.KeyByToken(ctx, plain); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("deleted key found")
		}
	})

	run("UpstreamsCredentials", func(t *testing.T, s store.Store, box *secret.Box) {
		u := &store.Upstream{ID: "openai", Provider: "provider.openai", Enabled: true, Source: store.SourceOfficial,
			Endpoints: []plugin.Endpoint{{ID: "chat", Protocol: "openai.chat", BaseURL: "https://api.openai.com/v1", Issuer: "openai",
				Caps: ir.Capabilities{}}},
			Config: map[string]any{"org": "x"}}
		must(t, s.PutUpstream(ctx, u))
		got, err := s.GetUpstream(ctx, "openai")
		must(t, err)
		if len(got.Endpoints) != 1 || got.Endpoints[0].BaseURL != "https://api.openai.com/v1" || got.Config["org"] != "x" {
			t.Fatalf("%+v", got)
		}
		if pu := got.Plugin(); pu.Provider != "provider.openai" {
			t.Fatal(pu)
		}
		c := &store.Credential{UpstreamID: "openai", Kind: store.CredShared, Secret: "sk-1", MaxConcurrency: 4, Meta: map[string]string{"tier": "5"}}
		must(t, s.PutCredential(ctx, c))
		if c.Secret != "" || c.Sealed == "" || strings.Contains(c.Sealed, "sk-1") {
			t.Fatal("secret not sealed")
		}
		gc, err := s.GetCredential(ctx, c.ID)
		must(t, err)
		pc, err := gc.Open(box)
		must(t, err)
		if pc.Secret != "sk-1" || pc.MaxConcurrency != 4 || pc.Meta["tier"] != "5" || gc.Status != store.CredActive {
			t.Fatalf("%+v", pc)
		}
		gc.Proxy = "socks5://p:1080"
		must(t, s.PutCredential(ctx, gc)) // no Secret: keeps it
		gc, _ = s.GetCredential(ctx, c.ID)
		if pc, _ := gc.Open(box); pc.Secret != "sk-1" || pc.Proxy != "socks5://p:1080" {
			t.Fatalf("%+v", pc)
		}
		if err := s.PutCredential(ctx, &store.Credential{UpstreamID: "openai", Kind: store.CredPersonal}); err == nil {
			t.Fatal("personal credential without owner accepted")
		}
		until := time.Now().Add(time.Hour).Truncate(time.Millisecond)
		must(t, s.SetCredentialStatus(ctx, c.ID, store.CredQuarantined, "suspected ban", until))
		gc, _ = s.GetCredential(ctx, c.ID)
		if gc.Status != store.CredQuarantined || !gc.StatusUntil.Equal(until) || gc.StatusReason != "suspected ban" {
			t.Fatalf("%+v", gc)
		}
		must(t, s.SetCredentialSecret(ctx, c.ID, "sk-2", until))
		gc, _ = s.GetCredential(ctx, c.ID)
		if pc, _ := gc.Open(box); pc.Secret != "sk-2" || !gc.ExpiresAt.Equal(until) {
			t.Fatal("refresh")
		}
		if _, err := gc.Open(newBox(t)); err == nil {
			t.Fatal("opened with the wrong key")
		}
		must(t, s.PutCredential(ctx, &store.Credential{UpstreamID: "other", Kind: store.CredService}))
		cs, _ := s.ListCredentials(ctx, "openai")
		if len(cs) != 1 {
			t.Fatalf("list: %d", len(cs))
		}
		must(t, s.ReplaceRoutes(ctx, "gpt", []store.Route{{UpstreamID: "openai"}}))
		must(t, s.DeleteUpstream(ctx, "openai"))
		if cs, _ := s.ListCredentials(ctx, "openai"); len(cs) != 0 {
			t.Fatal("credentials not cascaded")
		}
		if rs, _ := s.ListRoutes(ctx, "gpt"); len(rs) != 0 {
			t.Fatal("routes not cascaded")
		}
	})

	t.Run("NoBox", func(t *testing.T) {
		s := open(t, nil)
		err := s.PutCredential(ctx, &store.Credential{UpstreamID: "u", Kind: store.CredShared, Secret: "x"})
		if !errors.Is(err, store.ErrNoBox) {
			t.Fatal(err)
		}
		must(t, s.PutCredential(ctx, &store.Credential{UpstreamID: "u", Kind: store.CredShared}))
	})

	run("Catalog", func(t *testing.T, s store.Store, _ *secret.Box) {
		m := &store.Model{Name: "gpt-5.5", Enabled: true,
			Info: ir.ModelInfo{ID: "gpt-5.5", DisplayName: "GPT 5.5", ContextWindow: 400000, Tools: true, Efforts: []string{"low", "high"}}}
		must(t, s.PutModel(ctx, m))
		must(t, s.PutPreset(ctx, &store.Preset{Name: "fast", Models: []string{"gpt-5.5", "gpt-5-mini"}, Strategy: store.PresetCheapest}))
		if err := s.PutPreset(ctx, &store.Preset{Name: "loop", Models: []string{"loop"}}); err == nil {
			t.Fatal("self-listing preset accepted")
		}
		if err := s.PutPreset(ctx, &store.Preset{Name: "gpt-5.5", Models: []string{"x"}}); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("preset named like a model: %v", err)
		}
		if err := s.PutModel(ctx, &store.Model{Name: "fast"}); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("model named like a preset: %v", err)
		}
		if err := s.PutPreset(ctx, &store.Preset{Name: "odd", Models: []string{"x"}, Strategy: "random"}); err == nil {
			t.Fatal("unknown strategy accepted")
		}
		got, err := s.GetModel(ctx, "gpt-5.5")
		must(t, err)
		if got.Info.ContextWindow != 400000 || len(got.Info.Efforts) != 2 || got.Info.DisplayName != "GPT 5.5" {
			t.Fatalf("%+v", got)
		}
		must(t, s.ReplaceRoutes(ctx, "gpt-5.5", []store.Route{
			{UpstreamID: "openrouter", UpstreamModel: "openai/gpt-5.5", Priority: 5, Groups: []string{"vip"}},
			{UpstreamID: "openai", UpstreamModel: "gpt-5.5-2026", Priority: 10, Weight: 3},
			{UpstreamID: "azure", UpstreamModel: "gpt-55", Priority: 10, Weight: 1},
		}))
		rs, _ := s.ListRoutes(ctx, "gpt-5.5")
		if len(rs) != 3 || rs[0].UpstreamID != "openai" || rs[1].UpstreamID != "azure" || rs[2].Groups[0] != "vip" || rs[1].Weight != 1 {
			t.Fatalf("%+v", rs)
		}
		must(t, s.ReplaceRoutes(ctx, "gpt-5.5", []store.Route{{UpstreamID: "openai"}}))
		rs, _ = s.ListRoutes(ctx, "")
		if len(rs) != 1 || rs[0].UpstreamModel != "gpt-5.5" {
			t.Fatalf("%+v", rs)
		}
		must(t, s.PutPrice(ctx, &store.PriceRule{Model: "gpt-5.5", Input: 1_250_000, Output: 10_000_000, CacheRead: 125_000}))
		must(t, s.PutPrice(ctx, &store.PriceRule{UpstreamID: "openai", UpstreamModel: "gpt-5.5-2026", Input: 1_000_000}))
		must(t, s.PutPrice(ctx, &store.PriceRule{Model: "gpt-5.5", Input: 2_000_000, Output: 10_000_000}))
		ps, _ := s.ListPrices(ctx)
		if len(ps) != 2 {
			t.Fatalf("%+v", ps)
		}
		if err := s.PutPrice(ctx, &store.PriceRule{}); err == nil {
			t.Fatal("empty price accepted")
		}
		must(t, s.DeletePrice(ctx, "gpt-5.5", "", ""))
		must(t, s.DeleteModel(ctx, "gpt-5.5"))
		if rs, _ := s.ListRoutes(ctx, ""); len(rs) != 0 {
			t.Fatal("routes not deleted with model")
		}
		ps2, _ := s.ListPresets(ctx)
		if len(ps2) != 1 || ps2[0].Strategy != store.PresetCheapest || len(ps2[0].Models) != 2 {
			t.Fatalf("%+v", ps2)
		}
		if snap, _ := s.Snapshot(ctx); len(snap.Presets) != 1 {
			t.Fatalf("snapshot presets: %+v", snap.Presets)
		}
		must(t, s.DeletePreset(ctx, "fast"))
		if _, err := s.GetPreset(ctx, "fast"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
	})

	run("PluginsSettingsManifests", func(t *testing.T, s store.Store, _ *secret.Box) {
		must(t, s.PutPluginConfig(ctx, &store.PluginConfig{PluginID: "sink.log", Enabled: true, Order: 5, Config: map[string]any{"a": 1.0}}))
		must(t, s.PutPluginConfig(ctx, &store.PluginConfig{PluginID: "auth.key", Enabled: true}))
		ps, _ := s.ListPluginConfigs(ctx)
		if len(ps) != 2 || ps[0].PluginID != "auth.key" || ps[1].Config["a"] != 1.0 {
			t.Fatalf("%+v", ps)
		}
		must(t, s.DeletePluginConfig(ctx, "auth.key"))
		rows := []store.SettingRow{
			{Scope: plugin.Global, PluginID: "rl", Key: "rpm", Value: json.RawMessage(`100`)},
			{Scope: plugin.Group, ScopeID: "g1", PluginID: "rl", Key: "rpm", Value: json.RawMessage(`50`)},
			{Scope: plugin.Key, ScopeID: "k1", PluginID: "rl", Key: "rpm", Value: json.RawMessage(`10`)},
			{Scope: plugin.Key, ScopeID: "k1", PluginID: "other", Key: "list", Value: json.RawMessage(`["a", "b"]`)},
		}
		for _, r := range rows {
			must(t, s.PutSetting(ctx, r))
		}
		if err := s.PutSetting(ctx, store.SettingRow{Scope: plugin.Key, PluginID: "rl", Key: "rpm", Value: json.RawMessage(`1`)}); err == nil {
			t.Fatal("key scope without id accepted")
		}
		if err := s.PutSetting(ctx, store.SettingRow{Scope: plugin.Global, PluginID: "rl", Key: "rpm", Value: json.RawMessage(`{`)}); err == nil {
			t.Fatal("bad json accepted")
		}
		must(t, s.PutSetting(ctx, store.SettingRow{Scope: plugin.Group, ScopeID: "g1", PluginID: "rl", Key: "rpm", Value: json.RawMessage(`40`)}))
		all, _ := s.ListSettings(ctx, store.SettingFilter{})
		if len(all) != 4 {
			t.Fatalf("%+v", all)
		}
		ks, _ := s.ListSettings(ctx, store.SettingFilter{Scope: plugin.Key, ScopeID: "k1"})
		if len(ks) != 2 {
			t.Fatalf("%+v", ks)
		}
		rl, _ := s.ListSettings(ctx, store.SettingFilter{PluginID: "rl", Scope: plugin.Group})
		if len(rl) != 1 || string(rl[0].Value) != "40" || rl[0].Scope != plugin.Group {
			t.Fatalf("%+v", rl)
		}
		must(t, s.DeleteSetting(ctx, plugin.Key, "k1", "rl", "rpm"))
		if err := s.DeleteSetting(ctx, plugin.Key, "k1", "rl", "rpm"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		must(t, s.PutManifest(ctx, &store.Manifest{ID: "deepseek", YAML: "id: deepseek\n", Source: store.SourceOfficial}))
		m, err := s.GetManifest(ctx, "deepseek")
		must(t, err)
		if m.YAML != "id: deepseek\n" || m.Source != store.SourceOfficial {
			t.Fatalf("%+v", m)
		}
		ml, _ := s.ListManifests(ctx)
		if len(ml) != 1 {
			t.Fatal(ml)
		}
		must(t, s.DeleteManifest(ctx, "deepseek"))
	})

	run("RecordsAndUsage", func(t *testing.T, s store.Store, _ *secret.Box) {
		v0, _ := s.Version(ctx)
		base := time.Now().Truncate(time.Hour).Add(-3 * time.Hour).UTC()
		var recs []*plugin.Record
		for i := range 30 {
			r := &plugin.Record{
				ID:       fmt.Sprintf("req_%03d", i),
				Time:     base.Add(time.Duration(i/3) * 20 * time.Minute), // three per timestamp
				Caller:   plugin.Caller{KeyID: []string{"k1", "k2"}[i%2], UserID: "u1", GroupID: "g1", KeyName: "n"},
				Protocol: "anthropic.messages", Model: []string{"claude", "gpt"}[i%2], Stream: true,
				Status:    200,
				Attempts:  []plugin.AttemptRecord{{N: 1, UpstreamID: "anthropic", UpstreamModel: "claude-x", Tier: plugin.TierPassthrough, Status: 200, Sent: true}},
				Usage:     ir.Usage{Input: 100, CacheRead: 50, CacheWrite5m: 10, Output: 20, Raw: json.RawMessage(`{"input_tokens":100}`)},
				CostMicro: 7, ChargeMicro: 10, TTFB: 300 * time.Millisecond, Duration: 2 * time.Second,
			}
			if i%5 == 0 {
				r.Status, r.ErrorKind, r.Error = 529, ir.ErrOverloaded, "overloaded"
			}
			if i == 29 {
				r.Status = 0
			}
			recs = append(recs, r)
		}
		recs[0].Body = &plugin.RecordBody{Request: json.RawMessage(`{"model":"claude"}`)}
		must(t, s.InsertRecords(ctx, recs[:20]))
		must(t, s.InsertRecords(ctx, recs[15:])) // overlap is skipped
		if v1, _ := s.Version(ctx); v1 != v0 {
			t.Fatal("record insert bumped version")
		}
		got, err := s.GetRecord(ctx, "req_000")
		must(t, err)
		if got.Body == nil || string(got.Body.Request) != `{"model":"claude"}` || got.TTFB != 300*time.Millisecond ||
			string(got.Usage.Raw) != `{"input_tokens":100}` ||
			got.Usage.CacheRead != 50 || got.Attempts[0].Tier != plugin.TierPassthrough || got.Caller.KeyName != "n" ||
			!got.Time.Equal(base) || got.ErrorKind != ir.ErrOverloaded {
			t.Fatalf("%+v", got)
		}
		// Cursor pagination visits every record exactly once, newest first.
		seen := map[string]bool{}
		var cursor string
		var prev *plugin.Record
		pages := 0
		for {
			page, err := s.QueryRecords(ctx, store.RecordQuery{Limit: 7, Cursor: cursor})
			must(t, err)
			pages++
			for _, r := range page.Records {
				if seen[r.ID] {
					t.Fatalf("duplicate %s", r.ID)
				}
				seen[r.ID] = true
				if r.Body != nil {
					t.Fatal("body in list")
				}
				if prev != nil && (r.Time.After(prev.Time) || (r.Time.Equal(prev.Time) && r.ID > prev.ID)) {
					t.Fatalf("order: %s after %s", r.ID, prev.ID)
				}
				prev = r
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
		if len(seen) != 30 || pages != 5 {
			t.Fatalf("seen %d in %d pages", len(seen), pages)
		}
		count := func(q store.RecordQuery) int {
			q.Limit = 500
			p, err := s.QueryRecords(ctx, q)
			must(t, err)
			return len(p.Records)
		}
		if n := count(store.RecordQuery{KeyID: "k1"}); n != 15 {
			t.Fatalf("key filter %d", n)
		}
		if n := count(store.RecordQuery{StatusClass: 5}); n != 6 {
			t.Fatalf("5xx %d", n)
		}
		if n := count(store.RecordQuery{StatusClass: -1}); n != 1 {
			t.Fatalf("no status %d", n)
		}
		if n := count(store.RecordQuery{Model: "gpt", UpstreamID: "anthropic"}); n != 15 {
			t.Fatalf("model %d", n)
		}
		if n := count(store.RecordQuery{RequestID: "req_01"}); n != 10 {
			t.Fatalf("request id %d", n)
		}
		if n := count(store.RecordQuery{RequestID: "req%"}); n != 0 {
			t.Fatalf("like escape %d", n)
		}
		if n := count(store.RecordQuery{From: base.Add(time.Hour), To: base.Add(2 * time.Hour)}); n != 9 {
			t.Fatalf("time range %d", n)
		}
		if _, err := s.QueryRecords(ctx, store.RecordQuery{Cursor: "!!"}); err == nil {
			t.Fatal("bad cursor accepted")
		}

		// Spent: whole hours from rollups, partial first hour from the log.
		charge, reqs, err := s.Spent(ctx, "key:k1", time.Time{})
		must(t, err)
		if charge != 150 || reqs != 15 {
			t.Fatalf("spent all %d %d", charge, reqs)
		}
		charge, reqs, err = s.Spent(ctx, "user:u1", base.Add(30*time.Minute))
		must(t, err)
		// Records at +40m, +60m, ... : timestamps 2..9 → 24 records.
		if reqs != 24 || charge != 240 {
			t.Fatalf("spent partial %d %d", charge, reqs)
		}
		if _, _, err := s.Spent(ctx, "team:x", time.Time{}); err == nil {
			t.Fatal("bad subject accepted")
		}
		pts, err := s.UsageSeries(ctx, store.UsageQuery{From: base, To: base.Add(4 * time.Hour), By: "model"})
		must(t, err)
		var total, errs, cacheRead int64
		for _, p := range pts {
			total += p.Requests
			errs += p.Errors
			cacheRead += p.CacheRead
		}
		if total != 30 || errs != 7 || cacheRead != 1500 || pts[0].Time != base || pts[0].Series != "claude" {
			t.Fatalf("series %+v total %d errs %d", pts, total, errs)
		}
		day, err := s.UsageSeries(ctx, store.UsageQuery{From: base, Bucket: 24 * time.Hour, KeyID: "k2"})
		must(t, err)
		var dayTotal int64
		for _, p := range day {
			dayTotal += p.Requests
		}
		if dayTotal != 15 || len(day) > 2 {
			t.Fatalf("day %+v", day)
		}
		if _, err := s.UsageSeries(ctx, store.UsageQuery{Bucket: time.Minute}); err == nil {
			t.Fatal("minute bucket accepted")
		}
		n, err := s.PruneRecords(ctx, base.Add(time.Hour))
		must(t, err)
		if n != 9 {
			t.Fatalf("pruned %d", n)
		}
		if charge, _, _ := s.Spent(ctx, "key:k1", time.Time{}); charge != 150 {
			t.Fatal("prune touched rollups")
		}
	})

	run("Analytics", func(t *testing.T, s store.Store, _ *secret.Box) {
		base := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
		var recs []*plugin.Record
		// Session a: 3 requests by Claude Code (UA only), one failing on a
		// rate limit; session b: 2 by Codex (detected client given);
		// one request without a session that never reached an upstream.
		for i := range 3 {
			r := &plugin.Record{ID: fmt.Sprintf("a%d", i), Time: base.Add(time.Duration(i) * time.Minute), Model: "claude",
				SessionID: "sa", ClientUA: "claude-cli/2.1.0 (external, cli)", Status: 200, ChargeMicro: 100,
				Caller: plugin.Caller{KeyID: "k1", KeyName: "laptop", UserID: "u1"},
				Usage:  ir.Usage{Input: 10, CacheRead: 30}, Attempts: []plugin.AttemptRecord{{N: 1, UpstreamID: "up"}}}
			if i == 1 {
				r.Status, r.ChargeMicro = 429, 0
				r.Attempts[0].Cause = plugin.CauseRateLimit
			}
			recs = append(recs, r)
		}
		for i := range 2 {
			recs = append(recs, &plugin.Record{ID: fmt.Sprintf("b%d", i), Time: base.Add(time.Hour + time.Duration(i)*time.Minute),
				Model: []string{"gpt", "mini"}[i], SessionID: "sb", Client: "codex", ClientVersion: "0.50.0", Status: 200, ChargeMicro: 50,
				Caller: plugin.Caller{KeyID: "k2", UserID: "u2"}, Attempts: []plugin.AttemptRecord{{N: 1, UpstreamID: "up"}}})
		}
		recs = append(recs, &plugin.Record{ID: "c0", Time: base.Add(90 * time.Minute), Model: "gpt", Status: 401, ErrorKind: ir.ErrAuthentication})
		must(t, s.InsertRecords(ctx, recs))

		got, err := s.GetRecord(ctx, "a0")
		must(t, err)
		if got.Client != "claude-code" || got.ClientVersion != "2.1.0" {
			t.Fatalf("client from user agent: %q %q", got.Client, got.ClientVersion)
		}
		page, err := s.QueryRecords(ctx, store.RecordQuery{Client: "codex"})
		must(t, err)
		if len(page.Records) != 2 || page.Records[0].ClientVersion != "0.50.0" {
			t.Fatalf("client filter %+v", page.Records)
		}
		if p, _ := s.QueryRecords(ctx, store.RecordQuery{Cause: "rate_limit"}); len(p.Records) != 1 || p.Records[0].ID != "a1" {
			t.Fatalf("cause filter %+v", p.Records)
		}
		if p, _ := s.QueryRecords(ctx, store.RecordQuery{Cause: "not_sent"}); len(p.Records) != 1 || p.Records[0].ID != "c0" {
			t.Fatalf("not_sent filter %+v", p.Records)
		}

		pts, err := s.UsageSeries(ctx, store.UsageQuery{From: base, To: base.Add(3 * time.Hour), Bucket: 3 * time.Hour, By: "client"})
		must(t, err)
		bySeries := map[string]int64{}
		for _, p := range pts {
			bySeries[p.Series] += p.ChargeMicro
		}
		if bySeries["claude-code"] != 200 || bySeries["codex"] != 100 || bySeries["other"] != 0 || len(bySeries) != 3 {
			t.Fatalf("by client %v", bySeries)
		}
		if one, _ := s.UsageSeries(ctx, store.UsageQuery{From: base, To: base.Add(3 * time.Hour), Client: "codex"}); len(one) != 1 || one[0].Requests != 2 {
			t.Fatalf("client usage filter %+v", one)
		}

		list, err := s.SessionPage(ctx, store.RecordQuery{Limit: 1})
		must(t, err)
		if len(list.Sessions) != 1 || list.Next == "" {
			t.Fatalf("first page %+v", list)
		}
		b := list.Sessions[0]
		if b.ID != "sb" || b.Requests != 2 || b.ChargeMicro != 100 || b.Client != "codex" || b.KeyID != "k2" ||
			!b.First.Equal(base.Add(time.Hour)) || !b.Last.Equal(base.Add(61*time.Minute)) || len(b.Models) != 2 {
			t.Fatalf("session b %+v", b)
		}
		list, err = s.SessionPage(ctx, store.RecordQuery{Limit: 1, Cursor: list.Next})
		must(t, err)
		if len(list.Sessions) != 1 || list.Next != "" {
			t.Fatalf("second page %+v", list)
		}
		a := list.Sessions[0]
		if a.ID != "sa" || a.Requests != 3 || a.Failed != 1 || a.ChargeMicro != 200 || a.KeyName != "laptop" ||
			a.Input != 120 || a.CacheRead != 90 || len(a.Models) != 1 || a.Models[0] != "claude" {
			t.Fatalf("session a %+v", a)
		}
		if l, _ := s.SessionPage(ctx, store.RecordQuery{UserID: "u2"}); len(l.Sessions) != 1 || l.Sessions[0].ID != "sb" {
			t.Fatalf("user filter %+v", l)
		}

		tot, err := s.SessionTotalAt(ctx, "sa", "", base.Add(time.Minute), "a1")
		must(t, err)
		if tot.Requests != 3 || tot.Turn != 2 || tot.ChargeMicro != 200 || !tot.First.Equal(base) {
			t.Fatalf("session total %+v", tot)
		}
		if tot, _ := s.SessionTotalAt(ctx, "sa", "u2", base, "a0"); tot.Requests != 0 {
			t.Fatalf("other user's session %+v", tot)
		}

		fs, err := s.FailureSeries(ctx, store.RecordQuery{From: base, To: base.Add(3 * time.Hour)}, time.Hour)
		must(t, err)
		if len(fs) != 2 || fs[0].Cause != "rate_limit" || !fs[0].Time.Equal(base) || fs[1].Cause != "not_sent" || fs[1].Failed != 1 {
			t.Fatalf("failures %+v", fs)
		}
	})

	run("Audit", func(t *testing.T, s store.Store, _ *secret.Box) {
		now := time.Now()
		for i := range 5 {
			must(t, s.AddAudit(ctx, &store.Audit{Time: now.Add(time.Duration(i) * time.Second), Actor: "admin", Action: "key.create",
				Target: fmt.Sprintf("key:%d", i%2), Diff: json.RawMessage(`{"name":"x"}`)}))
		}
		got, err := s.ListAudit(ctx, store.AuditQuery{Target: "key:0", Before: now.Add(time.Hour)})
		must(t, err)
		if len(got) != 3 || !got[0].Time.After(got[1].Time) || string(got[0].Diff) == "" {
			t.Fatalf("%+v", got)
		}
		got, _ = s.ListAudit(ctx, store.AuditQuery{Limit: 2, Before: now.Add(time.Hour)})
		if len(got) != 2 {
			t.Fatal(len(got))
		}
	})

	run("KV", func(t *testing.T, s store.Store, _ *secret.Box) {
		a, b := s.KV("plugin.a"), s.KV("plugin.b")
		must(t, a.Put(ctx, "x", []byte("hello"), 0))
		if _, ok, _ := b.Get(ctx, "x"); ok {
			t.Fatal("kv not scoped")
		}
		v, ok, err := a.Get(ctx, "x")
		must(t, err)
		if !ok || string(v) != "hello" {
			t.Fatal(string(v))
		}
		must(t, a.Put(ctx, "t", []byte("1"), 50*time.Millisecond))
		time.Sleep(80 * time.Millisecond)
		if _, ok, _ := a.Get(ctx, "t"); ok {
			t.Fatal("not expired")
		}
		if n, _ := a.Incr(ctx, "t", 2, 0); n != 2 {
			t.Fatalf("incr over expired = %d", n)
		}
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				if _, err := a.Incr(ctx, "c", 1, time.Hour); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if v, _, _ := a.Get(ctx, "c"); string(v) != "20" {
			t.Fatalf("counter %q", v)
		}
		must(t, a.Put(ctx, "n", []byte("41"), 0))
		if n, _ := a.Incr(ctx, "n", 1, 0); n != 42 {
			t.Fatalf("incr over put %d", n)
		}
		must(t, a.Delete(ctx, "x"))
		if _, ok, _ := a.Get(ctx, "x"); ok {
			t.Fatal("deleted")
		}
		must(t, s.PurgeExpired(ctx))
	})

	run("State", func(t *testing.T, s store.Store, _ *secret.Box) {
		st, err := s.State(ctx)
		must(t, err)
		defer st.Close()
		must(t, st.Set(ctx, "bind:sess", []byte("cred1"), time.Minute))
		if v, ok, _ := st.Get(ctx, "bind:sess"); !ok || string(v) != "cred1" {
			t.Fatal("state roundtrip")
		}
	})

	run("SnapshotExportApply", func(t *testing.T, s store.Store, box *secret.Box) {
		doc := `
groups:
  - id: vip
    name: VIP
    price_multiplier: 0.8
upstreams:
  - id: deepseek
    provider: provider.manifest
    manifest: deepseek
  - id: openai
    provider: provider.openai
    endpoints:
      - id: chat
        protocol: openai.chat
        base_url: https://api.openai.com/v1
        issuer: openai
credentials:
  - id: ds-team-1
    upstream: deepseek
    kind: shared
    secret: sk-ds-1
  - id: mine
    upstream: openai
    kind: personal
    owner: u1
    secret: sk-mine
models:
  - name: gpt-5.5
    info: {context_window: 400000, tools: true}
    routes:
      - upstream: openai
        upstream_model: gpt-5.5-2026-04-24
        priority: 10
        weight: 3
      - upstream: deepseek
        priority: 5
        groups: [vip]
presets:
  - name: auto-coding
    models: [gpt-5.5]
    strategy: fastest
prices:
  - model: gpt-5.5
    input: 1250000
    output: 10000000
features:
  - {id: billing, enabled: true}
plugins:
  - id: sink.vendor
    order: 10
    config: {burst: 5}
settings:
  - scope: group
    scope_id: vip
    plugin: middleware.ratelimit
    key: rpm
    value: 600
manifests:
  - id: deepseek
    yaml: "id: deepseek\n"
`
		v0, _ := s.Version(ctx)
		dry, err := s.Apply(ctx, []byte(doc), true)
		must(t, err)
		if dry.Created != 13 || dry.Updated != 0 {
			t.Fatalf("dry %+v", dry)
		}
		if gs, _ := s.ListGroups(ctx); len(gs) != 0 {
			t.Fatal("dry run wrote")
		}
		if v, _ := s.Version(ctx); v != v0 {
			t.Fatal("dry run bumped version")
		}
		res, err := s.Apply(ctx, []byte(doc), false)
		must(t, err)
		if res.Created != 13 {
			t.Fatalf("apply %+v", res)
		}
		res, err = s.Apply(ctx, []byte(doc), false)
		must(t, err)
		if res.Unchanged != 13 || res.Created+res.Updated != 0 {
			t.Fatalf("reapply %+v", res)
		}
		snap, err := s.Snapshot(ctx)
		must(t, err)
		v1, _ := s.Version(ctx)
		if snap.Version != v1 || len(snap.Upstreams) != 2 || len(snap.Credentials) != 2 || len(snap.Routes) != 2 ||
			len(snap.Settings) != 1 || len(snap.Models) != 1 || len(snap.Presets) != 1 || !snap.Models[0].Enabled || !snap.Upstreams[0].Enabled ||
			snap.Models[0].Info.ContextWindow != 400000 {
			t.Fatalf("snapshot %+v", snap)
		}
		for _, c := range snap.Credentials {
			pc, err := c.Open(box)
			must(t, err)
			if !strings.HasPrefix(pc.Secret, "sk-") {
				t.Fatal("secret")
			}
		}
		// Changing one field updates exactly that row; status survives.
		must(t, s.SetCredentialStatus(ctx, "ds-team-1", store.CredQuarantined, "ban", time.Time{}))
		res, err = s.Apply(ctx, []byte(strings.Replace(doc, "sk-ds-1", "sk-ds-2", 1)), false)
		must(t, err)
		if res.Updated != 1 || res.Unchanged != 12 {
			t.Fatalf("secret change %+v", res)
		}
		c, _ := s.GetCredential(ctx, "ds-team-1")
		if pc, _ := c.Open(box); pc.Secret != "sk-ds-2" || c.Status != store.CredQuarantined {
			t.Fatalf("%+v", c)
		}

		out, err := s.Export(ctx, store.ExportOptions{})
		must(t, err)
		if strings.Contains(string(out), "sk-ds") {
			t.Fatal("secret exported without flag")
		}
		withSecrets, err := s.Export(ctx, store.ExportOptions{Secrets: true})
		must(t, err)
		if !strings.Contains(string(withSecrets), "sk-ds-2") {
			t.Fatalf("secret missing:\n%s", withSecrets)
		}
		// The export applies cleanly to a fresh store and round-trips.
		other := open(t, newBox(t))
		res, err = other.Apply(ctx, withSecrets, false)
		must(t, err)
		if res.Created != 13 {
			t.Fatalf("import %+v\n%s", res, withSecrets)
		}
		again, err := other.Export(ctx, store.ExportOptions{Secrets: true})
		must(t, err)
		if string(again) != string(withSecrets) {
			t.Fatalf("round trip differs:\n%s\n---\n%s", withSecrets, again)
		}
		res, err = s.Apply(ctx, out, false)
		must(t, err)
		if res.Unchanged != 13 {
			t.Fatalf("export reapply %+v", res)
		}
		if _, err := s.Apply(ctx, []byte("groups:\n  - id: x\n    nmae: typo\n"), true); err == nil {
			t.Fatal("unknown field accepted")
		}
	})
}
