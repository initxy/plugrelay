package compatible

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// passthroughHeaders keeps the upstream response headers a client may act
// on: retry hints, rate-limit state and request ids.
func passthroughHeaders(h http.Header) http.Header {
	out := http.Header{}
	for k, vs := range h {
		l := strings.ToLower(k)
		if l == "retry-after" || l == "retry-after-ms" || l == "x-should-retry" ||
			strings.Contains(l, "ratelimit") || strings.HasSuffix(l, "request-id") ||
			strings.HasSuffix(l, "requestid") { // AWS: x-amzn-RequestId
			out[k] = append([]string(nil), vs...)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// retryAfter reads retry-after-ms or retry-after (seconds or HTTP date).
func retryAfter(h http.Header, now time.Time) time.Duration {
	if v := h.Get("Retry-After-Ms"); v != "" {
		if ms, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if s, err := strconv.ParseFloat(v, 64); err == nil {
		if s <= 0 {
			return 0
		}
		return time.Duration(s * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// resetAt returns the latest reset time announced by any "*ratelimit*reset*"
// header. Values may be a duration ("6m0s", "20ms"), seconds from now, unix
// seconds, or RFC 3339.
func resetAt(h http.Header, now time.Time) time.Time {
	var latest time.Time
	for k, vs := range h {
		l := strings.ToLower(k)
		if !strings.Contains(l, "ratelimit") || !strings.Contains(l, "reset") {
			continue
		}
		for _, v := range vs {
			if t, ok := parseReset(strings.TrimSpace(v), now); ok && t.After(latest) {
				latest = t
			}
		}
	}
	return latest
}

func parseReset(v string, now time.Time) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		if f > 1e9 { // unix seconds
			sec := int64(f)
			return time.Unix(sec, int64((f-float64(sec))*1e9)), true
		}
		return now.Add(time.Duration(f * float64(time.Second))), true
	}
	if d, err := time.ParseDuration(v); err == nil {
		return now.Add(d), true
	}
	return time.Time{}, false
}
