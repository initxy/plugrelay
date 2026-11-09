package router

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Query is one routing request.
type Query struct {
	Model          string
	Caller         plugin.Caller
	ClientProtocol string
	Needs          ir.Needs
	// SessionKey is the resolved session identifier; "" disables stickiness.
	SessionKey string
	// TurnToken is a token from IssueTurnToken echoed by the client; a valid
	// one pins its candidate first.
	TurnToken string
	// Allowed reports whether the caller may use a model a preset lists;
	// nil admits every model.
	Allowed func(model string) bool
	// Assess grades converting this request for an endpoint and lists the
	// needs it cannot meet. nil means every endpoint is lossless.
	Assess           func(ep plugin.Endpoint) (ir.Fidelity, []string)
	AllowDiscouraged bool
	// PersonalOnly restricts routing to the caller's own personal credentials.
	PersonalOnly bool
	// Exclude holds Keys of candidates already tried for this request.
	Exclude map[string]bool
}

// Served says which model a candidate serves.
type Served struct {
	Model  string // model whose route produced the candidate
	Preset string // the requested preset, "" when the model was requested
}

// Plan is the ordered result of routing.
type Plan struct {
	// Candidates in try order. A Selector may reorder but never add.
	Candidates []plugin.Candidate
	// Strategy is the requested preset's pick strategy; "" for a model.
	Strategy string
	// Binding is the session's current binding, valid or not, when one exists.
	Binding *Binding
	served  map[string]Served
}

// Served reports the model a candidate of this plan serves.
func (p *Plan) Served(c plugin.Candidate) Served { return p.served[Key(c)] }

// drops counts why candidates were filtered, to pick the error kind.
type drops struct {
	routes      int // routes visible to the caller
	reachable   int // routes whose upstream is enabled
	capable     int // endpoints that passed Assess
	impossible  bool
	discouraged bool
	missing     []string
	cooling     int
	retryAt     time.Time
	busy        int
	excluded    int
	visible     int // credentials the caller may use at all
}

func (d *drops) cool(until time.Time) {
	d.cooling++
	if !until.IsZero() && (d.retryAt.IsZero() || until.Before(d.retryAt)) {
		d.retryAt = until
	}
}

type entry struct {
	cand   plugin.Candidate
	served Served
	user   bool // credential recently used by this user
	epRank int
	crRank int
}

type routeGroup struct {
	route   Route
	level   int
	entries []entry
	sticky  bool
	user    bool
	fit     int // rank of the route's best endpoint, lower is better
}

// Plan returns the candidates for q in try order, or a *PlanError.
//
// State read failures are treated as "no live restriction": losing the state
// backend costs cache hits and balance, never availability.
func (r *Router) Plan(ctx context.Context, q Query) (*Plan, error) {
	t := r.table.Load()
	now := r.now()
	levels, preset, strategy := t.resolve(q.Model, q.Allowed)

	var pinKey string
	if q.TurnToken != "" {
		pinKey, _ = r.ParseTurnToken(q.TurnToken)
	}
	plan := &Plan{served: map[string]Served{}, Strategy: strategy}
	var bindKey string
	if q.SessionKey != "" {
		if b, ok := r.loadBinding(ctx, q.SessionKey); ok {
			plan.Binding = &b
			if b.Valid(now) {
				bindKey = b.Key
			}
		}
	}
	newSession := pinKey == "" && bindKey == ""
	userKey := callerKey(q.Caller)
	recent := map[string]bool{}
	if userKey != "" {
		for _, k := range r.loadRecent(ctx, userKey) {
			recent[k] = true
		}
	}

	var d drops
	var groups []*routeGroup
	assessed := map[string]struct {
		f       ir.Fidelity
		missing []string
	}{}
	for li, lvl := range levels {
		for _, rt := range lvl.routes {
			if !groupAllowed(rt.Groups, q.Caller.GroupID) {
				continue
			}
			d.routes++
			up, ok := t.Upstreams[rt.UpstreamID]
			if !ok || !up.Enabled {
				continue
			}
			d.reachable++
			type epInfo struct {
				ep   plugin.Endpoint
				f    ir.Fidelity
				same bool
			}
			var eps []epInfo
			for _, ep := range up.Endpoints {
				ak := up.ID + "|" + ep.ID
				a, done := assessed[ak]
				if !done {
					a.f = ir.Lossless
					if q.Assess != nil {
						a.f, a.missing = q.Assess(ep)
					}
					assessed[ak] = a
				}
				switch a.f {
				case ir.Impossible:
					d.impossible = true
					d.missing = appendUniq(d.missing, a.missing...)
					continue
				case ir.Discouraged:
					if !q.AllowDiscouraged {
						d.discouraged = true
						d.missing = appendUniq(d.missing, a.missing...)
						continue
					}
				}
				eps = append(eps, epInfo{ep: ep, f: a.f, same: ep.Protocol == q.ClientProtocol})
			}
			if len(eps) == 0 {
				continue
			}
			d.capable += len(eps)
			if until, open := r.breakerBlocks(ctx, up.ID); open {
				d.cool(until)
				continue
			}
			// Endpoint order: same protocol first, then fidelity, then latency.
			sort.SliceStable(eps, func(i, j int) bool {
				a, b := eps[i], eps[j]
				if a.same != b.same {
					return a.same
				}
				if fr := fidRank(a.f) - fidRank(b.f); fr != 0 {
					return fr < 0
				}
				return r.latencyOf(up.ID, a.ep.ID) < r.latencyOf(up.ID, b.ep.ID)
			})

			creds := r.usableCreds(ctx, q, up, rt.UpstreamModel, now, &d)
			r.randMu.Lock()
			r.opt.Rand.Shuffle(len(creds), func(i, j int) { creds[i], creds[j] = creds[j], creds[i] })
			r.randMu.Unlock()
			sort.SliceStable(creds, func(i, j int) bool {
				ui, uj := recent[credKey(up.ID, creds[i].c.ID)], recent[credKey(up.ID, creds[j].c.ID)]
				if ui != uj {
					return ui
				}
				return creds[i].inflight < creds[j].inflight
			})

			g := &routeGroup{route: rt, level: li, fit: fitRank(eps[0].same, eps[0].f)}
			for ci, cr := range creds {
				for ei, e := range eps {
					c := plugin.Candidate{
						UpstreamID:    up.ID,
						EndpointID:    e.ep.ID,
						CredentialID:  cr.c.ID,
						Protocol:      e.ep.Protocol,
						UpstreamModel: rt.UpstreamModel,
						Priority:      rt.Priority,
						Weight:        rt.Weight,
						SameProtocol:  e.same,
						Fidelity:      e.f,
						LatencyMs:     r.latencyOf(up.ID, e.ep.ID),
						InFlight:      cr.inflight,
						QuotaReset:    cr.quotaReset,
					}
					k := Key(c)
					if q.Exclude[k] {
						d.excluded++
						continue
					}
					isUser := recent[credKey(up.ID, cr.c.ID)]
					g.user = g.user || isUser
					if k == pinKey || (bindKey != "" && sameCred(k, bindKey)) {
						c.Sticky = true
						g.sticky = true
					}
					g.entries = append(g.entries, entry{cand: c, served: Served{Model: levels[li].name, Preset: preset}, user: isUser, epRank: ei, crRank: ci})
				}
			}
			if len(g.entries) > 0 {
				groups = append(groups, g)
			}
		}
	}

	if len(groups) == 0 {
		return nil, d.err(q.Model)
	}

	ordered := r.orderGroups(groups, newSession)
	var out []entry
	for _, g := range ordered {
		// Within a route: endpoint rank first so every credential gets the
		// preferred protocol before any falls back to another endpoint.
		sort.SliceStable(g.entries, func(i, j int) bool {
			a, b := g.entries[i], g.entries[j]
			if a.epRank != b.epRank {
				return a.epRank < b.epRank
			}
			return a.crRank < b.crRank
		})
		out = append(out, g.entries...)
	}
	// Pinned and bound candidates go first regardless of priority: the
	// session's cache lives there.
	sort.SliceStable(out, func(i, j int) bool {
		return stickRank(out[i], pinKey, bindKey) < stickRank(out[j], pinKey, bindKey)
	})
	for _, e := range out {
		plan.Candidates = append(plan.Candidates, e.cand)
		plan.served[Key(e.cand)] = e.served
	}
	return plan, nil
}

func stickRank(e entry, pinKey, bindKey string) int {
	k := Key(e.cand)
	switch {
	case k == pinKey:
		return 0
	case bindKey != "" && k == bindKey:
		return 1
	case bindKey != "" && sameCred(k, bindKey):
		return 2
	}
	return 3
}

// orderGroups orders routes: fallback level, then priority desc; within a
// priority the sticky route first, then for new sessions a route holding the
// user's recent credential, then by how well the route's best endpoint fits
// (same protocol, then fidelity), then a weighted shuffle (new sessions) or
// weight order (existing sessions).
func (r *Router) orderGroups(groups []*routeGroup, newSession bool) []*routeGroup {
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].level != groups[j].level {
			return groups[i].level < groups[j].level
		}
		return groups[i].route.Priority > groups[j].route.Priority
	})
	var out []*routeGroup
	for i := 0; i < len(groups); {
		j := i
		for j < len(groups) && groups[j].level == groups[i].level && groups[j].route.Priority == groups[i].route.Priority {
			j++
		}
		bucket := slices.Clone(groups[i:j])
		var head []*routeGroup
		rest := bucket[:0]
		for _, g := range bucket {
			if g.sticky || (newSession && g.user) {
				head = append(head, g)
			} else {
				rest = append(rest, g)
			}
		}
		sort.SliceStable(head, func(a, b int) bool { return head[a].sticky && !head[b].sticky })
		out = append(out, head...)
		sort.SliceStable(rest, func(a, b int) bool { return rest[a].fit < rest[b].fit })
		for k := 0; k < len(rest); {
			m := k
			for m < len(rest) && rest[m].fit == rest[k].fit {
				m++
			}
			tier := rest[k:m]
			if newSession {
				tier = r.weightedShuffle(tier)
			} else {
				sort.SliceStable(tier, func(a, b int) bool { return weight(tier[a].route) > weight(tier[b].route) })
			}
			out = append(out, tier...)
			k = m
		}
		i = j
	}
	return out
}

func (r *Router) weightedShuffle(gs []*routeGroup) []*routeGroup {
	gs = slices.Clone(gs)
	out := make([]*routeGroup, 0, len(gs))
	r.randMu.Lock()
	defer r.randMu.Unlock()
	for len(gs) > 0 {
		total := 0
		for _, g := range gs {
			total += weight(g.route)
		}
		n := r.opt.Rand.IntN(total)
		idx := 0
		for k, g := range gs {
			n -= weight(g.route)
			if n < 0 {
				idx = k
				break
			}
		}
		out = append(out, gs[idx])
		gs = slices.Delete(gs, idx, idx+1)
	}
	return out
}

func weight(rt Route) int {
	if rt.Weight <= 0 {
		return 1
	}
	return rt.Weight
}

type usableCred struct {
	c          Cred
	inflight   int
	quotaReset time.Time // from the last quota probe; zero when unknown or past
}

func (r *Router) usableCreds(ctx context.Context, q Query, up Upstream, model string, now time.Time, d *drops) []usableCred {
	var out []usableCred
	active := 0
	var idle []usableCred
	creds := up.Credentials
	if up.Keyless() {
		creds = []Cred{{Kind: KindShared}}
	}
	for _, c := range creds {
		personal := c.Kind == KindPersonal
		if personal && (c.OwnerID == "" || c.OwnerID != q.Caller.UserID) {
			continue // never visible to anyone else, not even as "cooling"
		}
		if q.PersonalOnly && !personal {
			continue
		}
		d.visible++
		switch c.Status {
		case StatusDisabled, StatusQuarantined, StatusManual:
			continue
		case StatusCooling, StatusRefreshing:
			if c.StatusUntil.IsZero() || now.Before(c.StatusUntil) {
				d.cool(c.StatusUntil)
				continue
			}
		}
		if _, q := r.getMark(ctx, kQuar(up.ID, c.ID)); q {
			continue
		}
		if _, m := r.getMark(ctx, kManual(up.ID, c.ID)); m {
			continue
		}
		if m, ok := r.getMark(ctx, kRefresh(up.ID, c.ID)); ok {
			d.cool(m.Until)
			continue
		}
		if m, ok := r.getMark(ctx, kCool(up.ID, c.ID)); ok && now.Before(m.Until) {
			d.cool(m.Until)
			continue
		}
		if m, ok := r.getMark(ctx, kCoolModel(up.ID, c.ID, model)); ok && now.Before(m.Until) {
			d.cool(m.Until)
			continue
		}
		n := r.counter(ctx, kInflight(up.ID, c.ID))
		if n > 0 {
			active++
		}
		if c.MaxConcurrency > 0 && n >= c.MaxConcurrency {
			d.busy++
			continue
		}
		u := usableCred{c: c, inflight: n}
		if qr, ok := r.QuotaOf(ctx, up.ID, c.ID); ok && qr.ResetAt.After(now) {
			u.quotaReset = qr.ResetAt
		}
		if n > 0 {
			out = append(out, u)
		} else {
			idle = append(idle, u)
		}
	}
	if up.MaxActiveCredentials > 0 {
		room := up.MaxActiveCredentials - active
		if room < 0 {
			room = 0
		}
		if len(idle) > room {
			d.busy += len(idle) - room
			// Keep idle credentials in table order so the same few get woken.
			idle = idle[:room]
		}
	}
	return append(out, idle...)
}

func (d *drops) err(model string) error {
	e := &PlanError{Model: model}
	switch {
	case d.routes == 0 || d.reachable == 0:
		e.Kind = ErrNoRoute
	case d.capable == 0:
		e.Kind = ErrImpossible
		e.Missing = d.missing
		e.Discouraged = d.discouraged
	case d.visible == 0 && d.cooling == 0:
		e.Kind = ErrNoRoute
	case d.cooling > 0 && d.busy == 0:
		e.Kind = ErrCooling
		e.RetryAt = d.retryAt
	case d.busy > 0:
		e.Kind = ErrBusy
	default:
		e.Kind = ErrExhausted
	}
	return e
}

func groupAllowed(groups []string, g string) bool {
	return len(groups) == 0 || (g != "" && slices.Contains(groups, g))
}

func callerKey(c plugin.Caller) string {
	if c.UserID != "" {
		return "u:" + c.UserID
	}
	if c.KeyID != "" {
		return "k:" + c.KeyID
	}
	return ""
}

// fitRank orders endpoints as the per-route endpoint sort does: same
// protocol first, then fidelity.
func fitRank(same bool, f ir.Fidelity) int {
	if same {
		return fidRank(f)
	}
	return 4 + fidRank(f)
}

func fidRank(f ir.Fidelity) int {
	switch f {
	case ir.Lossless, "":
		return 0
	case ir.Lossy:
		return 1
	case ir.Discouraged:
		return 2
	}
	return 3
}

// sameCred reports whether two candidate keys share upstream, credential and
// upstream model (the endpoint may differ).
func sameCred(a, b string) bool {
	pa, pb := splitKey(a), splitKey(b)
	return pa[0] == pb[0] && pa[2] == pb[2] && pa[3] == pb[3]
}

func splitKey(k string) [4]string {
	var out [4]string
	i := 0
	start := 0
	for p := 0; p < len(k) && i < 3; p++ {
		if k[p] == '|' {
			out[i] = k[start:p]
			i++
			start = p + 1
		}
	}
	out[i] = k[start:]
	return out
}

func appendUniq(s []string, v ...string) []string {
	for _, x := range v {
		if !slices.Contains(s, x) {
			s = append(s, x)
		}
	}
	return s
}
