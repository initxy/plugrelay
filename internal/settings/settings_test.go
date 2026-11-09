package settings

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/pkg/plugin"
)

const all = plugin.Global | plugin.Group | plugin.User | plugin.Key

func raw(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

func lay(g, gr, u, k string) Layered {
	return Layered{Global: raw(g), Group: raw(gr), User: raw(u), Key: raw(k)}
}

// Hard rule 20: a Strictest setting can never be loosened by a lower
// layer, except on a key explicitly marked as an exception.
func TestHardRule20StrictestNeverLoosens(t *testing.T) {
	cases := []struct {
		name   string
		typ    plugin.SettingType
		layers Layered
		want   string
	}{
		{"int lower tighter", plugin.TypeInt, lay("100", "50", "", "10"), "10"},
		{"int lower looser", plugin.TypeInt, lay("100", "50", "", "500"), "50"},
		{"int zero does not lift", plugin.TypeInt, lay("", "60", "", "0"), "60"},
		{"int all unlimited", plugin.TypeInt, lay("0", "", "", "0"), "0"},
		{"int top unlimited", plugin.TypeInt, lay("0", "", "", "30"), "30"},
		{"float", plugin.TypeFloat, lay("1.5", "", "2.5", ""), "1.5"},
		{"duration", plugin.TypeDuration, lay(`"1h"`, `"30m"`, "", `"2h"`), `"30m"`},
		{"duration seconds", plugin.TypeDuration, lay(`"1m"`, "", "", "30"), "30"},
		{"bool false wins", plugin.TypeBool, lay("false", "true", "", "true"), "false"},
		{"bool all true", plugin.TypeBool, lay("true", "", "", "true"), "true"},
		{"list intersection", plugin.TypeList, lay(`["a","b","c"]`, `["b","c","d"]`, "", `["c","d","e"]`), `["c"]`},
		{"list cannot widen", plugin.TypeList, lay(`["a"]`, "", "", `["a","b"]`), `["a"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := plugin.Setting{Key: "x", Type: tc.typ, Scopes: all, Merge: plugin.Strictest}
			got := Merge(s, tc.layers, false)
			if string(got.Raw()) != tc.want {
				t.Fatalf("got %s, want %s", got.Raw(), tc.want)
			}
		})
	}
	t.Run("exception key bypasses", func(t *testing.T) {
		s := plugin.Setting{Key: "rpm", Type: plugin.TypeInt, Scopes: all, Merge: plugin.Strictest}
		if got := Merge(s, lay("100", "50", "", "500"), true).Int(); got != 500 {
			t.Fatalf("got %d", got)
		}
		// An exception without a key-level value still gets the strictest above.
		if got := Merge(s, lay("100", "50", "", ""), true).Int(); got != 50 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("list layers checked one by one", func(t *testing.T) {
		s := plugin.Setting{Key: "allow", Type: plugin.TypeList, Scopes: all, Merge: plugin.Strictest}
		has := func(want string) func([]string) bool {
			return func(l []string) bool {
				for _, p := range l {
					if ok, _ := path.Match(p, want); ok {
						return true
					}
				}
				return false
			}
		}
		// Patterns that share no string still narrow per layer.
		v := Merge(s, lay("", `["github__read_*"]`, "", `["github__list_issues","github__read_file"]`), false)
		if !v.ListAllows(has("github__read_file")) || v.ListAllows(has("github__list_issues")) || v.ListAllows(has("github__delete")) {
			t.Fatalf("per-layer check wrong: %v", v.ListLayers())
		}
		// An empty intersection is a deny-all list, never "no restriction".
		v = Merge(s, lay("", `["github__read_*"]`, "", `["github__list_issues"]`), false)
		if got := v.List(); len(got) != 1 || got[0] != plugin.DenyAll {
			t.Fatalf("List = %q", got)
		}
		if v.ListAllows(has("github__list_issues")) || v.ListAllows(has("github__read_x")) {
			t.Fatal("disjoint layers allowed a tool")
		}
		// An empty layer restricts nothing and cannot lift the layers above.
		v = Merge(s, lay(`["a"]`, "", "", `[]`), false)
		if string(v.Raw()) != `["a"]` || v.ListAllows(has("b")) {
			t.Fatalf("empty key layer lifted: %s", v.Raw())
		}
		if v = Merge(s, lay(`[]`, "", "", ""), false); !v.ListAllows(has("x")) {
			t.Fatal("empty list should allow all")
		}
	})
	t.Run("disallowed layer ignored", func(t *testing.T) {
		s := plugin.Setting{Key: "rpm", Type: plugin.TypeInt, Scopes: plugin.Global | plugin.Group, Merge: plugin.Override}
		if got := Merge(s, lay("100", "", "", "1"), false).Int(); got != 100 {
			t.Fatalf("got %d", got)
		}
	})
}

func TestUnionOnlyGrows(t *testing.T) {
	s := plugin.Setting{Key: "deny", Type: plugin.TypeList, Scopes: all, Merge: plugin.Union}
	for _, tc := range []struct {
		layers Layered
		exempt bool
		want   string
	}{
		{lay(`["a","b"]`, `["b","c"]`, "", `[]`), false, `["a","b","c"]`},
		{lay(`["a"]`, "", `["d"]`, ""), false, `["a","d"]`},
		{lay(`["a"]`, "", "", `["k"]`), true, `["k"]`},
		{lay("", "", "", ""), false, ""},
	} {
		if got := Merge(s, tc.layers, tc.exempt); string(got.Raw()) != tc.want {
			t.Errorf("%+v exempt=%v: got %s, want %s", tc.layers, tc.exempt, got.Raw(), tc.want)
		}
	}
}

// An invalid layer never lifts a restriction: a Strictest list's invalid
// layer allows nothing, a Union deny list keeps what it can of it.
func TestInvalidLayerFailsClosed(t *testing.T) {
	clients := plugin.Setting{Key: "allowed_clients", Type: plugin.TypeList, Scopes: all, Merge: plugin.Strictest,
		Enum: []string{"codex", "curl"}}
	ips := plugin.Setting{Key: "allowed_ips", Type: plugin.TypeList, Scopes: all, Merge: plugin.Strictest}
	for _, tc := range []struct {
		name string
		s    plugin.Setting
		l    Layered
	}{
		{"unknown id among known", clients, lay("", `["codex","claude-cli/"]`, "", "")},
		{"only an unknown id", clients, lay("", `["claude-cli/"]`, "", "")},
		{"a string, not a list", clients, lay("", `"codex"`, "", "")},
		{"invalid below a valid layer", clients, lay(`["curl"]`, "", "", `"curl"`)},
		{"ip range as a string", ips, lay("", `"10.0.0.0/8"`, "", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Merge(tc.s, tc.l, false)
			// Whatever a reader checks the entries for, a layer of only
			// DenyAll fails it, and no layers at all would pass.
			if v.ListAllows(func(l []string) bool { return !slices.Equal(l, []string{plugin.DenyAll}) }) {
				t.Fatalf("invalid layer lifts the restriction: %s %v", v.Raw(), v.ListLayers())
			}
		})
	}
	if v := Merge(clients, lay("", `"codex"`, "", `["curl"]`), true); !slices.Equal(v.List(), []string{"curl"}) {
		t.Fatalf("an exempted key's own valid value stands: %s", v.Raw())
	}

	deny := plugin.Setting{Key: "blocked_tools", Type: plugin.TypeList, Scopes: all, Merge: plugin.Union}
	if got := Merge(deny, lay(`["a"]`, `"b"`, `7`, ""), false).Raw(); string(got) != `["a","b"]` {
		t.Fatalf("union keeps a lone string: %s", got)
	}
	detect := plugin.Setting{Key: "detect", Type: plugin.TypeList, Scopes: all, Merge: plugin.Union, Enum: []string{"email", "phone"}}
	if got := Merge(detect, lay(`["email","gone"]`, `["phone"]`, "", ""), false).Raw(); string(got) != `["email","phone"]` {
		t.Fatalf("union keeps the valid entries: %s", got)
	}
	if got := Merge(deny, lay(`["gpt-4o", 7, null]`, "", "", ""), false).Raw(); string(got) != `["gpt-4o"]` {
		t.Fatalf("union keeps the string entries of a mixed list: %s", got)
	}
}

// Every invalid stored row is logged with its setting, layer and owner,
// and what the merge makes of it.
func TestWarnInvalid(t *testing.T) {
	decls := map[string][]plugin.Setting{"p": {
		{Key: "rpm", Type: plugin.TypeInt, Scopes: all, Merge: plugin.Strictest},
		{Key: "clients", Type: plugin.TypeList, Scopes: all, Merge: plugin.Strictest, Enum: []string{"codex"}},
		{Key: "deny", Type: plugin.TypeList, Scopes: all, Merge: plugin.Union},
		{Key: "tok", Type: plugin.TypeSecret, Scopes: plugin.Global},
	}}
	var buf strings.Builder
	NewResolver(decls, []Entry{
		{Scope: plugin.Group, ScopeID: "g1", PluginID: "p", Key: "rpm", Value: raw(`"10"`)},
		{Scope: plugin.Key, ScopeID: "k1", PluginID: "p", Key: "rpm", Value: raw(`-1`)},
		{Scope: plugin.Group, ScopeID: "g1", PluginID: "p", Key: "clients", Value: raw(`"codex"`)},
		{Scope: plugin.User, ScopeID: "u1", PluginID: "p", Key: "deny", Value: raw(`["a", 7]`)},
		{Scope: plugin.Global, PluginID: "p", Key: "rpm", Value: raw(`60`)},
		{Scope: plugin.Global, PluginID: "p", Key: "tok", Value: raw(`{"plain":"sk-live-SECRET123"}`)},
	}, nil).WarnInvalid(slog.New(slog.NewTextHandler(&buf, nil)))
	out := buf.String()
	for _, want := range []string{
		`setting=p/rpm layer=group owner=g1 effect=ignored`,
		`setting=p/rpm layer=key owner=k1 effect=ignored`,
		`setting=p/clients layer=group owner=g1 effect="allows nothing"`,
		`setting=p/deny layer=user owner=u1 effect="only its valid entries count"`,
		`setting=p/tok layer=global`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SECRET123") {
		t.Errorf("a secret's value reached the log:\n%s", out)
	}
	if n := strings.Count(out, "stored setting value is invalid"); n != 5 {
		t.Errorf("%d warnings, want 5 (valid rows are not logged):\n%s", n, out)
	}
}

func TestOverrideAndDefault(t *testing.T) {
	s := plugin.Setting{Key: "mode", Type: plugin.TypeString, Scopes: all, Default: "fast"}
	if got := Merge(s, Layered{}, false).String(); got != "fast" {
		t.Fatalf("default: %q", got)
	}
	if got := Merge(s, lay(`"a"`, `"b"`, "", `null`), false).String(); got != "b" {
		t.Fatalf("override: %q", got)
	}
	if v := Merge(plugin.Setting{Key: "n", Type: plugin.TypeInt, Scopes: all}, Layered{}, false); v.IsSet() {
		t.Fatal("unset with no default should not be set")
	}
}

func TestValidate(t *testing.T) {
	ok := []struct {
		typ plugin.SettingType
		v   string
	}{
		{plugin.TypeInt, "3"}, {plugin.TypeFloat, "1.5"}, {plugin.TypeBool, "true"},
		{plugin.TypeDuration, `"5m"`}, {plugin.TypeDuration, "30"}, {plugin.TypeList, `["a"]`},
		{plugin.TypeString, `"x"`}, {plugin.TypeJSON, `{"a":1}`}, {plugin.TypeInt, "null"},
	}
	for _, c := range ok {
		if err := Validate(plugin.Setting{Key: "k", Type: c.typ}, raw(c.v)); err != nil {
			t.Errorf("%s %s: %v", c.typ, c.v, err)
		}
	}
	bad := []struct {
		s plugin.Setting
		v string
	}{
		{plugin.Setting{Type: plugin.TypeInt}, "1.5"},
		{plugin.Setting{Type: plugin.TypeInt}, `"3"`},
		{plugin.Setting{Type: plugin.TypeInt, Merge: plugin.Strictest}, "-1"},
		{plugin.Setting{Type: plugin.TypeBool}, "1"},
		{plugin.Setting{Type: plugin.TypeDuration}, `"soon"`},
		{plugin.Setting{Type: plugin.TypeList}, `"a"`},
		{plugin.Setting{Type: plugin.TypeString, Enum: []string{"a"}}, `"b"`},
		{plugin.Setting{Type: plugin.TypeString}, `{`},
	}
	for _, c := range bad {
		if err := Validate(c.s, raw(c.v)); err == nil {
			t.Errorf("%s %s accepted", c.s.Type, c.v)
		}
	}
}

func TestResolver(t *testing.T) {
	mk, _ := secret.GenerateMasterKey()
	k, _ := secret.ParseMasterKey(mk)
	box, _ := secret.NewBox(k)
	sealed, err := SealSecret(box, "p", "token", raw(`"s3cret"`))
	if err != nil {
		t.Fatal(err)
	}
	decls := map[string][]plugin.Setting{"p": {
		{Key: "rpm", Type: plugin.TypeInt, Scopes: all, Merge: plugin.Strictest},
		{Key: "token", Type: plugin.TypeSecret, Scopes: plugin.Global},
		{Key: "ttl", Type: plugin.TypeDuration, Scopes: all, Default: "1m"},
	}}
	r := NewResolver(decls, []Entry{
		{Scope: plugin.Global, PluginID: "p", Key: "rpm", Value: raw("100")},
		{Scope: plugin.Group, ScopeID: "g1", PluginID: "p", Key: "rpm", Value: raw("60")},
		{Scope: plugin.Key, ScopeID: "k1", PluginID: "p", Key: "rpm", Value: raw("1000")},
		{Scope: plugin.Global, PluginID: "p", Key: "token", Value: sealed},
		{Scope: plugin.Global, PluginID: "other", Key: "rpm", Value: raw("1")},
	}, box)
	c := &plugin.Caller{KeyID: "k1", GroupID: "g1"}
	if got := r.Get(c, nil, "p", "rpm").Int(); got != 60 {
		t.Fatalf("rpm = %d", got)
	}
	if got := r.For(c, Exemptions{"p/rpm"})("p", "rpm").Int(); got != 1000 {
		t.Fatalf("exempt rpm = %d", got)
	}
	if got := r.Global("p").Get("rpm").Int(); got != 100 {
		t.Fatalf("global rpm = %d", got)
	}
	if got := r.Global("p").Get("token").String(); got != "s3cret" {
		t.Fatalf("token = %q", got)
	}
	if got := r.Global("p").Get("ttl").Duration(); got != time.Minute {
		t.Fatalf("ttl = %v", got)
	}
	if r.Global("other").Get("rpm").IsSet() {
		t.Fatal("undeclared plugin resolved")
	}
	if NewResolver(decls, []Entry{{Scope: plugin.Global, PluginID: "p", Key: "token", Value: sealed}}, nil).Global("p").Get("token").IsSet() {
		t.Fatal("secret resolved without box")
	}
}

// Money settings are micro-units at rest and currency units in the admin
// API, both ways.
func TestMoneyDisplay(t *testing.T) {
	n := plugin.Setting{Key: "budget", Type: plugin.TypeInt, Unit: plugin.UnitMoney}
	l := plugin.Setting{Key: "presets", Type: plugin.TypeList, Unit: plugin.UnitMoney}
	if got := ToDisplay(n, json.RawMessage("1500000")); got != 1.5 {
		t.Fatalf("display %v", got)
	}
	if got := fmt.Sprint(ToDisplay(l, json.RawMessage(`["5000000","20000000"]`))); got != "[5 20]" {
		t.Fatalf("display list %v", got)
	}
	if raw, err := FromDisplay(n, json.RawMessage("0.197")); err != nil || string(raw) != "197000" {
		t.Fatalf("store %s %v", raw, err)
	}
	if raw, err := FromDisplay(l, json.RawMessage(`[5, "0.5"]`)); err != nil || string(raw) != `["5000000","500000"]` {
		t.Fatalf("store list %s %v", raw, err)
	}
	if _, err := FromDisplay(n, json.RawMessage(`-1`)); err == nil {
		t.Fatal("negative amount accepted")
	}
	plain := plugin.Setting{Key: "rpm", Type: plugin.TypeInt}
	if raw, _ := FromDisplay(plain, json.RawMessage("60")); string(raw) != "60" || ToDisplay(plain, raw) != 60.0 {
		t.Fatal("plain setting changed")
	}
}
