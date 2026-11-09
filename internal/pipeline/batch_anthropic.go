package pipeline

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/pkg/ir"
)

const messageBatchesPath = "/v1/messages/batches"

func (b *batchReq) serveAnthropic() {
	rt, m := b.route, b.r.Method
	switch {
	case rt.id == "" && m == http.MethodPost:
		b.createMessageBatch()
	case rt.id == "" && m == http.MethodGet:
		b.listMessageBatches()
	case rt.action == "" && (m == http.MethodGet || m == http.MethodDelete),
		rt.action == "cancel" && m == http.MethodPost:
		b.messageBatchCall()
	case rt.action == "results" && m == http.MethodGet:
		b.messageBatchResults()
	default:
		b.fail(&ir.GwError{Kind: ir.ErrInvalidRequest, Status: http.StatusMethodNotAllowed, Message: "method not allowed"})
	}
}

// createMessageBatch routes a message batch by the model its requests
// name (one model per batch) and renames it when the upstream model
// differs.
func (b *batchReq) createMessageBatch() {
	body, gw := b.readJSON()
	if gw != nil {
		b.fail(gw)
		return
	}
	if gw := duplicateKey(gjson.ParseBytes(body), "request body"); gw != nil {
		b.fail(gw)
		return
	}
	reqs := gjson.GetBytes(body, "requests")
	if !reqs.IsArray() || len(reqs.Array()) == 0 {
		b.fail(ir.Errorf(ir.ErrInvalidRequest, "requests must be a non-empty array"))
		return
	}
	model := ""
	var bad *ir.GwError
	var in, out int64
	reqs.ForEach(func(i, r gjson.Result) bool {
		m := r.Get("params.model").String()
		if model == "" {
			model = m
		}
		if m != model {
			bad = ir.Errorf(ir.ErrInvalidRequest, "requests.%d: every request of a batch must use model %q on this relay (got %q)", i.Int(), model, m)
			return false
		}
		return true
	})
	if bad != nil {
		b.fail(bad)
		return
	}
	t, gw := b.place(model, "anthropic")
	if gw != nil {
		b.fail(gw)
		return
	}
	content, ok := b.admit(model)
	if !ok {
		return
	}
	hooks, gw := b.lineHooks(content, t, "anthropic", "/v1/messages", model)
	if gw != nil {
		b.fail(gw)
		return
	}
	up := t.cand.UpstreamModel
	rewrite := up != "" && up != model
	parts := make([]string, 0, len(reqs.Array()))
	var err error
	reqs.ForEach(func(i, r gjson.Result) bool {
		where := "requests." + i.String()
		params, gw := hooks.check(where, []byte(r.Get("params").Raw))
		if gw != nil {
			bad = gw
			return false
		}
		li, lo := lineTokens(gjson.ParseBytes(params), "anthropic")
		in, out = in+li, out+lo
		part := []byte(r.Raw)
		if !bytes.Equal(params, []byte(r.Get("params").Raw)) {
			rewrite = true
			if part, err = sjson.SetRawBytes(part, "params", params); err != nil {
				bad = ir.Errorf(ir.ErrInvalidRequest, "%s: %v", where, err)
				return false
			}
		}
		if up != "" && up != model {
			if part, err = sjson.SetBytes(part, "params.model", up); err != nil {
				bad = ir.Errorf(ir.ErrInvalidRequest, "%s: %v", where, err)
				return false
			}
		}
		parts = append(parts, string(part))
		return true
	})
	if bad != nil {
		b.fail(bad)
		return
	}
	if rewrite {
		if body, err = sjson.SetRawBytes(body, "requests", []byte("["+strings.Join(parts, ",")+"]")); err != nil {
			b.fail(ir.Errorf(ir.ErrInvalidRequest, "requests: %v", err))
			return
		}
	}
	o := b.owner(&BatchObject{Kind: BatchKindMessageBatch, Protocol: "anthropic", Model: model}, t)
	hold, ok := b.hold(o, in, out)
	if !ok {
		return
	}
	resp, answer, err := b.sendJSON(t, http.MethodPost, messageBatchesPath, body)
	if err != nil {
		b.release(hold)
		b.upstreamFailed(err)
		return
	}
	if id := gjson.GetBytes(answer, "id").String(); ok2xx(resp.StatusCode) && id != "" {
		o.ID, o.Object, o.HoldID = id, answer, hold
		b.save(o)
		answer = b.present(o, answer)
	} else {
		b.release(hold)
	}
	b.reply(resp, answer)
}

// messageBatchCall retrieves, cancels or deletes one message batch.
func (b *batchReq) messageBatchCall() {
	o, ok := b.lookup(BatchKindMessageBatch, b.route.id, "message batch")
	if !ok {
		return
	}
	t, gw := b.targetOf(o)
	if gw != nil {
		b.fail(gw)
		return
	}
	path := messageBatchesPath + "/" + url.PathEscape(o.ID)
	method := b.r.Method
	if b.route.action == "cancel" {
		path += "/cancel"
	}
	if method == http.MethodDelete {
		b.collectFirst(o) // the results go with the batch
	}
	resp, out, err := b.sendJSON(t, method, path, nil)
	if err != nil {
		b.upstreamFailed(err)
		return
	}
	if ok2xx(resp.StatusCode) {
		if method == http.MethodDelete {
			if o.BilledAt.IsZero() {
				b.closeUnbilled(o)
			}
			_ = b.store.DeleteBatch(b.ctx(), o.Kind, o.ID)
		} else {
			b.refresh(o, out)
			b.ended(o, out)
			out = b.present(o, out)
		}
	}
	b.reply(resp, out)
}

// messageBatchResults streams the results and bills them on the first
// complete read.
func (b *batchReq) messageBatchResults() {
	o, ok := b.lookup(BatchKindMessageBatch, b.route.id, "message batch")
	if !ok {
		return
	}
	t, gw := b.targetOf(o)
	if gw != nil {
		b.fail(gw)
		return
	}
	b.detach = o.BilledAt.IsZero()
	resp, err := b.send(t, http.MethodGet, messageBatchesPath+"/"+url.PathEscape(o.ID)+"/results", nil, nil)
	if err != nil {
		b.upstreamFailed(err)
		return
	}
	b.streamResults(o, resp)
}

// listMessageBatches lists the caller's message batches, refreshing the
// ones not ended.
func (b *batchReq) listMessageBatches() {
	q := b.r.URL.Query()
	objs, more, err := b.store.ListBatches(b.ctx(), BatchQuery{Kind: BatchKindMessageBatch, UserID: b.caller.UserID, KeyID: b.caller.KeyID,
		After: q.Get("after_id"), Before: q.Get("before_id"), Limit: listLimit(b.r, 20, 1000)})
	if err != nil {
		b.fail(ir.Errorf(ir.ErrInternal, "batch store: %v", err))
		return
	}
	data := b.refreshAll(objs, func(o *BatchObject) string { return messageBatchesPath + "/" + url.PathEscape(o.ID) },
		map[string]bool{"ended": true}, "processing_status")
	b.replyJSON(listPage(objs, data, more, ""))
}

// present rewrites what a client must not follow to the upstream: a
// message batch's results_url points at the relay instead.
func (b *batchReq) present(o *BatchObject, obj []byte) []byte {
	if o.Kind != BatchKindMessageBatch || gjson.GetBytes(obj, "results_url").Type != gjson.String {
		return obj
	}
	out, err := sjson.SetBytes(obj, "results_url", b.publicBase()+messageBatchesPath+"/"+url.PathEscape(o.ID)+"/results")
	if err != nil {
		return obj
	}
	return out
}

// publicBase is the relay's configured public URL, else its base URL as
// the client reached it.
func (b *batchReq) publicBase() string {
	if u := b.p.d.publicURL(b.r.Context()); u != "" {
		return strings.TrimRight(u, "/")
	}
	scheme := "http"
	if b.r.TLS != nil || strings.EqualFold(b.r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := b.r.Host
	if h := b.r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}
