package importnewapi_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/internal/importnewapi"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	plug "github.com/initxy/plugrelay/plugins/admin/importnewapi"
)

// fixture builds a new-api SQLite database from testdata/newapi.sql.
func fixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "one-api.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	body, err := os.ReadFile("testdata/newapi.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range strings.Split(string(body), ";\n") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%v\n%s", err, stmt)
		}
	}
	return path
}

func openStore(t *testing.T) store.Store {
	k, _ := secret.GenerateMasterKey()
	mk, _ := secret.ParseMasterKey(k)
	box, _ := secret.NewBox(mk)
	s, err := store.Open(context.Background(), store.Config{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "p.db"), Secrets: box})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var manifests = map[string]bool{"openai": true, "openai-compatible": false, "anthropic": true}

func importer(st store.Store) importnewapi.Importer {
	return importnewapi.Importer{Store: st, Opts: func() importnewapi.Options {
		return importnewapi.Options{Currency: "USD", Manifests: func(name string) (string, bool, bool) {
			off, ok := manifests[name]
			return "provider.openai-compatible", off, ok
		}}
	}}
}

func items(r *plug.Report, kind, op string) []plug.Item {
	var out []plug.Item
	for _, it := range r.Items {
		if it.Kind == kind && (op == "" || it.Op == op) {
			out = append(out, it)
		}
	}
	return out
}

func TestDryRunThenImport(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	path := fixture(t)
	hash, _ := secret.HashPassword("alice-password-1")
	if db, err := sql.Open("sqlite", path); err == nil {
		_, err = db.Exec(`UPDATE users SET password = ? WHERE id = 2`, hash)
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	src := plug.Source{Driver: "sqlite", DSN: path, ConfirmLocalFile: true}
	imp := importer(st)

	rep, err := imp.ImportNewAPI(ctx, src, true)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.DryRun || rep.Counts["upstream"] != 3 || rep.Counts["credential"] != 4 || rep.Counts["user"] != 3 ||
		rep.Counts["key"] != 2 || rep.Counts["group"] != 2 || rep.Counts["balance"] != 2 {
		b, _ := json.MarshalIndent(rep, "", " ")
		t.Fatalf("dry run report: %s", b)
	}
	if sk := items(rep, "upstream", "skip"); len(sk) != 1 || sk[0].Source != "4" {
		t.Fatalf("midjourney not skipped: %+v", sk)
	}
	if sk := items(rep, "key", "skip"); len(sk) != 2 {
		t.Fatalf("deleted-user and malformed tokens not skipped: %+v", sk)
	}
	// A dry run writes nothing.
	if ups, _ := st.ListUpstreams(ctx); len(ups) != 0 {
		t.Fatalf("dry run wrote upstreams: %v", ups)
	}
	if us, _ := st.ListUsers(ctx); len(us) != 0 {
		t.Fatalf("dry run wrote users")
	}

	rep, err = imp.ImportNewAPI(ctx, src, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.DryRun || len(items(rep, "upstream", "create")) != 3 {
		t.Fatalf("import report: %+v", rep.Items)
	}

	up, err := st.GetUpstream(ctx, "newapi-c2")
	if err != nil || up.Manifest != "openai-compatible" || up.Config["base_url"] != "https://relay.example.net/v1" {
		t.Fatalf("proxied openai channel: %+v %v", up, err)
	}
	if up, _ := st.GetUpstream(ctx, "newapi-c3"); up == nil || up.Enabled || up.Manifest != "anthropic" {
		t.Fatalf("disabled claude channel: %+v", up)
	}
	creds, _ := st.ListCredentials(ctx, "newapi-c1")
	if len(creds) != 2 {
		t.Fatalf("multi-key channel: %d credentials", len(creds))
	}
	routes, _ := st.ListRoutes(ctx, "gpt-4o-mini")
	if len(routes) != 1 || routes[0].UpstreamModel != "gpt-4o-mini-2024-07-18" || routes[0].Priority != 5 || routes[0].Weight != 10 ||
		strings.Join(routes[0].Groups, ",") != "newapi-default,newapi-vip" {
		t.Fatalf("mapped route: %+v", routes)
	}
	if g, err := st.GetGroup(ctx, "newapi-vip"); err != nil || g.PriceMultiplier != 0.8 {
		t.Fatalf("group ratio: %+v %v", g, err)
	}

	// bcrypt hashes carry over: the user logs in with the old password.
	if u, err := st.Authenticate(ctx, "alice@example.com", "alice-password-1"); err != nil || u.ID != "newapi-u2" {
		t.Fatalf("login with the new-api password: %+v %v", u, err)
	}
	root, err := st.GetUser(ctx, "newapi-u1")
	// new-api admins come over as users, and no address counts as verified.
	if err != nil || root.Role != store.RoleUser || root.Email != "root" || !root.EmailUnverified {
		t.Fatalf("root user: %+v %v", root, err)
	}
	if bob, _ := st.GetUser(ctx, "newapi-u3"); bob == nil || !bob.Disabled {
		t.Fatalf("disabled user: %+v", bob)
	}

	// Clients keep their new-api keys.
	k, err := st.KeyByToken(ctx, "sk-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGhIjKl")
	if err != nil || k.UserID != "newapi-u2" || k.GroupID != "newapi-vip" || string(k.Settings["models"]) != `["gpt-4o"]` ||
		string(k.Settings["ip_allowlist"]) != `["10.0.0.0/8"]` {
		t.Fatalf("imported key: %+v %v", k, err)
	}
	if k, _ := st.GetKey(ctx, "newapi-t2"); k == nil || k.ExpiresAt.Unix() != 1893456000 || k.GroupID != "newapi-vip" {
		t.Fatalf("expiring key: %+v", k)
	}

	l, _ := store.LedgerOf(st)
	if a, _ := l.Account(ctx, "user:newapi-u2"); a.BalanceMicro != 10_000_000 { // 5,000,000 quota / 500,000 per USD
		t.Fatalf("alice balance: %+v", a)
	}

	// Running again changes nothing and credits nothing twice.
	rep, err = imp.ImportNewAPI(ctx, src, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range rep.Items {
		if it.Op == "create" {
			t.Errorf("second import creates %+v", it)
		}
	}
	if a, _ := l.Account(ctx, "user:newapi-u2"); a.BalanceMicro != 10_000_000 {
		t.Fatalf("balance credited twice: %+v", a)
	}
}

func TestJSONExport(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	data := `{"channels":[{"id":7,"type":43,"key":"sk-ds","status":1,"name":"DeepSeek","models":"deepseek-chat","group":"default"}],
"users":[{"id":1,"username":"ann","password":"","role":1,"status":1,"email":"ann@example.com","quota":0,"group":"default"}],
"tokens":[],"options":{}}`
	rep, err := importer(st).ImportNewAPI(ctx, plug.Source{Data: json.RawMessage(data)}, true)
	if err != nil {
		t.Fatal(err)
	}
	// deepseek is not in this test's manifest list.
	if sk := items(rep, "upstream", "skip"); len(sk) != 1 || !strings.Contains(sk[0].Reason, "deepseek") {
		t.Fatalf("report: %+v", rep.Items)
	}
	if len(items(rep, "user", "create")) != 1 {
		t.Fatalf("users: %+v", rep.Items)
	}
}

// The DSN must not turn the relay into a file reader.
func TestSourceDSNHardening(t *testing.T) {
	ctx := context.Background()
	path := fixture(t)
	for name, src := range map[string]plug.Source{
		"sqlite unconfirmed":  {Driver: "sqlite", DSN: path},
		"sqlite URI options":  {Driver: "sqlite", DSN: "file:" + path + "?_pragma=foreign_keys(1)", ConfirmLocalFile: true},
		"sqlite missing file": {Driver: "sqlite", DSN: path + ".nope", ConfirmLocalFile: true},
		"sqlite memory":       {Driver: "sqlite", DSN: ":memory:", ConfirmLocalFile: true},
		"mysql allowAllFiles": {Driver: "mysql", DSN: "u:p@tcp(127.0.0.1:1)/db?allowAllFiles=true"},
		"mysql local_infile":  {Driver: "mysql", DSN: "u:p@tcp(127.0.0.1:1)/db?local_infile=1"},
		"mysql bad DSN":       {Driver: "mysql", DSN: "not a dsn"},
		"unknown driver":      {Driver: "oracle", DSN: "x"},
	} {
		if _, err := importnewapi.Load(ctx, src); err == nil || strings.Contains(err.Error(), "connect") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(path + ".nope"); err == nil {
		t.Fatal("a missing SQLite file was created")
	}
	if _, err := importnewapi.Load(ctx, plug.Source{Driver: "sqlite", DSN: path, ConfirmLocalFile: true}); err != nil {
		t.Fatalf("confirmed SQLite file: %v", err)
	}
}

// The mapped document goes through Options.Apply (the relay's checked
// apply); its refusal stops the import before any user is written.
func TestImportUsesOptionsApply(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	src := plug.Source{Driver: "sqlite", DSN: fixture(t), ConfirmLocalFile: true}
	var calls int
	imp := importnewapi.Importer{Store: st, Opts: func() importnewapi.Options {
		o := importer(st).Opts()
		o.Apply = func(ctx context.Context, doc []byte, dryRun bool) (*store.ApplyResult, error) {
			calls++
			return nil, errors.New("refused by a settings check")
		}
		return o
	}}
	if _, err := imp.ImportNewAPI(ctx, src, false); err == nil || !strings.Contains(err.Error(), "refused by a settings check") || calls != 1 {
		t.Fatalf("import: %v (calls %d)", err, calls)
	}
	if us, _ := st.ListUsers(ctx); len(us) != 0 {
		t.Fatalf("refused import wrote users: %v", us)
	}
}
