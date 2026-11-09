package pipeline

import (
	"slices"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/tidwall/gjson"
)

// unservedTTL is how long an endpoint that has no route for an operation is
// tried last for that operation.
const unservedTTL = time.Hour

// unservedSightings route-missing answers within unservedWindow mark an
// endpoint, so one stray 404 (a gateway's HTML page during a deploy) does
// not demote a working endpoint cluster-wide.
const (
	unservedSightings = 2
	unservedWindow    = 10 * time.Minute
)

// skipUnserved moves candidates whose endpoint is known not to serve this
// operation to the end of the plan, after the selector has ranked it,
// keeping the selector's relative order: they are tried only when every
// other candidate failed. A wrong mark therefore costs a fallback, unless
// the attempt limit runs out on the candidates in front of it.
func (x *exchange) skipUnserved(cands []plugin.Candidate) []plugin.Candidate {
	if len(cands) < 2 {
		return cands
	}
	if x.unserved == nil {
		x.unserved = map[string]bool{}
	}
	keep := make([]plugin.Candidate, 0, len(cands))
	var last []plugin.Candidate
	for _, c := range cands {
		k := c.UpstreamID + "|" + c.EndpointID
		gone, seen := x.unserved[k]
		if !seen {
			gone = x.p.d.Router.Unserved(x.ctx, c.UpstreamID, c.EndpointID, string(x.in.Op))
			x.unserved[k] = gone
		}
		if gone {
			last = append(last, c)
		} else {
			keep = append(keep, c)
		}
	}
	return append(keep, last...)
}

// noteUnserved remembers an endpoint that answered that it has no route
// for this operation, logging the first time.
func (x *exchange) noteUnserved(a *attempt, o *plugin.Outcome, v plugin.Verdict) {
	if !routeMissing(o, v) {
		return
	}
	op := string(x.in.Op)
	n, err := x.p.d.Router.SeenUnserved(x.ctx, a.cand.UpstreamID, a.cand.EndpointID, op, unservedWindow)
	if err != nil || n < unservedSightings {
		return
	}
	if fresh, err := x.p.d.Router.MarkUnserved(x.ctx, a.cand.UpstreamID, a.cand.EndpointID, op, unservedTTL); err == nil && fresh {
		x.p.d.Logger.Info("endpoint has no route for this operation; trying it last", "upstream", a.cand.UpstreamID,
			"endpoint", a.cand.EndpointID, "protocol", a.ep.Protocol, "op", op, "status", o.Status, "for", unservedTTL)
	}
}

// genericNotFound are whole 404 messages of web frameworks and servers.
var genericNotFound = []string{"not found", "404 not found", "404 page not found", "404: not found", "404"}

// routeHints are phrases servers use for an unknown path or method.
var routeHints = []string{"invalid url", "unknown url", "unrecognized request url", "unexpected endpoint",
	"no route", "route not found", "endpoint not found", "path not found", "cannot post", "cannot get",
	"method not allowed", "not implemented", "unsupported endpoint"}

// routeMissing reports whether a failed attempt says the endpoint has no
// route for the request (404/405/501 about the path), as opposed to a
// missing model or resource on a route it serves: those name what is
// missing ("model", "deployment", a response id), so a 404 counts only when
// its body is empty, a server's plain page, a bare "Not Found" or a phrase
// about the URL. Credential failures never count.
func routeMissing(o *plugin.Outcome, v plugin.Verdict) bool {
	switch o.Status {
	case 404, 405, 501:
	default:
		return false
	}
	if v.Cause != plugin.CauseRequest {
		return false
	}
	body := strings.ToLower(strings.TrimSpace(string(o.Body)))
	msg := ""
	if o.Signal != nil {
		msg = strings.ToLower(strings.TrimSpace(o.Signal.Message))
	}
	if gjson.Valid(body) {
		for _, p := range []string{"error.message", "detail", "message", "error"} {
			if r := gjson.Get(body, p); r.Type == gjson.String && r.Str != "" {
				msg = strings.TrimSpace(r.Str)
				break
			}
		}
	}
	for _, w := range []string{"model", "deployment"} {
		if strings.Contains(body, w) || strings.Contains(msg, w) {
			return false
		}
	}
	if o.Status != 404 || body == "" || !gjson.Valid(body) {
		return true
	}
	if slices.Contains(genericNotFound, strings.Trim(msg, ". ")) {
		return true
	}
	for _, h := range routeHints {
		if strings.Contains(msg, h) {
			return true
		}
	}
	return false
}
