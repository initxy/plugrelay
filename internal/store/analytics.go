package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

func (s *db) SessionPage(ctx context.Context, q RecordQuery) (*SessionList, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	where, args, err := recordWhere(q)
	if err != nil {
		return nil, fmt.Errorf("store: session page: %w", err)
	}
	where = append(where, "session_id <> ''")
	having, hargs := "", []any{}
	if q.Cursor != "" {
		t, id, err := decodeCursor(q.Cursor)
		if err != nil {
			return nil, err
		}
		having = " HAVING MAX(time) < ? OR (MAX(time) = ? AND session_id < ?)"
		hargs = append(hargs, t, t, id)
	}
	query := `SELECT session_id, MIN(time), MAX(time), CAST(COUNT(*) AS BIGINT),
  CAST(SUM(CASE WHEN ` + failedSQL + ` THEN 1 ELSE 0 END) AS BIGINT),
  MAX(key_id), MAX(key_name), MAX(user_id), MAX(client),
  CAST(SUM(input_tokens + cache_read_tokens + cache_write_5m_tokens + cache_write_1h_tokens) AS BIGINT),
  CAST(SUM(cache_read_tokens) AS BIGINT), CAST(SUM(charge_micro) AS BIGINT), CAST(SUM(cost_micro) AS BIGINT)
FROM request_log` + whereSQL(where) + ` GROUP BY session_id` + having + `
ORDER BY MAX(time) DESC, session_id DESC LIMIT ?`
	rows, err := s.r().query(ctx, query, append(append(args, hargs...), limit+1)...)
	if err != nil {
		return nil, wrap("session page", err)
	}
	out := &SessionList{Sessions: []Session{}}
	for rows.Next() {
		var x Session
		var first, last int64
		if err := rows.Scan(&x.ID, &first, &last, &x.Requests, &x.Failed, &x.KeyID, &x.KeyName, &x.UserID, &x.Client,
			&x.Input, &x.CacheRead, &x.ChargeMicro, &x.CostMicro); err != nil {
			rows.Close()
			return nil, wrap("session page", err)
		}
		x.First, x.Last = fromMs(first), fromMs(last)
		out.Sessions = append(out.Sessions, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, wrap("session page", err)
	}
	if len(out.Sessions) > limit {
		out.Sessions = out.Sessions[:limit]
		last := out.Sessions[limit-1]
		out.Next = encodeCursor(last.Last, last.ID)
	}
	if len(out.Sessions) == 0 {
		return out, nil
	}
	// The models of the page's sessions, most requests first.
	ids := make([]any, len(out.Sessions))
	for i, x := range out.Sessions {
		ids[i] = x.ID
	}
	mw := append(where, "session_id IN (?"+strings.Repeat(", ?", len(ids)-1)+")")
	rows, err = s.r().query(ctx, `SELECT session_id, model, CAST(COUNT(*) AS BIGINT) FROM request_log`+whereSQL(mw)+
		` GROUP BY session_id, model`, append(args, ids...)...)
	if err != nil {
		return nil, wrap("session page", err)
	}
	defer rows.Close()
	type mc struct {
		model string
		n     int64
	}
	models := map[string][]mc{}
	for rows.Next() {
		var id string
		var m mc
		if err := rows.Scan(&id, &m.model, &m.n); err != nil {
			return nil, wrap("session page", err)
		}
		models[id] = append(models[id], m)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("session page", err)
	}
	for i := range out.Sessions {
		ms := models[out.Sessions[i].ID]
		sort.Slice(ms, func(a, b int) bool { return ms[a].n > ms[b].n || (ms[a].n == ms[b].n && ms[a].model < ms[b].model) })
		out.Sessions[i].Models = []string{}
		for _, m := range ms {
			if m.model != "" {
				out.Sessions[i].Models = append(out.Sessions[i].Models, m.model)
			}
		}
	}
	return out, nil
}

func (s *db) SessionTotalAt(ctx context.Context, session, userID string, at time.Time, id string) (SessionTotal, error) {
	var tot SessionTotal
	if session == "" {
		return tot, nil
	}
	t := ms(at)
	var first, last int64
	err := s.r().row(ctx, `SELECT CAST(COUNT(*) AS BIGINT),
  CAST(COALESCE(SUM(CASE WHEN time < ? OR (time = ? AND id <= ?) THEN 1 ELSE 0 END), 0) AS BIGINT),
  CAST(COALESCE(SUM(charge_micro), 0) AS BIGINT), CAST(COALESCE(MIN(time), 0) AS BIGINT), CAST(COALESCE(MAX(time), 0) AS BIGINT)
FROM request_log WHERE session_id = ? AND (? = '' OR user_id = ?)`, t, t, id, session, userID, userID).
		Scan(&tot.Requests, &tot.Turn, &tot.ChargeMicro, &first, &last)
	if err != nil {
		return tot, wrap("session total", err)
	}
	tot.First, tot.Last = fromMs(first), fromMs(last)
	return tot, nil
}

func (s *db) FailureSeries(ctx context.Context, q RecordQuery, bucket time.Duration) ([]CausePoint, error) {
	if bucket < time.Second {
		return nil, fmt.Errorf("store: failure series: bucket %v under a second", bucket)
	}
	where, args, err := recordWhere(q)
	if err != nil {
		return nil, fmt.Errorf("store: failure series: %w", err)
	}
	where = append(where, failedSQL)
	rows, err := s.r().query(ctx, `SELECT time - time % ? AS b, `+causeSQL+` AS c, CAST(COUNT(*) AS BIGINT)
FROM request_log`+whereSQL(where)+` GROUP BY b, c ORDER BY b, c`, append([]any{bucket.Milliseconds()}, args...)...)
	if err != nil {
		return nil, wrap("failure series", err)
	}
	defer rows.Close()
	out := []CausePoint{}
	for rows.Next() {
		var p CausePoint
		var t int64
		if err := rows.Scan(&t, &p.Cause, &p.Failed); err != nil {
			return nil, wrap("failure series", err)
		}
		p.Time = fromMs(t)
		out = append(out, p)
	}
	return out, wrap("failure series", rows.Err())
}
