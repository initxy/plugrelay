package manifest

import (
	"fmt"

	"github.com/initxy/plugrelay/pkg/ir"
	"gopkg.in/yaml.v3"
)

// Reasoning styles an endpoint may declare.
var ReasoningStyles = []string{"effort", "budget", "reasoning_content"}

// Capabilities is a partial ir.Capabilities: unset fields keep the value
// they are merged over. Lists replace, they do not append.
type Capabilities struct {
	Cache             *ir.CacheMode  `yaml:"cache,omitempty" json:"cache,omitempty"`
	Cache1h           *bool          `yaml:"cache_1h,omitempty" json:"cache_1h,omitempty"`
	FunctionTools     *bool          `yaml:"function_tools,omitempty" json:"function_tools,omitempty"`
	FreeformTools     *bool          `yaml:"freeform_tools,omitempty" json:"freeform_tools,omitempty"`
	ServerTools       []string       `yaml:"server_tools,omitempty" json:"server_tools,omitempty"`
	DeferredTools     *bool          `yaml:"deferred_tools,omitempty" json:"deferred_tools,omitempty"`
	Reasoning         *ReasoningSpec `yaml:"reasoning,omitempty" json:"reasoning,omitempty"`
	Images            *bool          `yaml:"images,omitempty" json:"images,omitempty"`
	Documents         *bool          `yaml:"documents,omitempty" json:"documents,omitempty"`
	ToolResultImgs    *bool          `yaml:"tool_result_images,omitempty" json:"tool_result_images,omitempty"`
	EchoFields        []string       `yaml:"echo_fields,omitempty" json:"echo_fields,omitempty"`
	ServerState       *bool          `yaml:"server_state,omitempty" json:"server_state,omitempty"`
	Compaction        *bool          `yaml:"compaction,omitempty" json:"compaction,omitempty"`
	MaxContext        *int           `yaml:"max_context,omitempty" json:"max_context,omitempty"`
	MaxTokensField    *string        `yaml:"max_tokens_field,omitempty" json:"max_tokens_field,omitempty"`
	UnsupportedParams []string       `yaml:"unsupported_params,omitempty" json:"unsupported_params,omitempty"`
	FillInMiddle      *bool          `yaml:"fill_in_middle,omitempty" json:"fill_in_middle,omitempty"`
	// Media operations an endpoint of a media protocol serves; all of its
	// protocol's by default.
	Embeddings         *bool `yaml:"embeddings,omitempty" json:"embeddings,omitempty"`
	ImageGeneration    *bool `yaml:"image_generation,omitempty" json:"image_generation,omitempty"`
	ImageEdit          *bool `yaml:"image_edit,omitempty" json:"image_edit,omitempty"`
	AudioTranscription *bool `yaml:"audio_transcription,omitempty" json:"audio_transcription,omitempty"`
	Speech             *bool `yaml:"speech,omitempty" json:"speech,omitempty"`
	Rerank             *bool `yaml:"rerank,omitempty" json:"rerank,omitempty"`
	// Batches: the endpoint serves its vendor's batch API.
	Batches *bool `yaml:"batches,omitempty" json:"batches,omitempty"`
	// Realtime: the vendor's Realtime WebSocket API at <base_url>/realtime;
	// ResponsesWebSocket: the Responses WebSocket mode at <base_url>/responses.
	Realtime           *bool `yaml:"realtime,omitempty" json:"realtime,omitempty"`
	ResponsesWebSocket *bool `yaml:"responses_websocket,omitempty" json:"responses_websocket,omitempty"`
}

// ReasoningSpec is written either as a bool or as a reasoning style
// ("reasoning: reasoning_content"), which implies true.
type ReasoningSpec struct {
	On    bool
	Style string
}

// UnmarshalYAML accepts a bool or a style name.
func (r *ReasoningSpec) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: reasoning must be a bool or a style name", n.Line)
	}
	var b bool
	if n.Tag == "!!bool" && n.Decode(&b) == nil {
		*r = ReasoningSpec{On: b}
		return nil
	}
	*r = ReasoningSpec{On: true, Style: n.Value}
	return nil
}

// MarshalYAML writes the style when set, else the bool.
func (r ReasoningSpec) MarshalYAML() (any, error) {
	if r.Style != "" {
		return r.Style, nil
	}
	return r.On, nil
}

// Over returns c with the fields set in o replacing c's.
func (c Capabilities) Over(o Capabilities) Capabilities {
	if o.Cache != nil {
		c.Cache = o.Cache
	}
	setB := func(dst **bool, src *bool) {
		if src != nil {
			*dst = src
		}
	}
	setB(&c.Cache1h, o.Cache1h)
	setB(&c.FunctionTools, o.FunctionTools)
	setB(&c.FreeformTools, o.FreeformTools)
	setB(&c.DeferredTools, o.DeferredTools)
	setB(&c.Images, o.Images)
	setB(&c.Documents, o.Documents)
	setB(&c.ToolResultImgs, o.ToolResultImgs)
	setB(&c.ServerState, o.ServerState)
	setB(&c.Compaction, o.Compaction)
	setB(&c.FillInMiddle, o.FillInMiddle)
	setB(&c.Embeddings, o.Embeddings)
	setB(&c.ImageGeneration, o.ImageGeneration)
	setB(&c.ImageEdit, o.ImageEdit)
	setB(&c.AudioTranscription, o.AudioTranscription)
	setB(&c.Speech, o.Speech)
	setB(&c.Rerank, o.Rerank)
	setB(&c.Batches, o.Batches)
	setB(&c.Realtime, o.Realtime)
	setB(&c.ResponsesWebSocket, o.ResponsesWebSocket)
	if o.ServerTools != nil {
		c.ServerTools = o.ServerTools
	}
	if o.Reasoning != nil {
		c.Reasoning = o.Reasoning
	}
	if o.EchoFields != nil {
		c.EchoFields = o.EchoFields
	}
	if o.MaxContext != nil {
		c.MaxContext = o.MaxContext
	}
	if o.MaxTokensField != nil {
		c.MaxTokensField = o.MaxTokensField
	}
	if o.UnsupportedParams != nil {
		c.UnsupportedParams = o.UnsupportedParams
	}
	return c
}

// Apply returns base with the fields set in c replacing base's.
func (c Capabilities) Apply(base ir.Capabilities) ir.Capabilities {
	b := func(dst *bool, src *bool) {
		if src != nil {
			*dst = *src
		}
	}
	if c.Cache != nil {
		base.Cache = *c.Cache
	}
	b(&base.Cache1h, c.Cache1h)
	b(&base.FunctionTools, c.FunctionTools)
	b(&base.FreeformTools, c.FreeformTools)
	b(&base.DeferredTools, c.DeferredTools)
	b(&base.Images, c.Images)
	b(&base.Documents, c.Documents)
	b(&base.ToolResultImgs, c.ToolResultImgs)
	b(&base.ServerState, c.ServerState)
	b(&base.Compaction, c.Compaction)
	b(&base.FillInMiddle, c.FillInMiddle)
	b(&base.Embeddings, c.Embeddings)
	b(&base.ImageGeneration, c.ImageGeneration)
	b(&base.ImageEdit, c.ImageEdit)
	b(&base.AudioTranscription, c.AudioTranscription)
	b(&base.Speech, c.Speech)
	b(&base.Rerank, c.Rerank)
	b(&base.Batches, c.Batches)
	b(&base.Realtime, c.Realtime)
	b(&base.ResponsesWebSocket, c.ResponsesWebSocket)
	if c.ServerTools != nil {
		base.ServerTools = append([]string(nil), c.ServerTools...)
	}
	if c.Reasoning != nil {
		base.Reasoning = c.Reasoning.On
		if c.Reasoning.Style != "" {
			base.ReasoningStyle = c.Reasoning.Style
		}
		if !c.Reasoning.On {
			base.ReasoningStyle = ""
		}
	}
	if c.EchoFields != nil {
		base.EchoFields = append([]string(nil), c.EchoFields...)
	}
	if c.MaxContext != nil {
		base.MaxContext = *c.MaxContext
	}
	if c.MaxTokensField != nil {
		base.MaxTokensField = *c.MaxTokensField
	}
	if c.UnsupportedParams != nil {
		base.UnsupportedParams = append([]string(nil), c.UnsupportedParams...)
	}
	return base
}

// EndpointCaps returns e's effective capabilities: base (the codec's), then
// the manifest's shared capabilities, then e's own.
//
// Freeform (custom) tools are an OpenAI-only extension of the OpenAI
// protocols; OpenAI-compatible servers reject them (a Codex `apply_patch`
// sent as a custom tool is a 400). So for a manifest the codec's "yes" is
// not trusted: freeform tools are off unless the manifest says
// `freeform_tools: true`, and the relay wraps them as function tools.
func (m *Manifest) EndpointCaps(e *Endpoint, base ir.Capabilities) ir.Capabilities {
	caps := m.Capabilities.Over(e.Capabilities)
	if caps.FreeformTools == nil {
		base.FreeformTools = false
	}
	return caps.Apply(base)
}
