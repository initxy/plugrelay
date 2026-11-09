package admin

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/initxy/plugrelay/internal/extplugin"
)

var extpluginManifest = extplugin.Manifest{ID: "demo", Name: "Demo"}

// TestOpenAPICoverage fails when api/openapi.yaml and the registered
// operations drift apart in either direction.
func TestOpenAPICoverage(t *testing.T) {
	data, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	h := New(Deps{})
	have := map[string]bool{}
	for _, r := range h.Routes() {
		have[r.Method+" "+r.Path] = true
	}
	want := map[string]bool{}
	for p, ops := range doc.Paths {
		for m := range ops {
			switch m {
			case "get", "post", "put", "patch", "delete", "head":
				want[strings.ToUpper(m)+" "+p] = true
			}
		}
	}
	if len(want) == 0 {
		t.Fatal("no operations parsed")
	}
	for op := range want {
		if !have[op] {
			t.Errorf("no handler for %s", op)
		}
	}
	for op := range have {
		if !want[op] {
			t.Errorf("handler %s is not in api/openapi.yaml", op)
		}
	}
}
