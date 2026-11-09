package openaichat

import (
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
)

// errorShape is the OpenAI status, error type and code for a Kind.
type errorShape struct {
	status int
	typ    string
	code   string
}

var errorShapes = map[ir.ErrorKind]errorShape{
	ir.ErrInvalidRequest:    {http.StatusBadRequest, "invalid_request_error", ""},
	ir.ErrContextTooLong:    {http.StatusBadRequest, "invalid_request_error", "context_length_exceeded"},
	ir.ErrAuthentication:    {http.StatusUnauthorized, "invalid_request_error", "invalid_api_key"},
	ir.ErrPermission:        {http.StatusForbidden, "permission_error", "permission_denied"},
	ir.ErrNotFound:          {http.StatusNotFound, "invalid_request_error", "model_not_found"},
	ir.ErrRateLimited:       {http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded"},
	ir.ErrQuotaExhausted:    {http.StatusTooManyRequests, "insufficient_quota", "insufficient_quota"},
	ir.ErrOverloaded:        {http.StatusServiceUnavailable, "server_error", "server_is_overloaded"},
	ir.ErrContentFiltered:   {http.StatusBadRequest, "invalid_request_error", "content_filter"},
	ir.ErrCapabilityMissing: {http.StatusBadRequest, "invalid_request_error", ""},
	ir.ErrTimeout:           {http.StatusGatewayTimeout, "server_error", "timeout"},
	ir.ErrInternal:          {http.StatusInternalServerError, "server_error", ""},
	ir.ErrUpstream:          {http.StatusBadGateway, "server_error", "upstream_error"},
}

func shapeOf(e *ir.GwError) errorShape {
	s, ok := errorShapes[e.Kind]
	if !ok {
		s = errorShapes[ir.ErrInternal]
	}
	if e.Status != 0 {
		s.status = e.Status
	}
	if e.Code != "" {
		s.code = e.Code
	}
	return s
}

// errorBody renders {"error":{"message","type","param","code"}}.
func errorBody(e *ir.GwError) object {
	s := shapeOf(e)
	msg := e.Message
	if e.Kind == ir.ErrContextTooLong && msg == "" && e.TokensLimit > 0 {
		msg = "This model's maximum context length is " + strconv.Itoa(e.TokensLimit) +
			" tokens. However, your messages resulted in " + strconv.Itoa(e.TokensUsed) + " tokens."
	}
	var code any
	if s.code != "" {
		code = s.code
	}
	return object{{"error", object{{"message", msg}, {"type", s.typ}, {"param", nil}, {"code", code}}}}
}

// EncodeError renders a relay-generated error in the OpenAI error format.
func (Codec) EncodeError(e *ir.GwError) (int, http.Header, []byte) {
	s := shapeOf(e)
	h := http.Header{}
	for k, v := range e.Header {
		h[k] = append([]string(nil), v...)
	}
	h.Set("Content-Type", "application/json")
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
	if e.NoRetry || e.Kind == ir.ErrQuotaExhausted {
		h.Set("X-Should-Retry", "false")
	}
	b, _ := marshal(errorBody(e))
	return s.status, h, b
}

var (
	ctxLimitRe = regexp.MustCompile(`maximum context length is (\d+) tokens`)
	ctxUsedRe  = regexp.MustCompile(`(?:resulted in|requested) (\d+) tokens`)
)

// keptHeader reports whether an upstream response header is worth passing to clients.
func keptHeader(k string) bool {
	k = strings.ToLower(k)
	switch k {
	case "retry-after", "retry-after-ms", "x-request-id", "x-should-retry":
		return true
	}
	return strings.HasPrefix(k, "x-ratelimit-")
}

// DecodeError maps an upstream error to a Kind, keeping its body and code.
// status 0 means an error object that arrived inside a stream.
func (Codec) DecodeError(status int, header http.Header, body []byte) *ir.GwError {
	var w struct {
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(body, &w)
	var detail struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	}
	if json.Unmarshal(w.Error, &detail) != nil {
		detail.Message = str(w.Error)
	}
	if detail.Message == "" && len(w.Error) == 0 {
		_ = json.Unmarshal(body, &detail)
	}
	code := str(detail.Code)
	if code == "" && len(detail.Code) > 0 && !isNull(detail.Code) {
		code = string(detail.Code)
	}
	e := &ir.GwError{Kind: classify(status, detail.Type, code, detail.Message), Message: detail.Message,
		Code: code, Upstream: append([]byte(nil), body...)}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
		if len(e.Message) > 500 {
			e.Message = e.Message[:500]
		}
		if e.Message == "" {
			e.Message = http.StatusText(status)
		}
	}
	if e.Kind == ir.ErrContextTooLong {
		if m := ctxLimitRe.FindStringSubmatch(e.Message); m != nil {
			e.TokensLimit, _ = strconv.Atoi(m[1])
		}
		if m := ctxUsedRe.FindStringSubmatch(e.Message); m != nil {
			e.TokensUsed, _ = strconv.Atoi(m[1])
		}
	}
	if e.Kind == ir.ErrQuotaExhausted {
		e.NoRetry = true
	}
	for k, v := range header {
		if keptHeader(k) {
			if e.Header == nil {
				e.Header = http.Header{}
			}
			e.Header[k] = append([]string(nil), v...)
		}
	}
	if ms, err := strconv.Atoi(header.Get("retry-after-ms")); err == nil && ms > 0 {
		e.RetryAfter = time.Duration(ms) * time.Millisecond
	} else if s, err := strconv.Atoi(header.Get("retry-after")); err == nil && s > 0 {
		e.RetryAfter = time.Duration(s) * time.Second
	}
	return e
}

func classify(status int, typ, code, msg string) ir.ErrorKind {
	lmsg := strings.ToLower(msg)
	switch {
	case code == "context_length_exceeded" || strings.Contains(lmsg, "maximum context length") ||
		strings.Contains(lmsg, "context length exceeded") || strings.Contains(lmsg, "context_length_exceeded"):
		return ir.ErrContextTooLong
	case code == "insufficient_quota" || typ == "insufficient_quota" || code == "usage_limit_reached":
		return ir.ErrQuotaExhausted
	case code == "content_filter" || code == "content_policy_violation":
		return ir.ErrContentFiltered
	case code == "rate_limit_exceeded" || typ == "rate_limit_error" || typ == "requests" || typ == "tokens":
		return ir.ErrRateLimited
	case code == "server_is_overloaded" || typ == "overloaded_error":
		return ir.ErrOverloaded
	case code == "invalid_api_key" || typ == "authentication_error":
		return ir.ErrAuthentication
	case code == "model_not_found":
		return ir.ErrNotFound
	}
	switch {
	case status == http.StatusTooManyRequests:
		return ir.ErrRateLimited
	case status == http.StatusUnauthorized:
		return ir.ErrAuthentication
	case status == http.StatusForbidden:
		return ir.ErrPermission
	case status == http.StatusNotFound:
		return ir.ErrNotFound
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return ir.ErrTimeout
	case status == http.StatusServiceUnavailable || status == 529:
		return ir.ErrOverloaded
	case status >= 400 && status < 500:
		return ir.ErrInvalidRequest
	case status == 0 && typ == "invalid_request_error":
		return ir.ErrInvalidRequest
	}
	return ir.ErrUpstream
}
