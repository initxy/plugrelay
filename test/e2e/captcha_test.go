//go:build e2e

package e2e

// The console's login captcha and the sell kit's sign-up captcha against
// the providers' real siteverify endpoints, with the test keys they
// publish (checked 2026-11-06):
//
//	Turnstile https://developers.cloudflare.com/turnstile/troubleshooting/testing/
//	  secret 1x0000000000000000000000000000000AA always passes,
//	  2x0000000000000000000000000000000AA always fails; test secrets accept
//	  only the dummy token XXXX.DUMMY.TOKEN.XXXX
//	hCaptcha  https://docs.hcaptcha.com/#integration-testing-test-keys
//	  site key 10000000-ffff-ffff-ffff-000000000001, secret
//	  0x0000000000000000000000000000000000000000, response token
//	  10000000-aaaa-bbbb-cccc-000000000001
//
//	PLUGRELAY_E2E_CAPTCHA=1   opt in (needs direct HTTPS to both providers:
//	                          the relay's egress does not use HTTP_PROXY)

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	turnstilePassSecret = "1x0000000000000000000000000000000AA"
	turnstileFailSecret = "2x0000000000000000000000000000000AA"
	turnstileSiteKey    = "1x00000000000000000000AA"
	turnstileToken      = "XXXX.DUMMY.TOKEN.XXXX"
	hcaptchaSiteKey     = "10000000-ffff-ffff-ffff-000000000001"
	hcaptchaSecret      = "0x0000000000000000000000000000000000000000"
	hcaptchaToken       = "10000000-aaaa-bbbb-cccc-000000000001"
)

func TestCaptchaLive(t *testing.T) {
	if os.Getenv("PLUGRELAY_E2E_CAPTCHA") == "" {
		t.Skip("set PLUGRELAY_E2E_CAPTCHA=1 to verify captchas against Turnstile's and hCaptcha's real siteverify")
	}
	r := startWith(t, "http://127.0.0.1:9/v1",
		[]string{"PLUGRELAY_ADMIN_TOKEN=" + e2eAdminToken, "PLUGRELAY_E2E_KEY=unused"})
	const user, pass = "owner@localhost", "correct-horse-battery-1"
	r.run("user", "reset-password", "--user", user, "--password", pass)

	site := func(provider, siteKey, secret string) {
		r.admin(t, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{
			"security.captcha_provider": provider, "security.captcha_site_key": siteKey,
			"security.captcha_secret": secret, "security.login_captcha_after": 1}}, nil)
	}
	login := func(password, token string) (int, string) {
		return publicPost(t, r.base+"/api/v1/auth/login", map[string]any{"username": user, "password": password, "captcha_token": token})
	}
	// One wrong password from this address makes the next login ask; once
	// it asks, a wrong password is refused at the captcha already.
	fail := func(t *testing.T) {
		t.Helper()
		if code, ec := login("wrong-password-123", ""); code != http.StatusUnauthorized && ec != "captcha_required" {
			t.Fatalf("wrong password: %d %s", code, ec)
		}
	}
	expect := func(t *testing.T, what string, code int, ec string, wantCode int, wantEC string) {
		t.Helper()
		if code != wantCode || ec != wantEC {
			t.Fatalf("%s: %d %q, want %d %q", what, code, ec, wantCode, wantEC)
		}
		t.Logf("%s: %d %s", what, code, ec)
	}

	t.Run("login turnstile", func(t *testing.T) {
		site("turnstile", turnstileSiteKey, turnstilePassSecret)
		fail(t)
		code, ec := login(pass, "")
		expect(t, "no token", code, ec, http.StatusForbidden, "captcha_required")
		start := time.Now()
		code, ec = login(pass, turnstileToken)
		expect(t, "always-pass secret", code, ec, http.StatusOK, "")
		t.Logf("siteverify round trip through the relay: %s", time.Since(start).Round(time.Millisecond))

		site("turnstile", turnstileSiteKey, turnstileFailSecret)
		fail(t)
		code, ec = login(pass, turnstileToken)
		expect(t, "always-fail secret", code, ec, http.StatusForbidden, "captcha_failed")
	})

	t.Run("login hcaptcha", func(t *testing.T) {
		site("hcaptcha", hcaptchaSiteKey, hcaptchaSecret)
		fail(t)
		code, ec := login(pass, "FAKE-TOKEN")
		expect(t, "nonsense token", code, ec, http.StatusForbidden, "captcha_failed")
		start := time.Now()
		code, ec = login(pass, hcaptchaToken)
		expect(t, "test token", code, ec, http.StatusOK, "")
		t.Logf("siteverify round trip through the relay: %s", time.Since(start).Round(time.Millisecond))
	})

	t.Run("sign-up", func(t *testing.T) {
		doc := filepath.Join(t.TempDir(), "signup.yaml")
		_ = os.WriteFile(doc, []byte("features:\n  - {id: signup, enabled: true}\n"), 0o600)
		r.run("apply", "-f", doc)
		signup := func(provider, siteKey, secret string) {
			r.admin(t, "PATCH", "/api/v1/features/signup", map[string]any{"settings": map[string]any{"admin.signup": map[string]any{
				"captcha_provider": provider, "captcha_site_key": siteKey, "captcha_secret": secret, "per_ip_per_hour": 100}}}, nil)
		}
		n := 0
		register := func(token string) (int, string) {
			n++
			return publicPost(t, r.base+"/api/plugins/admin.signup/register", map[string]any{
				"email": "signup" + string(rune('a'+n)) + "@example.com", "password": "signup-password-1", "captcha_token": token})
		}
		signup("turnstile", turnstileSiteKey, turnstilePassSecret)
		var code int
		var ec string
		// The plugin and its settings apply on the next reload.
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(500 * time.Millisecond) {
			if code, ec = register(""); code == http.StatusForbidden && ec == "captcha_required" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("sign-up without a token: %d %q, want 403 captcha_required", code, ec)
			}
		}
		t.Logf("turnstile, no token: %d %s", code, ec)
		code, ec = register(turnstileToken)
		expect(t, "turnstile always-pass secret", code, ec, http.StatusCreated, "")

		signup("turnstile", turnstileSiteKey, turnstileFailSecret)
		waitFor(t, func() bool { code, ec = register(turnstileToken); return code == http.StatusForbidden }, "the fail secret")
		expect(t, "turnstile always-fail secret", code, ec, http.StatusForbidden, "captcha_failed")

		signup("hcaptcha", hcaptchaSiteKey, hcaptchaSecret)
		waitFor(t, func() bool {
			code, ec = register(hcaptchaToken)
			return code != http.StatusForbidden || ec != "captcha_failed"
		}, "hcaptcha")
		expect(t, "hcaptcha test token", code, ec, http.StatusCreated, "")
		code, ec = register("FAKE-TOKEN")
		expect(t, "hcaptcha nonsense token", code, ec, http.StatusForbidden, "captcha_failed")
	})
}

// waitFor retries cond until it holds or 20 seconds pass (settings apply
// on the next reload).
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !cond(); time.Sleep(500 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never took effect", what)
		}
	}
}

// publicPost sends JSON without credentials and returns the status and the
// error code of the body ("" on success).
func publicPost(t *testing.T, url string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 == 2 {
		return resp.StatusCode, ""
	}
	var e struct {
		Code  string `json:"code"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &e)
	if e.Code == "" {
		e.Code = e.Error.Code
	}
	if e.Code == "" {
		e.Code = string(data)
	}
	return resp.StatusCode, e.Code
}
