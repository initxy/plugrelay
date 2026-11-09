package extplugin

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Wire types mirror sdk/PROTOCOL.md. They are deliberately separate from the
// pkg/plugin structs so the protocol can stay stable while Go types evolve,
// and so durations travel as milliseconds instead of Go nanoseconds.

// ProtocolVersion is the plugin protocol major version this host speaks.
const ProtocolVersion = 1

type wireError struct {
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

func toWireError(e *ir.GwError) *wireError {
	if e == nil {
		return nil
	}
	return &wireError{
		Kind: e.Kind, Message: e.Message, RetryAfterMs: e.RetryAfter.Milliseconds(),
		ResetAt: e.ResetAt, NoRetry: e.NoRetry, Status: e.Status, Code: e.Code,
		TokensUsed: e.TokensUsed, TokensLimit: e.TokensLimit, Header: e.Header,
	}
}

func (w *wireError) gw() *ir.GwError {
	if w == nil {
		return nil
	}
	kind := w.Kind
	if kind == "" {
		kind = ir.ErrInternal
	}
	return &ir.GwError{
		Kind: kind, Message: w.Message, RetryAfter: time.Duration(w.RetryAfterMs) * time.Millisecond,
		ResetAt: w.ResetAt, NoRetry: w.NoRetry, Status: w.Status, Code: w.Code,
		TokensUsed: w.TokensUsed, TokensLimit: w.TokensLimit, Header: w.Header,
	}
}

type wireCall struct {
	ID        string                     `json:"id"`
	Caller    *plugin.Caller             `json:"caller,omitempty"`
	Protocol  string                     `json:"protocol,omitempty"`
	Model     string                     `json:"model,omitempty"`
	Stream    bool                       `json:"stream"`
	SessionID string                     `json:"session_id,omitempty"`
	ClientIP  string                     `json:"client_ip,omitempty"`
	Header    http.Header                `json:"header,omitempty"`
	Settings  map[string]json.RawMessage `json:"settings,omitempty"`
	// SettingLayers holds, for a strictest list set on more than one
	// layer, each layer's list: every one must allow an entry.
	SettingLayers map[string][][]string `json:"setting_layers,omitempty"`
	State         json.RawMessage       `json:"state,omitempty"`
	Request       *ir.Request           `json:"request,omitempty"`
}

type wireHTTPRequest struct {
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
	// that credential's egress (its proxy, else the upstream's).
	UpstreamID   string `json:"upstream_id,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
}

type wireHTTPResponse struct {
	Status     int         `json:"status"`
	Header     http.Header `json:"header,omitempty"`
	Body       *string     `json:"body,omitempty"`
	BodyBase64 string      `json:"body_base64,omitempty"`
}

type wireReply struct {
	Error    *wireError   `json:"error,omitempty"`
	Response *ir.Response `json:"response,omitempty"`
}

type wireCandidate struct {
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

func toWireCandidate(c plugin.Candidate) wireCandidate {
	return wireCandidate{
		UpstreamID: c.UpstreamID, EndpointID: c.EndpointID, CredentialID: c.CredentialID,
		Protocol: c.Protocol, UpstreamModel: c.UpstreamModel, Priority: c.Priority, Weight: c.Weight,
		SameProtocol: c.SameProtocol, Sticky: c.Sticky, Fidelity: c.Fidelity, LatencyMs: c.LatencyMs,
		InFlight: c.InFlight, PriceInMicro: c.PriceInMicro, PriceOutMicro: c.PriceOutMicro, QuotaReset: c.QuotaReset,
	}
}

type wireAttempt struct {
	N             int           `json:"n"`
	Candidate     wireCandidate `json:"candidate"`
	Tier          plugin.Tier   `json:"tier"`
	UpstreamModel string        `json:"upstream_model"`
	Header        http.Header   `json:"header,omitempty"`
}

type wireResult struct {
	Status    int        `json:"status"`
	Error     *wireError `json:"error,omitempty"`
	Usage     ir.Usage   `json:"usage"`
	Attempts  int        `json:"attempts"`
	Delivered bool       `json:"delivered"`
}

type wireReservation struct {
	ID          string `json:"id"`
	AmountMicro int64  `json:"amount_micro"`
}

type wireUpstream struct {
	ID        string            `json:"id"`
	Provider  string            `json:"provider,omitempty"`
	Endpoints []plugin.Endpoint `json:"endpoints,omitempty"`
	Config    map[string]any    `json:"config,omitempty"`
}

func toWireUpstream(u *plugin.Upstream) *wireUpstream {
	if u == nil {
		return nil
	}
	return &wireUpstream{ID: u.ID, Provider: u.Provider, Endpoints: u.Endpoints, Config: u.Config}
}

type wireCredential struct {
	ID         string            `json:"id"`
	UpstreamID string            `json:"upstream_id,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	OwnerID    string            `json:"owner_id,omitempty"`
	Secret     string            `json:"secret,omitempty"`
	Meta       map[string]string `json:"meta,omitempty"`
}

func toWireCredential(c *plugin.Credential) *wireCredential {
	if c == nil {
		return nil
	}
	return &wireCredential{ID: c.ID, UpstreamID: c.UpstreamID, Kind: c.Kind, OwnerID: c.OwnerID, Secret: c.Secret, Meta: c.Meta}
}

type wireOutgoing struct {
	Upstream   *wireUpstream    `json:"upstream,omitempty"`
	Endpoint   *plugin.Endpoint `json:"endpoint,omitempty"`
	Credential *wireCredential  `json:"credential,omitempty"`
	Model      string           `json:"model"`
	Method     string           `json:"method"`
	URL        string           `json:"url"`
	Header     http.Header      `json:"header,omitempty"`
	// Body is set only for provider.send: raw JSON when the body is JSON,
	// otherwise BodyBase64.
	Body       json.RawMessage `json:"body,omitempty"`
	BodyBase64 string          `json:"body_base64,omitempty"`
}

type wireOutcome struct {
	EndpointID   string      `json:"endpoint_id,omitempty"`
	CredentialID string      `json:"credential_id,omitempty"`
	Status       int         `json:"status"`
	Header       http.Header `json:"header,omitempty"`
	Body         string      `json:"body,omitempty"`
	Error        string      `json:"error,omitempty"`
	Signal       *wireError  `json:"signal,omitempty"`
	Sent         bool        `json:"sent"`
}

type wireVerdict struct {
	Cause       plugin.Cause               `json:"cause"`
	Retry       plugin.RetryMode           `json:"retry"`
	Credential  plugin.CredAction          `json:"credential"`
	Until       time.Time                  `json:"until,omitzero"`
	ModelScope  bool                       `json:"model_scope,omitempty"`
	Downgrade   map[string]json.RawMessage `json:"downgrade,omitempty"`
	ClientError *wireError                 `json:"client_error,omitempty"`
	Sent        bool                       `json:"sent"`
}

func (w wireVerdict) verdict() plugin.Verdict {
	v := plugin.Verdict{
		Cause: w.Cause, Retry: w.Retry, Credential: w.Credential, Until: w.Until,
		ModelScope: w.ModelScope, Downgrade: w.Downgrade, ClientErr: w.ClientError.gw(), Sent: w.Sent,
	}
	// Unknown or empty enum values degrade to the most conservative meaning.
	switch v.Cause {
	case plugin.CauseRequest, plugin.CauseTransient, plugin.CauseRateLimit, plugin.CauseQuota,
		plugin.CauseAuthExpired, plugin.CauseBanned, plugin.CauseContent, plugin.CauseUnknown:
	default:
		v.Cause = plugin.CauseUnknown
	}
	switch v.Retry {
	case plugin.RetryNext, plugin.RetryDowngrade, plugin.RetryNone:
	default:
		v.Retry = plugin.RetryNone
	}
	switch v.Credential {
	case plugin.CredCooldown, plugin.CredRefresh, plugin.CredQuarantine, plugin.CredManual, plugin.CredKeep:
	default:
		v.Credential = plugin.CredKeep
	}
	return v
}

// wireEvent is ir.Event with its error in wire form; the outer Err field
// shadows the embedded one in JSON.
type wireEvent struct {
	ir.Event
	Err *wireError `json:"error,omitempty"`
}

func (w *wireEvent) event() *ir.Event {
	ev := w.Event
	ev.Err = w.Err.gw()
	return &ev
}

type initializeParams struct {
	ProtocolVersion int                        `json:"protocol_version"`
	HostVersion     string                     `json:"host_version"`
	PluginID        string                     `json:"plugin_id"`
	Settings        map[string]json.RawMessage `json:"settings,omitempty"`
}

type initializeResult struct {
	ProtocolVersion int `json:"protocol_version"`
	Manifest        struct {
		ID      string `json:"id"`
		Version string `json:"version,omitempty"`
	} `json:"manifest"`
	Capabilities struct {
		Points []string `json:"points"`
	} `json:"capabilities"`
}
