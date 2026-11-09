package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
)

// baseDir is where relative paths in relay.yaml resolve: --data-dir, or
// the directory of the config file.
func (r *runner) baseDir() (string, error) {
	d := r.dataDir
	if d == "" {
		d = filepath.Dir(r.configPath)
	}
	return filepath.Abs(d)
}

// loadConfig reads relay.yaml with environment overrides and resolves its
// relative file paths against baseDir, so the result works from any
// working directory.
func (r *runner) loadConfig() (*config.Config, error) {
	cfg, err := config.Load(r.configPath, r.env.LookupEnv)
	if err != nil {
		return nil, err
	}
	base, err := r.baseDir()
	if err != nil {
		return nil, err
	}
	resolvePaths(cfg, base)
	return cfg, nil
}

func resolvePaths(cfg *config.Config, base string) {
	cfg.MasterKeyFile = absFrom(base, cfg.MasterKeyFile)
	if cfg.Store.Driver == "sqlite" && isPlainPath(cfg.Store.DSN) {
		cfg.Store.DSN = absFrom(base, cfg.Store.DSN)
	}
	if cfg.ExternalPlugins.Dir != "" {
		cfg.ExternalPlugins.Dir = absFrom(base, cfg.ExternalPlugins.Dir)
	}
}

// isPlainPath reports whether a sqlite DSN is a bare file path rather than
// a "file:" URI or an in-memory database.
func isPlainPath(dsn string) bool {
	return dsn != "" && !strings.HasPrefix(dsn, "file:") && !strings.HasPrefix(dsn, ":memory:")
}

// openStore loads the config and opens the store, applying migrations.
// The master key is loaded when one is configured; without one, writes that
// carry secrets fail with store.ErrNoBox.
func (r *runner) openStore() (store.Store, *config.Config, error) {
	cfg, err := r.loadConfig()
	if err != nil {
		return nil, nil, err
	}
	var box *secret.Box
	if cfg.MasterKey != "" || cfg.MasterKeyFile != "" {
		mk, err := cfg.LoadMasterKey()
		if err != nil {
			return nil, nil, err
		}
		if box, err = secret.NewBox(mk); err != nil {
			return nil, nil, err
		}
	}
	st, err := store.Open(r.ctx(), store.Config{Driver: cfg.Store.Driver, DSN: cfg.Store.DSN, MaxConns: int(cfg.Store.MaxConns), Secrets: box})
	if err != nil {
		return nil, nil, err
	}
	return st, cfg, nil
}

// isLoopback reports whether a listen address only accepts local
// connections. An empty host listens on every interface.
func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	return err == nil && isLoopbackHost(host)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table writes tab-separated rows aligned in columns.
func table(w io.Writer, header []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
