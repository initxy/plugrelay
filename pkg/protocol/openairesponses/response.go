package openairesponses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/tidwall/gjson"
)

// Response Extra keys (origin Name).
const xCreatedAt = "created_at"

// DecodeResponse parses an upstream's complete response.
func (Codec) DecodeResponse(body []byte) (*ir.Response, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("%s: response is not valid JSON", Name)
	}
	root := gjson.ParseBytes(body)
	if e := root.Get("error"); e.IsObject() && root.Get("status").Str != "completed" {
		return nil, errorFromObject(e)
	}
	resp := &ir.Response{ID: root.Get("id").Str, Model: root.Get("model").Str}
	var n ir.Needs
	for _, it := range root.Get("output").Array() {
		side, ok := sideOf(it)
		if !ok {
			side = ir.RoleAssistant
		}
		bs := itemBlocks(it, side, &n)
		for i := range bs {
			dropStatus(&bs[i])
		}
		resp.Content = append(resp.Content, bs...)
	}
	if u := root.Get("usage"); u.IsObject() {
		resp.Usage = decodeUsage(u)
	}
	resp.StopReason = stopReason(root.Get("status").Str, root.Get("incomplete_details.reason").Str, resp.Content)
	if c := root.Get("created_at"); c.Exists() {
		xset(&resp.Extra, xCreatedAt, json.RawMessage(c.Raw))
	}
	return resp, nil
}

// dropStatus removes the item status the encoder always sets itself.
func dropStatus(b *ir.Block) {
	if rest, ok := xget(&b.Extra, xItem); ok {
		r := restObject(string(rest), "status")
		if r == nil {
			r = json.RawMessage("{}")
		}
		xset(&b.Extra, xItem, r)
	}
}

func stopReason(status, reason string, content []ir.Block) ir.StopReason {
	if status == "incomplete" {
		switch reason {
		case "content_filter":
			return ir.StopContentFilter
		default:
			return ir.StopMaxTokens
		}
	}
	for _, b := range content {
		if b.Type == ir.BlockToolUse {
			return ir.StopToolUse
		}
	}
	return ir.StopEndTurn
}

func decodeUsage(u gjson.Result) ir.Usage {
	in := u.Get("input_tokens").Int()
	cached := u.Get("input_tokens_details.cached_tokens").Int()
	written := u.Get("input_tokens_details.cache_write_tokens").Int()
	us := ir.Usage{
		Input:        max(0, in-cached-written),
		CacheRead:    cached,
		CacheWrite5m: written,
		Output:       u.Get("output_tokens").Int(),
		Reasoning:    u.Get("output_tokens_details.reasoning_tokens").Int(),
		Raw:          json.RawMessage(u.Raw),
	}
	return us
}

func encodeUsage(u ir.Usage) json.RawMessage {
	o := newObject()
	in := u.TotalInput()
	o.num("input_tokens", in)
	d := newObject()
	d.num("cached_tokens", u.CacheRead)
	if w := u.CacheWrite5m + u.CacheWrite1h; w > 0 {
		d.num("cache_write_tokens", w)
	}
	o.set("input_tokens_details", d.raw())
	o.num("output_tokens", u.Output)
	od := newObject()
	od.num("reasoning_tokens", u.Reasoning)
	o.set("output_tokens_details", od.raw())
	o.num("total_tokens", in+u.Output)
	return o.raw()
}

// statusOf maps a stop reason to response status and incomplete reason.
func statusOf(r ir.StopReason) (status, incomplete string) {
	switch r {
	case ir.StopMaxTokens, ir.StopContextWindow:
		return "incomplete", "max_output_tokens"
	case ir.StopContentFilter, ir.StopRefusal:
		return "incomplete", "content_filter"
	}
	return "completed", ""
}

// idGen derives stable item ids from the response id and output position.
func idGen(respID string) func(prefix string, n int) string {
	return func(prefix string, n int) string {
		sum := sha256.Sum256([]byte(respID + ":" + strconv.Itoa(n)))
		return prefix + hex.EncodeToString(sum[:20])
	}
}

func newResponseID() string {
	return "resp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// responseShell is the response object without output and usage.
type responseShell struct {
	id, model  string
	created    int64
	status     string
	incomplete string
	err        json.RawMessage
	req        *ir.Request
}

func (s responseShell) object(output []json.RawMessage, usage *ir.Usage) *object {
	o := newObject()
	o.str("id", s.id)
	o.str("object", "response")
	o.num("created_at", s.created)
	o.str("status", s.status)
	o.boolean("background", false)
	if s.err != nil {
		o.set("error", s.err)
	} else {
		o.set("error", json.RawMessage("null"))
	}
	if s.incomplete != "" {
		d := newObject()
		d.str("reason", s.incomplete)
		o.set("incomplete_details", d.raw())
	} else {
		o.set("incomplete_details", json.RawMessage("null"))
	}
	req := s.req
	if req == nil {
		req = &ir.Request{}
	}
	if req.MaxOutputTokens != nil {
		o.num("max_output_tokens", int64(*req.MaxOutputTokens))
	} else {
		o.set("max_output_tokens", json.RawMessage("null"))
	}
	o.str("model", s.model)
	if output == nil {
		output = []json.RawMessage{}
	}
	o.set("output", jarray(output))
	o.boolean("parallel_tool_calls", req.ParallelTools == nil || *req.ParallelTools)
	if req.Previous != "" {
		o.str("previous_response_id", req.Previous)
	} else {
		o.set("previous_response_id", json.RawMessage("null"))
	}
	if req.CacheKey != "" {
		o.str("prompt_cache_key", req.CacheKey)
	}
	if rs := req.Reasoning; rs != nil && (rs.Effort != "" || rs.Summary != "") {
		r := newObject()
		if rs.Effort != "" {
			r.str("effort", rs.Effort)
		}
		if rs.Summary != "" {
			r.str("summary", rs.Summary)
		}
		o.set("reasoning", r.raw())
	}
	if req.ServiceTier != "" {
		o.str("service_tier", req.ServiceTier)
	}
	o.boolean("store", req.Store != nil && *req.Store)
	if usage != nil {
		o.set("usage", encodeUsage(*usage))
	} else {
		o.set("usage", json.RawMessage("null"))
	}
	o.set("metadata", json.RawMessage("{}"))
	return o
}

func clientModel(req *ir.Request, upstream string) string {
	if req != nil && req.Model != "" {
		return req.Model
	}
	return upstream
}

// EncodeResponse renders a complete response for the client.
func (Codec) EncodeResponse(req *ir.Request, resp *ir.Response) ([]byte, error) {
	if resp == nil {
		return nil, fmt.Errorf("%s: nil response", Name)
	}
	id := resp.ID
	if id == "" {
		id = newResponseID()
	}
	gen := idGen(id)
	w := &itemWriter{output: true}
	w.newID = func(prefix string) string { return gen(prefix, len(w.items)) }
	for _, b := range resp.Content {
		w.add(b, "assistant")
	}
	w.flush()
	status, incomplete := statusOf(resp.StopReason)
	created := time.Now().Unix()
	if c, ok := xget(&resp.Extra, xCreatedAt); ok {
		created = gjson.ParseBytes(c).Int()
	}
	shell := responseShell{id: id, model: clientModel(req, resp.Model), created: created, status: status, incomplete: incomplete, req: req}
	usage := resp.Usage
	return shell.object(w.items, &usage).bytes(), nil
}
