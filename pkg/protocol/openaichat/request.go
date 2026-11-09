package openaichat

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// DecodeRequest parses a chat completions request into IR and computes its needs.
func (Codec) DecodeRequest(_ *http.Request, _ protocol.Inbound, body []byte) (*ir.Request, ir.Needs, error) {
	top := rawMap(body)
	if top == nil {
		return nil, ir.Needs{}, ir.Errorf(ir.ErrInvalidRequest, "request body must be a JSON object")
	}
	req := &ir.Request{}
	bad := func(f string) (*ir.Request, ir.Needs, error) {
		return nil, ir.Needs{}, ir.Errorf(ir.ErrInvalidRequest, "invalid %s", f)
	}
	if v, ok := take(top, "model"); ok {
		req.Model = str(v)
	}
	if v, ok := take(top, "stream"); ok && json.Unmarshal(v, &req.Stream) != nil {
		return bad("stream")
	}
	msgs, _ := take(top, "messages")
	var raw []json.RawMessage
	if json.Unmarshal(msgs, &raw) != nil {
		return bad("messages")
	}
	d := decoder{custom: map[string]bool{}}
	if err := d.messages(req, raw); err != nil {
		return nil, ir.Needs{}, err
	}
	if v, ok := take(top, "tools"); ok {
		var ts []json.RawMessage
		if json.Unmarshal(v, &ts) != nil {
			return bad("tools")
		}
		for _, t := range ts {
			req.Tools = append(req.Tools, decodeTool(t))
		}
	}
	if v, ok := take(top, "tool_choice"); ok {
		tc, known := decodeToolChoice(v)
		if known {
			req.ToolChoice = tc
		} else {
			top["tool_choice"] = v
		}
	}
	nums := []struct {
		k string
		p any
	}{
		{"parallel_tool_calls", &req.ParallelTools},
		{"temperature", &req.Temperature},
		{"top_p", &req.TopP},
		{"top_k", &req.TopK},
		{"store", &req.Store},
	}
	for _, n := range nums {
		if v, ok := take(top, n.k); ok && json.Unmarshal(v, n.p) != nil {
			return bad(n.k)
		}
	}
	if v, ok := take(top, "max_completion_tokens"); ok {
		if json.Unmarshal(v, &req.MaxOutputTokens) != nil {
			return bad("max_completion_tokens")
		}
		delete(top, "max_tokens")
	} else if v, ok := take(top, "max_tokens"); ok {
		if json.Unmarshal(v, &req.MaxOutputTokens) != nil {
			return bad("max_tokens")
		}
		mark(&req.Extra, xMaxTokens, jsonTrue)
	}
	if v, ok := take(top, "stop"); ok {
		var s string
		if json.Unmarshal(v, &s) == nil {
			req.Stop = []string{s}
			mark(&req.Extra, xStopStr, jsonTrue)
		} else if json.Unmarshal(v, &req.Stop) != nil {
			return bad("stop")
		}
	}
	if v, ok := take(top, "reasoning_effort"); ok {
		e := str(v)
		req.Reasoning = &ir.Reasoning{Enabled: e != "none", Effort: e}
	}
	for k, p := range map[string]*string{"service_tier": &req.ServiceTier, "prompt_cache_key": &req.CacheKey, "verbosity": &req.Verbosity} {
		if v, ok := take(top, k); ok {
			*p = str(v)
		}
	}
	for k, v := range top {
		req.Extra.Set(Name, k, v)
	}
	return req, needs(req, body), nil
}

// decoder carries cross-message state: ids of freeform tool calls, so their
// results can be marked Freeform.
type decoder struct {
	custom map[string]bool
}

func (d *decoder) messages(req *ir.Request, raw []json.RawMessage) error {
	leading := true
	for _, rm := range raw {
		m := rawMap(rm)
		if m == nil {
			return ir.Errorf(ir.ErrInvalidRequest, "invalid message")
		}
		role := str(m["role"])
		delete(m, "role")
		switch role {
		case "system", "developer":
			if leading {
				req.System = append(req.System, decodeSystem(role, m)...)
				continue
			}
			blocks, isArr, err := d.parts(m)
			if err != nil {
				return err
			}
			msg := ir.Message{Role: ir.RoleUser, Content: blocks, Extra: extraFrom(m)}
			mark(&msg.Extra, xRole, mustRaw(role))
			if isArr {
				mark(&msg.Extra, xArray, jsonTrue)
			}
			req.Messages = append(req.Messages, msg)
		case "user":
			blocks, isArr, err := d.parts(m)
			if err != nil {
				return err
			}
			msg := ir.Message{Role: ir.RoleUser, Content: blocks, Extra: extraFrom(m)}
			if isArr {
				mark(&msg.Extra, xArray, jsonTrue)
			}
			req.Messages = append(req.Messages, msg)
		case "assistant":
			msg, err := d.assistant(m)
			if err != nil {
				return err
			}
			req.Messages = append(req.Messages, msg)
		case "tool":
			b, err := d.tool(m)
			if err != nil {
				return err
			}
			if n := len(req.Messages); n > 0 && isToolResults(req.Messages[n-1]) {
				req.Messages[n-1].Content = append(req.Messages[n-1].Content, b)
			} else {
				req.Messages = append(req.Messages, ir.Message{Role: ir.RoleUser, Content: []ir.Block{b}})
			}
		default:
			return ir.Errorf(ir.ErrInvalidRequest, "unknown message role %q", role)
		}
		leading = false
	}
	return nil
}

func isToolResults(m ir.Message) bool {
	if m.Role != ir.RoleUser || len(m.Content) == 0 || len(m.Extra.Fields) > 0 {
		return false
	}
	for _, b := range m.Content {
		if b.Type != ir.BlockToolResult {
			return false
		}
	}
	return true
}

func decodeSystem(role string, m map[string]json.RawMessage) []ir.Block {
	content := m["content"]
	delete(m, "content")
	msgCache, hasMsgCache := take(m, "cache_control")
	var blocks []ir.Block
	isArr := false
	var s string
	if json.Unmarshal(content, &s) == nil {
		blocks = []ir.Block{{Type: ir.BlockText, Text: s}}
	} else {
		var parts []json.RawMessage
		_ = json.Unmarshal(content, &parts)
		isArr = true
		for _, p := range parts {
			blocks = append(blocks, decodeTextPart(p))
		}
		if len(blocks) == 0 {
			blocks = []ir.Block{{Type: ir.BlockText}}
		}
	}
	for i := range blocks {
		b := &blocks[i]
		if role == "developer" {
			mark(&b.Extra, xRole, mustRaw(role))
		}
		if isArr {
			mark(&b.Extra, xArray, jsonTrue)
		}
		if i > 0 {
			mark(&b.Extra, xCont, jsonTrue)
		}
	}
	if len(m) > 0 {
		mark(&blocks[0].Extra, xMsg, mustRaw(m))
	}
	if hasMsgCache {
		last := &blocks[len(blocks)-1]
		last.Cache = cacheMark(msgCache)
		mark(&last.Extra, xMsgCache, jsonTrue)
	}
	return blocks
}

// decodeTextPart decodes a part that is expected to be text; anything else is
// kept verbatim as a raw block.
func decodeTextPart(p json.RawMessage) ir.Block {
	m := rawMap(p)
	if m == nil || str(m["type"]) != "text" {
		b := ir.Block{Type: ir.BlockRaw}
		mark(&b.Extra, xPart, p)
		return b
	}
	b := ir.Block{Type: ir.BlockText, Text: str(m["text"])}
	delete(m, "type")
	delete(m, "text")
	if c, ok := take(m, "cache_control"); ok {
		b.Cache = cacheMark(c)
	}
	for k, v := range m {
		b.Extra.Set(Name, k, v)
	}
	return b
}

// parts decodes m["content"] (string or part array) and a message-level
// cache_control, consuming both from m.
func (d *decoder) parts(m map[string]json.RawMessage) ([]ir.Block, bool, error) {
	content := m["content"]
	delete(m, "content")
	msgCache, hasMsgCache := take(m, "cache_control")
	var blocks []ir.Block
	isArr := false
	var s string
	if isNull(content) {
	} else if json.Unmarshal(content, &s) == nil {
		blocks = []ir.Block{{Type: ir.BlockText, Text: s}}
	} else {
		var parts []json.RawMessage
		if json.Unmarshal(content, &parts) != nil {
			return nil, false, ir.Errorf(ir.ErrInvalidRequest, "invalid message content")
		}
		isArr = true
		for _, p := range parts {
			blocks = append(blocks, decodePart(p))
		}
	}
	if hasMsgCache && len(blocks) > 0 {
		last := &blocks[len(blocks)-1]
		last.Cache = cacheMark(msgCache)
		mark(&last.Extra, xMsgCache, jsonTrue)
	} else if hasMsgCache {
		m["cache_control"] = msgCache
	}
	return blocks, isArr, nil
}

func decodePart(p json.RawMessage) ir.Block {
	m := rawMap(p)
	if m == nil {
		return rawBlock(p)
	}
	switch str(m["type"]) {
	case "text":
		return decodeTextPart(p)
	case "image_url":
		img := rawMap(m["image_url"])
		var url string
		if img == nil {
			url = str(m["image_url"])
		} else {
			url = str(img["url"])
		}
		media := mediaFromURL(url)
		if img != nil {
			media.Detail = str(img["detail"])
			delete(img, "url")
			delete(img, "detail")
			if len(img) > 0 {
				return rawBlock(p)
			}
		}
		b := ir.Block{Type: ir.BlockImage, Media: media}
		if c, ok := take(m, "cache_control"); ok {
			b.Cache = cacheMark(c)
		}
		delete(m, "type")
		delete(m, "image_url")
		for k, v := range m {
			b.Extra.Set(Name, k, v)
		}
		return b
	case "file":
		f := rawMap(m["file"])
		if f == nil {
			return rawBlock(p)
		}
		media := &ir.Media{FileID: str(f["file_id"]), Title: str(f["filename"])}
		if data := str(f["file_data"]); data != "" {
			fm := mediaFromURL(data)
			media.MediaType, media.Data, media.URL = fm.MediaType, fm.Data, fm.URL
		}
		delete(f, "file_id")
		delete(f, "filename")
		delete(f, "file_data")
		if len(f) > 0 {
			return rawBlock(p)
		}
		b := ir.Block{Type: ir.BlockDocument, Media: media}
		if c, ok := take(m, "cache_control"); ok {
			b.Cache = cacheMark(c)
		}
		return b
	}
	return rawBlock(p)
}

func rawBlock(p json.RawMessage) ir.Block {
	b := ir.Block{Type: ir.BlockRaw}
	if c := cacheMark(rawMap(p)["cache_control"]); c != nil {
		b.Cache = c
	}
	mark(&b.Extra, xPart, p)
	return b
}

// mediaFromURL splits a base64 data: URL; any other URL is kept as a reference.
func mediaFromURL(u string) *ir.Media {
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		if meta, data, ok := strings.Cut(rest, ","); ok {
			if mt, ok := strings.CutSuffix(meta, ";base64"); ok {
				return &ir.Media{MediaType: mt, Data: data}
			}
		}
	}
	return &ir.Media{URL: u}
}

func (d *decoder) assistant(m map[string]json.RawMessage) (ir.Message, error) {
	msg := ir.Message{Role: ir.RoleAssistant}
	think, ok := thinkingBlock(m)
	if ok {
		msg.Content = append(msg.Content, think)
	}
	content, isArr, err := d.parts(m)
	if err != nil {
		return msg, err
	}
	msg.Content = append(msg.Content, content...)
	if v, ok := take(m, "refusal"); ok {
		b := ir.Block{Type: ir.BlockText, Text: str(v)}
		mark(&b.Extra, xRefusal, jsonTrue)
		msg.Content = append(msg.Content, b)
	}
	if v, ok := take(m, "tool_calls"); ok {
		var calls []json.RawMessage
		if json.Unmarshal(v, &calls) != nil {
			return msg, ir.Errorf(ir.ErrInvalidRequest, "invalid tool_calls")
		}
		for _, c := range calls {
			b := decodeToolCall(c)
			if b.Tool != nil && b.Tool.Kind == ir.ToolFreeform {
				d.custom[b.Tool.ID] = true
			}
			msg.Content = append(msg.Content, b)
		}
	}
	msg.Extra = extraFrom(m)
	if isArr {
		mark(&msg.Extra, xArray, jsonTrue)
	}
	return msg, nil
}

// thinkingBlock consumes reasoning_content / reasoning / reasoning_details.
func thinkingBlock(m map[string]json.RawMessage) (ir.Block, bool) {
	b := ir.Block{Type: ir.BlockThinking}
	found := false
	if v, ok := take(m, "reasoning_content"); ok {
		b.Text, found = str(v), true
	} else if v, ok := take(m, "reasoning"); ok {
		if s := str(v); s != "" || string(v) == `""` {
			b.Text, found = s, true
			mark(&b.Extra, xField, mustRaw("reasoning"))
		} else {
			m["reasoning"] = v
		}
	}
	delete(m, "reasoning_content")
	if v, ok := take(m, "reasoning_details"); ok {
		mark(&b.Extra, "reasoning_details", v)
		if !found {
			b.Text = detailsText(v)
			mark(&b.Extra, xField, mustRaw("none"))
			found = true
		}
	}
	return b, found
}

// detailsText joins the readable text of a reasoning_details array.
func detailsText(v json.RawMessage) string {
	var items []struct {
		Text    string `json:"text"`
		Summary string `json:"summary"`
	}
	_ = json.Unmarshal(v, &items)
	var sb strings.Builder
	for _, it := range items {
		sb.WriteString(it.Text)
		sb.WriteString(it.Summary)
	}
	return sb.String()
}

func decodeToolCall(c json.RawMessage) ir.Block {
	m := rawMap(c)
	if m == nil {
		b := rawBlock(c)
		mark(&b.Extra, xCall, jsonTrue)
		return b
	}
	b := ir.Block{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: str(m["id"])}}
	typ := str(m["type"])
	delete(m, "id")
	delete(m, "type")
	switch typ {
	case "custom":
		cu := rawMap(m["custom"])
		delete(m, "custom")
		b.Tool.Kind, b.Tool.Name, b.Tool.Text = ir.ToolFreeform, str(cu["name"]), str(cu["input"])
	case "function", "":
		fn := rawMap(m["function"])
		delete(m, "function")
		b.Tool.Kind, b.Tool.Name = ir.ToolFunction, str(fn["name"])
		args := str(fn["arguments"])
		if json.Valid([]byte(args)) {
			b.Tool.Input = json.RawMessage(args)
		} else {
			b.Tool.Input = mustRaw(args)
			mark(&b.Extra, xArgsRaw, jsonTrue)
		}
	default:
		b = rawBlock(c)
		mark(&b.Extra, xCall, jsonTrue)
		return b
	}
	for k, v := range m {
		b.Extra.Set(Name, k, v)
	}
	return b
}

func (d *decoder) tool(m map[string]json.RawMessage) (ir.Block, error) {
	id := str(m["tool_call_id"])
	delete(m, "tool_call_id")
	content, isArr, err := d.parts(m)
	if err != nil {
		return ir.Block{}, err
	}
	res := &ir.ToolResult{ToolUseID: id, Freeform: d.custom[id]}
	b := ir.Block{Type: ir.BlockToolResult, Result: res}
	for _, c := range content {
		if c.Cache != nil && b.Cache == nil {
			b.Cache = c.Cache
			if has(&c.Extra, xMsgCache) {
				mark(&b.Extra, xMsgCache, jsonTrue)
			}
			c.Cache = nil
			delete(c.Extra.Fields, xMsgCache)
		}
		res.Content = append(res.Content, c)
	}
	if isArr {
		mark(&b.Extra, xArray, jsonTrue)
	}
	if len(m) > 0 {
		mark(&b.Extra, xMsg, mustRaw(m))
	}
	return b, nil
}

func decodeTool(t json.RawMessage) ir.Tool {
	m := rawMap(t)
	out := ir.Tool{}
	if c, ok := take(m, "cache_control"); ok {
		out.Cache = cacheMark(c)
	}
	typ := str(m["type"])
	inner := rawMap(m[typ])
	delete(m, "type")
	delete(m, typ)
	switch {
	case typ == "function" && inner != nil:
		fn := inner
		out.Kind, out.Name, out.Description = ir.ToolFunction, str(fn["name"]), str(fn["description"])
		out.InputSchema = fn["parameters"]
		delete(fn, "name")
		delete(fn, "description")
		delete(fn, "parameters")
		out.Extra = extraFrom(fn)
		if len(m) > 0 {
			mark(&out.Extra, xMsg, mustRaw(m))
		}
		return out
	case typ == "custom" && inner != nil:
		cu := inner
		out.Kind, out.Name, out.Description = ir.ToolFreeform, str(cu["name"]), str(cu["description"])
		out.Grammar = cu["format"]
		delete(cu, "name")
		delete(cu, "description")
		delete(cu, "format")
		out.Extra = extraFrom(cu)
		if len(m) > 0 {
			mark(&out.Extra, xMsg, mustRaw(m))
		}
		return out
	}
	out.Kind, out.ServerType = ir.ToolServer, typ
	mark(&out.Extra, xPart, t)
	return out
}

// decodeToolChoice reports false for shapes the IR cannot express
// (allowed_tools), which then stay in Extra.
func decodeToolChoice(v json.RawMessage) (*ir.ToolChoice, bool) {
	var s string
	if json.Unmarshal(v, &s) == nil {
		switch s {
		case "auto", "none":
			return &ir.ToolChoice{Mode: s}, true
		case "required":
			return &ir.ToolChoice{Mode: "any"}, true
		}
		return nil, false
	}
	m := rawMap(v)
	switch t := str(m["type"]); t {
	case "function", "custom":
		if name := str(rawMap(m[t])["name"]); name != "" {
			return &ir.ToolChoice{Mode: "tool", Name: name}, true
		}
	}
	return nil, false
}

func needs(req *ir.Request, body []byte) ir.Needs {
	var n ir.Needs
	mediaBytes := 0
	var walk func(bs []ir.Block, inResult bool)
	walk = func(bs []ir.Block, inResult bool) {
		for _, b := range bs {
			if b.Cache != nil {
				n.ExplicitCache = true
				n.Cache1h = n.Cache1h || b.Cache.TTL == "1h"
			}
			if b.Media != nil {
				mediaBytes += len(b.Media.Data)
			}
			switch b.Type {
			case ir.BlockImage:
				if inResult {
					n.ToolResultImgs = true
				} else {
					n.Images = true
				}
			case ir.BlockDocument:
				n.Documents = true
			case ir.BlockToolResult:
				walk(b.Result.Content, true)
			case ir.BlockThinking:
				if has(&b.Extra, "reasoning_details") {
					n.OpaqueState = true
				}
			}
		}
	}
	walk(req.System, false)
	for _, m := range req.Messages {
		walk(m.Content, false)
	}
	for _, t := range req.Tools {
		if t.Cache != nil {
			n.ExplicitCache = true
			n.Cache1h = n.Cache1h || t.Cache.TTL == "1h"
		}
		switch t.Kind {
		case ir.ToolFunction:
			n.FunctionTools = true
		case ir.ToolFreeform:
			n.FreeformTools = true
		case ir.ToolServer:
			n.ServerTools = append(n.ServerTools, t.ServerType)
		}
	}
	n.ServerToolsRequired = req.RequiredServerTools()
	n.Reasoning = req.Reasoning != nil && req.Reasoning.Enabled
	n.InputTokensEst = (len(body)-mediaBytes)/4 + 1
	return n
}
