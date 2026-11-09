package cli

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "setup", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s mismatch\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "setup", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

var testSetup = setupOpts{url: "http://127.0.0.1:8787", key: "pr_testkey", models: []string{"gpt-5.5"}}

func TestClientConfigWriters(t *testing.T) {
	cases := []struct {
		in, out string
		opts    setupOpts
		edit    func([]byte, setupOpts) ([]byte, error)
	}{
		{"claude.in.json", "claude.golden.json", testSetup, claudeSettings},
		{"", "claude-empty.golden.json", setupOpts{url: testSetup.url, key: testSetup.key}, claudeSettings},
		{"codex.in.toml", "codex.golden.toml", testSetup, codexEdit},
		{"codex-fresh.in.toml", "codex-fresh.golden.toml", setupOpts{url: testSetup.url, key: testSetup.key}, codexEdit},
		{"", "codex-empty.golden.toml", testSetup, codexEdit},
		{"opencode.in.json", "opencode.golden.json", setupOpts{url: testSetup.url, key: testSetup.key, models: []string{"claude-sonnet-4-5", "gpt-5.5"}}, openCodeConfig},
		{"", "opencode-empty.golden.json", testSetup, openCodeConfig},
	}
	for _, tc := range cases {
		t.Run(tc.out, func(t *testing.T) {
			var src []byte
			if tc.in != "" {
				src = readTestdata(t, tc.in)
			}
			got, err := tc.edit(src, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, tc.out, got)
			again, err := tc.edit(got, tc.opts)
			if err != nil || string(again) != string(got) {
				t.Errorf("second edit changed the file (err %v):\n%s", err, again)
			}
		})
	}
}

func codexEdit(src []byte, o setupOpts) ([]byte, error) { return codexConfig(src, o), nil }

func TestClaudeSettingsRejectsNonObject(t *testing.T) {
	for _, src := range []string{`[1]`, `{"env": "x"}`, `{"a":1} trailing`, `{`} {
		if _, err := claudeSettings([]byte(src), testSetup); err == nil {
			t.Errorf("claudeSettings(%q) succeeded", src)
		}
	}
}

func TestSetupWritesWithBackup(t *testing.T) {
	te := newTestEnv(t)
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := readTestdata(t, "claude.in.json")
	if err := os.WriteFile(path, orig, 0o640); err != nil {
		t.Fatal(err)
	}
	args := []string{"setup", "claude-code", "--home", home, "--url", "http://127.0.0.1:8787/v1/", "--key", "pr_testkey", "--model", "gpt-5.5"}

	if code := te.run(append(args, "--print")...); code != ExitOK {
		t.Fatalf("--print exit %d: %s", code, te.stderr)
	}
	if cur, _ := os.ReadFile(path); string(cur) != string(orig) {
		t.Error("--print modified the file")
	}
	golden(t, "claude.golden.json", te.stdout.Bytes())

	if code := te.run(args...); code != ExitOK {
		t.Fatalf("exit %d: %s", code, te.stderr)
	}
	backup := path + ".bak-20261105-103000"
	if b, err := os.ReadFile(backup); err != nil || string(b) != string(orig) {
		t.Errorf("backup %s: %v", backup, err)
	}
	got, _ := os.ReadFile(path)
	golden(t, "claude.golden.json", got)
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %o, want the original 640", fi.Mode().Perm())
	}

	if code := te.run(args...); code != ExitOK || !strings.Contains(te.stdout.String(), "up to date") {
		t.Errorf("rerun: exit %d, %s", code, te.stdout)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 2 {
		t.Errorf("rerun left %d files, want settings and one backup", len(entries))
	}
}

func TestSetupNewFilesAndGemini(t *testing.T) {
	te := newTestEnv(t)
	home := t.TempDir()
	base := []string{"--home", home, "--url", "https://relay.example.com", "--key", "pr_testkey"}
	if code := te.run(append([]string{"setup", "codex", "--model", "gpt-5.5"}, base...)...); code != ExitOK {
		t.Fatalf("codex exit %d: %s", code, te.stderr)
	}
	if !strings.Contains(te.stdout.String(), "export PLUGRELAY_API_KEY='pr_testkey'") {
		t.Errorf("codex output lacks the key export: %s", te.stdout)
	}
	data, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil || !strings.Contains(string(data), `base_url = "https://relay.example.com/v1"`) {
		t.Errorf("codex config: %s, %v", data, err)
	}

	if code := te.run(append([]string{"setup", "opencode"}, base...)...); code != ExitUsage {
		t.Errorf("opencode without --model: exit %d", code)
	}
	if code := te.run(append([]string{"setup", "opencode", "--model", "m"}, base...)...); code != ExitOK {
		t.Fatalf("opencode exit %d: %s", code, te.stderr)
	}
	fi, err := os.Stat(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("opencode.json: %v, %v", fi, err)
	}

	if code := te.run(append([]string{"setup", "gemini", "--model", "gemini-3-pro"}, base...)...); code != ExitOK {
		t.Fatalf("gemini exit %d: %s", code, te.stderr)
	}
	want := "export GOOGLE_GEMINI_BASE_URL='https://relay.example.com'\nexport GEMINI_API_KEY='pr_testkey'\nexport GEMINI_MODEL='gemini-3-pro'\n"
	if te.stdout.String() != want {
		t.Errorf("gemini output:\n%s", te.stdout)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini")); err == nil {
		t.Error("gemini setup wrote files")
	}
}

func TestSetupURLFromConfig(t *testing.T) {
	te := newTestEnv(t)
	cfg := initRelay(t, te, "--listen", "0.0.0.0:9000", "--no-admin")
	if code := te.run("--config", cfg, "setup", "gemini", "--key", "k", "--print"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, te.stderr)
	}
	if !strings.Contains(te.stdout.String(), "'http://127.0.0.1:9000'") {
		t.Errorf("url not derived from listen: %s", te.stdout)
	}
}

// TestSetupCodexWritesCatalog checks that setup codex saves the relay's
// catalog locally and points model_catalog_json at it, since Codex does not
// fetch model metadata from custom providers.
func TestSetupCodexWritesCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.URL.Query().Get("client_version") == "" || r.Header.Get("Authorization") != "Bearer pr_testkey" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.5"}]}`))
	}))
	defer srv.Close()
	te := newTestEnv(t)
	home := t.TempDir()
	if code := te.run("setup", "codex", "--home", home, "--url", srv.URL, "--key", "pr_testkey", "--model", "gpt-5.5"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, te.stderr)
	}
	catalog := filepath.Join(home, ".codex", codexCatalogFile)
	if b, err := os.ReadFile(catalog); err != nil || !strings.Contains(string(b), `"slug":"gpt-5.5"`) {
		t.Fatalf("catalog %s: %v", b, err)
	}
	cfg, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if !strings.Contains(string(cfg), "model_catalog_json = "+tomlString(catalog)) {
		t.Fatalf("config lacks model_catalog_json:\n%s", cfg)
	}
}

// TestSetupCodexRefusesEmptyCatalog: a key that sees no models would leave
// Codex with an empty catalog it refuses to start with, so setup fails and
// writes neither the catalog nor the config.
func TestSetupCodexRefusesEmptyCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	te := newTestEnv(t)
	home := t.TempDir()
	if code := te.run("setup", "codex", "--home", home, "--url", srv.URL, "--key", "pr_testkey", "--model", "gpt-5.5"); code == ExitOK {
		t.Fatal("setup succeeded with an empty catalog")
	}
	if !strings.Contains(te.stderr.String(), "at least one model") {
		t.Errorf("stderr: %s", te.stderr)
	}
	for _, f := range []string{codexCatalogFile, "config.toml"} {
		if _, err := os.Stat(filepath.Join(home, ".codex", f)); err == nil {
			t.Errorf("%s written", f)
		}
	}
}
