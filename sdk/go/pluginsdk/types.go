package pluginsdk

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Error is an error as it travels on the wire: in replies, verdicts,
// stream events and rejections.
type Error struct {
	Kind         ir.ErrorKind `json:"kind"`
	Message      string       `json:"message"`
	RetryAfterMs int64        `json:"retry_after_ms,omitempty"`
	ResetAt      time.Time    `json:"reset_at,omitzero"`
	NoRetry      bool         `json:"no_retry,omitempty"`
	Status       int          `json:"status,omitempty"`
	Code         string       `json:"code,omitempty"`
	TokensUsed   int          `json:"tokens_used,omitempty"`
	TokensLimit  int          `json:"tokens_limit,omitempty"`
	Header       http.Header  `json:"header,omitempty"`
}

// Call is the per-request context sent with request-scoped methods.
type Call struct {
	ID        string                     `json:"id"`
	Caller    *plugin.Caller             `json:"caller,omitempty"`
	Protocol  string                     `json:"protocol,omitempty"`
	Model     string                     `json:"model,omitempty"`
	Stream    bool                       `json:"stream"`
	SessionID string                     `json:"session_id,omitempty"`
	ClientIP  string                     `json:"client_ip,omitempty"`
	Header    http.Header                `json:"header,omitempty"`
	Settings  map[string]json.RawMessage `json:"settings,omitempty"`
	// SettingLayers holds each layer's list of a strictest list set on
	// more than one layer; Settings then has their intersection.
	SettingLayers map[string][][]string `json:"setting_layers,omitempty"`
	// State is what this plugin's earlier hook returned for the same request.
	State json.RawMessage `json:"state,omitempty"`
	// Request is the decoded request when the manifest sets request_body.
	Request *ir.Request `json:"request,omitempty"`

	newState json.RawMessage
}

// Setting returns a merged setting value for this request.
func (c *Call) Setting(key string) plugin.Value { return plugin.NewValue(c.Settings[key]) }

// SetState stores v (JSON-encoded) and hands it back in later hooks of the
// same request as Call.State.
func (c *Call) SetState(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.newState = raw
	return nil
}

// Reply short-circuits a request in middleware.onRequest.
type Reply struct {
	Error    *Error       `json:"error,omitempty"`
	Response *ir.Response `json:"response,omitempty"`
}

// Deny is a Reply that rejects the request with an error.
func Deny(kind ir.ErrorKind, message string) *Reply {
	return &Reply{Error: &Error{Kind: kind, Message: message}}
}

// Candidate is one routing option given to selector.rank.
type Candidate struct {
	UpstreamID    string      `json:"upstream_id"`
	EndpointID    string      `json:"endpoint_id"`
	CredentialID  string      `json:"credential_id"`
	Protocol      string      `json:"protocol"`
	UpstreamModel string      `json:"upstream_model"`
	Priority      int         `json:"priority"`
	Weight        int         `json:"weight"`
	SameProtocol  bool        `json:"same_protocol"`
	Sticky        bool        `json:"sticky"`
	Fidelity      ir.Fidelity `json:"fidelity,omitempty"`
	LatencyMs     float64     `json:"latency_ms"`
	InFlight      int         `json:"in_flight"`
	PriceInMicro  int64       `json:"price_in_micro"`
	PriceOutMicro int64       `json:"price_out_micro"`
	QuotaReset    time.Time   `json:"quota_reset,omitzero"`
}

// Attempt is one try against one candidate.
type Attempt struct {
	N             int         `json:"n"`
	Candidate     Candidate   `json:"candidate"`
	Tier          plugin.Tier `json:"tier"`
	UpstreamModel string      `json:"upstream_model"`
	Header        http.Header `json:"header,omitempty"`
}

// Result summarizes a finished request for middleware.onFinish.
type Result struct {
	Status    int      `json:"status"`
	Error     *Error   `json:"error,omitempty"`
	Usage     ir.Usage `json:"usage"`
	Attempts  int      `json:"attempts"`
	Delivered bool     `json:"delivered"`
}

// Reservation is a meter hold.
type Reservation struct {
	ID          string `json:"id"`
	AmountMicro int64  `json:"amount_micro"`
}

// HTTPRequest is an HTTP request as JSON (auth.authenticate, admin.handle,
// host.http).
type HTTPRequest struct {
	Method     string      `json:"method"`
	URL        string      `json:"url"`
	Host       string      `json:"host,omitempty"`
	Path       string      `json:"path,omitempty"`
	Query      string      `json:"query,omitempty"`
	Header     http.Header `json:"header,omitempty"`
	RemoteAddr string      `json:"remote_addr,omitempty"`
	Body       *string     `json:"body,omitempty"`
	BodyBase64 string      `json:"body_base64,omitempty"`
	TimeoutMs  int64       `json:"timeout_ms,omitempty"`
	// UpstreamID and CredentialID (host.http only) send the request through
	// that upstream credential's egress, so its proxy applies.
	UpstreamID   string `json:"upstream_id,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
}

// HTTPResponse is an HTTP response as JSON.
type HTTPResponse struct {
	Status     int         `json:"status"`
	Header     http.Header `json:"header,omitempty"`
	Body       *string     `json:"body,omitempty"`
	BodyBase64 string      `json:"body_base64,omitempty"`
}

// Text returns a text body (Body); a binary body travels in BodyBase64
// and gives "".
func (r *HTTPResponse) Text() string {
	if r.Body != nil {
		return *r.Body
	}
	return ""
}

// TextBody returns a pointer for HTTPRequest.Body / HTTPResponse.Body.
func TextBody(s string) *string { return &s }

// Upstream is a configured upstream instance.
type Upstream struct {
	ID        string            `json:"id"`
	Provider  string            `json:"provider,omitempty"`
	Endpoints []plugin.Endpoint `json:"endpoints,omitempty"`
	Config    map[string]any    `json:"config,omitempty"`
}

// Credential is the secret for one attempt.
type Credential struct {
	ID         string            `json:"id"`
	UpstreamID string            `json:"upstream_id,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	OwnerID    string            `json:"owner_id,omitempty"`
	Secret     string            `json:"secret,omitempty"`
	Meta       map[string]string `json:"meta,omitempty"`
}

// Outgoing is the request of one attempt. In provider.prepare, change
// Header (and URL) in place; the host replaces its header with yours.
type Outgoing struct {
	Upstream   *Upstream        `json:"upstream,omitempty"`
	Endpoint   *plugin.Endpoint `json:"endpoint,omitempty"`
	Credential *Credential      `json:"credential,omitempty"`
	Model      string           `json:"model"`
	Method     string           `json:"method"`
	URL        string           `json:"url"`
	Header     http.Header      `json:"header,omitempty"`
	Body       json.RawMessage  `json:"body,omitempty"`
	BodyBase64 string           `json:"body_base64,omitempty"`
}

// Outcome is what came back from an attempt.
type Outcome struct {
	EndpointID   string      `json:"endpoint_id,omitempty"`
	CredentialID string      `json:"credential_id,omitempty"`
	Status       int         `json:"status"`
	Header       http.Header `json:"header,omitempty"`
	Body         string      `json:"body,omitempty"`
	Error        string      `json:"error,omitempty"`
	Signal       *Error      `json:"signal,omitempty"`
	Sent         bool        `json:"sent"`
}

// Verdict is a provider's judgment of a failed attempt.
type Verdict struct {
	Cause       plugin.Cause               `json:"cause"`
	Retry       plugin.RetryMode           `json:"retry"`
	Credential  plugin.CredAction          `json:"credential"`
	Until       time.Time                  `json:"until,omitzero"`
	ModelScope  bool                       `json:"model_scope,omitempty"`
	Downgrade   map[string]json.RawMessage `json:"downgrade,omitempty"`
	ClientError *Error                     `json:"client_error,omitempty"`
	Sent        bool                       `json:"sent"`
}

// Event is a neutral stream event (ir.Event with a wire error).
type Event struct {
	ir.Event
	Err *Error `json:"error,omitempty"`
}
