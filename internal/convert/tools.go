package convert

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/ir"
)

// wrapParam is the single string parameter a wrapped freeform tool takes.
const wrapParam = "input"

var wrapSchema = json.RawMessage(`{"type":"object","properties":{"` + wrapParam +
	`":{"type":"string","description":"The complete raw input for this tool, exactly as it would be written without JSON."}},"required":["` +
	wrapParam + `"],"additionalProperties":false}`)

// stepTools bridges tool kinds the target lacks: freeform tools become
// single-string function tools, namespaced tools are flattened, server tools
// merely offered are removed from the upstream list, deferred loading is
// expanded.
func stepTools(req *ir.Request, needs ir.Needs, c *Context, r *recorder) *MissingError {
	t := c.Target
	var missing, dropped []string
	deferred, wrapped, flattened := false, false, false
	flat := flatNames(req)
	tools := req.Tools[:0:0]
	for _, tool := range req.Tools {
		origin := ToolOrigin{Kind: tool.Kind, Name: tool.Name, Namespace: tool.Namespace}
		switch tool.Kind {
		case ir.ToolFreeform:
			if !t.FreeformTools {
				if !t.FunctionTools {
					missing = append(missing, "freeform_tools")
					break
				}
				wrapTool(&tool)
				wrapped = true
			}
		case ir.ToolServer:
			if !slices.Contains(t.ServerTools, tool.ServerType) {
				if !slices.Contains(needs.ServerToolsRequired, tool.ServerType) {
					dropped = append(dropped, tool.ServerType)
					continue
				}
				missing = append(missing, "server_tool:"+tool.ServerType)
			}
		}
		if tool.Namespace != "" && !t.Namespaces {
			tool.Name, tool.Namespace = flat(origin.Namespace, origin.Name), ""
			flattened = true
		}
		if tool.Name != origin.Name || tool.Kind != origin.Kind {
			c.Tools[tool.Name] = origin
		}
		if tool.DeferLoading && !t.DeferredTools {
			tool.DeferLoading = false
			deferred = true
		}
		tools = append(tools, tool)
	}
	if len(missing) > 0 {
		return &MissingError{Missing: missing}
	}
	req.Tools = tools
	if wrapped {
		r.step("tools: freeform tools wrapped as function tools")
		r.major(lossFreeform)
	}
	if flattened {
		r.step("tools: namespaced tools flattened")
		forceFlatChoice(req, c, r)
	}
	if len(dropped) > 0 {
		r.step("tools: offered server tools the target cannot run removed")
		for _, st := range dropped {
			r.drop("server_tool:" + st)
		}
	}
	if deferred {
		r.step("tools: deferred loading expanded")
		r.drop("defer_loading")
	}
	if rewriteHistory(req, c, flat, r) {
		r.step("tools: freeform history rewritten as function calls")
		r.major(lossFreeform)
	}
	if !c.SameProtocol && serverHistory(req) {
		r.step("tools: server tool history rewritten as text")
		r.drop("server_tool_history")
	}
	return nil
}

// forceFlatChoice points a forced tool_choice at the flattened tool it
// means. The IR choice carries a bare name: a top-level tool of that name
// wins, otherwise the first namespaced tool of that name in tool order.
func forceFlatChoice(req *ir.Request, c *Context, r *recorder) {
	tc := req.ToolChoice
	if tc == nil || tc.Mode != "tool" {
		return
	}
	var match []string
	for _, tool := range req.Tools {
		o := c.Tools[tool.Name] // zero for a tool that kept its name
		switch {
		case o.Namespace == "" && tool.Name == tc.Name:
			return
		case o.Namespace != "" && o.Name == tc.Name:
			match = append(match, tool.Name)
		}
	}
	if len(match) == 0 {
		return
	}
	req.ToolChoice = &ir.ToolChoice{Mode: "tool", Name: match[0]}
	if len(match) > 1 {
		r.note("tool_choice " + tc.Name + " is in several namespaces; forced the first, " + match[0])
	}
}

// flatNames returns the flat name of each namespaced tool of req, for a
// protocol without namespaces. A name is a pure function of the request's
// tools and history, so history and later turns agree. The readable
// "ns__name" form is used unless a top-level tool or another namespaced
// tool would get the same name; then each claimant gets its hashed form.
func flatNames(req *ir.Request) func(ns, name string) string {
	type pair struct{ ns, name string }
	claims := map[string]map[pair]bool{}
	claim := func(flat string, p pair) {
		if claims[flat] == nil {
			claims[flat] = map[pair]bool{}
		}
		claims[flat][p] = true
	}
	top := map[string]bool{}
	see := func(ns, name string) {
		if ns == "" {
			top[name] = true
		} else {
			claim(flatName(ns, name), pair{ns, name})
		}
	}
	for _, t := range req.Tools {
		see(t.Namespace, t.Name)
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == ir.BlockToolUse && b.Tool != nil {
				see(b.Tool.Namespace, b.Tool.Name)
			}
		}
	}
	return func(ns, name string) string {
		f := flatName(ns, name)
		if top[f] || len(claims[f]) > 1 {
			return hashedName(ns, name)
		}
		return f
	}
}

// flatName is the readable flat name of a namespaced tool. Names that would
// break the common tool-name rule are hashed (see hashedName).
func flatName(ns, name string) string {
	s := ns + "__" + name
	if validToolName(s) {
		return s
	}
	return hashedName(ns, name)
}

// hashedName is "ns__name" cleaned to the common tool-name rule, cut, and
// suffixed with a hash of the original pair to stay unique.
func hashedName(ns, name string) string {
	clean := []byte(ns + "__" + name)
	for i, ch := range clean {
		if !nameChar(ch) {
			clean[i] = '_'
		}
	}
	if !isLetter(clean[0]) && clean[0] != '_' {
		clean = append([]byte{'_'}, clean...)
	}
	sum := sha256.Sum256([]byte(ns + "\x00" + name))
	suffix := "_" + hex.EncodeToString(sum[:4])
	if len(clean) > maxToolName-len(suffix) {
		clean = clean[:maxToolName-len(suffix)]
	}
	return string(clean) + suffix
}

// maxToolName and the character set are the strictest tool-name rule the
// protocols share: ^[a-zA-Z_][a-zA-Z0-9_-]{0,63}$.
const maxToolName = 64

func validToolName(s string) bool {
	if s == "" || len(s) > maxToolName || (!isLetter(s[0]) && s[0] != '_') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !nameChar(s[i]) {
			return false
		}
	}
	return true
}

func isLetter(ch byte) bool { return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' }

func nameChar(ch byte) bool {
	return isLetter(ch) || ch >= '0' && ch <= '9' || ch == '_' || ch == '-'
}

func wrapTool(tool *ir.Tool) {
	desc := tool.Description
	if g := grammarText(tool.Grammar); g != "" {
		if desc != "" {
			desc += "\n\n"
		}
		desc += "The `" + wrapParam + "` argument must follow this grammar:\n" + g
	}
	tool.Kind = ir.ToolFunction
	tool.Description = desc
	tool.InputSchema = wrapSchema
	tool.Grammar = nil
}

// grammarText renders a freeform format definition for a description:
// syntax plus definition when the shape is recognizable, verbatim otherwise.
func grammarText(g json.RawMessage) string {
	if len(g) == 0 {
		return ""
	}
	res := gjson.ParseBytes(g)
	if def := res.Get("definition"); def.Type == gjson.String {
		if syn := res.Get("syntax").String(); syn != "" {
			return "(" + syn + ")\n" + def.String()
		}
		return def.String()
	}
	if res.Get("type").String() == "text" && len(res.Map()) == 1 {
		return ""
	}
	return string(g)
}

// rewriteHistory applies the tool renames and wrapping of stepTools to the
// calls in history. It reports whether freeform history was rewritten.
func rewriteHistory(req *ir.Request, c *Context, flat func(ns, name string) string, r *recorder) bool {
	t := c.Target
	wrapped, flattened := false, false
	for i := range req.Messages {
		for j := range req.Messages[i].Content {
			b := &req.Messages[i].Content[j]
			switch {
			case b.Type == ir.BlockToolUse && b.Tool != nil:
				origin := ToolOrigin{Kind: b.Tool.Kind, Name: b.Tool.Name, Namespace: b.Tool.Namespace}
				tc := *b.Tool
				if tc.Kind == ir.ToolFreeform && !t.FreeformTools {
					tc.Kind = ir.ToolFunction
					tc.Input, _ = json.Marshal(map[string]string{wrapParam: tc.Text})
					tc.Text = ""
					wrapped = true
				}
				if tc.Namespace != "" && !t.Namespaces {
					tc.Name, tc.Namespace = flat(origin.Namespace, origin.Name), ""
					flattened = true
				}
				if tc.Name != origin.Name || tc.Kind != origin.Kind {
					c.Tools[tc.Name] = origin
					b.Tool = &tc
				}
			case b.Type == ir.BlockToolResult && b.Result != nil && b.Result.Freeform && !t.FreeformTools:
				res := *b.Result
				res.Freeform = false
				b.Result = &res
				wrapped = true
			}
		}
	}
	if flattened {
		r.step("tools: namespaced history calls flattened")
	}
	return wrapped
}

// serverHistory rewrites server tool blocks for another protocol: a call
// becomes a short text note, a result is dropped. It reports whether any
// block changed.
func serverHistory(req *ir.Request) bool {
	changed := false
	for i := range req.Messages {
		m := &req.Messages[i]
		out := m.Content[:0:0]
		for _, b := range m.Content {
			if b.Type != ir.BlockServerTool {
				out = append(out, b)
				continue
			}
			changed = true
			if b.Tool != nil {
				out = append(out, ir.Block{Type: ir.BlockText, Text: serverCallNote(b.Tool)})
			}
		}
		m.Content = out
	}
	if changed {
		req.Messages = compactMessages(req.Messages)
	}
	return changed
}

// maxNoteInput caps the arguments quoted in a server call note.
const maxNoteInput = 500

func serverCallNote(tc *ir.ToolCall) string {
	in := string(tc.Input)
	if len(in) > maxNoteInput {
		in = in[:maxNoteInput] + "…"
	}
	if in == "" {
		return "(called provider tool " + tc.Name + ")"
	}
	return "(called provider tool " + tc.Name + " with " + in + ")"
}

// unwrapCall turns a function call of a wrapped tool back into a freeform
// call. Input that does not follow the wrapper schema is passed on verbatim.
func unwrapCall(tc *ir.ToolCall, input []byte) {
	tc.Kind = ir.ToolFreeform
	if v := gjson.GetBytes(input, wrapParam); v.Type == gjson.String {
		tc.Text = v.String()
	} else {
		tc.Text = string(input)
	}
	tc.Input = nil
}
