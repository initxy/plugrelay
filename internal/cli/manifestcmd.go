package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/initxy/plugrelay/manifests"
	"github.com/initxy/plugrelay/plugins/provider/manifest"
)

func manifestCommand() *command {
	return &command{
		name:    "manifest",
		summary: "Validate upstream manifests and list the official ones.",
		usage:   "manifest <validate|list> [flags]",
		subs: []*command{
			{
				name: "validate",
				summary: `Validate manifest files, or every *.yaml / *.yml in a directory tree.
Problems are printed as file: field.path: message. Pricing files that a
manifest in the same tree references are checked as part of it.`,
				usage:   "manifest validate FILE|DIR...",
				example: "plugrelay manifest validate my-upstream.yaml\nplugrelay manifest validate ./manifests",
				run:     runManifestValidate,
			},
			{
				name:    "list",
				summary: "List the manifests shipped with plugrelay.",
				usage:   "manifest list [--json]",
				example: "plugrelay manifest list",
				run:     runManifestList,
			},
		},
	}
}

// manifestProblem is one validation failure.
type manifestProblem struct {
	File  string `json:"file"`
	Field string `json:"field,omitempty"`
	Msg   string `json:"message"`
}

func (p manifestProblem) String() string {
	if p.Field == "" {
		return p.File + ": " + p.Msg
	}
	return p.File + ": " + p.Field + ": " + p.Msg
}

func runManifestValidate(r *runner, cmd *command, args []string) error {
	fset := r.flags(cmd)
	pos, err := r.parse(fset, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef("give at least one file or directory")
	}
	var problems []manifestProblem
	checked := 0
	for _, arg := range pos {
		ps, n, err := validatePath(arg)
		if err != nil {
			return err
		}
		problems = append(problems, ps...)
		checked += n
	}
	for _, p := range problems {
		fmt.Fprintln(r.env.Stdout, p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(r.env.Stderr, "%d problem(s) in %d manifest(s)\n", len(problems), checked)
		return errSilent
	}
	fmt.Fprintf(r.env.Stdout, "%d manifest(s) valid\n", checked)
	return nil
}

// validatePath validates a file, or a directory tree, and returns the
// problems and the number of manifests checked.
func validatePath(p string) ([]manifestProblem, int, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return nil, 0, err
	}
	if !fi.IsDir() {
		return validateFile(os.DirFS(filepath.Dir(p)), filepath.Base(p), p), 1, nil
	}
	fsys := os.DirFS(p)
	var names []string
	err = fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")) {
			names = append(names, name)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	// Pricing files are not manifests; skip those a manifest points at.
	pricing := map[string]bool{}
	for _, name := range names {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, 0, err
		}
		if m, err := manifest.Decode(data); err == nil && m.Pricing != nil && m.Pricing.File != "" {
			pricing[path.Join(path.Dir(name), m.Pricing.File)] = true
		}
	}
	var problems []manifestProblem
	n := 0
	for _, name := range names {
		if pricing[name] {
			continue
		}
		n++
		problems = append(problems, validateFile(fsys, name, filepath.Join(p, filepath.FromSlash(name)))...)
	}
	return problems, n, nil
}

func validateFile(fsys fs.FS, name, display string) []manifestProblem {
	_, err := manifest.Load(fsys, name)
	if err == nil {
		return nil
	}
	var fe manifest.Errors
	if errors.As(err, &fe) {
		out := make([]manifestProblem, 0, len(fe))
		for _, e := range fe {
			out = append(out, manifestProblem{File: display, Field: e.Path, Msg: e.Msg})
		}
		return out
	}
	// Decode errors carry the file name already; drop it to avoid
	// repeating it after ours.
	msg := strings.TrimPrefix(err.Error(), name+": ")
	return []manifestProblem{{File: display, Msg: msg}}
}

type manifestInfo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Provider  string   `json:"provider"`
	Protocols []string `json:"protocols"`
	Variables []string `json:"variables,omitempty"`
	Homepage  string   `json:"homepage,omitempty"`
}

func officialManifests() ([]manifestInfo, error) {
	names, err := fs.Glob(manifests.FS, "*.yaml")
	if err != nil {
		return nil, err
	}
	out := make([]manifestInfo, 0, len(names))
	for _, name := range names {
		m, err := manifest.Load(manifests.FS, name)
		if err != nil {
			return nil, fmt.Errorf("official manifest %w", err)
		}
		info := manifestInfo{ID: m.ID, Name: m.Name, Provider: m.ProviderID(), Homepage: m.Homepage}
		for _, e := range m.Endpoints {
			if !slices.Contains(info.Protocols, e.Protocol) {
				info.Protocols = append(info.Protocols, e.Protocol)
			}
		}
		for v := range m.Variables {
			info.Variables = append(info.Variables, v)
		}
		sort.Strings(info.Variables)
		out = append(out, info)
	}
	return out, nil
}

func runManifestList(r *runner, cmd *command, args []string) error {
	fset := r.flags(cmd)
	asJSON := fset.Bool("json", false, "print JSON")
	if _, err := r.parse(fset, args); err != nil {
		return err
	}
	list, err := officialManifests()
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(r.env.Stdout, list)
	}
	rows := make([][]string, 0, len(list))
	for _, m := range list {
		rows = append(rows, []string{m.ID, m.Name, strings.Join(m.Protocols, ","), orDash(strings.Join(m.Variables, ","))})
	}
	return table(r.env.Stdout, []string{"ID", "NAME", "PROTOCOLS", "VARIABLES"}, rows)
}
