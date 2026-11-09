// Package router decides which upstream endpoint and credential serve a
// request, in what order, and keeps the live health of credentials and
// upstreams.
//
// Configuration arrives as an immutable Table snapshot swapped in with
// SetTable; live state (cooldowns, quarantine, breakers, in-flight slots,
// session bindings) lives in a state.State so every node of a cluster sees the
// same picture. The router never talks to the database.
package router

import (
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// Table is a read-only routing snapshot. Callers must not mutate a Table after
// handing it to SetTable.
type Table struct {
	Models map[string]Model
	// Presets are client-facing names served by one of several models;
	// a name is never both a model and a preset.
	Presets   map[string]Preset
	Upstreams map[string]Upstream
}

// Model is a client-facing model name.
type Model struct {
	Name    string
	Enabled bool
	Routes  []Route
}

// Preset serves a request for its name with one of its models: each model
// is a fallback level, tried in list order unless the pipeline reorders by
// Strategy.
type Preset struct {
	Name     string
	Models   []string
	Strategy string // "in_order", "cheapest" or "fastest"
}

// Route points a model at an upstream.
type Route struct {
	UpstreamID    string
	UpstreamModel string
	Priority      int // higher first
	Weight        int // split within a priority for new sessions; <= 0 counts as 1
	// Groups restricts the route to callers in these groups; empty = everyone.
	Groups []string
}

// Upstream is one configured upstream instance.
type Upstream struct {
	ID        string
	Enabled   bool
	Endpoints []plugin.Endpoint
	// Credentials serve the upstream's requests.
	Credentials []Cred
	// KeyOptional marks an upstream whose manifest says the server may run
	// without a key (auth.key_optional). See Keyless.
	KeyOptional bool
	// MaxActiveCredentials caps how many credentials may serve requests at
	// the same time; 0 = unlimited.
	MaxActiveCredentials int
}

// Keyless reports whether the upstream is routed without a key: its
// manifest makes the key optional and it has no credentials at all. It is
// then routed as one shared credential with the id "" and sent no auth.
// Any other upstream without credentials has no route.
func (u Upstream) Keyless() bool { return u.KeyOptional && len(u.Credentials) == 0 }

// Credential kinds.
const (
	KindShared   = "shared"
	KindPersonal = "personal"
	KindService  = "service"
)

// CredStatus is a credential's durable status as stored by the operator.
type CredStatus string

// Durable credential statuses. Live statuses observed by the router are
// reported by Live and are layered on top of these.
const (
	StatusActive      CredStatus = "active"
	StatusDisabled    CredStatus = "disabled"
	StatusCooling     CredStatus = "cooling" // until StatusUntil
	StatusRefreshing  CredStatus = "refreshing"
	StatusQuarantined CredStatus = "quarantined"
	StatusManual      CredStatus = "manual"
)

// Cred is a credential's routing-relevant metadata; secrets stay elsewhere.
type Cred struct {
	ID             string
	Kind           string
	OwnerID        string // user id, required for personal credentials
	MaxConcurrency int    // 0 = unlimited
	Status         CredStatus
	StatusUntil    time.Time
	Reason         string
}

type resolvedModel struct {
	name   string
	routes []Route
}

// resolve returns the fallback levels for a requested name: the model
// itself, or each enabled model of the preset that allowed admits, in the
// preset's order. Unknown and disabled models give no level. The strategy
// is the preset's ("" for a model).
func (t *Table) resolve(name string, allowed func(string) bool) (levels []resolvedModel, preset, strategy string) {
	use := func(m string) {
		if mod, ok := t.Models[m]; ok && mod.Enabled && len(mod.Routes) > 0 && (allowed == nil || allowed(m)) {
			levels = append(levels, resolvedModel{name: m, routes: mod.Routes})
		}
	}
	if _, ok := t.Models[name]; ok {
		use(name)
		return levels, "", ""
	}
	p, ok := t.Presets[name]
	if !ok {
		return nil, "", ""
	}
	seen := map[string]bool{}
	for _, m := range p.Models {
		if !seen[m] {
			seen[m] = true
			use(m)
		}
	}
	return levels, p.Name, p.Strategy
}
