package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/store"
)

// listFlag collects a repeatable flag; each value may also be
// comma-separated.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	*l = append(*l, splitCSV(v)...)
	return nil
}

// repeatFlag collects a repeatable flag without splitting on commas.
type repeatFlag []string

func (l *repeatFlag) String() string { return strings.Join(*l, " ") }

func (l *repeatFlag) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func splitCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseVars turns k=v pairs into a map; keys must be unique.
func parseVars(pairs []string) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("--var %q: want key=value", p)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("--var %s given twice", k)
		}
		out[k] = v
	}
	return out, nil
}

// parseExpires accepts a lifetime ("30d", "2w", "12h", "90m") or an
// absolute time (RFC 3339 or YYYY-MM-DD, UTC) and returns the expiry.
func parseExpires(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t.UTC(), nil
	}
	var d time.Duration
	if n, unit := s[:len(s)-1], s[len(s)-1]; unit == 'd' || unit == 'w' {
		v, err := strconv.Atoi(n)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid expiry %q", s)
		}
		d = time.Duration(v) * 24 * time.Hour
		if unit == 'w' {
			d *= 7
		}
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return time.Time{}, fmt.Errorf("invalid expiry %q: want 30d, 2w, 12h or a date", s)
		}
	}
	if d <= 0 {
		return time.Time{}, fmt.Errorf("invalid expiry %q: must be in the future", s)
	}
	return now.Add(d).UTC().Truncate(time.Second), nil
}

// parseRoute parses upstream:upstreamModel[:priority[:weight]]. The
// upstream model may itself contain colons ("ollama:qwen3:8b"); trailing
// integer parts are read as priority and weight only after a model part.
// An empty upstream model means the client-facing name.
func parseRoute(s string) (store.Route, error) {
	parts := strings.Split(s, ":")
	if parts[0] == "" {
		return store.Route{}, fmt.Errorf("--route %q: want upstream:upstream_model[:priority[:weight]]", s)
	}
	rt := store.Route{UpstreamID: parts[0]}
	rest := parts[1:]
	var nums []int
	for len(rest) > 1 && len(nums) < 2 {
		n, err := strconv.Atoi(rest[len(rest)-1])
		if err != nil {
			break
		}
		nums = append([]int{n}, nums...)
		rest = rest[:len(rest)-1]
	}
	rt.UpstreamModel = strings.Join(rest, ":")
	switch len(nums) {
	case 2:
		rt.Priority, rt.Weight = nums[0], nums[1]
	case 1:
		rt.Priority = nums[0]
	}
	if rt.Weight < 0 {
		return store.Route{}, fmt.Errorf("--route %q: weight must not be negative", s)
	}
	return rt, nil
}
