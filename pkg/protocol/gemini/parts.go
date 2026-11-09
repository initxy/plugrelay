package gemini

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/pkg/ir"
)

// Part keys the IR models; anything else on a part is kept in Block.Extra.
var knownPartKeys = map[string]bool{
	"text": true, "thought": true, "thoughtSignature": true,
	"inlineData": true, "fileData": true,
	"functionCall": true, "functionResponse": true,
}

// idState synthesizes and pairs tool call ids within one request or response.
type idState struct {
	salt    string // "" for requests
	n       int
	pending map[string][]string // function name -> unanswered call ids
}

func newIDState(salt string) *idState { return &idState{salt: salt, pending: map[string][]string{}} }

// responseSalt derives a short salt from a response id, or a random one.
func responseSalt(responseID string) string {
	if responseID == "" {
		return strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	}
	sum := sha256.Sum256([]byte(responseID))
	return hex.EncodeToString(sum[:4])
}

func (s *idState) synth(name string) string {
	id := "call_" + name + "_"
	if s.salt != "" {
		id += s.salt + "_"
	}
	id += strconv.Itoa(s.n)
	s.n++
	return id
}

func (s *idState) call(name, id string) string {
	if id == "" {
		id = s.synth(name)
	}
	s.pending[name] = append(s.pending[name], id)
	return id
}

func (s *idState) response(name, id string) string {
	q := s.pending[name]
	if id == "" {
		if len(q) == 0 {
			return s.synth(name)
		}
		s.pending[name] = q[1:]
		return q[0]
	}
	for i, v := range q {
		if v == id {
			s.pending[name] = append(q[:i:i], q[i+1:]...)
			break
		}
	}
	return id
}

// isSyntheticID reports whether id has the shape idState.synth produces.
func isSyntheticID(id, name string) bool {
	rest, ok := strings.CutPrefix(id, "call_"+name+"_")
	if !ok || rest == "" {
		return false
	}
	for _, c := range rest {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c == '_') {
			return false
		}
	}
	return true
}

// nameFromSyntheticID recovers the function name from a synthesized id.
func nameFromSyntheticID(id string) string {
	rest, ok := strings.CutPrefix(id, "call_")
	if !ok {
		return ""
	}
	i := strings.LastIndexByte(rest, '_')
	if i <= 0 || !allIn(rest[i+1:], "0123456789") {
		return ""
	}
	rest = rest[:i]
	if j := strings.LastIndexByte(rest, '_'); j > 0 && len(rest)-j-1 == 8 && allIn(rest[j+1:], "0123456789abcdef") {
		rest = rest[:j]
	}
	return rest
}

func allIn(s, set string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune(set, c) {
			return false
		}
	}
	return true
}

// signatureState wraps a thoughtSignature value as opaque state.
func signatureState(sig string, fromClient bool) *ir.OpaqueState {
	data, _ := json.Marshal(sig)
	return &ir.OpaqueState{
		Kind:   opaqueKind,
		Data:   data,
		Sealed: fromClient && strings.HasPrefix(sig, ir.SealPrefix),
	}
}

// decodeParts turns Gemini parts into IR blocks. fromClient marks sealed
// signatures.
func decodeParts(parts []json.RawMessage, ids *idState, fromClient bool, needs *ir.Needs) ([]ir.Block, error) {
	var out []ir.Block
	for _, raw := range parts {
		b, err := decodePart(raw, ids, fromClient, needs)
		if err != nil {
			return nil, err
		}
		if b != nil {
			out = append(out, *b)
		}
	}
	return out, nil
}

func decodePart(raw json.RawMessage, ids *idState, fromClient bool, needs *ir.Needs) (*ir.Block, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, ir.Errorf(ir.ErrInvalidRequest, "invalid part: %v", err)
	}
	var b ir.Block
	var sig *ir.OpaqueState
	if v, ok := m["thoughtSignature"]; ok {
		var s string
		if json.Unmarshal(v, &s) == nil && s != "" {
			sig = signatureState(s, fromClient)
		}
	}
	var thought bool
	if v, ok := m["thought"]; ok {
		_ = json.Unmarshal(v, &thought)
	}
	var text string
	_, hasText := m["text"]
	if hasText {
		_ = json.Unmarshal(m["text"], &text)
	}

	switch {
	case m["functionCall"] != nil:
		var fc struct {
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal(m["functionCall"], &fc); err != nil {
			return nil, ir.Errorf(ir.ErrInvalidRequest, "invalid functionCall: %v", err)
		}
		args := compactJSON(fc.Args)
		b = ir.Block{Type: ir.BlockToolUse, Tool: &ir.ToolCall{
			ID: ids.call(fc.Name, fc.ID), Name: fc.Name, Kind: ir.ToolFunction, Input: args,
		}}
		keepExtra(&b, m["functionCall"], "functionCall", "id", "name", "args")
	case m["functionResponse"] != nil:
		var fr struct {
			ID       string            `json:"id"`
			Name     string            `json:"name"`
			Response json.RawMessage   `json:"response"`
			Parts    []json.RawMessage `json:"parts"`
		}
		if err := json.Unmarshal(m["functionResponse"], &fr); err != nil {
			return nil, ir.Errorf(ir.ErrInvalidRequest, "invalid functionResponse: %v", err)
		}
		res := &ir.ToolResult{ToolUseID: ids.response(fr.Name, fr.ID)}
		txt, isErr := responseText(fr.Response)
		res.IsError = isErr
		if txt != "" {
			res.Content = append(res.Content, ir.Block{Type: ir.BlockText, Text: txt})
		}
		inner, err := decodeParts(fr.Parts, ids, fromClient, needs)
		if err != nil {
			return nil, err
		}
		for _, ib := range inner {
			if ib.Type == ir.BlockImage && needs != nil {
				needs.ToolResultImgs = true
			}
		}
		res.Content = append(res.Content, inner...)
		b = ir.Block{Type: ir.BlockToolResult, Result: res}
		if len(fr.Response) > 0 {
			b.Extra.Set(Name, "response", compactJSON(fr.Response))
		}
		keepExtra(&b, m["functionResponse"], "functionResponse", "id", "name", "response", "parts")
	case m["inlineData"] != nil || m["fileData"] != nil:
		var d struct {
			MimeType string `json:"mimeType"`
			Data     string `json:"data"`
			FileURI  string `json:"fileUri"`
		}
		src := m["inlineData"]
		if src == nil {
			src = m["fileData"]
		}
		if err := json.Unmarshal(src, &d); err != nil {
			return nil, ir.Errorf(ir.ErrInvalidRequest, "invalid media part: %v", err)
		}
		media := &ir.Media{MediaType: d.MimeType, Data: d.Data, URL: d.FileURI}
		switch {
		case strings.HasPrefix(d.MimeType, "image/"):
			b = ir.Block{Type: ir.BlockImage, Media: media}
			if needs != nil {
				needs.Images = true
			}
		case strings.HasPrefix(d.MimeType, "application/"), strings.HasPrefix(d.MimeType, "text/"):
			b = ir.Block{Type: ir.BlockDocument, Media: media}
			if needs != nil {
				needs.Documents = true
			}
		default:
			b = rawBlock(raw)
		}
	case hasText && (text != "" || sig == nil):
		if text == "" {
			return nil, nil
		}
		b = ir.Block{Type: ir.BlockText, Text: text}
		if thought {
			b.Type = ir.BlockThinking
		}
	case sig != nil && onlyKeys(m, "text", "thought", "thoughtSignature"):
		b = ir.Block{Type: ir.BlockOpaque, Opaque: sig}
		if needs != nil {
			needs.OpaqueState = true
		}
		return &b, nil
	default:
		b = rawBlock(raw)
		return &b, nil
	}
	if b.Type != ir.BlockRaw {
		b.Opaque = sig
		if sig != nil && needs != nil {
			needs.OpaqueState = true
		}
		for k, v := range m {
			if !knownPartKeys[k] {
				b.Extra.Set(Name, k, v)
			}
		}
	}
	return &b, nil
}

func rawBlock(raw json.RawMessage) ir.Block {
	b := ir.Block{Type: ir.BlockRaw}
	b.Extra.Set(Name, "part", compactJSON(raw))
	return b
}

func onlyKeys(m map[string]json.RawMessage, keys ...string) bool {
	for k := range m {
		ok := false
		for _, want := range keys {
			ok = ok || k == want
		}
		if !ok {
			return false
		}
	}
	return true
}

// keepExtra stores the unmodelled fields of a nested object under prefix.
func keepExtra(b *ir.Block, obj json.RawMessage, prefix string, known ...string) {
	gjson.ParseBytes(obj).ForEach(func(k, v gjson.Result) bool {
		for _, kn := range known {
			if k.String() == kn {
				return true
			}
		}
		b.Extra.Set(Name, prefix+"."+k.String(), json.RawMessage(v.Raw))
		return true
	})
}

// responseText renders a functionResponse.response as tool result text.
func responseText(resp json.RawMessage) (string, bool) {
	r := gjson.ParseBytes(resp)
	if !r.Exists() {
		return "", false
	}
	isErr := r.Get("error").Exists()
	if r.IsObject() {
		n := 0
		r.ForEach(func(_, _ gjson.Result) bool { n++; return true })
		if n == 1 {
			for _, k := range []string{"output", "result", "content", "error"} {
				if v := r.Get(k); v.Type == gjson.String {
					return v.String(), isErr
				}
			}
		}
	}
	return string(compactJSON(resp)), isErr
}

func compactJSON(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("{}")
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return raw
	}
	return buf.Bytes()
}

// encodeTarget says who will read encoded parts.
type encodeTarget int

const (
	toUpstream encodeTarget = iota
	toClient
)

// signatureFor returns the thoughtSignature to write for opaque state, or "".
// Clients get sealed state of any kind; upstreams only their own.
func signatureFor(o *ir.OpaqueState, t encodeTarget) string {
	if o == nil {
		return ""
	}
	if o.Sealed && t == toUpstream {
		return ""
	}
	if !o.Sealed && o.Kind != opaqueKind {
		return ""
	}
	var s string
	if json.Unmarshal(o.Data, &s) != nil {
		return ""
	}
	return s
}

// toolNames maps tool_use ids in history to their function names.
func toolNames(msgs []ir.Message) map[string]string {
	names := map[string]string{}
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse && b.Tool != nil {
				names[b.Tool.ID] = b.Tool.Name
			}
		}
	}
	return names
}

// encodeBlocks renders IR blocks as Gemini parts.
func encodeBlocks(blocks []ir.Block, names map[string]string, t encodeTarget) []json.RawMessage {
	var out []json.RawMessage
	for i := range blocks {
		if p := encodeBlock(&blocks[i], names, t); p != nil {
			out = append(out, p)
		}
	}
	return out
}

func encodeBlock(b *ir.Block, names map[string]string, t encodeTarget) json.RawMessage {
	var p []byte
	switch b.Type {
	case ir.BlockText:
		p = setJSON([]byte("{}"), "text", b.Text)
	case ir.BlockThinking:
		p = setJSON([]byte("{}"), "text", b.Text)
		p = setJSON(p, "thought", true)
	case ir.BlockImage, ir.BlockDocument:
		p = encodeMedia(b.Media)
		if p == nil {
			return nil
		}
	case ir.BlockToolUse:
		if b.Tool == nil || b.Tool.Kind == ir.ToolServer {
			return nil
		}
		fc := []byte("{}")
		if !isSyntheticID(b.Tool.ID, b.Tool.Name) && b.Tool.ID != "" {
			fc = setJSON(fc, "id", b.Tool.ID)
		}
		fc = setJSON(fc, "name", b.Tool.Name)
		args := b.Tool.Input
		if b.Tool.Kind == ir.ToolFreeform {
			args, _ = json.Marshal(map[string]string{"input": b.Tool.Text})
		}
		fc = setRaw(fc, "args", objectOr(args))
		fc = restoreNested(fc, b.Extra, "functionCall")
		p = setRaw([]byte("{}"), "functionCall", fc)
	case ir.BlockToolResult:
		if b.Result == nil {
			return nil
		}
		p = setRaw([]byte("{}"), "functionResponse", encodeResult(b, names, t))
	case ir.BlockOpaque:
		sig := signatureFor(b.Opaque, t)
		if sig == "" {
			return nil
		}
		p = setJSON([]byte("{}"), "text", "")
		return setJSON(p, "thoughtSignature", sig)
	case ir.BlockRaw:
		if raw, ok := b.Extra.Get(Name, "part"); ok {
			return raw
		}
		return nil
	default:
		return nil
	}
	if b.Extra.Origin == Name {
		for k, v := range b.Extra.Fields {
			if !strings.Contains(k, ".") && k != "response" && k != "part" {
				p = setRaw(p, k, v)
			}
		}
	}
	if sig := signatureFor(b.Opaque, t); sig != "" {
		p = setJSON(p, "thoughtSignature", sig)
	}
	return p
}

func encodeResult(b *ir.Block, names map[string]string, t encodeTarget) []byte {
	r := b.Result
	name := names[r.ToolUseID]
	if name == "" {
		name = nameFromSyntheticID(r.ToolUseID)
	}
	if name == "" {
		name = r.ToolUseID
	}
	fr := []byte("{}")
	if r.ToolUseID != "" && !isSyntheticID(r.ToolUseID, name) {
		fr = setJSON(fr, "id", r.ToolUseID)
	}
	fr = setJSON(fr, "name", name)
	var texts []string
	var media []ir.Block
	for _, c := range r.Content {
		switch c.Type {
		case ir.BlockText:
			texts = append(texts, c.Text)
		case ir.BlockImage, ir.BlockDocument:
			media = append(media, c)
		}
	}
	if raw, ok := b.Extra.Get(Name, "response"); ok {
		fr = setRaw(fr, "response", raw)
	} else {
		key := "output"
		if r.IsError {
			key = "error"
		}
		fr = setRaw(fr, "response", setJSON([]byte("{}"), key, strings.Join(texts, "\n")))
	}
	if parts := encodeBlocks(media, names, t); len(parts) > 0 {
		fr = setRaw(fr, "parts", rawArray(parts))
	}
	return restoreNested(fr, b.Extra, "functionResponse")
}

func encodeMedia(m *ir.Media) []byte {
	if m == nil {
		return nil
	}
	data, mime := m.Data, m.MediaType
	if data == "" && strings.HasPrefix(m.URL, "data:") {
		head, payload, ok := strings.Cut(strings.TrimPrefix(m.URL, "data:"), ",")
		if ok && strings.HasSuffix(head, ";base64") {
			data = payload
			if mime == "" {
				mime = strings.TrimSuffix(head, ";base64")
			}
		}
	}
	if data != "" {
		d := setJSON([]byte("{}"), "mimeType", mime)
		return setRaw([]byte("{}"), "inlineData", setJSON(d, "data", data))
	}
	uri := m.URL
	if uri == "" {
		uri = m.FileID
	}
	if uri == "" {
		return nil
	}
	d := []byte("{}")
	if mime != "" {
		d = setJSON(d, "mimeType", mime)
	}
	return setRaw([]byte("{}"), "fileData", setJSON(d, "fileUri", uri))
}

// restoreNested writes back Extra fields stored as "<prefix>.<key>".
func restoreNested(obj []byte, e ir.Extra, prefix string) []byte {
	if e.Origin != Name {
		return obj
	}
	for k, v := range e.Fields {
		if key, ok := strings.CutPrefix(k, prefix+"."); ok {
			obj = setRaw(obj, key, v)
		}
	}
	return obj
}

func objectOr(raw json.RawMessage) json.RawMessage {
	if gjson.ParseBytes(raw).IsObject() {
		return raw
	}
	return json.RawMessage("{}")
}

func rawArray[T ~[]byte](items []T) []byte {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, it := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(it)
	}
	b.WriteByte(']')
	return b.Bytes()
}

// setJSON and setRaw wrap sjson for keys that never contain path syntax.
func setJSON(obj []byte, key string, v any) []byte {
	out, err := sjson.SetBytes(obj, escapeKey(key), v)
	if err != nil {
		return obj
	}
	return out
}

func setRaw(obj []byte, key string, raw []byte) []byte {
	out, err := sjson.SetRawBytes(obj, escapeKey(key), raw)
	if err != nil {
		return obj
	}
	return out
}

var keyEscaper = strings.NewReplacer(".", `\.`, "*", `\*`, "?", `\?`, "|", `\|`, "#", `\#`, "@", `\@`)

func escapeKey(k string) string { return keyEscaper.Replace(k) }
