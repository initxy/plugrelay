package app

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/config"
)

// listen opens the configured address with two safety rules: until an
// admin account exists the relay binds to 127.0.0.1 whatever relay.yaml
// says, and the setup wizard's "listen publicly" choice widens a loopback
// address to every interface.
func (a *App) listen(ctx context.Context) (net.Listener, error) {
	host, port, err := net.SplitHostPort(a.cfg.Listen)
	if err != nil {
		return nil, err
	}
	needs, err := a.admin.NeedsSetup(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case needs:
		if !isLoopbackHost(host) {
			a.log.Warn("no admin account yet: listening on loopback only until setup is finished", "configured", a.cfg.Listen)
		}
		host = "127.0.0.1"
	case isLoopbackHost(host):
		if site, err := admin.LoadSite(ctx, a.store); err == nil {
			if public, set := site.Bool(admin.SettingListenPublic); set && public {
				host = "0.0.0.0"
			}
		}
	}
	addr := net.JoinHostPort(host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if needs {
		a.log.Warn("first run: open the setup wizard to create the admin account",
			"url", "http://"+net.JoinHostPort("127.0.0.1", portOf(ln.Addr()))+"/setup")
	}
	return ln, nil
}

func portOf(addr net.Addr) string {
	_, p, _ := net.SplitHostPort(addr.String())
	return p
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func isLoopback(addr net.Addr) bool {
	if t, ok := addr.(*net.TCPAddr); ok {
		return t.IP.IsLoopback()
	}
	host, _, err := net.SplitHostPort(addr.String())
	return err == nil && isLoopbackHost(host)
}

func newLogger(c config.Log) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: lvl}
	if c.Format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
