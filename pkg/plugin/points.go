package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
)

// ---------------------------------------------------------------- provider

// Endpoint is one protocol entry of an upstream.
type Endpoint struct {
	ID       string          `json:"id" yaml:"id"`
	Protocol string          `json:"protocol" yaml:"protocol"` // codec name
	BaseURL  string          `json:"base_url" yaml:"base_url"`
	Caps     ir.Capabilities `json:"capabilities" yaml:"capabilities"`
	// Issuer groups upstreams that accept each other's private state.
	// Two endpoints with the same model name but different issuers never
	// see each other's signatures.
	Issuer string `json:"issuer" yaml:"issuer"`
	// Path overrides the codec's upstream path template, with {model}
	// substituted (Azure deployments, vendor prefixes).
	Path string `json:"path,omitempty" yaml:"path"`
	// Query is appended to every request (api-version=...).
	Query map[string]string `json:"query,omitempty" yaml:"query"`
	// DropBetas lists anthropic-beta values to remove before sending.
	DropBetas []string `json:"drop_betas,omitempty" yaml:"drop_betas"`
}

// Credential is a secret used to call an upstream.
type Credential struct {
	ID         string
	UpstreamID string
	Kind       string // shared | personal | service
	OwnerID    string // user id for personal credentials
	Secret     string // decrypted
	// Proxy is the fixed egress for this credential ("" = direct). If the
	// proxy is down the attempt fails; it never falls back to direct.
	Proxy          string
	MaxConcurrency int
	Meta           map[string]string
}

// Upstream is a configured upstream instance.
type Upstream struct {
	ID        string
	Provider  string // provider plugin id
	Endpoints []Endpoint
	// Config is the provider-specific configuration (manifest fields).
	Config map[string]any
}

// Outgoing is the request a provider prepares for one attempt.
type Outgoing struct {
	Upstream   *Upstream
	Endpoint   *Endpoint
	Credential *Credential
	Model      string // upstream model name
	// Req is pre-built by the core with method, URL from the endpoint and
	// codec, body and forwarded headers. Prepare sets auth and owned headers.
	Req *http.Request
	// Client is this attempt's egress: the credential's fixed proxy (never
	// bypassed), the SSRF guard and the attempt's timers. A provider that
	// calls out itself while preparing or sending (a token exchange, a
	// Sender) must use it with the context it was given. The core always
	// sets it.
	Client *http.Client
}

// Outcome is what came back from one attempt, for Classify.
type Outcome struct {
	Upstream   *Upstream
	Endpoint   *Endpoint
	Credential *Credential
	Status     int         // 0 when the request never got a response
	Header     http.Header // response headers
	Body       []byte      // error body (bounded); nil on success
	Err        error       // transport error
	// Signal is the codec's reading of the error body.
	Signal *ir.GwError
	// Sent reports whether the request reached the upstream.
	Sent bool
}

// Provider adapts one kind of upstream.
type Provider interface {
	Plugin
	// Endpoints returns the effective endpoints of an upstream instance.
	Endpoints(u *Upstream) ([]Endpoint, error)
	// OwnedHeaders lists request headers this provider controls. Client
	// headers with these names are dropped; only the provider's values go out.
	OwnedHeaders(u *Upstream) []string
	// Prepare applies authentication and provider-specific headers.
	Prepare(ctx context.Context, o *Outgoing) error
	// Classify turns an outcome into a verdict the core executes.
	Classify(o *Outcome) Verdict
}

// ModelLister is implemented by providers that can list upstream models.
type ModelLister interface {
	ListModels(ctx context.Context, u *Upstream, c *Credential) ([]ir.ModelInfo, error)
}

// Refresher is implemented by providers whose credentials expire.
// The core schedules calls and holds a cluster-wide lock per credential.
type Refresher interface {
	Refresh(ctx context.Context, u *Upstream, c *Credential) (secret string, expires time.Time, err error)
}

// QuotaProber reports an upstream credential's remaining quota. The core's
// job.quotaprobe calls it periodically; a provider that cannot probe a
// given upstream returns an error wrapping errors.ErrUnsupported
// (ErrNoQuota), which the job skips silently.
type QuotaProber interface {
	Quota(ctx context.Context, u *Upstream, c *Credential) (Quota, error)
}

// ErrNoQuota reports that an upstream has no quota probe.
var ErrNoQuota = fmt.Errorf("no quota probe for this upstream: %w", errors.ErrUnsupported)

// Quota is a credential's remaining allowance. A Remaining of 0 means used
// up: the core keeps the credential out of rotation until ResetAt.
type Quota struct {
	Remaining float64 `json:"remaining"` // fraction 0..1, -1 if unknown
	// Left and Limit are the absolute amounts behind Remaining in the
	// upstream's unit (credits, USD, requests); 0 Limit means not reported.
	Left    float64   `json:"left,omitempty"`
	Limit   float64   `json:"limit,omitempty"`
	ResetAt time.Time `json:"reset_at,omitzero"`
}

// ---------------------------------------------------------------- verdict

// Cause is why an attempt failed.
type Cause string

const (
	CauseNone        Cause = ""
	CauseRequest     Cause = "request"      // the request itself is bad; same everywhere
	CauseTransient   Cause = "transient"    // network, 5xx — not the credential's fault
	CauseRateLimit   Cause = "rate_limit"   // throttled or overloaded
	CauseQuota       Cause = "quota"        // credential out of quota until reset
	CauseAuthExpired Cause = "auth_expired" // needs refresh
	CauseBanned      Cause = "banned"       // suspected account suspension
	CauseContent     Cause = "content"      // content policy rejection
	CauseUnknown     Cause = "unknown"
)

// RetryMode says what the core does next.
type RetryMode string

const (
	RetryNone      RetryMode = "none"      // return the error to the client
	RetryNext      RetryMode = "next"      // try the next candidate
	RetryDowngrade RetryMode = "downgrade" // resend to the same upstream with Downgrade applied
)

// CredAction says what happens to the credential.
type CredAction string

const (
	CredKeep       CredAction = "keep"
	CredCooldown   CredAction = "cooldown"   // until Until
	CredRefresh    CredAction = "refresh"    // call Refresher, then retry
	CredQuarantine CredAction = "quarantine" // out of rotation until a human acts
	CredManual     CredAction = "manual"     // needs human attention, stays out
)

// Verdict is a provider's structured judgment of a failed attempt.
type Verdict struct {
	Cause      Cause
	Retry      RetryMode
	Credential CredAction
	Until      time.Time // cooldown end; zero = provider default
	// ModelScope limits a cooldown to this model only.
	ModelScope bool
	// Downgrade lists gjson path → new value edits for RetryDowngrade.
	Downgrade map[string]json.RawMessage
	// ClientErr is what the client sees if no retry succeeds.
	ClientErr *ir.GwError
	// Sent reports whether the request reached the upstream; a request
	// that was not sent is always safe to retry.
	Sent bool
}

// ---------------------------------------------------------------- auth

// Authenticator identifies the caller.
type Authenticator interface {
	Plugin
	// Authenticate returns the caller, or (nil, nil) to let the next
	// authenticator try. An error rejects the request.
	Authenticate(r *http.Request) (*Caller, error)
}

// ---------------------------------------------------------------- middleware

// RequestHook runs once per request before routing.
type RequestHook interface {
	OnRequest(c *Call) (*Reply, error)
}

// AttemptHook runs once per attempt before sending.
type AttemptHook interface {
	OnAttempt(c *Call, a *Attempt) error
}

// EventAction is what an event hook decides.
type EventAction int

const (
	EventPass  EventAction = iota // forward (possibly modified)
	EventDrop                     // swallow this event
	EventAbort                    // end the stream with Call's error
)

// EventHook runs for every streamed event. Built-in plugins only; each
// call has a strict time budget.
type EventHook interface {
	OnEvent(c *Call, ev *ir.Event) (EventAction, error)
}

// EventHolder is an EventHook that can hold streamed events back from the
// client. After each OnEvent the pipeline asks Held how many of the most
// recent events this hook saw, the current one included, must not reach
// the client yet; older ones go out, in order. Held events are sent when
// released or when the stream ends normally, and dropped when the stream is
// aborted or retried. Holding applies to passthrough frames too: a frame
// waits until every event in it is released.
type EventHolder interface {
	EventHook
	Held(c *Call) int
}

// ResponseHook checks a complete, non-streamed response before the client
// gets it. Built-in plugins only. It may edit r and report edited (the
// response is then re-encoded, even in passthrough); returning an
// *ir.GwError answers the client with that error instead. Streams go
// through EventHook.
type ResponseHook interface {
	OnResponse(c *Call, r *ir.Response) (edited bool, err error)
}

// FinishHook runs once per request after the response ended, in reverse
// order, only for plugins whose OnRequest ran.
type FinishHook interface {
	OnFinish(c *Call, r *Result)
}

// BodyMutator lets a middleware edit the outgoing body in the patch tier.
// Only paths declared in Info.Mutates may be touched.
type BodyMutator interface {
	MutateBody(c *Call, a *Attempt, body []byte) ([]byte, error)
}

// ---------------------------------------------------------------- selector

// Candidate is one way to serve a request: an upstream endpoint plus a
// credential and the upstream's model name.
type Candidate struct {
	UpstreamID    string
	EndpointID    string
	CredentialID  string
	Protocol      string
	UpstreamModel string
	Priority      int
	Weight        int
	// SameProtocol is set when the endpoint speaks the client's protocol.
	SameProtocol bool
	// Sticky is set when the session is bound to this credential.
	Sticky bool
	// Fidelity of converting the request for this endpoint.
	Fidelity ir.Fidelity
	// Stats are live router observations.
	LatencyMs float64 // recent time-to-first-byte EWMA
	InFlight  int
	// PriceInMicro and PriceOutMicro are the upstream model's cost rates
	// per million tokens in micro-units (price rule, else the manifest's
	// list price); 0 when unpriced.
	PriceInMicro  int64
	PriceOutMicro int64
	QuotaReset    time.Time
}

// Selector orders candidates. It may only reorder; it can never add a
// candidate the router filtered out.
type Selector interface {
	Plugin
	Rank(c *Call, cands []Candidate) []Candidate
}

// ---------------------------------------------------------------- meter

// Reservation is a pre-authorized amount held for a request.
type Reservation struct {
	ID          string
	AmountMicro int64
}

// Meter owns quota and balance. All three calls are idempotent per
// request id: Settle and Refund may be called again and apply once.
type Meter interface {
	Plugin
	Reserve(c *Call, estimateMicro int64) (*Reservation, error)
	Settle(c *Call, r *Reservation, usage ir.Usage, costMicro int64) error
	Refund(c *Call, r *Reservation) error
}

// DeferredMeter is implemented by meters that hold money for work billed
// long after the request that starts it (batches). Hold reserves estimate
// under id for the call's payer, refusing like Reserve. Charge ends id's
// hold, if one is left, and charges chargeMicro once per id, in full: also
// when no hold was granted or it lapsed, and below zero like an overrun.
// A zero charge only releases.
type DeferredMeter interface {
	Hold(c *Call, id string, estimateMicro int64) error
	Charge(c *Call, id string, usage ir.Usage, chargeMicro int64) error
}

// QuotaHeaders is implemented by meters that report usage to clients in
// response headers.
type QuotaHeaders interface {
	Headers(c *Call) http.Header
}

// ---------------------------------------------------------------- sink

// AttemptRecord is one upstream try inside a request record.
type AttemptRecord struct {
	N            int    `json:"n"`
	UpstreamID   string `json:"upstream_id"`
	EndpointID   string `json:"endpoint_id"`
	CredentialID string `json:"credential_id"`
	Protocol     string `json:"protocol"`
	// Model is the client-facing model the attempt served when the client
	// asked for a preset; empty when it asked for the model itself.
	Model         string         `json:"model,omitempty"`
	UpstreamModel string         `json:"upstream_model"`
	Tier          Tier           `json:"tier"`
	Status        int            `json:"status"`
	Cause         Cause          `json:"cause,omitempty"`
	Sent          bool           `json:"sent"`
	TTFB          time.Duration  `json:"ttfb"`
	Duration      time.Duration  `json:"duration"`
	Usage         ir.Usage       `json:"usage"`
	CostMicro     int64          `json:"cost_micro"`
	Diagnostic    *ir.Diagnostic `json:"diagnostic,omitempty"`
	Error         string         `json:"error,omitempty"`
	Charge        *Charge        `json:"charge,omitempty"` // on the attempt the caller was billed for
	// BytesIn and BytesOut count request and response body bytes; set by
	// native passthrough, where no usage is known.
	BytesIn  int64 `json:"bytes_in,omitempty"`
	BytesOut int64 `json:"bytes_out,omitempty"`
}

// Charge is how a request's charge was computed: Rates (micro-units per
// million tokens, or per unit for media units) are the list rates times
// Multiplier, each rounded half up; the charge is the sum of tokens × rate
// and units × rate, rounded half up.
type Charge struct {
	Rates Rates `json:"rates"`
	// Multiplier is the caller's group price multiplier (1 without one).
	Multiplier float64 `json:"multiplier"`
	// Source is where the list rates came from: "model" (the client-facing
	// model's price) or "upstream" (the delivered attempt's cost rate).
	Source string `json:"source"`
	// BatchDiscount is the fraction taken off batch API usage; Rates
	// already include it.
	BatchDiscount float64 `json:"batch_discount,omitempty"`
}

// Rates are token prices per million tokens in micro-units, one per usage
// bucket, plus unit prices of the media operations in micro-units per unit
// (keys Unit*).
type Rates struct {
	Input        int64            `json:"input"`
	CacheRead    int64            `json:"cache_read"`
	CacheWrite5m int64            `json:"cache_write_5m"`
	CacheWrite1h int64            `json:"cache_write_1h"`
	Output       int64            `json:"output"`
	Units        map[string]int64 `json:"units,omitempty"`
}

// Unit price keys of Rates.Units. An image may be priced per size
// ("image:1024x1024") or per size and quality ("image:1024x1024:hd"); the
// most specific key set wins.
const (
	UnitImage       = "image"        // per image generated
	UnitAudioSecond = "audio_second" // per second of input audio
	UnitKChars      = "1k_chars"     // per 1000 input characters (speech)
	UnitSearch      = "search_unit"  // per rerank search unit
)

// ValidUnit reports whether k is a unit price key.
func ValidUnit(k string) bool {
	if k == UnitAudioSecond || k == UnitKChars || k == UnitImage || k == UnitSearch {
		return true
	}
	rest, ok := strings.CutPrefix(k, UnitImage+":")
	if !ok || rest == "" {
		return false
	}
	size, quality, two := strings.Cut(rest, ":")
	return size != "" && (!two || (quality != "" && !strings.Contains(quality, ":")))
}

// ImageRate is the per-image rate for size and quality.
func ImageRate(units map[string]int64, size, quality string) int64 {
	if size != "" && quality != "" {
		if r, ok := units[UnitImage+":"+size+":"+quality]; ok {
			return r
		}
	}
	if size != "" {
		if r, ok := units[UnitImage+":"+size]; ok {
			return r
		}
	}
	return units[UnitImage]
}

// Scale multiplies every rate by m, rounding half up. m is taken at its
// shortest decimal form, so 1.1 scales by exactly 11/10. m <= 0 is 1.
func (r Rates) Scale(m float64) Rates {
	if m <= 0 || m == 1 {
		return r
	}
	f, ok := new(big.Rat).SetString(strconv.FormatFloat(m, 'f', -1, 64))
	if !ok {
		return r
	}
	s := func(rate int64) int64 { return RoundMicro(new(big.Rat).Mul(new(big.Rat).SetInt64(rate), f)) }
	out := Rates{Input: s(r.Input), CacheRead: s(r.CacheRead), CacheWrite5m: s(r.CacheWrite5m),
		CacheWrite1h: s(r.CacheWrite1h), Output: s(r.Output)}
	if len(r.Units) > 0 {
		out.Units = make(map[string]int64, len(r.Units))
		for k, v := range r.Units {
			out.Units[k] = s(v)
		}
	}
	return out
}

// UnitCharge is the part of a charge at r that prices u's media units
// (images, audio seconds, characters, search units) rather than tokens,
// rounded half up.
func (r Rates) UnitCharge(u ir.Usage) int64 {
	if len(r.Units) == 0 {
		return 0
	}
	sum := new(big.Rat)
	add := func(n, rate, per int64) {
		if n > 0 && rate > 0 {
			sum.Add(sum, new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(n), big.NewInt(rate)), big.NewInt(per)))
		}
	}
	add(u.Images, ImageRate(r.Units, u.ImageSize, u.ImageQuality), 1)
	add(u.AudioMs, r.Units[UnitAudioSecond], 1_000)
	add(u.Chars, r.Units[UnitKChars], 1_000)
	add(u.SearchUnits, r.Units[UnitSearch], 1)
	return RoundMicro(sum)
}

// RoundMicro rounds a non-negative amount to whole micro-units, halves up.
func RoundMicro(x *big.Rat) int64 {
	n := new(big.Int).Mul(x.Num(), big.NewInt(2))
	n.Add(n, x.Denom())
	return n.Quo(n, new(big.Int).Mul(x.Denom(), big.NewInt(2))).Int64()
}

// Record is everything known about a finished request.
type Record struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Caller    Caller    `json:"caller"`
	Protocol  string    `json:"protocol"`
	Model     string    `json:"model"`
	Stream    bool      `json:"stream"`
	SessionID string    `json:"session_id,omitempty"`
	ClientUA  string    `json:"client_ua,omitempty"`
	// Client and ClientVersion name the client program (pkg/clientid
	// ids); empty when the record was made without the request headers.
	Client        string          `json:"client,omitempty"`
	ClientVersion string          `json:"client_version,omitempty"`
	ClientIP      string          `json:"client_ip,omitempty"`
	Status        int             `json:"status"`
	ErrorKind     ir.ErrorKind    `json:"error_kind,omitempty"`
	Error         string          `json:"error,omitempty"`
	Attempts      []AttemptRecord `json:"attempts"`
	Usage         ir.Usage        `json:"usage"`        // of the delivered attempt
	CostMicro     int64           `json:"cost_micro"`   // operator cost, all sent attempts
	ChargeMicro   int64           `json:"charge_micro"` // billed to the caller
	TTFB          time.Duration   `json:"ttfb"`
	Duration      time.Duration   `json:"duration"`
	Retried       bool            `json:"retried,omitempty"` // client resend of an earlier request
	// Body holds request/response text when content logging is enabled.
	Body *RecordBody `json:"body,omitempty"`
}

// RecordBody is optional content captured for conversation logs.
type RecordBody struct {
	Request  json.RawMessage `json:"request,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

// Sink consumes finished records asynchronously and may be called from
// several goroutines at once. The queue is bounded: when full, records for
// a FailOpen sink are dropped and counted; a FailClosed sink (a ledger)
// gets every record, and a failed Consume is retried, so a ledger sink must
// treat a Record.ID it already stored as done.
type Sink interface {
	Plugin
	Consume(ctx context.Context, r *Record) error
}

// ---------------------------------------------------------------- job & admin

// Job runs on a schedule, off the request path.
type Job interface {
	Plugin
	// Every is the interval between runs.
	Every() time.Duration
	Run(ctx context.Context) error
}

// AdminRoute is an HTTP endpoint a plugin mounts under /api/plugins/<id>/.
type AdminRoute struct {
	Method  string
	Path    string
	Handler http.HandlerFunc
	// Role required: "admin", "member" or "" for any authenticated user.
	Role string
	// Public routes are served without a console login (payment webhooks,
	// sign-up); the handler authenticates the request itself. Only
	// compiled-in plugins can declare them.
	Public bool
	// Reauth: a console session must have re-authenticated recently, as
	// for deleting a user (403 reauth_required otherwise).
	Reauth bool
}

// Admin mounts plugin-owned management API.
type Admin interface {
	Plugin
	Routes() []AdminRoute
}

// UserDeleteHook is a plugin that ends what a user has running through it
// before the user is deleted (a subscription a payment provider bills). An
// error stops the deletion; the admin is shown its message. When the admin
// forced the deletion (ForcedDelete), a hook ends what it can and lets it
// through.
type UserDeleteHook interface {
	Plugin
	BeforeUserDelete(ctx context.Context, userID string) error
}

type forcedDeleteKey struct{}

// WithForcedDelete marks ctx as that of a user deletion the admin forced.
func WithForcedDelete(ctx context.Context) context.Context {
	return context.WithValue(ctx, forcedDeleteKey{}, true)
}

// ForcedDelete reports whether ctx is that of a forced user deletion.
func ForcedDelete(ctx context.Context) bool {
	v, _ := ctx.Value(forcedDeleteKey{}).(bool)
	return v
}

// SettingsCheck is a plugin that vets a change to a plugin's settings at
// a scope before it is stored: values are all the settings the change
// leaves set there. An error refuses the change; the admin is shown its
// message.
type SettingsCheck interface {
	Plugin
	CheckSettings(ctx context.Context, pluginID string, scope Scope, values map[string]json.RawMessage) error
}
