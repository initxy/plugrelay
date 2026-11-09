package app

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// The playground sends a real relay request in every client protocol: the
// answer streams back as text, and the request log has the request, marked
// by the playground's User-Agent.
func TestPlaygroundEveryProtocol(t *testing.T) {
	ctx := context.Background()
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	st := a.Store()
	up := fakeChat(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.PutUpstream(ctx, &store.Upstream{ID: "fake", Provider: "provider.openai-compatible", Enabled: true,
		Config: map[string]any{"manifest": chatOnlyManifest(), "base_url": up.URL + "/v1", configAllowPrivate: true}}))
	must(st.PutCredential(ctx, &store.Credential{ID: "c1", UpstreamID: "fake", Kind: store.CredShared, Secret: "sk-upstream",
		Status: store.CredActive}))
	must(st.PutModel(ctx, &store.Model{Name: "m", Enabled: true}))
	must(st.ReplaceRoutes(ctx, "m", []store.Route{{Model: "m", UpstreamID: "fake", UpstreamModel: "fake-model", Weight: 1}}))
	k := &store.Key{Name: "it"}
	_, err = st.CreateKey(ctx, k)
	must(err)
	must(a.Reload(ctx))

	temp := 0.2
	for _, proto := range []string{"anthropic", "openai-chat", "openai-responses", "gemini"} {
		t.Run(proto, func(t *testing.T) {
			var text strings.Builder
			var start, done admin.PlaygroundEvent
			a.playground(ctx, k.Caller(), admin.PlaygroundRequest{Protocol: proto, Model: "m", System: "be brief",
				Messages: []admin.PlaygroundMessage{{Role: "user", Content: "hi"}}, Temperature: &temp, MaxTokens: 64, Effort: "low"},
				func(ev admin.PlaygroundEvent) {
					switch ev.Type {
					case "start":
						start = ev
					case "text":
						text.WriteString(ev.Text)
					case "done":
						done = ev
					}
				})
			if !done.OK || done.Status != 200 || text.String() != "Hello" || done.TTFBMs <= 0 || done.RequestID == "" ||
				start.RequestID != done.RequestID {
				t.Fatalf("done %+v text %q", done, text.String())
			}
			if done.Usage == nil || done.Usage.Output != 2 {
				t.Fatalf("usage %+v", done.Usage)
			}
			var rec *plugin.Record
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && rec == nil; time.Sleep(20 * time.Millisecond) {
				rec, _ = st.GetRecord(ctx, done.RequestID)
			}
			if rec == nil || rec.ClientUA != admin.PlaygroundUserAgent || rec.Protocol != proto || rec.Caller.KeyID != k.ID {
				t.Fatalf("record %+v", rec)
			}
		})
	}

	t.Run("unknown model", func(t *testing.T) {
		var done admin.PlaygroundEvent
		a.playground(ctx, k.Caller(), admin.PlaygroundRequest{Protocol: "anthropic", Model: "nope",
			Messages: []admin.PlaygroundMessage{{Role: "user", Content: "hi"}}}, func(ev admin.PlaygroundEvent) {
			if ev.Type == "done" {
				done = ev
			}
		})
		if done.OK || done.Status != 404 || done.Error == "" {
			t.Fatalf("done %+v", done)
		}
	})
}
