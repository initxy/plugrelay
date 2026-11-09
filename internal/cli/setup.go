package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// setupEnvKey is the variable Codex reads the platform key from.
const setupEnvKey = "PLUGRELAY_API_KEY"

// setupProvider is the provider name written into client configs.
const setupProvider = "plugrelay"

// codexCatalogFile is the model catalog setup codex writes next to
// config.toml. Codex reads per-model metadata (context window, reasoning
// levels, tool formats) for custom providers only from a local file named by
// the top-level model_catalog_json key; it does not fetch it from the
// provider. The file is a snapshot: re-run setup after adding models.
const codexCatalogFile = "plugrelay-models.json"

// codexCatalogVersion is sent as client_version so the relay answers in the
// coding-agent catalog format rather than the OpenAI list format.
const codexCatalogVersion = "0.166.0"

func setupCommand() *command {
	leaf := func(name, summary, example string, run func(r *runner, o setupOpts) error) *command {
		return &command{
			name:    name,
			summary: summary,
			usage:   "setup " + name + " --key KEY [--url URL] [--model M] [--home DIR] [--print]",
			example: example,
			run: func(r *runner, cmd *command, args []string) error {
				o, err := parseSetup(r, cmd, args)
				if err != nil {
					return err
				}
				return run(r, o)
			},
		}
	}
	return &command{
		name: "setup",
		summary: `Point a local coding client at this relay.

Files are edited in place: only the keys plugrelay owns change, and the
previous file is kept as <file>.bak-<timestamp>. --print shows the result
without writing anything. --url defaults to the listen address of
relay.yaml.`,
		usage: "setup <claude-code|codex|gemini|opencode> [flags]",
		subs: []*command{
			leaf("claude-code", "Write ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN into ~/.claude/settings.json.",
				"plugrelay setup claude-code --key pr_... --model gpt-5.5[,small-model]", setupClaude),
			leaf("codex", "Add a plugrelay model provider to ~/.codex/config.toml and select it.",
				"plugrelay setup codex --key pr_... --model gpt-5.5", setupCodex),
			leaf("gemini", "Print the environment Gemini CLI needs (it has no file to edit).",
				"eval \"$(plugrelay setup gemini --key pr_... --print)\"", setupGemini),
			leaf("opencode", "Add a plugrelay provider to ~/.config/opencode/opencode.json.",
				"plugrelay setup opencode --key pr_... --model claude-sonnet-4-5,gpt-5.5", setupOpenCode),
		},
	}
}

type setupOpts struct {
	url    string // without trailing slash or /v1
	key    string
	models []string
	home   string
	print  bool
	// codexCatalog is the model_catalog_json path written into Codex's
	// config, or "" when no catalog could be fetched.
	codexCatalog string
}

func parseSetup(r *runner, cmd *command, args []string) (setupOpts, error) {
	fset := r.flags(cmd)
	url := fset.String("url", "", "relay base URL (default: the listen address in relay.yaml)")
	key := fset.String("key", "", "platform key pr_... (default: $"+setupEnvKey+")")
	model := fset.String("model", "", "default model; opencode takes a comma-separated list")
	home := fset.String("home", "", "home directory holding the client config (default: your home)")
	print := fset.Bool("print", false, "print the result instead of writing files")
	pos, err := r.parse(fset, args)
	if err != nil {
		return setupOpts{}, err
	}
	if len(pos) > 0 {
		return setupOpts{}, usagef("unexpected arguments %q", pos)
	}
	o := setupOpts{url: *url, key: *key, models: splitCSV(*model), home: *home, print: *print}
	if o.key == "" {
		o.key = r.env.getenv(setupEnvKey)
	}
	if o.key == "" {
		return o, usagef("--key is required (create one with 'plugrelay key create')")
	}
	if o.url == "" {
		cfg, err := r.loadConfig()
		if err != nil {
			return o, fmt.Errorf("--url not given and relay.yaml unreadable: %w", err)
		}
		o.url = "http://" + strings.Replace(strings.Replace(cfg.Listen, "0.0.0.0:", "127.0.0.1:", 1), "[::]:", "127.0.0.1:", 1)
	}
	if !strings.HasPrefix(o.url, "http://") && !strings.HasPrefix(o.url, "https://") {
		return o, usagef("--url must start with http:// or https://, got %q", o.url)
	}
	o.url = strings.TrimSuffix(strings.TrimRight(o.url, "/"), "/v1")
	if o.home == "" && !o.print {
		if r.env.HomeDir == nil {
			return o, errors.New("no home directory; pass --home")
		}
		if o.home, err = r.env.HomeDir(); err != nil {
			return o, err
		}
	}
	return o, nil
}

func (o setupOpts) model() string {
	if len(o.models) == 0 {
		return ""
	}
	return o.models[0]
}

// claudeSettings merges the relay into Claude Code's settings.json.
func claudeSettings(src []byte, o setupOpts) ([]byte, error) {
	root, err := parseObject(src)
	if err != nil {
		return nil, fmt.Errorf("settings.json: %w", err)
	}
	env, err := root.child("env")
	if err != nil {
		return nil, fmt.Errorf("settings.json: %w", err)
	}
	vals := [][2]string{
		{"ANTHROPIC_BASE_URL", o.url},
		{"ANTHROPIC_AUTH_TOKEN", o.key},
		{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1"},
	}
	if m := o.model(); m != "" {
		// Claude Code also sends side tasks (titles, summaries) to its
		// small model, whose official name the relay may not serve; point
		// it at the same model unless a second --model names one.
		small := m
		if len(o.models) > 1 {
			small = o.models[1]
		}
		vals = append(vals, [2]string{"ANTHROPIC_MODEL", m},
			[2]string{"ANTHROPIC_DEFAULT_HAIKU_MODEL", small})
	}
	for _, kv := range vals {
		if err := env.setValue(kv[0], kv[1]); err != nil {
			return nil, err
		}
	}
	if err := root.setValue("env", env); err != nil {
		return nil, err
	}
	return root.encode()
}

// codexConfig edits Codex's config.toml.
func codexConfig(src []byte, o setupOpts) []byte {
	lines := splitLines(src)
	lines = tomlSetTopLevel(lines, "model_provider", tomlString(setupProvider))
	if o.codexCatalog != "" {
		lines = tomlSetTopLevel(lines, "model_catalog_json", tomlString(o.codexCatalog))
	}
	if m := o.model(); m != "" {
		lines = tomlSetTopLevel(lines, "model", tomlString(m))
	}
	lines = tomlReplaceTable(lines, "model_providers."+setupProvider, []string{
		"[model_providers." + setupProvider + "]",
		"name = " + tomlString(setupProvider),
		"base_url = " + tomlString(o.url+"/v1"),
		"env_key = " + tomlString(setupEnvKey),
		"wire_api = \"responses\"",
	})
	return joinLines(lines)
}

// openCodeConfig merges a provider block into opencode.json.
func openCodeConfig(src []byte, o setupOpts) ([]byte, error) {
	root, err := parseObject(src)
	if err != nil {
		return nil, fmt.Errorf("opencode.json: %w", err)
	}
	if _, ok := root.get("$schema"); !ok && len(root) == 0 {
		_ = root.setValue("$schema", "https://opencode.ai/config.json")
	}
	providers, err := root.child("provider")
	if err != nil {
		return nil, fmt.Errorf("opencode.json: %w", err)
	}
	models := jsonObject{}
	for _, m := range o.models {
		_ = models.setValue(m, map[string]string{"name": m})
	}
	block := jsonObject{}
	_ = block.setValue("npm", "@ai-sdk/openai-compatible")
	_ = block.setValue("name", setupProvider)
	_ = block.setValue("options", jsonObject{
		{Key: "baseURL", Value: mustJSON(o.url + "/v1")},
		{Key: "apiKey", Value: mustJSON(o.key)},
	})
	_ = block.setValue("models", models)
	_ = providers.setValue(setupProvider, block)
	_ = root.setValue("provider", providers)
	if m := o.model(); m != "" {
		_ = root.setValue("model", setupProvider+"/"+m)
	}
	return root.encode()
}

func mustJSON(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func setupClaude(r *runner, o setupOpts) error {
	path := filepath.Join(o.home, ".claude", "settings.json")
	return r.editFile(o, path, 0o600, func(src []byte) ([]byte, error) { return claudeSettings(src, o) },
		"Claude Code now uses "+o.url+". Restart running sessions.")
}

func setupCodex(r *runner, o setupOpts) error {
	dir := filepath.Join(o.home, ".codex")
	catalog, err := fetchCodexCatalog(r.ctx(), o)
	switch {
	case errors.Is(err, errNoModels):
		return err
	case err != nil:
		fmt.Fprintf(r.env.Stderr, "warning: could not fetch the model catalog (%v); Codex will use fallback model metadata. Re-run setup once the relay is reachable.\n", err)
	case o.print:
		// Codex refuses to start when model_catalog_json names a missing
		// file, so a dry run leaves the key out rather than point at a file
		// it did not write.
		fmt.Fprintf(r.env.Stderr, "note: without --print, setup also saves the model catalog as %s and sets model_catalog_json\n", filepath.Join(dir, codexCatalogFile))
	default:
		path := filepath.Join(dir, codexCatalogFile)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, catalog, 0o644); err != nil {
			return fmt.Errorf("write model catalog: %w", err)
		}
		o.codexCatalog = path
	}
	path := filepath.Join(dir, "config.toml")
	err = r.editFile(o, path, 0o644, func(src []byte) ([]byte, error) { return codexConfig(src, o), nil },
		"Codex now uses provider "+setupProvider+". It reads the key from $"+setupEnvKey+"; add this to your shell profile:\n  export "+setupEnvKey+"="+shellQuote(o.key))
	if err == nil && o.model() == "" && !o.print {
		fmt.Fprintln(r.env.Stderr, "note: no --model given; Codex keeps its configured model, which the relay must serve")
	}
	return err
}

// fetchCodexCatalog downloads the relay's model list in Codex's catalog
// format, authenticated with the platform key.
func fetchCodexCatalog(ctx context.Context, o setupOpts) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.url+"/v1/models?client_version="+codexCatalogVersion, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+o.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/models: HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Models == nil {
		return nil, errors.New("GET /v1/models: not a catalog response")
	}
	if len(doc.Models) == 0 {
		return nil, errNoModels
	}
	return body, nil
}

// errNoModels means the key sees no model. Codex refuses to start with an
// empty model_catalog_json, so setup stops instead of writing one.
var errNoModels = errors.New("the relay lists no models for this key; give the key (or its group) at least one model, then re-run setup")

func setupOpenCode(r *runner, o setupOpts) error {
	if len(o.models) == 0 {
		return usagef("opencode needs --model: it only offers models listed in its config")
	}
	path := filepath.Join(o.home, ".config", "opencode", "opencode.json")
	return r.editFile(o, path, 0o600, func(src []byte) ([]byte, error) { return openCodeConfig(src, o) },
		"OpenCode now has provider "+setupProvider+"; pick its models with /models.")
}

func setupGemini(r *runner, o setupOpts) error {
	w := r.env.Stdout
	fmt.Fprintf(w, "export GOOGLE_GEMINI_BASE_URL=%s\n", shellQuote(o.url))
	fmt.Fprintf(w, "export GEMINI_API_KEY=%s\n", shellQuote(o.key))
	if m := o.model(); m != "" {
		fmt.Fprintf(w, "export GEMINI_MODEL=%s\n", shellQuote(m))
	}
	if !o.print {
		fmt.Fprintln(r.env.Stderr, "Add these lines to your shell profile; Gemini CLI has no file plugrelay edits.")
	}
	return nil
}

// editFile applies edit to the file at path (missing = empty), backs the
// old file up and writes the result, or prints it with --print.
func (r *runner) editFile(o setupOpts, path string, perm os.FileMode, edit func([]byte) ([]byte, error), done string) error {
	src, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	out, err := edit(src)
	if err != nil {
		return err
	}
	if o.print {
		_, err := r.env.Stdout.Write(out)
		return err
	}
	if exists {
		if string(src) == string(out) {
			fmt.Fprintf(r.env.Stdout, "%s already up to date\n", path)
			return nil
		}
		if fi, err := os.Stat(path); err == nil {
			perm = fi.Mode().Perm()
		}
		backup := path + ".bak-" + r.env.Now().Format("20060102-150405")
		if err := os.WriteFile(backup, src, 0o600); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		fmt.Fprintf(r.env.Stdout, "Backed up %s to %s\n", path, backup)
	}
	if err := writeFileAtomic(path, out, perm); err != nil {
		return err
	}
	fmt.Fprintf(r.env.Stdout, "Wrote %s\n%s\n", path, done)
	return nil
}

// shellQuote single-quotes s for POSIX shells.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
