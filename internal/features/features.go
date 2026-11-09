// Package features is the registry of what an operator switches on and
// off. A feature is one switch and one form over built-in modules (plugins);
// the console shows its pages while it is on. Feature state is stored in the
// database (store.FeatureState); a module of a feature runs exactly when
// its feature is on. Core modules always run and belong to no feature.
// Plugins that are neither (external processes, plugins linked into a
// custom build) are extensions, switched one by one.
package features

import (
	"slices"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// Feature is one entry of the registry.
type Feature struct {
	ID          string
	Name        plugin.Text
	Description plugin.Text
	// Modules are the built-in plugins that run while the feature is on.
	Modules []string
	// Default is the state of a feature with nothing stored.
	Default bool
	// AlwaysOn features have no switch, only settings.
	AlwaysOn bool
	// Settings are the module settings the feature's form shows, at the
	// global layer; the first ones are the most important.
	Settings []Ref
	// Pages are the console pages the feature adds.
	Pages []Page
}

// Ref names one setting of one module.
type Ref struct {
	Module, Key string
}

// Page is a console page, by a stable id the console maps to a route.
type Page struct {
	ID string
	// Admin pages are for operators; the others for everyone else.
	Admin bool
}

// IDs of the features the core itself reads.
const (
	SSO         = "sso"
	MCP         = "mcp"
	PublicPages = "public_pages"
)

// Core are the built-in modules that always run, whatever is stored.
// Providers are not listed: they run whenever an upstream uses them.
var Core = []string{
	"auth.apikey",
	"middleware.access",
	"selector.routing",
	"sink.requestlog",
	"job.requestlog-retention",
	"job.modelsync",
	"job.quotaprobe",
}

func refs(module string, keys ...string) []Ref {
	out := make([]Ref, len(keys))
	for i, k := range keys {
		out[i] = Ref{module, k}
	}
	return out
}

func cat(lists ...[]Ref) []Ref { return slices.Concat(lists...) }

var budgets = []string{
	"key_daily_micro", "key_monthly_micro", "key_total_micro", "key_daily_tokens", "key_monthly_tokens",
	"user_daily_micro", "user_monthly_micro", "user_total_micro", "user_daily_tokens", "user_monthly_tokens",
	"group_daily_micro", "group_monthly_micro", "group_total_micro", "group_daily_tokens", "group_monthly_tokens",
}

var registry = []Feature{
	{
		ID:   "limits",
		Name: plugin.T("Rate limits & quotas", "限流与额度"),
		Description: plugin.T("Requests and tokens per minute, requests at once, spend and token budgets, and caps on output length and reasoning effort.",
			"限制每分钟请求数和 token 数、同时进行的请求数、花费和 token 额度，以及输出长度和思考强度的上限。"),
		Modules: []string{"middleware.ratelimit", "meter.quota", "middleware.param-policy"},
		Default: true,
		Settings: cat(refs("middleware.ratelimit", "rpm", "tpm", "concurrency", "subject"),
			refs("meter.quota", budgets...),
			refs("meter.quota", "usage_header", "reset_header"),
			refs("middleware.param-policy", "max_output_tokens", "max_reasoning_effort", "strip_params")),
	},
	{
		ID:   "routing",
		Name: plugin.T("Routing", "路由"),
		Description: plugin.T("Which upstream a request tries first, and how often upstreams are asked for their models and remaining quota.",
			"决定请求先发给哪个上游，以及多久向上游同步一次模型列表、查一次剩余额度。"),
		AlwaysOn: true,
		Settings: cat(refs("selector.routing", "strategy"), refs("job.modelsync", "interval"),
			refs("job.quotaprobe", "interval")),
	},
	{
		ID:   "request_log",
		Name: plugin.T("Request log", "请求日志"),
		Description: plugin.T("Every request with its upstream, tokens, cost and timing. Whether text is kept and how long entries stay.",
			"记录每个请求的上游、token、费用和耗时。这里决定是否保存正文、记录保留多久。"),
		AlwaysOn: true,
		Settings: cat(refs("sink.requestlog", "body_logging"), refs("job.requestlog-retention", "retention_days")),
	},
	{
		ID:   "content_safety",
		Name: plugin.T("Content safety", "内容安全"),
		Description: plugin.T("Find personal data, secrets, blocked words and blocked models or tools in requests and replies; refuse, mask or note them, optionally with a moderation service.",
			"在请求和回复里识别个人信息、密钥、敏感词以及禁用的模型和工具，可以拒绝、遮掉或只记录，也可以接内容审核服务。"),
		Modules: []string{"middleware.guardrails", "middleware.redact"},
		Settings: cat(refs("middleware.guardrails", "action", "detect", "deny_keywords", "deny_patterns", "check_response",
			"max_prompt_chars", "blocked_models", "blocked_tools", "moderation_upstream", "moderation_model",
			"moderation_fail", "max_moderated_events", "block_unscannable_audio", "stream_window"),
			refs("middleware.redact", "detect", "patterns")),
	},
	{
		ID:   "billing",
		Name: plugin.T("Billing", "计费"),
		Description: plugin.T("Prepaid balances, online top-up, redeem codes and subscription plans, for selling access; daily check-in, referral rewards, coupons and announcements each have their own switch below.",
			"预付余额、在线充值、兑换码和订阅套餐，用来对外售卖；每日签到、邀请返利、优惠券和公告在下面各有开关。"),
		Modules: []string{"meter.balance", "admin.payment", "admin.redeem", "admin.subscriptions", "admin.import-newapi",
			"admin.rewards", "admin.announcements"},
		Settings: cat(refs("meter.balance", "payer", "min_reserve_micro", "balance_header", "low_balance_micro", "hold_ttl", "exempt"),
			refs("admin.payment", "min_topup_micro", "max_topup_micro", "presets_micro",
				"stripe_secret_key", "stripe_webhook_secret", "stripe_subscription_webhook_secret", "stripe_product_name",
				"epay_url", "epay_pid", "epay_key", "epay_types", "epay_mode", "epay_product_name"),
			refs("admin.redeem", "failures_per_hour"),
			refs("admin.rewards", "checkin_enabled", "checkin_min_micro", "checkin_max_micro",
				"referral_enabled", "referral_mode", "referral_percent", "referral_fixed_micro", "referral_cap_micro"),
			refs("admin.payment", "coupons_enabled"),
			refs("admin.announcements", "enabled")),
		Pages: []Page{
			{"billing.balances", true}, {"billing.orders", true}, {"billing.codes", true}, {"billing.coupons", true},
			{"billing.plans", true}, {"billing.announcements", true}, {"billing.import", true},
			{"billing.wallet", false}, {"billing.plan", false},
		},
	},
	{
		ID:   "signup",
		Name: plugin.T("Self sign-up", "自助注册"),
		Description: plugin.T("Anyone can create an account with an email and password, optionally only with an invite code or from allowed email domains.",
			"任何人都能用邮箱和密码注册账号，可以要求邀请码或限定邮箱域名。"),
		Modules: []string{"admin.signup"},
		Settings: refs("admin.signup", "invite_required", "invite_codes", "allowed_domains", "default_group",
			"per_ip_per_hour", "captcha_provider", "captcha_site_key", "captcha_secret"),
	},
	{
		ID:   SSO,
		Name: plugin.T("Single sign-on", "单点登录"),
		Description: plugin.T("Sign in with GitHub, Google, Microsoft, Feishu or any OpenID Connect provider. While it is off, no provider is offered and password sign-in always works.",
			"用 GitHub、Google、Microsoft、飞书或任意 OpenID Connect 账号登录。关闭时不显示任何登录方式，密码登录始终可用。"),
		Pages: []Page{{"sso.providers", true}},
	},
	{
		ID:   MCP,
		Name: plugin.T("MCP gateway", "MCP 网关"),
		Description: plugin.T("Serve configured MCP servers to clients behind their platform key, with per-key tool access. While it is off, /mcp answers 404.",
			"把配置好的 MCP 服务通过平台密钥提供给客户端，可以按密钥控制能用哪些工具。关闭时 /mcp 返回 404。"),
		Settings: refs("middleware.access", "mcp_servers", "mcp_tools_allow", "mcp_tools_deny"),
		Pages:    []Page{{"mcp.servers", true}},
	},
	{
		ID:   "notifications",
		Name: plugin.T("Notifications", "通知"),
		Description: plugin.T("Alerts when a credential looks banned or runs low on quota, an upstream is paused or a budget is nearly used up, by email, Feishu, DingTalk, WeCom or webhook; a webhook can also receive every request record.",
			"凭据疑似被封、额度快用完、上游暂停或花费快到上限时发提醒，支持邮件、飞书、钉钉、企业微信和 Webhook；Webhook 还能接收每条请求记录。"),
		Modules:  []string{"job.notify", "sink.webhook"},
		Settings: refs("job.notify", "budget_percent", "quota_low_percent", "cooling_minutes", "repeat_hours"),
		Pages:    []Page{{"notifications.channels", true}},
	},
	{
		ID:   "price_sync",
		Name: plugin.T("Price sync", "价格同步"),
		Description: plugin.T("Checks models.dev for the official prices and context sizes of your models and suggests them on the prices tab. Prices entered by hand are never replaced; a model set to follow gets new prices by itself.",
			"定期从 models.dev 查模型的官方价格和上下文长度，在价格页给出修改建议。手填的价格不会被覆盖；设成「跟随」的模型会自动用新价格。"),
		Modules:  []string{"job.pricesync"},
		Settings: refs("job.pricesync", "usd_rate", "interval", "url"),
	},
	{
		ID:   PublicPages,
		Name: plugin.T("Public pages", "公开页面"),
		Description: plugin.T("A models-and-prices page and a status page anyone can open without signing in, linked from the sign-in page. Upstream names stay hidden unless end users may see them.",
			"不用登录也能打开的模型价格页和服务状态页，登录页上有入口。除非设置里允许终端用户看上游名称，否则不显示上游。"),
		Modules:  []string{"admin.public-pages"},
		Settings: refs("admin.public-pages", "price_group", "show_status", "per_ip_per_minute"),
	},
	{
		ID:          "metrics",
		Name:        plugin.T("Metrics export", "监控指标导出"),
		Description: plugin.T("Prometheus metrics on /metrics: success rate, latency, tokens, cache hits.", "在 /metrics 提供 Prometheus 指标：成功率、延迟、token、缓存命中。"),
		Modules:     []string{"sink.metrics"},
		Default:     true,
	},
}

// All returns every feature in display order.
func All() []Feature { return slices.Clone(registry) }

// Get returns the feature with id.
func Get(id string) (Feature, bool) {
	for _, f := range registry {
		if f.ID == id {
			return f, true
		}
	}
	return Feature{}, false
}

// Of returns the feature a module belongs to.
func Of(module string) (Feature, bool) {
	for _, f := range registry {
		if slices.Contains(f.Modules, module) {
			return f, true
		}
	}
	return Feature{}, false
}

// IsCore reports whether module always runs.
func IsCore(module string) bool { return slices.Contains(Core, module) }

// Builtin reports whether a plugin is part of the product: a core module or
// a module of a feature. Every other plugin is an extension.
func Builtin(module string) bool {
	_, ok := Of(module)
	return ok || IsCore(module)
}

// State is the stored on/off of features; features not in it are at their
// default.
type State map[string]bool

// On reports whether feature id is on.
func (s State) On(id string) bool {
	f, ok := Get(id)
	if !ok {
		return false
	}
	if f.AlwaysOn {
		return true
	}
	if on, ok := s[id]; ok {
		return on
	}
	return f.Default
}
