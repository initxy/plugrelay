// Package captcha verifies Cloudflare Turnstile and hCaptcha tokens with
// the provider's siteverify endpoint. Callers pass the core's egress
// client; the package opens no connections of its own.
package captcha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Providers.
const (
	Off       = "off"
	Turnstile = "turnstile"
	HCaptcha  = "hcaptcha"
)

// Providers lists the accepted provider names, "off" first.
var Providers = []string{Off, Turnstile, HCaptcha}

// VerifyURL is each provider's documented siteverify endpoint.
var VerifyURL = map[string]string{
	Turnstile: "https://challenges.cloudflare.com/turnstile/v0/siteverify",
	HCaptcha:  "https://api.hcaptcha.com/siteverify",
}

// Errors returned by Verify.
var (
	// ErrMissing: no token was sent.
	ErrMissing = errors.New("captcha: token missing")
	// ErrFailed: the provider rejected the token.
	ErrFailed = errors.New("captcha: verification failed")
	// ErrUnavailable: the provider could not be asked; callers fail closed.
	ErrUnavailable = errors.New("captcha: provider unavailable")
)

// Config is one captcha setup.
type Config struct {
	Provider string
	SiteKey  string
	Secret   string
	// Endpoint overrides the provider's siteverify URL (tests, proxies).
	Endpoint string
	// Timeout bounds one siteverify call (DefaultTimeout when zero). A
	// timeout refuses the login or sign-up, so it errs on the long side:
	// siteverify answered in 4–16 s from some networks in testing.
	Timeout time.Duration
}

// DefaultTimeout is the siteverify call limit when Config.Timeout is zero.
const DefaultTimeout = 20 * time.Second

// Enabled reports whether a provider and both keys are set.
func (c Config) Enabled() bool {
	_, known := VerifyURL[c.Provider]
	return known && c.SiteKey != "" && c.Secret != ""
}

// Wanted reports whether a provider other than "off" is chosen, set up
// completely or not.
func (c Config) Wanted() bool { return c.Provider != "" && c.Provider != Off }

// Check explains why a wanted captcha cannot run (unknown provider, a key
// missing, a secret that could not be read); nil when it can or when none
// is wanted. A caller with a wanted but broken captcha refuses requests
// rather than skipping the check.
func (c Config) Check() error {
	if !c.Wanted() {
		return nil
	}
	if _, known := VerifyURL[c.Provider]; !known {
		return fmt.Errorf("captcha: unknown provider %q", c.Provider)
	}
	if c.SiteKey == "" {
		return errors.New("captcha: site key missing")
	}
	if c.Secret == "" {
		return errors.New("captcha: secret missing or unreadable")
	}
	return nil
}

type verifyResponse struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
}

// Verify checks token with the provider. remoteIP is optional.
func Verify(ctx context.Context, client *http.Client, c Config, token, remoteIP string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrMissing
	}
	if len(token) > 4096 {
		return ErrFailed
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = VerifyURL[c.Provider]
	}
	if endpoint == "" || c.Secret == "" {
		return fmt.Errorf("%w: not configured", ErrUnavailable)
	}
	form := url.Values{"secret": {c.Secret}, "response": {token}}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	if c.Provider == HCaptcha && c.SiteKey != "" {
		form.Set("sitekey", c.SiteKey)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	var out verifyResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !out.Success {
		return fmt.Errorf("%w: %s", ErrFailed, strings.Join(out.ErrorCodes, ","))
	}
	return nil
}
