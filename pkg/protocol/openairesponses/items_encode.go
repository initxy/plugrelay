package openairesponses

import (
	"encoding/json"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/tidwall/gjson"
)

// itemWriter renders IR blocks as wire items. In output mode (a response to
// a client) every item gets an id and a final status.
type itemWriter struct {
	output bool
	newID  func(prefix string) string

	items    []json.RawMessage
	cur      *object // open message item
	parts    []json.RawMessage
	role     string
	asString bool // render the open item's single text part as a string
}

func (w *itemWriter) flush() {
	if w.cur == nil {
		return
	}
	if w.asString && len(w.parts) == 1 {
		w.cur.set("content", jstr(gjson.GetBytes(w.parts[0], "text").Str))
	} else {
		w.cur.set("content", jarray(w.parts))
	}
	w.items = append(w.items, w.cur.raw())
	w.cur, w.parts = nil, nil
}

func (w *itemWriter) itemID(rest json.RawMessage, prefix string) string {
	if id := gjson.GetBytes(rest, "id").Str; id != "" {
		return id
	}
	if w.output && w.newID != nil {
		return w.newID(prefix)
	}
	return ""
}

// start begins a non-message item with type and, in output mode, an id.
func (w *itemWriter) start(typ string, rest json.RawMessage, prefix string) *object {
	w.flush()
	o := newObject()
	o.str("type", typ)
	if id := w.itemID(rest, prefix); id != "" {
		o.str("id", id)
	}
	return o
}

func (w *itemWriter) finish(o *object, rest json.RawMessage) {
	if w.output {
		o.str("status", "completed")
	}
	o.merge(rest)
	w.items = append(w.items, o.raw())
}

// add renders one block; dflt is the message role implied by its side.
func (w *itemWriter) add(b ir.Block, dflt string) {
	rest, hasItem := xget(&b.Extra, xItem)
	switch b.Type {
	case ir.BlockText, ir.BlockImage, ir.BlockDocument:
		w.addPart(b, dflt, rest, hasItem)
	case ir.BlockRaw:
		if _, ok := xget(&b.Extra, xRawPart); ok {
			w.addPart(b, dflt, rest, hasItem)
		} else if raw, ok := xget(&b.Extra, xRaw); ok {
			w.flush()
			w.items = append(w.items, raw)
		}
	case ir.BlockToolUse:
		w.addToolUse(b, rest)
	case ir.BlockToolResult:
		w.addToolResult(b, rest)
	case ir.BlockThinking:
		w.addReasoning(b, b.Text, rest)
	case ir.BlockOpaque:
		op := b.Opaque
		switch {
		case op == nil:
		case op.Kind == kindCompaction && !op.Sealed:
			o := w.start(itemCompaction, rest, "cmp_")
			o.set("encrypted_content", op.Data)
			o.merge(rest)
			w.items = append(w.items, o.raw())
		case op.Sealed || op.Kind == kindEncrypted:
			w.addReasoning(b, "", rest)
		}
	}
}

func (w *itemWriter) addPart(b ir.Block, dflt string, rest json.RawMessage, hasItem bool) {
	role := dflt
	if r := xstring(&b.Extra, xRole); r != "" {
		role = r
	}
	if w.cur == nil || hasItem || role != w.role {
		w.flush()
		w.cur, w.role = newObject(), role
		if w.output || !xflag(&b.Extra, xUntyped) {
			w.cur.str("type", itemMessage)
		}
		if w.output {
			w.cur.str("id", w.itemID(rest, "msg_"))
			w.cur.str("status", "completed")
		}
		w.cur.str("role", role)
		w.cur.merge(rest)
		w.asString = xflag(&b.Extra, xContentString)
	}
	part := partJSON(b, role, w.output)
	if part == nil {
		return
	}
	if !isPlainText(part) {
		w.asString = false
	}
	w.parts = append(w.parts, part)
}

func isPlainText(part json.RawMessage) bool {
	r := gjson.ParseBytes(part)
	n := 0
	r.ForEach(func(_, _ gjson.Result) bool { n++; return true })
	return n == 2 && r.Get("text").Exists()
}

// partJSON renders a content part.
func partJSON(b ir.Block, role string, output bool) json.RawMessage {
	rest, _ := xget(&b.Extra, xPart)
	o := newObject()
	switch b.Type {
	case ir.BlockText:
		typ := xstring(&b.Extra, xPartType)
		if typ == "" {
			typ = defaultTextPart(role)
		}
		o.str("type", typ)
		if typ == "refusal" {
			o.str("refusal", b.Text)
		} else {
			o.str("text", b.Text)
			if output && typ == "output_text" && !gjson.GetBytes(rest, "annotations").Exists() {
				o.set("annotations", json.RawMessage("[]"))
			}
		}
	case ir.BlockImage:
		o.str("type", "input_image")
		if m := b.Media; m != nil {
			if m.Detail != "" {
				o.str("detail", m.Detail)
			}
			if m.FileID != "" {
				o.str("file_id", m.FileID)
			}
			if u := mediaURL(m); u != "" {
				o.str("image_url", u)
			}
		}
	case ir.BlockDocument:
		o.str("type", "input_file")
		if m := b.Media; m != nil {
			if m.FileID != "" {
				o.str("file_id", m.FileID)
			}
			if m.Title != "" {
				o.str("filename", m.Title)
			}
			if m.Data != "" {
				if m.MediaType != "" {
					o.str("file_data", "data:"+m.MediaType+";base64,"+m.Data)
				} else {
					o.str("file_data", m.Data)
				}
			}
			if m.URL != "" {
				o.str("file_url", m.URL)
			}
		}
	case ir.BlockRaw:
		p, _ := xget(&b.Extra, xRawPart)
		return p
	default:
		return nil
	}
	o.merge(rest)
	return o.raw()
}

func mediaURL(m *ir.Media) string {
	if m.Data != "" {
		mt := m.MediaType
		if mt == "" {
			mt = "application/octet-stream"
		}
		return "data:" + mt + ";base64," + m.Data
	}
	return m.URL
}

// toolArguments returns the wire arguments string of a function call.
func toolArguments(b ir.Block) json.RawMessage {
	if a, ok := xget(&b.Extra, xArguments); ok {
		return a
	}
	if b.Tool != nil && len(b.Tool.Input) > 0 {
		return jstr(string(b.Tool.Input))
	}
	return jstr("{}")
}

func (w *itemWriter) addToolUse(b ir.Block, rest json.RawMessage) {
	tc := b.Tool
	if tc == nil {
		return
	}
	var o *object
	if tc.Kind == ir.ToolFreeform {
		o = w.start(itemCustomCall, rest, "ctc_")
		o.str("call_id", tc.ID)
		o.str("name", tc.Name)
		o.str("input", tc.Text)
	} else {
		o = w.start(itemFunctionCall, rest, "fc_")
		o.str("call_id", tc.ID)
		o.str("name", tc.Name)
		o.set("arguments", toolArguments(b))
	}
	if tc.Namespace != "" {
		o.str("namespace", tc.Namespace)
	}
	w.finish(o, rest)
}

func (w *itemWriter) addToolResult(b ir.Block, rest json.RawMessage) {
	res := b.Result
	if res == nil {
		return
	}
	typ := itemFunctionOutput
	if res.Freeform {
		typ = itemCustomOutput
	}
	w.flush()
	o := newObject()
	o.str("type", typ)
	o.str("call_id", res.ToolUseID)
	o.set("output", toolOutput(b))
	o.merge(rest)
	w.items = append(w.items, o.raw())
}

func toolOutput(b ir.Block) json.RawMessage {
	if v, ok := xget(&b.Extra, "output"); ok {
		return v
	}
	array := xflag(&b.Extra, xOutputArray)
	var texts []string
	for _, c := range b.Result.Content {
		if c.Type != ir.BlockText {
			array = true
			break
		}
		texts = append(texts, c.Text)
	}
	if !array {
		return jstr(strings.Join(texts, "\n"))
	}
	parts := []json.RawMessage{}
	for _, c := range b.Result.Content {
		if p := partJSON(c, "user", false); p != nil {
			parts = append(parts, p)
		}
	}
	return jarray(parts)
}

func (w *itemWriter) addReasoning(b ir.Block, text string, rest json.RawMessage) {
	o := w.start(itemReasoning, rest, "rs_")
	fromContent := xstring(&b.Extra, xTextSource) == "content"
	summary := json.RawMessage("[]")
	if v, ok := xget(&b.Extra, xSummary); ok && (fromContent || string(v) == "null" || joinTexts(gjson.ParseBytes(v), partSummaryText) == text) {
		summary = v
	} else if text != "" && !fromContent {
		p := newObject()
		p.str("type", partSummaryText)
		p.str("text", text)
		summary = jarray([]json.RawMessage{p.raw()})
	}
	if string(summary) != "null" {
		o.set("summary", summary)
	}
	if v, ok := xget(&b.Extra, xContent); ok && (!fromContent || joinTexts(gjson.ParseBytes(v), partReasoningText) == text) {
		o.set("content", v)
	} else if fromContent {
		p := newObject()
		p.str("type", partReasoningText)
		p.str("text", text)
		o.set("content", jarray([]json.RawMessage{p.raw()}))
	}
	if op := b.Opaque; op != nil && (op.Sealed || op.Kind == kindEncrypted) && len(op.Data) > 0 {
		o.set("encrypted_content", op.Data)
	}
	o.merge(rest)
	w.items = append(w.items, o.raw())
}

// encodeMessages renders IR history as an input array.
func encodeMessages(msgs []ir.Message, w *itemWriter) {
	for _, m := range msgs {
		dflt := defaultRole(m.Role)
		for _, b := range m.Content {
			w.add(b, dflt)
		}
		w.flush()
	}
}
