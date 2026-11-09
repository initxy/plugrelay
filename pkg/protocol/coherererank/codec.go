// Package coherererank implements Cohere's v2 rerank wire protocol
// ("cohere-rerank"): POST /v2/rerank on the client side and upstream.
//
// It differs from the common rerank protocol (package rerank) in what it
// leaves out: documents are strings only, there is no return_documents,
// and results carry index and relevance_score but never the document.
// Usage is reported in meta.billed_units (search_units, input_tokens) and
// meta.tokens. Objects arriving from another protocol are sent as their
// text. Errors are Cohere's {"message"} body.
package coherererank

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
const Name = "cohere-rerank"

// Codec is the Cohere v2 rerank codec. The zero value is ready to use.
type Codec struct{}

func init() { protocol.Register(Codec{}) }

var _ protocol.Codec = Codec{}

var openai = openaichat.Codec{}

// Name returns "cohere-rerank".
func (Codec) Name() string { return Name }

// Capabilities reports rerank only.
func (Codec) Capabilities() ir.Capabilities {
	return ir.Capabilities{Cache: ir.CacheNone, MediaOnly: true, Rerank: true}
}

// Routes lists the v2 rerank endpoint.
func (Codec) Routes() []protocol.Route {
	return []protocol.Route{{Method: http.MethodPost, Path: "/v2/rerank", Op: protocol.OpRerank}}
}

// Sniff reads the model.
func (Codec) Sniff(_ *http.Request, route protocol.Route, body []byte) (protocol.Inbound, error) {
	return media.SniffModel(route, body)
}

var known = map[string]bool{"model": true, "query": true, "documents": true, "top_n": true, "max_tokens_per_doc": true}

// DecodeRequest parses a v2 rerank request.
func (Codec) DecodeRequest(_ *http.Request, _ protocol.Inbound, body []byte) (*ir.Request, ir.Needs, error) {
	return media.DecodeRerank(body, known, Name)
}

// EncodeRequest renders a v2 request: documents as strings.
func (Codec) EncodeRequest(req *ir.Request) ([]byte, error) {
	rr := req.Rerank
	if rr == nil {
		return nil, ir.Errorf(ir.ErrCapabilityMissing, "cohere-rerank only serves rerank requests")
	}
	docs := make([]string, len(rr.Documents))
	for i, d := range rr.Documents {
		docs[i] = d.Text
	}
	out := map[string]any{"model": req.Model, "query": rr.Query, "documents": docs}
	if rr.TopN > 0 {
		out["top_n"] = rr.TopN
	}
	if rr.MaxTokensPerDoc > 0 {
		out["max_tokens_per_doc"] = rr.MaxTokensPerDoc
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

// DecodeResponse parses the results and billed units.
func (Codec) DecodeResponse(body []byte) (*ir.Response, error) {
	if !gjson.ValidBytes(body) {
		return nil, ir.Errorf(ir.ErrUpstream, "rerank response is not valid JSON")
	}
	r := gjson.ParseBytes(body)
	results := r.Get("results")
	if !results.IsArray() {
		return nil, ir.Errorf(ir.ErrUpstream, "rerank response has no results")
	}
	resp := &ir.Response{ID: r.Get("id").String(), Model: r.Get("model").String(), StopReason: ir.StopEndTurn}
	results.ForEach(func(_, v gjson.Result) bool {
		resp.Ranks = append(resp.Ranks, ir.Rank{Index: int(v.Get("index").Int()), Score: v.Get("relevance_score").Float()})
		return true
	})
	sort.SliceStable(resp.Ranks, func(i, j int) bool { return resp.Ranks[i].Score > resp.Ranks[j].Score })
	resp.Usage = media.RerankUsage(r)
	return resp, nil
}

// EncodeResponse renders v2 results and billed units.
func (Codec) EncodeResponse(req *ir.Request, resp *ir.Response) ([]byte, error) {
	var rr *ir.Rerank
	if req != nil {
		rr = req.Rerank
	}
	ranks := media.TopRanks(resp.Ranks, rr)
	results := make([]map[string]any, len(ranks))
	for i, k := range ranks {
		results[i] = map[string]any{"index": k.Index, "relevance_score": k.Score}
	}
	billed := map[string]int64{}
	if n := resp.Usage.TotalInput(); n > 0 {
		billed["input_tokens"] = n
	}
	if su := resp.Usage.SearchUnits; su > 0 {
		billed["search_units"] = su
	}
	out := map[string]any{"results": results,
		"meta": map[string]any{"api_version": map[string]string{"version": "2"}, "billed_units": billed}}
	if resp.ID != "" {
		out["id"] = resp.ID
	}
	return json.Marshal(out)
}

// NewStreamEncoder returns an encoder that only renders errors.
func (Codec) NewStreamEncoder(w sse.Writer, _ *ir.Request) protocol.StreamEncoder {
	return &media.NoStreamEncoder{W: w}
}

// NewStreamDecoder returns a decoder that never completes.
func (Codec) NewStreamDecoder() protocol.StreamDecoder { return media.NoStreamDecoder{} }

// EncodeError renders Cohere's {"message"} error with the status the
// OpenAI mapping picks for the error kind.
func (Codec) EncodeError(e *ir.GwError) (int, http.Header, []byte) {
	status, h, _ := openai.EncodeError(e)
	body, _ := json.Marshal(map[string]string{"message": e.Message})
	return status, h, body
}

// DecodeError reads a {"message"} error (or an OpenAI-style one).
func (Codec) DecodeError(status int, header http.Header, body []byte) *ir.GwError {
	return openai.DecodeError(status, header, body)
}

// EncodeModels renders the OpenAI list format.
func (Codec) EncodeModels(r *http.Request, models []ir.ModelInfo) (string, []byte) {
	return openai.EncodeModels(r, models)
}

// CountTokens is not part of this protocol.
func (Codec) CountTokens(*ir.Request) ([]byte, error) { return nil, media.ErrUnsupported }

// UpstreamPath returns "/rerank"; the base URL ends in /v2.
func (Codec) UpstreamPath(*ir.Request, protocol.Inbound) string { return "/rerank" }

// SetModel rewrites the top-level model field, leaving every other byte intact.
func (Codec) SetModel(body []byte, model string) ([]byte, error) {
	return sjson.SetBytes(body, "model", model)
}

// StripOpaque is a no-op.
func (Codec) StripOpaque(body []byte) ([]byte, int, error) { return body, 0, nil }
