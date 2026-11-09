//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRerankAndBatches checks /v1/rerank and the OpenAI batch API through
// the relay. Each part first asks the upstream whether it serves the API
// and skips, saying so, when it does not; when neither is served no relay
// is built.
//
//	PLUGRELAY_E2E_RERANK_MODEL  rerank model (default bge-reranker-v2-m3)
//	PLUGRELAY_E2E_BATCH_WAIT    how long to wait for a batch to finish
//	                            (default 0: create, get and cancel only)
func TestRerankAndBatches(t *testing.T) {
	upBase, upKey := os.Getenv("PLUGRELAY_E2E_BASE"), os.Getenv("PLUGRELAY_E2E_KEY")
	if upBase == "" || upKey == "" {
		t.Skip("set PLUGRELAY_E2E_BASE and PLUGRELAY_E2E_KEY to run against a live upstream")
	}
	up := upstream{base: upBase, key: upKey}
	rerank, batches := up.serves(t, http.MethodPost, "/rerank"), up.serves(t, http.MethodGet, "/batches")
	if !rerank && !batches {
		t.Skip("the upstream serves neither /rerank nor /batches (404); not checked live")
	}
	model := envOr("PLUGRELAY_E2E_MODEL", defaultModel)
	rerankModel := envOr("PLUGRELAY_E2E_RERANK_MODEL", "bge-reranker-v2-m3")
	r := start(t, upBase, model, rerankModel)

	t.Run("rerank", func(t *testing.T) {
		if !rerank {
			t.Skip("the upstream does not serve /rerank (404); not checked live")
		}
		var out struct {
			Results []struct {
				Index int     `json:"index"`
				Score float64 `json:"relevance_score"`
			} `json:"results"`
		}
		decode(t, r.post(t, "/v1/rerank", map[string]any{"model": rerankModel, "query": "capital of France",
			"documents": []string{"Berlin is in Germany.", "Paris is the capital of France."}, "top_n": 1}, nil), &out)
		if len(out.Results) == 0 || out.Results[0].Index != 1 {
			t.Fatalf("results %+v", out.Results)
		}
	})

	t.Run("openai batch", func(t *testing.T) {
		if !batches {
			t.Skip("the upstream does not serve /batches (404); not checked live")
		}
		// The generic manifest does not declare batches; a stored copy
		// that does is added as a second upstream for the model.
		doc := filepath.Join(t.TempDir(), "batch.yaml")
		yml := "id: e2e-batch\nkind: provider\ninterpreter: openai-compatible\nvariables:\n  base_url: {default: \"" + upBase + "\"}\n" +
			"auth: {header: Authorization, scheme: Bearer}\nendpoints:\n  - {id: chat, protocol: openai-chat, base_url: \"{base_url}\", capabilities: {batches: true}}\n"
		b, _ := json.Marshal(map[string]any{"manifests": []map[string]string{{"id": "e2e-batch", "yaml": yml}}})
		_ = os.WriteFile(doc, b, 0o600)
		r.run("apply", "-f", doc)
		r.run("upstream", "add", "--manifest", "e2e-batch", "--id", "e2e-batch", "--credential-env", "PLUGRELAY_E2E_KEY")
		r.run("model", "add", "--name", "batch-"+model, "--route", "e2e-batch:"+model)
		time.Sleep(3 * time.Second) // the next reload picks the upstream up

		line := `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"batch-` + model +
			`","messages":[{"role":"user","content":"` + prompt + `"}]}}` + "\n"
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "in.jsonl")
		_, _ = io.WriteString(fw, line)
		_ = mw.WriteField("purpose", "batch")
		_ = mw.Close()
		var file struct{ ID string }
		r.call(t, http.MethodPost, "/v1/files", mw.FormDataContentType(), buf.Bytes(), &file)
		var batch struct {
			ID string `json:"id"`
		}
		r.call(t, http.MethodPost, "/v1/batches", "application/json",
			[]byte(`{"input_file_id":"`+file.ID+`","endpoint":"/v1/chat/completions","completion_window":"24h"}`), &batch)
		var got map[string]any
		r.call(t, http.MethodGet, "/v1/batches/"+batch.ID, "", nil, &got)
		if got["id"] != batch.ID {
			t.Fatalf("get %+v", got)
		}
		wait, _ := time.ParseDuration(os.Getenv("PLUGRELAY_E2E_BATCH_WAIT"))
		deadline := time.Now().Add(wait)
		for time.Now().Before(deadline) && got["status"] != "completed" {
			time.Sleep(10 * time.Second)
			r.call(t, http.MethodGet, "/v1/batches/"+batch.ID, "", nil, &got)
		}
		if out, _ := got["output_file_id"].(string); got["status"] == "completed" && out != "" {
			req, _ := http.NewRequest(http.MethodGet, r.base+"/v1/files/"+out+"/content", nil)
			req.Header.Set("Authorization", "Bearer "+r.key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("results: %v %v", err, resp)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var first struct {
				CustomID string `json:"custom_id"`
			}
			line, _, _ := bytes.Cut(body, []byte("\n"))
			if json.Unmarshal(line, &first) != nil || first.CustomID != "a" {
				t.Fatalf("results %s", body)
			}
			return
		}
		r.call(t, http.MethodPost, "/v1/batches/"+batch.ID+"/cancel", "", nil, &got)
	})
}

// serves reports whether the upstream answers path with anything but 404
// or 405.
func (u upstream) serves(t *testing.T, method, path string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader("{}")
	}
	req, _ := http.NewRequestWithContext(ctx, method, strings.TrimRight(u.base, "/")+path, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+u.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed
}

// call sends a request with the platform key, fails on a non-200 and
// decodes the body into v.
func (r *relay) call(t *testing.T, method, path, contentType string, body []byte, v any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, r.base+path, rd)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+r.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		t.Fatalf("%s %s: status %d: %s", method, path, resp.StatusCode, msg)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}
