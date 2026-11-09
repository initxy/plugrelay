// Package cli implements the plugrelay command line: a small subcommand
// dispatcher over the standard flag package, and the commands that set up
// a relay (init, doctor), manage its business data (key, upstream, model,
// apply, export), validate manifests and write client configs (setup).
//
// Every command reads and writes through Env, so tests run against temp
// directories and buffers; nothing touches the real home directory unless a
// caller passes it in.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Exit codes.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// Env is everything a command may touch outside its arguments.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// LookupEnv reads environment variables.
	LookupEnv func(string) (string, bool)
	// Now stamps backups and computes expiry times.
	Now func() time.Time
	// Interactive reports whether Stdin is a terminal a person types into.
	Interactive func() bool
	// HomeDir is the default --home of setup.
	HomeDir func() (string, error)
}

// DefaultEnv is the process environment.
func DefaultEnv() *Env {
	return &Env{
		Stdin:       os.Stdin,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		LookupEnv:   os.LookupEnv,
		Now:         time.Now,
		Interactive: stdinIsTerminal,
		HomeDir:     os.UserHomeDir,
	}
}

// stdinIsTerminal approximates isatty without x/term: a character device
// other than /dev/null.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err != nil || !os.SameFile(fi, null)
}

func (e *Env) getenv(name string) string {
	if e.LookupEnv == nil {
		return ""
	}
	v, _ := e.LookupEnv(name)
	return v
}

// usageError is a bad invocation; it exits with ExitUsage.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// errSilent signals failure whose details the command already printed.
var errSilent = errors.New("failed")

// command is one node of the command tree: a group with subs, or a leaf
// with run.
type command struct {
	name    string
	summary string
	// usage is the synopsis after "plugrelay ".
	usage   string
	example string
	subs    []*command
	run     func(r *runner, cmd *command, args []string) error
}

// runner carries the global flags and the environment through a run.
type runner struct {
	env        *Env
	configPath string
	dataDir    string
	path       []string // command names from the root, for help text
	c          context.Context
}

// Run executes the command line args (without the program name) and returns
// the process exit code.
func Run(ctx context.Context, args []string, env *Env) int {
	r := &runner{env: env}
	r.configPath = env.getenv("PLUGRELAY_CONFIG")
	r.dataDir = env.getenv("PLUGRELAY_DATA_DIR")
	if r.configPath == "" {
		r.configPath = "relay.yaml"
	}
	root := rootCommand()
	fs := flag.NewFlagSet("plugrelay", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	r.globalFlags(fs)
	help := fs.Bool("help", false, "")
	fs.BoolVar(help, "h", false, "")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(env.Stderr, "plugrelay: %v\n\n", err)
		r.printGroupHelp(env.Stderr, root)
		return ExitUsage
	}
	rest := fs.Args()
	if *help || len(rest) == 0 {
		w := env.Stdout
		if len(rest) == 0 && !*help {
			w = env.Stderr
		}
		r.printGroupHelp(w, root)
		if *help {
			return ExitOK
		}
		return ExitUsage
	}
	return r.exit(r.dispatch(ctx, root, rest))
}

func (r *runner) exit(err error) int {
	var ue *usageError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return ExitOK
	case errors.As(err, &ue):
		fmt.Fprintf(r.env.Stderr, "plugrelay: %v\nRun 'plugrelay %s --help' for usage.\n", ue.msg, strings.Join(r.path, " "))
		return ExitUsage
	case errors.Is(err, errSilent):
		return ExitError
	default:
		fmt.Fprintf(r.env.Stderr, "plugrelay: %v\n", err)
		return ExitError
	}
}

func (r *runner) dispatch(ctx context.Context, cmd *command, args []string) error {
	if cmd.name != "plugrelay" {
		r.path = append(r.path, cmd.name)
	}
	if cmd.run != nil {
		r.c = ctx
		return cmd.run(r, cmd, args)
	}
	if len(args) == 0 {
		r.printGroupHelp(r.env.Stderr, cmd)
		return usagef("%s: missing subcommand", r.title())
	}
	name := args[0]
	if name == "-h" || name == "--help" || name == "help" {
		if cmd.name == "plugrelay" && len(args) > 1 {
			// "plugrelay help key create" → "plugrelay key create --help"
			return r.dispatch(ctx, cmd, append(args[1:], "--help"))
		}
		r.printGroupHelp(r.env.Stdout, cmd)
		return nil
	}
	for _, s := range cmd.subs {
		if s.name == name {
			return r.dispatch(ctx, s, args[1:])
		}
	}
	return usagef("%s: unknown command %q", r.title(), name)
}

func (r *runner) title() string {
	if len(r.path) == 0 {
		return "plugrelay"
	}
	return strings.Join(r.path, " ")
}

func (r *runner) ctx() context.Context {
	if r.c == nil {
		return context.Background()
	}
	return r.c
}

func (r *runner) globalFlags(fs *flag.FlagSet) {
	fs.StringVar(&r.configPath, "config", r.configPath, "path to relay.yaml (env PLUGRELAY_CONFIG)")
	fs.StringVar(&r.dataDir, "data-dir", r.dataDir, "directory relative store and master key paths resolve against (env PLUGRELAY_DATA_DIR; default: the config file's directory)")
}

// flags returns a FlagSet for a leaf command with the global flags and a
// help text built from cmd.
func (r *runner) flags(cmd *command) *flag.FlagSet {
	fs := flag.NewFlagSet(cmd.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	r.globalFlags(fs)
	fs.Usage = func() { r.printLeafHelp(r.env.Stdout, cmd, fs) }
	return fs
}

// parse parses args into fs, interleaving flags and positionals, and
// turns flag errors into usage errors.
func (r *runner) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fs.Usage()
				return nil, flag.ErrHelp
			}
			return nil, usagef("%v", err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func (r *runner) printGroupHelp(w io.Writer, cmd *command) {
	if cmd.usage != "" {
		fmt.Fprintf(w, "Usage: plugrelay %s\n\n", cmd.usage)
	}
	if cmd.summary != "" {
		fmt.Fprintf(w, "%s\n\n", cmd.summary)
	}
	fmt.Fprintln(w, "Commands:")
	subs := append([]*command(nil), cmd.subs...)
	if cmd.name != "plugrelay" {
		sort.SliceStable(subs, func(i, j int) bool { return subs[i].name < subs[j].name })
	}
	width := 0
	for _, s := range subs {
		width = max(width, len(s.name))
	}
	for _, s := range subs {
		short, _, _ := strings.Cut(s.summary, "\n")
		fmt.Fprintf(w, "  %-*s  %s\n", width, s.name, short)
	}
	if cmd.name == "plugrelay" {
		fmt.Fprint(w, "\nGlobal flags:\n  --config FILE    path to relay.yaml (default ./relay.yaml or $PLUGRELAY_CONFIG)\n  --data-dir DIR   directory relative store and master key paths resolve against ($PLUGRELAY_DATA_DIR)\n\nRun 'plugrelay <command> --help' for details.\n")
	}
	if cmd.example != "" {
		fmt.Fprintf(w, "\nExample:\n%s\n", indent(cmd.example))
	}
}

func (r *runner) printLeafHelp(w io.Writer, cmd *command, fs *flag.FlagSet) {
	fmt.Fprintf(w, "Usage: plugrelay %s\n\n%s\n\nFlags:\n", cmd.usage, cmd.summary)
	fs.SetOutput(w)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
	if cmd.example != "" {
		fmt.Fprintf(w, "\nExample:\n%s\n", indent(cmd.example))
	}
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

func rootCommand() *command {
	return &command{
		name:    "plugrelay",
		usage:   "[--config FILE] [--data-dir DIR] <command> [flags]",
		summary: "plugrelay is an LLM relay: one address for Claude Code, Codex, Gemini CLI and other clients.",
		subs: []*command{
			initCommand(),
			serveCommand(),
			demoCommand(),
			doctorCommand(),
			keyCommand(),
			userCommand(),
			upstreamCommand(),
			modelCommand(),
			presetCommand(),
			applyCommand(),
			exportCommand(),
			setupCommand(),
			manifestCommand(),
			versionCommand(),
		},
	}
}

// absFrom resolves p against base unless it is already absolute.
func absFrom(base, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}
