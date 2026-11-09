package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/middleware/guardrails"
	"github.com/initxy/plugrelay/plugins/middleware/redact"
)

// wsGuardEnv is a WebSocket env (caps: realtime or responses_websocket) whose
// chain is plugins, with settings set.
func wsGuardEnv(t *testing.T, up *wsUpstream, caps string, set map[string]map[string]any, plugins ...plugin.Plugin) *env {
	t.Helper()
	e := realtimeEnv(t, up, caps, func(d *Deps) { d.Settings = pluginSettings(set) })
	e.chain.Plugins = plugins
	return e
}

func guardSettings(kv ...any) map[string]map[string]any {
	m := map[string]any{"deny_keywords": []string{"forbidden"}, "check_response": true}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return map[string]map[string]any{guardrails.ID: m}
}

func dialSession(t *testing.T, e *env, kind string) *websocket.Conn {
	t.Helper()
	path := "/v1/realtime?model=m"
	if kind == WSResponses {
		path = "/v1/responses"
	}
	c, _, err := wsDial(t, wsURL(e.wsServer(kind), path), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wsRead(t, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func isGuardError(ev string) bool {
	return gjson.Get(ev, "type").String() == "error" && strings.Contains(gjson.Get(ev, "error.message").String(), "guardrails")
}

// Client events carrying a blocked keyword get an error event and are not
// forwarded, on both WebSocket APIs; the session stays open.
func TestWebSocketGuardrailsBlockClientText(t *testing.T) {
	t.Run("realtime", func(t *testing.T) {
		up := newWSUpstream(t, `{"type":"response.done","response":{}}`)
		e := wsGuardEnv(t, up, "{realtime: true}", guardSettings(), guardrails.New())
		c := dialSession(t, e, WSRealtime)
		for _, ev := range []string{
			`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"a FORBIDDEN word"}]}}`,
			`{"type":"conversation.item.create","item":{"type":"function_call_output","call_id":"c1","output":"forbidden"}}`,
			`{"type":"response.create","response":{"instructions":"say forbidden"}}`,
			`{"type":"response.create","response":{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"forbidden"}]}]}}`,
			`{"type":"session.update","session":{"instructions":"forbidden things"}}`,
		} {
			wsSend(t, c, ev)
			if got, _ := wsRead(t, c); !isGuardError(got) {
				t.Fatalf("%s: got %s", ev, got)
			}
		}
		ok := `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"fine"}]}}`
		wsSend(t, c, ok)
		if got, _ := wsRead(t, c); gjson.Get(got, "type").String() != "echo" {
			t.Fatalf("allowed event: %s", got)
		}
		if got := up.received(); len(got) != 1 || got[0] != ok {
			t.Fatalf("upstream received %v", got)
		}
	})
	t.Run("responses", func(t *testing.T) {
		up := newWSUpstream(t, `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`)
		e := wsGuardEnv(t, up, "{responses_websocket: true}", guardSettings(), guardrails.New())
		c := dialSession(t, e, WSResponses)
		for _, ev := range []string{
			`{"type":"response.create","model":"m","input":"forbidden"}`,
			`{"type":"response.create","model":"m","instructions":"forbidden","input":[]}`,
			`{"type":"response.create","model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"x forbidden"}]}]}`,
		} {
			wsSend(t, c, ev)
			if got, _ := wsRead(t, c); !isGuardError(got) {
				t.Fatalf("%s: got %s", ev, got)
			}
		}
		wsSend(t, c, `{"type":"response.create","model":"m","input":"fine"}`)
		if got, _ := wsRead(t, c); gjson.Get(got, "type").String() != "response.created" {
			t.Fatalf("allowed: %s", got)
		}
		if got := up.received(); len(got) != 1 {
			t.Fatalf("upstream received %v", got)
		}
	})
}

// Redaction (guardrails in redact mode, and middleware.redact) edits the
// user text that is forwarded; other text is left as sent.
func TestWebSocketRedactsClientText(t *testing.T) {
	for _, tc := range []struct {
		name string
		pl   plugin.Plugin
		set  map[string]map[string]any
	}{
		{"guardrails", guardrails.New(), map[string]map[string]any{guardrails.ID: {"detect": []string{"email"}, "action": "redact"}}},
		{"redact", redact.New(), nil},
	} {
		t.Run(tc.name+" realtime", func(t *testing.T) {
			up := newWSUpstream(t, `{}`)
			e := wsGuardEnv(t, up, "{realtime: true}", tc.set, tc.pl)
			c := dialSession(t, e, WSRealtime)
			wsSend(t, c, `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"mail a@example.com"},{"type":"input_text","text":"plain"}]}}`)
			got, _ := wsRead(t, c)
			if txt := gjson.Get(got, "of.item.content.0.text").String(); txt != "mail [REDACTED:email]" {
				t.Fatalf("forwarded %s", got)
			}
			if txt := gjson.Get(got, "of.item.content.1.text").String(); txt != "plain" {
				t.Fatalf("forwarded %s", got)
			}
			wsSend(t, c, `{"type":"session.update","session":{"instructions":"a@example.com"}}`)
			if got, _ := wsRead(t, c); gjson.Get(got, "of.session.instructions").String() != "a@example.com" {
				t.Fatalf("instructions edited: %s", got)
			}
		})
		t.Run(tc.name+" responses", func(t *testing.T) {
			up := newWSUpstream(t, `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`)
			e := wsGuardEnv(t, up, "{responses_websocket: true}", tc.set, tc.pl)
			c := dialSession(t, e, WSResponses)
			wsSend(t, c, `{"type":"response.create","model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"mail a@example.com"}]}]}`)
			_, _ = wsRead(t, c)
			got := up.received()
			if len(got) != 1 || gjson.Get(got[0], "input.0.content.0.text").String() != "mail [REDACTED:email]" {
				t.Fatalf("upstream received %v", got)
			}
		})
	}
}

// outputUpstream answers response.create with text split over deltas (the
// Realtime shape carries response_id, the Responses one does not), then
// the terminal event repeating the whole text.
func outputUpstream(t *testing.T, kind string, deltas ...string) *wsUpstream {
	return scriptedUpstream(t, []string{`{"type":"session.created"}`}, func(ctx context.Context, c *websocket.Conn, msg string) {
		if gjson.Get(msg, "type").String() != "response.create" {
			return
		}
		full := strings.Join(deltas, "")
		send(ctx, c, `{"type":"response.created","response":{"id":"resp_1"}}`,
			`{"type":"response.output_item.added","response_id":"resp_1","item":{"id":"item_1","type":"message"}}`)
		for _, d := range deltas {
			if kind == WSRealtime {
				send(ctx, c, `{"type":"response.output_text.delta","response_id":"resp_1","item_id":"item_1","delta":"`+d+`"}`)
			} else {
				send(ctx, c, `{"type":"response.output_text.delta","item_id":"item_1","delta":"`+d+`"}`)
			}
		}
		out := `"output":[{"id":"item_1","content":[{"type":"output_text","text":"` + full + `"}]}],"usage":{"input_tokens":1,"output_tokens":2}`
		if kind == WSRealtime {
			send(ctx, c, `{"type":"response.output_text.done","response_id":"resp_1","item_id":"item_1","text":"`+full+`"}`,
				`{"type":"conversation.item.done","item":{"id":"item_1","content":[{"type":"output_text","text":"`+full+`"}]}}`,
				`{"type":"response.done","response":{"id":"resp_1","status":"completed",`+out+`}}`)
		} else {
			send(ctx, c, `{"type":"response.output_text.done","item_id":"item_1","text":"`+full+`"}`,
				`{"type":"response.completed","response":{"id":"resp_1","status":"completed",`+out+`}}`)
		}
	})
}

// A blocked keyword split across two output deltas stops the response: the
// client gets an error event and the terminal event with no output, and no
// frame carrying any part of the match; a Realtime upstream is sent
// response.cancel. The response is still billed and its record notes the
// verdict.
func TestWebSocketGuardrailsBlockOutput(t *testing.T) {
	for _, kind := range []string{WSRealtime, WSResponses} {
		t.Run(kind, func(t *testing.T) {
			up := outputUpstream(t, kind, "the forb", "idden fruit")
			caps := "{realtime: true}"
			terminal := "response.done"
			if kind == WSResponses {
				caps, terminal = "{responses_websocket: true}", "response.completed"
			}
			e := wsGuardEnv(t, up, caps, guardSettings(), guardrails.New())
			c := dialSession(t, e, kind)
			wsSend(t, c, `{"type":"response.create","model":"m"}`)
			var got []string
			for {
				m, err := wsRead(t, c)
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, m)
				if gjson.Get(m, "type").String() == terminal {
					break
				}
			}
			sawError := false
			for _, m := range got {
				if strings.Contains(m, "forb") {
					t.Fatalf("leaked: %s (all %v)", m, got)
				}
				sawError = sawError || isGuardError(m)
			}
			if !sawError {
				t.Fatalf("no error event: %v", got)
			}
			if out := gjson.Get(got[len(got)-1], "response.output"); !out.IsArray() || len(out.Array()) != 0 {
				t.Fatalf("terminal %s", got[len(got)-1])
			}
			_ = c.Close(websocket.StatusNormalClosure, "")
			recs := e.waitRecordsOf(2)
			if notes := recs[0].Attempts[0].Diagnostic; notes == nil || !strings.Contains(strings.Join(notes.Notes, ";"), "guardrails: block stream keyword") {
				t.Errorf("response record %+v", recs[0].Attempts[0])
			}
			if recs[0].Usage.Output != 2 {
				t.Errorf("usage %+v", recs[0].Usage)
			}
			cancel := false
			for _, m := range up.received() {
				cancel = cancel || (gjson.Get(m, "type").String() == "response.cancel" && gjson.Get(m, "response_id").String() == "resp_1")
			}
			if cancel != (kind == WSRealtime) {
				t.Errorf("upstream received %v", up.received())
			}
		})
	}
}

// Flag mode lets output through unchanged and notes the verdict on the
// response record; flagged client text is noted on the session record.
func TestWebSocketGuardrailsFlag(t *testing.T) {
	up := outputUpstream(t, WSRealtime, "the forb", "idden fruit")
	e := wsGuardEnv(t, up, "{realtime: true}", guardSettings("action", "flag"), guardrails.New())
	c := dialSession(t, e, WSRealtime)
	wsSend(t, c, `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"forbidden"}]}}`)
	wsSend(t, c, `{"type":"response.create"}`)
	var got []string
	for {
		m, err := wsRead(t, c)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
		if gjson.Get(m, "type").String() == "response.done" {
			break
		}
	}
	if !strings.Contains(strings.Join(got, ""), `"delta":"the forb"`) {
		t.Fatalf("flag changed output: %v", got)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	recs := e.waitRecordsOf(2)
	if d := recs[0].Attempts[0].Diagnostic; d == nil || !strings.Contains(strings.Join(d.Notes, ";"), "guardrails: flag stream keyword") {
		t.Errorf("response record %+v", recs[0].Attempts[0])
	}
	if d := recs[1].Attempts[0].Diagnostic; d == nil || !strings.Contains(strings.Join(d.Notes, ";"), "guardrails: flag request keyword") {
		t.Errorf("session record %+v", recs[1].Attempts[0])
	}
}

// A caller without content rules gets every frame byte for byte, both
// ways, though guardrails and redaction are in the chain.
func TestWebSocketGuardrailsOffByteExact(t *testing.T) {
	frames := []string{`{"type":"response.created","response":{"id":"resp_1"}}`,
		`{ "type" : "response.output_text.delta", "response_id":"resp_1",  "delta":"forbidden A"}`,
		`{"type":"response.done","response":{"id":"resp_1","output":[{"x":"forbidden"}]}}`}
	up := scriptedUpstream(t, []string{`{"type":"session.created"}`}, func(ctx context.Context, c *websocket.Conn, msg string) {
		if gjson.Get(msg, "type").String() == "response.create" {
			send(ctx, c, frames...)
		}
	})
	e := wsGuardEnv(t, up, "{realtime: true}", map[string]map[string]any{redact.ID: {"detect": []string{}}}, guardrails.New(), redact.New())
	c := dialSession(t, e, WSRealtime)
	create := `{"type":"response.create",  "response":{"instructions":"forbidden a@example.com"}}`
	wsSend(t, c, create)
	for _, want := range frames {
		if got, _ := wsRead(t, c); got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	}
	if got := up.received(); len(got) != 1 || got[0] != create {
		t.Fatalf("upstream received %v", got)
	}
}

// Audio cannot be checked: with block_unscannable_audio and a blocking
// rule it is refused, else forwarded.
func TestRealtimeGuardrailsAudio(t *testing.T) {
	audio := `{"type":"input_audio_buffer.append","audio":"AAAA"}`
	for _, on := range []bool{false, true} {
		up := newWSUpstream(t, `{}`)
		e := wsGuardEnv(t, up, "{realtime: true}", guardSettings("block_unscannable_audio", on), guardrails.New())
		c := dialSession(t, e, WSRealtime)
		for range 2 {
			wsSend(t, c, audio)
			got, _ := wsRead(t, c)
			if isGuardError(got) != on {
				t.Fatalf("block_unscannable_audio %v: %s", on, got)
			}
		}
		wsSend(t, c, `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_audio","audio":"AAAA"}]}}`)
		if got, _ := wsRead(t, c); isGuardError(got) != on {
			t.Fatalf("block_unscannable_audio %v: item %s", on, got)
		}
	}
}

// A blocked response's text stays in the upstream conversation. After the
// response ends and its per-response state is pruned, a client asking for
// the item again (conversation.item.retrieve) must still not get the text.
func TestWebSocketBlockedItemNotRetrievable(t *testing.T) {
	full := "the forbidden fruit"
	up := scriptedUpstream(t, []string{`{"type":"session.created"}`}, func(ctx context.Context, c *websocket.Conn, msg string) {
		switch gjson.Get(msg, "type").String() {
		case "response.create":
			send(ctx, c, `{"type":"response.created","response":{"id":"resp_1"}}`,
				`{"type":"response.output_item.added","response_id":"resp_1","item":{"id":"item_1","type":"message"}}`,
				`{"type":"response.output_text.delta","response_id":"resp_1","item_id":"item_1","delta":"the forb"}`,
				`{"type":"response.output_text.delta","response_id":"resp_1","item_id":"item_1","delta":"idden fruit"}`,
				`{"type":"response.done","response":{"id":"resp_1","status":"cancelled","output":[],"usage":{"input_tokens":1,"output_tokens":2}}}`)
		case "conversation.item.retrieve":
			send(ctx, c, `{"type":"conversation.item.retrieved","event_id":"ev_9","item":{"id":"item_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"`+full+`"}]}}`,
				`{"type":"conversation.item.retrieved","event_id":"ev_10","item":{"id":"item_2","type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`)
		}
	})
	e := wsGuardEnv(t, up, "{realtime: true}", guardSettings(), guardrails.New())
	c := dialSession(t, e, WSRealtime)
	wsSend(t, c, `{"type":"response.create"}`)
	var got []string
	for {
		m, err := wsRead(t, c)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
		if gjson.Get(m, "type").String() == "response.done" {
			break
		}
	}
	wsSend(t, c, `{"type":"conversation.item.retrieve","item_id":"item_1"}`)
	// An unrelated item still comes through, so the check is not "drop
	// every retrieve".
	m, err := wsRead(t, c)
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, m)
	if gjson.Get(m, "item.id").Str != "item_2" {
		t.Fatalf("unrelated item not forwarded; got %s", m)
	}
	for _, m := range got {
		if strings.Contains(m, "forbidden") {
			t.Errorf("blocked text reached the client: %s", m)
		}
	}
}
