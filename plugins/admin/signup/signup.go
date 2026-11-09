// Package signup is the admin.signup plugin (sell kit): self-service
// console accounts by email and password, off unless enabled.
//
// Registration may require an invite code (invite_codes) and an email
// domain on an allowlist (allowed_domains). New accounts get the "user"
// role and default_group; they log in through the normal console login and
// create their own keys there. Attempts are limited per client address,
// and an optional captcha (Cloudflare Turnstile or hCaptcha, verified
// server-side through the core egress) guards registration. There is no
// email verification: the core marks these accounts' addresses
// unverified, so SSO link_by_email never links an identity to them. A
// referral code from an invite link (?ref=) records who invited the new
// account, for admin.rewards.
package signup

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/captcha"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

// ID is the plugin id.
const ID = "admin.signup"

func init() { plugin.Register(ID, func() plugin.Plugin { return New(nil, nil) }) }

// ErrExists: an account with this email exists.
var ErrExists = errors.New("an account with this email already exists")

// Accounts creates console users; the core implements it and marks their
// email unverified.
type Accounts interface {
	CreateUser(ctx context.Context, email, name, password, groupID string) (userID string, err error)
}

// AccountsHost is implemented by a Host that lets this plugin create users.
type AccountsHost interface {
	SignupAccounts() Accounts
}

// clientIPHost resolves the client address behind trusted proxies.
type clientIPHost interface {
	ClientIP(r *http.Request) string
}

// MinPassword is the shortest password accepted, as for console users.
const MinPassword = 10

// Plugin implements plugin.Admin.
type Plugin struct {
	acc      Accounts
	ops      ledger.Operations // nil without the sell kit's store
	kv       plugin.KV
	settings plugin.Settings
	clientIP func(*http.Request) string
	now      func() time.Time
	http     *http.Client
	// captchaEndpoint overrides the siteverify URL (tests).
	captchaEndpoint string
}

var _ plugin.Admin = (*Plugin)(nil)

// New returns the plugin; nil fields are taken from the Host in Init.
func New(acc Accounts, kv plugin.KV) *Plugin {
	return &Plugin{acc: acc, kv: kv, now: time.Now, clientIP: func(r *http.Request) string {
		host := r.RemoteAddr
		if i := strings.LastIndexByte(host, ':'); i > 0 {
			host = host[:i]
		}
		return host
	}}
}

// Info implements plugin.Plugin.
func (p *Plugin) Info() plugin.Info {
	return plugin.Info{ID: ID, Kind: plugin.KindAdmin, Version: "1.0.0",
		Description: "Self-service sign-up with email and password; optional invite codes and email-domain allowlist.",
		Settings: []plugin.Setting{
			{Key: "invite_required", Type: plugin.TypeBool, Default: false, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Invite code required", "必须填邀请码"),
				Help:  plugin.T("Only people with one of the invite codes below can sign up.", "只有拿到下面某个邀请码的人才能注册。")},
			{Key: "invite_codes", Type: plugin.TypeSecret, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Invite codes", "邀请码"),
				Help:  plugin.T("Separate with commas or new lines; each can be used many times.", "用逗号或换行分隔，每个都可以重复使用。")},
			{Key: "allowed_domains", Type: plugin.TypeList, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Allowed email domains", "允许注册的邮箱域名"),
				Help: plugin.T("Such as example.com; empty allows any. The address is not verified, so this is no proof that someone belongs to the domain; use single sign-on for that.",
					"例如 example.com；留空表示不限。注册时不验证邮箱，所以这只限制填写的地址，不能证明对方属于这个域名；要按公司身份放行请用单点登录。")},
			{Key: "default_group", Type: plugin.TypeGroup, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Group for new accounts", "新账号加入的分组"),
				Help:  plugin.T("Empty leaves them in no group.", "留空表示不加入分组。")},
			{Key: "per_ip_per_hour", Type: plugin.TypeInt, Default: 5, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Sign-up attempts per address per hour", "每个地址每小时可尝试注册几次"),
				Help:  plugin.T("Counts every attempt from one network address.", "按来源网络地址计数。")},
			{Key: "captcha_provider", Type: plugin.TypeString, Default: captcha.Off, Enum: captcha.Providers, Scopes: plugin.Global,
				Merge: plugin.Override, Label: plugin.T("Captcha on the sign-up form", "注册页的人机验证"),
				Help:       plugin.T("Cloudflare Turnstile or hCaptcha.", "可选 Cloudflare Turnstile 或 hCaptcha。"),
				EnumLabels: map[string]plugin.Text{"off": plugin.T("Off", "不用"), "turnstile": plugin.T("Cloudflare Turnstile", "Cloudflare Turnstile"), "hcaptcha": plugin.T("hCaptcha", "hCaptcha")}},
			{Key: "captcha_site_key", Type: plugin.TypeString, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Captcha site key", "人机验证站点密钥"),
				Help:  plugin.T("The public key from the captcha provider.", "人机验证服务给的公开密钥（site key）。")},
			{Key: "captcha_secret", Type: plugin.TypeSecret, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Captcha secret key", "人机验证服务端密钥"),
				Help:  plugin.T("Used by the relay to check answers with the captcha provider.", "中转用它向人机验证服务核对结果（secret key）。")},
		}}
}

// Init implements plugin.Initializer.
func (p *Plugin) Init(_ context.Context, h plugin.Host) error {
	if p.acc == nil {
		ah, ok := h.(AccountsHost)
		if !ok || ah.SignupAccounts() == nil {
			return errors.New("signup: this relay cannot create accounts from plugins")
		}
		p.acc = ah.SignupAccounts()
	}
	if p.kv == nil {
		p.kv = h.KV()
	}
	if l, err := ledger.From(h); err == nil {
		p.ops = l
	}
	if ch, ok := h.(clientIPHost); ok {
		p.clientIP = ch.ClientIP
	}
	p.settings = h.Settings()
	if p.http == nil {
		p.http = h.HTTPClient()
	}
	return nil
}

func (p *Plugin) captcha() captcha.Config {
	return captcha.Config{Provider: p.get("captcha_provider").String(), SiteKey: p.get("captcha_site_key").String(),
		Secret: p.get("captcha_secret").String(), Endpoint: p.captchaEndpoint}
}

// Routes implements plugin.Admin. Both routes are public.
func (p *Plugin) Routes() []plugin.AdminRoute {
	return []plugin.AdminRoute{
		{Method: http.MethodGet, Path: "/config", Handler: p.getConfig, Public: true},
		{Method: http.MethodPost, Path: "/register", Handler: p.postRegister, Public: true},
	}
}

func (p *Plugin) get(key string) plugin.Value {
	if p.settings == nil {
		return plugin.Value{}
	}
	return p.settings.Get(key)
}

func (p *Plugin) domains() []string {
	var out []string
	for _, d := range p.get("allowed_domains").List() {
		if d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@")); d != "" {
			out = append(out, d)
		}
	}
	return out
}

func (p *Plugin) getConfig(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{"enabled": true, "invite_required": p.get("invite_required").Bool(),
		"allowed_domains": p.domains(), "min_password": MinPassword}
	if c := p.captcha(); c.Enabled() {
		out["captcha"] = map[string]any{"provider": c.Provider, "site_key": c.SiteKey}
	}
	ledger.WriteJSON(w, http.StatusOK, out)
}

func (p *Plugin) inviteOK(code string) bool {
	if !p.get("invite_required").Bool() {
		return true
	}
	ok := false
	for _, c := range strings.FieldsFunc(p.get("invite_codes").String(), func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
		if code != "" && subtle.ConstantTimeCompare([]byte(c), []byte(strings.TrimSpace(code))) == 1 {
			ok = true
		}
	}
	return ok
}

func (p *Plugin) postRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := int64(5)
	if v := p.get("per_ip_per_hour"); v.IsSet() {
		limit = v.Int()
	}
	key := "ip:" + p.clientIP(r) + ":" + p.now().UTC().Format("2006010215")
	n, err := p.kv.Incr(ctx, key, 1, time.Hour+time.Minute)
	if err != nil {
		ledger.WriteError(w, http.StatusServiceUnavailable, "unavailable", "sign-up is unavailable")
		return
	}
	if limit > 0 && n > limit {
		w.Header().Set("Retry-After", "3600")
		ledger.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many sign-ups from this address; try again later")
		return
	}
	var in struct {
		Email        string `json:"email"`
		Password     string `json:"password"`
		Name         string `json:"name"`
		InviteCode   string `json:"invite_code"`
		CaptchaToken string `json:"captcha_token"`
		// ReferralCode is the inviter's code from an invite link.
		ReferralCode string `json:"referral_code"`
	}
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	if c := p.captcha(); c.Wanted() {
		if c.Check() != nil {
			// Chosen but not set up: refuse rather than skip the check.
			ledger.WriteError(w, http.StatusServiceUnavailable, "captcha_unavailable", "sign-up is misconfigured; ask an admin to fix the captcha")
			return
		}
		switch err := captcha.Verify(ctx, p.http, c, in.CaptchaToken, p.clientIP(r)); {
		case errors.Is(err, captcha.ErrMissing):
			ledger.WriteError(w, http.StatusForbidden, "captcha_required", "complete the captcha to continue")
			return
		case errors.Is(err, captcha.ErrFailed):
			ledger.WriteError(w, http.StatusForbidden, "captcha_failed", "the captcha was not accepted; try again")
			return
		case err != nil:
			ledger.WriteError(w, http.StatusServiceUnavailable, "captcha_unavailable", "the captcha service cannot be reached; try again later")
			return
		}
	}
	addr, err := mail.ParseAddress(strings.TrimSpace(in.Email))
	if err != nil || addr.Address != strings.TrimSpace(in.Email) {
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "a valid email address is required")
		return
	}
	email := strings.ToLower(addr.Address)
	domain := email[strings.LastIndexByte(email, '@')+1:]
	if ds := p.domains(); len(ds) > 0 && !slices.Contains(ds, domain) {
		ledger.WriteError(w, http.StatusForbidden, "domain_not_allowed", "sign-up is not open to this email domain")
		return
	}
	if len(in.Password) < MinPassword {
		ledger.WriteError(w, http.StatusBadRequest, "invalid_request", "the password must be at least 10 characters")
		return
	}
	if !p.inviteOK(in.InviteCode) {
		ledger.WriteError(w, http.StatusForbidden, "invite_required", "a valid invite code is required")
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = email[:strings.IndexByte(email, '@')]
	}
	id, err := p.acc.CreateUser(ctx, email, name, in.Password, p.get("default_group").String())
	if errors.Is(err, ErrExists) {
		ledger.WriteError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		ledger.WriteError(w, http.StatusInternalServerError, "internal", "could not create the account")
		return
	}
	if code := strings.TrimSpace(in.ReferralCode); code != "" && p.ops != nil {
		// An unknown or own code only records nothing: the account exists.
		_ = p.ops.Refer(context.WithoutCancel(ctx), code, id)
	}
	ledger.WriteJSON(w, http.StatusCreated, map[string]any{"user_id": id, "email": email})
}
