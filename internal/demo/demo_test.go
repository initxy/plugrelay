package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/app"
	"github.com/initxy/plugrelay/internal/store"
)

// TestSeed seeds a temporary store and checks that the console would have
// data on every main page and that the demo key really gets answers.
func TestSeed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := t.TempDir()
	fake := httptest.NewServer(fakeUpstream())
	t.Cleanup(fake.Close)
	cfg, err := demoConfig(dir, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.New(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Seed(ctx, a, dir, fake.URL, 7)
	if err != nil {
		t.Fatal(err)
	}
	st := a.Store()
	count := func(name string, n int, err error, want int) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n != want {
			t.Errorf("%s = %d, want %d", name, n, want)
		}
	}
	us, err := st.ListUsers(ctx)
	// Three more signed up through Alice's invite link.
	count("users", len(us), err, len(userSpecs)+3)
	gs, err := st.ListGroups(ctx)
	count("groups", len(gs), err, len(groupSpecs))
	ups, err := st.ListUpstreams(ctx)
	count("upstreams", len(ups), err, len(upstreamSpecs))
	cs, err := st.ListCredentials(ctx, "")
	count("credentials", len(cs), err, 9)
	ms, err := st.ListModels(ctx)
	count("models", len(ms), err, len(modelSpecs))
	ps, err := st.ListPresets(ctx)
	count("presets", len(ps), err, len(presetSpecs))
	ks, err := st.ListKeys(ctx, store.KeyFilter{})
	count("keys", len(ks), err, len(keySpecs))
	prices, err := st.ListPrices(ctx)
	count("price rules", len(prices), err, len(modelSpecs))
	page, err := st.QueryRecords(ctx, store.RecordQuery{From: time.Now().Add(-8 * 24 * time.Hour), Limit: 500})
	if err != nil || len(page.Records) < 500 {
		t.Errorf("request log: %d records (err %v), want at least 500", len(page.Records), err)
	}
	audit, err := st.ListAudit(ctx, store.AuditQuery{Limit: 500})
	if err != nil || len(audit) < 30 {
		t.Errorf("audit log: %d entries (err %v), want at least 30", len(audit), err)
	}
	// Seeded entries name their target as the admin API does.
	for _, e := range audit {
		if e.Action == "settings.update" && e.Target != "settings:site" {
			t.Errorf("settings.update targets %q, want settings:site", e.Target)
		}
	}

	srv := httptest.NewServer(a.Handler().Handler())
	t.Cleanup(srv.Close)
	c := newAPI(srv.URL)
	if err := c.login(userSpecs[0].email, password); err != nil {
		t.Fatal(err)
	}
	var ov struct {
		Requests int64 `json:"requests"`
		Spend    struct {
			Micro int64 `json:"micro"`
		} `json:"spend"`
		SpendPrev struct {
			Micro int64 `json:"micro"`
		} `json:"spend_prev"`
		CacheHitRate float64 `json:"cache_hit_rate"`
		SuccessRate  float64 `json:"success_rate"`
	}
	if err := c.v1("GET", "/overview?range=7d", nil, &ov); err != nil {
		t.Fatal(err)
	}
	if ov.Requests == 0 || ov.Spend.Micro == 0 || ov.CacheHitRate == 0 || ov.SuccessRate >= 1 {
		t.Errorf("overview = %+v, want non-zero requests, spend, cache hits and some failures", ov)
	}
	// The previous 7 days lie before the seed; the previous 24 hours lie
	// inside it at any time of day.
	if err := c.v1("GET", "/overview?range=24h", nil, &ov); err != nil {
		t.Fatal(err)
	}
	if ov.Spend.Micro == 0 || ov.SpendPrev.Micro == 0 {
		t.Errorf("24h overview = %+v, want non-zero spend and previous spend", ov)
	}

	var upv struct {
		Items []struct {
			ID      string `json:"id"`
			Circuit struct {
				State string `json:"state"`
			} `json:"circuit"`
			Credentials []struct {
				Label string `json:"label"`
				State struct {
					Status string `json:"status"`
				} `json:"state"`
			} `json:"credentials"`
		} `json:"items"`
	}
	if err := c.v1("GET", "/upstreams", nil, &upv); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, u := range upv.Items {
		seen["circuit:"+u.Circuit.State] = true
		for _, cr := range u.Credentials {
			seen[cr.State.Status] = true
		}
	}
	for _, want := range []string{"ok", "cooldown", "quarantined", "circuit_open", "circuit:open"} {
		if !seen[want] {
			t.Errorf("no credential or upstream shown as %s; saw %v", want, seen)
		}
	}

	var bal struct {
		Items []struct {
			Subject string `json:"subject"`
		} `json:"items"`
	}
	// The invitees who paid have an account too.
	if err := c.plugin("meter.balance", "GET", "/accounts", nil, &bal); err != nil || len(bal.Items) != len(userSpecs)+2 {
		t.Errorf("balances: %d accounts (err %v), want %d", len(bal.Items), err, len(userSpecs)+2)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	statuses := map[any]int{}
	if err := c.plugin("admin.payment", "GET", "/orders", nil, &list); err != nil {
		t.Fatal(err)
	}
	for _, o := range list.Items {
		statuses[o["status"]]++
	}
	if statuses["paid"] != 5 || statuses["pending"] != 1 || statuses["canceled"] != 1 {
		t.Errorf("orders by status %v", statuses)
	}
	for path, want := range map[string]int{"/api/plugins/admin.payment/coupons": 4, "/api/plugins/admin.announcements/announcements": 4,
		"/api/plugins/admin.rewards/referrals": 3, "/api/plugins/admin.redeem/batches": 2} {
		if err := c.do("GET", path, nil, &list); err != nil || len(list.Items) != want {
			t.Errorf("%s: %d items (err %v), want %d", path, len(list.Items), err, want)
		}
	}
	var txn struct {
		Items []struct {
			Kind string `json:"kind"`
		} `json:"items"`
	}
	kinds := map[string]bool{}
	if err := c.plugin("meter.balance", "GET", "/transactions?limit=500", nil, &txn); err != nil {
		t.Fatal(err)
	}
	for _, x := range txn.Items {
		kinds[x.Kind] = true
	}
	for _, k := range []string{"topup", "redeem", "adjustment", "checkin", "referral"} {
		if !kinds[k] {
			t.Errorf("no %s transaction in the ledger: %v", k, kinds)
		}
	}

	for _, body := range []string{
		`{"model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"claude-haiku-4-5","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`,
	} {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", bytes.NewBufferString(body))
		req.Header.Set("x-api-key", s.Key)
		req.Header.Set("content-type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Contains(buf.Bytes(), []byte("demo upstream")) {
			t.Errorf("demo key request: %d %s", resp.StatusCode, buf.String())
		}
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		body := map[string]any{"model": "gpt-5.4", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
		if path == "/v1/responses" {
			body = map[string]any{"model": "gpt-5.3-codex", "input": "hi", "stream": true}
		}
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+s.Key)
		req.Header.Set("content-type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !bytes.Contains(buf.Bytes(), []byte("demo upstream")) {
			t.Errorf("%s: %d %s", path, resp.StatusCode, buf.String())
		}
	}
}

// TestRecordsDeterministic: the same seed draws the same history.
func TestRecordsDeterministic(t *testing.T) {
	w := &world{groups: map[string]string{"default": "g1", "vip": "g2"}, users: map[string]string{}, upstreams: map[string]string{},
		creds: map[string]string{}}
	for _, k := range keySpecs {
		w.keys = append(w.keys, seededKey{spec: k, id: k.name})
	}
	now := time.Date(2026, 11, 7, 12, 30, 0, 0, time.UTC)
	a, b := genRecords(w, 3, now), genRecords(w, 3, now)
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("%d vs %d records", len(a), len(b))
	}
	for i := range a {
		ja, _ := json.Marshal(a[i])
		jb, _ := json.Marshal(b[i])
		if !bytes.Equal(ja, jb) {
			t.Fatalf("record %d differs", i)
		}
	}
}
