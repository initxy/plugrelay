package clientid

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestConsoleNamesEveryClient: the console words each client in its own
// language from "client.<id>" entries; every id here (and other) needs one,
// and the console keeps none for an id that is gone.
func TestConsoleNamesEveryClient(t *testing.T) {
	src, err := os.ReadFile("../../web/src/i18n/en.ts")
	if err != nil {
		t.Fatal(err)
	}
	console := map[string]bool{}
	for _, m := range regexp.MustCompile(`"client\.([\w-]+)":`).FindAllStringSubmatch(string(src), -1) {
		console[m[1]] = true
	}
	want := map[string]bool{Other: true}
	for _, c := range Known() {
		want[c.ID] = true
	}
	for id := range want {
		if !console[id] {
			t.Errorf(`en.ts lacks "client.%s"`, id)
		}
	}
	for id := range console {
		if !want[id] {
			t.Errorf(`en.ts has "client.%s", which is no client id`, id)
		}
	}
}

func TestDetect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		id, ver string
	}{
		{"claude code", map[string]string{"User-Agent": "claude-cli/2.0.14 (external, cli)", "X-Stainless-Lang": "js"}, "claude-code", "2.0.14"},
		{"claude agent sdk", map[string]string{"User-Agent": "claude-cli/1.0.83 (external, sdk-ts)"}, "claude-code", "1.0.83"},
		{"codex cli", map[string]string{"User-Agent": "codex_cli_rs/0.46.0 (Mac OS 15.6.1; arm64) iTerm.app/3.6.1", "Originator": "codex_cli_rs"}, "codex", "0.46.0"},
		{"codex vscode", map[string]string{"User-Agent": "codex_vscode/0.42.0 (Windows 10.0.26100; x86_64) unknown (VS Code; 1.104.1)"}, "codex", "0.42.0"},
		{"codex exec", map[string]string{"User-Agent": "codex_exec/0.50.0 (Ubuntu 24.4.0; x86_64) xterm-256color"}, "codex", "0.50.0"},
		{"codex originator only", map[string]string{"User-Agent": "reqwest/0.12", "Originator": "codex_cli_rs"}, "codex", ""},
		{"gemini cli", map[string]string{"User-Agent": "GeminiCLI/0.8.2 (darwin; arm64)"}, "gemini-cli", "0.8.2"},
		{"gemini cli node version", map[string]string{"User-Agent": "GeminiCLI/v22.17.0 (linux; x64)"}, "gemini-cli", "22.17.0"},
		{"cline title", map[string]string{"User-Agent": "OpenAI/JS 4.96.0", "X-Title": "Cline", "HTTP-Referer": "https://cline.bot", "X-Stainless-Lang": "js"}, "cline", ""},
		{"cline ua", map[string]string{"User-Agent": "Cline/3.32.5"}, "cline", "3.32.5"},
		{"roo code", map[string]string{"User-Agent": "OpenAI/JS 4.78.1", "X-Title": "Roo Code", "HTTP-Referer": "https://github.com/RooVetGit/Roo-Cline"}, "roo", ""},
		{"roo code ua", map[string]string{"User-Agent": "RooCode/3.25.15", "X-Title": "Roo Code"}, "roo", "3.25.15"},
		{"kilo code", map[string]string{"User-Agent": "Kilo-Code/4.97.0", "X-Title": "Kilo Code", "HTTP-Referer": "https://kilocode.ai"}, "kilo", "4.97.0"},
		{"opencode", map[string]string{"User-Agent": "opencode/0.15.8 ai-sdk/provider-utils/3.0.10 runtime/bun/1.2.21"}, "opencode", "0.15.8"},
		{"opencode referer", map[string]string{"User-Agent": "ai-sdk/openai-compatible/1.0.19", "HTTP-Referer": "https://opencode.ai/", "X-Title": "opencode"}, "opencode", ""},
		{"continue", map[string]string{"User-Agent": "OpenAI/JS 4.104.0", "X-Title": "Continue", "HTTP-Referer": "https://www.continue.dev/"}, "continue", ""},
		{"cursor", map[string]string{"User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Cursor/1.7.38 Chrome/132.0.6834.210 Electron/34.5.8 Safari/537.36"}, "cursor", "1.7.38"},
		{"zed", map[string]string{"User-Agent": "Zed/0.207.3 (macos; aarch64)"}, "zed", "0.207.3"},
		{"curl", map[string]string{"User-Agent": "curl/8.7.1"}, "curl", "8.7.1"},
		{"playground", map[string]string{"User-Agent": "plugrelay-playground/1"}, "playground", "1"},
		{"anthropic python", map[string]string{"User-Agent": "Anthropic/Python 0.69.0", "X-Stainless-Lang": "python", "X-Stainless-Package-Version": "0.69.0"}, "sdk-python", "0.69.0"},
		{"openai async python", map[string]string{"User-Agent": "AsyncOpenAI/Python 1.109.1"}, "sdk-python", "1.109.1"},
		{"genai python", map[string]string{"User-Agent": "google-genai-sdk/1.38.0 gl-python/3.12.3"}, "sdk-python", "1.38.0"},
		{"stainless python behind httpx", map[string]string{"User-Agent": "python-httpx/0.28.1", "X-Stainless-Lang": "python", "X-Stainless-Package-Version": "1.2.3"}, "sdk-python", "1.2.3"},
		{"openai node", map[string]string{"User-Agent": "OpenAI/JS 5.23.1", "X-Stainless-Lang": "js"}, "sdk-node", "5.23.1"},
		{"anthropic node", map[string]string{"User-Agent": "Anthropic/JS 0.65.0"}, "sdk-node", "0.65.0"},
		{"genai node", map[string]string{"User-Agent": "google-genai-sdk/1.20.0 gl-node/v22.17.0"}, "sdk-node", "1.20.0"},
		{"browser", map[string]string{"User-Agent": "Mozilla/5.0 (X11; Linux x86_64) Firefox/131.0"}, Other, ""},
		{"python requests", map[string]string{"User-Agent": "python-requests/2.32.3"}, Other, ""},
		{"nothing", map[string]string{}, Other, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.headers {
				h.Set(k, v)
			}
			got := Detect(h)
			if got.ID != tc.id || got.Version != tc.ver {
				t.Fatalf("Detect = %+v, want %s %q", got, tc.id, tc.ver)
			}
		})
	}
}

func TestFromUserAgent(t *testing.T) {
	if c := FromUserAgent("claude-cli/2.1.31 (external, cli)"); c.ID != "claude-code" || c.Version != "2.1.31" {
		t.Fatalf("got %+v", c)
	}
	if c := FromUserAgent(""); c.ID != Other {
		t.Fatalf("empty: %+v", c)
	}
}

// Every rule has a unique id and a name, and the version stays inside the
// stored width.
func TestRulesWellFormed(t *testing.T) {
	seen := map[string]bool{Other: true}
	for _, r := range rules {
		if seen[r.id] || r.name == "" || len(r.id) > MaxLen || len(r.match) == 0 {
			t.Errorf("rule %q: duplicate, unnamed, too long or empty", r.id)
		}
		seen[r.id] = true
		if Name(r.id) != r.name {
			t.Errorf("Name(%q) = %q", r.id, Name(r.id))
		}
	}
	if Name(Other) != "" || len(Known()) != len(rules) {
		t.Fatal("Other has a name or Known is incomplete")
	}
	long := FromUserAgent("curl/1." + strings.Repeat("9", 100))
	if len(long.Version) > MaxLen {
		t.Fatalf("version of %d bytes", len(long.Version))
	}
}
