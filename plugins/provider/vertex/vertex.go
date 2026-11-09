// Package vertex is the Google Vertex AI provider (provider.vertex) for
// Anthropic models (manifests/vertex-anthropic.yaml) and Gemini models
// (manifests/vertex-gemini.yaml).
//
// What a manifest cannot say lives here:
//
//   - authentication: the credential is a service account key file; the
//     provider trades it for an OAuth 2.0 access token with the JWT bearer
//     grant and caches the token per credential, replacing it five minutes
//     before it expires
//     (https://developers.google.com/identity/protocols/oauth2/service-account);
//   - Anthropic models: the model goes into the path
//     (…/publishers/anthropic/models/{model}:rawPredict, :streamRawPredict
//     for streams), the body carries anthropic_version "vertex-2023-10-16"
//     and no model field; token counting is …/models/count-tokens:rawPredict
//     with the model in the body
//     (https://cloud.google.com/vertex-ai/generative-ai/docs/partner-models/claude/use-claude);
//   - Gemini models: the Gemini API path /v1beta/models/{model}:{action}
//     becomes …/publishers/google/models/{model}:{action}; bodies and
//     streams (alt=sse) are unchanged
//     (https://cloud.google.com/vertex-ai/generative-ai/docs/reference/rest/v1/projects.locations.endpoints/generateContent);
//   - the "global" location is served from aiplatform.googleapis.com, the
//     others from {location}-aiplatform.googleapis.com.
//
// Paths are under /v1/projects/{project}/locations/{location}. The project
// is the upstream's "project" variable, else the key file's project_id.
//
// Credential secret: the service account key JSON as downloaded from the
// console. Anything that is not JSON is used as a ready-made access token
// (`gcloud auth print-access-token`); such a token expires within the hour
// and is not renewed, so it suits trying things out only.
package vertex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/provider/compatible"
	"github.com/initxy/plugrelay/plugins/provider/internal/gcpauth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ID is the plugin id; manifests select it with interpreter: vertex.
const ID = "provider.vertex"

// AnthropicVersion is the body field Claude on Vertex AI requires.
const AnthropicVersion = "vertex-2023-10-16"

// Token cache policy.
const (
	refreshMargin = 5 * time.Minute
	failFor       = 30 * time.Second
)

func init() {
	plugin.Register(ID, func() plugin.Plugin { return New() })
}

// Provider is the Vertex AI provider.
type Provider struct {
	*compatible.RefreshingProvider
	tokens gcpauth.Cache
}

var (
	_ plugin.Provider  = (*Provider)(nil)
	_ plugin.Refresher = (*Provider)(nil)
)

// New returns the provider. Options are those of the manifest-driven
// provider it builds on (WithHTTPClient, WithClock, WithIndex, …).
func New(opts ...compatible.Option) *Provider {
	p := &Provider{tokens: gcpauth.Cache{Margin: refreshMargin, FailFor: failFor}}
	opts = append([]compatible.Option{compatible.WithDescription("Google Vertex AI (service account; Anthropic and Gemini models)")}, opts...)
	p.RefreshingProvider = compatible.NewRefreshing("vertex", p, opts...)
	return p
}

// Endpoints implements plugin.Provider, serving the global location from
// its host without a location prefix.
func (p *Provider) Endpoints(u *plugin.Upstream) ([]plugin.Endpoint, error) {
	eps, err := p.RefreshingProvider.Endpoints(u)
	for i := range eps {
		eps[i].BaseURL = strings.Replace(eps[i].BaseURL, "://global-aiplatform.googleapis.com", "://aiplatform.googleapis.com", 1)
	}
	return eps, err
}

// Prepare implements plugin.Provider.
func (p *Provider) Prepare(ctx context.Context, o *plugin.Outgoing) error {
	if o == nil || o.Req == nil || o.Endpoint == nil {
		return errors.New("vertex: incomplete outgoing request")
	}
	blank := plugin.Outgoing{Upstream: o.Upstream, Endpoint: o.Endpoint, Model: o.Model, Req: o.Req}
	if err := p.RefreshingProvider.Prepare(ctx, &blank); err != nil {
		return err
	}
	if o.Credential == nil || strings.TrimSpace(o.Credential.Secret) == "" {
		return errors.New("vertex: credential has no secret")
	}
	sa, static, err := parseSecret(o.Credential.Secret)
	if err != nil {
		return err
	}
	vs, err := p.Vars(o.Upstream)
	if err != nil {
		return err
	}
	project, location := vs["project"], vs["location"]
	if project == "" && sa != nil {
		project = sa.ProjectID
	}
	if project == "" || location == "" {
		return errors.New("vertex: project and location required (upstream config, or project_id in the key file)")
	}
	base, err := url.Parse(o.Endpoint.BaseURL)
	if err != nil {
		return fmt.Errorf("vertex: base URL: %w", err)
	}
	prefix := strings.TrimRight(base.Path, "/") + "/v1/projects/" + url.PathEscape(project) + "/locations/" + url.PathEscape(location)
	switch o.Endpoint.Protocol {
	case "anthropic":
		err = anthropicRequest(o, prefix)
	case "gemini":
		err = geminiRequest(o, prefix)
	default:
		err = fmt.Errorf("vertex: protocol %q is not served", o.Endpoint.Protocol)
	}
	if err != nil {
		return err
	}
	token := static
	if sa != nil {
		client, err := p.AttemptClient(o)
		if err != nil {
			return err
		}
		tok, err := p.token(ctx, client, o.Credential, sa)
		if err != nil {
			return err
		}
		token = tok.AccessToken
	}
	o.Req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func anthropicRequest(o *plugin.Outgoing, prefix string) error {
	body, err := readBody(o.Req)
	if err != nil {
		return err
	}
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return errors.New("vertex: request body is not a JSON object")
	}
	model := o.Model
	if model == "" {
		model = gjson.GetBytes(body, "model").String()
	}
	var path string
	if strings.HasSuffix(o.Req.URL.Path, "/count_tokens") {
		path = prefix + "/publishers/anthropic/models/count-tokens:rawPredict"
		if model != "" {
			body, err = sjson.SetBytes(body, "model", model)
		}
	} else {
		if model == "" {
			return errors.New("vertex: no model")
		}
		action := "rawPredict"
		if gjson.GetBytes(body, "stream").Bool() {
			action = "streamRawPredict"
		}
		path = prefix + "/publishers/anthropic/models/" + url.PathEscape(model) + ":" + action
		body, err = sjson.DeleteBytes(body, "model")
	}
	if err != nil {
		return err
	}
	if !gjson.GetBytes(body, "anthropic_version").Exists() {
		if body, err = sjson.SetBytes(body, "anthropic_version", AnthropicVersion); err != nil {
			return err
		}
	}
	setPath(o.Req.URL, path)
	o.Req.URL.RawQuery = ""
	setBody(o.Req, body)
	return nil
}

// geminiRequest keeps the action and query of the Gemini API path and
// moves them under the Vertex publisher path.
func geminiRequest(o *plugin.Outgoing, prefix string) error {
	p := o.Req.URL.Path
	i := strings.LastIndex(p, "/models/")
	if i < 0 {
		return fmt.Errorf("vertex: unexpected Gemini path %q", p)
	}
	model, action, ok := strings.Cut(p[i+len("/models/"):], ":")
	if !ok || action == "" {
		return fmt.Errorf("vertex: Gemini path %q has no action", p)
	}
	if o.Model != "" {
		model = strings.TrimPrefix(o.Model, "models/")
	}
	setPath(o.Req.URL, prefix+"/publishers/google/models/"+url.PathEscape(model)+":"+action)
	return nil
}

// setPath sets an already escaped path.
func setPath(u *url.URL, escaped string) {
	if p, err := url.PathUnescape(escaped); err == nil {
		u.Path, u.RawPath = p, escaped
	}
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, errors.New("vertex: empty request body")
	}
	b, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("vertex: read body: %w", err)
	}
	return b, nil
}

func setBody(r *http.Request, b []byte) {
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	r.Header.Del("Content-Length")
}

// parseSecret returns the service account of a key file, or a static
// access token.
func parseSecret(secret string) (*gcpauth.ServiceAccount, string, error) {
	s := strings.TrimSpace(secret)
	if !strings.HasPrefix(s, "{") {
		return nil, s, nil
	}
	sa, err := gcpauth.ParseServiceAccount([]byte(s))
	if err != nil {
		return nil, "", fmt.Errorf("vertex: %w", err)
	}
	return sa, "", nil
}

// cacheKey ties a cached token to the credential and its current secret,
// so replacing the key file never reuses the old token.
func cacheKey(c *plugin.Credential) string {
	sum := sha256.Sum256([]byte(c.Secret))
	return c.UpstreamID + "/" + c.ID + "/" + hex.EncodeToString(sum[:8])
}

// token returns the cached access token or exchanges the key for one
// through client, the credential's egress.
func (p *Provider) token(ctx context.Context, client *http.Client, c *plugin.Credential, sa *gcpauth.ServiceAccount) (gcpauth.Token, error) {
	tok, err := p.tokens.Get(ctx, cacheKey(c), p.Now, func(ctx context.Context) (gcpauth.Token, error) {
		return sa.Exchange(ctx, client, gcpauth.ScopeCloudPlatform, p.Now())
	})
	if err != nil {
		return gcpauth.Token{}, fmt.Errorf("vertex: access token for %s: %w", sa.ClientEmail, err)
	}
	return tok, nil
}

// Refresh implements plugin.Refresher: it drops the cached access token
// and fetches a new one. The stored secret (the key file) is unchanged;
// the returned expiry is the new token's.
func (p *Provider) Refresh(ctx context.Context, u *plugin.Upstream, c *plugin.Credential) (string, time.Time, error) {
	if c == nil {
		return "", time.Time{}, errors.New("vertex: no credential")
	}
	sa, _, err := parseSecret(c.Secret)
	if err != nil {
		return "", time.Time{}, err
	}
	if sa == nil {
		return "", time.Time{}, errors.New("vertex: a static access token cannot be refreshed; use a service account key")
	}
	client, err := p.ClientFor(u, c)
	if err != nil {
		return "", time.Time{}, err
	}
	p.tokens.Invalidate(cacheKey(c))
	tok, err := p.token(ctx, client, c, sa)
	if err != nil {
		return "", time.Time{}, err
	}
	return c.Secret, tok.Expiry, nil
}

// Classify implements plugin.Provider. A rejected access token is dropped
// so the next attempt with this credential fetches a new one.
func (p *Provider) Classify(o *plugin.Outcome) plugin.Verdict {
	v := p.RefreshingProvider.Classify(o)
	if v.Cause == plugin.CauseAuthExpired && o != nil && o.Credential != nil {
		p.tokens.Invalidate(cacheKey(o.Credential))
	}
	return v
}
