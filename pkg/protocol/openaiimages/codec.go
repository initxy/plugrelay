// Package openaiimages implements the OpenAI images wire protocol
// ("openai-images"): POST /v1/images/generations (JSON) and
// /v1/images/edits (multipart upload) on the client side and upstream.
//
// Requests are only passed through to the same protocol; the codec reads
// what is metered: images asked for (n), size and quality from the
// request, images delivered and any token usage from the response. An
// event-stream response (partial images) is copied to the client as is and
// billed by the pipeline for the images asked for.
// Errors and model lists use the OpenAI shapes of package openaichat.
package openaiimages

import (
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
const Name = "openai-images"

// Codec is the images codec. The zero value is ready to use.
type Codec struct{}

func init() { protocol.Register(Codec{}) }

var (
	_ protocol.Codec        = Codec{}
	_ protocol.BodyStreamer = Codec{}
)

var openai = openaichat.Codec{}

// Name returns "openai-images".
func (Codec) Name() string { return Name }

// Capabilities reports image generation and edits.
func (Codec) Capabilities() ir.Capabilities {
	return ir.Capabilities{Cache: ir.CacheNone, MediaOnly: true, ImageGeneration: true, ImageEdit: true}
}

// Routes lists generations and edits.
func (Codec) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/v1/images/generations", Op: protocol.OpImage},
		{Method: http.MethodPost, Path: "/v1/images/edits", Op: protocol.OpImageEdit, Upload: true},
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
	m := &ir.MediaUnits{Images: 1, ImageSize: r.Get("size").String(), ImageQuality: r.Get("quality").String()}
	if n := r.Get("n"); n.Exists() && n.Int() > 0 {
		m.Images = int(n.Int())
	}
	needs := ir.Needs{Modality: ir.ModalityImageGeneration,
		InputTokensEst: media.EstimateTokens(utf8.RuneCountInString(r.Get("prompt").String()))}
	if in.Op == protocol.OpImageEdit {
		needs.Modality = ir.ModalityImageEdit
	}
	return &ir.Request{Model: r.Get("model").String(), Units: m}, needs, nil
}

// EncodeRequest is unsupported: image requests are only passed through.
func (Codec) EncodeRequest(*ir.Request) ([]byte, error) {
	return nil, ir.Errorf(ir.ErrCapabilityMissing, "image requests are only passed through")
}

// DecodeResponse reads images delivered, size, quality and token usage.
func (Codec) DecodeResponse(body []byte) (*ir.Response, error) {
	if !gjson.ValidBytes(body) {
		return nil, ir.Errorf(ir.ErrUpstream, "images response is not valid JSON")
	}
	r := gjson.ParseBytes(body)
	data := r.Get("data")
	if !data.IsArray() {
		return nil, ir.Errorf(ir.ErrUpstream, "images response has no data")
	}
	u := ir.Usage{Images: int64(len(data.Array())), ImageSize: r.Get("size").String(), ImageQuality: r.Get("quality").String()}
	if u.ImageSize == "" {
		u.ImageSize = data.Get("0.size").String()
	}
	if us := r.Get("usage"); us.Exists() {
		u.Input, u.Output, u.Raw = us.Get("input_tokens").Int(), us.Get("output_tokens").Int(), []byte(us.Raw)
	}
	return &ir.Response{Model: r.Get("model").String(), StopReason: ir.StopEndTurn, Usage: u}, nil
}

// EncodeResponse is unsupported: responses are only passed through.
func (Codec) EncodeResponse(*ir.Request, *ir.Response) ([]byte, error) {
	return nil, ir.Errorf(ir.ErrInternal, "image responses are only passed through")
}

// StreamsBody reports an event stream of partial images.
func (Codec) StreamsBody(_ protocol.Inbound, contentType string) bool {
	return strings.HasPrefix(strings.ToLower(contentType), "text/event-stream")
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
	if in.Op == protocol.OpImageEdit {
		return "/images/edits"
	}
	return "/images/generations"
}

// SetModel rewrites the top-level model field, leaving every other byte
// intact. Multipart uploads get their model field from the pipeline.
func (Codec) SetModel(body []byte, model string) ([]byte, error) {
	return sjson.SetBytes(body, "model", model)
}

// StripOpaque is a no-op.
func (Codec) StripOpaque(body []byte) ([]byte, int, error) { return body, 0, nil }
