// Package convert holds the generic conversion steps that bridge a request
// from the client's protocol to an endpoint with different capabilities, and
// the response back.
//
// Every decision here is taken by comparing capability declarations; no
// client or upstream product is ever named (a test enforces it). A step runs
// only when the capabilities it covers differ, so a new protocol needs only a
// codec and a capability declaration.
//
// Fidelity rules, shared by Assess and Request:
//
//   - impossible: a need the target lacks and no step can bridge (function
//     tools, images, documents, required server tools nobody runs, server
//     state, compaction, context window; freeform tools when the target has
//     no function tools; tool-result images when the target has no images;
//     a suffix (fill-in-the-middle) the target cannot complete around; a
//     request of a message-based protocol sent to a prompt-only one, which
//     would flatten turns into text to continue).
//   - lossy: anything dropped or approximated (cache breakpoints removed for a
//     target without caching, 1h TTL shortened, deferred tools expanded,
//     offered-only server tools removed, server tool history rewritten,
//     tool-result images moved, unsupported parameters removed, raw items
//     dropped, a prompt-only request sent as one user message) or exactly
//     one major loss.
//   - discouraged: two or more major losses at once. Major losses are the
//     ones that change how the model behaves in an agent loop: freeform tools
//     wrapped as function tools, upstream-private history state dropped, and
//     requested reasoning dropped.
//
// A server tool is required only when tool_choice forces it or the model is
// in the middle of a call to it (ir.Needs.ServerToolsRequired); merely
// offered ones the target cannot run are removed from the tool list. Server
// tool calls in history going to another protocol become a short text note
// and their results are dropped, since results are provider-private.
//
// Namespaced tools going to a protocol without namespaces are flattened to
// "<namespace>__<name>" (shortened with a hash when that is not a valid
// tool name), in the tool list and in history alike; Response maps calls
// back to name and namespace.
//
// Media operations (embeddings, images, audio, rerank) are served only by
// an endpoint declaring the operation; a text request never goes to a
// media-only protocol. Embeddings and rerank are the only media operations
// converted across protocols (see media.go).
package convert

import (
	"slices"
	"strings"

	"github.com/initxy/plugrelay/internal/seal"
	"github.com/initxy/plugrelay/pkg/ir"
)

// Major loss kinds; see the package doc.
const (
	lossFreeform  = "freeform_wrapped"
	lossOpaque    = "opaque_dropped"
	lossReasoning = "reasoning_dropped"
)

// Context is everything the steps need to know about one request/endpoint
// pairing. Build one per attempt; Request fills Tools for the response side.
type Context struct {
	Source       ir.Capabilities // client protocol capabilities
	Target       ir.Capabilities // effective endpoint capabilities (codec ∩ endpoint)
	SameProtocol bool            // endpoint speaks the client's protocol
	TargetIssuer string          // issuer group of the target endpoint
	SessionID    string          // fills keyed caches
	Sealer       *seal.Sealer
	// AllowDiscouraged admits discouraged conversions (admin opt-in).
	AllowDiscouraged bool
	// HistoryIssuers are the issuers of the opaque states in the request's
	// history, as returned by HistoryIssuers; "" stands for raw (unsealed)
	// state. Nil means unknown and is treated as raw state.
	HistoryIssuers []string
	// Tools maps tool names that Request rewrote to how the client declared
	// them, so the response side can restore them. Filled by Request.
	Tools ToolMap
}

// ToolMap maps a tool name as sent upstream to how the client declared it.
type ToolMap map[string]ToolOrigin

// ToolOrigin is a tool as the client declared it.
type ToolOrigin struct {
	Kind      ir.ToolKind
	Name      string
	Namespace string
}

// MissingError reports needs the target cannot serve; the pipeline turns it
// into a 400 in the client's protocol when no endpoint is left.
type MissingError struct{ Missing []string }

func (e *MissingError) Error() string {
	return "convert: endpoint cannot serve: " + strings.Join(e.Missing, ", ")
}

// Admits reports whether the router may use an endpoint graded f.
func (c *Context) Admits(f ir.Fidelity) bool {
	return f != ir.Impossible && (f != ir.Discouraged || c.AllowDiscouraged)
}

// Assess grades, without mutating anything, how well the target can serve a
// request with these needs. Missing lists the needs that make it impossible.
// It is a prediction: parameters and raw items are not visible in Needs, so
// Request may still end up lossy where Assess said lossless.
func Assess(needs ir.Needs, c *Context) (ir.Fidelity, []string) {
	t := c.Target
	if needs.Modality != ir.ModalityText || t.MediaOnly {
		return assessMedia(needs, c)
	}
	var r recorder
	var missing []string
	miss := func(s string) { missing = append(missing, s) }

	if needs.FunctionTools && !t.FunctionTools {
		miss("function_tools")
	}
	if needs.FreeformTools && !t.FreeformTools {
		if t.FunctionTools {
			r.major(lossFreeform)
		} else {
			miss("freeform_tools")
		}
	}
	for _, st := range needs.ServerTools {
		if !c.runsServerTool(st) && !slices.Contains(needs.ServerToolsRequired, st) {
			r.lossy = true
		}
	}
	for _, st := range needs.ServerToolsRequired {
		if !c.runsServerTool(st) {
			miss("server_tool:" + st)
		}
	}
	if needs.DeferredTools && !t.DeferredTools {
		r.lossy = true
	}
	if needs.Reasoning && !t.Reasoning {
		r.major(lossReasoning)
	}
	if needs.Images && !t.Images {
		miss("images")
	}
	if needs.Documents && !t.Documents {
		miss("documents")
	}
	if needs.ToolResultImgs && !t.ToolResultImgs {
		if t.Images {
			r.lossy = true
		} else {
			miss("tool_result_images")
		}
	}
	if needs.ServerState && !t.ServerState {
		miss("server_state")
	}
	if needs.Compaction && !t.Compaction {
		miss("compaction")
	}
	if needs.FillInMiddle && !t.FillInMiddle {
		miss("fill_in_middle")
	}
	if t.PromptOnly && !c.Source.PromptOnly {
		miss("conversation")
	}
	if c.Source.PromptOnly && !t.PromptOnly {
		r.lossy = true // a prompt to continue becomes a message to answer
	}
	if t.MaxContext > 0 && needs.InputTokensEst > t.MaxContext {
		miss("context_window")
	}
	if needs.ExplicitCache {
		switch cacheMode(t.Cache) {
		case ir.CacheExplicit:
			if needs.Cache1h && !t.Cache1h {
				r.lossy = true
			}
		case ir.CacheNone:
			r.lossy = true
		}
	}
	if needs.OpaqueState {
		issuers := c.HistoryIssuers
		if issuers == nil {
			issuers = []string{""}
		}
		for _, iss := range issuers {
			if !c.keepsIssuer(iss, iss == "") {
				r.major(lossOpaque)
				break
			}
		}
	}
	if len(missing) > 0 {
		return ir.Impossible, missing
	}
	return r.fidelity(), nil
}

// DropsServerTools reports whether serving the request on the target means
// removing offered server tools, which only the convert tier can do.
func DropsServerTools(needs ir.Needs, c *Context) bool {
	for _, st := range needs.ServerTools {
		if !c.runsServerTool(st) {
			return true
		}
	}
	return false
}

// runsServerTool reports whether the target executes the server tool type.
func (c *Context) runsServerTool(typ string) bool {
	return slices.Contains(c.Target.ServerTools, typ)
}

// Request rewrites a decoded request in place for the target and reports
// what it did. It fails with *MissingError when Assess would say impossible.
func Request(req *ir.Request, needs ir.Needs, c *Context) (ir.Diagnostic, error) {
	if f, missing := Assess(needs, c); f == ir.Impossible {
		return ir.Diagnostic{Fidelity: ir.Impossible, Missing: missing}, &MissingError{Missing: missing}
	}
	r := &recorder{}
	if needs.Modality != ir.ModalityText {
		stepEmbed(req, c, r)
		stepRerank(req, c, r)
		return r.diagnostic(), nil
	}
	if c.Tools == nil {
		c.Tools = ToolMap{}
	}
	stepCache(req, c, r)
	if err := stepTools(req, needs, c, r); err != nil {
		d := r.diagnostic()
		d.Fidelity = ir.Impossible
		d.Missing = err.Missing
		return d, err
	}
	if c.Source.PromptOnly && !c.Target.PromptOnly {
		r.step("prompt: sent as one user message")
		r.lossy = true
	}
	stepOpaque(req, c, r)
	stepParams(req, c, r)
	stepContent(req, c, r)
	return r.diagnostic(), nil
}

// recorder accumulates a Diagnostic and the losses that grade it.
type recorder struct {
	d      ir.Diagnostic
	lossy  bool
	majors []string
}

func (r *recorder) step(s string) { r.d.Steps = append(r.d.Steps, s) }
func (r *recorder) note(s string) { r.d.Notes = append(r.d.Notes, s) }

// drop records a lost field or block and makes the conversion lossy.
func (r *recorder) drop(what string) {
	if !slices.Contains(r.d.Dropped, what) {
		r.d.Dropped = append(r.d.Dropped, what)
	}
	r.lossy = true
}

func (r *recorder) major(kind string) {
	if !slices.Contains(r.majors, kind) {
		r.majors = append(r.majors, kind)
	}
}

func (r *recorder) fidelity() ir.Fidelity {
	switch {
	case len(r.majors) >= 2:
		return ir.Discouraged
	case len(r.majors) == 1 || r.lossy:
		return ir.Lossy
	}
	return ir.Lossless
}

func (r *recorder) diagnostic() ir.Diagnostic {
	d := r.d
	d.Fidelity = r.fidelity()
	return d
}

func cacheMode(m ir.CacheMode) ir.CacheMode {
	if m == "" {
		return ir.CacheNone
	}
	return m
}
