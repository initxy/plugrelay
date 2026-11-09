package openairesponses

import (
	"encoding/json"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/tidwall/gjson"
)

// Wire item types.
const (
	itemMessage          = "message"
	itemFunctionCall     = "function_call"
	itemFunctionOutput   = "function_call_output"
	itemCustomCall       = "custom_tool_call"
	itemCustomOutput     = "custom_tool_call_output"
	itemReasoning        = "reasoning"
	itemCompaction       = "compaction"
	kindEncrypted        = "encrypted_content"
	kindCompaction       = "compaction"
	partSummaryText      = "summary_text"
	partReasoningText    = "reasoning_text"
	includeEncryptedRsn  = "reasoning.encrypted_content"
	placementInstruction = "instructions"
	placementInput       = "input"
)

// Block Extra keys (origin Name). An "item" key marks the first block of a
// wire item and holds the item's fields the IR has no slot for; blocks of
// the same message item that follow it carry no "item" key.
const (
	xItem          = "item"           // object: unmodeled item fields
	xRole          = "role"           // string: message role when not the side's default
	xPart          = "part"           // object: unmodeled content-part fields
	xPartType      = "part_type"      // string: content-part type when not the role's default
	xRaw           = "raw"            // verbatim item (BlockRaw)
	xRawPart       = "raw_part"       // verbatim content part (BlockRaw)
	xContentString = "content_string" // message content was a plain string
	xUntyped       = "untyped"        // message item had no type field
	xOutputArray   = "output_array"   // tool output was an array of parts
	xArguments     = "arguments"      // function arguments that are not valid JSON
	xSummary       = "summary"        // verbatim multi-part reasoning summary
	xContent       = "content"        // verbatim reasoning content
	xTextSource    = "text_source"    // "content" when thinking text came from reasoning content
	xPlacement     = "placement"      // system block origin: instructions | input
	xAnnotation    = "annotation"     // citation delta payload
)

// sideOf reports which conversation side an item belongs to. ok is false for
// unknown item types, which stay with the surrounding message.
func sideOf(it gjson.Result) (ir.Role, bool) {
	typ := it.Get("type").Str
	switch typ {
	case itemFunctionCall, itemCustomCall, itemReasoning, itemCompaction:
		return ir.RoleAssistant, true
	case itemFunctionOutput, itemCustomOutput:
		return ir.RoleUser, true
	case itemMessage, "":
		if it.Get("role").Str == "assistant" {
			return ir.RoleAssistant, true
		}
		return ir.RoleUser, true
	}
	switch {
	case strings.HasSuffix(typ, "_call"):
		return ir.RoleAssistant, true
	case strings.HasSuffix(typ, "_output"):
		return ir.RoleUser, true
	}
	return "", false
}

func isSystemMessage(it gjson.Result) bool {
	t := it.Get("type").Str
	r := it.Get("role").Str
	return (t == itemMessage || t == "") && (r == "developer" || r == "system")
}

func defaultRole(side ir.Role) string {
	if side == ir.RoleAssistant {
		return "assistant"
	}
	return "user"
}

// decodeInput turns an input array into leading system blocks and messages.
func decodeInput(items []gjson.Result, n *ir.Needs) (system []ir.Block, msgs []ir.Message) {
	lead := true
	for _, it := range items {
		if lead && isSystemMessage(it) {
			bs := messageBlocks(it, "developer", n)
			for i := range bs {
				xset(&bs[i].Extra, xPlacement, jstr(placementInput))
			}
			system = append(system, bs...)
			continue
		}
		lead = false
		side, ok := sideOf(it)
		if !ok {
			side = ir.RoleUser
			if len(msgs) > 0 {
				side = msgs[len(msgs)-1].Role
			}
		}
		bs := itemBlocks(it, side, n)
		if len(msgs) > 0 && msgs[len(msgs)-1].Role == side {
			msgs[len(msgs)-1].Content = append(msgs[len(msgs)-1].Content, bs...)
			continue
		}
		msgs = append(msgs, ir.Message{Role: side, Content: bs})
	}
	return system, msgs
}

// itemBlocks converts one wire item into IR blocks.
func itemBlocks(it gjson.Result, side ir.Role, n *ir.Needs) []ir.Block {
	switch it.Get("type").Str {
	case itemMessage, "":
		if it.Get("role").Exists() {
			return messageBlocks(it, defaultRole(side), n)
		}
	case itemFunctionCall:
		return []ir.Block{functionCallBlock(it, n)}
	case itemCustomCall:
		n.FreeformTools = true
		b := ir.Block{Type: ir.BlockToolUse, Tool: &ir.ToolCall{
			ID: it.Get("call_id").Str, Name: it.Get("name").Str, Kind: ir.ToolFreeform,
			Text: it.Get("input").Str, Namespace: it.Get("namespace").Str,
		}}
		xset(&b.Extra, xItem, itemRest(it, "call_id", "name", "input", "namespace"))
		return []ir.Block{b}
	case itemFunctionOutput, itemCustomOutput:
		return []ir.Block{toolOutputBlock(it, n)}
	case itemReasoning:
		return []ir.Block{reasoningBlock(it, n)}
	case itemCompaction:
		if enc := it.Get("encrypted_content"); enc.Type == gjson.String {
			n.Compaction, n.OpaqueState = true, true
			b := ir.Block{Type: ir.BlockOpaque, Opaque: opaqueOf(kindCompaction, enc)}
			xset(&b.Extra, xItem, itemRest(it, "encrypted_content"))
			return []ir.Block{b}
		}
	}
	return []ir.Block{rawBlock(it)}
}

func rawBlock(it gjson.Result) ir.Block {
	b := ir.Block{Type: ir.BlockRaw}
	xset(&b.Extra, xRaw, json.RawMessage(it.Raw))
	return b
}

// itemRest is the item's fields minus type and the given modeled keys; it is
// "{}" rather than nil so it can always mark an item start.
func itemRest(it gjson.Result, known ...string) json.RawMessage {
	r := restObject(it.Raw, append(known, "type")...)
	if r == nil {
		return json.RawMessage("{}")
	}
	return r
}

func opaqueOf(kind string, v gjson.Result) *ir.OpaqueState {
	return &ir.OpaqueState{
		Kind:   kind,
		Data:   json.RawMessage(v.Raw),
		Sealed: strings.HasPrefix(v.Str, ir.SealPrefix),
	}
}

// messageBlocks converts a message item; dflt is the role implied by its side.
func messageBlocks(it gjson.Result, dflt string, n *ir.Needs) []ir.Block {
	role := it.Get("role").Str
	content := it.Get("content")
	var bs []ir.Block
	if content.Type == gjson.String {
		bs = []ir.Block{{Type: ir.BlockText, Text: content.Str}}
	} else {
		for _, p := range content.Array() {
			bs = append(bs, partBlock(p, role, n))
		}
	}
	if len(bs) == 0 {
		return []ir.Block{rawBlock(it)}
	}
	first := &bs[0].Extra
	xset(first, xItem, itemRest(it, "role", "content"))
	if role != dflt {
		xset(first, xRole, jstr(role))
	}
	if content.Type == gjson.String {
		xset(first, xContentString, jtrue)
	}
	if !it.Get("type").Exists() {
		xset(first, xUntyped, jtrue)
	}
	return bs
}

func defaultTextPart(role string) string {
	if role == "assistant" {
		return "output_text"
	}
	return "input_text"
}

// partBlock converts one content part of a message or tool output.
func partBlock(p gjson.Result, role string, n *ir.Needs) ir.Block {
	typ := p.Get("type").Str
	var b ir.Block
	switch typ {
	case "input_text", "output_text", "text":
		b = ir.Block{Type: ir.BlockText, Text: p.Get("text").Str}
		xset(&b.Extra, xPart, restObject(p.Raw, "type", "text"))
	case "refusal":
		b = ir.Block{Type: ir.BlockText, Text: p.Get("refusal").Str}
		xset(&b.Extra, xPart, restObject(p.Raw, "type", "refusal"))
	case "input_image":
		n.Images = true
		m := &ir.Media{FileID: p.Get("file_id").Str, Detail: p.Get("detail").Str}
		setDataURL(m, p.Get("image_url").Str)
		b = ir.Block{Type: ir.BlockImage, Media: m}
		xset(&b.Extra, xPart, restObject(p.Raw, "type", "image_url", "file_id", "detail"))
		return b
	case "input_file":
		n.Documents = true
		m := &ir.Media{FileID: p.Get("file_id").Str, URL: p.Get("file_url").Str, Title: p.Get("filename").Str}
		if fd := p.Get("file_data").Str; fd != "" {
			if !parseDataURL(m, fd) {
				m.Data = fd
			}
		}
		b = ir.Block{Type: ir.BlockDocument, Media: m}
		xset(&b.Extra, xPart, restObject(p.Raw, "type", "file_id", "file_url", "filename", "file_data"))
		return b
	default:
		b = ir.Block{Type: ir.BlockRaw}
		xset(&b.Extra, xRawPart, json.RawMessage(p.Raw))
		return b
	}
	if typ != defaultTextPart(role) {
		xset(&b.Extra, xPartType, jstr(typ))
	}
	return b
}

func setDataURL(m *ir.Media, u string) {
	if u == "" {
		return
	}
	if !parseDataURL(m, u) {
		m.URL = u
	}
}

// parseDataURL splits "data:<type>;base64,<data>".
func parseDataURL(m *ir.Media, u string) bool {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return false
	}
	mt, data, ok := strings.Cut(rest, ";base64,")
	if !ok || strings.ContainsAny(mt, ";,") {
		return false
	}
	m.MediaType, m.Data = mt, data
	return true
}

func functionCallBlock(it gjson.Result, n *ir.Needs) ir.Block {
	n.FunctionTools = true
	tc := &ir.ToolCall{
		ID: it.Get("call_id").Str, Name: it.Get("name").Str, Kind: ir.ToolFunction,
		Namespace: it.Get("namespace").Str,
	}
	b := ir.Block{Type: ir.BlockToolUse, Tool: tc}
	args := it.Get("arguments")
	if args.Type == gjson.String && args.Str != "" && json.Valid([]byte(args.Str)) {
		tc.Input = json.RawMessage(args.Str)
	} else if args.Exists() {
		xset(&b.Extra, xArguments, json.RawMessage(args.Raw))
	}
	xset(&b.Extra, xItem, itemRest(it, "call_id", "name", "arguments", "namespace"))
	return b
}

func toolOutputBlock(it gjson.Result, n *ir.Needs) ir.Block {
	res := &ir.ToolResult{ToolUseID: it.Get("call_id").Str, Freeform: it.Get("type").Str == itemCustomOutput}
	if res.Freeform {
		n.FreeformTools = true
	}
	b := ir.Block{Type: ir.BlockToolResult, Result: res}
	out := it.Get("output")
	switch {
	case out.IsArray():
		for _, p := range out.Array() {
			pb := partBlock(p, "user", n)
			if pb.Type == ir.BlockImage {
				n.ToolResultImgs = true
			}
			res.Content = append(res.Content, pb)
		}
		xset(&b.Extra, xOutputArray, jtrue)
	case out.Type == gjson.String:
		res.Content = []ir.Block{{Type: ir.BlockText, Text: out.Str}}
	case out.Exists():
		xset(&b.Extra, "output", json.RawMessage(out.Raw))
	}
	xset(&b.Extra, xItem, itemRest(it, "call_id", "output"))
	return b
}

func reasoningBlock(it gjson.Result, n *ir.Needs) ir.Block {
	var op *ir.OpaqueState
	if enc := it.Get("encrypted_content"); enc.Type == gjson.String {
		op = opaqueOf(kindEncrypted, enc)
		n.OpaqueState = true
	}
	summary := it.Get("summary")
	text := joinTexts(summary, partSummaryText)
	var b ir.Block
	content := it.Get("content")
	fromContent := false
	if text == "" {
		if text = joinTexts(content, partReasoningText); text != "" {
			fromContent = true
		}
	}
	switch {
	case text != "":
		b = ir.Block{Type: ir.BlockThinking, Text: text, Opaque: op}
	case op != nil:
		b = ir.Block{Type: ir.BlockOpaque, Opaque: op}
	default:
		return rawBlock(it)
	}
	xset(&b.Extra, xItem, itemRest(it, "summary", "content", "encrypted_content"))
	if len(summary.Array()) > 1 || (summary.Exists() && !summary.IsArray()) {
		xset(&b.Extra, xSummary, json.RawMessage(summary.Raw))
	} else if !summary.Exists() {
		xset(&b.Extra, xSummary, json.RawMessage("null"))
	}
	if content.Exists() {
		xset(&b.Extra, xContent, json.RawMessage(content.Raw))
	}
	if fromContent {
		xset(&b.Extra, xTextSource, jstr("content"))
	}
	return b
}

func joinTexts(arr gjson.Result, typ string) string {
	var parts []string
	for _, p := range arr.Array() {
		if t := p.Get("type").Str; t == typ || t == "" {
			parts = append(parts, p.Get("text").Str)
		}
	}
	return strings.Join(parts, "\n\n")
}
