package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/plugins/job/quotaprobe"
	"github.com/initxy/plugrelay/plugins/selector/routing"
)

// End to end against a fake upstream quota endpoint: job.quotaprobe probes
// each credential through the upstream's egress, the readings reach the
// router (so quota_reset orders by them and a used-up credential
// sits out until its reset) and the admin API shows them on credentials.
func TestQuotaProbeEndToEnd(t *testing.T) {
	const token = "admin-test-token"
	t.Setenv(EnvAdminToken, token)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	// Per key: remaining, limit, reset.
	quota := map[string]struct {
		left, limit float64
		reset       time.Time
	}{
		"sk-late":  {60, 100, now.Add(5 * time.Hour)},
		"sk-soon":  {20, 100, now.Add(time.Hour)},
		"sk-spent": {0, 100, now.Add(2 * time.Hour)},
	}
	var (
		mu     sync.Mutex
		probes []string
	)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/quota" {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		probes = append(probes, key)
		mu.Unlock()
		q, ok := quota[key]
		if !ok {
			w.WriteHeader(401)
			return
		}
		_, _ = fmt.Fprintf(w, `{"left":%g,"total":%g,"reset":%d}`, q.left, q.limit, q.reset.Unix())
	}))
	t.Cleanup(fake.Close)

	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.closeAll)
	st := a.Store()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.CreateUser(ctx, &store.User{Name: "admin", Email: "admin", Role: store.RoleAdmin}, "correct-horse-1"))
	must(st.PutManifest(ctx, &store.Manifest{ID: "metered", YAML: `id: metered
kind: provider
interpreter: openai-compatible
variables: {base_url: {default: "http://127.0.0.1:1/v1"}}
auth: {header: Authorization, scheme: Bearer}
endpoints: [{id: chat, protocol: openai-chat, base_url: "{base_url}"}]
quota: {path: /quota, remaining: left, limit: total, reset: reset}
`}))
	for _, id := range []string{"up", "off"} {
		c := map[string]any{"manifest": "metered", "base_url": fake.URL + "/v1", configAllowPrivate: true}
		if id == "off" {
			c["_quota_probe"] = "off"
		}
		must(st.PutUpstream(ctx, &store.Upstream{ID: id, Provider: "provider.openai-compatible", Manifest: "metered", Enabled: true, Config: c}))
	}
	for _, id := range []string{"late", "soon", "spent"} {
		must(st.PutCredential(ctx, &store.Credential{ID: id, UpstreamID: "up", Kind: store.CredShared, Secret: "sk-" + id, Status: store.CredActive}))
	}
	must(st.PutCredential(ctx, &store.Credential{ID: "unprobed", UpstreamID: "off", Kind: store.CredShared, Secret: "sk-late", Status: store.CredActive}))
	must(st.PutModel(ctx, &store.Model{Name: "m", Enabled: true}))
	must(st.ReplaceRoutes(ctx, "m", []store.Route{{Model: "m", UpstreamID: "up", UpstreamModel: "m", Weight: 1}}))
	must(a.Reload(ctx))

	order := func() []string {
		t.Helper()
		p, err := a.router.Plan(ctx, router.Query{Model: "m", ClientProtocol: "openai-chat"})
		must(err)
		var out []string
		for _, c := range routing.Rank(routing.QuotaReset, p.Candidates) {
			out = append(out, c.CredentialID)
		}
		return out
	}
	before := order()
	if len(before) != 3 {
		t.Fatalf("before probing: %v", before)
	}

	j := a.runtime().job(quotaprobe.ID)
	if j == nil {
		t.Fatal("job.quotaprobe not running by default")
	}
	must(j.Run(ctx))
	slices.Sort(probes)
	if !slices.Equal(probes, []string{"sk-late", "sk-soon", "sk-spent"}) {
		t.Fatalf("probed keys %v (the upstream with quota_probe off must be skipped)", probes)
	}
	if got := order(); !slices.Equal(got, []string{"soon", "late"}) {
		t.Fatalf("after probing, quota_reset order = %v, want soon, late (spent sits out)", got)
	}
	// Probing again right away is not due.
	must(j.Run(ctx))
	if len(probes) != 3 {
		t.Fatalf("re-probed before the interval: %v", probes)
	}

	srv := httptest.NewServer(a.Handler().Handler())
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/upstreams/up/credentials", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	must(err)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("list credentials: %d", resp.StatusCode)
	}
	var listed struct {
		Items []struct {
			ID    string `json:"id"`
			State struct {
				Status string     `json:"status"`
				Reason string     `json:"reason"`
				Until  *time.Time `json:"until"`
				Quota  *struct {
					Remaining float64    `json:"remaining"`
					Left      *float64   `json:"left"`
					Limit     *float64   `json:"limit"`
					ResetAt   *time.Time `json:"reset_at"`
					CheckedAt time.Time  `json:"checked_at"`
				} `json:"quota"`
			} `json:"state"`
		}
	}
	must(json.NewDecoder(resp.Body).Decode(&listed))
	seen := 0
	for _, c := range listed.Items {
		q := c.State.Quota
		want := quota["sk-"+c.ID]
		if q == nil || q.Left == nil || *q.Left != want.left || *q.Limit != want.limit || q.ResetAt == nil || !q.ResetAt.Equal(want.reset) ||
			q.Remaining != want.left/want.limit || q.CheckedAt.IsZero() {
			t.Errorf("%s quota shown as %+v", c.ID, q)
		}
		if c.ID == "spent" {
			if c.State.Status != "cooldown" || c.State.Until == nil || !c.State.Until.Equal(want.reset) || c.State.Reason != router.ReasonQuotaProbe {
				t.Errorf("spent credential shown as %+v", c.State)
			}
		} else if c.State.Status != "ok" {
			t.Errorf("%s shown as %s", c.ID, c.State.Status)
		}
		seen++
	}
	if seen != 3 {
		t.Fatalf("listed %d credentials", seen)
	}
}
