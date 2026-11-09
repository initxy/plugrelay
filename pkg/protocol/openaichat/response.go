package openaichat

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/initxy/plugrelay/pkg/ir"
)

// DecodeResponse parses an upstream's complete chat completion.
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
	if len(choices) == 0 {
		resp.StopReason = ir.StopEndTurn
		return resp, nil
	}
	ch := rawMap(choices[0])
	delete(ch, "index")
	finish, _ := take(ch, "finish_reason")
	msg := rawMap(ch["message"])
	delete(ch, "message")
	if len(ch) > 0 {
		mark(&resp.Extra, xChoice, mustRaw(ch))
	}
	if msg != nil {
		delete(msg, "role")
		d := decoder{custom: map[string]bool{}}
		m, err := d.assistant(msg)
		if err != nil {
			return nil, ir.Errorf(ir.ErrUpstream, "upstream response: %s", err.Error())
		}
		resp.Content = m.Content
		wire := map[string]json.RawMessage{}
		for k, v := range m.Extra.Fields {
			if !strings.HasPrefix(k, "_") {
				wire[k] = v
			}
		}
		if len(wire) > 0 {
			mark(&resp.Extra, xMessage, mustRaw(wire))
		}
	}
	resp.StopReason = stopReason(str(finish), hasToolUse(resp.Content))
	if resp.StopReason == "" {
		resp.StopReason = ir.StopEndTurn
		if !isNull(finish) {
			mark(&resp.Extra, xFinish, finish)
		}
	}
	if resp.StopReason == ir.StopEndTurn && hasRefusal(resp.Content) {
		resp.StopReason = ir.StopRefusal
	}
	return resp, nil
}

func hasToolUse(bs []ir.Block) bool {
	for _, b := range bs {
		if b.Type == ir.BlockToolUse {
			return true
		}
	}
	return false
}

func hasRefusal(bs []ir.Block) bool {
	for _, b := range bs {
		if b.Type == ir.BlockText && has(&b.Extra, xRefusal) {
			return true
		}
	}
	return false
}

// stopReason maps a finish_reason; "" means unknown. Some compatible
// upstreams report "stop" on a turn that called tools.
func stopReason(finish string, toolUse bool) ir.StopReason {
	switch finish {
	case "stop", "":
		if toolUse {
			return ir.StopToolUse
		}
		if finish == "" {
			return ""
		}
		return ir.StopEndTurn
	case "length":
		return ir.StopMaxTokens
	case "tool_calls", "function_call":
		return ir.StopToolUse
	case "content_filter":
		return ir.StopContentFilter
	}
	return ""
}

func finishReason(s ir.StopReason) string {
	switch s {
	case ir.StopMaxTokens, ir.StopContextWindow:
		return "length"
	case ir.StopToolUse:
		return "tool_calls"
	case ir.StopContentFilter:
		return "content_filter"
	}
	return "stop"
}

// decodeUsage reads a usage object. prompt_tokens includes cached and
// cache-written tokens; IR buckets do not overlap.
func decodeUsage(v json.RawMessage) ir.Usage {
	var w struct {
		Prompt     int64 `json:"prompt_tokens"`
		Completion int64 `json:"completion_tokens"`
		PromptD    struct {
			Cached     int64 `json:"cached_tokens"`
			CacheWrite int64 `json:"cache_write_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionD struct {
			Reasoning int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	_ = json.Unmarshal(v, &w)
	u := ir.Usage{
		CacheRead:    w.PromptD.Cached,
		CacheWrite5m: w.PromptD.CacheWrite,
		Output:       w.Completion,
		Reasoning:    w.CompletionD.Reasoning,
		Raw:          append(json.RawMessage(nil), v...),
	}
	u.Input = max(w.Prompt-u.CacheRead-u.CacheWrite5m, 0)
	return u
}

func encodeUsage(u ir.Usage) object {
	in := u.TotalInput()
	pd := object{{"cached_tokens", u.CacheRead}}
	if w := u.CacheWrite5m + u.CacheWrite1h; w > 0 {
		pd.add("cache_write_tokens", w)
	}
	return object{
		{"prompt_tokens", in},
		{"completion_tokens", u.Output},
		{"total_tokens", in + u.Output},
		{"prompt_tokens_details", pd},
		{"completion_tokens_details", object{{"reasoning_tokens", u.Reasoning}}},
	}
}

// EncodeResponse renders a complete chat completion for the client.
func (Codec) EncodeResponse(req *ir.Request, resp *ir.Response) ([]byte, error) {
	id := resp.ID
	if id == "" {
		id = newID()
	}
	var created any = time.Now().Unix()
	if v, ok := resp.Extra.Get(Name, xCreated); ok {
		created = v
	}
	msg := encodeAssistant(resp.Content, ir.Extra{}, requestHasCustom(req), nil)
	if v, ok := resp.Extra.Get(Name, xMessage); ok {
		mergeRaw(&msg, v)
	}
	finish := any(finishReason(resp.StopReason))
	if v, ok := resp.Extra.Get(Name, xFinish); ok {
		finish = v
	}
	choice := object{{"index", 0}, {"message", msg}, {"finish_reason", finish}}
	restoreRaw(&choice, resp.Extra, xChoice)
	if !choice.has("logprobs") {
		choice.add("logprobs", nil)
	}
	o := object{
		{"id", id},
		{"object", "chat.completion"},
		{"created", created},
		{"model", resp.Model},
		{"choices", []any{choice}},
		{"usage", encodeUsage(resp.Usage)},
	}
	restore(&o, resp.Extra)
	return marshal(o)
}

func requestHasCustom(req *ir.Request) bool {
	if req == nil {
		return true
	}
	for _, t := range req.Tools {
		if t.Kind == ir.ToolFreeform {
			return true
		}
	}
	return false
}

func newID() string { return "chatcmpl-" + uuid.NewString() }
