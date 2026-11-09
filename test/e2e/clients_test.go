//go:build e2e

package e2e

// Real-client cases: drive the installed Claude Code and Codex CLIs through
// a live relay on a small agentic task (write a file, run it), over both a
// same-protocol-friendly route and a chat-only upstream that forces
// cross-protocol conversion. Client configuration is written by
// `plugrelay setup` into a throwaway home, so the user's own ~/.claude and
// ~/.codex are never read or written.
//
//	PLUGRELAY_E2E_CLIENTS=1        opt in (the runs take minutes and spend tokens)
//	PLUGRELAY_E2E_CHAT_MODEL       model on the chat-only route (default deepseek-chat)
//
// Each client is skipped when its binary is not on PATH.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const defaultChatModel = "deepseek-chat"

// task is the agentic job every client gets; checkTask verifies its effect.
const task = "Create a file named answer.py that prints the product of 6 and 7. " +
	"Then run it with python3 and reply with its output."

// chatOnlyManifestDoc stores a manifest with a single openai-chat endpoint.
const chatOnlyManifestDoc = `manifests:
  - id: e2e-chat-only
    yaml: |
      id: e2e-chat-only
      kind: provider
      name: Chat only (e2e)
      interpreter: openai-compatible
      variables:
        base_url: {default: "http://localhost/v1"}
      auth: {header: Authorization, scheme: Bearer}
      endpoints:
        - {id: chat, protocol: openai-chat, base_url: "{base_url}"}
`

// clientTimeout bounds one client run; slow upstreams can take minutes.
const clientTimeout = 8 * time.Minute

func TestClients(t *testing.T) {
	upBase, upKey := os.Getenv("PLUGRELAY_E2E_BASE"), os.Getenv("PLUGRELAY_E2E_KEY")
	if upBase == "" || upKey == "" || os.Getenv("PLUGRELAY_E2E_CLIENTS") == "" {
		t.Skip("set PLUGRELAY_E2E_BASE, PLUGRELAY_E2E_KEY and PLUGRELAY_E2E_CLIENTS=1 to drive real clients")
	}
	model := envOr("PLUGRELAY_E2E_MODEL", defaultModel)
	chatModel := envOr("PLUGRELAY_E2E_CHAT_MODEL", defaultChatModel)
	r := start(t, upBase, model)
	// A second upstream that speaks only chat completions: every request
	// from Claude Code (Anthropic) or Codex (Responses) must be converted.
	// Its manifest is the test's own, so no shipped manifest gaining an
	// endpoint can turn this into passthrough.
	doc := filepath.Join(t.TempDir(), "chat-only.yaml")
	_ = os.WriteFile(doc, []byte(chatOnlyManifestDoc), 0o600)
	r.run("apply", "-f", doc)
	r.run("upstream", "add", "--manifest", "e2e-chat-only", "--id", "e2e-chat",
		"--credential-env", "PLUGRELAY_E2E_KEY", "--var", "base_url="+upBase)
	r.run("model", "add", "--name", "e2e-chat", "--route", "e2e-chat:"+chatModel)

	routes := []struct{ name, model, upstream, upstreamModel string }{
		{"responses upstream", model, "e2e", model},
		{"chat-only upstream", "e2e-chat", "e2e-chat", chatModel},
	}
	for _, c := range []struct {
		name, bin string
		run       func(t *testing.T, r *relay, home, proj, model string) *exec.Cmd
	}{
		{"claude-code", "claude", claudeCmd},
		{"codex", "codex", codexCmd},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := exec.LookPath(c.bin); err != nil {
				t.Skipf("%s not on PATH", c.bin)
			}
			for _, rt := range routes {
				t.Run(rt.name, func(t *testing.T) {
					t.Logf("compat: model=%s", rt.upstreamModel) // read by scripts/compat.sh
					home, proj := t.TempDir(), t.TempDir()
					since := time.Now()
					cmd := c.run(t, r, home, proj, rt.model)
					out, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("%s: %v\n%s", c.name, err, tail(out))
					}
					checkTask(t, proj, out)
					checkLog(t, r, rt.upstream, since)
				})
			}
		})
	}
}

// claudeCmd configures Claude Code with `plugrelay setup claude-code` in an
// isolated config dir and returns a non-interactive run of the task.
func claudeCmd(t *testing.T, r *relay, home, proj, model string) *exec.Cmd {
	r.run("setup", "claude-code", "--home", home, "--url", r.base, "--key", r.key, "--model", model)
	ctx := runCtx(t)
	cmd := exec.CommandContext(ctx, "claude", "-p", task, "--dangerously-skip-permissions", "--output-format", "text")
	cmd.Dir = proj
	cmd.Env = isolatedEnv(home, "CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"))
	return cmd
}

// codexCmd configures Codex with `plugrelay setup codex` (which also saves
// the relay's model catalog) and returns `codex exec` of the task.
func codexCmd(t *testing.T, r *relay, home, proj, model string) *exec.Cmd {
	r.run("setup", "codex", "--home", home, "--url", r.base, "--key", r.key, "--model", model)
	ctx := runCtx(t)
	cmd := exec.CommandContext(ctx, "codex", "exec", "--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox", task)
	cmd.Dir = proj
	cmd.Stdin = nil // codex exec reads stdin when it is not a terminal
	cmd.Env = isolatedEnv(home, "CODEX_HOME="+filepath.Join(home, ".codex"), "PLUGRELAY_API_KEY="+r.key)
	return cmd
}

// isolatedEnv is the process environment with HOME moved to home and any
// client configuration from the caller's environment removed.
func isolatedEnv(home string, extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "HOME", k == "CLAUDE_CONFIG_DIR", k == "CODEX_HOME",
			strings.HasPrefix(k, "ANTHROPIC_"), strings.HasPrefix(k, "OPENAI_"), strings.HasPrefix(k, "CLAUDE_CODE_"):
			continue
		}
		env = append(env, kv)
	}
	return append(append(env, "HOME="+home), extra...)
}

// checkTask verifies the client really wrote and ran the program.
func checkTask(t *testing.T, proj string, out []byte) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(proj, "answer.py"))
	if err != nil {
		t.Fatalf("answer.py not written: %v\nclient output:\n%s", err, tail(out))
	}
	run := exec.Command("python3", "answer.py")
	run.Dir = proj
	got, err := run.Output()
	if err != nil || strings.TrimSpace(string(got)) != "42" {
		t.Fatalf("answer.py printed %q (%v); source:\n%s", got, err, src)
	}
	if !strings.Contains(string(out), "42") {
		t.Errorf("client did not report the output; it said:\n%s", tail(out))
	}
}

// checkLog asserts the relay served the run through the expected upstream
// with every request succeeding. The local owner reads the admin API from
// loopback without a login.
func checkLog(t *testing.T, r *relay, upstream string, since time.Time) {
	t.Helper()
	resp, err := http.Get(r.base + "/api/v1/logs?limit=200")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page struct {
		Items []struct {
			Time     time.Time `json:"time"`
			Status   int       `json:"status"`
			Tier     string    `json:"tier"`
			Upstream struct {
				ID string `json:"id"`
			} `json:"upstream"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("logs: %v", err)
	}
	n := 0
	for _, it := range page.Items {
		// The log stores milliseconds; compare at that precision so a record
		// written in the same millisecond as since still counts.
		if it.Time.Before(since.Truncate(time.Millisecond)) || it.Upstream.ID != upstream {
			continue
		}
		n++
		if it.Status != http.StatusOK {
			t.Errorf("request via %s ended with status %d", upstream, it.Status)
		}
		if upstream == "e2e-chat" && it.Tier != "convert" {
			t.Errorf("chat-only upstream served tier %q, want convert", it.Tier)
		}
	}
	if n == 0 {
		t.Fatalf("no request log entries via %s since %s", upstream, since.Format(time.RFC3339))
	}
}

func runCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	t.Cleanup(cancel)
	return ctx
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// tail keeps the end of long client output for failure messages.
func tail(b []byte) string {
	const max = 4000
	if len(b) > max {
		return "…" + string(b[len(b)-max:])
	}
	return string(b)
}
