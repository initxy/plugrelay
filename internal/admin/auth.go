package admin

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Access is who may call an operation.
type Access int

const (
	// Public needs no identity (setup status, login).
	Public Access = iota
	// SetupOnly is open only while no admin exists.
	SetupOnly
	// Anyone needs an authenticated identity of any role; handlers scope
	// what non-admins see.
	Anyone
	// AdminOnly needs the admin role.
	AdminOnly
)

// Cookie and header names.
const (
	SessionCookie = "pr_session"
	CSRFCookie    = "pr_csrf"
	CSRFHeader    = "X-CSRF-Token"
)

// Auth kinds reported by GET /me.
const (
	authSession = "session"
	authBearer  = "bearer"
	authNone    = "none"
)

// identity is the authenticated caller of an admin request.
type identity struct {
	user *store.User // nil for operator tokens
	auth string
	role store.Role
}

func (i *identity) isAdmin() bool {
	return i != nil && i.role == store.RoleAdmin
}

// userID is the caller's user id ("" for operator tokens).
func (i *identity) userID() string {
	if i == nil || i.user == nil {
		return ""
	}
	return i.user.ID
}

func (i *identity) actor() (id, name string) {
	if i == nil {
		return "anonymous", "anonymous"
	}
	if i.user == nil {
		return "token", "admin token"
	}
	return i.user.ID, displayName(i.user)
}

func isUnsafe(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// authorize resolves the caller and enforces access, CSRF and the setup
// gate.
func (h *Handler) authorize(c *call, a Access) error {
	needs, err := h.NeedsSetup(c.ctx)
	if err != nil {
		return err
	}
	switch a {
	case Public:
		c.who, _ = h.identify(c) // optional
		return nil
	case SetupOnly:
		if !needs {
			return errConflict(eSetupDone)
		}
		// Same-origin alone does not stop DNS rebinding: a page on a name
		// the attacker re-points at 127.0.0.1 is same-origin with itself.
		if _, loopback := h.d.Listen(); loopback && !isLoopbackHost(c.r.Host) {
			return errForbidden(eSetupLoopback)
		}
		return h.checkOrigin(c.r)
	}
	if needs {
		return errForbidden(eSetupRequired)
	}
	who, err := h.identify(c)
	if err != nil {
		return err
	}
	if who == nil {
		return errUnauthorized(eNotLoggedIn)
	}
	c.who = who
	if a == AdminOnly && !who.isAdmin() {
		return errForbidden(eAdminRequired)
	}
	if who.isAdmin() {
		err := h.enforceMFAPolicy(c)
		var ae *apiError
		if err == nil || a == AdminOnly || !errors.As(err, &ae) || ae.Code != codeMFAEnrollment {
			return err
		}
		// An admin past the enrollment deadline keeps the pages every user
		// has (to enroll), but no admin power anywhere: handlers branch on
		// isAdmin, so the identity itself drops the role.
		demoted := *who
		demoted.role = store.RoleMember
		c.who = &demoted
	}
	return nil
}

// identify reads the session cookie, a Bearer token or the local-owner
// rule. It returns (nil, nil) when the request carries no credentials.
func (h *Handler) identify(c *call) (*identity, error) {
	r := c.r
	if tok, ok := bearer(r); ok {
		return h.bearerIdentity(c.ctx, tok)
	}
	if ck, err := r.Cookie(SessionCookie); err == nil && ck.Value != "" {
		u, err := h.d.Store.SessionUser(c.ctx, ck.Value)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, errUnauthorized(eSessionExpired)
			}
			return nil, err
		}
		if isUnsafe(r.Method) {
			if err := checkCSRF(r); err != nil {
				return nil, err
			}
			if err := h.checkOrigin(r); err != nil {
				return nil, err
			}
		}
		return &identity{user: u, auth: authSession, role: u.Role}, nil
	}
	if h.localOwner(c.ctx, r) {
		owner, err := h.firstAdmin(c.ctx)
		if err != nil || owner == nil {
			return nil, err
		}
		if isUnsafe(r.Method) {
			// A web page on another site can make the browser post to
			// loopback; the double-submit token stops it.
			if err := checkCSRF(r); err != nil {
				return nil, err
			}
			if err := h.checkOrigin(r); err != nil {
				return nil, err
			}
		}
		ensureCSRFCookie(c.w, r)
		return &identity{user: owner, auth: authNone, role: store.RoleAdmin}, nil
	}
	return nil, nil
}

func bearer(r *http.Request) (string, bool) {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

func (h *Handler) bearerIdentity(ctx context.Context, tok string) (*identity, error) {
	if strings.HasPrefix(tok, secret.KeyPrefix) {
		return nil, errUnauthorized(ePlatformKey)
	}
	if h.d.AdminToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(h.d.AdminToken)) == 1 {
		return &identity{auth: authBearer, role: store.RoleAdmin}, nil
	}
	site, err := h.site(ctx)
	if err != nil {
		return nil, err
	}
	if hash := site.adminTokenHash(); hash != "" && secret.EqualHash(hash, secret.HashKey(tok)) {
		return &identity{auth: authBearer, role: store.RoleAdmin}, nil
	}
	return nil, errUnauthorized(eAdminTokenInvalid)
}

// IsAdminToken reports whether r carries a valid operator admin token
// (used to protect /metrics).
func (h *Handler) IsAdminToken(r *http.Request) bool {
	tok, ok := bearer(r)
	if !ok {
		return false
	}
	who, err := h.bearerIdentity(r.Context(), tok)
	return err == nil && who != nil && who.auth == authBearer
}

func checkCSRF(r *http.Request) error {
	ck, err := r.Cookie(CSRFCookie)
	hdr := r.Header.Get(CSRFHeader)
	if err != nil || ck.Value == "" || hdr == "" || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(hdr)) != 1 {
		return errForbidden(eCSRF.with("header", CSRFHeader))
	}
	return nil
}

// checkOrigin rejects browser requests sent from another origin.
func (h *Handler) checkOrigin(r *http.Request) error {
	o := r.Header.Get("Origin")
	if o == "" {
		return nil
	}
	u, err := url.Parse(o)
	if err != nil || o == "null" || !strings.EqualFold(u.Host, r.Host) {
		return errForbidden(eCrossOrigin)
	}
	return nil
}

// localOwner applies the local-owner rule (site setting
// security.local_owner_login): the relay listens on loopback only and the
// request comes from loopback with a loopback Host header (so
// a DNS-rebound name cannot pass) and without forwarding headers (so a
// reverse proxy on the same machine does not make every visitor local).
func (h *Handler) localOwner(ctx context.Context, r *http.Request) bool {
	if _, loopback := h.d.Listen(); !loopback {
		return false
	}
	if !h.localOwnerLogin(ctx) {
		return false
	}
	for _, name := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP"} {
		if len(r.Header.Values(name)) > 0 {
			return false
		}
	}
	// The socket peer, never Deps.ClientIP: a forwarded address must not
	// make a remote caller look local.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return false
	}
	return isLoopbackHost(r.Host)
}

// hostTrusted reports whether the request's Host may stand in for an unset
// public URL: only on a loopback-only listener, reached by a loopback name.
func (h *Handler) hostTrusted(c *call) bool {
	_, loopback := h.d.Listen()
	return loopback && isLoopbackHost(c.r.Host)
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}

func (h *Handler) firstAdmin(ctx context.Context) (*store.User, error) {
	users, err := h.d.Store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range users {
		if users[i].Role == store.RoleAdmin && !users[i].Disabled {
			return &users[i], nil
		}
	}
	return nil, nil
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// startSession sets the session and CSRF cookies.
func (h *Handler) startSession(c *call, u *store.User) error {
	ttl := h.sessionTTL(c.ctx)
	tok, err := h.d.Store.CreateSession(c.ctx, u.ID, ttl)
	if err != nil {
		return err
	}
	// Signing in counts as a fresh re-authentication.
	if err := h.markReauth(c.ctx, secret.HashKey(tok)); err != nil {
		return err
	}
	csrf, err := secret.GenerateToken()
	if err != nil {
		return err
	}
	sec := secureRequest(c.r)
	http.SetCookie(c.w, &http.Cookie{Name: SessionCookie, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: sec, MaxAge: int(ttl / time.Second)})
	http.SetCookie(c.w, &http.Cookie{Name: CSRFCookie, Value: csrf, Path: "/",
		SameSite: http.SameSiteLaxMode, Secure: sec, MaxAge: int(ttl / time.Second)})
	return nil
}

func ensureCSRFCookie(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie(CSRFCookie); err == nil && ck.Value != "" {
		return
	}
	tok, err := secret.GenerateToken()
	if err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{Name: CSRFCookie, Value: tok, Path: "/", SameSite: http.SameSiteLaxMode,
		Secure: secureRequest(r)})
}

func clearSession(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{SessionCookie, CSRFCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: name == SessionCookie,
			SameSite: http.SameSiteLaxMode, Secure: secureRequest(r)})
	}
}

// loginLimiter slows password guessing per client address. IPv6 clients
// are counted per /64, the smallest block one subscriber usually gets, so
// rotating addresses inside it does not reset the count.
//
// Each node keeps an exact sliding window in memory. With shared state the
// events are also counted cluster-wide in a fixed window, so spreading
// guesses over nodes does not multiply the budget; a state error falls
// back to the local count.
type loginLimiter struct {
	mu     sync.Mutex
	fail   map[string][]time.Time
	max    int // tracked addresses at most
	limit  int // events per window before blocking
	window time.Duration
	shared plugin.SharedState
	name   string // shared key prefix
}

// share counts this limiter's events in st as well, under name.
func (l *loginLimiter) share(st plugin.SharedState, name string) *loginLimiter {
	l.shared, l.name = st, name
	return l
}

// sharedCount adds delta to ip's cluster-wide count and returns it; ok is
// false without shared state or when it fails.
func (l *loginLimiter) sharedCount(ip string, delta int64) (int64, bool) {
	if l.shared == nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	n, err := l.shared.Incr(ctx, "admin-limit:"+l.name+":"+loginKey(ip), delta, l.window)
	return n, err == nil
}

const (
	loginWindow   = 5 * time.Minute
	loginMaxFails = 10
	// loginMaxTracked caps the limiter's memory; at the cap the address
	// whose last failure is oldest is forgotten first.
	loginMaxTracked = 10000
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{fail: map[string][]time.Time{}, max: loginMaxTracked, limit: loginMaxFails, window: loginWindow}
}

// newStartLimiter bounds sign-in starts per address (SSO redirects,
// passwordless passkey challenges): each one stores pending state, and
// offices share one address, so the bar is high.
func newStartLimiter() *loginLimiter {
	return &loginLimiter{fail: map[string][]time.Time{}, max: loginMaxTracked, limit: ssoMaxStarts, window: loginWindow}
}

const ssoMaxStarts = 60

// newPublicLimiter bounds calls to plugins' public routes (payment
// callbacks, sign-up) per address. Payment providers retry a refused
// callback later, so the bar only has to stop floods.
func newPublicLimiter() *loginLimiter {
	return &loginLimiter{fail: map[string][]time.Time{}, max: loginMaxTracked, limit: publicMaxCalls, window: loginWindow}
}

const publicMaxCalls = 600

// newPageLimiter bounds loads of the public pages per address per minute;
// the limit is a setting (setLimit).
func newPageLimiter() *loginLimiter {
	return &loginLimiter{fail: map[string][]time.Time{}, max: loginMaxTracked, limit: 60, window: time.Minute}
}

// setLimit changes the events allowed per window.
func (l *loginLimiter) setLimit(n int) {
	l.mu.Lock()
	l.limit = n
	l.mu.Unlock()
}

// loginKey is the bucket a client address counts against.
func loginKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if a = a.Unmap(); a.Is6() {
		p, _ := a.WithZone("").Prefix(64)
		return p.String()
	}
	return a.String()
}

// blocked returns how long ip must wait, or 0. A cluster-wide block has
// no exact end here, so it waits out a whole window.
func (l *loginLimiter) blocked(ip string, now time.Time) time.Duration {
	l.mu.Lock()
	limit := l.limit
	l.mu.Unlock()
	if n, ok := l.sharedCount(ip, 0); ok && n >= int64(limit) {
		return l.window
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := l.prune(loginKey(ip), now)
	if len(recent) < l.limit {
		return 0
	}
	return recent[0].Add(l.window).Sub(now)
}

// count is how many failures ip has in the window.
func (l *loginLimiter) count(ip string, now time.Time) int {
	shared, _ := l.sharedCount(ip, 0)
	l.mu.Lock()
	defer l.mu.Unlock()
	return max(len(l.prune(loginKey(ip), now)), int(shared))
}

func (l *loginLimiter) failed(ip string, now time.Time) {
	l.sharedCount(ip, 1)
	l.mu.Lock()
	defer l.mu.Unlock()
	k := loginKey(ip)
	ts := l.prune(k, now)
	if len(ts) == 0 && len(l.fail) >= l.max {
		l.evict(now)
	}
	l.fail[k] = append(ts, now)
}

// evict drops expired entries and, if still at the cap, the entry whose
// last failure is oldest.
func (l *loginLimiter) evict(now time.Time) {
	oldest, at := "", now
	for k := range l.fail {
		ts := l.prune(k, now)
		if len(ts) > 0 && !ts[len(ts)-1].After(at) {
			oldest, at = k, ts[len(ts)-1]
		}
	}
	if len(l.fail) >= l.max && oldest != "" {
		delete(l.fail, oldest)
	}
}

func (l *loginLimiter) reset(ip string) {
	if l.shared != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = l.shared.Delete(ctx, "admin-limit:"+l.name+":"+loginKey(ip))
		cancel()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fail, loginKey(ip))
}

func (l *loginLimiter) prune(ip string, now time.Time) []time.Time {
	ts := l.fail[ip]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > l.window {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.fail, ip)
	} else {
		l.fail[ip] = ts
	}
	return ts
}

// peerIP is the host part of the socket peer address.
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
