package gemini

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// Top-level request keys the IR models; everything else goes to Extra.
var knownRequestKeys = map[string]bool{
	"contents": true, "systemInstruction": true, "tools": true,
	"toolConfig": true, "generationConfig": true, "model": true,
}

// generationConfig keys the IR models.
var knownGenKeys = []string{"maxOutputTokens", "temperature", "topP", "topK", "stopSequences", "thinkingConfig"}

// Request Extra keys owned by this codec (not top-level wire fields).
const (
	extraGenRest         = "generationConfig"
	extraToolConfig      = "toolConfig"
	extraIncludeThoughts = "thinkingConfig.includeThoughts"
)

type wireContent struct {
	Role  string            `json:"role"`
	Parts []json.RawMessage `json:"parts"`
}

type wireRequest struct {
	Contents          []wireContent     `json:"contents"`
	SystemInstruction *wireContent      `json:"systemInstruction"`
	Tools             []json.RawMessage `json:"tools"`
	ToolConfig        json.RawMessage   `json:"toolConfig"`
	GenerationConfig  json.RawMessage   `json:"generationConfig"`
}

// DecodeRequest implements protocol.Codec. A countTokens body may wrap the
// request in generateContentRequest.
func (Codec) DecodeRequest(_ *http.Request, in protocol.Inbound, body []byte) (*ir.Request, ir.Needs, error) {
	body = requestBody(body)
	var needs ir.Needs
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, needs, ir.Errorf(ir.ErrInvalidRequest, "invalid JSON body: %v", err)
	}
	req := &ir.Request{Model: in.Model, Stream: in.Stream}
	ids := newIDState("")

	if w.SystemInstruction != nil {
		sys, err := decodeParts(w.SystemInstruction.Parts, ids, true, &needs)
		if err != nil {
			return nil, needs, err
		}
		req.System = sys
	}
	for _, c := range w.Contents {
		blocks, err := decodeParts(c.Parts, ids, true, &needs)
		if err != nil {
			return nil, needs, err
		}
		role := ir.RoleUser
		if c.Role == "model" {
			role = ir.RoleAssistant
		}
		req.Messages = append(req.Messages, ir.Message{Role: role, Content: blocks})
	}

	for _, t := range w.Tools {
		tools, err := decodeTool(t)
		if err != nil {
			return nil, needs, err
		}
		for _, tool := range tools {
			if tool.Kind == ir.ToolServer {
				needs.ServerTools = append(needs.ServerTools, tool.ServerType)
			} else {
				needs.FunctionTools = true
			}
		}
		req.Tools = append(req.Tools, tools...)
	}
	if len(w.ToolConfig) > 0 {
		req.ToolChoice = decodeToolChoice(w.ToolConfig)
		req.Extra.Set(Name, extraToolConfig, compactJSON(w.ToolConfig))
	}
	if err := decodeGenConfig(req, w.GenerationConfig); err != nil {
		return nil, needs, err
	}
	gjson.ParseBytes(body).ForEach(func(k, v gjson.Result) bool {
		if !knownRequestKeys[k.String()] {
			req.Extra.Set(Name, k.String(), json.RawMessage(v.Raw))
		}
		return true
	})
	needs.ServerToolsRequired = req.RequiredServerTools()
	needs.Reasoning = req.Reasoning != nil && req.Reasoning.Enabled
	needs.InputTokensEst = estimateTokens(req)
	return req, needs, nil
}

func decodeTool(raw json.RawMessage) ([]ir.Tool, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, ir.Errorf(ir.ErrInvalidRequest, "invalid tool: %v", err)
	}
	var out []ir.Tool
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		if k == "functionDeclarations" {
			var decls []json.RawMessage
			if err := json.Unmarshal(v, &decls); err != nil {
				return nil, ir.Errorf(ir.ErrInvalidRequest, "invalid functionDeclarations: %v", err)
			}
			for _, d := range decls {
				out = append(out, decodeDecl(d))
			}
			continue
		}
		st := serverToolTypes[k]
		if st == "" {
			st = snakeCase(k)
		}
		t := ir.Tool{Kind: ir.ToolServer, Name: st, ServerType: st}
		if c := compactJSON(v); string(c) != "{}" {
			t.Extra.Set(Name, "config", c)
		}
		out = append(out, t)
	}
	return out, nil
}

func decodeDecl(d json.RawMessage) ir.Tool {
	g := gjson.ParseBytes(d)
	t := ir.Tool{Kind: ir.ToolFunction, Name: g.Get("name").String(), Description: g.Get("description").String()}
	if s := g.Get("parametersJsonSchema"); s.Exists() {
		t.InputSchema = json.RawMessage(s.Raw)
		t.Extra.Set(Name, "schemaField", json.RawMessage(`"parametersJsonSchema"`))
	} else if s := g.Get("parameters"); s.Exists() {
		t.InputSchema = json.RawMessage(s.Raw)
	}
	g.ForEach(func(k, v gjson.Result) bool {
		switch k.String() {
		case "name", "description", "parameters", "parametersJsonSchema":
		default:
			t.Extra.Set(Name, k.String(), json.RawMessage(v.Raw))
		}
		return true
	})
	return t
}

func decodeToolChoice(raw json.RawMessage) *ir.ToolChoice {
	fc := gjson.GetBytes(raw, "functionCallingConfig")
	if !fc.Exists() {
		return nil
	}
	tc := &ir.ToolChoice{}
	switch strings.ToUpper(fc.Get("mode").String()) {
	case "NONE":
		tc.Mode = "none"
	case "ANY", "VALIDATED":
		tc.Mode = "any"
		if names := fc.Get("allowedFunctionNames").Array(); len(names) == 1 {
			tc.Mode, tc.Name = "tool", names[0].String()
		}
	default:
		tc.Mode = "auto"
	}
	return tc
}

func decodeGenConfig(req *ir.Request, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	g := gjson.ParseBytes(raw)
	if !g.IsObject() {
		return ir.Errorf(ir.ErrInvalidRequest, "generationConfig must be an object")
	}
	if v := g.Get("maxOutputTokens"); v.Exists() {
		n := int(v.Int())
		req.MaxOutputTokens = &n
	}
	if v := g.Get("temperature"); v.Exists() {
		f := v.Float()
		req.Temperature = &f
	}
	if v := g.Get("topP"); v.Exists() {
		f := v.Float()
		req.TopP = &f
	}
	if v := g.Get("topK"); v.Exists() {
		n := int(v.Int())
		req.TopK = &n
	}
	for _, s := range g.Get("stopSequences").Array() {
		req.Stop = append(req.Stop, s.String())
	}
	if tc := g.Get("thinkingConfig"); tc.Exists() {
		r := &ir.Reasoning{}
		budget := tc.Get("thinkingBudget")
		level := tc.Get("thinkingLevel")
		switch {
		case budget.Exists() && budget.Int() == 0:
			b := 0
			r.Budget = &b
		case budget.Exists() && budget.Int() > 0:
			b := int(budget.Int())
			r.Enabled, r.Budget = true, &b
		case budget.Exists():
			r.Enabled = true // -1: dynamic budget
		}
		if level.Exists() {
			r.Enabled, r.Effort = true, strings.ToLower(level.String())
		}
		if inc := tc.Get("includeThoughts"); inc.Exists() {
			req.Extra.Set(Name, extraIncludeThoughts, json.RawMessage(inc.Raw))
			if !budget.Exists() && !level.Exists() {
				r.Enabled = inc.Bool()
			}
		}
		req.Reasoning = r
	}
	rest := []byte(raw)
	for _, k := range knownGenKeys {
		rest, _ = sjson.DeleteBytes(rest, k)
	}
	if c := compactJSON(rest); string(c) != "{}" {
		req.Extra.Set(Name, extraGenRest, c)
	}
	return nil
}

// EncodeRequest implements protocol.Codec.
func (Codec) EncodeRequest(req *ir.Request) ([]byte, error) {
	out := []byte("{}")
	names := toolNames(req.Messages)
	contents := make([]json.RawMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		parts := encodeBlocks(m.Content, names, toUpstream)
		if len(parts) == 0 {
			continue
		}
		role := "user"
		if m.Role == ir.RoleAssistant {
			role = "model"
		}
		c := setJSON([]byte("{}"), "role", role)
		contents = append(contents, setRaw(c, "parts", rawArray(parts)))
	}
	out = setRaw(out, "contents", rawArray(contents))
	if parts := encodeBlocks(req.System, names, toUpstream); len(parts) > 0 {
		out = setRaw(out, "systemInstruction", setRaw([]byte("{}"), "parts", rawArray(parts)))
	}
	if tools := encodeTools(req.Tools); tools != nil {
		out = setRaw(out, "tools", tools)
	}
	if tc := encodeToolConfig(req); tc != nil {
		out = setRaw(out, "toolConfig", tc)
	}
	if gc := encodeGenConfig(req); gc != nil {
		out = setRaw(out, "generationConfig", gc)
	}
	if req.Extra.Origin == Name {
		keys := make([]string, 0, len(req.Extra.Fields))
		for k := range req.Extra.Fields {
			if !knownRequestKeys[k] && !strings.Contains(k, ".") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = setRaw(out, k, req.Extra.Fields[k])
		}
	}
	return out, nil
}

func encodeTools(tools []ir.Tool) []byte {
	var decls, entries []json.RawMessage
	for _, t := range tools {
		switch t.Kind {
		case ir.ToolFunction, ir.ToolFreeform:
			d := setJSON([]byte("{}"), "name", t.Name)
			if t.Description != "" {
				d = setJSON(d, "description", t.Description)
			}
			field := "parameters"
			if v, ok := t.Extra.Get(Name, "schemaField"); ok {
				_ = json.Unmarshal(v, &field)
			}
			schema := t.InputSchema
			if t.Kind == ir.ToolFreeform {
				schema = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}`)
			}
			if len(schema) > 0 {
				d = setRaw(d, field, schema)
			}
			if t.Extra.Origin == Name {
				for k, v := range t.Extra.Fields {
					if k != "schemaField" {
						d = setRaw(d, k, v)
					}
				}
			}
			decls = append(decls, d)
		case ir.ToolServer:
			cfg := json.RawMessage("{}")
			if v, ok := t.Extra.Get(Name, "config"); ok {
				cfg = v
			}
			entries = append(entries, setRaw([]byte("{}"), camelCase(t.ServerType), cfg))
		}
	}
	if len(decls) > 0 {
		entries = append([]json.RawMessage{setRaw([]byte("{}"), "functionDeclarations", rawArray(decls))}, entries...)
	}
	if len(entries) == 0 {
		return nil
	}
	return rawArray(entries)
}

func encodeToolConfig(req *ir.Request) []byte {
	raw, hasRaw := req.Extra.Get(Name, extraToolConfig)
	if hasRaw && toolChoiceEqual(decodeToolChoice(raw), req.ToolChoice) {
		return raw
	}
	if req.ToolChoice == nil {
		return nil
	}
	fc := []byte("{}")
	switch req.ToolChoice.Mode {
	case "none":
		fc = setJSON(fc, "mode", "NONE")
	case "any":
		fc = setJSON(fc, "mode", "ANY")
	case "tool":
		fc = setJSON(fc, "mode", "ANY")
		fc = setJSON(fc, "allowedFunctionNames", []string{req.ToolChoice.Name})
	default:
		fc = setJSON(fc, "mode", "AUTO")
	}
	return setRaw([]byte("{}"), "functionCallingConfig", fc)
}

func toolChoiceEqual(a, b *ir.ToolChoice) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func encodeGenConfig(req *ir.Request) []byte {
	gc := []byte("{}")
	if v, ok := req.Extra.Get(Name, extraGenRest); ok {
		gc = append([]byte(nil), v...)
	}
	if req.MaxOutputTokens != nil {
		gc = setJSON(gc, "maxOutputTokens", *req.MaxOutputTokens)
	}
	if req.Temperature != nil {
		gc = setJSON(gc, "temperature", *req.Temperature)
	}
	if req.TopP != nil {
		gc = setJSON(gc, "topP", *req.TopP)
	}
	if req.TopK != nil {
		gc = setJSON(gc, "topK", *req.TopK)
	}
	if len(req.Stop) > 0 {
		gc = setJSON(gc, "stopSequences", req.Stop)
	}
	if tc := encodeThinking(req); tc != nil {
		gc = setRaw(gc, "thinkingConfig", tc)
	}
	if string(gc) == "{}" {
		return nil
	}
	return gc
}

func encodeThinking(req *ir.Request) []byte {
	r := req.Reasoning
	inc, hasInc := req.Extra.Get(Name, extraIncludeThoughts)
	if r == nil {
		if hasInc {
			return setRaw([]byte("{}"), "includeThoughts", inc)
		}
		return nil
	}
	tc := []byte("{}")
	switch {
	case r.Budget != nil && !r.Enabled:
		tc = setJSON(tc, "thinkingBudget", 0)
	case r.Budget != nil:
		tc = setJSON(tc, "thinkingBudget", *r.Budget)
	case r.Effort != "" && r.Enabled:
		tc = setJSON(tc, "thinkingLevel", r.Effort)
	case hasInc:
	case !r.Enabled:
		tc = setJSON(tc, "thinkingBudget", 0)
	default:
		tc = setJSON(tc, "thinkingBudget", -1)
	}
	switch {
	case hasInc:
		tc = setRaw(tc, "includeThoughts", inc)
	case r.Enabled:
		tc = setJSON(tc, "includeThoughts", true)
	}
	return tc
}

func snakeCase(s string) string {
	var b strings.Builder
	for i, c := range s {
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			c += 'a' - 'A'
		}
		b.WriteRune(c)
	}
	return b.String()
}

func camelCase(s string) string {
	for k, v := range serverToolTypes {
		if v == s {
			return k
		}
	}
	var b strings.Builder
	up := false
	for _, c := range s {
		if c == '_' {
			up = true
			continue
		}
		if up && c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		up = false
		b.WriteRune(c)
	}
	return b.String()
}
