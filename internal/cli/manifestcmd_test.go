package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodManifest = `id: example
kind: provider
name: Example
auth:
  header: Authorization
endpoints:
  - protocol: openai-chat
    base_url: https://api.example.com/v1
    interpreter: openai-compatible
`

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestManifestValidate(t *testing.T) {
	badURL := strings.Replace(goodManifest, "https://api.example.com/v1", "api.example.com", 1)
	badBoth := strings.Replace(badURL, "kind: provider", "kind: plugin", 1)
	withPricing := strings.Replace(goodManifest, "name: Example\n", "name: Example\npricing: prices/example.yaml\n", 1)
	dir := writeFiles(t, map[string]string{
		"good.yaml":                  goodManifest,
		"bad-url.yaml":               badURL,
		"sub/bad-both.yml":           badBoth,
		"syntax.yaml":                "id: [unclosed\n",
		"unknown.yaml":               goodManifest + "colour: blue\n",
		"priced/ok.yaml":             withPricing,
		"priced/prices/example.yaml": "currency: USD\nmodels:\n  m1: {input: 1, output: 2}\n",
		"notes.txt":                  "ignored",
	})

	cases := []struct {
		name  string
		args  []string
		code  int
		lines []string
	}{
		{"good file", []string{filepath.Join(dir, "good.yaml")}, ExitOK, []string{"1 manifest(s) valid"}},
		{"priced dir", []string{filepath.Join(dir, "priced")}, ExitOK, []string{"1 manifest(s) valid"}},
		{"bad file", []string{filepath.Join(dir, "bad-url.yaml")}, ExitError,
			[]string{filepath.Join(dir, "bad-url.yaml") + ": endpoints[0].base_url: "}},
		{"tree", []string{dir}, ExitError, []string{
			filepath.Join(dir, "bad-url.yaml") + ": endpoints[0].base_url: ",
			filepath.Join(dir, "sub", "bad-both.yml") + ": kind: ",
			filepath.Join(dir, "sub", "bad-both.yml") + ": endpoints[0].base_url: ",
			filepath.Join(dir, "syntax.yaml") + ": ",
			filepath.Join(dir, "unknown.yaml") + ": ",
		}},
		{"missing", []string{filepath.Join(dir, "nope.yaml")}, ExitError, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			te := newTestEnv(t)
			code := te.run(append([]string{"manifest", "validate"}, tc.args...)...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d\n%s%s", code, tc.code, te.stdout, te.stderr)
			}
			out := te.stdout.String()
			for _, l := range tc.lines {
				if !strings.Contains(out, l) {
					t.Errorf("output lacks %q:\n%s", l, out)
				}
			}
			if strings.Contains(out, "good.yaml:") || strings.Contains(out, "prices/example.yaml") {
				t.Errorf("valid or pricing file reported:\n%s", out)
			}
		})
	}
}

func TestManifestValidateOfficial(t *testing.T) {
	te := newTestEnv(t)
	if code := te.run("manifest", "validate", filepath.Join("..", "..", "manifests")); code != ExitOK {
		t.Fatalf("official manifests invalid:\n%s", te.stdout)
	}
}

func TestManifestList(t *testing.T) {
	te := newTestEnv(t)
	if code := te.run("manifest", "list", "--json"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, te.stderr)
	}
	var list []manifestInfo
	if err := json.Unmarshal(te.stdout.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range list {
		if m.ID == "azure-openai" {
			found = strings.Contains(strings.Join(m.Variables, ","), "resource")
		}
	}
	if len(list) < 10 || !found {
		t.Errorf("manifest list = %+v", list)
	}
}
