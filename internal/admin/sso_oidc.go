package admin

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	discoveryTTL = time.Hour
	jwksTTL      = time.Hour
	// jwksMinRefresh bounds refetches triggered by an unknown key id.
	jwksMinRefresh = 30 * time.Second
	clockSkew      = time.Minute
	maxIdPBody     = 1 << 20
)

type oidcDiscovery struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	UserinfoEndpoint      string   `json:"userinfo_endpoint"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
}

type cachedDiscovery struct {
	doc     *oidcDiscovery
	fetched time.Time
}

type cachedJWKS struct {
	keys    map[string]crypto.PublicKey // by kid; "" holds a lone kid-less key
	fetched time.Time
}

// ssoCache holds discovery documents and signing keys per provider. Its
// zero value is ready.
type ssoCache struct {
	mu   sync.Mutex
	disc map[string]cachedDiscovery // by provider id
	jwks map[string]*cachedJWKS     // by jwks_uri
	// inflight coalesces concurrent JWKS fetches of one URI.
	inflight map[string]*jwksFetch
}

type jwksFetch struct {
	done chan struct{}
	c    *cachedJWKS
	err  error
}

func (s *ssoCache) forget(providerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.disc, providerID)
}

// idpClient makes the calls to an identity provider (discovery, keys,
// token, user info) through the admin API's egress (Handler.httpClient).
type idpClient struct{ hc *http.Client }

func (h *Handler) idp() idpClient { return idpClient{hc: h.httpClient()} }

// getJSON fetches url and decodes a JSON body of at most maxIdPBody.
func (c idpClient) getJSON(ctx context.Context, rawURL, bearer string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return c.doJSON(req, v)
}

func (c idpClient) doJSON(req *http.Request, v any) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIdPBody))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
			Desc  string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error != "" {
			return fmt.Errorf("%s: HTTP %d %s %s", req.URL.Host, resp.StatusCode, e.Error, e.Desc)
		}
		return fmt.Errorf("%s: HTTP %d", req.URL.Host, resp.StatusCode)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: bad JSON: %w", req.URL.Host, err)
	}
	return nil
}

// oidcIssuer is the issuer a provider is configured with.
func oidcIssuer(p *ssoProvider) string {
	switch p.Preset {
	case presetGoogle:
		return "https://accounts.google.com"
	case presetMicrosoft:
		return microsoftAuthority + "/" + p.Tenant + "/v2.0"
	}
	return p.Issuer
}

// discover returns the provider's discovery document, cached.
func (s *ssoCache) discover(ctx context.Context, c idpClient, p *ssoProvider, now time.Time) (*oidcDiscovery, error) {
	s.mu.Lock()
	if d, ok := s.disc[p.ID]; ok && now.Sub(d.fetched) < discoveryTTL {
		s.mu.Unlock()
		return d.doc, nil
	}
	s.mu.Unlock()
	iss := oidcIssuer(p)
	var doc oidcDiscovery
	if err := c.getJSON(ctx, strings.TrimRight(iss, "/")+"/.well-known/openid-configuration", "", &doc); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	// Entra publishes its own issuer form (a tenant GUID or a {tenantid}
	// template) for the authority it was fetched from; elsewhere the
	// document must name the configured issuer.
	if p.Preset != presetMicrosoft && strings.TrimRight(doc.Issuer, "/") != strings.TrimRight(iss, "/") {
		return nil, fmt.Errorf("discovery: issuer %q does not match configured %q", doc.Issuer, iss)
	}
	if p.Preset == presetMicrosoft && !strings.HasPrefix(doc.Issuer, microsoftAuthority+"/") {
		return nil, fmt.Errorf("discovery: unexpected issuer %q", doc.Issuer)
	}
	for name, u := range map[string]string{"authorization_endpoint": doc.AuthorizationEndpoint,
		"token_endpoint": doc.TokenEndpoint, "jwks_uri": doc.JWKSURI} {
		if err := checkEndpointURL(u); err != nil {
			return nil, fmt.Errorf("discovery: %s %w", name, err)
		}
	}
	s.mu.Lock()
	if s.disc == nil {
		s.disc = map[string]cachedDiscovery{}
	}
	s.disc[p.ID] = cachedDiscovery{doc: &doc, fetched: now}
	s.mu.Unlock()
	return &doc, nil
}

// key returns the signing key kid from jwksURI, refetching the set when
// it is stale or does not hold kid (key rotation).
func (s *ssoCache) key(ctx context.Context, idp idpClient, jwksURI, kid string, now time.Time) (crypto.PublicKey, error) {
	s.mu.Lock()
	c := s.jwks[jwksURI]
	if c != nil {
		if k, ok := c.lookup(kid); ok && now.Sub(c.fetched) < jwksTTL {
			s.mu.Unlock()
			return k, nil
		}
		if now.Sub(c.fetched) < jwksMinRefresh {
			s.mu.Unlock()
			if k, ok := c.lookup(kid); ok {
				return k, nil
			}
			return nil, fmt.Errorf("unknown signing key %q", kid)
		}
	}
	f := s.inflight[jwksURI]
	if f == nil {
		f = &jwksFetch{done: make(chan struct{})}
		if s.inflight == nil {
			s.inflight = map[string]*jwksFetch{}
		}
		s.inflight[jwksURI] = f
		s.mu.Unlock()
		// Detached from the caller so one cancelled sign-in does not fail
		// the others waiting on it; the client bounds the time.
		keys, err := idp.fetchJWKS(context.WithoutCancel(ctx), jwksURI)
		s.mu.Lock()
		if err == nil {
			f.c = &cachedJWKS{keys: keys, fetched: now}
			if s.jwks == nil {
				s.jwks = map[string]*cachedJWKS{}
			}
			s.jwks[jwksURI] = f.c
		}
		f.err = err
		delete(s.inflight, jwksURI)
		close(f.done)
	}
	s.mu.Unlock()
	select {
	case <-f.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	if k, ok := f.c.lookup(kid); ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

func (c *cachedJWKS) lookup(kid string) (crypto.PublicKey, bool) {
	if k, ok := c.keys[kid]; ok {
		return k, true
	}
	// A token without kid may use the set's only key.
	if kid == "" && len(c.keys) == 1 {
		for _, k := range c.keys {
			return k, true
		}
	}
	return nil, false
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (c idpClient) fetchJWKS(ctx context.Context, uri string) (map[string]crypto.PublicKey, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := c.getJSON(ctx, uri, "", &set); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	out := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := k.public()
		if err != nil {
			continue // keys of unsupported types are skipped, not fatal
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, errors.New("jwks: no usable signing keys")
	}
	return out, nil
}

func (k jwk) public() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err1 := b64url(k.N)
		e, err2 := b64url(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("bad RSA key")
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			return nil, errors.New("RSA key too short")
		}
		return pub, nil
	case "EC":
		var curve elliptic.Curve
		size := 0
		switch k.Crv {
		case "P-256":
			curve, size = elliptic.P256(), 32
		case "P-384":
			curve, size = elliptic.P384(), 48
		case "P-521":
			curve, size = elliptic.P521(), 66
		default:
			return nil, errors.New("unsupported curve")
		}
		x, err1 := b64url(k.X)
		y, err2 := b64url(k.Y)
		if err1 != nil || err2 != nil || len(x) != size || len(y) != size {
			return nil, errors.New("bad EC key")
		}
		return ecdsa.ParseUncompressedPublicKey(curve, append(append([]byte{4}, x...), y...))
	}
	return nil, errors.New("unsupported key type")
}

// jwtParts splits and decodes a compact JWS.
func jwtParts(tok string) (header, claims map[string]any, signed, sig []byte, err error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, nil, nil, nil, errors.New("malformed token")
	}
	hb, err1 := b64url(parts[0])
	cb, err2 := b64url(parts[1])
	sig, err3 := b64url(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, nil, nil, nil, errors.New("malformed token")
	}
	if json.Unmarshal(hb, &header) != nil {
		return nil, nil, nil, nil, errors.New("malformed token header")
	}
	dec := json.NewDecoder(strings.NewReader(string(cb)))
	dec.UseNumber()
	if dec.Decode(&claims) != nil {
		return nil, nil, nil, nil, errors.New("malformed token claims")
	}
	return header, claims, []byte(parts[0] + "." + parts[1]), sig, nil
}

// verifySignature checks a JWS signature with an asymmetric algorithm.
// "none" and HMAC algorithms are refused: the client secret must never
// double as a token key.
func verifySignature(alg string, pub crypto.PublicKey, signed, sig []byte) error {
	var h crypto.Hash
	switch alg[2:] {
	case "256":
		h = crypto.SHA256
	case "384":
		h = crypto.SHA384
	case "512":
		h = crypto.SHA512
	default:
		return fmt.Errorf("unsupported algorithm %q", alg)
	}
	hasher := h.New()
	hasher.Write(signed)
	digest := hasher.Sum(nil)
	switch alg[:2] {
	case "RS", "PS":
		k, ok := pub.(*rsa.PublicKey)
		if !ok {
			return errors.New("key type does not match algorithm")
		}
		if alg[:2] == "RS" {
			return rsa.VerifyPKCS1v15(k, h, digest, sig)
		}
		return rsa.VerifyPSS(k, h, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	case "ES":
		k, ok := pub.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("key type does not match algorithm")
		}
		size := (k.Curve.Params().BitSize + 7) / 8
		want := map[string]int{"256": 32, "384": 48, "512": 66}[alg[2:]]
		if size != want || len(sig) != 2*size {
			return errors.New("bad ECDSA signature")
		}
		r, s := new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(k, digest, r, s) {
			return errors.New("bad ECDSA signature")
		}
		return nil
	}
	return fmt.Errorf("unsupported algorithm %q", alg)
}

var jwsAlgs = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"}

// verifyIDToken checks an ID token's signature against the provider's
// JWKS and its iss, aud, azp, exp, nbf, iat and nonce claims.
func (h *Handler) verifyIDToken(ctx context.Context, p *ssoProvider, doc *oidcDiscovery, raw, nonce string) (map[string]any, error) {
	header, claims, signed, sig, err := jwtParts(raw)
	if err != nil {
		return nil, err
	}
	alg, _ := header["alg"].(string)
	kid, _ := header["kid"].(string)
	known := false
	for _, a := range jwsAlgs {
		known = known || a == alg
	}
	if !known {
		return nil, fmt.Errorf("unsupported algorithm %q", alg)
	}
	now := h.d.Now()
	pub, err := h.sso.key(ctx, h.idp(), doc.JWKSURI, kid, now)
	if err != nil {
		return nil, err
	}
	if err := verifySignature(alg, pub, signed, sig); err != nil {
		return nil, fmt.Errorf("bad signature: %w", err)
	}
	if err := checkIDClaims(p, doc, claims, now, nonce); err != nil {
		return nil, err
	}
	return claims, nil
}

func numClaim(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
	case float64:
		return int64(n), true
	}
	return 0, false
}

func checkIDClaims(p *ssoProvider, doc *oidcDiscovery, claims map[string]any, now time.Time, nonce string) error {
	iss, _ := claims["iss"].(string)
	want := []string{doc.Issuer}
	switch p.Preset {
	case presetGoogle:
		want = append(want, "accounts.google.com")
	case presetMicrosoft:
		if tid, _ := claims["tid"].(string); tid != "" {
			want = []string{strings.ReplaceAll(doc.Issuer, "{tenantid}", tid)}
		}
	}
	okIss := false
	for _, w := range want {
		okIss = okIss || (iss != "" && iss == w)
	}
	if !okIss {
		return fmt.Errorf("wrong issuer %q", iss)
	}
	var aud []string
	switch a := claims["aud"].(type) {
	case string:
		aud = []string{a}
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok {
				aud = append(aud, s)
			}
		}
	}
	okAud := false
	for _, a := range aud {
		okAud = okAud || a == p.ClientID
	}
	if !okAud {
		return errors.New("token is not for this client (aud)")
	}
	if azp, ok := claims["azp"].(string); ok && azp != "" && azp != p.ClientID {
		return errors.New("token is not for this client (azp)")
	}
	exp, ok := numClaim(claims["exp"])
	if !ok || now.After(time.Unix(exp, 0).Add(clockSkew)) {
		return errors.New("token expired")
	}
	if nbf, ok := numClaim(claims["nbf"]); ok && now.Add(clockSkew).Before(time.Unix(nbf, 0)) {
		return errors.New("token not valid yet")
	}
	if iat, ok := numClaim(claims["iat"]); ok && now.Add(5*time.Minute).Before(time.Unix(iat, 0)) {
		return errors.New("token issued in the future")
	}
	got, _ := claims["nonce"].(string)
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(nonce)) != 1 {
		return errors.New("nonce mismatch")
	}
	if sub, _ := claims["sub"].(string); sub == "" {
		return errors.New("token has no subject")
	}
	return nil
}
