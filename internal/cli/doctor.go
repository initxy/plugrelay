package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"

	"github.com/go-sql-driver/mysql"

	"github.com/initxy/plugrelay/internal/config"
	_ "github.com/initxy/plugrelay/internal/store" // registers the sqlite, pgx and mysql drivers
)

func doctorCommand() *command {
	return &command{
		name: "doctor",
		summary: `Check the config, master key, database and listen address.

Read-only: it never migrates or writes. Exits 1 when any check fails.`,
		usage:   "doctor [--json]",
		example: "plugrelay doctor --config /etc/plugrelay/relay.yaml",
		run:     runDoctor,
	}
}

type checkStatus string

const (
	statusOK   checkStatus = "ok"
	statusWarn checkStatus = "warn"
	statusFail checkStatus = "fail"
)

type checkResult struct {
	Name   string      `json:"name"`
	Status checkStatus `json:"status"`
	Detail string      `json:"detail"`
}

func runDoctor(r *runner, cmd *command, args []string) error {
	fset := r.flags(cmd)
	asJSON := fset.Bool("json", false, "print JSON")
	if _, err := r.parse(fset, args); err != nil {
		return err
	}
	results := r.doctor()
	failed := false
	for _, c := range results {
		failed = failed || c.Status == statusFail
	}
	if *asJSON {
		if err := printJSON(r.env.Stdout, results); err != nil {
			return err
		}
	} else {
		for _, c := range results {
			fmt.Fprintf(r.env.Stdout, "[%-4s] %-11s %s\n", c.Status, c.Name, c.Detail)
		}
	}
	if failed {
		return errSilent
	}
	return nil
}

func (r *runner) doctor() []checkResult {
	var out []checkResult
	add := func(name string, s checkStatus, format string, args ...any) {
		out = append(out, checkResult{Name: name, Status: s, Detail: fmt.Sprintf(format, args...)})
	}

	if _, err := os.Stat(r.configPath); errors.Is(err, fs.ErrNotExist) {
		add("config", statusWarn, "%s not found; using defaults (run 'plugrelay init')", r.configPath)
	}
	cfg, err := r.loadConfig()
	if err != nil {
		add("config", statusFail, "%v", err)
		return out
	}
	if len(out) == 0 {
		add("config", statusOK, "%s", r.configPath)
	}

	switch {
	case cfg.MasterKey == "" && cfg.MasterKeyFile == "":
		add("master key", statusFail, "none configured; credentials cannot be stored (set master_key_file)")
	default:
		if _, err := cfg.LoadMasterKey(); err != nil {
			add("master key", statusFail, "%v", err)
		} else if cfg.MasterKeyFile != "" {
			if fi, err := os.Stat(cfg.MasterKeyFile); err == nil && fi.Mode().Perm()&0o077 != 0 {
				add("master key", statusWarn, "%s is readable by others (mode %o); chmod 600 it", cfg.MasterKeyFile, fi.Mode().Perm())
			} else {
				add("master key", statusOK, "%s", cfg.MasterKeyFile)
			}
		} else {
			add("master key", statusOK, "inline in config or environment")
		}
	}

	r.checkDatabase(cfg, add)

	switch {
	case isLoopback(cfg.Listen):
		add("listen", statusOK, "%s (this machine only)", cfg.Listen)
	default:
		add("listen", statusWarn, "%s is reachable from the network; serve it behind TLS", cfg.Listen)
	}

	if cfg.ConsoleEnabled() && !isLoopback(cfg.Listen) {
		add("console", statusWarn, "the admin console is reachable from the network")
	}
	if cfg.State.Driver == "memory" && !isLoopback(cfg.Listen) {
		add("state", statusWarn, "memory state is lost on restart and not shared between nodes")
	}
	return out
}

// checkDatabase reports reachability and the applied migration version
// without migrating.
func (r *runner) checkDatabase(cfg *config.Config, add func(string, checkStatus, string, ...any)) {
	driver, dsn := cfg.Store.Driver, cfg.Store.DSN
	shown := dsn
	switch driver {
	case "sqlite":
		if isPlainPath(dsn) {
			if _, err := os.Stat(dsn); err != nil {
				add("database", statusFail, "%s does not exist (run 'plugrelay init')", dsn)
				return
			}
			dsn = "file:" + dsn + "?mode=ro"
		}
	case "postgres":
		driver = "pgx"
		if u, err := url.Parse(dsn); err == nil {
			shown = u.Redacted()
		}
	case "mysql":
		if c, err := mysql.ParseDSN(dsn); err == nil {
			if c.Passwd != "" {
				c.Passwd = "xxxxx"
			}
			shown = c.FormatDSN()
		}
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		add("database", statusFail, "%v", err)
		return
	}
	defer db.Close()
	if err := db.PingContext(r.ctx()); err != nil {
		add("database", statusFail, "%s: %v", shown, err)
		return
	}
	var version sql.NullInt64
	var count int
	err = db.QueryRowContext(r.ctx(), `SELECT MAX(version), COUNT(*) FROM schema_migrations`).Scan(&version, &count)
	switch {
	case err != nil && strings.Contains(err.Error(), "schema_migrations"):
		add("database", statusWarn, "%s reachable but not migrated; 'plugrelay init' or 'serve' migrates it", shown)
	case err != nil:
		add("database", statusFail, "%s: %v", shown, err)
	default:
		add("database", statusOK, "%s (%s, schema version %d, %d migrations)", shown, cfg.Store.Driver, version.Int64, count)
	}
}
