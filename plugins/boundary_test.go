// Package plugins_test guards the plugin boundary: compiled-in plugins are
// written against the public contracts (pkg/...) exactly like third-party
// plugins, so they must not import the relay's own internal packages, reach
// the network except through the core egress, or write charges except
// through the meter (hard rule 23).
package plugins_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/pkg/plugin"
)

const forbidden = "github.com/initxy/plugrelay/internal/"

func TestPluginsImportOnlyPublicContracts(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(p, forbidden) {
				t.Errorf("%s imports %s; plugins may use pkg/... and plugins/internal/... only", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// pluginFiles parses every non-test plugin source. plugins/internal/testkit
// is a test helper and fakes the Host with http.DefaultClient on purpose.
func pluginFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	out := map[string]*ast.File{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path == filepath.Join("internal", "testkit") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		out[path] = f
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("no plugin sources found")
	}
	return out
}

// importName is the name f refers to pkg by, or "" when f does not import it.
func importName(f *ast.File, pkg string) string {
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == pkg {
			if imp.Name != nil {
				return imp.Name.Name
			}
			return pkg[strings.LastIndex(pkg, "/")+1:]
		}
	}
	return ""
}

// What reaches the network outside the core egress (which applies header,
// proxy and SSRF rules): package-level clients and helpers, used anywhere,
// and client, transport and dialer types, when constructed.
var (
	egressValues = map[string][]string{
		"net/http": {"DefaultClient", "DefaultTransport", "Get", "Head", "Post", "PostForm"},
		"net":      {"Dial", "DialTimeout", "DialTCP", "DialUDP", "DialIP", "DialUnix"},
	}
	egressTypes = map[string][]string{
		"net/http": {"Client", "Transport"},
		"net":      {"Dialer"},
	}
)

// TestPluginsUseCoreEgress: plugins send HTTP only through the clients the
// core hands out (Host.HTTPClient, Host.ClientFor, Outgoing.Client).
// Naming *http.Client as a type is fine; creating or using any other client,
// transport or dialer is not. Providers talk to upstreams on behalf of a
// credential, so they never take the direct Host.HTTPClient, which would
// skip the credential's proxy (hard rule 15).
func TestPluginsUseCoreEgress(t *testing.T) {
	for path, f := range pluginFiles(t) {
		if strings.HasPrefix(path, "provider"+string(filepath.Separator)) {
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "HTTPClient" {
					t.Errorf("%s: a provider uses HTTPClient; upstream calls take Outgoing.Client or Host.ClientFor", path)
				}
				return true
			})
		}
		names := map[string]string{} // local name -> import path
		for pkg := range egressValues {
			if n := importName(f, pkg); n != "" && n != "_" {
				names[n] = pkg
			}
		}
		if len(names) == 0 {
			continue
		}
		// match reports the import path when x is <pkg>.<one of list[pkg]>.
		match := func(x ast.Expr, list map[string][]string) (string, string, bool) {
			sel, ok := x.(*ast.SelectorExpr)
			if !ok {
				return "", "", false
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return "", "", false
			}
			pkg, ok := names[id.Name]
			return pkg, sel.Sel.Name, ok && slices.Contains(list[pkg], sel.Sel.Name)
		}
		report := func(pkg, name string) {
			t.Errorf("%s: uses %s.%s; plugins must send through Host.HTTPClient", path, pkg, name)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if pkg, name, ok := match(x, egressValues); ok {
					report(pkg, name)
				}
			case *ast.CompositeLit: // http.Client{...}, &net.Dialer{...}
				if pkg, name, ok := match(x.Type, egressTypes); ok {
					report(pkg, name)
				}
			case *ast.CallExpr: // new(http.Transport)
				if fn, ok := x.Fun.(*ast.Ident); ok && fn.Name == "new" && len(x.Args) == 1 {
					if pkg, name, ok := match(x.Args[0], egressTypes); ok {
						report(pkg, name)
					}
				}
			}
			return true
		})
	}
}

// TestPluginsCannotChangeBalances: what a caller is charged is decided by
// the meter (Reserve/Settle/Refund, called by the core) and persisted with
// the request record. Plugins get no database, the Host offers only reads of
// recorded spend, and the record write path belongs to the request log sink.
// The sell kit's prepaid ledger (an optional Host interface) is written only
// by the plugins listed in owner below.
func TestPluginsCannotChangeBalances(t *testing.T) {
	host := methods(reflect.TypeFor[plugin.Host]())
	if want := []string{"ClientFor", "HTTPClient", "KV", "Logger", "Settings", "Usage", "Version"}; !slices.Equal(host, want) {
		t.Errorf("plugin.Host methods = %v, want %v: a new Host method must not let non-meter plugins change charges", host, want)
	}
	if got := methods(reflect.TypeFor[plugin.UsageReader]()); !slices.Equal(got, []string{"Spent"}) {
		t.Errorf("plugin.UsageReader methods = %v, want read-only [Spent]", got)
	}

	dbImports := []string{"database/sql", "github.com/jackc/pgx", "modernc.org/sqlite", "github.com/redis/go-redis"}
	// Charge-bearing writes: record insertion and purge (the request log
	// sink and its retention job), and the meter's own calls.
	// Balance ledger writes (sell kit): holds and charges by the balance
	// meter, credits by it (admin adjustments) and by payments, redeems
	// by the redeem plugin, check-ins by the rewards plugin, referral
	// rewards by payments; sellkit/ledger declares them.
	owner := map[string][]string{
		"InsertRecord": {"sink/requestlog"}, "InsertRecords": {"sink/requestlog"},
		"RecordWriter": {"sink/requestlog"}, "RecordPurger": {"sink/requestlog"},
		"Reserve": {"meter/"}, "Settle": {"meter/"}, "Refund": {"meter/"},
		"Hold": {"meter/balance", "sellkit/ledger"}, "Capture": {"meter/balance", "sellkit/ledger"},
		"Release": {"meter/balance", "sellkit/ledger"},
		"Credit":  {"meter/balance", "admin/payment", "sellkit/ledger"},
		"Redeem":  {"admin/redeem", "sellkit/ledger"}, "CreateCodes": {"admin/redeem", "sellkit/ledger"},
		// Rewards: a check-in by the rewards plugin, a referral reward when
		// a top-up is credited.
		"CheckIn": {"admin/rewards", "sellkit/ledger"}, "RewardReferral": {"admin/payment", "sellkit/ledger"},
		// Plan subscriptions: allowance draws by the balance meter, periods
		// started (and paid from the balance) by the subscriptions plugin.
		"SubscriptionHold": {"meter/balance", "sellkit/ledger"}, "SubscriptionSettle": {"meter/balance", "sellkit/ledger"},
		"SubscriptionRelease": {"meter/balance", "sellkit/ledger"},
		"StartPeriod":         {"admin/subscriptions", "sellkit/ledger"},
		"RenewDue":            {"meter/balance", "admin/subscriptions", "sellkit/ledger"},
	}
	for path, f := range pluginFiles(t) {
		slash := filepath.ToSlash(path)
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			for _, db := range dbImports {
				if p == db || strings.HasPrefix(p, db+"/") {
					t.Errorf("%s imports %s; plugins reach stored data only through the Host", path, p)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			var name string
			switch x := n.(type) {
			case *ast.SelectorExpr:
				name = x.Sel.Name
			case *ast.Field: // interface method declarations
				if _, ok := x.Type.(*ast.FuncType); ok && len(x.Names) == 1 {
					name = x.Names[0].Name
				}
			case *ast.FuncDecl:
				if x.Recv != nil {
					name = x.Name.Name
				}
			}
			if dirs, ok := owner[name]; ok && !slices.ContainsFunc(dirs, func(d string) bool { return strings.HasPrefix(slash, d) }) {
				t.Errorf("%s: %s belongs to plugins/%s only; other plugins must not write charges", path, name, strings.Join(dirs, ", plugins/"))
			}
			return true
		})
	}
}

func methods(t reflect.Type) []string {
	var out []string
	for i := range t.NumMethod() {
		out = append(out, t.Method(i).Name)
	}
	return out
}
