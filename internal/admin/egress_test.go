package admin

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/initxy/plugrelay/internal/egress"
)

// The admin API's own calls (SSO, captcha) leave through Deps.HTTPClient,
// the core's SSRF-guarded egress: a loopback IdP or captcha verifier is
// unreachable until the egress allows it.
func TestOutboundCallsUseEgress(t *testing.T) {
	var allow atomic.Bool
	guarded := egress.New(egress.Options{})
	open := egress.New(egress.Options{AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	e := newEnv(t, sqliteStore, false, func(d *Deps) {
		d.HTTPClient = func() *http.Client {
			eg := guarded
			if allow.Load() {
				eg = open
			}
			c, err := eg.HTTPClient("", false)
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	})
	e.setSite(SettingPublicURL, e.srv.URL)
	admin := e.setup()

	idp := newFakeIdP(t)
	admin.must(200, "PATCH", "/api/v1/features/sso", map[string]any{"enabled": true})
	admin.must(201, "POST", "/api/v1/sso/providers", idp.provider(nil))
	idp.set(func(f *fakeIdP) { f.claims = person("alice-1", "alice@example.com", true) })
	if final, _ := noFollow(e.browser()).ssoFlow("corp", "/"); final != "/login?sso_error=provider_unavailable" {
		t.Fatalf("SSO through the guarded egress: %q", final)
	}
	allow.Store(true)
	if final, _ := noFollow(e.browser()).ssoFlow("corp", "/"); final != "/" {
		t.Fatalf("SSO once the egress allows the IdP: %q", final)
	}

	allow.Store(false)
	verifier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"success": true})
	}))
	t.Cleanup(verifier.Close)
	e.h.captchaEndpoint = verifier.URL
	admin.must(200, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{
		"security.captcha_provider": "turnstile", "security.captcha_site_key": "site-key",
		"security.captcha_secret": "captcha-secret", "security.login_captcha_after": 1}})
	login := func() (int, string) {
		e.h.logins.reset("127.0.0.1")
		e.loginStep1("admin", "wrong-password-x")
		code, out := e.browser().do("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "correct-horse-1", "captcha_token": "t"})
		return code, errCode(out)
	}
	if code, ec := login(); code != 503 || ec != "captcha_unavailable" {
		t.Fatalf("captcha through the guarded egress: %d %s", code, ec)
	}
	allow.Store(true)
	if code, ec := login(); code != 200 {
		t.Fatalf("captcha once the egress allows the verifier: %d %s", code, ec)
	}
}
