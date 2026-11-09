package ir

// Needs is what a request requires from an upstream endpoint. Codecs compute
// it while decoding; the router drops endpoints that can neither provide a
// need natively nor have it bridged by a conversion step.
type Needs struct {
	ExplicitCache bool // carries cache breakpoints
	Cache1h       bool // some breakpoint asks for a 1h TTL
	FunctionTools bool
	FreeformTools bool
	ServerTools   []string // server tool types offered (web_search_20250305, ...)
	// ServerToolsRequired is the subset of ServerTools the request depends
	// on: forced by tool_choice, or a server tool call the model is in the
	// middle of. Offered-only server tools may be dropped by conversion.
	ServerToolsRequired []string
	DeferredTools       bool
	Reasoning           bool
	Images              bool
	Documents           bool
	ToolResultImgs      bool // images inside tool results
	OpaqueState         bool // history carries upstream-private state
	ServerState         bool // relies on previous_response_id
	Compaction          bool // remote compaction items/triggers
	// FillInMiddle: the request carries a suffix and asks for the text
	// between prompt and suffix.
	FillInMiddle bool
	// Modality is the operation asked for; "" is text generation.
	Modality Modality
	// EmbedTokens: embeddings input given as token ids.
	EmbedTokens    bool
	InputTokensEst int // rough input size, for context-window filtering
}

// CacheMode is how an endpoint does prompt caching.
type CacheMode string

const (
	CacheExplicit CacheMode = "explicit" // breakpoints in the request (Anthropic)
	CacheKeyed    CacheMode = "keyed"    // automatic, routed by a cache key (OpenAI)
	CacheImplicit CacheMode = "implicit" // automatic, no key (DeepSeek, Gemini)
	CacheNone     CacheMode = "none"
)

// Capabilities is what a protocol can express or an endpoint supports.
// An endpoint's effective capabilities are the intersection of its codec's
// and its own declaration.
type Capabilities struct {
	Cache         CacheMode `json:"cache" yaml:"cache"`
	Cache1h       bool      `json:"cache_1h" yaml:"cache_1h"`
	FunctionTools bool      `json:"function_tools" yaml:"function_tools"`
	FreeformTools bool      `json:"freeform_tools" yaml:"freeform_tools"`
	ServerTools   []string  `json:"server_tools,omitempty" yaml:"server_tools"`
	// Namespaces reports that tools can be grouped under a namespace and
	// called by name plus namespace. It is a protocol property; endpoints
	// cannot change it.
	Namespaces     bool   `json:"namespaces,omitempty" yaml:"-"`
	DeferredTools  bool   `json:"deferred_tools" yaml:"deferred_tools"`
	Reasoning      bool   `json:"reasoning" yaml:"reasoning"`
	ReasoningStyle string `json:"reasoning_style,omitempty" yaml:"reasoning_style"` // effort | budget | reasoning_content
	Images         bool   `json:"images" yaml:"images"`
	Documents      bool   `json:"documents" yaml:"documents"`
	ToolResultImgs bool   `json:"tool_result_images" yaml:"tool_result_images"`
	// EchoFields lists fields clients echo back verbatim, usable to carry
	// sealed upstream state: "redacted_thinking", "encrypted_content",
	// "thought_signature". Empty means opaque state is lost on this protocol.
	EchoFields  []string `json:"echo_fields,omitempty" yaml:"echo_fields"`
	ServerState bool     `json:"server_state" yaml:"server_state"`
	Compaction  bool     `json:"compaction" yaml:"compaction"`
	MaxContext  int      `json:"max_context,omitempty" yaml:"max_context"`
	// MaxTokensField is the output-limit parameter name the endpoint wants.
	MaxTokensField    string   `json:"max_tokens_field,omitempty" yaml:"max_tokens_field"`
	UnsupportedParams []string `json:"unsupported_params,omitempty" yaml:"unsupported_params"`
	// FillInMiddle reports that the endpoint completes the text between a
	// prompt and a suffix. A request with a suffix is never routed to an
	// endpoint without it. Manifest endpoints must declare it.
	FillInMiddle bool `json:"fill_in_middle,omitempty" yaml:"fill_in_middle"`
	// PromptOnly reports a protocol that takes one text prompt and returns
	// a continuation: no roles, system text, history or tools. Only requests
	// from another prompt-only protocol are routed to it. It is a protocol
	// property; endpoints cannot change it.
	PromptOnly bool `json:"prompt_only,omitempty" yaml:"-"`
	// MediaOnly reports a protocol that serves only the media operations
	// it declares below, never text generation. It is a protocol property;
	// endpoints cannot change it.
	MediaOnly bool `json:"media_only,omitempty" yaml:"-"`
	// Media operations (see Modality). Endpoints narrow them like the
	// other flags.
	Embeddings         bool `json:"embeddings,omitempty" yaml:"embeddings"`
	ImageGeneration    bool `json:"image_generation,omitempty" yaml:"image_generation"`
	ImageEdit          bool `json:"image_edit,omitempty" yaml:"image_edit"`
	AudioTranscription bool `json:"audio_transcription,omitempty" yaml:"audio_transcription"`
	Speech             bool `json:"speech,omitempty" yaml:"speech"`
	Rerank             bool `json:"rerank,omitempty" yaml:"rerank"`
	// Batches reports that the endpoint serves its vendor's batch API
	// (OpenAI files + batches, Anthropic message batches) at its base
	// URL. Only an endpoint's own declaration sets it; codecs never do.
	Batches bool `json:"batches,omitempty" yaml:"batches"`
	// Realtime reports that the endpoint serves its vendor's Realtime API
	// over WebSocket at <base URL>/realtime; ResponsesWebSocket, the
	// Responses API's WebSocket mode at <base URL>/responses. Only an
	// endpoint's own declaration sets them; codecs never do.
	Realtime           bool `json:"realtime,omitempty" yaml:"realtime"`
	ResponsesWebSocket bool `json:"responses_websocket,omitempty" yaml:"responses_websocket"`
	// EmbedTokens reports that embeddings input may be token ids. It is a
	// protocol property.
	EmbedTokens bool `json:"embed_tokens,omitempty" yaml:"-"`
}

// Fidelity grades a conversion.
type Fidelity string

const (
	Lossless    Fidelity = "lossless"
	Lossy       Fidelity = "lossy"
	Discouraged Fidelity = "discouraged" // excluded from routing unless allowed
	Impossible  Fidelity = "impossible"
)

// Diagnostic records what a conversion did, for the request log.
type Diagnostic struct {
	Fidelity Fidelity `json:"fidelity"`
	Steps    []string `json:"steps,omitempty"`   // conversion steps that ran
	Dropped  []string `json:"dropped,omitempty"` // fields or blocks lost
	Missing  []string `json:"missing,omitempty"` // needs that could not be met
	Notes    []string `json:"notes,omitempty"`
}

// Worse returns the lower of two fidelities.
func Worse(a, b Fidelity) Fidelity {
	rank := map[Fidelity]int{Lossless: 0, Lossy: 1, Discouraged: 2, Impossible: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}
