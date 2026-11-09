// Package bedrock is the Amazon Bedrock provider (provider.bedrock) for
// Anthropic models served through Bedrock Runtime's InvokeModel and
// InvokeModelWithResponseStream:
// https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_InvokeModel.html
// https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_InvokeModelWithResponseStream.html
//
// Endpoints, capabilities, error rules and models come from a manifest
// (manifests/bedrock.yaml) as for any manifest-driven upstream. What a
// manifest cannot say lives here:
//
//   - the model id goes into the path (/model/{id}/invoke), not the body;
//   - the body carries anthropic_version "bedrock-2023-05-31" and the
//     anthropic-beta header values as anthropic_beta, and no model or
//     stream field
//     (https://docs.aws.amazon.com/bedrock/latest/userguide/model-parameters-anthropic-claude-messages-request-response.html);
//   - requests are signed with SigV4 (service "bedrock"), or carry a
//     Bedrock API key as a bearer token
//     (https://docs.aws.amazon.com/bedrock/latest/userguide/api-keys-use.html);
//   - streams arrive in the AWS event stream binary framing, each chunk
//     wrapping one Anthropic stream event, so the provider performs the call
//     itself (plugin.Sender) and decodes the chunks into IR events.
//
// Credential secret, one of:
//
//	ACCESS_KEY_ID:SECRET_ACCESS_KEY[:SESSION_TOKEN]
//	{"access_key_id": "...", "secret_access_key": "...", "session_token": "..."}
//	<Bedrock API key>
//
// The JSON form also accepts the AWS CLI's spelling (AccessKeyId,
// SecretAccessKey, SessionToken), at the top level or under "Credentials"
// as `aws sts get-session-token` prints it. Temporary credentials are used
// as given; nothing here assumes roles or renews them.
package bedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/provider/compatible"
	"github.com/initxy/plugrelay/plugins/provider/internal/sigv4"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ID is the plugin id; manifests select it with interpreter: bedrock.
const ID = "provider.bedrock"

// Wire constants of the Bedrock Messages dialect.
const (
	AnthropicVersion = "bedrock-2023-05-31"
	signingService   = "bedrock"
	defaultRegion    = "us-east-1"
	contentJSON      = "application/json"
	contentStream    = "application/vnd.amazon.eventstream"
)

func init() {
	plugin.Register(ID, func() plugin.Plugin { return New() })
}

// Provider is the Bedrock provider.
type Provider struct {
	*compatible.Provider
}

var (
	_ plugin.Provider    = (*Provider)(nil)
	_ plugin.Sender      = (*Provider)(nil)
	_ plugin.Initializer = (*Provider)(nil)
)

// New returns the provider. Options are those of the manifest-driven
// provider it builds on (WithHTTPClient, WithClock, WithIndex, …).
func New(opts ...compatible.Option) *Provider {
	opts = append([]compatible.Option{compatible.WithDefaultManifest("bedrock"),
		compatible.WithDescription("Amazon Bedrock (SigV4 or Bedrock API key; Anthropic models)")}, opts...)
	return &Provider{Provider: compatible.New("bedrock", opts...)}
}

// Prepare implements plugin.Provider: it rewrites the request the core
// built for the Anthropic Messages API into a Bedrock invocation and
// authenticates it. Token counting is refused so the core answers it
// locally: Bedrock's CountTokens has its own request and response shape.
func (p *Provider) Prepare(ctx context.Context, o *plugin.Outgoing) error {
	if o == nil || o.Req == nil || o.Endpoint == nil {
		return errors.New("bedrock: incomplete outgoing request")
	}
	if strings.HasSuffix(o.Req.URL.Path, "/count_tokens") {
		return errors.New("bedrock: token counting is not forwarded")
	}
	// The generic Prepare drops every owned header and applies the
	// endpoint's beta filter; the credential is applied below, so it gets
	// none.
	blank := plugin.Outgoing{Upstream: o.Upstream, Endpoint: o.Endpoint, Model: o.Model, Req: o.Req}
	if err := p.Provider.Prepare(ctx, &blank); err != nil {
		return err
	}
	body, err := readBody(o.Req)
	if err != nil {
		return err
	}
	model := o.Model
	if model == "" {
		model = gjson.GetBytes(body, "model").String()
	}
	if model == "" {
		return errors.New("bedrock: no model id")
	}
	stream := gjson.GetBytes(body, "stream").Bool()
	h := o.Req.Header
	if body, err = bedrockBody(body, h.Values("Anthropic-Beta")); err != nil {
		return err
	}
	h.Del("Anthropic-Beta")
	h.Del("Anthropic-Version")

	base, err := url.Parse(o.Endpoint.BaseURL)
	if err != nil {
		return fmt.Errorf("bedrock: base URL: %w", err)
	}
	action := "invoke"
	h.Set("Content-Type", contentJSON)
	h.Set("Accept", contentJSON)
	if stream {
		action = "invoke-with-response-stream"
		h.Set("Accept", contentStream)
		h.Set("X-Amzn-Bedrock-Accept", contentJSON)
	}
	prefix := strings.TrimRight(base.Path, "/")
	o.Req.URL.Path = prefix + "/model/" + model + "/" + action
	o.Req.URL.RawPath = prefix + "/model/" + escapeModel(model) + "/" + action
	o.Req.URL.RawQuery = ""
	setBody(o.Req, body)

	return p.authenticate(o, body)
}

// escapeModel escapes a model id or ARN as one path segment, the way the
// AWS SDKs do (":" and "/" included).
func escapeModel(id string) string {
	return strings.ReplaceAll(url.PathEscape(id), ":", "%3A")
}

// bedrockBody removes model and stream, sets anthropic_version and moves
// the beta header values into anthropic_beta.
func bedrockBody(body []byte, betaHeaders []string) ([]byte, error) {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return nil, errors.New("bedrock: request body is not a JSON object")
	}
	var err error
	for _, k := range []string{"model", "stream"} {
		if body, err = sjson.DeleteBytes(body, k); err != nil {
			return nil, err
		}
	}
	if !gjson.GetBytes(body, "anthropic_version").Exists() {
		if body, err = sjson.SetBytes(body, "anthropic_version", AnthropicVersion); err != nil {
			return nil, err
		}
	}
	var betas []string
	for _, b := range gjson.GetBytes(body, "anthropic_beta").Array() {
		betas = appendNew(betas, b.String())
	}
	for _, line := range betaHeaders {
		for _, v := range strings.Split(line, ",") {
			betas = appendNew(betas, strings.TrimSpace(v))
		}
	}
	if len(betas) > 0 {
		raw, _ := json.Marshal(betas)
		if body, err = sjson.SetRawBytes(body, "anthropic_beta", raw); err != nil {
			return nil, err
		}
	}
	return body, nil
}

func appendNew(list []string, v string) []string {
	if v == "" {
		return list
	}
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, errors.New("bedrock: empty request body")
	}
	b, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("bedrock: read body: %w", err)
	}
	return b, nil
}

func setBody(r *http.Request, b []byte) {
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
	r.Header.Del("Content-Length")
}

// authenticate signs the request with the credential's access keys or
// sends its Bedrock API key as a bearer token.
func (p *Provider) authenticate(o *plugin.Outgoing, body []byte) error {
	h := o.Req.Header
	for _, k := range []string{sigv4.HeaderAuthorization, sigv4.HeaderDate, sigv4.HeaderSecurityToken} {
		h.Del(k)
	}
	if o.Credential == nil || o.Credential.Secret == "" {
		return errors.New("bedrock: credential has no secret")
	}
	keys, apiKey, err := ParseSecret(o.Credential.Secret)
	if err != nil {
		return err
	}
	if apiKey != "" {
		h.Set("Authorization", "Bearer "+apiKey)
		return nil
	}
	region := defaultRegion
	if vs, err := p.Vars(o.Upstream); err == nil && vs["region"] != "" {
		region = vs["region"]
	}
	return sigv4.Sign(o.Req, body, *keys, region, signingService, p.Now())
}

// ParseSecret reads a credential secret: access keys, or else a Bedrock
// API key.
func ParseSecret(secret string) (keys *sigv4.Credentials, apiKey string, err error) {
	s := strings.TrimSpace(secret)
	switch {
	case strings.HasPrefix(s, "{"):
		var f map[string]json.RawMessage
		if json.Unmarshal([]byte(s), &f) != nil {
			return nil, "", errors.New("bedrock: credential JSON is malformed")
		}
		if inner, ok := f["Credentials"]; ok {
			f = nil
			if json.Unmarshal(inner, &f) != nil {
				return nil, "", errors.New("bedrock: credential JSON is malformed")
			}
		}
		pick := func(names ...string) string {
			for _, n := range names {
				var v string
				if raw, ok := f[n]; ok && json.Unmarshal(raw, &v) == nil && v != "" {
					return v
				}
			}
			return ""
		}
		k := &sigv4.Credentials{
			AccessKeyID:     pick("access_key_id", "aws_access_key_id", "AccessKeyId"),
			SecretAccessKey: pick("secret_access_key", "aws_secret_access_key", "SecretAccessKey"),
			SessionToken:    pick("session_token", "aws_session_token", "SessionToken"),
		}
		if k.AccessKeyID == "" || k.SecretAccessKey == "" {
			return nil, "", errors.New("bedrock: credential JSON needs access_key_id and secret_access_key")
		}
		return k, "", nil
	case strings.Contains(s, ":"):
		parts := strings.Split(s, ":")
		if len(parts) > 3 || parts[0] == "" || parts[1] == "" || (len(parts) == 3 && parts[2] == "") {
			return nil, "", errors.New("bedrock: credential must be ACCESS_KEY_ID:SECRET_ACCESS_KEY[:SESSION_TOKEN]")
		}
		k := &sigv4.Credentials{AccessKeyID: parts[0], SecretAccessKey: parts[1]}
		if len(parts) == 3 {
			k.SessionToken = parts[2]
		}
		return k, "", nil
	case s == "":
		return nil, "", errors.New("bedrock: empty credential")
	}
	return nil, s, nil
}
