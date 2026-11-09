package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
)

// slowPluginConfig is a relay with one enabled external plugin whose
// process records its pid and never finishes the handshake, so a start
// is still in flight when the relay closes.
func slowPluginConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	pdir := filepath.Join(dir, "slow")
	if err := os.Mkdir(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(pdir, "pid")
	man := "id: slow\nversion: 1.0.0\nkind: middleware\npoints: [middleware.onRequest]\ncommand: /bin/sh\n" +
		"args: [-c, 'echo $$ > " + pidFile + "; exec sleep 60']\ntimeouts:\n  startup: 30s\n"
	if err := os.WriteFile(filepath.Join(pdir, "plugin.yaml"), []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	cfg.ExternalPlugins = config.ExternalPlugins{Enabled: true, Dir: dir}
	seedPlugins(t, &cfg, on("middleware.slow")...)
	return &cfg, pidFile
}

// Closing the relay while an external plugin is still starting waits for
// that start and stops the process: no panic, no orphan.
func TestCloseAllStopsStartingExternalPlugin(t *testing.T) {
	cfg, pidFile := slowPluginConfig(t)
	a, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0; {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if pid == 0 && time.Now().After(deadline) {
			a.closeAll()
			t.Fatal("external plugin never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() {
		a.closeAll()
		close(done)
	}()
	// A rebuild racing the close starts nothing new.
	a.syncExternal(a.runtime().states)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("closeAll did not return")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("external plugin process %d outlived the relay", pid)
	}
}

// plugrelay apply loads no external plugin, so none is started.
func TestApplyDocumentStartsNoExternalPlugin(t *testing.T) {
	cfg, pidFile := slowPluginConfig(t)
	if _, err := ApplyDocument(context.Background(), cfg, []byte("{}"), true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(pidFile); err == nil {
		t.Fatal("apply started the external plugin")
	}
}
