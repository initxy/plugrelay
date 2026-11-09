package anthropic

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/tidwall/gjson"
)

// errInvalidJSON is wrapped into an invalid_request GwError by callers.
var errInvalidJSON = errors.New("request body is not valid JSON")

// requestFields are the top-level request members modelled by ir.Request.
// Everything else (metadata, context_management, container, mcp_servers, ...)
// is kept verbatim in Request.Extra.
var requestFields = []string{
	"model", "system", "messages", "tools", "tool_choice", "max_tokens",
	"temperature", "top_p", "top_k", "stop_sequences", "stream", "thinking",
	"output_config", "service_tier",
}

// DecodeRequest parses a messages (or count_tokens) request into IR.
func (Codec) DecodeRequest(_ *http.Request, _ protocol.Inbound, body []byte) (*ir.Request, ir.Needs, error) {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return nil, ir.Needs{}, &ir.GwError{Kind: ir.ErrInvalidRequest, Message: errInvalidJSON.Error()}
	}
	req := decodeRequest(gjson.ParseBytes(body))
	return req, needsOf(req), nil
}

func decodeRequest(root gjson.Result) *ir.Request {
	req := &ir.Request{Model: root.Get("model").String()}
	ex := &req.Extra

	if sys := root.Get("system"); sys.Exists() {
		var shape ir.Extra
		req.System = decodeBlocks(sys, &shape)
		if v, ok := shape.Get(name, markShape); ok {
			ex.Set(name, "$system", v)
		}
	}
	for _, m := range root.Get("messages").Array() {
		msg := ir.Message{Role: ir.Role(m.Get("role").String())}
		msg.Content = decodeBlocks(m.Get("content"), &msg.Extra)
		m.ForEach(func(k, v gjson.Result) bool {
			if k.String() != "role" && k.String() != "content" {
				msg.Extra.Set(name, k.String(), raw(v))
			}
			return true
		})
		req.Messages = append(req.Messages, msg)
	}
	paired := serverToolNames(root)
	for _, t := range root.Get("tools").Array() {
		req.Tools = append(req.Tools, decodeTool(t, paired))
	}
	if tc := root.Get("tool_choice"); tc.IsObject() {
		req.ToolChoice = &ir.ToolChoice{Mode: tc.Get("type").String(), Name: tc.Get("name").String()}
		known := []string{"type", "name"}
		if d := tc.Get("disable_parallel_tool_use"); d.IsBool() {
			p := !d.Bool()
			req.ParallelTools = &p
			known = append(known, "disable_parallel_tool_use")
		}
		keepRest(ex, "tool_choice", tc, known...)
	} else if tc.Exists() {
		ex.Set(name, "tool_choice", raw(tc))
	}
	if v := root.Get("max_tokens"); v.Type == gjson.Number {
		n := int(v.Int())
		req.MaxOutputTokens = &n
	}
	if v := root.Get("temperature"); v.Type == gjson.Number {
		f := v.Float()
		req.Temperature = &f
	}
	if v := root.Get("top_p"); v.Type == gjson.Number {
		f := v.Float()
		req.TopP = &f
	}
	if v := root.Get("top_k"); v.Type == gjson.Number {
		n := int(v.Int())
		req.TopK = &n
	}
	if v := root.Get("stop_sequences"); v.IsArray() {
		req.Stop = []string{}
		for _, s := range v.Array() {
			req.Stop = append(req.Stop, s.String())
		}
	}
	req.Stream = root.Get("stream").Bool()
	req.ServiceTier = root.Get("service_tier").String()
	decodeReasoning(root, req)

	root.ForEach(func(k, v gjson.Result) bool {
		key := k.String()
		for _, f := range requestFields {
			if key == f {
				return true
			}
		}
		ex.Set(name, key, raw(v))
		return true
	})
	// Fields read above in a lossy way are kept verbatim when the IR value
	// would not reproduce them.
	for _, f := range []string{"stream", "max_tokens", "temperature", "top_p", "top_k", "service_tier"} {
		if v := root.Get(f); v.Exists() && !reproduces(f, v) {
			ex.Set(name, f, raw(v))
		}
	}
	return req
}

// reproduces reports whether the IR value of a scalar field encodes back to v.
func reproduces(field string, v gjson.Result) bool {
	switch field {
	case "stream":
		return v.Type == gjson.True
	case "service_tier":
		return v.Type == gjson.String && v.String() != ""
	default:
		return v.Type == gjson.Number
	}
}

func decodeReasoning(root gjson.Result, req *ir.Request) {
	th, oc := root.Get("thinking"), root.Get("output_config")
	if !th.Exists() && !oc.Get("effort").Exists() {
		if oc.Exists() {
			req.Extra.Set(name, "output_config", raw(oc))
		}
		return
	}
	r := &ir.Reasoning{}
	if th.IsObject() {
		known := []string{"type"}
		switch th.Get("type").String() {
		case "enabled":
			r.Enabled = true
			if b := th.Get("budget_tokens"); b.Type == gjson.Number {
				n := int(b.Int())
				r.Budget = &n
				known = append(known, "budget_tokens")
			}
		case "adaptive":
			r.Enabled, r.Adaptive = true, true
		case "disabled":
		default:
			known = nil
		}
		keepRest(&req.Extra, "thinking", th, known...)
	} else if th.Exists() {
		req.Extra.Set(name, "thinking", raw(th))
		req.Extra.Set(name, markNoThink, json.RawMessage(trueJSON))
	} else {
		req.Extra.Set(name, markNoThink, json.RawMessage(trueJSON))
	}
	if oc.IsObject() {
		r.Effort = oc.Get("effort").String()
		keepRest(&req.Extra, "output_config", oc, "effort")
	}
	req.Reasoning = r
}

// serverToolPrefixes name the typed tools the provider executes.
var serverToolPrefixes = []string{"web_search_", "web_fetch_", "code_execution_"}

// serverToolNames returns the tool names history pairs with server_tool_use,
// marking their typed definitions as provider-executed.
func serverToolNames(root gjson.Result) map[string]bool {
	names := map[string]bool{}
	for _, m := range root.Get("messages").Array() {
		for _, b := range m.Get("content").Array() {
			if b.Get("type").String() == wireServerToolUse {
				names[b.Get("name").String()] = true
			}
		}
	}
	return names
}

func isServerTool(typ, name string, paired map[string]bool) bool {
	for _, p := range serverToolPrefixes {
		if strings.HasPrefix(typ, p) {
			return true
		}
	}
	return paired[name]
}

// Input schemas for client-executed typed tools, which the wire defines by
// type instead of a schema. Other protocols receive them as function tools.
var (
	bashSchema       = json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"restart":{"type":"boolean"}}}`)
	textEditorSchema = json.RawMessage(`{"type":"object","properties":{` +
		`"command":{"type":"string","enum":["view","create","str_replace","insert"]},` +
		`"path":{"type":"string"},"file_text":{"type":"string"},"old_str":{"type":"string"},` +
		`"new_str":{"type":"string"},"insert_line":{"type":"integer"},` +
		`"view_range":{"type":"array","items":{"type":"integer"}}},"required":["command","path"]}`)
	objectSchema = json.RawMessage(`{"type":"object"}`)
)

// markSynthSchema records that a typed tool's InputSchema was synthesised and
// must not be written back to this protocol.
const markSynthSchema = "$synth_schema"

func typedToolSchema(typ string) json.RawMessage {
	switch {
	case strings.HasPrefix(typ, "bash_"):
		return bashSchema
	case strings.HasPrefix(typ, "text_editor_"):
		return textEditorSchema
	}
	return objectSchema
}

func decodeTool(t gjson.Result, paired map[string]bool) ir.Tool {
	tool := ir.Tool{Kind: ir.ToolFunction, Name: t.Get("name").String()}
	known := []string{"name", "cache_control"}
	typ := t.Get("type").String()
	switch {
	case typ != "" && typ != "custom" && isServerTool(typ, tool.Name, paired):
		tool.Kind, tool.ServerType = ir.ToolServer, typ
		known = append(known, "type")
	default:
		// Client-executed: plain function tools, and typed ones whose "type"
		// stays in Extra for an exact same-protocol round trip.
		tool.Description = t.Get("description").String()
		if s := t.Get("input_schema"); s.Exists() {
			tool.InputSchema = raw(s)
		} else if typ != "" && typ != "custom" {
			tool.InputSchema = typedToolSchema(typ)
			tool.Extra.Set(name, markSynthSchema, json.RawMessage(trueJSON))
		}
		known = append(known, "input_schema")
		if t.Get("description").Type == gjson.String {
			known = append(known, "description")
		}
	}
	if t.Get("defer_loading").Type == gjson.True {
		tool.DeferLoading = true
		known = append(known, "defer_loading")
	}
	tool.Cache = decodeCache(t, &tool.Extra)
	t.ForEach(func(k, v gjson.Result) bool {
		for _, n := range known {
			if k.String() == n {
				return true
			}
		}
		tool.Extra.Set(name, k.String(), raw(v))
		return true
	})
	return tool
}

// EncodeRequest renders IR as a messages request body.
func (Codec) EncodeRequest(req *ir.Request) ([]byte, error) {
	return encodeRequest(req), nil
}

func encodeRequest(req *ir.Request) []byte {
	ex := req.Extra
	o := newObject()
	o.setv("model", req.Model)
	if req.MaxOutputTokens != nil {
		o.setv("max_tokens", *req.MaxOutputTokens)
	}
	if len(req.System) > 0 {
		var shape ir.Extra
		if v, ok := ex.Get(name, "$system"); ok {
			shape.Set(name, markShape, v)
		}
		o.set("system", encodeBlocks(req.System, shape))
	} else if v, ok := ex.Get(name, "$system"); ok && string(v) == shapeEmptyAr {
		o.set("system", json.RawMessage("[]"))
	}
	msgs := make([]json.RawMessage, 0, len(req.Messages))
	for i := range req.Messages {
		m := &req.Messages[i]
		mo := newObject()
		mo.setv("role", string(m.Role))
		mo.set("content", encodeBlocks(m.Content, m.Extra))
		mo.extra(m.Extra)
		msgs = append(msgs, mo.bytes())
	}
	o.set("messages", array(msgs))
	if len(req.Tools) > 0 {
		tools := make([]json.RawMessage, 0, len(req.Tools))
		for i := range req.Tools {
			if t, ok := encodeTool(&req.Tools[i]); ok {
				tools = append(tools, t)
			}
		}
		o.set("tools", array(tools))
	}
	if req.ToolChoice != nil || req.ParallelTools != nil {
		tc := newObject()
		mode := "auto"
		if req.ToolChoice != nil && req.ToolChoice.Mode != "" {
			mode = req.ToolChoice.Mode
		}
		tc.setv("type", mode)
		if req.ToolChoice != nil && req.ToolChoice.Name != "" {
			tc.setv("name", req.ToolChoice.Name)
		}
		if req.ParallelTools != nil {
			tc.setv("disable_parallel_tool_use", !*req.ParallelTools)
		}
		tc.rest(ex, "$tool_choice")
		o.set("tool_choice", tc.bytes())
	}
	if req.Temperature != nil {
		o.setv("temperature", *req.Temperature)
	}
	if req.TopP != nil {
		o.setv("top_p", *req.TopP)
	}
	if req.TopK != nil {
		o.setv("top_k", *req.TopK)
	}
	if req.Stop != nil {
		o.setv("stop_sequences", req.Stop)
	}
	if req.Stream {
		o.setv("stream", true)
	}
	if req.ServiceTier != "" {
		o.setv("service_tier", req.ServiceTier)
	}
	encodeReasoning(o, req)
	o.extra(ex)
	return o.bytes()
}

func encodeReasoning(o *object, req *ir.Request) {
	r, ex := req.Reasoning, req.Extra
	if r == nil {
		return
	}
	_, noThink := ex.Get(name, markNoThink)
	if r.Enabled || r.Budget != nil || !noThink {
		th := newObject()
		switch {
		case r.Adaptive:
			th.setv("type", "adaptive")
		case r.Enabled || r.Budget != nil:
			th.setv("type", "enabled")
			if r.Budget != nil {
				th.setv("budget_tokens", *r.Budget)
			}
		default:
			th.setv("type", "disabled")
		}
		th.rest(ex, "$thinking")
		o.set("thinking", th.bytes())
	}
	if r.Effort != "" {
		oc := newObject()
		oc.setv("effort", r.Effort)
		oc.rest(ex, "$output_config")
		o.set("output_config", oc.bytes())
	}
}

func encodeTool(t *ir.Tool) (json.RawMessage, bool) {
	o := newObject()
	switch t.Kind {
	case ir.ToolServer:
		o.setv("type", t.ServerType)
		o.setv("name", t.Name)
	case ir.ToolFunction:
		o.setv("name", t.Name)
		if t.Description != "" {
			o.setv("description", t.Description)
		}
		// A typed tool decoded here has its schema implied by its type.
		_, synth := t.Extra.Get(name, markSynthSchema)
		switch {
		case synth:
		case len(t.InputSchema) > 0:
			o.set("input_schema", t.InputSchema)
		default:
			o.set("input_schema", objectSchema)
		}
	default:
		return nil, false
	}
	if t.DeferLoading {
		o.setv("defer_loading", true)
	}
	encodeCache(o, t.Cache, t.Extra)
	o.extra(t.Extra)
	return o.bytes(), true
}

// needsOf computes what a decoded request requires from an upstream.
func needsOf(req *ir.Request) ir.Needs {
	var n ir.Needs
	mark := func(c *ir.CacheMark) {
		if c != nil {
			n.ExplicitCache = true
			n.Cache1h = n.Cache1h || c.TTL == "1h"
		}
	}
	var walk func(bs []ir.Block, inResult bool)
	walk = func(bs []ir.Block, inResult bool) {
		for i := range bs {
			b := &bs[i]
			mark(b.Cache)
			switch b.Type {
			case ir.BlockImage:
				n.Images = true
				n.ToolResultImgs = n.ToolResultImgs || inResult
			case ir.BlockDocument:
				n.Documents = true
			case ir.BlockToolResult:
				walk(b.Result.Content, true)
			}
			if b.Opaque != nil {
				n.OpaqueState = true
			}
		}
	}
	walk(req.System, false)
	for _, m := range req.Messages {
		walk(m.Content, false)
	}
	seen := map[string]bool{}
	for _, t := range req.Tools {
		mark(t.Cache)
		n.DeferredTools = n.DeferredTools || t.DeferLoading
		switch t.Kind {
		case ir.ToolFunction:
			n.FunctionTools = true
		case ir.ToolServer:
			if !seen[t.ServerType] {
				seen[t.ServerType] = true
				n.ServerTools = append(n.ServerTools, t.ServerType)
			}
		}
	}
	n.ServerToolsRequired = req.RequiredServerTools()
	if _, ok := req.Extra.Get(name, "cache_control"); ok {
		n.ExplicitCache = true
	}
	if r := req.Reasoning; r != nil && (r.Enabled || r.Effort != "") {
		n.Reasoning = true
	}
	n.InputTokensEst = estimateTokens(req)
	return n
}

// estimateTokens is a rough input size: characters of text, tool inputs and
// tool definitions divided by four, plus a flat cost per image or document
// (base64 payloads are not counted as text).
func estimateTokens(req *ir.Request) int {
	const mediaTokens = 1500
	chars, media := 0, 0
	var walk func(bs []ir.Block)
	walk = func(bs []ir.Block) {
		for i := range bs {
			b := &bs[i]
			chars += utf8.RuneCountInString(b.Text)
			switch {
			case b.Media != nil:
				media++
			case b.Tool != nil:
				chars += len(b.Tool.Input) + len(b.Tool.Name)
			case b.Result != nil:
				walk(b.Result.Content)
			}
		}
	}
	walk(req.System)
	for _, m := range req.Messages {
		walk(m.Content)
	}
	for _, t := range req.Tools {
		chars += len(t.Name) + utf8.RuneCountInString(t.Description) + len(t.InputSchema)
	}
	return (chars+3)/4 + media*mediaTokens
}
