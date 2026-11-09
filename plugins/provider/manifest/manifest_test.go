package manifest

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
)

const good = `
id: example
kind: provider
name: Example
homepage: https://example.com
variables:
  host: {default: api.example.com}
auth: {header: Authorization}
owned_headers: [X-Org]
extra_headers: {X-Client: relay}
capabilities:
  cache: implicit
  max_tokens_field: max_tokens
endpoints:
  - protocol: openai-chat
    base_url: https://{host}/v1
    interpreter: openai-compatible
    capabilities:
      reasoning: reasoning_content
  - id: msgs
    protocol: anthropic
    base_url: https://{host}/anthropic
    interpreter: anthropic-compatible
    unsupported_betas: [context-1m-2025-08-07]
    auth: {header: x-api-key}
errors:
  - when: {status: 402}
    verdict: {cause: quota, credential: cool_until_reset, retry: next}
  - when: {status: 400, message_contains: "maximum context length"}
    verdict: {cause: request, signal: context_too_long}
  - when: {status: 429}
    verdict: {cause: rate_limit, cooldown: 45s, model_scope: true}
models: {list: /models}
pricing:
  currency: USD
  models:
    m1: {input: 1, cache_read: 0.1, output: 2}
quota:
  path: /balance
  remaining: data.left
  limit: data.total
`

func TestParseGood(t *testing.T) {
	m, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if m.Endpoints[0].ID != "openai-chat" {
		t.Errorf("endpoint id default = %q", m.Endpoints[0].ID)
	}
	if m.Endpoints[0].Issuer != "" {
		t.Errorf("templated base_url must leave the issuer to the provider, got %q", m.Endpoints[0].Issuer)
	}
	if m.Auth.Scheme != SchemeBearer || m.Endpoints[1].Auth.Scheme != SchemeRaw {
		t.Errorf("scheme defaults = %q, %q", m.Auth.Scheme, m.Endpoints[1].Auth.Scheme)
	}
	if got := time.Duration(m.Errors[2].Verdict.Cooldown); got != 45*time.Second {
		t.Errorf("cooldown = %v", got)
	}
	if m.ProviderID() != "provider.openai-compatible" {
		t.Errorf("provider id = %q", m.ProviderID())
	}
	base := ir.Capabilities{Cache: ir.CacheNone, FunctionTools: true, MaxTokensField: "max_completion_tokens"}
	c := m.EndpointCaps(&m.Endpoints[0], base)
	if c.Cache != ir.CacheImplicit || !c.Reasoning || c.ReasoningStyle != "reasoning_content" ||
		c.MaxTokensField != "max_tokens" || !c.FunctionTools {
		t.Errorf("caps = %+v", c)
	}
	c = m.EndpointCaps(&m.Endpoints[1], base)
	if c.Reasoning {
		t.Errorf("reasoning leaked across endpoints: %+v", c)
	}
}

func TestValidationPaths(t *testing.T) {
	cases := []struct {
		name, from, to, path string
	}{
		{"scheme", "https://{host}/v1", "ftp://{host}/v1", "endpoints[0].base_url"},
		{"no host", "https://{host}/anthropic", "https:///anthropic", "endpoints[1].base_url"},
		{"trailing slash", "https://{host}/v1", "https://{host}/v1/", "endpoints[0].base_url"},
		{"undeclared var", "https://{host}/v1", "https://{region}/v1", "endpoints[0].base_url"},
		{"kind", "kind: provider", "kind: middleware", "kind"},
		{"id", "id: example", "id: Example!", "id"},
		{"protocol", "protocol: openai-chat", "protocol: grpc", "endpoints[0].protocol"},
		{"interpreter mismatch", "interpreter: anthropic-compatible", "interpreter: openai-compatible", "endpoints[1].interpreter"},
		{"betas on openai", "    interpreter: openai-compatible\n", "    interpreter: openai-compatible\n    unsupported_betas: [x]\n", "endpoints[0].unsupported_betas"},
		{"status", "status: 402", "status: 42", "errors[0].when.status"},
		{"cause", "cause: quota", "cause: oops", "errors[0].verdict.cause"},
		{"credential", "credential: cool_until_reset", "credential: nuke", "errors[0].verdict.credential"},
		{"signal", "signal: context_too_long", "signal: whatever", "errors[1].verdict.signal"},
		{"models", "models: {list: /models}", "models: {list: models}", "models.list"},
		{"price", "input: 1,", "input: -1,", "pricing.models.m1.input"},
		{"currency", "currency: USD", "currency: usd", "pricing.currency"},
		{"quota", "  limit: data.total\n", "", "quota"},
		{"homepage", "homepage: https://example.com", "homepage: example.com", "homepage"},
		{"header name", "owned_headers: [X-Org]", "owned_headers: [\"X Org\"]", "owned_headers[0]"},
		{"reserved var", "  host: {default: api.example.com}", "  host: {default: api.example.com}\n  model: {}", "variables.model"},
		{"key_optional on an endpoint", "auth: {header: x-api-key}", "auth: {header: x-api-key, key_optional: true}", "endpoints[1].auth.key_optional"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(good, tc.from, tc.to, 1)
			if src == good {
				t.Fatalf("replacement %q not found", tc.from)
			}
			_, err := Parse([]byte(src))
			var es Errors
			if !errors.As(err, &es) {
				t.Fatalf("want validation errors, got %v", err)
			}
			if !es.Has(tc.path) {
				t.Fatalf("want error at %s, got %v", tc.path, es)
			}
			if !strings.Contains(err.Error(), tc.path+": ") {
				t.Errorf("message lacks path: %v", err)
			}
		})
	}
}

// Hard rules 16 and 17 bind declarative rules as well as built-in ones.
func TestValidationHardRules(t *testing.T) {
	cases := []struct {
		name, verdict, path string
	}{
		{"content retried", "{cause: content, retry: next}", "errors[0].verdict.retry"},
		{"ban retried", "{cause: banned, retry: next}", "errors[0].verdict.retry"},
		{"ban kept", "{cause: banned, credential: keep}", "errors[0].verdict.credential"},
		{"ban cooled", "{cause: banned, credential: cooldown}", "errors[0].verdict.credential"},
		{"uncertain kept", "{cause: unknown, credential: keep}", "errors[0].verdict.credential"},
		{"request touches credential", "{cause: request, credential: cooldown}", "errors[0].verdict.credential"},
		{"transient quarantines", "{cause: transient, credential: quarantine}", "errors[0].verdict.credential"},
		{"downgrade without edits", "{cause: request, retry: downgrade}", "errors[0].verdict.downgrade"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(good, "{cause: quota, credential: cool_until_reset, retry: next}", tc.verdict, 1)
			_, err := Parse([]byte(src))
			var es Errors
			if !errors.As(err, &es) || !es.Has(tc.path) {
				t.Fatalf("want error at %s, got %v", tc.path, err)
			}
		})
	}
	ok := strings.Replace(good, "{cause: quota, credential: cool_until_reset, retry: next}", "{cause: banned, credential: manual}", 1)
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatalf("banned/manual rejected: %v", err)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	for _, src := range []string{
		strings.Replace(good, "name: Example", "nmae: Example", 1),
		strings.Replace(good, "m1: {input: 1,", "m1: {inptu: 1,", 1),
		strings.Replace(good, "cache: implicit", "cache: implicit\n  bogus: true", 1),
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("unknown field accepted")
		}
	}
	if _, err := Parse(nil); err == nil {
		t.Error("empty manifest accepted")
	}
}

func TestLoadResolvesPricingFile(t *testing.T) {
	src := strings.Replace(good, "pricing:\n  currency: USD\n  models:\n    m1: {input: 1, cache_read: 0.1, output: 2}\n",
		"pricing: ./example.prices.yaml\n", 1)
	fsys := fstest.MapFS{
		"dir/example.yaml":        {Data: []byte(src)},
		"dir/example.prices.yaml": {Data: []byte("currency: CNY\nmodels:\n  m1: {input: 2, output: 8, cache_write_1h: 4}\n")},
	}
	m, err := Load(fsys, "dir/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if p := m.Pricing.Models["m1"]; p.Input != 2 || p.CacheWrite1h != 4 || m.Pricing.Currency != "CNY" {
		t.Errorf("pricing = %+v", m.Pricing)
	}
	fsys["dir/example.prices.yaml"] = &fstest.MapFile{Data: []byte("models:\n  m1: {input: 2, output: 8}\n")}
	if _, err := Load(fsys, "dir/example.yaml"); err == nil || !strings.Contains(err.Error(), "pricing.models.m1.currency") {
		t.Errorf("missing currency: %v", err)
	}
	delete(fsys, "dir/example.prices.yaml")
	if _, err := Load(fsys, "dir/example.yaml"); err == nil {
		t.Error("missing pricing file accepted")
	}
}

func TestExpand(t *testing.T) {
	got := Expand("https://{host}/openai/deployments/{model}?v={v}", map[string]string{"host": "h", "v": "1"})
	if got != "https://h/openai/deployments/{model}?v=1" {
		t.Errorf("Expand = %q", got)
	}
}

// The JSON Schema and the Go types describe the same fields.
func TestSchemaMatchesTypes(t *testing.T) {
	raw, err := os.ReadFile("../../../manifests/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Schema     string                     `json:"$schema"`
		Properties map[string]json.RawMessage `json:"properties"`
		Defs       map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Schema != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("$schema = %q", s.Schema)
	}
	check := func(name string, props map[string]json.RawMessage, typ any) {
		want := yamlFields(reflect.TypeOf(typ))
		var got []string
		for k := range props {
			got = append(got, k)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: schema %v, Go %v", name, got, want)
		}
	}
	check("manifest", s.Properties, Manifest{})
	check("endpoint", s.Defs["endpoint"].Properties, Endpoint{})
	check("capabilities", s.Defs["capabilities"].Properties, Capabilities{})
	check("auth", s.Defs["auth"].Properties, Auth{})
	check("model", s.Defs["model"].Properties, ModelInfo{})
	check("price", s.Defs["price"].Properties, Price{})
	check("pricing", s.Defs["pricing"].Properties, Pricing{})
}

func yamlFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		tag := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]
		if tag != "" && tag != "-" {
			out = append(out, tag)
		}
	}
	sort.Strings(out)
	return out
}
