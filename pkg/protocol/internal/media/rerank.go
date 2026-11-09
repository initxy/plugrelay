package media

import (
	"encoding/json"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// SniffModel reads the top-level model of a JSON body.
func SniffModel(route protocol.Route, body []byte) (protocol.Inbound, error) {
	in := protocol.Inbound{Op: route.Op}
	if !gjson.ValidBytes(body) {
		return in, ir.Errorf(ir.ErrInvalidRequest, "request body is not valid JSON")
	}
	in.Model = gjson.GetBytes(body, "model").String()
	return in, nil
}

// DecodeRerank parses the fields the rerank protocols share: model, query,
// documents (strings, or objects with a "text" field), top_n and
// max_tokens_per_doc. Fields not in known are kept in Extra for origin.
func DecodeRerank(body []byte, known map[string]bool, origin string) (*ir.Request, ir.Needs, error) {
	needs := ir.Needs{Modality: ir.ModalityRerank}
	if !gjson.ValidBytes(body) {
		return nil, needs, ir.Errorf(ir.ErrInvalidRequest, "request body is not valid JSON")
	}
	root := gjson.ParseBytes(body)
	q := root.Get("query")
	if q.Type != gjson.String {
		return nil, needs, ir.Errorf(ir.ErrInvalidRequest, "only a text query can be converted for another protocol")
	}
	rr := &ir.Rerank{Query: q.String(), TopN: int(root.Get("top_n").Int()),
		MaxTokensPerDoc: int(root.Get("max_tokens_per_doc").Int())}
	docs := root.Get("documents")
	if !docs.IsArray() {
		return nil, needs, ir.Errorf(ir.ErrInvalidRequest, "documents must be an array")
	}
	chars := 0
	var bad bool
	docs.ForEach(func(_, d gjson.Result) bool {
		switch {
		case d.Type == gjson.String:
			rr.Documents = append(rr.Documents, ir.RerankDoc{Text: d.String()})
		case d.IsObject() && d.Get("text").Type == gjson.String:
			rr.Documents = append(rr.Documents, ir.RerankDoc{Text: d.Get("text").String(), Object: json.RawMessage(d.Raw)})
		default:
			bad = true
			return false
		}
		chars += len(rr.Documents[len(rr.Documents)-1].Text) + len(rr.Query)
		return true
	})
	if bad {
		return nil, needs, ir.Errorf(ir.ErrInvalidRequest, "only text documents can be converted for another protocol")
	}
	if len(rr.Documents) == 0 {
		return nil, needs, ir.Errorf(ir.ErrInvalidRequest, "documents must not be empty")
	}
	needs.InputTokensEst = EstimateTokens(chars)
	req := &ir.Request{Model: root.Get("model").String(), Rerank: rr}
	root.ForEach(func(k, v gjson.Result) bool {
		if !known[k.String()] {
			req.Extra.Set(origin, k.String(), json.RawMessage(v.Raw))
		}
		return true
	})
	return req, needs, nil
}

// RerankUsage reads token counts (usage.prompt_tokens when not 0, else total_tokens,
// else Cohere's billed or counted input tokens, else SiliconFlow's
// top-level tokens.input_tokens) and search units (Cohere's
// meta.billed_units, or usage.search_units as OpenRouter and Portkey send).
func RerankUsage(r gjson.Result) ir.Usage {
	var u ir.Usage
	us, meta, toks := r.Get("usage"), r.Get("meta"), r.Get("tokens")
	switch {
	// Some servers send prompt_tokens 0 next to the real total_tokens.
	case us.Get("prompt_tokens").Int() > 0 || us.Get("prompt_tokens").Exists() && !us.Get("total_tokens").Exists():
		u.Input = us.Get("prompt_tokens").Int()
	case us.Get("total_tokens").Exists():
		u.Input = us.Get("total_tokens").Int()
	case meta.Get("billed_units.input_tokens").Exists():
		u.Input = meta.Get("billed_units.input_tokens").Int()
	case meta.Get("tokens.input_tokens").Exists():
		u.Input = meta.Get("tokens.input_tokens").Int()
	default:
		u.Input = toks.Get("input_tokens").Int()
	}
	u.SearchUnits = meta.Get("billed_units.search_units").Int()
	if u.SearchUnits == 0 {
		u.SearchUnits = us.Get("search_units").Int()
	}
	switch {
	case us.Exists():
		u.Raw = json.RawMessage(us.Raw)
	case meta.Exists():
		u.Raw = json.RawMessage(meta.Raw)
	case toks.Exists():
		u.Raw = json.RawMessage(toks.Raw)
	}
	return u
}

// TopRanks returns ranks cut to the request's top_n.
func TopRanks(ranks []ir.Rank, rr *ir.Rerank) []ir.Rank {
	if rr != nil && rr.TopN > 0 && len(ranks) > rr.TopN {
		return ranks[:rr.TopN]
	}
	return ranks
}
