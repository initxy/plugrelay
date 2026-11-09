// Package egress is the single way out to upstreams: per-credential HTTP
// clients with a fixed proxy, an SSRF guard on every dialed address, the
// outbound and inbound header rules, and the per-attempt timers.
package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"syscall"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// Options configures a Client. Zero durations take the defaults.
type Options struct {
	DialTimeout         time.Duration // 10s
	TLSHandshakeTimeout time.Duration // 10s
	IdleConnTimeout     time.Duration // 90s
	MaxIdleConnsPerHost int           // 32
	// AllowCIDRs are private ranges every upstream may reach (an internal
	// gateway network, for example).
	AllowCIDRs []netip.Prefix
	// Resolver resolves target hosts when the request goes through a proxy;
	// nil uses net.DefaultResolver.
	Resolver *net.Resolver
}

// Client hands out HTTP clients for upstream calls. Transports are shared per
// (proxy, allowPrivate) so connections are pooled across credentials with the
// same egress.
type Client struct {
	opt Options
	mu  sync.Mutex
	rts map[transportKey]http.RoundTripper
}

type transportKey struct {
	proxy        string
	allowPrivate bool
}

// New returns a Client.
func New(opt Options) *Client {
	if opt.DialTimeout <= 0 {
		opt.DialTimeout = 10 * time.Second
	}
	if opt.TLSHandshakeTimeout <= 0 {
		opt.TLSHandshakeTimeout = 10 * time.Second
	}
	if opt.IdleConnTimeout <= 0 {
		opt.IdleConnTimeout = 90 * time.Second
	}
	if opt.MaxIdleConnsPerHost <= 0 {
		opt.MaxIdleConnsPerHost = 32
	}
	if opt.Resolver == nil {
		opt.Resolver = net.DefaultResolver
	}
	return &Client{opt: opt, rts: map[transportKey]http.RoundTripper{}}
}

// HTTPClient returns the client for a credential's fixed egress. proxy is the
// credential's proxy URL ("" = direct); allowPrivate is the upstream's
// allow_private flag. The client has no overall timeout (use Watch) and does
// not follow redirects.
func (c *Client) HTTPClient(proxy string, allowPrivate bool) (*http.Client, error) {
	rt, err := c.transport(proxy, allowPrivate)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport:     rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// Dial connects directly to addr under the SSRF guard (private ranges only
// when listed in AllowCIDRs), for traffic that is not HTTP, such as SMTP.
func (c *Client) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	g := &guard{allow: c.opt.AllowCIDRs}
	d := &net.Dialer{Timeout: c.opt.DialTimeout, Control: g.control}
	return d.DialContext(ctx, network, addr)
}

// For returns the client for a credential's fixed proxy.
func (c *Client) For(cred *plugin.Credential, allowPrivate bool) (*http.Client, error) {
	return c.HTTPClient(cred.Proxy, allowPrivate)
}

func (c *Client) transport(proxy string, allowPrivate bool) (http.RoundTripper, error) {
	key := transportKey{proxy, allowPrivate}
	c.mu.Lock()
	defer c.mu.Unlock()
	if rt, ok := c.rts[key]; ok {
		return rt, nil
	}
	rt, err := c.build(proxy, allowPrivate)
	if err != nil {
		return nil, err
	}
	c.rts[key] = rt
	return rt, nil
}

func (c *Client) build(proxy string, allowPrivate bool) (http.RoundTripper, error) {
	g := &guard{allow: c.opt.AllowCIDRs, open: allowPrivate}
	t := &http.Transport{
		// Never ProxyFromEnvironment: the egress is fixed per credential.
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   c.opt.TLSHandshakeTimeout,
		IdleConnTimeout:       c.opt.IdleConnTimeout,
		MaxIdleConnsPerHost:   c.opt.MaxIdleConnsPerHost,
		ExpectContinueTimeout: time.Second,
	}
	if proxy == "" {
		d := &net.Dialer{Timeout: c.opt.DialTimeout, KeepAlive: 30 * time.Second, Control: g.control}
		t.DialContext = d.DialContext
		return t, nil
	}
	pu, err := parseProxy(proxy)
	if err != nil {
		return nil, err
	}
	t.Proxy = http.ProxyURL(pu)
	// The only address dialed here is the operator-configured proxy, so it
	// is not subject to the guard; the target is checked in proxied below.
	d := &net.Dialer{Timeout: c.opt.DialTimeout, KeepAlive: 30 * time.Second}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, &ProxyError{Proxy: redact(proxy), Err: err}
		}
		return conn, nil
	}
	return &proxied{rt: t, guard: g, resolver: c.opt.Resolver}, nil
}

// CheckProxy reports whether proxy ("" = direct) is a usable proxy URL:
// http, https, socks5 or socks5h with a host.
func CheckProxy(proxy string) error {
	if proxy == "" {
		return nil
	}
	_, err := parseProxy(proxy)
	return err
}

func parseProxy(proxy string) (*url.URL, error) {
	pu, err := url.Parse(proxy)
	if err != nil || pu.Host == "" {
		return nil, fmt.Errorf("egress: invalid proxy URL %q", redact(proxy))
	}
	switch pu.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("egress: unsupported proxy scheme %q", pu.Scheme)
	}
	return pu, nil
}

// ProxyError means the credential's proxy could not be reached. The attempt
// fails; there is no fallback to a direct connection.
type ProxyError struct {
	Proxy string // with credentials removed
	Err   error
}

func (e *ProxyError) Error() string { return "egress: proxy " + e.Proxy + ": " + e.Err.Error() }
func (e *ProxyError) Unwrap() error { return e.Err }

// BlockedError means the target address is not allowed (SSRF guard).
type BlockedError struct {
	Addr string
}

func (e *BlockedError) Error() string {
	return "egress: refusing to connect to non-public address " + e.Addr
}

// ErrBlocked matches any *BlockedError with errors.Is.
var ErrBlocked = errors.New("egress: blocked address")

// Is reports ErrBlocked.
func (e *BlockedError) Is(target error) bool { return target == ErrBlocked }

func redact(proxy string) string {
	u, err := url.Parse(proxy)
	if err != nil {
		return "(invalid)"
	}
	u.User = nil
	return u.String()
}

// proxied checks the target host before handing the request to a proxy,
// since the proxy, not us, resolves and dials it. A hostname whose
// resolution changes between our check and the proxy's dial is not caught.
type proxied struct {
	rt       *http.Transport
	guard    *guard
	resolver *net.Resolver
}

func (p *proxied) RoundTrip(r *http.Request) (*http.Response, error) {
	if !p.guard.open {
		host := r.URL.Hostname()
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			ips = []netip.Addr{ip}
		} else {
			ips, err = p.resolver.LookupNetIP(r.Context(), "ip", host)
			if err != nil {
				return nil, err
			}
		}
		for _, ip := range ips {
			if !p.guard.allowed(ip) {
				return nil, &BlockedError{Addr: ip.String()}
			}
		}
	}
	return p.rt.RoundTrip(r)
}

func (p *proxied) CloseIdleConnections() { p.rt.CloseIdleConnections() }

type guard struct {
	allow []netip.Prefix
	open  bool
}

// control runs after DNS resolution with the literal address being dialed.
func (g *guard) control(_, address string, _ syscall.RawConn) error {
	if g.open {
		return nil
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return &BlockedError{Addr: address}
	}
	if !g.allowed(ap.Addr()) {
		return &BlockedError{Addr: ap.Addr().String()}
	}
	return nil
}

var blocked = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b:1::/48", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func (g *guard) allowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range g.allow {
		if p.Contains(ip) {
			return true
		}
	}
	for _, p := range blocked {
		if p.Contains(ip) {
			return false
		}
	}
	if v4, ok := embeddedIPv4(ip); ok {
		return g.allowed(v4)
	}
	return true
}

var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
)

// embeddedIPv4 extracts the IPv4 address that a NAT64 (64:ff9b::/96) or
// 6to4 (2002::/16) address routes to, so a blocked IPv4 range cannot be
// reached through its IPv6 form.
func embeddedIPv4(ip netip.Addr) (netip.Addr, bool) {
	b := ip.As16()
	switch {
	case !ip.Is6():
		return netip.Addr{}, false
	case nat64.Contains(ip):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFour.Contains(ip):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}
