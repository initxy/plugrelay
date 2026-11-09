// Package openaiaudio implements the OpenAI audio wire protocol
// ("openai-audio"): POST /v1/audio/transcriptions and
// /v1/audio/translations (multipart uploads) and /v1/audio/speech (JSON in,
// audio out) on the client side and upstream.
//
// Requests are only passed through to the same protocol. What is metered:
// the characters of speech input (from the request), and for
// transcriptions the audio duration or token usage the upstream reports
// (usage.seconds, verbose_json duration, or usage tokens). Plain text,
// SRT and VTT transcripts report neither; the pipeline then estimates the
// duration from the uploaded file. Speech audio and event-stream
// transcripts are copied to the client as they arrive. Errors and model
// lists use the OpenAI shapes of package openaichat.
package openaiaudio

import (
	"math"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/protocol/internal/media"
	"github.com/initxy/plugrelay/pkg/protocol/openaichat"
	"github.com/initxy/plugrelay/pkg/sse"
)

// Name is the protocol id of this codec.
const Name = "openai-audio"

// Codec is the audio codec. The zero value is ready to use.
type Codec struct{}

func init() { protocol.Register(Codec{}) }

var (
	_ protocol.Codec        = Codec{}
	_ protocol.BodyStreamer = Codec{}
)

var openai = openaichat.Codec{}

// Name returns "openai-audio".
func (Codec) Name() string { return Name }

// Capabilities reports transcription and speech.
func (Codec) Capabilities() ir.Capabilities {
	return ir.Capabilities{Cache: ir.CacheNone, MediaOnly: true, AudioTranscription: true, Speech: true}
}

// Routes lists transcriptions, translations and speech.
func (Codec) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/v1/audio/transcriptions", Op: protocol.OpTranscribe, Upload: true},
		{Method: http.MethodPost, Path: "/v1/audio/translations", Op: protocol.OpTranslate, Upload: true},
		{Method: http.MethodPost, Path: "/v1/audio/speech", Op: protocol.OpSpeech},
	}
}

// Sniff reads the model.
func (Codec) Sniff(_ *http.Request, route protocol.Route, body []byte) (protocol.Inbound, error) {
	return media.SniffModel(route, body)
}

// DecodeRequest reads the metered facts; the body itself is passed through.
func (Codec) DecodeRequest(_ *http.Request, in protocol.Inbound, body []byte) (*ir.Request, ir.Needs, error) {
	if !gjson.ValidBytes(body) {
		return nil, ir.Needs{}, ir.Errorf(ir.ErrInvalidRequest, "request body is not valid JSON")
	}
	r := gjson.ParseBytes(body)
	req := &ir.Request{Model: r.Get("model").String()}
	if in.Op == protocol.OpSpeech {
		chars := int64(utf8.RuneCountInString(r.Get("input").String()))
		req.Units = &ir.MediaUnits{Chars: chars}
		return req, ir.Needs{Modality: ir.ModalitySpeech, InputTokensEst: media.EstimateTokens(int(chars))}, nil
	}
	return req, ir.Needs{Modality: ir.ModalityTranscription}, nil
}

// EncodeRequest is unsupported: audio requests are only passed through.
func (Codec) EncodeRequest(*ir.Request) ([]byte, error) {
	return nil, ir.Errorf(ir.ErrCapabilityMissing, "audio requests are only passed through")
}

// DecodeResponse reads a JSON transcript's duration or token usage.
func (Codec) DecodeResponse(body []byte) (*ir.Response, error) {
	if !gjson.ValidBytes(body) {
		return nil, ir.Errorf(ir.ErrUpstream, "not a JSON transcript")
	}
	r := gjson.ParseBytes(body)
	var u ir.Usage
	us := r.Get("usage")
	switch us.Get("type").String() {
	case "duration":
		u.AudioMs = millis(us.Get("seconds").Float())
	case "tokens":
		u.Input, u.Output = us.Get("input_tokens").Int(), us.Get("output_tokens").Int()
	}
	if us.Exists() {
		u.Raw = []byte(us.Raw)
	}
	if u.AudioMs == 0 {
		u.AudioMs = millis(r.Get("duration").Float())
	}
	return &ir.Response{StopReason: ir.StopEndTurn, Usage: u}, nil
}

func millis(sec float64) int64 {
	if sec <= 0 {
		return 0
	}
	return int64(math.Round(sec * 1000))
}

// EncodeResponse is unsupported: responses are only passed through.
func (Codec) EncodeResponse(*ir.Request, *ir.Response) ([]byte, error) {
	return nil, ir.Errorf(ir.ErrInternal, "audio responses are only passed through")
}

// StreamsBody reports speech audio and event-stream transcripts.
func (Codec) StreamsBody(in protocol.Inbound, contentType string) bool {
	return in.Op == protocol.OpSpeech || strings.HasPrefix(strings.ToLower(contentType), "text/event-stream")
}

// NewStreamEncoder returns an encoder that only renders errors.
func (Codec) NewStreamEncoder(w sse.Writer, _ *ir.Request) protocol.StreamEncoder {
	return &media.NoStreamEncoder{W: w}
}

// NewStreamDecoder returns a decoder that never completes.
func (Codec) NewStreamDecoder() protocol.StreamDecoder { return media.NoStreamDecoder{} }

// EncodeError renders the OpenAI error format.
func (Codec) EncodeError(e *ir.GwError) (int, http.Header, []byte) { return openai.EncodeError(e) }

// DecodeError reads an OpenAI-style upstream error.
func (Codec) DecodeError(status int, header http.Header, body []byte) *ir.GwError {
	return openai.DecodeError(status, header, body)
}

// EncodeModels renders the OpenAI list format.
func (Codec) EncodeModels(r *http.Request, models []ir.ModelInfo) (string, []byte) {
	return openai.EncodeModels(r, models)
}

// CountTokens is not part of this protocol.
func (Codec) CountTokens(*ir.Request) ([]byte, error) { return nil, media.ErrUnsupported }

// UpstreamPath returns the operation's path; upstream base URLs include /v1.
func (Codec) UpstreamPath(_ *ir.Request, in protocol.Inbound) string {
	switch in.Op {
	case protocol.OpTranslate:
		return "/audio/translations"
	case protocol.OpSpeech:
		return "/audio/speech"
	}
	return "/audio/transcriptions"
}

// SetModel rewrites the top-level model field, leaving every other byte
// intact. Multipart uploads get their model field from the pipeline.
func (Codec) SetModel(body []byte, model string) ([]byte, error) {
	return sjson.SetBytes(body, "model", model)
}

// StripOpaque is a no-op.
func (Codec) StripOpaque(body []byte) ([]byte, int, error) { return body, 0, nil }
