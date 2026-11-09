package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/version"
)

// Serve runs the relay until ctx is cancelled and returns after a graceful
// shutdown. The main package sets it to app.Run; it is a variable so this
// package does not import the whole server.
var Serve func(ctx context.Context, cfg *config.Config) error

func serveCommand() *command {
	return &command{
		name:    "serve",
		summary: "Run the relay. SIGINT or SIGTERM shuts it down gracefully; a second signal exits at once.",
		usage:   "serve [--config FILE] [--data-dir DIR]",
		example: "plugrelay serve --config /etc/plugrelay/relay.yaml",
		run:     runServe,
	}
}

func runServe(r *runner, cmd *command, args []string) error {
	fs := r.flags(cmd)
	pos, err := r.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("serve takes no arguments")
	}
	if Serve == nil {
		return errors.New("serve not wired into this binary")
	}
	cfg, err := r.loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(r.ctx(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Restore default handling so a second signal kills the process.
		stop()
	}()
	return Serve(ctx, cfg)
}

func versionCommand() *command {
	return &command{
		name:    "version",
		summary: "Print the plugrelay version.",
		usage:   "version [--json]",
		example: "plugrelay version",
		run: func(r *runner, cmd *command, args []string) error {
			fs := r.flags(cmd)
			asJSON := fs.Bool("json", false, "print JSON")
			if _, err := r.parse(fs, args); err != nil {
				return err
			}
			if *asJSON {
				return printJSON(r.env.Stdout, map[string]string{"version": version.Version, "go": runtime.Version()})
			}
			fmt.Fprintf(r.env.Stdout, "plugrelay %s (%s)\n", version.Version, runtime.Version())
			return nil
		},
	}
}
