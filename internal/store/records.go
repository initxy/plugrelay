package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/sqldialect"
	"github.com/initxy/plugrelay/pkg/clientid"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

const recordCols = `id, time, key_id, key_name, user_id, group_id, protocol, model, stream, session_id, client_ua, client_ip,
  status, error_kind, error, upstream_id, upstream_model, tier, attempt_count, retried, estimated,
  input_tokens, cache_read_tokens, cache_write_5m_tokens, cache_write_1h_tokens, output_tokens, reasoning_tokens,
  cost_micro, charge_micro, ttfb_ms, duration_ms, attempts, "usage", client, client_version`

const hourMs = int64(time.Hour / time.Millisecond)

func isError(r *plugin.Record) bool { return r.Status == 0 || r.Status >= 400 || r.ErrorKind != "" }

func (s *db) InsertRecords(ctx context.Context, recs []*plugin.Record) error {
	if len(recs) == 0 {
		return nil
	}
	return s.tx(ctx, func(r run) error {
		for _, rec := range recs {
			if err := insertRecord(ctx, r, rec); err != nil {
				return err
			}
		}
		return nil
	})
}

// modelWidth is the width of the indexed model columns (MySQL
// VARCHAR(191)).
const modelWidth = 191

func insertRecord(ctx context.Context, r run, rec *plugin.Record) error {
	if rec.ID == "" {
		return fmt.Errorf("store: insert record: empty id")
	}
	var upstream, upstreamModel, tier string
	if n := len(rec.Attempts); n > 0 {
		last := rec.Attempts[n-1]
		upstream, upstreamModel, tier = last.UpstreamID, last.UpstreamModel, string(last.Tier)
	}
	var body sql.NullString
	if rec.Body != nil {
		b, err := json.Marshal(rec.Body)
		if err != nil {
			return fmt.Errorf("store: insert record %s: body: %w", rec.ID, err)
		}
		body = sql.NullString{String: string(b), Valid: true}
	}
	attempts := js(rec.Attempts, "[]")
	usage := js(rec.Usage, "{}")
	u := rec.Usage
	t := rec.Time
	if t.IsZero() {
		t = time.Now()
	}
	// The model is the client's; it is cut to its indexed column's width
	// on every database, so the log and the roll-up agree.
	model := sqldialect.Truncate(rec.Model, modelWidth)
	client := clientid.Client{ID: rec.Client, Version: rec.ClientVersion}
	if client.ID == "" {
		client = clientid.FromUserAgent(rec.ClientUA)
	}
	client.ID, client.Version = sqldialect.Truncate(client.ID, clientid.MaxLen), sqldialect.Truncate(client.Version, clientid.MaxLen)
	var cause string
	if isError(rec) {
		for _, a := range rec.Attempts {
			if a.Cause != "" {
				cause = string(a.Cause)
			}
		}
	}
	res, err := r.exec(ctx, `INSERT INTO request_log (`+recordCols+`, cause, body) VALUES (?`+strings.Repeat(", ?", 36)+`)
ON CONFLICT (id) DO NOTHING`,
		rec.ID, ms(t), rec.Caller.KeyID, rec.Caller.KeyName, rec.Caller.UserID, rec.Caller.GroupID, rec.Protocol, model, rec.Stream,
		rec.SessionID, rec.ClientUA, rec.ClientIP, rec.Status, string(rec.ErrorKind), rec.Error, upstream, upstreamModel, tier,
		len(rec.Attempts), rec.Retried, u.Estimated, u.Input, u.CacheRead, u.CacheWrite5m, u.CacheWrite1h, u.Output, u.Reasoning,
		rec.CostMicro, rec.ChargeMicro, rec.TTFB.Milliseconds(), rec.Duration.Milliseconds(), attempts, usage, client.ID, client.Version,
		cause, body)
	if err != nil {
		return wrap("insert record", err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return wrap("insert record", err) // duplicate: already rolled up
	}
	errs := 0
	if isError(rec) {
		errs = 1
	}
	bucket := ms(t) / hourMs * hourMs
	_, err = r.exec(ctx, `INSERT INTO usage_hourly (bucket, key_id, user_id, group_id, model, upstream_id, client, requests, errors,
  input_tokens, cache_read_tokens, cache_write_5m_tokens, cache_write_1h_tokens, output_tokens, reasoning_tokens, cost_micro, charge_micro)
VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (bucket, key_id, user_id, group_id, model, upstream_id, client) DO UPDATE SET
  requests = usage_hourly.requests + 1,
  errors = usage_hourly.errors + excluded.errors,
  input_tokens = usage_hourly.input_tokens + excluded.input_tokens,
  cache_read_tokens = usage_hourly.cache_read_tokens + excluded.cache_read_tokens,
  cache_write_5m_tokens = usage_hourly.cache_write_5m_tokens + excluded.cache_write_5m_tokens,
  cache_write_1h_tokens = usage_hourly.cache_write_1h_tokens + excluded.cache_write_1h_tokens,
  output_tokens = usage_hourly.output_tokens + excluded.output_tokens,
  reasoning_tokens = usage_hourly.reasoning_tokens + excluded.reasoning_tokens,
  cost_micro = usage_hourly.cost_micro + excluded.cost_micro,
  charge_micro = usage_hourly.charge_micro + excluded.charge_micro`,
		bucket, rec.Caller.KeyID, rec.Caller.UserID, rec.Caller.GroupID, model, upstream, client.ID, errs,
		u.Input, u.CacheRead, u.CacheWrite5m, u.CacheWrite1h, u.Output, u.Reasoning, rec.CostMicro, rec.ChargeMicro)
	return wrap("roll up record", err)
}

func scanRecord(sc interface{ Scan(...any) error }, withBody bool) (*plugin.Record, error) {
	var (
		rec                              plugin.Record
		t, ttfb, dur                     int64
		errKind, upstream, upModel, tier string
		attemptN                         int
		estimated                        bool
		in, cr, cw5, cw1, out, reason    int64
		attempts, usage                  string
		body                             sql.NullString
	)
	dest := []any{&rec.ID, &t, &rec.Caller.KeyID, &rec.Caller.KeyName, &rec.Caller.UserID, &rec.Caller.GroupID, &rec.Protocol, &rec.Model,
		&rec.Stream, &rec.SessionID, &rec.ClientUA, &rec.ClientIP, &rec.Status, &errKind, &rec.Error, &upstream, &upModel, &tier,
		&attemptN, &rec.Retried, &estimated, &in, &cr, &cw5, &cw1, &out, &reason, &rec.CostMicro, &rec.ChargeMicro, &ttfb, &dur,
		&attempts, &usage, &rec.Client, &rec.ClientVersion}
	if withBody {
		dest = append(dest, &body)
	}
	if err := sc.Scan(dest...); err != nil {
		return nil, err
	}
	rec.Time, rec.ErrorKind = fromMs(t), ir.ErrorKind(errKind)
	rec.TTFB, rec.Duration = time.Duration(ttfb)*time.Millisecond, time.Duration(dur)*time.Millisecond
	if err := unjs(attempts, &rec.Attempts); err != nil {
		return nil, err
	}
	if err := unjs(usage, &rec.Usage); err != nil {
		return nil, err
	}
	if body.Valid {
		rec.Body = &plugin.RecordBody{}
		if err := unjs(body.String, rec.Body); err != nil {
			return nil, err
		}
	}
	return &rec, nil
}

func (s *db) GetRecord(ctx context.Context, id string) (*plugin.Record, error) {
	rec, err := scanRecord(s.r().row(ctx, `SELECT `+recordCols+`, body FROM request_log WHERE id = ?`, id), true)
	return rec, wrap("get record", err)
}

func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(ms(t), 10) + ":" + id))
}

func decodeCursor(c string) (int64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", fmt.Errorf("store: bad cursor")
	}
	ts, id, ok := strings.Cut(string(b), ":")
	t, err := strconv.ParseInt(ts, 10, 64)
	if !ok || err != nil {
		return 0, "", fmt.Errorf("store: bad cursor")
	}
	return t, id, nil
}

func likePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s) + "%"
}

// recordWhere is the WHERE clause of q's filters (without the cursor).
func recordWhere(q RecordQuery) ([]string, []any, error) {
	var where []string
	var args []any
	add := func(cond string, a ...any) { where = append(where, cond); args = append(args, a...) }
	if !q.From.IsZero() {
		add("time >= ?", ms(q.From))
	}
	if !q.To.IsZero() {
		add("time < ?", ms(q.To))
	}
	for _, f := range []struct{ col, v string }{
		{"key_id", q.KeyID}, {"user_id", q.UserID}, {"group_id", q.GroupID}, {"model", q.Model}, {"upstream_id", q.UpstreamID},
		{"tier", q.Tier}, {"session_id", q.SessionID}, {"client", q.Client},
	} {
		if f.v != "" {
			add(f.col+" = ?", f.v)
		}
	}
	switch {
	case q.StatusClass == -1:
		add("status = 0")
	case q.StatusClass >= 1 && q.StatusClass <= 5:
		add("status >= ? AND status < ?", q.StatusClass*100, q.StatusClass*100+100)
	case q.StatusClass != 0:
		return nil, nil, fmt.Errorf("invalid status class %d", q.StatusClass)
	}
	if q.Failed {
		add("(status < 200 OR status >= 300)")
	}
	if q.Retried {
		add("attempt_count > 1")
	}
	if q.RequestID != "" {
		add(`id LIKE ? ESCAPE '\'`, likePrefix(q.RequestID))
	}
	if q.Cause != "" {
		add(failedSQL+" AND "+causeSQL+" = ?", q.Cause)
	}
	return where, args, nil
}

const (
	failedSQL = "(status < 200 OR status >= 300)"
	// causeSQL is a failed row's final cause.
	causeSQL = "(CASE WHEN cause <> '' THEN cause WHEN attempt_count = 0 THEN 'not_sent' ELSE 'unknown' END)"
)

func whereSQL(where []string) string {
	if len(where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(where, " AND ")
}

func (s *db) QueryRecords(ctx context.Context, q RecordQuery) (*RecordPage, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	where, args, err := recordWhere(q)
	if err != nil {
		return nil, fmt.Errorf("store: query records: %w", err)
	}
	if q.Cursor != "" {
		t, id, err := decodeCursor(q.Cursor)
		if err != nil {
			return nil, err
		}
		where = append(where, "(time < ? OR (time = ? AND id < ?))")
		args = append(args, t, t, id)
	}
	query := `SELECT ` + recordCols + ` FROM request_log` + whereSQL(where) + " ORDER BY time DESC, id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := s.r().query(ctx, query, args...)
	if err != nil {
		return nil, wrap("query records", err)
	}
	defer rows.Close()
	page := &RecordPage{Records: []*plugin.Record{}}
	for rows.Next() {
		rec, err := scanRecord(rows, false)
		if err != nil {
			return nil, wrap("query records", err)
		}
		page.Records = append(page.Records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("query records", err)
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		last := page.Records[limit-1]
		page.Next = encodeCursor(last.Time, last.ID)
	}
	return page, nil
}

func (s *db) RecordHistogram(ctx context.Context, q RecordQuery, bucket time.Duration) ([]RecordBucket, error) {
	if bucket < time.Second {
		return nil, fmt.Errorf("store: record histogram: bucket %v under a second", bucket)
	}
	where, args, err := recordWhere(q)
	if err != nil {
		return nil, fmt.Errorf("store: record histogram: %w", err)
	}
	bms := bucket.Milliseconds()
	rows, err := s.r().query(ctx, `SELECT time - time % ? AS b, CAST(COUNT(*) AS BIGINT),
  CAST(SUM(CASE WHEN status < 200 OR status >= 300 THEN 1 ELSE 0 END) AS BIGINT)
FROM request_log`+whereSQL(where)+` GROUP BY b ORDER BY b`, append([]any{bms}, args...)...)
	if err != nil {
		return nil, wrap("record histogram", err)
	}
	defer rows.Close()
	out := []RecordBucket{}
	for rows.Next() {
		var b RecordBucket
		var t int64
		if err := rows.Scan(&t, &b.Requests, &b.Failed); err != nil {
			return nil, wrap("record histogram", err)
		}
		b.Time = fromMs(t)
		out = append(out, b)
	}
	return out, wrap("record histogram", rows.Err())
}

func (s *db) TTFBQuantile(ctx context.Context, q RecordQuery, p float64) (time.Duration, bool, error) {
	where, args, err := recordWhere(q)
	if err != nil {
		return 0, false, fmt.Errorf("store: ttfb quantile: %w", err)
	}
	where = append(where, "ttfb_ms > 0")
	var n int64
	if err := s.r().row(ctx, `SELECT CAST(COUNT(*) AS BIGINT) FROM request_log`+whereSQL(where), args...).Scan(&n); err != nil {
		return 0, false, wrap("ttfb quantile", err)
	}
	if n == 0 {
		return 0, false, nil
	}
	// Nearest rank: the smallest value with at least p of the rows at or below it.
	rank := int64(math.Ceil(p*float64(n))) - 1
	rank = max(0, min(rank, n-1))
	var v int64
	if err := s.r().row(ctx, `SELECT ttfb_ms FROM request_log`+whereSQL(where)+` ORDER BY ttfb_ms LIMIT 1 OFFSET ?`,
		append(args, rank)...).Scan(&v); err != nil {
		return 0, false, wrap("ttfb quantile", err)
	}
	return time.Duration(v) * time.Millisecond, true, nil
}

func (s *db) PruneRecords(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.r().exec(ctx, `DELETE FROM request_log WHERE time < ?`, ms(before))
	if err != nil {
		return 0, wrap("prune records", err)
	}
	n, err := res.RowsAffected()
	return n, wrap("prune records", err)
}

// ---------------------------------------------------------------- usage

var subjectCols = map[string]string{"key": "key_id", "user": "user_id", "group": "group_id"}

func (s *db) Spent(ctx context.Context, subject string, since time.Time) (int64, int64, error) {
	kind, id, ok := strings.Cut(subject, ":")
	col := subjectCols[kind]
	if !ok || col == "" || id == "" {
		return 0, 0, fmt.Errorf("store: spent: subject %q, want key:<id>, user:<id> or group:<id>", subject)
	}
	from := ms(since)
	// Whole hours come from the rollups; the partial first hour from the log.
	edge := (from + hourMs - 1) / hourMs * hourMs
	var charge, reqs int64
	err := s.r().row(ctx, `SELECT CAST(COALESCE(SUM(charge_micro), 0) AS BIGINT), CAST(COALESCE(SUM(requests), 0) AS BIGINT)
FROM usage_hourly WHERE `+col+` = ? AND bucket >= ?`, id, edge).Scan(&charge, &reqs)
	if err != nil {
		return 0, 0, wrap("spent", err)
	}
	if edge > from {
		var c2, r2 int64
		err := s.r().row(ctx, `SELECT CAST(COALESCE(SUM(charge_micro), 0) AS BIGINT), CAST(COUNT(*) AS BIGINT)
FROM request_log WHERE `+col+` = ? AND time >= ? AND time < ?`, id, from, edge).Scan(&c2, &r2)
		if err != nil {
			return 0, 0, wrap("spent", err)
		}
		charge, reqs = charge+c2, reqs+r2
	}
	return charge, reqs, nil
}

var seriesCols = map[string]string{"": "''", "key": "key_id", "user": "user_id", "group": "group_id", "model": "model",
	"upstream": "upstream_id", "client": "client"}

func (s *db) UsageSeries(ctx context.Context, q UsageQuery) ([]UsagePoint, error) {
	series, ok := seriesCols[q.By]
	if !ok {
		return nil, fmt.Errorf("store: usage series: cannot split by %q", q.By)
	}
	bucket := q.Bucket
	if bucket <= 0 {
		bucket = time.Hour
	}
	if bucket%time.Hour != 0 {
		return nil, fmt.Errorf("store: usage series: bucket %v is not a whole number of hours", bucket)
	}
	bms := bucket.Milliseconds()
	to := ms(q.To)
	if q.To.IsZero() {
		to = time.Now().Add(time.Hour).UnixMilli()
	}
	from := ms(q.From) / hourMs * hourMs
	query := `SELECT bucket - bucket % ? AS b, ` + series + ` AS series,
  CAST(SUM(requests) AS BIGINT), CAST(SUM(errors) AS BIGINT), CAST(SUM(input_tokens) AS BIGINT), CAST(SUM(cache_read_tokens) AS BIGINT),
  CAST(SUM(cache_write_5m_tokens) AS BIGINT), CAST(SUM(cache_write_1h_tokens) AS BIGINT), CAST(SUM(output_tokens) AS BIGINT),
  CAST(SUM(reasoning_tokens) AS BIGINT), CAST(SUM(cost_micro) AS BIGINT), CAST(SUM(charge_micro) AS BIGINT)
FROM usage_hourly
WHERE bucket >= ? AND bucket < ?
  AND (? = '' OR key_id = ?) AND (? = '' OR user_id = ?) AND (? = '' OR group_id = ?)
  AND (? = '' OR model = ?) AND (? = '' OR upstream_id = ?) AND (? = '' OR client = ?)
GROUP BY b, series ORDER BY b, series`
	rows, err := s.r().query(ctx, query, bms, from, to,
		q.KeyID, q.KeyID, q.UserID, q.UserID, q.GroupID, q.GroupID, q.Model, q.Model, q.UpstreamID, q.UpstreamID, q.Client, q.Client)
	if err != nil {
		return nil, wrap("usage series", err)
	}
	defer rows.Close()
	var out []UsagePoint
	for rows.Next() {
		var p UsagePoint
		var b int64
		if err := rows.Scan(&b, &p.Series, &p.Requests, &p.Errors, &p.Input, &p.CacheRead, &p.CacheWrite5m, &p.CacheWrite1h,
			&p.Output, &p.Reasoning, &p.CostMicro, &p.ChargeMicro); err != nil {
			return nil, wrap("usage series", err)
		}
		p.Time = fromMs(b)
		if b == 0 {
			p.Time = time.UnixMilli(0).UTC()
		}
		out = append(out, p)
	}
	return out, wrap("usage series", rows.Err())
}
