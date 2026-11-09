package admin

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

func TestDecimal(t *testing.T) {
	for micro, want := range map[int64]string{0: "0", 1_500_000: "1.5", 1: "0.000001", 12_345_678: "12.345678", -2_000_000: "-2"} {
		if got := decimal(micro); got != want {
			t.Errorf("decimal(%d) = %s, want %s", micro, got, want)
		}
	}
}

// The session list looks back 30 days unless the request names a time:
// an older session shows only when from (or to) reaches it.
func TestSessionsDefaultSpan(t *testing.T) {
	e := newEnv(t, sqliteStore, false)
	admin := e.setup()
	now := time.Now().UTC().Truncate(time.Millisecond)
	rec := func(id, sess string, at time.Time) *plugin.Record {
		return &plugin.Record{ID: id, Time: at, Model: "m", Status: 200, SessionID: sess, Client: "codex"}
	}
	if err := e.st.InsertRecords(context.Background(), []*plugin.Record{
		rec("new", "recent", now.Add(-29*24*time.Hour)),
		rec("old", "stale", now.Add(-31*24*time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
	ids := func(query string) []string {
		var out []string
		for _, s := range admin.must(200, "GET", "/api/v1/logs/sessions"+query, nil)["items"].([]any) {
			out = append(out, s.(map[string]any)["id"].(string))
		}
		return out
	}
	if got := ids(""); len(got) != 1 || got[0] != "recent" {
		t.Fatalf("no time filter: %v, want only the session of the last 30 days", got)
	}
	from := url.QueryEscape(now.Add(-40 * 24 * time.Hour).Format(time.RFC3339))
	if got := ids("?from=" + from); len(got) != 2 {
		t.Fatalf("from 40 days ago: %v", got)
	}
	to := url.QueryEscape(now.Add(-30 * 24 * time.Hour).Format(time.RFC3339))
	if got := ids("?to=" + to); len(got) != 1 || got[0] != "stale" {
		t.Fatalf("to 30 days ago: %v", got)
	}
}

// Clients, sessions, the CSV export, the top tables and failures by cause,
// for an admin and for a member who only sees their own requests.
func TestAnalytics(t *testing.T) {
	e := newEnv(t, sqliteStore, false)
	admin := e.setup()
	bob := admin.must(201, "POST", "/api/v1/users", map[string]any{"username": "bob", "role": "member", "password": "bobs-password-1"})
	bobID := bob["id"].(string)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond).Add(-10 * time.Minute)
	var recs []*plugin.Record
	for i := range 4 {
		recs = append(recs, &plugin.Record{ID: fmt.Sprintf("s1-%d", i), Time: now.Add(time.Duration(i) * time.Second),
			Caller: plugin.Caller{KeyID: "k1", KeyName: "laptop", UserID: bobID}, Model: "claude", Status: 200, SessionID: "sess-1",
			ClientUA: "claude-cli/2.1.0 (external, cli)", ClientIP: "10.0.0.8", ChargeMicro: 1_000_000, TTFB: 300 * time.Millisecond,
			Usage:    ir.Usage{Input: 10, CacheRead: 90, Output: 5},
			Attempts: []plugin.AttemptRecord{{N: 1, UpstreamID: "up1", Tier: plugin.TierPassthrough, Status: 200, Sent: true}}})
	}
	recs = append(recs,
		&plugin.Record{ID: "s2-0", Time: now.Add(time.Minute), Caller: plugin.Caller{KeyID: "k2", KeyName: "ci"}, Model: "gpt",
			Status: 429, SessionID: "sess-2", Client: "codex", ClientVersion: "0.50.0",
			Attempts: []plugin.AttemptRecord{{N: 1, UpstreamID: "up1", Status: 429, Sent: true, Cause: plugin.CauseRateLimit}}},
		&plugin.Record{ID: "lone", Time: now.Add(2 * time.Minute), Model: "gpt", Status: 401, ClientUA: "python-requests/2.32"})
	if err := e.st.InsertRecords(ctx, recs); err != nil {
		t.Fatal(err)
	}

	item := admin.must(200, "GET", "/api/v1/logs?client=claude-code", nil)["items"].([]any)
	if len(item) != 4 {
		t.Fatalf("client filter: %v", item)
	}
	first := item[0].(map[string]any)
	if cl := first["client"].(map[string]any); cl["id"] != "claude-code" || cl["name"] != "Claude Code" || cl["version"] != "2.1.0" ||
		first["client_ip"] != "10.0.0.8" {
		t.Fatalf("client: %v", first)
	}
	lone := admin.must(200, "GET", "/api/v1/logs/lone", nil)
	if cl := lone["client"].(map[string]any); cl["id"] != "other" || cl["name"] != nil || lone["session"] != nil {
		t.Fatalf("other client: %v", lone)
	}
	d := admin.must(200, "GET", "/api/v1/logs/s1-1", nil)["session"].(map[string]any)
	if d["requests"] != float64(4) || d["turn"] != float64(2) || d["charge"].(map[string]any)["micro"] != float64(4_000_000) {
		t.Fatalf("session total: %v", d)
	}

	sessions := admin.must(200, "GET", "/api/v1/logs/sessions", nil)["items"].([]any)
	if len(sessions) != 2 {
		t.Fatalf("sessions: %v", sessions)
	}
	s2, s1 := sessions[0].(map[string]any), sessions[1].(map[string]any)
	if s2["id"] != "sess-2" || s2["failed"] != float64(1) || s2["client"].(map[string]any)["id"] != "codex" ||
		s1["requests"] != float64(4) || s1["key"].(map[string]any)["name"] != "laptop" || s1["user"].(map[string]any)["name"] != "bob" ||
		s1["cache_hit_rate"] != 0.9 || s1["charge"].(map[string]any)["micro"] != float64(4_000_000) {
		t.Fatalf("sessions: %v", sessions)
	}
	if failed := admin.must(200, "GET", "/api/v1/logs/sessions?status=error", nil)["items"].([]any); len(failed) != 1 {
		t.Fatalf("failed sessions: %v", failed)
	}

	soon := url.QueryEscape(now.Add(time.Hour).Format(time.RFC3339))
	top := admin.must(200, "GET", "/api/v1/usage/top?range=24h&by=client&to="+soon, nil)["items"].([]any)
	if len(top) != 3 {
		t.Fatalf("top clients: %v", top)
	}
	cc := top[0].(map[string]any)
	if cc["key"] != "claude-code" || cc["label"] != "Claude Code" || cc["spend"].(map[string]any)["micro"] != float64(4_000_000) ||
		cc["ttfb_p95_ms"] != float64(300) || cc["cache_hit_rate"] != 0.9 || cc["tokens"] != float64(420) {
		t.Fatalf("top client row: %v", cc)
	}
	if keys := admin.must(200, "GET", "/api/v1/usage/top?range=24h&by=key&to="+soon, nil)["items"].([]any); len(keys) != 3 {
		t.Fatalf("top keys: %v", keys)
	}
	admin.must(422, "GET", "/api/v1/usage/top?by=group", nil)
	ts := admin.must(200, "GET", "/api/v1/usage/timeseries?range=24h&group_by=client&metric=requests&bucket=hour&to="+soon, nil)
	if ts["bucket_seconds"] != float64(3600) || len(ts["series"].([]any)) != 3 {
		t.Fatalf("by client hourly: %v", ts)
	}
	fs := admin.must(200, "GET", "/api/v1/usage/failures?range=24h&to="+soon, nil)["series"].([]any)
	if len(fs) != 2 || fs[0].(map[string]any)["total"] != float64(1) {
		t.Fatalf("failures: %v", fs)
	}
	causes := map[any]bool{}
	for _, s := range fs {
		causes[s.(map[string]any)["cause"]] = true
	}
	if !causes["rate_limit"] || !causes["not_sent"] {
		t.Fatalf("failure causes: %v", fs)
	}

	// CSV: headers in the asked language, filters honoured, money in units.
	rows := exportCSV(t, admin, "/api/v1/logs/export?client=claude-code&lang=zh-CN", "")
	if len(rows) != 5 || rows[0][0] != "时间（UTC）" || rows[1][1] != "s1-3" || rows[1][2] != "Claude Code" {
		t.Fatalf("csv zh: %v", rows)
	}
	col := map[string]int{}
	for i, h := range exportCSV(t, admin, "/api/v1/logs/export", "en-US,en;q=0.9")[0] {
		col[h] = i
	}
	en := exportCSV(t, admin, "/api/v1/logs/export?status=error", "en")
	if len(en) != 3 || en[1][col["Request ID"]] != "lone" || en[1][col["Client"]] != "Other" ||
		en[1][col["Failure"]] != "not sent upstream" || en[2][col["Failure"]] != "rate limited" {
		t.Fatalf("csv en: %v", en)
	}
	if _, ok := col["Upstream cost"]; !ok {
		t.Fatal("admin export without cost")
	}

	// A member sees their own sessions and no cost column.
	member := e.browser()
	member.must(200, "POST", "/api/v1/auth/login", map[string]any{"username": "bob", "password": "bobs-password-1"})
	if own := member.must(200, "GET", "/api/v1/logs/sessions", nil)["items"].([]any); len(own) != 1 || own[0].(map[string]any)["id"] != "sess-1" {
		t.Fatalf("member sessions: %v", own)
	}
	mrows := exportCSV(t, member, "/api/v1/logs/export?lang=en", "")
	if len(mrows) != 5 || strings.Contains(strings.Join(mrows[0], ","), "cost") {
		t.Fatalf("member csv: %v", mrows)
	}
	if mtop := member.must(200, "GET", "/api/v1/usage/top?range=24h&by=client&to="+soon, nil)["items"].([]any); len(mtop) != 1 {
		t.Fatalf("member top: %v", mtop)
	}
}

func exportCSV(t *testing.T, c *client, path, acceptLanguage string) [][]string {
	t.Helper()
	req, _ := http.NewRequest("GET", c.e.srv.URL+path, nil)
	if acceptLanguage != "" {
		req.Header.Set("Accept-Language", acceptLanguage)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), ".csv") || !strings.HasPrefix(string(body), "\xef\xbb\xbf") {
		t.Fatalf("export %s: %d %v %q", path, resp.StatusCode, resp.Header, body)
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(body), "\xef\xbb\xbf"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Exported cells a client controls cannot run as spreadsheet formulas;
// numbers, negative ones included, stay numbers.
func TestExportEscapesFormulas(t *testing.T) {
	e := newEnv(t, sqliteStore, false)
	admin := e.setup()
	rec := &plugin.Record{ID: "f1", Time: time.Now().UTC().Add(-time.Minute), Model: `=HYPERLINK("http://x","y")`, Status: 200,
		SessionID: "@SUM(A1)", ClientIP: "+1+1", ChargeMicro: -1_500_000}
	if err := e.st.InsertRecords(context.Background(), []*plugin.Record{rec}); err != nil {
		t.Fatal(err)
	}
	rows := exportCSV(t, admin, "/api/v1/logs/export?lang=en", "")
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	r := rows[1]
	if r[col["Model"]] != `'=HYPERLINK("http://x","y")` || r[col["Session"]] != "'@SUM(A1)" || r[col["Source IP"]] != "'+1+1" ||
		r[col["Charged"]] != "-1.5" {
		t.Fatalf("row %v", r)
	}
	for in, want := range map[string]string{"-": "'-", "\tx": "'\tx", "\rx": "'\rx", "-3": "-3", "a=b": "a=b", "": ""} {
		if got := csvCell(in); got != want {
			t.Errorf("csvCell(%q) = %q, want %q", in, got, want)
		}
	}
}
