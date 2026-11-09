package ir

import (
	"encoding/json"
	"slices"
)

// RawFields holds protocol-specific fields the IR has no slot for, keyed by
// their wire name, values kept as the exact source bytes. A codec restores
// them only when encoding back to the protocol named in Origin.
type RawFields map[string]json.RawMessage

// Extra is RawFields tagged with the protocol that produced them, so an
// encoder for a different protocol knows to ignore them.
type Extra struct {
	Origin string    `json:"origin,omitempty"` // codec name, e.g. "anthropic"
	Fields RawFields `json:"fields,omitempty"`
}

// Get returns the raw field when it was produced by the given protocol.
func (e *Extra) Get(protocol, key string) (json.RawMessage, bool) {
	if e == nil || e.Origin != protocol || e.Fields == nil {
		return nil, false
	}
	v, ok := e.Fields[key]
	return v, ok
}

// Set records a raw field produced by protocol.
func (e *Extra) Set(protocol, key string, v json.RawMessage) {
	if e.Fields == nil || e.Origin != protocol {
		e.Origin = protocol
		e.Fields = RawFields{}
	}
	e.Fields[key] = v
}

// Role of a message author. Tool results travel as blocks inside user
// messages, the Anthropic way; codecs for other protocols split or merge.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Request is one model call in neutral form.
type Request struct {
	Model    string    `json:"model"`
	System   []Block   `json:"system,omitempty"` // ordered; position and boundaries matter for caching
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`

	ToolChoice      *ToolChoice `json:"tool_choice,omitempty"`
	ParallelTools   *bool       `json:"parallel_tools,omitempty"`
	MaxOutputTokens *int        `json:"max_output_tokens,omitempty"`
	Temperature     *float64    `json:"temperature,omitempty"`
	TopP            *float64    `json:"top_p,omitempty"`
	TopK            *int        `json:"top_k,omitempty"`
	Stop            []string    `json:"stop,omitempty"`
	Stream          bool        `json:"stream"`
	Reasoning       *Reasoning  `json:"reasoning,omitempty"`

	// CacheKey is a client-provided routing key for implicit prompt caches
	// (OpenAI prompt_cache_key). Empty means none was sent.
	CacheKey    string `json:"cache_key,omitempty"`
	ServiceTier string `json:"service_tier,omitempty"`
	// Verbosity is OpenAI responses text.verbosity.
	Verbosity string `json:"verbosity,omitempty"`
	// Previous is a server-side conversation pointer (previous_response_id).
	Previous string `json:"previous,omitempty"`
	// Store reports whether the client asked the upstream to keep state.
	Store *bool `json:"store,omitempty"`

	// Embed is set for an embeddings request (no messages).
	Embed *Embed `json:"embed,omitempty"`
	// Rerank is set for a rerank request (no messages).
	Rerank *Rerank `json:"rerank,omitempty"`
	// Units is set for an image or audio request.
	Units *MediaUnits `json:"units,omitempty"`

	Extra Extra `json:"extra,omitzero"`
}

// Message is one turn of conversation history.
type Message struct {
	Role    Role    `json:"role"`
	Content []Block `json:"content"`
	Extra   Extra   `json:"extra,omitzero"`
}

// ToolChoice constrains whether and which tool the model calls.
type ToolChoice struct {
	Mode string `json:"mode"`           // auto | none | any | tool
	Name string `json:"name,omitempty"` // when Mode == "tool"
}

// Reasoning expresses thinking controls in both styles a protocol may use:
// a named effort level or a token budget. Conversion maps between them.
type Reasoning struct {
	Enabled bool   `json:"enabled"`
	Effort  string `json:"effort,omitempty"` // minimal | low | medium | high | xhigh | max
	Budget  *int   `json:"budget,omitempty"` // thinking budget tokens
	// Adaptive is Anthropic's "adaptive" thinking type.
	Adaptive bool `json:"adaptive,omitempty"`
	// Summary is OpenAI reasoning.summary (auto | concise | detailed).
	Summary string `json:"summary,omitempty"`
	// IncludeEncrypted asks the upstream to return encrypted reasoning
	// (responses include: ["reasoning.encrypted_content"]).
	IncludeEncrypted bool `json:"include_encrypted,omitempty"`
}

// ToolKind distinguishes how a tool is invoked.
type ToolKind string

const (
	ToolFunction ToolKind = "function" // JSON-schema arguments
	ToolFreeform ToolKind = "freeform" // free text input, optionally with a grammar
	ToolServer   ToolKind = "server"   // executed by the provider (web_search, ...)
)

// Tool is a tool definition offered to the model.
type Tool struct {
	Kind        ToolKind        `json:"kind"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"` // ToolFunction
	// Grammar is the freeform tool's format definition, verbatim
	// (e.g. {"type":"grammar","syntax":"lark","definition":"..."}).
	Grammar json.RawMessage `json:"grammar,omitempty"`
	// ServerType names a provider-executed tool, e.g. "web_search_20250305".
	ServerType   string `json:"server_type,omitempty"`
	DeferLoading bool   `json:"defer_loading,omitempty"`
	// Namespace groups the tool with others under a common prefix
	// (responses namespace tools); calls name it in ToolCall.Namespace.
	Namespace string     `json:"namespace,omitempty"`
	Cache     *CacheMark `json:"cache,omitempty"`
	Extra     Extra      `json:"extra,omitzero"`
}

// RequiredServerTools lists, without duplicates, the server tool types the
// request depends on rather than merely offers: the one tool_choice forces,
// and those with a call in the last assistant message that has no result
// there yet (a server-side turn the model paused and is to continue).
func (r *Request) RequiredServerTools() []string {
	var out []string
	add := func(name string) {
		for _, t := range r.Tools {
			if t.Kind == ToolServer && t.Name == name && t.ServerType != "" && !slices.Contains(out, t.ServerType) {
				out = append(out, t.ServerType)
			}
		}
	}
	if tc := r.ToolChoice; tc != nil && tc.Mode == "tool" {
		add(tc.Name)
	}
	if n := len(r.Messages); n > 0 && r.Messages[n-1].Role == RoleAssistant {
		answered := map[string]bool{}
		for _, b := range r.Messages[n-1].Content {
			if b.Type == BlockServerTool && b.Result != nil {
				answered[b.Result.ToolUseID] = true
			}
		}
		for _, b := range r.Messages[n-1].Content {
			if b.Type == BlockServerTool && b.Tool != nil && !answered[b.Tool.ID] {
				add(b.Tool.Name)
			}
		}
	}
	return out
}
