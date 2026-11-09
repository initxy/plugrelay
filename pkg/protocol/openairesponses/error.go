package openairesponses

import (
	"encoding/json"
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

// Error codes clients of this protocol react to.
const (
	codeContextLength = "context_length_exceeded"
	codeRateLimit     = "rate_limit_exceeded"
	codeQuota         = "insufficient_quota"
	codeUsageLimit    = "usage_limit_reached"
	codeOverloaded    = "server_is_overloaded"
)

type errInfo struct {
	status  int
	typ     string
	code    string
	message string
}

// errorInfo maps a relay error to its wire signal.
func errorInfo(e *ir.GwError) errInfo {
	info := errInfo{message: e.Message}
	switch e.Kind {
	case ir.ErrInvalidRequest:
		info = errInfo{400, "invalid_request_error", e.Code, e.Message}
	case ir.ErrContextTooLong:
		info = errInfo{400, "invalid_request_error", codeContextLength, e.Message}
		if info.message == "" {
			info.message = "Your input exceeds the context window of this model."
		}
		if e.TokensUsed > 0 && e.TokensLimit > 0 && !strings.Contains(info.message, strconv.Itoa(e.TokensUsed)) {
			info.message += fmt.Sprintf(" (%d tokens > %d maximum)", e.TokensUsed, e.TokensLimit)
		}
	case ir.ErrAuthentication:
		info = errInfo{401, "invalid_request_error", orDefault(e.Code, "invalid_api_key"), e.Message}
	case ir.ErrPermission:
		info = errInfo{403, "invalid_request_error", orDefault(e.Code, "permission_denied"), e.Message}
	case ir.ErrNotFound:
		info = errInfo{404, "invalid_request_error", orDefault(e.Code, "model_not_found"), e.Message}
	case ir.ErrRateLimited:
		info = errInfo{429, "rate_limit_error", codeRateLimit, e.Message}
		if e.RetryAfter > 0 && !strings.Contains(info.message, "try again in") {
			info.message = strings.TrimSpace(info.message + fmt.Sprintf(" Please try again in %.3fs.", e.RetryAfter.Seconds()))
		}
	case ir.ErrQuotaExhausted:
		code := codeQuota
		if e.Code == codeUsageLimit {
			code = codeUsageLimit
		}
		info = errInfo{429, code, code, e.Message}
	case ir.ErrOverloaded:
		info = errInfo{503, "server_error", codeOverloaded, e.Message}
	case ir.ErrContentFiltered:
		info = errInfo{400, "invalid_request_error", orDefault(e.Code, "content_filter"), e.Message}
	case ir.ErrCapabilityMissing:
		info = errInfo{400, "invalid_request_error", e.Code, e.Message}
	case ir.ErrTimeout:
		info = errInfo{504, "server_error", orDefault(e.Code, "timeout"), e.Message}
	case ir.ErrUpstream:
		info = errInfo{502, "server_error", orDefault(e.Code, "upstream_error"), e.Message}
	default:
		info = errInfo{500, "server_error", orDefault(e.Code, "server_error"), e.Message}
	}
	if e.Status != 0 {
		info.status = e.Status
	}
	if info.message == "" {
		info.message = http.StatusText(info.status)
	}
	return info
}

func orDefault(s, d string) string {
	if s != "" {
		return s
	}
	return d
}

func errorBody(info errInfo, e *ir.GwError) []byte {
	o := newObject()
	o.str("message", info.message)
	o.str("type", info.typ)
	o.set("param", json.RawMessage("null"))
	if info.code != "" {
		o.str("code", info.code)
	} else {
		o.set("code", json.RawMessage("null"))
	}
	if e != nil && !e.ResetAt.IsZero() {
		o.num("resets_at", e.ResetAt.Unix())
		o.num("resets_in_seconds", int64(math.Max(0, math.Ceil(time.Until(e.ResetAt).Seconds()))))
	}
	w := newObject()
	w.set("error", o.raw())
	return w.bytes()
}

// EncodeError renders a relay-generated error in the OpenAI error format.
func (Codec) EncodeError(e *ir.GwError) (int, http.Header, []byte) {
	if e == nil {
		e = ir.Errorf(ir.ErrInternal, "unknown error")
	}
	info := errorInfo(e)
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
	if e.NoRetry {
		h.Set("x-should-retry", "false")
	}
	return info.status, h, errorBody(info, e)
}

var tryAgainRe = regexp.MustCompile(`(?i)try again in ([0-9.]+)\s*(ms|s|seconds?)`)

// DecodeError extracts the signal and message from an upstream error.
func (Codec) DecodeError(status int, header http.Header, body []byte) *ir.GwError {
	root := gjson.ParseBytes(body)
	ev := root.Get("error")
	if !ev.IsObject() {
		if root.IsObject() && root.Get("message").Exists() {
			ev = root
		} else if ev.Type == gjson.String {
			ev = gjson.Parse(`{"message":` + ev.Raw + `}`)
		}
	}
	msg := ev.Get("message").Str
	code := ev.Get("code").String()
	typ := ev.Get("type").Str
	if msg == "" {
		msg = strings.TrimSpace(string(body))
		if msg == "" {
			msg = http.StatusText(status)
		}
	}
	e := &ir.GwError{Kind: classify(status, code, typ, msg), Message: msg, Code: code, Upstream: body}
	if e.Code == "" && (typ == codeUsageLimit || typ == codeQuota) {
		e.Code = typ
	}
	if ra := retryAfter(header, msg); ra > 0 {
		e.RetryAfter = ra
	}
	if r := ev.Get("resets_at"); r.Type == gjson.Number {
		e.ResetAt = time.Unix(r.Int(), 0)
	}
	if e.Kind == ir.ErrQuotaExhausted {
		e.NoRetry = true
	}
	e.Header = passHeaders(header)
	return e
}

func classify(status int, code, typ, msg string) ir.ErrorKind {
	lc := strings.ToLower(msg)
	switch {
	case code == codeContextLength || strings.Contains(lc, "context length") ||
		strings.Contains(lc, "context window") || strings.Contains(lc, "maximum context"):
		return ir.ErrContextTooLong
	case code == codeQuota || code == codeUsageLimit || typ == codeQuota || typ == codeUsageLimit ||
		code == "usage_not_included":
		return ir.ErrQuotaExhausted
	case code == codeOverloaded || code == "slow_down" || status == 503 || status == 529:
		return ir.ErrOverloaded
	case code == codeRateLimit || status == 429:
		return ir.ErrRateLimited
	case code == "content_filter" || code == "content_policy_violation":
		return ir.ErrContentFiltered
	case status == 401:
		return ir.ErrAuthentication
	case status == 403:
		return ir.ErrPermission
	case status == 404 || code == "model_not_found":
		return ir.ErrNotFound
	case status == 408 || status == 504:
		return ir.ErrTimeout
	case status >= 400 && status < 500:
		return ir.ErrInvalidRequest
	}
	return ir.ErrUpstream
}

func retryAfter(h http.Header, msg string) time.Duration {
	if v := h.Get("retry-after-ms"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return time.Duration(f * float64(time.Millisecond))
		}
	}
	if v := h.Get("Retry-After"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	if m := tryAgainRe.FindStringSubmatch(msg); m != nil {
		f, _ := strconv.ParseFloat(m[1], 64)
		if strings.HasPrefix(strings.ToLower(m[2]), "ms") {
			return time.Duration(f * float64(time.Millisecond))
		}
		return time.Duration(f * float64(time.Second))
	}
	return 0
}

// passHeaders keeps the upstream headers a client of this protocol uses.
func passHeaders(h http.Header) http.Header {
	out := http.Header{}
	for k, vs := range h {
		lk := strings.ToLower(k)
		if lk == "retry-after" || lk == "retry-after-ms" || lk == "x-request-id" ||
			strings.HasPrefix(lk, "x-ratelimit-") {
			out[k] = append([]string(nil), vs...)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// streamError renders the error object of a response.failed event.
func streamError(e *ir.GwError) json.RawMessage {
	info := errorInfo(e)
	o := newObject()
	code := info.code
	if code == "" {
		code = info.typ
	}
	o.str("code", code)
	o.str("message", info.message)
	return o.raw()
}

// errorFromObject maps a response.error / error event payload.
func errorFromObject(v gjson.Result) *ir.GwError {
	code := v.Get("code").String()
	msg := v.Get("message").Str
	typ := v.Get("type").Str
	if msg == "" {
		msg = "upstream response failed"
	}
	e := &ir.GwError{Kind: classify(0, code, typ, msg), Message: msg, Code: code, Upstream: []byte(v.Raw)}
	if ra := retryAfter(http.Header{}, msg); ra > 0 {
		e.RetryAfter = ra
	}
	return e
}
