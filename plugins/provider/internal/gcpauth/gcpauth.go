// Package gcpauth turns a Google Cloud service account key into OAuth 2.0
// access tokens with the JWT bearer grant (RFC 7523), as described in
// https://developers.google.com/identity/protocols/oauth2/service-account#authorizingrequests
// and keeps them cached until shortly before they expire.
package gcpauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/initxy/plugrelay/pkg/plugin"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Defaults of the token exchange.
const (
	ScopeCloudPlatform = "https://www.googleapis.com/auth/cloud-platform"
	DefaultTokenURI    = "https://oauth2.googleapis.com/token"
	grantType          = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	// assertionLifetime is the longest Google accepts.
	assertionLifetime = time.Hour
	maxTokenResponse  = 64 << 10
)

// ServiceAccount is a parsed service account key file.
type ServiceAccount struct {
	ProjectID    string
	ClientEmail  string
	PrivateKeyID string
	TokenURI     string
	key          *rsa.PrivateKey
}

// ParseServiceAccount reads the JSON key file Google issues for a service
// account. Other credential types (authorized_user, external_account) are
// refused.
func ParseServiceAccount(data []byte) (*ServiceAccount, error) {
	var f struct {
		Type         string `json:"type"`
		ProjectID    string `json:"project_id"`
		PrivateKeyID string `json:"private_key_id"`
		PrivateKey   string `json:"private_key"`
		ClientEmail  string `json:"client_email"`
		TokenURI     string `json:"token_uri"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, errors.New("gcpauth: service account key is not valid JSON")
	}
	if f.Type != "" && f.Type != "service_account" {
		return nil, fmt.Errorf("gcpauth: credential type %q is not supported, use a service account key", f.Type)
	}
	if f.ClientEmail == "" || f.PrivateKey == "" {
		return nil, errors.New("gcpauth: service account key needs client_email and private_key")
	}
	key, err := parseKey(f.PrivateKey)
	if err != nil {
		return nil, err
	}
	sa := &ServiceAccount{ProjectID: f.ProjectID, ClientEmail: f.ClientEmail, PrivateKeyID: f.PrivateKeyID, TokenURI: f.TokenURI, key: key}
	if sa.TokenURI == "" {
		sa.TokenURI = DefaultTokenURI
	}
	if err := checkTokenURI(sa.TokenURI); err != nil {
		return nil, err
	}
	return sa, nil
}

func parseKey(s string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("gcpauth: private_key is not PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, errors.New("gcpauth: private_key is not an RSA key")
	}
	k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("gcpauth: private_key cannot be parsed")
	}
	return k, nil
}

// checkTokenURI refuses to send a signed assertion in clear text, except to
// a loopback address (emulators and tests).
func checkTokenURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("gcpauth: token_uri %q is not a URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("gcpauth: token_uri %q must be https", raw)
}

// Token is an access token and its expiry.
type Token struct {
	AccessToken string
	Expiry      time.Time
}

// Assertion returns the signed JWT asking for scope.
func (sa *ServiceAccount) Assertion(scope string, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": sa.PrivateKeyID})
	claims, _ := json.Marshal(map[string]any{
		"iss":   sa.ClientEmail,
		"scope": scope,
		"aud":   sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(assertionLifetime).Unix(),
	})
	unsigned := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, sa.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("gcpauth: sign assertion: %w", err)
	}
	return unsigned + "." + enc.EncodeToString(sig), nil
}

// Exchange trades a fresh assertion for an access token at TokenURI.
func (sa *ServiceAccount) Exchange(ctx context.Context, client *http.Client, scope string, now time.Time) (Token, error) {
	if client == nil {
		return Token{}, errors.New("gcpauth: no HTTP client")
	}
	jwt, err := sa.Assertion(scope, now)
	if err != nil {
		return Token{}, err
	}
	form := url.Values{"grant_type": {grantType}, "assertion": {jwt}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sa.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("gcpauth: token request: %w", plugin.RedactURLError(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse))
	if err != nil {
		return Token{}, fmt.Errorf("gcpauth: token response: %w", err)
	}
	var tr struct {
		AccessToken string  `json:"access_token"`
		ExpiresIn   float64 `json:"expires_in"`
		Error       string  `json:"error"`
		Description string  `json:"error_description"`
	}
	_ = json.Unmarshal(body, &tr)
	if resp.StatusCode/100 != 2 || tr.AccessToken == "" {
		msg := strings.TrimSpace(tr.Error + " " + tr.Description)
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return Token{}, &ExchangeError{Status: resp.StatusCode, Message: msg}
	}
	exp := time.Duration(tr.ExpiresIn * float64(time.Second))
	if exp <= 0 {
		exp = assertionLifetime
	}
	return Token{AccessToken: tr.AccessToken, Expiry: now.Add(exp)}, nil
}

// ExchangeError is a refusal by the token endpoint (a deleted or disabled
// key answers 400 invalid_grant).
type ExchangeError struct {
	Status  int
	Message string
}

func (e *ExchangeError) Error() string {
	return fmt.Sprintf("gcpauth: token endpoint answered %d: %s", e.Status, e.Message)
}

// Cache holds one access token per key. A token is replaced Margin before
// it expires; concurrent callers for the same key wait for one exchange;
// a failed exchange is remembered for FailFor so a broken key is not
// retried on every request.
type Cache struct {
	Margin  time.Duration
	FailFor time.Duration

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	mu       sync.Mutex
	tok      Token
	err      error
	errUntil time.Time
}

// Get returns a valid token for key, calling fetch when there is none.
func (c *Cache) Get(ctx context.Context, key string, now func() time.Time, fetch func(context.Context) (Token, error)) (Token, error) {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[string]*entry{}
	}
	e := c.entries[key]
	if e == nil {
		e = &entry{}
		c.entries[key] = e
	}
	c.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	t := now()
	if e.tok.AccessToken != "" && t.Add(c.Margin).Before(e.tok.Expiry) {
		return e.tok, nil
	}
	if e.err != nil && t.Before(e.errUntil) {
		return Token{}, e.err
	}
	tok, err := fetch(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.err, e.errUntil = err, t.Add(c.FailFor)
		}
		return Token{}, err
	}
	e.tok, e.err = tok, nil
	return tok, nil
}

// Invalidate drops key's token and remembered failure.
func (c *Cache) Invalidate(key string) {
	c.mu.Lock()
	e := c.entries[key]
	delete(c.entries, key)
	c.mu.Unlock()
	if e != nil {
		e.mu.Lock()
		e.tok, e.err = Token{}, nil
		e.mu.Unlock()
	}
}
