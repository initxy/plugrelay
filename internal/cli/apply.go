package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/store"
)

// Apply applies a document through the relay so the enabled plugins vet
// its settings first. The main package sets it to app.ApplyDocument; when
// it is nil (this package's tests) the store applies the document alone.
var Apply func(ctx context.Context, cfg *config.Config, data []byte, dryRun bool) (*store.ApplyResult, error)

func applyCommand() *command {
	return &command{
		name: "apply",
		summary: `Import business data from a YAML document.

Upserts the groups, upstreams, credentials, models, presets, prices,
features, extensions, settings and manifests it lists, in one transaction. Rows not mentioned
are left alone. The format is what 'plugrelay export' writes.`,
		usage:   "apply -f FILE [--dry-run] [--json]",
		example: "plugrelay export -o backup.yaml\nplugrelay apply -f backup.yaml --dry-run\ncat team.yaml | plugrelay apply -f -",
		run:     runApply,
	}
}

func runApply(r *runner, cmd *command, args []string) error {
	fs := r.flags(cmd)
	file := fs.String("f", "", "YAML document to apply; - reads stdin (required)")
	dry := fs.Bool("dry-run", false, "report what would change without writing")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if _, err := r.parse(fs, args); err != nil {
		return err
	}
	if *file == "" {
		return usagef("-f is required")
	}
	var data []byte
	var err error
	if *file == "-" {
		data, err = io.ReadAll(r.env.Stdin)
	} else {
		data, err = os.ReadFile(*file)
	}
	if err != nil {
		return err
	}
	res, err := r.apply(data, *dry)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(r.env.Stdout, res)
	}
	printApply(r.env.Stdout, res, *dry)
	return nil
}

func (r *runner) apply(data []byte, dry bool) (*store.ApplyResult, error) {
	if Apply != nil {
		cfg, err := r.loadConfig()
		if err != nil {
			return nil, err
		}
		return Apply(r.ctx(), cfg, data, dry)
	}
	st, _, err := r.openStore()
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return st.Apply(r.ctx(), data, dry)
}

func printApply(w io.Writer, res *store.ApplyResult, dry bool) {
	for _, c := range res.Changes {
		if c.Op == "unchanged" {
			continue
		}
		fmt.Fprintf(w, "%-9s %s %s\n", c.Op, c.Kind, c.ID)
	}
	prefix := ""
	if dry {
		prefix = "dry run, nothing written: "
	}
	fmt.Fprintf(w, "%s%d created, %d updated, %d unchanged\n", prefix, res.Created, res.Updated, res.Unchanged)
}

func exportCommand() *command {
	return &command{
		name: "export",
		summary: `Write business data as YAML, for backup or 'plugrelay apply'.

Users, keys and logs are not included. Credential secrets are left out
unless --with-secrets is given; such a file is as sensitive as the keys.`,
		usage:   "export [-o FILE] [--with-secrets]",
		example: "plugrelay export -o relay-data.yaml",
		run:     runExport,
	}
}

func runExport(r *runner, cmd *command, args []string) error {
	fs := r.flags(cmd)
	out := fs.String("o", "", "output file (default: stdout)")
	withSecrets := fs.Bool("with-secrets", false, "include decrypted credential secrets")
	if _, err := r.parse(fs, args); err != nil {
		return err
	}
	st, _, err := r.openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	data, err := st.Export(r.ctx(), store.ExportOptions{Secrets: *withSecrets})
	if err != nil {
		return err
	}
	if *out == "" || *out == "-" {
		_, err = r.env.Stdout.Write(data)
		return err
	}
	perm := os.FileMode(0o644)
	if *withSecrets {
		perm = 0o600
	}
	if err := writeFileAtomic(*out, data, perm); err != nil {
		return err
	}
	fmt.Fprintf(r.env.Stderr, "Wrote %s\n", *out)
	return nil
}
