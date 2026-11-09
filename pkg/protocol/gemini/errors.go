package gemini

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/ir"
)

// Google RPC detail type URLs.
const (
	typeRetryInfo    = "type.googleapis.com/google.rpc.RetryInfo"
	typeQuotaFailure = "type.googleapis.com/google.rpc.QuotaFailure"
	typeErrorInfo    = "type.googleapis.com/google.rpc.ErrorInfo"
)

// Quota ids written into QuotaFailure. Clients stop retrying on a quota id
// containing "PerDay" and back off on "PerMinute".
const (
	quotaPerDay    = "GenerateRequestsPerDayPerKey"
	quotaPerMinute = "GenerateRequestsPerMinutePerKey"
)

var rpcStatusByCode = map[int]string{
	400: "INVALID_ARGUMENT", 401: "UNAUTHENTICATED", 403: "PERMISSION_DENIED",
	404: "NOT_FOUND", 409: "ABORTED", 429: "RESOURCE_EXHAUSTED", 499: "CANCELLED",
	500: "INTERNAL", 501: "UNIMPLEMENTED", 503: "UNAVAILABLE", 504: "DEADLINE_EXCEEDED",
}

func kindStatus(k ir.ErrorKind) int {
	switch k {
	case ir.ErrInvalidRequest, ir.ErrContextTooLong, ir.ErrContentFiltered, ir.ErrCapabilityMissing:
		return 400
	case ir.ErrAuthentication:
		return 401
	case ir.ErrPermission:
		return 403
	case ir.ErrNotFound:
		return 404
	case ir.ErrRateLimited, ir.ErrQuotaExhausted:
		return 429
	case ir.ErrOverloaded:
		return 503
	case ir.ErrTimeout:
		return 504
	}
	return 500
}

func rpcStatus(code int) string {
	if s, ok := rpcStatusByCode[code]; ok {
		return s
	}
	if code >= 500 {
		return "INTERNAL"
	}
	return "UNKNOWN"
}

// EncodeError implements protocol.Codec.
func (Codec) EncodeError(e *ir.GwError) (int, http.Header, []byte) {
	code := kindStatus(e.Kind)
	if e.Status != 0 {
		code = e.Status
	}
	h := http.Header{}
	for k, vs := range e.Header {
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	h.Set("Content-Type", "application/json; charset=UTF-8")
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int((e.RetryAfter+time.Second-1)/time.Second)))
	}
	return code, h, errorBody(e, code)
}

// errorBody renders the {"error":{...}} object for e with the given code.
func errorBody(e *ir.GwError, code int) []byte {
	status := rpcStatus(code)
	if e.Code != "" && codeFor(e.Code) == code {
		status = e.Code
	}
	msg := e.Message
	if e.Kind == ir.ErrContextTooLong && e.TokensUsed > 0 && e.TokensLimit > 0 {
		msg = fmt.Sprintf("The input token count (%d) exceeds the maximum number of tokens allowed (%d).", e.TokensUsed, e.TokensLimit)
	}
	if e.Kind == ir.ErrQuotaExhausted && !e.ResetAt.IsZero() && !strings.Contains(msg, "reset") {
		msg += " Quota resets at " + e.ResetAt.UTC().Format(time.RFC3339) + "."
	}
	if msg == "" {
		msg = string(e.Kind)
	}

	var details [][]byte
	if e.Kind == ir.ErrQuotaExhausted {
		quota := quotaPerMinute
		if e.NoRetry {
			quota = quotaPerDay
		}
		v := setJSON([]byte("{}"), "quotaMetric", "generate_requests")
		v = setJSON(v, "quotaId", quota)
		v = setJSON(v, "description", msg)
		d := setJSON([]byte("{}"), "@type", typeQuotaFailure)
		details = append(details, setRaw(d, "violations", rawArray([][]byte{v})))
	}
	if (e.Kind == ir.ErrRateLimited || e.Kind == ir.ErrQuotaExhausted && !e.NoRetry) && e.RetryAfter > 0 {
		d := setJSON([]byte("{}"), "@type", typeRetryInfo)
		details = append(details, setJSON(d, "retryDelay", formatDelay(e.RetryAfter)))
	}

	obj := setJSON([]byte("{}"), "code", code)
	obj = setJSON(obj, "message", msg)
	obj = setJSON(obj, "status", status)
	if len(details) > 0 {
		obj = setRaw(obj, "details", rawArray(details))
	}
	return setRaw([]byte("{}"), "error", obj)
}

// codeFor returns the HTTP code of an RPC status name, or 0.
func codeFor(status string) int {
	for c, s := range rpcStatusByCode {
		if s == status {
			return c
		}
	}
	return 0
}

// formatDelay renders a google.protobuf.Duration ("12s", "1.5s").
func formatDelay(d time.Duration) string {
	s := strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
	return s + "s"
}

var tokenCounts = regexp.MustCompile(`\((\d+)\)[^()]*\((\d+)\)`)

// DecodeError implements protocol.Codec.
func (Codec) DecodeError(status int, header http.Header, body []byte) *ir.GwError {
	g := gjson.ParseBytes(body)
	if g.IsArray() {
		g = g.Get("0")
	}
	eo := g.Get("error")
	code := status
	if c := eo.Get("code").Int(); code == 0 && c > 0 {
		code = int(c)
	}
	rpc := eo.Get("status").String()
	if rpc == "" {
		rpc = rpcStatus(code)
	}
	e := &ir.GwError{Message: eo.Get("message").String(), Code: rpc, Upstream: body}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
	}
	if ra := header.Get("Retry-After"); ra != "" {
		e.Header = http.Header{"Retry-After": {ra}}
		if n, err := strconv.Atoi(ra); err == nil {
			e.RetryAfter = time.Duration(n) * time.Second
		}
	}
	perDay, keyInvalid := false, false
	eo.Get("details").ForEach(func(_, d gjson.Result) bool {
		switch d.Get("@type").String() {
		case typeRetryInfo:
			if dur, err := time.ParseDuration(d.Get("retryDelay").String()); err == nil && dur > 0 {
				e.RetryAfter = dur
			}
		case typeQuotaFailure:
			d.Get("violations").ForEach(func(_, v gjson.Result) bool {
				perDay = perDay || strings.Contains(v.Get("quotaId").String(), "PerDay")
				return true
			})
		case typeErrorInfo:
			keyInvalid = keyInvalid || d.Get("reason").String() == "API_KEY_INVALID"
		}
		return true
	})

	lower := strings.ToLower(e.Message)
	switch {
	case rpc == "UNAUTHENTICATED" || code == 401 || keyInvalid:
		e.Kind = ir.ErrAuthentication
	case rpc == "PERMISSION_DENIED" || code == 403:
		e.Kind = ir.ErrPermission
	case rpc == "NOT_FOUND" || code == 404:
		e.Kind = ir.ErrNotFound
	case rpc == "RESOURCE_EXHAUSTED" || code == 429:
		e.Kind = ir.ErrRateLimited
		if perDay {
			e.Kind, e.NoRetry = ir.ErrQuotaExhausted, true
		}
	case rpc == "INVALID_ARGUMENT" || rpc == "FAILED_PRECONDITION" || code == 400:
		e.Kind = ir.ErrInvalidRequest
		if strings.Contains(lower, "token count") || strings.Contains(lower, "maximum number of tokens") ||
			strings.Contains(lower, "too long") || strings.Contains(lower, "context length") {
			e.Kind = ir.ErrContextTooLong
			if m := tokenCounts.FindStringSubmatch(e.Message); m != nil {
				e.TokensUsed, _ = strconv.Atoi(m[1])
				e.TokensLimit, _ = strconv.Atoi(m[2])
			}
		}
	case rpc == "UNAVAILABLE" || code == 503:
		e.Kind = ir.ErrOverloaded
	case rpc == "DEADLINE_EXCEEDED" || code == 504:
		e.Kind = ir.ErrTimeout
	default:
		e.Kind = ir.ErrUpstream
	}
	return e
}
