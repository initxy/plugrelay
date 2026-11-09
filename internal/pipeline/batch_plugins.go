package pipeline

import (
	"bytes"
	"net/http"
	"net/url"
	"reflect"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// errBatchBody is what a request plugin gets asking for the body of a
// call made for a whole batch.
var errBatchBody = ir.Errorf(ir.ErrInvalidRequest, "batch requests have no decoded body")

// admit runs the request plugins once for a batch being created, on a
// call that names its model and carries no body, as for any request: a
// refusal (access lists, blocked models, rate limits) answers the client.
// It returns the plugins that asked for the body: they inspect content,
// so they run again on every request of the batch (lineHooks.check).
func (b *batchReq) admit(model string) ([]plugin.Plugin, bool) {
	chain := b.p.chain()
	if chain == nil {
		return nil, true
	}
	var asked bool
	var content []plugin.Plugin
	call := b.callWithRequest(b.caller, b.rec.ID, model, func() (*ir.Request, error) {
		asked = true
		return nil, errBatchBody
	})
	b.hooks = call
	for _, pl := range chain.Plugins {
		h, ok := pl.(plugin.RequestHook)
		if !ok || !supports(pl, "middleware.onRequest") {
			continue
		}
		info := pl.Info()
		b.entered = append(b.entered, pl)
		asked = false
		reply, err := h.OnRequest(call.WithPlugin(info.ID))
		if gw, stop := b.hookRefusal(info, reply, err); stop {
			b.fail(gw)
			return nil, false
		}
		if asked {
			content = append(content, pl)
		}
	}
	return content, true
}

// callWithRequest is a plugin call for caller under id whose Request is
// req.
func (b *batchReq) callWithRequest(caller *plugin.Caller, id, model string, req func() (*ir.Request, error)) *plugin.Call {
	c := plugin.NewCall(b.ctx(), id, b.p.settingsFor(caller), req)
	// Protocol is the client's family, whose refusals (402 for Anthropic)
	// a plugin may pick.
	c.Started, c.Caller, c.Protocol, c.Model = b.started, caller, b.codec.Name(), model
	c.ClientIP, c.Header = b.rec.ClientIP, b.r.Header.Clone()
	return c
}

// hookRefusal reads a request hook's outcome as the main pipeline does:
// a reply or a deliberate rejection stops, a malfunction follows the
// plugin's FailPolicy.
func (b *batchReq) hookRefusal(info plugin.Info, reply *plugin.Reply, err error) (*ir.GwError, bool) {
	if err != nil {
		return b.p.hookFailure(info, "batch hook", b.rec.ID, err)
	}
	if reply == nil {
		return nil, false
	}
	if reply.Error != nil {
		return reply.Error, true
	}
	return ir.Errorf(ir.ErrPermission, "plugin %s answers requests itself, which a batch cannot carry", info.ID), true
}

// lineHooks runs plugins on each request of a batch, decoded with its
// protocol's codec: the content-inspecting request plugins, whose
// refusals refuse the batch, then the body mutators (redaction, parameter
// caps), whose declared edits are written into the line as on a patch-tier
// attempt. A request plugin that rewrites the decoded request refuses the
// batch, since a line is forwarded in its own encoding. Attempt hooks do
// not run: a batch line is never converted, and its upstream call carries
// no per-line headers.
type lineHooks struct {
	b        *batchReq
	hooks    []plugin.Plugin
	mutators []plugin.Plugin
	attempt  *plugin.Attempt
	codec    protocol.Codec
	route    protocol.Route
	header   http.Header
	model    string
	n        int // requests checked so far
}

func (b *batchReq) lineHooks(hooks []plugin.Plugin, t *batchTarget, proto, path, model string) (*lineHooks, *ir.GwError) {
	var mutators []plugin.Plugin
	if chain := b.p.chain(); chain != nil {
		for _, pl := range chain.Plugins {
			if _, ok := pl.(plugin.BodyMutator); ok && supports(pl, "middleware.mutateBody") && len(pl.Info().Mutates) > 0 {
				mutators = append(mutators, pl)
			}
		}
	}
	if len(hooks) == 0 && len(mutators) == 0 {
		return nil, nil
	}
	cand := t.cand
	cand.Protocol = proto
	h := &lineHooks{b: b, hooks: hooks, mutators: mutators, header: b.r.Header.Clone(), model: model,
		attempt: &plugin.Attempt{N: 1, Candidate: cand, Tier: plugin.TierPatch, UpstreamModel: cand.UpstreamModel}}
	codec, ok := protocol.Get(proto)
	if !ok {
		return nil, ir.Errorf(ir.ErrInternal, "no codec for %s", proto)
	}
	h.codec = codec
	for _, rt := range codec.Routes() {
		if rt.Path == path {
			h.route = rt
			return h, nil
		}
	}
	if len(hooks) == 0 {
		return h, nil
	}
	return nil, ir.Errorf(ir.ErrPermission, "plugin %s inspects requests, and %s requests cannot be inspected in a batch",
		hooks[0].Info().ID, path)
}

// check runs the plugins on one request body and returns the body to
// forward; where names the request in errors.
func (h *lineHooks) check(where string, body []byte) ([]byte, *ir.GwError) {
	if h == nil {
		return body, nil
	}
	decode := func() (*ir.Request, error) {
		r := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: h.route.Path}, Header: h.header}
		in, err := h.codec.Sniff(r, h.route, body)
		if err != nil {
			return nil, err
		}
		req, _, err := h.codec.DecodeRequest(r, in, body)
		return req, err
	}
	stop := func(gw *ir.GwError) ([]byte, *ir.GwError) {
		out := *gw
		out.Message = where + ": " + gw.Message
		return nil, &out
	}
	var req, orig *ir.Request
	call := h.b.callWithRequest(h.b.caller, h.b.rec.ID, h.model, func() (*ir.Request, error) { return req, nil })
	h.n++
	call.BatchIndex = h.n
	if len(h.hooks) > 0 {
		var err error
		if req, err = decode(); err != nil {
			return nil, ir.Errorf(ir.ErrInvalidRequest, "%s cannot be inspected by %s: %v", where, h.hooks[0].Info().ID, err)
		}
		orig, _ = decode()
	}
	for _, pl := range h.hooks {
		info := pl.Info()
		reply, err := pl.(plugin.RequestHook).OnRequest(call.WithPlugin(info.ID))
		if gw, refused := h.b.hookRefusal(info, reply, err); refused {
			return stop(gw)
		}
		if !reflect.DeepEqual(req, orig) {
			return nil, ir.Errorf(ir.ErrPermission, "%s: plugin %s rewrites requests for this key, which a batch cannot carry", where, info.ID)
		}
	}
	for _, pl := range h.mutators {
		info := pl.Info()
		edited, err := pl.(plugin.BodyMutator).MutateBody(call.WithPlugin(info.ID), h.attempt, bytes.Clone(body))
		if err != nil {
			if gw, refused := h.b.hookRefusal(info, nil, err); refused {
				return stop(gw)
			}
			continue
		}
		body, _ = keepDeclared(body, edited, info.Mutates)
	}
	return body, nil
}
