package ir

import "encoding/json"

// StopReason is why generation ended, in neutral terms.
type StopReason string

const (
	StopEndTurn       StopReason = "end_turn"
	StopMaxTokens     StopReason = "max_tokens"
	StopToolUse       StopReason = "tool_use"
	StopSequence      StopReason = "stop_sequence"
	StopContentFilter StopReason = "content_filter"
	StopRefusal       StopReason = "refusal"
	StopPause         StopReason = "pause_turn"
	StopContextWindow StopReason = "model_context_window_exceeded"
)

// Response is a complete, non-streamed model answer.
type Response struct {
	ID         string     `json:"id"`
	Model      string     `json:"model"`
	Content    []Block    `json:"content"`
	StopReason StopReason `json:"stop_reason"`
	StopSeq    string     `json:"stop_sequence,omitempty"`
	Usage      Usage      `json:"usage"`
	// Embeddings are the vectors of an embeddings response, one per input.
	Embeddings [][]float64 `json:"embeddings,omitempty"`
	// Ranks are the results of a rerank response, best first.
	Ranks []Rank `json:"ranks,omitempty"`
	Extra Extra  `json:"extra,omitzero"`
}

// Usage is token accounting in five non-overlapping buckets plus the
// upstream's raw report. Total input = Input + CacheRead + CacheWrite5m + CacheWrite1h.
type Usage struct {
	Input        int64 `json:"input"`          // uncached input
	CacheRead    int64 `json:"cache_read"`     // input served from cache
	CacheWrite5m int64 `json:"cache_write_5m"` // input written to a 5-minute cache
	CacheWrite1h int64 `json:"cache_write_1h"` // input written to a 1-hour cache
	Output       int64 `json:"output"`         // all output, reasoning included
	Reasoning    int64 `json:"reasoning"`      // portion of Output spent on reasoning

	// Media units, priced per unit (image, second of audio, 1000
	// characters) rather than per token.
	Images       int64  `json:"images,omitempty"` // images generated
	ImageSize    string `json:"image_size,omitempty"`
	ImageQuality string `json:"image_quality,omitempty"`
	AudioMs      int64  `json:"audio_ms,omitempty"` // input audio duration
	Chars        int64  `json:"chars,omitempty"`    // input characters (speech)
	// SearchUnits are a rerank vendor's billing units (one query over up
	// to 100 documents, by Cohere's definition).
	SearchUnits int64 `json:"search_units,omitempty"`

	// Estimated is set when the upstream never reported usage (stream cut).
	Estimated bool `json:"estimated,omitempty"`
	// Raw is the upstream's usage object, verbatim.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// TotalInput is every input token, cached or not.
func (u Usage) TotalInput() int64 {
	return u.Input + u.CacheRead + u.CacheWrite5m + u.CacheWrite1h
}

// Add accumulates another usage report (the responses of a batch, a
// WebSocket session's turns).
func (u *Usage) Add(o Usage) {
	u.Input += o.Input
	u.CacheRead += o.CacheRead
	u.CacheWrite5m += o.CacheWrite5m
	u.CacheWrite1h += o.CacheWrite1h
	u.Output += o.Output
	u.Reasoning += o.Reasoning
	u.Images += o.Images
	u.AudioMs += o.AudioMs
	u.Chars += o.Chars
	u.SearchUnits += o.SearchUnits
	if u.ImageSize == "" {
		u.ImageSize, u.ImageQuality = o.ImageSize, o.ImageQuality
	}
	u.Estimated = u.Estimated || o.Estimated
}

// IsZero reports whether no tokens and no media units were counted.
func (u Usage) IsZero() bool {
	return u.TotalInput() == 0 && u.Output == 0 && u.Images == 0 && u.AudioMs == 0 && u.Chars == 0 && u.SearchUnits == 0
}

// ModelInfo describes a model as listed to clients. Each protocol renders
// the same data in its own /models format.
type ModelInfo struct {
	ID            string   `json:"id"`
	DisplayName   string   `json:"display_name,omitempty"`
	OwnedBy       string   `json:"owned_by,omitempty"`
	ContextWindow int      `json:"context_window,omitempty"`
	MaxOutput     int      `json:"max_output,omitempty"`
	Tools         bool     `json:"tools"`
	Vision        bool     `json:"vision"`
	Reasoning     bool     `json:"reasoning"`
	Efforts       []string `json:"efforts,omitempty"` // supported reasoning efforts
	Created       int64    `json:"created,omitempty"` // unix seconds
}
