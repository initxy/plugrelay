package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/initxy/plugrelay/internal/features"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
)

// SSO configuration lives in plugin KV next to the site settings.
const (
	ssoKV        = "core.sso"
	ssoConfigKey = "config"
)

// SSO presets.
const (
	presetOIDC      = "oidc"
	presetGoogle    = "google"
	presetMicrosoft = "microsoft"
	presetKeycloak  = "keycloak"
	presetAuthentik = "authentik"
	presetGitHub    = "github"
	presetFeishu    = "feishu"
	presetLark      = "lark"
)

var ssoPresets = []string{presetOIDC, presetGoogle, presetMicrosoft, presetKeycloak, presetAuthentik, presetGitHub, presetFeishu, presetLark}

// Protocol families a preset speaks.
const (
	flowOIDC   = "oidc"
	flowGitHub = "github"
	flowFeishu = "feishu"
)

// oauthURLs are the fixed endpoints of a non-OIDC preset. A variable so
// tests can point them at a fake.
type oauthURLs struct{ auth, token, userinfo, emails string }

var presetURLs = map[string]oauthURLs{
	presetGitHub: {auth: "https://github.com/login/oauth/authorize", token: "https://github.com/login/oauth/access_token",
		userinfo: "https://api.github.com/user", emails: "https://api.github.com/user/emails"},
	// v2 token endpoints: the ones the vendors document for PKCE.
	presetFeishu: {auth: "https://accounts.feishu.cn/open-apis/authen/v1/authorize",
		token:    "https://open.feishu.cn/open-apis/authen/v2/oauth/token",
		userinfo: "https://open.feishu.cn/open-apis/authen/v1/user_info"},
	presetLark: {auth: "https://accounts.larksuite.com/open-apis/authen/v1/authorize",
		token:    "https://open.larksuite.com/open-apis/authen/v2/oauth/token",
		userinfo: "https://open.larksuite.com/open-apis/authen/v1/user_info"},
}

// microsoftAuthority is where Entra ID tenants live.
var microsoftAuthority = "https://login.microsoftonline.com"

// Entra tenant aliases that admit accounts from many tenants.
var entraMultiTenant = []string{"common", "organizations", "consumers"}

func presetFlow(preset string) string {
	switch preset {
	case presetGitHub:
		return flowGitHub
	case presetFeishu, presetLark:
		return flowFeishu
	}
	return flowOIDC
}

func presetName(preset string) string {
	switch preset {
	case presetGoogle:
		return "Google"
	case presetMicrosoft:
		return "Microsoft"
	case presetKeycloak:
		return "Keycloak"
	case presetAuthentik:
		return "Authentik"
	case presetGitHub:
		return "GitHub"
	case presetFeishu:
		return "Feishu"
	case presetLark:
		return "Lark"
	}
	return "SSO"
}

func presetScopes(preset string) []string {
	switch presetFlow(preset) {
	case flowGitHub:
		return []string{"read:user", "user:email"}
	case flowFeishu:
		return nil
	}
	return []string{"openid", "email", "profile"}
}

// ssoProvider is one configured sign-in provider.
type ssoProvider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Preset  string `json:"preset"`
	Enabled bool   `json:"enabled"`
	// Issuer is the OIDC issuer URL (oidc, keycloak, authentik).
	Issuer string `json:"issuer,omitempty"`
	// Tenant is the Entra tenant id or domain (microsoft).
	Tenant   string   `json:"tenant,omitempty"`
	ClientID string   `json:"client_id"`
	Secret   string   `json:"client_secret,omitempty"` // sealed at rest
	Scopes   []string `json:"scopes,omitempty"`

	AllowedDomains []string `json:"allowed_domains,omitempty"`
	// AllowedTenants restricts Entra tid / Feishu tenant_key.
	AllowedTenants []string       `json:"allowed_tenants,omitempty"`
	AutoCreate     bool           `json:"auto_create"`
	DefaultRole    store.Role     `json:"default_role,omitempty"`
	DefaultGroupID string         `json:"default_group_id,omitempty"`
	GroupRules     []ssoGroupRule `json:"group_rules,omitempty"`
	LinkByEmail    bool           `json:"link_by_email"`
	// LinkAdmins lets link_by_email reach admin accounts too.
	LinkAdmins bool `json:"link_admins,omitempty"`
	// TrustEmail treats the reported email as verified for IdPs that do
	// not say (Feishu/Lark contact emails are admin-entered). An explicit
	// email_verified=false still wins.
	TrustEmail bool `json:"trust_email"`
}

// claimRuleAllowed reports whether a group rule may match claim for a
// preset. GitHub profile fields and Feishu/Lark profile fields are typed
// in by the user, so only IdP-controlled claims may place people.
func claimRuleAllowed(preset, claim string) bool {
	switch presetFlow(preset) {
	case flowGitHub:
		return false
	case flowFeishu:
		return claim == "tenant_key"
	}
	return true
}

// identityKey is what a provider's subjects are scoped to: when it
// changes, the same subject may be someone else.
func identityKey(p ssoProvider) string {
	return strings.Join([]string{p.Preset, oidcIssuer(&p), p.ClientID}, "\x00")
}

// ssoGroupRule puts a new user in a group when an email domain or a claim
// matches. The first matching rule wins.
type ssoGroupRule struct {
	Domain  string `json:"domain,omitempty"`
	Claim   string `json:"claim,omitempty"`
	Value   string `json:"value,omitempty"`
	GroupID string `json:"group_id"`
}

type ssoConfig struct {
	Providers []ssoProvider `json:"providers,omitempty"`
	// on is the single sign-on feature: while it is off no provider is
	// offered, whatever its own switch says.
	on bool
}

func (c *ssoConfig) find(id string) (int, *ssoProvider) {
	for i := range c.Providers {
		if c.Providers[i].ID == id {
			return i, &c.Providers[i]
		}
	}
	return -1, nil
}

func (c *ssoConfig) enabled() []ssoProvider {
	if !c.on {
		return nil
	}
	var out []ssoProvider
	for _, p := range c.Providers {
		if p.Enabled {
			out = append(out, p)
		}
	}
	return out
}

func loadSSO(ctx context.Context, st store.Store) (*ssoConfig, error) {
	fs, err := st.ListFeatures(ctx)
	if err != nil {
		return nil, err
	}
	on := features.State{}
	for _, f := range fs {
		on[f.ID] = f.Enabled
	}
	c := &ssoConfig{}
	b, ok, err := st.KV(ssoKV).Get(ctx, ssoConfigKey)
	if err != nil {
		return nil, err
	}
	if ok && len(b) > 0 {
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("sso settings: %w", err)
		}
	}
	c.on = on.On(features.SSO)
	return c, nil
}

func saveSSO(ctx context.Context, st store.Store, c *ssoConfig) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return st.KV(ssoKV).Put(ctx, ssoConfigKey, b, 0)
}

// EnablePasswordLogin turns password sign-in back on. It is the CLI's
// break-glass path when single sign-on stops working.
func EnablePasswordLogin(ctx context.Context, st store.Store) (changed bool, err error) {
	s, err := LoadSite(ctx, st)
	if err != nil || passwordLogin(s) {
		return false, err
	}
	return true, PutSiteSetting(ctx, st, settingPasswordLogin, true)
}

// passwordLogin is the site setting (default on).
func passwordLogin(s Site) bool {
	on, set := s.Bool(settingPasswordLogin)
	return on || !set
}

// passwordLoginDisabled reports whether password sign-in is refused: the
// site setting is off and a provider is offered.
func (h *Handler) passwordLoginDisabled(ctx context.Context) (bool, error) {
	s, err := h.site(ctx)
	if err != nil {
		return false, err
	}
	if passwordLogin(s) {
		return false, nil
	}
	c, err := loadSSO(ctx, h.d.Store)
	if err != nil {
		return false, err
	}
	return len(c.enabled()) > 0, nil
}

func secretAD(providerID string) string { return "sso.client_secret:" + providerID }

// clientSecret opens the provider's sealed client secret.
func (h *Handler) clientSecret(p *ssoProvider) (string, error) {
	if p.Secret == "" {
		return "", nil
	}
	if h.d.Box == nil {
		return "", store.ErrNoBox
	}
	b, err := h.d.Box.Open(p.Secret, secretAD(p.ID))
	return string(b), err
}

// ---------------------------------------------------------------- validation

var ssoIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func lowerList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// checkIssuerURL accepts https URLs, and http only on loopback (tests and
// a local IdP).
func checkIssuerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return eURLPlain
	}
	return checkEndpointURL(raw)
}

func checkEndpointURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return eURLAbsolute
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return eURLHTTPS
}

func (h *Handler) validateProvider(ctx context.Context, p *ssoProvider, creating bool) error {
	var bad invalid
	p.ID = strings.TrimSpace(p.ID)
	if !ssoIDRe.MatchString(p.ID) {
		bad.add("id", eIDFormat)
	}
	if !slices.Contains(ssoPresets, p.Preset) {
		bad.add("preset", oneOf(ssoPresets...))
	}
	if p.Name = strings.TrimSpace(p.Name); p.Name == "" {
		p.Name = presetName(p.Preset)
	}
	p.ClientID = strings.TrimSpace(p.ClientID)
	if p.ClientID == "" {
		bad.add("client_id", eRequired)
	}
	if creating && p.Secret == "" {
		bad.add("client_secret", eRequired)
	}
	p.Issuer = strings.TrimSpace(p.Issuer)
	p.Tenant = strings.TrimSpace(p.Tenant)
	switch p.Preset {
	case presetOIDC, presetKeycloak, presetAuthentik:
		if p.Issuer == "" {
			bad.add("issuer", eRequired)
		} else if err := checkIssuerURL(p.Issuer); err != nil {
			bad.addErr("issuer", err)
		}
	case presetMicrosoft:
		p.Issuer = ""
		if p.Tenant == "" {
			bad.add("tenant", eTenantRequired)
		} else if strings.ContainsAny(p.Tenant, "/?#{} ") {
			bad.add("tenant", eTenantInvalid)
		}
	default:
		p.Issuer, p.Tenant = "", ""
	}
	var scopes []string
	for _, s := range p.Scopes {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	p.Scopes = scopes
	if presetFlow(p.Preset) == flowOIDC && len(p.Scopes) > 0 && !slices.Contains(p.Scopes, "openid") {
		p.Scopes = append([]string{"openid"}, p.Scopes...)
	}
	p.AllowedDomains = lowerList(p.AllowedDomains)
	for i, d := range p.AllowedDomains {
		if strings.ContainsAny(d, "@/ ") || !strings.Contains(d, ".") {
			bad.add(fmt.Sprintf("allowed_domains.%d", i), eDomainInvalid)
		}
	}
	p.AllowedTenants = lowerList(p.AllowedTenants)
	if p.TrustEmail && presetFlow(p.Preset) == flowFeishu && len(p.AllowedTenants) == 0 {
		bad.add("allowed_tenants", eTenantsForTrust)
	}
	if p.DefaultRole == "" {
		p.DefaultRole = store.RoleUser
	}
	if p.DefaultRole != store.RoleUser && p.DefaultRole != store.RoleMember {
		bad.add("default_role", eDefaultRole)
	}
	groups := map[string]bool{}
	if gs, err := h.d.Store.ListGroups(ctx); err == nil {
		for _, g := range gs {
			groups[g.ID] = true
		}
	} else {
		return err
	}
	if p.DefaultGroupID != "" && !groups[p.DefaultGroupID] {
		bad.add("default_group_id", eNoSuch.with("object", objGroup))
	}
	for i := range p.GroupRules {
		r := &p.GroupRules[i]
		r.Domain = strings.ToLower(strings.TrimSpace(r.Domain))
		r.Claim = strings.TrimSpace(r.Claim)
		path := fmt.Sprintf("group_rules.%d", i)
		if (r.Domain == "") == (r.Claim == "") {
			bad.add(path, eRuleOneOf)
		}
		if r.Claim != "" && r.Value == "" {
			bad.add(path+".value", eRuleClaimValue)
		}
		if r.Claim != "" && !claimRuleAllowed(p.Preset, r.Claim) {
			if presetFlow(p.Preset) == flowFeishu {
				bad.add(path+".claim", eClaimEditableT)
			} else {
				bad.add(path+".claim", eClaimEditable)
			}
		}
		if !groups[r.GroupID] {
			bad.add(path+".group_id", eNoSuch.with("object", objGroup))
		}
	}
	return bad.err()
}

// ---------------------------------------------------------------- admin API

// ssoProviderView is a provider as the API shows it: no secret.
type ssoProviderView struct {
	ssoProvider
	Secret          string `json:"client_secret,omitempty"`
	HasClientSecret bool   `json:"has_client_secret"`
	RedirectURI     string `json:"redirect_uri,omitempty"`
}

func (h *Handler) providerView(c *call, p ssoProvider) ssoProviderView {
	v := ssoProviderView{ssoProvider: p, HasClientSecret: p.Secret != ""}
	if v.Scopes == nil {
		v.Scopes = presetScopes(p.Preset)
	}
	if base, err := h.trustedBaseURL(c); err == nil {
		v.RedirectURI = ssoRedirectURI(base, p.ID)
	}
	return v
}

func (h *Handler) listSSOProviders(c *call) error {
	cfg, err := loadSSO(c.ctx, h.d.Store)
	if err != nil {
		return err
	}
	views := []ssoProviderView{}
	for _, p := range cfg.Providers {
		views = append(views, h.providerView(c, p))
	}
	out := map[string]any{"items": views, "presets": ssoPresets}
	if base, err := h.trustedBaseURL(c); err == nil {
		out["redirect_base"] = base + Prefix + "/auth/sso/"
	} else {
		out["redirect_problem"] = err.Error()
	}
	return c.json(200, out)
}

// canDisablePasswords refuses to turn password sign-in off unless SSO is
// shown to work for the caller: an enabled provider the caller has signed
// in with. An operator token always keeps working, so it may.
func (h *Handler) canDisablePasswords(c *call, cfg *ssoConfig) error {
	on := cfg.enabled()
	if len(on) == 0 {
		return errConflict(eSSOEnableFirst)
	}
	if c.who.user == nil {
		return nil
	}
	ids, err := h.d.Store.ListIdentities(c.ctx, c.who.user.ID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		for _, p := range on {
			if id.Provider == p.ID {
				return nil
			}
		}
	}
	return errConflict(eSSOSignInFirst)
}

// stillReachable refuses a change that would leave password sign-in off
// with no enabled provider.
func (h *Handler) stillReachable(ctx context.Context, cfg *ssoConfig) error {
	s, err := h.site(ctx)
	if err != nil {
		return err
	}
	if !passwordLogin(s) && cfg.on && len(cfg.enabled()) == 0 {
		return errConflict(eSSOKeepProvider)
	}
	return nil
}

func (h *Handler) sealProvider(p *ssoProvider) error {
	if p.Secret == "" || secret.IsSealed(p.Secret) {
		return nil
	}
	if h.d.Box == nil {
		return store.ErrNoBox
	}
	s, err := h.d.Box.Seal([]byte(p.Secret), secretAD(p.ID))
	if err != nil {
		return err
	}
	p.Secret = s
	return nil
}

func (h *Handler) createSSOProvider(c *call) error {
	var p ssoProvider
	p.Enabled = true
	if err := c.decode(&p); err != nil {
		return err
	}
	if secret.IsSealed(p.Secret) {
		return fieldInvalid("client_secret", eInvalidValue)
	}
	if err := h.validateProvider(c.ctx, &p, true); err != nil {
		return err
	}
	h.ssoMu.Lock()
	defer h.ssoMu.Unlock()
	cfg, err := loadSSO(c.ctx, h.d.Store)
	if err != nil {
		return err
	}
	if _, old := cfg.find(p.ID); old != nil {
		return errConflict(eProviderExists.with("id", p.ID))
	}
	if err := h.sealProvider(&p); err != nil {
		return storeErr(err, objProvider)
	}
	cfg.Providers = append(cfg.Providers, p)
	if err := saveSSO(c.ctx, h.d.Store, cfg); err != nil {
		return err
	}
	c.audit("sso.provider.create", "sso_provider", p.ID, p.Name, []change{{Field: "preset", To: p.Preset}})
	return c.json(201, h.providerView(c, p))
}

func (h *Handler) updateSSOProvider(c *call) error {
	id := c.r.PathValue("id")
	h.ssoMu.Lock()
	defer h.ssoMu.Unlock()
	cfg, err := loadSSO(c.ctx, h.d.Store)
	if err != nil {
		return err
	}
	i, old := cfg.find(id)
	if old == nil {
		return errNotFound(eNotFound.with("object", objProvider))
	}
	var raw map[string]json.RawMessage
	if err := c.decode(&raw); err != nil {
		return err
	}
	p := *old
	p.Secret = ""
	// Lists in the body replace the stored ones; decoding into the stored
	// slices would merge elements and write through to old.
	for k, dst := range map[string]any{"scopes": &p.Scopes, "allowed_domains": &p.AllowedDomains,
		"allowed_tenants": &p.AllowedTenants, "group_rules": &p.GroupRules} {
		if _, ok := raw[k]; ok {
			switch d := dst.(type) {
			case *[]string:
				*d = nil
			case *[]ssoGroupRule:
				*d = nil
			}
		}
	}
	if b, _ := json.Marshal(raw); json.Unmarshal(b, &p) != nil {
		return errBadRequest(eInvalidJSON.with("reason", "wrong field types"))
	}
	if secret.IsSealed(p.Secret) {
		return fieldInvalid("client_secret", eInvalidValue)
	}
	p.ID = id
	newSecret := p.Secret != ""
	if !newSecret {
		p.Secret = old.Secret
	}
	if err := h.validateProvider(c.ctx, &p, false); err != nil {
		return err
	}
	if err := h.sealProvider(&p); err != nil {
		return storeErr(err, objProvider)
	}
	before := *old
	cfg.Providers[i] = p
	if err := h.stillReachable(c.ctx, cfg); err != nil {
		return err
	}
	if err := saveSSO(c.ctx, h.d.Store, cfg); err != nil {
		return err
	}
	changes := providerChanges(before, p, newSecret)
	// Subjects are only unique per issuer and client: links made under
	// the old ones must not sign a different person in.
	if identityKey(before) != identityKey(p) {
		n, err := h.dropIdentities(c.ctx, id)
		if err != nil {
			return err
		}
		changes = append(changes, change{Field: "identities_unlinked", To: n})
	}
	c.audit("sso.provider.update", "sso_provider", p.ID, p.Name, changes)
	h.sso.forget(id)
	return c.json(200, h.providerView(c, p))
}

// dropIdentities unlinks every identity of a provider.
func (h *Handler) dropIdentities(ctx context.Context, providerID string) (int, error) {
	ids, err := h.d.Store.ListIdentities(ctx, "")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, x := range ids {
		if x.Provider == providerID {
			if err := h.d.Store.DeleteIdentity(ctx, x.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

func providerChanges(a, b ssoProvider, newSecret bool) []change {
	var out []change
	ja, jb := map[string]any{}, map[string]any{}
	ba, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	_ = json.Unmarshal(ba, &ja)
	_ = json.Unmarshal(bb, &jb)
	delete(ja, "client_secret")
	delete(jb, "client_secret")
	keys := map[string]bool{}
	for k := range ja {
		keys[k] = true
	}
	for k := range jb {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	slices.Sort(sorted)
	for _, k := range sorted {
		x, _ := json.Marshal(ja[k])
		y, _ := json.Marshal(jb[k])
		if string(x) != string(y) {
			out = append(out, change{Field: k, From: ja[k], To: jb[k]})
		}
	}
	if newSecret {
		out = append(out, change{Field: "client_secret", To: "********"})
	}
	return out
}

func (h *Handler) deleteSSOProvider(c *call) error {
	id := c.r.PathValue("id")
	h.ssoMu.Lock()
	defer h.ssoMu.Unlock()
	cfg, err := loadSSO(c.ctx, h.d.Store)
	if err != nil {
		return err
	}
	i, p := cfg.find(id)
	if p == nil {
		return errNotFound(eNotFound.with("object", objProvider))
	}
	name := p.Name
	cfg.Providers = slices.Delete(cfg.Providers, i, i+1)
	if err := h.stillReachable(c.ctx, cfg); err != nil {
		return err
	}
	if err := saveSSO(c.ctx, h.d.Store, cfg); err != nil {
		return err
	}
	// A later provider reusing the id must not inherit these links.
	if _, err := h.dropIdentities(c.ctx, id); err != nil {
		return err
	}
	h.sso.forget(id)
	c.audit("sso.provider.delete", "sso_provider", id, name, nil)
	return c.noContent()
}

type identityView struct {
	store.Identity
	ProviderName string `json:"provider_name"`
}

func (h *Handler) listUserIdentities(c *call) error {
	uid := c.r.PathValue("id")
	if _, err := h.d.Store.GetUser(c.ctx, uid); err != nil {
		return storeErr(err, objUser)
	}
	ids, err := h.d.Store.ListIdentities(c.ctx, uid)
	if err != nil {
		return err
	}
	cfg, err := loadSSO(c.ctx, h.d.Store)
	if err != nil {
		return err
	}
	items := []identityView{}
	for _, x := range ids {
		name := x.Provider
		if _, p := cfg.find(x.Provider); p != nil {
			name = p.Name
		}
		items = append(items, identityView{Identity: x, ProviderName: name})
	}
	return c.json(200, map[string]any{"items": items})
}

func (h *Handler) deleteUserIdentity(c *call) error {
	uid, iid := c.r.PathValue("id"), c.r.PathValue("identity_id")
	ids, err := h.d.Store.ListIdentities(c.ctx, uid)
	if err != nil {
		return err
	}
	for _, x := range ids {
		if x.ID == iid {
			if err := h.d.Store.DeleteIdentity(c.ctx, iid); err != nil {
				return storeErr(err, objIdentity)
			}
			c.audit("user.identity.unlink", "user", uid, "", []change{{Field: "identity", From: x.Provider + ":" + x.Subject}})
			return c.noContent()
		}
	}
	return errNotFound(eNotFound.with("object", objIdentity))
}
