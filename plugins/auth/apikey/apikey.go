// Package apikey is the auth.apikey plugin: it checks the platform key a
// client presents.
//
// Keys live in the core, hashed; this plugin never sees the stored hash. It
// pulls the raw key out of the request, asks a KeyLookup for the caller
// behind it, and enforces the key's own restrictions (disabled, expiry,
// source IPs, client programs).
//
// The key is read from, in order: Authorization: Bearer, x-api-key,
// x-goog-api-key, and the ?key= query parameter. A request carrying none of
// them is passed on to the next authenticator.
//
// Wiring: construct directly with New(lookup), or let the registered
// factory pick the lookup up in Init from a Host that implements
// LookupHost, and the client address resolver from one implementing
// ClientIPHost.
package apikey

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// ID is the plugin id.
const ID = "auth.apikey"

func init() { plugin.Register(ID, func() plugin.Plugin { return New(nil) }) }

// KeyInfo is the access policy stored on a key record.
type KeyInfo struct {
	Disabled bool
	// Expires is zero for keys that never expire.
	Expires time.Time
	// AllowedIPs holds addresses or CIDRs; empty allows any source.
	AllowedIPs []string
	// AllowClient reports whether the client program that sent a request
	// with these headers may use the key; nil allows any client.
	AllowClient func(http.Header) bool
}

// KeyLookup resolves a raw platform key. It returns a nil Caller and a nil
// error when no such key exists.
type KeyLookup interface {
	LookupKey(ctx context.Context, rawKey string) (*plugin.Caller, KeyInfo, error)
}

// LookupHost is implemented by a Host that can hand the plugin its key
// lookup. The registered factory uses it in Init.
type LookupHost interface {
	KeyLookup() KeyLookup
}

// ClientIPHost is implemented by a Host that resolves the client address
// (honouring trusted reverse proxies). The registered factory uses it in
// Init; without one the source address is the host part of RemoteAddr.
type ClientIPHost interface {
	ClientIP(r *http.Request) string
}

// Option configures the plugin.
type Option func(*Auth)

// WithClock overrides time.Now for expiry checks.
func WithClock(now func() time.Time) Option { return func(a *Auth) { a.now = now } }

// Auth implements plugin.Authenticator.
type Auth struct {
	lookup   KeyLookup
	now      func() time.Time
	clientIP func(*http.Request) string
}

// New returns the plugin. lookup may be nil when it will come from the Host.
func New(lookup KeyLookup, opts ...Option) *Auth {
	a := &Auth{lookup: lookup, now: time.Now, clientIP: remoteIP}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Info implements plugin.Plugin.
func (a *Auth) Info() plugin.Info {
	return plugin.Info{
		ID:          ID,
		Kind:        plugin.KindAuth,
		Version:     "1.0.0",
		Description: "Platform key authentication: disabled, expiry, source IP and client checks.",
		FailPolicy:  plugin.FailClosed,
	}
}

// Init implements plugin.Initializer.
func (a *Auth) Init(_ context.Context, h plugin.Host) error {
	if a.lookup == nil {
		if lh, ok := h.(LookupHost); ok {
			a.lookup = lh.KeyLookup()
		}
	}
	if ch, ok := h.(ClientIPHost); ok {
		a.clientIP = ch.ClientIP
	}
	if a.lookup == nil {
		return errors.New("apikey: no KeyLookup: construct with New(lookup) or provide a Host implementing LookupHost")
	}
	return nil
}

// Authenticate implements plugin.Authenticator.
func (a *Auth) Authenticate(r *http.Request) (*plugin.Caller, error) {
	raw := ExtractKey(r)
	if raw == "" {
		return nil, nil
	}
	if a.lookup == nil {
		return nil, ir.Errorf(ir.ErrInternal, "key lookup is not configured")
	}
	caller, info, err := a.lookup.LookupKey(r.Context(), raw)
	if err != nil {
		var gw *ir.GwError
		if errors.As(err, &gw) {
			return nil, gw
		}
		return nil, ir.Errorf(ir.ErrInternal, "key lookup failed")
	}
	if caller == nil {
		return nil, ir.Errorf(ir.ErrAuthentication, "invalid API key")
	}
	if info.Disabled {
		return nil, ir.Errorf(ir.ErrAuthentication, "API key is disabled")
	}
	if !info.Expires.IsZero() && !a.now().Before(info.Expires) {
		return nil, ir.Errorf(ir.ErrAuthentication, "API key has expired")
	}
	if len(info.AllowedIPs) > 0 && !IPAllowed(a.clientIP(r), info.AllowedIPs) {
		return nil, ir.Errorf(ir.ErrPermission, "source address is not allowed for this API key")
	}
	if info.AllowClient != nil && !info.AllowClient(r.Header) {
		return nil, ir.Errorf(ir.ErrPermission, "client is not allowed for this API key")
	}
	return caller, nil
}

// ExtractKey returns the platform key carried by r, or "".
func ExtractKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if scheme, tok, ok := strings.Cut(strings.TrimSpace(h), " "); ok && strings.EqualFold(scheme, "Bearer") {
			if tok = strings.TrimSpace(tok); tok != "" {
				return tok
			}
		}
	}
	for _, name := range []string{"x-api-key", "x-goog-api-key"} {
		if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
			return v
		}
	}
	if r.URL != nil {
		return strings.TrimSpace(r.URL.Query().Get("key"))
	}
	return ""
}

// IPAllowed reports whether ip matches one of the addresses or CIDRs.
// Malformed entries match nothing.
func IPAllowed(ip string, allowed []string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, a := range allowed {
		a = strings.TrimSpace(a)
		if strings.Contains(a, "/") {
			if p, err := netip.ParsePrefix(a); err == nil && p.Contains(addr) {
				return true
			}
			continue
		}
		if b, err := netip.ParseAddr(a); err == nil && b.Unmap() == addr {
			return true
		}
	}
	return false
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
