// Package geminiembeddings implements the Gemini embeddings wire protocol
// ("gemini-embeddings"): POST /v1beta/models/{model}:embedContent and
// :batchEmbedContents on the client side and upstream.
//
// One embedContent request or each entry of a batch is one input; the
// text parts of its content are joined with newlines. Content with other
// parts (images, audio) does not decode, which limits it to same-protocol
// passthrough. Requests built from another protocol always go upstream as
// batchEmbedContents. The protocol has no base64 output and no token-id
// input. Errors and model lists use the Gemini shapes of package gemini.
package geminiembeddings

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/protocol/gemini"
	"github.com/initxy/plugrelay/pkg/protocol/internal/media"
	"github.com/initxy/plugrelay/pkg/sse"
)

// Name is the protocol id of this codec.
const Name = "gemini-embeddings"

// Path actions.
const (
	ActionEmbed = "embedContent"
	ActionBatch = "batchEmbedContents"
)

// Codec is the Gemini embeddings codec. The zero value is ready to use.
type Codec struct{}

func init() { protocol.Register(Codec{}) }

var _ protocol.Codec = Codec{}

var native = gemini.Codec{}

// Name returns "gemini-embeddings".
func (Codec) Name() string { return Name }

// Capabilities reports embeddings only.
func (Codec) Capabilities() ir.Capabilities {
	return ir.Capabilities{Cache: ir.CacheNone, MediaOnly: true, Embeddings: true}
}

// Routes lists both embedding actions.
func (Codec) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/v1beta/models/{model}:" + ActionEmbed, Op: protocol.OpEmbed},
		{Method: http.MethodPost, Path: "/v1beta/models/{model}:" + ActionBatch, Op: protocol.OpEmbed},
	}
}

// Sniff reads model and action from the path.
func (Codec) Sniff(r *http.Request, route protocol.Route, body []byte) (protocol.Inbound, error) {
	in := protocol.Inbound{Op: route.Op}
	if r == nil {
		return in, ir.Errorf(ir.ErrInvalidRequest, "no request path")
	}
	model, action, ok := gemini.ParsePath(r.URL.Path)
	if !ok || (action != ActionEmbed && action != ActionBatch) {
		return in, ir.Errorf(ir.ErrNotFound, "unrecognized Gemini embeddings path %q", r.URL.Path)
	}
	if !gjson.ValidBytes(body) {
		return in, ir.Errorf(ir.ErrInvalidRequest, "request body is not valid JSON")
	}
	in.Model, in.Action = model, action
	return in, nil
}

// DecodeRequest parses embedContent or batchEmbedContents.
func (Codec) DecodeRequest(_ *http.Request, in protocol.Inbound, body []byte) (*ir.Request, ir.Needs, error) {
	if !gjson.ValidBytes(body) {
		return nil, ir.Needs{}, ir.Errorf(ir.ErrInvalidRequest, "request body is not valid JSON")
	}
	root := gjson.ParseBytes(body)
	entries := []gjson.Result{root}
	e := &ir.Embed{}
	if in.Action == ActionBatch {
		e.Batch = true
		entries = root.Get("requests").Array()
	}
	needs := ir.Needs{Modality: ir.ModalityEmbeddings}
	chars := 0
	for i, r := range entries {
		var texts []string
		var bad bool
		r.Get("content.parts").ForEach(func(_, p gjson.Result) bool {
			t := p.Get("text")
			if !t.Exists() {
				bad = true
				return false
			}
			texts = append(texts, t.String())
			return true
		})
		if bad {
			return nil, needs, ir.Errorf(ir.ErrInvalidRequest, "only text parts can be converted for another protocol")
		}
		text := strings.Join(texts, "\n")
		chars += len(text)
		e.Inputs = append(e.Inputs, ir.EmbedInput{Text: text})
		if i == 0 {
			e.Dimensions = int(r.Get("outputDimensionality").Int())
			e.TaskType = r.Get("taskType").String()
			e.Title = r.Get("title").String()
		}
	}
	if len(e.Inputs) == 0 {
		return nil, needs, ir.Errorf(ir.ErrInvalidRequest, "requests must not be empty")
	}
	needs.InputTokensEst = media.EstimateTokens(chars)
	return &ir.Request{Model: in.Model, Embed: e}, needs, nil
}

type wireEntry struct {
	Model                string      `json:"model"`
	Content              wireContent `json:"content"`
	TaskType             string      `json:"taskType,omitempty"`
	Title                string      `json:"title,omitempty"`
	OutputDimensionality int         `json:"outputDimensionality,omitempty"`
}

type wireContent struct {
	Parts []wirePart `json:"parts"`
}

type wirePart struct {
	Text string `json:"text"`
}

// EncodeRequest renders a batchEmbedContents body.
func (Codec) EncodeRequest(req *ir.Request) ([]byte, error) {
	e := req.Embed
	if e == nil {
		return nil, ir.Errorf(ir.ErrCapabilityMissing, "gemini-embeddings only serves embeddings")
	}
	model := "models/" + strings.TrimPrefix(req.Model, "models/")
	out := make([]wireEntry, len(e.Inputs))
	for i, in := range e.Inputs {
		if in.Tokens != nil {
			return nil, ir.Errorf(ir.ErrCapabilityMissing, "gemini-embeddings takes text, not token ids")
		}
		out[i] = wireEntry{Model: model, Content: wireContent{Parts: []wirePart{{Text: in.Text}}},
			TaskType: e.TaskType, Title: e.Title, OutputDimensionality: e.Dimensions}
	}
	return json.Marshal(map[string]any{"requests": out})
}

// DecodeResponse parses one embedding or a batch of them.
func (Codec) DecodeResponse(body []byte) (*ir.Response, error) {
	if !gjson.ValidBytes(body) {
		return nil, ir.Errorf(ir.ErrUpstream, "embeddings response is not valid JSON")
	}
	root := gjson.ParseBytes(body)
	resp := &ir.Response{StopReason: ir.StopEndTurn}
	list := root.Get("embeddings").Array()
	if one := root.Get("embedding"); one.Exists() {
		list = []gjson.Result{one}
	}
	for _, v := range list {
		var vec []float64
		if err := json.Unmarshal([]byte(v.Get("values").Raw), &vec); err != nil {
			return nil, err
		}
		resp.Embeddings = append(resp.Embeddings, vec)
	}
	if u := root.Get("usageMetadata"); u.Exists() {
		resp.Usage = ir.Usage{Input: u.Get("promptTokenCount").Int(), Raw: json.RawMessage(u.Raw)}
	}
	return resp, nil
}

// EncodeResponse renders the shape of the client's action.
func (Codec) EncodeResponse(req *ir.Request, resp *ir.Response) ([]byte, error) {
	vals := func(v []float64) map[string]any {
		if v == nil {
			v = []float64{}
		}
		return map[string]any{"values": v}
	}
	if req != nil && req.Embed != nil && !req.Embed.Batch && len(resp.Embeddings) == 1 {
		return json.Marshal(map[string]any{"embedding": vals(resp.Embeddings[0])})
	}
	list := make([]map[string]any, len(resp.Embeddings))
	for i, v := range resp.Embeddings {
		list[i] = vals(v)
	}
	return json.Marshal(map[string]any{"embeddings": list})
}

// NewStreamEncoder returns an encoder that only renders errors.
func (Codec) NewStreamEncoder(w sse.Writer, _ *ir.Request) protocol.StreamEncoder {
	return &media.NoStreamEncoder{W: w}
}

// NewStreamDecoder returns a decoder that never completes.
func (Codec) NewStreamDecoder() protocol.StreamDecoder { return media.NoStreamDecoder{} }

// EncodeError renders the Gemini error format.
func (Codec) EncodeError(e *ir.GwError) (int, http.Header, []byte) { return native.EncodeError(e) }

// DecodeError reads a Gemini upstream error.
func (Codec) DecodeError(status int, header http.Header, body []byte) *ir.GwError {
	return native.DecodeError(status, header, body)
}

// EncodeModels renders the Gemini list format.
func (Codec) EncodeModels(r *http.Request, models []ir.ModelInfo) (string, []byte) {
	return native.EncodeModels(r, models)
}

// CountTokens is not part of this protocol.
func (Codec) CountTokens(*ir.Request) ([]byte, error) { return nil, media.ErrUnsupported }

// UpstreamPath keeps the client's action for the same protocol and uses
// batchEmbedContents otherwise.
func (Codec) UpstreamPath(req *ir.Request, in protocol.Inbound) string {
	action := in.Action
	if action != ActionEmbed && action != ActionBatch {
		action = ActionBatch
	}
	return "/v1beta/models/" + strings.TrimPrefix(req.Model, "models/") + ":" + action
}

// SetModel rewrites the model each batch entry names, if any; the path
// carries the model otherwise.
func (Codec) SetModel(body []byte, model string) ([]byte, error) {
	n := int(gjson.GetBytes(body, "requests.#").Int())
	out := body
	for i := range n {
		p := "requests." + strconv.Itoa(i) + ".model"
		if !gjson.GetBytes(out, p).Exists() {
			continue
		}
		var err error
		if out, err = sjson.SetBytes(out, p, "models/"+strings.TrimPrefix(model, "models/")); err != nil {
			return body, err
		}
	}
	if gjson.GetBytes(out, "model").Exists() {
		return sjson.SetBytes(out, "model", "models/"+strings.TrimPrefix(model, "models/"))
	}
	return out, nil
}

// StripOpaque is a no-op: embeddings carry no upstream-private state.
func (Codec) StripOpaque(body []byte) ([]byte, int, error) { return body, 0, nil }
