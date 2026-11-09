package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/tidwall/gjson"
)

// Wire block types.
const (
	wireText             = "text"
	wireImage            = "image"
	wireDocument         = "document"
	wireThinking         = "thinking"
	wireRedactedThinking = "redacted_thinking"
	wireToolUse          = "tool_use"
	wireToolResult       = "tool_result"
	wireServerToolUse    = "server_tool_use"
)

// Opaque state kinds this protocol carries.
const (
	kindSignature = "signature"
	kindRedacted  = "redacted_thinking"
)

// isServerBlock reports whether a wire block type is a provider-executed
// tool call or its result (server_tool_use, web_search_tool_result,
// mcp_tool_use, code_execution_tool_result, ...).
func isServerBlock(t string) bool {
	return t == wireServerToolUse || t == "mcp_tool_use" || t == "mcp_tool_result" ||
		strings.HasSuffix(t, "_tool_result") && t != wireToolResult
}

// decodeBlocks parses a content value that is either a string or an array.
// The string form becomes one text block and is recorded in shape.
func decodeBlocks(v gjson.Result, shape *ir.Extra) []ir.Block {
	if v.Type == gjson.String {
		shape.Set(name, markShape, json.RawMessage(shapeString))
		return []ir.Block{{Type: ir.BlockText, Text: v.String()}}
	}
	if !v.IsArray() {
		return nil
	}
	arr := v.Array()
	if len(arr) == 0 {
		shape.Set(name, markShape, json.RawMessage(shapeEmptyAr))
		return nil
	}
	out := make([]ir.Block, 0, len(arr))
	for _, b := range arr {
		out = append(out, decodeBlock(b))
	}
	return out
}

// encodeBlocks renders blocks as a content value, honouring a recorded
// string shape when the blocks still fit it.
func encodeBlocks(blocks []ir.Block, shape ir.Extra) json.RawMessage {
	s, _ := shape.Get(name, markShape)
	if string(s) == shapeString && len(blocks) == 1 && blocks[0].Type == ir.BlockText &&
		blocks[0].Cache == nil && blocks[0].Extra.Origin == "" {
		return marshal(blocks[0].Text)
	}
	items := make([]json.RawMessage, 0, len(blocks))
	for i := range blocks {
		if b, ok := encodeBlock(&blocks[i]); ok {
			items = append(items, b)
		}
	}
	return array(items)
}

func decodeCache(b gjson.Result, e *ir.Extra) *ir.CacheMark {
	cc := b.Get("cache_control")
	if !cc.Exists() {
		return nil
	}
	known := []string{"ttl"}
	if cc.Get("type").String() == ephemeral {
		known = append(known, "type")
	}
	keepRest(e, "cache_control", cc, known...)
	return &ir.CacheMark{TTL: cc.Get("ttl").String()}
}

func encodeCache(o *object, c *ir.CacheMark, e ir.Extra) {
	if c == nil {
		return
	}
	cc := newObject()
	cc.setv("type", ephemeral)
	if c.TTL != "" {
		cc.setv("ttl", c.TTL)
	}
	cc.rest(e, restPrefix+"cache_control")
	o.set("cache_control", cc.bytes())
}

// sourceTypes are the source shapes recoverable from ir.Media alone.
var sourceTypes = map[string]bool{"base64": true, "url": true, "file": true}

func decodeSource(src gjson.Result, e *ir.Extra) *ir.Media {
	m := &ir.Media{
		MediaType: src.Get("media_type").String(),
		Data:      src.Get("data").String(),
		URL:       src.Get("url").String(),
		FileID:    src.Get("file_id").String(),
	}
	known := []string{"media_type", "data", "url", "file_id"}
	if sourceTypes[src.Get("type").String()] {
		known = append(known, "type")
	}
	keepRest(e, "source", src, known...)
	return m
}

func encodeSource(m *ir.Media, e ir.Extra) json.RawMessage {
	s := newObject()
	switch {
	case m.Data != "":
		s.setv("type", "base64")
		s.setv("media_type", m.MediaType)
		s.setv("data", m.Data)
	case m.URL != "":
		s.setv("type", "url")
		s.setv("url", m.URL)
	case m.FileID != "":
		s.setv("type", "file")
		s.setv("file_id", m.FileID)
	}
	s.rest(e, restPrefix+"source")
	return s.bytes()
}

// opaqueOf wraps a wire string as opaque state, marking relay seals.
func opaqueOf(kind string, v gjson.Result) *ir.OpaqueState {
	return &ir.OpaqueState{
		Kind:   kind,
		Data:   raw(v),
		Sealed: strings.HasPrefix(v.String(), ir.SealPrefix),
	}
}

// echoString returns the string to write into an Anthropic echo slot, or
// false when the state cannot be expressed in this protocol.
func echoString(op *ir.OpaqueState, kinds ...string) (string, bool) {
	if op == nil {
		return "", false
	}
	ok := op.Sealed
	for _, k := range kinds {
		ok = ok || op.Kind == k
	}
	if !ok {
		return "", false
	}
	return unquote(op.Data)
}

// decodeBlock parses one wire content block. Fields with no IR slot land in
// Block.Extra; unknown block types become BlockRaw.
func decodeBlock(b gjson.Result) ir.Block {
	t := b.Get("type").String()
	var blk ir.Block
	keep := func(known ...string) {
		known = append(known, "type", "cache_control")
		b.ForEach(func(k, v gjson.Result) bool {
			for _, n := range known {
				if k.String() == n {
					return true
				}
			}
			blk.Extra.Set(name, k.String(), raw(v))
			return true
		})
	}
	switch {
	case t == wireText:
		blk = ir.Block{Type: ir.BlockText, Text: b.Get("text").String()}
		keep("text")
	case t == wireImage:
		blk = ir.Block{Type: ir.BlockImage}
		blk.Media = decodeSource(b.Get("source"), &blk.Extra)
		keep("source")
	case t == wireDocument:
		blk = ir.Block{Type: ir.BlockDocument}
		blk.Media = decodeSource(b.Get("source"), &blk.Extra)
		if title := b.Get("title"); title.Type == gjson.String {
			blk.Media.Title = title.String()
			keep("source", "title")
		} else {
			keep("source")
		}
	case t == wireThinking:
		blk = ir.Block{Type: ir.BlockThinking, Text: b.Get("thinking").String()}
		// An empty signature carries no state; it stays a plain field.
		if sig := b.Get("signature"); sig.Type == gjson.String && sig.Str != "" {
			blk.Opaque = opaqueOf(kindSignature, sig)
			keep("thinking", "signature")
		} else {
			keep("thinking")
		}
	case t == wireRedactedThinking:
		blk = ir.Block{Type: ir.BlockOpaque, Opaque: opaqueOf(kindRedacted, b.Get("data"))}
		keep("data")
	case t == wireToolUse:
		blk = ir.Block{Type: ir.BlockToolUse, Tool: &ir.ToolCall{
			ID: b.Get("id").String(), Name: b.Get("name").String(), Kind: ir.ToolFunction,
		}}
		if in := b.Get("input"); in.Exists() {
			blk.Tool.Input = raw(in)
		}
		keep("id", "name", "input")
	case t == wireToolResult:
		blk = ir.Block{Type: ir.BlockToolResult, Result: &ir.ToolResult{
			ToolUseID: b.Get("tool_use_id").String(),
		}}
		known := []string{"tool_use_id", "content"}
		if b.Get("is_error").Type == gjson.True {
			blk.Result.IsError = true
			known = append(known, "is_error")
		}
		blk.Result.Content = decodeBlocks(b.Get("content"), &blk.Extra)
		keep(known...)
	case isServerBlock(t):
		blk = ir.Block{Type: ir.BlockServerTool}
		if t == wireServerToolUse || t == "mcp_tool_use" {
			blk.Tool = &ir.ToolCall{ID: b.Get("id").String(), Name: b.Get("name").String(),
				Kind: ir.ToolServer, Input: raw(b.Get("input"))}
		} else {
			blk.Result = &ir.ToolResult{ToolUseID: b.Get("tool_use_id").String()}
		}
		blk.Extra.Set(name, "type", raw(b.Get("type")))
		keep()
	default:
		blk = ir.Block{Type: ir.BlockRaw}
		blk.Extra.Set(name, "type", raw(b.Get("type")))
		keep()
	}
	blk.Cache = decodeCache(b, &blk.Extra)
	return blk
}

// encodeBlock renders one block. It returns false for blocks this protocol
// cannot express (foreign raw items, foreign unsealed opaque state).
func encodeBlock(b *ir.Block) (json.RawMessage, bool) {
	o := newObject()
	switch b.Type {
	case ir.BlockText:
		o.setv("type", wireText)
		o.setv("text", b.Text)
	case ir.BlockImage, ir.BlockDocument:
		if b.Media == nil {
			return nil, false
		}
		t := wireImage
		if b.Type == ir.BlockDocument {
			t = wireDocument
		}
		o.setv("type", t)
		o.set("source", encodeSource(b.Media, b.Extra))
		if b.Type == ir.BlockDocument && b.Media.Title != "" {
			o.setv("title", b.Media.Title)
		}
	case ir.BlockThinking:
		o.setv("type", wireThinking)
		o.setv("thinking", b.Text)
		if sig, ok := echoString(b.Opaque, kindSignature); ok {
			o.setv("signature", sig)
		}
	case ir.BlockOpaque:
		data, ok := echoString(b.Opaque, kindRedacted)
		if !ok {
			return nil, false
		}
		o.setv("type", wireRedactedThinking)
		o.setv("data", data)
	case ir.BlockToolUse:
		if b.Tool == nil {
			return nil, false
		}
		o.setv("type", wireToolUse)
		o.setv("id", b.Tool.ID)
		o.setv("name", b.Tool.Name)
		o.set("input", toolInput(b.Tool))
	case ir.BlockToolResult:
		if b.Result == nil {
			return nil, false
		}
		o.setv("type", wireToolResult)
		o.setv("tool_use_id", b.Result.ToolUseID)
		if len(b.Result.Content) > 0 || b.Extra.Origin == name && b.Extra.Fields[markShape] != nil {
			o.set("content", encodeBlocks(b.Result.Content, b.Extra))
		}
		if b.Result.IsError {
			o.setv("is_error", true)
		}
	case ir.BlockServerTool:
		if b.Extra.Origin != name {
			if b.Tool == nil {
				return nil, false
			}
			o.setv("type", wireServerToolUse)
			o.setv("id", b.Tool.ID)
			o.setv("name", b.Tool.Name)
			o.set("input", toolInput(b.Tool))
		}
	case ir.BlockRaw:
		if b.Extra.Origin != name {
			return nil, false
		}
	default:
		return nil, false
	}
	encodeCache(o, b.Cache, b.Extra)
	o.extra(b.Extra)
	return o.bytes(), true
}

func toolInput(t *ir.ToolCall) json.RawMessage {
	if len(t.Input) == 0 {
		return json.RawMessage(emptyObject)
	}
	return t.Input
}
