package admin

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
)

// fakeIdP is an OIDC provider (discovery, JWKS, authorize, token,
// userinfo) plus GitHub- and Feishu-shaped OAuth endpoints.
type fakeIdP struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	key      *rsa.PrivateKey
	kid      string
	signKey  *rsa.PrivateKey // nil: key
	clientID string
	secret   string
	// claims is the person who signs in next; mod edits the ID token.
	claims map[string]any
	mod    func(map[string]any)
	grants map[string]grant
	// GitHub / Feishu answers.
	ghUser   map[string]any
	ghEmails []map[string]any
	feishu   map[string]any
}

type grant struct {
	nonce, challenge, redirect string
	claims                     map[string]any
}

func newRSAKey(t *testing.T) *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newFakeIdP(t *testing.T) *fakeIdP {
	f := &fakeIdP{t: t, key: newRSAKey(t), kid: "k1", clientID: "relay-client", secret: "relay-secret", grants: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint": f.srv.URL + "/token", "jwks_uri": f.srv.URL + "/jwks", "userinfo_endpoint": f.srv.URL + "/userinfo",
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic"}})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		e := big.NewInt(int64(f.key.E)).Bytes()
		writeJSON(w, 200, map[string]any{"keys": []map[string]any{{"kty": "RSA", "kid": f.kid, "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e)}}})
	})
	authorize := func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != f.clientID || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			http.Error(w, "bad authorize request", 400)
			return
		}
		f.mu.Lock()
		code := "code-" + q.Get("state")[:8] + "-" + time.Now().Format("150405.000000")
		cl := map[string]any{}
		for k, v := range f.claims {
			cl[k] = v
		}
		f.grants[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), claims: cl}
		f.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	}
	mux.HandleFunc("GET /authorize", authorize)
	mux.HandleFunc("GET /gh/authorize", authorize)
	mux.HandleFunc("GET /fs/authorize", authorize)
	redeem := func(r *http.Request, code, verifier, redirect string) (grant, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		g, ok := f.grants[code]
		delete(f.grants, code)
		sum := sha256.Sum256([]byte(verifier))
		return g, ok && g.redirect == redirect && base64.RawURLEncoding.EncodeToString(sum[:]) == g.challenge
	}
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if user != f.clientID || pass != f.secret {
			writeJSON(w, 401, map[string]any{"error": "invalid_client"})
			return
		}
		g, ok := redeem(r, r.FormValue("code"), r.FormValue("code_verifier"), r.FormValue("redirect_uri"))
		if !ok || r.FormValue("grant_type") != "authorization_code" {
			writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
			return
		}
		now := time.Now()
		claims := map[string]any{"iss": f.srv.URL, "aud": f.clientID, "nonce": g.nonce, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
		for k, v := range g.claims {
			claims[k] = v
		}
		f.mu.Lock()
		mod, key, kid := f.mod, f.key, f.kid
		if f.signKey != nil {
			key = f.signKey
		}
		f.mu.Unlock()
		if mod != nil {
			mod(claims)
		}
		writeJSON(w, 200, map[string]any{"access_token": "at-" + g.nonce, "token_type": "Bearer", "id_token": signJWT(t, key, kid, claims)})
	})
	mux.HandleFunc("POST /gh/token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("client_id") != f.clientID || r.FormValue("client_secret") != f.secret {
			writeJSON(w, 200, map[string]any{"error": "incorrect_client_credentials"})
			return
		}
		if _, ok := redeem(r, r.FormValue("code"), r.FormValue("code_verifier"), r.FormValue("redirect_uri")); !ok {
			writeJSON(w, 200, map[string]any{"error": "bad_verification_code"})
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": "gho_test", "token_type": "bearer"})
	})
	bearerOK := func(r *http.Request, tok string) bool { return r.Header.Get("Authorization") == "Bearer "+tok }
	mux.HandleFunc("GET /gh/user", func(w http.ResponseWriter, r *http.Request) {
		if !bearerOK(r, "gho_test") {
			w.WriteHeader(401)
			return
		}
		writeJSON(w, 200, f.ghUser)
	})
	mux.HandleFunc("GET /gh/emails", func(w http.ResponseWriter, r *http.Request) {
		if !bearerOK(r, "gho_test") {
			w.WriteHeader(401)
			return
		}
		writeJSON(w, 200, f.ghEmails)
	})
	mux.HandleFunc("POST /fs/token", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["client_id"] != f.clientID || in["client_secret"] != f.secret {
			writeJSON(w, 400, map[string]any{"code": 20002, "error": "invalid_client"})
			return
		}
		if _, ok := redeem(r, in["code"], in["code_verifier"], in["redirect_uri"]); !ok {
			writeJSON(w, 400, map[string]any{"code": 20003, "error": "invalid_grant"})
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "access_token": "u-fs", "expires_in": 7200, "token_type": "Bearer"})
	})
	mux.HandleFunc("GET /fs/user_info", func(w http.ResponseWriter, r *http.Request) {
		if !bearerOK(r, "u-fs") {
			writeJSON(w, 200, map[string]any{"code": 20005, "msg": "invalid token"})
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "msg": "success", "data": f.feishu})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	saved := map[string]oauthURLs{}
	for k, v := range presetURLs {
		saved[k] = v
	}
	presetURLs[presetGitHub] = oauthURLs{auth: f.srv.URL + "/gh/authorize", token: f.srv.URL + "/gh/token",
		userinfo: f.srv.URL + "/gh/user", emails: f.srv.URL + "/gh/emails"}
	presetURLs[presetFeishu] = oauthURLs{auth: f.srv.URL + "/fs/authorize", token: f.srv.URL + "/fs/token", userinfo: f.srv.URL + "/fs/user_info"}
	t.Cleanup(func() {
		for k, v := range saved {
			presetURLs[k] = v
		}
	})
	return f
}

func signJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	h, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid})
	c, _ := json.Marshal(claims)
	signed := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *fakeIdP) set(fn func(f *fakeIdP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// noFollow makes a browser stop at redirects so each hop can be checked.
func noFollow(c *client) *client {
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func (c *client) get(rawURL string) *http.Response {
	c.e.t.Helper()
	resp, err := c.http.Get(rawURL)
	if err != nil {
		c.e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// ssoFlow runs start → IdP authorize → callback and returns the final
// redirect target and the callback URL.
func (c *client) ssoFlow(provider, returnTo string) (final, callback string) {
	c.e.t.Helper()
	resp := c.get(c.e.srv.URL + "/api/v1/auth/sso/" + provider + "/start?return_to=" + url.QueryEscape(returnTo))
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || loc == "" {
		c.e.t.Fatalf("start: %d %q", resp.StatusCode, loc)
	}
	if strings.HasPrefix(loc, "/login") {
		return loc, ""
	}
	resp = c.get(loc)
	callback = resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound {
		c.e.t.Fatalf("authorize: %d", resp.StatusCode)
	}
	resp = c.get(callback)
	return resp.Header.Get("Location"), callback
}

func ssoEnv(t *testing.T, open opener) (*env, *fakeIdP, *client) {
	e := newEnv(t, open, false)
	e.setSite(SettingPublicURL, e.srv.URL)
	idp := newFakeIdP(t)
	admin := e.setup()
	admin.must(200, "PATCH", "/api/v1/features/sso", map[string]any{"enabled": true})
	return e, idp, admin
}

func (f *fakeIdP) provider(extra map[string]any) map[string]any {
	p := map[string]any{"id": "corp", "name": "Corp SSO", "preset": "oidc", "issuer": f.srv.URL,
		"client_id": f.clientID, "client_secret": f.secret, "auto_create": true}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func person(sub, email string, verified bool) map[string]any {
	return map[string]any{"sub": sub, "email": email, "email_verified": verified, "name": "Person " + sub}
}

func TestSSO(t *testing.T) {
	for name, open := range stores(t) {
		t.Run(name, func(t *testing.T) {
			t.Run("OIDC", func(t *testing.T) { testSSOOIDC(t, open) })
			t.Run("TokenChecks", func(t *testing.T) { testSSOTokenChecks(t, open) })
			t.Run("Policy", func(t *testing.T) { testSSOPolicy(t, open) })
			t.Run("OAuthPresets", func(t *testing.T) { testSSOOAuthPresets(t, open) })
			t.Run("PasswordLogin", func(t *testing.T) { testSSOPasswordLogin(t, open) })
		})
	}
}

func testSSOOIDC(t *testing.T, open opener) {
	e, idp, admin := ssoEnv(t, open)
	g := admin.must(201, "POST", "/api/v1/groups", map[string]any{"name": "Eng"})
	p := admin.must(201, "POST", "/api/v1/sso/providers", idp.provider(map[string]any{
		"allowed_domains": []string{"Example.com"},
		"group_rules":     []map[string]any{{"claim": "groups", "value": "eng", "group_id": g["id"]}}}))
	if p["client_secret"] != nil || p["has_client_secret"] != true || p["redirect_uri"] != e.srv.URL+"/api/v1/auth/sso/corp/callback" {
		t.Fatalf("provider view: %v", p)
	}
	raw, _, _ := e.st.KV(ssoKV).Get(context.Background(), ssoConfigKey)
	if strings.Contains(string(raw), idp.secret) {
		t.Fatal("client secret stored in plaintext")
	}

	anon := e.browser()
	out := anon.must(200, "GET", "/api/v1/auth/providers", nil)
	if out["password_login"] != true || len(out["providers"].([]any)) != 1 || out["site"].(map[string]any)["name"] != "plugrelay" {
		t.Fatalf("providers: %v", out)
	}

	idp.set(func(f *fakeIdP) {
		f.claims = person("alice-1", "Alice@Example.com", true)
		f.claims["groups"] = []string{"eng", "ops"}
	})
	b := noFollow(e.browser())
	final, callback := b.ssoFlow("corp", "/keys?tab=mine")
	if final != "/keys?tab=mine" {
		t.Fatalf("final redirect %q", final)
	}
	me := b.must(200, "GET", "/api/v1/me", nil)
	u := me["user"].(map[string]any)
	if u["username"] != "alice@example.com" || u["role"] != "user" || u["group_id"] != g["id"] || me["auth"] != "session" {
		t.Fatalf("me: %v", me)
	}
	if b.csrfToken() == "" {
		t.Fatal("no CSRF cookie after SSO login")
	}
	// The SSO session is a normal session: unsafe calls need CSRF.
	b.must(201, "POST", "/api/v1/keys", map[string]any{"name": "laptop"})

	// Replaying the callback (same state, cookie put back by hand) fails.
	cu, _ := url.Parse(callback)
	req, _ := http.NewRequest("GET", callback, nil)
	req.AddCookie(&http.Cookie{Name: ssoStateCookie, Value: cu.Query().Get("state")})
	resp, err := noFollow(e.browser()).http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/login?sso_error=state_replayed" {
		t.Fatalf("replay: %q", loc)
	}

	// Second sign-in finds the linked identity even if the email changed.
	idp.set(func(f *fakeIdP) { f.claims = person("alice-1", "alice@example.com", true) })
	b2 := noFollow(e.browser())
	if final, _ := b2.ssoFlow("corp", ""); final != "/" {
		t.Fatalf("second login: %q", final)
	}
	if b2.must(200, "GET", "/api/v1/me", nil)["user"].(map[string]any)["id"] != u["id"] {
		t.Fatal("second login made another user")
	}
	ids := admin.must(200, "GET", "/api/v1/users/"+u["id"].(string)+"/identities", nil)["items"].([]any)
	if len(ids) != 1 || ids[0].(map[string]any)["provider_name"] != "Corp SSO" {
		t.Fatalf("identities: %v", ids)
	}

	// Open redirects are cut to "/".
	for _, rt := range []string{"//evil.example/x", "https://evil.example/", "/\\evil.example", "/api/v1/me", "javascript:alert(1)"} {
		if final, _ := noFollow(e.browser()).ssoFlow("corp", rt); final != "/" {
			t.Fatalf("return_to %q → %q", rt, final)
		}
	}

	// Audit has the login, the creation and the link.
	got := map[string]bool{}
	for _, it := range admin.must(200, "GET", "/api/v1/audit?limit=100", nil)["items"].([]any) {
		got[it.(map[string]any)["action"].(string)] = true
	}
	for _, a := range []string{"auth.login", "user.create", "user.identity.link", "sso.provider.create"} {
		if !got[a] {
			t.Errorf("audit lacks %s: %v", a, got)
		}
	}

	// Unlinking removes the identity.
	iid := ids[0].(map[string]any)["id"].(string)
	admin.must(204, "DELETE", "/api/v1/users/"+u["id"].(string)+"/identities/"+iid, nil)
	admin.must(404, "DELETE", "/api/v1/users/"+u["id"].(string)+"/identities/"+iid, nil)
}

func testSSOTokenChecks(t *testing.T, open opener) {
	e, idp, admin := ssoEnv(t, open)
	clock := time.Now()
	var clockMu sync.Mutex
	e.h.d.Now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	admin.must(201, "POST", "/api/v1/sso/providers", idp.provider(nil))
	other := newRSAKey(t)
	n := 0
	try := func(mod func(f *fakeIdP)) string {
		t.Helper()
		n++
		idp.set(func(f *fakeIdP) {
			f.claims, f.mod, f.signKey = person("sub-"+string(rune('a'+n)), "", false), nil, nil
			f.claims["email"] = "u" + string(rune('a'+n)) + "@example.com"
			f.claims["email_verified"] = true
			if mod != nil {
				mod(f)
			}
		})
		e.h.logins.reset("127.0.0.1")
		final, _ := noFollow(e.browser()).ssoFlow("corp", "")
		return final
	}
	if got := try(nil); got != "/" {
		t.Fatalf("baseline: %q", got)
	}
	cases := map[string]func(f *fakeIdP){
		"bad signature":  func(f *fakeIdP) { f.signKey = other },
		"wrong audience": func(f *fakeIdP) { f.mod = func(c map[string]any) { c["aud"] = "someone-else" } },
		"wrong azp": func(f *fakeIdP) {
			f.mod = func(c map[string]any) { c["aud"] = []string{f.clientID, "x"}; c["azp"] = "x" }
		},
		"expired":        func(f *fakeIdP) { f.mod = func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() } },
		"no exp":         func(f *fakeIdP) { f.mod = func(c map[string]any) { delete(c, "exp") } },
		"wrong issuer":   func(f *fakeIdP) { f.mod = func(c map[string]any) { c["iss"] = "https://evil.example" } },
		"nonce mismatch": func(f *fakeIdP) { f.mod = func(c map[string]any) { c["nonce"] = "not-the-nonce" } },
		"no nonce":       func(f *fakeIdP) { f.mod = func(c map[string]any) { delete(c, "nonce") } },
	}
	for name, mod := range cases {
		if got := try(mod); got != "/login?sso_error=invalid_token" {
			t.Errorf("%s: %q", name, got)
		}
	}

	// Key rotation: a new kid makes the relay refetch the JWKS once the
	// minimum refresh interval passed.
	idp.set(func(f *fakeIdP) { f.key, f.kid = newRSAKey(t), "k2" })
	clockMu.Lock()
	clock = clock.Add(time.Minute)
	clockMu.Unlock()
	if got := try(nil); got != "/" {
		t.Fatalf("after rotation: %q", got)
	}

	// No state cookie (login CSRF), and a state that was never issued.
	b := noFollow(e.browser())
	resp := b.get(e.srv.URL + "/api/v1/auth/sso/corp/callback?code=x&state=y")
	if loc := resp.Header.Get("Location"); loc != "/login?sso_error=state_mismatch" {
		t.Fatalf("no cookie: %q", loc)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1/auth/sso/corp/callback?code=x&state=never-issued", nil)
	req.AddCookie(&http.Cookie{Name: ssoStateCookie, Value: "never-issued"})
	r2, _ := b.http.Do(req)
	r2.Body.Close()
	if loc := r2.Header.Get("Location"); loc != "/login?sso_error=state_expired" {
		t.Fatalf("unissued state: %q", loc)
	}

	// Failures feed the login rate limiter.
	e.h.logins.reset("127.0.0.1")
	for range loginMaxFails {
		b.get(e.srv.URL + "/api/v1/auth/sso/corp/callback?code=x&state=y")
	}
	if loc := b.get(e.srv.URL + "/api/v1/auth/sso/corp/start").Header.Get("Location"); loc != "/login?sso_error=rate_limited" {
		t.Fatalf("rate limit: %q", loc)
	}
}

func testSSOPolicy(t *testing.T, open opener) {
	e, idp, admin := ssoEnv(t, open)
	ctx := context.Background()
	carol := &store.User{Name: "Carol", Email: "carol@example.com", Role: store.RoleMember}
	if err := e.st.CreateUser(ctx, carol, "carol-password-1"); err != nil {
		t.Fatal(err)
	}
	admin.must(201, "POST", "/api/v1/sso/providers", idp.provider(map[string]any{"allowed_domains": []string{"example.com"}, "auto_create": false}))
	flow := func(claims map[string]any) string {
		t.Helper()
		idp.set(func(f *fakeIdP) { f.claims = claims })
		e.h.logins.reset("127.0.0.1")
		final, _ := noFollow(e.browser()).ssoFlow("corp", "")
		return final
	}
	if got := flow(person("m1", "mallory@evil.example", true)); got != "/login?sso_error=domain_not_allowed" {
		t.Fatalf("domain: %q", got)
	}
	if got := flow(person("m2", "dave@example.com", false)); got != "/login?sso_error=email_unverified" {
		t.Fatalf("unverified: %q", got)
	}
	if got := flow(person("d1", "dave@example.com", true)); got != "/login?sso_error=no_account" {
		t.Fatalf("auto-create off: %q", got)
	}
	// An existing account is not taken over unless linking is on.
	if got := flow(person("c1", "Carol@example.com", true)); got != "/login?sso_error=account_exists" {
		t.Fatalf("link off: %q", got)
	}
	admin.must(200, "PATCH", "/api/v1/sso/providers/corp", map[string]any{"link_by_email": true})
	idp.set(func(f *fakeIdP) { f.claims = person("c1", "Carol@example.com", true) })
	b := noFollow(e.browser())
	if final, _ := b.ssoFlow("corp", ""); final != "/" {
		t.Fatalf("link: %q", final)
	}
	if id := b.must(200, "GET", "/api/v1/me", nil)["user"].(map[string]any)["id"]; id != carol.ID {
		t.Fatalf("linked to %v, want %s", id, carol.ID)
	}
	// A disabled user stays out.
	carol.Disabled = true
	if err := e.st.UpdateUser(ctx, carol); err != nil {
		t.Fatal(err)
	}
	if got := flow(person("c1", "carol@example.com", true)); got != "/login?sso_error=account_disabled" {
		t.Fatalf("disabled: %q", got)
	}
	// Disabled providers cannot start.
	admin.must(200, "PATCH", "/api/v1/sso/providers/corp", map[string]any{"enabled": false})
	if got := flow(person("c1", "carol@example.com", true)); got != "/login?sso_error=unknown_provider" {
		t.Fatalf("disabled provider: %q", got)
	}

	// Validation.
	_, out := admin.do("POST", "/api/v1/sso/providers", map[string]any{"id": "Bad ID", "preset": "nope", "issuer": "http://idp.example"})
	paths := map[string]bool{}
	for _, f := range out["error"].(map[string]any)["fields"].([]any) {
		paths[f.(map[string]any)["path"].(string)] = true
	}
	for _, p := range []string{"id", "preset", "client_id", "client_secret"} {
		if !paths[p] {
			t.Errorf("no field error for %s: %v", p, paths)
		}
	}
	_, out = admin.do("POST", "/api/v1/sso/providers", idp.provider(map[string]any{"id": "x", "issuer": "http://idp.example", "default_role": "admin"}))
	paths = map[string]bool{}
	for _, f := range out["error"].(map[string]any)["fields"].([]any) {
		paths[f.(map[string]any)["path"].(string)] = true
	}
	if !paths["issuer"] || !paths["default_role"] {
		t.Errorf("want issuer and default_role errors: %v", paths)
	}
	admin.must(409, "POST", "/api/v1/sso/providers", idp.provider(nil))

	// A relay reachable from other machines refuses SSO without a
	// configured public URL.
	e.setSite(SettingPublicURL, nil)
	e.listenPublic()
	admin.must(200, "PATCH", "/api/v1/sso/providers/corp", map[string]any{"enabled": true})
	if got := flow(person("c1", "carol@example.com", true)); got != "/login?sso_error=public_url_missing" {
		t.Fatalf("no public url: %q", got)
	}
	if sso := admin.must(200, "GET", "/api/v1/sso/providers", nil); sso["redirect_problem"] == nil {
		t.Fatalf("GET /sso should explain the missing public URL: %v", sso)
	}

	// Deleting a provider drops its identities.
	admin.must(204, "DELETE", "/api/v1/sso/providers/corp", nil)
	if ids, _ := e.st.ListIdentities(ctx, carol.ID); len(ids) != 0 {
		t.Fatalf("identities kept: %v", ids)
	}
}

func testSSOOAuthPresets(t *testing.T, open opener) {
	e, idp, admin := ssoEnv(t, open)
	admin.must(201, "POST", "/api/v1/sso/providers", map[string]any{"id": "gh", "preset": "github",
		"client_id": idp.clientID, "client_secret": idp.secret, "auto_create": true})
	flow := func(p string) (*client, string) {
		t.Helper()
		e.h.logins.reset("127.0.0.1")
		b := noFollow(e.browser())
		final, _ := b.ssoFlow(p, "")
		return b, final
	}
	idp.set(func(f *fakeIdP) {
		f.ghUser = map[string]any{"id": 4242, "login": "octo", "name": ""}
		f.ghEmails = []map[string]any{{"email": "octo@users.example", "primary": true, "verified": false},
			{"email": "octo@verified.example", "primary": false, "verified": true}}
	})
	if _, got := flow("gh"); got != "/login?sso_error=email_unverified" {
		t.Fatalf("github unverified primary: %q", got)
	}
	idp.set(func(f *fakeIdP) { f.ghEmails[0]["verified"] = true })
	b, got := flow("gh")
	if got != "/" {
		t.Fatalf("github: %q", got)
	}
	u := b.must(200, "GET", "/api/v1/me", nil)["user"].(map[string]any)
	if u["username"] != "octo@users.example" || u["display_name"] != "octo" {
		t.Fatalf("github user: %v", u)
	}
	if _, err := e.st.GetIdentity(context.Background(), "gh", "4242"); err != nil {
		t.Fatalf("github identity: %v", err)
	}

	admin.must(201, "POST", "/api/v1/sso/providers", map[string]any{"id": "fs", "preset": "feishu",
		"client_id": idp.clientID, "client_secret": idp.secret, "auto_create": true, "allowed_tenants": []string{"tenant-a"}})
	idp.set(func(f *fakeIdP) {
		f.feishu = map[string]any{"open_id": "ou_1", "union_id": "on_1", "name": "张三", "tenant_key": "tenant-b", "email": "zs@corp.example"}
	})
	if _, got := flow("fs"); got != "/login?sso_error=tenant_not_allowed" {
		t.Fatalf("feishu tenant: %q", got)
	}
	idp.set(func(f *fakeIdP) { f.feishu["tenant_key"] = "tenant-a" })
	b, got = flow("fs")
	if got != "/" {
		t.Fatalf("feishu: %q", got)
	}
	u = b.must(200, "GET", "/api/v1/me", nil)["user"].(map[string]any)
	// The contact email is not verified, so the account gets none.
	if u["display_name"] != "张三" || u["username"] != "" {
		t.Fatalf("feishu user: %v", u)
	}
}

func testSSOPasswordLogin(t *testing.T, open opener) {
	e, idp, admin := ssoEnv(t, open)
	op := e.browser()
	op.csrf, op.auth = false, adminToken

	admin.must(409, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"security.password_login": false}}) // no provider
	admin.must(201, "POST", "/api/v1/sso/providers", idp.provider(map[string]any{"link_by_email": true}))
	// The admin has not signed in through SSO yet.
	admin.must(409, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"security.password_login": false}})

	// Link the admin (email "admin" is not an address; link by hand).
	users := admin.must(200, "GET", "/api/v1/users", nil)["items"].([]any)
	adminID := users[0].(map[string]any)["id"].(string)
	if err := e.st.LinkIdentity(context.Background(), &store.Identity{Provider: "corp", Subject: "root", UserID: adminID}); err != nil {
		t.Fatal(err)
	}
	admin.must(200, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"security.password_login": false}})
	anon := e.browser()
	if anon.must(200, "GET", "/api/v1/auth/providers", nil)["password_login"] != false {
		t.Fatal("login page still offers passwords")
	}
	code, res := anon.do("POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "correct-horse-1"})
	if code != 403 || errCode(res) != "password_login_disabled" {
		t.Fatalf("password login: %d %v", code, res)
	}
	// SSO still signs the admin in.
	idp.set(func(f *fakeIdP) { f.claims = map[string]any{"sub": "root", "name": "Root"} })
	b := noFollow(e.browser())
	if final, _ := b.ssoFlow("corp", "/settings"); final != "/settings" {
		t.Fatalf("admin sso: %q", final)
	}
	if b.must(200, "GET", "/api/v1/me", nil)["user"].(map[string]any)["role"] != "admin" {
		t.Fatal("admin role lost")
	}
	// Never lock out: the last provider cannot go while passwords are off.
	admin.must(409, "PATCH", "/api/v1/sso/providers/corp", map[string]any{"enabled": false})
	admin.must(409, "DELETE", "/api/v1/sso/providers/corp", nil)
	// Break-glass: the operator token works and can turn passwords back on;
	// so can the CLI helper.
	op.must(200, "GET", "/api/v1/sso/providers", nil)
	op.must(200, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"security.password_login": true}})
	anon.must(200, "POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "correct-horse-1"})
	op.must(200, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"security.password_login": false}})
	if changed, err := EnablePasswordLogin(context.Background(), e.st); err != nil || !changed {
		t.Fatalf("EnablePasswordLogin: %v %v", changed, err)
	}
	e.browser().must(200, "POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "correct-horse-1"})
	// Single sign-on off: no provider is offered and passwords work again,
	// whatever the setting says.
	op.must(200, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"security.password_login": false}})
	op.must(200, "PATCH", "/api/v1/features/sso", map[string]any{"enabled": false})
	if out := anon.must(200, "GET", "/api/v1/auth/providers", nil); out["password_login"] != true || len(out["providers"].([]any)) != 0 {
		t.Fatalf("sso off: %v", out)
	}
	if loc := noFollow(e.browser()).get(e.srv.URL + "/api/v1/auth/sso/corp/start").Header.Get("Location"); !strings.Contains(loc, "unknown_provider") {
		t.Fatalf("sso off, start: %q", loc)
	}
	e.browser().must(200, "POST", "/api/v1/auth/login", map[string]any{"username": "admin", "password": "correct-horse-1"})
}

func TestSafeReturnTo(t *testing.T) {
	for in, want := range map[string]string{
		"": "/", "/": "/", "/keys": "/keys", "/logs?id=1#x": "/logs?id=1#x",
		"//evil.example": "/", "/\\evil.example": "/", "https://evil.example": "/", "evil": "/",
		"/api/v1/me": "/", "/a\nb": "/", "/%0d%0aSet-Cookie": "/%0d%0aSet-Cookie",
	} {
		if got := safeReturnTo(in); got != want {
			t.Errorf("safeReturnTo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEntraIssuerTemplate(t *testing.T) {
	p := &ssoProvider{Preset: presetMicrosoft, Tenant: "organizations", ClientID: "app"}
	doc := &oidcDiscovery{Issuer: "https://login.microsoftonline.com/{tenantid}/v2.0"}
	now := time.Now()
	claims := map[string]any{"iss": "https://login.microsoftonline.com/t-1/v2.0", "tid": "t-1", "aud": "app",
		"exp": json.Number("9999999999"), "nonce": "n", "sub": "s"}
	if err := checkIDClaims(p, doc, claims, now, "n"); err != nil {
		t.Fatal(err)
	}
	claims["tid"] = "t-2"
	if err := checkIDClaims(p, doc, claims, now, "n"); err == nil {
		t.Fatal("issuer of another tenant accepted")
	}
}

func TestVerifySignatureRefusesSymmetric(t *testing.T) {
	_, claims, _, _, err := jwtParts(signJWT(t, newRSAKey(t), "k", map[string]any{"sub": "x"}))
	if err != nil || claims["sub"] != "x" {
		t.Fatal(err)
	}
	h := &Handler{d: Deps{Now: time.Now}}
	for _, alg := range []string{"none", "HS256"} {
		hb, _ := json.Marshal(map[string]any{"alg": alg})
		tok := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`)) + "."
		if _, err := h.verifyIDToken(context.Background(), &ssoProvider{}, &oidcDiscovery{}, tok, "n"); err == nil ||
			!strings.Contains(err.Error(), "unsupported algorithm") {
			t.Fatalf("%s: %v", alg, err)
		}
	}
}
