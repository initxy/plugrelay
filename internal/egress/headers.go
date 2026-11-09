package egress

import (
	"net/http"
	"net/textproto"
	"strings"

	"github.com/initxy/plugrelay/internal/version"
)

var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// dangerous request headers never forwarded from a client: credentials,
// account selectors, framing, and anything that reveals the client's address.
var dangerous = []string{
	"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Api-Key",
	"Openai-Organization", "Openai-Project",
	"Cookie", "Host", "Content-Length", "Accept-Encoding",
	"Forwarded", "Via", "X-Real-Ip", "True-Client-Ip", "Cf-Connecting-Ip", "X-Client-Ip",
}

func isDangerous(name string) bool {
	if strings.HasPrefix(name, "X-Forwarded-") {
		return true
	}
	for _, d := range dangerous {
		if name == d {
			return true
		}
	}
	return false
}

func canonSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(n))] = true
	}
	return m
}

// hopSet returns the hop-by-hop names, including those listed in Connection.
func hopSet(h http.Header) map[string]bool {
	m := canonSet(hopByHop)
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				m[textproto.CanonicalMIMEHeaderKey(tok)] = true
			}
		}
	}
	return m
}

// OutboundHeaders returns the client headers to forward upstream.
//
// Dangerous and hop-by-hop headers and every name the provider owns are
// dropped. With sameProtocol everything else is forwarded verbatim; across
// protocols only keepCross names survive. Nothing is added except a
// User-Agent when none survives.
func OutboundHeaders(client http.Header, owned []string, sameProtocol bool, keepCross []string) http.Header {
	hop := hopSet(client)
	own := canonSet(owned)
	keep := canonSet(keepCross)
	out := http.Header{}
	for name, vals := range client {
		cn := textproto.CanonicalMIMEHeaderKey(name)
		if hop[cn] || own[cn] || isDangerous(cn) {
			continue
		}
		if !sameProtocol && !keep[cn] {
			continue
		}
		out[cn] = append([]string(nil), vals...)
	}
	if out.Get("User-Agent") == "" {
		out.Set("User-Agent", "plugrelay/"+version.Version)
	}
	return out
}

// InboundResponseHeaders returns the upstream response headers to pass to
// the client: everything except hop-by-hop, Set-Cookie, Alt-Svc and the
// framing headers that no longer match the body we send (Content-Length,
// Content-Encoding — bodies are decoded before relaying).
func InboundResponseHeaders(upstream http.Header) http.Header {
	hop := hopSet(upstream)
	out := http.Header{}
	for name, vals := range upstream {
		cn := textproto.CanonicalMIMEHeaderKey(name)
		switch cn {
		case "Set-Cookie", "Set-Cookie2", "Content-Length", "Content-Encoding", "Alt-Svc":
			continue
		}
		if hop[cn] {
			continue
		}
		out[cn] = append([]string(nil), vals...)
	}
	return out
}

// ExtraHeaderAllowed reports whether an operator may configure name as an
// extra request header of an upstream: never a hop-by-hop or dangerous
// header (credentials, account selectors, framing, client address).
func ExtraHeaderAllowed(name string) bool {
	cn := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
	return cn != "" && !canonSet(hopByHop)[cn] && !isDangerous(cn)
}

// SetExtraHeaders sets an upstream's configured extra headers on an
// outgoing request header, replacing a forwarded client value. Names that
// ExtraHeaderAllowed rejects and names the provider owns are skipped, so
// the provider's auth and account headers keep only the provider's values.
// It returns the skipped names.
func SetExtraHeaders(h http.Header, extra map[string]string, owned []string) (skipped []string) {
	own := canonSet(owned)
	for name, v := range extra {
		cn := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
		if !ExtraHeaderAllowed(cn) || own[cn] {
			skipped = append(skipped, cn)
			continue
		}
		h.Set(cn, v)
	}
	return skipped
}
