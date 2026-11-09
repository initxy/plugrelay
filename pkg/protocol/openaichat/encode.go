package openaichat

import (
	"encoding/json"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
)

// EncodeRequest renders IR as a chat completions request body. The output
// limit goes in max_completion_tokens unless the client sent max_tokens; the
// pipeline renames it to what the endpoint wants. Streaming requests always
// ask for a usage chunk.
func (Codec) EncodeRequest(req *ir.Request) ([]byte, error) {
	o := object{{"model", req.Model}, {"messages", encodeMessages(req)}}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, encodeTool(t))
		}
		o.add("tools", tools)
	}
	if req.ToolChoice != nil {
		o.add("tool_choice", encodeToolChoice(req.ToolChoice, req.Tools))
	}
	if req.ParallelTools != nil {
		o.add("parallel_tool_calls", *req.ParallelTools)
	}
	if req.MaxOutputTokens != nil {
		k := "max_completion_tokens"
		if has(&req.Extra, xMaxTokens) {
			k = "max_tokens"
		}
		o.add(k, *req.MaxOutputTokens)
	}
	if req.Temperature != nil {
		o.add("temperature", *req.Temperature)
	}
	if req.TopP != nil {
		o.add("top_p", *req.TopP)
	}
	if req.TopK != nil {
		o.add("top_k", *req.TopK)
	}
	if len(req.Stop) == 1 && has(&req.Extra, xStopStr) {
		o.add("stop", req.Stop[0])
	} else if len(req.Stop) > 0 {
		o.add("stop", req.Stop)
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		o.add("reasoning_effort", req.Reasoning.Effort)
	}
	if req.Stream {
		o.add("stream", true)
		so := object{}
		if v, ok := req.Extra.Get(Name, "stream_options"); ok {
			mergeRaw(&so, v)
		}
		so.set("include_usage", true)
		o.add("stream_options", so)
	}
	if req.ServiceTier != "" {
		o.add("service_tier", req.ServiceTier)
	}
	if req.CacheKey != "" {
		o.add("prompt_cache_key", req.CacheKey)
	}
	if req.Verbosity != "" {
		o.add("verbosity", req.Verbosity)
	}
	if req.Store != nil {
		o.add("store", *req.Store)
	}
	restore(&o, req.Extra, "stream_options", "max_tokens", "max_completion_tokens")
	return marshal(o)
}

func encodeMessages(req *ir.Request) []any {
	var out []any
	var cur object
	var parts []ir.Block
	flush := func() {
		if cur == nil {
			return
		}
		b0 := parts[0]
		content, cache := contentOf(parts, has(&b0.Extra, xArray))
		cur.add("content", content)
		if cache != nil {
			cur.add("cache_control", cacheControl(cache))
		}
		restoreRaw(&cur, b0.Extra, xMsg)
		out = append(out, cur)
		cur, parts = nil, nil
	}
	for _, b := range req.System {
		if cur == nil || !has(&b.Extra, xCont) {
			flush()
			role := getStr(&b.Extra, xRole)
			if role == "" {
				role = "system"
			}
			cur = object{{"role", role}}
		}
		parts = append(parts, b)
	}
	flush()
	custom := customToolNames(req.Tools)
	for _, m := range req.Messages {
		if m.Role == ir.RoleAssistant {
			out = append(out, encodeAssistant(m.Content, m.Extra, true, custom))
			continue
		}
		for _, mm := range encodeUser(m) {
			out = append(out, mm)
		}
	}
	if out == nil {
		out = []any{}
	}
	return out
}

func customToolNames(tools []ir.Tool) map[string]bool {
	names := map[string]bool{}
	for _, t := range tools {
		if t.Kind == ir.ToolFreeform {
			names[t.Name] = true
		}
	}
	return names
}

// contentOf renders blocks as a string when they are all plain text, else
// as a part array. A cache mark flagged as message-level is returned separately.
func contentOf(blocks []ir.Block, forceArray bool) (any, *ir.CacheMark) {
	var msgCache *ir.CacheMark
	if n := len(blocks); n > 0 && blocks[n-1].Cache != nil && has(&blocks[n-1].Extra, xMsgCache) {
		msgCache = blocks[n-1].Cache
		last := blocks[n-1]
		last.Cache = nil
		blocks = append(blocks[:n-1:n-1], last)
	}
	if len(blocks) == 0 {
		return "", msgCache
	}
	if !forceArray && allPlain(blocks) {
		// Several text parts become one string too: some chat upstreams
		// accept only string content.
		texts := make([]string, len(blocks))
		for i, b := range blocks {
			texts[i] = b.Text
		}
		return strings.Join(texts, "\n\n"), msgCache
	}
	parts := make([]any, 0, len(blocks))
	for _, b := range blocks {
		if p, ok := encodePart(b); ok {
			parts = append(parts, p)
		}
	}
	return parts, msgCache
}

// plainText reports whether b is text with nothing a string could not carry.
func plainText(b ir.Block) bool {
	if b.Type != ir.BlockText || b.Cache != nil {
		return false
	}
	if b.Extra.Origin == Name {
		for k := range b.Extra.Fields {
			if !strings.HasPrefix(k, "_") {
				return false
			}
		}
	}
	return true
}

func encodePart(b ir.Block) (any, bool) {
	var o object
	switch b.Type {
	case ir.BlockText:
		o = object{{"type", "text"}, {"text", b.Text}}
		restore(&o, b.Extra)
	case ir.BlockImage:
		if b.Media == nil {
			return nil, false
		}
		img := object{{"url", mediaURL(b.Media)}}
		if b.Media.Detail != "" {
			img.add("detail", b.Media.Detail)
		}
		o = object{{"type", "image_url"}, {"image_url", img}}
		restore(&o, b.Extra)
	case ir.BlockDocument:
		if b.Media == nil {
			return nil, false
		}
		f := object{}
		if b.Media.Title != "" {
			f.add("filename", b.Media.Title)
		}
		switch {
		case b.Media.Data != "" || b.Media.URL != "":
			f.add("file_data", mediaURL(b.Media))
		case b.Media.FileID != "":
			f.add("file_id", b.Media.FileID)
		}
		o = object{{"type", "file"}, {"file", f}}
	case ir.BlockRaw:
		if v, ok := b.Extra.Get(Name, xPart); ok && !has(&b.Extra, xCall) {
			return v, true
		}
		return nil, false
	default:
		return nil, false
	}
	if b.Cache != nil {
		o.add("cache_control", cacheControl(b.Cache))
	}
	return o, true
}

func mediaURL(m *ir.Media) string {
	if m.Data != "" {
		return "data:" + m.MediaType + ";base64," + m.Data
	}
	return m.URL
}

// encodeUser splits a user message into tool messages (one per result) and
// user messages for the runs of other blocks, preserving order.
func encodeUser(m ir.Message) []object {
	var out []object
	var run []ir.Block
	first := true
	flush := func() {
		if len(run) == 0 {
			return
		}
		role := getStr(&m.Extra, xRole)
		if role == "" {
			role = "user"
		}
		o := object{{"role", role}}
		content, cache := contentOf(run, has(&m.Extra, xArray))
		o.add("content", content)
		if cache != nil {
			o.add("cache_control", cacheControl(cache))
		}
		if first {
			restore(&o, m.Extra)
			first = false
		}
		out = append(out, o)
		run = nil
	}
	for _, b := range m.Content {
		if b.Type != ir.BlockToolResult || b.Result == nil {
			run = append(run, b)
			continue
		}
		flush()
		out = append(out, encodeToolResult(b))
	}
	flush()
	if len(out) == 0 {
		o := object{{"role", "user"}, {"content", ""}}
		restore(&o, m.Extra)
		out = append(out, o)
	}
	return out
}

func encodeToolResult(b ir.Block) object {
	content := append([]ir.Block(nil), b.Result.Content...)
	if b.Cache != nil && len(content) > 0 {
		last := content[len(content)-1]
		last.Cache = b.Cache
		if has(&b.Extra, xMsgCache) {
			last.Extra = copyExtra(last.Extra)
			mark(&last.Extra, xMsgCache, jsonTrue)
		}
		content[len(content)-1] = last
	}
	o := object{{"role", "tool"}, {"tool_call_id", b.Result.ToolUseID}}
	c, cache := contentOf(content, has(&b.Extra, xArray))
	o.add("content", c)
	if cache != nil {
		o.add("cache_control", cacheControl(cache))
	}
	restoreRaw(&o, b.Extra, xMsg)
	return o
}

func copyExtra(e ir.Extra) ir.Extra {
	out := ir.Extra{Origin: e.Origin}
	if e.Fields != nil {
		out.Fields = make(ir.RawFields, len(e.Fields))
		for k, v := range e.Fields {
			out.Fields[k] = v
		}
	}
	return out
}

// encodeAssistant renders an assistant message. Freeform tool calls use the
// custom shape when customOK, else the function shape with the raw input as
// arguments.
func encodeAssistant(blocks []ir.Block, extra ir.Extra, customOK bool, custom map[string]bool) object {
	var texts, others []ir.Block
	var reasoning strings.Builder
	var refusal *string
	var details json.RawMessage
	field := ""
	thinking := false
	var calls []any
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockThinking:
			if !thinking {
				field = getStr(&b.Extra, xField)
			}
			thinking = true
			reasoning.WriteString(b.Text)
			if v, ok := b.Extra.Get(Name, "reasoning_details"); ok && details == nil {
				details = v
			}
		case ir.BlockText:
			if has(&b.Extra, xRefusal) {
				s := b.Text
				refusal = &s
				continue
			}
			texts = append(texts, b)
		case ir.BlockToolUse:
			if b.Tool != nil {
				calls = append(calls, encodeToolCall(b, customOK || custom[b.Tool.Name]))
			}
		case ir.BlockRaw:
			if v, ok := b.Extra.Get(Name, xPart); ok && has(&b.Extra, xCall) {
				calls = append(calls, v)
			} else {
				others = append(others, b)
			}
		}
	}
	o := object{{"role", "assistant"}}
	switch {
	case has(&extra, xArray) || len(others) > 0 || !allPlain(texts):
		c, cache := contentOf(append(texts, others...), true)
		o.add("content", c)
		if cache != nil {
			o.add("cache_control", cacheControl(cache))
		}
	case len(texts) > 0:
		var sb strings.Builder
		for _, t := range texts {
			sb.WriteString(t.Text)
		}
		o.add("content", sb.String())
		if c := texts[len(texts)-1].Cache; c != nil {
			o.add("cache_control", cacheControl(c))
		}
	case len(calls) > 0 || refusal != nil:
		o.add("content", nil)
	default:
		o.add("content", "")
	}
	if thinking {
		switch field {
		case "none":
		case "reasoning":
			o.add("reasoning", reasoning.String())
		default:
			o.add("reasoning_content", reasoning.String())
		}
		if details != nil {
			o.add("reasoning_details", details)
		}
	}
	if refusal != nil {
		o.add("refusal", *refusal)
	}
	if len(calls) > 0 {
		o.add("tool_calls", calls)
	}
	restore(&o, extra)
	return o
}

// allPlain reports whether texts can be joined into one string; a
// message-level cache mark on the last block is allowed.
func allPlain(texts []ir.Block) bool {
	for i, t := range texts {
		if i == len(texts)-1 && t.Cache != nil && has(&t.Extra, xMsgCache) {
			t.Cache = nil
		}
		if !plainText(t) {
			return false
		}
	}
	return true
}

func encodeToolCall(b ir.Block, customOK bool) object {
	t := b.Tool
	var o object
	if t.Kind == ir.ToolFreeform && customOK {
		o = object{{"id", t.ID}, {"type", "custom"}, {"custom", object{{"name", t.Name}, {"input", t.Text}}}}
	} else {
		o = object{{"id", t.ID}, {"type", "function"}, {"function", object{{"name", t.Name}, {"arguments", toolArgs(b)}}}}
	}
	restore(&o, b.Extra)
	return o
}

func toolArgs(b ir.Block) string {
	t := b.Tool
	if t.Kind == ir.ToolFreeform {
		return t.Text
	}
	if has(&b.Extra, xArgsRaw) {
		return str(t.Input)
	}
	if len(t.Input) == 0 {
		return "{}"
	}
	return string(t.Input)
}

func encodeTool(t ir.Tool) any {
	var o object
	switch t.Kind {
	case ir.ToolFunction:
		fn := object{{"name", t.Name}}
		if t.Description != "" {
			fn.add("description", t.Description)
		}
		params := t.InputSchema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		fn.add("parameters", params)
		restore(&fn, t.Extra)
		o = object{{"type", "function"}, {"function", fn}}
	case ir.ToolFreeform:
		cu := object{{"name", t.Name}}
		if t.Description != "" {
			cu.add("description", t.Description)
		}
		if len(t.Grammar) > 0 {
			cu.add("format", t.Grammar)
		}
		restore(&cu, t.Extra)
		o = object{{"type", "custom"}, {"custom", cu}}
	default:
		if v, ok := t.Extra.Get(Name, xPart); ok {
			return v
		}
		o = object{{"type", t.ServerType}}
	}
	restoreRaw(&o, t.Extra, xMsg)
	if t.Cache != nil {
		o.add("cache_control", cacheControl(t.Cache))
	}
	return o
}

func encodeToolChoice(tc *ir.ToolChoice, tools []ir.Tool) any {
	switch tc.Mode {
	case "none", "auto":
		return tc.Mode
	case "any":
		return "required"
	case "tool":
		typ := "function"
		for _, t := range tools {
			if t.Name == tc.Name && t.Kind == ir.ToolFreeform {
				typ = "custom"
			}
		}
		return object{{"type", typ}, {typ, object{{"name", tc.Name}}}}
	}
	return "auto"
}
