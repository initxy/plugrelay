package extplugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../sdk/plugin.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	return s
}

func yamlKeys(t reflect.Type) []string {
	var out []string
	for f := range t.Fields() {
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag != "" && tag != "-" {
			out = append(out, tag)
		}
	}
	sort.Strings(out)
	return out
}

func schemaKeys(node any) []string {
	props, _ := node.(map[string]any)["properties"].(map[string]any)
	var out []string
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func prop(node any, path ...string) any {
	for _, p := range path {
		node = node.(map[string]any)["properties"].(map[string]any)[p]
	}
	return node
}

// TestSchemaMatchesManifest keeps sdk/plugin.schema.json in step with the
// Go loader's fields and method list.
func TestSchemaMatchesManifest(t *testing.T) {
	s := loadSchema(t)
	item := func(n any) any { return n.(map[string]any)["items"] }
	cases := []struct {
		name   string
		typ    reflect.Type
		schema any
	}{
		{"manifest", reflect.TypeFor[Manifest](), s},
		{"timeouts", reflect.TypeFor[Timeouts](), prop(s, "timeouts")},
		{"settings", reflect.TypeFor[SettingSpec](), item(prop(s, "settings"))},
		{"permissions", reflect.TypeFor[Permissions](), prop(s, "permissions")},
		{"provider", reflect.TypeFor[ProviderSpec](), prop(s, "provider")},
		{"job", reflect.TypeFor[JobSpec](), prop(s, "job")},
		{"admin", reflect.TypeFor[AdminSpec](), prop(s, "admin")},
		{"admin.routes", reflect.TypeFor[RouteSpec](), item(prop(s, "admin", "routes"))},
	}
	for _, c := range cases {
		if got, want := schemaKeys(c.schema), yamlKeys(c.typ); !slices.Equal(got, want) {
			t.Errorf("%s: schema properties %v, loader fields %v", c.name, got, want)
		}
	}
	var points []string
	for _, v := range s["$defs"].(map[string]any)["point"].(map[string]any)["enum"].([]any) {
		points = append(points, v.(string))
	}
	sort.Strings(points)
	var methods []string
	for m := range Methods {
		methods = append(methods, m)
	}
	sort.Strings(methods)
	if !slices.Equal(points, methods) {
		t.Errorf("schema points %v, loader methods %v", points, methods)
	}
}

// TestExampleManifests validates every example's plugin.yaml with the loader.
func TestExampleManifests(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(examplesDir, "*", ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) < 2 {
		t.Fatalf("found %d example manifests, want at least 2", len(dirs))
	}
	for _, p := range dirs {
		t.Run(filepath.Base(filepath.Dir(p)), func(t *testing.T) {
			m, err := LoadManifest(p)
			if err != nil {
				t.Fatal(err)
			}
			if len(m.Kinds) == 0 || len(m.Points) == 0 {
				t.Fatalf("manifest %+v", m)
			}
		})
	}
}
