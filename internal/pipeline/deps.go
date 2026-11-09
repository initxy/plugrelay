package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/seal"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Defaults for zero Deps fields.
const (
	DefaultKeepalive       = 15 * time.Second
	DefaultKeepaliveDelay  = 5 * time.Second
	DefaultMaxAttempts     = 4
	DefaultMaxResponseBody = 64 << 20
	DefaultMaxErrorBody    = 1 << 20
	DefaultEventHookBudget = 50 * time.Millisecond
)

// Snapshot is the configuration one request runs against. The pipeline
// reads it once per request and never mutates it.
type Snapshot struct {
	// Table is the routing table; the pipeline hands a new pointer to the
	// router before planning.
	Table *router.Table
	// Upstreams by id, with provider instance and decrypted credentials.
	Upstreams map[string]*Upstream
	// Models lists every model the relay serves, for model listings.
	Models []ir.ModelInfo
	// Visible reports whether the caller may see and use a client-facing
	// model name; nil admits every model.
	Visible func(caller *plugin.Caller, model string) bool
	// Price returns the operator's cost of an upstream model; ok=false
	// means unpriced (cost 0).
	Price func(upstreamID, upstreamModel string) (Price, bool)
	// ModelPrice returns the list price of a client-facing model, what a
	// caller is charged before the group multiplier; ok=false charges the
	// delivered attempt's cost rate instead.
	ModelPrice func(model string) (Price, bool)
	// Multiplier returns a group's price multiplier; nil, or a value <= 0,
	// means 1.
	Multiplier func(groupID string) float64
	// AllowDiscouraged admits conversions graded discouraged.
	AllowDiscouraged bool
}

// Upstream is one configured upstream instance as the pipeline needs it.
type Upstream struct {
	// Config carries the provider config and the effective endpoints.
	Config   *plugin.Upstream
	Provider plugin.Provider
	// Credentials by id, secrets decrypted.
	Credentials map[string]*plugin.Credential
	// AllowPrivate lets this upstream reach private addresses.
	AllowPrivate bool
	// Timeouts override Deps.Timeouts field by field (zero keeps the
	// default).
	Timeouts egress.Timeouts
	// Proxy is the egress of credentials that set none ("" = direct).
	Proxy string
	// Headers are extra request headers, applied under
	// egress.SetExtraHeaders.
	Headers map[string]string
	// Keyless is router.Upstream.Keyless of the same upstream.
	Keyless bool
}

// credential returns the candidate's credential, nil when it is not
// configured. A keyless upstream is served by the router with credential
// id "", which sends an empty secret.
func (u *Upstream) credential(c plugin.Candidate) *plugin.Credential {
	if c.CredentialID == "" && u.Keyless {
		return &plugin.Credential{UpstreamID: c.UpstreamID, Kind: router.KindShared}
	}
	return u.Credentials[c.CredentialID]
}

// Price is a cost rule in micro-units of the site currency per million
// tokens, one rate per usage bucket, plus per-unit rates of the media
// operations keyed by plugin.Unit* (image, second of audio, 1000
// characters, rerank search unit).
type Price struct {
	Input        int64
	Output       int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
	Units        map[string]int64
	// BatchDiscount is the fraction taken off usage served through a
	// batch API (0.5 = half price).
	BatchDiscount float64
}

// Chain is the request-scoped plugins of the current configuration.
type Chain struct {
	// Plugins are middleware in configured order; each may implement any of
	// plugin.RequestHook, AttemptHook, EventHook, FinishHook, BodyMutator.
	Plugins []plugin.Plugin
	// Selector reorders candidates; nil keeps the router's order.
	Selector plugin.Selector
	// Pick reorders the candidates of a requested preset by its pick
	// strategy, in place of Selector; nil keeps the router's order.
	Pick func(strategy string, cands []plugin.Candidate) []plugin.Candidate
	// Meter owns quota and balance; nil disables metering.
	Meter plugin.Meter
}

// Router is the part of *router.Router the pipeline uses.
type Router interface {
	SetTable(t *router.Table)
	Plan(ctx context.Context, q router.Query) (*router.Plan, error)
	Acquire(ctx context.Context, c plugin.Candidate) (release func(), ok bool, err error)
	Report(ctx context.Context, c plugin.Candidate, v plugin.Verdict, model string) error
	Observe(ctx context.Context, t router.Turn) error
	IssueTurnToken(c plugin.Candidate) string
	ObserveLatency(upstreamID, endpointID string, ttfb time.Duration)
	PlanUpstream(ctx context.Context, upstreamID, endpointID string, caller plugin.Caller) (*router.Plan, error)
	RefreshDone(ctx context.Context, upstreamID, credID string) error
	MarkAttention(ctx context.Context, upstreamID, credID, reason string) error
	ClearAttention(ctx context.Context, upstreamID, credID string) error
	SeenUnserved(ctx context.Context, upstreamID, endpointID, op string, window time.Duration) (int64, error)
	MarkRefreshed(ctx context.Context, upstreamID, credID, oldFP string, ttl time.Duration) error
	Refreshed(ctx context.Context, upstreamID, credID, oldFP string) bool
	MarkUnserved(ctx context.Context, upstreamID, endpointID, op string, ttl time.Duration) (bool, error)
	Unserved(ctx context.Context, upstreamID, endpointID, op string) bool
}

// Egress hands out the HTTP client of a credential's fixed egress;
// *egress.Client implements it. ForAttempt is the client handed to the
// provider as plugin.Outgoing.Client.
type Egress interface {
	For(cred *plugin.Credential, allowPrivate bool) (*http.Client, error)
	ForAttempt(cred *plugin.Credential, allowPrivate bool, w *egress.Watch) (*http.Client, error)
}

// RecordQueue takes finished request records for the sinks; it reports
// false when the record was dropped. Enqueue runs after the response has
// been written and flushed, so it may block briefly (a full ledger lane),
// but it must stay bounded: the request's handler waits for it.
type RecordQueue interface {
	Enqueue(r *plugin.Record) bool
}

// CredentialStore persists live credential status changes that must
// survive a state backend loss (quarantine, manual hold), and secrets a
// provider's Refresher renewed.
type CredentialStore interface {
	SaveCredentialState(ctx context.Context, ev router.CredEvent) error
	SaveCredentialSecret(ctx context.Context, credID, secret string, expires time.Time) error
	// LoadCredentialSecret returns the credential's current secret from
	// the store, for a node that learns another node refreshed it.
	LoadCredentialSecret(ctx context.Context, credID string) (string, error)
}

// Metrics receives observations; every method must be cheap.
type Metrics interface {
	Attempt(protocol string, rec *plugin.AttemptRecord)
	Request(rec *plugin.Record)
}

// Deps is everything the pipeline needs. Snapshot, Router, Egress and
// Sealer are required; zero values elsewhere take defaults.
type Deps struct {
	Snapshot func() *Snapshot
	Router   Router
	Egress   Egress
	Sealer   *seal.Sealer
	// Plugins returns the current plugin chain; nil means none.
	Plugins func() *Chain
	// Settings resolves a plugin setting merged for the caller.
	Settings func(caller *plugin.Caller, pluginID, key string) plugin.Value
	Records  RecordQueue
	// Credentials persists credential status changes reported through
	// OnCredential.
	Credentials CredentialStore
	Metrics     Metrics
	// Lock takes a cluster-wide lock (state.State.Lock). Refreshing a
	// credential on a refresh verdict holds one per credential; nil turns
	// refreshing off (the credential just sits out).
	Lock func(ctx context.Context, key string, ttl time.Duration) (unlock func(), ok bool, err error)

	Now      func() time.Time
	Logger   *slog.Logger
	NewID    func() string
	ClientIP func(r *http.Request) string

	Timeouts egress.Timeouts
	// Keepalive is the idle interval between stream keepalives (15s).
	Keepalive time.Duration
	// KeepaliveDelay is how long a stream waits for upstream output before
	// committing HTTP 200 and starting keepalives (5s).
	KeepaliveDelay time.Duration
	// MaxAttempts bounds upstream tries per request (4).
	MaxAttempts     int
	MaxResponseBody int64 // non-stream upstream body (64 MiB)
	MaxErrorBody    int64 // upstream error body (1 MiB)
	// EventHookBudget bounds one OnEvent call (50ms); an overrun counts as
	// a plugin failure and its edits are discarded.
	EventHookBudget time.Duration
	// Native configures /native/ passthrough (ServeNative).
	Native Native
	// WebSocket bounds WebSocket sessions (ServeWebSocket).
	WebSocket WebSocket
	// Batches records batch objects (ServeBatch); nil answers the batch
	// APIs with 404.
	Batches BatchStore
	// BatchMaxBody bounds a batch request body (DefaultBatchMaxBody).
	BatchMaxBody int64
	// PublicURL returns the relay's configured public base URL, for links
	// the relay hands out (a message batch's results_url); nil or empty =
	// the address the request reached.
	PublicURL func(ctx context.Context) string
}

func (d *Deps) publicURL(ctx context.Context) string {
	if d.PublicURL == nil {
		return ""
	}
	return d.PublicURL(ctx)
}

var (
	_ Router = (*router.Router)(nil)
	_ Egress = (*egress.Client)(nil)
)

// Pipeline serves requests. It is safe for concurrent use.
type Pipeline struct {
	d        Deps
	table    atomic.Pointer[router.Table]
	inflight inflight
	// refreshed holds secrets renewed by a Refresher until the snapshot
	// carries them: upstream|credential → refreshedSecret.
	refreshed sync.Map
	// unsaved holds refreshed secrets the store refused, for
	// PersistPending: upstream|credential → unsavedSecret.
	unsaved            sync.Map
	secretSaveFailures atomic.Int64
	// sweeping: a SettleBatches run is in progress.
	sweeping atomic.Bool
}

// New returns a Pipeline. It panics when a required dependency is missing.
func New(d Deps) *Pipeline {
	if d.Snapshot == nil || d.Router == nil || d.Egress == nil || d.Sealer == nil {
		panic("pipeline: Snapshot, Router, Egress and Sealer are required")
	}
	if d.Plugins == nil {
		d.Plugins = func() *Chain { return nil }
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.NewID == nil {
		d.NewID = randomID
	}
	if d.ClientIP == nil {
		d.ClientIP = remoteIP
	}
	if d.Keepalive <= 0 {
		d.Keepalive = DefaultKeepalive
	}
	if d.KeepaliveDelay <= 0 {
		d.KeepaliveDelay = DefaultKeepaliveDelay
	}
	if d.MaxAttempts <= 0 {
		d.MaxAttempts = DefaultMaxAttempts
	}
	if d.MaxResponseBody <= 0 {
		d.MaxResponseBody = DefaultMaxResponseBody
	}
	if d.EventHookBudget <= 0 {
		d.EventHookBudget = DefaultEventHookBudget
	}
	if d.BatchMaxBody <= 0 {
		d.BatchMaxBody = DefaultBatchMaxBody
	}
	if d.MaxErrorBody <= 0 {
		d.MaxErrorBody = DefaultMaxErrorBody
	}
	return &Pipeline{d: d}
}

// OnCredential persists a router credential event. Wire it into
// router.Options.OnCredential so quarantine and manual holds survive a loss
// of the state backend.
func (p *Pipeline) OnCredential(ev router.CredEvent) {
	if p.d.Credentials == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.d.Credentials.SaveCredentialState(ctx, ev); err != nil {
		p.d.Logger.Error("persist credential state", "upstream", ev.UpstreamID,
			"credential", ev.CredentialID, "status", ev.Status, "err", err)
	}
}

// Wait blocks until every request being served has finished its
// accounting (meter settlement, finish hooks, record enqueued) or ctx is
// done. Call it after the HTTP server stopped and before closing what
// finishing requests use, such as the record queue.
func (p *Pipeline) Wait(ctx context.Context) error { return p.inflight.wait(ctx) }

// inflight counts requests between ServeProtocol entry and the end of
// finish. Unlike sync.WaitGroup it allows new requests while someone waits.
type inflight struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed when n drops to 0
}

func (f *inflight) add() {
	f.mu.Lock()
	if f.n == 0 {
		f.idle = make(chan struct{})
	}
	f.n++
	f.mu.Unlock()
}

func (f *inflight) done() {
	f.mu.Lock()
	if f.n--; f.n == 0 {
		close(f.idle)
	}
	f.mu.Unlock()
}

func (f *inflight) wait(ctx context.Context) error {
	f.mu.Lock()
	if f.n == 0 {
		f.mu.Unlock()
		return nil
	}
	idle, n := f.idle, f.n
	f.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("pipeline: %d requests still finishing: %w", n, ctx.Err())
	}
}

// snapshot returns the current snapshot and keeps the router's table in step.
func (p *Pipeline) snapshot() *Snapshot {
	s := p.d.Snapshot()
	if s == nil {
		s = &Snapshot{}
	}
	if s.Table != nil && p.table.Swap(s.Table) != s.Table {
		p.d.Router.SetTable(s.Table)
	}
	return s
}

func (p *Pipeline) chain() *Chain {
	if c := p.d.Plugins(); c != nil {
		return c
	}
	return &Chain{}
}

func randomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
