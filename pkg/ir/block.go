package ir

import "encoding/json"

// BlockType tags the variant held by a Block.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockImage      BlockType = "image"
	BlockDocument   BlockType = "document"
	BlockThinking   BlockType = "thinking"    // visible reasoning text; Opaque may carry its signature
	BlockToolUse    BlockType = "tool_use"    // model calls a client tool
	BlockToolResult BlockType = "tool_result" // client returns a tool result
	BlockServerTool BlockType = "server_tool" // provider-executed tool call or result
	// BlockOpaque carries upstream-private state with no visible content
	// (Anthropic redacted_thinking, responses reasoning with only
	// encrypted_content, compaction items).
	BlockOpaque BlockType = "opaque"
	// BlockRaw is an item the IR cannot model at all (a new responses input
	// type, say). It survives only a same-protocol round trip.
	BlockRaw BlockType = "raw"
)

// Block is one content element. Exactly the fields relevant to Type are set.
type Block struct {
	Type BlockType `json:"type"`
	Text string    `json:"text,omitempty"`

	Media  *Media       `json:"media,omitempty"`  // image, document
	Tool   *ToolCall    `json:"tool,omitempty"`   // tool_use, server_tool
	Result *ToolResult  `json:"result,omitempty"` // tool_result
	Opaque *OpaqueState `json:"opaque,omitempty"`
	Cache  *CacheMark   `json:"cache,omitempty"`

	// Citations and other annotations, kept verbatim.
	Extra Extra `json:"extra,omitzero"`
}

// Media is inline or referenced binary content.
type Media struct {
	MediaType string `json:"media_type,omitempty"` // image/png, application/pdf, ...
	Data      string `json:"data,omitempty"`       // base64
	URL       string `json:"url,omitempty"`
	FileID    string `json:"file_id,omitempty"`
	Title     string `json:"title,omitempty"`
	Detail    string `json:"detail,omitempty"` // OpenAI image detail
}

// ToolCall is a model's invocation of a tool.
type ToolCall struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Kind ToolKind `json:"kind"`
	// Input is JSON arguments for function and server tools.
	Input json.RawMessage `json:"input,omitempty"`
	// Text is the raw input of a freeform tool.
	Text string `json:"text,omitempty"`
	// Namespace is the responses tool namespace, if any.
	Namespace string `json:"namespace,omitempty"`
}

// ToolResult answers a ToolCall.
type ToolResult struct {
	ToolUseID string  `json:"tool_use_id"`
	Content   []Block `json:"content,omitempty"` // text and images
	IsError   bool    `json:"is_error,omitempty"`
	// Freeform marks the result of a freeform tool (custom_tool_call_output).
	Freeform bool `json:"freeform,omitempty"`
}

// CacheMark is an explicit prompt-cache breakpoint.
type CacheMark struct {
	TTL string `json:"ttl,omitempty"` // "5m" (default) or "1h"
}

// SealPrefix starts every relay-sealed opaque string. A codec decoding a
// client request marks any echo-field value with this prefix as Sealed.
const SealPrefix = "prs1."

// OpaqueState is upstream-private state the client must echo back verbatim
// and that only its issuer accepts: thinking signatures, encrypted reasoning,
// Gemini thoughtSignature, server-side response ids.
//
// On a block of type opaque it is the whole block. On any other block it is
// state attached to that block: the signature of a thinking block, the
// thoughtSignature of a Gemini text or functionCall part, the encrypted
// content of a reasoning item that also has a visible summary.
//
// When a codec encodes for a client, a Sealed state's Data is a JSON string
// holding the seal; the codec writes it into the protocol's echo slot
// (Anthropic: thinking.signature or redacted_thinking.data; responses:
// reasoning.encrypted_content; Gemini: thoughtSignature). Protocols without
// an echo slot drop it.
type OpaqueState struct {
	// Issuer is the issuer group of the upstream that produced the state
	// (e.g. "anthropic", "bedrock", "openai"). Set by the pipeline from the
	// provider's declaration; empty when it came from a client of unknown origin.
	Issuer string `json:"issuer,omitempty"`
	// Kind says which wire field holds it: "signature", "redacted_thinking",
	// "encrypted_content", "thought_signature", "compaction", ...
	Kind string `json:"kind"`
	// Data is the state verbatim (a string or JSON value).
	Data json.RawMessage `json:"data"`
	// Sealed is set when Data is a plugrelay seal rather than the upstream's
	// own bytes; the pipeline unseals before routing.
	Sealed bool `json:"sealed,omitempty"`
}
