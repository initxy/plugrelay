package ir

import "encoding/json"

// Modality is the operation a request asks for. The zero value is text
// generation; the others are the media operations, each served only by an
// endpoint whose capabilities declare it.
type Modality string

const (
	ModalityText            Modality = ""
	ModalityEmbeddings      Modality = "embeddings"
	ModalityImageGeneration Modality = "image_generation"
	ModalityImageEdit       Modality = "image_edit"
	ModalityTranscription   Modality = "audio_transcription"
	ModalitySpeech          Modality = "speech"
	ModalityRerank          Modality = "rerank"
)

// Serves reports whether an endpoint with these capabilities serves m.
func (c Capabilities) Serves(m Modality) bool {
	switch m {
	case ModalityText:
		return !c.MediaOnly
	case ModalityEmbeddings:
		return c.Embeddings
	case ModalityImageGeneration:
		return c.ImageGeneration
	case ModalityImageEdit:
		return c.ImageEdit
	case ModalityTranscription:
		return c.AudioTranscription
	case ModalitySpeech:
		return c.Speech
	case ModalityRerank:
		return c.Rerank
	}
	return false
}

// Embed is an embeddings request: one vector per input.
type Embed struct {
	Inputs []EmbedInput `json:"inputs"`
	// Dimensions asks for shorter vectors (0 = the model's default).
	Dimensions int `json:"dimensions,omitempty"`
	// Base64 reports that the client wants vectors as base64 of
	// little-endian float32; the relay encodes them when the upstream
	// returned floats.
	Base64 bool `json:"base64,omitempty"`
	// TaskType and Title are retrieval hints some protocols take.
	TaskType string `json:"task_type,omitempty"`
	Title    string `json:"title,omitempty"`
	// User is an end-user tag.
	User string `json:"user,omitempty"`
	// Batch reports that the client sent its inputs as a batch, so the
	// response is a list even for one input.
	Batch bool `json:"batch,omitempty"`
}

// EmbedInput is one text, or a sequence of token ids.
type EmbedInput struct {
	Text   string  `json:"text,omitempty"`
	Tokens []int64 `json:"tokens,omitempty"`
}

// Rerank is a rerank request: documents scored against one query.
type Rerank struct {
	Query     string      `json:"query"`
	Documents []RerankDoc `json:"documents"`
	// TopN keeps the best n results (0 = all).
	TopN int `json:"top_n,omitempty"`
	// ReturnDocuments asks for each result's document; nil leaves it to
	// the protocol's default.
	ReturnDocuments *bool `json:"return_documents,omitempty"`
	// MaxTokensPerDoc truncates long documents (0 = the upstream default).
	MaxTokensPerDoc int `json:"max_tokens_per_doc,omitempty"`
}

// RerankDoc is one document: its text, and the object it was sent as when
// the client sent an object rather than a string.
type RerankDoc struct {
	Text   string          `json:"text"`
	Object json.RawMessage `json:"object,omitempty"`
}

// Rank is one scored document of a rerank response, by request index.
type Rank struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

// MediaUnits are the metered facts of an image or audio request whose body
// is passed through as is.
type MediaUnits struct {
	Images       int    `json:"images,omitempty"` // images asked for (n)
	ImageSize    string `json:"image_size,omitempty"`
	ImageQuality string `json:"image_quality,omitempty"`
	Chars        int64  `json:"chars,omitempty"` // characters to speak
}
