// Package protocol defines the contract every wire protocol implements and a
// registry of the built-in codecs.
//
// A codec serves both directions of one protocol: the client side (decode
// requests, encode responses, streams, errors and model lists) and the
// upstream side (encode requests, decode responses, streams and errors).
// Codecs only move fields between the wire format and package ir; they never
// decide what to drop when the other side lacks a capability — that belongs to
// the generic conversion steps. Codecs must not mention any client or
// upstream product by name in their logic.
package protocol

import (
	"net/http"
	"sort"
	"sync"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/sse"
)

// Op is what an inbound route does.
type Op string

const (
	OpGenerate    Op = "generate"     // model call (stream or not)
	OpCountTokens Op = "count_tokens" // token counting
	OpModels      Op = "models"       // model listing
	OpHello       Op = "hello"        // liveness probe a client sends at startup
	// Media operations; served like OpGenerate.
	OpEmbed      Op = "embed"
	OpImage      Op = "image"      // image generation
	OpImageEdit  Op = "image_edit" // image edit (multipart upload)
	OpTranscribe Op = "transcribe" // speech to text (multipart upload)
	OpTranslate  Op = "translate"  // speech to English text (multipart upload)
	OpSpeech     Op = "speech"     // text to speech (binary response)
	OpRerank     Op = "rerank"     // documents scored against a query
)

// Route is one client-facing endpoint. Path is the canonical form; the HTTP
// server additionally accepts it without a leading "/v1" or "/v1beta" and with
// Azure-style query parameters. A "{model}" segment and a ":{action}" suffix
// are captured into Inbound by Sniff.
//
// A ":{action}" may also be a fixed action ("{model}:embedContent"); such
// a route wins over a ":{action}" route on the same path.
type Route struct {
	Method string
	Path   string // e.g. "/v1/messages", "/v1beta/models/{model}:{action}"
	Op     Op
	// Upload marks a multipart/form-data route whose body is not read into
	// memory: the pipeline reads the form fields before the first file,
	// hands them to Sniff and DecodeRequest as a JSON object (repeated
	// fields as arrays, files as {"filename","content_type"}), and streams
	// the parts to the upstream.
	Upload bool
}

// SessionHints are the conversation identifiers a request carries, already
// extracted from protocol-specific headers and body fields.
type SessionHints struct {
	SessionID string // explicit client session id (header)
	AgentID   string // sub-agent id; bound separately from the parent
	CacheKey  string // body-level session/cache key
	UserTag   string // body-level user/session tag
	// TurnState is a relay-issued sticky token the client echoed back.
	TurnState string
	// PrefixHash hashes the conversation's stable start (system + first
	// user message), cache markers and injected environment blocks removed.
	PrefixHash string
}

// Inbound is what can be read from a request without decoding it fully;
// passthrough relies on it.
type Inbound struct {
	Op      Op
	Model   string
	Action  string // path action, e.g. "streamGenerateContent"
	Stream  bool
	Session SessionHints
}

// Codec is one wire protocol. Implementations are stateless and safe for
// concurrent use; per-response state lives in the stream encoder/decoder.
type Codec interface {
	// Name is the protocol id used in config and manifests, such as
	// "anthropic" or "openai-chat".
	Name() string
	// Capabilities is everything the protocol can express.
	Capabilities() ir.Capabilities
	// Routes lists the client-facing endpoints.
	Routes() []Route

	// --- client side ---

	// Sniff reads the routing facts from a raw request cheaply (gjson).
	Sniff(r *http.Request, route Route, body []byte) (Inbound, error)
	// DecodeRequest parses a client request into IR and computes its needs.
	DecodeRequest(r *http.Request, in Inbound, body []byte) (*ir.Request, ir.Needs, error)
	// EncodeResponse renders a complete response for the client.
	EncodeResponse(req *ir.Request, resp *ir.Response) ([]byte, error)
	// NewStreamEncoder renders IR events as the client's stream.
	NewStreamEncoder(w sse.Writer, req *ir.Request) StreamEncoder
	// EncodeError renders a relay-generated error (non-streaming).
	EncodeError(e *ir.GwError) (status int, header http.Header, body []byte)
	// EncodeModels renders a model list in the format this request expects.
	EncodeModels(r *http.Request, models []ir.ModelInfo) (contentType string, body []byte)
	// CountTokens answers a token-count request locally when no upstream
	// can; it may estimate.
	CountTokens(req *ir.Request) ([]byte, error)

	// --- upstream side ---

	// UpstreamPath is the path (and query) to call on an upstream base URL.
	UpstreamPath(req *ir.Request, in Inbound) string
	// EncodeRequest renders IR as a request body for an upstream.
	EncodeRequest(req *ir.Request) ([]byte, error)
	// DecodeResponse parses an upstream's complete response.
	DecodeResponse(body []byte) (*ir.Response, error)
	// NewStreamDecoder parses an upstream's stream into IR events. It is
	// also used read-only beside passthrough to observe usage and the end
	// of stream.
	NewStreamDecoder() StreamDecoder
	// DecodeError extracts the signal and message from an upstream error.
	DecodeError(status int, header http.Header, body []byte) *ir.GwError

	// --- same-protocol patching (byte-preserving, sjson) ---

	// SetModel rewrites the model name. For protocols that carry the model
	// in the path it returns body unchanged and the pipeline uses
	// UpstreamPath with the new model.
	SetModel(body []byte, model string) ([]byte, error)
	// StripOpaque removes all upstream-private state from history, used when
	// a conversation moves to an upstream with a different issuer.
	StripOpaque(body []byte) (out []byte, removed int, err error)
}

// StreamEncoder writes IR events in the client's protocol.
type StreamEncoder interface {
	// Encode writes one event. EvError and EvDone end the stream; later
	// calls are no-ops.
	Encode(ev *ir.Event) error
	// Keepalive writes the protocol's idle keepalive.
	Keepalive() error
	// Usage returns the usage reported so far.
	Usage() ir.Usage
}

// StreamDecoder turns an upstream's SSE events (or JSON stream chunks,
// framed by the sse package) into IR events.
type StreamDecoder interface {
	Feed(ev sse.Event) ([]ir.Event, error)
	// Finish is called at end of input; it returns trailing events and
	// reports whether the stream ended with the protocol's terminal event.
	Finish() (events []ir.Event, complete bool)
	// Usage returns the usage reported so far.
	Usage() ir.Usage
}

// BodyStreamer is implemented by codecs whose successful responses may be
// binary audio or an event stream the relay does not interpret. When
// StreamsBody reports true for a same-protocol response, the pipeline
// copies the body to the client as it arrives instead of reading it whole.
type BodyStreamer interface {
	StreamsBody(in Inbound, contentType string) bool
}

// StickyHeader is implemented by codecs whose clients echo a relay-issued
// response header back on the next request of the same turn.
type StickyHeader interface {
	StickyHeaderName() string
}

var (
	mu     sync.RWMutex
	codecs = map[string]Codec{}
)

// Register adds a codec. Built-in codecs register from init.
func Register(c Codec) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := codecs[c.Name()]; dup {
		panic("protocol: duplicate codec " + c.Name())
	}
	codecs[c.Name()] = c
}

// Get returns a registered codec.
func Get(name string) (Codec, bool) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := codecs[name]
	return c, ok
}

// All returns every registered codec sorted by name.
func All() []Codec {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Codec, 0, len(codecs))
	for _, c := range codecs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
