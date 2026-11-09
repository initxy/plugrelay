package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/secret"
)

func envMap(m map[string]string) LookupEnv {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "missing.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8787" || c.Store.Driver != "sqlite" || c.State.Driver != "memory" {
		t.Fatalf("defaults: %+v", c)
	}
	if !c.ConsoleEnabled() {
		t.Fatal("console off by default")
	}
	if c.Egress.DefaultTimeouts.FirstByte.D() != 2*time.Minute {
		t.Fatal("first byte default")
	}
}

func TestExampleParses(t *testing.T) {
	data, err := os.ReadFile("../../examples/relay.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data, nil); err != nil {
		t.Fatal(err)
	}
}

func TestEnvOverrides(t *testing.T) {
	yml := "master_key_file: /x\nstore: {driver: sqlite, dsn: a.db}\n"
	c, err := Parse([]byte(yml), envMap(map[string]string{
		"PLUGRELAY_LISTEN":                     "0.0.0.0:9000",
		"PLUGRELAY_STORE_DRIVER":               "postgres",
		"PLUGRELAY_STORE_DSN":                  "postgres://x",
		"PLUGRELAY_STATE_DRIVER":               "redis",
		"PLUGRELAY_STATE_URL":                  "redis://h:6379",
		"PLUGRELAY_MASTER_KEY":                 "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"PLUGRELAY_EGRESS_IDLE_TIMEOUT":        "90s",
		"PLUGRELAY_EGRESS_ALLOW_PRIVATE_CIDRS": "10.0.0.0/8, 192.168.0.0/16",
		"PLUGRELAY_CONSOLE_ENABLED":            "false",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "0.0.0.0:9000" || c.Store.Driver != "postgres" || c.State.URL != "redis://h:6379" {
		t.Fatalf("%+v", c)
	}
	if c.MasterKeyFile != "" || c.MasterKey == "" {
		t.Fatal("env master key should replace the file")
	}
	if c.Egress.DefaultTimeouts.Idle.D() != 90*time.Second || len(c.Egress.AllowPrivateCIDRs) != 2 {
		t.Fatalf("egress %+v", c.Egress)
	}
	if c.ConsoleEnabled() {
		t.Fatal("console env override")
	}
	if _, err := c.LoadMasterKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(nil, envMap(map[string]string{"PLUGRELAY_CONSOLE_ENABLED": "maybe"})); err == nil {
		t.Fatal("bad bool accepted")
	}
}

func TestValidationPaths(t *testing.T) {
	yml := `
listen: nope
store: {driver: oracle, dsn: ""}
state: {driver: redis}
log: {level: loud}
egress:
  allow_private_cidrs: [10.0.0.0/33]
  default_timeouts: {first_byte: 0s}
`
	_, err := Parse([]byte(yml), nil)
	if err == nil {
		t.Fatal("accepted")
	}
	want := []string{"listen", "store.driver", "store.dsn", "state.url", "log.level", "egress.allow_private_cidrs[0]", "egress.default_timeouts.first_byte"}
	var paths []string
	for _, e := range err.(interface{ Unwrap() []error }).Unwrap() {
		var fe *FieldError
		if errors.As(e, &fe) {
			paths = append(paths, fe.Path)
		}
	}
	got := strings.Join(paths, ",")
	for _, w := range want {
		if !strings.Contains(","+got+",", ","+w+",") {
			t.Errorf("missing %s in %s", w, got)
		}
	}
	if _, err := Parse([]byte("listne: x\n"), nil); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestTrustedProxies(t *testing.T) {
	c, err := Parse([]byte("trusted_proxies: [10.0.0.0/8, 192.168.1.10, \"::1\", 172.16.5.4/12]\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := c.TrustedProxyPrefixes()
	want := []string{"10.0.0.0/8", "192.168.1.10/32", "::1/128", "172.16.0.0/12"}
	if len(got) != len(want) {
		t.Fatalf("prefixes %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("prefix %d: %s, want %s", i, got[i], want[i])
		}
	}

	if d, _ := Parse(nil, nil); len(d.TrustedProxyPrefixes()) != 0 {
		t.Fatal("trusted proxies must default to none")
	}

	c, err = Parse(nil, envMap(map[string]string{"PLUGRELAY_TRUSTED_PROXIES": "127.0.0.1, 10.0.0.0/8"}))
	if err != nil || len(c.TrustedProxies) != 2 || c.TrustedProxies[1] != "10.0.0.0/8" {
		t.Fatalf("env: %v %v", c, err)
	}

	_, err = Parse([]byte("trusted_proxies: [10.0.0.0/8, not-an-ip, 1.2.3.4/99]\n"), nil)
	var fe *FieldError
	if !errors.As(err, &fe) || !strings.Contains(err.Error(), "trusted_proxies[1]") || !strings.Contains(err.Error(), "trusted_proxies[2]") {
		t.Fatalf("invalid entries: %v", err)
	}
}

// previous_master_keys come from relay.yaml or a comma-separated env list,
// and each must be a valid master key.
func TestPreviousMasterKeys(t *testing.T) {
	k1, _ := secret.GenerateMasterKey()
	k2, _ := secret.GenerateMasterKey()
	c, err := Parse(nil, envMap(map[string]string{"PLUGRELAY_PREVIOUS_MASTER_KEYS": k1 + ", " + k2}))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := c.LoadPreviousMasterKeys()
	if err != nil || len(keys) != 2 || len(keys[1]) != secret.MasterKeySize {
		t.Fatalf("env: %v %v", keys, err)
	}
	if _, err := Parse([]byte("previous_master_keys: [\""+k1+"\", short]\n"), nil); err == nil || !strings.Contains(err.Error(), "previous_master_keys[1]") {
		t.Fatalf("invalid key: %v", err)
	}
}

// websocket.max_session may not outlive a concurrency slot, and a response
// must hold for some output.
func TestWebSocketLimits(t *testing.T) {
	for yml, want := range map[string]string{
		"websocket: {max_session: 31m}":           "websocket.max_session",
		"websocket: {response_reserve_tokens: 0}": "websocket.response_reserve_tokens",
	} {
		if _, err := Parse([]byte(yml), nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", yml, err)
		}
	}
	c, err := Parse([]byte("websocket: {max_session: 30m, response_reserve_tokens: 2048}"),
		envMap(map[string]string{"PLUGRELAY_WEBSOCKET_RESPONSE_RESERVE_TOKENS": "1024"}))
	if err != nil || c.WebSocket.ResponseReserveTokens != 1024 {
		t.Fatalf("%v %+v", err, c)
	}
}
