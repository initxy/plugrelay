// Package compatible is the generic, manifest-driven provider: it serves
// any upstream a manifest (package manifest) can describe, for every
// interpreter name (openai-compatible, anthropic-compatible, gemini) and for
// the thin official registrations bound to a default manifest.
//
// An upstream instance selects its manifest with Upstream.Config:
//
//	manifest: <id>            # looked up in the provider's Index
//	<variable>: <value>       # fills the manifest's {variable} placeholders
//	issuer: <issuer>          # optional override of every endpoint's issuer
//
// Config["manifest"] may also hold a *manifest.Manifest, or the manifest as
// a decoded YAML/JSON map, for manifests defined in the console.
//
// Behaviour that differs by wire protocol (version and beta headers of the
// anthropic protocol) keys off the endpoint's protocol, never off an
// upstream's identity.
package compatible

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/plugins/provider/manifest"
	"gopkg.in/yaml.v3"
)

// Header names of the anthropic wire protocol.
const (
	headerVersion       = "anthropic-version"
	headerBeta          = "anthropic-beta"
	defaultProtoVersion = "2023-06-01"
	protoAnthropic      = "anthropic"
)

// Provider is a plugin.Provider driven by manifests.
type Provider struct {
	id          string
	interpreter string
	description string
	// defaultManifest is used when an upstream config names none.
	defaultManifest string
	// issuers, when set, restricts Config["issuer"] overrides.
	issuers   []string
	index     *Index
	client    *http.Client // test override of every egress client
	host      plugin.Host
	logger    *slog.Logger
	now       func() time.Time
	refresher plugin.Refresher

	// bound remembers the manifest of each upstream id seen, because
	// Outcome does not carry the upstream.
	bound sync.Map // upstream id → *manifest.Manifest
}

// Option configures a Provider.
type Option func(*Provider)

// WithIndex sets the manifest index; the default is Default().
func WithIndex(x *Index) Option { return func(p *Provider) { p.index = x } }

// WithHTTPClient makes every call of the provider's own, inside an attempt
// or not, use c instead of the core's egress. For tests: c applies no
// credential proxy.
func WithHTTPClient(c *http.Client) Option { return func(p *Provider) { p.client = c } }

// WithDefaultManifest binds upstreams without a manifest config to id.
func WithDefaultManifest(id string) Option { return func(p *Provider) { p.defaultManifest = id } }

// WithIssuers restricts the issuer overrides an upstream config may set.
func WithIssuers(issuers ...string) Option { return func(p *Provider) { p.issuers = issuers } }

// WithClock sets the time source used for cooldown ends.
func WithClock(now func() time.Time) Option { return func(p *Provider) { p.now = now } }

// WithDescription sets Info().Description.
func WithDescription(d string) Option { return func(p *Provider) { p.description = d } }

// New returns a provider registered as "provider.<interpreter>".
func New(interpreter string, opts ...Option) *Provider {
	p := &Provider{
		id:          "provider." + interpreter,
		interpreter: interpreter,
		description: "Manifest-driven " + interpreter + " upstream",
		now:         time.Now,
		logger:      slog.Default(),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Info implements plugin.Plugin.
func (p *Provider) Info() plugin.Info {
	return plugin.Info{ID: p.id, Kind: plugin.KindProvider, Version: "1.0.0", Description: p.description}
}

// Init takes the core host and logger.
func (p *Provider) Init(_ context.Context, h plugin.Host) error {
	p.host = h
	if l := h.Logger(); l != nil {
		p.logger = l
	}
	return nil
}

// RefreshingProvider is a Provider whose credentials expire: 401s ask the
// core to refresh instead of cooling the credential down.
type RefreshingProvider struct {
	*Provider
}

// NewRefreshing returns a provider that refreshes credentials with r.
func NewRefreshing(interpreter string, r plugin.Refresher, opts ...Option) *RefreshingProvider {
	p := New(interpreter, opts...)
	p.refresher = r
	return &RefreshingProvider{p}
}

// Refresh implements plugin.Refresher.
func (p *RefreshingProvider) Refresh(ctx context.Context, u *plugin.Upstream, c *plugin.Credential) (string, time.Time, error) {
	return p.refresher.Refresh(ctx, u, c)
}

// ClientFor is the egress of a call the provider makes to u with c outside
// an attempt (model listing, quota probes, credential refresh): Host.ClientFor,
// so the credential's proxy applies.
func (p *Provider) ClientFor(u *plugin.Upstream, c *plugin.Credential) (*http.Client, error) {
	if p.client != nil {
		return p.client, nil
	}
	if p.host == nil {
		return nil, errors.New(p.id + ": no egress client (provider not initialised)")
	}
	return p.host.ClientFor(u, c)
}

// AttemptClient is the egress of a call the provider makes during an
// attempt: o.Client, which the core always sets.
func (p *Provider) AttemptClient(o *plugin.Outgoing) (*http.Client, error) {
	if p.client != nil {
		return p.client, nil
	}
	if o == nil || o.Client == nil {
		return nil, errors.New(p.id + ": the core gave this attempt no egress client")
	}
	return o.Client, nil
}

// Now reads the provider's clock.
func (p *Provider) Now() time.Time { return p.now() }

// Vars returns the manifest variables of an upstream instance, from its
// config or the variables' defaults.
func (p *Provider) Vars(u *plugin.Upstream) (map[string]string, error) {
	m, err := p.Manifest(u)
	if err != nil {
		return nil, err
	}
	return vars(m, u)
}

func (p *Provider) idx() *Index {
	if p.index != nil {
		return p.index
	}
	return Default()
}

// Manifest resolves the manifest of an upstream instance.
func (p *Provider) Manifest(u *plugin.Upstream) (*manifest.Manifest, error) {
	if u == nil {
		return nil, errors.New("compatible: nil upstream")
	}
	m, err := p.resolve(u)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: %w", u.ID, err)
	}
	p.bound.Store(u.ID, m)
	return m, nil
}

func (p *Provider) resolve(u *plugin.Upstream) (*manifest.Manifest, error) {
	switch v := u.Config["manifest"].(type) {
	case *manifest.Manifest:
		return v, nil
	case string:
		if m, ok := p.idx().Get(v); ok {
			return m, nil
		}
		return nil, fmt.Errorf("unknown manifest %q", v)
	case map[string]any:
		raw, err := yaml.Marshal(v)
		if err != nil {
			return nil, err
		}
		return manifest.Parse(raw)
	case nil:
		if p.defaultManifest == "" {
			return nil, errors.New("config.manifest required")
		}
		if m, ok := p.idx().Get(p.defaultManifest); ok {
			return m, nil
		}
		return nil, fmt.Errorf("default manifest %q not loaded", p.defaultManifest)
	default:
		return nil, fmt.Errorf("config.manifest: unsupported type %T", v)
	}
}

// vars collects variable values from the upstream config, falling back to
// defaults; a missing required variable is an error.
func vars(m *manifest.Manifest, u *plugin.Upstream) (map[string]string, error) {
	out := make(map[string]string, len(m.Variables))
	for name, d := range m.Variables {
		val := d.Default
		if s, ok := u.Config[name].(string); ok && s != "" {
			val = s
		}
		if val == "" && d.Required {
			return nil, fmt.Errorf("upstream %s: config.%s required by manifest %s", u.ID, name, m.ID)
		}
		out[name] = val
	}
	return out, nil
}

// Endpoints implements plugin.Provider.
func (p *Provider) Endpoints(u *plugin.Upstream) ([]plugin.Endpoint, error) {
	m, err := p.Manifest(u)
	if err != nil {
		return nil, err
	}
	vs, err := vars(m, u)
	if err != nil {
		return nil, err
	}
	issuer, _ := u.Config["issuer"].(string)
	if issuer != "" && len(p.issuers) > 0 && !slices.Contains(p.issuers, issuer) {
		return nil, fmt.Errorf("upstream %s: issuer %q not one of %s", u.ID, issuer, strings.Join(p.issuers, ", "))
	}
	out := make([]plugin.Endpoint, 0, len(m.Endpoints))
	for i := range m.Endpoints {
		e := &m.Endpoints[i]
		base := manifest.Expand(e.BaseURL, vs)
		if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
			return nil, fmt.Errorf("upstream %s: endpoint %s: base URL %q is not http(s)", u.ID, e.ID, base)
		}
		ep := plugin.Endpoint{
			ID:        e.ID,
			Protocol:  e.Protocol,
			BaseURL:   strings.TrimRight(base, "/"),
			Caps:      m.EndpointCaps(e, codecCapabilities(e.Protocol)),
			Issuer:    e.Issuer,
			Path:      manifest.Expand(e.Path, vs),
			DropBetas: slices.Clone(e.UnsupportedBetas),
		}
		if issuer != "" {
			ep.Issuer = issuer
		}
		if ep.Issuer == "" {
			// An undeclared issuer must never match another upstream's.
			ep.Issuer = "upstream:" + u.ID
		}
		if len(e.Query) > 0 {
			ep.Query = make(map[string]string, len(e.Query))
			for k, v := range e.Query {
				ep.Query[k] = manifest.Expand(v, vs)
			}
		}
		out = append(out, ep)
	}
	return out, nil
}

// codecCapabilities is the registered codec's capabilities, the base a
// manifest endpoint's declaration is merged over; zero when the codec is not
// linked in. fill_in_middle is left out: few servers honour a suffix, so an
// endpoint has it only when its manifest declares it.
func codecCapabilities(name string) ir.Capabilities {
	if codec, ok := protocol.Get(name); ok {
		caps := codec.Capabilities()
		caps.FillInMiddle = false
		return caps
	}
	return ir.Capabilities{}
}

// OwnedHeaders implements plugin.Provider: every endpoint's auth header,
// owned_headers, extra_headers and credential_headers.
func (p *Provider) OwnedHeaders(u *plugin.Upstream) []string {
	m, err := p.Manifest(u)
	if err != nil {
		return nil
	}
	return ownedHeaders(m)
}

func ownedHeaders(m *manifest.Manifest) []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		h = textproto.CanonicalMIMEHeaderKey(h)
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	add(m.Auth.Header)
	for i := range m.Endpoints {
		if a := m.Endpoints[i].Auth; a != nil {
			add(a.Header)
		}
	}
	for _, h := range m.OwnedHeaders {
		add(h)
	}
	for h := range m.ExtraHeaders {
		add(h)
	}
	for h := range m.CredentialHeaders {
		add(h)
	}
	slices.Sort(out)
	return out
}

// Prepare implements plugin.Provider. It removes every owned header from
// the outgoing request before setting the provider's values, so a client's
// copy never survives even if the core forwarded it.
func (p *Provider) Prepare(_ context.Context, o *plugin.Outgoing) error {
	if o == nil || o.Req == nil || o.Endpoint == nil {
		return errors.New("compatible: incomplete outgoing request")
	}
	m, err := p.Manifest(o.Upstream)
	if err != nil {
		return err
	}
	me, ok := m.Endpoint(o.Endpoint.ID)
	if !ok {
		return fmt.Errorf("compatible: manifest %s has no endpoint %q", m.ID, o.Endpoint.ID)
	}
	h := o.Req.Header
	if h == nil {
		h = http.Header{}
		o.Req.Header = h
	}
	for _, name := range ownedHeaders(m) {
		h.Del(name)
	}
	if err := applyAuth(o.Req, m.EndpointAuth(me), o.Credential); err != nil {
		return err
	}
	for k, v := range m.ExtraHeaders {
		h.Set(k, v)
	}
	if o.Credential != nil {
		for k, key := range m.CredentialHeaders {
			if v := o.Credential.Meta[key]; v != "" {
				h.Set(k, v)
			}
		}
	}
	if len(o.Endpoint.Query) > 0 {
		q := o.Req.URL.Query()
		for k, v := range o.Endpoint.Query {
			if q.Get(k) == "" {
				q.Set(k, v)
			}
		}
		o.Req.URL.RawQuery = q.Encode()
	}
	if o.Endpoint.Protocol == protoAnthropic {
		if h.Get(headerVersion) == "" {
			h.Set(headerVersion, defaultProtoVersion)
		}
		dropBetas(h, o.Endpoint.DropBetas)
	}
	return nil
}

// applyAuth places the credential secret. An empty secret sends nothing,
// for keyless self-hosted servers.
func applyAuth(r *http.Request, a manifest.Auth, c *plugin.Credential) error {
	if a.QueryParam != "" {
		q := r.URL.Query()
		q.Del(a.QueryParam)
		if c != nil && c.Secret != "" {
			q.Set(a.QueryParam, c.Secret)
		}
		r.URL.RawQuery = q.Encode()
		return nil
	}
	if a.Header == "" {
		return errors.New("compatible: manifest auth has no header")
	}
	r.Header.Del(a.Header)
	if c == nil || c.Secret == "" {
		return nil
	}
	if a.Scheme == manifest.SchemeBearer {
		r.Header.Set(a.Header, "Bearer "+c.Secret)
	} else {
		r.Header.Set(a.Header, c.Secret)
	}
	return nil
}

// dropBetas removes values from the comma-separated beta header, across
// repeated header lines, and deletes the header when nothing is left.
func dropBetas(h http.Header, drop []string) {
	if len(drop) == 0 {
		return
	}
	var keep []string
	for _, line := range h.Values(headerBeta) {
		for _, v := range strings.Split(line, ",") {
			v = strings.TrimSpace(v)
			if v != "" && !slices.Contains(drop, v) {
				keep = append(keep, v)
			}
		}
	}
	h.Del(headerBeta)
	if len(keep) > 0 {
		h.Set(headerBeta, strings.Join(keep, ","))
	}
}
