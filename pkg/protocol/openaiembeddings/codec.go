// Package openaiembeddings implements the OpenAI embeddings wire protocol
// ("openai-embeddings"): POST /v1/embeddings on the client side and the
// same endpoint upstream.
//
// Input may be a string, an array of strings, an array of token ids or an
// array of token-id arrays; token input sets ir.Needs.EmbedTokens, which
// only a protocol taking token ids can serve. Requested base64 output is
// produced by this codec when the vectors arrive as floats, so an upstream
// that cannot encode base64 still serves such a client. Upstream requests
// always ask for floats. Fields without an IR slot are kept in ir.Extra
// and restored only for this protocol. Errors and model lists use the
// OpenAI shapes of package openaichat.
package openaiembeddings

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/protocol/internal/media"
	"github.com/initxy/plugrelay/pkg/protocol/openaichat"
	"github.com/initxy/plugrelay/pkg/sse"
)

// Name is the protocol id of this codec.
const Name = "openai-embeddings"

// Codec is the embeddings codec. The zero value is ready to use.
type Codec struct{}

func init() { protocol.Register(Codec{}) }

var _ protocol.Codec = Codec{}

var openai = openaichat.Codec{}

// Name returns "openai-embeddings".
func (Codec) Name() string { return Name }

// Capabilities reports embeddings only, with token-id input.
func (Codec) Capabilities() ir.Capabilities {
	return ir.Capabilities{Cache: ir.CacheNone, MediaOnly: true, Embeddings: true, EmbedTokens: true}
}

// Routes lists the embeddings endpoint.
func (Codec) Routes() []protocol.Route {
	return []protocol.Route{{Method: http.MethodPost, Path: "/v1/embeddings", Op: protocol.OpEmbed}}
}

// Sniff reads the model.
func (Codec) Sniff(_ *http.Request, route protocol.Route, body []byte) (protocol.Inbound, error) {
	return media.SniffModel(route, body)
}

// known are the fields with an IR slot.
var known = map[string]bool{"model": true, "input": true, "dimensions": true, "encoding_format": true, "user": true}

// DecodeRequest parses an embeddings request.
func (Codec) DecodeRequest(_ *http.Request, _ protocol.Inbound, body []byte) (*ir.Request, ir.Needs, error) {
	if !gjson.ValidBytes(body) {
		return nil, ir.Needs{}, ir.Errorf(ir.ErrInvalidRequest, "request body is not valid JSON")
	}
	root := gjson.ParseBytes(body)
	e := &ir.Embed{Dimensions: int(root.Get("dimensions").Int()), User: root.Get("user").String()}
	switch f := root.Get("encoding_format").String(); f {
	case "", "float":
	case "base64":
		e.Base64 = true
	default:
		return nil, ir.Needs{}, ir.Errorf(ir.ErrInvalidRequest, "encoding_format %q is not supported", f)
	}
	needs := ir.Needs{Modality: ir.ModalityEmbeddings}
	in := root.Get("input")
	chars := 0
	switch {
	case in.Type == gjson.String:
		e.Inputs = []ir.EmbedInput{{Text: in.String()}}
		chars = len(in.String())
	case in.IsArray():
		arr := in.Array()
		if len(arr) > 0 && arr[0].Type == gjson.Number {
			toks, ok := tokens(in)
			if !ok {
				return nil, needs, ir.Errorf(ir.ErrInvalidRequest, "input: mixed token ids and text")
			}
			e.Inputs = []ir.EmbedInput{{Tokens: toks}}
			needs.EmbedTokens = true
			chars = 4 * len(toks)
			break
		}
		e.Batch = true
		for _, v := range arr {
			switch {
			case v.Type == gjson.String:
				e.Inputs = append(e.Inputs, ir.EmbedInput{Text: v.String()})
				chars += len(v.String())
			case v.IsArray():
				toks, ok := tokens(v)
				if !ok {
					return nil, needs, ir.Errorf(ir.ErrInvalidRequest, "input: an array must hold strings or token ids")
				}
				e.Inputs = append(e.Inputs, ir.EmbedInput{Tokens: toks})
				needs.EmbedTokens = true
				chars += 4 * len(toks)
			default:
				return nil, needs, ir.Errorf(ir.ErrInvalidRequest, "input: an array must hold strings or token ids")
			}
		}
	}
	if len(e.Inputs) == 0 {
		return nil, needs, ir.Errorf(ir.ErrInvalidRequest, "input must not be empty")
	}
	needs.InputTokensEst = media.EstimateTokens(chars)
	req := &ir.Request{Model: root.Get("model").String(), Embed: e}
	root.ForEach(func(k, v gjson.Result) bool {
		if !known[k.String()] {
			req.Extra.Set(Name, k.String(), json.RawMessage(v.Raw))
		}
		return true
	})
	return req, needs, nil
}

func tokens(v gjson.Result) ([]int64, bool) {
	var out []int64
	ok := true
	v.ForEach(func(_, t gjson.Result) bool {
		if t.Type != gjson.Number {
			ok = false
			return false
		}
		out = append(out, t.Int())
		return true
	})
	return out, ok && len(out) > 0
}

// EncodeRequest renders an upstream request asking for float vectors.
func (Codec) EncodeRequest(req *ir.Request) ([]byte, error) {
	if req.Embed == nil {
		return nil, ir.Errorf(ir.ErrCapabilityMissing, "openai-embeddings only serves embeddings")
	}
	e := req.Embed
	var input any
	if len(e.Inputs) == 1 && !e.Batch {
		input = inputValue(e.Inputs[0])
	} else {
		list := make([]any, len(e.Inputs))
		for i, in := range e.Inputs {
			list[i] = inputValue(in)
		}
		input = list
	}
	out := map[string]any{"model": req.Model, "input": input}
	if e.Dimensions > 0 {
		out["dimensions"] = e.Dimensions
	}
	if e.User != "" {
		out["user"] = e.User
	}
	if req.Extra.Origin == Name {
		for k, v := range req.Extra.Fields {
			if !known[k] {
				out[k] = v
			}
		}
	}
	return json.Marshal(out)
}

func inputValue(in ir.EmbedInput) any {
	if in.Tokens != nil {
		return in.Tokens
	}
	return in.Text
}

type wireResponse struct {
	Model string `json:"model"`
	Data  []struct {
		Index     int             `json:"index"`
		Embedding json.RawMessage `json:"embedding"`
	} `json:"data"`
	Usage json.RawMessage `json:"usage"`
}

// DecodeResponse parses an upstream embeddings list; base64 vectors are
// decoded.
func (Codec) DecodeResponse(body []byte) (*ir.Response, error) {
	var w wireResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, err
	}
	sort.SliceStable(w.Data, func(i, j int) bool { return w.Data[i].Index < w.Data[j].Index })
	resp := &ir.Response{Model: w.Model, StopReason: ir.StopEndTurn}
	for _, d := range w.Data {
		var v []float64
		var s string
		if err := json.Unmarshal(d.Embedding, &v); err != nil {
			if err := json.Unmarshal(d.Embedding, &s); err != nil {
				return nil, err
			}
			if v, err = media.DecodeBase64(s); err != nil {
				return nil, err
			}
		}
		resp.Embeddings = append(resp.Embeddings, v)
	}
	if len(w.Usage) > 0 {
		u := gjson.ParseBytes(w.Usage)
		resp.Usage = ir.Usage{Input: u.Get("prompt_tokens").Int(), Raw: w.Usage}
	}
	return resp, nil
}

// EncodeResponse renders the embeddings list, base64 when the client asked.
func (Codec) EncodeResponse(req *ir.Request, resp *ir.Response) ([]byte, error) {
	b64 := req != nil && req.Embed != nil && req.Embed.Base64
	data := make([]map[string]any, len(resp.Embeddings))
	for i, v := range resp.Embeddings {
		var emb any = v
		if b64 {
			emb = media.EncodeBase64(v)
		} else if v == nil {
			emb = []float64{}
		}
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": emb}
	}
	model := resp.Model
	if model == "" && req != nil {
		model = req.Model
	}
	n := resp.Usage.TotalInput()
	return json.Marshal(map[string]any{"object": "list", "data": data, "model": model,
		"usage": map[string]int64{"prompt_tokens": n, "total_tokens": n}})
}

// NewStreamEncoder returns an encoder that only renders errors; embeddings
// have no stream.
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

// UpstreamPath returns "/embeddings"; upstream base URLs include /v1.
func (Codec) UpstreamPath(*ir.Request, protocol.Inbound) string { return "/embeddings" }

// SetModel rewrites the top-level model field, leaving every other byte intact.
func (Codec) SetModel(body []byte, model string) ([]byte, error) {
	return sjson.SetBytes(body, "model", model)
}

// StripOpaque is a no-op: embeddings carry no upstream-private state.
func (Codec) StripOpaque(body []byte) ([]byte, int, error) { return body, 0, nil }
