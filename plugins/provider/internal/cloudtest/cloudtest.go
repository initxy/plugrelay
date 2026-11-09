// Package cloudtest holds test doubles for the cloud providers: a Google
// OAuth token endpoint that verifies service account assertions, and the
// plugintest fixtures of Bedrock's binary event stream. Tests only.
package cloudtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TokenServer is a fake https://oauth2.googleapis.com/token.
type TokenServer struct {
	URL string
	// ServiceAccount is a key file whose token_uri points at this server.
	ServiceAccount string
	ClientEmail    string
	// Exchanges counts successful token grants.
	Exchanges atomic.Int64

	mu      sync.Mutex
	fail    string // error code to answer with, "" = grant
	ttl     time.Duration
	issued  []string
	claims  []map[string]any
	pub     *rsa.PublicKey
	counter atomic.Int64
}

// NewTokenServer starts a token endpoint and generates a service account
// key for it; both are closed with t.
func NewTokenServer(t testing.TB) *TokenServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	ts := &TokenServer{pub: &key.PublicKey, ClientEmail: "relay@conformance.iam.gserviceaccount.com", ttl: time.Hour}
	srv := httptest.NewServer(http.HandlerFunc(ts.serve))
	t.Cleanup(srv.Close)
	ts.URL = srv.URL + "/token"
	sa, _ := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "conformance-project",
		"private_key_id": "kid-1",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   ts.ClientEmail,
		"client_id":      "1",
		"token_uri":      ts.URL,
	})
	ts.ServiceAccount = string(sa)
	return ts
}

// Fail makes the server refuse grants with an OAuth error code ("" to
// grant again).
func (ts *TokenServer) Fail(code string) { ts.mu.Lock(); ts.fail = code; ts.mu.Unlock() }

// Issued returns the access tokens granted so far.
func (ts *TokenServer) Issued() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string(nil), ts.issued...)
}

// Claims returns the verified claims of every granted assertion.
func (ts *TokenServer) Claims() []map[string]any {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]map[string]any(nil), ts.claims...)
}

func (ts *TokenServer) serve(w http.ResponseWriter, r *http.Request) {
	refuse := func(status int, code, desc string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
	}
	if r.Method != http.MethodPost || r.ParseForm() != nil {
		refuse(400, "invalid_request", "POST a form")
		return
	}
	if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		refuse(400, "unsupported_grant_type", r.PostForm.Get("grant_type"))
		return
	}
	claims, err := ts.verify(r.PostForm.Get("assertion"))
	if err != nil {
		refuse(400, "invalid_grant", err.Error())
		return
	}
	ts.mu.Lock()
	fail, ttl := ts.fail, ts.ttl
	ts.mu.Unlock()
	if fail != "" {
		refuse(400, fail, "refused by test")
		return
	}
	tok := fmt.Sprintf("ya29.test-%d", ts.counter.Add(1))
	ts.mu.Lock()
	ts.issued = append(ts.issued, tok)
	ts.claims = append(ts.claims, claims)
	ts.mu.Unlock()
	ts.Exchanges.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "expires_in": int(ttl / time.Second), "token_type": "Bearer"})
}

// verify checks the RS256 signature and the claims Google requires.
func (ts *TokenServer) verify(jwt string) (map[string]any, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("assertion is not a JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(ts.pub, crypto.SHA256, sum[:], sig); err != nil {
		return nil, fmt.Errorf("bad signature")
	}
	var header map[string]any
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if json.Unmarshal(hb, &header) != nil || header["alg"] != "RS256" || header["kid"] != "kid-1" {
		return nil, fmt.Errorf("bad header %s", hb)
	}
	var c map[string]any
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(cb, &c); err != nil {
		return nil, err
	}
	iat, _ := c["iat"].(float64)
	exp, _ := c["exp"].(float64)
	switch {
	case c["iss"] != ts.ClientEmail:
		return nil, fmt.Errorf("iss %v", c["iss"])
	case c["aud"] != ts.URL:
		return nil, fmt.Errorf("aud %v", c["aud"])
	case c["scope"] != "https://www.googleapis.com/auth/cloud-platform":
		return nil, fmt.Errorf("scope %v", c["scope"])
	case exp-iat <= 0 || exp-iat > 3600:
		return nil, fmt.Errorf("lifetime %v", exp-iat)
	}
	return c, nil
}
