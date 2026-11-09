package admin

import (
	"context"
	"encoding/json"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/clientid"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

type logSummary struct {
	ID            string       `json:"id"`
	Time          time.Time    `json:"time"`
	Protocol      string       `json:"protocol,omitempty"`
	Model         string       `json:"model"`
	ServedModel   string       `json:"served_model,omitempty"`
	UpstreamModel string       `json:"upstream_model,omitempty"`
	Key           *ref         `json:"key,omitempty"`
	User          *ref         `json:"user,omitempty"`
	Upstream      *ref         `json:"upstream,omitempty"`
	Status        int          `json:"status"`
	ErrorKind     ir.ErrorKind `json:"error_kind,omitempty"`
	Cause         plugin.Cause `json:"cause,omitempty"`
	Tier          plugin.Tier  `json:"tier,omitempty"`
	Stream        bool         `json:"stream"`
	Attempts      int          `json:"attempts"`
	CacheHitRate  *float64     `json:"cache_hit_rate"`
	TTFBMs        *int64       `json:"ttfb_ms"`
	DurationMs    int64        `json:"duration_ms"`
	Charge        money        `json:"charge"`
	Client        clientRef    `json:"client"`
	ClientIP      string       `json:"client_ip,omitempty"`
	Resent        bool         `json:"resent,omitempty"`
	attempts      []plugin.AttemptRecord
	retriedInside bool
}

type attemptView struct {
	N             int            `json:"n"`
	Upstream      ref            `json:"upstream"`
	EndpointID    string         `json:"endpoint_id,omitempty"`
	Credential    *ref           `json:"credential,omitempty"`
	Protocol      string         `json:"protocol,omitempty"`
	UpstreamModel string         `json:"upstream_model,omitempty"`
	Tier          plugin.Tier    `json:"tier"`
	Status        int            `json:"status"`
	Cause         plugin.Cause   `json:"cause,omitempty"`
	Sent          bool           `json:"sent"`
	StartMs       int64          `json:"start_ms"`
	TTFBMs        *int64         `json:"ttfb_ms"`
	DurationMs    int64          `json:"duration_ms"`
	Usage         ir.Usage       `json:"usage"`
	Cost          money          `json:"cost"`
	Charged       bool           `json:"charged"`
	Diagnostic    *ir.Diagnostic `json:"diagnostic,omitempty"`
	Error         string         `json:"error,omitempty"`
	BytesIn       int64          `json:"bytes_in,omitempty"`
	BytesOut      int64          `json:"bytes_out,omitempty"`
}

type billingLine struct {
	Bucket    string `json:"bucket"`
	Tokens    int64  `json:"tokens"`
	UnitPrice money  `json:"unit_price"`
	Amount    money  `json:"amount"`
}

type billing struct {
	Lines   []billingLine `json:"lines"`
	Charged money         `json:"charged"`
	Cost    *money        `json:"cost,omitempty"`
	// Multiplier is the group multiplier already in the unit prices;
	// admins only, callers see prices in money.
	Multiplier *float64 `json:"multiplier,omitempty"`
	// Note is a code the console words: no_price or estimated.
	Note string `json:"note,omitempty"`
}

type logDetail struct {
	logSummary
	SessionID      string        `json:"session_id,omitempty"`
	Session        *sessionTotal `json:"session,omitempty"`
	ClientUA       string        `json:"client_ua,omitempty"`
	Error          string        `json:"error,omitempty"`
	AttemptsDetail []attemptView `json:"attempts_detail"`
	Usage          ir.Usage      `json:"usage"`
	Billing        billing       `json:"billing"`
	HasBody        bool          `json:"has_body"`
	Body           *recordBody   `json:"body,omitempty"`
}

type recordBody struct {
	Request  json.RawMessage `json:"request,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

// clientRef names the client program of a request; Name is empty for
// clientid.Other (the console words it).
type clientRef struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

func clientOf(id, version string) clientRef {
	return clientRef{ID: id, Name: clientid.Name(id), Version: version}
}

type sessionTotal struct {
	Requests int64     `json:"requests"`
	Turn     int64     `json:"turn"`
	Charge   money     `json:"charge"`
	First    time.Time `json:"first"`
	Last     time.Time `json:"last"`
}

// logRefs resolves names for log rows.
type logRefs struct {
	names     *names
	keys      map[string]string
	upstreams map[string]string
	hide      bool
}

func (h *Handler) logRefs(c *call) (*logRefs, error) {
	n, err := h.names(c.ctx)
	if err != nil {
		return nil, err
	}
	lr := &logRefs{names: n, keys: map[string]string{}, upstreams: map[string]string{}}
	ups, err := h.d.Store.ListUpstreams(c.ctx)
	if err != nil {
		return nil, err
	}
	for i := range ups {
		lr.upstreams[ups[i].ID] = UpstreamName(&ups[i])
	}
	if !c.who.isAdmin() {
		s, err := h.site(c.ctx)
		if err != nil {
			return nil, err
		}
		lr.hide = !s.showUpstreamNames()
	}
	return lr, nil
}

func (lr *logRefs) upstream(id string) ref {
	if lr.hide {
		return ref{ID: "", Name: "upstream"}
	}
	if n, ok := lr.upstreams[id]; ok {
		return ref{ID: id, Name: n}
	}
	return ref{ID: id, Name: id}
}

func ms(d time.Duration) int64 { return d.Milliseconds() }

func (h *Handler) summarize(r *plugin.Record, lr *logRefs, cur string) logSummary {
	s := logSummary{ID: r.ID, Time: r.Time, Protocol: r.Protocol, Model: r.Model, ServedModel: r.Model,
		Status: r.Status, ErrorKind: r.ErrorKind, Stream: r.Stream, Attempts: len(r.Attempts),
		DurationMs: ms(r.Duration), Charge: money{Micro: r.ChargeMicro, Currency: cur},
		Client: clientOf(r.Client, r.ClientVersion), ClientIP: r.ClientIP, Resent: r.Retried, attempts: r.Attempts,
		retriedInside: len(r.Attempts) > 1}
	if r.Caller.KeyID != "" {
		s.Key = &ref{ID: r.Caller.KeyID, Name: orDefault(r.Caller.KeyName, r.Caller.KeyID)}
	}
	s.User = lr.names.user(r.Caller.UserID)
	if n := len(r.Attempts); n > 0 {
		last := r.Attempts[n-1]
		up := lr.upstream(last.UpstreamID)
		s.Upstream, s.UpstreamModel, s.Tier = &up, last.UpstreamModel, last.Tier
		if last.Model != "" {
			s.ServedModel = last.Model
		}
		for _, a := range r.Attempts {
			if a.Cause != "" {
				s.Cause = a.Cause
			}
		}
	}
	if in := r.Usage.TotalInput(); in > 0 {
		rate := float64(r.Usage.CacheRead) / float64(in)
		s.CacheHitRate = &rate
	}
	if r.TTFB > 0 {
		t := ms(r.TTFB)
		s.TTFBMs = &t
	}
	return s
}

// logFilter is the /logs filter set: what the store applies (q) and what
// is matched on each record afterwards.
type logFilter struct {
	q                   store.RecordQuery
	status, tier, cause string
	session             string
	retried             bool
}

// logFilter reads the /logs filters; callers who are not admins only see
// their own requests.
func (c *call) logFilter() (logFilter, error) {
	f := logFilter{q: store.RecordQuery{KeyID: c.query("key_id"), UserID: c.query("user_id"), Model: c.query("model"),
		UpstreamID: c.query("upstream_id"), RequestID: c.query("q"), Client: c.query("client")},
		status: c.query("status"), tier: c.query("tier"), cause: c.query("cause"), session: c.query("session_id"),
		retried: c.query("retried") == "true"}
	var err error
	if f.q.From, err = c.timeQuery("from"); err != nil {
		return f, err
	}
	if f.q.To, err = c.timeQuery("to"); err != nil {
		return f, err
	}
	if !c.who.isAdmin() {
		f.q.UserID = c.who.userID()
		if f.q.UserID == "" {
			return f, errForbidden(eUserRequired)
		}
	}
	if f.status == "ok" {
		f.q.StatusClass = 2
	}
	return f, nil
}

func (f logFilter) post() bool {
	return f.status == "error" || f.tier != "" || f.cause != "" || f.session != "" || f.retried
}

// eachLog calls fn with the summaries f matches, newest first, from
// cursor on, page by page until fn has asked for no more (it still sees
// the rest of that page) or the log ends; it returns the cursor after the
// last page read ("" at the end). Filters the store cannot apply are
// applied here.
func (h *Handler) eachLog(c *call, f logFilter, cursor string, pageSize, maxPages int,
	fn func(r *plugin.Record, s logSummary) bool) (string, error) {
	lr, err := h.logRefs(c)
	if err != nil {
		return "", err
	}
	cur := h.currency(c.ctx)
	q := f.q
	q.Cursor, q.Limit = cursor, pageSize
	post := f.post()
	for range maxPages {
		page, err := h.d.Store.QueryRecords(c.ctx, q)
		if err != nil {
			return "", err
		}
		more := true
		for _, r := range page.Records {
			s := h.summarize(r, lr, cur)
			if post && !matchLogFilters(r, s, f.status, f.tier, f.cause, f.session, f.retried) {
				continue
			}
			if !fn(r, s) {
				more = false
			}
		}
		if page.Next == "" || !more {
			return page.Next, nil
		}
		q.Cursor = page.Next
	}
	return q.Cursor, nil
}

func (h *Handler) listLogs(c *call) error {
	limit := c.intQuery("limit", 100, 1, 500)
	f, err := c.logFilter()
	if err != nil {
		return err
	}
	items := []logSummary{}
	pages := 20
	if !f.post() {
		pages = 1
	}
	// Reads on until a page is full or the log ends.
	nextCursor, err := h.eachLog(c, f, c.query("cursor"), limit, pages, func(_ *plugin.Record, s logSummary) bool {
		items = append(items, s)
		return len(items) < limit
	})
	if err != nil {
		return err
	}
	var next *string
	if nextCursor != "" {
		next = &nextCursor
	}
	return c.json(200, map[string]any{"items": items, "next_cursor": next})
}

// histogramBucket picks a bucket giving 30-60 bars over the window.
func histogramBucket(span time.Duration) time.Duration {
	for _, b := range []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute,
		30 * time.Minute, time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour, 48 * time.Hour} {
		if span/b <= 60 {
			return b
		}
	}
	return 7 * 24 * time.Hour
}

type histogramPoint struct {
	T        time.Time `json:"t"`
	Requests int64     `json:"requests"`
	Failed   int64     `json:"failed"`
}

// logHistogram counts the requests /logs would list per time bucket. Every
// bucket from `from` to `to` is present, so the bars match the range.
func (h *Handler) logHistogram(c *call) error {
	q := store.RecordQuery{KeyID: c.query("key_id"), UserID: c.query("user_id"), Model: c.query("model"),
		UpstreamID: c.query("upstream_id"), RequestID: c.query("q"), Tier: c.query("tier"), SessionID: c.query("session_id"),
		Client: c.query("client"), Retried: c.query("retried") == "true"}
	var err error
	if q.From, err = c.timeQuery("from"); err != nil {
		return err
	}
	if q.To, err = c.timeQuery("to"); err != nil {
		return err
	}
	if q.To.IsZero() {
		q.To = c.h.d.Now().UTC()
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-24 * time.Hour)
	}
	if !q.From.Before(q.To) {
		return fieldInvalid("from", eFromAfterTo)
	}
	if !c.who.isAdmin() {
		q.UserID = c.who.userID()
		if q.UserID == "" {
			return errForbidden(eUserRequired)
		}
	}
	switch c.query("status") {
	case "ok":
		q.StatusClass = 2
	case "error":
		q.Failed = true
	}
	bucket := histogramBucket(q.To.Sub(q.From))
	bs, err := h.d.Store.RecordHistogram(c.ctx, q, bucket)
	if err != nil {
		return err
	}
	byT := map[int64]store.RecordBucket{}
	for _, b := range bs {
		byT[b.Time.UnixMilli()] = b
	}
	points := []histogramPoint{}
	bms := bucket.Milliseconds()
	for t := q.From.UnixMilli() / bms * bms; t < q.To.UnixMilli(); t += bms {
		b := byT[t]
		points = append(points, histogramPoint{T: time.UnixMilli(t).UTC(), Requests: b.Requests, Failed: b.Failed})
	}
	return c.json(200, map[string]any{"from": q.From, "to": q.To, "bucket_seconds": int(bucket / time.Second), "points": points})
}

func matchLogFilters(r *plugin.Record, s logSummary, status, tier, cause, session string, retried bool) bool {
	if status == "error" && r.Status >= 200 && r.Status < 300 {
		return false
	}
	if tier != "" && string(s.Tier) != tier {
		return false
	}
	if cause != "" {
		found := false
		for _, a := range r.Attempts {
			found = found || string(a.Cause) == cause
		}
		if !found {
			return false
		}
	}
	if session != "" && r.SessionID != session {
		return false
	}
	return !retried || s.retriedInside
}

func (h *Handler) getLog(c *call) error {
	r, err := h.d.Store.GetRecord(c.ctx, c.r.PathValue("id"))
	if err != nil {
		return storeErr(err, objRequest)
	}
	if !c.who.isAdmin() && r.Caller.UserID != c.who.userID() {
		return errNotFound(eNotFound.with("object", objRequest))
	}
	lr, err := h.logRefs(c)
	if err != nil {
		return err
	}
	cur := h.currency(c.ctx)
	d := logDetail{logSummary: h.summarize(r, lr, cur), SessionID: r.SessionID, ClientUA: r.ClientUA,
		Error: r.Error, Usage: r.Usage, HasBody: r.Body != nil, AttemptsDetail: []attemptView{}}
	if r.SessionID != "" {
		var owner string
		if !c.who.isAdmin() {
			owner = c.who.userID()
		}
		tot, err := h.d.Store.SessionTotalAt(c.ctx, r.SessionID, owner, r.Time, r.ID)
		if err != nil {
			return err
		}
		d.Session = &sessionTotal{Requests: tot.Requests, Turn: tot.Turn, Charge: money{Micro: tot.ChargeMicro, Currency: cur},
			First: tot.First, Last: tot.Last}
	}
	if r.Body != nil {
		d.Body = &recordBody{Request: r.Body.Request, Response: r.Body.Response}
	}
	admin := c.who.isAdmin()
	credLabels := map[string]string{}
	if admin {
		if cs, err := h.d.Store.ListCredentials(c.ctx, ""); err == nil {
			for _, cr := range cs {
				credLabels[cr.ID] = CredentialLabel(&cr)
			}
		}
	}
	var start time.Duration
	for _, a := range r.Attempts {
		av := attemptView{N: a.N, Upstream: lr.upstream(a.UpstreamID), EndpointID: a.EndpointID, Protocol: a.Protocol,
			UpstreamModel: a.UpstreamModel, Tier: a.Tier, Status: a.Status, Cause: a.Cause, Sent: a.Sent,
			StartMs: ms(start), DurationMs: ms(a.Duration), Usage: a.Usage, Diagnostic: a.Diagnostic, Error: a.Error,
			BytesIn: a.BytesIn, BytesOut: a.BytesOut,
			Cost: money{Currency: cur}, Charged: a.Charge != nil}
		if admin {
			av.Cost.Micro = a.CostMicro
			if a.CredentialID != "" {
				av.Credential = &ref{ID: a.CredentialID, Name: orDefault(credLabels[a.CredentialID], a.CredentialID)}
			}
		}
		if a.TTFB > 0 {
			t := ms(a.TTFB)
			av.TTFBMs = &t
		}
		if lr.hide {
			av.EndpointID = ""
		}
		start += a.Duration
		d.AttemptsDetail = append(d.AttemptsDetail, av)
	}
	d.Billing = h.billing(c.ctx, r, cur, admin)
	return c.json(200, d)
}

// billing explains the charge bucket by bucket at the rates the pipeline
// recorded on the billed attempt (list price × group multiplier). Records
// without them are explained at the current price rules.
func (h *Handler) billing(ctx context.Context, r *plugin.Record, cur string, admin bool) billing {
	b := billing{Lines: []billingLine{}, Charged: money{Micro: r.ChargeMicro, Currency: cur}}
	if admin {
		b.Cost = &money{Micro: r.CostMicro, Currency: cur}
	}
	var rule *store.PriceRule
	for _, a := range r.Attempts {
		if ch := a.Charge; ch != nil {
			rule = &store.PriceRule{Input: ch.Rates.Input, CacheRead: ch.Rates.CacheRead, CacheWrite5m: ch.Rates.CacheWrite5m,
				CacheWrite1h: ch.Rates.CacheWrite1h, Output: ch.Rates.Output}
			if admin {
				m := ch.Multiplier
				b.Multiplier = &m
			}
		}
	}
	if n := len(r.Attempts); rule == nil && n > 0 {
		a := r.Attempts[n-1]
		if ps, err := h.d.Store.ListPrices(ctx); err == nil {
			rule = UpstreamPrice(ps, a.UpstreamID, a.UpstreamModel)
		}
	}
	u := r.Usage
	for _, l := range []struct {
		bucket string
		tokens int64
		rate   func(p *store.PriceRule) int64
	}{
		{"input", u.Input, func(p *store.PriceRule) int64 { return p.Input }},
		{"cache_read", u.CacheRead, func(p *store.PriceRule) int64 { return p.CacheRead }},
		{"cache_write_5m", u.CacheWrite5m, func(p *store.PriceRule) int64 { return p.CacheWrite5m }},
		{"cache_write_1h", u.CacheWrite1h, func(p *store.PriceRule) int64 { return p.CacheWrite1h }},
		{"output", u.Output, func(p *store.PriceRule) int64 { return p.Output }},
	} {
		if l.tokens == 0 {
			continue
		}
		line := billingLine{Bucket: l.bucket, Tokens: l.tokens, UnitPrice: money{Currency: cur}, Amount: money{Currency: cur}}
		if rule != nil {
			line.UnitPrice.Micro = l.rate(rule)
			line.Amount.Micro = (l.tokens*line.UnitPrice.Micro + 500_000) / 1_000_000
		}
		b.Lines = append(b.Lines, line)
	}
	if rule == nil {
		b.Note = "no_price"
	}
	if u.Estimated {
		b.Note = "estimated"
	}
	return b
}

// UpstreamPrice mirrors how the relay prices an attempt: a rule for the
// upstream and model, then for the upstream, then for the model name.
func UpstreamPrice(ps []store.PriceRule, upstreamID, upstreamModel string) *store.PriceRule {
	for _, want := range []func(p store.PriceRule) bool{
		func(p store.PriceRule) bool { return p.UpstreamID == upstreamID && p.UpstreamModel == upstreamModel },
		func(p store.PriceRule) bool {
			return p.UpstreamID == upstreamID && p.UpstreamModel == "" && p.Model == ""
		},
	} {
		for i := range ps {
			if want(ps[i]) {
				return &ps[i]
			}
		}
	}
	if p, ok := modelPrice(ps, upstreamModel); ok {
		return &p
	}
	return nil
}
