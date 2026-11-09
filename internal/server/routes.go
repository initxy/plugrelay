package server

import (
	"net/http"
	"strings"

	"github.com/initxy/plugrelay/internal/httpx"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// Codec names the model-list dispatch picks between.
const (
	codecAnthropic = "anthropic"
	codecChat      = "openai-chat"
	codecResponses = "openai-responses"
	codecGemini    = "gemini"
)

// matched is a protocol route found for a request.
type matched struct {
	codec protocol.Codec
	route protocol.Route
}

type routeEntry struct {
	matched
	// action marks a "{model}:{action}" segment; the matched segment must
	// then carry a ":action" suffix.
	action bool
	// fixed is the action of a "{model}:embedContent" segment; such an
	// entry matches only that action and wins over a ":{action}" entry.
	fixed string
}

// routeTable resolves request paths to codec routes per method.
type routeTable struct {
	codecs map[string]protocol.Codec
	// byMethod matches paths of one method; entries are keyed by the
	// matcher's pattern.
	byMethod map[string]*httpx.PathMatcher
	entries  map[string]map[string][]routeEntry
	// anyMethod matches every route path regardless of method, for
	// WebSocket upgrades (a GET on a POST route).
	anyMethod  *httpx.PathMatcher
	anyEntries map[string][]routeEntry
}

func newRouteTable(codecs []protocol.Codec) *routeTable {
	t := &routeTable{
		codecs:     map[string]protocol.Codec{},
		byMethod:   map[string]*httpx.PathMatcher{},
		entries:    map[string]map[string][]routeEntry{},
		anyEntries: map[string][]routeEntry{},
	}
	paths := map[string][]string{}
	var anyPaths []string
	for _, c := range codecs {
		t.codecs[c.Name()] = c
		for _, rt := range c.Routes() {
			pat, action, fixed := matcherPattern(rt.Path)
			e := routeEntry{matched: matched{codec: c, route: rt}, action: action, fixed: fixed}
			if t.entries[rt.Method] == nil {
				t.entries[rt.Method] = map[string][]routeEntry{}
			}
			if len(t.entries[rt.Method][pat]) == 0 {
				paths[rt.Method] = append(paths[rt.Method], pat)
			}
			t.entries[rt.Method][pat] = append(t.entries[rt.Method][pat], e)
			if len(t.anyEntries[pat]) == 0 {
				anyPaths = append(anyPaths, pat)
			}
			t.anyEntries[pat] = append(t.anyEntries[pat], e)
		}
	}
	for m, ps := range paths {
		t.byMethod[m] = httpx.NewPathMatcher(sortPatterns(ps)...)
	}
	t.anyMethod = httpx.NewPathMatcher(sortPatterns(anyPaths)...)
	return t
}

// sortPatterns puts "/v1/..." before "/v1beta/..." so a bare path such as
// "/models" resolves to the shared OpenAI-style listing, then keeps
// registration order.
func sortPatterns(ps []string) []string {
	var v1, rest []string
	for _, p := range ps {
		if strings.HasPrefix(p, "/v1/") || p == "/v1" {
			v1 = append(v1, p)
		} else {
			rest = append(rest, p)
		}
	}
	return append(v1, rest...)
}

// matcherPattern turns a "{model}:{action}" or "{model}:<action>" segment,
// which PathMatcher cannot express, into "{model}"; action reports that it
// did, fixed returns a literal action.
func matcherPattern(p string) (string, bool, string) {
	segs := strings.Split(p, "/")
	action, fixed := false, ""
	for i, s := range segs {
		if !strings.HasPrefix(s, "{") {
			continue
		}
		if j := strings.Index(s, "}:"); j >= 0 {
			if a := s[j+2:]; a != "{action}" {
				fixed = a
			}
			segs[i] = s[:j+1]
			action = true
		}
	}
	return strings.Join(segs, "/"), action, fixed
}

// match finds the codec route for r.
func (t *routeTable) match(r *http.Request) (matched, bool) {
	method := r.Method
	pm := t.byMethod[method]
	if pm == nil && method == http.MethodHead {
		method, pm = http.MethodGet, t.byMethod[http.MethodGet]
	}
	if pm == nil {
		return matched{}, false
	}
	pat, params, ok := pm.Match(r.URL.Path)
	if !ok {
		return matched{}, false
	}
	es := t.entries[method][pat]
	if len(es) == 0 {
		return matched{}, false
	}
	if es[0].action {
		act, ok := actionOf(params)
		if !ok {
			return matched{}, false
		}
		for _, e := range es {
			if e.fixed == act {
				return e.matched, true
			}
		}
		for _, e := range es {
			if e.fixed == "" {
				return e.matched, true
			}
		}
		return matched{}, false
	}
	if es[0].route.Op == protocol.OpModels {
		return t.modelsCodec(r, es), true
	}
	return es[0].matched, true
}

// actionOf returns the ":action" suffix of a path parameter.
func actionOf(params map[string]string) (string, bool) {
	for _, v := range params {
		if i := strings.LastIndexByte(v, ':'); i > 0 && i < len(v)-1 {
			return v[i+1:], true
		}
	}
	return "", false
}

// modelsCodec picks the listing format from the request's features: a
// client_version query (catalog-style clients) wants the responses codec,
// an anthropic-version header the anthropic codec, a /v1beta path the
// gemini codec, and anything else the OpenAI chat format.
func (t *routeTable) modelsCodec(r *http.Request, es []routeEntry) matched {
	want := codecChat
	switch {
	case r.URL.Query().Has("client_version"):
		want = codecResponses
	case r.Header.Get("anthropic-version") != "":
		want = codecAnthropic
	case strings.HasPrefix(httpx.CleanPath(r.URL.Path), "/v1beta/"):
		want = codecGemini
	}
	if c, ok := t.codecs[want]; ok {
		for _, rt := range c.Routes() {
			if rt.Op == protocol.OpModels {
				return matched{codec: c, route: rt}
			}
		}
	}
	return es[0].matched
}

// matchAny finds a route on r's path regardless of method.
func (t *routeTable) matchAny(r *http.Request) (matched, bool) {
	pat, _, ok := t.anyMethod.Match(r.URL.Path)
	if !ok || len(t.anyEntries[pat]) == 0 {
		return matched{}, false
	}
	return t.anyEntries[pat][0].matched, true
}

// isBrowserNavigation reports a page load by a browser without API
// credentials; such GETs on paths shared with the console (e.g. "/models")
// go to the console.
func isBrowserNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.Header.Get("Authorization") != "" || r.Header.Get("x-api-key") != "" ||
		r.Header.Get("x-goog-api-key") != "" || r.Header.Get("anthropic-version") != "" {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
