package admin

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/initxy/plugrelay/internal/features"
	"github.com/initxy/plugrelay/internal/store"
)

const minPassword = 10

func (h *Handler) getSetupStatus(c *call) error {
	needs, err := h.NeedsSetup(c.ctx)
	if err != nil {
		return err
	}
	addr, _ := h.d.Listen()
	return c.json(200, map[string]any{"needs_setup": needs, "version": h.d.Version, "listen": addr})
}

func (h *Handler) runSetup(c *call) error {
	var in struct {
		Username     string `json:"admin_username"`
		Password     string `json:"admin_password"`
		Usage        string `json:"usage"`
		ListenPublic *bool  `json:"listen_public"`
		SiteName     string `json:"site_name"`
		Currency     string `json:"currency"`
		PublicURL    string `json:"public_url"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" {
		bad.add("admin_username", eNotEmpty)
	}
	if len(in.Password) < minPassword {
		bad.add("admin_password", eMinLength.with("min", minPassword))
	}
	if !slices.Contains([]string{usageMyself, usageTeam, usageSell}, in.Usage) {
		bad.add("usage", oneOf(usageMyself, usageTeam, usageSell))
	}
	if in.ListenPublic == nil {
		bad.add("listen_public", eRequired)
	}
	if in.Currency == "" {
		in.Currency = "USD"
	}
	if len(in.Currency) != 3 {
		bad.add("currency", eCurrencyCode)
	}
	in.PublicURL = strings.TrimRight(strings.TrimSpace(in.PublicURL), "/")
	if u, err := url.Parse(in.PublicURL); in.PublicURL != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
		bad.add("public_url", eURLHTTP)
	}
	if err := bad.err(); err != nil {
		return err
	}
	h.siteMu.Lock()
	defer h.siteMu.Unlock()
	if needs, err := h.NeedsSetup(c.ctx); err != nil {
		return err
	} else if !needs {
		return errConflict(eSetupDone)
	}
	if err := h.setupFeatures(c, in.Usage); err != nil {
		return err
	}
	u := &store.User{Name: in.Username, Email: in.Username, Role: store.RoleAdmin}
	if err := h.d.Store.CreateUser(c.ctx, u, in.Password); err != nil {
		return storeErr(err, objUser)
	}
	h.setupDone.Store(true)
	s, err := h.site(c.ctx)
	if err != nil {
		return err
	}
	set := func(k string, v any) { s[k], _ = json.Marshal(v) }
	set(SettingListenPublic, *in.ListenPublic)
	if in.Usage != usageMyself {
		set(SettingLocalOwner, false)
	}
	set(settingCurrency, strings.ToUpper(in.Currency))
	if in.SiteName != "" {
		set(settingName, in.SiteName)
	}
	if in.PublicURL != "" {
		set(SettingPublicURL, in.PublicURL)
	}
	if in.Usage == usageSell {
		set(settingShowNames, false)
	}
	if err := saveSite(c.ctx, h.d.Store, s); err != nil {
		return err
	}
	if err := h.startSession(c, u); err != nil {
		return err
	}
	c.who = &identity{user: u, auth: authSession, role: u.Role}
	c.audit("setup.run", "user", u.ID, u.Name, []change{{Field: "usage", To: in.Usage}, {Field: "listen_public", To: *in.ListenPublic}})
	return h.writeMe(c)
}

// Answers to the setup wizard's "how will you use it"; only sell changes
// what runs.
const (
	usageMyself = "myself"
	usageTeam   = "team"
	usageSell   = "sell"
)

// setupFeatures turns features on for a setup answer. Selling turns on
// billing; self sign-up stays off (it opens public registration).
func (h *Handler) setupFeatures(c *call, usage string) error {
	if usage != usageSell {
		return nil
	}
	fs := store.FeatureState{ID: "billing", Enabled: true}
	if err := h.d.Runtime.CheckFeature(c.ctx, fs); err != nil {
		return errConflict(eBillingCannotRun.with("reason", err.Error()))
	}
	return storeErr(h.d.Store.PutFeature(c.ctx, fs), objFeature)
}

func (h *Handler) login(c *call) error {
	// A cross-site form could post a JSON-looking text/plain body and sign
	// the browser into the attacker's account (login CSRF).
	if err := h.checkOrigin(c.r); err != nil {
		return err
	}
	if mt, _, _ := mime.ParseMediaType(c.r.Header.Get("Content-Type")); mt != "application/json" {
		return errForbidden(eCrossOrigin)
	}
	var in struct {
		Username     string `json:"username"`
		Password     string `json:"password"`
		CaptchaToken string `json:"captcha_token"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if needs, err := h.NeedsSetup(c.ctx); err != nil {
		return err
	} else if needs {
		return errForbidden(eSetupRequired)
	}
	if off, err := h.passwordLoginDisabled(c.ctx); err != nil {
		return err
	} else if off {
		return errForbidden(ePasswordLoginOff)
	}
	ip := h.d.ClientIP(c.r)
	now := h.d.Now()
	if wait := h.logins.blocked(ip, now); wait > 0 {
		c.w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		return newError(http.StatusTooManyRequests, eRateLimited)
	}
	if err := h.checkLoginCaptcha(c.ctx, ip, in.CaptchaToken, now); err != nil {
		return err
	}
	u, err := h.d.Store.Authenticate(c.ctx, strings.TrimSpace(in.Username), in.Password)
	if err != nil {
		if errors.Is(err, store.ErrInvalidLogin) {
			h.logins.failed(ip, now)
			return errUnauthorized(eWrongPassword)
		}
		return err
	}
	if need, err := h.needsSecondFactor(c.ctx, u, false); err != nil {
		return err
	} else if need {
		if err := h.beginSecondFactor(c, u, "password", ""); err != nil {
			return err
		}
		return h.secondFactorChallenge(c, u)
	}
	h.logins.reset(ip)
	if err := h.startSession(c, u); err != nil {
		return err
	}
	c.who = &identity{user: u, auth: authSession, role: u.Role}
	c.audit("auth.login", "user", u.ID, displayName(u), nil)
	return h.writeMe(c)
}

func (h *Handler) logout(c *call) error {
	if ck, err := c.r.Cookie(SessionCookie); err == nil && ck.Value != "" {
		if err := h.d.Store.DeleteSession(c.ctx, ck.Value); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	clearSession(c.w, c.r)
	return c.noContent()
}

func (h *Handler) getMe(c *call) error { return h.writeMe(c) }

var adminPermissions = []string{"upstreams.manage", "models.manage", "users.manage", "features.manage",
	"settings.manage", "audit.read", "keys.manage_all", "logs.read_all"}

func (h *Handler) writeMe(c *call) error {
	who := c.who
	user := map[string]any{"id": "token", "username": "admin-token", "role": who.role}
	if who.user != nil {
		user = map[string]any{"id": who.user.ID, "username": who.user.Email, "display_name": who.user.Name,
			"role": who.user.Role}
		if who.user.GroupID != "" {
			user["group_id"] = who.user.GroupID
		}
	}
	perms := []string{}
	if who.isAdmin() {
		perms = adminPermissions
	}
	s, err := h.site(c.ctx)
	if err != nil {
		return err
	}
	site := map[string]any{"name": s.name(), "currency": h.currency(c.ctx),
		"show_upstream_names": s.showUpstreamNames(), "public_base_url": h.baseURL(c)}
	if v := s.String(settingLogo); v != "" {
		site["logo_url"] = v
	}
	if v := s.String(settingAccent); v != "" {
		site["accent"] = v
	}
	on, pages, err := h.visibleFeatures(c)
	if err != nil {
		return err
	}
	out := map[string]any{"user": user, "auth": who.auth, "permissions": perms, "site": site, "features": on, "pages": pages}
	if who.auth == authSession && who.user != nil {
		if out["mfa"], err = h.mfaMe(c.ctx, s, who.user); err != nil {
			return err
		}
	}
	return c.json(200, out)
}

// visibleFeatures lists the features that are on and the console pages
// they add for the caller: operator pages for admins, the others for
// everyone else.
func (h *Handler) visibleFeatures(c *call) ([]string, []string, error) {
	st, err := h.featureState(c)
	if err != nil {
		return nil, nil, err
	}
	on, pages := []string{}, []string{}
	for _, f := range features.All() {
		if !st.On(f.ID) {
			continue
		}
		on = append(on, f.ID)
		for _, p := range f.Pages {
			if p.Admin == c.who.isAdmin() {
				pages = append(pages, p.ID)
			}
		}
	}
	return on, pages, nil
}

// baseURL is the URL clients should use: the stored public base URL, or
// the address this request reached.
func (h *Handler) baseURL(c *call) string {
	if s, err := h.site(c.ctx); err == nil {
		if v := s.String(SettingPublicURL); v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	scheme := "http"
	if secureRequest(c.r) {
		scheme = "https"
	}
	return scheme + "://" + c.r.Host
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func displayName(u *store.User) string {
	if u.Name != "" {
		return u.Name
	}
	return u.Email
}
