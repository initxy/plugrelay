// Package manifest defines the declarative upstream manifest: the YAML file
// that describes an upstream's protocol endpoints, authentication, owned
// headers, capabilities, error-to-verdict rules, model catalogue, prices and
// quota probe. A manifest is interpreted by one of the generic provider
// plugins; it carries no code.
//
// Base URL convention: an endpoint's base_url is what the official SDK of
// that protocol calls its base URL. OpenAI-style endpoints include the
// version prefix ("https://api.openai.com/v1"); Anthropic and Gemini
// endpoints do not ("https://api.anthropic.com"), because their codecs'
// upstream paths start with "/v1" and "/v1beta". models.list and quota.path
// are relative to the same base URL.
//
// Placeholders: "{name}" in base_url, path and query values refers to a
// declared variable, filled from the upstream instance's config or the
// variable's default. "{model}" in path is left for the core, which
// substitutes the upstream model name.
package manifest

import (
	"fmt"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"gopkg.in/yaml.v3"
)

// Categories are the accepted values of Manifest.Category.
var Categories = []string{"vendor", "cloud", "inference", "gateway", "self_hosted", "custom"}

// Kind is the only accepted value of Manifest.Kind.
const Kind = "provider"

// Protocols are the codec names an endpoint may speak.
var Protocols = []string{"anthropic", "openai-chat", "openai-responses", "openai-completions", "gemini",
	"openai-embeddings", "openai-images", "openai-audio", "gemini-embeddings", "rerank", "cohere-rerank", "tei-rerank"}

// Interpreters maps each interpreter name to the protocols it can drive.
// The interpreter of an endpoint names the provider plugin
// "provider.<interpreter>" that executes the manifest.
var Interpreters = map[string][]string{
	"openai-compatible":    {"openai-chat", "openai-responses", "openai-completions", "openai-embeddings", "openai-images", "openai-audio", "rerank", "cohere-rerank", "tei-rerank"},
	"anthropic-compatible": {"anthropic"},
	"gemini":               {"gemini", "gemini-embeddings"},
	"openai":               {"openai-chat", "openai-responses", "openai-completions", "openai-embeddings", "openai-images", "openai-audio"},
	"anthropic":            {"anthropic"},
	// Cloud platforms whose auth a manifest cannot express: request
	// signing (Bedrock) and service account tokens (Vertex AI).
	"bedrock": {"anthropic"},
	"vertex":  {"anthropic", "gemini"},
}

// Manifest is one upstream description.
type Manifest struct {
	ID   string `yaml:"id" json:"id"`
	Kind string `yaml:"kind" json:"kind"`
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// NameZh is the name in the Chinese console; Name otherwise.
	NameZh string `yaml:"name_zh,omitempty" json:"name_zh,omitempty"`
	// Category groups the manifest in the console's add-upstream gallery.
	Category string `yaml:"category,omitempty" json:"category,omitempty"`
	Homepage string `yaml:"homepage,omitempty" json:"homepage,omitempty"`
	// Interpreter is the default for endpoints that do not set their own.
	Interpreter string `yaml:"interpreter,omitempty" json:"interpreter,omitempty"`
	// Variables are the per-instance values an upstream config supplies
	// (resource name, host, api version).
	Variables map[string]Variable `yaml:"variables,omitempty" json:"variables,omitempty"`
	Endpoints []Endpoint          `yaml:"endpoints" json:"endpoints"`
	Auth      Auth                `yaml:"auth" json:"auth"`
	// OwnedHeaders are request headers the provider controls; client
	// headers of the same names never reach the upstream. The auth header,
	// extra_headers and credential_headers are owned implicitly.
	OwnedHeaders []string `yaml:"owned_headers,omitempty" json:"owned_headers,omitempty"`
	// ExtraHeaders are static headers set on every request.
	ExtraHeaders map[string]string `yaml:"extra_headers,omitempty" json:"extra_headers,omitempty"`
	// CredentialHeaders maps a header name to a Credential.Meta key whose
	// value is sent in it (organization, project).
	CredentialHeaders map[string]string `yaml:"credential_headers,omitempty" json:"credential_headers,omitempty"`
	// Capabilities are shared defaults merged over each endpoint codec's.
	Capabilities Capabilities `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	// Errors are checked in order before the built-in rules.
	Errors  []ErrorRule `yaml:"errors,omitempty" json:"errors,omitempty"`
	Models  Models      `yaml:"models,omitempty" json:"models,omitempty"`
	Pricing *Pricing    `yaml:"pricing,omitempty" json:"pricing,omitempty"`
	Quota   *QuotaProbe `yaml:"quota,omitempty" json:"quota,omitempty"`
}

// Variable is a per-instance placeholder.
type Variable struct {
	Default       string `yaml:"default,omitempty" json:"default,omitempty"`
	Required      bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Description   string `yaml:"description,omitempty" json:"description,omitempty"`
	DescriptionZh string `yaml:"description_zh,omitempty" json:"description_zh,omitempty"`
}

// Endpoint is one protocol entry.
type Endpoint struct {
	// ID defaults to the protocol name.
	ID          string `yaml:"id,omitempty" json:"id,omitempty"`
	Protocol    string `yaml:"protocol" json:"protocol"`
	BaseURL     string `yaml:"base_url" json:"base_url"`
	Interpreter string `yaml:"interpreter,omitempty" json:"interpreter,omitempty"`
	// Path overrides the codec's upstream path; "{model}" is substituted.
	Path  string            `yaml:"path,omitempty" json:"path,omitempty"`
	Query map[string]string `yaml:"query,omitempty" json:"query,omitempty"`
	// UnsupportedBetas are anthropic-beta values removed before sending.
	UnsupportedBetas []string     `yaml:"unsupported_betas,omitempty" json:"unsupported_betas,omitempty"`
	Capabilities     Capabilities `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	// Issuer defaults to the manifest id, or to a per-upstream group when
	// base_url is templated from a variable.
	Issuer string `yaml:"issuer,omitempty" json:"issuer,omitempty"`
	// Auth overrides the manifest's auth for this endpoint.
	Auth *Auth `yaml:"auth,omitempty" json:"auth,omitempty"`
}

// Auth schemes.
const (
	SchemeBearer = "Bearer"
	SchemeRaw    = "raw"
)

// Auth says where the credential secret goes.
type Auth struct {
	Header string `yaml:"header,omitempty" json:"header,omitempty"`
	// Scheme is "Bearer" (value "Bearer <secret>") or "raw" (the secret
	// alone). Defaults to Bearer for Authorization and raw otherwise.
	Scheme string `yaml:"scheme,omitempty" json:"scheme,omitempty"`
	// QueryParam, when set, carries the secret in the query string instead.
	QueryParam string `yaml:"query_param,omitempty" json:"query_param,omitempty"`
	// KeyOptional marks a server that may run without a key (a self-hosted
	// server, a gateway without a master key): an upstream of it with no
	// credentials is routed with no auth. Only on the manifest's own auth.
	KeyOptional bool `yaml:"key_optional,omitempty" json:"key_optional,omitempty"`
}

// Credential actions accepted in manifests. CoolUntilReset is a cooldown
// whose end comes from the upstream's reset headers.
const (
	CredKeep           = "keep"
	CredCooldown       = "cooldown"
	CredCoolUntilReset = "cool_until_reset"
	CredRefresh        = "refresh"
	CredQuarantine     = "quarantine"
	CredManual         = "manual"
)

// ErrorRule maps an upstream failure to a verdict.
type ErrorRule struct {
	When    Match       `yaml:"when" json:"when"`
	Verdict RuleVerdict `yaml:"verdict" json:"verdict"`
}

// Match conditions; every set condition must hold.
type Match struct {
	Status   int   `yaml:"status,omitempty" json:"status,omitempty"`
	StatusIn []int `yaml:"status_in,omitempty" json:"status_in,omitempty"`
	// MessageContains is matched case-insensitively against the upstream
	// message and body.
	MessageContains string `yaml:"message_contains,omitempty" json:"message_contains,omitempty"`
	// Code matches the upstream's error code or type, case-insensitively.
	Code          string       `yaml:"code,omitempty" json:"code,omitempty"`
	HeaderPresent string       `yaml:"header_present,omitempty" json:"header_present,omitempty"`
	Signal        ir.ErrorKind `yaml:"signal,omitempty" json:"signal,omitempty"`
}

// IsZero reports whether no condition is set.
func (m Match) IsZero() bool {
	return m.Status == 0 && len(m.StatusIn) == 0 && m.MessageContains == "" &&
		m.Code == "" && m.HeaderPresent == "" && m.Signal == ""
}

// RuleVerdict is the declarative form of plugin.Verdict. Empty retry and
// credential take the cause's default.
type RuleVerdict struct {
	Cause      string   `yaml:"cause" json:"cause"`
	Retry      string   `yaml:"retry,omitempty" json:"retry,omitempty"`
	Credential string   `yaml:"credential,omitempty" json:"credential,omitempty"`
	Cooldown   Duration `yaml:"cooldown,omitempty" json:"cooldown,omitempty"`
	ModelScope bool     `yaml:"model_scope,omitempty" json:"model_scope,omitempty"`
	// Signal is the client-facing error kind.
	Signal ir.ErrorKind `yaml:"signal,omitempty" json:"signal,omitempty"`
	// Downgrade lists gjson path → new value edits for retry: downgrade.
	Downgrade map[string]any `yaml:"downgrade,omitempty" json:"downgrade,omitempty"`
}

// Models is the model catalogue: listed live from the upstream or static.
type Models struct {
	// List is the listing path relative to the endpoint's base URL.
	List string `yaml:"list,omitempty" json:"list,omitempty"`
	// Endpoint is the endpoint id used for listing; defaults to the first.
	Endpoint string      `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Static   []ModelInfo `yaml:"static,omitempty" json:"static,omitempty"`
}

// ModelInfo is the YAML form of ir.ModelInfo.
type ModelInfo struct {
	ID            string   `yaml:"id" json:"id"`
	DisplayName   string   `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	OwnedBy       string   `yaml:"owned_by,omitempty" json:"owned_by,omitempty"`
	ContextWindow int      `yaml:"context_window,omitempty" json:"context_window,omitempty"`
	MaxOutput     int      `yaml:"max_output,omitempty" json:"max_output,omitempty"`
	Tools         bool     `yaml:"tools,omitempty" json:"tools,omitempty"`
	Vision        bool     `yaml:"vision,omitempty" json:"vision,omitempty"`
	Reasoning     bool     `yaml:"reasoning,omitempty" json:"reasoning,omitempty"`
	Efforts       []string `yaml:"efforts,omitempty" json:"efforts,omitempty"`
}

// IR converts to ir.ModelInfo.
func (m ModelInfo) IR() ir.ModelInfo {
	return ir.ModelInfo{
		ID: m.ID, DisplayName: m.DisplayName, OwnedBy: m.OwnedBy,
		ContextWindow: m.ContextWindow, MaxOutput: m.MaxOutput,
		Tools: m.Tools, Vision: m.Vision, Reasoning: m.Reasoning, Efforts: m.Efforts,
	}
}

// Pricing is per-model prices per million tokens. In YAML it is either a
// file path relative to the manifest (resolved at load time into the inline
// form) or the inline mapping.
type Pricing struct {
	File string `yaml:"-" json:"-"`
	// Currency is the default for prices that do not set one (ISO 4217).
	Currency string           `yaml:"currency,omitempty" json:"currency,omitempty"`
	Models   map[string]Price `yaml:"models" json:"models"`
}

// UnmarshalYAML accepts a file path or the inline form.
func (p *Pricing) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		p.File = n.Value
		return nil
	}
	type plain Pricing
	return strictDecode(n, (*plain)(p))
}

// MarshalYAML writes the file path when the prices were not resolved.
func (p Pricing) MarshalYAML() (any, error) {
	if p.File != "" && p.Models == nil {
		return p.File, nil
	}
	type plain Pricing
	return plain(p), nil
}

// Price is one model's prices per million tokens.
type Price struct {
	Input        float64 `yaml:"input" json:"input"`
	CacheRead    float64 `yaml:"cache_read,omitempty" json:"cache_read,omitempty"`
	CacheWrite5m float64 `yaml:"cache_write_5m,omitempty" json:"cache_write_5m,omitempty"`
	CacheWrite1h float64 `yaml:"cache_write_1h,omitempty" json:"cache_write_1h,omitempty"`
	Output       float64 `yaml:"output" json:"output"`
	Currency     string  `yaml:"currency,omitempty" json:"currency,omitempty"`
}

// QuotaProbe reads a credential's remaining allowance over HTTP. Values are
// gjson paths into the response.
type QuotaProbe struct {
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Method   string `yaml:"method,omitempty" json:"method,omitempty"`
	Path     string `yaml:"path" json:"path"`
	// Remaining and Limit give the fraction Remaining/Limit; or
	// RemainingFraction gives it directly (0..1).
	Remaining         string `yaml:"remaining,omitempty" json:"remaining,omitempty"`
	Limit             string `yaml:"limit,omitempty" json:"limit,omitempty"`
	RemainingFraction string `yaml:"remaining_fraction,omitempty" json:"remaining_fraction,omitempty"`
	// Reset is unix seconds or RFC 3339.
	Reset string `yaml:"reset,omitempty" json:"reset,omitempty"`
	// ResetPeriod, read when Reset gives no time, names a calendar period
	// ("daily", "weekly", "monthly"); the reset is the next 00:00 UTC that
	// starts one (weeks start on Monday).
	ResetPeriod string `yaml:"reset_period,omitempty" json:"reset_period,omitempty"`
}

// Duration is a time.Duration written as "30s" in YAML.
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML writes the duration string.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// EndpointInterpreter returns the effective interpreter of e.
func (m *Manifest) EndpointInterpreter(e *Endpoint) string {
	if e.Interpreter != "" {
		return e.Interpreter
	}
	return m.Interpreter
}

// ProviderID is the plugin id that runs this manifest: the top-level
// interpreter, or the first endpoint's.
func (m *Manifest) ProviderID() string {
	in := m.Interpreter
	if in == "" && len(m.Endpoints) > 0 {
		in = m.Endpoints[0].Interpreter
	}
	return "provider." + in
}

// Endpoint returns the endpoint with the given id.
func (m *Manifest) Endpoint(id string) (*Endpoint, bool) {
	for i := range m.Endpoints {
		if m.Endpoints[i].ID == id {
			return &m.Endpoints[i], true
		}
	}
	return nil, false
}

// EndpointAuth returns the effective auth of e.
func (m *Manifest) EndpointAuth(e *Endpoint) Auth {
	if e.Auth != nil {
		return *e.Auth
	}
	return m.Auth
}

// Expand substitutes "{name}" placeholders from vars, leaving names not in
// vars (such as "{model}") untouched.
func Expand(s string, vars map[string]string) string {
	if !strings.Contains(s, "{") {
		return s
	}
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		name := s[i+1 : i+j]
		b.WriteString(s[:i])
		if v, ok := vars[name]; ok {
			b.WriteString(v)
		} else {
			b.WriteString(s[i : i+j+1])
		}
		s = s[i+j+1:]
	}
}

// placeholders lists the "{name}" references in s.
func placeholders(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return out
		}
		out = append(out, s[i+1:i+j])
		s = s[i+j+1:]
	}
}
