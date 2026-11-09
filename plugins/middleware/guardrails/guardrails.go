// Package guardrails is the optional middleware.guardrails plugin: request
// and response checks with one of three actions.
//
// Request checks (OnRequest): blocked models, blocked tools (offered in the
// request or called in its history), a maximum prompt size in characters,
// and content rules over all client-supplied text (system prompts, every
// turn, tool calls and results) — built-in detectors for personal data and
// secrets (plugins/internal/textscan), deny keywords and deny patterns —
// plus an optional external moderation call on the user turns. Response
// checks run the content rules over complete responses (OnResponse) and
// over streams (OnEvent) in a bounded window.
//
// Actions for content rules: block (the client gets its protocol's
// content-filter error), redact (matches in user turns become
// "[REDACTED:<rule>]" through MutateBody / OnAttempt, matches elsewhere in
// the request are flagged; complete responses are masked in place) or flag
// (recorded only). Blocked models, tools and oversized prompts are always
// refused. A stream cannot be redacted once bytes are out (and passthrough
// is byte-exact), so redact acts as flag there. On block, streamed text
// that may still be part of a match is held back (plugin.EventHolder) and
// the stream stops with the protocol's error event as soon as a match
// appears, so no part of a match no longer than the window leaves.
//
// On WebSocket sessions the core hands the same hooks the text of each
// client event and the output text deltas; audio cannot be read, and
// block_unscannable_audio refuses it when a blocking rule is on.
//
// Every verdict is noted on the request log ("guardrails: <action>
// <phase> <rules>") and counted in plugrelay_guardrail_verdicts_total.
//
// Layered settings: deny lists and detectors merge as a union (a group or
// key can add rules, never drop one), max_prompt_chars takes the smallest
// limit, and the action and moderation choices take the nearest layer.
package guardrails

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/match"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/textscan"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tidwall/gjson"
)

// ID is the plugin id.
const ID = "middleware.guardrails"

func init() { plugin.Register(ID, func() plugin.Plugin { return New() }) }

// Actions.
const (
	ActionBlock  = "block"
	ActionRedact = "redact"
	ActionFlag   = "flag"
)

// Phases a verdict is noted under.
const (
	PhaseRequest  = "request"
	PhaseResponse = "response"
	PhaseStream   = "stream"
)

// Rule names besides the detectors.
const (
	RuleKeyword    = "keyword"
	RulePattern    = "pattern"
	RuleModel      = "model"
	RuleTool       = "tool"
	RulePromptSize = "prompt_size"
	RuleModeration = "moderation"
	RuleAudio      = "audio"
)

// DefaultWindow is the stream window in bytes.
const DefaultWindow = 2048

// moderationTimeout bounds one moderation call.
const moderationTimeout = 10 * time.Second

// UpstreamCaller is implemented by a Host that can send a request to a
// configured upstream with one of its credentials, the provider's auth and
// the credential's egress. path is appended to the base URL of the
// upstream's first endpoint.
type UpstreamCaller interface {
	CallUpstream(ctx context.Context, upstreamID, method, path string, body []byte) (status int, resp []byte, err error)
}

// Guard implements RequestHook, AttemptHook, BodyMutator, ResponseHook and
// EventHook, and is a prometheus.Collector for its verdict counter.
type Guard struct {
	upstreams UpstreamCaller
	verdicts  *prometheus.CounterVec

	mu        sync.Mutex
	moderated map[string]*sessionCount // per WebSocket session: events sent to moderation
	pruned    time.Time
}

type sessionCount struct {
	n    int64
	last time.Time
}

// moderatedSessionTTL forgets a session's moderation count this long after
// its last moderated event.
const moderatedSessionTTL = 6 * time.Hour

// moderatedEvent counts one more event of session sent to moderation and
// returns the count.
func (g *Guard) moderatedEvent(session string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.moderated == nil {
		g.moderated = map[string]*sessionCount{}
	}
	if now.Sub(g.pruned) > time.Minute {
		for k, v := range g.moderated {
			if now.Sub(v.last) > moderatedSessionTTL {
				delete(g.moderated, k)
			}
		}
		g.pruned = now
	}
	sc := g.moderated[session]
	if sc == nil {
		sc = &sessionCount{}
		g.moderated[session] = sc
	}
	sc.n++
	sc.last = now
	return sc.n
}

// New returns the plugin.
func New() *Guard {
	return &Guard{verdicts: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "plugrelay_guardrail_verdicts_total",
		Help: "Guardrail verdicts by phase (request, response, stream), action (block, redact, flag) and rule."},
		[]string{"phase", "action", "rule"})}
}

// Info implements plugin.Plugin.
func (*Guard) Info() plugin.Info {
	all := plugin.Global | plugin.Group | plugin.User | plugin.Key
	return plugin.Info{
		ID:          ID,
		Kind:        plugin.KindMiddleware,
		Version:     "1.0.0",
		Description: "Request and response guardrails: deny lists, PII and secret detection, size and model/tool limits, moderation.",
		FailPolicy:  plugin.FailClosed,
		Mutates:     textscan.UserTextFields,
		Settings: []plugin.Setting{
			{Key: "detect", Type: plugin.TypeList, Scopes: all, Merge: plugin.Union, Enum: textscan.Detectors,
				EnumLabels: textscan.DetectorLabels, Label: plugin.T("Look for", "要识别的内容"),
				Help: plugin.T("Personal data and secrets found in requests and responses; numbers count only when their check digits hold.",
					"在请求和回复里找这些个人信息和密钥；号码类只有校验位对得上才算。")},
			{Key: "deny_keywords", Type: plugin.TypeList, Scopes: all, Merge: plugin.Union,
				Label: plugin.T("Blocked words", "敏感词"),
				Help: plugin.T("Words or phrases anywhere in the text; case, zero-width characters and full-width letters are ignored.",
					"出现在文本任何位置都算；不区分大小写，忽略零宽字符和全角字母。")},
			{Key: "deny_patterns", Type: plugin.TypeList, Scopes: all, Merge: plugin.Union,
				Label: plugin.T("Blocked patterns", "敏感内容规则（正则）"),
				Help:  plugin.T("Regular expressions (RE2 syntax).", "正则表达式，RE2 语法。")},
			{Key: "action", Type: plugin.TypeString, Default: ActionBlock, Scopes: all, Merge: plugin.Override,
				Enum: []string{ActionBlock, ActionRedact, ActionFlag}, Label: plugin.T("When something is found", "发现后怎么办"),
				Help: plugin.T("Refuse the request, mask the match, or only note it in the request log.",
					"拒绝请求、把命中的内容遮掉，或者只在请求日志里记一笔。"),
				EnumLabels: map[string]plugin.Text{ActionBlock: plugin.T("Refuse", "拒绝"), ActionRedact: plugin.T("Mask it", "遮掉"),
					ActionFlag: plugin.T("Only note it", "只记录")}},
			{Key: "check_response", Type: plugin.TypeBool, Default: true, Scopes: all, Merge: plugin.Override,
				Label: plugin.T("Check replies too", "也检查模型的回复"),
				Help: plugin.T("Streams are checked in a sliding window as they arrive.",
					"流式回复边收边查，用一段滑动窗口匹配。")},
			{Key: "max_prompt_chars", Type: plugin.TypeInt, Default: 0, Scopes: all, Merge: plugin.Strictest,
				Label: plugin.T("Longest prompt (characters)", "提示词最长字符数"),
				Help: plugin.T("System prompt, messages and tool results together; 0 means no limit.",
					"系统提示、对话和工具结果加起来；0 表示不限。")},
			{Key: "blocked_models", Type: plugin.TypeList, Scopes: all, Merge: plugin.Union,
				Label: plugin.T("Blocked models", "禁用的模型"),
				Help:  plugin.T("Model names; * matches anything.", "填模型名，可用 * 通配。")},
			{Key: "blocked_tools", Type: plugin.TypeList, Scopes: all, Merge: plugin.Union,
				Label: plugin.T("Blocked tools", "禁用的工具"),
				Help: plugin.T("Tool names; * matches anything. Refused when offered or found in the conversation.",
					"填工具名，可用 * 通配；请求里提供了或者对话里调用过都会被拒绝。")},
			{Key: "stream_window", Type: plugin.TypeInt, Default: DefaultWindow, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Streaming check window (bytes)", "流式检查窗口（字节）"),
				Help: plugin.T("How much streamed text is kept to catch matches split across chunks, at most 16384. "+
					"When refusing with patterns or detectors, streamed text is held back this far.",
					"为了发现被拆到几段里的内容，最多保留多少流式文本，上限 16384。"+
						"设为拒绝且用了规则或识别项时，流式输出会因此晚这么多再发给客户端。")},
			{Key: "block_unscannable_audio", Type: plugin.TypeBool, Default: false, Scopes: all, Merge: plugin.Override,
				Label: plugin.T("Refuse audio that cannot be checked", "拒绝无法检查的语音输入"),
				Help: plugin.T("Realtime voice input cannot be read by the rules; refuse it while refusing is on and any rule or moderation is set.",
					"实时语音的内容规则读不到；在设为拒绝、并且配了规则或审核时，直接拒绝语音输入。")},
			{Key: "moderation_upstream", Type: plugin.TypeUpstream, Scopes: plugin.Global | plugin.Group, Merge: plugin.Override,
				Label: plugin.T("Moderation service", "内容审核服务"),
				Help: plugin.T("An upstream with an OpenAI-style moderation API that checks what users write; empty turns moderation off.",
					"选一个提供 OpenAI 式审核接口的上游，用来审核用户输入；留空就不审核。")},
			{Key: "moderation_model", Type: plugin.TypeString, Default: "omni-moderation-latest", Scopes: plugin.Global | plugin.Group,
				Merge: plugin.Override, Label: plugin.T("Moderation model", "审核用的模型"),
				Help: plugin.T("Model name sent to the moderation service.", "发给审核服务的模型名。")},
			{Key: "max_moderated_events", Type: plugin.TypeInt, Default: DefaultMaxModeratedEvents, Scopes: plugin.Global | plugin.Group,
				Merge: plugin.Strictest, Label: plugin.T("Most moderated messages per voice session", "每个实时会话最多审核几条消息"),
				Help: plugin.T("Past this, moderation counts as failed and the rule below decides.",
					"超过后按审核失败处理，由下面的失败规则决定放不放行。")},
			{Key: "moderation_fail", Type: plugin.TypeString, Default: "closed", Scopes: plugin.Global | plugin.Group,
				Merge: plugin.Override, Enum: []string{"open", "closed"},
				Label: plugin.T("If moderation fails", "审核服务出错时"),
				Help: plugin.T("Refuse the request, or let it through and note it in the request log.",
					"拒绝请求，或者放行并在请求日志里记一笔。"),
				EnumLabels: map[string]plugin.Text{"closed": plugin.T("Refuse", "拒绝"), "open": plugin.T("Let it through", "放行")}},
		},
	}
}

// Init implements plugin.Initializer.
func (g *Guard) Init(_ context.Context, h plugin.Host) error {
	g.upstreams, _ = h.(UpstreamCaller)
	return nil
}

// SetUpstreams sets the moderation caller (tests).
func (g *Guard) SetUpstreams(u UpstreamCaller) { g.upstreams = u }

// Describe implements prometheus.Collector.
func (g *Guard) Describe(ch chan<- *prometheus.Desc) { g.verdicts.Describe(ch) }

// Collect implements prometheus.Collector.
func (g *Guard) Collect(ch chan<- prometheus.Metric) { g.verdicts.Collect(ch) }

// contentRules compiles the caller's detectors, keywords and patterns.
func contentRules(c *plugin.Call) ([]textscan.Rule, error) {
	var out []textscan.Rule
	for _, d := range c.Setting("detect").List() {
		if r, ok := textscan.Builtin(d); ok {
			out = append(out, r)
		}
	}
	if r, ok := textscan.Keywords(RuleKeyword, c.Setting("deny_keywords").List()); ok {
		out = append(out, r)
	}
	for i, p := range c.Setting("deny_patterns").List() {
		r, err := textscan.Pattern(RulePattern, p)
		if err != nil {
			return nil, fmt.Errorf("%w: guardrails: deny_patterns %d: %v", plugin.ErrPluginFailure, i, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func action(c *plugin.Call) string {
	switch a := c.Setting("action").String(); a {
	case ActionRedact, ActionFlag:
		return a
	}
	return ActionBlock
}

// verdict notes and counts one decision.
func (g *Guard) verdict(c *plugin.Call, phase, act string, rules []string) {
	rules = uniq(rules)
	c.Note("guardrails: " + act + " " + phase + " " + strings.Join(rules, ","))
	for _, r := range rules {
		g.verdicts.WithLabelValues(phase, act, r).Inc()
	}
}

func uniq(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func ruleNames(ms []textscan.Match) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Rule)
	}
	return uniq(out)
}

func blocked(phase string, rules []string) *ir.GwError {
	return ir.Errorf(ir.ErrContentFiltered, "%s blocked by guardrails (%s)", phase, strings.Join(rules, ", "))
}

// OnRequest implements plugin.RequestHook.
func (g *Guard) OnRequest(c *plugin.Call) (*plugin.Reply, error) {
	if pats := c.Setting("blocked_models").List(); len(pats) > 0 && match.Any(c.Model, pats) {
		g.verdict(c, PhaseRequest, ActionBlock, []string{RuleModel})
		return &plugin.Reply{Error: ir.Errorf(ir.ErrPermission, "model %s is blocked by guardrails", c.Model)}, nil
	}
	if c.UnscannedAudio && c.Setting("block_unscannable_audio").Bool() && action(c) == ActionBlock {
		rules, err := contentRules(c)
		if err != nil {
			return nil, err
		}
		if len(rules) > 0 || c.Setting("moderation_upstream").String() != "" {
			g.verdict(c, PhaseRequest, ActionBlock, []string{RuleAudio})
			return &plugin.Reply{Error: ir.Errorf(ir.ErrContentFiltered, "audio input cannot be checked by guardrails and is refused")}, nil
		}
	}
	req, err := c.Request()
	if err != nil || req == nil {
		return nil, nil // nothing decodable to inspect (media uploads)
	}
	if pats := c.Setting("blocked_tools").List(); len(pats) > 0 {
		if name := blockedTool(req, pats); name != "" {
			g.verdict(c, PhaseRequest, ActionBlock, []string{RuleTool})
			return &plugin.Reply{Error: ir.Errorf(ir.ErrPermission, "tool %s is blocked by guardrails", name)}, nil
		}
	}
	if max := c.Setting("max_prompt_chars").Int(); max > 0 {
		if n := promptChars(req); n > max {
			g.verdict(c, PhaseRequest, ActionBlock, []string{RulePromptSize})
			return &plugin.Reply{Error: ir.Errorf(ir.ErrInvalidRequest, "prompt of %d characters exceeds the guardrails limit of %d", n, max)}, nil
		}
	}
	rules, err := contentRules(c)
	if err != nil {
		return nil, err
	}
	texts, other := requestTexts(req)
	if len(rules) > 0 {
		find := func(ts []string) (hits []textscan.Match) {
			for _, t := range ts {
				hits = append(hits, textscan.Find(rules, t)...)
			}
			return hits
		}
		userHits, otherHits := find(texts), find(other)
		act := action(c)
		if act == ActionRedact {
			// Only user text is masked; a hit anywhere else is flagged.
			if len(otherHits) > 0 {
				g.verdict(c, PhaseRequest, ActionFlag, ruleNames(otherHits))
			}
			if len(userHits) > 0 {
				g.verdict(c, PhaseRequest, ActionRedact, ruleNames(userHits))
				c.Set("redact", true)
			}
		} else if hits := append(userHits, otherHits...); len(hits) > 0 {
			names := ruleNames(hits)
			g.verdict(c, PhaseRequest, act, names)
			if act == ActionBlock {
				return &plugin.Reply{Error: blocked("request", names)}, nil
			}
		}
	}
	if up := c.Setting("moderation_upstream").String(); up != "" && len(texts) > 0 {
		if reply := g.moderate(c, up, texts); reply != nil {
			return reply, nil
		}
	}
	return nil, nil
}

// MaxModeratedBatchRequests bounds the moderation calls a batch being
// created makes, one per request: past it, moderation counts as failed.
const MaxModeratedBatchRequests = 100

// DefaultMaxModeratedEvents bounds the moderation calls of one Realtime
// session, one per client event whose user text is sent to moderation.
const DefaultMaxModeratedEvents = 200

// moderate asks the moderation endpoint about the user's text.
func (g *Guard) moderate(c *plugin.Call, upstream string, texts []string) *plugin.Reply {
	var cats []string
	var err error
	overEvents := false
	if n := c.Setting("max_moderated_events").Int(); c.EventIndex > 0 && n > 0 {
		overEvents = g.moderatedEvent(c.SessionID) > n
	}
	if c.BatchIndex > MaxModeratedBatchRequests {
		err = fmt.Errorf("moderation checks at most %d requests of a batch", MaxModeratedBatchRequests)
	} else if overEvents {
		err = fmt.Errorf("moderation checks at most %d events of a session", c.Setting("max_moderated_events").Int())
	} else {
		cats, err = g.callModeration(c, upstream, texts)
	}
	if err != nil {
		if c.Setting("moderation_fail").String() == "open" {
			c.Note("guardrails: moderation unavailable, let through: " + err.Error())
			g.verdicts.WithLabelValues(PhaseRequest, "fail_open", RuleModeration).Inc()
			return nil
		}
		g.verdict(c, PhaseRequest, ActionBlock, []string{RuleModeration})
		if c.BatchIndex > MaxModeratedBatchRequests {
			return &plugin.Reply{Error: ir.Errorf(ir.ErrInvalidRequest, "%v; split the batch", err)}
		}
		if overEvents {
			return &plugin.Reply{Error: ir.Errorf(ir.ErrInvalidRequest, "%v; start a new session", err)}
		}
		return &plugin.Reply{Error: ir.Errorf(ir.ErrUpstream, "moderation check unavailable")}
	}
	if len(cats) == 0 {
		return nil
	}
	rules := make([]string, len(cats))
	for i, cat := range cats {
		rules[i] = RuleModeration + ":" + cat
	}
	act := action(c)
	if act == ActionRedact {
		act = ActionBlock // a moderation verdict has no span to mask
	}
	g.verdict(c, PhaseRequest, act, rules)
	if act == ActionBlock {
		return &plugin.Reply{Error: blocked("request", rules)}
	}
	return nil
}

func (g *Guard) callModeration(c *plugin.Call, upstream string, texts []string) ([]string, error) {
	if g.upstreams == nil {
		return nil, errors.New("no upstream caller")
	}
	body, _ := json.Marshal(map[string]any{"model": c.Setting("moderation_model").String(), "input": texts})
	ctx, cancel := context.WithTimeout(c.Context, moderationTimeout)
	defer cancel()
	status, resp, err := g.upstreams.CallUpstream(ctx, upstream, http.MethodPost, "/moderations", body)
	if err != nil {
		return nil, plugin.RedactURLError(err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("moderation endpoint answered %d", status)
	}
	res := gjson.GetBytes(resp, "results")
	if !res.IsArray() {
		return nil, errors.New("moderation endpoint returned no results")
	}
	var cats []string
	res.ForEach(func(_, r gjson.Result) bool {
		if !r.Get("flagged").Bool() {
			return true
		}
		found := false
		r.Get("categories").ForEach(func(k, v gjson.Result) bool {
			if v.Bool() {
				cats, found = append(cats, k.String()), true
			}
			return true
		})
		if !found {
			cats = append(cats, "flagged")
		}
		return true
	})
	slices.Sort(cats)
	return slices.Compact(cats), nil
}

// blockedTool returns the first tool offered or called that a pattern blocks.
func blockedTool(req *ir.Request, pats []string) string {
	for _, t := range req.Tools {
		if name := toolName(t.Namespace, t.Name); match.Any(name, pats) || match.Any(t.Name, pats) {
			return name
		}
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Tool != nil && (match.Any(b.Tool.Name, pats)) {
				return b.Tool.Name
			}
		}
	}
	return ""
}

func toolName(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "__" + name
}

// promptChars counts the characters of everything the model reads.
func promptChars(req *ir.Request) int64 {
	var n int64
	count := func(bs []ir.Block) {
		for _, b := range bs {
			n += int64(utf8.RuneCountInString(b.Text))
			if b.Tool != nil {
				n += int64(utf8.RuneCount(b.Tool.Input)) + int64(utf8.RuneCountInString(b.Tool.Text))
			}
			if b.Result != nil {
				for _, rb := range b.Result.Content {
					n += int64(utf8.RuneCountInString(rb.Text))
				}
			}
		}
	}
	count(req.System)
	for _, m := range req.Messages {
		count(m.Content)
	}
	return n
}

// requestTexts splits the client-supplied text of a request: user is the
// text blocks of user turns (what redact masks and moderation checks),
// other is everything else the model reads — system prompts, other roles'
// turns, thinking, tool calls' arguments and tool results.
func requestTexts(req *ir.Request) (user, other []string) {
	add := func(dst *[]string, s string) {
		if s != "" {
			*dst = append(*dst, s)
		}
	}
	walk := func(bs []ir.Block, isUser bool) {
		for _, b := range bs {
			if isUser && b.Type == ir.BlockText {
				add(&user, b.Text)
			} else {
				add(&other, b.Text)
			}
			if b.Tool != nil {
				add(&other, string(b.Tool.Input))
				add(&other, b.Tool.Text)
			}
			if b.Result != nil {
				for _, rb := range b.Result.Content {
					add(&other, rb.Text)
				}
			}
		}
	}
	walk(req.System, false)
	for _, m := range req.Messages {
		walk(m.Content, m.Role == ir.RoleUser)
	}
	return user, other
}

func (g *Guard) redacting(c *plugin.Call) ([]textscan.Rule, bool, error) {
	if v, _ := c.Get("redact"); v != true {
		return nil, false, nil
	}
	rules, err := contentRules(c)
	return rules, err == nil && len(rules) > 0, err
}

// OnAttempt implements plugin.AttemptHook: masks user text in the convert tier.
func (g *Guard) OnAttempt(c *plugin.Call, a *plugin.Attempt) error {
	if a.Tier != plugin.TierConvert {
		return nil
	}
	rules, ok, err := g.redacting(c)
	if !ok {
		return err
	}
	req, err := c.Request()
	if err != nil {
		return err
	}
	textscan.MaskRequest(req, func(s string) string { return textscan.Mask(rules, s) })
	return nil
}

// MutateBody implements plugin.BodyMutator: masks user text in the patch tier.
func (g *Guard) MutateBody(c *plugin.Call, a *plugin.Attempt, body []byte) ([]byte, error) {
	rules, ok, err := g.redacting(c)
	if !ok {
		return body, err
	}
	return textscan.MaskBody(body, plugin.BodyProtocol(c, a), func(s string) string { return textscan.Mask(rules, s) })
}

// OnResponse implements plugin.ResponseHook.
func (g *Guard) OnResponse(c *plugin.Call, r *ir.Response) (bool, error) {
	if !c.Setting("check_response").Bool() {
		return false, nil
	}
	rules, err := contentRules(c)
	if err != nil || len(rules) == 0 {
		return false, err
	}
	var hits []textscan.Match
	for _, b := range r.Content {
		if b.Type == ir.BlockText {
			hits = append(hits, textscan.Find(rules, b.Text)...)
		}
	}
	if len(hits) == 0 {
		return false, nil
	}
	names, act := ruleNames(hits), action(c)
	g.verdict(c, PhaseResponse, act, names)
	switch act {
	case ActionBlock:
		return false, blocked("response", names)
	case ActionRedact:
		for i := range r.Content {
			if b := &r.Content[i]; b.Type == ir.BlockText {
				b.Text = textscan.Mask(rules, b.Text)
			}
		}
		return true, nil
	}
	return false, nil
}

// window is the streamed text kept for matching across chunks.
type window struct {
	buf     []byte
	base    int // offset of buf[0] in the whole stream
	seenEnd int // matches ending at or before this were handled
	noted   map[string]bool
	rules   []textscan.Rule
	act     string
	max     int
	off     bool
	events  int      // events seen
	held    []textEv // block mode: text events not yet cleared, oldest first
}

// textEv is one streamed text event: its number among the events seen and
// the stream offset its text ends at.
type textEv struct{ n, end int }

// OnEvent implements plugin.EventHook. In block mode, text that might still
// turn out to be part of a match is held back (Held) until more text clears
// it, so a blocked match never reaches the client.
func (g *Guard) OnEvent(c *plugin.Call, ev *ir.Event) (plugin.EventAction, error) {
	w := g.window(c)
	w.events++
	if w.off {
		return plugin.EventPass, nil
	}
	block := w.act == ActionBlock
	final := false
	switch {
	case ev.Kind == ir.EvDelta && ev.Delta != nil && ev.Delta.Kind == ir.DeltaText && ev.Delta.Text != "":
		w.buf = append(w.buf, ev.Delta.Text...)
		if block {
			w.held = append(w.held, textEv{n: w.events, end: w.base + len(w.buf)})
		}
	case ev.Kind == ir.EvBlockStop || ev.Kind == ir.EvDone:
		final = true
	default:
		return plugin.EventPass, nil
	}
	var fresh []textscan.Match
	pending := -1 // stream offset of the earliest match still undecided
	for _, m := range textscan.Find(w.rules, string(w.buf)) {
		// A match touching the end may grow, or stop matching (a number
		// that goes on past a \b): flag waits for more text or the end;
		// block stops now unless the next text could undo the match, and
		// holds it meanwhile.
		if !final && m.End >= len(w.buf) && (!block || !settled(w.rules, string(w.buf), m)) {
			if pending < 0 || w.base+m.Start < pending {
				pending = w.base + m.Start
			}
			continue
		}
		if w.base+m.End > w.seenEnd {
			fresh = append(fresh, m)
		}
	}
	for _, m := range fresh {
		w.seenEnd = max(w.seenEnd, w.base+m.End)
	}
	if final {
		w.seenEnd = max(w.seenEnd, w.base+len(w.buf))
	}
	if over := len(w.buf) - w.max; over > 0 {
		for over < len(w.buf) && !utf8.RuneStart(w.buf[over]) {
			over++
		}
		w.buf = bytes.Clone(w.buf[over:])
		w.base += over
	}
	if block && len(fresh) == 0 {
		w.release(final, pending)
	}
	if len(fresh) == 0 {
		return plugin.EventPass, nil
	}
	var names []string
	for _, n := range ruleNames(fresh) {
		if !w.noted[n] {
			w.noted[n] = true
			names = append(names, n)
		}
	}
	if block {
		g.verdict(c, PhaseStream, ActionBlock, ruleNames(fresh))
		return plugin.EventPass, blocked("response", ruleNames(fresh))
	}
	if len(names) > 0 {
		g.verdict(c, PhaseStream, ActionFlag, names)
	}
	return plugin.EventPass, nil
}

// settled reports whether m, a match touching the end of text, stays a
// match of its rule whatever text comes next, judged by a few probes: a
// letter, a digit, punctuation and a space.
func settled(rules []textscan.Rule, text string, m textscan.Match) bool {
	for _, p := range []string{"a", "0", "-", ".", "@", " "} {
		if !slices.ContainsFunc(textscan.Find(rules, text+p), func(n textscan.Match) bool {
			return n.Rule == m.Rule && n.Start <= m.Start && n.End >= m.End
		}) {
			return false
		}
	}
	return true
}

// release clears the held text events that no match can reach any more:
// everything before the tail that could start a keyword, or, with patterns
// and detectors, everything that left the window.
// pending, when not negative, is the start of an undecided match.
func (w *window) release(final bool, pending int) {
	safe := w.base + len(w.buf)
	if !final {
		tail, _ := textscan.HoldBack(w.rules, string(w.buf))
		safe -= tail
		if pending >= 0 {
			safe = min(safe, pending)
		}
	}
	i := 0
	for i < len(w.held) && w.held[i].end <= safe {
		i++
	}
	w.held = w.held[i:]
}

// Held implements plugin.EventHolder: the events from the oldest held text
// event on stay back.
func (g *Guard) Held(c *plugin.Call) int {
	w := g.window(c)
	if len(w.held) == 0 {
		return 0
	}
	return w.events - w.held[0].n + 1
}

// MaxWindow caps stream_window: every streamed event rescans the window.
const MaxWindow = 16 << 10

func (g *Guard) window(c *plugin.Call) *window {
	if v, ok := c.Get("window"); ok {
		return v.(*window)
	}
	w := &window{noted: map[string]bool{}, act: action(c), max: int(c.Setting("stream_window").Int())}
	if w.max <= 0 {
		w.max = DefaultWindow
	}
	w.max = min(w.max, MaxWindow)
	rules, err := contentRules(c)
	w.rules = rules
	w.off = err != nil || len(rules) == 0 || !c.Setting("check_response").Bool()
	c.Set("window", w)
	return w
}
