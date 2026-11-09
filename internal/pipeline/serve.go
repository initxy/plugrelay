package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/internal/convert"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// ServeProtocol serves one client request on a protocol route. The HTTP
// server has already matched route, authenticated caller and read body
// (bounded and decompressed). Model-list and hello routes are dispatched to
// ServeModels and ServeHello.
func (p *Pipeline) ServeProtocol(w http.ResponseWriter, r *http.Request, codec protocol.Codec, route protocol.Route, caller *plugin.Caller, body []byte) {
	switch route.Op {
	case protocol.OpModels:
		p.ServeModels(w, r, codec, caller)
		return
	case protocol.OpHello:
		p.ServeHello(w, r)
		return
	}
	p.inflight.add()
	defer p.inflight.done()
	if caller == nil {
		caller = &plugin.Caller{}
	}
	x := &exchange{
		p:         p,
		snap:      p.snapshot(),
		chain:     p.chain(),
		w:         w,
		r:         r,
		ctx:       r.Context(),
		codec:     codec,
		route:     route,
		caller:    caller,
		body:      body,
		id:        p.requestID(r.Context()),
		started:   p.d.Now(),
		edits:     map[string]bool{},
		downgrade: map[string]map[string]json.RawMessage{},
	}
	defer x.finish()
	x.serve()
}

type requestIDKey struct{}

// WithRequestID makes id the log id of the request served with ctx; the
// relay's own requests (playground, key tests) use it to find their entry.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func (p *Pipeline) requestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok && id != "" {
		return id
	}
	return p.d.NewID()
}

// exchange is the state of one client request.
type exchange struct {
	p     *Pipeline
	snap  *Snapshot
	chain *Chain
	w     http.ResponseWriter
	r     *http.Request
	ctx   context.Context

	codec  protocol.Codec // client protocol
	route  protocol.Route
	caller *plugin.Caller
	body   []byte
	id     string
	// upload is the multipart body of an Upload route; body then holds
	// its form fields as JSON.
	upload *upload

	started time.Time
	in      protocol.Inbound
	call    *plugin.Call

	// req is the client request decoded once; never mutated. decodeErr is
	// set when the codec could not decode it: only same-protocol
	// passthrough can serve such a request.
	req       *ir.Request
	needs     ir.Needs
	decodeErr error
	// attemptReq is the IR of the convert-tier attempt whose hooks are
	// running; Call.Request returns it instead of req.
	attemptReq *ir.Request
	issuers    []string // convert.HistoryIssuers of req
	sealed     bool     // history carries relay-sealed state
	session    string
	// lastIssuer is the issuer of the endpoint that served the session's
	// previous turn ("" when unknown).
	lastIssuer string
	// sentSession is the session id sent upstream on the previous turn,
	// reused so the upstream sees a stable id.
	sentSession string
	planned     bool
	// served maps a candidate key to the model of a requested preset it
	// serves.
	served map[string]string
	// unserved caches, per upstream|endpoint, whether it is marked as
	// having no route for this operation.
	unserved map[string]bool
	// refreshedCreds are upstream|credential pairs refreshed by this
	// request, which never refreshes one twice.
	refreshedCreds map[string]bool

	base        http.Header     // quota headers, see baseHeader
	entered     []plugin.Plugin // plugins whose request hook ran, in order
	reservation *plugin.Reservation

	out      *clientOut // stream output; nil for non-stream requests
	held     heldFrames // stream frames event hooks hold back
	attempts []*attempt
	// edits marks candidates already resent with downgrade edits;
	// downgrade holds those edits.
	edits     map[string]bool
	downgrade map[string]map[string]json.RawMessage

	// Outcome of the request.
	status    int
	err       *ir.GwError
	delivered *attempt // attempt whose response reached the client
	complete  bool     // delivered response ended normally
	// disconnected is set when the client left mid-response.
	disconnected bool
	// committed is set once a copied body's status reached the client.
	committed bool
	// replyUsage is the usage of a plugin's short-circuit response.
	replyUsage *ir.Usage
}

func (x *exchange) serve() {
	if x.route.Upload {
		u, gw := readUpload(x.r)
		if gw != nil {
			x.fail(gw)
			return
		}
		x.upload, x.body = u, u.json
	}
	if b := bytes.TrimSpace(x.body); len(b) > 0 && (b[0] == '{' || b[0] == '[') {
		if gw := duplicateKey(gjson.ParseBytes(b), "request body"); gw != nil {
			x.fail(gw)
			return
		}
	}
	in, err := x.codec.Sniff(x.r, x.route, x.body)
	if err != nil {
		x.fail(asGwError(err, ir.ErrInvalidRequest))
		return
	}
	x.in = in
	if in.Model == "" {
		x.fail(ir.Errorf(ir.ErrInvalidRequest, "model is required"))
		return
	}
	if v := x.snap.Visible; v != nil && !v(x.caller, in.Model) {
		x.fail(ir.Errorf(ir.ErrNotFound, "model %q not found", in.Model))
		return
	}
	x.decode()
	x.session = resolveSession(x.caller, in)
	x.call = x.newCall()

	if x.runRequestHooks() {
		return
	}
	if in.Op == protocol.OpCountTokens {
		x.countTokens()
		return
	}
	if !x.reserve() {
		return
	}
	if in.Stream {
		x.out = newClientOut(x)
		go x.out.keepalive(x.p.d.KeepaliveDelay, x.p.d.Keepalive)
	}
	x.run()
}

// decode parses the request once for its needs and history issuers.
func (x *exchange) decode() {
	req, needs, err := x.codec.DecodeRequest(x.r, x.in, x.body)
	if err != nil {
		x.decodeErr = err
		return
	}
	x.req, x.needs = req, needs
	x.issuers = convert.HistoryIssuers(req, x.p.d.Sealer)
	x.sealed = hasSealed(req)
}

func hasSealed(req *ir.Request) bool {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Opaque != nil && b.Opaque.Sealed {
				return true
			}
		}
	}
	return false
}

// run is the attempt loop: plan, try, and on a retryable failure plan again
// without the candidates that failed.
func (x *exchange) run() {
	exclude := map[string]bool{}
	var last *failure
	tries := 0
	for tries < x.p.d.MaxAttempts && x.ctx.Err() == nil {
		plan, err := x.plan(exclude)
		if err != nil {
			if last != nil {
				x.failWith(last)
			} else {
				x.fail(planError(err))
			}
			return
		}
		tried := false
		for _, cand := range plan.Candidates {
			key := router.Key(cand)
			res, ok := x.tryCandidate(cand, &tries)
			exclude[key] = true
			if !ok {
				continue // at its concurrency cap
			}
			tried = true
			switch res.next {
			case nextDone:
				return
			case nextStop:
				x.failWith(res.fail)
				return
			}
			last = res.fail
			break
		}
		if !tried && len(plan.Candidates) == 0 {
			break
		}
	}
	if last != nil {
		x.failWith(last)
		return
	}
	x.fail(ir.Errorf(ir.ErrOverloaded, "no upstream available for model %q", x.in.Model))
}

// tryCandidate runs attempts against one candidate: one, plus one resend
// when the verdict asks for a downgrade. ok=false means no slot was free.
func (x *exchange) tryCandidate(cand plugin.Candidate, tries *int) (result, bool) {
	key := router.Key(cand)
	var res result
	for {
		release, ok, err := x.p.d.Router.Acquire(x.ctx, cand)
		if err != nil || !ok {
			if res.fail != nil {
				return res, true
			}
			return result{}, false
		}
		*tries++
		res = x.attempt(cand, len(x.attempts)+1)
		release()
		if res.next == nextRefreshed {
			if *tries >= x.p.d.MaxAttempts {
				res.next = nextRetry
				return res, true
			}
			continue
		}
		if res.next != nextDowngrade {
			return res, true
		}
		if x.edits[key] || *tries >= x.p.d.MaxAttempts {
			res.next = nextRetry
			return res, true
		}
		x.edits[key] = true
	}
}

// failure is a failed attempt as the client would see it.
type failure struct {
	err *ir.GwError
	// verbatim holds the upstream's own error response when the upstream
	// speaks the client's protocol; it is passed through unchanged.
	verbatim *upstreamError
}

type upstreamError struct {
	status int
	header http.Header
	body   []byte
}

// next says what the attempt loop does after an attempt.
type next int

const (
	nextDone      next = iota // response delivered (fully or not); stop
	nextRetry                 // try another candidate
	nextDowngrade             // resend to the same candidate with edits
	nextRefreshed             // resend to the same candidate: its credential was refreshed
	nextStop                  // final failure, nothing delivered
)

type result struct {
	next next
	fail *failure
}

func asGwError(err error, kind ir.ErrorKind) *ir.GwError {
	var gw *ir.GwError
	if errors.As(err, &gw) {
		return gw
	}
	return &ir.GwError{Kind: kind, Message: err.Error()}
}
