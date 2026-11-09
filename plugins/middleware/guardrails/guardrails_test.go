package guardrails

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/internal/settings"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
	"github.com/prometheus/client_golang/prometheus"
)

func userReq(texts ...string) *ir.Request {
	r := &ir.Request{Model: "m"}
	for _, t := range texts {
		r.Messages = append(r.Messages, ir.Message{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: t}}})
	}
	return r
}

func call(set testkit.Settings, req *ir.Request) *plugin.Call {
	if _, ok := set["check_response"]; !ok {
		set["check_response"] = true
	}
	return testkit.NewCall(ID, testkit.CallOpts{Protocol: "anthropic", Model: "m", Settings: set, Request: req})
}

func hasNote(c *plugin.Call, want string) bool { return slices.Contains(c.Notes(), want) }

func TestRequestActions(t *testing.T) {
	g := New()
	text := "mail a@b.io key sk-abcdefghijklmnopqrstuv"

	c := call(testkit.Settings{"detect": []string{"email", "secret"}}, userReq(text))
	reply, err := g.OnRequest(c)
	if err != nil || reply == nil || reply.Error.Kind != ir.ErrContentFiltered {
		t.Fatalf("block: %+v %v", reply, err)
	}
	if !strings.Contains(reply.Error.Message, "email, secret") || !hasNote(c, "guardrails: block request email,secret") {
		t.Fatalf("block verdict: %q %v", reply.Error.Message, c.Notes())
	}

	c = call(testkit.Settings{"detect": []string{"email"}, "action": "flag"}, userReq(text))
	if reply, err := g.OnRequest(c); err != nil || reply != nil || !hasNote(c, "guardrails: flag request email") {
		t.Fatalf("flag: %+v %v %v", reply, err, c.Notes())
	}
	body := []byte(`{"messages":[{"role":"user","content":"` + text + `"}]}`)
	if out, _ := g.MutateBody(c, &plugin.Attempt{Tier: plugin.TierPatch}, body); string(out) != string(body) {
		t.Fatal("flag edited the body")
	}

	req := userReq(text)
	c = call(testkit.Settings{"detect": []string{"email"}, "deny_keywords": []string{"KEY"}, "action": "redact"}, req)
	if reply, err := g.OnRequest(c); err != nil || reply != nil {
		t.Fatalf("redact: %+v %v", reply, err)
	}
	out, err := g.MutateBody(c, &plugin.Attempt{Tier: plugin.TierPatch}, body)
	if err != nil || string(out) != `{"messages":[{"role":"user","content":"mail [REDACTED:email] [REDACTED:keyword] sk-abcdefghijklmnopqrstuv"}]}` {
		t.Fatalf("redact patch: %s %v", out, err)
	}
	if err := g.OnAttempt(c, &plugin.Attempt{Tier: plugin.TierConvert}); err != nil || req.Messages[0].Content[0].Text != "mail [REDACTED:email] [REDACTED:keyword] sk-abcdefghijklmnopqrstuv" {
		t.Fatalf("redact convert: %q %v", req.Messages[0].Content[0].Text, err)
	}

	// Nothing configured: nothing happens.
	c = call(testkit.Settings{}, userReq(text))
	if reply, err := g.OnRequest(c); err != nil || reply != nil || len(c.Notes()) != 0 {
		t.Fatalf("no rules: %+v %v %v", reply, err, c.Notes())
	}
	// A broken pattern is a plugin failure (fail closed), not a refusal.
	c = call(testkit.Settings{"deny_patterns": []string{"("}}, userReq(text))
	if _, err := g.OnRequest(c); !plugin.IsFailure(err) {
		t.Fatalf("bad pattern: %v", err)
	}
	if got := counter(t, g, PhaseRequest, ActionBlock, "secret"); got != 1 {
		t.Fatalf("verdict counter %v", got)
	}
}

func TestLimits(t *testing.T) {
	g := New()
	c := testkit.NewCall(ID, testkit.CallOpts{Model: "gpt-5-mini", Settings: testkit.Settings{"blocked_models": []string{"gpt-5*"}}})
	if reply, _ := g.OnRequest(c); reply == nil || reply.Error.Kind != ir.ErrPermission {
		t.Fatalf("blocked model: %+v", reply)
	}
	req := userReq("hello")
	req.Tools = []ir.Tool{{Name: "Read"}, {Name: "Bash"}}
	c = call(testkit.Settings{"blocked_tools": []string{"bash"}}, req)
	if reply, _ := g.OnRequest(c); reply == nil || !strings.Contains(reply.Error.Message, "tool Bash") {
		t.Fatalf("blocked tool: %+v", reply)
	}
	req = userReq("hi")
	req.Messages = append(req.Messages, ir.Message{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse, Tool: &ir.ToolCall{Name: "web_fetch"}}}})
	c = call(testkit.Settings{"blocked_tools": []string{"web_*"}}, req)
	if reply, _ := g.OnRequest(c); reply == nil {
		t.Fatal("tool called in history not blocked")
	}
	req = userReq("12345", "6789")
	req.System = []ir.Block{{Type: ir.BlockText, Text: "sys"}}
	c = call(testkit.Settings{"max_prompt_chars": 11}, req)
	if reply, _ := g.OnRequest(c); reply == nil || reply.Error.Kind != ir.ErrInvalidRequest || !strings.Contains(reply.Error.Message, "12 characters") {
		t.Fatalf("prompt size: %+v", reply)
	}
	c = call(testkit.Settings{"max_prompt_chars": 12}, req)
	if reply, _ := g.OnRequest(c); reply != nil {
		t.Fatalf("prompt at the limit refused: %+v", reply)
	}
}

func TestResponseActions(t *testing.T) {
	g := New()
	resp := func() *ir.Response {
		return &ir.Response{Content: []ir.Block{{Type: ir.BlockText, Text: "reach me at x@y.io"}}}
	}
	c := call(testkit.Settings{"detect": []string{"email"}}, nil)
	var gw *ir.GwError
	if _, err := g.OnResponse(c, resp()); !errors.As(err, &gw) || gw.Kind != ir.ErrContentFiltered || plugin.IsFailure(err) {
		t.Fatalf("block: %v", err)
	}
	r := resp()
	c = call(testkit.Settings{"detect": []string{"email"}, "action": "redact"}, nil)
	if edited, err := g.OnResponse(c, r); err != nil || !edited || r.Content[0].Text != "reach me at [REDACTED:email]" {
		t.Fatalf("redact: %v %v %q", edited, err, r.Content[0].Text)
	}
	c = call(testkit.Settings{"detect": []string{"email"}, "check_response": false}, nil)
	if edited, err := g.OnResponse(c, resp()); err != nil || edited {
		t.Fatal("check_response off still checked")
	}
}

func delta(s string) *ir.Event {
	return &ir.Event{Kind: ir.EvDelta, Delta: &ir.Delta{Kind: ir.DeltaText, Text: s}}
}

func TestStreamWindow(t *testing.T) {
	g := New()
	// A secret split over three chunks blocks as soon as it matches; with
	// a detector every chunk is held back meanwhile, so none of it left.
	c := call(testkit.Settings{"detect": []string{"secret"}}, nil)
	for i, s := range []string{"here: sk-abcdef", "ghijklmno"} {
		if _, err := g.OnEvent(c, delta(s)); err != nil {
			t.Fatalf("blocked early at %q: %v", s, err)
		}
		if held := g.Held(c); held != i+1 {
			t.Fatalf("after %q held %d events", s, held)
		}
	}
	_, err := g.OnEvent(c, delta("pqrstuv"))
	var gw *ir.GwError
	if !errors.As(err, &gw) || gw.Kind != ir.ErrContentFiltered || !hasNote(c, "guardrails: block stream secret") {
		t.Fatalf("not cut: %v %v", err, c.Notes())
	}

	// A match at the very end blocks at once; flag waits for block stop.
	c = call(testkit.Settings{"detect": []string{"email"}}, nil)
	if _, err := g.OnEvent(c, delta("x@y.io")); err == nil {
		t.Fatal("trailing match missed")
	}
	c = call(testkit.Settings{"detect": []string{"email"}, "action": "flag"}, nil)
	_, _ = g.OnEvent(c, delta("x@y.io"))
	if _, err := g.OnEvent(c, &ir.Event{Kind: ir.EvBlockStop}); err != nil || !hasNote(c, "guardrails: flag stream email") {
		t.Fatalf("trailing flag: %v %v", err, c.Notes())
	}

	// Flag notes each rule once; the window stays bounded.
	c = call(testkit.Settings{"detect": []string{"email"}, "action": "flag", "stream_window": 64}, nil)
	for range 50 {
		if _, err := g.OnEvent(c, delta("mail a@b.io and some filler text here. ")); err != nil {
			t.Fatal(err)
		}
	}
	w, _ := c.Get("window")
	if n := len(w.(*window).buf); n > 64 {
		t.Fatalf("window grew to %d", n)
	}
	if got := c.Notes(); !slices.Equal(got, []string{"guardrails: flag stream email"}) {
		t.Fatalf("notes %v", got)
	}

	// Redact cannot apply to a stream: it is a flag.
	c = call(testkit.Settings{"detect": []string{"email"}, "action": "redact"}, nil)
	if _, err := g.OnEvent(c, delta("a@b.io ")); err != nil || !hasNote(c, "guardrails: flag stream email") {
		t.Fatalf("redact on stream: %v %v", err, c.Notes())
	}
	// Tool input and thinking are not scanned.
	c = call(testkit.Settings{"detect": []string{"email"}}, nil)
	if _, err := g.OnEvent(c, &ir.Event{Kind: ir.EvDelta, Delta: &ir.Delta{Kind: ir.DeltaToolInput, Text: `"a@b.io" `}}); err != nil {
		t.Fatal(err)
	}
}

// TestStreamBlockHoldsMatchingText: in block mode a keyword never leaves,
// whether it arrives in one frame or split over frames, and text that
// cannot start a keyword is released at once.
func TestStreamBlockHoldsMatchingText(t *testing.T) {
	g := New()
	set := testkit.Settings{"deny_keywords": []string{"forbidden-word"}}
	c := call(set, nil)
	if _, err := g.OnEvent(c, delta("say ")); err != nil || g.Held(c) != 0 {
		t.Fatalf("plain text: err %v, held %d", err, g.Held(c))
	}
	// The whole keyword in one frame, ending at its boundary: blocked
	// before that frame goes out.
	if _, err := g.OnEvent(c, delta("forbidden-word")); err == nil {
		t.Fatal("keyword at a frame boundary let through")
	}

	c = call(set, nil)
	for _, s := range []string{"say ", "forbid"} {
		if _, err := g.OnEvent(c, delta(s)); err != nil {
			t.Fatal(err)
		}
	}
	if held := g.Held(c); held != 1 {
		t.Fatalf("a keyword's start is not held: %d", held)
	}
	if _, err := g.OnEvent(c, delta("den-word ok")); err == nil {
		t.Fatal("split keyword let through")
	}

	// A start that does not continue is released by the next frame.
	c = call(set, nil)
	for _, s := range []string{"forbid", "s it"} {
		if _, err := g.OnEvent(c, delta(s)); err != nil {
			t.Fatal(err)
		}
	}
	if held := g.Held(c); held != 0 {
		t.Fatalf("cleared text still held: %d", held)
	}
	// Block stop releases everything.
	_, _ = g.OnEvent(c, delta(" forb"))
	if _, err := g.OnEvent(c, &ir.Event{Kind: ir.EvBlockStop}); err != nil || g.Held(c) != 0 {
		t.Fatalf("block stop: %v, held %d", err, g.Held(c))
	}
	// Flag mode holds nothing.
	c = call(testkit.Settings{"deny_keywords": []string{"forbidden-word"}, "action": "flag"}, nil)
	_, _ = g.OnEvent(c, delta("forbid"))
	if g.Held(c) != 0 {
		t.Fatal("flag mode held text")
	}
}

func TestStreamWindowCapped(t *testing.T) {
	g := New()
	c := call(testkit.Settings{"detect": []string{"email"}, "stream_window": 1 << 30}, nil)
	_, _ = g.OnEvent(c, delta("x"))
	if w, _ := c.Get("window"); w.(*window).max != MaxWindow {
		t.Fatalf("window %d", w.(*window).max)
	}
}

// TestLayeredSettings merges the plugin's declarations through the real
// resolver: deny lists and detectors add up across layers, the prompt
// limit takes the smallest value, the action the nearest layer, and an
// exempted key keeps only its own values.
func TestLayeredSettings(t *testing.T) {
	g := New()
	decls := map[string][]plugin.Setting{ID: g.Info().Settings}
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	entries := []settings.Entry{
		{Scope: plugin.Global, PluginID: ID, Key: "detect", Value: raw([]string{"email"})},
		{Scope: plugin.Group, ScopeID: "g1", PluginID: ID, Key: "detect", Value: raw([]string{"secret"})},
		{Scope: plugin.Key, ScopeID: "k1", PluginID: ID, Key: "detect", Value: raw([]string{})},
		{Scope: plugin.Global, PluginID: ID, Key: "max_prompt_chars", Value: raw(1000)},
		{Scope: plugin.Key, ScopeID: "k1", PluginID: ID, Key: "max_prompt_chars", Value: raw(5000)},
		{Scope: plugin.Group, ScopeID: "g1", PluginID: ID, Key: "action", Value: raw("flag")},
		{Scope: plugin.Key, ScopeID: "k1", PluginID: ID, Key: "action", Value: raw("block")},
	}
	r := settings.NewResolver(decls, entries, nil)
	caller := &plugin.Caller{KeyID: "k1", GroupID: "g1"}
	get := r.For(caller, nil)
	if got := get(ID, "detect").List(); !slices.Equal(got, []string{"email", "secret"}) {
		t.Errorf("detect = %v, want the union", got)
	}
	if got := get(ID, "max_prompt_chars").Int(); got != 1000 {
		t.Errorf("max_prompt_chars = %d, want the strictest", got)
	}
	if got := get(ID, "action").String(); got != "block" {
		t.Errorf("action = %q, want the key's override", got)
	}
	c := plugin.NewCall(context.Background(), "r", get, func() (*ir.Request, error) { return userReq("a@b.io"), nil }).WithPlugin(ID)
	c.Caller = caller
	if reply, _ := g.OnRequest(c); reply == nil {
		t.Error("global detector loosened by the key layer")
	}
	exempt := r.For(caller, settings.Exemptions{ID + "/detect"})
	if got := exempt(ID, "detect").List(); len(got) != 0 {
		t.Errorf("exempted key detect = %v, want its own empty list", got)
	}
}

type fakeUpstreams struct {
	url  string
	got  []string
	fail bool
}

func (f *fakeUpstreams) CallUpstream(ctx context.Context, id, method, path string, body []byte) (int, []byte, error) {
	f.got = append(f.got, id+" "+method+" "+path+" "+string(body))
	if f.fail {
		return 0, nil, errors.New("dial tcp: connection refused")
	}
	req, _ := http.NewRequestWithContext(ctx, method, f.url+path, strings.NewReader(string(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

func TestModeration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.URL.Path != "/moderations" || in.Model != "omni-moderation-latest" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if strings.Contains(strings.Join(in.Input, " "), "boom") {
			_, _ = io.WriteString(w, `{"results":[{"flagged":true,"categories":{"violence":true,"hate":false,"illicit":true}}]}`)
			return
		}
		if strings.Contains(strings.Join(in.Input, " "), "oops") {
			http.Error(w, "upstream down", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"flagged":false,"categories":{"violence":false}}]}`)
	}))
	defer srv.Close()
	fu := &fakeUpstreams{url: srv.URL}
	g := New()
	g.SetUpstreams(fu)
	set := func(extra testkit.Settings) testkit.Settings {
		s := testkit.Settings{"moderation_upstream": "openai", "moderation_model": "omni-moderation-latest", "moderation_fail": "closed"}
		for k, v := range extra {
			s[k] = v
		}
		return s
	}
	c := call(set(nil), userReq("make it go boom"))
	reply, _ := g.OnRequest(c)
	if reply == nil || reply.Error.Kind != ir.ErrContentFiltered || !hasNote(c, "guardrails: block request moderation:illicit,moderation:violence") {
		t.Fatalf("flagged: %+v %v", reply, c.Notes())
	}
	if len(fu.got) != 1 || !strings.HasPrefix(fu.got[0], "openai POST /moderations ") {
		t.Fatalf("calls %v", fu.got)
	}
	c = call(set(testkit.Settings{"action": "flag"}), userReq("boom"))
	if reply, _ := g.OnRequest(c); reply != nil || !hasNote(c, "guardrails: flag request moderation:illicit,moderation:violence") {
		t.Fatalf("flag: %+v %v", reply, c.Notes())
	}
	c = call(set(nil), userReq("hello"))
	if reply, _ := g.OnRequest(c); reply != nil || len(c.Notes()) != 0 {
		t.Fatalf("clean text: %+v", reply)
	}

	// Fail policy: an endpoint error refuses (closed) or passes noted (open).
	c = call(set(nil), userReq("oops"))
	if reply, _ := g.OnRequest(c); reply == nil || reply.Error.Kind != ir.ErrUpstream {
		t.Fatalf("closed: %+v", reply)
	}
	c = call(set(testkit.Settings{"moderation_fail": "open"}), userReq("oops"))
	if reply, _ := g.OnRequest(c); reply != nil || len(c.Notes()) != 1 || !strings.Contains(c.Notes()[0], "moderation unavailable") {
		t.Fatalf("open: %+v %v", reply, c.Notes())
	}
	// A batch being created is moderated for its first requests only;
	// past them moderation counts as failed, without a call.
	calls := len(fu.got)
	c = call(set(nil), userReq("hello"))
	c.BatchIndex = MaxModeratedBatchRequests
	if reply, _ := g.OnRequest(c); reply != nil || len(fu.got) != calls+1 {
		t.Fatalf("batch request within the cap: %+v", reply)
	}
	c = call(set(nil), userReq("hello"))
	c.BatchIndex = MaxModeratedBatchRequests + 1
	if reply, _ := g.OnRequest(c); reply == nil || reply.Error.Kind != ir.ErrInvalidRequest ||
		!strings.Contains(reply.Error.Message, "split the batch") || len(fu.got) != calls+1 {
		t.Fatalf("batch request past the cap, closed: %+v", reply)
	}
	c = call(set(testkit.Settings{"moderation_fail": "open"}), userReq("hello"))
	c.BatchIndex = MaxModeratedBatchRequests + 1
	if reply, _ := g.OnRequest(c); reply != nil || len(fu.got) != calls+1 || len(c.Notes()) != 1 {
		t.Fatalf("batch request past the cap, open: %+v %v", reply, c.Notes())
	}
	fu.fail = true
	c = call(set(nil), userReq("hello"))
	if reply, _ := g.OnRequest(c); reply == nil {
		t.Fatal("transport failure let through under closed")
	}
	g.SetUpstreams(nil)
	c = call(set(nil), userReq("hello"))
	if reply, _ := g.OnRequest(c); reply == nil {
		t.Fatal("no caller let through under closed")
	}
}

// counter reads one verdict series through the collector interface.
func counter(t *testing.T, g *Guard, labels ...string) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(g)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			var got []string
			for _, l := range m.GetLabel() {
				got = append(got, l.GetValue())
			}
			// labels sort by name: action, phase, rule
			if slices.Equal(got, []string{labels[1], labels[0], labels[2]}) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestRequestScansAllText: block and flag see every piece of text the
// client supplies, not only user turns; redact masks user text and flags
// the rest.
func TestRequestScansAllText(t *testing.T) {
	g := New()
	set := testkit.Settings{"deny_keywords": []string{"forbidden-word"}}
	text := func(s string) []ir.Block { return []ir.Block{{Type: ir.BlockText, Text: s}} }
	hi := ir.Message{Role: ir.RoleUser, Content: text("hi")}
	cases := map[string]*ir.Request{
		"system":    {Model: "m", System: text("forbidden-word"), Messages: []ir.Message{hi}},
		"assistant": {Model: "m", Messages: []ir.Message{{Role: ir.RoleAssistant, Content: text("forbidden-word")}, hi}},
		"thinking":  {Model: "m", Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockThinking, Text: "forbidden-word"}}}, hi}},
		"tool args": {Model: "m", Messages: []ir.Message{{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse,
			Tool: &ir.ToolCall{ID: "t", Name: "run", Input: []byte(`{"q":"forbidden-word"}`)}}}}, hi}},
		"tool result": {Model: "m", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult,
			Result: &ir.ToolResult{ToolUseID: "t", Content: text("forbidden-word")}}}}, hi}},
	}
	for name, req := range cases {
		if r, err := g.OnRequest(call(set, req)); err != nil || r == nil || r.Error == nil {
			t.Errorf("%s: let through (%v, %v)", name, r, err)
		}
		c := call(testkit.Settings{"deny_keywords": []string{"forbidden-word"}, "action": "redact"}, req)
		if r, err := g.OnRequest(c); err != nil || r != nil || !hasNote(c, "guardrails: flag request keyword") {
			t.Errorf("%s redact: %v %v notes %v", name, r, err, c.Notes())
		}
		if v, _ := c.Get("redact"); v == true {
			t.Errorf("%s: redact armed for text it cannot mask", name)
		}
	}
}

// TestKeywordNormalization: zero-width characters, full-width forms and
// case do not hide a keyword.
func TestKeywordNormalization(t *testing.T) {
	g := New()
	for _, s := range []string{"forb\u200bidden", "FORBIDDEN", "ｆｏｒｂｉｄｄｅｎ", "Ｆorb\u00adid\ufeffden"} {
		c := call(testkit.Settings{"deny_keywords": []string{"forbidden"}}, userReq("say "+s))
		if r, _ := g.OnRequest(c); r == nil {
			t.Errorf("%q let through", s)
		}
	}
	if r, _ := g.OnRequest(call(testkit.Settings{"deny_keywords": []string{"forbidden"}}, userReq("forbade"))); r != nil {
		t.Error("near miss blocked")
	}
}

// A number split over frames is judged whole: a match touching the end
// that more digits undo is held, not blocked, and a real one blocks once
// it is complete.
func TestStreamBlockNumberAtFrameEnd(t *testing.T) {
	g := New()
	for _, tc := range []struct {
		det    string
		frames []string
		block  bool
	}{
		{"phone", []string{"order 13812345678", "90123 shipped"}, false},
		{"phone", []string{"id 555-123-4567", "89 ok"}, false},
		{"us_ssn", []string{"ref 123-45-6789", "0 done"}, false},
		{"phone", []string{"call 555-123-4567", " now"}, true},
		{"us_ssn", []string{"ref 123-45-6789"}, true},
	} {
		c := call(testkit.Settings{"detect": []string{tc.det}}, nil)
		var err error
		for _, s := range tc.frames {
			if _, err = g.OnEvent(c, delta(s)); err != nil {
				break
			}
			if g.Held(c) == 0 {
				t.Fatalf("%v: %q left before it was judged", tc.frames, s)
			}
		}
		if err == nil {
			_, err = g.OnEvent(c, &ir.Event{Kind: ir.EvBlockStop})
		}
		if blocked := err != nil; blocked != tc.block {
			t.Fatalf("%v: blocked %v, err %v", tc.frames, blocked, err)
		}
		if !tc.block && g.Held(c) != 0 {
			t.Fatalf("%v: still held after the end", tc.frames)
		}
	}
	// A keyword start written with the Kelvin sign is held like "ki".
	c := call(testkit.Settings{"deny_keywords": []string{"kill"}}, nil)
	if _, err := g.OnEvent(c, delta("we Ki")); err != nil || g.Held(c) != 1 {
		t.Fatalf("Kelvin-sign keyword start: err %v, held %d", err, g.Held(c))
	}
	if _, err := g.OnEvent(c, delta("ll it")); err == nil {
		t.Fatal("Kelvin-sign keyword let through")
	}
}

// Audio input the rules cannot read is refused only with
// block_unscannable_audio, action block and some rule on.
func TestGuardrailsUnscannableAudio(t *testing.T) {
	g := New()
	for _, tc := range []struct {
		set   testkit.Settings
		block bool
	}{
		{testkit.Settings{"deny_keywords": []string{"x"}}, false},
		{testkit.Settings{"deny_keywords": []string{"x"}, "block_unscannable_audio": true}, true},
		{testkit.Settings{"deny_keywords": []string{"x"}, "block_unscannable_audio": true, "action": "flag"}, false},
		{testkit.Settings{"block_unscannable_audio": true}, false},
	} {
		c := call(tc.set, userReq())
		c.UnscannedAudio = true
		reply, err := g.OnRequest(c)
		if err != nil || (reply != nil) != tc.block {
			t.Fatalf("%v: %+v %v", tc.set, reply, err)
		}
		if tc.block && (reply.Error.Kind != ir.ErrContentFiltered || !hasNote(c, "guardrails: block request audio")) {
			t.Fatalf("%v: %+v %v", tc.set, reply.Error, c.Notes())
		}
	}
}
