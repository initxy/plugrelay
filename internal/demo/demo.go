// Package demo runs "plugrelay demo": a real relay on a throwaway SQLite
// store, seeded with realistic data through the admin API and the store
// (never raw SQL), with every upstream pointed at an in-process fake
// upstream so requests made with the demo keys succeed. It exists for
// console development and screenshots.
package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/initxy/plugrelay/internal/app"
	"github.com/initxy/plugrelay/internal/cli"
	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
)

// allowPrivate is the upstream config flag that lets the demo upstreams
// reach the fake upstream on loopback.
const allowPrivate = "allow_private"

// stateFile keeps, in the demo directory, what a later run with the same
// --dir needs: ids for the live state and the demo key's plaintext.
const stateFile = "demo.json"

type saved struct {
	World struct {
		Groups    map[string]string `json:"groups"`
		Users     map[string]string `json:"users"`
		Upstreams map[string]string `json:"upstreams"`
		Creds     map[string]string `json:"creds"`
	} `json:"world"`
	Key string `json:"key"`
}

// Run starts the demo relay and blocks until ctx is cancelled.
func Run(ctx context.Context, o cli.DemoOptions) error {
	out := o.Out
	if out == nil {
		out = io.Discard
	}
	dir := o.Dir
	if dir == "" {
		d, err := os.MkdirTemp("", "plugrelay-demo-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(d)
		dir = d
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fake := &http.Server{Handler: fakeUpstream(), ReadHeaderTimeout: 10 * time.Second}
	fl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	go func() { _ = fake.Serve(fl) }()
	defer fake.Close()
	fakeURL := "http://" + fl.Addr().String()

	cfg, err := demoConfig(dir, o.Listen)
	if err != nil {
		return err
	}
	a, err := app.New(ctx, cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Demo store: %s\n", dir)
	s, err := Seed(ctx, a, dir, fakeURL, o.Days)
	if err != nil {
		return fmt.Errorf("demo seed: %w", err)
	}
	go keepBreakerOpen(ctx, a, s)
	host := cfg.Listen
	if h, p, err := net.SplitHostPort(host); err == nil && (h == "" || h == "0.0.0.0" || h == "::") {
		host = net.JoinHostPort("127.0.0.1", p)
	}
	// Top-ups made in the demo come back to this relay, and its fake
	// Stripe confirms them here.
	relay := "http://" + host
	demoRelay.Store(&relay)
	if err := pointPaymentsAt(a, fakeURL, relay); err != nil {
		return fmt.Errorf("demo payments: %w", err)
	}
	fmt.Fprintf(out, `
plugrelay demo is running at http://%s  (Ctrl-C stops it%s)

  Console login   %s / %s   (members: alice@, bob@, carol@, dave@demo.local, same password)
  Demo key        %s

  curl http://%s/v1/messages -H 'x-api-key: %s' -H 'content-type: application/json' \
    -d '{"model":"claude-sonnet-4-6","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'

`, host, removedNote(o.Dir), userSpecs[0].email, password, s.Key, host, s.Key)
	return a.Serve(ctx)
}

func removedNote(dir string) string {
	if dir == "" {
		return " and deletes its data"
	}
	return ""
}

func demoConfig(dir, listen string) (*config.Config, error) {
	cfg := config.Defaults()
	cfg.Listen = listen
	cfg.Store.Driver = "sqlite"
	cfg.Store.DSN = filepath.Join(dir, "relay.db")
	// Price sync and notification channels reach the fake upstream on
	// loopback.
	cfg.Egress.AllowPrivateCIDRs = []string{"127.0.0.0/8"}
	cfg.MasterKeyFile = filepath.Join(dir, "master.key")
	if _, err := os.Stat(cfg.MasterKeyFile); errors.Is(err, os.ErrNotExist) {
		mk, err := secret.GenerateMasterKey()
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(cfg.MasterKeyFile, []byte(mk+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	return &cfg, nil
}

// Seed fills a fresh store in dir with the demo data, or, when dir holds
// an earlier run, points its upstreams at the new fake upstream and puts
// the live state back (it lives in memory). It returns what the run
// should print.
func Seed(ctx context.Context, a *app.App, dir, fakeURL string, days int) (*saved, error) {
	srv := httptest.NewServer(a.Handler().Handler())
	defer srv.Close()
	c := newAPI(srv.URL)
	path := filepath.Join(dir, stateFile)
	var s saved
	w := &world{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		w.groups, w.users, w.upstreams, w.creds = s.World.Groups, s.World.Users, s.World.Upstreams, s.World.Creds
		if err := c.login(userSpecs[0].email, password); err != nil {
			return nil, err
		}
		if err := pointUpstreams(c, w.upstreams, fakeURL); err != nil {
			return nil, err
		}
		if err := pointChannels(c, fakeURL); err != nil {
			return nil, err
		}
		if err := pointPayments(c, fakeURL, c.base); err != nil {
			return nil, err
		}
	} else {
		now := time.Now()
		var err error
		if w, err = seedConfig(c, fakeURL, now); err != nil {
			return nil, err
		}
		if err := seedBilling(ctx, a, c, w); err != nil {
			return nil, err
		}
		if err := seedPriceSync(ctx, a, c, fakeURL); err != nil {
			return nil, err
		}
		if err := c.v1("PATCH", "/features/public_pages", map[string]any{"enabled": true}, nil); err != nil {
			return nil, err
		}
		if err := insertRecords(ctx, a.Store(), genRecords(w, days, now)); err != nil {
			return nil, err
		}
		if err := seedAudit(ctx, a.Store(), w, now); err != nil {
			return nil, err
		}
		if err := seedNotifications(ctx, a.Store(), c, w, fakeURL, now); err != nil {
			return nil, err
		}
		if err := seedOperations(ctx, a, c, w, fakeURL, now); err != nil {
			return nil, fmt.Errorf("operations: %w", err)
		}
		s.World.Groups, s.World.Users, s.World.Upstreams, s.World.Creds = w.groups, w.users, w.upstreams, w.creds
		s.Key = w.demoKey
		b, _ := json.MarshalIndent(s, "", "  ")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return nil, err
		}
	}
	if err := a.Reload(ctx); err != nil {
		return nil, err
	}
	if err := seedLiveState(ctx, a, w); err != nil {
		return nil, err
	}
	return &s, nil
}

// pointPaymentsAt points the demo's Stripe at fakeURL and its return
// address at consoleURL, as the admin.
func pointPaymentsAt(a *app.App, fakeURL, consoleURL string) error {
	srv := httptest.NewServer(a.Handler().Handler())
	defer srv.Close()
	c := newAPI(srv.URL)
	if err := c.login(userSpecs[0].email, password); err != nil {
		return err
	}
	return pointPayments(c, fakeURL, consoleURL)
}

// keepBreakerOpen re-opens Bedrock's breaker whenever its cooldown lets a
// probe through, so the console keeps showing it open.
func keepBreakerOpen(ctx context.Context, a *app.App, s *saved) {
	w := &world{upstreams: s.World.Upstreams, creds: s.World.Creds}
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = tripBreaker(ctx, a, w)
		}
	}
}
