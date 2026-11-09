package httpx

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// IPResolver extracts the client address, trusting forwarding headers only
// from configured proxies.
type IPResolver struct {
	// Trusted are the networks of reverse proxies in front of the relay.
	Trusted []netip.Prefix
}

func (x IPResolver) trusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range x.Trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIP returns the client's address. When the peer is a trusted proxy,
// X-Forwarded-For is walked right to left past trusted hops, falling back to
// X-Real-IP; otherwise the peer address is the answer.
func (x IPResolver) ClientIP(r *http.Request) string {
	peer := remoteAddr(r.RemoteAddr)
	if !peer.IsValid() {
		return r.RemoteAddr
	}
	if !x.trusted(peer) {
		return peer.Unmap().String()
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break // a malformed hop ends what we can trust
		}
		if !x.trusted(a) {
			return a.Unmap().String()
		}
	}
	if len(hops) == 0 {
		if a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-Ip"))); err == nil {
			return a.Unmap().String()
		}
	}
	return peer.Unmap().String()
}

func remoteAddr(s string) netip.Addr {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr()
	}
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s
	}
	a, _ := netip.ParseAddr(host)
	return a
}
