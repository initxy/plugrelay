package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/initxy/plugrelay/pkg/captcha"
)

const captchaSecretAD = "site:" + settingCaptchaSecret

// captchaConfig is the console's captcha setup (site settings); the
// secret is sealed under the master key.
func (h *Handler) captchaConfig(s Site) captcha.Config {
	c := captcha.Config{Provider: s.String(settingCaptchaProvider), SiteKey: s.String(settingCaptchaSiteKey),
		Endpoint: h.captchaEndpoint}
	if sealed := s.String(settingCaptchaSecret); sealed != "" && h.d.Box != nil {
		if b, err := h.d.Box.Open(sealed, captchaSecretAD); err == nil {
			c.Secret = string(b)
		}
	}
	return c
}

// loginCaptchaAfter is how many recent failures from an address make the
// password login ask for a captcha; 0 = never.
func loginCaptchaAfter(s Site) int {
	var n int
	_ = json.Unmarshal(s[settingLoginCaptcha], &n)
	return n
}

// publicCaptcha is what the login page needs to render the widget.
func (h *Handler) publicCaptcha(s Site) map[string]any {
	c := h.captchaConfig(s)
	n := loginCaptchaAfter(s)
	if !c.Enabled() || n <= 0 {
		return nil
	}
	return map[string]any{"provider": c.Provider, "site_key": c.SiteKey, "after_failures": n}
}

// httpClient is the egress of the admin API's own outbound calls (captcha
// verification, SSO): Deps.HTTPClient, each call bounded to 15 s.
func (h *Handler) httpClient() *http.Client {
	c := &http.Client{}
	if h.d.HTTPClient != nil {
		c = h.d.HTTPClient()
	}
	cp := *c
	if cp.Timeout == 0 {
		cp.Timeout = 15 * time.Second
	}
	return &cp
}

// checkLoginCaptcha asks for a captcha once an address has failed
// loginCaptchaAfter times; a provider outage fails closed.
func (h *Handler) checkLoginCaptcha(ctx context.Context, ip, token string, now time.Time) error {
	s, err := h.site(ctx)
	if err != nil {
		return err
	}
	cfg := h.captchaConfig(s)
	n := loginCaptchaAfter(s)
	if !cfg.Wanted() || n <= 0 || h.logins.count(ip, now) < n {
		return nil
	}
	if err := cfg.Check(); err != nil {
		// Turned on but unusable (say, a master key change left the secret
		// unreadable): refuse rather than drop the check.
		h.d.Logger.Error("login captcha is on but cannot run", "err", err)
		return newError(http.StatusServiceUnavailable, eCaptchaUnavailable)
	}
	err = captcha.Verify(ctx, h.httpClient(), cfg, token, ip)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, captcha.ErrMissing):
		return errForbidden(eCaptchaRequired)
	case errors.Is(err, captcha.ErrFailed):
		return errForbidden(eCaptchaFailed)
	}
	h.d.Logger.Warn("captcha verification unavailable", "provider", cfg.Provider, "err", err)
	return newError(http.StatusServiceUnavailable, eCaptchaUnavailable)
}

// CaptchaProviders lists the captcha provider the console's login uses,
// for the console's CSP.
func (h *Handler) CaptchaProviders(ctx context.Context) []string {
	s, err := h.site(ctx)
	if err != nil {
		return nil
	}
	if c := h.captchaConfig(s); c.Wanted() {
		return []string{c.Provider}
	}
	return nil
}
