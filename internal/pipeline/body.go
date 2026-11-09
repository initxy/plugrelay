package pipeline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/internal/convert"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// usageOptIn are the protocols whose streams carry usage only when the
// request sets stream_options.include_usage.
var usageOptIn = map[string]bool{"openai-chat": true, "openai-completions": true}

// limitFields are the top-level output-limit names the codecs write.
var limitFields = []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}

// patchBody prepares a same-protocol attempt from the client's bytes. The
// tier is passthrough when no edit was needed.
func (x *exchange) patchBody(a *attempt) *ir.GwError {
	if x.upload != nil {
		return x.uploadBody(a)
	}
	body := x.body
	var steps []string
	a.pa.Tier = plugin.TierPatch
	model := a.cand.UpstreamModel
	if model != x.in.Model {
		out, err := a.target.SetModel(body, model)
		if err != nil {
			return ir.Errorf(ir.ErrInvalidRequest, "rewrite model: %v", err)
		}
		if !bytes.Equal(out, body) {
			steps = append(steps, "patch: model")
		}
		body = out
	}
	if x.needs.OpaqueState && x.lastIssuer != "" && x.lastIssuer != a.ep.Issuer {
		out, n, err := a.target.StripOpaque(body)
		if err == nil && n > 0 {
			body = out
			steps = append(steps, "patch: "+strconv.Itoa(n)+" private state items of issuer "+x.lastIssuer+" removed")
		}
	}
	// Only an endpoint that names another limit field than its protocol's
	// default gets the client's field renamed.
	if want := effectiveCaps(a.target.Capabilities(), a.ep.Caps).MaxTokensField; want != a.target.Capabilities().MaxTokensField {
		if out, ok := renameLimit(body, want); ok {
			body = out
			steps = append(steps, "patch: output limit sent as "+want)
		}
	}
	if x.in.Stream && usageOptIn[a.target.Name()] && !gjson.GetBytes(body, "stream_options.include_usage").Bool() {
		// Without it the upstream reports no usage and billing falls back
		// to a character estimate; the client still gets the stream it asked for.
		if out, err := sjson.SetBytes(body, "stream_options.include_usage", true); err == nil {
			body, a.hideUsage = out, true
			steps = append(steps, "patch: stream_options.include_usage")
		}
	}
	body, mSteps, mutated, gw := x.mutate(a, body)
	if gw != nil {
		return gw
	}
	steps = append(steps, mSteps...)
	body = x.applyDowngrade(a, body, &steps)

	a.body, a.sent = body, x.clientReq()
	a.tier = plugin.TierPassthrough
	if mutated || !bytes.Equal(body, x.body) {
		a.tier = plugin.TierPatch
	}
	if len(steps) > 0 {
		a.diag = &ir.Diagnostic{Fidelity: ir.Lossless, Steps: steps}
	}
	in := x.in
	in.Model = model
	a.path = a.target.UpstreamPath(&ir.Request{Model: model, Stream: x.in.Stream}, in)
	return nil
}

// convertBody decodes a fresh copy of the request, converts it for the
// endpoint and encodes it in the endpoint's protocol.
func (x *exchange) convertBody(a *attempt) *ir.GwError {
	if x.decodeErr != nil {
		return asGwError(x.decodeErr, ir.ErrInvalidRequest)
	}
	req, needs, err := x.codec.DecodeRequest(x.r, x.in, x.body)
	if err != nil {
		return asGwError(err, ir.ErrInvalidRequest)
	}
	cctx, _ := x.convertContext(a.ep)
	cctx.SessionID = upstreamSession(x.session)
	if x.sentSession != "" {
		cctx.SessionID = x.sentSession
	}
	diag, err := convert.Request(req, needs, cctx)
	if err != nil {
		var me *convert.MissingError
		if errors.As(err, &me) {
			return ir.Errorf(ir.ErrCapabilityMissing, "endpoint cannot serve: %s", strings.Join(me.Missing, ", "))
		}
		return ir.Errorf(ir.ErrInternal, "convert: %v", err)
	}
	req.Model = a.cand.UpstreamModel
	a.pa.Tier = plugin.TierConvert
	if gw := x.runAttemptHooks(a, req); gw != nil {
		a.fatal = true
		return gw
	}
	body, err := a.target.EncodeRequest(req)
	if err != nil {
		return ir.Errorf(ir.ErrInternal, "encode for %s: %v", a.ep.Protocol, err)
	}
	if want := cctx.Target.MaxTokensField; want != "" {
		if out, ok := renameLimit(body, want); ok {
			body = out
		}
	}
	body = x.applyDowngrade(a, body, &diag.Steps)

	a.body, a.sent, a.cctx, a.diag = body, req, cctx, &diag
	a.tier = plugin.TierConvert
	in := protocol.Inbound{Op: x.in.Op, Model: a.cand.UpstreamModel, Stream: x.in.Stream}
	if cctx.SameProtocol {
		in.Action = x.in.Action
	}
	a.path = a.target.UpstreamPath(req, in)
	return nil
}

// upstreamSession is the session id sent to upstreams for a resolved
// session; hashing keeps the relay's key ids out of upstream requests.
func upstreamSession(session string) string {
	if session == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(session))
	return "pr-" + hex.EncodeToString(sum[:12])
}

// renameLimit moves a top-level output-limit field to want. Nested targets
// are written by their codec and left alone.
func renameLimit(body []byte, want string) ([]byte, bool) {
	if want == "" || strings.ContainsAny(want, ".#*?") || gjson.GetBytes(body, want).Exists() {
		return body, false
	}
	for _, f := range limitFields {
		if f == want {
			continue
		}
		v := gjson.GetBytes(body, f)
		if !v.Exists() {
			continue
		}
		out, err := sjson.SetRawBytes(body, want, []byte(v.Raw))
		if err != nil {
			return body, false
		}
		if out, err = sjson.DeleteBytes(out, f); err != nil {
			return body, false
		}
		return out, true
	}
	return body, false
}

// mutate runs the body mutators of the patch tier, keeping from each only
// the edits to the paths it declared in Info.Mutates. changedAny reports
// whether those edits changed the body, which makes the attempt a patch.
func (x *exchange) mutate(a *attempt, body []byte) (out []byte, steps []string, changedAny bool, gw *ir.GwError) {
	for _, pl := range x.entered {
		m, ok := pl.(plugin.BodyMutator)
		if !ok || !supports(pl, "middleware.mutateBody") {
			continue
		}
		info := pl.Info()
		if len(info.Mutates) == 0 {
			continue
		}
		edited, err := m.MutateBody(x.call.WithPlugin(info.ID), a.pa, bytes.Clone(body))
		if err != nil {
			if gw, stop := x.hookError(info, "body mutator", err); stop {
				a.fatal = true
				return nil, nil, changedAny, gw
			}
			continue
		}
		kept, changed := keepDeclared(body, edited, info.Mutates)
		if changed {
			steps = append(steps, "patch: "+info.ID)
			changedAny = true
		}
		body = kept
	}
	return body, steps, changedAny, nil
}

// keepDeclared applies to orig only the values edited has at paths. A "#"
// segment in a declared path stands for every array index. Each array is
// walked once and unchanged subtrees are skipped by comparing their raw
// bytes, so the cost stays linear in the body size.
func keepDeclared(orig, edited []byte, paths []string) ([]byte, bool) {
	if bytes.Equal(orig, edited) {
		return orig, false
	}
	out := orig
	changed := false
	ed := gjson.ParseBytes(edited)
	for _, p := range paths {
		raw, ch, _ := mergeDeclared(gjson.ParseBytes(out), ed, strings.Split(p, "."))
		if ch {
			out, changed = []byte(raw), true
		}
	}
	return out, changed
}

// mergeDeclared returns orig with edited's values at the path segs, whether
// that differs from orig, and whether the result exists at all (false when
// the edit deleted it).
func mergeDeclared(orig, edited gjson.Result, segs []string) (raw string, changed, exists bool) {
	if orig.Exists() && edited.Exists() && orig.Raw == edited.Raw {
		return orig.Raw, false, true
	}
	if len(segs) == 0 {
		if !edited.Exists() {
			return "", orig.Exists(), false
		}
		return edited.Raw, true, true
	}
	seg, rest := segs[0], segs[1:]
	if seg == "#" {
		if !orig.IsArray() && !edited.IsArray() {
			return orig.Raw, false, orig.Exists()
		}
		var oa, ea []gjson.Result
		if orig.IsArray() {
			oa = orig.Array()
		}
		if edited.IsArray() {
			ea = edited.Array()
		}
		elems := make([]string, 0, max(len(oa), len(ea)))
		anyCh := false
		for j := range max(len(oa), len(ea)) {
			var o, e gjson.Result
			if j < len(oa) {
				o = oa[j]
			}
			if j < len(ea) {
				e = ea[j]
			}
			r, ch, ok := mergeDeclared(o, e, rest)
			anyCh = anyCh || ch
			if ok {
				elems = append(elems, r)
			}
		}
		if !anyCh {
			return orig.Raw, false, orig.Exists()
		}
		return "[" + strings.Join(elems, ",") + "]", true, true
	}
	r, ch, ok := mergeDeclared(orig.Get(seg), edited.Get(seg), rest)
	if !ch {
		return orig.Raw, false, orig.Exists()
	}
	base := orig.Raw
	if !orig.Exists() {
		if !ok {
			return "", false, false
		}
		base = "{}"
	}
	var err error
	if ok {
		base, err = sjson.SetRaw(base, seg, r)
	} else {
		base, err = sjson.Delete(base, seg)
	}
	if err != nil {
		return orig.Raw, false, orig.Exists()
	}
	return base, true, true
}

// applyDowngrade applies a verdict's downgrade edits for this candidate.
func (x *exchange) applyDowngrade(a *attempt, body []byte, steps *[]string) []byte {
	edits := x.downgrade[router.Key(a.cand)]
	for path, raw := range edits {
		if b, err := sjson.SetRawBytes(body, path, raw); err == nil {
			body = b
			*steps = append(*steps, "downgrade: "+path+"="+string(raw))
		}
	}
	return body
}

// clientReq is the decoded client request, or a stub carrying the model
// when the request could not be decoded.
func (x *exchange) clientReq() *ir.Request {
	if x.req != nil {
		return x.req
	}
	return &ir.Request{Model: x.in.Model, Stream: x.in.Stream}
}

// uploadBody prepares a same-protocol attempt of a multipart upload: the
// parts go out as received, with the model field set to the upstream
// model (see upload). Body mutators and downgrade edits, which address
// JSON paths, do not apply.
func (x *exchange) uploadBody(a *attempt) *ir.GwError {
	model := a.cand.UpstreamModel
	a.sent = x.clientReq()
	a.tier = plugin.TierPassthrough
	if model != x.in.Model {
		a.tier = plugin.TierPatch
		a.diag = &ir.Diagnostic{Fidelity: ir.Lossless, Steps: []string{"patch: model"}}
	}
	in := x.in
	in.Model = model
	a.path = a.target.UpstreamPath(&ir.Request{Model: model}, in)
	return nil
}
