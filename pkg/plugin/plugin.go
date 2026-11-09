// Package plugin is the public plugin API of plugrelay.
//
// Plugins attach to fixed extension points (provider, auth, middleware,
// selector, meter, sink, job, admin). The same points are reachable in
// three ways: a declarative YAML manifest (providers only), an external
// process speaking JSON-RPC over stdio, or Go code compiled in. To the core
// all three look the same.
//
// Plugins see the core only through this package and the Host they receive
// in Init; they must not import plugrelay's internal packages.
package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Kind is an extension point.
type Kind string

const (
	KindProvider   Kind = "provider"
	KindAuth       Kind = "auth"
	KindMiddleware Kind = "middleware"
	KindSelector   Kind = "selector"
	KindMeter      Kind = "meter"
	KindSink       Kind = "sink"
	KindJob        Kind = "job"
	KindAdmin      Kind = "admin"
)

// FailPolicy decides what happens to a request when a plugin errors or times out.
type FailPolicy string

const (
	FailClosed FailPolicy = "closed" // reject the request (auth, billing, guardrails)
	FailOpen   FailPolicy = "open"   // carry on (logging, observability)
)

// Info describes a plugin. ID is "<kind>.<name>", e.g. "middleware.ratelimit".
type Info struct {
	ID          string
	Kind        Kind
	Version     string
	Description string
	// Settings this plugin reads; the core merges them across layers and
	// renders forms for them in the console.
	Settings []Setting
	// Mutates lists request fields (gjson paths) the plugin may change.
	// A plugin that declares none can only read, which keeps passthrough
	// byte-exact.
	Mutates []string
	// FailPolicy defaults to closed for auth/meter/middleware and open
	// for sinks and jobs when empty (see EffectiveFailPolicy). For a sink,
	// closed means its records must not be lost (a billing ledger): the
	// core waits for queue room instead of dropping them and retries failed
	// writes.
	FailPolicy FailPolicy
	// ReportsOffBox marks plugins that send data off the box (traces,
	// telemetry, third-party log shipping). Test mode never runs them.
	ReportsOffBox bool
	// Requires lists plugin ids this plugin cannot work without (a budget
	// meter needs the request log it sums spend from). The core turns a
	// required plugin on with the dependent. If a requirement is not
	// installed, a dependent whose effective policy is FailClosed stops the
	// configuration from loading; any other dependent is turned off and the
	// conflict logged.
	Requires []string
}

// EffectiveFailPolicy is FailPolicy, or the default of the plugin's kind
// when it is empty.
func (i Info) EffectiveFailPolicy() FailPolicy {
	if i.FailPolicy != "" {
		return i.FailPolicy
	}
	switch i.Kind {
	case KindSink, KindJob:
		return FailOpen
	}
	return FailClosed
}

// Plugin is implemented by every plugin.
type Plugin interface {
	Info() Info
}

// Initializer is implemented by plugins that need the Host or their
// plugin-level config before serving.
type Initializer interface {
	Init(ctx context.Context, h Host) error
}

// Closer is implemented by plugins holding resources.
type Closer interface {
	Close() error
}

// Host is what the core offers plugins.
type Host interface {
	// Logger is scoped to the plugin.
	Logger() *slog.Logger
	// HTTPClient returns the core's direct egress client (SSRF guard, no
	// proxy) for traffic that belongs to no upstream credential. Built-in
	// plugins must not open connections of their own.
	HTTPClient() *http.Client
	// ClientFor returns the egress client for calls a plugin makes to an
	// upstream outside a request (model listing, quota probes, credential
	// refresh): the credential's proxy, else the upstream's, the upstream's
	// private-address setting and timeouts, as the core uses for requests.
	// c may be nil (the upstream's egress). A configured proxy is never
	// bypassed; an unknown upstream is an error. During an attempt use
	// Outgoing.Client instead.
	ClientFor(u *Upstream, c *Credential) (*http.Client, error)
	// KV is durable storage scoped to the plugin.
	KV() KV
	// Settings resolves the plugin's global-layer settings (no caller).
	Settings() Settings
	// Usage queries recorded usage, for meters and quota display.
	Usage() UsageReader
	// Version is the plugrelay version string.
	Version() string
}

// KV is a small plugin-scoped key-value store.
type KV interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Put(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	// Incr atomically adds delta and returns the new value; a missing key starts at 0.
	Incr(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error)
}

// UsageReader sums recorded usage.
type UsageReader interface {
	// Spent returns what the subject ("key:<id>", "user:<id>", "group:<id>")
	// was charged since t, in micro-units of the site currency (the billed
	// amount, not the operator's upstream cost), and its request count.
	Spent(ctx context.Context, subject string, since time.Time) (chargeMicros int64, requests int64, err error)
}

// Factory builds a plugin instance.
type Factory func() Plugin

var (
	regMu     sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes a compiled-in plugin available. Call it from init. It
// runs only while switched on: a core module always, a module while its
// feature is on, any other plugin while its extension is enabled.
func Register(id string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := factories[id]; dup {
		panic(fmt.Sprintf("plugin: duplicate registration %q", id))
	}
	factories[id] = f
}

// Lookup returns a registered factory.
func Lookup(id string) (Factory, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	f, ok := factories[id]
	return f, ok
}

// Registered returns all registered plugin ids, sorted.
func Registered() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	ids := make([]string, 0, len(factories))
	for id := range factories {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
