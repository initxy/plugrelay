// Package rerank implements the common rerank wire protocol ("rerank"):
// POST /v1/rerank in the shape Jina AI introduced and vLLM and Cohere's v1
// API share, on the client side and upstream.
//
// A request is a query, documents given as strings or as objects with a
// "text" field (other fields are kept and returned with the result),
// top_n and return_documents. A document or query that is not text
// (an image, a multimodal content list) does not decode, which limits the
// request to same-protocol passthrough. Results carry the request index
// and relevance score, plus the document unless return_documents is
// false; the relay fills documents from the request when an upstream of
// another protocol returns none, and keeps only the best top_n. Usage is
// read from usage.total_tokens (prompt_tokens when present) and from
// Cohere's meta.billed_units.search_units. Errors and model lists use the
// OpenAI shapes of package openaichat.
package rerank

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
const Name = "rerank"

// Codec is the rerank codec. The zero value is ready to use.
type Codec struct{}

func init() { protocol.Register(Codec{}) }

var _ protocol.Codec = Codec{}

var openai = openaichat.Codec{}

// Name returns "rerank".
func (Codec) Name() string { return Name }

// Capabilities reports rerank only.
func (Codec) Capabilities() ir.Capabilities {
	return ir.Capabilities{Cache: ir.CacheNone, MediaOnly: true, Rerank: true}
}

// Routes lists the rerank endpoint.
func (Codec) Routes() []protocol.Route {
	return []protocol.Route{{Method: http.MethodPost, Path: "/v1/rerank", Op: protocol.OpRerank}}
}

// Sniff reads the model.
func (Codec) Sniff(_ *http.Request, route protocol.Route, body []byte) (protocol.Inbound, error) {
	return media.SniffModel(route, body)
}

// known are the fields with an IR slot.
var known = map[string]bool{"model": true, "query": true, "documents": true, "top_n": true,
	"return_documents": true, "max_tokens_per_doc": true}

// DecodeRequest parses a rerank request.
func (Codec) DecodeRequest(_ *http.Request, _ protocol.Inbound, body []byte) (*ir.Request, ir.Needs, error) {
	req, needs, err := media.DecodeRerank(body, known, Name)
	if err != nil {
		return nil, needs, err
	}
	if rd := gjson.GetBytes(body, "return_documents"); rd.Exists() {
		b := rd.Bool()
		req.Rerank.ReturnDocuments = &b
	}
	return req, needs, nil
}

// EncodeRequest renders an upstream request.
func (Codec) EncodeRequest(req *ir.Request) ([]byte, error) {
	rr := req.Rerank
	if rr == nil {
		return nil, ir.Errorf(ir.ErrCapabilityMissing, "rerank only serves rerank requests")
	}
	docs := make([]any, len(rr.Documents))
	for i, d := range rr.Documents {
		if d.Object != nil {
			docs[i] = d.Object
		} else {
			docs[i] = d.Text
		}
	}
	out := map[string]any{"model": req.Model, "query": rr.Query, "documents": docs}
	if rr.TopN > 0 {
		out["top_n"] = rr.TopN
	}
	if rr.ReturnDocuments != nil {
		out["return_documents"] = *rr.ReturnDocuments
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

// DecodeResponse parses an upstream's results, best first.
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

// EncodeResponse renders the results, with documents from the request
// unless the client turned them off.
func (Codec) EncodeResponse(req *ir.Request, resp *ir.Response) ([]byte, error) {
	var rr *ir.Rerank
	if req != nil {
		rr = req.Rerank
	}
	withDocs := rr != nil && (rr.ReturnDocuments == nil || *rr.ReturnDocuments)
	ranks := media.TopRanks(resp.Ranks, rr)
	results := make([]map[string]any, len(ranks))
	for i, k := range ranks {
		res := map[string]any{"index": k.Index, "relevance_score": k.Score}
		if withDocs && k.Index >= 0 && k.Index < len(rr.Documents) {
			d := rr.Documents[k.Index]
			if d.Object != nil {
				res["document"] = d.Object
			} else {
				res["document"] = map[string]string{"text": d.Text}
			}
		}
		results[i] = res
	}
	model := resp.Model
	if model == "" && req != nil {
		model = req.Model
	}
	out := map[string]any{"model": model, "results": results}
	if resp.ID != "" {
		out["id"] = resp.ID
	}
	n := resp.Usage.TotalInput()
	out["usage"] = map[string]int64{"prompt_tokens": n, "total_tokens": n}
	if su := resp.Usage.SearchUnits; su > 0 {
		out["meta"] = map[string]any{"billed_units": map[string]int64{"search_units": su}}
	}
	return json.Marshal(out)
}

// NewStreamEncoder returns an encoder that only renders errors.
func (Codec) NewStreamEncoder(w sse.Writer, _ *ir.Request) protocol.StreamEncoder {
	return &media.NoStreamEncoder{W: w}
}

// NewStreamDecoder returns a decoder that never completes.
func (Codec) NewStreamDecoder() protocol.StreamDecoder { return media.NoStreamDecoder{} }

// EncodeError renders the OpenAI error format.
func (Codec) EncodeError(e *ir.GwError) (int, http.Header, []byte) { return openai.EncodeError(e) }

// DecodeError reads an OpenAI-style error, or a bare {"message"} or
// {"error": "..."} body.
func (Codec) DecodeError(status int, header http.Header, body []byte) *ir.GwError {
	return openai.DecodeError(status, header, body)
}

// EncodeModels renders the OpenAI list format.
func (Codec) EncodeModels(r *http.Request, models []ir.ModelInfo) (string, []byte) {
	return openai.EncodeModels(r, models)
}

// CountTokens is not part of this protocol.
func (Codec) CountTokens(*ir.Request) ([]byte, error) { return nil, media.ErrUnsupported }

// UpstreamPath returns "/rerank"; upstream base URLs include the version
// (/v1).
func (Codec) UpstreamPath(*ir.Request, protocol.Inbound) string { return "/rerank" }

// SetModel rewrites the top-level model field, leaving every other byte intact.
func (Codec) SetModel(body []byte, model string) ([]byte, error) {
	return sjson.SetBytes(body, "model", model)
}

// StripOpaque is a no-op: rerank carries no upstream-private state.
func (Codec) StripOpaque(body []byte) ([]byte, int, error) { return body, 0, nil }
