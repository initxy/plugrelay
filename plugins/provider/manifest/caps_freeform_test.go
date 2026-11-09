package manifest

import (
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
)

// Freeform tools are an OpenAI-only extension: a manifest that does not
// declare them gets them off, whatever the codec's default, so a
// user-written OpenAI-compatible upstream never receives Codex's custom
// apply_patch tool verbatim.
func TestFreeformToolsOffUnlessDeclared(t *testing.T) {
	codec := ir.Capabilities{FreeformTools: true, FunctionTools: true}
	yes := true
	cases := []struct {
		name string
		m    Manifest
		want bool
	}{
		{"undeclared", Manifest{}, false},
		{"declared on the manifest", Manifest{Capabilities: Capabilities{FreeformTools: &yes}}, true},
	}
	for _, tc := range cases {
		if got := tc.m.EndpointCaps(&Endpoint{}, codec).FreeformTools; got != tc.want {
			t.Errorf("%s: freeform %v, want %v", tc.name, got, tc.want)
		}
	}
	ep := &Endpoint{Capabilities: Capabilities{FreeformTools: &yes}}
	if !(&Manifest{}).EndpointCaps(ep, codec).FreeformTools {
		t.Error("declared on the endpoint: want on")
	}
	if !(&Manifest{}).EndpointCaps(&Endpoint{}, codec).FunctionTools {
		t.Error("other codec defaults must stay")
	}
}
