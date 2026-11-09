package manifest

import (
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
)

// FieldError is one validation failure at a YAML path such as
// "endpoints[1].base_url".
type FieldError struct {
	Path string
	Msg  string
}

func (e FieldError) Error() string { return e.Path + ": " + e.Msg }

// Errors is every validation failure of a manifest.
type Errors []FieldError

func (es Errors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

// Has reports whether some failure is at path.
func (es Errors) Has(path string) bool {
	for _, e := range es {
		if e.Path == path {
			return true
		}
	}
	return false
}

// Verdict vocabulary accepted in manifests.
var (
	Causes      = []string{"request", "transient", "rate_limit", "quota", "auth_expired", "banned", "content", "unknown"}
	RetryModes  = []string{"none", "next", "downgrade"}
	CredActions = []string{CredKeep, CredCooldown, CredCoolUntilReset, CredRefresh, CredQuarantine, CredManual}
	Signals     = []ir.ErrorKind{
		ir.ErrInvalidRequest, ir.ErrContextTooLong, ir.ErrAuthentication, ir.ErrPermission,
		ir.ErrNotFound, ir.ErrRateLimited, ir.ErrQuotaExhausted, ir.ErrOverloaded,
		ir.ErrContentFiltered, ir.ErrCapabilityMissing, ir.ErrTimeout, ir.ErrInternal, ir.ErrUpstream,
	}
	CacheModes = []ir.CacheMode{ir.CacheExplicit, ir.CacheKeyed, ir.CacheImplicit, ir.CacheNone}
)

// ReservedVariables cannot be declared: they are upstream config keys or
// placeholders the core fills.
var ReservedVariables = []string{"model", "manifest", "issuer"}

var (
	idRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	varRe      = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	tokenRe    = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// Validate checks m after Normalize. It returns Errors or nil.
func (m *Manifest) Validate() error {
	v := &validator{m: m}
	v.run()
	if len(v.errs) == 0 {
		return nil
	}
	return v.errs
}

type validator struct {
	m    *Manifest
	errs Errors
}

func (v *validator) add(path, format string, a ...any) {
	v.errs = append(v.errs, FieldError{Path: path, Msg: fmt.Sprintf(format, a...)})
}

func (v *validator) run() {
	m := v.m
	if m.ID == "" {
		v.add("id", "required")
	} else if !idRe.MatchString(m.ID) {
		v.add("id", "must match %s", idRe)
	}
	if m.Kind != Kind {
		v.add("kind", "must be %q", Kind)
	}
	if m.Category != "" && !slices.Contains(Categories, m.Category) {
		v.add("category", "unknown category %q (want one of %s)", m.Category, strings.Join(Categories, ", "))
	}
	if m.Homepage != "" && !isHTTPURL(m.Homepage) {
		v.add("homepage", "must be an http(s) URL")
	}
	if m.Interpreter != "" {
		if _, ok := Interpreters[m.Interpreter]; !ok {
			v.add("interpreter", "unknown interpreter %q (want one of %s)", m.Interpreter, strings.Join(interpreterNames(), ", "))
		}
	}
	for name := range m.Variables {
		p := "variables." + name
		if !varRe.MatchString(name) {
			v.add(p, "name must match %s", varRe)
		}
		if slices.Contains(ReservedVariables, name) {
			v.add(p, "%q is reserved", name)
		}
	}
	v.auth("auth", &m.Auth)
	v.endpoints()
	v.headerNames("owned_headers", m.OwnedHeaders)
	for name := range m.ExtraHeaders {
		v.headerName("extra_headers."+name, name)
	}
	for name, key := range m.CredentialHeaders {
		v.headerName("credential_headers."+name, name)
		if key == "" {
			v.add("credential_headers."+name, "credential meta key required")
		}
	}
	v.caps("capabilities", &m.Capabilities)
	for i := range m.Errors {
		v.rule(fmt.Sprintf("errors[%d]", i), &m.Errors[i])
	}
	v.models()
	v.pricing()
	v.quota()
}

func interpreterNames() []string {
	out := make([]string, 0, len(Interpreters))
	for k := range Interpreters {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (v *validator) endpoints() {
	m := v.m
	if len(m.Endpoints) == 0 {
		v.add("endpoints", "at least one endpoint required")
		return
	}
	seen := map[string]bool{}
	for i := range m.Endpoints {
		e := &m.Endpoints[i]
		p := fmt.Sprintf("endpoints[%d]", i)
		if e.ID == "" {
			v.add(p+".id", "required")
		} else if !idRe.MatchString(e.ID) {
			v.add(p+".id", "must match %s", idRe)
		} else if seen[e.ID] {
			v.add(p+".id", "duplicate endpoint id %q", e.ID)
		}
		seen[e.ID] = true
		if !slices.Contains(Protocols, e.Protocol) {
			v.add(p+".protocol", "unknown protocol %q (want one of %s)", e.Protocol, strings.Join(Protocols, ", "))
		}
		in := m.EndpointInterpreter(e)
		switch protos, ok := Interpreters[in]; {
		case in == "":
			v.add(p+".interpreter", "required (no manifest-level default)")
		case !ok:
			if e.Interpreter != "" {
				v.add(p+".interpreter", "unknown interpreter %q", in)
			}
		case !slices.Contains(protos, e.Protocol):
			v.add(p+".interpreter", "interpreter %q cannot drive protocol %q", in, e.Protocol)
		}
		if e.BaseURL == "" {
			v.add(p+".base_url", "required")
		} else {
			v.placeholders(p+".base_url", e.BaseURL, false)
			if !isHTTPURL(v.sample(e.BaseURL)) {
				v.add(p+".base_url", "must be an http(s) URL with a host, got %q", e.BaseURL)
			}
			if strings.HasSuffix(e.BaseURL, "/") {
				v.add(p+".base_url", "must not end with /")
			}
		}
		if e.Path != "" {
			if !strings.HasPrefix(e.Path, "/") {
				v.add(p+".path", "must start with /")
			}
			v.placeholders(p+".path", e.Path, true)
		}
		for k, val := range e.Query {
			if k == "" {
				v.add(p+".query", "empty parameter name")
			}
			v.placeholders(p+".query."+k, val, false)
		}
		if len(e.UnsupportedBetas) > 0 && e.Protocol != "anthropic" {
			v.add(p+".unsupported_betas", "only applies to protocol anthropic")
		}
		if e.Issuer != "" && !idRe.MatchString(e.Issuer) {
			v.add(p+".issuer", "must match %s", idRe)
		}
		if e.Auth != nil {
			v.auth(p+".auth", e.Auth)
			if e.Auth.KeyOptional {
				v.add(p+".auth.key_optional", "set it on the manifest's auth, not an endpoint's")
			}
		}
		v.caps(p+".capabilities", &e.Capabilities)
	}
}

// sample expands placeholders with defaults, or "example" when there is none.
func (v *validator) sample(s string) string {
	vars := map[string]string{}
	for _, name := range placeholders(s) {
		val := "example"
		if d, ok := v.m.Variables[name]; ok && d.Default != "" {
			val = d.Default
		}
		vars[name] = val
	}
	return Expand(s, vars)
}

func (v *validator) placeholders(path, s string, modelOK bool) {
	for _, name := range placeholders(s) {
		if name == "model" && modelOK {
			continue
		}
		if _, ok := v.m.Variables[name]; !ok {
			v.add(path, "placeholder {%s} is not a declared variable", name)
		}
	}
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

func (v *validator) auth(path string, a *Auth) {
	if a.Header == "" && a.QueryParam == "" {
		v.add(path, "header or query_param required")
		return
	}
	if a.Header != "" && a.QueryParam != "" {
		v.add(path, "set header or query_param, not both")
	}
	if a.Header != "" {
		v.headerName(path+".header", a.Header)
		if a.Scheme != SchemeBearer && a.Scheme != SchemeRaw {
			v.add(path+".scheme", "must be %q or %q", SchemeBearer, SchemeRaw)
		}
	}
}

func (v *validator) headerNames(path string, names []string) {
	for i, n := range names {
		v.headerName(fmt.Sprintf("%s[%d]", path, i), n)
	}
}

func (v *validator) headerName(path, n string) {
	if !tokenRe.MatchString(n) {
		v.add(path, "invalid header name %q", n)
	}
}

func (v *validator) caps(path string, c *Capabilities) {
	if c.Cache != nil && !slices.Contains(CacheModes, *c.Cache) {
		v.add(path+".cache", "unknown cache mode %q", *c.Cache)
	}
	if c.Reasoning != nil && c.Reasoning.Style != "" && !slices.Contains(ReasoningStyles, c.Reasoning.Style) {
		v.add(path+".reasoning", "must be a bool or one of %s", strings.Join(ReasoningStyles, ", "))
	}
	if c.MaxContext != nil && *c.MaxContext < 0 {
		v.add(path+".max_context", "must not be negative")
	}
}

func (v *validator) rule(path string, r *ErrorRule) {
	w := r.When
	if w.IsZero() {
		v.add(path+".when", "at least one condition required")
	}
	if w.Status != 0 && (w.Status < 100 || w.Status > 599) {
		v.add(path+".when.status", "must be an HTTP status")
	}
	for i, s := range w.StatusIn {
		if s < 100 || s > 599 {
			v.add(fmt.Sprintf("%s.when.status_in[%d]", path, i), "must be an HTTP status")
		}
	}
	if w.HeaderPresent != "" {
		v.headerName(path+".when.header_present", w.HeaderPresent)
	}
	if w.Signal != "" && !slices.Contains(Signals, w.Signal) {
		v.add(path+".when.signal", "unknown signal %q", w.Signal)
	}
	vd := r.Verdict
	vp := path + ".verdict"
	if !slices.Contains(Causes, vd.Cause) {
		v.add(vp+".cause", "must be one of %s", strings.Join(Causes, ", "))
	}
	if vd.Retry != "" && !slices.Contains(RetryModes, vd.Retry) {
		v.add(vp+".retry", "must be one of %s", strings.Join(RetryModes, ", "))
	}
	if vd.Credential != "" && !slices.Contains(CredActions, vd.Credential) {
		v.add(vp+".credential", "must be one of %s", strings.Join(CredActions, ", "))
	}
	if vd.Signal != "" && !slices.Contains(Signals, vd.Signal) {
		v.add(vp+".signal", "unknown signal %q", vd.Signal)
	}
	if vd.Cooldown < 0 {
		v.add(vp+".cooldown", "must not be negative")
	}
	if (vd.Retry == "downgrade") != (len(vd.Downgrade) > 0) {
		v.add(vp+".downgrade", "downgrade edits and retry: downgrade go together")
	}
	// Hard rules 16 and 17 hold for declarative rules too.
	retry, cred := RuleDefaults(vd.Cause)
	if vd.Retry != "" {
		retry = vd.Retry
	}
	if vd.Credential != "" {
		cred = vd.Credential
	}
	switch vd.Cause {
	case "content":
		if retry != "none" {
			v.add(vp+".retry", "content rejections are never retried on another credential")
		}
	case "banned", "unknown":
		if retry != "none" {
			v.add(vp+".retry", "suspected bans are not retried")
		}
		if cred != CredQuarantine && cred != CredManual {
			v.add(vp+".credential", "suspected bans must quarantine the credential")
		}
	case "request", "transient":
		if cred != CredKeep {
			v.add(vp+".credential", "failures that are not the credential's fault must keep it")
		}
	}
}

// RuleDefaults returns the retry mode and credential action a rule with
// this cause gets when it sets neither.
func RuleDefaults(cause string) (retry, credential string) {
	switch cause {
	case "transient":
		return "next", CredKeep
	case "rate_limit":
		return "next", CredCooldown
	case "quota":
		return "next", CredCoolUntilReset
	case "auth_expired":
		return "next", CredRefresh
	case "banned", "unknown":
		return "none", CredQuarantine
	default: // request, content
		return "none", CredKeep
	}
}

func (v *validator) models() {
	md := v.m.Models
	if md.List != "" && len(md.Static) > 0 {
		v.add("models", "set list or static, not both")
	}
	if md.List != "" && !strings.HasPrefix(md.List, "/") {
		v.add("models.list", "must start with /")
	}
	if md.Endpoint != "" {
		if _, ok := v.m.Endpoint(md.Endpoint); !ok {
			v.add("models.endpoint", "no endpoint %q", md.Endpoint)
		}
	}
	seen := map[string]bool{}
	for i, mi := range md.Static {
		p := fmt.Sprintf("models.static[%d].id", i)
		if mi.ID == "" {
			v.add(p, "required")
		} else if seen[mi.ID] {
			v.add(p, "duplicate model %q", mi.ID)
		}
		seen[mi.ID] = true
	}
}

func (v *validator) pricing() {
	p := v.m.Pricing
	if p == nil {
		return
	}
	if p.Models == nil {
		if p.File == "" {
			v.add("pricing.models", "required")
		}
		return
	}
	if p.Currency != "" && !currencyRe.MatchString(p.Currency) {
		v.add("pricing.currency", "must be an ISO 4217 code")
	}
	names := make([]string, 0, len(p.Models))
	for n := range p.Models {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		pr := p.Models[n]
		path := "pricing.models." + n
		for field, val := range map[string]float64{
			"input": pr.Input, "cache_read": pr.CacheRead, "cache_write_5m": pr.CacheWrite5m,
			"cache_write_1h": pr.CacheWrite1h, "output": pr.Output,
		} {
			if val < 0 {
				v.add(path+"."+field, "must not be negative")
			}
		}
		cur := pr.Currency
		if cur == "" {
			cur = p.Currency
		}
		if cur == "" {
			v.add(path+".currency", "required (no pricing-level default)")
		} else if !currencyRe.MatchString(cur) {
			v.add(path+".currency", "must be an ISO 4217 code")
		}
	}
}

func (v *validator) quota() {
	q := v.m.Quota
	if q == nil {
		return
	}
	if !strings.HasPrefix(q.Path, "/") {
		v.add("quota.path", "must start with /")
	}
	if q.Method != "" && q.Method != http.MethodGet && q.Method != http.MethodPost {
		v.add("quota.method", "must be GET or POST")
	}
	if q.Endpoint != "" {
		if _, ok := v.m.Endpoint(q.Endpoint); !ok {
			v.add("quota.endpoint", "no endpoint %q", q.Endpoint)
		}
	}
	if q.RemainingFraction == "" && (q.Remaining == "" || q.Limit == "") {
		v.add("quota", "remaining_fraction, or remaining with limit, required")
	}
}

func http1Canon(s string) string { return textproto.CanonicalMIMEHeaderKey(s) }
