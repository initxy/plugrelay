package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// DemoOptions configure "plugrelay demo".
type DemoOptions struct {
	Listen string
	// Dir holds the demo store; empty means a temporary directory removed
	// on exit.
	Dir  string
	Days int
	Out  io.Writer
}

// Demo runs a seeded demo relay until ctx is cancelled. The main package
// sets it; it is a variable so this package does not import the server.
var Demo func(ctx context.Context, o DemoOptions) error

func demoCommand() *command {
	return &command{
		name: "demo",
		summary: "Run a throwaway relay seeded with realistic data and a fake upstream, for console development and screenshots.\n" +
			"Never point it at real data: it creates its own SQLite store.",
		usage:   "demo [--listen ADDR] [--dir DIR] [--days N]",
		example: "plugrelay demo\nplugrelay demo --dir /tmp/prdemo --days 30",
		run:     runDemo,
	}
}

func runDemo(r *runner, cmd *command, args []string) error {
	fs := r.flags(cmd)
	listen := fs.String("listen", "127.0.0.1:8787", "address to listen on")
	dir := fs.String("dir", "", "directory for the demo store, kept between runs (default: a temporary directory removed on exit)")
	days := fs.Int("days", 14, "days of request history to generate (1-90)")
	pos, err := r.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("demo takes no arguments")
	}
	if *days < 1 || *days > 90 {
		return usagef("--days must be between 1 and 90")
	}
	if Demo == nil {
		return errors.New("demo not wired into this binary")
	}
	ctx, stop := signal.NotifyContext(r.ctx(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	return Demo(ctx, DemoOptions{Listen: *listen, Dir: *dir, Days: *days, Out: r.env.Stdout})
}
