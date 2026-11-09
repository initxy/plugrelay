package anthropic

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/tidwall/gjson"
)

// Wire error types.
const (
	errTypeInvalid    = "invalid_request_error"
	errTypeAuth       = "authentication_error"
	errTypePermission = "permission_error"
	errTypeNotFound   = "not_found_error"
	errTypeRateLimit  = "rate_limit_error"
	errTypeOverloaded = "overloaded_error"
	errTypeAPI        = "api_error"
	errTypeTimeout    = "timeout_error"
	errTypeBilling    = "billing_error"
	errTypeTooLarge   = "request_too_large"
)

// statusOverloaded is the Messages API's non-standard overload status.
const statusOverloaded = 529

// promptTooLong is the wording clients match to start compacting history.
const promptTooLong = "prompt is too long"

// capabilityPrefix marks a request rejected because no upstream offers a
// capability it needs.
const capabilityPrefix = "capability_rejected: "

var tooLongRE = regexp.MustCompile(`(\d+)\s*tokens\s*>\s*(\d+)`)

// EncodeError renders a relay error with the status, type and wording
// clients of this protocol react to.
func (Codec) EncodeError(e *ir.GwError) (int, http.Header, []byte) {
	status, typ := errorClass(e.Kind)
	if e.Status != 0 {
		status = e.Status
	}
	if e.Kind == ir.ErrQuotaExhausted && e.Code == errTypeBilling {
		typ = errTypeBilling
	}
	h := http.Header{}
	for k, vs := range e.Header {
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	h.Set("Content-Type", "application/json")
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
	if e.NoRetry || e.Kind == ir.ErrQuotaExhausted {
		h.Set("X-Should-Retry", "false")
	}
	return status, h, errorBody(e, typ)
}

// errorBody renders the error JSON shared by responses and stream events.
func errorBody(e *ir.GwError, typ string) []byte {
	o := newObject()
	o.setv("type", "error")
	eo := newObject()
	eo.setv("type", typ)
	eo.setv("message", errorMessage(e))
	o.set("error", eo.bytes())
	if id := e.Header.Get("request-id"); id != "" {
		o.setv("request_id", id)
	}
	return o.bytes()
}

func errorMessage(e *ir.GwError) string {
	msg := e.Message
	switch e.Kind {
	case ir.ErrContextTooLong:
		if e.TokensUsed > 0 && e.TokensLimit > 0 {
			return fmt.Sprintf("%s: %d tokens > %d maximum", promptTooLong, e.TokensUsed, e.TokensLimit)
		}
		if msg == "" {
			return promptTooLong
		}
		if !strings.Contains(msg, promptTooLong) {
			return promptTooLong + ": " + msg
		}
	case ir.ErrCapabilityMissing:
		if !strings.HasPrefix(msg, capabilityPrefix) {
			return capabilityPrefix + msg
		}
	case ir.ErrQuotaExhausted:
		if !e.ResetAt.IsZero() {
			return fmt.Sprintf("%s (resets at %s)", msg, e.ResetAt.UTC().Format(time.RFC3339))
		}
	}
	return msg
}

// errorClass maps a kind to its default status and wire type.
func errorClass(k ir.ErrorKind) (int, string) {
	switch k {
	case ir.ErrInvalidRequest, ir.ErrContextTooLong, ir.ErrContentFiltered, ir.ErrCapabilityMissing:
		return http.StatusBadRequest, errTypeInvalid
	case ir.ErrAuthentication:
		return http.StatusUnauthorized, errTypeAuth
	case ir.ErrPermission:
		return http.StatusForbidden, errTypePermission
	case ir.ErrNotFound:
		return http.StatusNotFound, errTypeNotFound
	case ir.ErrRateLimited, ir.ErrQuotaExhausted:
		return http.StatusTooManyRequests, errTypeRateLimit
	case ir.ErrOverloaded:
		return statusOverloaded, errTypeOverloaded
	case ir.ErrTimeout:
		return http.StatusGatewayTimeout, errTypeTimeout
	}
	return http.StatusInternalServerError, errTypeAPI
}

// passHeaders are the upstream error headers worth forwarding to clients.
var passHeaders = []string{"Retry-After", "X-Should-Retry", "Request-Id"}

// DecodeError extracts the signal and message from an upstream error.
func (Codec) DecodeError(status int, header http.Header, body []byte) *ir.GwError {
	e := &ir.GwError{Upstream: body}
	typ, msg := "", strings.TrimSpace(string(body))
	if gjson.ValidBytes(body) {
		root := gjson.ParseBytes(body)
		typ = root.Get("error.type").String()
		if m := root.Get("error.message"); m.Exists() {
			msg = m.String()
		}
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	e.Message, e.Code = msg, typ
	e.Kind = kindOf(typ, status)
	if e.Kind == ir.ErrInvalidRequest && strings.Contains(msg, promptTooLong) {
		e.Kind = ir.ErrContextTooLong
		if m := tooLongRE.FindStringSubmatch(msg); m != nil {
			e.TokensUsed, _ = strconv.Atoi(m[1])
			e.TokensLimit, _ = strconv.Atoi(m[2])
		}
	}
	if header != nil {
		e.RetryAfter = retryAfter(header.Get("Retry-After"))
		e.NoRetry = strings.EqualFold(header.Get("X-Should-Retry"), "false")
		e.Header = http.Header{}
		for k, vs := range header {
			ck := http.CanonicalHeaderKey(k)
			keep := strings.HasPrefix(strings.ToLower(ck), "anthropic-ratelimit-")
			for _, p := range passHeaders {
				keep = keep || ck == p
			}
			if keep {
				e.Header[ck] = append([]string(nil), vs...)
			}
		}
	}
	return e
}

func kindOf(typ string, status int) ir.ErrorKind {
	switch typ {
	case errTypeInvalid, errTypeTooLarge:
		return ir.ErrInvalidRequest
	case errTypeAuth:
		return ir.ErrAuthentication
	case errTypePermission:
		return ir.ErrPermission
	case errTypeNotFound:
		return ir.ErrNotFound
	case errTypeRateLimit:
		return ir.ErrRateLimited
	case errTypeOverloaded:
		return ir.ErrOverloaded
	case errTypeTimeout:
		return ir.ErrTimeout
	case errTypeBilling:
		return ir.ErrQuotaExhausted
	case errTypeAPI:
		return ir.ErrUpstream
	}
	switch {
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge:
		return ir.ErrInvalidRequest
	case status == http.StatusUnauthorized:
		return ir.ErrAuthentication
	case status == http.StatusForbidden:
		return ir.ErrPermission
	case status == http.StatusNotFound:
		return ir.ErrNotFound
	case status == http.StatusTooManyRequests:
		return ir.ErrRateLimited
	case status == statusOverloaded || status == http.StatusServiceUnavailable:
		return ir.ErrOverloaded
	case status == http.StatusGatewayTimeout:
		return ir.ErrTimeout
	}
	return ir.ErrUpstream
}

// retryAfter parses delay-seconds or an HTTP date.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil && s > 0 {
		return time.Duration(s * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
