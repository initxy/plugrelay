package admin

import (
	"encoding/csv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/clientid"
	"github.com/initxy/plugrelay/pkg/plugin"
)

type sessionView struct {
	ID           string    `json:"id"`
	First        time.Time `json:"first"`
	Last         time.Time `json:"last"`
	Requests     int64     `json:"requests"`
	Failed       int64     `json:"failed"`
	Key          *ref      `json:"key,omitempty"`
	User         *ref      `json:"user,omitempty"`
	Client       clientRef `json:"client"`
	Models       []string  `json:"models"`
	Charge       money     `json:"charge"`
	CacheHitRate *float64  `json:"cache_hit_rate"`
}

// sessionSpan is how far back the session list looks when the request
// names no time: grouping the whole log would scan all of it.
const sessionSpan = 30 * 24 * time.Hour

// listSessions lists the sessions the /logs filters match, newest last
// request first; each sums only the requests that matched.
func (h *Handler) listSessions(c *call) error {
	f, err := c.logFilter()
	if err != nil {
		return err
	}
	q := f.q
	if q.From.IsZero() && q.To.IsZero() {
		q.From = h.d.Now().Add(-sessionSpan)
	}
	q.Limit, q.Cursor = c.intQuery("limit", 50, 1, 200), c.query("cursor")
	q.SessionID, q.Tier, q.Retried, q.Cause = f.session, f.tier, f.retried, f.cause
	if f.status == "error" {
		q.Failed = true
	}
	page, err := h.d.Store.SessionPage(c.ctx, q)
	if err != nil {
		return err
	}
	n, err := h.names(c.ctx)
	if err != nil {
		return err
	}
	cur := h.currency(c.ctx)
	items := make([]sessionView, 0, len(page.Sessions))
	for _, s := range page.Sessions {
		v := sessionView{ID: s.ID, First: s.First, Last: s.Last, Requests: s.Requests, Failed: s.Failed,
			User: n.user(s.UserID), Client: clientOf(s.Client, ""), Models: s.Models,
			Charge: money{Micro: s.ChargeMicro, Currency: cur}}
		if s.KeyID != "" {
			v.Key = &ref{ID: s.KeyID, Name: orDefault(s.KeyName, s.KeyID)}
		}
		if s.Input > 0 {
			r := float64(s.CacheRead) / float64(s.Input)
			v.CacheHitRate = &r
		}
		items = append(items, v)
	}
	var next *string
	if page.Next != "" {
		next = &page.Next
	}
	return c.json(200, map[string]any{"items": items, "next_cursor": next})
}

// listClients lists the client programs the relay recognises, then other.
func (h *Handler) listClients(c *call) error {
	items := clientid.Known()
	items = append(items, clientid.Info{ID: clientid.Other})
	return c.json(200, map[string]any{"items": items})
}

// exportMaxRows bounds one CSV export.
const exportMaxRows = 100_000

type exportCol struct {
	id, en, zh string
	admin      bool
	value      func(r *plugin.Record, s logSummary, zh bool) string
}

func num(n int64) string { return strconv.FormatInt(n, 10) }

// decimal writes micro-units as a plain decimal in currency units.
func decimal(micro int64) string {
	sign := ""
	if micro < 0 {
		sign, micro = "-", -micro
	}
	frac := strings.TrimRight(strconv.FormatInt(1_000_000+micro%1_000_000, 10)[1:], "0")
	if frac == "" {
		return sign + num(micro/1_000_000)
	}
	return sign + num(micro/1_000_000) + "." + frac
}

var tierWords = map[plugin.Tier][2]string{
	plugin.TierPassthrough: {"passed through", "原样转发"},
	plugin.TierPatch:       {"lightly edited", "小改转发"},
	plugin.TierConvert:     {"converted", "协议转换"},
}

var causeWords = map[string][2]string{
	"request":      {"bad request", "请求本身有问题"},
	"transient":    {"upstream error", "上游临时出错"},
	"rate_limit":   {"rate limited", "被限流"},
	"quota":        {"out of quota", "额度用完"},
	"auth_expired": {"credential expired", "凭据过期"},
	"banned":       {"account suspended", "疑似封号"},
	"content":      {"content refused", "内容被拒"},
	"unknown":      {"unknown", "原因不明"},
	"not_sent":     {"not sent upstream", "没发到上游"},
}

func pick(w [2]string, zh bool) string {
	if zh {
		return w[1]
	}
	return w[0]
}

// exportCols are the CSV columns in order (documented in
// docs/api.md#request-log-export).
var exportCols = []exportCol{
	{"time", "Time (UTC)", "时间（UTC）", false, func(r *plugin.Record, _ logSummary, _ bool) string {
		return r.Time.UTC().Format(time.RFC3339)
	}},
	{"request_id", "Request ID", "请求 ID", false, func(r *plugin.Record, _ logSummary, _ bool) string { return r.ID }},
	{"client", "Client", "客户端", false, func(_ *plugin.Record, s logSummary, zh bool) string {
		if s.Client.Name == "" {
			return pick([2]string{"Other", "其他"}, zh)
		}
		return s.Client.Name
	}},
	{"client_version", "Client version", "客户端版本", false, func(_ *plugin.Record, s logSummary, _ bool) string { return s.Client.Version }},
	{"ip", "Source IP", "来源 IP", false, func(r *plugin.Record, _ logSummary, _ bool) string { return r.ClientIP }},
	{"key", "Key", "密钥", false, func(_ *plugin.Record, s logSummary, _ bool) string {
		if s.Key == nil {
			return ""
		}
		return s.Key.Name
	}},
	{"user", "User", "用户", false, func(_ *plugin.Record, s logSummary, _ bool) string {
		if s.User == nil {
			return ""
		}
		return s.User.Name
	}},
	{"model", "Model", "模型", false, func(r *plugin.Record, _ logSummary, _ bool) string { return r.Model }},
	{"served_model", "Served by model", "实际回答的模型", false, func(_ *plugin.Record, s logSummary, _ bool) string { return s.ServedModel }},
	{"upstream", "Upstream", "上游", false, func(_ *plugin.Record, s logSummary, _ bool) string {
		if s.Upstream == nil {
			return ""
		}
		return s.Upstream.Name
	}},
	{"status", "Status", "状态码", false, func(r *plugin.Record, _ logSummary, _ bool) string { return strconv.Itoa(r.Status) }},
	{"failure", "Failure", "失败原因", false, func(r *plugin.Record, s logSummary, zh bool) string {
		if r.Status >= 200 && r.Status < 300 {
			return ""
		}
		cause := string(s.Cause)
		if cause == "" {
			cause = "unknown"
			if len(r.Attempts) == 0 {
				cause = "not_sent"
			}
		}
		return pick(causeWords[cause], zh)
	}},
	{"handling", "Handling", "处理方式", false, func(_ *plugin.Record, s logSummary, zh bool) string {
		if w, ok := tierWords[s.Tier]; ok {
			return pick(w, zh)
		}
		return ""
	}},
	{"attempts", "Attempts", "尝试次数", false, func(r *plugin.Record, _ logSummary, _ bool) string { return strconv.Itoa(len(r.Attempts)) }},
	{"input_tokens", "Input tokens", "输入 tokens", false, func(r *plugin.Record, _ logSummary, _ bool) string { return num(r.Usage.Input) }},
	{"cache_read_tokens", "Cache read tokens", "缓存读取 tokens", false, func(r *plugin.Record, _ logSummary, _ bool) string {
		return num(r.Usage.CacheRead)
	}},
	{"cache_write_tokens", "Cache write tokens", "缓存写入 tokens", false, func(r *plugin.Record, _ logSummary, _ bool) string {
		return num(r.Usage.CacheWrite5m + r.Usage.CacheWrite1h)
	}},
	{"output_tokens", "Output tokens", "输出 tokens", false, func(r *plugin.Record, _ logSummary, _ bool) string { return num(r.Usage.Output) }},
	{"reasoning_tokens", "Of which reasoning", "其中推理 tokens", false, func(r *plugin.Record, _ logSummary, _ bool) string {
		return num(r.Usage.Reasoning)
	}},
	{"first_token_ms", "First token (ms)", "首字耗时（毫秒）", false, func(r *plugin.Record, _ logSummary, _ bool) string {
		if r.TTFB <= 0 {
			return ""
		}
		return num(r.TTFB.Milliseconds())
	}},
	{"duration_ms", "Total time (ms)", "总耗时（毫秒）", false, func(r *plugin.Record, _ logSummary, _ bool) string {
		return num(r.Duration.Milliseconds())
	}},
	{"charge", "Charged", "费用", false, func(r *plugin.Record, _ logSummary, _ bool) string { return decimal(r.ChargeMicro) }},
	{"cost", "Upstream cost", "上游成本", true, func(r *plugin.Record, _ logSummary, _ bool) string { return decimal(r.CostMicro) }},
	{"currency", "Currency", "币种", false, func(_ *plugin.Record, s logSummary, _ bool) string { return s.Charge.Currency }},
	{"session_id", "Session", "会话", false, func(r *plugin.Record, _ logSummary, _ bool) string { return r.SessionID }},
}

// exportZh reports whether the export is in Chinese: lang=zh-CN|en, else
// the first Accept-Language entry.
func exportZh(c *call) bool {
	lang := c.query("lang")
	if lang == "" {
		lang, _, _ = strings.Cut(c.r.Header.Get("Accept-Language"), ",")
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(lang)), "zh")
}

// exportLogs streams the requests the /logs filters match as CSV, newest
// first, at most exportMaxRows.
func (h *Handler) exportLogs(c *call) error {
	f, err := c.logFilter()
	if err != nil {
		return err
	}
	zh, admin := exportZh(c), c.who.isAdmin()
	cols := make([]exportCol, 0, len(exportCols))
	for _, col := range exportCols {
		if !col.admin || admin {
			cols = append(cols, col)
		}
	}
	w := c.w
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="requests-`+h.d.Now().UTC().Format("20060102-1504")+`.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	// A byte-order mark makes spreadsheet apps read the file as UTF-8.
	_, _ = w.Write([]byte("\xef\xbb\xbf"))
	cw := csv.NewWriter(w)
	row := make([]string, len(cols))
	for i, col := range cols {
		row[i] = pick([2]string{col.en, col.zh}, zh)
	}
	_ = cw.Write(row)
	n := 0
	_, err = h.eachLog(c, f, "", 500, exportMaxRows/500+1, func(r *plugin.Record, s logSummary) bool {
		if n >= exportMaxRows {
			return false
		}
		n++
		for i, col := range cols {
			row[i] = csvCell(col.value(r, s, zh))
		}
		_ = cw.Write(row)
		if n%1000 == 0 {
			cw.Flush()
		}
		return n < exportMaxRows
	})
	cw.Flush()
	// Headers are sent: a failure mid-way can only cut the file short.
	if err != nil {
		h.d.Logger.Warn("admin: log export cut short", "err", err)
	}
	return nil
}

// csvCell keeps a spreadsheet from running a cell as a formula: text
// starting with = + - @ tab or CR gets a leading quote. Numbers stay as
// they are; a negative amount is not a formula.
func csvCell(v string) string {
	if v == "" || !strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return v
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return v
	}
	return "'" + v
}

type topRow struct {
	Key          string   `json:"key"`
	Label        string   `json:"label"`
	Requests     int64    `json:"requests"`
	Errors       int64    `json:"errors"`
	Spend        money    `json:"spend"`
	Tokens       int64    `json:"tokens"`
	CacheHitRate *float64 `json:"cache_hit_rate"`
	TTFBP95Ms    *int64   `json:"ttfb_p95_ms"`
}

// getUsageTop ranks models, keys, users or clients by spend over the
// window, with tokens, cache hit and the p95 first-token time.
func (h *Handler) getUsageTop(c *call) error {
	rng, from, to, err := c.window()
	if err != nil {
		return err
	}
	by := c.query("by")
	if by != "model" && by != "key" && by != "user" && by != "client" {
		return fieldInvalid("by", oneOf("model", "key", "user", "client"))
	}
	limit := c.intQuery("limit", 10, 1, 50)
	q := store.UsageQuery{From: from, To: to, Bucket: to.Sub(from).Truncate(time.Hour) + 2*time.Hour, By: by}
	if err := c.usageScope(&q); err != nil {
		return err
	}
	pts, err := h.d.Store.UsageSeries(c.ctx, q)
	if err != nil {
		return err
	}
	type acc struct{ requests, errors, charge, input, cacheRead, output int64 }
	sums := map[string]*acc{}
	for _, p := range pts {
		key := p.Series
		a := sums[key]
		if a == nil {
			a = &acc{}
			sums[key] = a
		}
		a.requests += p.Requests
		a.errors += p.Errors
		a.charge += p.ChargeMicro
		a.input += p.Input + p.CacheRead + p.CacheWrite5m + p.CacheWrite1h
		a.cacheRead += p.CacheRead
		a.output += p.Output
	}
	keys := make([]string, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := sums[keys[i]], sums[keys[j]]
		if a.charge != b.charge {
			return a.charge > b.charge
		}
		if a.requests != b.requests {
			return a.requests > b.requests
		}
		return keys[i] < keys[j]
	})
	if len(keys) > limit {
		keys = keys[:limit]
	}
	label, err := h.topLabels(c, by)
	if err != nil {
		return err
	}
	cur := h.currency(c.ctx)
	items := make([]topRow, 0, len(keys))
	for _, k := range keys {
		a := sums[k]
		row := topRow{Key: k, Label: label(k), Requests: a.requests, Errors: a.errors, Spend: money{Micro: a.charge, Currency: cur},
			Tokens: a.input + a.output}
		if a.input > 0 {
			r := float64(a.cacheRead) / float64(a.input)
			row.CacheHitRate = &r
		}
		rq := store.RecordQuery{From: from, To: to, KeyID: q.KeyID, UserID: q.UserID, StatusClass: 2}
		switch by {
		case "model":
			rq.Model = k
		case "key":
			rq.KeyID = k
		case "user":
			rq.UserID = k
		case "client":
			rq.Client = k
		}
		if d, ok, err := h.d.Store.TTFBQuantile(c.ctx, rq, 0.95); err != nil {
			return err
		} else if ok {
			v := d.Milliseconds()
			row.TTFBP95Ms = &v
		}
		items = append(items, row)
	}
	return c.json(200, map[string]any{"range": rng, "from": from, "to": to, "by": by, "items": items})
}

// topLabels names the keys of one top table. Client "other" has no name
// (the console words it); unknown keys keep their id.
func (h *Handler) topLabels(c *call, by string) (func(string) string, error) {
	switch by {
	case "client":
		return clientid.Name, nil
	case "user":
		n, err := h.names(c.ctx)
		if err != nil {
			return nil, err
		}
		return func(id string) string {
			if r := n.user(id); r != nil {
				return r.Name
			}
			return id
		}, nil
	case "key":
		f := store.KeyFilter{}
		if !c.who.isAdmin() {
			f.UserID = c.who.userID()
		}
		ks, err := h.d.Store.ListKeys(c.ctx, f)
		if err != nil {
			return nil, err
		}
		names := map[string]string{}
		for _, k := range ks {
			names[k.ID] = k.Name
		}
		return func(id string) string { return orDefault(names[id], id) }, nil
	}
	return func(id string) string { return id }, nil
}

// getUsageFailures counts failed requests per bucket by final cause over
// the window; every series has every bucket.
func (h *Handler) getUsageFailures(c *call) error {
	rng, from, to, err := c.window()
	if err != nil {
		return err
	}
	uq := store.UsageQuery{}
	if err := c.usageScope(&uq); err != nil {
		return err
	}
	bucket := bucketFor(rng)
	q := store.RecordQuery{From: from, To: to, KeyID: uq.KeyID, UserID: uq.UserID}
	pts, err := h.d.Store.FailureSeries(c.ctx, q, bucket)
	if err != nil {
		return err
	}
	bms := bucket.Milliseconds()
	var slots []int64
	for t := from.UnixMilli() / bms * bms; t < to.UnixMilli(); t += bms {
		slots = append(slots, t)
	}
	byCause := map[string]map[int64]int64{}
	totals := map[string]int64{}
	for _, p := range pts {
		m := byCause[p.Cause]
		if m == nil {
			m = map[int64]int64{}
			byCause[p.Cause] = m
		}
		m[p.Time.UnixMilli()] += p.Failed
		totals[p.Cause] += p.Failed
	}
	causes := make([]string, 0, len(byCause))
	for k := range byCause {
		causes = append(causes, k)
	}
	sort.Slice(causes, func(i, j int) bool {
		if totals[causes[i]] != totals[causes[j]] {
			return totals[causes[i]] > totals[causes[j]]
		}
		return causes[i] < causes[j]
	})
	out := []map[string]any{}
	for _, cause := range causes {
		points := make([]seriesPoint, 0, len(slots))
		for _, t := range slots {
			points = append(points, seriesPoint{T: time.UnixMilli(t).UTC(), V: float64(byCause[cause][t])})
		}
		out = append(out, map[string]any{"cause": cause, "total": totals[cause], "points": points})
	}
	return c.json(200, map[string]any{"range": rng, "bucket_seconds": int(bucket / time.Second), "series": out})
}
