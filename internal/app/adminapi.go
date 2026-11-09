package app

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"slices"
	"strings"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/manifests"
	"github.com/initxy/plugrelay/pkg/captcha"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/job/modelsync"
	"github.com/initxy/plugrelay/plugins/provider/compatible"
)

// runtimeAPI is the admin API's view of the running relay.
type runtimeAPI struct{ a *App }

var _ admin.Runtime = runtimeAPI{}

func (r runtimeAPI) Live(ctx context.Context) router.Live { return r.a.router.Live(ctx) }

func (r runtimeAPI) RestoreCredential(ctx context.Context, upstreamID, credID string) error {
	return r.a.router.Restore(ctx, upstreamID, credID)
}

func (r runtimeAPI) Plugins() []admin.PluginEntry { return r.a.entries() }

func (r runtimeAPI) CheckPlugin(ctx context.Context, pc *store.PluginConfig) error {
	return r.a.checkPluginConfig(ctx, pc)
}

func (r runtimeAPI) CheckFeature(ctx context.Context, f store.FeatureState) error {
	return r.a.checkFeature(ctx, f)
}

func (r runtimeAPI) BeforeUserDelete(ctx context.Context, userID string) error {
	for _, p := range r.a.enabledPlugins(ctx) {
		if h, ok := p.(plugin.UserDeleteHook); ok {
			if err := h.BeforeUserDelete(ctx, userID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r runtimeAPI) CheckSettings(ctx context.Context, pluginID string, scope plugin.Scope, values map[string]json.RawMessage) error {
	for _, p := range r.a.enabledPlugins(ctx) {
		if c, ok := p.(plugin.SettingsCheck); ok {
			if err := c.CheckSettings(ctx, pluginID, scope, values); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r runtimeAPI) ReloadError() string {
	if msg := r.a.reloadErr.Load(); msg != nil {
		return *msg
	}
	return ""
}

func (r runtimeAPI) SettingsFor(pluginID string, c plugin.Caller) plugin.Settings {
	return callerSettings{a: r.a, id: pluginID, c: c}
}

func officialManifest(id string) (string, bool) {
	name := id + ".yaml"
	_, err := fs.Stat(manifests.FS, name)
	return name, err == nil
}

func (r runtimeAPI) Manifests(context.Context) []admin.ManifestInfo {
	idx := compatible.Default()
	var out []admin.ManifestInfo
	for _, id := range idx.IDs() {
		m, ok := idx.Get(id)
		if !ok {
			continue
		}
		mi := admin.ManifestInfo{Name: m.ID, Title: m.Name, TitleZh: m.NameZh, Category: m.Category, Kind: "file", KeyOptional: m.Auth.KeyOptional,
			DocsURL: m.Homepage, Provider: m.ProviderID(), Protocols: []string{}, Variables: []admin.ManifestVariable{}}
		if mi.Category == "" {
			mi.Category = "custom"
		}
		for name, v := range m.Variables {
			mi.Variables = append(mi.Variables, admin.ManifestVariable{Name: name, Description: v.Description,
				DescriptionZh: v.DescriptionZh, Default: v.Default, Required: v.Required && v.Default == ""})
		}
		slices.SortFunc(mi.Variables, func(a, b admin.ManifestVariable) int { return strings.Compare(a.Name, b.Name) })
		if mi.Title == "" {
			mi.Title = m.ID
		}
		if _, official := officialManifest(id); official {
			mi.Kind = "official"
		}
		for _, e := range m.Endpoints {
			if !slices.Contains(mi.Protocols, e.Protocol) {
				mi.Protocols = append(mi.Protocols, e.Protocol)
			}
		}
		out = append(out, mi)
	}
	return out
}

func (r runtimeAPI) Endpoints(u *store.Upstream) ([]plugin.Endpoint, error) {
	_, eps, err := r.a.endpoints(context.Background(), u)
	return eps, err
}

func (r runtimeAPI) TestUpstream(ctx context.Context, upstreamID string, req admin.TestRequest) []admin.TestResult {
	return r.a.testUpstream(ctx, upstreamID, req)
}

func (r runtimeAPI) TestKey(ctx context.Context, k *store.Key, protocol, model string) admin.TestResult {
	return r.a.testKey(ctx, k, protocol, model)
}

func (r runtimeAPI) Playground(ctx context.Context, caller plugin.Caller, req admin.PlaygroundRequest, emit func(admin.PlaygroundEvent)) {
	r.a.playground(ctx, caller, req, emit)
}

func (r runtimeAPI) RemoteModels(ctx context.Context, upstreamID string) ([]ir.ModelInfo, error) {
	ts, err := targets{r.a, []string{upstreamID}}.SyncTargets(ctx)
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		if lacksKey(r.a.runtime(), upstreamID) {
			return nil, errNoKey
		}
		return nil, errors.New("upstream is disabled or misconfigured")
	}
	if ts[0].Lister == nil {
		return nil, errors.New("this upstream's provider cannot list models")
	}
	return ts[0].Lister.ListModels(ctx, ts[0].Upstream, ts[0].Credential)
}

func (r runtimeAPI) SyncModels(ctx context.Context, ids []string) admin.SyncResult {
	var res admin.SyncResult
	ts, err := targets{r.a, ids}.SyncTargets(ctx)
	if err != nil {
		res.Failed = append(res.Failed, admin.SyncFailure{UpstreamID: "*", Error: err.Error()})
		return res
	}
	rt := r.a.runtime()
	for _, id := range ids {
		if lacksKey(rt, id) {
			res.Failed = append(res.Failed, admin.SyncFailure{UpstreamID: id, Error: errNoKey.Error()})
		}
	}
	for _, t := range ts {
		if t.Lister == nil {
			res.Failed = append(res.Failed, admin.SyncFailure{UpstreamID: t.Upstream.ID, Error: "provider cannot list models"})
			continue
		}
		rec := &syncRecord{}
		if err := modelsync.New(catalog{r.a, rec}, nil).Sync(ctx, t); err != nil {
			res.Failed = append(res.Failed, admin.SyncFailure{UpstreamID: t.Upstream.ID, Error: err.Error()})
		}
		res.Added = append(res.Added, rec.added...)
		res.Disabled = append(res.Disabled, rec.disabled...)
		res.Unchanged += rec.unchanged
	}
	_ = r.a.Reload(ctx)
	return res
}

// signupID is the sign-up plugin of the sell kit.
const signupID = "admin.signup"

// captchaProviders lists the captcha providers the console's pages may
// load: the login's (site settings) and the sign-up plugin's.
func (a *App) captchaProviders(ctx context.Context) []string {
	out := a.admin.CaptchaProviders(ctx)
	if c := (captcha.Config{Provider: a.settingFor(nil, signupID, "captcha_provider").String()}); c.Wanted() && !slices.Contains(out, c.Provider) {
		out = append(out, c.Provider)
	}
	return out
}
