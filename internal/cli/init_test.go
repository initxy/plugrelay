package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
)

// initRelay runs init in a fresh directory and returns the config path.
func initRelay(t *testing.T, te *testEnv, extra ...string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "relay.yaml")
	if code := te.run(append([]string{"--config", cfg, "init"}, extra...)...); code != ExitOK {
		t.Fatalf("init exit %d: %s", code, te.stderr)
	}
	return cfg
}

func TestInitCreatesWorkingRelay(t *testing.T) {
	te := newTestEnv(t)
	cfgPath := initRelay(t, te)
	dir := filepath.Dir(cfgPath)

	fi, err := os.Stat(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("master.key mode %o, want 600", fi.Mode().Perm())
	}
	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatalf("written config does not load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8787" || cfg.MasterKeyFile != "master.key" || cfg.Store.DSN != "plugrelay.db" {
		t.Errorf("config = %+v", cfg)
	}
	if _, err := os.Stat(filepath.Join(dir, "plugrelay.db")); err != nil {
		t.Errorf("database not created: %v", err)
	}

	// Commands work from another working directory: paths resolve
	// against the config file.
	if code := te.run("--config", cfgPath, "doctor"); code != ExitOK {
		t.Fatalf("doctor exit %d:\n%s%s", code, te.stdout, te.stderr)
	}
	if !strings.Contains(te.stdout.String(), "schema version") {
		t.Errorf("doctor output: %s", te.stdout)
	}
}

func TestInitIdempotent(t *testing.T) {
	te := newTestEnv(t)
	cfgPath := initRelay(t, te)
	keyPath := filepath.Join(filepath.Dir(cfgPath), "master.key")
	key1, _ := os.ReadFile(keyPath)

	if code := te.run("--config", cfgPath, "init"); code != ExitError {
		t.Fatalf("second init exit %d, want %d", code, ExitError)
	}
	if !strings.Contains(te.stderr.String(), "--force") {
		t.Errorf("stderr: %s", te.stderr)
	}

	if code := te.run("--config", cfgPath, "init", "--force", "--listen", "127.0.0.1:9999"); code != ExitOK {
		t.Fatalf("init --force exit %d: %s", code, te.stderr)
	}
	key2, _ := os.ReadFile(keyPath)
	if string(key1) != string(key2) {
		t.Error("init --force replaced the master key")
	}
	if !strings.Contains(te.stdout.String(), "Kept existing master key") {
		t.Errorf("stdout: %s", te.stdout)
	}
	cfg, err := config.Load(cfgPath, nil)
	if err != nil || cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("forced config: %+v, %v", cfg, err)
	}
}

func TestInitRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
		msg  string
	}{
		{"bad listen", []string{"--listen", "8787"}, ExitUsage, "listen"},
		{"network without admin", []string{"--listen", "0.0.0.0:8787"}, ExitUsage, "--admin-email"},
		{"no password", []string{"--admin-email", "a@b.c"}, ExitError, "no admin password"},
		{"short password", []string{"--admin-email", "a@b.c", "--admin-password", "short"}, ExitError, "at least 8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			cfg := filepath.Join(t.TempDir(), "relay.yaml")
			code := te.run(append([]string{"--config", cfg, "init"}, tc.args...)...)
			if code != tc.code || !strings.Contains(te.stderr.String(), tc.msg) {
				t.Fatalf("exit %d (want %d), stderr %q (want %q)", code, tc.code, te.stderr, tc.msg)
			}
			if entries, _ := os.ReadDir(filepath.Dir(cfg)); len(entries) > 0 {
				t.Errorf("files left behind despite the error: %v", entries)
			}
		})
	}
}

func TestInitCreatesAdmin(t *testing.T) {
	for _, src := range []string{"env", "stdin"} {
		t.Run(src, func(t *testing.T) {
			te := newTestEnv(t)
			if src == "env" {
				te.vars["PLUGRELAY_ADMIN_PASSWORD"] = "correct horse"
			} else {
				te.stdin.WriteString("correct horse\n")
			}
			dataDir := t.TempDir()
			cfgPath := filepath.Join(t.TempDir(), "relay.yaml")
			if code := te.run("--config", cfgPath, "--data-dir", dataDir, "init", "--admin-email", "ops@example.com", "--public-url", "https://relay.example.com/"); code != ExitOK {
				t.Fatalf("init exit %d: %s", code, te.stderr)
			}
			cfg, err := config.Load(cfgPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.MasterKeyFile != filepath.Join(dataDir, "master.key") {
				t.Errorf("master_key_file = %q, want absolute path in --data-dir", cfg.MasterKeyFile)
			}
			st, err := store.Open(context.Background(), store.Config{Driver: "sqlite", DSN: cfg.Store.DSN})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			u, err := st.Authenticate(context.Background(), "ops@example.com", "correct horse")
			if err != nil || u.Role != store.RoleAdmin {
				t.Fatalf("admin login: %+v, %v", u, err)
			}
			// A relay with an admin needs a login on this machine too.
			site, err := admin.LoadSite(context.Background(), st)
			if err != nil {
				t.Fatal(err)
			}
			if on, set := site.Bool(admin.SettingLocalOwner); on || !set || site.String(admin.SettingPublicURL) != "https://relay.example.com" {
				t.Errorf("site settings %v", site)
			}

			// Re-running keeps the admin and its password.
			te.vars["PLUGRELAY_ADMIN_PASSWORD"] = "another password"
			if code := te.run("--config", cfgPath, "--data-dir", dataDir, "init", "--force", "--admin-email", "ops@example.com"); code != ExitOK {
				t.Fatalf("re-init exit %d: %s", code, te.stderr)
			}
			if _, err := st.Authenticate(context.Background(), "ops@example.com", "correct horse"); err != nil {
				t.Errorf("re-init changed the admin password: %v", err)
			}
		})
	}
}

func TestBusinessCommands(t *testing.T) {
	te := newTestEnv(t)
	cfg := initRelay(t, te)
	run := func(want int, args ...string) string {
		t.Helper()
		if code := te.run(append([]string{"--config", cfg}, args...)...); code != want {
			t.Fatalf("%v: exit %d, want %d\n%s%s", args, code, want, te.stdout, te.stderr)
		}
		return te.stdout.String()
	}

	run(ExitError, "upstream", "add", "--manifest", "deepseek", "--credential-env", "DEEPSEEK_API_KEY")
	te.vars["DEEPSEEK_API_KEY"] = "sk-test-123"
	run(ExitOK, "upstream", "add", "--manifest", "deepseek", "--credential-env", "DEEPSEEK_API_KEY")
	run(ExitOK, "upstream", "add", "--manifest", "deepseek", "--credential-env", "DEEPSEEK_API_KEY") // replaces, no duplicate
	run(ExitError, "upstream", "add", "--manifest", "no-such-manifest")
	run(ExitUsage, "upstream", "add", "--manifest", "azure-openai", "--id", "az")
	run(ExitUsage, "upstream", "add", "--manifest", "deepseek", "--var", "nope=1")

	var ups []struct {
		ID          string `json:"id"`
		Provider    string `json:"provider"`
		Credentials int    `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(run(ExitOK, "upstream", "list", "--json")), &ups); err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].ID != "deepseek" || ups[0].Provider != "provider.openai-compatible" || ups[0].Credentials != 1 {
		t.Errorf("upstreams = %+v", ups)
	}

	run(ExitError, "model", "add", "--name", "ds", "--route", "missing:m")
	run(ExitOK, "model", "add", "--name", "ds", "--route", "deepseek:deepseek-chat:10:2", "--route", "deepseek", "--display-name", "DeepSeek Chat")
	if out := run(ExitOK, "model", "list"); !strings.Contains(out, "deepseek:deepseek-chat (p10 w2), deepseek:ds (p0 w1)") {
		t.Errorf("model list:\n%s", out)
	}

	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(run(ExitOK, "key", "create", "--name", "me", "--models", "ds", "--expires", "30d", "--json")), &created); err != nil {
		t.Fatal(err)
	}
	if !secret.ValidKeyFormat(created.Key) {
		t.Errorf("key %q", created.Key)
	}
	run(ExitOK, "key", "disable", "--id", created.ID)
	if out := run(ExitOK, "key", "list"); !strings.Contains(out, "disabled") || strings.Contains(out, created.Key) {
		t.Errorf("key list after disable:\n%s", out)
	}
	run(ExitOK, "key", "enable", created.ID)
	if out := run(ExitOK, "key", "list"); !strings.Contains(out, "active") || !strings.Contains(out, "2026-12-05") {
		t.Errorf("key list after enable:\n%s", out)
	}
	run(ExitOK, "key", "rm", "--id", created.ID)
	run(ExitError, "key", "rm", "--id", created.ID)

	exported := run(ExitOK, "export")
	if !strings.Contains(exported, "display_name: DeepSeek Chat") {
		t.Errorf("export lacks the model display name:\n%s", exported)
	}
	if strings.Contains(exported, "sk-test-123") {
		t.Error("export leaked the secret without --with-secrets")
	}
	withSecrets := filepath.Join(t.TempDir(), "all.yaml")
	run(ExitOK, "export", "--with-secrets", "-o", withSecrets)
	data, _ := os.ReadFile(withSecrets)
	if !strings.Contains(string(data), "sk-test-123") {
		t.Error("export --with-secrets lacks the secret")
	}
	if fi, _ := os.Stat(withSecrets); fi.Mode().Perm() != 0o600 {
		t.Errorf("secret export mode %o", fi.Mode().Perm())
	}
	if out := run(ExitOK, "apply", "-f", withSecrets, "--dry-run"); !strings.Contains(out, "dry run") || !strings.Contains(out, "0 created") {
		t.Errorf("apply dry run:\n%s", out)
	}
	te.stdin.WriteString("models:\n  - name: extra\n    routes: [{upstream: deepseek, upstream_model: x}]\n")
	if out := run(ExitOK, "apply", "-f", "-"); !strings.Contains(out, "create") {
		t.Errorf("apply:\n%s", out)
	}

	run(ExitOK, "upstream", "rm", "deepseek")
	if out := run(ExitOK, "model", "list"); strings.Contains(out, "deepseek:") {
		t.Errorf("routes survived upstream rm:\n%s", out)
	}
}

func TestServeGetsResolvedConfig(t *testing.T) {
	te := newTestEnv(t)
	cfgPath := initRelay(t, te)
	var got *config.Config
	Serve = func(ctx context.Context, cfg *config.Config) error {
		got = cfg
		return ctx.Err()
	}
	t.Cleanup(func() { Serve = nil })
	if code := te.run("--config", cfgPath, "serve"); code != ExitOK {
		t.Fatalf("serve exit %d: %s", code, te.stderr)
	}
	dir := filepath.Dir(cfgPath)
	if got == nil || got.Store.DSN != filepath.Join(dir, "plugrelay.db") || got.MasterKeyFile != filepath.Join(dir, "master.key") {
		t.Errorf("serve config = %+v", got)
	}
}

func TestInitCreatesLocalOwner(t *testing.T) {
	te := newTestEnv(t)
	cfgPath := initRelay(t, te)
	cfg, err := config.Load(cfgPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	dsn := filepath.Join(filepath.Dir(cfgPath), cfg.Store.DSN)
	st, err := store.Open(context.Background(), store.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	users, err := st.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Role != store.RoleAdmin || users[0].Email != localOwnerEmail {
		t.Fatalf("init without an admin must leave exactly one owner admin, got %+v", users)
	}
}
