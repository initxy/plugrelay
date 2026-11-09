package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/httpx"
	"github.com/initxy/plugrelay/internal/state"
)

// TestLoginLimitBehindProxy: with the test server's loopback peer trusted
// as a proxy, failed logins are counted per forwarded client, so one
// client's failures do not lock out everyone behind the same proxy.
func TestLoginLimitBehindProxy(t *testing.T) {
	trusted := httpx.IPResolver{Trusted: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}}
	e := newEnv(t, sqliteStore, false, func(d *Deps) { d.ClientIP = trusted.ClientIP })
	e.setup()
	from := func(ip string) *client {
		c := e.browser()
		c.header = http.Header{"X-Forwarded-For": {ip}}
		return c
	}
	for range loginMaxFails {
		from("203.0.113.1").do("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "nope-nope-nope"})
	}
	if code, _ := from("203.0.113.1").do("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "correct-horse-1"}); code != 429 {
		t.Fatalf("throttled client: %d, want 429", code)
	}
	from("203.0.113.2").must(200, "POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "correct-horse-1"})
}

// TestLocalOwnerIgnoresClientIP: the local-owner rule reads the socket
// peer, so neither a forwarded header nor a resolver that trusts it can
// make a remote caller the local owner.
func TestLocalOwnerIgnoresClientIP(t *testing.T) {
	e := newEnv(t, sqliteStore, true, func(d *Deps) {
		d.ClientIP = func(*http.Request) string { return "127.0.0.1" }
	})
	ctx := context.Background()
	remote := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/v1/me", nil)
	remote.RemoteAddr = "203.0.113.5:40000"
	remote.Header.Set("X-Forwarded-For", "127.0.0.1")
	if e.h.localOwner(ctx, remote) {
		t.Fatal("remote peer treated as local owner")
	}
	local := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/v1/me", nil)
	local.RemoteAddr = "127.0.0.1:40000"
	if !e.h.localOwner(ctx, local) {
		t.Fatal("loopback peer not treated as local owner")
	}
}

// TestLocalOwnerRefusesForwarded: a request relayed by a proxy on the same
// machine arrives from loopback, but its forwarding headers show it is not
// the local owner.
func TestLocalOwnerRefusesForwarded(t *testing.T) {
	e := newEnv(t, sqliteStore, true)
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/v1/me", nil)
		r.RemoteAddr = "127.0.0.1:40000"
		r.Header.Set(h, "127.0.0.1")
		if e.h.localOwner(context.Background(), r) {
			t.Errorf("request with %s treated as local owner", h)
		}
	}
}

// TestSetupRefusesRebindingHost: while the relay listens on loopback, first
// -run setup only answers loopback Host names, so a page on a DNS-rebound
// name cannot claim the admin account even though it is same-origin.
func TestSetupRefusesRebindingHost(t *testing.T) {
	body := `{"admin_username":"admin","admin_password":"correct-horse-1","usage":"team","listen_public":false,"currency":"CNY"}`
	post := func(e *env, host string) int {
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/setup", strings.NewReader(body))
		req.Host = host
		req.Header.Set("Origin", "http://"+host)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	e := newEnv(t, sqliteStore, true)
	if code := post(e, "rebind.attacker.example:8787"); code != http.StatusForbidden {
		t.Fatalf("rebound host: %d, want 403", code)
	}
	if code := post(e, "localhost:8787"); code != http.StatusOK {
		t.Fatalf("loopback host: %d, want 200", code)
	}
	public := newEnv(t, sqliteStore, false, func(d *Deps) {
		d.Listen = func() (string, bool) { return "0.0.0.0:8787", false }
	})
	if code := post(public, "relay.example.com"); code != http.StatusOK {
		t.Fatalf("public listener: %d, want 200", code)
	}
}

// TestLoginLimiterBuckets: IPv6 failures count per /64, and the limiter
// forgets the stalest address once it tracks its maximum.
func TestLoginLimiterBuckets(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLoginLimiter()
	for i := range loginMaxFails {
		l.failed(fmt.Sprintf("2001:db8:1:2::%x", i+1), now)
	}
	if l.blocked("2001:db8:1:2:ffff::1", now) == 0 {
		t.Error("rotating addresses inside one /64 escaped the limit")
	}
	if l.blocked("2001:db8:1:3::1", now) != 0 || l.blocked("192.0.2.1", now) != 0 {
		t.Error("another /64 or an IPv4 client was throttled")
	}

	l = newLoginLimiter()
	l.max = 3
	for i, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"} {
		l.failed(ip, now.Add(time.Duration(i)*time.Second))
	}
	if len(l.fail) != 3 {
		t.Fatalf("tracked %d addresses, cap 3", len(l.fail))
	}
	if _, ok := l.fail["192.0.2.1"]; ok {
		t.Error("stalest address kept")
	}
	if _, ok := l.fail["192.0.2.4"]; !ok {
		t.Error("newest address not tracked")
	}
}

// TestLoginLimiterShared: with shared state, failures seen by one node
// block the address on every node, and a success clears it everywhere.
func TestLoginLimiterShared(t *testing.T) {
	st := state.NewMemory(time.Minute)
	defer st.Close()
	now := time.Now()
	a, b := newLoginLimiter().share(st, "login"), newLoginLimiter().share(st, "login")
	for range loginMaxFails / 2 {
		a.failed("192.0.2.9", now)
		b.failed("192.0.2.9", now)
	}
	if a.blocked("192.0.2.9", now) == 0 || b.blocked("192.0.2.9", now) == 0 {
		t.Fatal("failures split over two nodes escaped the limit")
	}
	if c := newLoginLimiter().share(st, "login"); c.count("192.0.2.9", now) != loginMaxFails {
		t.Fatalf("third node counts %d", c.count("192.0.2.9", now))
	}
	if other := newLoginLimiter().share(st, "start"); other.blocked("192.0.2.9", now) != 0 {
		t.Fatal("limiters of another name share the count")
	}
	a.reset("192.0.2.9")
	b.reset("192.0.2.9")
	if b.blocked("192.0.2.9", now) != 0 {
		t.Fatal("reset left the shared count")
	}
}
