package convert

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/internal/seal"
	"github.com/initxy/plugrelay/pkg/ir"
)

func full() ir.Capabilities {
	return ir.Capabilities{
		Cache: ir.CacheExplicit, Cache1h: true, FunctionTools: true, FreeformTools: true,
		ServerTools: []string{"web_search"}, DeferredTools: true, Reasoning: true,
		ReasoningStyle: StyleBudget, Images: true, Documents: true, ToolResultImgs: true,
		EchoFields: []string{"signature"}, ServerState: true, Compaction: true,
	}
}

func with(f func(*ir.Capabilities)) ir.Capabilities {
	c := full()
	f(&c)
	return c
}

func newSealer(t *testing.T) *seal.Sealer {
	t.Helper()
	s, err := seal.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAssessMatrix(t *testing.T) {
	cases := []struct {
		name    string
		needs   ir.Needs
		target  ir.Capabilities
		ctx     func(*Context)
		want    ir.Fidelity
		missing []string
	}{
		{"nothing needed", ir.Needs{}, ir.Capabilities{}, nil, ir.Lossless, nil},
		{"function tools native", ir.Needs{FunctionTools: true}, full(), nil, ir.Lossless, nil},
		{"function tools missing", ir.Needs{FunctionTools: true}, with(func(c *ir.Capabilities) { c.FunctionTools = false }), nil, ir.Impossible, []string{"function_tools"}},
		{"freeform native", ir.Needs{FreeformTools: true}, full(), nil, ir.Lossless, nil},
		{"freeform bridged", ir.Needs{FreeformTools: true}, with(func(c *ir.Capabilities) { c.FreeformTools = false }), nil, ir.Lossy, nil},
		{"freeform excluded", ir.Needs{FreeformTools: true}, with(func(c *ir.Capabilities) { c.FreeformTools, c.FunctionTools = false, false }), nil, ir.Impossible, []string{"freeform_tools"}},
		{"server tool native", ir.Needs{ServerTools: []string{"web_search"}}, full(), nil, ir.Lossless, nil},
		{"server tool offered only", ir.Needs{ServerTools: []string{"code_exec"}}, full(), nil, ir.Lossy, nil},
		{"server tool required", ir.Needs{ServerTools: []string{"code_exec"}, ServerToolsRequired: []string{"code_exec"}}, full(), nil, ir.Impossible, []string{"server_tool:code_exec"}},
		{"server tool required native", ir.Needs{ServerTools: []string{"web_search"}, ServerToolsRequired: []string{"web_search"}}, full(), nil, ir.Lossless, nil},
		{"deferred expanded", ir.Needs{DeferredTools: true}, with(func(c *ir.Capabilities) { c.DeferredTools = false }), nil, ir.Lossy, nil},
		{"reasoning dropped", ir.Needs{Reasoning: true}, with(func(c *ir.Capabilities) { c.Reasoning = false }), nil, ir.Lossy, nil},
		{"images excluded", ir.Needs{Images: true}, with(func(c *ir.Capabilities) { c.Images = false }), nil, ir.Impossible, []string{"images"}},
		{"documents excluded", ir.Needs{Documents: true}, with(func(c *ir.Capabilities) { c.Documents = false }), nil, ir.Impossible, []string{"documents"}},
		{"tool result images moved", ir.Needs{ToolResultImgs: true}, with(func(c *ir.Capabilities) { c.ToolResultImgs = false }), nil, ir.Lossy, nil},
		{"tool result images excluded", ir.Needs{ToolResultImgs: true}, with(func(c *ir.Capabilities) { c.ToolResultImgs, c.Images = false, false }), nil, ir.Impossible, []string{"tool_result_images"}},
		{"server state excluded", ir.Needs{ServerState: true}, with(func(c *ir.Capabilities) { c.ServerState = false }), nil, ir.Impossible, []string{"server_state"}},
		{"compaction excluded", ir.Needs{Compaction: true}, with(func(c *ir.Capabilities) { c.Compaction = false }), nil, ir.Impossible, []string{"compaction"}},
		{"context window", ir.Needs{InputTokensEst: 300000}, with(func(c *ir.Capabilities) { c.MaxContext = 200000 }), nil, ir.Impossible, []string{"context_window"}},
		{"cache to keyed", ir.Needs{ExplicitCache: true}, with(func(c *ir.Capabilities) { c.Cache = ir.CacheKeyed }), nil, ir.Lossless, nil},
		{"cache to none", ir.Needs{ExplicitCache: true}, with(func(c *ir.Capabilities) { c.Cache = ir.CacheNone }), nil, ir.Lossy, nil},
		{"cache 1h shortened", ir.Needs{ExplicitCache: true, Cache1h: true}, with(func(c *ir.Capabilities) { c.Cache1h = false }), nil, ir.Lossy, nil},
		{"opaque same issuer", ir.Needs{OpaqueState: true}, full(), func(c *Context) { c.HistoryIssuers = []string{"grp-a"} }, ir.Lossless, nil},
		{"opaque other issuer", ir.Needs{OpaqueState: true}, full(), func(c *Context) { c.HistoryIssuers = []string{"grp-b"} }, ir.Lossy, nil},
		{"opaque raw cross protocol", ir.Needs{OpaqueState: true}, full(), nil, ir.Lossy, nil},
		{"opaque raw same protocol", ir.Needs{OpaqueState: true}, full(), func(c *Context) { c.SameProtocol = true }, ir.Lossless, nil},
		{"freeform wrapped and opaque dropped", ir.Needs{FreeformTools: true, OpaqueState: true}, with(func(c *ir.Capabilities) { c.FreeformTools = false }), func(c *Context) { c.HistoryIssuers = []string{"grp-b"} }, ir.Discouraged, nil},
		{"fill in middle native", ir.Needs{FillInMiddle: true}, with(func(c *ir.Capabilities) { c.FillInMiddle, c.PromptOnly = true, true }), func(c *Context) { c.Source.PromptOnly = true }, ir.Lossless, nil},
		{"fill in middle excluded", ir.Needs{FillInMiddle: true}, full(), func(c *Context) { c.Source.PromptOnly = true }, ir.Impossible, []string{"fill_in_middle"}},
		{"prompt to message protocol", ir.Needs{}, full(), func(c *Context) { c.Source.PromptOnly = true }, ir.Lossy, nil},
		{"messages to prompt-only", ir.Needs{}, with(func(c *ir.Capabilities) { c.PromptOnly = true }), nil, ir.Impossible, []string{"conversation"}},
		{"freeform wrapped, opaque kept", ir.Needs{FreeformTools: true, OpaqueState: true}, with(func(c *ir.Capabilities) { c.FreeformTools = false }), func(c *Context) { c.HistoryIssuers = []string{"grp-a"} }, ir.Lossy, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Context{Source: full(), Target: tc.target, TargetIssuer: "grp-a"}
			if tc.ctx != nil {
				tc.ctx(c)
			}
			got, missing := Assess(tc.needs, c)
			if got != tc.want || !slices.Equal(missing, tc.missing) {
				t.Fatalf("got %s %v, want %s %v", got, missing, tc.want, tc.missing)
			}
			req := &ir.Request{}
			d, err := Request(req, tc.needs, c)
			if (tc.want == ir.Impossible) != (err != nil) || (err != nil && d.Fidelity != ir.Impossible) {
				t.Fatalf("Request err=%v fidelity=%s", err, d.Fidelity)
			}
		})
	}
}

func TestAdmits(t *testing.T) {
	c := &Context{}
	if c.Admits(ir.Discouraged) || c.Admits(ir.Impossible) || !c.Admits(ir.Lossy) {
		t.Fatal("default admission wrong")
	}
	c.AllowDiscouraged = true
	if !c.Admits(ir.Discouraged) || c.Admits(ir.Impossible) {
		t.Fatal("opt-in admission wrong")
	}
}

func userText(s string) ir.Message {
	return ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: s}}}
}

func TestCachePlacement(t *testing.T) {
	req := &ir.Request{
		System: []ir.Block{{Type: ir.BlockText, Text: "a"}, {Type: ir.BlockText, Text: "b"}},
		Messages: []ir.Message{
			userText("1"),
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockText, Text: "x"}, {Type: ir.BlockThinking, Text: "t"}}},
			userText("2"),
		},
	}
	c := &Context{Source: with(func(c *ir.Capabilities) { c.Cache = ir.CacheKeyed }), Target: full()}
	d, err := Request(req, ir.Needs{}, c)
	if err != nil {
		t.Fatal(err)
	}
	marked := func(b ir.Block) bool { return b.Cache != nil && b.Cache.TTL == "5m" }
	if marked(req.System[0]) || !marked(req.System[1]) {
		t.Fatal("system mark misplaced")
	}
	if marked(req.Messages[0].Content[0]) {
		t.Fatal("third-last message marked")
	}
	if !marked(req.Messages[1].Content[0]) || req.Messages[1].Content[1].Cache != nil {
		t.Fatal("mark must skip thinking blocks")
	}
	if !marked(req.Messages[2].Content[0]) {
		t.Fatal("last message not marked")
	}
	if d.Fidelity != ir.Lossless || len(d.Steps) != 1 {
		t.Fatalf("diag %+v", d)
	}

	// The client's own protocol had breakpoints available and used none.
	req2 := &ir.Request{Messages: []ir.Message{userText("1")}}
	Request(req2, ir.Needs{}, &Context{Source: full(), Target: full()})
	if hasMarks(req2) {
		t.Fatal("marks placed although the client chose none")
	}
}

func TestCacheModes(t *testing.T) {
	mk := func() *ir.Request {
		return &ir.Request{
			System:   []ir.Block{{Type: ir.BlockText, Text: "s", Cache: &ir.CacheMark{TTL: "1h"}}},
			Messages: []ir.Message{userText("u")},
		}
	}
	needs := ir.Needs{ExplicitCache: true, Cache1h: true}

	req := mk()
	d, _ := Request(req, needs, &Context{Source: full(), Target: with(func(c *ir.Capabilities) { c.Cache1h = false })})
	if req.System[0].Cache.TTL != "5m" || d.Fidelity != ir.Lossy || !slices.Contains(d.Dropped, "cache_ttl_1h") {
		t.Fatalf("1h not shortened: %+v", d)
	}

	req = mk()
	d, _ = Request(req, needs, &Context{Source: full(), Target: with(func(c *ir.Capabilities) { c.Cache = ir.CacheKeyed }), SessionID: "sess"})
	if hasMarks(req) || req.CacheKey != "sess" || d.Fidelity != ir.Lossless {
		t.Fatalf("keyed: key=%q %+v", req.CacheKey, d)
	}

	req = mk()
	req.CacheKey = "client"
	Request(req, needs, &Context{Source: full(), Target: with(func(c *ir.Capabilities) { c.Cache = ir.CacheKeyed }), SessionID: "sess"})
	if req.CacheKey != "client" {
		t.Fatal("client cache key overwritten")
	}

	req = mk()
	d, _ = Request(req, needs, &Context{Source: full(), Target: with(func(c *ir.Capabilities) { c.Cache = ir.CacheImplicit })})
	if hasMarks(req) || d.Fidelity != ir.Lossless {
		t.Fatalf("implicit: %+v", d)
	}

	req = mk()
	d, _ = Request(req, needs, &Context{Source: full(), Target: with(func(c *ir.Capabilities) { c.Cache = ir.CacheNone })})
	if hasMarks(req) || d.Fidelity != ir.Lossy {
		t.Fatalf("none: %+v", d)
	}
}

func freeformReq() *ir.Request {
	return &ir.Request{
		Tools: []ir.Tool{
			{Kind: ir.ToolFreeform, Name: "patch", Description: "Apply a patch.",
				Grammar: json.RawMessage(`{"type":"grammar","syntax":"lark","definition":"start: line+"}`)},
			{Kind: ir.ToolFunction, Name: "ls", InputSchema: json.RawMessage(`{"type":"object"}`), DeferLoading: true},
		},
		Messages: []ir.Message{
			userText("go"),
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "c1", Name: "patch", Kind: ir.ToolFreeform, Text: "*** Begin\n\"q\""}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, Result: &ir.ToolResult{ToolUseID: "c1", Freeform: true, Content: []ir.Block{{Type: ir.BlockText, Text: "ok"}}}}}},
		},
	}
}

func TestFreeformWrap(t *testing.T) {
	req := freeformReq()
	c := &Context{Source: full(), Target: with(func(c *ir.Capabilities) { c.FreeformTools, c.DeferredTools = false, false })}
	d, err := Request(req, ir.Needs{FreeformTools: true, FunctionTools: true, DeferredTools: true}, c)
	if err != nil {
		t.Fatal(err)
	}
	w := req.Tools[0]
	if w.Kind != ir.ToolFunction || w.Grammar != nil || !strings.Contains(w.Description, "start: line+") || !strings.Contains(w.Description, "lark") {
		t.Fatalf("not wrapped: %+v", w)
	}
	if !json.Valid(w.InputSchema) || !strings.Contains(string(w.InputSchema), `"input"`) {
		t.Fatalf("schema %s", w.InputSchema)
	}
	if req.Tools[1].DeferLoading {
		t.Fatal("defer_loading kept")
	}
	call := req.Messages[1].Content[0].Tool
	if call.Kind != ir.ToolFunction || call.Text != "" || string(call.Input) != `{"input":"*** Begin\n\"q\""}` {
		t.Fatalf("history call %+v %s", call, call.Input)
	}
	if req.Messages[2].Content[0].Result.Freeform {
		t.Fatal("history result still freeform")
	}
	if c.Tools["patch"].Kind != ir.ToolFreeform || d.Fidelity != ir.Lossy || !slices.Contains(d.Dropped, "defer_loading") {
		t.Fatalf("map %v diag %+v", c.Tools, d)
	}

	// Native freeform target leaves everything alone.
	req = freeformReq()
	c2 := &Context{Source: full(), Target: full()}
	d, _ = Request(req, ir.Needs{FreeformTools: true}, c2)
	if req.Tools[0].Kind != ir.ToolFreeform || len(c2.Tools) != 0 || d.Fidelity != ir.Lossless {
		t.Fatal("native target rewritten")
	}
}

func TestServerToolForced(t *testing.T) {
	forced := &ir.Request{
		Tools:      []ir.Tool{{Kind: ir.ToolServer, Name: "run", ServerType: "code_exec"}},
		ToolChoice: &ir.ToolChoice{Mode: "tool", Name: "run"},
	}
	_, err := Request(forced, ir.Needs{ServerTools: []string{"code_exec"}, ServerToolsRequired: forced.RequiredServerTools()}, &Context{Target: full()})
	var me *MissingError
	if err == nil || !asMissing(err, &me) || me.Missing[0] != "server_tool:code_exec" {
		t.Fatalf("forced server tool: %v", err)
	}
}

func TestServerToolOfferedDropped(t *testing.T) {
	req := &ir.Request{Tools: []ir.Tool{
		{Kind: ir.ToolFunction, Name: "f"},
		{Kind: ir.ToolServer, Name: "web_search", ServerType: "web_search"},
		{Kind: ir.ToolServer, Name: "code", ServerType: "code_exec"},
	}}
	needs := ir.Needs{FunctionTools: true, ServerTools: []string{"web_search", "code_exec"}, ServerToolsRequired: req.RequiredServerTools()}
	c := &Context{Source: full(), Target: full()}
	if !DropsServerTools(needs, c) {
		t.Fatal("DropsServerTools false")
	}
	d, err := Request(req, needs, c)
	if err != nil || len(req.Tools) != 2 || req.Tools[1].Name != "web_search" {
		t.Fatalf("err=%v tools=%+v", err, req.Tools)
	}
	if d.Fidelity != ir.Lossy || !slices.Equal(d.Dropped, []string{"server_tool:code_exec"}) {
		t.Fatalf("diag %+v", d)
	}
	if DropsServerTools(ir.Needs{ServerTools: []string{"web_search"}}, c) {
		t.Fatal("native server tool reported dropped")
	}
}

func TestRequiredServerTools(t *testing.T) {
	req := &ir.Request{
		Tools: []ir.Tool{{Kind: ir.ToolServer, Name: "web_search", ServerType: "web_search_20250305"}, {Kind: ir.ToolServer, Name: "fetch", ServerType: "web_fetch_1"}},
		Messages: []ir.Message{userText("q"), {Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockServerTool, Tool: &ir.ToolCall{ID: "s1", Name: "fetch", Kind: ir.ToolServer}},
			{Type: ir.BlockServerTool, Result: &ir.ToolResult{ToolUseID: "s1"}},
			{Type: ir.BlockServerTool, Tool: &ir.ToolCall{ID: "s2", Name: "web_search", Kind: ir.ToolServer}},
		}}},
	}
	if got := req.RequiredServerTools(); !slices.Equal(got, []string{"web_search_20250305"}) {
		t.Fatalf("paused call: %v", got)
	}
	req.Messages = req.Messages[:1]
	if got := req.RequiredServerTools(); len(got) != 0 {
		t.Fatalf("offered only: %v", got)
	}
}

func TestServerToolHistory(t *testing.T) {
	req := &ir.Request{Messages: []ir.Message{
		userText("find it"),
		{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockServerTool, Tool: &ir.ToolCall{ID: "s1", Name: "web_search", Kind: ir.ToolServer, Input: json.RawMessage(`{"query":"go"}`)}},
			{Type: ir.BlockServerTool, Result: &ir.ToolResult{ToolUseID: "s1"}},
			{Type: ir.BlockText, Text: "found"},
		}},
		userText("thanks"),
	}}
	c := &Context{Source: full(), Target: full()}
	d, err := Request(req, ir.Needs{}, c)
	if err != nil {
		t.Fatal(err)
	}
	asst := req.Messages[1].Content
	if len(asst) != 2 || asst[0].Type != ir.BlockText || asst[0].Text != `(called provider tool web_search with {"query":"go"})` {
		t.Fatalf("history %+v", asst)
	}
	if !slices.Contains(d.Dropped, "server_tool_history") || d.Fidelity != ir.Lossy {
		t.Fatalf("diag %+v", d)
	}
	same := &ir.Request{Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockServerTool, Tool: &ir.ToolCall{Name: "web_search"}}}}}}
	if _, err := Request(same, ir.Needs{}, &Context{Target: full(), SameProtocol: true}); err != nil || same.Messages[0].Content[0].Type != ir.BlockServerTool {
		t.Fatal("same protocol history rewritten")
	}
}

func namespaceReq() *ir.Request {
	return &ir.Request{
		Tools: []ir.Tool{
			{Kind: ir.ToolFunction, Name: "exec"},
			{Kind: ir.ToolFunction, Name: "add_note", Namespace: "mcp__notes"},
			{Kind: ir.ToolFreeform, Name: "draw", Namespace: "mcp__notes"},
		},
		ToolChoice: &ir.ToolChoice{Mode: "tool", Name: "add_note"},
		Messages: []ir.Message{
			userText("q"),
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "c1", Name: "add_note", Namespace: "mcp__notes", Kind: ir.ToolFunction, Input: json.RawMessage(`{}`)}}}},
			{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, Result: &ir.ToolResult{ToolUseID: "c1"}}}},
		},
	}
}

func TestNamespaceFlatten(t *testing.T) {
	req := namespaceReq()
	c := &Context{Source: full(), Target: with(func(c *ir.Capabilities) { c.FreeformTools = false })}
	d, err := Request(req, ir.Needs{FunctionTools: true, FreeformTools: true}, c)
	if err != nil {
		t.Fatal(err)
	}
	if req.Tools[0].Name != "exec" || req.Tools[1].Name != "mcp__notes__add_note" || req.Tools[1].Namespace != "" ||
		req.Tools[2].Name != "mcp__notes__draw" || req.Tools[2].Kind != ir.ToolFunction {
		t.Fatalf("tools %+v", req.Tools)
	}
	if call := req.Messages[1].Content[0].Tool; call.Name != "mcp__notes__add_note" || call.Namespace != "" {
		t.Fatalf("history %+v", call)
	}
	if req.ToolChoice.Name != "mcp__notes__add_note" {
		t.Fatalf("tool choice %+v", req.ToolChoice)
	}
	if !slices.Contains(d.Steps, "tools: namespaced tools flattened") || !slices.Contains(d.Steps, "tools: namespaced history calls flattened") {
		t.Fatalf("diag %+v", d)
	}
	if c.Tools["mcp__notes__draw"] != (ToolOrigin{Kind: ir.ToolFreeform, Name: "draw", Namespace: "mcp__notes"}) {
		t.Fatalf("map %+v", c.Tools)
	}

	resp := &ir.Response{Content: []ir.Block{
		{Type: ir.BlockToolUse, Tool: &ir.ToolCall{Name: "mcp__notes__add_note", Kind: ir.ToolFunction, Input: json.RawMessage(`{"text":"x"}`)}},
		{Type: ir.BlockToolUse, Tool: &ir.ToolCall{Name: "mcp__notes__draw", Kind: ir.ToolFunction, Input: json.RawMessage(`{"input":"abc"}`)}},
		{Type: ir.BlockToolUse, Tool: &ir.ToolCall{Name: "exec", Kind: ir.ToolFunction}},
	}}
	NewResponse(c).Full(resp)
	if t0 := resp.Content[0].Tool; t0.Name != "add_note" || t0.Namespace != "mcp__notes" || string(t0.Input) != `{"text":"x"}` {
		t.Fatalf("full %+v", t0)
	}
	if t1 := resp.Content[1].Tool; t1.Name != "draw" || t1.Namespace != "mcp__notes" || t1.Kind != ir.ToolFreeform || t1.Text != "abc" {
		t.Fatalf("full freeform %+v", t1)
	}
	if t2 := resp.Content[2].Tool; t2.Name != "exec" || t2.Namespace != "" {
		t.Fatalf("plain %+v", t2)
	}

	r := NewResponse(c)
	out := collect(r, []ir.Event{
		{Kind: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "c2", Name: "mcp__notes__add_note", Kind: ir.ToolFunction}}},
		{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: `{}`}},
		{Kind: ir.EvBlockStop, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "c2", Name: "mcp__notes__add_note", Kind: ir.ToolFunction, Input: json.RawMessage(`{}`)}}},
	})
	if s := out[0].Block.Tool; s.Name != "add_note" || s.Namespace != "mcp__notes" {
		t.Fatalf("stream start %+v", s)
	}
	if s := out[2].Block.Tool; s.Name != "add_note" || s.Namespace != "mcp__notes" {
		t.Fatalf("stream stop %+v", s)
	}

	// A target with namespaces keeps them.
	req = namespaceReq()
	c = &Context{Source: full(), Target: with(func(c *ir.Capabilities) { c.Namespaces = true })}
	if _, err := Request(req, ir.Needs{}, c); err != nil || req.Tools[1].Name != "add_note" || req.Tools[1].Namespace != "mcp__notes" || len(c.Tools) != 0 {
		t.Fatalf("namespaces target rewritten: %v %+v %v", err, req.Tools, c.Tools)
	}
}

// TestNamespaceFlattenCollisions: when "ns__name" is already taken, by a
// top-level tool or by another namespace pair that flattens to the same
// string, every claimant gets its hashed form, consistently in the tool
// list, the history and the reverse map. A forced choice prefers the
// top-level tool and otherwise picks the first namespace in tool order.
func TestNamespaceFlattenCollisions(t *testing.T) {
	newReq := func(choice string) *ir.Request {
		return &ir.Request{
			Model: "m",
			Tools: []ir.Tool{
				{Kind: ir.ToolFunction, Name: "a__b"},
				{Kind: ir.ToolFunction, Name: "b", Namespace: "a"},
				{Kind: ir.ToolFunction, Name: "_y", Namespace: "x_"},
				{Kind: ir.ToolFunction, Name: "__y", Namespace: "x"},
				{Kind: ir.ToolFunction, Name: "run", Namespace: "zeta"},
				{Kind: ir.ToolFunction, Name: "run", Namespace: "alpha"},
			},
			ToolChoice: &ir.ToolChoice{Mode: "tool", Name: choice},
			Messages: []ir.Message{
				userText("q"),
				{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "c1", Name: "b", Namespace: "a", Kind: ir.ToolFunction, Input: json.RawMessage(`{}`)}}}},
				{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, Result: &ir.ToolResult{ToolUseID: "c1"}}}},
			},
		}
	}
	req := newReq("run")
	c := &Context{Source: full(), Target: with(func(c *ir.Capabilities) { c.Namespaces = false })}
	if _, err := Request(req, ir.Needs{FunctionTools: true}, c); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range req.Tools {
		if seen[tool.Name] || !validToolName(tool.Name) {
			t.Fatalf("duplicate or invalid tool name %q in %+v", tool.Name, req.Tools)
		}
		seen[tool.Name] = true
	}
	if req.Tools[0].Name != "a__b" || req.Tools[1].Name != hashedName("a", "b") {
		t.Fatalf("top-level tool must keep its name, the namespaced one is hashed: %+v", req.Tools[:2])
	}
	if req.Tools[2].Name != hashedName("x_", "_y") || req.Tools[3].Name != hashedName("x", "__y") {
		t.Fatalf("colliding namespace pairs: %+v", req.Tools[2:4])
	}
	if req.Tools[4].Name != "zeta__run" || req.Tools[5].Name != "alpha__run" {
		t.Fatalf("non-colliding names changed: %+v", req.Tools[4:])
	}
	if call := req.Messages[1].Content[0].Tool; call.Name != hashedName("a", "b") {
		t.Fatalf("history call %q disagrees with the tool list", call.Name)
	}
	if o := c.Tools[hashedName("a", "b")]; o.Namespace != "a" || o.Name != "b" {
		t.Fatalf("reverse map %+v", o)
	}
	if req.ToolChoice.Name != "zeta__run" {
		t.Fatalf("ambiguous choice should take the first in tool order: %+v", req.ToolChoice)
	}

	req = newReq("a__b")
	c = &Context{Source: full(), Target: with(func(c *ir.Capabilities) { c.Namespaces = false })}
	if _, err := Request(req, ir.Needs{FunctionTools: true}, c); err != nil {
		t.Fatal(err)
	}
	if req.ToolChoice.Name != "a__b" {
		t.Fatalf("choice of a top-level tool rewritten: %+v", req.ToolChoice)
	}
	req = newReq("b")
	c = &Context{Source: full(), Target: with(func(c *ir.Capabilities) { c.Namespaces = false })}
	if _, err := Request(req, ir.Needs{FunctionTools: true}, c); err != nil {
		t.Fatal(err)
	}
	if req.ToolChoice.Name != hashedName("a", "b") {
		t.Fatalf("choice of a namespaced tool: %+v", req.ToolChoice)
	}
}

func TestFlatName(t *testing.T) {
	long := strings.Repeat("n", 40)
	cases := []struct{ ns, name string }{{"mcp__notes", "add_note"}, {long, long}, {"my.server", "do it"}, {"9ns", "x"}}
	seen := map[string]bool{}
	for _, tc := range cases {
		got := flatName(tc.ns, tc.name)
		if !validToolName(got) || seen[got] || got != flatName(tc.ns, tc.name) {
			t.Fatalf("%q/%q -> %q", tc.ns, tc.name, got)
		}
		seen[got] = true
	}
	if flatName("mcp__notes", "add_note") != "mcp__notes__add_note" {
		t.Fatal("valid name changed")
	}
	if flatName(long, long+"a") == flatName(long, long+"b") {
		t.Fatal("shortened names collide")
	}
}

func asMissing(err error, out **MissingError) bool {
	me, ok := err.(*MissingError)
	*out = me
	return ok
}

func collect(r *Response, evs []ir.Event) []ir.Event {
	var out []ir.Event
	for i := range evs {
		out = append(out, r.Event(&evs[i])...)
	}
	return out
}

func TestUnwrapStreaming(t *testing.T) {
	c := &Context{Source: full(), Target: with(func(c *ir.Capabilities) { c.FreeformTools = false })}
	if _, err := Request(freeformReq(), ir.Needs{FreeformTools: true}, c); err != nil {
		t.Fatal(err)
	}
	r := NewResponse(c)
	out := collect(r, []ir.Event{
		{Kind: ir.EvStart, ID: "r1"},
		{Kind: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
		{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaText, Text: "hi"}},
		{Kind: ir.EvBlockStop, Index: 0},
		{Kind: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "c2", Name: "patch", Kind: ir.ToolFunction}}},
		{Kind: ir.EvDelta, Index: 1, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: `{"inp`}},
		{Kind: ir.EvDelta, Index: 1, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: `ut": "line1\nlineé"}`}},
		{Kind: ir.EvBlockStop, Index: 1},
		{Kind: ir.EvBlockStart, Index: 2, Block: &ir.Block{Type: ir.BlockToolUse, Tool: &ir.ToolCall{ID: "c3", Name: "ls", Kind: ir.ToolFunction}}},
		{Kind: ir.EvDelta, Index: 2, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: `{}`}},
		{Kind: ir.EvBlockStop, Index: 2},
		{Kind: ir.EvDone, StopReason: ir.StopToolUse},
	})
	kinds := []ir.EventKind{}
	for _, e := range out {
		kinds = append(kinds, e.Kind)
	}
	want := []ir.EventKind{ir.EvStart, ir.EvBlockStart, ir.EvDelta, ir.EvBlockStop, ir.EvBlockStart, ir.EvDelta, ir.EvBlockStop, ir.EvBlockStart, ir.EvDelta, ir.EvBlockStop, ir.EvDone}
	if !slices.Equal(kinds, want) {
		t.Fatalf("kinds %v", kinds)
	}
	if out[4].Block.Tool.Kind != ir.ToolFreeform {
		t.Fatal("start not freeform")
	}
	if out[5].Delta.Text != "line1\nlineé" {
		t.Fatalf("delta %q", out[5].Delta.Text)
	}
	if out[8].Delta.Text != "{}" || out[7].Block.Tool.Kind != ir.ToolFunction {
		t.Fatal("unwrapped tool touched")
	}
}

func TestUnwrapFull(t *testing.T) {
	c := &Context{Tools: ToolMap{"patch": {Kind: ir.ToolFreeform, Name: "patch"}}}
	resp := &ir.Response{Content: []ir.Block{
		{Type: ir.BlockToolUse, Tool: &ir.ToolCall{Name: "patch", Kind: ir.ToolFunction, Input: json.RawMessage(`{"input":"abc"}`)}},
		{Type: ir.BlockToolUse, Tool: &ir.ToolCall{Name: "patch", Kind: ir.ToolFunction, Input: json.RawMessage(`{"other":1}`)}},
	}}
	NewResponse(c).Full(resp)
	if t0 := resp.Content[0].Tool; t0.Kind != ir.ToolFreeform || t0.Text != "abc" || t0.Input != nil {
		t.Fatalf("%+v", t0)
	}
	if resp.Content[1].Tool.Text != `{"other":1}` {
		t.Fatal("malformed input must pass verbatim")
	}
}

func sealedState(t *testing.T, s *seal.Sealer, issuer string) *ir.OpaqueState {
	t.Helper()
	str, err := s.Seal(ir.OpaqueState{Issuer: issuer, Kind: "signature", Data: json.RawMessage(`"sig-` + issuer + `"`)})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(str)
	return &ir.OpaqueState{Kind: "signature", Data: data, Sealed: true}
}

func TestOpaqueHistory(t *testing.T) {
	s := newSealer(t)
	mk := func() *ir.Request {
		bad := sealedState(t, s, "grp-a")
		bad.Data = json.RawMessage(strings.Replace(string(bad.Data), "prs1.", "prs1.AA", 1))
		return &ir.Request{Messages: []ir.Message{
			userText("q"),
			{Role: ir.RoleAssistant, Content: []ir.Block{
				{Type: ir.BlockThinking, Text: "mine", Opaque: sealedState(t, s, "grp-a")},
				{Type: ir.BlockThinking, Text: "theirs", Opaque: sealedState(t, s, "grp-b")},
				{Type: ir.BlockOpaque, Opaque: bad},
				{Type: ir.BlockText, Text: "answer", Opaque: sealedState(t, s, "grp-b")},
				{Type: ir.BlockRaw, Extra: ir.Extra{Origin: "x"}},
			}},
			{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockOpaque, Opaque: sealedState(t, s, "grp-b")}}},
			userText("again"),
		}}
	}
	req := mk()
	if got := HistoryIssuers(req, s); !slices.Equal(got, []string{"grp-a", "grp-b"}) {
		t.Fatalf("issuers %v", got)
	}
	c := &Context{Source: full(), Target: full(), TargetIssuer: "grp-a", Sealer: s}
	c.HistoryIssuers = HistoryIssuers(req, s)
	d, err := Request(req, ir.Needs{OpaqueState: true}, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("empty assistant message not removed: %d", len(req.Messages))
	}
	a := req.Messages[1].Content
	if len(a) != 2 || a[0].Text != "mine" || a[0].Opaque.Sealed || string(a[0].Opaque.Data) != `"sig-grp-a"` || a[0].Opaque.Issuer != "grp-a" {
		t.Fatalf("kept state wrong: %+v", a)
	}
	if a[1].Text != "answer" || a[1].Opaque != nil {
		t.Fatalf("foreign state on text must be stripped: %+v", a[1])
	}
	if d.Fidelity != ir.Lossy || !slices.Contains(d.Dropped, "opaque_state") || !slices.Contains(d.Dropped, "raw_items") {
		t.Fatalf("diag %+v", d)
	}

	// Raw state is trusted only on the same protocol.
	raw := func() *ir.Request {
		return &ir.Request{Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{
			{Type: ir.BlockOpaque, Opaque: &ir.OpaqueState{Kind: "encrypted_content", Data: json.RawMessage(`"e"`)}},
			{Type: ir.BlockText, Text: "t"},
		}}}}
	}
	req = raw()
	Request(req, ir.Needs{}, &Context{Target: full(), SameProtocol: true, TargetIssuer: "grp-a"})
	if len(req.Messages[0].Content) != 2 {
		t.Fatal("raw state dropped on same protocol")
	}
	req = raw()
	Request(req, ir.Needs{}, &Context{Target: full(), TargetIssuer: "grp-a"})
	if len(req.Messages[0].Content) != 1 {
		t.Fatal("raw state kept across protocols")
	}
}

func TestOpaqueResponseRoundTrip(t *testing.T) {
	s := newSealer(t)
	src := with(func(c *ir.Capabilities) { c.EchoFields = []string{"encrypted_content"} })
	c := &Context{Source: src, Target: full(), TargetIssuer: "grp-a", Sealer: s}
	r := NewResponse(c)
	out := collect(r, []ir.Event{
		{Kind: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockThinking}},
		{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaThinking, Text: "hmm"}},
		{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaOpaque, Opaque: &ir.OpaqueState{Kind: "signature", Data: json.RawMessage(`"ab"`)}}},
		{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaOpaque, Opaque: &ir.OpaqueState{Kind: "signature", Data: json.RawMessage(`"cd"`)}}},
		{Kind: ir.EvBlockStop, Index: 0},
	})
	if len(out) != 3 {
		t.Fatalf("events %d", len(out))
	}
	st := out[2].Block.Opaque
	if st == nil || !st.Sealed || st.Issuer != "grp-a" {
		t.Fatalf("stop state %+v", st)
	}

	// Next turn: the client echoes it back.
	next := func(issuer string) *ir.Request {
		return &ir.Request{Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockThinking, Text: "hmm", Opaque: st}, {Type: ir.BlockText, Text: "x"}}}}}
	}
	req := next("grp-a")
	Request(req, ir.Needs{OpaqueState: true}, &Context{Source: src, Target: full(), TargetIssuer: "grp-a", Sealer: s})
	got := req.Messages[0].Content[0].Opaque
	if got == nil || got.Sealed || string(got.Data) != `"abcd"` || got.Kind != "signature" {
		t.Fatalf("restored %+v", got)
	}
	req = next("grp-b")
	Request(req, ir.Needs{OpaqueState: true}, &Context{Source: src, Target: full(), TargetIssuer: "grp-b", Sealer: s})
	if len(req.Messages[0].Content) != 1 || req.Messages[0].Content[0].Type != ir.BlockText {
		t.Fatal("state reached another issuer")
	}
}

func TestOpaqueResponseNoEcho(t *testing.T) {
	c := &Context{Source: ir.Capabilities{}, Target: full(), TargetIssuer: "grp-a", Sealer: newSealer(t)}
	r := NewResponse(c)
	out := collect(r, []ir.Event{
		{Kind: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockOpaque, Opaque: &ir.OpaqueState{Kind: "redacted", Data: json.RawMessage(`"z"`)}}},
		{Kind: ir.EvBlockStop, Index: 0},
		{Kind: ir.EvBlockStart, Index: 1, Block: &ir.Block{Type: ir.BlockThinking}},
		{Kind: ir.EvDelta, Index: 1, Delta: &ir.Delta{Kind: ir.DeltaOpaque, Opaque: &ir.OpaqueState{Kind: "signature", Data: json.RawMessage(`"s"`)}}},
		{Kind: ir.EvBlockStop, Index: 1},
		{Kind: ir.EvBlockStart, Index: 2, Block: &ir.Block{Type: ir.BlockText}},
		{Kind: ir.EvBlockStop, Index: 2},
	})
	if len(out) != 4 || out[0].Index != 0 || out[2].Index != 1 || out[3].Index != 1 {
		t.Fatalf("indexes not dense: %+v", out)
	}
	if out[1].Block != nil && out[1].Block.Opaque != nil {
		t.Fatal("signature leaked to a client without echo fields")
	}
	if d := r.Diagnostic(); d.Fidelity != ir.Lossy || len(d.Notes) != 1 {
		t.Fatalf("diag %+v", d)
	}

	resp := &ir.Response{Content: []ir.Block{
		{Type: ir.BlockOpaque, Opaque: &ir.OpaqueState{Kind: "k"}},
		{Type: ir.BlockThinking, Text: "t", Opaque: &ir.OpaqueState{Kind: "k"}},
	}}
	NewResponse(c).Full(resp)
	if len(resp.Content) != 1 || resp.Content[0].Opaque != nil {
		t.Fatalf("full: %+v", resp.Content)
	}
}

func TestOpaqueResponseSameProtocol(t *testing.T) {
	c := &Context{SameProtocol: true, TargetIssuer: "grp-a"}
	resp := &ir.Response{Content: []ir.Block{{Type: ir.BlockOpaque, Opaque: &ir.OpaqueState{Kind: "k", Data: json.RawMessage(`"v"`)}}}}
	NewResponse(c).Full(resp)
	st := resp.Content[0].Opaque
	if st.Sealed || st.Issuer != "grp-a" || string(st.Data) != `"v"` {
		t.Fatalf("%+v", st)
	}
}

func intp(v int) *int { return &v }

func TestParams(t *testing.T) {
	temp := 0.5
	req := &ir.Request{
		MaxOutputTokens: intp(8000), Temperature: &temp, Stop: []string{"x"}, ServiceTier: "flex",
		Reasoning: &ir.Reasoning{Enabled: true, Effort: "high"},
	}
	c := &Context{Source: full(), Target: with(func(c *ir.Capabilities) {
		c.UnsupportedParams = []string{"temperature", "stop", "service_tier", "top_p"}
	})}
	d, _ := Request(req, ir.Needs{Reasoning: true}, c)
	if req.Temperature != nil || req.Stop != nil || req.ServiceTier != "" {
		t.Fatal("params not removed")
	}
	if *req.Reasoning.Budget != 7999 {
		t.Fatalf("budget %d", *req.Reasoning.Budget)
	}
	if d.Fidelity != ir.Lossy || len(d.Dropped) != 3 {
		t.Fatalf("diag %+v", d)
	}

	req = &ir.Request{Reasoning: &ir.Reasoning{Enabled: true, Budget: intp(20000)}}
	Request(req, ir.Needs{}, &Context{Target: with(func(c *ir.Capabilities) { c.ReasoningStyle = StyleEffort })})
	if req.Reasoning.Effort != "high" {
		t.Fatalf("effort %q", req.Reasoning.Effort)
	}

	req = &ir.Request{Reasoning: &ir.Reasoning{Enabled: true, Effort: "low"}}
	d, _ = Request(req, ir.Needs{Reasoning: true}, &Context{Target: ir.Capabilities{}})
	if req.Reasoning != nil || d.Fidelity != ir.Lossy {
		t.Fatal("reasoning not dropped")
	}

	for budget, want := range map[int]string{0: "minimal", 3000: "low", 7000: "low", 8000: "medium", 17000: "medium", 28000: "high", 100000: "max"} {
		if got := BudgetEffort(budget); got != want {
			t.Errorf("BudgetEffort(%d)=%s want %s", budget, got, want)
		}
	}
}

func TestToolResultImages(t *testing.T) {
	img := ir.Block{Type: ir.BlockImage, Media: &ir.Media{MediaType: "image/png", Data: "AA"}}
	req := &ir.Request{Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{
		{Type: ir.BlockToolResult, Result: &ir.ToolResult{ToolUseID: "a", Content: []ir.Block{{Type: ir.BlockText, Text: "shot"}, img}}},
		{Type: ir.BlockToolResult, Result: &ir.ToolResult{ToolUseID: "b", Content: []ir.Block{img}}},
		{Type: ir.BlockText, Text: "next"},
	}}}}
	d, err := Request(req, ir.Needs{ToolResultImgs: true}, &Context{Target: with(func(c *ir.Capabilities) { c.ToolResultImgs = false })})
	if err != nil {
		t.Fatal(err)
	}
	got := []ir.BlockType{}
	for _, b := range req.Messages[0].Content {
		got = append(got, b.Type)
	}
	want := []ir.BlockType{ir.BlockToolResult, ir.BlockToolResult, ir.BlockImage, ir.BlockImage, ir.BlockText}
	if !slices.Equal(got, want) {
		t.Fatalf("order %v", got)
	}
	if r := req.Messages[0].Content[1].Result; len(r.Content) != 1 || r.Content[0].Text != movedImageNote {
		t.Fatalf("placeholder %+v", r.Content)
	}
	if d.Fidelity != ir.Lossy {
		t.Fatal("not lossy")
	}
}

func TestEstimateUsage(t *testing.T) {
	u := EstimateUsage(&ir.Request{Messages: []ir.Message{userText(strings.Repeat("a", 400))}}, 80)
	if !u.Estimated || u.Input != 100 || u.Output != 20 {
		t.Fatalf("%+v", u)
	}
}

// TestNoProductNames enforces that conversion steps are capability-driven.
func TestNoProductNames(t *testing.T) {
	deny := []string{
		"claude", "codex", "anthropic", "openai", "gemini", "deepseek", "cursor",
		"bedrock", "vertex", "azure", "google", "mistral", "qwen", "doubao", "kimi",
		"moonshot", "cline", "opencode", "grok", "copilot", "gpt",
	}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		low := strings.ToLower(string(b))
		for _, w := range deny {
			if strings.Contains(low, w) {
				t.Errorf("%s mentions %q", f, w)
			}
		}
	}
}
