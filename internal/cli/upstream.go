package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/manifests"
	"github.com/initxy/plugrelay/plugins/provider/manifest"
)

func upstreamCommand() *command {
	return &command{
		name:    "upstream",
		summary: "Manage upstreams: provider instances built from a manifest, with their credentials.",
		usage:   "upstream <add|list|rm> [flags]",
		subs: []*command{
			{
				name: "add",
				summary: `Add or replace an upstream from an official or stored manifest.

The API key is read from the environment variable named by
--credential-env and stored encrypted under the master key; it never
appears on the command line. Omit it for a self-hosted server that needs
no key (ollama): it is then sent requests without auth. Any other upstream
without a credential is not routed until one is added.`,
				usage:   "upstream add --manifest ID [--id ID] [--credential-env VAR] [--var k=v]... [--disabled]",
				example: "export DEEPSEEK_API_KEY=sk-...\nplugrelay upstream add --manifest deepseek --credential-env DEEPSEEK_API_KEY\nplugrelay upstream add --manifest azure-openai --id azure-eu --credential-env AZURE_KEY --var resource=acme-eu",
				run:     runUpstreamAdd,
			},
			{
				name:    "list",
				summary: "List upstreams and how many credentials each has.",
				usage:   "upstream list [--json]",
				example: "plugrelay upstream list",
				run:     runUpstreamList,
			},
			{
				name:    "rm",
				summary: "Delete an upstream with its credentials and routes.",
				usage:   "upstream rm --id ID",
				example: "plugrelay upstream rm --id deepseek",
				run:     runUpstreamRm,
			},
		},
	}
}

// officialManifest loads a manifest shipped in the binary.
func officialManifest(id string) (*manifest.Manifest, bool, error) {
	name := id + ".yaml"
	if _, err := fs.Stat(manifests.FS, name); err != nil {
		return nil, false, nil
	}
	m, err := manifest.Load(manifests.FS, name)
	return m, true, err
}

func runUpstreamAdd(r *runner, cmd *command, args []string) error {
	fset := r.flags(cmd)
	manifestID := fset.String("manifest", "", "manifest id (see 'plugrelay manifest list') (required)")
	id := fset.String("id", "", "upstream id (default: the manifest id)")
	credEnv := fset.String("credential-env", "", "environment variable holding the API key")
	var vars repeatFlag
	fset.Var(&vars, "var", "manifest variable as key=value; repeatable")
	disabled := fset.Bool("disabled", false, "add the upstream switched off")
	if _, err := r.parse(fset, args); err != nil {
		return err
	}
	if *manifestID == "" {
		return usagef("--manifest is required")
	}
	if *id == "" {
		*id = *manifestID
	}
	values, err := parseVars(vars)
	if err != nil {
		return usagef("%v", err)
	}
	var secretValue string
	if *credEnv != "" {
		secretValue = strings.TrimSpace(r.env.getenv(*credEnv))
		if secretValue == "" {
			return fmt.Errorf("environment variable %s is empty or unset", *credEnv)
		}
	}

	st, _, err := r.openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	m, found, err := officialManifest(*manifestID)
	if err != nil {
		return fmt.Errorf("official manifest %s: %w", *manifestID, err)
	}
	if !found {
		sm, err := st.GetManifest(r.ctx(), *manifestID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("unknown manifest %q; see 'plugrelay manifest list'", *manifestID)
			}
			return err
		}
		if m, err = manifest.Parse([]byte(sm.YAML)); err != nil {
			return fmt.Errorf("stored manifest %s: %w", *manifestID, err)
		}
	}
	if err := checkVars(m, values); err != nil {
		return usagef("%v", err)
	}

	conf := map[string]any{"manifest": m.ID}
	for k, v := range values {
		conf[k] = v
	}
	u := &store.Upstream{ID: *id, Provider: m.ProviderID(), Manifest: m.ID, Config: conf, Enabled: !*disabled}
	if found {
		u.Source = store.SourceOfficial
	}
	if err := st.PutUpstream(r.ctx(), u); err != nil {
		return err
	}
	fmt.Fprintf(r.env.Stdout, "Upstream %s saved (manifest %s, provider %s)\n", u.ID, m.ID, u.Provider)
	if secretValue == "" {
		if !m.Auth.KeyOptional {
			fmt.Fprintf(r.env.Stdout, "No credential stored: upstream %s is not routed until one is added (--credential-env)\n", u.ID)
		}
		return nil
	}
	// Re-adding an upstream replaces the credential this command created
	// before instead of piling up copies.
	c := &store.Credential{ID: u.ID + "-cli", UpstreamID: u.ID, Kind: store.CredShared, Secret: secretValue,
		Meta: map[string]string{"source_env": *credEnv}}
	if err := st.PutCredential(r.ctx(), c); err != nil {
		if errors.Is(err, store.ErrNoBox) {
			return errors.New("cannot store the credential: no master key configured (set master_key_file in relay.yaml)")
		}
		return err
	}
	fmt.Fprintf(r.env.Stdout, "Credential %s stored encrypted (from $%s)\n", c.ID, *credEnv)
	return nil
}

// checkVars rejects undeclared variables and missing required ones.
func checkVars(m *manifest.Manifest, values map[string]string) error {
	var unknown, missing []string
	for k := range values {
		if _, ok := m.Variables[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	for k, v := range m.Variables {
		if v.Required && v.Default == "" && values[k] == "" {
			missing = append(missing, k)
		}
	}
	sort.Strings(unknown)
	sort.Strings(missing)
	var errs []error
	if len(unknown) > 0 {
		declared := make([]string, 0, len(m.Variables))
		for k := range m.Variables {
			declared = append(declared, k)
		}
		slices.Sort(declared)
		errs = append(errs, fmt.Errorf("manifest %s does not declare variable(s) %s (declared: %s)",
			m.ID, strings.Join(unknown, ", "), orDash(strings.Join(declared, ", "))))
	}
	if len(missing) > 0 {
		errs = append(errs, fmt.Errorf("manifest %s requires --var %s", m.ID, strings.Join(missing, "=... --var ")+"=..."))
	}
	return errors.Join(errs...)
}

type upstreamRow struct {
	store.Upstream
	Credentials int `json:"credentials"`
}

func runUpstreamList(r *runner, cmd *command, args []string) error {
	fs := r.flags(cmd)
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := r.parse(fs, args); err != nil {
		return err
	}
	st, _, err := r.openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	ups, err := st.ListUpstreams(r.ctx())
	if err != nil {
		return err
	}
	creds, err := st.ListCredentials(r.ctx(), "")
	if err != nil {
		return err
	}
	count := map[string]int{}
	for _, c := range creds {
		count[c.UpstreamID]++
	}
	out := make([]upstreamRow, 0, len(ups))
	for _, u := range ups {
		out = append(out, upstreamRow{Upstream: u, Credentials: count[u.ID]})
	}
	if *asJSON {
		return printJSON(r.env.Stdout, out)
	}
	rows := make([][]string, 0, len(out))
	for _, u := range out {
		rows = append(rows, []string{u.ID, u.Provider, orDash(u.Manifest), strconv.FormatBool(u.Enabled), strconv.Itoa(u.Credentials)})
	}
	return table(r.env.Stdout, []string{"ID", "PROVIDER", "MANIFEST", "ENABLED", "CREDENTIALS"}, rows)
}

func runUpstreamRm(r *runner, cmd *command, args []string) error {
	id, err := parseID(r, cmd, args)
	if err != nil {
		return err
	}
	st, _, err := r.openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.DeleteUpstream(r.ctx(), id); err != nil {
		return notFound("upstream", id, err)
	}
	fmt.Fprintf(r.env.Stdout, "Upstream %s deleted with its credentials and routes\n", id)
	return nil
}
