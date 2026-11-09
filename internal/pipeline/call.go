package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// resolveSession picks the session id: the client's session header, then a
// body-level cache key or user tag, then a hash of key id, model and the
// conversation's stable prefix. A sub-agent is bound under its own id.
func resolveSession(c *plugin.Caller, in protocol.Inbound) string {
	h := in.Session
	id := h.SessionID
	if id == "" {
		id = h.CacheKey
	}
	if id == "" {
		id = h.UserTag
	}
	if id == "" && h.PrefixHash != "" {
		sum := sha256.Sum256([]byte(c.KeyID + "\x00" + in.Model + "\x00" + h.PrefixHash))
		id = "h:" + hex.EncodeToString(sum[:16])
	}
	if id == "" {
		return ""
	}
	// Session ids from different keys never share a binding.
	id = c.KeyID + "/" + id
	if h.AgentID != "" {
		id += "/agent:" + h.AgentID
	}
	return id
}

func (x *exchange) newCall() *plugin.Call {
	caller := x.caller
	request := func() (*ir.Request, error) {
		if x.attemptReq != nil {
			return x.attemptReq, nil
		}
		if x.decodeErr != nil {
			return nil, x.decodeErr
		}
		return x.req, nil
	}
	c := plugin.NewCall(x.ctx, x.id, x.p.settingsFor(caller), request)
	c.Started = x.started
	c.Caller = caller
	c.Protocol = x.codec.Name()
	c.Model = x.in.Model
	c.Stream = x.in.Stream
	c.ClientIP = x.p.d.ClientIP(x.r)
	c.Header = x.r.Header.Clone()
	c.SessionID = x.session
	return c
}

// settingsFor resolves plugin settings merged for caller.
func (p *Pipeline) settingsFor(caller *plugin.Caller) func(pluginID, key string) plugin.Value {
	return func(pluginID, key string) plugin.Value {
		if p.d.Settings == nil {
			return plugin.Value{}
		}
		return p.d.Settings(caller, pluginID, key)
	}
}

// supports reports whether a plugin really implements an optional hook;
// adapters of external plugins declare theirs at runtime.
func supports(p any, method string) bool {
	s, ok := p.(plugin.Supporter)
	return !ok || s.Supports(method)
}

// hookError decides what an error of a request's hook means (see
// hookFailure).
func (x *exchange) hookError(info plugin.Info, hook string, err error) (*ir.GwError, bool) {
	return x.p.hookFailure(info, hook+" hook", x.id, err)
}

// hookFailure decides what a plugin hook's error means. A deliberate
// rejection (an *ir.GwError that is not a plugin failure) goes to the
// client as is; a malfunction follows the plugin's FailPolicy, logged
// under what for request. stop=false means carry on.
func (p *Pipeline) hookFailure(info plugin.Info, what, request string, err error) (gw *ir.GwError, stop bool) {
	if errors.As(err, &gw) && !plugin.IsFailure(err) {
		return gw, true
	}
	if info.EffectiveFailPolicy() == plugin.FailOpen {
		p.d.Logger.Warn(what+" failed", "plugin", info.ID, "request", request, "err", err)
		return nil, false
	}
	return ir.Errorf(ir.ErrInternal, "plugin %s failed", info.ID), true
}

// runRequestHooks runs OnRequest in configured order. It reports true when
// the request was answered (a reply or a closed-policy failure).
func (x *exchange) runRequestHooks() bool {
	for _, pl := range x.chain.Plugins {
		info := pl.Info()
		x.entered = append(x.entered, pl)
		h, ok := pl.(plugin.RequestHook)
		if !ok || !supports(pl, "middleware.onRequest") {
			continue
		}
		reply, err := h.OnRequest(x.call.WithPlugin(info.ID))
		if err != nil {
			if gw, stop := x.hookError(info, "request", err); stop {
				x.fail(gw)
				return true
			}
			continue
		}
		if reply != nil {
			x.reply(reply)
			return true
		}
	}
	return false
}

// reserve pre-authorizes the request with the meter.
func (x *exchange) reserve() bool {
	m := x.chain.Meter
	if m == nil {
		return true
	}
	if !supports(m, "meter.reserve") {
		x.reservation = &plugin.Reservation{ID: x.id}
		return true
	}
	x.call.EstimateTokens = x.estimateTokens()
	x.call.UnitPriced = unitPriced(x.needs.Modality)
	res, err := m.Reserve(x.call.WithPlugin(m.Info().ID), x.estimate())
	if err != nil {
		x.fail(meterRefusal(err))
		return false
	}
	if res == nil {
		res = &plugin.Reservation{ID: x.id}
	}
	x.reservation = res
	return true
}

// estimateTokens is the request's input estimate plus its output limit.
func (x *exchange) estimateTokens() int64 {
	n := int64(x.needs.InputTokensEst)
	if x.req != nil && x.req.MaxOutputTokens != nil {
		n += int64(*x.req.MaxOutputTokens)
	}
	return n
}

// estimate prices the request's input estimate and output limit at the
// caller's rate: the model's list price (for a preset, the highest of its
// models' and its own), else its most expensive route, times the group
// multiplier.
func (x *exchange) estimate() int64 {
	var in, out int64
	var units map[string]int64
	listed := false
	names := []string{x.in.Model}
	if t := x.snap.Table; t != nil {
		names = append(names, t.Presets[x.in.Model].Models...)
	}
	for _, m := range names {
		if pr, ok := x.snap.listPrice(m); ok {
			in, out, listed = max(in, pr.Input), max(out, pr.Output), true
			units = maxUnits(units, pr.Units)
		}
	}
	if !listed && x.snap.Table != nil {
		for _, rt := range routesOf(x.snap.Table, x.in.Model) {
			if pr, ok := x.snap.costRate(rt.UpstreamID, rt.UpstreamModel); ok {
				in, out = max(in, pr.Input), max(out, pr.Output)
				units = maxUnits(units, pr.Units)
			}
		}
	}
	pr := Price{Input: in, Output: out, Units: units}.scaled(x.multiplier())
	var maxOut int64
	if x.req != nil && x.req.MaxOutputTokens != nil {
		maxOut = int64(*x.req.MaxOutputTokens)
	}
	return pr.tokenEstimate(int64(x.needs.InputTokensEst), maxOut) + Price{Units: pr.Units}.Cost(expectedUnits(x.req))
}

// unitPriced reports the operations a price may charge per unit
// (plugin.Rates.Units) rather than per token.
func unitPriced(m ir.Modality) bool {
	switch m {
	case ir.ModalityImageGeneration, ir.ModalityImageEdit, ir.ModalitySpeech, ir.ModalityTranscription, ir.ModalityRerank:
		return true
	}
	return false
}

// expectedUnits are the media units a request asks for.
func expectedUnits(req *ir.Request) ir.Usage {
	if req == nil || req.Units == nil {
		return ir.Usage{}
	}
	m := req.Units
	return ir.Usage{Images: int64(m.Images), ImageSize: m.ImageSize, ImageQuality: m.ImageQuality, Chars: m.Chars}
}

// maxUnits keeps, per unit key, the higher rate.
func maxUnits(dst, src map[string]int64) map[string]int64 {
	for k, v := range src {
		if dst == nil {
			dst = map[string]int64{}
		}
		dst[k] = max(dst[k], v)
	}
	return dst
}
