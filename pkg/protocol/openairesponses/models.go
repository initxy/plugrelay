package openairesponses

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/initxy/plugrelay/pkg/ir"
)

// catalogBaseInstructions is the system prompt the coding-agent catalog
// format requires for every model. The client sends it as the request's
// instructions, so it must describe the agent loop generically: the model
// behind the relay may be any vendor's. It is original text, not copied
// from any client.
const catalogBaseInstructions = `You are a coding agent working in the user's terminal and workspace.

- Read the relevant code before changing it, and keep changes focused on what was asked.
- Use the tools you are given to inspect files, run commands and apply edits; do not invent their output.
- Prefer small, verifiable steps. After editing, run the project's tests or the command the user cares about when it is safe to do so.
- Do not run destructive commands or touch files outside the task without asking.
- When you finish, say briefly what you changed and how you checked it. If something failed or was skipped, say so plainly.`

// catalogContextFallback is used when a model declares no context window;
// the catalog format requires one.
const catalogContextFallback = 128000

var effortDescriptions = map[string]string{
	"none":    "No reasoning, fastest responses",
	"minimal": "Minimal reasoning for the fastest responses",
	"low":     "Fast responses with lighter reasoning",
	"medium":  "Balances speed and reasoning depth for everyday tasks",
	"high":    "Greater reasoning depth for complex problems",
	"xhigh":   "Extra high reasoning depth for complex problems",
	"max":     "Maximum reasoning depth",
}

// EncodeModels renders the model list. A request carrying a client_version
// query parameter gets the coding-agent catalog format; others get the
// OpenAI list format.
func (Codec) EncodeModels(r *http.Request, models []ir.ModelInfo) (string, []byte) {
	if r != nil && r.URL != nil && r.URL.Query().Has("client_version") {
		return "application/json", catalogModels(models)
	}
	data := make([]json.RawMessage, 0, len(models))
	for _, m := range models {
		o := newObject()
		o.str("id", m.ID)
		o.str("object", "model")
		o.num("created", m.Created)
		owner := m.OwnedBy
		if owner == "" {
			owner = "system"
		}
		o.str("owned_by", owner)
		if m.DisplayName != "" {
			o.str("display_name", m.DisplayName)
		}
		if m.ContextWindow > 0 {
			o.num("context_window", int64(m.ContextWindow))
		}
		if m.MaxOutput > 0 {
			o.num("max_output_tokens", int64(m.MaxOutput))
		}
		data = append(data, o.raw())
	}
	o := newObject()
	o.str("object", "list")
	o.set("data", jarray(data))
	return "application/json", o.bytes()
}

func catalogModels(models []ir.ModelInfo) []byte {
	list := make([]json.RawMessage, 0, len(models))
	for i, m := range models {
		o := newObject()
		o.str("slug", m.ID)
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		o.str("display_name", name)
		o.str("description", name)
		levels := []json.RawMessage{}
		if m.Reasoning {
			for _, e := range m.Efforts {
				l := newObject()
				l.str("effort", e)
				d := effortDescriptions[e]
				if d == "" {
					d = e
				}
				l.str("description", d)
				levels = append(levels, l.raw())
			}
			if def := defaultEffort(m.Efforts); def != "" {
				o.str("default_reasoning_level", def)
			}
		}
		o.set("supported_reasoning_levels", jarray(levels))
		o.str("shell_type", "shell_command")
		o.str("base_instructions", catalogBaseInstructions)
		o.boolean("support_verbosity", false)
		o.str("visibility", "list")
		o.boolean("supported_in_api", true)
		o.num("priority", int64(i+1))
		o.str("default_reasoning_summary", "none")
		o.str("apply_patch_tool_type", "freeform")
		tp := newObject()
		tp.str("mode", "tokens")
		tp.num("limit", 10000)
		o.set("truncation_policy", tp.raw())
		o.boolean("supports_parallel_tool_calls", m.Tools)
		cw := m.ContextWindow
		if cw <= 0 {
			cw = catalogContextFallback
		}
		o.num("context_window", int64(cw))
		o.num("max_context_window", int64(cw))
		o.set("experimental_supported_tools", json.RawMessage("[]"))
		mods := []string{"text"}
		if m.Vision {
			mods = append(mods, "image")
		}
		o.any("input_modalities", mods)
		list = append(list, o.raw())
	}
	o := newObject()
	o.set("models", jarray(list))
	return o.bytes()
}

func defaultEffort(efforts []string) string {
	for _, e := range []string{"medium", "high", "low"} {
		if slices.Contains(efforts, e) {
			return e
		}
	}
	if len(efforts) > 0 {
		return efforts[0]
	}
	return ""
}
