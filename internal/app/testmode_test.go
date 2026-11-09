package app

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

const offBoxID = "sink.offbox-test"

var offBoxInits atomic.Int32

// offBoxSink stands in for a sink that ships records to a third party.
type offBoxSink struct{}

func (offBoxSink) Info() plugin.Info {
	return plugin.Info{ID: offBoxID, Kind: plugin.KindSink, Version: "0", ReportsOffBox: true}
}
func (offBoxSink) Init(context.Context, plugin.Host) error       { offBoxInits.Add(1); return nil }
func (offBoxSink) Consume(context.Context, *plugin.Record) error { return nil }

func init() { plugin.Register(offBoxID, func() plugin.Plugin { return offBoxSink{} }) }

// TestTestModeDropsOffBoxPlugins pins hard rule 22 end to end: with
// PLUGRELAY_TEST_MODE set, a plugin that reports off the box is not started
// even though its stored config enables it; other plugins are unaffected.
func TestTestModeDropsOffBoxPlugins(t *testing.T) {
	for _, tc := range []struct {
		env     string
		running bool
	}{{"", true}, {"1", false}} {
		t.Run("PLUGRELAY_TEST_MODE="+tc.env, func(t *testing.T) {
			t.Setenv(EnvTestMode, tc.env)
			offBoxInits.Store(0)
			mk, _ := secret.GenerateMasterKey()
			cfg := config.Defaults()
			cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
			cfg.MasterKey = mk
			seedPlugins(t, &cfg, on(offBoxID)...)
			a, err := New(context.Background(), &cfg, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.closeAll)

			var sinks []string
			for _, s := range a.runtime().sinks {
				sinks = append(sinks, s.Info().ID)
			}
			if got := slices.Contains(sinks, offBoxID); got != tc.running {
				t.Errorf("off-box sink in the running sinks = %v, want %v (sinks %v)", got, tc.running, sinks)
			}
			if !slices.Contains(sinks, "sink.metrics") {
				t.Errorf("sink.metrics not running: %v", sinks)
			}
			if inited := offBoxInits.Load() > 0; inited != tc.running {
				t.Errorf("off-box sink initialized = %v, want %v", inited, tc.running)
			}
			want := "stopped"
			if tc.running {
				want = "running"
			}
			for _, e := range a.entries() {
				if e.Info.ID == offBoxID && e.Status != want {
					t.Errorf("admin status = %q, want %q", e.Status, want)
				}
			}
		})
	}
}

// unregisteredOffBox stands in for an external plugin: it is known to the
// app but not in the plugin registry.
type unregisteredOffBox struct{}

func (unregisteredOffBox) Info() plugin.Info {
	return plugin.Info{ID: "ext.offbox", Kind: plugin.KindSink, Version: "0", ReportsOffBox: true}
}
func (unregisteredOffBox) Init(context.Context, plugin.Host) error { return nil }

// TestTestModeDropsOffBoxExtensions: an enabled extension that reports off
// the box is turned off in test mode, like a built-in one.
func TestTestModeDropsOffBoxExtensions(t *testing.T) {
	for _, testMode := range []bool{false, true} {
		a := &App{testMode: testMode}
		known := []plugin.Plugin{unregisteredOffBox{}}
		stored := []store.PluginConfig{{PluginID: "ext.offbox", Enabled: true}}
		out, err := a.states(known, stored, nil, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		if got := out["ext.offbox"].enabled; got == testMode {
			t.Errorf("test mode %v: extension enabled = %v", testMode, got)
		}
	}
}
