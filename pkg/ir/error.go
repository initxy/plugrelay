package ir

import (
	"fmt"
	"net/http"
	"time"
)

// ErrorKind is a client-actionable error signal. Each codec maps it to the
// status code, error type and wording its clients react to (for example
// "prompt is too long" triggers Claude Code's auto-compaction).
type ErrorKind string

const (
	ErrInvalidRequest    ErrorKind = "invalid_request"
	ErrContextTooLong    ErrorKind = "context_too_long"
	ErrAuthentication    ErrorKind = "authentication"
	ErrPermission        ErrorKind = "permission"
	ErrNotFound          ErrorKind = "not_found"
	ErrRateLimited       ErrorKind = "rate_limited"
	ErrQuotaExhausted    ErrorKind = "quota_exhausted"
	ErrOverloaded        ErrorKind = "overloaded"
	ErrContentFiltered   ErrorKind = "content_filtered"
	ErrCapabilityMissing ErrorKind = "capability_missing"
	ErrTimeout           ErrorKind = "timeout"
	ErrInternal          ErrorKind = "internal"
	ErrUpstream          ErrorKind = "upstream" // upstream failure with no finer signal
)

// GwError is an error plugrelay returns to a client.
type GwError struct {
	Kind    ErrorKind `json:"kind"`
	Message string    `json:"message"`

	// RetryAfter, when > 0, is sent as the protocol's retry hint.
	RetryAfter time.Duration `json:"retry_after,omitempty"`
	// ResetAt is when a quota window reopens (zero if unknown).
	ResetAt time.Time `json:"reset_at,omitzero"`
	// NoRetry tells clients not to retry (x-should-retry: false, PerDay quota).
	NoRetry bool `json:"no_retry,omitempty"`

	// TokensUsed and TokensLimit fill context-too-long messages.
	TokensUsed  int `json:"tokens_used,omitempty"`
	TokensLimit int `json:"tokens_limit,omitempty"`

	// Status overrides the codec's default status for Kind (0 = default).
	Status int `json:"status,omitempty"`
	// Header holds upstream headers to pass through (retry-after,
	// ratelimit and request-id headers), already filtered.
	Header http.Header `json:"-"`
	// Upstream is the upstream's error body verbatim, for logs.
	Upstream []byte `json:"-"`
	// Code is the upstream's own error code, if any, preserved for clients
	// of the same protocol family.
	Code string `json:"code,omitempty"`
}

func (e *GwError) Error() string { return fmt.Sprintf("%s: %s", e.Kind, e.Message) }

// Errorf builds a GwError.
func Errorf(kind ErrorKind, format string, a ...any) *GwError {
	return &GwError{Kind: kind, Message: fmt.Sprintf(format, a...)}
}
