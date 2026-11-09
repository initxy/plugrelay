package anthropic

import (
	"encoding/json"

	"github.com/google/uuid"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/tidwall/gjson"
)

// usageFields are the usage members mapped to the five IR buckets.
var usageFields = []string{
	"input_tokens", "output_tokens", "cache_read_input_tokens",
	"cache_creation_input_tokens", "cache_creation",
}

// DecodeResponse parses a complete messages response.
func (Codec) DecodeResponse(body []byte) (*ir.Response, error) {
	if !gjson.ValidBytes(body) {
		return nil, &ir.GwError{Kind: ir.ErrUpstream, Message: "upstream response is not valid JSON", Upstream: body}
	}
	root := gjson.ParseBytes(body)
	resp := &ir.Response{
		ID:         root.Get("id").String(),
		Model:      root.Get("model").String(),
		StopReason: ir.StopReason(root.Get("stop_reason").String()),
		StopSeq:    root.Get("stop_sequence").String(),
		Usage:      decodeUsage(root.Get("usage")),
	}
	for _, b := range root.Get("content").Array() {
		resp.Content = append(resp.Content, decodeBlock(b))
	}
	known := []string{"id", "type", "role", "model", "content", "stop_reason", "stop_sequence", "usage"}
	root.ForEach(func(k, v gjson.Result) bool {
		for _, n := range known {
			if k.String() == n {
				return true
			}
		}
		resp.Extra.Set(name, k.String(), raw(v))
		return true
	})
	// Origin marks the response as ours even without extra fields, so
	// EncodeResponse restores the usage members it does not model.
	if resp.Extra.Origin == "" {
		resp.Extra.Origin = name
	}
	return resp, nil
}

// EncodeResponse renders a complete messages response.
func (Codec) EncodeResponse(req *ir.Request, resp *ir.Response) ([]byte, error) {
	o := newObject()
	o.setv("id", messageID(resp.ID))
	o.setv("type", "message")
	o.setv("role", string(ir.RoleAssistant))
	o.setv("model", modelOf(req, resp.Model))
	items := make([]json.RawMessage, 0, len(resp.Content))
	for i := range resp.Content {
		if b, ok := encodeBlock(&resp.Content[i]); ok {
			items = append(items, b)
		}
	}
	o.set("content", array(items))
	o.setv("stop_reason", stopReason(resp.StopReason))
	if resp.StopSeq != "" {
		o.setv("stop_sequence", resp.StopSeq)
	} else {
		o.set("stop_sequence", json.RawMessage(nullJSON))
	}
	o.set("usage", encodeUsage(resp.Usage, resp.Extra.Origin == name))
	o.extra(resp.Extra)
	return o.bytes(), nil
}

func messageID(id string) string {
	if id != "" {
		return id
	}
	return "msg_" + uuid.NewString()
}

func modelOf(req *ir.Request, model string) string {
	if model == "" && req != nil {
		return req.Model
	}
	return model
}

// stopReason maps a neutral stop reason to the wire value.
func stopReason(s ir.StopReason) string {
	switch s {
	case "":
		return string(ir.StopEndTurn)
	case ir.StopContentFilter:
		return string(ir.StopRefusal)
	}
	return string(s)
}

// decodeUsage maps a usage object to the five buckets. Cache writes come
// from the per-TTL breakdown when present, otherwise the whole
// cache_creation_input_tokens counts as a 5-minute write.
func decodeUsage(u gjson.Result) ir.Usage {
	if !u.IsObject() {
		return ir.Usage{}
	}
	out := ir.Usage{
		Input:     u.Get("input_tokens").Int(),
		CacheRead: u.Get("cache_read_input_tokens").Int(),
		Output:    u.Get("output_tokens").Int(),
		Raw:       json.RawMessage(u.Raw),
	}
	if cc := u.Get("cache_creation"); cc.IsObject() {
		out.CacheWrite5m = cc.Get("ephemeral_5m_input_tokens").Int()
		out.CacheWrite1h = cc.Get("ephemeral_1h_input_tokens").Int()
	} else {
		out.CacheWrite5m = u.Get("cache_creation_input_tokens").Int()
	}
	return out
}

// encodeUsage renders the buckets as a usage object. With sameOrigin the
// members of Raw this codec does not model (server_tool_use, service_tier,
// ...) are carried over.
func encodeUsage(u ir.Usage, sameOrigin bool) json.RawMessage {
	o := newObject()
	o.setv("input_tokens", u.Input)
	o.setv("cache_creation_input_tokens", u.CacheWrite5m+u.CacheWrite1h)
	o.setv("cache_read_input_tokens", u.CacheRead)
	cc := newObject()
	cc.setv("ephemeral_5m_input_tokens", u.CacheWrite5m)
	cc.setv("ephemeral_1h_input_tokens", u.CacheWrite1h)
	o.set("cache_creation", cc.bytes())
	o.setv("output_tokens", u.Output)
	if sameOrigin && len(u.Raw) > 0 {
		gjson.ParseBytes(u.Raw).ForEach(func(k, v gjson.Result) bool {
			for _, f := range usageFields {
				if k.String() == f {
					return true
				}
			}
			o.set(k.String(), raw(v))
			return true
		})
	}
	return o.bytes()
}
