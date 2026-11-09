package gemini

import (
	"encoding/json"
	"sort"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/ir"
)

// Response Extra keys owned by this codec.
const (
	extraCandidate    = "candidate" // unmodelled candidate fields, as an object
	extraFinishReason = "finishReason"
)

var knownResponseKeys = map[string]bool{
	"candidates": true, "usageMetadata": true, "modelVersion": true, "responseId": true,
}

var knownCandidateKeys = map[string]bool{"content": true, "finishReason": true, "index": true}

type wireUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
}

// decodeUsage maps usageMetadata to IR buckets. promptTokenCount includes
// cached tokens; tool-use prompt tokens count as input.
func decodeUsage(raw json.RawMessage) (ir.Usage, bool) {
	var w wireUsage
	if len(raw) == 0 || json.Unmarshal(raw, &w) != nil {
		return ir.Usage{}, false
	}
	return ir.Usage{
		Input:     w.PromptTokenCount + w.ToolUsePromptTokenCount - w.CachedContentTokenCount,
		CacheRead: w.CachedContentTokenCount,
		Output:    w.CandidatesTokenCount + w.ThoughtsTokenCount,
		Reasoning: w.ThoughtsTokenCount,
		Raw:       compactJSON(raw),
	}, true
}

func encodeUsage(u ir.Usage) []byte {
	prompt := u.TotalInput()
	out := setJSON([]byte("{}"), "promptTokenCount", prompt)
	out = setJSON(out, "candidatesTokenCount", u.Output-u.Reasoning)
	out = setJSON(out, "totalTokenCount", prompt+u.Output)
	if u.CacheRead > 0 {
		out = setJSON(out, "cachedContentTokenCount", u.CacheRead)
	}
	if u.Reasoning > 0 {
		out = setJSON(out, "thoughtsTokenCount", u.Reasoning)
	}
	return out
}

// stopReason maps a finishReason (or promptFeedback.blockReason).
func stopReason(finish string, sawCall bool) ir.StopReason {
	switch finish {
	case "MAX_TOKENS":
		return ir.StopMaxTokens
	case "SAFETY", "RECITATION", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION", "LANGUAGE":
		return ir.StopContentFilter
	}
	if sawCall {
		return ir.StopToolUse
	}
	return ir.StopEndTurn
}

func finishReason(s ir.StopReason) string {
	switch s {
	case ir.StopMaxTokens, ir.StopContextWindow:
		return "MAX_TOKENS"
	case ir.StopContentFilter, ir.StopRefusal:
		return "SAFETY"
	}
	return "STOP"
}

func hasToolUse(blocks []ir.Block) bool {
	for _, b := range blocks {
		if b.Type == ir.BlockToolUse {
			return true
		}
	}
	return false
}

// DecodeResponse implements protocol.Codec.
func (Codec) DecodeResponse(body []byte) (*ir.Response, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, ir.Errorf(ir.ErrUpstream, "invalid response JSON: %v", err)
	}
	if _, ok := top["error"]; ok {
		return nil, Codec{}.DecodeError(0, nil, body)
	}
	g := gjson.ParseBytes(body)
	resp := &ir.Response{ID: g.Get("responseId").String(), Model: g.Get("modelVersion").String()}
	ids := newIDState(responseSalt(resp.ID))
	finish := ""
	if cands := g.Get("candidates").Array(); len(cands) > 0 {
		c := cands[0]
		var parts []json.RawMessage
		for _, p := range c.Get("content.parts").Array() {
			parts = append(parts, json.RawMessage(p.Raw))
		}
		blocks, err := decodeParts(parts, ids, false, nil)
		if err != nil {
			return nil, err
		}
		resp.Content = blocks
		finish = c.Get("finishReason").String()
		rest := []byte("{}")
		c.ForEach(func(k, v gjson.Result) bool {
			if !knownCandidateKeys[k.String()] {
				rest = setRaw(rest, k.String(), []byte(v.Raw))
			}
			return true
		})
		if string(rest) != "{}" {
			resp.Extra.Set(Name, extraCandidate, rest)
		}
	} else if br := g.Get("promptFeedback.blockReason"); br.Exists() {
		finish = "SAFETY"
	}
	if finish != "" {
		resp.Extra.Set(Name, extraFinishReason, json.RawMessage(`"`+finish+`"`))
	}
	resp.StopReason = stopReason(finish, hasToolUse(resp.Content))
	if u, ok := decodeUsage(top["usageMetadata"]); ok {
		resp.Usage = u
	}
	for k, v := range top {
		if !knownResponseKeys[k] {
			resp.Extra.Set(Name, k, v)
		}
	}
	return resp, nil
}

// EncodeResponse implements protocol.Codec.
func (Codec) EncodeResponse(req *ir.Request, resp *ir.Response) ([]byte, error) {
	var msgs []ir.Message
	if req != nil {
		msgs = req.Messages
	}
	parts := encodeBlocks(resp.Content, toolNames(msgs), toClient)
	if len(parts) == 0 {
		parts = []json.RawMessage{json.RawMessage(`{"text":""}`)}
	}
	content := setJSON([]byte("{}"), "role", "model")
	content = setRaw(content, "parts", rawArray(parts))
	cand := setRaw([]byte("{}"), "content", content)

	finish := finishReason(resp.StopReason)
	if v, ok := resp.Extra.Get(Name, extraFinishReason); ok {
		var f string
		if json.Unmarshal(v, &f) == nil && stopReason(f, hasToolUse(resp.Content)) == resp.StopReason {
			finish = f
		}
	}
	cand = setJSON(cand, "finishReason", finish)
	cand = setJSON(cand, "index", 0)
	if v, ok := resp.Extra.Get(Name, extraCandidate); ok {
		gjson.ParseBytes(v).ForEach(func(k, val gjson.Result) bool {
			cand = setRaw(cand, k.String(), []byte(val.Raw))
			return true
		})
	}
	out := setRaw([]byte("{}"), "candidates", rawArray([]json.RawMessage{cand}))
	out = setRaw(out, "usageMetadata", encodeUsage(resp.Usage))
	model := resp.Model
	if model == "" && req != nil {
		model = req.Model
	}
	if model != "" {
		out = setJSON(out, "modelVersion", model)
	}
	if resp.ID != "" {
		out = setJSON(out, "responseId", resp.ID)
	}
	if resp.Extra.Origin == Name {
		keys := make([]string, 0, len(resp.Extra.Fields))
		for k := range resp.Extra.Fields {
			if k != extraCandidate && k != extraFinishReason && !knownResponseKeys[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = setRaw(out, k, resp.Extra.Fields[k])
		}
	}
	return out, nil
}
