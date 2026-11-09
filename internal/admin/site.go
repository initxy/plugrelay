package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/captcha"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// siteKV is the plugin-KV namespace holding site settings.
const (
	siteKV  = "core.site"
	siteKey = "settings"
)

// Site setting keys. The exported ones are also written by other
// commands (plugrelay init) and read by the app.
const (
	SettingListenPublic = "network.listen_public"
	SettingPublicURL    = "site.public_base_url"
	SettingLocalOwner   = "security.local_owner_login"
	settingName         = "site.name"
	settingCurrency     = "site.currency"
	settingLogo         = "site.logo_url"
	settingAccent       = "site.accent"
	settingShowNames    = "site.show_upstream_names"
	settingTimezone     = "site.timezone"
	settingAdminToken   = "security.admin_token"
	settingSessionHours = "security.session_ttl_hours"
	// settingPasswordLogin off refuses password sign-in while a single
	// sign-on provider is offered.
	settingPasswordLogin = "security.password_login"
)

// Site is the stored site settings.
type Site map[string]json.RawMessage

// LoadSite reads the stored site settings.
func LoadSite(ctx context.Context, st store.Store) (Site, error) {
	b, ok, err := st.KV(siteKV).Get(ctx, siteKey)
	if err != nil || !ok || len(b) == 0 {
		return Site{}, err
	}
	s := Site{}
	if err := json.Unmarshal(b, &s); err != nil {
		return Site{}, fmt.Errorf("site settings: %w", err)
	}
	return s, nil
}

func saveSite(ctx context.Context, st store.Store, s Site) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return st.KV(siteKV).Put(ctx, siteKey, b, 0)
}

// String returns a string setting ("" when unset).
func (s Site) String(key string) string {
	var v string
	_ = json.Unmarshal(s[key], &v)
	return v
}

// Bool returns a bool setting and whether it is set.
func (s Site) Bool(key string) (bool, bool) {
	raw, ok := s[key]
	if !ok {
		return false, false
	}
	var v bool
	return v, json.Unmarshal(raw, &v) == nil
}

// PutSiteSetting stores one site setting outside a running relay (plugrelay
// init). It does not validate the value.
func PutSiteSetting(ctx context.Context, st store.Store, key string, v any) error {
	s, err := LoadSite(ctx, st)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s[key] = raw
	return saveSite(ctx, st, s)
}

func (s Site) adminTokenHash() string { return s.String(settingAdminToken) }

// name is the site name ("plugrelay" unless set).
func (s Site) name() string { return orDefault(s.String(settingName), "plugrelay") }

// siteName is the stored site name, "plugrelay" when unset or unreadable.
func (h *Handler) siteName(ctx context.Context) string {
	s, _ := h.site(ctx)
	return s.name()
}

func (h *Handler) site(ctx context.Context) (Site, error) { return LoadSite(ctx, h.d.Store) }

// localOwnerLogin reports whether the local-owner rule is on (default on).
func (h *Handler) localOwnerLogin(ctx context.Context) bool {
	s, err := h.site(ctx)
	if err != nil {
		return false
	}
	on, set := s.Bool(SettingLocalOwner)
	return on || !set
}

// showUpstreamNames reports whether non-admins see upstream names
// (default on).
func (s Site) showUpstreamNames() bool {
	on, set := s.Bool(settingShowNames)
	return on || !set
}

func (h *Handler) currency(ctx context.Context) string { return Currency(ctx, h.d.Store) }

// Currency is the site currency every price and balance is in (USD unless
// set).
func Currency(ctx context.Context, st store.Store) string {
	if s, err := LoadSite(ctx, st); err == nil {
		if c := s.String(settingCurrency); c != "" {
			return c
		}
	}
	return "USD"
}

// Timezone is the site's time zone, the one every day and month boundary
// is counted in (UTC unless set).
func Timezone(ctx context.Context, st store.Store) *time.Location {
	if s, err := LoadSite(ctx, st); err == nil {
		if name := s.String(settingTimezone); name != "" {
			if loc, ok := zones.Load(name); ok {
				return loc.(*time.Location)
			}
			if loc, err := time.LoadLocation(name); err == nil {
				zones.Store(name, loc)
				return loc
			}
		}
	}
	return time.UTC
}

// zones caches loaded locations: LoadLocation reads the zone database.
var zones sync.Map

// PublicURL is the site's public base URL; "" when unset.
func PublicURL(ctx context.Context, st store.Store) string {
	if s, err := LoadSite(ctx, st); err == nil {
		return strings.TrimRight(s.String(SettingPublicURL), "/")
	}
	return ""
}

func (h *Handler) sessionTTL(ctx context.Context) time.Duration {
	if s, err := h.site(ctx); err == nil {
		var n int
		if json.Unmarshal(s[settingSessionHours], &n) == nil && n > 0 {
			return time.Duration(n) * time.Hour
		}
	}
	return 7 * 24 * time.Hour
}

// settingDef declares one site setting.
type settingDef struct {
	key, section, typ string
	def               any
	enum              []string
	// enumText names enum values for people.
	enumText map[string]plugin.Text
	apply    string // live | restart | file
	title    plugin.Text
	help     plugin.Text
	// file returns the relay.yaml value of a file-owned setting.
	file func(c *config.Config) string
}

func siteDefs() []settingDef {
	T := plugin.T
	return []settingDef{
		{key: settingName, section: "branding", typ: "string", def: "plugrelay", apply: "live", title: T("Site name", "站点名称"),
			help: T("Shown in the console and on the sign-in page.", "显示在控制台和登录页上。")},
		{key: settingCurrency, section: "branding", typ: "string", def: "USD", apply: "live", title: T("Currency", "币种"),
			help: T("Three-letter code such as USD or CNY; every price and balance is in it.",
				"三个字母的代码，例如 USD、CNY；所有价格和余额都按它计。")},
		{key: settingLogo, section: "branding", typ: "string", def: "", apply: "live", title: T("Logo URL", "Logo 地址"),
			help: T("Image shown in place of the site name; empty shows the name.", "用来代替站点名称显示的图片；留空就显示名称。")},
		{key: settingAccent, section: "branding", typ: "string", def: "", apply: "live", title: T("Accent color", "主色"),
			help: T("A CSS color such as #0f766e; empty keeps the default.", "CSS 颜色，例如 #0f766e；留空用默认颜色。")},
		{key: settingShowNames, section: "branding", typ: "bool", def: true, apply: "live",
			title: T("Show upstream names to end users", "向终端用户显示上游名称"),
			help: T("Off, people who are not admins see neither upstream names nor which upstream served a request.",
				"关闭后，非管理员看不到上游名称，也看不到请求是哪个上游处理的。")},
		{key: SettingPublicURL, section: "general", typ: "string", def: "", apply: "live",
			title: T("Public base URL", "对外地址"),
			help: T("The address clients and browsers reach the relay at, such as https://relay.example.com; used in setup snippets, sign-in redirects and payment callbacks. Empty uses the address of each request.",
				"客户端和浏览器访问中转用的地址，例如 https://relay.example.com；用在配置片段、登录跳转和支付回调里。留空就用每次请求访问的地址。")},
		{key: settingTimezone, section: "general", typ: "string", def: "UTC", apply: "live",
			title: T("Time zone", "时区"),
			help: T("An IANA zone name such as Asia/Shanghai; daily and monthly budgets, daily check-ins and times in notifications follow it.",
				"填 IANA 时区名，例如 Asia/Shanghai；每日、每月额度和每日签到都按这里的零点换天，通知里的时间也按它显示。")},
		{key: "listen", section: "network", typ: "string", apply: "file", title: T("Listen address", "监听地址"),
			help: T("Set in relay.yaml.", "在 relay.yaml 里设置。"),
			file: func(c *config.Config) string { return c.Listen }},
		{key: SettingListenPublic, section: "network", typ: "bool", def: false, apply: "restart",
			title: T("Listen on all interfaces", "对外监听所有网卡"),
			help:  T("Lets other machines reach the relay; takes effect after a restart.", "让其他机器也能访问中转；重启后生效。")},
		{key: "store.driver", section: "network", typ: "string", apply: "file", title: T("Database", "数据库"),
			help: T("Set in relay.yaml.", "在 relay.yaml 里设置。"),
			file: func(c *config.Config) string { return c.Store.Driver }},
		{key: "state.driver", section: "network", typ: "string", apply: "file", title: T("Shared state backend", "共享状态存储"),
			help: T("Set in relay.yaml.", "在 relay.yaml 里设置。"),
			file: func(c *config.Config) string { return c.State.Driver }},
		{key: settingAdminToken, section: "security", typ: "secret", apply: "live",
			title: T("Admin API token", "管理 API 令牌"),
			help: T("For scripts calling the admin API with Authorization: Bearer; at least 16 characters, stored hashed.",
				"给脚本调用管理接口用（Authorization: Bearer）；至少 16 个字符，只保存哈希。")},
		{key: settingSessionHours, section: "security", typ: "int", def: 168, apply: "live",
			title: T("Console session lifetime (hours)", "控制台登录有效期（小时）"),
			help:  T("How long a console sign-in lasts.", "登录控制台后多久需要重新登录。")},
		{key: SettingLocalOwner, section: "security", typ: "bool", def: true, apply: "live",
			title: T("No sign-in for the console on this machine while the relay listens on loopback only",
				"只监听本机时，本机打开控制台无需登录"),
			help: T("Only works while the relay listens on this machine alone; turn it off on a shared machine.",
				"只在中转只监听本机时生效；多人共用的机器上请关掉。")},
		{key: settingPasswordLogin, section: "security", typ: "bool", def: true, apply: "live",
			title: T("Allow password sign-in", "允许密码登录"),
			help: T("Off, people sign in only with single sign-on. It can be turned off only by someone who has signed in with single sign-on, and comes back on by itself while single sign-on is off or has no provider.",
				"关闭后只能用单点登录。只有用单点登录登录过的人才能关闭；单点登录关闭或没有可用的登录方式时，密码登录会自动恢复。")},
		{key: settingRequire2FA, section: "security", typ: "bool", def: false, apply: "live",
			title: T("Require two-step sign-in for admins", "管理员必须开启两步验证"),
			help: T("Admins without it are asked to set it up and lose admin pages after the grace period.",
				"没开启的管理员会被要求设置，过了宽限期就不能使用管理页面。")},
		{key: setting2FAGraceDays, section: "security", typ: "int", def: 7, apply: "live",
			title: T("Days admins get to set up two-step sign-in", "管理员设置两步验证的宽限天数"),
			help:  T("Counted from when the rule was turned on.", "从开启这条规则时开始算。")},
		{key: settingRequire2FASSO, section: "security", typ: "bool", def: false, apply: "live",
			title: T("Ask single sign-on users for their second step too", "单点登录用户也要做第二步验证"),
			help:  T("Off, signing in with single sign-on is enough.", "关闭时，用单点登录登录就够了。")},
		{key: settingLoginCaptcha, section: "security", typ: "int", def: 0, apply: "live",
			title: T("Captcha on password sign-in after this many failures (0 = off)", "密码登录失败几次后要求人机验证（0 = 不要求）"),
			help:  T("Counted per network address; needs a captcha provider below.", "按来源网络地址计数；需要配好下面的人机验证服务。")},
		{key: settingCaptchaProvider, section: "security", typ: "string", def: captcha.Off, enum: captcha.Providers, apply: "live",
			title: T("Captcha provider", "人机验证服务"),
			help:  T("Cloudflare Turnstile or hCaptcha.", "可选 Cloudflare Turnstile 或 hCaptcha。"),
			enumText: map[string]plugin.Text{captcha.Off: T("Off", "不用"), captcha.Turnstile: T("Cloudflare Turnstile", "Cloudflare Turnstile"),
				captcha.HCaptcha: T("hCaptcha", "hCaptcha")}},
		{key: settingCaptchaSiteKey, section: "security", typ: "string", def: "", apply: "live",
			title: T("Captcha site key", "人机验证站点密钥（site key）"),
			help:  T("The public key from the captcha provider.", "人机验证服务给的公开密钥。")},
		{key: settingCaptchaSecret, section: "security", typ: "secret", apply: "live",
			title: T("Captcha secret key", "人机验证服务端密钥（secret key）"),
			help:  T("Used by the relay to check answers with the captcha provider.", "中转用它向人机验证服务核对结果。")},
	}
}

type settingItem struct {
	Key            string                 `json:"key"`
	Section        string                 `json:"section"`
	Type           string                 `json:"type"`
	Value          any                    `json:"value"`
	Default        any                    `json:"default,omitempty"`
	Enum           []string               `json:"enum,omitempty"`
	EnumLabels     map[string]plugin.Text `json:"enum_labels,omitempty"`
	Apply          string                 `json:"apply"`
	Source         string                 `json:"source"`
	PendingRestart bool                   `json:"pending_restart,omitempty"`
	Title          plugin.Text            `json:"title"`
	Help           plugin.Text            `json:"help"`
}

func (h *Handler) settingItems(s Site) []settingItem {
	var out []settingItem
	for _, d := range siteDefs() {
		it := settingItem{Key: d.key, Section: d.section, Type: d.typ, Default: d.def, Enum: d.enum, EnumLabels: d.enumText,
			Apply: d.apply, Source: "default", Value: d.def, Title: d.title, Help: d.help}
		if d.file != nil && h.d.Config != nil {
			it.Value, it.Source, it.Apply = d.file(h.d.Config), "file", "file"
			out = append(out, it)
			continue
		}
		if raw, ok := s[d.key]; ok {
			it.Source = "database"
			var v any
			_ = json.Unmarshal(raw, &v)
			it.Value = v
			if d.apply == "restart" {
				it.PendingRestart = !h.restartApplied(d.key, raw)
			}
		}
		if d.typ == "secret" {
			it.Value = nil
		}
		out = append(out, it)
	}
	return out
}

// restartApplied reports whether a restart-only value is what the process
// started with.
func (h *Handler) restartApplied(key string, raw json.RawMessage) bool {
	if key != SettingListenPublic {
		return true
	}
	addr, _ := h.d.Listen()
	var v bool
	_ = json.Unmarshal(raw, &v)
	return v == !isLoopbackAddr(addr)
}

func (h *Handler) getSettings(c *call) error {
	s, err := h.site(c.ctx)
	if err != nil {
		return err
	}
	return c.json(200, map[string]any{"items": h.settingItems(s)})
}

func (h *Handler) updateSettings(c *call) error {
	var body struct {
		Values map[string]json.RawMessage `json:"values"`
		// ConfirmCurrencyChange accepts changing site.currency while
		// balances exist; amounts are not converted.
		ConfirmCurrencyChange bool `json:"confirm_currency_change"`
	}
	if err := c.decode(&body); err != nil {
		return err
	}
	if body.Values == nil {
		return fieldInvalid("values", eRequired)
	}
	h.siteMu.Lock()
	defer h.siteMu.Unlock()
	s, err := h.site(c.ctx)
	if err != nil {
		return err
	}
	was := passwordLogin(s)
	defs := map[string]settingDef{}
	for _, d := range siteDefs() {
		defs[d.key] = d
	}
	var bad invalid
	restart := false
	var changes []change
	for key, raw := range body.Values {
		d, ok := defs[key]
		if !ok {
			bad.add("values."+key, eUnknownSetting)
			continue
		}
		if d.file != nil && h.d.Config != nil {
			return errConflict(eSettingFromFile)
		}
		if string(raw) == "null" {
			if key == settingCurrency && !body.ConfirmCurrencyChange && !sameCurrency(s, json.RawMessage(`"USD"`)) {
				if err := h.refuseCurrencyChange(c.ctx); err != nil {
					return err
				}
			}
			if _, had := s[key]; had {
				changes = append(changes, change{Field: key, From: redactSetting(d, s[key])})
				delete(s, key)
			}
			continue
		}
		val, err := validateSetting(d, raw)
		if err != nil {
			bad.addErr("values."+key, err)
			continue
		}
		if key == settingCaptchaSecret {
			if val, err = h.sealSecretSetting(val, captchaSecretAD); err != nil {
				return storeErr(err, objSetting)
			}
		}
		if key == settingCurrency && !body.ConfirmCurrencyChange && !sameCurrency(s, val) {
			if err := h.refuseCurrencyChange(c.ctx); err != nil {
				return err
			}
		}
		changes = append(changes, change{Field: key, From: redactSetting(d, s[key]), To: redactSetting(d, val)})
		s[key] = val
		restart = restart || d.apply == "restart"
	}
	if was && !passwordLogin(s) {
		cfg, err := loadSSO(c.ctx, h.d.Store)
		if err != nil {
			return err
		}
		if err := h.canDisablePasswords(c, cfg); err != nil {
			return err
		}
	}
	if err := h.captchaConfig(s).Check(); err != nil && loginCaptchaAfter(s) > 0 {
		bad.add("values."+settingCaptchaProvider, eCaptchaConfig.with("reason", strings.TrimPrefix(err.Error(), "captcha: ")))
	}
	if err := bad.err(); err != nil {
		return err
	}
	// The admin grace period counts from when the rule turned on.
	if on, _ := s.Bool(settingRequire2FA); !on {
		delete(s, settingRequire2FASince)
	} else if _, ok := s[settingRequire2FASince]; !ok {
		s[settingRequire2FASince], _ = json.Marshal(h.d.Now().UnixMilli())
	}
	if err := saveSite(c.ctx, h.d.Store, s); err != nil {
		return err
	}
	c.audit("settings.update", "settings", "site", "", changes)
	return c.json(200, map[string]any{"items": h.settingItems(s), "restart_required": restart})
}

// sealSecretSetting stores a secret setting sealed under the master key.
func (h *Handler) sealSecretSetting(val json.RawMessage, ad string) (json.RawMessage, error) {
	var v string
	_ = json.Unmarshal(val, &v)
	if h.d.Box == nil {
		return nil, store.ErrNoBox
	}
	sealed, err := h.d.Box.Seal([]byte(v), ad)
	if err != nil {
		return nil, err
	}
	return json.Marshal(sealed)
}

func sameCurrency(s Site, val json.RawMessage) bool {
	cur, next := s.String(settingCurrency), ""
	_ = json.Unmarshal(val, &next)
	return strings.EqualFold(orDefault(cur, "USD"), next)
}

// refuseCurrencyChange stops a site.currency change while prepaid
// balances exist: they are amounts in the old currency and stay as they
// are, so the admin must confirm.
func (h *Handler) refuseCurrencyChange(ctx context.Context) error {
	l, ok := store.LedgerOf(h.d.Store)
	if !ok {
		return nil
	}
	accts, err := l.ListAccounts(ctx)
	if err != nil {
		return err
	}
	for _, a := range accts {
		if a.BalanceMicro != 0 || a.HeldMicro != 0 {
			return errConflict(eBalancesExist)
		}
	}
	return nil
}

func redactSetting(d settingDef, raw json.RawMessage) any {
	if raw == nil {
		return nil
	}
	if d.typ == "secret" {
		return "********"
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

func validateSetting(d settingDef, raw json.RawMessage) (json.RawMessage, error) {
	switch d.typ {
	case "bool":
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return nil, eBool
		}
	case "int":
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 0 {
			return nil, eNonNegativeInt
		}
	case "string", "secret":
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return nil, eString
		}
		if len(d.enum) > 0 && !slices.Contains(d.enum, v) {
			return nil, oneOf(d.enum...)
		}
		switch d.key {
		case settingCurrency:
			if len(v) != 3 {
				return nil, eCurrencyCode
			}
			raw, _ = json.Marshal(strings.ToUpper(v))
		case SettingPublicURL:
			if u, err := url.Parse(v); v != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
				return nil, eURLHTTP
			}
		case settingTimezone:
			if _, err := time.LoadLocation(v); v == "" || err != nil {
				return nil, eTimezone
			}
		case settingAdminToken:
			if len(v) < 16 {
				return nil, eMinLength.with("min", 16)
			}
			raw, _ = json.Marshal(secret.HashKey(v))
		}
	}
	return raw, nil
}
