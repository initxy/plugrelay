package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/plugin"
)

type fakeBatch struct {
	path   string
	caller *plugin.Caller
}

func (f *fakeBatch) BatchFamily(p string) (string, bool) {
	switch {
	case strings.Contains(p, "/messages/batches"):
		return "anthropic", true
	case strings.Contains(p, "/files"), strings.Contains(p, "/batches"):
		return "openai-chat", true
	}
	return "", false
}

func (f *fakeBatch) ServeBatch(w http.ResponseWriter, r *http.Request, caller *plugin.Caller) {
	f.path, f.caller = r.URL.Path, caller
	w.WriteHeader(http.StatusAccepted)
}

func TestBatchDispatch(t *testing.T) {
	s, fp := newTestServer(true)
	fb := &fakeBatch{}
	s.o.Batch = fb
	w := do(s, "GET", "/v1/messages/batches/msgbatch_1/results", "", "x-api-key", "pr_good")
	if w.Code != http.StatusAccepted || fb.caller == nil || fb.caller.KeyID != "k1" || fb.path != "/v1/messages/batches/msgbatch_1/results" {
		t.Fatalf("%d %+v", w.Code, fb)
	}
	w = do(s, "POST", "/v1/messages/batches", "{}")
	if w.Code != http.StatusUnauthorized || gjson.Get(w.Body.String(), "error.type").String() != "authentication_error" {
		t.Fatalf("anthropic auth error: %d %s", w.Code, w.Body)
	}
	w = do(s, "GET", "/v1/batches", "")
	if w.Code != http.StatusUnauthorized || !gjson.Get(w.Body.String(), "error.message").Exists() || gjson.Get(w.Body.String(), "type").Exists() {
		t.Fatalf("openai auth error: %d %s", w.Code, w.Body)
	}
	// Messages themselves still go to the protocol route.
	if w := do(s, "POST", "/v1/messages", `{"model":"m"}`, "x-api-key", "pr_good"); w.Code != http.StatusTeapot || fp.last == nil {
		t.Fatalf("messages: %d", w.Code)
	}
	// A browser navigating to /batches gets the console.
	if w := do(s, "GET", "/batches", "", "Accept", "text/html", "Sec-Fetch-Mode", "navigate"); !strings.Contains(w.Body.String(), "console") {
		t.Fatalf("navigation: %d %s", w.Code, w.Body)
	}
}

func TestRerankRoutes(t *testing.T) {
	s, fp := newTestServer(false)
	for path, codec := range map[string]string{"/v1/rerank": "rerank", "/rerank": "rerank", "/v2/rerank": "cohere-rerank"} {
		fp.last = nil
		if w := do(s, "POST", path, `{"model":"m","query":"q","documents":["a"]}`, "Authorization", "Bearer pr_good"); w.Code != http.StatusTeapot ||
			fp.last == nil || fp.last.codec != codec {
			t.Errorf("%s: %d %+v", path, w.Code, fp.last)
		}
	}
}
