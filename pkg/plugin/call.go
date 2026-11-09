package plugin

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
)

// Caller is the authenticated identity behind a request.
type Caller struct {
	KeyID   string `json:"key_id"`
	KeyName string `json:"key_name,omitempty"`
	UserID  string `json:"user_id,omitempty"`
	GroupID string `json:"group_id,omitempty"`
}

// Call is the per-request context handed to request-scoped plugins.
// Fields are read-only for plugins unless a method says otherwise.
type Call struct {
	Context  context.Context
	ID       string
	Started  time.Time
	Caller   *Caller
	Protocol string // client protocol (codec name)
	Model    string // model name as requested by the client
	Stream   bool
	ClientIP string
	Header   http.Header // client request headers (read-only)

	// SessionID is the resolved session identifier used for stickiness.
	SessionID string
	// BatchIndex is the 1-based position of the request being checked
	// within a batch being created; 0 outside batches.
	BatchIndex int
	// EventIndex is the 1-based position, within its session, of the
	// Realtime client event being checked as a request; 0 otherwise.
	EventIndex int
	// EstimateTokens is the request's input estimate plus its output
	// limit, as the core estimated it for Meter.Reserve; 0 when unknown.
	EstimateTokens int64
	// UnitPriced reports a media operation that may be priced per unit
	// rather than per token: image generation or edit, speech,
	// transcription, rerank. Set with EstimateTokens.
	UnitPriced bool
	// UnitChargeMicro is the part of the charge handed to Meter.Settle
	// that prices media units (Rates.UnitCharge: images, audio seconds,
	// characters, search units) rather than tokens; set before Settle.
	UnitChargeMicro int64
	// UnscannedAudio reports that the request carries audio input that
	// content checks cannot read (a Realtime audio event).
	UnscannedAudio bool

	// settings resolves merged settings for the plugin being invoked.
	settings func(pluginID, key string) Value
	// request decodes the IR lazily; nil in passthrough until asked.
	request func() (*ir.Request, error)
	current string
	values  map[string]any
	notesMu sync.Mutex
	notes   []string
}

// NewCall is used by the core to build a Call.
func NewCall(ctx context.Context, id string, settings func(pluginID, key string) Value, request func() (*ir.Request, error)) *Call {
	return &Call{Context: ctx, ID: id, Started: time.Now(), settings: settings, request: request, values: map[string]any{}}
}

// WithPlugin returns the Call bound to a plugin so Setting reads that
// plugin's settings. The core calls this before each hook.
func (c *Call) WithPlugin(id string) *Call { c.current = id; return c }

// Setting returns the invoked plugin's setting, merged Global → Group → User → Key.
func (c *Call) Setting(key string) Value {
	if c.settings == nil {
		return Value{}
	}
	return c.settings(c.current, key)
}

// Request returns the decoded request. Decoding a passthrough request costs
// a parse but does not change the bytes sent upstream.
func (c *Call) Request() (*ir.Request, error) {
	if c.request == nil {
		return nil, ir.Errorf(ir.ErrInternal, "request not available")
	}
	return c.request()
}

// Set stores a per-request value for later hooks of the same plugin.
func (c *Call) Set(key string, v any) { c.values[c.current+"/"+key] = v }

// Get reads a value stored with Set.
func (c *Call) Get(key string) (any, bool) { v, ok := c.values[c.current+"/"+key]; return v, ok }

// Note adds a line to the request log: the core appends the notes to the
// diagnostic of the attempt that answered (else the last attempt) when the
// request finishes. Notes added in OnFinish come too late.
func (c *Call) Note(text string) {
	c.notesMu.Lock()
	c.notes = append(c.notes, text)
	c.notesMu.Unlock()
}

// Notes returns the notes added so far.
func (c *Call) Notes() []string {
	c.notesMu.Lock()
	defer c.notesMu.Unlock()
	return append([]string(nil), c.notes...)
}

// Reply short-circuits a request: a pre-hook returning one stops the chain
// and the core sends it instead of calling an upstream.
type Reply struct {
	// Error, when set, is encoded in the client's protocol.
	Error *ir.GwError
	// Response, when set, is a complete model response (cache hit, canned answer).
	Response *ir.Response
}

// Attempt is one try against one candidate.
type Attempt struct {
	N             int // 1-based attempt number
	Candidate     Candidate
	Tier          Tier // how the request is being transformed
	UpstreamModel string
	// Header is the outgoing request header; middleware may add headers
	// that the provider does not own.
	Header http.Header
}

// BodyProtocol is the protocol of the body a BodyMutator edits on attempt
// a: its endpoint's, else (a candidate without one) the client's.
func BodyProtocol(c *Call, a *Attempt) string {
	if a != nil && a.Candidate.Protocol != "" {
		return a.Candidate.Protocol
	}
	return c.Protocol
}

// Tier is the processing level of an attempt.
type Tier string

const (
	TierPassthrough Tier = "passthrough" // body byte-for-byte
	TierPatch       Tier = "patch"       // named fields edited in place
	TierConvert     Tier = "convert"     // decoded to IR and re-encoded
)

// Result summarizes a finished request for OnFinish and sinks.
type Result struct {
	Status    int
	Err       *ir.GwError
	Usage     ir.Usage
	Attempts  int
	Delivered bool // some content reached the client
}
