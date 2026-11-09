package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/job/notify"
)

// End to end: with the feature on, a quarantined credential (router live
// state) and a budget notice (raised like the meter does) reach a webhook
// channel through the core egress, and the log keeps each delivery.
// With the feature off nothing runs and notices are dropped.
func TestNotificationsEndToEnd(t *testing.T) {
	ctx := context.Background()
	var (
		mu     sync.Mutex
		bodies []map[string]any
	)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
	}))
	t.Cleanup(hook.Close)

	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	cfg.Egress.AllowPrivateCIDRs = []string{"127.0.0.1/32"}
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
	must(st.PutUpstream(ctx, &store.Upstream{ID: "up", Provider: "provider.openai-compatible", Enabled: true,
		Config: map[string]any{"_name": "Main", "manifest": "vllm", "base_url": "http://127.0.0.1:1/v1", configAllowPrivate: true}}))
	must(st.PutCredential(ctx, &store.Credential{ID: "c1", UpstreamID: "up", Kind: store.CredShared, Secret: "sk", Status: store.CredActive,
		Meta: map[string]string{"label": "Team key"}}))
	must(st.PutModel(ctx, &store.Model{Name: "m", Enabled: true}))
	must(st.ReplaceRoutes(ctx, "m", []store.Route{{Model: "m", UpstreamID: "up", UpstreamModel: "m", Weight: 1}}))
	ns, _ := store.NotifyOf(st)
	must(ns.PutNotifyChannel(ctx, &store.NotifyChannel{Name: "hook", Type: store.NotifyWebhook, Enabled: true, Language: "en",
		Events: []string{}, Secrets: store.NotifySecrets{URL: hook.URL}}))
	must(a.Reload(ctx))
	if a.notifier() != nil {
		t.Fatal("the notifier runs while the feature is off")
	}
	a.notice(ctx, plugin.Notice{Event: notify.EventBudget}) // dropped, no panic

	must(st.PutFeature(ctx, store.FeatureState{ID: "notifications", Enabled: true}))
	must(a.Reload(ctx))
	nt := a.notifier()
	if nt == nil {
		t.Fatal("the notifier is not running with the feature on")
	}
	must(a.router.Report(ctx, plugin.Candidate{UpstreamID: "up", CredentialID: "c1"},
		plugin.Verdict{Cause: plugin.CauseBanned, Credential: plugin.CredQuarantine}, "m"))
	a.notice(ctx, plugin.Notice{Event: notify.EventBudget, Object: plugin.NoticeObject{Type: "key", ID: "k1"},
		Key: "spend:monthly:2026-11-01", Detail: map[string]string{"percent": "100", "window": "monthly", "kind": "spend"}})
	deadline := time.Now().Add(3 * time.Second)
	for {
		p, err := ns.PendingNotifications(ctx)
		must(err)
		if len(p) == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	must(nt.Run(ctx))

	mu.Lock()
	defer mu.Unlock()
	var titles []string
	for _, b := range bodies {
		titles = append(titles, b["title"].(string))
	}
	joined := strings.Join(titles, "\n")
	if len(bodies) != 2 || !strings.Contains(joined, "looks suspended") || !strings.Contains(joined, "budget") {
		t.Fatalf("webhook got %q", titles)
	}
	log, err := ns.ListNotifications(ctx, 10)
	must(err)
	if len(log) != 2 {
		t.Fatalf("%+v", log)
	}
	for _, e := range log {
		if e.Status != store.NotificationDone || len(e.Deliveries) != 1 || !e.Deliveries[0].OK {
			t.Fatalf("%+v", e)
		}
		if e.Event == notify.EventQuarantined && (e.Objects[0].Name != "Team key" || e.Objects[0].ParentName != "Main") {
			t.Fatalf("%+v", e.Objects)
		}
	}
}
