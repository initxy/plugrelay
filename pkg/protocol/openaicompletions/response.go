package openaicompletions

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/initxy/plugrelay/pkg/ir"
)

// DecodeResponse parses an upstream's complete text_completion.
func (Codec) DecodeResponse(body []byte) (*ir.Response, error) {
	top := rawMap(body)
	if top == nil {
		return nil, ir.Errorf(ir.ErrUpstream, "upstream response is not a JSON object")
	}
	if e, ok := top["error"]; ok && !isNull(e) {
		return nil, Codec{}.DecodeError(0, nil, body)
	}
	resp := &ir.Response{}
	if v, ok := take(top, "id"); ok {
		resp.ID = str(v)
	}
	if v, ok := take(top, "model"); ok {
		resp.Model = str(v)
	}
	if v, ok := take(top, "created"); ok {
		mark(&resp.Extra, xCreated, v)
	}
	delete(top, "object")
	if v, ok := take(top, "usage"); ok {
		resp.Usage = decodeUsage(v)
	}
	var choices []json.RawMessage
	if v, ok := take(top, "choices"); ok && json.Unmarshal(v, &choices) != nil {
		return nil, ir.Errorf(ir.ErrUpstream, "upstream response has invalid choices")
	}
	for k, v := range top {
		resp.Extra.Set(Name, k, v)
	}
	resp.StopReason = ir.StopEndTurn
	if len(choices) == 0 {
		return resp, nil
	}
	ch := rawMap(choices[0])
	delete(ch, "index")
	text, _ := take(ch, "text")
	finish, _ := take(ch, "finish_reason")
	if len(ch) > 0 {
		mark(&resp.Extra, xChoice, mustRaw(ch))
	}
	if s := str(text); s != "" {
		resp.Content = []ir.Block{{Type: ir.BlockText, Text: s}}
	}
	if sr := stopReason(str(finish)); sr != "" {
		resp.StopReason = sr
	} else if !isNull(finish) {
		mark(&resp.Extra, xFinish, finish)
	}
	return resp, nil
}

// stopReason maps a finish_reason; "" means unknown.
func stopReason(finish string) ir.StopReason {
	switch finish {
	case "stop":
		return ir.StopEndTurn
	case "length":
		return ir.StopMaxTokens
	case "content_filter":
		return ir.StopContentFilter
	}
	return ""
}

func finishReason(s ir.StopReason) string {
	switch s {
	case ir.StopMaxTokens, ir.StopContextWindow:
		return "length"
	case ir.StopContentFilter, ir.StopRefusal:
		return "content_filter"
	}
	return "stop"
}

// decodeUsage reads a usage object. prompt_tokens includes cached tokens;
// IR buckets do not overlap.
func decodeUsage(v json.RawMessage) ir.Usage {
	var w struct {
		Prompt     int64 `json:"prompt_tokens"`
		Completion int64 `json:"completion_tokens"`
		PromptD    struct {
			Cached int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionD struct {
			Reasoning int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	_ = json.Unmarshal(v, &w)
	u := ir.Usage{
		CacheRead: w.PromptD.Cached,
		Output:    w.Completion,
		Reasoning: w.CompletionD.Reasoning,
		Raw:       append(json.RawMessage(nil), v...),
	}
	u.Input = max(w.Prompt-u.CacheRead, 0)
	return u
}

func encodeUsage(u ir.Usage) object {
	in := u.TotalInput()
	o := object{{"prompt_tokens", in}, {"completion_tokens", u.Output}, {"total_tokens", in + u.Output}}
	if u.CacheRead > 0 {
		o.add("prompt_tokens_details", object{{"cached_tokens", u.CacheRead}})
	}
	return o
}

// responseText is the visible text of a response; reasoning has no field
// in this protocol.
func responseText(bs []ir.Block) string {
	var b strings.Builder
	for _, bl := range bs {
		if bl.Type == ir.BlockText {
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}

// EncodeResponse renders a complete text_completion for the client.
func (Codec) EncodeResponse(_ *ir.Request, resp *ir.Response) ([]byte, error) {
	id := resp.ID
	if id == "" {
		id = newID()
	}
	var created any = time.Now().Unix()
	if v, ok := resp.Extra.Get(Name, xCreated); ok {
		created = v
	}
	finish := any(finishReason(resp.StopReason))
	if v, ok := resp.Extra.Get(Name, xFinish); ok {
		finish = v
	}
	choice := object{{"text", responseText(resp.Content)}, {"index", 0}}
	if v, ok := resp.Extra.Get(Name, xChoice); ok {
		mergeRaw(&choice, v)
	}
	if !choice.has("logprobs") {
		choice.add("logprobs", nil)
	}
	choice.add("finish_reason", finish)
	o := object{
		{"id", id},
		{"object", "text_completion"},
		{"created", created},
		{"model", resp.Model},
		{"choices", []any{choice}},
		{"usage", encodeUsage(resp.Usage)},
	}
	restore(&o, resp.Extra)
	return marshal(o)
}

func newID() string { return "cmpl-" + uuid.NewString() }
