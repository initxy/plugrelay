package admin

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// errText is how one admin API error is worded: a stable code clients
// branch on and translate (the console's dictionary key is
// "apiError.<code>"), and the English text with {name} placeholders that
// with() fills from params.
type errText struct{ code, en string }

// errTexts holds every code the admin API can answer with, and its English
// text. TestErrorCodesTranslated holds the console dictionary to it.
var errTexts = map[string]string{}

func text(code, en string) errText {
	if _, dup := errTexts[code]; dup {
		panic("admin: duplicate error code " + code)
	}
	errTexts[code] = en
	return errText{code, en}
}

// errValue is a param value the console translates too (dictionary key
// "apiError.<param>.<id>"), such as the object a not-found error names.
type errValue struct{ param, id, en string }

// errValues holds every translated param value by "<param>.<id>".
var errValues = map[string]string{}

func value(param, id, en string) errValue {
	key := param + "." + id
	if _, dup := errValues[key]; dup {
		panic("admin: duplicate error value " + key)
	}
	errValues[key] = en
	return errValue{param, id, en}
}

// wording is an error's code with its params filled in. It and errText
// are errors themselves, so validators can return one for a field.
type wording struct {
	code    string
	params  map[string]any
	message string
}

func (w wording) Error() string { return w.message }
func (t errText) Error() string { return t.en }

// worded is an errText (no params) or a wording.
type worded interface{ wording() wording }

func (t errText) wording() wording { return t.with() }
func (w wording) wording() wording { return w }

var placeholderRe = regexp.MustCompile(`\{(\w+)\}`)

// with fills the placeholders from name/value pairs. A value is a string,
// a number or an errValue (sent as its id, written in English as its en).
func (t errText) with(kv ...any) wording {
	if len(kv)%2 != 0 {
		panic("admin: odd params for " + t.code)
	}
	var params map[string]any
	english := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		name := kv[i].(string)
		if params == nil {
			params = map[string]any{}
		}
		switch v := kv[i+1].(type) {
		case errValue:
			params[name], english[name] = v.id, v.en
		case int, int64, float64:
			params[name], english[name] = v, fmt.Sprint(v)
		default:
			s := fmt.Sprint(v)
			params[name], english[name] = s, s
		}
	}
	msg := placeholderRe.ReplaceAllStringFunc(t.en, func(m string) string {
		if s, ok := english[m[1:len(m)-1]]; ok {
			return s
		}
		return m
	})
	return wording{code: t.code, params: params, message: msg}
}

// Objects errors name; the console translates them.
var (
	objAuthenticator = value("object", "authenticator", "authenticator app")
	objCredential    = value("object", "credential", "credential")
	objExtension     = value("object", "extension", "extension")
	objFeature       = value("object", "feature", "feature")
	objGroup         = value("object", "group", "group")
	objIdentity      = value("object", "identity", "linked sign-in")
	objKey           = value("object", "key", "key")
	objMCPServer     = value("object", "mcp_server", "MCP server")
	objModel         = value("object", "model", "model")
	objNotifyChannel = value("object", "notify_channel", "notification channel")
	objPasskey       = value("object", "passkey", "passkey")
	objPlugin        = value("object", "plugin", "plugin")
	objPreset        = value("object", "preset", "preset")
	objPriceRule     = value("object", "price_rule", "pricing rule")
	objProvider      = value("object", "provider", "single sign-on provider")
	objRequest       = value("object", "request", "request")
	objRoute         = value("object", "route", "route")
	objSetting       = value("object", "setting", "setting")
	objUpstream      = value("object", "upstream", "upstream")
	objUser          = value("object", "user", "user")
)

// Roles and settings layers errors name.
var (
	roleAdmin  = value("role", "admin", "admin")
	roleMember = value("role", "member", "member")

	scopeGroup = value("scope", "group", "group")
	scopeUser  = value("scope", "user", "user")
	scopeKey   = value("scope", "key", "key")
)

// scopeValue names a record-owned settings layer.
func scopeValue(s plugin.Scope) errValue {
	switch s {
	case plugin.Group:
		return scopeGroup
	case plugin.User:
		return scopeUser
	}
	return scopeKey
}

// ---------------------------------------------------------------- codes

// Signing in and permissions.
var (
	eNotLoggedIn        = text("not_logged_in", "not logged in")
	eSessionExpired     = text("session_expired", "session expired")
	eAdminRequired      = text("admin_required", "admin role required")
	eRoleRequired       = text("role_required", "{role} role required")
	eUserRequired       = text("user_required", "sign in with a user account; an admin token has no user")
	ePlatformKey        = text("platform_key_refused", "platform keys are not accepted here; use an admin token")
	eAdminTokenInvalid  = text("admin_token_invalid", "invalid admin token")
	eSetupRequired      = text("setup_required", "finish first-run setup first")
	eSetupDone          = text("setup_done", "setup has already been completed")
	eSetupLoopback      = text("setup_loopback_only", "this relay listens on loopback only: open first-run setup at localhost or 127.0.0.1, or run plugrelay init")
	eCSRF               = text("csrf", "missing or wrong {header} header")
	eCrossOrigin        = text("cross_origin", "cross-origin request refused")
	eRateLimited        = text("rate_limited", "too many attempts; try again later")
	eWrongPassword      = text("wrong_password", "wrong username or password")
	ePasswordLoginOff   = text("password_login_disabled", "password sign-in is turned off; use single sign-on")
	eCaptchaRequired    = text("captcha_required", "complete the captcha to continue")
	eCaptchaFailed      = text("captcha_failed", "the captcha was not accepted; try again")
	eCaptchaUnavailable = text("captcha_unavailable", "the captcha cannot run right now; try again later, or ask an admin to check its settings")
)

// Two-step sign-in and passkeys.
var (
	eMFAUnsupported    = text("mfa_unsupported", "this store does not support two-step sign-in")
	eMFAEnrollment     = text(codeMFAEnrollment, "set up two-step sign-in (an authenticator app or a passkey) on your security page first")
	eMFANeedsUser      = text("mfa_needs_user", "sign in to the console with a user account to manage two-step sign-in")
	eMFAExpired        = text("mfa_expired", "the sign-in timed out; start again")
	eMFAInvalid        = text("mfa_invalid", "wrong or already used code")
	eMFAOffFirst       = text("mfa_off", "turn on two-step sign-in first")
	eReauthRequired    = text("reauth_required", "confirm it is you (password, authenticator code or passkey) and try again")
	eReauthFailed      = text("reauth_failed", "that did not match; try again")
	eTOTPSetupExpired  = text("totp_setup_expired", "start the authenticator setup again")
	eTOTPMismatch      = text("totp_mismatch", "the code does not match; check the device clock and try the current code")
	ePublicURLMissing  = text("public_url_missing", "set the public URL in the site settings first")
	ePublicURLInvalid  = text("public_url_invalid", "the public URL is not a valid URL")
	eNoPasskey         = text("no_passkey", "no passkey is registered for this account")
	ePasskeyLoginFail  = text("passkey_invalid", "this passkey could not sign you in")
	ePasskeyRejected   = text("passkey_rejected", "the passkey was not accepted: {reason}")
	ePasskeyIDMismatch = text("passkey_id_mismatch", "credential id does not match the authenticator data")
	eBadEncoding       = text("bad_encoding", "bad encoding")
)

// Store results and generic refusals.
var (
	eNotFound         = text("not_found", "{object} not found")
	eConflict         = text("conflict", "{object} already exists or conflicts with existing data")
	eNoMasterKey      = text("no_master_key", "no master key configured; secrets cannot be stored")
	eInternal         = text("internal", "internal error")
	eNoSuchEndpoint   = text("endpoint_not_found", "no such endpoint")
	eFeatureNotFound  = text("feature_unavailable", "{object} not found or not enabled")
	eMCPOff           = text("mcp_off", "the MCP gateway is not running")
	eNotifyOff        = text("notifications_off", "turn on the notifications feature first")
	eNoPriceMatch     = text("no_price_match", "models.dev has no price for this model")
	ePublicPagesOff   = text("public_pages_off", "public pages are turned off")
	eKeyUnusable      = text("key_unusable", "this key is disabled or expired")
	eBodyUnreadable   = text("body_unreadable", "cannot read request body")
	eBodyTooLarge     = text("body_too_large", "request body too large")
	eInvalidJSON      = text("invalid_json", "invalid JSON: {reason}")
	eInvalid          = text("invalid", "{count} fields are invalid")
	eChangeCannotRun  = text("change_cannot_run", "this change cannot run: {reason}")
	eUpstreamFailed   = text("upstream_failed", "the upstream answered with an error: {reason}")
	eFeatureAlwaysOn  = text("feature_always_on", "this feature always runs and cannot be turned off")
	eApproveFirst     = text("approve_first", "approve these first: {permissions}")
	eSettingFromFile  = text("setting_from_file", "this setting comes from relay.yaml and cannot be changed here")
	eBalancesExist    = text("balances_exist", "prepaid balances exist in the current currency and are not converted; send confirm_currency_change: true to change it anyway")
	eBillingCannotRun = text("billing_cannot_enable", "cannot turn on billing: {reason}")
	eLastAdminDemote  = text("last_admin_demote", "the last admin cannot be demoted or disabled")
	eLastAdminDelete  = text("last_admin_delete", "the last admin cannot be deleted")
	eUserDelBlocked   = text("user_delete_blocked", "the user cannot be deleted yet: {reason}")
	eGroupHasMembers  = text("group_has_members", "the group still has members")
	eGroupHasKeys     = text("group_has_keys", "keys still belong to the group")
	eSSOEnableFirst   = text("sso_enable_provider_first", "enable a single sign-on provider first")
	eSSOSignInFirst   = text("sso_sign_in_first", "sign in once with single sign-on yourself before turning password sign-in off")
	eSSOKeepProvider  = text("sso_keep_provider", "password sign-in is off; keep at least one provider enabled or turn password sign-in back on")
	eProviderExists   = text("provider_exists", "a provider with id {id} already exists")
	eMCPServerExists  = text("mcp_server_exists", "an MCP server with this id already exists")
)

// Field values.
var (
	eRequired        = text("required", "required")
	eNotEmpty        = text("not_empty", "must not be empty")
	eNotNegative     = text("not_negative", "must not be negative")
	eLengthRange     = text("length_range", "must be {min} to {max} characters")
	eMinLength       = text("min_length", "must be at least {min} characters")
	eNumberRange     = text("number_range", "must be between {min} and {max}")
	eOneOf           = text("one_of", "must be one of: {values}")
	eNoSuch          = text("no_such", "no such {object}")
	eNameTaken       = text("name_taken", "a {object} with this name exists")
	eAlreadyTaken    = text("already_taken", "already taken")
	eInvalidValue    = text("invalid_value", "invalid")
	eValueRejected   = text("value_rejected", "not accepted: {reason}")
	eBool            = text("want_bool", "must be true or false")
	eNonNegativeInt  = text("want_non_negative_int", "must be a whole number of 0 or more")
	eString          = text("want_string", "must be text")
	eCurrencyCode    = text("currency_code", "must be a 3-letter currency code")
	eTimezone        = text("timezone", "must be an IANA time zone such as Asia/Shanghai")
	eURLHTTP         = text("url_http", "must be an absolute http(s) URL")
	eURLProxy        = text("url_proxy", "want an http, https, socks5 or socks5h URL")
	eURLAbsolute     = text("url_absolute", "must be an absolute URL")
	eURLPlain        = text("url_no_query", "must be an absolute URL without query or fragment")
	eURLHTTPS        = text("url_https", "must use https")
	eURLNoCreds      = text("url_no_credentials", "must be an http(s) URL without credentials")
	eEmailAddress    = text("email_address", "must be an email address")
	eDateTime        = text("date_time", "want an RFC 3339 date-time")
	eFromAfterTo     = text("from_after_to", "must be before the end of the range")
	eNeedsTargetType = text("needs_target_type", "needs a target type")
	eCursor          = text("cursor_invalid", "invalid cursor")
	eIDFormat        = text("id_format", "use 1-32 lowercase letters, digits or dashes")
	eImmutable       = text("immutable", "cannot be changed; create a new one instead")
	eModelRename     = text("model_rename", "cannot be renamed; create a preset instead")
	eListedTwice     = text("listed_twice", "listed twice")
	ePresetSelf      = text("preset_lists_itself", "a preset cannot list itself")
	ePresetEmpty     = text("preset_empty", "a preset needs at least one model")
	eIPOrCIDR        = text("ip_or_cidr", "not a valid address or CIDR")
	eUnknownClient   = text("unknown_client", "unknown client")
	eAdminOnlyExcept = text("admin_only_exceptions", "only admins may set exceptions")
	eOwnKeysOnly     = text("own_keys_only", "you may only create keys for yourself")
	eAdminOnlyMove   = text("admin_only_move_group", "only admins may move keys between groups")
	ePersonalOwner   = text("personal_needs_owner", "personal credentials need an owner")
	eBatchDiscount   = text("batch_discount_range", "must be at least 0 and below 1")
	ePriceUnit       = text("price_unit_unknown", "unknown unit (want image, image:<size>[:<quality>], audio_second, 1k_chars or search_unit)")
	eGlob            = text("glob_invalid", "not a valid glob")
	eUnknownManifest = text("unknown_manifest", "unknown manifest")
	eProviderNeeded  = text("provider_required", "provider or manifest is required")
	eReservedKey     = text("reserved_key", "reserved key")
	eHeaderRefused   = text("header_refused", "credential, framing, hop-by-hop and client-address headers cannot be set")
	eHeaderInvalid   = text("header_invalid", "invalid header: {reason}")
	eSecretReenter   = text("secret_reenter", "the server address changed: enter this secret again, or clear it")
	eInterval        = text("interval", "want an interval like 15m")
	eQuotaProbe      = text("quota_probe_interval", "want an interval of at least 1m, like 15m, or off (0)")
	eUpstreamConfig  = text("upstream_config", "the upstream configuration is not usable: {reason}")
	eMultiplier      = text("multiplier_range", "must be greater than 0 and at most 1000")
	eEmailIsLogin    = text("email_is_login", "the login name is the email; set username instead")
	eLoginImmutable  = text("login_immutable", "the login name cannot be changed")
	eDefaultRole     = text("default_role_admin", "must be user or member; promote admins by hand")
	eTenantRequired  = text("tenant_required", "required: the directory (tenant) id, a verified domain, or organizations")
	eTenantInvalid   = text("tenant_invalid", "must be a tenant id or domain")
	eDomainInvalid   = text("domain_invalid", "must be a domain such as example.com")
	eTenantsForTrust = text("tenants_required_trust_email", "required when trusting emails: any tenant's admin can type any contact email")
	eRuleOneOf       = text("rule_domain_or_claim", "set either a domain or a claim")
	eRuleClaimValue  = text("rule_claim_value", "required with a claim")
	eClaimEditable   = text("claim_user_editable", "users can edit this claim; use a domain rule")
	eClaimEditableT  = text("claim_user_editable_tenant", "users can edit this claim; use a domain rule or the tenant id")
	eRecordOwned     = text("setting_record_owned", "set on the {scope} itself")
	eNotASetting     = text("not_a_setting", "not a setting here")
	eUnknownSetting  = text("unknown_setting", "unknown setting")
	eCaptchaConfig   = text("captcha_config", "{reason}; set the site key and secret, or turn the captcha off")
)

// oneOf words "must be one of: a, b, c".
func oneOf(values ...string) wording {
	return eOneOf.with("values", strings.Join(values, ", "))
}
