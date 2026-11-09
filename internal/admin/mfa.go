package admin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Two-step sign-in: TOTP, recovery codes and passkeys. Pending logins,
// TOTP enrollments, WebAuthn challenges and re-authentication windows
// live in the plugin KV namespace core.mfa; factors live in store.MFA.

const (
	mfaKV = "core.mfa"
	// mfaCookie binds a login waiting for its second factor to the browser.
	mfaCookie     = "pr_mfa"
	mfaPendingTTL = 5 * time.Minute
	mfaMaxTries   = 5
	// A fresh sign-in or re-authentication allows sensitive changes for
	// this long.
	reauthTTL    = 5 * time.Minute
	challengeTTL = 5 * time.Minute
	totpSetupTTL = 15 * time.Minute
)

// Site settings for two-step sign-in and captcha.
const (
	settingRequire2FA      = "security.require_2fa_admins"
	settingRequire2FASince = "security.require_2fa_since" // set by the relay when the policy turns on
	setting2FAGraceDays    = "security.require_2fa_grace_days"
	settingRequire2FASSO   = "security.require_2fa_sso"
	settingCaptchaProvider = "security.captcha_provider"
	settingCaptchaSiteKey  = "security.captcha_site_key"
	settingCaptchaSecret   = "security.captcha_secret"
	settingLoginCaptcha    = "security.login_captcha_after"
)

func (h *Handler) mfaStore() (store.MFA, error) {
	m, ok := store.MFAOf(h.d.Store)
	if !ok {
		return nil, errConflict(eMFAUnsupported)
	}
	return m, nil
}

// factors is what a user can prove a second factor with.
type factors struct {
	totp     bool
	passkeys []store.Passkey
	codes    int
}

func (f factors) any() bool { return f.totp || len(f.passkeys) > 0 }

func (f factors) methods() []string {
	out := []string{}
	if f.totp {
		out = append(out, "totp")
	}
	if len(f.passkeys) > 0 {
		out = append(out, "passkey")
	}
	if f.codes > 0 {
		out = append(out, "recovery")
	}
	return out
}

func (h *Handler) factorsOf(ctx context.Context, userID string) (factors, error) {
	m, ok := store.MFAOf(h.d.Store)
	if !ok || userID == "" {
		return factors{}, nil
	}
	var f factors
	if _, err := m.TOTPSecret(ctx, userID); err == nil {
		f.totp = true
	} else if !errors.Is(err, store.ErrNotFound) {
		return f, err
	}
	var err error
	if f.passkeys, err = m.ListPasskeys(ctx, userID); err != nil {
		return f, err
	}
	f.codes, err = m.CountRecoveryCodes(ctx, userID)
	return f, err
}

// needsSecondFactor reports whether a sign-in by password (sso=false) or
// single sign-on must be followed by a second factor.
func (h *Handler) needsSecondFactor(ctx context.Context, u *store.User, sso bool) (bool, error) {
	if sso {
		s, err := h.site(ctx)
		if err != nil {
			return false, err
		}
		if on, _ := s.Bool(settingRequire2FASSO); !on {
			return false, nil
		}
	}
	f, err := h.factorsOf(ctx, u.ID)
	return f.any(), err
}

// mfaPolicy is the admin enrollment rule for one user.
type mfaPolicy struct {
	required bool
	enrollBy time.Time
}

// policyFor applies security.require_2fa_admins: admins must enroll
// within the grace period counted from when the rule turned on (or the
// account was created, if later). Admins who can only sign in by SSO are
// exempt unless SSO sign-ins ask for the second factor too.
func (h *Handler) policyFor(s Site, u *store.User) mfaPolicy {
	on, _ := s.Bool(settingRequire2FA)
	if !on || u == nil || u.Role != store.RoleAdmin {
		return mfaPolicy{}
	}
	if sso, _ := s.Bool(settingRequire2FASSO); !u.HasPassword() && !sso {
		return mfaPolicy{}
	}
	var since int64
	_ = json.Unmarshal(s[settingRequire2FASince], &since)
	start := time.UnixMilli(since)
	if u.CreatedAt.After(start) {
		start = u.CreatedAt
	}
	days := 7
	if raw, ok := s[setting2FAGraceDays]; ok {
		_ = json.Unmarshal(raw, &days)
	}
	return mfaPolicy{required: true, enrollBy: start.Add(time.Duration(days) * 24 * time.Hour)}
}

// codeMFAEnrollment is the error code of an admin past the enrollment
// deadline without a second factor.
const codeMFAEnrollment = "mfa_enrollment_required"

// enforceMFAPolicy refuses admin operations to an admin past the grace
// period without a second factor; their own security page stays open.
func (h *Handler) enforceMFAPolicy(c *call) error {
	who := c.who
	if who.auth != authSession || who.user == nil {
		return nil
	}
	return h.mfaEnrollmentDue(c.ctx, who.user)
}

// mfaEnrollmentDue returns the codeMFAEnrollment error when u is past the
// enrollment deadline without a second factor.
func (h *Handler) mfaEnrollmentDue(ctx context.Context, u *store.User) error {
	s, err := h.site(ctx)
	if err != nil {
		return err
	}
	p := h.policyFor(s, u)
	if !p.required || h.d.Now().Before(p.enrollBy) {
		return nil
	}
	f, err := h.factorsOf(ctx, u.ID)
	if err != nil || f.any() {
		return err
	}
	return errForbidden(eMFAEnrollment)
}

// mfaMe is the two-step sign-in summary GET /me reports.
func (h *Handler) mfaMe(ctx context.Context, s Site, u *store.User) (map[string]any, error) {
	f, err := h.factorsOf(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"enabled": f.any(), "required": false}
	if p := h.policyFor(s, u); p.required {
		out["required"] = true
		if !f.any() {
			out["enroll_by"] = p.enrollBy.UTC()
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- KV helpers

func (h *Handler) mfaKV() plugin.KV { return h.d.Store.KV(mfaKV) }

func (h *Handler) putJSON(ctx context.Context, key string, v any, ttl time.Duration) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return h.mfaKV().Put(ctx, key, b, ttl)
}

// takeOnce reads and deletes key; only the first caller across nodes
// gets the value.
func (h *Handler) takeOnce(ctx context.Context, key string, ttl time.Duration, v any) (bool, error) {
	kv := h.mfaKV()
	n, err := kv.Incr(ctx, "used:"+key, 1, ttl)
	if err != nil || n != 1 {
		return false, err
	}
	raw, ok, err := kv.Get(ctx, key)
	if err != nil || !ok {
		return false, err
	}
	_ = kv.Delete(ctx, key)
	return json.Unmarshal(raw, v) == nil, nil
}

func sessionHash(r *http.Request) string {
	if ck, err := r.Cookie(SessionCookie); err == nil && ck.Value != "" {
		return secret.HashKey(ck.Value)
	}
	return ""
}

func (h *Handler) markReauth(ctx context.Context, sessHash string) error {
	return h.mfaKV().Put(ctx, "reauth:"+sessHash, []byte("1"), reauthTTL)
}

// ---------------------------------------------------------------- WebAuthn RP

// webauthnRP is the relying party: the RP ID is the host of the public
// URL and the only accepted origin is that URL's origin. A relay reachable
// from other machines must have it set; a Host header must never pick the
// RP.
func (h *Handler) webauthnRP(c *call) (rpID, origin string, err error) {
	base, err := h.trustedBaseURL(c)
	if err != nil {
		return "", "", err
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return "", "", errConflict(ePublicURLInvalid)
	}
	return strings.ToLower(u.Hostname()), webOrigin(u), nil
}

// webOrigin serializes u's origin as browsers put it in clientDataJSON:
// lower case, without a default port ("https://x:443" is "https://x").
func webOrigin(u *url.URL) string {
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

// waChallenge is what an issued WebAuthn challenge is for.
type waChallenge struct {
	Purpose string `json:"purpose"` // register | login | mfa | reauth
	UserID  string `json:"user_id,omitempty"`
	// Bind is the pending login (mfa) or session (reauth) it belongs to.
	Bind string `json:"bind,omitempty"`
}

func (h *Handler) newChallenge(ctx context.Context, ch waChallenge) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := enc64(b)
	return s, h.putJSON(ctx, "challenge:"+s, ch, challengeTTL)
}

// redeemer consumes a challenge if it was issued for want.
func (h *Handler) redeemer(ctx context.Context, want waChallenge) func(string) bool {
	return func(ch string) bool {
		if len(ch) > 128 {
			return false
		}
		var got waChallenge
		ok, err := h.takeOnce(ctx, "challenge:"+ch, challengeTTL, &got)
		return err == nil && ok && got == want
	}
}

type credentialRef struct {
	Type       string   `json:"type"`
	ID         string   `json:"id"`
	Transports []string `json:"transports,omitempty"`
}

func credentialRefs(pks []store.Passkey) []credentialRef {
	out := []credentialRef{}
	for _, p := range pks {
		out = append(out, credentialRef{Type: "public-key", ID: p.CredentialID, Transports: p.Transports})
	}
	return out
}

// passkeyCredential is PublicKeyCredential.toJSON().
type passkeyCredential struct {
	ID       string `json:"id"`
	RawID    string `json:"rawId"`
	Type     string `json:"type"`
	Response struct {
		ClientDataJSON    string   `json:"clientDataJSON"`
		AttestationObject string   `json:"attestationObject"`
		AuthenticatorData string   `json:"authenticatorData"`
		Signature         string   `json:"signature"`
		UserHandle        string   `json:"userHandle"`
		Transports        []string `json:"transports"`
	} `json:"response"`
}

func (p *passkeyCredential) rawID() (string, error) {
	id := p.RawID
	if id == "" {
		id = p.ID
	}
	b, err := b64url(id)
	if err != nil || len(b) == 0 {
		return "", waErr("bad credential id")
	}
	return enc64(b), nil
}

// checkPasskey verifies an assertion for want (purpose, user, binding);
// want.UserID "" accepts any user, checked against the user handle.
func (h *Handler) checkPasskey(c *call, cred *passkeyCredential, want waChallenge, requireUV bool) (*store.Passkey, error) {
	m, err := h.mfaStore()
	if err != nil {
		return nil, err
	}
	rpID, origin, err := h.webauthnRP(c)
	if err != nil {
		return nil, err
	}
	if cred == nil {
		return nil, waErr("no credential")
	}
	id, err := cred.rawID()
	if err != nil {
		return nil, err
	}
	pk, err := m.PasskeyByCredential(c.ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, waErr("unknown passkey")
		}
		return nil, err
	}
	if want.UserID != "" && pk.UserID != want.UserID {
		return nil, waErr("passkey belongs to another user")
	}
	if cred.Response.UserHandle != "" {
		if uh, err := b64url(cred.Response.UserHandle); err != nil || string(uh) != pk.UserID {
			return nil, waErr("user handle does not match the passkey")
		}
	}
	cdj, err1 := b64url(cred.Response.ClientDataJSON)
	adb, err2 := b64url(cred.Response.AuthenticatorData)
	sig, err3 := b64url(cred.Response.Signature)
	key, err4 := b64url(pk.PublicKey)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return nil, waErr("bad encoding")
	}
	want.UserID = pk.UserID
	if want.Purpose == "login" {
		want.UserID = ""
	}
	as, err := verifyAssertion(cdj, adb, sig, key, rpID, origin, requireUV, h.redeemer(c.ctx, want))
	if err != nil {
		return nil, err
	}
	ok, err := m.UsePasskey(c.ctx, pk.ID, as.signCount, h.d.Now())
	if err != nil {
		return nil, err
	}
	if !ok {
		h.d.Logger.Warn("passkey signature counter did not grow; possible cloned authenticator", "user", pk.UserID,
			"passkey", pk.ID, "stored", pk.SignCount, "got", as.signCount)
		return nil, waErr("signature counter went backwards")
	}
	return pk, nil
}

// ---------------------------------------------------------------- factor checks

// factorInput proves a factor: a TOTP code, a recovery code, a passkey
// assertion or (re-authentication only) the password.
type factorInput struct {
	Method     string             `json:"method"`
	Code       string             `json:"code"`
	Password   string             `json:"password"`
	Credential *passkeyCredential `json:"credential"`
}

// checkFactor reports whether in proves a factor of u; ch says what a
// passkey challenge must have been issued for.
func (h *Handler) checkFactor(c *call, u *store.User, in factorInput, ch waChallenge, allowPassword bool) (bool, error) {
	m, err := h.mfaStore()
	if err != nil {
		return false, err
	}
	switch in.Method {
	case "totp":
		key, err := m.TOTPSecret(c.ctx, u.ID)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		step, ok := matchTOTP(key, in.Code, h.d.Now())
		if !ok {
			return false, nil
		}
		// A code is good for one sign-in: its step is spent on first use.
		return m.UseTOTPStep(c.ctx, u.ID, step)
	case "recovery":
		if normalizeCode(in.Code) == "" {
			return false, nil
		}
		ok, err := m.UseRecoveryCode(c.ctx, u.ID, hashRecoveryCode(u.ID, in.Code))
		if ok {
			c.audit("auth.recovery_code_used", "user", u.ID, displayName(u), nil)
		}
		return ok, err
	case "passkey":
		ch.UserID = u.ID
		if _, err := h.checkPasskey(c, in.Credential, ch, false); err != nil {
			if errors.Is(err, errWebAuthn) {
				h.d.Logger.Info("passkey check failed", "user", u.ID, "err", err)
				return false, nil
			}
			return false, err
		}
		return true, nil
	case "password":
		return allowPassword && u.CheckPassword(in.Password), nil
	}
	return false, fieldInvalid("method", oneOf("totp", "recovery", "passkey", "password"))
}

// throttled refuses when the address or the account has too many recent
// failures.
func (h *Handler) throttled(c *call, ip, userID string) error {
	now := h.d.Now()
	wait := h.logins.blocked(ip, now)
	if w := h.logins.blocked("user:"+userID, now); w > wait {
		wait = w
	}
	if wait > 0 {
		c.w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		return newError(http.StatusTooManyRequests, eRateLimited)
	}
	return nil
}

func (h *Handler) failedFactor(ip, userID string) {
	now := h.d.Now()
	h.logins.failed(ip, now)
	h.logins.failed("user:"+userID, now)
}

// ---------------------------------------------------------------- re-authentication

// sessionCaller is the signed-in console user managing their own
// sign-in methods.
func (h *Handler) sessionCaller(c *call) (*store.User, error) {
	if c.who == nil || c.who.auth != authSession || c.who.user == nil {
		return nil, errConflict(eMFANeedsUser)
	}
	return c.who.user, nil
}

// requireReauth refuses a sensitive change unless the session signed in
// or re-authenticated within reauthTTL. Users with nothing to prove
// (SSO only, no factor yet) pass.
func (h *Handler) requireReauth(c *call, u *store.User) error {
	f, err := h.factorsOf(c.ctx, u.ID)
	if err != nil {
		return err
	}
	if !u.HasPassword() && !f.any() {
		return nil
	}
	if sh := sessionHash(c.r); sh != "" {
		if _, ok, err := h.mfaKV().Get(c.ctx, "reauth:"+sh); err != nil {
			return err
		} else if ok {
			return nil
		}
	}
	return errForbidden(eReauthRequired)
}

// requireAdminReauth is requireReauth for an admin creating an admin or
// changing a user's password, role, status or second factors. Callers
// without a console session (the operator token, the local owner) have no
// session to re-authenticate and pass, as they would pass a sign-in.
func (h *Handler) requireAdminReauth(c *call) error {
	if c.who == nil || c.who.auth != authSession || c.who.user == nil {
		return nil
	}
	return h.requireReauth(c, c.who.user)
}

func (h *Handler) reauth(c *call) error {
	u, err := h.sessionCaller(c)
	if err != nil {
		return err
	}
	var in factorInput
	if err := c.decode(&in); err != nil {
		return err
	}
	ip := h.d.ClientIP(c.r)
	if err := h.throttled(c, ip, u.ID); err != nil {
		return err
	}
	sh := sessionHash(c.r)
	ok, err := h.checkFactor(c, u, in, waChallenge{Purpose: "reauth", Bind: sh}, true)
	if err != nil {
		return err
	}
	if !ok {
		h.failedFactor(ip, u.ID)
		return errUnauthorized(eReauthFailed)
	}
	if err := h.markReauth(c.ctx, sh); err != nil {
		return err
	}
	return c.json(200, map[string]any{"until": h.d.Now().Add(reauthTTL).UTC()})
}

// ---------------------------------------------------------------- security page

type passkeyView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	Transports []string   `json:"transports,omitempty"`
}

func passkeyViews(pks []store.Passkey) []passkeyView {
	out := []passkeyView{}
	for _, p := range pks {
		out = append(out, passkeyView{ID: p.ID, Name: p.Name, CreatedAt: p.CreatedAt, LastUsedAt: timePtr(p.LastUsedAt), Transports: p.Transports})
	}
	return out
}

func (h *Handler) getSecurity(c *call) error {
	u, err := h.sessionCaller(c)
	if err != nil {
		return err
	}
	f, err := h.factorsOf(c.ctx, u.ID)
	if err != nil {
		return err
	}
	s, err := h.site(c.ctx)
	if err != nil {
		return err
	}
	out := map[string]any{"totp_enabled": f.totp, "recovery_codes_left": f.codes, "passkeys": passkeyViews(f.passkeys),
		"has_password": u.HasPassword(), "required": false}
	if p := h.policyFor(s, u); p.required {
		out["required"] = true
		out["enroll_by"] = p.enrollBy.UTC()
	}
	if _, _, err := h.webauthnRP(c); err != nil {
		out["passkeys_unavailable"] = "public_url_missing"
	}
	return c.json(200, out)
}

type totpSetup struct {
	Secret string `json:"secret"` // sealed
}

func (h *Handler) setupTOTP(c *call) error {
	u, err := h.sessionCaller(c)
	if err != nil {
		return err
	}
	if _, err := h.mfaStore(); err != nil {
		return err
	}
	if err := h.requireReauth(c, u); err != nil {
		return err
	}
	if h.d.Box == nil {
		return errConflict(eNoMasterKey)
	}
	key, err := newTOTPSecret()
	if err != nil {
		return err
	}
	sealed, err := h.d.Box.Seal(key, totpSetupAD(u.ID))
	if err != nil {
		return err
	}
	if err := h.putJSON(c.ctx, "totp-setup:"+u.ID, totpSetup{Secret: sealed}, totpSetupTTL); err != nil {
		return err
	}
	return c.json(200, map[string]any{"secret": b32.EncodeToString(key), "otpauth_uri": otpauthURI(h.siteName(c.ctx), u.Email, key),
		"expires_at": h.d.Now().Add(totpSetupTTL).UTC()})
}

func totpSetupAD(userID string) string { return "totp-setup:" + userID }

func (h *Handler) enableTOTP(c *call) error {
	u, err := h.sessionCaller(c)
	if err != nil {
		return err
	}
	m, err := h.mfaStore()
	if err != nil {
		return err
	}
	var in struct {
		Code string `json:"code"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	ip := h.d.ClientIP(c.r)
	if err := h.throttled(c, ip, u.ID); err != nil {
		return err
	}
	raw, ok, err := h.mfaKV().Get(c.ctx, "totp-setup:"+u.ID)
	if err != nil {
		return err
	}
	var setup totpSetup
	if !ok || json.Unmarshal(raw, &setup) != nil {
		return errConflict(eTOTPSetupExpired)
	}
	key, err := h.d.Box.Open(setup.Secret, totpSetupAD(u.ID))
	if err != nil {
		return err
	}
	step, ok := matchTOTP(key, in.Code, h.d.Now())
	if !ok {
		h.failedFactor(ip, u.ID)
		return fieldInvalid("code", eTOTPMismatch)
	}
	codes, err := newRecoveryCodes()
	if err != nil {
		return err
	}
	if err := m.EnableTOTP(c.ctx, u.ID, key, step, hashRecoveryCodes(u.ID, codes)); err != nil {
		return storeErr(err, objAuthenticator)
	}
	_ = h.mfaKV().Delete(c.ctx, "totp-setup:"+u.ID)
	c.audit("user.mfa_enable", "user", u.ID, displayName(u), []change{{Field: "totp", To: true}})
	return c.json(200, map[string]any{"recovery_codes": codes})
}

func (h *Handler) disableTOTP(c *call) error {
	u, err := h.sessionCaller(c)
	if err != nil {
		return err
	}
	m, err := h.mfaStore()
	if err != nil {
		return err
	}
	if err := h.requireReauth(c, u); err != nil {
		return err
	}
	if err := m.DeleteTOTP(c.ctx, u.ID); err != nil {
		return storeErr(err, objAuthenticator)
	}
	c.audit("user.mfa_disable", "user", u.ID, displayName(u), []change{{Field: "totp", From: true, To: false}})
	return c.noContent()
}

func (h *Handler) regenerateRecoveryCodes(c *call) error {
	u, err := h.sessionCaller(c)
	if err != nil {
		return err
	}
	m, err := h.mfaStore()
	if err != nil {
		return err
	}
	if err := h.requireReauth(c, u); err != nil {
		return err
	}
	f, err := h.factorsOf(c.ctx, u.ID)
	if err != nil {
		return err
	}
	if !f.any() {
		return errConflict(eMFAOffFirst)
	}
	codes, err := newRecoveryCodes()
	if err != nil {
		return err
	}
	if err := m.ReplaceRecoveryCodes(c.ctx, u.ID, hashRecoveryCodes(u.ID, codes)); err != nil {
		return err
	}
	c.audit("user.recovery_codes", "user", u.ID, displayName(u), nil)
	return c.json(200, map[string]any{"recovery_codes": codes})
}

// changeOwnPassword sets the caller's password. Proving who they are is the
// re-authentication step, which asks for the current password when there is one.
func (h *Handler) changeOwnPassword(c *call) error {
	u, err := h.sessionCaller(c)
	if err != nil {
		return err
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if len(in.Password) < minPassword {
		return fieldInvalid("password", eMinLength.with("min", minPassword))
	}
	if err := h.requireReauth(c, u); err != nil {
		return err
	}
	if err := h.d.Store.SetPassword(c.ctx, u.ID, in.Password); err != nil {
		return storeErr(err, objUser)
	}
	c.audit("user.update", "user", u.ID, displayName(u), []change{{Field: "password", To: "********"}})
	return c.noContent()
}

// ---------------------------------------------------------------- passkeys

func (h *Handler) passkeyRegisterOptions(c *call) error {
	u, err := h.sessionCaller(c)
	if err != nil {
		return err
	}
	m, err := h.mfaStore()
	if err != nil {
		return err
	}
	rpID, _, err := h.webauthnRP(c)
	if err != nil {
		return err
	}
	if err := h.requireReauth(c, u); err != nil {
		return err
	}
	pks, err := m.ListPasskeys(c.ctx, u.ID)
	if err != nil {
		return err
	}
	ch, err := h.newChallenge(c.ctx, waChallenge{Purpose: "register", UserID: u.ID})
	if err != nil {
		return err
	}
	params := []map[string]any{}
	for _, a := range coseAlgs {
		params = append(params, map[string]any{"type": "public-key", "alg": a})
	}
	return c.json(200, map[string]any{
		"challenge": ch,
		"rp":        map[string]any{"id": rpID, "name": h.siteName(c.ctx)},
		"user": map[string]any{"id": enc64([]byte(u.ID)), "name": orDefault(u.Email, u.ID),
			"displayName": orDefault(u.Name, u.Email)},
		"pubKeyCredParams":       params,
		"timeout":                challengeTTL.Milliseconds(),
		"attestation":            "none",
		"excludeCredentials":     credentialRefs(pks),
		"authenticatorSelection": map[string]any{"residentKey": "preferred", "userVerification": "preferred"},
	})
}

func (h *Handler) registerPasskey(c *call) error {
	u, err := h.sessionCaller(c)
	if err != nil {
		return err
	}
	m, err := h.mfaStore()
	if err != nil {
		return err
	}
	var in struct {
		Name       string             `json:"name"`
		Credential *passkeyCredential `json:"credential"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if in.Credential == nil {
		return fieldInvalid("credential", eRequired)
	}
	rpID, origin, err := h.webauthnRP(c)
	if err != nil {
		return err
	}
	if err := h.requireReauth(c, u); err != nil {
		return err
	}
	cdj, err1 := b64url(in.Credential.Response.ClientDataJSON)
	att, err2 := b64url(in.Credential.Response.AttestationObject)
	if errors.Join(err1, err2) != nil {
		return fieldInvalid("credential", eBadEncoding)
	}
	reg, err := verifyRegistration(cdj, att, rpID, origin, h.redeemer(c.ctx, waChallenge{Purpose: "register", UserID: u.ID}))
	if err != nil {
		return newError(http.StatusUnprocessableEntity, ePasskeyRejected.with("reason", strings.TrimPrefix(err.Error(), "webauthn: ")))
	}
	if id, err := in.Credential.rawID(); err != nil || id != enc64(reg.credID) {
		return newError(http.StatusUnprocessableEntity, ePasskeyIDMismatch)
	}
	before, err := h.factorsOf(c.ctx, u.ID)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "Passkey " + strconv.Itoa(len(before.passkeys)+1)
	}
	if len(name) > 64 {
		name = name[:64]
	}
	pk := &store.Passkey{UserID: u.ID, CredentialID: enc64(reg.credID), PublicKey: enc64(reg.coseKey), Alg: reg.alg,
		SignCount: reg.signCount, Name: name, Transports: in.Credential.Response.Transports}
	if err := m.AddPasskey(c.ctx, pk); err != nil {
		return storeErr(err, objPasskey)
	}
	out := map[string]any{"passkey": passkeyViews([]store.Passkey{*pk})[0]}
	if before.codes == 0 {
		codes, err := newRecoveryCodes()
		if err != nil {
			return err
		}
		if err := m.ReplaceRecoveryCodes(c.ctx, u.ID, hashRecoveryCodes(u.ID, codes)); err != nil {
			return err
		}
		out["recovery_codes"] = codes
	}
	c.audit("user.passkey_add", "user", u.ID, displayName(u), []change{{Field: "passkey", To: name}})
	return c.json(201, out)
}

func (h *Handler) renamePasskey(c *call) error {
	u, err := h.sessionCaller(c)
	if err != nil {
		return err
	}
	m, err := h.mfaStore()
	if err != nil {
		return err
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 64 {
		return fieldInvalid("name", eLengthRange.with("min", 1, "max", 64))
	}
	if err := m.RenamePasskey(c.ctx, u.ID, c.r.PathValue("id"), name); err != nil {
		return storeErr(err, objPasskey)
	}
	return c.noContent()
}

func (h *Handler) deletePasskey(c *call) error {
	u, err := h.sessionCaller(c)
	if err != nil {
		return err
	}
	m, err := h.mfaStore()
	if err != nil {
		return err
	}
	if err := h.requireReauth(c, u); err != nil {
		return err
	}
	if err := m.DeletePasskey(c.ctx, u.ID, c.r.PathValue("id")); err != nil {
		return storeErr(err, objPasskey)
	}
	c.audit("user.passkey_delete", "user", u.ID, displayName(u), []change{{Field: "passkey", From: c.r.PathValue("id")}})
	return c.noContent()
}

// ---------------------------------------------------------------- admin reset

func (h *Handler) resetUserMFA(c *call) error {
	u, err := h.d.Store.GetUser(c.ctx, c.r.PathValue("id"))
	if err != nil {
		return storeErr(err, objUser)
	}
	if err := h.requireAdminReauth(c); err != nil {
		return err
	}
	if err := ResetMFA(c.ctx, h.d.Store, u.ID); err != nil {
		return err
	}
	c.audit("user.mfa_reset", "user", u.ID, displayName(u), []change{{Field: "mfa", To: "reset"}})
	return c.noContent()
}

// ResetMFA removes every second factor of a user (admin reset, CLI).
func ResetMFA(ctx context.Context, st store.Store, userID string) error {
	m, ok := store.MFAOf(st)
	if !ok {
		return errors.New("this store does not support two-step sign-in")
	}
	if err := m.ResetMFA(ctx, userID); err != nil {
		return err
	}
	return st.KV(mfaKV).Delete(ctx, "totp-setup:"+userID)
}

// AuditMFAReset records a reset done by the CLI on the database directly.
func AuditMFAReset(ctx context.Context, st store.Store, u *store.User) error {
	diff, err := json.Marshal(auditDiff{ActorName: "command line", TargetName: displayName(u),
		Changes: []change{{Field: "mfa", To: "reset"}, {Field: "method", To: "cli"}}})
	if err != nil {
		return err
	}
	return st.AddAudit(ctx, &store.Audit{Time: time.Now(), Actor: "cli", Action: "user.mfa_reset", Target: "user:" + u.ID, Diff: diff})
}
