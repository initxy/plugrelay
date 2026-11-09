package router

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/initxy/plugrelay/internal/state"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Options tunes a Router. Zero fields take the documented defaults.
type Options struct {
	// Secret signs turn tokens. Required for IssueTurnToken to be meaningful.
	Secret []byte
	// Now and Rand are injectable for tests.
	Now  func() time.Time
	Rand *rand.Rand

	BreakerThreshold int           // consecutive upstream failures that open the breaker (5)
	BreakerWindow    time.Duration // failures older than this are forgotten (1m)
	BreakerCooldown  time.Duration // open duration before a probe (30s)
	ProbeTimeout     time.Duration // how long one half-open probe holds the slot (1m)

	BackoffBase time.Duration // first default cooldown (30s), doubled per strike
	BackoffMax  time.Duration // cap of the default cooldown (30m)

	SlotTTL time.Duration // lifetime of an in-flight slot if never released (30m)

	DefaultCacheTTL time.Duration // binding validity after a cached turn (5m)
	LongCacheTTL    time.Duration // validity when the 1h cache was written (1h)

	RecentPerUser int           // credentials remembered per user (3)
	RecentTTL     time.Duration // how long they are remembered (24h)

	TurnTokenMaxAge time.Duration // (6h)

	// OnCredential is called after the router changes a credential's live
	// status, so the caller can persist statuses that must survive a state
	// backend loss (quarantine, manual) and audit the change.
	OnCredential func(CredEvent)
}

// CredEvent describes a live credential status change.
type CredEvent struct {
	UpstreamID   string
	CredentialID string
	Status       CredStatus
	Model        string // set for a model-scoped cooldown
	Reason       string
	Until        time.Time
}

func (o *Options) defaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	if o.BreakerThreshold <= 0 {
		o.BreakerThreshold = 5
	}
	if o.RecentPerUser <= 0 {
		o.RecentPerUser = 3
	}
	def(&o.BreakerWindow, time.Minute)
	def(&o.BreakerCooldown, 30*time.Second)
	def(&o.ProbeTimeout, time.Minute)
	def(&o.BackoffBase, 30*time.Second)
	def(&o.BackoffMax, 30*time.Minute)
	def(&o.SlotTTL, 30*time.Minute)
	def(&o.DefaultCacheTTL, 5*time.Minute)
	def(&o.LongCacheTTL, time.Hour)
	def(&o.RecentTTL, 24*time.Hour)
	def(&o.TurnTokenMaxAge, 6*time.Hour)
}

// Router plans candidates and tracks live health. It is safe for concurrent
// use.
type Router struct {
	st    state.State
	opt   Options
	table atomic.Pointer[Table]

	randMu sync.Mutex

	latMu   sync.Mutex
	latency map[string]float64 // upstream/endpoint → TTFB EWMA in ms
}

// New returns a Router over the given state backend.
func New(st state.State, opt Options) *Router {
	opt.defaults()
	r := &Router{st: st, opt: opt, latency: map[string]float64{}}
	r.table.Store(&Table{})
	return r
}

// SetTable atomically replaces the routing snapshot. Requests already planned
// keep the candidates they got.
func (r *Router) SetTable(t *Table) {
	if t == nil {
		t = &Table{}
	}
	r.table.Store(t)
}

func (r *Router) now() time.Time { return r.opt.Now() }

// Key identifies a candidate: upstream, endpoint, credential and upstream
// model. Used for Query.Exclude, bindings and turn tokens.
func Key(c plugin.Candidate) string {
	return c.UpstreamID + "|" + c.EndpointID + "|" + c.CredentialID + "|" + c.UpstreamModel
}

func credKey(upstreamID, credID string) string { return upstreamID + "|" + credID }

// State keys. Credential IDs are scoped by upstream so two upstreams may reuse
// an ID.
func kCool(up, cred string) string         { return "rt:cd:" + credKey(up, cred) }
func kCoolModel(up, cred, m string) string { return "rt:cdm:" + credKey(up, cred) + "|" + m }
func kQuar(up, cred string) string         { return "rt:q:" + credKey(up, cred) }
func kManual(up, cred string) string       { return "rt:man:" + credKey(up, cred) }
func kRefresh(up, cred string) string      { return "rt:rf:" + credKey(up, cred) }
func kStrikes(up, cred string) string      { return "rt:strk:" + credKey(up, cred) }
func kSlot(up, cred string) string         { return "rt:slot:" + credKey(up, cred) }
func kInflight(up, cred string) string     { return "rt:inf:" + credKey(up, cred) }
func kBrFail(up string) string             { return "rt:bf:" + up }
func kBrOpen(up string) string             { return "rt:bo:" + up }
func kBrHalf(up string) string             { return "rt:bh:" + up }
func kBrProbe(up string) string            { return "rt:bp:" + up }
func kBinding(session string) string       { return "rt:sb:" + session }
func kRecent(user string) string           { return "rt:ur:" + user }

// mark is the value stored for cooldowns, quarantine and breaker state.
type mark struct {
	Reason string    `json:"r,omitempty"`
	Until  time.Time `json:"u,omitempty"`
	At     time.Time `json:"a"`
}

func (r *Router) getMark(ctx context.Context, key string) (mark, bool) {
	b, ok, err := r.st.Get(ctx, key)
	if err != nil || !ok {
		return mark{}, false
	}
	var m mark
	if json.Unmarshal(b, &m) != nil {
		return mark{}, false
	}
	return m, true
}

func (r *Router) setMark(ctx context.Context, key string, m mark, ttl time.Duration) error {
	b, _ := json.Marshal(m)
	return r.st.Set(ctx, key, b, ttl)
}

// counter reads an Incr-managed counter without changing it.
func (r *Router) counter(ctx context.Context, key string) int {
	n, err := r.st.Incr(ctx, key, 0, r.opt.SlotTTL)
	if err != nil || n < 0 {
		return 0
	}
	return int(n)
}

// ObserveLatency feeds one time-to-first-byte sample for an endpoint. It is
// node-local: each node ranks by what it sees.
func (r *Router) ObserveLatency(upstreamID, endpointID string, ttfb time.Duration) {
	ms := float64(ttfb) / float64(time.Millisecond)
	k := upstreamID + "|" + endpointID
	r.latMu.Lock()
	defer r.latMu.Unlock()
	if old, ok := r.latency[k]; ok {
		r.latency[k] = old*0.7 + ms*0.3
	} else {
		r.latency[k] = ms
	}
}

func (r *Router) latencyOf(upstreamID, endpointID string) float64 {
	r.latMu.Lock()
	defer r.latMu.Unlock()
	return r.latency[upstreamID+"|"+endpointID]
}

func (r *Router) lookupCred(up, cred string) (Cred, bool) {
	u, ok := r.table.Load().Upstreams[up]
	if !ok {
		return Cred{}, false
	}
	for _, c := range u.Credentials {
		if c.ID == cred {
			return c, true
		}
	}
	return Cred{}, false
}

// Acquire takes an in-flight slot on the candidate's credential. ok=false
// means the credential is at MaxConcurrency and the pipeline should move on.
// release must be called exactly once when the attempt ends; extra calls are
// ignored.
func (r *Router) Acquire(ctx context.Context, c plugin.Candidate) (release func(), ok bool, err error) {
	cr, found := r.lookupCred(c.UpstreamID, c.CredentialID)
	rel := func() {}
	if found && cr.MaxConcurrency > 0 {
		rel, ok, err = r.st.Acquire(ctx, kSlot(c.UpstreamID, c.CredentialID), cr.MaxConcurrency, r.opt.SlotTTL)
		if err != nil || !ok {
			return nil, false, err
		}
	}
	key := kInflight(c.UpstreamID, c.CredentialID)
	if _, err := r.st.Incr(ctx, key, 1, r.opt.SlotTTL); err != nil {
		rel()
		return nil, false, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// A fresh context: the request context is usually done by now.
			bg := context.WithoutCancel(ctx)
			if n, err := r.st.Incr(bg, key, -1, r.opt.SlotTTL); err == nil && n < 0 {
				_, _ = r.st.Incr(bg, key, -n, r.opt.SlotTTL)
			}
			rel()
		})
	}, true, nil
}
