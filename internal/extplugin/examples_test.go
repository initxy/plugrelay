package extplugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/plugin/plugintest"
)

const examplesDir = "../../examples/plugins"

func startExample(t *testing.T, dir string, mutate func(*Manifest)) (*Manager, *Manifest) {
	t.Helper()
	man, err := LoadManifest(filepath.Join(examplesDir, dir))
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(man)
	}
	m := NewManager(Options{HostVersion: "test", Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	if err := m.Add(man); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	return m, man
}

func middlewareOf(t *testing.T, m *Manager, id string) *Middleware {
	t.Helper()
	for _, p := range m.Plugins(id) {
		if mw, ok := p.(*Middleware); ok {
			return mw
		}
	}
	t.Fatal("no middleware adapter")
	return nil
}

// buildExample compiles the Go example in dir and returns the binary.
func buildExample(t *testing.T, dir string) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not in PATH")
	}
	bin := filepath.Join(t.TempDir(), dir)
	build := exec.Command(goBin, "build", "-o", bin, ".")
	build.Dir = filepath.Join(examplesDir, dir)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

func TestExampleEchoProvider(t *testing.T) {
	bin := buildExample(t, "echo-provider-go")
	m, _ := startExample(t, "echo-provider-go", func(man *Manifest) { man.Command = bin })
	p := m.Plugins("echo")[0].(*Provider)

	t.Run("conformance", func(t *testing.T) {
		plugintest.RunProvider(t, plugintest.ProviderSuite{
			Protocol:       "openai-chat",
			FirstByteDelay: 100 * time.Millisecond,
			New: func(baseURL string) (plugin.Provider, *plugin.Upstream, *plugin.Credential) {
				u := &plugin.Upstream{ID: "up", Provider: "provider.echo", Endpoints: []plugin.Endpoint{{ID: "e", Protocol: "openai-chat", BaseURL: baseURL}}}
				return p, u, &plugin.Credential{ID: "c", Secret: "sk-echo"}
			},
		})
	})

	t.Run("send", func(t *testing.T) {
		req, _ := http.NewRequest("POST", "http://echo.invalid/v1/chat/completions",
			strings.NewReader(`{"model":"echo","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"x"},{"role":"user","content":"héllo wörld"}]}`))
		u := &plugin.Upstream{ID: "up", Config: map[string]any{"prefix": "> ", "chunk_size": 4}}
		var text strings.Builder
		var last ir.EventKind
		out, err := p.Send(context.Background(), &plugin.Outgoing{Upstream: u, Model: "echo", Req: req}, nil, func(ev *ir.Event) error {
			if ev.Kind == ir.EvDelta {
				text.WriteString(ev.Delta.Text)
			}
			last = ev.Kind
			return nil
		})
		if err != nil || out != nil {
			t.Fatalf("send: %v %+v", err, out)
		}
		if text.String() != "> héllo wörld" || last != ir.EvDone {
			t.Fatalf("echo %q, last %s", text.String(), last)
		}
	})
}

func TestExampleIPAllowlist(t *testing.T) {
	bin := buildExample(t, "ip-allowlist-go")
	m, _ := startExample(t, "ip-allowlist-go", func(man *Manifest) { man.Command = bin })
	mw := middlewareOf(t, m, "ip-allowlist")
	allow := map[string]json.RawMessage{"allow": json.RawMessage(`["10.0.0.0/8","2001:db8::/32","not-a-cidr"]`)}
	for ip, blocked := range map[string]bool{"10.1.2.3": false, "2001:db8::1": false, "192.168.1.1": true, "": true} {
		c := newCall("m", allow)
		c.ClientIP = ip
		reply, err := mw.OnRequest(c)
		if err != nil {
			t.Fatal(err)
		}
		if (reply != nil) != blocked {
			t.Errorf("%q: reply %+v, want blocked=%v", ip, reply, blocked)
		}
		if reply != nil && reply.Error.Kind != ir.ErrPermission {
			t.Errorf("%q: kind %s", ip, reply.Error.Kind)
		}
	}
	c := newCall("m", nil)
	c.ClientIP = "192.168.1.1"
	if reply, err := mw.OnRequest(c); err != nil || reply != nil {
		t.Fatalf("empty allow list must allow: %v %+v", err, reply)
	}
}
