package admin

import (
	"context"
	"sort"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/clientid"
	"github.com/initxy/plugrelay/pkg/plugin"
)

var ranges = map[string]time.Duration{
	"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "90d": 90 * 24 * time.Hour,
}

// window resolves range/from/to into [from, to).
func (c *call) window() (rng string, from, to time.Time, err error) {
	rng = orDefault(c.query("range"), "7d")
	d, ok := ranges[rng]
	if !ok {
		return "", time.Time{}, time.Time{}, fieldInvalid("range", oneOf("1h", "24h", "7d", "30d", "90d"))
	}
	if from, err = c.timeQuery("from"); err != nil {
		return
	}
	if to, err = c.timeQuery("to"); err != nil {
		return
	}
	if to.IsZero() {
		to = c.h.d.Now().UTC()
	}
	if from.IsZero() {
		from = to.Add(-d)
	}
	if !from.Before(to) {
		return "", time.Time{}, time.Time{}, fieldInvalid("from", eFromAfterTo)
	}
	return rng, from, to, nil
}

// usageScope narrows usage reads to what the caller may see.
func (c *call) usageScope(q *store.UsageQuery) error {
	q.KeyID, q.UserID = c.query("key_id"), c.query("user_id")
	if c.who.isAdmin() {
		return nil
	}
	q.UserID = c.who.userID()
	if q.KeyID != "" {
		k, err := c.h.d.Store.GetKey(c.ctx, q.KeyID)
		if err != nil || k.UserID != q.UserID {
			return errNotFound(eNotFound.with("object", objKey))
		}
	}
	return nil
}

type totals struct {
	requests, errors, input, cacheRead, cacheWrite, output, charge int64
}

func sum(pts []store.UsagePoint) totals {
	var t totals
	for _, p := range pts {
		t.requests += p.Requests
		t.errors += p.Errors
		t.input += p.Input
		t.cacheRead += p.CacheRead
		t.cacheWrite += p.CacheWrite5m + p.CacheWrite1h
		t.output += p.Output
		t.charge += p.ChargeMicro
	}
	return t
}

func (h *Handler) getOverview(c *call) error {
	rng, from, to, err := c.window()
	if err != nil {
		return err
	}
	q := store.UsageQuery{From: from, To: to, Bucket: to.Sub(from).Truncate(time.Hour) + time.Hour, By: "model"}
	if err := c.usageScope(&q); err != nil {
		return err
	}
	pts, err := h.d.Store.UsageSeries(c.ctx, q)
	if err != nil {
		return err
	}
	prevQ := q
	prevQ.From, prevQ.To = from.Add(-to.Sub(from)), from
	prev, err := h.d.Store.UsageSeries(c.ctx, prevQ)
	if err != nil {
		return err
	}
	cur := h.currency(c.ctx)
	t, tp := sum(pts), sum(prev)
	out := map[string]any{
		"range": rng, "from": from, "to": to,
		"spend":         money{Micro: t.charge, Currency: cur},
		"spend_prev":    money{Micro: tp.charge, Currency: cur},
		"requests":      t.requests,
		"requests_prev": tp.requests,
	}
	out["success_rate"] = 1.0
	if t.requests > 0 {
		out["success_rate"] = float64(t.requests-t.errors) / float64(t.requests)
	}
	if tp.requests > 0 {
		out["success_rate_prev"] = float64(tp.requests-tp.errors) / float64(tp.requests)
	}
	allIn := t.input + t.cacheRead + t.cacheWrite
	out["cache_hit_rate"] = 0.0
	if allIn > 0 {
		out["cache_hit_rate"] = float64(t.cacheRead) / float64(allIn)
	}
	if in := tp.input + tp.cacheRead + tp.cacheWrite; in > 0 {
		out["cache_hit_rate_prev"] = float64(tp.cacheRead) / float64(in)
	}
	rq := store.RecordQuery{From: from, To: to, KeyID: q.KeyID, UserID: q.UserID, StatusClass: 2}
	if d, ok, err := h.d.Store.TTFBQuantile(c.ctx, rq, 0.95); err != nil {
		return err
	} else if ok {
		out["ttfb_p95_ms"] = d.Milliseconds()
	}
	rq.From, rq.To = prevQ.From, prevQ.To
	if d, ok, err := h.d.Store.TTFBQuantile(c.ctx, rq, 0.95); err != nil {
		return err
	} else if ok {
		out["ttfb_p95_prev_ms"] = d.Milliseconds()
	}
	var saved int64
	if ps, err := h.d.Store.ListPrices(c.ctx); err == nil {
		for _, p := range pts {
			if rule, ok := modelPrice(ps, p.Series); ok && rule.Input > rule.CacheRead {
				saved += p.CacheRead * (rule.Input - rule.CacheRead) / 1_000_000
			}
		}
	}
	out["cache_saved"] = money{Micro: saved, Currency: cur}
	var avg int64
	if tok := allIn + t.output; tok > 0 {
		avg = t.charge * 1_000_000 / tok
	}
	out["avg_price_per_mtok"] = money{Micro: avg, Currency: cur}
	if b := h.overviewBalance(c, t.charge, to.Sub(from), q.UserID == c.who.userID() && q.KeyID == ""); b != nil {
		out["balance"] = b
	}
	return c.json(200, out)
}

func bucketFor(rng string) time.Duration {
	switch rng {
	case "1h", "24h":
		return time.Hour
	case "7d":
		return 6 * time.Hour
	}
	return 24 * time.Hour
}

const hourMs = int64(time.Hour / time.Millisecond)

type seriesPoint struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

type series struct {
	Key    string        `json:"key"`
	Label  string        `json:"label"`
	Points []seriesPoint `json:"points"`
}

func (h *Handler) getTimeseries(c *call) error {
	rng, from, to, err := c.window()
	if err != nil {
		return err
	}
	metric := orDefault(c.query("metric"), "spend")
	groupBy := orDefault(c.query("group_by"), "model")
	by := map[string]string{"none": "", "model": "model", "key": "key", "upstream": "upstream", "user": "user", "client": "client"}
	b, ok := by[groupBy]
	if !ok {
		return fieldInvalid("group_by", oneOf("none", "model", "key", "upstream", "user", "client"))
	}
	if metric != "spend" && metric != "requests" && metric != "tokens" && metric != "cache_hit_rate" {
		return fieldInvalid("metric", oneOf("spend", "requests", "tokens", "cache_hit_rate"))
	}
	bucket := bucketFor(rng)
	switch c.query("bucket") {
	case "":
	case "hour":
		bucket = time.Hour
	default:
		return fieldInvalid("bucket", oneOf("hour"))
	}
	q := store.UsageQuery{From: from, To: to, Bucket: bucket, By: b}
	if err := c.usageScope(&q); err != nil {
		return err
	}
	pts, err := h.d.Store.UsageSeries(c.ctx, q)
	if err != nil {
		return err
	}
	var lr *logRefs
	if b == "upstream" || b == "user" {
		if lr, err = h.logRefs(c); err != nil {
			return err
		}
	}
	bySeries := map[string]*series{}
	var order []string
	for _, p := range pts {
		key := p.Series
		if key == "" {
			key = "all"
		}
		s := bySeries[key]
		if s == nil {
			label := key
			switch {
			case b == "client":
				label = clientid.Name(key)
			case b == "upstream" && lr != nil:
				label = lr.upstream(key).Name
			case b == "user" && lr != nil:
				if r := lr.names.user(key); r != nil {
					label = r.Name
				}
			}
			s = &series{Key: key, Label: label, Points: []seriesPoint{}}
			bySeries[key] = s
			order = append(order, key)
		}
		var v float64
		switch metric {
		case "spend":
			v = float64(p.ChargeMicro)
		case "requests":
			v = float64(p.Requests)
		case "tokens":
			v = float64(p.Input + p.CacheRead + p.CacheWrite5m + p.CacheWrite1h + p.Output)
		case "cache_hit_rate":
			if in := p.Input + p.CacheRead + p.CacheWrite5m + p.CacheWrite1h; in > 0 {
				v = float64(p.CacheRead) / float64(in)
			}
		}
		s.Points = append(s.Points, seriesPoint{T: p.Time, V: v})
	}
	sort.Strings(order)
	// Every series gets every bucket of the window, zero where nothing
	// happened, so all series line up and the bars match the range.
	bms := bucket.Milliseconds()
	var slots []time.Time
	for t := from.UnixMilli() / hourMs * hourMs / bms * bms; t < to.UnixMilli(); t += bms {
		slots = append(slots, time.UnixMilli(t).UTC())
	}
	out := []series{}
	for _, k := range order {
		s := bySeries[k]
		have := map[int64]float64{}
		for _, p := range s.Points {
			have[p.T.UnixMilli()] += p.V
		}
		filled := make([]seriesPoint, 0, len(slots))
		for _, t := range slots {
			filled = append(filled, seriesPoint{T: t, V: have[t.UnixMilli()]})
		}
		s.Points = filled
		out = append(out, *s)
	}
	resp := map[string]any{"metric": metric, "bucket_seconds": int(bucket / time.Second), "series": out}
	if metric == "spend" {
		resp["currency"] = h.currency(c.ctx)
	}
	return c.json(200, resp)
}

type quotaWindow struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"`
	Scope    string     `json:"scope"`
	Used     money      `json:"used"`
	Limit    money      `json:"limit"`
	ResetsAt *time.Time `json:"resets_at"`
}

// getQuota reports the spend budgets of meter.quota that apply to the
// caller (or one key), side by side.
func (h *Handler) getQuota(c *call) error {
	caller := plugin.Caller{}
	if u := c.who.user; u != nil {
		caller.UserID, caller.GroupID = u.ID, u.GroupID
	}
	if id := c.query("key_id"); id != "" {
		k, err := h.ownKey(c, id)
		if err != nil {
			return err
		}
		caller = k.Caller()
	}
	set := h.d.Runtime.SettingsFor("meter.quota", caller)
	now, loc := h.d.Now(), Timezone(c.ctx, h.d.Store)
	day, month := dayStart(now, loc), monthStart(now, loc)
	cur := h.currency(c.ctx)
	windows := []quotaWindow{}
	for _, lvl := range []struct{ scope, id string }{{"key", caller.KeyID}, {"user", caller.UserID}, {"group", caller.GroupID}} {
		if lvl.id == "" {
			continue
		}
		for _, w := range []struct {
			kind, name  string
			since, next time.Time
		}{
			{"daily", "daily", day, day.AddDate(0, 0, 1)},
			{"monthly", "monthly", month, month.AddDate(0, 1, 0)},
			{"total", "total", time.Unix(0, 0), time.Time{}},
		} {
			limit := set.Get(lvl.scope + "_" + w.name + "_micro").Int()
			if limit <= 0 {
				continue
			}
			used, _, err := h.d.Store.Spent(c.ctx, lvl.scope+":"+lvl.id, w.since)
			if err != nil {
				return err
			}
			windows = append(windows, quotaWindow{ID: lvl.scope + "-" + w.kind, Kind: w.kind, Scope: lvl.scope,
				Used: money{Micro: used, Currency: cur}, Limit: money{Micro: limit, Currency: cur}, ResetsAt: timePtr(w.next.UTC())})
		}
	}
	return c.json(200, map[string]any{"windows": windows})
}

type statusItem struct {
	Model       string     `json:"model"`
	State       string     `json:"state"`
	SuccessRate float64    `json:"success_rate_24h"`
	Hourly      []*float64 `json:"hourly"`
}

func (h *Handler) getStatus(c *call) error {
	allowed := h.callerModelFilter(c)
	if c.who.isAdmin() {
		allowed = func(string) bool { return true }
	}
	items, err := h.statusItems(c.ctx, allowed)
	if err != nil {
		return err
	}
	return c.json(200, map[string]any{"items": items, "updated_at": h.d.Now().UTC()})
}

// statusItems is the last 24 hours of every enabled model allowed passes.
func (h *Handler) statusItems(ctx context.Context, allowed func(string) bool) ([]statusItem, error) {
	ms, err := h.d.Store.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	stats, err := h.hourlyStats(ctx, "model")
	if err != nil {
		return nil, err
	}
	items := []statusItem{}
	for _, m := range ms {
		if !m.Enabled || !allowed(m.Name) {
			continue
		}
		s, ok := stats[m.Name]
		if !ok {
			s = emptyStats()
		}
		it := statusItem{Model: m.Name, State: "ok", SuccessRate: 1, Hourly: s.Hourly}
		if s.SuccessRate != nil {
			it.SuccessRate = *s.SuccessRate
		}
		// The latest hour with traffic decides the state.
		for i := len(s.Hourly) - 1; i >= 0; i-- {
			if r := s.Hourly[i]; r != nil {
				switch {
				case *r < 0.5:
					it.State = "down"
				case *r < 0.95:
					it.State = "degraded"
				}
				break
			}
		}
		items = append(items, it)
	}
	return items, nil
}
