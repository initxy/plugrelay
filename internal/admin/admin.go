// Package admin implements plugrelay's public management API
// (api/openapi.yaml, served under /api/v1) and the plugin-owned APIs under
// /api/plugins/{id}/.
//
// The console uses nothing but this API. Browsers authenticate with a
// session cookie plus a double-submit CSRF header on unsafe methods;
// scripts send a Bearer admin token. With the local-owner rule on, while
// the relay listens on loopback only, loopback requests act as the local
// owner without a login. Every mutation is written to the audit log.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/mcp"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Prefix is where the management API is mounted.
const Prefix = "/api/v1"

// Runtime is what the admin API needs from the running relay beyond the
// store: live router state, the plugin set and calls that go through the
// pipeline or a provider.
type Runtime interface {
	// Live is the router's live state (hard rule 25: never a stored copy).
	Live(ctx context.Context) router.Live
	// RestoreCredential clears every live restriction on a credential.
	RestoreCredential(ctx context.Context, upstreamID, credentialID string) error

	// Plugins lists every installed plugin in run order.
	Plugins() []PluginEntry
	// CheckPlugin reports why storing pc would leave a plugin configuration
	// the relay cannot run (a FailClosed plugin losing a plugin it
	// requires, a plugin turned on refusing stored settings); nil when it
	// can. CheckFeature is the same for a feature state.
	CheckPlugin(ctx context.Context, pc *store.PluginConfig) error
	CheckFeature(ctx context.Context, f store.FeatureState) error
	// BeforeUserDelete runs the enabled plugins' plugin.UserDeleteHook.
	BeforeUserDelete(ctx context.Context, userID string) error
	// CheckSettings runs the enabled plugins' plugin.SettingsCheck.
	CheckSettings(ctx context.Context, pluginID string, scope plugin.Scope, values map[string]json.RawMessage) error
	// ReloadError is why the last configuration reload failed while the
	// relay keeps running the one before; "" once a reload applies.
	ReloadError() string
	// SettingsFor merges a plugin's settings for a caller.
	SettingsFor(pluginID string, c plugin.Caller) plugin.Settings

	// Manifests lists upstream manifests available to create from.
	Manifests(ctx context.Context) []ManifestInfo
	// Endpoints resolves an upstream's effective endpoints.
	Endpoints(u *store.Upstream) ([]plugin.Endpoint, error)

	// TestUpstream sends a tiny request to one upstream, once per
	// credential (or only the one asked for).
	TestUpstream(ctx context.Context, upstreamID string, req TestRequest) []TestResult
	// Playground streams one conversation through the relay as caller.
	Playground(ctx context.Context, caller plugin.Caller, req PlaygroundRequest, emit func(PlaygroundEvent))
	// TestKey sends a tiny request through the relay as the key's caller.
	TestKey(ctx context.Context, k *store.Key, protocol, model string) TestResult
	// RemoteModels asks an upstream which models it serves.
	RemoteModels(ctx context.Context, upstreamID string) ([]ir.ModelInfo, error)
	// SyncModels pulls model lists from upstreams (all when ids is empty).
	SyncModels(ctx context.Context, upstreamIDs []string) SyncResult
	// TestNotifyChannel sends a test message to a channel; on is false
	// while the notifications feature is off.
	TestNotifyChannel(ctx context.Context, c *store.NotifyChannel) (res NotifyTest, on bool)
}

// Deps wires the admin API.
type Deps struct {
	Store   store.Store
	Box     *secret.Box
	Config  *config.Config
	Runtime Runtime
	Version string
	// MCP is the MCP gateway whose servers the API manages; nil answers
	// its routes with 404.
	MCP *mcp.Gateway
	// AdminToken is an operator token from the environment; empty disables
	// it. Tokens can also be set in site settings.
	AdminToken string
	// Listen reports the address the relay listens on and whether that is
	// loopback only.
	Listen func() (addr string, loopback bool)
	// ClientIP resolves the client address for login rate limits and the
	// audit log, honouring trusted proxies; nil means the socket peer.
	// The local-owner check never uses it: it needs the real peer.
	ClientIP func(r *http.Request) string
	// HTTPClient is the core's direct, SSRF-guarded egress for calls of the
	// admin API's own (captcha verification, SSO); nil uses a plain client.
	HTTPClient func() *http.Client
	// State is the cluster's shared runtime state; with it, sign-in rate
	// limits count across all nodes. nil counts per node.
	State  plugin.SharedState
	Logger *slog.Logger
	Now    func() time.Time
}

// Handler serves the admin API and the plugins' HTTP routes.
type Handler struct {
	d      Deps
	mux    *http.ServeMux
	routes []RouteInfo
	// setupDone caches "an admin exists" once true.
	setupDone atomic.Bool
	logins    *loginLimiter
	ssoStarts *loginLimiter
	// publicCalls bounds calls to plugins' public routes per address.
	publicCalls *loginLimiter
	// publicLimit bounds loads of the public pages per address.
	publicLimit *loginLimiter
	publicCache publicCache
	siteMu      sync.Mutex
	ssoMu       sync.Mutex
	sso         ssoCache
	// captchaEndpoint overrides the siteverify URL (tests).
	captchaEndpoint string
}

// RouteInfo is one registered management operation.
type RouteInfo struct {
	Method string
	Path   string // OpenAPI form, relative to Prefix
	Access Access
}

// New returns a Handler.
func New(d Deps) *Handler {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.ClientIP == nil {
		d.ClientIP = peerIP
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Listen == nil {
		addr := ""
		if d.Config != nil {
			addr = d.Config.Listen
		}
		d.Listen = func() (string, bool) { return addr, isLoopbackAddr(addr) }
	}
	h := &Handler{d: d, mux: http.NewServeMux(), logins: newLoginLimiter().share(d.State, "login"),
		ssoStarts: newStartLimiter().share(d.State, "start"), publicCalls: newPublicLimiter().share(d.State, "public"),
		publicLimit: newPageLimiter().share(d.State, "pages")}
	h.register()
	h.mux.HandleFunc("/api/plugins/{id}/{path...}", h.pluginAPI)
	h.mux.HandleFunc(Prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, errNotFound(eNoSuchEndpoint))
	})
	return h
}

// API serves everything under /api/.
func (h *Handler) API() http.Handler { return h.mux }

// Routes lists the registered management operations.
func (h *Handler) Routes() []RouteInfo {
	out := append([]RouteInfo(nil), h.routes...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// NeedsSetup reports whether no admin account exists yet.
func (h *Handler) NeedsSetup(ctx context.Context) (bool, error) {
	if h.setupDone.Load() {
		return false, nil
	}
	admin, err := h.firstAdmin(ctx)
	if err != nil || admin == nil {
		return err == nil, err
	}
	h.setupDone.Store(true)
	return false, nil
}

// op is one management operation.
type op struct {
	method string
	path   string
	access Access
	fn     func(c *call) error
}

func (h *Handler) handle(o op) {
	h.routes = append(h.routes, RouteInfo{Method: o.method, Path: o.path, Access: o.access})
	h.mux.HandleFunc(o.method+" "+Prefix+o.path, func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, o)
	})
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, o op) {
	w.Header().Set("Cache-Control", "no-store")
	c := &call{h: h, w: w, r: r, ctx: r.Context()}
	if err := h.authorize(c, o.access); err != nil {
		writeError(w, r, err)
		return
	}
	if err := o.fn(c); err != nil {
		h.fail(w, r, err)
	}
}

// fail writes err, logging failures that are not API errors.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		h.d.Logger.Error("admin request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	writeError(w, r, err)
}

// register lists every operation of api/openapi.yaml.
func (h *Handler) register() {
	for _, o := range []op{
		{"GET", "/setup/status", Public, h.getSetupStatus},
		{"POST", "/setup", SetupOnly, h.runSetup},
		{"POST", "/auth/login", Public, h.login},
		{"POST", "/auth/logout", Anyone, h.logout},
		{"GET", "/me", Anyone, h.getMe},
		{"GET", "/auth/providers", Public, h.listAuthProviders},
		{"GET", "/auth/sso/{id}/start", Public, h.startSSO},
		{"GET", "/auth/sso/{id}/callback", Public, h.ssoCallback},
		{"GET", "/auth/mfa", Public, h.getPendingMFA},
		{"POST", "/auth/mfa", Public, h.verifyMFA},
		{"POST", "/auth/passkey/options", Public, h.passkeyOptions},
		{"POST", "/auth/passkey", Public, h.passkeyLogin},

		{"GET", "/me/security", Anyone, h.getSecurity},
		{"POST", "/me/reauth", Anyone, h.reauth},
		{"POST", "/me/totp/setup", Anyone, h.setupTOTP},
		{"POST", "/me/totp", Anyone, h.enableTOTP},
		{"DELETE", "/me/totp", Anyone, h.disableTOTP},
		{"POST", "/me/recovery-codes", Anyone, h.regenerateRecoveryCodes},
		{"POST", "/me/password", Anyone, h.changeOwnPassword},
		{"POST", "/me/passkeys/options", Anyone, h.passkeyRegisterOptions},
		{"POST", "/me/passkeys", Anyone, h.registerPasskey},
		{"PATCH", "/me/passkeys/{id}", Anyone, h.renamePasskey},
		{"DELETE", "/me/passkeys/{id}", Anyone, h.deletePasskey},

		{"GET", "/overview", Anyone, h.getOverview},
		{"GET", "/usage/timeseries", Anyone, h.getTimeseries},
		{"GET", "/usage/top", Anyone, h.getUsageTop},
		{"GET", "/clients", Anyone, h.listClients},
		{"GET", "/usage/failures", Anyone, h.getUsageFailures},
		{"GET", "/quota", Anyone, h.getQuota},
		{"GET", "/status", Anyone, h.getStatus},
		{"GET", "/catalog", Anyone, h.getCatalog},
		{"GET", "/public/catalog", Public, h.getPublicCatalog},
		{"GET", "/public/status", Public, h.getPublicStatus},

		{"GET", "/keys", Anyone, h.listKeys},
		{"POST", "/keys", Anyone, h.createKey},
		{"GET", "/keys/{id}", Anyone, h.getKey},
		{"PATCH", "/keys/{id}", Anyone, h.updateKey},
		{"DELETE", "/keys/{id}", Anyone, h.deleteKey},
		{"POST", "/keys/{id}/rotate", Anyone, h.rotateKey},
		{"GET", "/keys/{id}/connect", Anyone, h.getKeyConnect},
		{"POST", "/keys/{id}/test", Anyone, h.testKey},
		{"POST", "/playground", Anyone, h.playground},

		{"GET", "/users", AdminOnly, h.listUsers},
		{"POST", "/users", AdminOnly, h.createUser},
		{"GET", "/users/{id}", AdminOnly, h.getUser},
		{"PATCH", "/users/{id}", AdminOnly, h.updateUser},
		{"DELETE", "/users/{id}", AdminOnly, h.deleteUser},
		{"GET", "/users/{id}/identities", AdminOnly, h.listUserIdentities},
		{"DELETE", "/users/{id}/identities/{identity_id}", AdminOnly, h.deleteUserIdentity},
		{"DELETE", "/users/{id}/mfa", AdminOnly, h.resetUserMFA},
		{"GET", "/groups", AdminOnly, h.listGroups},
		{"POST", "/groups", AdminOnly, h.createGroup},
		{"PATCH", "/groups/{id}", AdminOnly, h.updateGroup},
		{"DELETE", "/groups/{id}", AdminOnly, h.deleteGroup},
		{"GET", "/groups/{id}/settings", AdminOnly, h.getObjectSettings(plugin.Group)},
		{"PUT", "/groups/{id}/settings", AdminOnly, h.putObjectSettings(plugin.Group)},
		{"GET", "/users/{id}/settings", AdminOnly, h.getObjectSettings(plugin.User)},
		{"PUT", "/users/{id}/settings", AdminOnly, h.putObjectSettings(plugin.User)},
		{"GET", "/keys/{id}/settings", AdminOnly, h.getObjectSettings(plugin.Key)},
		{"PUT", "/keys/{id}/settings", AdminOnly, h.putObjectSettings(plugin.Key)},

		{"GET", "/upstreams", AdminOnly, h.listUpstreams},
		{"POST", "/upstreams", AdminOnly, h.createUpstream},
		{"GET", "/upstreams/{id}", AdminOnly, h.getUpstream},
		{"PATCH", "/upstreams/{id}", AdminOnly, h.updateUpstream},
		{"DELETE", "/upstreams/{id}", AdminOnly, h.deleteUpstream},
		{"POST", "/upstreams/{id}/test", AdminOnly, h.testUpstream},
		{"GET", "/upstreams/{id}/remote-models", AdminOnly, h.listRemoteModels},
		{"GET", "/manifests", AdminOnly, h.listManifests},

		{"GET", "/upstreams/{id}/credentials", AdminOnly, h.listCredentials},
		{"POST", "/upstreams/{id}/credentials", AdminOnly, h.createCredential},
		{"GET", "/credentials/{id}", AdminOnly, h.getCredential},
		{"PATCH", "/credentials/{id}", AdminOnly, h.updateCredential},
		{"DELETE", "/credentials/{id}", AdminOnly, h.deleteCredential},
		{"POST", "/credentials/{id}/reset", AdminOnly, h.resetCredential},

		{"GET", "/models", Anyone, h.listModels},
		{"POST", "/models", AdminOnly, h.createModel},
		{"PATCH", "/models/{id}", AdminOnly, h.updateModel},
		{"DELETE", "/models/{id}", AdminOnly, h.deleteModel},
		{"POST", "/models/sync", AdminOnly, h.syncModels},
		{"GET", "/presets", Anyone, h.listPresets},
		{"POST", "/presets", AdminOnly, h.createPreset},
		{"PATCH", "/presets/{name}", AdminOnly, h.updatePreset},
		{"DELETE", "/presets/{name}", AdminOnly, h.deletePreset},

		{"GET", "/pricing-rules", AdminOnly, h.listPricingRules},
		{"POST", "/pricing-rules", AdminOnly, h.createPricingRule},
		{"PATCH", "/pricing-rules/{id}", AdminOnly, h.updatePricingRule},
		{"DELETE", "/pricing-rules/{id}", AdminOnly, h.deletePricingRule},
		{"PATCH", "/price-sync/{model}", AdminOnly, h.updatePriceSync},
		{"POST", "/price-sync/{model}/accept", AdminOnly, h.acceptPriceSuggestion},
		{"POST", "/price-sync/{model}/reject", AdminOnly, h.rejectPriceSuggestion},

		{"GET", "/logs", Anyone, h.listLogs},
		{"GET", "/logs/histogram", Anyone, h.logHistogram},
		{"GET", "/logs/sessions", Anyone, h.listSessions},
		{"GET", "/logs/export", Anyone, h.exportLogs},
		{"GET", "/logs/{id}", Anyone, h.getLog},

		{"GET", "/features", AdminOnly, h.listFeatures},
		{"PATCH", "/features/{id}", AdminOnly, h.updateFeature},
		{"GET", "/extensions", AdminOnly, h.listExtensions},
		{"PATCH", "/extensions/{id}", AdminOnly, h.updateExtension},

		{"GET", "/settings", AdminOnly, h.getSettings},
		{"PATCH", "/settings", AdminOnly, h.updateSettings},
		{"GET", "/sso/providers", AdminOnly, h.listSSOProviders},
		{"POST", "/sso/providers", AdminOnly, h.createSSOProvider},
		{"PATCH", "/sso/providers/{id}", AdminOnly, h.updateSSOProvider},
		{"DELETE", "/sso/providers/{id}", AdminOnly, h.deleteSSOProvider},
		{"GET", "/mcp/servers", AdminOnly, h.listMCPServers},
		{"POST", "/mcp/servers", AdminOnly, h.createMCPServer},
		{"GET", "/mcp/servers/{id}", AdminOnly, h.getMCPServer},
		{"PATCH", "/mcp/servers/{id}", AdminOnly, h.updateMCPServer},
		{"DELETE", "/mcp/servers/{id}", AdminOnly, h.deleteMCPServer},
		{"POST", "/mcp/servers/{id}/test", AdminOnly, h.testMCPServer},
		{"GET", "/notifications", AdminOnly, h.listNotifications},
		{"GET", "/notifications/channels", AdminOnly, h.listNotifyChannels},
		{"POST", "/notifications/channels", AdminOnly, h.createNotifyChannel},
		{"PATCH", "/notifications/channels/{id}", AdminOnly, h.updateNotifyChannel},
		{"DELETE", "/notifications/channels/{id}", AdminOnly, h.deleteNotifyChannel},
		{"POST", "/notifications/channels/{id}/test", AdminOnly, h.testNotifyChannel},
		{"GET", "/audit", AdminOnly, h.listAudit},
		{"GET", "/search", Anyone, h.search},
	} {
		h.handle(o)
	}
}
