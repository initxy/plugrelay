package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Console-only upstream fields live in Upstream.Config under these keys;
// providers ignore keys their manifest does not declare.
const (
	cfgName          = "_name"
	cfgTags          = "_tags"
	cfgHeaders       = "_headers"
	cfgProxy         = "_proxy"
	cfgTimeouts      = "_timeouts"
	cfgScheduledTest = "_scheduled_test"
	cfgQuotaProbe    = "_quota_probe"
	cfgModelSync     = "_model_sync"
)

type upstreamTimeouts struct {
	ConnectMs   int64 `json:"connect_ms,omitempty"`
	FirstByteMs int64 `json:"first_byte_ms,omitempty"`
	IdleMs      int64 `json:"idle_ms,omitempty"`
	TotalMs     int64 `json:"total_ms,omitempty"`
}

type manifestSource struct {
	Kind       string   `json:"kind"`
	Name       string   `json:"name,omitempty"`
	Version    string   `json:"version,omitempty"`
	Overridden []string `json:"overridden,omitempty"`
}

// Circuit reasons: open after repeated upstream failures; half open while
// one probe request decides whether it closes.
const (
	circuitFailures = "failures"
	circuitProbing  = "probing"
)

type circuitState struct {
	State  string     `json:"state"`
	Until  *time.Time `json:"until"`
	Reason string     `json:"reason,omitempty"`
}

type upstreamStats struct {
	SuccessRate *float64   `json:"success_rate_24h"`
	Requests    int64      `json:"requests_24h"`
	Hourly      []*float64 `json:"hourly"`
}

type upstreamView struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Provider    string            `json:"provider"`
	TypeLabel   string            `json:"type_label,omitempty"`
	Enabled     bool              `json:"enabled"`
	Tags        []string          `json:"tags,omitempty"`
	Endpoints   []plugin.Endpoint `json:"endpoints"`
	Headers     map[string]string `json:"headers,omitempty"`
	Proxy       string            `json:"proxy,omitempty"`
	Timeouts    *upstreamTimeouts `json:"timeouts,omitempty"`
	Config      map[string]any    `json:"config,omitempty"`
	Source      manifestSource    `json:"source"`
	Circuit     circuitState      `json:"circuit"`
	Stats       upstreamStats     `json:"stats"`
	Credentials []credentialView  `json:"credentials"`
	// KeyOptional: the upstream's manifest lets it run without a key, so
	// with no credentials it is routed with no auth; any other upstream
	// with no credentials is not routed.
	KeyOptional   bool    `json:"key_optional"`
	ScheduledTest *string `json:"scheduled_test"`
	QuotaProbe    *string `json:"quota_probe"`
	ModelSync     *bool   `json:"model_sync"`
}

type upstreamInput struct {
	Name          *string            `json:"name"`
	Provider      *string            `json:"provider"`
	Manifest      *string            `json:"manifest"`
	Enabled       *bool              `json:"enabled"`
	Tags          *[]string          `json:"tags"`
	Endpoints     *[]plugin.Endpoint `json:"endpoints"`
	Headers       *map[string]string `json:"headers"`
	Proxy         *string            `json:"proxy"`
	Timeouts      *upstreamTimeouts  `json:"timeouts"`
	Config        map[string]any     `json:"config"`
	ScheduledTest *string            `json:"scheduled_test"`
	QuotaProbe    *string            `json:"quota_probe"`
	// ModelSync turns model sync on or off for the upstream; unset keeps
	// the default (on when its provider can list models).
	ModelSync *bool `json:"model_sync"`
}

// upstreamCtx is what rendering upstreams needs, read once per request.
type upstreamCtx struct {
	live      map[string]router.UpstreamLive
	stats     map[string]upstreamStats
	manifests map[string]ManifestInfo
	names     *names
	creds     map[string][]store.Credential
}

func (h *Handler) loadUpstreamCtx(ctx context.Context) (*upstreamCtx, error) {
	uc := &upstreamCtx{live: map[string]router.UpstreamLive{}, manifests: map[string]ManifestInfo{},
		creds: map[string][]store.Credential{}}
	for _, ul := range h.d.Runtime.Live(ctx).Upstreams {
		uc.live[ul.ID] = ul
	}
	var err error
	if uc.stats, err = h.hourlyStats(ctx, "upstream"); err != nil {
		return nil, err
	}
	for _, m := range h.d.Runtime.Manifests(ctx) {
		uc.manifests[m.Name] = m
	}
	if uc.names, err = h.names(ctx); err != nil {
		return nil, err
	}
	creds, err := h.d.Store.ListCredentials(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, c := range creds {
		uc.creds[c.UpstreamID] = append(uc.creds[c.UpstreamID], c)
	}
	return uc, nil
}

// hourlyStats reads the last 24 hours of success ratios per series.
func (h *Handler) hourlyStats(ctx context.Context, by string) (map[string]upstreamStats, error) {
	now := h.d.Now().UTC().Truncate(time.Hour)
	from := now.Add(-23 * time.Hour)
	pts, err := h.d.Store.UsageSeries(ctx, store.UsageQuery{From: from, To: now.Add(time.Hour), Bucket: time.Hour, By: by})
	if err != nil {
		return nil, err
	}
	type acc struct{ req, errs [24]int64 }
	accs := map[string]*acc{}
	for _, p := range pts {
		i := int(p.Time.UTC().Sub(from) / time.Hour)
		if i < 0 || i >= 24 {
			continue
		}
		a := accs[p.Series]
		if a == nil {
			a = &acc{}
			accs[p.Series] = a
		}
		a.req[i] += p.Requests
		a.errs[i] += p.Errors
	}
	out := map[string]upstreamStats{}
	for k, a := range accs {
		s := upstreamStats{Hourly: make([]*float64, 24)}
		var req, errs int64
		for i := range 24 {
			req += a.req[i]
			errs += a.errs[i]
			if a.req[i] > 0 {
				r := float64(a.req[i]-a.errs[i]) / float64(a.req[i])
				s.Hourly[i] = &r
			}
		}
		s.Requests = req
		if req > 0 {
			r := float64(req-errs) / float64(req)
			s.SuccessRate = &r
		}
		out[k] = s
	}
	return out, nil
}

// UpstreamName is the name the console shows for an upstream.
func UpstreamName(u *store.Upstream) string {
	if s, _ := u.Config[cfgName].(string); s != "" {
		return s
	}
	return u.ID
}

func emptyStats() upstreamStats { return upstreamStats{Hourly: make([]*float64, 24)} }

func (h *Handler) upstreamView(ctx context.Context, u *store.Upstream, uc *upstreamCtx) upstreamView {
	v := upstreamView{ID: u.ID, Name: UpstreamName(u), Provider: u.Provider, Enabled: u.Enabled,
		Config: map[string]any{}, Credentials: []credentialView{}}
	for k, val := range u.Config {
		switch k {
		case cfgName: // shown as Name
		case cfgTags:
			v.Tags = toStrings(val)
		case cfgHeaders:
			v.Headers = toStringMap(val)
		case cfgProxy:
			v.Proxy, _ = val.(string)
		case cfgTimeouts:
			t := timeoutsOf(val)
			v.Timeouts = &t
		case cfgScheduledTest:
			if s, ok := val.(string); ok && s != "" {
				v.ScheduledTest = &s
			}
		case cfgQuotaProbe:
			if s, ok := val.(string); ok && s != "" {
				v.QuotaProbe = &s
			}
		case cfgModelSync:
			if b, ok := val.(bool); ok {
				v.ModelSync = &b
			}
		default:
			v.Config[k] = val
		}
	}
	if eps, err := h.d.Runtime.Endpoints(u); err == nil {
		v.Endpoints = eps
	} else {
		v.Endpoints = u.Endpoints
	}
	if v.Endpoints == nil {
		v.Endpoints = []plugin.Endpoint{}
	}
	v.Source = manifestSource{Kind: "site", Name: u.Manifest}
	if m, ok := uc.manifests[u.Manifest]; ok {
		v.TypeLabel = m.Title
		v.KeyOptional = m.KeyOptional
		v.Source.Version = m.Version
		v.Source.Kind = "file"
		if m.Kind == "official" {
			v.Source.Kind = "official"
		}
		if len(u.Endpoints) > 0 {
			v.Source.Overridden = []string{"endpoints"}
		}
	}
	live := uc.live[u.ID]
	v.Circuit = circuitState{State: orDefault(live.Breaker, router.BreakerClosed)}
	// The router clears its failure count when the breaker opens, so the
	// count says nothing about an open breaker; the reason is a code.
	switch live.Breaker {
	case router.BreakerOpen:
		v.Circuit.Until = timePtr(live.BreakerOpen)
		v.Circuit.Reason = circuitFailures
	case router.BreakerHalfOpen:
		v.Circuit.Reason = circuitProbing
	}
	v.Stats = emptyStats()
	if s, ok := uc.stats[u.ID]; ok {
		v.Stats = s
	}
	for i := range uc.creds[u.ID] {
		v.Credentials = append(v.Credentials, credentialViewOf(&uc.creds[u.ID][i], live, uc.names))
	}
	return v
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}

func timeoutsOf(v any) upstreamTimeouts {
	var t upstreamTimeouts
	m, _ := v.(map[string]any)
	ms := func(k string) int64 {
		switch n := m[k].(type) {
		case float64:
			return int64(n)
		case int64:
			return n
		case int:
			return int64(n)
		case json.Number:
			i, _ := n.Int64()
			return i
		}
		return 0
	}
	t.ConnectMs, t.FirstByteMs, t.IdleMs, t.TotalMs = ms("connect_ms"), ms("first_byte_ms"), ms("idle_ms"), ms("total_ms")
	return t
}

// UpstreamEgress returns the egress overrides stored in an upstream's
// config: extra request headers, the proxy of credentials that set none,
// and timeouts (zero fields keep the relay's defaults).
func UpstreamEgress(cfg map[string]any) (headers map[string]string, proxy string, timeouts egress.Timeouts) {
	headers = toStringMap(cfg[cfgHeaders])
	proxy, _ = cfg[cfgProxy].(string)
	t := timeoutsOf(cfg[cfgTimeouts])
	ms := func(n int64) time.Duration { return time.Duration(n) * time.Millisecond }
	timeouts = egress.Timeouts{Connect: ms(t.ConnectMs), FirstByte: ms(t.FirstByteMs), Idle: ms(t.IdleMs), Total: ms(t.TotalMs)}
	return headers, proxy, timeouts
}

const quotaProbeOff = "off"

// ModelSync returns the upstream's model sync switch and whether one is
// set; unset means the default.
func ModelSync(cfg map[string]any) (on, set bool) {
	on, set = cfg[cfgModelSync].(bool)
	return on, set
}

// QuotaProbeInterval returns the upstream's quota probe interval: 0 when
// unset (job.quotaprobe's own interval applies), negative when off.
func QuotaProbeInterval(cfg map[string]any) time.Duration {
	s, _ := cfg[cfgQuotaProbe].(string)
	if s == quotaProbeOff || s == "0" {
		return -1
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

func toStringMap(v any) map[string]string {
	switch x := v.(type) {
	case map[string]string:
		return x
	case map[string]any:
		out := make(map[string]string, len(x))
		for k, e := range x {
			out[k] = fmt.Sprint(e)
		}
		return out
	}
	return nil
}

func (h *Handler) listUpstreams(c *call) error {
	ups, err := h.d.Store.ListUpstreams(c.ctx)
	if err != nil {
		return err
	}
	uc, err := h.loadUpstreamCtx(c.ctx)
	if err != nil {
		return err
	}
	q := c.query("q")
	items := []upstreamView{}
	for i := range ups {
		v := h.upstreamView(c.ctx, &ups[i], uc)
		if q != "" && !containsFold(v.ID, q) && !containsFold(v.Name, q) && !containsFold(v.Provider, q) &&
			!slices.ContainsFunc(v.Tags, func(t string) bool { return containsFold(t, q) }) {
			continue
		}
		items = append(items, v)
	}
	return c.json(200, map[string]any{"items": items})
}

func (h *Handler) writeUpstream(c *call, status int, id string) error {
	u, err := h.d.Store.GetUpstream(c.ctx, id)
	if err != nil {
		return storeErr(err, objUpstream)
	}
	uc, err := h.loadUpstreamCtx(c.ctx)
	if err != nil {
		return err
	}
	return c.json(status, h.upstreamView(c.ctx, u, uc))
}

func (h *Handler) getUpstream(c *call) error { return h.writeUpstream(c, 200, c.r.PathValue("id")) }

var nonSlug = regexp.MustCompile(`[^a-z0-9-]+`)

func slugify(s string) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

func (h *Handler) createUpstream(c *call) error {
	var in upstreamInput
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	u := &store.Upstream{Enabled: true, Config: map[string]any{}, Source: store.SourceCustom}
	if in.Manifest != nil && *in.Manifest != "" {
		found := false
		for _, m := range h.d.Runtime.Manifests(c.ctx) {
			if m.Name == *in.Manifest {
				u.Manifest, u.Provider, found = m.Name, m.Provider, true
				if m.Kind == "official" {
					u.Source = store.SourceOfficial
				}
			}
		}
		if !found {
			bad.add("manifest", eUnknownManifest)
		}
		u.Config["manifest"] = *in.Manifest
	}
	if in.Provider != nil && *in.Provider != "" {
		u.Provider = *in.Provider
	}
	if u.Provider == "" {
		bad.add("provider", eProviderNeeded)
	}
	name := ""
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
	}
	if name == "" {
		name = u.Manifest
	}
	base := slugify(name)
	if base == "" {
		base = slugify(strings.TrimPrefix(u.Provider, "provider."))
	}
	if base == "" {
		base = "upstream"
	}
	u.ID = base
	for i := 2; ; i++ {
		if _, err := h.d.Store.GetUpstream(c.ctx, u.ID); err != nil {
			break
		}
		u.ID = fmt.Sprintf("%s-%d", base, i)
	}
	if name != "" {
		in.Name = &name
	}
	h.applyUpstreamInput(u, in, &bad)
	if err := bad.err(); err != nil {
		return err
	}
	if _, err := h.d.Runtime.Endpoints(u); err != nil {
		return fieldInvalid("config", eUpstreamConfig.with("reason", err.Error()))
	}
	if err := h.d.Store.PutUpstream(c.ctx, u); err != nil {
		return storeErr(err, objUpstream)
	}
	c.audit("upstream.create", "upstream", u.ID, name, []change{{Field: "provider", To: u.Provider}})
	return h.writeUpstream(c, 201, u.ID)
}

func (h *Handler) applyUpstreamInput(u *store.Upstream, in upstreamInput, bad *invalid) {
	for k, v := range in.Config {
		if strings.HasPrefix(k, "_") {
			bad.add("config."+k, eReservedKey)
			continue
		}
		if v == nil {
			delete(u.Config, k)
		} else {
			u.Config[k] = v
		}
	}
	setOrDel := func(k string, v any, set bool) {
		if set {
			u.Config[k] = v
		} else {
			delete(u.Config, k)
		}
	}
	if in.Name != nil {
		setOrDel(cfgName, *in.Name, *in.Name != "")
	}
	if in.Enabled != nil {
		u.Enabled = *in.Enabled
	}
	if in.Tags != nil {
		setOrDel(cfgTags, *in.Tags, len(*in.Tags) > 0)
	}
	if in.Headers != nil {
		for name := range *in.Headers {
			if !egress.ExtraHeaderAllowed(name) {
				bad.add("headers."+name, eHeaderRefused)
			}
		}
		setOrDel(cfgHeaders, *in.Headers, len(*in.Headers) > 0)
	}
	if in.Proxy != nil {
		if err := egress.CheckProxy(*in.Proxy); err != nil {
			bad.add("proxy", eURLProxy)
		}
		setOrDel(cfgProxy, *in.Proxy, *in.Proxy != "")
	}
	if in.Timeouts != nil {
		t := *in.Timeouts
		if t.ConnectMs < 0 || t.FirstByteMs < 0 || t.IdleMs < 0 || t.TotalMs < 0 {
			bad.add("timeouts", eNotNegative)
		}
		setOrDel(cfgTimeouts, map[string]any{"connect_ms": t.ConnectMs, "first_byte_ms": t.FirstByteMs, "idle_ms": t.IdleMs, "total_ms": t.TotalMs},
			t != upstreamTimeouts{})
	}
	if in.ScheduledTest != nil {
		if *in.ScheduledTest != "" {
			if _, err := time.ParseDuration(*in.ScheduledTest); err != nil {
				bad.add("scheduled_test", eInterval)
			}
		}
		setOrDel(cfgScheduledTest, *in.ScheduledTest, *in.ScheduledTest != "")
	}
	if in.QuotaProbe != nil {
		if q := *in.QuotaProbe; q != "" && q != quotaProbeOff && q != "0" {
			if d, err := time.ParseDuration(q); err != nil || d < time.Minute {
				bad.add("quota_probe", eQuotaProbe)
			}
		}
		setOrDel(cfgQuotaProbe, *in.QuotaProbe, *in.QuotaProbe != "")
	}
	if in.Endpoints != nil {
		for i, ep := range *in.Endpoints {
			if ep.ID == "" {
				bad.add(fmt.Sprintf("endpoints.%d.id", i), eRequired)
			}
			if ep.BaseURL != "" && !strings.HasPrefix(ep.BaseURL, "http://") && !strings.HasPrefix(ep.BaseURL, "https://") {
				bad.add(fmt.Sprintf("endpoints.%d.base_url", i), eURLHTTP)
			}
		}
		u.Endpoints = *in.Endpoints
	}
	if in.ModelSync != nil {
		u.Config[cfgModelSync] = *in.ModelSync
	}
}

func (h *Handler) updateUpstream(c *call) error {
	u, err := h.d.Store.GetUpstream(c.ctx, c.r.PathValue("id"))
	if err != nil {
		return storeErr(err, objUpstream)
	}
	var in upstreamInput
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	if in.Manifest != nil && *in.Manifest != u.Manifest {
		bad.add("manifest", eImmutable)
	}
	if in.Provider != nil && *in.Provider != "" {
		u.Provider = *in.Provider
	}
	before := map[string]any{"enabled": u.Enabled, "config": maps.Clone(u.Config)}
	if u.Config == nil {
		u.Config = map[string]any{}
	}
	h.applyUpstreamInput(u, in, &bad)
	if err := bad.err(); err != nil {
		return err
	}
	if _, err := h.d.Runtime.Endpoints(u); err != nil {
		return fieldInvalid("config", eUpstreamConfig.with("reason", err.Error()))
	}
	if err := h.d.Store.PutUpstream(c.ctx, u); err != nil {
		return storeErr(err, objUpstream)
	}
	c.audit("upstream.update", "upstream", u.ID, u.ID, []change{{Field: "upstream", From: before,
		To: map[string]any{"enabled": u.Enabled, "config": u.Config}}})
	return h.writeUpstream(c, 200, u.ID)
}

func (h *Handler) deleteUpstream(c *call) error {
	id := c.r.PathValue("id")
	if _, err := h.d.Store.GetUpstream(c.ctx, id); err != nil {
		return storeErr(err, objUpstream)
	}
	if err := h.d.Store.DeleteUpstream(c.ctx, id); err != nil {
		return storeErr(err, objUpstream)
	}
	h.dropUpstreamPrices(c.ctx, id)
	c.audit("upstream.delete", "upstream", id, id, nil)
	return c.noContent()
}

func (h *Handler) dropUpstreamPrices(ctx context.Context, upstreamID string) {
	ps, err := h.d.Store.ListPrices(ctx)
	if err != nil {
		return
	}
	for _, p := range ps {
		if p.UpstreamID == upstreamID {
			_ = h.d.Store.DeletePrice(ctx, p.Model, p.UpstreamID, p.UpstreamModel)
		}
	}
}

func (h *Handler) testUpstream(c *call) error {
	id := c.r.PathValue("id")
	if _, err := h.d.Store.GetUpstream(c.ctx, id); err != nil {
		return storeErr(err, objUpstream)
	}
	var in TestRequest
	if err := c.decode(&in); err != nil {
		return err
	}
	res := h.d.Runtime.TestUpstream(c.ctx, id, in)
	c.audit("upstream.test", "upstream", id, id, nil)
	return c.json(200, testOutcome(res))
}

// testOutcome is the answer of every test action (upstream, key, MCP
// server): ok when there was something to check and every check passed.
func testOutcome[T interface{ Passed() bool }](results []T) map[string]any {
	ok := len(results) > 0
	for _, r := range results {
		ok = ok && r.Passed()
	}
	if results == nil {
		results = []T{}
	}
	return map[string]any{"ok": ok, "results": results}
}

func (h *Handler) listRemoteModels(c *call) error {
	id := c.r.PathValue("id")
	if _, err := h.d.Store.GetUpstream(c.ctx, id); err != nil {
		return storeErr(err, objUpstream)
	}
	ms, err := h.d.Runtime.RemoteModels(c.ctx, id)
	if err != nil {
		return errUpstream(eUpstreamFailed.with("reason", err.Error()))
	}
	return c.json(200, map[string]any{"items": nonNil(ms)})
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (h *Handler) listManifests(c *call) error {
	return c.json(200, map[string]any{"items": nonNil(h.d.Runtime.Manifests(c.ctx))})
}
