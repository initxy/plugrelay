// Package parampolicy is the middleware.param-policy plugin: it caps the
// output-token limit and reasoning effort of a request and strips
// parameters the operator does not want forwarded.
//
// The same policy is applied on two paths and both derive from one policy
// value built from the merged settings:
//
//   - Patch tier: MutateBody edits the outgoing JSON with sjson, touching
//     only the protocol's own field names (all listed in Info.Mutates), so
//     every other byte, field order and cache_control stays as sent.
//   - Convert tier: OnAttempt edits the decoded ir.Request from
//     Call.Request() before the core encodes it for the upstream.
//
// Every edit only lowers a value or removes a field, so applying the
// policy twice (on retries, or on both paths) gives the same result.
//
// A missing output limit is filled in with the cap, so the cap also binds
// clients that rely on the upstream default. A missing effort is left alone.
//
// OpenAI Realtime client events ("openai-realtime", the session or
// response object of session.update and response.create) only have their
// present values lowered ("inf" included) and parameters stripped: an
// update is partial, so filling a missing limit could raise one set
// earlier. The core binds the session default by sending the upstream a
// session.update it runs through here.
package parampolicy

import (
	"slices"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ID is the plugin id.
const ID = "middleware.param-policy"

func init() { plugin.Register(ID, func() plugin.Plugin { return New() }) }

// Protocol (codec) names this plugin knows the wire fields of.
const (
	protoAnthropic = "anthropic"
	protoChat      = "openai-chat"
	protoResponses = "openai-responses"
	protoText      = "openai-completions"
	protoGemini    = "gemini"
	protoRealtime  = "openai-realtime"
)

// realtimeObjects are the objects of Realtime client events this plugin
// edits, and realtimeLimits their output-limit fields (GA, beta).
var (
	realtimeObjects = map[string]string{"session.update": "session", "response.create": "response"}
	realtimeLimits  = []string{"max_output_tokens", "max_response_output_tokens"}
)

// Efforts lists reasoning effort levels from lowest to highest.
var Efforts = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// paramLabels names each strippable parameter by its wire name, which is
// what people know it by.
func paramLabels() map[string]plugin.Text {
	out := map[string]plugin.Text{}
	for _, p := range Strippable {
		out[p] = plugin.T(p, p)
	}
	return out
}

// Strippable lists the parameters strip_params may name. They are fixed so
// the plugin can declare every path it touches in Info.Mutates.
var Strippable = []string{
	"temperature", "top_p", "top_k", "frequency_penalty", "presence_penalty", "seed",
	"logit_bias", "logprobs", "top_logprobs", "stop", "service_tier", "store", "metadata",
	"user", "parallel_tool_calls", "prompt_cache_key", "safety_identifier",
}

// wire holds a protocol's field paths.
type wire struct {
	maxTokens []string // first one is used when the field is absent
	effort    string
	budget    string
	strip     map[string][]string // strip_params name → paths
}

func topLevel(extra map[string][]string) map[string][]string {
	m := map[string][]string{}
	for _, s := range Strippable {
		m[s] = []string{s}
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

var wires = map[string]wire{
	protoAnthropic: {maxTokens: []string{"max_tokens"}, effort: "output_config.effort", budget: "thinking.budget_tokens",
		strip: topLevel(map[string][]string{"stop": {"stop_sequences"}})},
	protoChat:      {maxTokens: []string{"max_tokens", "max_completion_tokens"}, effort: "reasoning_effort", strip: topLevel(nil)},
	protoResponses: {maxTokens: []string{"max_output_tokens"}, effort: "reasoning.effort", strip: topLevel(nil)},
	protoText:      {maxTokens: []string{"max_tokens"}, strip: topLevel(nil)},
	protoGemini: {maxTokens: []string{"generationConfig.maxOutputTokens"}, effort: "generationConfig.thinkingConfig.thinkingLevel",
		budget: "generationConfig.thinkingConfig.thinkingBudget",
		strip: map[string][]string{
			"temperature": {"generationConfig.temperature"}, "top_p": {"generationConfig.topP"},
			"top_k": {"generationConfig.topK"}, "seed": {"generationConfig.seed"},
			"stop": {"generationConfig.stopSequences"}, "presence_penalty": {"generationConfig.presencePenalty"},
			"frequency_penalty": {"generationConfig.frequencyPenalty"}, "logprobs": {"generationConfig.logprobs"},
		}},
}

// Policy implements plugin.AttemptHook and plugin.BodyMutator.
type Policy struct{}

// New returns the plugin.
func New() *Policy { return &Policy{} }

// Info implements plugin.Plugin.
func (*Policy) Info() plugin.Info {
	return plugin.Info{
		ID:          ID,
		Kind:        plugin.KindMiddleware,
		Version:     "1.0.0",
		Description: "Caps output tokens and reasoning effort; strips unwanted parameters.",
		FailPolicy:  plugin.FailClosed,
		Mutates:     mutates(),
		Settings: []plugin.Setting{
			{Key: "max_output_tokens", Type: plugin.TypeInt, Default: 0, Scopes: plugin.Global | plugin.Group | plugin.Key,
				Merge: plugin.Strictest, Label: plugin.T("Most output tokens per request", "单次请求最多输出 token 数"),
				Help: plugin.T("Lowers a larger output limit a client asks for; 0 means no cap.",
					"客户端要的输出上限比这个大时压到这个值；0 表示不限。")},
			{Key: "max_reasoning_effort", Type: plugin.TypeString, Scopes: plugin.Global | plugin.Group | plugin.Key,
				Merge: plugin.Override, Enum: Efforts, Label: plugin.T("Highest reasoning effort", "最高思考强度"),
				Help: plugin.T("A higher effort a client asks for is lowered to this; empty means no cap.",
					"客户端要的思考强度更高时降到这一档；留空表示不限。"),
				EnumLabels: map[string]plugin.Text{"minimal": plugin.T("Minimal", "最低"), "low": plugin.T("Low", "低"),
					"medium": plugin.T("Medium", "中"), "high": plugin.T("High", "高"), "xhigh": plugin.T("Very high", "很高"),
					"max": plugin.T("Maximum", "最高")}},
			{Key: "strip_params", Type: plugin.TypeList, Scopes: plugin.Global | plugin.Group,
				Merge: plugin.Override, Enum: Strippable, EnumLabels: paramLabels(),
				Label: plugin.T("Request parameters to remove", "发出前去掉的请求参数"),
				Help: plugin.T("These fields are taken out of every request before it goes upstream.",
					"请求发往上游前，把这些字段去掉。")},
		},
	}
}

func mutates() []string {
	var out []string
	for _, w := range wires {
		out = append(out, w.maxTokens...)
		out = append(out, w.effort)
		if w.budget != "" {
			out = append(out, w.budget)
		}
		for _, ps := range w.strip {
			out = append(out, ps...)
		}
	}
	for _, obj := range realtimeObjects {
		for _, f := range realtimeLimits {
			out = append(out, obj+"."+f)
		}
		out = append(out, obj+".reasoning.effort")
		for _, st := range Strippable {
			out = append(out, obj+"."+st)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// policy is the merged settings of one call.
type policy struct {
	maxTokens int64
	effort    string // "" = no cap
	strip     []string
}

func load(c *plugin.Call) policy {
	p := policy{maxTokens: c.Setting("max_output_tokens").Int(), strip: c.Setting("strip_params").List()}
	if e := c.Setting("max_reasoning_effort").String(); effortRank(e) >= 0 {
		p.effort = e
	}
	return p
}

func (p policy) empty() bool { return p.maxTokens <= 0 && p.effort == "" && len(p.strip) == 0 }

func effortRank(e string) int { return slices.Index(Efforts, e) }

// capEffort returns the effort to send, or "" when e is within the cap or
// not a known level.
func (p policy) capEffort(e string) string {
	if p.effort == "" {
		return ""
	}
	if r := effortRank(e); r > effortRank(p.effort) {
		return p.effort
	}
	return ""
}

// OnAttempt implements plugin.AttemptHook. It edits the IR on the convert
// tier only; the patch tier is handled by MutateBody and passthrough has
// no edits to make when no mutator runs.
func (pp *Policy) OnAttempt(c *plugin.Call, a *plugin.Attempt) error {
	if a.Tier != plugin.TierConvert {
		return nil
	}
	p := load(c)
	if p.empty() {
		return nil
	}
	req, err := c.Request()
	if err != nil {
		return err
	}
	p.applyIR(req, c.Protocol)
	return nil
}

func (p policy) applyIR(req *ir.Request, origin string) {
	if p.maxTokens > 0 {
		capped := int(p.maxTokens)
		if req.MaxOutputTokens == nil || *req.MaxOutputTokens > capped {
			req.MaxOutputTokens = &capped
		}
		if rs := req.Reasoning; rs != nil && rs.Budget != nil && *rs.Budget >= capped {
			b := max(capped-1, 0)
			rs.Budget = &b
		}
	}
	if rs := req.Reasoning; rs != nil {
		if e := p.capEffort(rs.Effort); e != "" {
			rs.Effort = e
		}
	}
	for _, s := range p.strip {
		switch s {
		case "temperature":
			req.Temperature = nil
		case "top_p":
			req.TopP = nil
		case "top_k":
			req.TopK = nil
		case "stop":
			req.Stop = nil
		case "service_tier":
			req.ServiceTier = ""
		case "store":
			req.Store = nil
		case "parallel_tool_calls":
			req.ParallelTools = nil
		case "prompt_cache_key":
			req.CacheKey = ""
		}
		// Fields without an IR slot ride in Extra under their wire names.
		if req.Extra.Fields != nil {
			for _, path := range wires[origin].strip[s] {
				delete(req.Extra.Fields, path)
			}
			delete(req.Extra.Fields, s)
		}
	}
}

// MutateBody implements plugin.BodyMutator for the patch tier.
func (pp *Policy) MutateBody(c *plugin.Call, a *plugin.Attempt, body []byte) ([]byte, error) {
	p := load(c)
	if p.empty() {
		return body, nil
	}
	proto := plugin.BodyProtocol(c, a)
	if proto == protoRealtime {
		return p.applyRealtime(body)
	}
	w, ok := wires[proto]
	if !ok {
		return body, nil
	}
	return p.applyBody(w, body)
}

// applyRealtime lowers and strips within the object a Realtime client
// event configures; other events pass unchanged.
func (p policy) applyRealtime(body []byte) ([]byte, error) {
	obj, ok := realtimeObjects[gjson.GetBytes(body, "type").Str]
	if !ok {
		return body, nil
	}
	var err error
	if p.maxTokens > 0 {
		for _, f := range realtimeLimits {
			v := gjson.GetBytes(body, obj+"."+f)
			if v.Exists() && (v.Type != gjson.Number || v.Int() > p.maxTokens) {
				if body, err = sjson.SetBytes(body, obj+"."+f, p.maxTokens); err != nil {
					return nil, err
				}
			}
		}
	}
	if v := gjson.GetBytes(body, obj+".reasoning.effort"); v.Type == gjson.String {
		if e := p.capEffort(v.Str); e != "" {
			if body, err = sjson.SetBytes(body, obj+".reasoning.effort", e); err != nil {
				return nil, err
			}
		}
	}
	for _, st := range p.strip {
		if gjson.GetBytes(body, obj+"."+st).Exists() {
			if body, err = sjson.DeleteBytes(body, obj+"."+st); err != nil {
				return nil, err
			}
		}
	}
	return body, nil
}

func (p policy) applyBody(w wire, body []byte) ([]byte, error) {
	var err error
	if p.maxTokens > 0 {
		present := false
		for _, path := range w.maxTokens {
			v := gjson.GetBytes(body, path)
			if !v.Exists() {
				continue
			}
			present = true
			if v.Int() > p.maxTokens {
				if body, err = sjson.SetBytes(body, path, p.maxTokens); err != nil {
					return nil, err
				}
			}
		}
		if !present {
			if body, err = sjson.SetBytes(body, w.maxTokens[0], p.maxTokens); err != nil {
				return nil, err
			}
		}
		if w.budget != "" {
			if b := gjson.GetBytes(body, w.budget); b.Exists() && b.Int() >= p.maxTokens {
				if body, err = sjson.SetBytes(body, w.budget, max(p.maxTokens-1, 0)); err != nil {
					return nil, err
				}
			}
		}
	}
	if w.effort != "" {
		if v := gjson.GetBytes(body, w.effort); v.Type == gjson.String {
			if e := p.capEffort(v.Str); e != "" {
				if body, err = sjson.SetBytes(body, w.effort, e); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, s := range p.strip {
		for _, path := range w.strip[s] {
			if gjson.GetBytes(body, path).Exists() {
				if body, err = sjson.DeleteBytes(body, path); err != nil {
					return nil, err
				}
			}
		}
	}
	return body, nil
}
