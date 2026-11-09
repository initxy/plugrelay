package pipeline

import (
	"reflect"
	"slices"
	"strings"

	"github.com/initxy/plugrelay/internal/convert"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// effectiveCaps is what an endpoint can do: its codec's capabilities
// narrowed by the endpoint's own declaration. An endpoint that declares
// nothing gets the codec's.
func effectiveCaps(codec, ep ir.Capabilities) ir.Capabilities {
	if reflect.ValueOf(ep).IsZero() {
		return codec
	}
	out := codec
	if ep.Cache != "" {
		out.Cache = ep.Cache
	}
	out.Cache1h = codec.Cache1h && ep.Cache1h
	out.FunctionTools = codec.FunctionTools && ep.FunctionTools
	out.FreeformTools = codec.FreeformTools && ep.FreeformTools
	out.DeferredTools = codec.DeferredTools && ep.DeferredTools
	out.Reasoning = codec.Reasoning && ep.Reasoning
	out.Images = codec.Images && ep.Images
	out.Documents = codec.Documents && ep.Documents
	out.ToolResultImgs = codec.ToolResultImgs && ep.ToolResultImgs
	out.ServerState = codec.ServerState && ep.ServerState
	out.Compaction = codec.Compaction && ep.Compaction
	out.FillInMiddle = codec.FillInMiddle && ep.FillInMiddle
	out.Embeddings = codec.Embeddings && ep.Embeddings
	out.ImageGeneration = codec.ImageGeneration && ep.ImageGeneration
	out.ImageEdit = codec.ImageEdit && ep.ImageEdit
	out.AudioTranscription = codec.AudioTranscription && ep.AudioTranscription
	out.Speech = codec.Speech && ep.Speech
	out.Rerank = codec.Rerank && ep.Rerank
	out.ServerTools = intersect(codec.ServerTools, ep.ServerTools)
	out.EchoFields = intersect(codec.EchoFields, ep.EchoFields)
	if ep.ReasoningStyle != "" {
		out.ReasoningStyle = ep.ReasoningStyle
	}
	if !out.Reasoning {
		out.ReasoningStyle = ""
	}
	if ep.MaxContext > 0 && (out.MaxContext == 0 || ep.MaxContext < out.MaxContext) {
		out.MaxContext = ep.MaxContext
	}
	if ep.MaxTokensField != "" {
		out.MaxTokensField = ep.MaxTokensField
	}
	out.UnsupportedParams = slices.Clone(codec.UnsupportedParams)
	for _, p := range ep.UnsupportedParams {
		if !slices.Contains(out.UnsupportedParams, p) {
			out.UnsupportedParams = append(out.UnsupportedParams, p)
		}
	}
	return out
}

func intersect(a, b []string) []string {
	var out []string
	for _, v := range a {
		if slices.Contains(b, v) {
			out = append(out, v)
		}
	}
	return out
}

// convertContext is the conversion context for sending this request to ep.
func (x *exchange) convertContext(ep *plugin.Endpoint) (*convert.Context, bool) {
	target, ok := protocol.Get(ep.Protocol)
	if !ok {
		return nil, false
	}
	return &convert.Context{
		Source:           x.codec.Capabilities(),
		Target:           effectiveCaps(target.Capabilities(), ep.Caps),
		SameProtocol:     ep.Protocol == x.codec.Name(),
		TargetIssuer:     ep.Issuer,
		SessionID:        x.session,
		Sealer:           x.p.d.Sealer,
		AllowDiscouraged: x.snap.AllowDiscouraged,
		HistoryIssuers:   x.issuers,
	}, true
}

// dropsServerTools reports whether ep can serve the request only with some
// offered server tools removed, which the byte-level tiers cannot do.
func (x *exchange) dropsServerTools(ep *plugin.Endpoint) bool {
	c, ok := x.convertContext(ep)
	return ok && x.decodeErr == nil && convert.DropsServerTools(x.needs, c)
}

// assess grades an endpoint for the router.
func (x *exchange) assess(ep plugin.Endpoint) (ir.Fidelity, []string) {
	c, ok := x.convertContext(&ep)
	if !ok {
		return ir.Impossible, []string{"protocol:" + ep.Protocol}
	}
	if x.decodeErr != nil {
		if c.SameProtocol {
			return ir.Lossless, nil
		}
		return ir.Impossible, []string{"decodable_request"}
	}
	return convert.Assess(x.needs, c)
}

// plan asks the router for candidates, minus the excluded ones, and lets
// the selector reorder them.
func (x *exchange) plan(exclude map[string]bool) (*router.Plan, error) {
	q := router.Query{
		Model:            x.in.Model,
		Caller:           *x.caller,
		ClientProtocol:   x.codec.Name(),
		Needs:            x.needs,
		SessionKey:       x.session,
		TurnToken:        x.in.Session.TurnState,
		Allowed:          x.allowed,
		Assess:           x.assess,
		AllowDiscouraged: x.snap.AllowDiscouraged,
		Exclude:          exclude,
	}
	plan, err := x.p.d.Router.Plan(x.ctx, q)
	if err != nil {
		return nil, err
	}
	if !x.planned {
		x.planned = true
		if b := plan.Binding; b != nil {
			x.lastIssuer = x.issuerOfKey(b.Key)
			x.sentSession = b.SentSessionID
		}
	}
	if plan.Strategy != "" {
		if x.served == nil {
			x.served = map[string]string{}
		}
		for _, c := range plan.Candidates {
			x.served[router.Key(c)] = plan.Served(c).Model
		}
	}
	x.price(plan.Candidates)
	switch sel := x.chain.Selector; {
	case len(plan.Candidates) < 2:
	case plan.Strategy != "":
		if x.chain.Pick != nil {
			plan.Candidates = rerank(plan.Candidates, x.chain.Pick(plan.Strategy, slices.Clone(plan.Candidates)))
		}
	case sel != nil && supports(sel, "selector.rank"):
		plan.Candidates = rerank(plan.Candidates, sel.Rank(x.call.WithPlugin(sel.Info().ID), slices.Clone(plan.Candidates)))
	}
	// After the selector: its ordering (priority, stickiness) would
	// otherwise put a demoted endpoint back in front.
	plan.Candidates = x.skipUnserved(plan.Candidates)
	return plan, nil
}

// allowed reports whether the caller may use a model a preset lists.
func (x *exchange) allowed(model string) bool { return x.snap.allows(x.caller, model) }

// allows reports whether caller may use model; every router query names
// it as Allowed so a preset reaches only the members the caller may use.
func (s *Snapshot) allows(caller *plugin.Caller, model string) bool {
	return s.Visible == nil || s.Visible(caller, model)
}

// price fills each candidate's input and output rate from the source its
// cost is computed from (price rules, else the manifest's list prices);
// unpriced candidates stay at 0, as their cost does.
func (x *exchange) price(cands []plugin.Candidate) {
	if x.snap.Price == nil {
		return
	}
	for i := range cands {
		if p, ok := x.snap.Price(cands[i].UpstreamID, cands[i].UpstreamModel); ok {
			cands[i].PriceInMicro, cands[i].PriceOutMicro = p.Input, p.Output
		}
	}
}

// rerank accepts a selector's order but only over the planned candidates:
// unknown or duplicate entries are dropped and omitted ones keep their
// relative order at the end.
func rerank(planned, ranked []plugin.Candidate) []plugin.Candidate {
	known := map[string]plugin.Candidate{}
	for _, c := range planned {
		known[router.Key(c)] = c
	}
	out := make([]plugin.Candidate, 0, len(planned))
	seen := map[string]bool{}
	for _, c := range ranked {
		k := router.Key(c)
		if orig, ok := known[k]; ok && !seen[k] {
			seen[k] = true
			out = append(out, orig)
		}
	}
	for _, c := range planned {
		if k := router.Key(c); !seen[k] {
			out = append(out, c)
		}
	}
	return out
}

// issuerOfKey resolves a candidate key's endpoint issuer.
func (x *exchange) issuerOfKey(key string) string {
	parts := strings.SplitN(key, "|", 4)
	if len(parts) < 2 {
		return ""
	}
	if ep := x.endpoint(parts[0], parts[1]); ep != nil {
		return ep.Issuer
	}
	return ""
}

// endpoint finds an endpoint in the routing table, then in the upstream
// config.
func (x *exchange) endpoint(upstreamID, endpointID string) *plugin.Endpoint {
	if t := x.snap.Table; t != nil {
		if u, ok := t.Upstreams[upstreamID]; ok {
			for i := range u.Endpoints {
				if u.Endpoints[i].ID == endpointID {
					return &u.Endpoints[i]
				}
			}
		}
	}
	if u := x.snap.Upstreams[upstreamID]; u != nil && u.Config != nil {
		for i := range u.Config.Endpoints {
			if u.Config.Endpoints[i].ID == endpointID {
				return &u.Config.Endpoints[i]
			}
		}
	}
	return nil
}

// routesOf returns the routes of a model, or of every model of a preset in
// the preset's order.
func routesOf(t *router.Table, model string) []router.Route {
	if m, ok := t.Models[model]; ok {
		return m.Routes
	}
	var out []router.Route
	for _, name := range t.Presets[model].Models {
		out = append(out, t.Models[name].Routes...)
	}
	return out
}
