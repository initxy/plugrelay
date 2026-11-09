package builtin

import (
	"slices"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/pkg/plugin"
)

func TestRegistered(t *testing.T) {
	want := append(Order(), "middleware.redact", "job.requestlog-retention", "provider.openai-compatible")
	for _, id := range want {
		f, ok := plugin.Lookup(id)
		if !ok {
			t.Errorf("%s not registered", id)
			continue
		}
		info := f().Info()
		if info.ID != id {
			t.Errorf("%s: Info().ID = %s", id, info.ID)
		}
		if kind, _, _ := strings.Cut(id, "."); string(info.Kind) != kind {
			t.Errorf("%s: kind %s", id, info.Kind)
		}
	}
}

func TestSettingsDeclared(t *testing.T) {
	for _, id := range plugin.Registered() {
		f, _ := plugin.Lookup(id)
		seen := map[string]bool{}
		for _, s := range f().Info().Settings {
			if s.Key == "" || s.Type == "" || s.Scopes == 0 || s.Merge == "" {
				t.Errorf("%s: incomplete setting %+v", id, s)
			}
			if seen[s.Key] {
				t.Errorf("%s: duplicate setting %s", id, s.Key)
			}
			seen[s.Key] = true
		}
	}
}

func TestForTestMode(t *testing.T) {
	in := []string{"auth.apikey", "sink.otel", "sink.metrics", "sink.vendor"}
	got := ForTestMode(in, "sink.vendor", "sink.otel")
	if !slices.Equal(got, []string{"auth.apikey", "sink.metrics"}) {
		t.Fatal(got)
	}
	if len(in) != 4 {
		t.Fatal("input modified")
	}
}
