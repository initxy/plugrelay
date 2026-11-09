package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
)

// mfaPending is a sign-in whose first step passed. No session exists
// until the second factor does.
type mfaPending struct {
	UserID   string `json:"user_id"`
	Method   string `json:"method"` // password | sso:<provider>
	ReturnTo string `json:"return_to,omitempty"`
}

func clearMFACookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: mfaCookie, Value: "", Path: Prefix + "/auth/", MaxAge: -1, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secureRequest(r)})
}

// beginSecondFactor stores a pending sign-in and binds it to the browser.
func (h *Handler) beginSecondFactor(c *call, u *store.User, method, returnTo string) error {
	tok, err := secret.GenerateToken()
	if err != nil {
		return err
	}
	if err := h.putJSON(c.ctx, "pending:"+secret.HashKey(tok), mfaPending{UserID: u.ID, Method: method, ReturnTo: returnTo}, mfaPendingTTL); err != nil {
		return err
	}
	http.SetCookie(c.w, &http.Cookie{Name: mfaCookie, Value: tok, Path: Prefix + "/auth/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secureRequest(c.r), MaxAge: int(mfaPendingTTL / time.Second)})
	return nil
}

var errMFAExpired = errUnauthorized(eMFAExpired)

// pendingLogin resolves the pr_mfa cookie; key is the pending entry's KV key.
func (h *Handler) pendingLogin(c *call) (*mfaPending, string, error) {
	ck, err := c.r.Cookie(mfaCookie)
	if err != nil || ck.Value == "" {
		return nil, "", errMFAExpired
	}
	key := "pending:" + secret.HashKey(ck.Value)
	raw, ok, err := h.mfaKV().Get(c.ctx, key)
	if err != nil {
		return nil, "", err
	}
	var p mfaPending
	if !ok || json.Unmarshal(raw, &p) != nil {
		return nil, "", errMFAExpired
	}
	return &p, key, nil
}

func (h *Handler) secondFactorChallenge(c *call, u *store.User) error {
	f, err := h.factorsOf(c.ctx, u.ID)
	if err != nil {
		return err
	}
	return c.json(http.StatusAccepted, map[string]any{"mfa_required": true, "methods": f.methods(),
		"expires_at": h.d.Now().Add(mfaPendingTTL).UTC()})
}

func (h *Handler) getPendingMFA(c *call) error {
	p, _, err := h.pendingLogin(c)
	if err != nil {
		return err
	}
	f, err := h.factorsOf(c.ctx, p.UserID)
	if err != nil {
		return err
	}
	return c.json(200, map[string]any{"methods": f.methods(), "return_to": orDefault(p.ReturnTo, "/")})
}

// verifyMFA is the second step of a sign-in.
func (h *Handler) verifyMFA(c *call) error {
	if err := h.checkOrigin(c.r); err != nil {
		return err
	}
	p, key, err := h.pendingLogin(c)
	if err != nil {
		return err
	}
	var in factorInput
	if err := c.decode(&in); err != nil {
		return err
	}
	ip := h.d.ClientIP(c.r)
	if err := h.throttled(c, ip, p.UserID); err != nil {
		return err
	}
	kv := h.mfaKV()
	if n, err := kv.Incr(c.ctx, "tries:"+key, 1, mfaPendingTTL); err != nil {
		return err
	} else if n > mfaMaxTries {
		_ = kv.Delete(c.ctx, key)
		clearMFACookie(c.w, c.r)
		return errMFAExpired
	}
	u, err := h.d.Store.GetUser(c.ctx, p.UserID)
	if err != nil || u.Disabled {
		_ = kv.Delete(c.ctx, key)
		clearMFACookie(c.w, c.r)
		return errMFAExpired
	}
	c.who = &identity{user: u, auth: authSession, role: u.Role}
	if in.Method == "password" {
		in.Method = ""
	}
	ok, err := h.checkFactor(c, u, in, waChallenge{Purpose: "mfa", Bind: key}, false)
	if err != nil {
		return err
	}
	if !ok {
		h.failedFactor(ip, u.ID)
		return errUnauthorized(eMFAInvalid)
	}
	// One pending sign-in yields one session, even if two answers race.
	var again mfaPending
	if took, err := h.takeOnce(c.ctx, key, mfaPendingTTL, &again); err != nil {
		return err
	} else if !took {
		return errMFAExpired
	}
	clearMFACookie(c.w, c.r)
	h.logins.reset(ip)
	h.logins.reset("user:" + u.ID)
	if err := h.startSession(c, u); err != nil {
		return err
	}
	c.audit("auth.login", "user", u.ID, displayName(u), []change{{Field: "method", To: p.Method}, {Field: "second_factor", To: in.Method}})
	return h.writeMe(c)
}

// passkeyOptions issues an assertion challenge: for a passwordless
// sign-in (any discoverable passkey, user verification required), the
// second step of a pending sign-in, or re-authentication.
func (h *Handler) passkeyOptions(c *call) error {
	var in struct {
		Purpose string `json:"purpose"`
	}
	if err := c.decode(&in); err != nil {
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
	ch := waChallenge{Purpose: in.Purpose}
	uv := "preferred"
	var userID string
	switch in.Purpose {
	case "login":
		if err := h.passkeyLoginAllowed(c.ctx); err != nil {
			return err
		}
		// Unauthenticated, and each challenge is a stored row: bounded per
		// address like SSO starts.
		ip, now := h.d.ClientIP(c.r), h.d.Now()
		if wait := max(h.logins.blocked(ip, now), h.ssoStarts.blocked(ip, now)); wait > 0 {
			c.w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			return newError(http.StatusTooManyRequests, eRateLimited)
		}
		h.ssoStarts.failed(ip, now)
		uv = "required"
	case "mfa":
		p, key, err := h.pendingLogin(c)
		if err != nil {
			return err
		}
		userID, ch.Bind = p.UserID, key
	case "reauth":
		u, err := h.sessionCaller(c)
		if err != nil {
			return errUnauthorized(eNotLoggedIn)
		}
		userID, ch.Bind = u.ID, sessionHash(c.r)
	default:
		return fieldInvalid("purpose", oneOf("login", "mfa", "reauth"))
	}
	ch.UserID = userID
	allow := []credentialRef{}
	if userID != "" {
		pks, err := m.ListPasskeys(c.ctx, userID)
		if err != nil {
			return err
		}
		if len(pks) == 0 {
			return errConflict(eNoPasskey)
		}
		allow = credentialRefs(pks)
	}
	s, err := h.newChallenge(c.ctx, ch)
	if err != nil {
		return err
	}
	return c.json(200, map[string]any{"challenge": s, "rpId": rpID, "timeout": challengeTTL.Milliseconds(),
		"userVerification": uv, "allowCredentials": allow})
}

// passkeyLoginAllowed: passwordless sign-in follows the password switch,
// so an SSO-only site stays SSO-only.
func (h *Handler) passkeyLoginAllowed(ctx context.Context) error {
	if needs, err := h.NeedsSetup(ctx); err != nil {
		return err
	} else if needs {
		return errForbidden(eSetupRequired)
	}
	if off, err := h.passwordLoginDisabled(ctx); err != nil {
		return err
	} else if off {
		return errForbidden(ePasswordLoginOff)
	}
	return nil
}

// passkeyLogin signs in with a passkey alone; the authenticator's user
// verification makes it two factors, so no second step follows.
func (h *Handler) passkeyLogin(c *call) error {
	if err := h.checkOrigin(c.r); err != nil {
		return err
	}
	if err := h.passkeyLoginAllowed(c.ctx); err != nil {
		return err
	}
	var in struct {
		Credential *passkeyCredential `json:"credential"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	ip := h.d.ClientIP(c.r)
	now := h.d.Now()
	if wait := h.logins.blocked(ip, now); wait > 0 {
		c.w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		return newError(http.StatusTooManyRequests, eRateLimited)
	}
	pk, err := h.checkPasskey(c, in.Credential, waChallenge{Purpose: "login"}, true)
	if err != nil {
		if errors.Is(err, errWebAuthn) {
			h.logins.failed(ip, now)
			h.d.Logger.Info("passkey sign-in refused", "ip", ip, "err", err)
			return errUnauthorized(ePasskeyLoginFail)
		}
		return err
	}
	u, err := h.d.Store.GetUser(c.ctx, pk.UserID)
	if err != nil || u.Disabled {
		h.logins.failed(ip, now)
		return errUnauthorized(ePasskeyLoginFail)
	}
	h.logins.reset(ip)
	if err := h.startSession(c, u); err != nil {
		return err
	}
	c.who = &identity{user: u, auth: authSession, role: u.Role}
	c.audit("auth.login", "user", u.ID, displayName(u), []change{{Field: "method", To: "passkey"}})
	return h.writeMe(c)
}
