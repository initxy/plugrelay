package access

import (
	"net/http"
	"slices"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
)

func TestOnRequest(t *testing.T) {
	tests := []struct {
		name  string
		set   testkit.Settings
		ip    string
		ua    string
		model string
		deny  bool
	}{
		{name: "no settings", model: "m"},
		{name: "ip in cidr", set: testkit.Settings{"allowed_ips": []string{"10.0.0.0/8"}}, ip: "10.1.1.1"},
		{name: "ip outside", set: testkit.Settings{"allowed_ips": []string{"10.0.0.0/8"}}, ip: "11.1.1.1", deny: true},
		{name: "ip missing", set: testkit.Settings{"allowed_ips": []string{"10.0.0.0/8"}}, deny: true},
		{name: "client ok", set: testkit.Settings{"allowed_clients": []string{"codex"}}, ua: "codex_cli_rs/0.50"},
		{name: "client denied", set: testkit.Settings{"allowed_clients": []string{"codex"}}, ua: "curl/8", deny: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testkit.NewCall(ID, testkit.CallOpts{Settings: tt.set, ClientIP: tt.ip, Model: tt.model,
				Header: http.Header{"User-Agent": {tt.ua}}})
			r, err := New().OnRequest(c)
			if err != nil {
				t.Fatal(err)
			}
			if (r != nil) != tt.deny {
				t.Fatalf("reply = %+v, want deny=%v", r, tt.deny)
			}
		})
	}
}

func TestCheckNative(t *testing.T) {
	tests := []struct {
		name string
		set  testkit.Settings
		ip   string
		ua   string
		deny bool
	}{
		{name: "no grant", deny: true},
		{name: "empty grant", set: testkit.Settings{SettingNativeUpstreams: []string{}}, deny: true},
		{name: "other upstream", set: testkit.Settings{SettingNativeUpstreams: []string{"anthropic"}}, deny: true},
		{name: "granted", set: testkit.Settings{SettingNativeUpstreams: []string{"anthropic", "openai"}}},
		{name: "wildcard", set: testkit.Settings{SettingNativeUpstreams: []string{"*"}}},
		{name: "ip still applies", set: testkit.Settings{SettingNativeUpstreams: []string{"*"}, "allowed_ips": []string{"10.0.0.0/8"}},
			ip: "11.0.0.1", deny: true},
		{name: "client still applies", set: testkit.Settings{SettingNativeUpstreams: []string{"*"}, "allowed_clients": []string{"codex"}},
			ua: "curl/8", deny: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testkit.NewCall(ID, testkit.CallOpts{Settings: tt.set})
			gw := CheckNative(c.Setting, tt.ip, http.Header{"User-Agent": {tt.ua}}, "openai")
			if (gw != nil) != tt.deny {
				t.Fatalf("err = %v, want deny=%v", gw, tt.deny)
			}
		})
	}
}

// CheckClient applies the group's source-address and client lists.
func TestCheckClient(t *testing.T) {
	setting := testkit.Settings{"allowed_ips": []string{"10.1.0.0/16"}, "allowed_clients": []string{"codex"}}.Get
	if gw := CheckClient(setting, "10.1.2.3", ua("codex_cli_rs/0.50")); gw != nil {
		t.Errorf("allowed: %v", gw)
	}
	if gw := CheckClient(setting, "10.2.0.1", ua("codex_cli_rs/0.50")); gw == nil || gw.Kind != ir.ErrPermission {
		t.Errorf("address: %v", gw)
	}
	if gw := CheckClient(setting, "10.1.2.3", ua("curl/8")); gw == nil {
		t.Errorf("client: %v", gw)
	}
	// Clients are client ids: a User-Agent prefix names nothing, and
	// "other" admits whatever no rule recognises.
	if gw := CheckClient(testkit.Settings{"allowed_clients": []string{"codex_cli"}}.Get, "", ua("codex_cli_rs/0.50")); gw == nil {
		t.Error("a User-Agent prefix admitted a client")
	}
	other := testkit.Settings{"allowed_clients": []string{"other"}}.Get
	if CheckClient(other, "", ua("in-house-tool/1")) != nil || CheckClient(other, "", ua("codex_cli_rs/0.50")) == nil {
		t.Error("other")
	}
	// The Originator header names Codex too.
	if gw := CheckClient(setting, "10.1.2.3", http.Header{"Originator": {"codex_vscode"}}); gw != nil {
		t.Errorf("by header: %v", gw)
	}
}

// allowed_clients only takes client ids.
func TestAllowedClientsAreClientIDs(t *testing.T) {
	for _, s := range New().Info().Settings {
		if s.Key == "allowed_clients" {
			if !slices.Contains(s.Enum, "codex") || !slices.Contains(s.Enum, "other") || s.EnumLabels["claude-code"].EN != "Claude Code" {
				t.Fatalf("enum %v labels %v", s.Enum, s.EnumLabels)
			}
			return
		}
	}
	t.Fatal("no allowed_clients setting")
}

func ua(s string) http.Header { return http.Header{"User-Agent": {s}} }
