package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// resultMeter sums the usage of a JSONL results stream as it is copied:
// OpenAI output lines ({response: {status_code, body}}) or Anthropic
// result lines ({result: {type: "succeeded", message}}). Only successful
// results count; a line longer than max is skipped.
type resultMeter struct {
	codec     protocol.Codec // protocol of the requests inside
	anthropic bool
	max       int
	buf       []byte
	skip      bool
	usage     ir.Usage
	results   int
}

func (m *resultMeter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			m.add(p)
			break
		}
		m.add(p[:i])
		m.line()
		p = p[i+1:]
	}
	return n, nil
}

func (m *resultMeter) add(p []byte) {
	if m.skip {
		return
	}
	if len(m.buf)+len(p) > m.max {
		m.skip, m.buf = true, m.buf[:0]
		return
	}
	m.buf = append(m.buf, p...)
}

func (m *resultMeter) line() {
	l := bytes.TrimSpace(m.buf)
	skip := m.skip
	m.buf, m.skip = m.buf[:0], false
	if skip || len(l) == 0 || m.codec == nil {
		return
	}
	var body string
	if m.anthropic {
		r := gjson.GetBytes(l, "result")
		if r.Get("type").String() != "succeeded" {
			return
		}
		body = r.Get("message").Raw
	} else {
		r := gjson.GetBytes(l, "response")
		if !ok2xx(int(r.Get("status_code").Int())) {
			return
		}
		body = r.Get("body").Raw
	}
	resp, err := m.codec.DecodeResponse([]byte(body))
	if err != nil {
		return
	}
	m.usage.Add(resp.Usage)
	m.results++
}

// close meters a last line without a newline.
func (m *resultMeter) close() {
	if len(m.buf) > 0 || m.skip {
		m.line()
	}
}

// newResultMeter meters the results of o, a batch or message batch.
func (b *batchReq) newResultMeter(o *BatchObject) *resultMeter {
	codec, _ := protocol.Get(o.Protocol)
	return &resultMeter{codec: codec, anthropic: o.Kind == BatchKindMessageBatch, max: int(b.p.d.MaxResponseBody)}
}

// streamResults copies a results download to the client. bill is the
// batch the results belong to, nil when they are not billable; a complete
// 200 answer is metered and billed to it unless it was billed already. A
// client that leaves early does not stop the metering: the rest is read
// from the upstream and discarded.
func (b *batchReq) streamResults(bill *BatchObject, resp *http.Response) {
	defer resp.Body.Close()
	h := b.w.Header()
	for k, vs := range egress.InboundResponseHeaders(resp.Header) {
		h[k] = vs
	}
	if resp.ContentLength >= 0 && !resp.Uncompressed {
		h.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	b.w.WriteHeader(resp.StatusCode)
	b.rec.Status = resp.StatusCode
	b.rec.Stream = true
	var meter *resultMeter
	if resp.StatusCode == http.StatusOK && bill != nil && bill.BilledAt.IsZero() {
		meter = b.newResultMeter(bill)
	}
	var client io.Writer = b.w
	rc := http.NewResponseController(b.w)
	buf := make([]byte, 32<<10)
	var copied int64
	clientGone := false
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if meter != nil {
				_, _ = meter.Write(buf[:n])
			}
			if !clientGone {
				if _, werr := client.Write(buf[:n]); werr != nil {
					clientGone = true
					b.rec.ErrorKind, b.rec.Error = ErrClientClosed, "client went away"
					if meter == nil {
						return
					}
				} else {
					copied += int64(n)
					_ = rc.Flush()
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if b.att != nil {
				b.att.Error = errText(err)
			}
			gw := transportError(err)
			b.rec.ErrorKind, b.rec.Error = gw.Kind, gw.Message
			return
		}
	}
	if b.att != nil {
		b.att.BytesOut = copied
	}
	if meter != nil {
		meter.close()
		b.bill(bill, meter)
	}
}

// creator is the caller that made o: its usage is billed to them.
func (b *batchReq) creator(o *BatchObject) *plugin.Caller {
	return &plugin.Caller{UserID: o.UserID, KeyID: o.KeyID, GroupID: o.GroupID}
}

// holdID is the id o's money is held and charged under.
func holdID(o *BatchObject) string {
	if o.HoldID != "" {
		return o.HoldID
	}
	return "batch:" + o.Kind + ":" + o.ID
}

// bill charges o's metered usage to its creator, then marks o billed:
// cost at the upstream model's rate, charge at the list price (else that
// cost rate) times the creator's group multiplier, both less their rule's
// batch discount. The charge is idempotent on o's hold id, so a charge
// that fails leaves o unbilled for the next read or SettleBatches, and a
// repeated one is not charged twice.
func (b *batchReq) bill(o *BatchObject, m *resultMeter) {
	ctx := context.WithoutCancel(b.ctx())
	u := m.usage
	who := b.creator(o)
	att := plugin.AttemptRecord{N: 2, UpstreamID: o.UpstreamID, EndpointID: o.EndpointID, CredentialID: o.CredentialID,
		Protocol: o.Protocol, Model: o.ServedModel, UpstreamModel: o.UpstreamModel, Tier: plugin.TierPassthrough, Status: http.StatusOK, Sent: true,
		Usage: u, Diagnostic: &ir.Diagnostic{Fidelity: ir.Lossless,
			Notes: []string{"batch " + o.ID + ": " + strconv.Itoa(m.results) + " results billed"}}}
	if pr, ok := b.snap.costRate(o.UpstreamID, o.UpstreamModel); ok {
		att.CostMicro = pr.scaled(1 - pr.BatchDiscount).Cost(u)
	}
	var charge int64
	if pr, src, ok := b.rate(o); ok {
		var r Price
		r, att.Charge = chargeAt(pr, b.snap.multiplier(who.GroupID), src, pr.BatchDiscount)
		charge = r.charge(u)
	}
	if err := b.settle(o, who, u, charge); err != nil {
		b.p.d.Logger.Error("batch charge failed; left unbilled for a retry", "kind", o.Kind, "id", o.ID, "err", err)
		b.rec.Error = "batch charge failed: " + err.Error()
		return
	}
	first, err := b.store.ClaimBatchBilling(ctx, o.Kind, o.ID, b.p.d.Now())
	if err != nil {
		b.p.d.Logger.Error("batch billing not marked; the next read charges again, idempotently", "kind", o.Kind, "id", o.ID, "err", err)
		return
	}
	if !first {
		return
	}
	o.BilledAt = b.p.d.Now()
	b.billed = true
	b.rec.Caller = *who
	b.rec.Model = o.Model
	b.rec.Usage = u
	b.rec.ChargeMicro = charge
	b.rec.Attempts = append(b.rec.Attempts, att)
}

// rate is the unscaled price o's usage is charged at (Snapshot.chargeRate).
func (b *batchReq) rate(o *BatchObject) (Price, string, bool) {
	return b.snap.chargeRate(o.ServedModel, o.Model, o.UpstreamID, o.UpstreamModel)
}

// estimate prices a batch's token estimate as it would be charged; for a
// preset, at the highest list price of it and its members, as a request
// to a preset is held.
func (b *batchReq) estimate(o *BatchObject, in, out int64) int64 {
	pr, _, ok := b.rate(o)
	if !ok {
		return 0
	}
	mult := b.snap.multiplier(b.caller.GroupID)
	u := ir.Usage{Input: in, Output: out}
	est := pr.scaled(mult * (1 - pr.BatchDiscount)).charge(u)
	if t := b.snap.Table; t != nil {
		for _, m := range append([]string{o.Model}, t.Presets[o.Model].Models...) {
			if lp, ok := b.snap.listPrice(m); ok {
				est = max(est, lp.scaled(mult*(1-lp.BatchDiscount)).charge(u))
			}
		}
	}
	return est
}

// deferred is the meter when it holds money for batches.
func (b *batchReq) deferred() (plugin.Meter, plugin.DeferredMeter) {
	m := b.p.chain().Meter
	if m == nil || !supports(m, "meter.charge") {
		return nil, nil
	}
	dm, ok := m.(plugin.DeferredMeter)
	if !ok {
		return nil, nil
	}
	return m, dm
}

// hold reserves a new batch's estimated charge under a fresh hold id,
// which it returns, until the batch is billed or ends; the caller is
// answered when the balance does not cover it. Meters that cannot hold
// for later only admit the caller (gate).
func (b *batchReq) hold(o *BatchObject, in, out int64) (string, bool) {
	m, dm := b.deferred()
	if dm == nil {
		return "", b.gate()
	}
	id := "batch:" + b.rec.ID
	call := b.callAs(b.caller, id)
	if err := dm.Hold(call.WithPlugin(m.Info().ID), id, b.estimate(o, in, out)); err != nil {
		b.fail(meterRefusal(err))
		return "", false
	}
	return id, true
}

// release drops a hold made for a batch that was not created.
func (b *batchReq) release(id string) {
	m, dm := b.deferred()
	if dm == nil || id == "" {
		return
	}
	call := b.callAs(b.caller, id)
	call.Context = context.WithoutCancel(b.ctx())
	if err := dm.Charge(call.WithPlugin(m.Info().ID), id, ir.Usage{}, 0); err != nil {
		b.p.d.Logger.Warn("batch hold not released", "hold", id, "err", err)
	}
}

// settle hands o's charge to the meter as its creator's: charged under
// o's hold id (the hold, if any is left, released) by a meter that holds
// for batches, else a zero hold captured at the charge.
func (b *batchReq) settle(o *BatchObject, who *plugin.Caller, u ir.Usage, charge int64) error {
	m := b.p.chain().Meter
	if m == nil {
		return nil
	}
	id := holdID(o)
	call := b.callAs(who, id)
	call.Context = context.WithoutCancel(b.ctx())
	call.Model = o.Model
	if _, dm := b.deferred(); dm != nil {
		return dm.Charge(call.WithPlugin(m.Info().ID), id, u, charge)
	}
	res := &plugin.Reservation{ID: id}
	if supports(m, "meter.reserve") {
		r, err := m.Reserve(call.WithPlugin(m.Info().ID), 0)
		if err != nil {
			b.p.d.Logger.Error("batch charge not reserved; settling without a hold", "request", b.rec.ID, "err", err)
		} else if r != nil {
			res = r
		}
	}
	if supports(m, "meter.settle") {
		return m.Settle(call.WithPlugin(m.Info().ID), res, u, charge)
	}
	return nil
}

// batchEnd reads an upstream batch object: whether it ended, when, and
// whether it has results to bill.
func batchEnd(o *BatchObject, obj []byte) (ended bool, at time.Time, results bool) {
	j := gjson.ParseBytes(obj)
	if o.Kind == BatchKindMessageBatch {
		if j.Get("processing_status").String() != "ended" {
			return false, time.Time{}, false
		}
		at, _ = time.Parse(time.RFC3339, j.Get("ended_at").String())
		ok := j.Get("request_counts.succeeded")
		return true, at, !ok.Exists() || ok.Int() > 0
	}
	switch j.Get("status").String() {
	case "completed", "failed", "expired", "cancelled":
	default:
		return false, time.Time{}, false
	}
	var last int64
	for _, f := range []string{"completed_at", "failed_at", "expired_at", "cancelled_at"} {
		last = max(last, j.Get(f).Int())
	}
	if last > 0 {
		at = time.Unix(last, 0)
	}
	return true, at, j.Get("output_file_id").String() != ""
}

// ended handles a batch seen in its final state: one with nothing to bill
// has its hold released and is marked billed; one with results is left
// to the client's read, or collected by SettleBatches.
func (b *batchReq) ended(o *BatchObject, obj []byte) {
	if !o.BilledAt.IsZero() {
		return
	}
	if ended, _, results := batchEnd(o, obj); ended && !results {
		b.closeUnbilled(o)
	}
}

// closeUnbilled releases o's hold and marks it billed with nothing
// charged.
func (b *batchReq) closeUnbilled(o *BatchObject) {
	if m, dm := b.deferred(); dm != nil {
		id := holdID(o)
		call := b.callAs(b.creator(o), id)
		call.Context = context.WithoutCancel(b.ctx())
		if err := dm.Charge(call.WithPlugin(m.Info().ID), id, ir.Usage{}, 0); err != nil {
			b.p.d.Logger.Warn("batch hold not released", "kind", o.Kind, "id", o.ID, "err", err)
			return
		}
	}
	if _, err := b.store.ClaimBatchBilling(context.WithoutCancel(b.ctx()), o.Kind, o.ID, b.p.d.Now()); err != nil {
		b.p.d.Logger.Warn("batch not marked billed", "kind", o.Kind, "id", o.ID, "err", err)
		return
	}
	o.BilledAt = b.p.d.Now()
}

// collect downloads o's results itself and bills them. o.Object must be
// current (an OpenAI batch names its output file there).
func (b *batchReq) collect(o *BatchObject) bool {
	t, gw := b.targetOf(o)
	if gw != nil {
		return false
	}
	path := messageBatchesPath + "/" + url.PathEscape(o.ID) + "/results"
	if o.Kind == BatchKindBatch {
		id := gjson.GetBytes(o.Object, "output_file_id").String()
		if id == "" {
			return false
		}
		path = "/files/" + url.PathEscape(id) + "/content"
	}
	b.detach = !b.background
	resp, err := b.send(t, http.MethodGet, path, nil, nil)
	if err != nil {
		b.p.d.Logger.Warn("batch results not fetched", "kind", o.Kind, "id", o.ID, "err", err)
		return false
	}
	defer resp.Body.Close()
	b.rec.Status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		return false
	}
	m := b.newResultMeter(o)
	n, err := io.Copy(m, resp.Body)
	if err != nil {
		b.p.d.Logger.Warn("batch results not read", "kind", o.Kind, "id", o.ID, "err", err)
		return false
	}
	if b.att != nil {
		b.att.BytesOut = n
	}
	m.close()
	b.bill(o, m)
	return !o.BilledAt.IsZero()
}

// collectFirst bills o's results before a call that makes them
// unreadable (deleting a message batch or an output file) when nobody
// read them yet.
func (b *batchReq) collectFirst(o *BatchObject) {
	if o == nil || !o.BilledAt.IsZero() {
		return
	}
	bg := b.p.backgroundReq(context.WithoutCancel(b.ctx()), b.creator(o))
	defer bg.finish()
	bg.collect(o)
}

// Timing of SettleBatches.
const (
	batchCheckEvery = 5 * time.Minute // between two status checks of one batch
	// batchCollectAfter leaves the client this long to read the results
	// of an ended batch through the relay before the relay fetches them.
	batchCollectAfter = 10 * time.Minute
	// batchGiveUpAfter: a batch still unbilled this long after creation
	// (unreachable, results expired) has its hold released.
	batchGiveUpAfter = 30 * 24 * time.Hour
	batchSweepSize   = 20
)

// SettleBatches bills batches whose results nobody read through the
// relay, and releases the holds of batches that ended with nothing to
// bill. The app calls it every minute; it returns at once while a sweep
// is still running, and runs on one node at a time when a lock is
// configured.
func (p *Pipeline) SettleBatches(ctx context.Context) {
	if p.d.Batches == nil || !p.sweeping.CompareAndSwap(false, true) {
		return
	}
	p.inflight.add()
	go func() {
		defer p.inflight.done()
		defer p.sweeping.Store(false)
		if p.d.Lock != nil {
			unlock, ok, err := p.d.Lock(ctx, "batch:settle", batchCheckEvery)
			if err != nil || !ok {
				return
			}
			defer unlock()
		}
		now := p.d.Now()
		objs, err := p.d.Batches.UnbilledBatches(ctx, now.Add(-batchCheckEvery), now, batchSweepSize)
		if err != nil {
			p.d.Logger.Warn("unbilled batches not listed", "err", err)
			return
		}
		for i := range objs {
			if ctx.Err() != nil {
				return
			}
			p.settleBatch(ctx, &objs[i])
		}
	}()
}

// backgroundReq is a batch request the relay makes itself, for caller;
// it stops when ctx ends.
func (p *Pipeline) backgroundReq(ctx context.Context, caller *plugin.Caller) *batchReq {
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	b := p.newBatchReq(discardWriter{}, r, caller)
	b.background = true
	return b
}

// settleBatch checks one unbilled batch with its upstream.
func (p *Pipeline) settleBatch(ctx context.Context, o *BatchObject) {
	b := p.backgroundReq(ctx, &plugin.Caller{UserID: o.UserID, KeyID: o.KeyID, GroupID: o.GroupID})
	defer b.finish()
	b.quiet = true
	giveUp := p.d.Now().Sub(o.CreatedAt) > batchGiveUpAfter
	t, gw := b.targetOf(o)
	if gw != nil {
		if giveUp {
			p.d.Logger.Warn("batch abandoned unbilled: its credential is gone", "kind", o.Kind, "id", o.ID)
			b.closeUnbilled(o)
		}
		return
	}
	path := "/batches/" + url.PathEscape(o.ID)
	if o.Kind == BatchKindMessageBatch {
		path = messageBatchesPath + "/" + url.PathEscape(o.ID)
	}
	resp, out, err := b.sendJSON(t, http.MethodGet, path, nil)
	switch {
	case err == nil && ok2xx(resp.StatusCode) && gjson.ValidBytes(out):
		b.refresh(o, out)
		o.Object = out
		if o.Kind == BatchKindBatch {
			b.noteFiles(o, out)
		}
	case giveUp:
		p.d.Logger.Warn("batch abandoned unbilled: the upstream no longer answers for it", "kind", o.Kind, "id", o.ID)
		b.closeUnbilled(o)
		return
	default:
		return
	}
	ended, at, results := batchEnd(o, o.Object)
	switch {
	case !ended:
		if giveUp {
			b.closeUnbilled(o)
		}
	case !results:
		b.closeUnbilled(o)
	case at.IsZero() || p.d.Now().Sub(at) >= batchCollectAfter:
		b.quiet = false
		if !b.collect(o) && giveUp {
			b.closeUnbilled(o)
		}
	}
}

// discardWriter is the response writer of a request the relay makes
// itself.
type discardWriter struct{}

func (discardWriter) Header() http.Header         { return http.Header{} }
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriter) WriteHeader(int)             {}
