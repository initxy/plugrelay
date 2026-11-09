package pipeline

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/middleware/guardrails"
	"github.com/initxy/plugrelay/plugins/middleware/parampolicy"
	"github.com/initxy/plugrelay/plugins/middleware/redact"
)

// pluginSettings serves settings per plugin id.
func pluginSettings(set map[string]map[string]any) func(*plugin.Caller, string, string) plugin.Value {
	return func(_ *plugin.Caller, id, key string) plugin.Value {
		if v, ok := set[id][key]; ok {
			b, _ := json.Marshal(v)
			return plugin.NewValue(b)
		}
		return plugin.Value{}
	}
}

// The body mutators of the real redaction, guardrails (redact mode) and
// parameter-policy plugins edit every request of a batch before it goes
// upstream, on both batch APIs.
func TestBatchAppliesBodyMutators(t *testing.T) {
	for _, tc := range []struct {
		name string
		pl   plugin.Plugin
		set  map[string]map[string]any
		// check inspects one request body as the upstream got it.
		check func(body gjson.Result) bool
	}{
		{"guardrails", guardrails.New(), map[string]map[string]any{guardrails.ID: {"detect": []string{"email"}, "action": "redact"}},
			func(b gjson.Result) bool {
				return !strings.Contains(b.Raw, "a.b@example.com") && strings.Contains(b.Raw, "[REDACTED:email]")
			}},
		{"redact", redact.New(), map[string]map[string]any{redact.ID: {"detect": []string{"email"}}},
			func(b gjson.Result) bool {
				return !strings.Contains(b.Raw, "a.b@example.com") && strings.Contains(b.Raw, "[REDACTED:email]")
			}},
		{"param-policy", parampolicy.New(), map[string]map[string]any{parampolicy.ID: {"max_output_tokens": 50}},
			func(b gjson.Result) bool {
				return b.Get("max_tokens").Int() == 50 || b.Get("max_completion_tokens").Int() == 50
			}},
	} {
		t.Run(tc.name+"/anthropic", func(t *testing.T) {
			f := &messageBatchFake{}
			up := newFake(t, f.handle)
			e, _ := batchEnvMod(t, up, func(d *Deps) { d.Settings = pluginSettings(tc.set) })
			e.chain.Plugins = []plugin.Plugin{tc.pl}
			in := strings.Replace(createTwo, `"yo"`, `"mail me at a.b@example.com"`, 1)
			in = strings.ReplaceAll(in, `"max_tokens":100`, `"max_tokens":1000`)
			resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(in))
			if resp.StatusCode != http.StatusOK || up.calls() != 1 {
				t.Fatalf("create: %d %s", resp.StatusCode, out)
			}
			got := gjson.GetBytes(up.body(0), "requests")
			if n := len(got.Array()); n != 2 || got.Get("0.custom_id").String() != "a" {
				t.Fatalf("requests sent: %s", got.Raw)
			}
			if p := got.Get("1.params"); !tc.check(p) || p.Get("model").String() != "claude" {
				t.Fatalf("request sent as %s", p.Raw)
			}
		})
		t.Run(tc.name+"/openai", func(t *testing.T) {
			fo := &openAIBatchFake{}
			upo := newFake(t, fo.handle)
			store := newMemBatches()
			eo := newEnv(t, "gpt", []upSpec{{id: "a", protocol: "openai-chat", issuer: "o", fake: upo, model: "gpt-up", caps: "{batches: true}"}},
				func(d *Deps) { d.Batches, d.Settings = store, pluginSettings(tc.set) })
			eo.chain.Plugins = []plugin.Plugin{tc.pl}
			lines := `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt","max_completion_tokens":1000,"messages":[{"role":"user","content":"ok"}]}}
{"custom_id":"b","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt","max_completion_tokens":1000,"messages":[{"role":"user","content":"mail me at a.b@example.com"}]}}
`
			body, ct := batchUploadBody(t, true, "batch", lines)
			if resp, out := eo.batch("POST", "/v1/files", ct, body); resp.StatusCode != http.StatusOK {
				t.Fatalf("upload: %d %s", resp.StatusCode, out)
			}
			fo.mu.Lock()
			sent := strings.Split(strings.TrimSpace(fo.uploaded), "\n")
			fo.mu.Unlock()
			if len(sent) != 2 || gjson.Get(sent[0], "custom_id").String() != "a" {
				t.Fatalf("lines sent: %q", sent)
			}
			if b := gjson.Get(sent[1], "body"); !tc.check(b) || b.Get("model").String() != "gpt-up" {
				t.Fatalf("line sent as %s", sent[1])
			}
		})
	}
}

func batchEnvMod(t *testing.T, up *fakeUpstream, mod func(*Deps)) (*env, *memBatches) {
	store := newMemBatches()
	e := newEnv(t, "claude", []upSpec{{id: "an", protocol: "anthropic", issuer: "an", fake: up, caps: "{batches: true}"}}, func(d *Deps) { d.Batches = store }, mod)
	return e, store
}
