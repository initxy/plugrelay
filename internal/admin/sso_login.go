package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
)

const (
	// ssoStateCookie binds a pending sign-in to the browser that started it.
	ssoStateCookie = "pr_sso"
	ssoStateTTL    = 10 * time.Minute
)

// ssoPending is what the start step remembers for the callback.
type ssoPending struct {
	Provider    string `json:"provider"`
	Nonce       string `json:"nonce"`
	Verifier    string `json:"verifier"`
	RedirectURI string `json:"redirect_uri"`
	ReturnTo    string `json:"return_to"`
}

// ssoIdentity is what a provider asserted about the person signing in.
type ssoIdentity struct {
	Subject       string
	Email         string
	EmailVerified bool
	// EmailVerifiedKnown is set when the IdP said whether the address is
	// verified; trust_email only fills in when it did not.
	EmailVerifiedKnown bool
	Name               string
	Tenant             string
	Claims             map[string]any
}

// ssoFailure is a sign-in refusal shown on the login page by code.
type ssoFailure struct {
	code string
	err  error
}

func (f *ssoFailure) Error() string {
	if f.err != nil {
		return f.code + ": " + f.err.Error()
	}
	return f.code
}

func fail(code string, err error) *ssoFailure { return &ssoFailure{code: code, err: err} }

// trustedBaseURL is the relay's external URL where a Host header must not
// decide it: where an IdP delivers codes, and the passkey relying party.
// A relay reachable from other machines must have the public URL set; a
// Host header is attacker-chosen.
func (h *Handler) trustedBaseURL(c *call) (string, error) {
	if s, err := h.site(c.ctx); err == nil {
		if v := s.String(SettingPublicURL); v != "" {
			return strings.TrimRight(v, "/"), nil
		}
	}
	if h.hostTrusted(c) {
		return h.baseURL(c), nil
	}
	return "", errConflict(ePublicURLMissing)
}

func ssoRedirectURI(base, id string) string {
	return base + Prefix + "/auth/sso/" + url.PathEscape(id) + "/callback"
}

// safeReturnTo keeps a post-login destination on this site: an absolute
// path, never a scheme, host, protocol-relative or backslash form.
func safeReturnTo(s string) string {
	if s == "" || len(s) > 512 || !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") ||
		strings.ContainsAny(s, "\\\r\n\t") || strings.HasPrefix(s, Prefix+"/") {
		return "/"
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "/"
		}
	}
	return s
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ---------------------------------------------------------------- public endpoints

func (h *Handler) listAuthProviders(c *call) error {
	cfg, err := loadSSO(c.ctx, h.d.Store)
	if err != nil {
		return err
	}
	items := []map[string]any{}
	for _, p := range cfg.enabled() {
		items = append(items, map[string]any{"id": p.ID, "name": p.Name, "preset": p.Preset})
	}
	off, err := h.passwordLoginDisabled(c.ctx)
	if err != nil {
		return err
	}
	pw := !off
	out := map[string]any{"password_login": pw, "providers": items}
	s, err := h.site(c.ctx)
	if err != nil {
		return err
	}
	if pc := h.publicCaptcha(s); pc != nil && pw {
		out["captcha"] = pc
	}
	// The login page shows whose relay this is before anyone signs in.
	site := map[string]any{"name": s.name()}
	if v := s.String(settingLogo); v != "" {
		site["logo_url"] = v
	}
	out["site"] = site
	if models, status, err := h.publicPages(c); err != nil {
		return err
	} else if models {
		out["public_pages"] = map[string]bool{"models": models, "status": status}
	}
	return c.json(200, out)
}

// redirectLogin sends the browser to the login page with an error code.
func (h *Handler) redirectLogin(c *call, code string) error {
	http.Redirect(c.w, c.r, "/login?sso_error="+url.QueryEscape(code), http.StatusFound)
	return nil
}

func (h *Handler) startSSO(c *call) error {
	if needs, err := h.NeedsSetup(c.ctx); err != nil {
		return err
	} else if needs {
		return h.redirectLogin(c, "setup_required")
	}
	ip := h.d.ClientIP(c.r)
	now := h.d.Now()
	if h.logins.blocked(ip, now) > 0 || h.ssoStarts.blocked(ip, now) > 0 {
		return h.redirectLogin(c, "rate_limited")
	}
	h.ssoStarts.failed(ip, now)
	cfg, err := loadSSO(c.ctx, h.d.Store)
	if err != nil {
		return err
	}
	_, p := cfg.find(c.r.PathValue("id"))
	if p == nil || !p.Enabled || !cfg.on {
		return h.redirectLogin(c, "unknown_provider")
	}
	base, err := h.trustedBaseURL(c)
	if err != nil {
		h.d.Logger.Warn("sso start refused", "provider", p.ID, "err", err)
		return h.redirectLogin(c, "public_url_missing")
	}
	state, err1 := secret.GenerateToken()
	nonce, err2 := secret.GenerateToken()
	verifier, err3 := secret.GenerateToken()
	if err := errors.Join(err1, err2, err3); err != nil {
		return err
	}
	pend := ssoPending{Provider: p.ID, Nonce: nonce, Verifier: verifier, RedirectURI: ssoRedirectURI(base, p.ID),
		ReturnTo: safeReturnTo(c.query("return_to"))}
	authURL, err := h.authorizeURL(c.ctx, p, pend, state)
	if err != nil {
		h.d.Logger.Warn("sso start failed", "provider", p.ID, "err", err)
		return h.redirectLogin(c, "provider_unavailable")
	}
	b, _ := json.Marshal(pend)
	if err := h.d.Store.KV(ssoKV).Put(c.ctx, "state:"+secret.HashKey(state), b, ssoStateTTL); err != nil {
		return err
	}
	http.SetCookie(c.w, &http.Cookie{Name: ssoStateCookie, Value: state, Path: Prefix + "/auth/sso/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secureRequest(c.r), MaxAge: int(ssoStateTTL / time.Second)})
	http.Redirect(c.w, c.r, authURL, http.StatusFound)
	return nil
}

func (h *Handler) authorizeURL(ctx context.Context, p *ssoProvider, pend ssoPending, state string) (string, error) {
	scopes := p.Scopes
	if scopes == nil {
		scopes = presetScopes(p.Preset)
	}
	q := url.Values{}
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", pend.RedirectURI)
	q.Set("state", state)
	q.Set("code_challenge", pkceChallenge(pend.Verifier))
	q.Set("code_challenge_method", "S256")
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	var endpoint string
	switch presetFlow(p.Preset) {
	case flowOIDC:
		doc, err := h.sso.discover(ctx, h.idp(), p, h.d.Now())
		if err != nil {
			return "", err
		}
		endpoint = doc.AuthorizationEndpoint
		q.Set("response_type", "code")
		q.Set("nonce", pend.Nonce)
	case flowGitHub:
		endpoint = presetURLs[presetGitHub].auth
		q.Set("allow_signup", "false")
	case flowFeishu:
		endpoint = presetURLs[p.Preset].auth
		q.Set("response_type", "code")
	}
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	return endpoint + sep + q.Encode(), nil
}

func (h *Handler) ssoCallback(c *call) error {
	ip := h.d.ClientIP(c.r)
	now := h.d.Now()
	providerID := c.r.PathValue("id")
	http.SetCookie(c.w, &http.Cookie{Name: ssoStateCookie, Value: "", Path: Prefix + "/auth/sso/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secureRequest(c.r)})
	if h.logins.blocked(ip, now) > 0 {
		return h.redirectLogin(c, "rate_limited")
	}
	u, pend, f := h.finishSSO(c, providerID)
	if f != nil {
		if f.code != "denied" {
			h.logins.failed(ip, now)
		}
		h.d.Logger.Warn("sso sign-in refused", "provider", providerID, "ip", ip, "reason", f.Error())
		return h.redirectLogin(c, f.code)
	}
	if need, err := h.needsSecondFactor(c.ctx, u, true); err != nil {
		return err
	} else if need {
		if err := h.beginSecondFactor(c, u, "sso:"+providerID, pend.ReturnTo); err != nil {
			return err
		}
		http.Redirect(c.w, c.r, "/login?mfa=1", http.StatusFound)
		return nil
	}
	h.logins.reset(ip)
	if err := h.startSession(c, u); err != nil {
		return err
	}
	c.who = &identity{user: u, auth: authSession, role: u.Role}
	c.audit("auth.login", "user", u.ID, displayName(u), []change{{Field: "method", To: "sso:" + providerID}})
	http.Redirect(c.w, c.r, pend.ReturnTo, http.StatusFound)
	return nil
}

// finishSSO checks the callback against the pending state, redeems the
// code and maps the asserted identity to a user.
func (h *Handler) finishSSO(c *call, providerID string) (*store.User, *ssoPending, *ssoFailure) {
	if needs, err := h.NeedsSetup(c.ctx); err != nil || needs {
		return nil, nil, fail("setup_required", err)
	}
	q := c.r.URL.Query()
	state := q.Get("state")
	ck, err := c.r.Cookie(ssoStateCookie)
	if state == "" || err != nil || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(state)) != 1 {
		return nil, nil, fail("state_mismatch", nil)
	}
	kv := h.d.Store.KV(ssoKV)
	key := secret.HashKey(state)
	// Incr is atomic across nodes: only the first callback for a state
	// gets 1.
	n, err := kv.Incr(c.ctx, "used:"+key, 1, ssoStateTTL)
	if err != nil {
		return nil, nil, fail("internal", err)
	}
	if n != 1 {
		return nil, nil, fail("state_replayed", nil)
	}
	raw, ok, err := kv.Get(c.ctx, "state:"+key)
	if err != nil {
		return nil, nil, fail("internal", err)
	}
	_ = kv.Delete(c.ctx, "state:"+key)
	var pend ssoPending
	if !ok || json.Unmarshal(raw, &pend) != nil {
		return nil, nil, fail("state_expired", nil)
	}
	if pend.Provider != providerID {
		return nil, nil, fail("state_mismatch", nil)
	}
	if e := q.Get("error"); e != "" {
		return nil, nil, fail("denied", errors.New(e))
	}
	code := q.Get("code")
	if code == "" {
		return nil, nil, fail("denied", errors.New("no code"))
	}
	cfg, err := loadSSO(c.ctx, h.d.Store)
	if err != nil {
		return nil, nil, fail("internal", err)
	}
	_, p := cfg.find(providerID)
	if p == nil || !p.Enabled || !cfg.on {
		return nil, nil, fail("unknown_provider", nil)
	}
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	var who *ssoIdentity
	switch presetFlow(p.Preset) {
	case flowOIDC:
		who, err = h.redeemOIDC(ctx, p, &pend, code)
	case flowGitHub:
		who, err = h.redeemGitHub(ctx, p, &pend, code)
	case flowFeishu:
		who, err = h.redeemFeishu(ctx, p, &pend, code)
	}
	if err != nil {
		var f *ssoFailure
		if errors.As(err, &f) {
			return nil, nil, f
		}
		return nil, nil, fail("provider_error", err)
	}
	u, f := h.ssoUser(c, p, who)
	if f != nil {
		return nil, nil, f
	}
	return u, &pend, nil
}

// ---------------------------------------------------------------- code redemption

func (c idpClient) postForm(ctx context.Context, endpoint string, form url.Values, basicUser, basicPass string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basicUser != "" {
		req.SetBasicAuth(url.QueryEscape(basicUser), url.QueryEscape(basicPass))
	}
	return c.doJSON(req, v)
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

func (h *Handler) redeemOIDC(ctx context.Context, p *ssoProvider, pend *ssoPending, code string) (*ssoIdentity, error) {
	doc, err := h.sso.discover(ctx, h.idp(), p, h.d.Now())
	if err != nil {
		return nil, err
	}
	sec, err := h.clientSecret(p)
	if err != nil {
		return nil, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {pend.RedirectURI},
		"code_verifier": {pend.Verifier}}
	user, pass := "", ""
	if slices.Contains(doc.TokenAuthMethods, "client_secret_post") {
		form.Set("client_id", p.ClientID)
		form.Set("client_secret", sec)
	} else {
		user, pass = p.ClientID, sec
	}
	var tok tokenResponse
	if err := h.idp().postForm(ctx, doc.TokenEndpoint, form, user, pass, &tok); err != nil {
		return nil, err
	}
	if tok.IDToken == "" {
		return nil, fmt.Errorf("token response has no id_token %s", tok.Error)
	}
	claims, err := h.verifyIDToken(ctx, p, doc, tok.IDToken, pend.Nonce)
	if err != nil {
		return nil, fail("invalid_token", err)
	}
	// Some IdPs leave email out of the ID token; userinfo fills it for the
	// same subject only.
	if _, has := claims["email"]; !has && doc.UserinfoEndpoint != "" && tok.AccessToken != "" {
		var info map[string]any
		if err := h.idp().getJSON(ctx, doc.UserinfoEndpoint, tok.AccessToken, &info); err == nil && info["sub"] == claims["sub"] {
			for _, k := range []string{"email", "email_verified", "name", "preferred_username", "groups"} {
				if _, ok := claims[k]; !ok && info[k] != nil {
					claims[k] = info[k]
				}
			}
		}
	}
	who := &ssoIdentity{Claims: claims}
	who.Subject, _ = claims["sub"].(string)
	who.Email, _ = claims["email"].(string)
	who.EmailVerified = truthy(claims["email_verified"])
	_, who.EmailVerifiedKnown = claims["email_verified"]
	who.Name, _ = claims["name"].(string)
	if p.Preset == presetMicrosoft {
		who.Tenant, _ = claims["tid"].(string)
		if who.Email == "" {
			if pu, _ := claims["preferred_username"].(string); strings.Contains(pu, "@") {
				who.Email = pu
			}
		}
		// In a single-tenant app the tenant's admins own every address;
		// multi-tenant apps rely on xms_edov (domain owner verified).
		who.EmailVerified = !slices.Contains(entraMultiTenant, strings.ToLower(p.Tenant)) || truthy(claims["xms_edov"])
		who.EmailVerifiedKnown = true
	}
	return who, nil
}

func truthy(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true")
	}
	return false
}

func (h *Handler) redeemGitHub(ctx context.Context, p *ssoProvider, pend *ssoPending, code string) (*ssoIdentity, error) {
	urls := presetURLs[presetGitHub]
	sec, err := h.clientSecret(p)
	if err != nil {
		return nil, err
	}
	var tok tokenResponse
	form := url.Values{"client_id": {p.ClientID}, "client_secret": {sec}, "code": {code},
		"redirect_uri": {pend.RedirectURI}, "code_verifier": {pend.Verifier}}
	if err := h.idp().postForm(ctx, urls.token, form, "", "", &tok); err != nil {
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("token: %s %s", tok.Error, tok.Description)
	}
	var user map[string]any
	if err := h.idp().getJSON(ctx, urls.userinfo, tok.AccessToken, &user); err != nil {
		return nil, err
	}
	id, ok := numClaim(user["id"])
	if !ok || id <= 0 {
		return nil, errors.New("github user has no id")
	}
	who := &ssoIdentity{Subject: strconv.FormatInt(id, 10), Claims: user}
	who.Name, _ = user["name"].(string)
	if who.Name == "" {
		who.Name, _ = user["login"].(string)
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := h.idp().getJSON(ctx, urls.emails, tok.AccessToken, &emails); err != nil {
		return nil, err
	}
	for _, e := range emails {
		if e.Primary {
			who.Email, who.EmailVerified, who.EmailVerifiedKnown = e.Email, e.Verified, true
		}
	}
	return who, nil
}

func (h *Handler) redeemFeishu(ctx context.Context, p *ssoProvider, pend *ssoPending, code string) (*ssoIdentity, error) {
	urls := presetURLs[p.Preset]
	sec, err := h.clientSecret(p)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"grant_type": "authorization_code", "client_id": p.ClientID,
		"client_secret": sec, "code": code, "redirect_uri": pend.RedirectURI, "code_verifier": pend.Verifier})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urls.token, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	var tok struct {
		Code        any    `json:"code"` // a number on Feishu, a string on Lark
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := h.idp().doJSON(req, &tok); err != nil {
		return nil, err
	}
	if fmt.Sprint(tok.Code) != "0" || tok.AccessToken == "" {
		return nil, fmt.Errorf("token: code %v %s %s", tok.Code, tok.Error, tok.Description)
	}
	var info struct {
		Code any            `json:"code"`
		Msg  string         `json:"msg"`
		Data map[string]any `json:"data"`
	}
	if err := h.idp().getJSON(ctx, urls.userinfo, tok.AccessToken, &info); err != nil {
		return nil, err
	}
	if fmt.Sprint(info.Code) != "0" || info.Data == nil {
		return nil, fmt.Errorf("user_info: code %v %s", info.Code, info.Msg)
	}
	d := info.Data
	who := &ssoIdentity{Claims: d}
	who.Subject, _ = d["open_id"].(string)
	if who.Subject == "" {
		return nil, errors.New("user_info has no open_id")
	}
	who.Name, _ = d["name"].(string)
	who.Tenant, _ = d["tenant_key"].(string)
	// Contact emails are typed in by tenant admins and never confirmed by
	// the user (vendor docs), so they only count as verified with
	// trust_email.
	who.Email, _ = d["enterprise_email"].(string)
	if who.Email == "" {
		who.Email, _ = d["email"].(string)
	}
	return who, nil
}

// ---------------------------------------------------------------- user mapping

func emailDomain(email string) string {
	_, d, ok := strings.Cut(email, "@")
	if !ok {
		return ""
	}
	return strings.ToLower(d)
}

func claimMatches(v any, want string) bool {
	switch x := v.(type) {
	case string:
		return x == want
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok && s == want {
				return true
			}
		}
	case bool, json.Number, float64:
		return fmt.Sprint(x) == want
	}
	return false
}

// ssoUser applies the provider's policy and returns the user to sign in:
// the linked one, an existing one with the same verified email (when
// linking is on), or a new one (when auto-create is on).
func (h *Handler) ssoUser(c *call, p *ssoProvider, who *ssoIdentity) (*store.User, *ssoFailure) {
	ctx := c.ctx
	email := strings.ToLower(strings.TrimSpace(who.Email))
	verified := email != "" && strings.Contains(email, "@") && (who.EmailVerified || (p.TrustEmail && !who.EmailVerifiedKnown))
	if len(p.AllowedTenants) > 0 && !slices.Contains(p.AllowedTenants, strings.ToLower(who.Tenant)) {
		return nil, fail("tenant_not_allowed", fmt.Errorf("tenant %q", who.Tenant))
	}
	if len(p.AllowedDomains) > 0 {
		if !verified {
			return nil, fail("email_unverified", nil)
		}
		if !slices.Contains(p.AllowedDomains, emailDomain(email)) {
			return nil, fail("domain_not_allowed", fmt.Errorf("domain %q", emailDomain(email)))
		}
	}
	st := h.d.Store
	if id, err := st.GetIdentity(ctx, p.ID, who.Subject); err == nil {
		u, err := st.GetUser(ctx, id.UserID)
		switch {
		case err == nil:
			if u.Disabled {
				return nil, fail("account_disabled", nil)
			}
			_ = st.TouchIdentity(ctx, id.ID, h.d.Now())
			return u, nil
		case errors.Is(err, store.ErrNotFound):
			_ = st.DeleteIdentity(ctx, id.ID)
		default:
			return nil, fail("internal", err)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, fail("internal", err)
	}
	link := func(u *store.User) error {
		id := &store.Identity{Provider: p.ID, Subject: who.Subject, UserID: u.ID, LastLoginAt: h.d.Now()}
		if verified {
			id.Email = email
		}
		if err := st.LinkIdentity(ctx, id); err != nil {
			return err
		}
		c.who = &identity{user: u, auth: authSession, role: u.Role}
		c.audit("user.identity.link", "user", u.ID, displayName(u), []change{{Field: "identity", To: p.ID + ":" + who.Subject}})
		return nil
	}
	if verified {
		u, err := st.UserByEmail(ctx, email)
		switch {
		case err == nil:
			if !p.LinkByEmail {
				return nil, fail("account_exists", nil)
			}
			if u.Disabled {
				return nil, fail("account_disabled", nil)
			}
			// Anyone could have signed up with this address before its owner
			// arrived through the IdP.
			if u.EmailUnverified {
				return nil, fail("account_exists", errors.New("link_by_email skips accounts whose email was never verified (self-service sign-up, import)"))
			}
			// An admin account is the relay's keys; an IdP that can assert
			// its address must be trusted that far explicitly.
			if u.Role == store.RoleAdmin && !p.LinkAdmins {
				return nil, fail("account_exists", errors.New("link_by_email skips admin accounts unless link_admins is on"))
			}
			if err := link(u); err != nil {
				return nil, fail("internal", err)
			}
			return u, nil
		case !errors.Is(err, store.ErrNotFound):
			return nil, fail("internal", err)
		}
	}
	if !p.AutoCreate {
		return nil, fail("no_account", nil)
	}
	// Without a verified address only a tenant allowlist says who this is.
	if !verified && len(p.AllowedTenants) == 0 {
		return nil, fail("email_unverified", nil)
	}
	u := &store.User{Name: who.Name, Role: p.DefaultRole, GroupID: ssoGroup(p, who, email, verified)}
	if verified {
		u.Email = email
	}
	if u.Name == "" {
		u.Name = orDefault(email, p.Name+" user")
	}
	if err := st.CreateUser(ctx, u, ""); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return h.ssoRaceWinner(ctx, p.ID, who.Subject, err)
		}
		return nil, fail("internal", err)
	}
	if err := link(u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// A concurrent sign-in for the same subject linked first.
			_ = st.DeleteUser(ctx, u.ID)
			return h.ssoRaceWinner(ctx, p.ID, who.Subject, err)
		}
		return nil, fail("internal", err)
	}
	c.who = &identity{user: u, auth: authSession, role: u.Role}
	c.audit("user.create", "user", u.ID, displayName(u), []change{{Field: "source", To: "sso:" + p.ID},
		{Field: "role", To: u.Role}, {Field: "group_id", To: u.GroupID}})
	return u, nil
}

// ssoRaceWinner resolves an auto-create that lost a uniqueness race to a
// concurrent sign-in of the same subject: the winner creates its user
// before linking it, so its link is waited for briefly.
func (h *Handler) ssoRaceWinner(ctx context.Context, provider, subject string, cause error) (*store.User, *ssoFailure) {
	for range 20 {
		if id, err := h.d.Store.GetIdentity(ctx, provider, subject); err == nil {
			u, err := h.d.Store.GetUser(ctx, id.UserID)
			if err != nil {
				return nil, fail("internal", err)
			}
			if u.Disabled {
				return nil, fail("account_disabled", nil)
			}
			return u, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, fail("internal", err)
		}
		select {
		case <-ctx.Done():
			return nil, fail("internal", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	// No link appeared: the conflict was another account's address.
	return nil, fail("account_exists", cause)
}

func ssoGroup(p *ssoProvider, who *ssoIdentity, email string, verified bool) string {
	for _, r := range p.GroupRules {
		if r.Domain != "" {
			if verified && emailDomain(email) == r.Domain {
				return r.GroupID
			}
			continue
		}
		if claimRuleAllowed(p.Preset, r.Claim) && claimMatches(who.Claims[r.Claim], r.Value) {
			return r.GroupID
		}
	}
	return p.DefaultGroupID
}
