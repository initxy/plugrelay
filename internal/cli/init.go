package cli

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
)

const minPasswordLen = 8

func initCommand() *command {
	return &command{
		name: "init",
		summary: `Create relay.yaml, a master key and the database for a new relay.

Writes <config> and <data-dir>/master.key (0600), creates the SQLite
database at <data-dir>/plugrelay.db and applies migrations. Refuses to
overwrite an existing config without --force; an existing master key is
always kept, because replacing it makes stored credentials unreadable.

An admin user is created when --admin-email is given, and is required
when the relay listens beyond this machine; with one, the console needs a
login even on this machine. There is no default password: it comes from
--admin-password, $PLUGRELAY_ADMIN_PASSWORD, or is read from stdin. Without
an admin, a local owner account is created that the console on this machine
signs in as.`,
		usage:   "init [--listen ADDR] [--admin-email E [--admin-name N] [--admin-password P] | --no-admin] [--public-url URL] [--force]",
		example: "plugrelay init\nPLUGRELAY_ADMIN_PASSWORD=... plugrelay init --listen 0.0.0.0:8787 --admin-email ops@example.com --config /etc/plugrelay/relay.yaml",
		run:     runInit,
	}
}

type initOpts struct {
	listen     string
	publicURL  string
	adminEmail string
	adminName  string
	adminPass  string
	noAdmin    bool
	force      bool
}

func runInit(r *runner, cmd *command, args []string) error {
	fset := r.flags(cmd)
	var o initOpts
	fset.StringVar(&o.listen, "listen", "127.0.0.1:8787", "listen address; 0.0.0.0 exposes the relay to the network")
	fset.StringVar(&o.publicURL, "public-url", "", "externally reachable base URL, stored as the site setting site.public_base_url (needed for Cursor)")
	fset.StringVar(&o.adminEmail, "admin-email", "", "create a console admin with this email")
	fset.StringVar(&o.adminName, "admin-name", "admin", "display name of the admin")
	fset.StringVar(&o.adminPass, "admin-password", "", "admin password (visible in shell history; prefer $PLUGRELAY_ADMIN_PASSWORD)")
	fset.BoolVar(&o.noAdmin, "no-admin", false, "do not create an admin user or local owner")
	fset.BoolVar(&o.force, "force", false, "overwrite an existing config file")
	pos, err := r.parse(fset, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("init takes no arguments")
	}
	set := map[string]bool{}
	fset.Visit(func(f *flag.Flag) { set[f.Name] = true })
	in := bufio.NewReader(r.env.Stdin)
	interactive := r.env.Interactive != nil && r.env.Interactive()

	if interactive && !set["listen"] {
		o.listen = r.ask(in, "Listen address (127.0.0.1 keeps it on this machine)", o.listen)
	}
	if _, _, err := net.SplitHostPort(o.listen); err != nil {
		return usagef("--listen: want host:port, got %q", o.listen)
	}
	if o.publicURL != "" {
		u, err := url.Parse(o.publicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return usagef("--public-url: want an absolute http(s) URL, got %q", o.publicURL)
		}
	}
	needAdmin := !o.noAdmin && (o.adminEmail != "" || !isLoopback(o.listen))
	if needAdmin && o.adminEmail == "" {
		if !interactive {
			return usagef("a relay reachable from other machines needs an admin: pass --admin-email (or --no-admin)")
		}
		o.adminEmail = r.ask(in, "Admin email", "")
		if o.adminEmail == "" {
			return usagef("admin email required")
		}
	}

	cfgPath, err := filepath.Abs(r.configPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfgPath); err == nil && !o.force {
		return fmt.Errorf("%s already exists; pass --force to overwrite it (the master key and database are kept)", cfgPath)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	cfgDir := filepath.Dir(cfgPath)
	dataDir := cfgDir
	if r.dataDir != "" {
		if dataDir, err = filepath.Abs(r.dataDir); err != nil {
			return err
		}
	}

	if needAdmin {
		if o.adminPass, err = r.adminPassword(in, o.adminPass, interactive); err != nil {
			return err
		}
	}

	keyPath := filepath.Join(dataDir, "master.key")
	dbPath := filepath.Join(dataDir, "plugrelay.db")
	// Bare file names keep a config next to its data portable; anything
	// elsewhere is written absolute so it works from any directory.
	refKey, refDB := keyPath, dbPath
	if r.dataDir == "" {
		refKey, refDB = "master.key", "plugrelay.db"
	}
	text, err := renderConfig(configValues{
		Listen: o.listen, MasterKeyFile: refKey, DSN: refDB,
	})
	if err != nil {
		return err
	}
	// Validate before touching the disk so bad flags leave nothing behind.
	if _, err := config.Parse(text, nil); err != nil {
		return usagef("%v", err)
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	keyCreated, err := ensureMasterKey(keyPath)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(cfgPath, text, 0o644); err != nil {
		return err
	}

	saved := r.dataDir
	r.dataDir = cfgDir
	st, cfg, err := r.openStore()
	r.dataDir = saved
	if err != nil {
		return err
	}
	defer st.Close()

	out := r.env.Stdout
	fmt.Fprintf(out, "Wrote %s\n", cfgPath)
	if keyCreated {
		fmt.Fprintf(out, "Generated master key %s (0600). Back it up: without it stored credentials cannot be decrypted.\n", keyPath)
	} else {
		fmt.Fprintf(out, "Kept existing master key %s\n", keyPath)
	}
	fmt.Fprintf(out, "Database ready at %s\n", cfg.Store.DSN)

	if o.publicURL != "" {
		if err := admin.PutSiteSetting(r.ctx(), st, admin.SettingPublicURL, strings.TrimRight(o.publicURL, "/")); err != nil {
			return err
		}
	}
	if needAdmin {
		if err := ensureAdmin(r, st, o); err != nil {
			return err
		}
		// A relay with a real admin is shared: no login-free console.
		if err := admin.PutSiteSetting(r.ctx(), st, admin.SettingLocalOwner, false); err != nil {
			return err
		}
	} else if !o.noAdmin {
		if err := ensureLocalOwner(r, st); err != nil {
			return err
		}
	}
	if !isLoopback(o.listen) {
		fmt.Fprintf(r.env.Stderr, "warning: %s accepts connections from other machines; put it behind TLS\n", o.listen)
	}
	fmt.Fprintf(out, `
Next steps:
  export DEEPSEEK_API_KEY=...
  plugrelay upstream add --manifest deepseek --credential-env DEEPSEEK_API_KEY
  plugrelay model add --name deepseek-chat --route deepseek:deepseek-chat
  plugrelay key create --name me
  plugrelay serve
`)
	return nil
}

// ask prints a prompt and returns the trimmed answer, or def when empty.
func (r *runner) ask(in *bufio.Reader, prompt, def string) string {
	if def != "" {
		fmt.Fprintf(r.env.Stderr, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(r.env.Stderr, "%s: ", prompt)
	}
	line, _ := in.ReadString('\n')
	if line = strings.TrimSpace(line); line != "" {
		return line
	}
	return def
}

func (r *runner) adminPassword(in *bufio.Reader, flagValue string, interactive bool) (string, error) {
	pw := flagValue
	if pw == "" {
		pw = r.env.getenv("PLUGRELAY_ADMIN_PASSWORD")
	}
	if pw == "" {
		if interactive {
			fmt.Fprintln(r.env.Stderr, "warning: the password is echoed while you type; set PLUGRELAY_ADMIN_PASSWORD to avoid that")
		}
		fmt.Fprint(r.env.Stderr, "Admin password: ")
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("no admin password: pass --admin-password, set PLUGRELAY_ADMIN_PASSWORD or type it on stdin")
		}
		pw = strings.TrimRight(line, "\r\n")
		if interactive {
			fmt.Fprint(r.env.Stderr, "Repeat password: ")
			again, _ := in.ReadString('\n')
			if strings.TrimRight(again, "\r\n") != pw {
				return "", errors.New("passwords do not match")
			}
		}
	}
	if len(pw) < minPasswordLen {
		return "", fmt.Errorf("admin password must be at least %d characters", minPasswordLen)
	}
	return pw, nil
}

func ensureAdmin(r *runner, st store.Store, o initOpts) error {
	users, err := st.ListUsers(r.ctx())
	if err != nil {
		return err
	}
	for _, u := range users {
		if strings.EqualFold(u.Email, o.adminEmail) {
			fmt.Fprintf(r.env.Stdout, "Admin %s already exists; password unchanged\n", u.Email)
			return nil
		}
	}
	u := &store.User{Name: o.adminName, Email: o.adminEmail, Role: store.RoleAdmin}
	if err := st.CreateUser(r.ctx(), u, o.adminPass); err != nil {
		return err
	}
	fmt.Fprintf(r.env.Stdout, "Created admin %s\n", u.Email)
	return nil
}

// localOwnerEmail names the account the console on this machine acts as.
// Its password is random and never shown: on a loopback listener the
// console needs no login (site setting security.local_owner_login), and
// sharing the relay means creating a real admin.
const localOwnerEmail = "owner@localhost"

// ensureLocalOwner creates the local owner so the console works
// without the setup wizard.
func ensureLocalOwner(r *runner, st store.Store) error {
	users, err := st.ListUsers(r.ctx())
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.Role == store.RoleAdmin && !u.Disabled {
			return nil
		}
	}
	pw := make([]byte, 32)
	if _, err := rand.Read(pw); err != nil {
		return err
	}
	u := &store.User{Name: "owner", Email: localOwnerEmail, Role: store.RoleAdmin}
	if err := st.CreateUser(r.ctx(), u, base64.RawURLEncoding.EncodeToString(pw)); err != nil {
		return err
	}
	fmt.Fprintln(r.env.Stdout, "The console on this machine needs no login")
	return nil
}

// ensureMasterKey keeps a valid existing key file or creates a new one.
func ensureMasterKey(path string) (created bool, err error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if _, err := secret.ParseMasterKey(string(data)); err != nil {
			return false, fmt.Errorf("existing %s: %w", path, err)
		}
		return false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	key, err := secret.GenerateMasterKey()
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, fmt.Errorf("write master key: %w", err)
	}
	if _, err := f.WriteString(key + "\n"); err != nil {
		f.Close()
		return false, fmt.Errorf("write master key: %w", err)
	}
	return true, f.Close()
}

// writeFileAtomic replaces path through a temp file in the same directory.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type configValues struct {
	Listen, MasterKeyFile, DSN string
}

var configTemplate = template.Must(template.New("relay.yaml").Funcs(template.FuncMap{"q": yamlQuote}).Parse(`# relay.yaml — written by plugrelay init. Edit by hand; plugrelay never
# writes it again. Upstreams, keys, models, features and site settings
# (such as the public URL) live in the database: use the console,
# plugrelay apply, or the key/upstream/model commands.
#
# Every field can be overridden by a PLUGRELAY_* environment variable.
# Relative paths resolve against this file's directory.

# Keep 127.0.0.1 unless a proxy or a team needs access.
listen: {{q .Listen}}

# Encrypts upstream credentials. Losing it makes them unreadable.
master_key_file: {{q .MasterKeyFile}}

store:
  driver: sqlite
  dsn: {{q .DSN}}

state:
  driver: memory

log:
  level: info
  format: text
`))

func renderConfig(v configValues) ([]byte, error) {
	var b bytes.Buffer
	if err := configTemplate.Execute(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// yamlQuote writes s as a double-quoted YAML scalar.
func yamlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(c)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
