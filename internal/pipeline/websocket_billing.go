package pipeline

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/clientid"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// wsResponse is one response of a session, from its response.create
// (Responses) or response.created (Realtime) until it is settled.
type wsResponse struct {
	// id is the upstream's response id, once response.created named it.
	id    string
	model string // client model
	call  *plugin.Call
	hold  *plugin.Reservation
	// entered are the request plugins that ran for this response
	// (Responses mode), whose finish hooks run when it is settled.
	entered []plugin.Plugin
	start   time.Time
	// inTokens (estimated) and outChars feed the estimate when no usage
	// ever comes.
	inTokens, outChars int64
	transcription      bool
}

// errWSBody is what a request plugin gets asking for the body of a
// WebSocket session or response.
var errWSBody = ir.Errorf(ir.ErrInvalidRequest, "WebSocket messages have no decoded body")

// admit runs the request plugins on call, whose request is body: access
// lists, blocked models and rate limits apply, and on a Responses
// response.create the content checks too. It returns the plugins entered,
// whose finish hooks must run, those that asked for the request (content),
// and the refusal if one stopped it.
func (s *wsSession) admit(call *plugin.Call, body *wsBody) (entered, content []plugin.Plugin, gw *ir.GwError) {
	chain := s.p.chain()
	if chain == nil {
		return nil, nil, nil
	}
	for _, pl := range chain.Plugins {
		h, ok := pl.(plugin.RequestHook)
		if !ok || !supports(pl, "middleware.onRequest") {
			continue
		}
		info := pl.Info()
		entered = append(entered, pl)
		body.asked = false
		reply, err := s.onRequest(h, call.WithPlugin(info.ID))
		if body.asked {
			content = append(content, pl)
		}
		if gw, stop := s.hookOutcome(info, call.ID, reply, err); stop {
			return entered, content, gw
		}
	}
	return entered, content, nil
}

func (s *wsSession) onRequest(h plugin.RequestHook, c *plugin.Call) (reply *plugin.Reply, err error) {
	defer func() {
		if v := recover(); v != nil {
			s.p.d.Logger.Error("request hook panicked", "request", c.ID, "panic", v)
			reply, err = nil, fmt.Errorf("%w: panic: %v", plugin.ErrPluginFailure, v)
		}
	}()
	return h.OnRequest(c)
}

// finishHooks runs the finish hooks of the plugins entered for call, in
// reverse order.
func (s *wsSession) finishHooks(call *plugin.Call, entered []plugin.Plugin, res *plugin.Result) {
	for i := len(entered) - 1; i >= 0; i-- {
		pl := entered[i]
		h, ok := pl.(plugin.FinishHook)
		if !ok || !supports(pl, "middleware.onFinish") {
			continue
		}
		id := pl.Info().ID
		func() {
			defer func() {
				if v := recover(); v != nil {
					s.p.d.Logger.Error("finish hook panicked", "plugin", id, "request", call.ID, "panic", v)
				}
			}()
			h.OnFinish(call.WithPlugin(id), res)
		}()
	}
}

// reserveOn asks the meter to hold amount under call, whose input estimate
// plus output cap is tokens (what a token plan holds). No meter: nil, nil.
func (s *wsSession) reserveOn(call *plugin.Call, amount, tokens int64) (res *plugin.Reservation, gw *ir.GwError) {
	m := s.meter
	if m == nil {
		return nil, nil
	}
	if !supports(m, "meter.reserve") {
		return &plugin.Reservation{ID: call.ID}, nil
	}
	call.EstimateTokens = max(tokens, 0)
	defer func() {
		if v := recover(); v != nil {
			s.p.d.Logger.Error("meter reserve panicked", "request", call.ID, "panic", v)
			res, gw = nil, ir.Errorf(ir.ErrInternal, "metering unavailable")
		}
	}()
	res, err := m.Reserve(call.WithPlugin(m.Info().ID), amount)
	if err != nil {
		return nil, meterRefusal(err)
	}
	if res == nil {
		res = &plugin.Reservation{ID: call.ID}
	}
	return res, nil
}

func (s *wsSession) refundHold(call *plugin.Call, hold *plugin.Reservation) {
	if m := s.meter; m != nil && hold != nil && supports(m, "meter.refund") {
		defer func() {
			if v := recover(); v != nil {
				s.p.d.Logger.Error("meter refund panicked", "request", call.ID, "panic", v)
			}
		}()
		if err := m.Refund(call.WithPlugin(m.Info().ID), hold); err != nil {
			s.p.d.Logger.Error("meter refund failed", "request", call.ID, "err", err)
		}
	}
}

// reserveNext holds for the next Realtime response: the output cap at the
// model's output price, plus the session's input so far. At the start a refusal is answered as an HTTP
// error; later it is kept in refusal and ends the session once the open
// responses are settled.
func (s *wsSession) reserveNext(first bool) bool {
	if s.meter == nil {
		return true
	}
	s.mu.Lock()
	model, cap, in := s.model, s.maxOut, s.inTokens
	s.mu.Unlock()
	if cap <= 0 {
		cap = s.lim.ResponseReserve
	}
	call := s.newCall(s.nextHoldID(), model)
	res, gw := s.reserveOn(call, s.estimate(model, cap, in), in+cap)
	if gw != nil {
		if first {
			s.fail(gw)
		} else {
			s.mu.Lock()
			s.refusal = gw
			s.mu.Unlock()
		}
		return false
	}
	s.mu.Lock()
	s.next = &wsResponse{call: call, hold: res}
	s.mu.Unlock()
	return true
}

// estimate is what a response of model may cost: inTokens of input and
// outTokens of output at the caller's rate.
func (s *wsSession) estimate(model string, outTokens, inTokens int64) int64 {
	pr, _, ok := s.rate(model, s.upstreamModel(model))
	if !ok {
		return 0
	}
	return pr.tokenEstimate(inTokens, outTokens)
}

// rate is the caller's price for a client model: its list price (for a
// preset, the list price of the member that served), else the upstream
// model's cost rate, times the group multiplier.
func (s *wsSession) rate(model, upModel string) (Price, *plugin.Charge, bool) {
	pr, src, ok := s.snap.chargeRate(s.member(model), model, s.cand.UpstreamID, upModel)
	if !ok {
		return Price{}, nil, false
	}
	r, ch := chargeAt(pr, s.snap.multiplier(s.caller.GroupID), src, 0)
	return r, ch, true
}

// member is the preset member that serves model on a Realtime session
// opened for a preset; "" otherwise.
func (s *wsSession) member(model string) string {
	if s.kind != WSRealtime || s.served == nil {
		return ""
	}
	s.mu.Lock()
	same := model == s.model
	s.mu.Unlock()
	if !same {
		return ""
	}
	return s.served[router.Key(s.cand)]
}

// upstreamModel is the upstream's name for a client model: its route on a
// Responses session, the routed model on a Realtime one, else the name
// itself (a transcription model).
func (s *wsSession) upstreamModel(model string) string {
	if um, ok := s.routes[model]; ok {
		return um
	}
	s.mu.Lock()
	same := model == s.model
	s.mu.Unlock()
	if s.kind == WSRealtime && same {
		return s.cand.UpstreamModel
	}
	return model
}

// settle bills one response with usage u, records it, and runs its finish
// hooks. cut, when set, says why the response never finished.
func (s *wsSession) settle(r *wsResponse, u ir.Usage, cut string) {
	finished := false
	defer func() {
		if v := recover(); v != nil {
			// The finish hooks still release what the request plugins
			// hold (a rate-limit slot) before the panic ends the session.
			if !finished && len(r.entered) > 0 {
				s.finishHooks(r.call, r.entered, &plugin.Result{Status: http.StatusInternalServerError,
					Err: ir.Errorf(ir.ErrInternal, "settlement failed"), Usage: u, Attempts: 1, Delivered: true})
			}
			panic(v)
		}
	}()
	now := s.p.d.Now()
	if r.start.IsZero() {
		r.start = now
	}
	if r.call == nil {
		r.call = s.newCall(s.nextHoldID(), r.model)
	}
	r.call.Model = r.model
	upModel := s.upstreamModel(r.model)
	var cost int64
	if pr, ok := s.snap.costRate(s.cand.UpstreamID, upModel); ok {
		cost = pr.Cost(u)
	}
	var charge int64
	pr, detail, ok := s.rate(r.model, upModel)
	if ok {
		charge = pr.charge(u)
	}
	if m := s.meter; m != nil && supports(m, "meter.settle") {
		if r.hold == nil {
			// A response the meter refused to hold for still ran: try once
			// more so it is captured, else it goes unbilled.
			var gw *ir.GwError
			if r.hold, gw = s.reserveOn(r.call, 0, u.TotalInput()+u.Output); gw != nil {
				s.p.d.Logger.Error("websocket response unbilled: meter refused", "request", r.call.ID, "charge", charge, "err", gw.Message)
			}
		}
		if r.hold != nil {
			if detail != nil {
				r.call.UnitChargeMicro = min(detail.Rates.UnitCharge(u), charge)
			}
			if err := m.Settle(r.call.WithPlugin(m.Info().ID), r.hold, u, charge); err != nil {
				s.p.d.Logger.Error("meter settlement failed", "request", r.call.ID, "err", err)
			}
		}
	}
	status := http.StatusOK
	var kind ir.ErrorKind
	if cut != "" {
		// Cut off by the upstream, else by the client or a relay limit.
		status = statusClientClosed
		s.mu.Lock()
		if s.endKind == ir.ErrUpstream && !s.clientGone.Load() {
			status, kind = http.StatusBadGateway, ir.ErrUpstream
		}
		s.mu.Unlock()
	}
	ar := plugin.AttemptRecord{N: 1, UpstreamID: s.cand.UpstreamID, EndpointID: s.cand.EndpointID, CredentialID: s.cand.CredentialID,
		Protocol: s.ep.Protocol, Model: s.member(r.model), UpstreamModel: upModel, Tier: plugin.TierPassthrough, Status: status, Sent: true,
		Duration: now.Sub(r.start), Usage: u, CostMicro: cost, Charge: detail, Error: cut}
	if notes := r.call.Notes(); len(notes) > 0 {
		ar.Diagnostic = &ir.Diagnostic{Fidelity: ir.Lossless, Notes: notes}
	}
	rec := &plugin.Record{ID: r.call.ID, Time: r.start, Caller: *s.caller, Protocol: s.proto, Model: r.model, Stream: true,
		SessionID: s.id, ClientUA: s.r.UserAgent(), ClientIP: s.clientIP, Status: status, ErrorKind: kind, Error: cut,
		Attempts: []plugin.AttemptRecord{ar}, Usage: u, CostMicro: cost, ChargeMicro: charge, Duration: now.Sub(r.start)}
	clientid.Stamp(rec, s.r.Header)
	s.emit(rec, &ar)
	s.mu.Lock()
	if r.transcription {
		s.transcriptions++
	} else {
		s.responses++
	}
	s.usage.Add(u)
	if !r.transcription && !u.Estimated {
		// The conversation now holds at least what this response read
		// and wrote.
		s.inTokens = max(s.inTokens, u.TotalInput()+u.Output)
	}
	s.mu.Unlock()
	finished = true
	if len(r.entered) > 0 {
		var gw *ir.GwError
		if cut != "" {
			gw = &ir.GwError{Kind: kind, Message: cut}
		}
		s.finishHooks(r.call, r.entered, &plugin.Result{Status: status, Err: gw, Usage: u, Attempts: 1, Delivered: true})
	}
	select {
	case s.settled <- struct{}{}:
	default:
	}
}

// drop releases a Responses response.create the upstream refused or never
// started: its hold is refunded and its plugins finish, nothing is billed.
func (s *wsSession) drop(r *wsResponse, gw *ir.GwError) {
	s.refundHold(r.call, r.hold)
	status := http.StatusBadRequest
	if gw != nil && gw.Status != 0 {
		status = gw.Status
	}
	s.finishHooks(r.call, r.entered, &plugin.Result{Status: status, Err: gw})
	select {
	case s.settled <- struct{}{}:
	default:
	}
}

// createResponse admits a Responses response.create: the model must be one
// this session's upstream serves (renamed to its upstream model), the request plugins
// must let it through (reading it as a Responses request), the body
// mutators edit it as on a patch-tier attempt (keeping the paths each
// declared), the attempt hooks must let it through, and the meter must
// hold for it.
func (s *wsSession) createResponse(data []byte) ([]byte, *ir.GwError) {
	model := gjson.GetBytes(data, "model").String()
	s.mu.Lock()
	if model == "" {
		model = s.model
	}
	s.mu.Unlock()
	if model == "" {
		return nil, &ir.GwError{Kind: ir.ErrInvalidRequest, Code: "missing_required_parameter",
			Message: "the first response.create on a WebSocket connection must name a model"}
	}
	um, ok := s.routes[model]
	if !ok {
		return nil, &ir.GwError{Kind: ir.ErrNotFound, Code: "model_not_found",
			Message: "model " + quote(model) + " is not served on this WebSocket connection; use HTTP"}
	}
	if um != model && gjson.GetBytes(data, "model").Exists() {
		b, err := sjson.SetBytes(data, "model", um)
		if err != nil {
			return nil, ir.Errorf(ir.ErrInvalidRequest, "response.create: %v", err)
		}
		data = b
	}
	body := &wsBody{decode: decodeResponses(data)}
	r := &wsResponse{model: model, call: s.newCallWith(s.nextHoldID(), model, body)}
	if _, err := body.load(); err == nil {
		r.inTokens = int64(body.needs.InputTokensEst)
	} else {
		r.inTokens = int64(len(gjson.GetBytes(data, "input").Raw)+len(gjson.GetBytes(data, "instructions").String())) / 4
	}
	entered, content, gw := s.admit(r.call, body)
	r.entered = entered
	finished := false
	defer func() {
		if v := recover(); v != nil {
			if !finished {
				s.finishHooks(r.call, entered, &plugin.Result{Status: http.StatusInternalServerError, Err: ir.Errorf(ir.ErrInternal, "internal error")})
			}
			panic(v)
		}
	}()
	if gw == nil && len(content) > 0 && body.err != nil {
		gw = ir.Errorf(ir.ErrInvalidRequest, "response.create cannot be checked by %s: %v", content[0].Info().ID, body.err)
	}
	if gw == nil && len(s.bodyMutators) > 0 {
		data, gw = s.mutate(r.call, data, s.bodyMutators, "openai-responses")
	}
	if gw == nil {
		gw = s.attemptHooks(r.call, um)
	}
	if gw == nil {
		cap := gjson.GetBytes(data, "max_output_tokens").Int()
		if cap <= 0 {
			cap = s.lim.ResponseReserve
		}
		r.hold, gw = s.reserveOn(r.call, s.estimate(model, cap, r.inTokens), r.inTokens+cap)
	}
	if gw != nil {
		finished = true
		s.finishHooks(r.call, entered, &plugin.Result{Status: statusOf(gw), Err: gw})
		return nil, gw
	}
	finished = true
	s.mu.Lock()
	s.model = model
	if s.rec.Model == "" {
		s.rec.Model = model
	}
	s.open = append(s.open, r)
	s.mu.Unlock()
	return data, nil
}

func statusOf(gw *ir.GwError) int {
	status, _, _ := responsesCodec().EncodeError(gw)
	if gw.Status != 0 {
		status = gw.Status
	}
	return status
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// fromUpstream watches upstream events for started and finished responses
// and bills each one. It returns the meter's refusal when the session must
// end because the caller cannot pay for more.
func (s *wsSession) fromUpstream(data []byte) *ir.GwError {
	typ := gjson.GetBytes(data, "type").String()
	switch {
	case typ == "response.created":
		s.created(gjson.GetBytes(data, "response.id").String())
	case typ == "response.done" && s.kind == WSRealtime,
		(typ == "response.completed" || typ == "response.failed" || typ == "response.incomplete") && s.kind == WSResponses:
		if r := s.take(gjson.GetBytes(data, "response.id").String()); r != nil {
			s.settle(r, wsUsage(gjson.GetBytes(data, "response.usage")), "")
		}
		s.mu.Lock()
		gw := s.refusal
		if gw != nil && len(s.open) > 0 {
			gw = nil
		}
		s.mu.Unlock()
		return gw
	case typ == "error":
		if !strings.HasPrefix(gjson.GetBytes(data, "error.event_id").String(), relayEventPrefix) {
			s.upstreamError(data)
		}
	case s.kind == WSRealtime && (typ == "session.created" || typ == "session.updated" ||
		typ == "transcription_session.created" || typ == "transcription_session.updated"):
		v := gjson.GetBytes(data, "session")
		s.readSession(v)
		if typ == "session.created" {
			s.capSession(v)
		}
		if strings.HasSuffix(typ, ".created") {
			s.sessionReady()
		}
	case s.kind == WSRealtime && typ == "conversation.item.input_audio_transcription.completed":
		return s.transcribed(gjson.GetBytes(data, "usage"))
	case strings.HasSuffix(typ, ".delta") && !strings.HasSuffix(typ, "audio.delta") && !strings.HasPrefix(typ, "conversation."):
		s.delta(gjson.GetBytes(data, "response_id").String(), int64(len(gjson.GetBytes(data, "delta").String())))
	}
	return nil
}

// created binds a started response: on Realtime it opens one (servers
// start responses on their own) and holds for the next; on Responses it
// names the oldest response.create not yet started.
func (s *wsSession) created(id string) {
	now := s.p.d.Now()
	s.mu.Lock()
	if s.kind == WSResponses {
		for _, r := range s.open {
			if r.id == "" {
				r.id, r.start = id, now
				break
			}
		}
		s.mu.Unlock()
		return
	}
	r := &wsResponse{id: id, model: s.model, start: now, inTokens: s.inTokens}
	if n := s.next; n != nil {
		r.call, r.hold = n.call, n.hold
		s.next = nil
	}
	s.open = append(s.open, r)
	s.awaiting = max(s.awaiting-1, 0)
	s.mu.Unlock()
	if !s.clientGone.Load() {
		s.reserveNext(false)
	}
}

// take removes the open response with id (else the oldest started one)
// for settling.
func (s *wsSession) take(id string) *wsResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := -1
	for j, r := range s.open {
		if id != "" && r.id == id {
			i = j
			break
		}
	}
	if i < 0 {
		for j, r := range s.open {
			if r.id != "" || s.kind == WSRealtime {
				i = j
				break
			}
		}
	}
	if i < 0 {
		if s.kind == WSResponses && len(s.open) > 0 {
			i = 0
		} else {
			// A finished response the relay never saw start.
			return &wsResponse{id: id, model: s.model}
		}
	}
	r := s.open[i]
	s.open = append(s.open[:i:i], s.open[i+1:]...)
	return r
}

// upstreamError handles an error event. On Responses the oldest
// response.create not yet started is the one refused, whether or not the
// event carries a status; a started response ends with its own terminal
// event. On Realtime it settles nothing.
func (s *wsSession) upstreamError(data []byte) {
	s.mu.Lock()
	if s.kind == WSRealtime {
		s.awaiting = max(s.awaiting-1, 0)
		s.mu.Unlock()
		return
	}
	var r *wsResponse
	for i, o := range s.open {
		if o.id == "" {
			r = o
			s.open = append(s.open[:i:i], s.open[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	if r == nil {
		return
	}
	gw := &ir.GwError{Kind: ir.ErrUpstream, Message: gjson.GetBytes(data, "error.message").String(),
		Code: gjson.GetBytes(data, "error.code").String(), Status: int(gjson.GetBytes(data, "status").Int())}
	s.drop(r, gw)
}

// delta counts output characters toward the response's estimate.
func (s *wsSession) delta(id string, n int64) {
	if n == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.open) - 1; i >= 0; i-- {
		r := s.open[i]
		if (id != "" && r.id == id) || (id == "" && (r.id != "" || s.kind == WSRealtime)) {
			r.outChars += n
			return
		}
	}
}

// readSession keeps what billing needs from the session the upstream
// reports: the output cap (GA max_output_tokens, beta
// max_response_output_tokens; "inf" = none), the input transcription
// model (GA audio.input.transcription, beta input_audio_transcription) and
// the input audio format (GA audio.input.format, beta input_audio_format).
func (s *wsSession) readSession(v gjson.Result) {
	if !v.IsObject() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range []string{"max_output_tokens", "max_response_output_tokens"} {
		if c := v.Get(k); c.Exists() {
			s.maxOut = 0
			if c.Type == gjson.Number {
				s.maxOut = c.Int()
			}
		}
	}
	for _, k := range []string{"audio.input.transcription", "input_audio_transcription"} {
		if t := v.Get(k); t.Exists() {
			s.transcribeModel = t.Get("model").String()
		}
	}
	f := v.Get("audio.input.format.type").String()
	if f == "" {
		f = v.Get("input_audio_format").String()
	}
	switch f {
	case "audio/pcmu", "audio/pcma", "g711_ulaw", "g711_alaw":
		s.audioRate = 8000
	case "audio/pcm", "pcm16":
		s.audioRate = pcmRate
	}
}

// pcmRate is the bytes per second of Realtime pcm16 audio (24 kHz mono),
// the default input format; G.711 is 8000. audioTokensPerSecond is roughly
// what OpenAI counts for input audio.
const (
	pcmRate              = 48000
	audioTokensPerSecond = 10
)

// countInput adds what a Realtime client event puts in the conversation to
// the session's input estimate: its text at four bytes a token, its audio
// at audioTokensPerSecond.
func (s *wsSession) countInput(t string, data []byte) {
	var chars, audio int64
	switch t {
	case "input_audio_buffer.append":
		audio = int64(len(gjson.GetBytes(data, "audio").Str))
	case "conversation.item.create":
		gjson.GetBytes(data, "item.content").ForEach(func(_, p gjson.Result) bool {
			audio += int64(len(p.Get("audio").Str))
			return true
		})
	}
	if x := realtimeText(t, data); x != nil {
		chars = int64(len(x.instructions))
		for _, it := range x.items {
			chars += int64(len(it))
		}
		for _, n := range x.tools {
			chars += int64(len(n))
		}
	}
	if chars == 0 && audio == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rate := s.audioRate
	if rate <= 0 {
		rate = pcmRate
	}
	// Base64 carries three bytes in four characters.
	s.inTokens += chars/4 + audio*3/4*audioTokensPerSecond/rate
}

// checkTranscription admits the input transcription a Realtime
// session.update or transcription_session.update sets (GA
// audio.input.transcription, beta input_audio_transcription): with a
// meter it must name its model, which is admitted as by admitTranscription.
func (s *wsSession) checkTranscription(v gjson.Result) *ir.GwError {
	tr := v.Get("audio.input.transcription")
	if !tr.Exists() {
		tr = v.Get("input_audio_transcription")
	}
	if !tr.IsObject() {
		return nil
	}
	model := tr.Get("model").String()
	if model == "" {
		if s.meter == nil {
			return nil
		}
		return &ir.GwError{Kind: ir.ErrInvalidRequest, Code: "missing_required_parameter",
			Message: "the input audio transcription must name its model, which this relay bills it by"}
	}
	if gw := s.admitTranscription(model); gw != nil {
		return gw
	}
	s.clientTranscribe = model
	return nil
}

// transcriptionIntent reports a Realtime transcription session
// (?intent=transcription), whose input audio is all transcribed.
func (s *wsSession) transcriptionIntent() bool {
	return s.kind == WSRealtime && s.r.URL.Query().Get("intent") == "transcription"
}

// transcriptionReady refuses input audio on a transcription session while
// the transcription model, as the client named it or the upstream reports
// it, is not one the caller may use and (with a meter) priced.
func (s *wsSession) transcriptionReady() *ir.GwError {
	if !s.transcriptionIntent() || (s.meter == nil && s.snap.Visible == nil) {
		return nil
	}
	s.mu.Lock()
	reported := s.transcribeModel
	s.mu.Unlock()
	if reported == "" && s.clientTranscribe == "" {
		return &ir.GwError{Kind: ir.ErrInvalidRequest, Code: "missing_required_parameter",
			Message: "name the transcription model (transcription_session.update input_audio_transcription.model) before sending audio"}
	}
	for _, m := range []string{s.clientTranscribe, reported} {
		if m == "" {
			continue
		}
		if gw := s.admitTranscription(m); gw != nil {
			return gw
		}
	}
	return nil
}

// admitTranscription admits an input transcription model as a request of
// that model: the caller must see it, the request plugins (access lists,
// blocked models, rate limits) must let it through, and with a meter it
// must have a price. A model is admitted once per session.
func (s *wsSession) admitTranscription(model string) *ir.GwError {
	if s.transcribeOK[model] {
		return nil
	}
	if vis := s.snap.Visible; vis != nil && !vis(s.caller, model) {
		return &ir.GwError{Kind: ir.ErrNotFound, Code: "model_not_found", Message: "transcription model " + quote(model) + " not found"}
	}
	if s.meter != nil {
		if _, _, ok := s.rate(model, s.upstreamModel(model)); !ok {
			return &ir.GwError{Kind: ir.ErrInvalidRequest, Code: "model_not_priced",
				Message: "transcription model " + quote(model) + " has no price on this relay"}
		}
	}
	body := &wsBody{}
	s.checks++
	call := s.newCallWith(s.id+".event."+strconv.Itoa(s.checks), model, body)
	call.EventIndex = s.checks // the session holds the concurrency slot
	entered, _, gw := s.admit(call, body)
	res := &plugin.Result{Status: http.StatusOK, Delivered: true}
	if gw != nil {
		res = &plugin.Result{Status: statusOf(gw), Err: gw}
	}
	s.finishHooks(call, entered, res)
	if gw != nil {
		return gw
	}
	if s.transcribeOK == nil {
		s.transcribeOK = map[string]bool{}
	}
	s.transcribeOK[model] = true
	return nil
}

// transcribed bills an input audio transcription: tokens or seconds of
// audio at the transcription model's price, as its own request.
func (s *wsSession) transcribed(v gjson.Result) *ir.GwError {
	if !v.IsObject() {
		return nil
	}
	var u ir.Usage
	if v.Get("type").String() == "duration" {
		u.AudioMs = int64(math.Round(v.Get("seconds").Float() * 1000))
	} else {
		u.Input, u.Output = v.Get("input_tokens").Int(), v.Get("output_tokens").Int()
	}
	u.Raw = []byte(v.Raw)
	if u.IsZero() {
		return nil
	}
	s.mu.Lock()
	model := s.transcribeModel
	s.mu.Unlock()
	r := &wsResponse{model: model, call: s.newCall(s.nextHoldID(), model), transcription: true}
	var gw *ir.GwError
	r.hold, gw = s.reserveOn(r.call, 0, u.Input+u.Output)
	s.settle(r, u, "")
	return gw
}

// closeOpen settles what is still open when the session ends: a started
// response with an estimate (output from the deltas received, input from
// the request), a Responses response.create that never started is
// dropped; then the hold for the next response is refunded.
func (s *wsSession) closeOpen(note string) {
	s.mu.Lock()
	open, next := s.open, s.next
	s.open, s.next = nil, nil
	s.mu.Unlock()
	for _, r := range open {
		if s.kind == WSResponses && r.id == "" {
			s.drop(r, ir.Errorf(ir.ErrUpstream, "the session ended before the response started"))
			continue
		}
		u := ir.Usage{Input: r.inTokens, Output: r.outChars / 4, Estimated: true}
		s.settle(r, u, "response unfinished: "+note)
	}
	if next != nil {
		s.refundHold(next.call, next.hold)
	}
}

// openCount is the number of responses whose usage may still arrive.
func (s *wsSession) openCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.open) + s.awaiting
}

// guard runs f, turning a panic into a logged internal error.
func (s *wsSession) guard(what string, f func()) (ok bool) {
	defer func() {
		if v := recover(); v != nil {
			s.p.d.Logger.Error("websocket "+what+" panicked", "session", s.id, "panic", v, "stack", string(debug.Stack()))
			ok = false
		}
	}()
	f()
	return true
}

func (s *wsSession) nextHoldID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holds++
	return s.id + "." + strconv.Itoa(s.holds)
}

// newCall is a plugin call for one session or response, with no body.
func (s *wsSession) newCall(id, model string) *plugin.Call {
	return s.newCallWith(id, model, &wsBody{})
}

// newCallWith is a plugin call whose request is body.
func (s *wsSession) newCallWith(id, model string, body *wsBody) *plugin.Call {
	caller := s.caller
	c := plugin.NewCall(context.WithoutCancel(s.r.Context()), id, s.p.settingsFor(caller), body.get)
	c.Started = s.started
	c.Caller = caller
	c.Protocol = s.proto
	c.Model = model
	c.Stream = true
	c.ClientIP = s.clientIP
	c.Header = s.r.Header.Clone()
	c.SessionID = s.id
	return c
}

// wsUsage maps a Realtime or Responses usage object to the usage buckets.
// Cached tokens are part of input_tokens in both.
func wsUsage(v gjson.Result) ir.Usage {
	if !v.Exists() || v.Type == gjson.Null {
		return ir.Usage{}
	}
	in := v.Get("input_tokens").Int()
	cached := v.Get("input_token_details.cached_tokens").Int()
	if !v.Get("input_token_details").Exists() {
		cached = v.Get("input_tokens_details.cached_tokens").Int()
	}
	cached = min(max(cached, 0), in)
	return ir.Usage{Input: in - cached, CacheRead: cached, Output: v.Get("output_tokens").Int(),
		Reasoning: v.Get("output_tokens_details.reasoning_tokens").Int(), Raw: []byte(v.Raw)}
}
