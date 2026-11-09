package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// wsBody is the request behind a WebSocket plugin call, decoded once when
// a plugin (or the estimate) first asks for it.
type wsBody struct {
	decode func() (*ir.Request, ir.Needs, error)
	asked  bool // a plugin asked since the flag was last cleared
	done   bool
	req    *ir.Request
	needs  ir.Needs
	err    error
}

func (b *wsBody) get() (*ir.Request, error) {
	b.asked = true
	return b.load()
}

func (b *wsBody) load() (*ir.Request, error) {
	if b.decode == nil {
		return nil, errWSBody
	}
	if !b.done {
		b.req, b.needs, b.err = b.decode()
		b.done = true
	}
	return b.req, b.err
}

// decodeResponses decodes body as a Responses request.
func decodeResponses(body []byte) func() (*ir.Request, ir.Needs, error) {
	return func() (*ir.Request, ir.Needs, error) {
		return responsesCodec().DecodeRequest(nil, protocol.Inbound{Op: protocol.OpGenerate}, body)
	}
}

// textMutatorPath is the Responses user-text path a body mutator must
// declare to be run on WebSocket events.
const textMutatorPath = "input.#.content.#.text"

// contentPlugins fixes, at the handshake, the body mutators (and among
// them those that edit user text, which also run on Realtime text) and the
// event hooks run over output text.
func (s *wsSession) contentPlugins() {
	chain := s.p.chain()
	if chain == nil {
		return
	}
	for _, pl := range chain.Plugins {
		if _, ok := pl.(plugin.BodyMutator); ok && supports(pl, "middleware.mutateBody") && len(pl.Info().Mutates) > 0 {
			s.bodyMutators = append(s.bodyMutators, pl)
			if slices.Contains(pl.Info().Mutates, textMutatorPath) {
				s.mutators = append(s.mutators, pl)
			}
		}
		if _, ok := pl.(plugin.EventHook); ok && supports(pl, "middleware.onEvent") {
			s.eventHooks = append(s.eventHooks, pl)
		}
	}
}

// hookOutcome reads a request hook's or body mutator's outcome as the
// main pipeline does: a reply or a deliberate rejection stops, a
// malfunction follows the plugin's FailPolicy.
func (s *wsSession) hookOutcome(info plugin.Info, id string, reply *plugin.Reply, err error) (*ir.GwError, bool) {
	if err != nil {
		return s.p.hookFailure(info, "websocket hook", id, err)
	}
	if reply == nil {
		return nil, false
	}
	if reply.Error != nil {
		return reply.Error, true
	}
	return ir.Errorf(ir.ErrPermission, "plugin %s answers requests itself, which a WebSocket session cannot carry", info.ID), true
}

// mutate runs body mutators over body, a request of protocol proto (a
// Responses request, or a Realtime client event as "openai-realtime"),
// keeping only the edits each declared.
func (s *wsSession) mutate(call *plugin.Call, body []byte, mutators []plugin.Plugin, proto string) ([]byte, *ir.GwError) {
	a := &plugin.Attempt{N: 1, Candidate: s.cand, Tier: plugin.TierPatch, UpstreamModel: s.cand.UpstreamModel}
	a.Candidate.Protocol = proto
	for _, pl := range mutators {
		info := pl.Info()
		edited, err := s.mutateBody(pl.(plugin.BodyMutator), call.WithPlugin(info.ID), a, bytes.Clone(body))
		if err != nil {
			if gw, stop := s.hookOutcome(info, call.ID, nil, err); stop {
				return nil, gw
			}
			continue
		}
		body, _ = keepDeclared(body, edited, info.Mutates)
	}
	return body, nil
}

// attemptHooks runs the attempt hooks for a Responses response.create the
// request plugins admitted, as on a patch-tier attempt: they may refuse
// it. Headers they set are dropped, since the upstream connection is
// already open.
func (s *wsSession) attemptHooks(call *plugin.Call, upModel string) *ir.GwError {
	chain := s.p.chain()
	if chain == nil {
		return nil
	}
	a := &plugin.Attempt{N: 1, Candidate: s.cand, Tier: plugin.TierPatch, UpstreamModel: upModel, Header: http.Header{}}
	a.Candidate.Protocol = "openai-responses"
	for _, pl := range chain.Plugins {
		h, ok := pl.(plugin.AttemptHook)
		if !ok || !supports(pl, "middleware.onAttempt") {
			continue
		}
		info := pl.Info()
		if err := s.onAttempt(h, call.WithPlugin(info.ID), a); err != nil {
			if gw, stop := s.hookOutcome(info, call.ID, nil, err); stop {
				return gw
			}
		}
	}
	return nil
}

func (s *wsSession) onAttempt(h plugin.AttemptHook, c *plugin.Call, a *plugin.Attempt) (err error) {
	defer func() {
		if v := recover(); v != nil {
			s.p.d.Logger.Error("attempt hook panicked", "request", c.ID, "panic", v)
			err = fmt.Errorf("%w: panic: %v", plugin.ErrPluginFailure, v)
		}
	}()
	return h.OnAttempt(c, a)
}

func (s *wsSession) mutateBody(m plugin.BodyMutator, c *plugin.Call, a *plugin.Attempt, body []byte) (out []byte, err error) {
	defer func() {
		if v := recover(); v != nil {
			s.p.d.Logger.Error("body mutator panicked", "request", c.ID, "panic", v)
			out, err = nil, fmt.Errorf("%w: panic: %v", plugin.ErrPluginFailure, v)
		}
	}()
	return m.MutateBody(c, a, body)
}

// wsText is what a Realtime client event gives the content checks: its
// text as the items of a Responses request.
type wsText struct {
	instructions string
	items        [][]byte // Responses input items
	paths        []string // per item: the event path of its user text, else ""
	texts        []string // per item: that text
	tools        []string // function tool names
	audio        bool     // the event carries audio input
}

// realtimeText collects the text of a Realtime client event that the
// model reads: message text (user, system and assistant turns, audio
// transcripts the client supplies), tool calls and outputs, instructions
// and tool names. nil: the event carries none and no audio.
func realtimeText(t string, data []byte) *wsText {
	v := gjson.ParseBytes(data)
	x := &wsText{}
	switch t {
	case "conversation.item.create":
		x.item(v.Get("item"), "item")
	case "response.create":
		x.instructions = v.Get("response.instructions").Str
		for i, it := range v.Get("response.input").Array() {
			x.item(it, "response.input."+strconv.Itoa(i))
		}
		x.toolNames(v.Get("response.tools"))
	case "session.update":
		x.instructions = v.Get("session.instructions").Str
		x.toolNames(v.Get("session.tools"))
	case "input_audio_buffer.append":
		x.audio = true
	default:
		return nil
	}
	if !x.audio && x.empty() {
		return nil
	}
	return x
}

func (x *wsText) empty() bool { return len(x.items) == 0 && x.instructions == "" && len(x.tools) == 0 }

func (x *wsText) item(it gjson.Result, path string) {
	switch it.Get("type").Str {
	case "function_call_output":
		if out := it.Get("output").Str; out != "" {
			b, _ := sjson.SetBytes([]byte(`{"type":"function_call_output"}`), "call_id", "call_"+strconv.Itoa(len(x.items)))
			b, _ = sjson.SetBytes(b, "output", out)
			x.add(b, "", "")
		}
	case "function_call":
		b, _ := sjson.SetBytes([]byte(`{"type":"function_call"}`), "call_id", "call_"+strconv.Itoa(len(x.items)))
		b, _ = sjson.SetBytes(b, "name", it.Get("name").Str)
		b, _ = sjson.SetBytes(b, "arguments", it.Get("arguments").Str)
		x.add(b, "", "")
	case "message", "":
		role := it.Get("role").Str
		for j, p := range it.Get("content").Array() {
			at := path + ".content." + strconv.Itoa(j)
			switch p.Get("type").Str {
			case "input_text", "text", "output_text":
				x.message(role, p.Get("text").Str, at+".text")
			case "input_audio", "audio":
				if role != "assistant" {
					x.audio = true
				}
				// A transcript is read but never edited.
				x.message("assistant", p.Get("transcript").Str, "")
			}
		}
	}
}

func (x *wsText) message(role, text, path string) {
	if text == "" {
		return
	}
	part := "input_text"
	switch role {
	case "assistant":
		part, path = "output_text", ""
	case "system", "developer":
		path = ""
	default:
		role = "user"
	}
	b, _ := sjson.SetBytes([]byte(`{"type":"message"}`), "role", role)
	b, _ = sjson.SetBytes(b, "content.0.type", part)
	b, _ = sjson.SetBytes(b, "content.0.text", text)
	x.add(b, path, text)
}

func (x *wsText) add(item []byte, path, text string) {
	x.items = append(x.items, item)
	x.paths = append(x.paths, path)
	x.texts = append(x.texts, text)
}

func (x *wsText) toolNames(v gjson.Result) {
	v.ForEach(func(_, t gjson.Result) bool {
		if typ := t.Get("type").Str; (typ == "function" || typ == "") && t.Get("name").Str != "" {
			x.tools = append(x.tools, t.Get("name").Str)
		}
		return true
	})
}

// body renders x as a Responses request for model.
func (x *wsText) body(model string) []byte {
	b, _ := sjson.SetBytes([]byte(`{"input":[]}`), "model", model)
	if x.instructions != "" {
		b, _ = sjson.SetBytes(b, "instructions", x.instructions)
	}
	for _, it := range x.items {
		b, _ = sjson.SetRawBytes(b, "input.-1", it)
	}
	for _, n := range x.tools {
		t, _ := sjson.SetBytes([]byte(`{"type":"function","parameters":{"type":"object"}}`), "name", n)
		b, _ = sjson.SetRawBytes(b, "tools.-1", t)
	}
	return b
}

// checkRealtime runs the content checks over a Realtime client event:
// the request plugins entered at the handshake over its text, as one
// request (Call.EventIndex numbers it; rate limits count it), then the
// text-editing body mutators, whose edits to user text are written into
// the event. An event with nothing to check is returned unchanged. Audio
// cannot be read: an audio-only event is checked once per session with
// Call.UnscannedAudio set, and that verdict holds for the rest.
func (s *wsSession) checkRealtime(t string, data []byte) ([]byte, *ir.GwError) {
	if len(s.entered) == 0 && len(s.mutators) == 0 {
		return data, nil
	}
	x := realtimeText(t, data)
	if x == nil {
		return data, nil
	}
	if x.empty() {
		if len(s.entered) == 0 {
			return data, nil
		}
		if !s.audioChecked {
			_, s.audioRefusal = s.checkText(t, data, x)
			s.audioChecked = true
		}
		if s.audioRefusal != nil {
			gw := *s.audioRefusal
			return nil, &gw
		}
		return data, nil
	}
	return s.checkText(t, data, x)
}

func (s *wsSession) checkText(t string, data []byte, x *wsText) (out []byte, gw *ir.GwError) {
	s.checks++
	s.mu.Lock()
	model := s.model
	s.mu.Unlock()
	syn := x.body(model)
	body := &wsBody{decode: decodeResponses(syn)}
	call := s.newCallWith(s.id+".event."+strconv.Itoa(s.checks), model, body)
	call.UnscannedAudio = x.audio
	call.EventIndex = s.checks
	var ran []plugin.Plugin
	defer func() {
		res := &plugin.Result{Status: http.StatusOK, Delivered: true}
		if gw != nil {
			res = &plugin.Result{Status: statusOf(gw), Err: gw}
		}
		s.finishHooks(call, ran, res)
		s.addNotes(call.Notes())
	}()
	asked := false
	for _, pl := range s.entered {
		info := pl.Info()
		ran = append(ran, pl)
		body.asked = false
		reply, err := s.onRequest(pl.(plugin.RequestHook), call.WithPlugin(info.ID))
		asked = asked || body.asked
		if gw, stop := s.hookOutcome(info, call.ID, reply, err); stop {
			return nil, gw
		}
	}
	if asked && body.err != nil {
		return nil, ir.Errorf(ir.ErrInvalidRequest, "%s cannot be checked: %v", t, body.err)
	}
	if len(s.mutators) == 0 || data == nil {
		return data, nil
	}
	edited, gw := s.mutate(call, syn, s.mutators, "openai-responses")
	if gw != nil {
		return nil, gw
	}
	for i, p := range x.paths {
		if p == "" {
			continue
		}
		nv := gjson.GetBytes(edited, "input."+strconv.Itoa(i)+".content.0.text")
		if nv.Type != gjson.String || nv.Str == x.texts[i] {
			continue
		}
		b, err := sjson.SetBytes(data, p, nv.Str)
		if err != nil {
			return nil, ir.Errorf(ir.ErrInvalidRequest, "%s: %v", t, err)
		}
		data = b
	}
	return data, nil
}

func (s *wsSession) addNotes(notes []string) {
	if len(notes) == 0 {
		return
	}
	s.mu.Lock()
	s.notes = append(s.notes, notes...)
	s.mu.Unlock()
}

// relayEventPrefix starts the event_id of events the relay sends upstream
// itself; the upstream's errors about them never reach the client.
const relayEventPrefix = "event_relay_guard_"

// wsScreen is the upstream pump's state for checking output text: the
// event hooks' holds (heldFrames numbers the events they saw) and the
// frames waiting behind them, and the responses a hook stopped.
type wsScreen struct {
	held    heldFrames
	queue   []wsFrame
	blocked map[string]bool   // responses stopped, by id
	items   map[string]string // output item id → its response id
	// stopped holds the output item ids of blocked responses for the whole
	// session: the upstream keeps the text in the conversation, and a later
	// conversation.item.retrieve (or any event naming the item) must not
	// hand it back once the response's own state is pruned.
	stopped map[string]bool
	loose   map[string]*wsResponse
	calls   map[string]*wsResponse // hook call id → its response
	sent    int                    // events the relay sent upstream
}

type wsFrame struct {
	typ  websocket.MessageType
	b    []byte
	seq  int64
	resp string
}

func (s *wsSession) writeTo(c *websocket.Conn, typ websocket.MessageType, b []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.lim.Idle)
	defer cancel()
	return c.Write(ctx, typ, b)
}

// toClient forwards one upstream message to the client. With event hooks
// in the chain, output text (text and transcript deltas) runs through
// them first, per response: frames wait while a hook holds text back
// (plugin.EventHolder), and a hook's refusal stops the response — the
// client gets an error event and none of its later frames but a terminal
// event with the output removed; a Realtime upstream is sent
// response.cancel. Frames are never re-encoded otherwise.
func (s *wsSession) toClient(uc, cc *websocket.Conn, typ websocket.MessageType, data []byte) error {
	if typ == websocket.MessageText && bytes.Contains(data, []byte(relayEventPrefix)) {
		if v := gjson.ParseBytes(data); v.Get("type").Str == "error" && strings.HasPrefix(v.Get("error.event_id").Str, relayEventPrefix) {
			return nil
		}
	}
	if len(s.eventHooks) == 0 {
		return s.writeTo(cc, typ, data)
	}
	sc := &s.screen
	if typ != websocket.MessageText {
		return s.push(cc, wsFrame{typ: typ, b: data, seq: sc.held.seq})
	}
	v := gjson.ParseBytes(data)
	t := v.Get("type").Str
	if t == "error" {
		// An error is never dropped with the response it names.
		if sc.held.from() == math.MaxInt64 {
			return s.push(cc, wsFrame{typ: typ, b: data, seq: sc.held.seq})
		}
		id, ends := errorEnds(v)
		if !ends {
			// About a client event: held output goes on, the error
			// does not wait behind it.
			return s.writeTo(cc, typ, data)
		}
		if err := s.endHeld(uc, cc, id); err != nil {
			return err
		}
		return s.push(cc, wsFrame{typ: typ, b: data, seq: sc.held.seq})
	}
	r, key := s.outputResponse(t, v)
	item := v.Get("item_id").Str
	if item == "" {
		item = v.Get("item.id").Str
	}
	if item != "" && sc.stopped[item] {
		return nil // output of a blocked response, after its state was pruned
	}
	if item != "" {
		if k, ok := sc.items[item]; ok {
			if sc.blocked[k] {
				return nil
			}
		} else if key != "" {
			if sc.items == nil {
				sc.items = map[string]string{}
			}
			sc.items[item] = key
		}
	}
	terminal := s.terminal(t)
	if terminal && key != "" {
		defer s.forget(cc, key)
	}
	if key != "" && sc.blocked[key] {
		if !terminal {
			return nil
		}
		return s.push(cc, wsFrame{typ: typ, b: scrubOutput(data), seq: sc.held.seq, resp: key})
	}
	if ev := outputEvent(t, v, terminal); ev != nil {
		if r == nil && key != "" {
			r = s.looseResponse(key)
		}
		if r != nil {
			gw := s.screenEvent(r, ev)
			if sc.calls == nil {
				sc.calls = map[string]*wsResponse{}
			}
			sc.calls[r.call.ID] = r
			if gw != nil {
				if err := s.stop(uc, cc, r, key, gw, terminal); err != nil {
					return err
				}
				if !terminal {
					return nil
				}
				data = scrubOutput(data)
			}
		}
	}
	return s.push(cc, wsFrame{typ: typ, b: data, seq: sc.held.seq, resp: key})
}

func (s *wsSession) terminal(t string) bool {
	if s.kind == WSRealtime {
		return t == "response.done"
	}
	return t == "response.completed" || t == "response.failed" || t == "response.incomplete"
}

// outputEvent maps an upstream event to what the event hooks see: output
// text and audio transcript deltas, the end of a text part, the end of the
// response. Function call arguments and reasoning are not output text.
func outputEvent(t string, v gjson.Result, terminal bool) *ir.Event {
	switch t {
	case "response.output_text.delta", "response.text.delta",
		"response.output_audio_transcript.delta", "response.audio_transcript.delta":
		d := v.Get("delta").Str
		if d == "" {
			return nil
		}
		return &ir.Event{Kind: ir.EvDelta, Delta: &ir.Delta{Kind: ir.DeltaText, Text: d}}
	case "response.output_text.done", "response.text.done",
		"response.output_audio_transcript.done", "response.audio_transcript.done", "response.content_part.done":
		return &ir.Event{Kind: ir.EvBlockStop}
	}
	if terminal {
		return &ir.Event{Kind: ir.EvDone}
	}
	return nil
}

// outputResponse finds the open response an upstream event belongs to: by
// its response id, else (Responses events, which carry none) the newest
// started one. key is the response id.
func (s *wsSession) outputResponse(t string, v gjson.Result) (*wsResponse, string) {
	id := v.Get("response_id").Str
	if id == "" && strings.HasPrefix(t, "response.") {
		id = v.Get("response.id").Str
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.open) - 1; i >= 0; i-- {
		if r := s.open[i]; id != "" && r.id == id {
			return r, id
		}
	}
	if id == "" && s.kind == WSResponses && strings.HasPrefix(t, "response.") {
		for i := len(s.open) - 1; i >= 0; i-- {
			if r := s.open[i]; r.id != "" {
				return r, r.id
			}
		}
	}
	return nil, id
}

// looseResponse stands in for output of a response the relay never saw
// start, so its text is still checked.
func (s *wsSession) looseResponse(id string) *wsResponse {
	sc := &s.screen
	if r, ok := sc.loose[id]; ok {
		return r
	}
	if sc.loose == nil {
		sc.loose = map[string]*wsResponse{}
	}
	s.mu.Lock()
	model := s.model
	s.mu.Unlock()
	r := &wsResponse{id: id, model: model}
	sc.loose[id] = r
	return r
}

// screenEvent runs the event hooks over one output event of r and records
// what holders hold back. It returns the refusal that stops r.
func (s *wsSession) screenEvent(r *wsResponse, ev *ir.Event) *ir.GwError {
	if r.call == nil {
		r.call = s.newCall(s.nextHoldID(), r.model)
	}
	sc := &s.screen
	sc.held.seq++
	for _, pl := range s.eventHooks {
		h := pl.(plugin.EventHook)
		info := pl.Info()
		key := r.call.ID + "\x00" + info.ID
		start := time.Now()
		act, err := s.onEvent(h, r.call.WithPlugin(info.ID), copyEvent(ev))
		if took := time.Since(start); err == nil && took > s.p.d.EventHookBudget {
			err = fmt.Errorf("%w: event hook took %v (budget %v)", plugin.ErrPluginFailure, took, s.p.d.EventHookBudget)
		}
		holder, holds := h.(plugin.EventHolder)
		if err != nil {
			if gw, stop := s.hookOutcome(info, r.call.ID, nil, err); stop {
				return gw
			}
			if holds {
				sc.held.hold(key, 0)
			}
			continue
		}
		if holds {
			sc.held.hold(key, holder.Held(r.call.WithPlugin(info.ID)))
		}
		if act == plugin.EventAbort {
			return ir.Errorf(ir.ErrPermission, "response stopped by %s", info.ID)
		}
	}
	return nil
}

func (s *wsSession) onEvent(h plugin.EventHook, c *plugin.Call, ev *ir.Event) (act plugin.EventAction, err error) {
	defer func() {
		if v := recover(); v != nil {
			s.p.d.Logger.Error("event hook panicked", "request", c.ID, "panic", v)
			act, err = plugin.EventPass, fmt.Errorf("%w: panic: %v", plugin.ErrPluginFailure, v)
		}
	}()
	return h.OnEvent(c, ev)
}

// forget drops the checking state of response key once its terminal event
// went through: hook calls, output items, the stop, holds left over.
func (s *wsSession) forget(cc *websocket.Conn, key string) {
	sc := &s.screen
	for id, r := range sc.calls {
		if r.id == key {
			s.release(id)
			delete(sc.calls, id)
		}
	}
	for item, k := range sc.items {
		if k == key {
			if sc.blocked[key] {
				if sc.stopped == nil {
					sc.stopped = map[string]bool{}
				}
				sc.stopped[item] = true
			}
			delete(sc.items, item)
		}
	}
	delete(sc.loose, key)
	delete(sc.blocked, key)
	if err := s.flush(cc); err != nil {
		s.p.d.Logger.Debug("websocket flush failed", "session", s.id, "err", err)
	}
}

// stop ends response r (id key) after a hook refused its output: its
// waiting frames and holds are dropped, the client gets the error, and a
// Realtime upstream is asked to cancel it.
func (s *wsSession) stop(uc, cc *websocket.Conn, r *wsResponse, key string, gw *ir.GwError, terminal bool) error {
	s.block(uc, r.call.ID, key, !terminal)
	if err := s.writeTo(cc, websocket.MessageText, errorEvent(s.id, gw)); err != nil {
		return err
	}
	return s.flush(cc)
}

// block drops the holds of hook call callID and the waiting frames of its
// response (id key), whose later frames are dropped too; a Realtime
// upstream is asked to cancel it when it may still run.
func (s *wsSession) block(uc *websocket.Conn, callID, key string, cancel bool) {
	s.end(uc, callID, key, cancel)
	s.screen.queue = slices.DeleteFunc(s.screen.queue, func(f wsFrame) bool { return key != "" && f.resp == key })
}

// end drops the holds of hook call callID; the later frames of its
// response (id key) are dropped and its terminal event emptied.
func (s *wsSession) end(uc *websocket.Conn, callID, key string, cancel bool) {
	sc := &s.screen
	if sc.blocked == nil {
		sc.blocked = map[string]bool{}
	}
	sc.blocked[key] = true
	s.release(callID)
	if s.kind == WSRealtime && cancel && key != "" {
		sc.sent++
		ev, _ := sjson.SetBytes([]byte(`{"type":"response.cancel"}`), "event_id", relayEventPrefix+strconv.Itoa(sc.sent))
		ev, _ = sjson.SetBytes(ev, "response_id", key)
		if err := s.writeTo(uc, websocket.MessageText, ev); err != nil {
			s.p.d.Logger.Warn("websocket response.cancel failed", "session", s.id, "err", err)
		}
	}
}

// release drops the holds of hook call callID.
func (s *wsSession) release(callID string) {
	prefix := callID + "\x00"
	for k := range s.screen.held.seen {
		if strings.HasPrefix(k, prefix) {
			delete(s.screen.held.seen, k)
		}
	}
}

// errorEnds reports whether an upstream error event ends responses in
// flight, and which (id; "" = every one): an error naming a response, or a
// failure that answers no client event and is not about a request. An
// error carrying a client's event_id answers that event.
func errorEnds(v gjson.Result) (id string, ends bool) {
	if v.Get("error.event_id").Str != "" {
		return "", false
	}
	for _, k := range []string{"response_id", "error.response_id", "response.id"} {
		if id = v.Get(k).Str; id != "" {
			return id, true
		}
	}
	return "", v.Get("error.type").Str != "invalid_request_error"
}

// endHeld ends the responses (id, or all when "") whose output a hook
// still holds, as an upstream error ended them: the held text is checked
// as complete and released unless a hook refuses it, which stops the
// response. A released response's later frames are checked as before.
func (s *wsSession) endHeld(uc, cc *websocket.Conn, id string) error {
	sc := &s.screen
	var calls []string
	for k, seqs := range sc.held.seen {
		if len(seqs) > 0 {
			c, _, _ := strings.Cut(k, "\x00")
			if r := sc.calls[c]; r != nil && (id == "" || r.id == id) && !slices.Contains(calls, c) {
				calls = append(calls, c)
			}
		}
	}
	slices.Sort(calls)
	for _, c := range calls {
		r := sc.calls[c]
		if gw := s.screenEvent(r, &ir.Event{Kind: ir.EvDone}); gw != nil {
			if err := s.stop(uc, cc, r, r.id, gw, false); err != nil {
				return err
			}
			continue
		}
		s.release(c)
	}
	return s.flush(cc)
}

// scrubOutput removes the output from a stopped response's terminal event.
func scrubOutput(data []byte) []byte {
	b, err := sjson.SetRawBytes(data, "response.output", []byte(`[]`))
	if err != nil {
		return []byte(`{"type":"error"}`)
	}
	if gjson.GetBytes(b, "response.output_text").Exists() {
		b, _ = sjson.DeleteBytes(b, "response.output_text")
	}
	return b
}

// push writes f, or queues it while some event before it is held.
func (s *wsSession) push(cc *websocket.Conn, f wsFrame) error {
	sc := &s.screen
	if len(sc.queue) == 0 && sc.held.seq < sc.held.from() {
		return s.writeTo(cc, f.typ, f.b)
	}
	sc.queue = append(sc.queue, f)
	return s.flush(cc)
}

// flush writes the queued frames no hook holds any more.
func (s *wsSession) flush(cc *websocket.Conn) error {
	sc := &s.screen
	from := sc.held.from()
	for len(sc.queue) > 0 && sc.queue[0].seq < from {
		f := sc.queue[0]
		sc.queue = sc.queue[1:]
		if err := s.writeTo(cc, f.typ, f.b); err != nil {
			return err
		}
	}
	if from == math.MaxInt64 {
		sc.held.seen = nil
	}
	return nil
}

// capEvent runs the body mutators over a Realtime session.update or
// response.create as an "openai-realtime" request, keeping the paths each
// declared: middleware.param-policy lowers the output limit and reasoning
// effort there and strips parameters.
func (s *wsSession) capEvent(t string, data []byte) ([]byte, *ir.GwError) {
	if len(s.bodyMutators) == 0 || (t != "session.update" && t != "response.create") {
		return data, nil
	}
	s.mu.Lock()
	model := s.model
	s.mu.Unlock()
	data, gw := s.mutate(s.newCall(s.id+".event", model), data, s.bodyMutators, "openai-realtime")
	if gw != nil || t != "response.create" || s.outCap <= 0 {
		return data, gw
	}
	// A response's own limit overrides the session's, so only the
	// policy cap bounds it.
	for _, k := range []string{"max_output_tokens", "max_response_output_tokens"} {
		if f := gjson.GetBytes(data, "response."+k); f.Exists() && (f.Type != gjson.Number || f.Int() > s.outCap) {
			data, _ = sjson.SetBytes(data, "response."+k, s.outCap)
		}
	}
	return data, nil
}

// sessionReady lets client events through (see wsSession.ready).
func (s *wsSession) sessionReady() {
	s.readyOnce.Do(func() { close(s.ready) })
}

// capSession binds the session the upstream created to the body mutators:
// its reported output limit, run through them as a session.update, is sent
// back to the upstream as the relay's own session.update when they lower
// it (an unlimited default included). Client events wait until then, so
// the client's own later limit is never overwritten.
func (s *wsSession) capSession(v gjson.Result) {
	if len(s.bodyMutators) == 0 || s.uc == nil {
		return
	}
	probe := []byte(`{"type":"session.update","session":{"max_output_tokens":"inf"}}`)
	if b, gw := s.capEvent("session.update", probe); gw == nil {
		if c := gjson.GetBytes(b, "session.max_output_tokens"); c.Type == gjson.Number {
			s.outCap = c.Int()
		}
	}
	ev := []byte(`{"type":"session.update","session":{}}`)
	for _, k := range []string{"type", "max_output_tokens", "max_response_output_tokens"} {
		if f := v.Get(k); f.Exists() {
			ev, _ = sjson.SetRawBytes(ev, "session."+k, []byte(f.Raw))
		}
	}
	capped, gw := s.capEvent("session.update", ev)
	if gw != nil || bytes.Equal(capped, ev) {
		return
	}
	capped, _ = sjson.SetBytes(capped, "event_id", relayEventPrefix+"session")
	if err := s.writeTo(s.uc, websocket.MessageText, capped); err != nil {
		s.p.d.Logger.Warn("websocket session.update failed", "session", s.id, "err", err)
	}
}
