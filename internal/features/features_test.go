package features_test

import (
	"strings"
	"testing"

	"github.com/initxy/plugrelay/internal/features"
	"github.com/initxy/plugrelay/pkg/plugin"
	_ "github.com/initxy/plugrelay/plugins/builtin"
)

func setting(module, key string) (plugin.Setting, bool) {
	f, ok := plugin.Lookup(module)
	if !ok {
		return plugin.Setting{}, false
	}
	for _, s := range f().Info().Settings {
		if s.Key == key {
			return s, true
		}
	}
	return plugin.Setting{}, false
}

func complete(t plugin.Text) bool {
	return strings.TrimSpace(t.EN) != "" && strings.TrimSpace(t.ZH) != ""
}

func TestRegistry(t *testing.T) {
	seen, pages := map[string]bool{}, map[string]bool{}
	for _, f := range features.All() {
		if seen[f.ID] {
			t.Errorf("duplicate feature %s", f.ID)
		}
		seen[f.ID] = true
		if !complete(f.Name) || !complete(f.Description) {
			t.Errorf("%s: name and description need en and zh", f.ID)
		}
		if f.AlwaysOn && len(f.Modules) > 0 {
			t.Errorf("%s: an always-on feature has no modules of its own (they are core)", f.ID)
		}
		for _, m := range f.Modules {
			if _, ok := plugin.Lookup(m); !ok {
				t.Errorf("%s: module %s is not registered", f.ID, m)
			}
			if features.IsCore(m) {
				t.Errorf("%s: module %s is core", f.ID, m)
			}
			if g, _ := features.Of(m); g.ID != f.ID {
				t.Errorf("module %s is in %s and %s", m, g.ID, f.ID)
			}
		}
		for _, r := range f.Settings {
			s, ok := setting(r.Module, r.Key)
			if !ok {
				t.Errorf("%s: no setting %s/%s", f.ID, r.Module, r.Key)
				continue
			}
			if s.Scopes&plugin.Global == 0 {
				t.Errorf("%s: %s/%s cannot be set globally", f.ID, r.Module, r.Key)
			}
			if r.Module != "middleware.access" && !features.IsCore(r.Module) {
				if g, _ := features.Of(r.Module); g.ID != f.ID {
					t.Errorf("%s shows %s/%s of another feature", f.ID, r.Module, r.Key)
				}
			}
		}
		for _, p := range f.Pages {
			if pages[p.ID] || !strings.HasPrefix(p.ID, strings.SplitN(f.ID, "_", 2)[0]) {
				t.Errorf("%s: page id %s duplicate or not under the feature", f.ID, p.ID)
			}
			pages[p.ID] = true
		}
	}
	for _, id := range features.Core {
		if _, ok := plugin.Lookup(id); !ok {
			t.Errorf("core module %s is not registered", id)
		}
	}
}

// Every compiled-in plugin is a core module, a feature module or a
// provider: none would be left without a switch.
func TestEveryBuiltinPlaced(t *testing.T) {
	for _, id := range plugin.Registered() {
		f, _ := plugin.Lookup(id)
		if f().Info().Kind == plugin.KindProvider {
			continue
		}
		if !features.Builtin(id) {
			t.Errorf("%s is neither core nor part of a feature", id)
		}
	}
}

// Every built-in setting is labelled for people in both languages, enum
// values included.
func TestSettingsLabelled(t *testing.T) {
	for _, id := range plugin.Registered() {
		f, _ := plugin.Lookup(id)
		for _, s := range f().Info().Settings {
			if !complete(s.Label) || !complete(s.Help) {
				t.Errorf("%s/%s: label and help need en and zh", id, s.Key)
			}
			for _, v := range s.Enum {
				if !complete(s.EnumLabels[v]) {
					t.Errorf("%s/%s: value %s has no label", id, s.Key, v)
				}
			}
			if s.Unit == plugin.UnitMoney && s.Type != plugin.TypeInt && s.Type != plugin.TypeList {
				t.Errorf("%s/%s: money must be an int or a list", id, s.Key)
			}
		}
	}
}

func TestState(t *testing.T) {
	s := features.State{"limits": false, "billing": true, "routing": false}
	if s.On("limits") || !s.On("billing") || !s.On("routing") || !s.On("metrics") || s.On("sso") || s.On("nope") {
		t.Fatal(s)
	}
}
