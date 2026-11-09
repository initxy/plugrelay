package pipeline

import (
	"net/http"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// countTokens forwards a token count to a same-protocol upstream when one
// is routable, and otherwise answers with the codec's local estimate.
func (x *exchange) countTokens() {
	if plan, err := x.plan(map[string]bool{}); err == nil {
		for _, cand := range plan.Candidates {
			if !cand.SameProtocol || x.sealed {
				continue
			}
			if x.forwardCount(cand) {
				return
			}
			break
		}
	}
	if x.req == nil {
		x.fail(asGwError(x.decodeErr, ir.ErrInvalidRequest))
		return
	}
	body, err := x.codec.CountTokens(x.req)
	if err != nil {
		x.fail(ir.Errorf(ir.ErrInternal, "could not count tokens"))
		return
	}
	h := http.Header{"Content-Type": {"application/json"}}
	writePlain(x.w, http.StatusOK, h, body)
	x.status = http.StatusOK
}

// forwardCount sends the count request upstream; it reports whether the
// upstream's answer was relayed.
func (x *exchange) forwardCount(cand plugin.Candidate) bool {
	a, gw := x.prepare(cand, 1)
	if gw != nil {
		return false
	}
	resp, gw := x.send(a)
	if gw != nil || resp == nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false
	}
	data, err := readBounded(resp.Body, x.p.d.MaxResponseBody)
	if err != nil {
		return false
	}
	writePlain(x.w, resp.StatusCode, egress.InboundResponseHeaders(resp.Header), data)
	x.status = resp.StatusCode
	return true
}
