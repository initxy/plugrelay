package openairesponses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/tidwall/gjson"
)

// Request Extra keys (origin Name), besides unknown top-level fields kept
// under their own names.
const (
	xInputString   = "$input_string"  // input was a plain string
	xText          = "$text"          // text object without verbosity
	xInclude       = "$include"       // include array, unless it held only encrypted reasoning
	xReasoning     = "$reasoning"     // reasoning object without effort and summary
	xEffortNumber  = "$effort_number" // reasoning.effort was a number
	xToolChoice    = "$tool_choice"   // tool_choice the IR cannot model
	xInstructions  = "$instructions"  // instructions that are not a non-empty string
	xToolRest      = "rest"           // Tool Extra: unmodeled tool fields
	xToolRaw       = "raw"            // Tool Extra: verbatim non-function tool
	xNamespace     = "namespace"      // Tool Extra on a namespace's first tool: namespace fields besides name and tools
	xFromItem      = "from_item"      // Tool Extra: declared by an input item, not the tools list
	xNoReasoningOb = "$no_reasoning"  // include asked for encrypted reasoning without a reasoning object
	xStreamFalse   = "$stream_false"  // stream was sent as false
)

var knownRequestKeys = []string{
	"model", "input", "instructions", "tools", "tool_choice", "parallel_tool_calls",
	"max_output_tokens", "temperature", "top_p", "stream", "reasoning", "include",
	"text", "store", "previous_response_id", "prompt_cache_key", "service_tier",
}

// DecodeRequest parses a client request into IR and computes its needs.
func (Codec) DecodeRequest(r *http.Request, in protocol.Inbound, body []byte) (*ir.Request, ir.Needs, error) {
	var n ir.Needs
	if !gjson.ValidBytes(body) {
		return nil, n, ir.Errorf(ir.ErrInvalidRequest, "request body is not valid JSON")
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, n, ir.Errorf(ir.ErrInvalidRequest, "request body must be a JSON object")
	}
	req := &ir.Request{Model: root.Get("model").Str, Stream: root.Get("stream").Bool()}
	if v := root.Get("stream"); v.Exists() && !v.Bool() {
		xset(&req.Extra, xStreamFalse, jtrue)
	}
	if in.Op == protocol.OpCountTokens {
		req.Stream = false
	}

	if ins := root.Get("instructions"); ins.Type == gjson.String && ins.Str != "" {
		b := ir.Block{Type: ir.BlockText, Text: ins.Str}
		xset(&b.Extra, xPlacement, jstr(placementInstruction))
		req.System = append(req.System, b)
	} else if ins.Exists() {
		xset(&req.Extra, xInstructions, json.RawMessage(ins.Raw))
	}

	switch input := root.Get("input"); {
	case input.Type == gjson.String:
		req.Messages = []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: input.Str}}}}
		xset(&req.Extra, xInputString, jtrue)
	case input.IsArray():
		sys, msgs := decodeInput(input.Array(), &n)
		req.System = append(req.System, sys...)
		req.Messages = msgs
	case input.Exists() && input.Type != gjson.Null:
		return nil, n, ir.Errorf(ir.ErrInvalidRequest, "input must be a string or an array")
	}

	for _, t := range root.Get("tools").Array() {
		if t.Get("type").Str == "namespace" && len(t.Get("tools").Array()) > 0 {
			req.Tools = append(req.Tools, decodeNamespace(t, &n)...)
			continue
		}
		req.Tools = append(req.Tools, decodeTool(t, &n))
	}
	req.Tools = append(req.Tools, itemTools(root.Get("input"), req.Tools, &n)...)
	decodeToolChoice(root.Get("tool_choice"), req)
	n.ServerToolsRequired = req.RequiredServerTools()
	if tc, ok := xget(&req.Extra, xToolChoice); ok {
		if typ := gjson.GetBytes(tc, "type").Str; slices.Contains(n.ServerTools, typ) && !slices.Contains(n.ServerToolsRequired, typ) {
			n.ServerToolsRequired = append(n.ServerToolsRequired, typ)
		}
	}
	if v := root.Get("parallel_tool_calls"); v.IsBool() {
		b := v.Bool()
		req.ParallelTools = &b
	}
	if v := root.Get("max_output_tokens"); v.Type == gjson.Number {
		i := int(v.Int())
		req.MaxOutputTokens = &i
	}
	if v := root.Get("temperature"); v.Type == gjson.Number {
		f := v.Float()
		req.Temperature = &f
	}
	if v := root.Get("top_p"); v.Type == gjson.Number {
		f := v.Float()
		req.TopP = &f
	}
	if v := root.Get("store"); v.IsBool() {
		b := v.Bool()
		req.Store = &b
	}
	req.Previous = root.Get("previous_response_id").Str
	req.CacheKey = root.Get("prompt_cache_key").Str
	req.ServiceTier = root.Get("service_tier").Str
	n.ServerState = req.Previous != ""

	decodeReasoning(root, req)
	if req.Reasoning != nil && req.Reasoning.Enabled {
		n.Reasoning = true
	}
	if t := root.Get("text"); t.IsObject() {
		req.Verbosity = t.Get("verbosity").Str
		if rest := restObject(t.Raw, "verbosity"); rest != nil {
			xset(&req.Extra, xText, rest)
		}
	}

	root.ForEach(func(k, v gjson.Result) bool {
		if !slices.Contains(knownRequestKeys, k.Str) {
			req.Extra.Set(Name, k.Str, json.RawMessage(v.Raw))
		}
		return true
	})
	if _, ok := req.Extra.Get(Name, "context_management"); ok {
		n.Compaction = true
	}

	for _, b := range req.System {
		markNeeds(b, &n)
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			markNeeds(b, &n)
		}
	}
	n.InputTokensEst = estimateTokens(req)
	return req, n, nil
}

// markNeeds records needs a block implies beyond what item decoding saw.
func markNeeds(b ir.Block, n *ir.Needs) {
	if raw, ok := xget(&b.Extra, xRaw); ok {
		if strings.Contains(gjson.GetBytes(raw, "type").Str, "compaction") {
			n.Compaction = true
		}
	}
}

func decodeTool(t gjson.Result, n *ir.Needs) ir.Tool {
	typ := t.Get("type").Str
	tool := ir.Tool{Name: t.Get("name").Str, Description: t.Get("description").Str, DeferLoading: t.Get("defer_loading").Bool()}
	if tool.DeferLoading {
		n.DeferredTools = true
	}
	switch typ {
	case "function":
		n.FunctionTools = true
		tool.Kind = ir.ToolFunction
		if p := t.Get("parameters"); p.Exists() {
			tool.InputSchema = json.RawMessage(p.Raw)
		}
		xset(&tool.Extra, xToolRest, restObject(t.Raw, "type", "name", "description", "parameters", "defer_loading"))
	case "custom":
		n.FreeformTools = true
		tool.Kind = ir.ToolFreeform
		if f := t.Get("format"); f.Exists() {
			tool.Grammar = json.RawMessage(f.Raw)
		}
		xset(&tool.Extra, xToolRest, restObject(t.Raw, "type", "name", "description", "format", "defer_loading"))
	default:
		tool.Kind = ir.ToolServer
		tool.ServerType = typ
		if tool.Name == "" {
			tool.Name = typ
		}
		if !slices.Contains(n.ServerTools, typ) {
			n.ServerTools = append(n.ServerTools, typ)
		}
		if typ == "tool_search" {
			n.DeferredTools = true
		}
		xset(&tool.Extra, xToolRaw, json.RawMessage(t.Raw))
	}
	return tool
}

// itemTools decodes the tools that input items add mid-conversation
// (additional_tools, tool_search_output), skipping ones already declared.
// The items themselves stay raw blocks; the copies let a target of another
// protocol see the tools and are never written back to the tools list.
func itemTools(input gjson.Result, have []ir.Tool, n *ir.Needs) []ir.Tool {
	var out []ir.Tool
	seen := map[[2]string]bool{}
	for _, t := range have {
		seen[[2]string{t.Namespace, t.Name}] = true
	}
	for _, it := range input.Array() {
		switch it.Get("type").Str {
		case "additional_tools", "tool_search_output":
		default:
			continue
		}
		for _, t := range it.Get("tools").Array() {
			var ts []ir.Tool
			if t.Get("type").Str == "namespace" {
				ts = decodeNamespace(t, n)
			} else {
				ts = []ir.Tool{decodeTool(t, n)}
			}
			for _, tool := range ts {
				if tool.Kind == ir.ToolServer || seen[[2]string{tool.Namespace, tool.Name}] {
					continue
				}
				seen[[2]string{tool.Namespace, tool.Name}] = true
				xset(&tool.Extra, xFromItem, jtrue)
				out = append(out, tool)
			}
		}
	}
	return out
}

// decodeNamespace flattens a namespace tool into its member tools, each
// tagged with the namespace; the first carries the namespace's other fields
// so encoding can regroup them.
func decodeNamespace(t gjson.Result, n *ir.Needs) []ir.Tool {
	ns := t.Get("name").Str
	var out []ir.Tool
	for i, inner := range t.Get("tools").Array() {
		tool := decodeTool(inner, n)
		tool.Namespace = ns
		if i == 0 {
			rest := restObject(t.Raw, "type", "name", "tools")
			if rest == nil {
				rest = json.RawMessage("{}")
			}
			xset(&tool.Extra, xNamespace, rest)
		}
		out = append(out, tool)
	}
	return out
}

func decodeToolChoice(v gjson.Result, req *ir.Request) {
	switch {
	case v.Type == gjson.String:
		switch v.Str {
		case "auto", "none":
			req.ToolChoice = &ir.ToolChoice{Mode: v.Str}
		case "required":
			req.ToolChoice = &ir.ToolChoice{Mode: "any"}
		default:
			xset(&req.Extra, xToolChoice, json.RawMessage(v.Raw))
		}
	case v.IsObject():
		t := v.Get("type").Str
		if (t == "function" || t == "custom") && v.Get("name").Str != "" {
			req.ToolChoice = &ir.ToolChoice{Mode: "tool", Name: v.Get("name").Str}
			return
		}
		xset(&req.Extra, xToolChoice, json.RawMessage(v.Raw))
	case v.Exists():
		xset(&req.Extra, xToolChoice, json.RawMessage(v.Raw))
	}
}

func decodeReasoning(root gjson.Result, req *ir.Request) {
	encrypted := false
	inc := root.Get("include")
	for _, v := range inc.Array() {
		if v.Str == includeEncryptedRsn {
			encrypted = true
		}
	}
	if inc.Exists() && !(encrypted && len(inc.Array()) == 1) {
		xset(&req.Extra, xInclude, json.RawMessage(inc.Raw))
	}
	r := root.Get("reasoning")
	if !r.IsObject() {
		if r.Exists() {
			req.Extra.Set(Name, "reasoning", json.RawMessage(r.Raw))
		}
		if encrypted {
			req.Reasoning = &ir.Reasoning{IncludeEncrypted: true}
			xset(&req.Extra, xNoReasoningOb, jtrue)
		}
		return
	}
	rs := &ir.Reasoning{Summary: r.Get("summary").Str, IncludeEncrypted: encrypted}
	switch e := r.Get("effort"); e.Type {
	case gjson.String:
		rs.Effort = e.Str
	case gjson.Number:
		rs.Effort = e.Raw
		xset(&req.Extra, xEffortNumber, jtrue)
	}
	rs.Enabled = rs.Effort != "none"
	xset(&req.Extra, xReasoning, restObject(r.Raw, "effort", "summary"))
	req.Reasoning = rs
}

// EncodeRequest renders IR as a request body for an upstream.
func (Codec) EncodeRequest(req *ir.Request) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("%s: nil request", Name)
	}
	o := newObject()
	o.str("model", req.Model)

	var instr []string
	sys := &itemWriter{}
	for _, b := range req.System {
		if xstring(&b.Extra, xPlacement) == placementInput {
			sys.add(b, "developer")
			continue
		}
		if b.Type == ir.BlockText {
			instr = append(instr, b.Text)
		}
	}
	sys.flush()
	if len(instr) > 0 {
		o.str("instructions", strings.Join(instr, "\n\n"))
	} else if v, ok := xget(&req.Extra, xInstructions); ok {
		o.set("instructions", v)
	}

	w := &itemWriter{items: sys.items}
	encodeMessages(req.Messages, w)
	if s, ok := plainInput(req, w.items); ok {
		o.str("input", s)
	} else {
		o.set("input", jarray(w.items))
	}

	if tools := encodeTools(req.Tools); tools != nil {
		o.set("tools", tools)
	}
	if tc := encodeToolChoice(req); tc != nil {
		o.set("tool_choice", tc)
	}
	if req.ParallelTools != nil {
		o.boolean("parallel_tool_calls", *req.ParallelTools)
	}
	if rs := encodeReasoning(req); rs != nil {
		o.set("reasoning", rs)
	}
	if req.Store != nil {
		o.boolean("store", *req.Store)
	}
	if req.Stream {
		o.boolean("stream", true)
	} else if xflag(&req.Extra, xStreamFalse) {
		o.boolean("stream", false)
	}
	if inc := encodeInclude(req); inc != nil {
		o.set("include", inc)
	}
	if req.CacheKey != "" {
		o.str("prompt_cache_key", req.CacheKey)
	}
	if req.ServiceTier != "" {
		o.str("service_tier", req.ServiceTier)
	}
	if text := encodeText(req); text != nil {
		o.set("text", text)
	}
	if req.MaxOutputTokens != nil {
		o.num("max_output_tokens", int64(*req.MaxOutputTokens))
	}
	if req.Temperature != nil {
		o.any("temperature", *req.Temperature)
	}
	if req.TopP != nil {
		o.any("top_p", *req.TopP)
	}
	if req.Previous != "" {
		o.str("previous_response_id", req.Previous)
	}
	if req.Extra.Origin == Name {
		keys := make([]string, 0, len(req.Extra.Fields))
		for k := range req.Extra.Fields {
			if !strings.HasPrefix(k, "$") {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			if !o.has(k) {
				o.set(k, req.Extra.Fields[k])
			}
		}
	}
	return o.bytes(), nil
}

// plainInput reports whether the input should be rendered as a string, as
// the client originally sent it.
func plainInput(req *ir.Request, items []json.RawMessage) (string, bool) {
	if !xflag(&req.Extra, xInputString) || len(items) != 1 {
		return "", false
	}
	it := gjson.ParseBytes(items[0])
	parts := it.Get("content").Array()
	if it.Get("role").Str != "user" || len(parts) != 1 || !isPlainText(json.RawMessage(parts[0].Raw)) {
		return "", false
	}
	return parts[0].Get("text").Str, true
}

// encodeTools renders the tool list, regrouping namespaced tools into one
// namespace tool at the position of the namespace's first member. It is nil
// when there is nothing to list.
func encodeTools(ts []ir.Tool) json.RawMessage {
	out := make([]json.RawMessage, 0, len(ts))
	done := map[string]bool{}
	for i, t := range ts {
		if xflag(&t.Extra, xFromItem) {
			continue
		}
		if t.Namespace == "" {
			out = append(out, encodeTool(t))
			continue
		}
		if done[t.Namespace] {
			continue
		}
		done[t.Namespace] = true
		o := newObject()
		o.str("type", "namespace")
		o.str("name", t.Namespace)
		var members []json.RawMessage
		for _, m := range ts[i:] {
			if m.Namespace != t.Namespace || xflag(&m.Extra, xFromItem) {
				continue
			}
			if rest, ok := xget(&m.Extra, xNamespace); ok {
				o.merge(rest)
			}
			members = append(members, encodeTool(m))
		}
		o.set("tools", jarray(members))
		out = append(out, o.raw())
	}
	if len(out) == 0 {
		return nil
	}
	return jarray(out)
}

func encodeTool(t ir.Tool) json.RawMessage {
	if raw, ok := xget(&t.Extra, xToolRaw); ok {
		return raw
	}
	o := newObject()
	rest, _ := xget(&t.Extra, xToolRest)
	switch t.Kind {
	case ir.ToolFreeform:
		o.str("type", "custom")
		o.str("name", t.Name)
		if t.Description != "" {
			o.str("description", t.Description)
		}
		if len(t.Grammar) > 0 {
			o.set("format", t.Grammar)
		}
	case ir.ToolServer:
		o.str("type", t.ServerType)
		if t.Name != "" && t.Name != t.ServerType {
			o.str("name", t.Name)
		}
	default:
		o.str("type", "function")
		o.str("name", t.Name)
		if t.Description != "" {
			o.str("description", t.Description)
		}
		if len(t.InputSchema) > 0 {
			o.set("parameters", t.InputSchema)
		} else {
			o.set("parameters", json.RawMessage(`{"type":"object","properties":{}}`))
		}
	}
	if t.DeferLoading {
		o.boolean("defer_loading", true)
	}
	o.merge(rest)
	return o.raw()
}

func encodeToolChoice(req *ir.Request) json.RawMessage {
	tc := req.ToolChoice
	if tc == nil {
		v, _ := xget(&req.Extra, xToolChoice)
		return v
	}
	switch tc.Mode {
	case "auto", "none":
		return jstr(tc.Mode)
	case "any":
		return jstr("required")
	case "tool":
		typ := "function"
		for _, t := range req.Tools {
			if t.Name == tc.Name && t.Kind == ir.ToolFreeform {
				typ = "custom"
			}
		}
		o := newObject()
		o.str("type", typ)
		o.str("name", tc.Name)
		return o.raw()
	}
	return nil
}

func encodeReasoning(req *ir.Request) json.RawMessage {
	rs := req.Reasoning
	rest, hasRest := xget(&req.Extra, xReasoning)
	if rs == nil {
		v, _ := xget(&req.Extra, "reasoning")
		return v
	}
	if !rs.Enabled && rs.Effort == "" && rs.Summary == "" && !hasRest {
		return nil
	}
	if xflag(&req.Extra, xNoReasoningOb) && rs.Effort == "" && rs.Summary == "" && !hasRest {
		return nil
	}
	o := newObject()
	if rs.Effort != "" {
		if xflag(&req.Extra, xEffortNumber) && gjson.Valid(rs.Effort) {
			o.set("effort", json.RawMessage(rs.Effort))
		} else {
			o.str("effort", rs.Effort)
		}
	}
	if rs.Summary != "" {
		o.str("summary", rs.Summary)
	}
	o.merge(rest)
	return o.raw()
}

func encodeInclude(req *ir.Request) json.RawMessage {
	want := req.Reasoning != nil && req.Reasoning.IncludeEncrypted
	orig, had := xget(&req.Extra, xInclude)
	var items []json.RawMessage
	has := false
	for _, v := range gjson.ParseBytes(orig).Array() {
		if v.Str == includeEncryptedRsn {
			has = true
			if !want {
				continue
			}
		}
		items = append(items, json.RawMessage(v.Raw))
	}
	if want && !has {
		items = append(items, jstr(includeEncryptedRsn))
	}
	switch {
	case had && want == has:
		return orig
	case len(items) == 0 && !had:
		return nil
	}
	return jarray(items)
}

func encodeText(req *ir.Request) json.RawMessage {
	rest, ok := xget(&req.Extra, xText)
	if !ok && req.Verbosity == "" {
		return nil
	}
	o := newObject()
	o.merge(rest)
	if req.Verbosity != "" {
		o.str("verbosity", req.Verbosity)
	}
	return o.raw()
}
