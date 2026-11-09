package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// memBatches is an in-memory BatchStore.
type memBatches struct {
	mu      sync.Mutex
	objs    map[string]BatchObject
	checked map[string]time.Time
}

func newMemBatches() *memBatches { return &memBatches{objs: map[string]BatchObject{}} }

func (m *memBatches) PutBatch(_ context.Context, o *BatchObject) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.objs[o.Kind+"/"+o.ID]; ok {
		o.BilledAt = cur.BilledAt
	}
	m.objs[o.Kind+"/"+o.ID] = *o
	return nil
}

func (m *memBatches) GetBatch(_ context.Context, kind, id string) (*BatchObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[kind+"/"+id]
	if !ok {
		return nil, nil
	}
	return &o, nil
}

func (m *memBatches) ListBatches(_ context.Context, q BatchQuery) ([]BatchObject, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []BatchObject
	for _, o := range m.objs {
		mine := o.UserID == q.UserID && (q.UserID != "" || o.KeyID == q.KeyID)
		if o.Kind == q.Kind && mine {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, false, nil
}

func (m *memBatches) SetBatchObject(_ context.Context, kind, id string, obj json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[kind+"/"+id]
	if !ok {
		return errors.New("not found")
	}
	o.Object = obj
	m.objs[kind+"/"+id] = o
	return nil
}

func (m *memBatches) ClaimBatchBilling(_ context.Context, kind, id string, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[kind+"/"+id]
	if !ok || !o.BilledAt.IsZero() {
		return false, nil
	}
	o.BilledAt = at
	m.objs[kind+"/"+id] = o
	return true, nil
}

func (m *memBatches) UnbilledBatches(_ context.Context, checkedBefore, now time.Time, limit int) ([]BatchObject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.checked == nil {
		m.checked = map[string]time.Time{}
	}
	var out []BatchObject
	for k, o := range m.objs {
		if (o.Kind == BatchKindBatch || o.Kind == BatchKindMessageBatch) && o.BilledAt.IsZero() && m.checked[k].Before(checkedBefore) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	for _, o := range out {
		m.checked[o.Kind+"/"+o.ID] = now
	}
	return out, nil
}

func (m *memBatches) DeleteBatch(_ context.Context, kind, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, kind+"/"+id)
	return nil
}

// batchEnv is a test environment whose pipeline serves the batch APIs.
func batchEnv(t *testing.T, model string, ups []upSpec) (*env, *memBatches) {
	t.Helper()
	store := newMemBatches()
	e := newEnv(t, model, ups, func(d *Deps) { d.Batches = store })
	return e, store
}

func (e *env) batch(method, path, contentType string, body []byte) (*http.Response, []byte) {
	e.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := e.caller
		e.p.ServeBatch(w, r, &caller)
	}))
	defer srv.Close()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer pr_client_key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func batchUploadBody(t *testing.T, fileFirst bool, purpose, content string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	field := func() { _ = mw.WriteField("purpose", purpose) }
	if !fileFirst {
		field()
	}
	fw, _ := mw.CreateFormFile("file", "in.jsonl")
	_, _ = io.WriteString(fw, content)
	if fileFirst {
		field()
	}
	_ = mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

// openAIBatchFake answers like the OpenAI files and batches API and keeps
// what was uploaded.
type openAIBatchFake struct {
	mu       sync.Mutex
	uploaded string
	purpose  string
	output   string
}

func (f *openAIBatchFake) handle(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	switch p := r.URL.Path; {
	case r.Method == http.MethodPost && p == "/v1/files":
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			v, _ := io.ReadAll(part)
			f.mu.Lock()
			if part.FormName() == "file" {
				f.uploaded = string(v)
			} else if part.FormName() == "purpose" {
				f.purpose = string(v)
			}
			f.mu.Unlock()
		}
		_, _ = io.WriteString(w, `{"id":"file-in1","object":"file","bytes":10,"created_at":1,"filename":"in.jsonl","purpose":"batch"}`)
	case r.Method == http.MethodPost && p == "/v1/batches":
		_, _ = io.WriteString(w, `{"id":"batch_1","object":"batch","endpoint":"/v1/chat/completions","input_file_id":"file-in1","completion_window":"24h","status":"validating","created_at":1}`)
	case r.Method == http.MethodGet && p == "/v1/batches/batch_1":
		_, _ = io.WriteString(w, `{"id":"batch_1","object":"batch","status":"completed","input_file_id":"file-in1","output_file_id":"file-out1","error_file_id":"file-err1","request_counts":{"total":2,"completed":1,"failed":1}}`)
	case r.Method == http.MethodPost && p == "/v1/batches/batch_1/cancel":
		_, _ = io.WriteString(w, `{"id":"batch_1","object":"batch","status":"cancelling"}`)
	case r.Method == http.MethodGet && p == "/v1/files/file-out1/content":
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, f.output)
	case r.Method == http.MethodGet && p == "/v1/files/file-err1/content":
		_, _ = io.WriteString(w, `{"id":"r2","custom_id":"b","response":{"status_code":500,"body":{"error":{"message":"x"}}}}`+"\n")
	case r.Method == http.MethodDelete && p == "/v1/files/file-in1":
		_, _ = io.WriteString(w, `{"id":"file-in1","object":"file","deleted":true}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"unexpected `+r.Method+` `+p+`"}}`)
	}
}

const batchOutput = `{"id":"r1","custom_id":"a","response":{"status_code":200,"request_id":"q1","body":{"id":"c1","object":"chat.completion","model":"gpt-up","choices":[{"index":0,"message":{"role":"assistant","content":"4"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}},"error":null}
{"id":"r2","custom_id":"b","response":{"status_code":429,"body":{"error":{"message":"rate"}}},"error":null}
{"id":"r3","custom_id":"c","response":{"status_code":200,"body":{"id":"c3","object":"chat.completion","model":"gpt-up","choices":[{"index":0,"message":{"role":"assistant","content":"5"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}}}`

// The OpenAI flow: the file goes to the credential its model routes to,
// the batch and its output stay there even when routing changes, other
// users see nothing, and the output is billed once at the batch discount.
func TestOpenAIBatchLifecycle(t *testing.T) {
	fa, fb := &openAIBatchFake{output: batchOutput}, &openAIBatchFake{output: batchOutput}
	upA, upB := newFake(t, fa.handle), newFake(t, fb.handle)
	e, store := batchEnv(t, "gpt", []upSpec{
		{id: "a", protocol: "openai-chat", issuer: "o", fake: upA, model: "gpt-up", priority: 1, caps: "{batches: true}"},
		{id: "b", protocol: "openai-chat", issuer: "o", fake: upB, caps: "{batches: true}"},
	})
	meter := &fakeMeter{}
	e.chain.Meter = meter
	e.snap.ModelPrice = func(string) (Price, bool) {
		return Price{Input: 1_000_000, Output: 2_000_000, BatchDiscount: 0.5}, true
	}
	lines := `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt","messages":[{"role":"user","content":"2+2"}]}}
{"custom_id":"c","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt","messages":[{"role":"user","content":"2+3"}]}}
`
	body, ct := batchUploadBody(t, true, "batch", lines)
	resp, out := e.batch("POST", "/v1/files", ct, body)
	if resp.StatusCode != 200 || gjson.GetBytes(out, "id").String() != "file-in1" {
		t.Fatalf("upload: %d %s", resp.StatusCode, out)
	}
	if upB.calls() != 0 || upA.headers[0].Get("Authorization") != "Bearer sk-a" {
		t.Fatalf("upload went to b (%d calls) or carried %q", upB.calls(), upA.headers[0].Get("Authorization"))
	}
	if fa.purpose != "batch" || strings.Count(fa.uploaded, `"model":"gpt-up"`) != 2 || strings.Contains(fa.uploaded, `"model":"gpt"`) {
		t.Fatalf("uploaded purpose %q file %s", fa.purpose, fa.uploaded)
	}

	// Routing changes do not move the objects.
	e.onlyRoute("gpt", "b")

	resp, out = e.batch("POST", "/v1/batches", "application/json",
		[]byte(`{"input_file_id":"file-in1","endpoint":"/v1/chat/completions","completion_window":"24h"}`))
	if resp.StatusCode != 200 || gjson.GetBytes(out, "id").String() != "batch_1" {
		t.Fatalf("create: %d %s", resp.StatusCode, out)
	}
	resp, out = e.batch("GET", "/v1/batches/batch_1", "", nil)
	if resp.StatusCode != 200 || gjson.GetBytes(out, "output_file_id").String() != "file-out1" {
		t.Fatalf("get: %d %s", resp.StatusCode, out)
	}
	if o, _ := store.GetBatch(context.Background(), BatchKindFile, "file-out1"); o == nil || o.Role != roleOutput || o.CredentialID != "c1" || o.UpstreamID != "a" {
		t.Fatalf("output file record %+v", o)
	}

	// Another user gets 404 for every object, in the OpenAI format.
	owner := e.caller
	e.caller = plugin.Caller{KeyID: "k2", UserID: "u2"}
	for _, c := range [][2]string{{"GET", "/v1/batches/batch_1"}, {"POST", "/v1/batches/batch_1/cancel"},
		{"GET", "/v1/files/file-out1/content"}, {"GET", "/v1/files/file-in1"}} {
		resp, out := e.batch(c[0], c[1], "", nil)
		if resp.StatusCode != 404 || !gjson.GetBytes(out, "error.message").Exists() {
			t.Errorf("%s %s as another user: %d %s", c[0], c[1], resp.StatusCode, out)
		}
	}
	if resp, out := e.batch("GET", "/v1/batches", "", nil); gjson.GetBytes(out, "data.#").Int() != 0 {
		t.Errorf("other user's list: %d %s", resp.StatusCode, out)
	}
	e.caller = owner
	if resp, out := e.batch("GET", "/v1/batches", "", nil); resp.StatusCode != 200 || gjson.GetBytes(out, "data.0.id").String() != "batch_1" {
		t.Errorf("owner's list: %d %s", resp.StatusCode, out)
	}

	calls := upA.calls()
	resp, out = e.batch("GET", "/v1/files/file-out1/content", "", nil)
	if resp.StatusCode != 200 || string(out) != batchOutput {
		t.Fatalf("content: %d %s", resp.StatusCode, out)
	}
	rec := e.waitRecords(e.recordCount())
	// Two successful lines of 1000 in / 500 out: list 2000 each, halved.
	if rec.ChargeMicro != 2000 || rec.Usage.Input != 2000 || rec.Usage.Output != 1000 || meter.charged != 2000 {
		t.Fatalf("charge %d usage %+v meter %d", rec.ChargeMicro, rec.Usage, meter.charged)
	}
	billed := rec.Attempts[len(rec.Attempts)-1]
	if billed.CostMicro != 2*(1000*3+500*15) || billed.Charge == nil || billed.Charge.BatchDiscount != 0.5 || billed.UpstreamModel != "gpt-up" {
		t.Fatalf("billed attempt %+v charge %+v", billed, billed.Charge)
	}
	// A second download is not billed again; the error file never is.
	_, _ = e.batch("GET", "/v1/files/file-out1/content", "", nil)
	if rec := e.waitRecords(e.recordCount()); rec.ChargeMicro != 0 {
		t.Errorf("second download charged %d", rec.ChargeMicro)
	}
	_, _ = e.batch("GET", "/v1/files/file-err1/content", "", nil)
	if rec := e.waitRecords(e.recordCount()); rec.ChargeMicro != 0 || meter.charged != 2000 {
		t.Errorf("error file charged %d (meter %d)", rec.ChargeMicro, meter.charged)
	}

	resp, out = e.batch("POST", "/v1/batches/batch_1/cancel", "", nil)
	if resp.StatusCode != 200 || gjson.GetBytes(out, "status").String() != "cancelling" {
		t.Errorf("cancel: %d %s", resp.StatusCode, out)
	}
	resp, _ = e.batch("DELETE", "/v1/files/file-in1", "", nil)
	if o, _ := store.GetBatch(context.Background(), BatchKindFile, "file-in1"); resp.StatusCode != 200 || o != nil {
		t.Errorf("delete: %d, record %+v", resp.StatusCode, o)
	}
	if upB.calls() != 0 || upA.calls() != calls+5 {
		t.Errorf("calls a %d (want %d) b %d", upA.calls(), calls+5, upB.calls())
	}
}

func (e *env) recordCount() int {
	e.records.mu.Lock()
	defer e.records.mu.Unlock()
	return len(e.records.recs)
}

// onlyRoute swaps in a routing table where model routes to one upstream.
func (e *env) onlyRoute(model, upstream string) {
	nt := *e.snap.Table
	nt.Models = map[string]router.Model{}
	for k, v := range e.snap.Table.Models {
		nt.Models[k] = v
	}
	m := nt.Models[model]
	var keep []router.Route
	for _, r := range m.Routes {
		if r.UpstreamID == upstream {
			keep = append(keep, r)
		}
	}
	m.Routes = keep
	nt.Models[model] = m
	e.snap.Table = &nt
}

// An upload is checked line by line: one model and one url per batch,
// purpose batch (also when the field follows the file).
func TestOpenAIBatchUploadValidation(t *testing.T) {
	f := &openAIBatchFake{}
	up := newFake(t, f.handle)
	e, _ := batchEnv(t, "gpt", []upSpec{{id: "a", protocol: "openai-chat", issuer: "o", fake: up, caps: "{batches: true}"}})
	line := func(model, url string) string {
		return `{"custom_id":"x","method":"POST","url":"` + url + `","body":{"model":"` + model + `"}}` + "\n"
	}
	cases := []struct {
		name, purpose, content, want string
		fileFirst                    bool
	}{
		{"two models", "batch", line("gpt", "/v1/chat/completions") + line("other", "/v1/chat/completions"), "must use model", false},
		{"two urls", "batch", line("gpt", "/v1/chat/completions") + line("gpt", "/v1/embeddings"), "must use url", false},
		{"not a batch url", "batch", line("gpt", "/v1/images/generations"), "not a batch endpoint", false},
		{"other purpose", "fine-tune", line("gpt", "/v1/chat/completions"), "purpose=batch", false},
		{"purpose after the file", "assistants", line("gpt", "/v1/chat/completions"), "purpose must be", true},
		{"unknown model", "batch", line("nope", "/v1/chat/completions"), "not found", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, ct := batchUploadBody(t, tc.fileFirst, tc.purpose, tc.content)
			resp, out := e.batch("POST", "/v1/files", ct, body)
			if resp.StatusCode < 400 || resp.StatusCode >= 500 || !strings.Contains(string(out), tc.want) {
				t.Fatalf("%d %s", resp.StatusCode, out)
			}
		})
	}
	// Purpose after the file is fine when it is batch.
	body, ct := batchUploadBody(t, true, "batch", line("gpt", "/v1/chat/completions"))
	if resp, out := e.batch("POST", "/v1/files", ct, body); resp.StatusCode != 200 {
		t.Fatalf("purpose last: %d %s", resp.StatusCode, out)
	}
	if f.uploaded != line("gpt", "/v1/chat/completions") {
		t.Errorf("lines not byte-exact: %q", f.uploaded)
	}
	// An endpoint without `batches` is never used.
	e2, _ := batchEnv(t, "gpt", []upSpec{{id: "a", protocol: "openai-chat", issuer: "o", fake: up}})
	resp, out := e2.batch("POST", "/v1/files", ct, body)
	if resp.StatusCode != 400 || !strings.Contains(string(out), "batches") {
		t.Fatalf("no batches endpoint: %d %s", resp.StatusCode, out)
	}
}

const messageBatchResults = `{"custom_id":"a","result":{"type":"succeeded","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-up","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":100,"cache_read_input_tokens":0,"output_tokens":50}}}}
{"custom_id":"b","result":{"type":"errored","error":{"type":"error","error":{"type":"invalid_request_error","message":"x"}}}}
{"custom_id":"c","result":{"type":"expired"}}
`

func anthropicBatchFake(w http.ResponseWriter, r *http.Request, _ []byte) {
	w.Header().Set("Content-Type", "application/json")
	switch p := r.URL.Path; {
	case r.Method == http.MethodPost && p == "/v1/messages/batches":
		_, _ = io.WriteString(w, `{"id":"msgbatch_1","type":"message_batch","processing_status":"in_progress","results_url":null}`)
	case r.Method == http.MethodGet && p == "/v1/messages/batches/msgbatch_1":
		_, _ = io.WriteString(w, `{"id":"msgbatch_1","type":"message_batch","processing_status":"ended","results_url":"https://api.anthropic.com/v1/messages/batches/msgbatch_1/results"}`)
	case r.Method == http.MethodPost && p == "/v1/messages/batches/msgbatch_1/cancel":
		_, _ = io.WriteString(w, `{"id":"msgbatch_1","type":"message_batch","processing_status":"canceling","results_url":null}`)
	case r.Method == http.MethodGet && p == "/v1/messages/batches/msgbatch_1/results":
		w.Header().Set("Content-Type", "application/binary")
		_, _ = io.WriteString(w, messageBatchResults)
	case r.Method == http.MethodDelete && p == "/v1/messages/batches/msgbatch_1":
		_, _ = io.WriteString(w, `{"id":"msgbatch_1","type":"message_batch_deleted"}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"unexpected"}}`)
	}
}

// The Anthropic flow: routed by model and renamed, results_url points at
// the relay, results billed once from the succeeded lines.
func TestAnthropicMessageBatchLifecycle(t *testing.T) {
	up := newFake(t, anthropicBatchFake)
	e, _ := batchEnv(t, "claude", []upSpec{{id: "an", protocol: "anthropic", issuer: "an", fake: up, model: "claude-up", caps: "{batches: true}"}})
	e.snap.ModelPrice = func(string) (Price, bool) {
		return Price{Input: 1_000_000, Output: 10_000_000, BatchDiscount: 0.5}, true
	}
	create := `{"requests":[{"custom_id":"a","params":{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}},{"custom_id":"b","params":{"model":"claude","max_tokens":10,"messages":[]}}]}`
	resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(create))
	if resp.StatusCode != 200 || gjson.GetBytes(out, "id").String() != "msgbatch_1" {
		t.Fatalf("create: %d %s", resp.StatusCode, out)
	}
	sent := gjson.ParseBytes(up.body(0))
	if sent.Get("requests.0.params.model").String() != "claude-up" || sent.Get("requests.1.params.model").String() != "claude-up" ||
		sent.Get("requests.0.custom_id").String() != "a" || up.headers[0].Get("X-Api-Key") != "sk-an" {
		t.Fatalf("upstream got %s (%v)", up.body(0), up.headers[0])
	}
	resp, out = e.batch("GET", "/v1/messages/batches/msgbatch_1", "", nil)
	if resp.StatusCode != 200 || !strings.HasSuffix(gjson.GetBytes(out, "results_url").String(), "/v1/messages/batches/msgbatch_1/results") ||
		strings.Contains(string(out), "api.anthropic.com") {
		t.Fatalf("get: %d %s", resp.StatusCode, out)
	}
	owner := e.caller
	e.caller = plugin.Caller{KeyID: "k3"}
	if resp, out := e.batch("GET", "/messages/batches/msgbatch_1/results", "", nil); resp.StatusCode != 404 ||
		gjson.GetBytes(out, "error.type").String() != "not_found_error" {
		t.Fatalf("other key: %d %s", resp.StatusCode, out)
	}
	e.caller = owner
	resp, out = e.batch("GET", "/v1/messages/batches/msgbatch_1/results", "", nil)
	if resp.StatusCode != 200 || string(out) != messageBatchResults {
		t.Fatalf("results: %d %s", resp.StatusCode, out)
	}
	rec := e.waitRecords(e.recordCount())
	// One succeeded line: (100×1 + 50×10) / 2.
	if rec.ChargeMicro != 300 || rec.Usage.Input != 100 || rec.Usage.Output != 50 || rec.Protocol != BatchProtocol {
		t.Fatalf("charge %d usage %+v protocol %s", rec.ChargeMicro, rec.Usage, rec.Protocol)
	}
	_, _ = e.batch("GET", "/v1/messages/batches/msgbatch_1/results", "", nil)
	if rec := e.waitRecords(e.recordCount()); rec.ChargeMicro != 0 {
		t.Errorf("billed twice: %d", rec.ChargeMicro)
	}
	if resp, out := e.batch("GET", "/v1/messages/batches", "", nil); resp.StatusCode != 200 || gjson.GetBytes(out, "data.0.id").String() != "msgbatch_1" ||
		gjson.GetBytes(out, "first_id").String() != "msgbatch_1" {
		t.Errorf("list: %d %s", resp.StatusCode, out)
	}
	if resp, out := e.batch("POST", "/v1/messages/batches/msgbatch_1/cancel", "", nil); resp.StatusCode != 200 ||
		gjson.GetBytes(out, "processing_status").String() != "canceling" {
		t.Errorf("cancel: %d %s", resp.StatusCode, out)
	}
	if resp, _ := e.batch("DELETE", "/v1/messages/batches/msgbatch_1", "", nil); resp.StatusCode != 200 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
	if resp, _ := e.batch("GET", "/v1/messages/batches/msgbatch_1", "", nil); resp.StatusCode != 404 {
		t.Errorf("after delete: %d", resp.StatusCode)
	}
	// Mixed models are refused before anything is sent.
	calls := up.calls()
	mixed := `{"requests":[{"custom_id":"a","params":{"model":"claude"}},{"custom_id":"b","params":{"model":"other-model"}}]}`
	if resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(mixed)); resp.StatusCode != 400 || up.calls() != calls {
		t.Errorf("mixed models: %d %s", resp.StatusCode, out)
	}
}

// A meter that refuses the caller blocks batch creation.
func TestBatchCreateGatedByMeter(t *testing.T) {
	up := newFake(t, anthropicBatchFake)
	e, store := batchEnv(t, "claude", []upSpec{{id: "an", protocol: "anthropic", issuer: "an", fake: up, caps: "{batches: true}"}})
	e.chain.Meter = &fakeMeter{reserveErr: &ir.GwError{Kind: ir.ErrQuotaExhausted, Message: "balance exhausted"}}
	resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(`{"requests":[{"custom_id":"a","params":{"model":"claude"}}]}`))
	if resp.StatusCode != 429 || up.calls() != 0 || len(store.objs) != 0 {
		t.Fatalf("%d %s (calls %d)", resp.StatusCode, out, up.calls())
	}
}

func TestParseBatchPath(t *testing.T) {
	for path, want := range map[string]string{
		"/v1/files": "files||", "/files/file-1/content": "files|file-1|content", "/v1/batches/b/cancel": "batches|b|cancel",
		"/v1/messages/batches": "messages/batches||", "/messages/batches/m/results": "messages/batches|m|results",
	} {
		r, ok := parseBatchPath(path)
		if got := r.resource + "|" + r.id + "|" + r.action; !ok || got != want {
			t.Errorf("%s: %s %v", path, got, ok)
		}
	}
	for _, bad := range []string{"/v1/files/a/b", "/v1/batches/b/results", "/v1/messages", "/v1/filesx", "/v1/messages/batches/m/content", "/v1/batches/a/b/c"} {
		if _, ok := parseBatchPath(bad); ok {
			t.Errorf("%s accepted", bad)
		}
	}
}
