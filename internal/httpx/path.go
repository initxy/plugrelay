package httpx

import "strings"

// versionPrefixes are the leading path segments clients add or omit
// depending on how their base URL was configured.
var versionPrefixes = []string{"/v1beta", "/v1"}

// CleanPath collapses repeated slashes and drops a trailing slash. It does
// not resolve "." or ".." (routes never contain them, so they will not match).
func CleanPath(p string) string {
	if p == "" {
		return "/"
	}
	var b strings.Builder
	b.Grow(len(p) + 1)
	if p[0] != '/' {
		b.WriteByte('/')
	}
	prev := byte(0)
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '/' && prev == '/' {
			continue
		}
		b.WriteByte(c)
		prev = c
	}
	out := b.String()
	if len(out) > 1 {
		out = strings.TrimSuffix(out, "/")
	}
	return out
}

func stripVersion(p string) (string, bool) {
	for _, v := range versionPrefixes {
		if p == v {
			return "/", true
		}
		if strings.HasPrefix(p, v+"/") {
			return p[len(v):], true
		}
	}
	return p, false
}

// PathMatcher matches request paths against canonical route patterns.
//
// A pattern segment is a literal, "{name}" (any one segment), or
// "{name}:suffix" (a segment ending in ":suffix", as in
// "/v1beta/models/{model}:generateContent").
type PathMatcher struct {
	pats []pattern
}

type pattern struct {
	canonical string
	exact     []string // segments of the canonical path
	bare      []string // segments with the version prefix removed
}

// NewPathMatcher compiles canonical paths. Earlier paths win ties.
func NewPathMatcher(canonical ...string) *PathMatcher {
	m := &PathMatcher{}
	for _, c := range canonical {
		bare, _ := stripVersion(c)
		m.pats = append(m.pats, pattern{canonical: c, exact: segments(c), bare: segments(bare)})
	}
	return m
}

func segments(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// Match finds the canonical path for a request path. It tolerates repeated
// slashes, a trailing slash, a missing or extra version prefix ("/messages",
// "/v1/v1/messages" both match "/v1/messages"), and ignores the query string.
// Exact matches win over version-tolerant ones.
func (m *PathMatcher) Match(path string) (canonical string, params map[string]string, ok bool) {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	p := CleanPath(path)
	for {
		inner, had := stripVersion(p)
		if !had {
			break
		}
		if _, again := stripVersion(inner); !again {
			break
		}
		p = inner
	}
	segs := segments(p)
	for _, pt := range m.pats {
		if params, ok := matchSegs(pt.exact, segs); ok {
			return pt.canonical, params, true
		}
	}
	bare, _ := stripVersion(p)
	bsegs := segments(bare)
	for _, pt := range m.pats {
		if params, ok := matchSegs(pt.bare, bsegs); ok {
			return pt.canonical, params, true
		}
	}
	return "", nil, false
}

func matchSegs(pat, segs []string) (map[string]string, bool) {
	if len(pat) != len(segs) {
		return nil, false
	}
	var params map[string]string
	for i, ps := range pat {
		s := segs[i]
		if !strings.HasPrefix(ps, "{") {
			if ps != s {
				return nil, false
			}
			continue
		}
		end := strings.IndexByte(ps, '}')
		if end < 0 {
			return nil, false
		}
		name, suffix := ps[1:end], ps[end+1:]
		if !strings.HasSuffix(s, suffix) || len(s) == len(suffix) {
			return nil, false
		}
		if params == nil {
			params = map[string]string{}
		}
		params[name] = s[:len(s)-len(suffix)]
	}
	return params, true
}
