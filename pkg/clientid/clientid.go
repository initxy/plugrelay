// Package clientid names the client program behind a request (Claude Code,
// Codex, an SDK…) from the headers it sends. The rules table below is the
// only list of clients: the request log, the roll-ups, the CSV export and
// the console take ids and names from here, so a new client is one row.
package clientid

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// Other is the id of every request no rule recognises.
const Other = "other"

// MaxLen bounds ids and versions as stored (part of the roll-up key).
const MaxLen = 48

// Client is a recognised client and its version, when it sends one.
type Client struct {
	ID      string
	Version string
}

// Info describes a known client.
type Info struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type matcher struct {
	header string
	re     *regexp.Regexp
	// version names a header holding the version when the match itself
	// carries none (the SDKs' X-Stainless-Package-Version).
	version string
}

type rule struct {
	id, name string
	match    []matcher
}

func m(header, pattern string) matcher {
	return matcher{header: header, re: regexp.MustCompile(pattern)}
}

func mv(header, pattern, version string) matcher {
	return matcher{header: header, re: regexp.MustCompile(pattern), version: version}
}

const (
	ua      = "User-Agent"
	title   = "X-Title"
	referer = "Http-Referer"
	ver     = `v?([0-9][\w.+-]*)`
)

// rules are tried in order; the first rule with a matching header wins.
// Clients built on an SDK come before the SDKs, and a client's own
// User-Agent before attribution headers. A pattern's first group is the
// version.
var rules = []rule{
	{"playground", "Playground", []matcher{m(ua, `^plugrelay-playground/`+ver)}},
	{"claude-code", "Claude Code", []matcher{m(ua, `(?i)^claude-(?:cli|code)/`+ver)}},
	{"codex", "Codex", []matcher{
		m(ua, `(?i)^codex(?:_cli_rs|_exec|_vscode|_sdk_ts|-cli)?/`+ver),
		m("Originator", `(?i)^codex`),
	}},
	{"gemini-cli", "Gemini CLI", []matcher{m(ua, `(?i)^gemini-?cli/`+ver)}},
	{"roo", "Roo Code", []matcher{
		m(ua, `(?i)^roo-?code/`+ver),
		m(title, `(?i)^roo[ -]?code\b`),
		m(referer, `(?i)roovetgit|roocode\.com`),
	}},
	{"kilo", "Kilo Code", []matcher{
		m(ua, `(?i)^kilo-?code/`+ver),
		m(title, `(?i)^kilo[ -]?code\b`),
		m(referer, `(?i)kilocode\.ai`),
	}},
	{"cline", "Cline", []matcher{
		m(ua, `(?i)^cline/`+ver),
		m(title, `(?i)^cline$`),
		m(referer, `(?i)cline\.bot`),
	}},
	{"opencode", "OpenCode", []matcher{
		m(ua, `(?i)^opencode/`+ver),
		m(title, `(?i)^opencode$`),
		m(referer, `(?i)opencode\.ai`),
	}},
	{"continue", "Continue", []matcher{
		m(ua, `(?i)^continue/`+ver),
		m(title, `(?i)^continue$`),
		m(referer, `(?i)continue\.dev`),
	}},
	{"cursor", "Cursor", []matcher{m(ua, `(?i)\bcursor/`+ver)}},
	{"zed", "Zed", []matcher{m(ua, `(?i)^zed(?:-editor)?/`+ver)}},
	{"curl", "curl", []matcher{m(ua, `^curl/`+ver)}},
	{"sdk-python", "Python SDK", []matcher{
		m(ua, `(?i)^(?:async)?(?:anthropic|openai)/python `+ver),
		m(ua, `(?i)^google-genai-sdk/`+ver+`.*\bgl-python/`),
		mv("X-Stainless-Lang", `(?i)^python$`, "X-Stainless-Package-Version"),
	}},
	{"sdk-node", "Node SDK", []matcher{
		m(ua, `(?i)^(?:anthropic|openai)/js `+ver),
		m(ua, `(?i)^google-genai-sdk/`+ver+`.*\bgl-node/`),
		m(ua, `(?i)^ai-sdk/`),
		mv("X-Stainless-Lang", `(?i)^js$`, "X-Stainless-Package-Version"),
	}},
}

// Detect names the client of a request from its headers.
func Detect(h http.Header) Client {
	for _, r := range rules {
		if c, ok := r.detect(h); ok {
			return c
		}
	}
	return Client{ID: Other}
}

// detect reports whether any matcher of r hits; the version is the first
// one a hitting matcher yields.
func (r rule) detect(h http.Header) (Client, bool) {
	hit := false
	var version string
	for _, mt := range r.match {
		v := h.Get(mt.header)
		if v == "" {
			continue
		}
		sm := mt.re.FindStringSubmatch(v)
		if sm == nil {
			continue
		}
		hit = true
		if len(sm) > 1 && sm[1] != "" {
			version = sm[1]
		} else if mt.version != "" {
			version = strings.TrimPrefix(strings.TrimSpace(h.Get(mt.version)), "v")
		}
		if version != "" {
			break
		}
	}
	return Client{ID: r.id, Version: clip(version)}, hit
}

// FromUserAgent names the client from its User-Agent alone, for records
// that kept nothing else.
func FromUserAgent(userAgent string) Client {
	h := http.Header{}
	if userAgent != "" {
		h.Set(ua, userAgent)
	}
	return Detect(h)
}

// Name is the display name of a client id; Other and unknown ids have
// none (the console words them).
func Name(id string) string {
	for _, r := range rules {
		if r.id == id {
			return r.name
		}
	}
	return ""
}

// Known lists every client the rules recognise, in rule order.
func Known() []Info {
	out := make([]Info, 0, len(rules))
	for _, r := range rules {
		out = append(out, Info{ID: r.id, Name: r.name})
	}
	return out
}

func clip(s string) string {
	if len(s) <= MaxLen {
		return s
	}
	return s[:MaxLen]
}

// Stamp records the client of the request with headers h on rec.
func Stamp(rec *plugin.Record, h http.Header) {
	c := Detect(h)
	rec.Client, rec.ClientVersion = c.ID, c.Version
}
