//go:build e2e

package e2e

// The MCP gateway against the official reference server
// (@modelcontextprotocol/server-everything over Streamable HTTP), driven by
// the official TypeScript SDK client and, when a model upstream is set and
// the binary is on PATH, by Claude Code itself.
//
//	PLUGRELAY_E2E_MCP=1             opt in (installs both packages with npm)
//	PLUGRELAY_E2E_MCP_EVERYTHING    server version (default 2.2.1)
//	PLUGRELAY_E2E_MCP_SDK           client SDK version (default 1.32.2)
//
// The Claude Code case also needs PLUGRELAY_E2E_BASE and PLUGRELAY_E2E_KEY.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// mcpClientJS lists the tools at a URL and calls one with {message}, printing
// one JSON line: {protocol, server, tools, call | callError | error}.
const mcpClientJS = `import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
const [url, key, tool, message] = process.argv.slice(2);
const client = new Client({ name: "plugrelay-e2e", version: "1.0.0" });
const out = {};
try {
  await client.connect(new StreamableHTTPClientTransport(new URL(url),
    { requestInit: { headers: key ? { Authorization: "Bearer " + key } : {} } }));
  out.protocol = client.transport?.protocolVersion ?? null;
  const v = client.getServerVersion();
  out.server = v ? v.name + "@" + v.version : null;
  out.tools = (await client.listTools()).tools.map((t) => t.name);
  try {
    out.call = await client.callTool({ name: tool, arguments: { message } });
  } catch (e) {
    out.callError = { code: e.code ?? null, message: String(e.message ?? e) };
  }
  await client.close();
} catch (e) {
  out.error = String(e?.stack ?? e);
}
console.log(JSON.stringify(out));
`

type mcpRun struct {
	Protocol string   `json:"protocol"`
	Server   string   `json:"server"`
	Tools    []string `json:"tools"`
	Call     *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"call"`
	CallError *struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
	} `json:"callError"`
	Error string `json:"error"`
}

func (m mcpRun) text() string {
	if m.Call == nil || len(m.Call.Content) == 0 {
		return ""
	}
	return m.Call.Content[0].Text
}

func TestMCPReferenceServer(t *testing.T) {
	if os.Getenv("PLUGRELAY_E2E_MCP") == "" {
		t.Skip("set PLUGRELAY_E2E_MCP=1 to run the MCP gateway against the official reference server (needs node and npm)")
	}
	for _, bin := range []string{"node", "npm"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	js := installMCPPackages(t)
	srvURL := startEverything(t, js)

	// A model upstream is only needed for the Claude Code case.
	upBase, upKey := os.Getenv("PLUGRELAY_E2E_BASE"), os.Getenv("PLUGRELAY_E2E_KEY")
	model := envOr("PLUGRELAY_E2E_MODEL", defaultModel)
	env := []string{"PLUGRELAY_ADMIN_TOKEN=" + e2eAdminToken}
	var models []string
	if upBase == "" || upKey == "" {
		upBase = "http://127.0.0.1:9/v1"
		env = append(env, "PLUGRELAY_E2E_KEY=unused")
	} else {
		models = []string{model}
	}
	r := startWith(t, upBase, env, models...)

	r.admin(t, "PATCH", "/api/v1/features/mcp", map[string]any{"enabled": true}, nil)
	r.admin(t, "POST", "/api/v1/mcp/servers", map[string]any{
		"id": "everything", "url": srvURL, "transport": "auto", "allow_private": true}, nil)
	var outcome struct {
		Results []struct {
			OK        bool     `json:"ok"`
			Transport string   `json:"transport"`
			Protocol  string   `json:"protocol_version"`
			Tools     []string `json:"tools"`
			Error     string   `json:"error"`
		} `json:"results"`
	}
	r.admin(t, "POST", "/api/v1/mcp/servers/everything/test", nil, &outcome)
	if len(outcome.Results) != 1 {
		t.Fatalf("server test: %+v", outcome)
	}
	probe := outcome.Results[0]
	if !probe.OK || !slices.Contains(probe.Tools, "echo") {
		t.Fatalf("server test: %+v", probe)
	}
	t.Logf("server test: transport %s, protocol %s, %d tools", probe.Transport, probe.Protocol, len(probe.Tools))

	// Key 1 may use every server; key 2 the same, minus the echo tool.
	grant := func(keyID string, values map[string]any) {
		r.admin(t, "PUT", "/api/v1/keys/"+keyID+"/settings",
			map[string]any{"settings": map[string]any{"middleware.access": values}}, nil)
	}
	grant(r.keyID, map[string]any{"mcp_servers": []string{"*"}})
	var k2 struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(r.run("key", "create", "--name", "e2e-deny", "--json")), &k2); err != nil {
		t.Fatal(err)
	}
	grant(k2.ID, map[string]any{"mcp_servers": []string{"*"}, "mcp_tools_deny": []string{"everything__echo"}})

	client := func(t *testing.T, path, key, tool, msg string) mcpRun {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "node", filepath.Join(js, "client.mjs"), r.base+path, key, tool, msg)
		cmd.Dir = js
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("client: %v\n%s", err, out)
		}
		var m mcpRun
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("client output %q: %v", out, err)
		}
		callErr := ""
		if m.CallError != nil {
			callErr = m.CallError.Message
		}
		connErr, _, _ := strings.Cut(m.Error, "\n")
		t.Logf("%s %s: protocol %s, server %s, %d tools, result %q, call error %q, error %q",
			path, tool, m.Protocol, m.Server, len(m.Tools), m.text(), callErr, connErr)
		return m
	}
	// The grant reaches the gateway on the next reload.
	waitGranted := func(t *testing.T, key string) {
		deadline := time.Now().Add(20 * time.Second)
		for {
			m := client(t, "/mcp/everything", key, "get-sum", "")
			if m.Error == "" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("still refused after the grant: %s", m.Error)
			}
			time.Sleep(time.Second)
		}
	}

	t.Run("single server", func(t *testing.T) {
		waitGranted(t, r.key)
		since := time.Now()
		m := client(t, "/mcp/everything", r.key, "echo", "hello through plugrelay")
		if m.Error != "" || !slices.Contains(m.Tools, "echo") {
			t.Fatalf("tools %v, error %s", m.Tools, m.Error)
		}
		if got := m.text(); !strings.Contains(got, "hello through plugrelay") {
			t.Fatalf("echo returned %q (%+v)", got, m.CallError)
		}
		checkMCPLog(t, r, "everything__echo", since)
	})

	t.Run("aggregated", func(t *testing.T) {
		m := client(t, "/mcp", r.key, "everything__echo", "hello via /mcp")
		if m.Error != "" || !slices.Contains(m.Tools, "everything__echo") {
			t.Fatalf("tools %v, error %s", m.Tools, m.Error)
		}
		if got := m.text(); !strings.Contains(got, "hello via /mcp") {
			t.Fatalf("echo returned %q (%+v)", got, m.CallError)
		}
	})

	t.Run("deny rule hides the tool", func(t *testing.T) {
		waitGranted(t, k2.Key)
		for _, c := range []struct{ path, tool string }{{"/mcp/everything", "echo"}, {"/mcp", "everything__echo"}} {
			m := client(t, c.path, k2.Key, c.tool, "must not arrive")
			if m.Error != "" || len(m.Tools) == 0 {
				t.Fatalf("%s: tools %v, error %s", c.path, m.Tools, m.Error)
			}
			if slices.Contains(m.Tools, c.tool) {
				t.Fatalf("%s: denied tool %s still listed", c.path, c.tool)
			}
			if m.Call != nil && !m.Call.IsError {
				t.Fatalf("%s: denied tool answered %q", c.path, m.text())
			}
			if m.CallError == nil && (m.Call == nil || !m.Call.IsError) {
				t.Fatalf("%s: denied call neither failed nor errored", c.path)
			}
		}
	})

	t.Run("no key", func(t *testing.T) {
		if m := client(t, "/mcp", "", "everything__echo", "x"); m.Error == "" {
			t.Fatalf("no key was accepted: %+v", m)
		}
	})

	t.Run("claude-code", func(t *testing.T) {
		if len(models) == 0 {
			t.Skip("set PLUGRELAY_E2E_BASE and PLUGRELAY_E2E_KEY to drive Claude Code")
		}
		if _, err := exec.LookPath("claude"); err != nil {
			t.Skip("claude not on PATH")
		}
		home, proj := t.TempDir(), t.TempDir()
		r.run("setup", "claude-code", "--home", home, "--url", r.base, "--key", r.key, "--model", model)
		cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"plugrelay": map[string]any{
			"type": "http", "url": r.base + "/mcp", "headers": map[string]string{"Authorization": "Bearer " + r.key}}}})
		mcpCfg := filepath.Join(home, "mcp.json")
		_ = os.WriteFile(mcpCfg, cfg, 0o600)
		since := time.Now()
		cmd := exec.CommandContext(runCtx(t), "claude", "-p",
			"Call the MCP tool mcp__plugrelay__everything__echo with message \"relay-mcp-7731\" and reply with exactly what it returned.",
			"--mcp-config", mcpCfg, "--strict-mcp-config", "--allowedTools", "mcp__plugrelay__everything__echo",
			"--output-format", "text")
		cmd.Dir = proj
		cmd.Env = isolatedEnv(home, "CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("claude: %v\n%s", err, tail(out))
		}
		t.Logf("claude said: %s", strings.TrimSpace(tail(out)))
		checkMCPLog(t, r, "everything__echo", since)
		if !strings.Contains(string(out), "relay-mcp-7731") {
			t.Errorf("claude did not report the echo; it said:\n%s", tail(out))
		}
	})
}

// installMCPPackages installs the reference server and the client SDK into
// a temp dir and writes the client script there.
func installMCPPackages(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"e2e-mcp","private":true,"type":"module"}`), 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npm", "install", "--no-audit", "--no-fund",
		"@modelcontextprotocol/server-everything@"+envOr("PLUGRELAY_E2E_MCP_EVERYTHING", "2.2.1"),
		"@modelcontextprotocol/sdk@"+envOr("PLUGRELAY_E2E_MCP_SDK", "1.32.2"))
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("npm install failed (no registry access?): %v\n%s", err, tail(out))
	}
	_ = os.WriteFile(filepath.Join(dir, "client.mjs"), []byte(mcpClientJS), 0o600)
	return dir
}

// startEverything runs the reference server's Streamable HTTP transport on
// a free port and returns its MCP URL.
func startEverything(t *testing.T, dir string) string {
	t.Helper()
	addr := freeAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "node", filepath.Join(dir, "node_modules/@modelcontextprotocol/server-everything/dist/index.js"), "streamableHttp")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PORT="+port)
	var logs strings.Builder
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	u := "http://127.0.0.1:" + port + "/mcp"
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if c, err := net.Dial("tcp", "127.0.0.1:"+port); err == nil {
			c.Close()
			return u
		}
		if time.Now().After(deadline) {
			t.Fatalf("reference server did not start:\n%s", logs.String())
		}
	}
}

// checkMCPLog asserts a tools/call of tool was logged as protocol mcp with
// status 200 since the given time.
func checkMCPLog(t *testing.T, r *relay, tool string, since time.Time) {
	t.Helper()
	var page struct {
		Items []struct {
			Time     time.Time `json:"time"`
			Protocol string    `json:"protocol"`
			Model    string    `json:"model"`
			Status   int       `json:"status"`
			Upstream struct {
				ID string `json:"id"`
			} `json:"upstream"`
		} `json:"items"`
	}
	deadline := time.Now().Add(10 * time.Second) // the request log is written off the request path
	for {
		r.admin(t, "GET", "/api/v1/logs?limit=50", nil, &page)
		for _, it := range page.Items {
			if !it.Time.Before(since.Truncate(time.Millisecond)) && it.Protocol == "mcp" && it.Model == tool {
				if it.Status != http.StatusOK {
					t.Fatalf("logged %s with status %d", tool, it.Status)
				}
				t.Logf("request log: protocol %s, model %s, upstream %s, status %d", it.Protocol, it.Model, it.Upstream.ID, it.Status)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no mcp log record for %s since %s: %+v", tool, since.Format(time.RFC3339), page.Items)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
