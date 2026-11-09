// Package teirerank implements the rerank route of Hugging Face Text
// Embeddings Inference ("tei-rerank"): POST /rerank on an upstream. It has
// no client routes: the server also accepts the common rerank protocol's
// /v1/rerank as /rerank, so a client route here would collide.
//
// The request is {query, texts, return_text, raw_scores, truncate}; the
// response a bare array of {index, score, text?} with no model and no
// usage, so the relay estimates tokens. There is no top_n: every text is
// scored and the client codec keeps the best n.
package teirerank

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/protocol/internal/media"
	"github.com/initxy/plugrelay/pkg/protocol/openaichat"
	"github.com/initxy/plugrelay/pkg/sse"
)

// Name is the protocol id of this codec.
const Name = "tei-rerank"

// Codec is the TEI rerank codec. The zero value is ready to use.
type Codec struct{}

func init() { protocol.Register(Codec{}) }

var _ protocol.Codec = Codec{}

var openai = openaichat.Codec{}

// Name returns "tei-rerank".
func (Codec) Name() string { return Name }

// Capabilities reports rerank only.
func (Codec) Capabilities() ir.Capabilities {
	return ir.Capabilities{Cache: ir.CacheNone, MediaOnly: true, Rerank: true}
}

// Routes is empty: the protocol is upstream-only.
func (Codec) Routes() []protocol.Route { return nil }

// Sniff is never called without client routes.
func (Codec) Sniff(_ *http.Request, route protocol.Route, body []byte) (protocol.Inbound, error) {
	return media.SniffModel(route, body)
}

var known = map[string]bool{"query": true, "texts": true, "documents": true}

// DecodeRequest parses a TEI request (used by tests and fakes).
func (Codec) DecodeRequest(_ *http.Request, _ protocol.Inbound, body []byte) (*ir.Request, ir.Needs, error) {
	b, err := renameTexts(body)
	if err != nil {
		return nil, ir.Needs{Modality: ir.ModalityRerank}, err
	}
	return media.DecodeRerank(b, known, Name)
}

func renameTexts(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, ir.Errorf(ir.ErrInvalidRequest, "request body is not valid JSON")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, ir.Errorf(ir.ErrInvalidRequest, "request body must be an object")
	}
	m["documents"] = m["texts"]
	delete(m, "texts")
	return json.Marshal(m)
}

// EncodeRequest renders a TEI request: texts only, scores normalized.
func (Codec) EncodeRequest(req *ir.Request) ([]byte, error) {
	rr := req.Rerank
	if rr == nil {
		return nil, ir.Errorf(ir.ErrCapabilityMissing, "tei-rerank only serves rerank requests")
	}
	texts := make([]string, len(rr.Documents))
	for i, d := range rr.Documents {
		texts[i] = d.Text
	}
	out := map[string]any{"query": rr.Query, "texts": texts, "return_text": false, "raw_scores": false}
	if req.Extra.Origin == Name {
		for k, v := range req.Extra.Fields {
			if !known[k] {
				out[k] = v
			}
		}
	}
	return json.Marshal(out)
}

// DecodeResponse parses the array of ranks, best first.
func (Codec) DecodeResponse(body []byte) (*ir.Response, error) {
	r := gjson.ParseBytes(body)
	if !gjson.ValidBytes(body) || !r.IsArray() {
		return nil, ir.Errorf(ir.ErrUpstream, "rerank response is not a JSON array")
	}
	resp := &ir.Response{StopReason: ir.StopEndTurn}
	r.ForEach(func(_, v gjson.Result) bool {
		resp.Ranks = append(resp.Ranks, ir.Rank{Index: int(v.Get("index").Int()), Score: v.Get("score").Float()})
		return true
	})
	sort.SliceStable(resp.Ranks, func(i, j int) bool { return resp.Ranks[i].Score > resp.Ranks[j].Score })
	return resp, nil
}

// EncodeResponse renders the TEI array (used by tests and fakes).
func (Codec) EncodeResponse(req *ir.Request, resp *ir.Response) ([]byte, error) {
	out := make([]map[string]any, len(resp.Ranks))
	for i, k := range resp.Ranks {
		out[i] = map[string]any{"index": k.Index, "score": k.Score}
	}
	return json.Marshal(out)
}

// NewStreamEncoder returns an encoder that only renders errors.
func (Codec) NewStreamEncoder(w sse.Writer, _ *ir.Request) protocol.StreamEncoder {
	return &media.NoStreamEncoder{W: w}
}

// NewStreamDecoder returns a decoder that never completes.
func (Codec) NewStreamDecoder() protocol.StreamDecoder { return media.NoStreamDecoder{} }

// EncodeError renders TEI's {"error", "error_type"} body.
func (Codec) EncodeError(e *ir.GwError) (int, http.Header, []byte) {
	status, h, _ := openai.EncodeError(e)
	body, _ := json.Marshal(map[string]string{"error": e.Message, "error_type": string(e.Kind)})
	return status, h, body
}

// DecodeError reads TEI's {"error": "..."} (or an OpenAI-style) body.
func (Codec) DecodeError(status int, header http.Header, body []byte) *ir.GwError {
	return openai.DecodeError(status, header, body)
}

// EncodeModels renders the OpenAI list format.
func (Codec) EncodeModels(r *http.Request, models []ir.ModelInfo) (string, []byte) {
	return openai.EncodeModels(r, models)
}

// CountTokens is not part of this protocol.
func (Codec) CountTokens(*ir.Request) ([]byte, error) { return nil, media.ErrUnsupported }

// UpstreamPath returns "/rerank"; the base URL is the server root.
func (Codec) UpstreamPath(*ir.Request, protocol.Inbound) string { return "/rerank" }

// SetModel returns body unchanged: a TEI server serves one model.
func (Codec) SetModel(body []byte, _ string) ([]byte, error) { return body, nil }

// StripOpaque is a no-op.
func (Codec) StripOpaque(body []byte) ([]byte, int, error) { return body, 0, nil }
