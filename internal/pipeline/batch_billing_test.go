package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/meter/balance"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger/ledgertest"
)

// messageBatchFake answers like Anthropic's message batches; status is
// what a GET reports, results the results file.
type messageBatchFake struct {
	mu        sync.Mutex
	status    string
	results   string
	ranges    []string
	resultsN  int
	cancelled bool
}

func (f *messageBatchFake) handle(w http.ResponseWriter, r *http.Request, _ []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch p := r.URL.Path; {
	case r.Method == http.MethodPost && p == "/v1/messages/batches":
		_, _ = io.WriteString(w, `{"id":"msgbatch_1","type":"message_batch","processing_status":"in_progress","results_url":null}`)
	case r.Method == http.MethodGet && p == "/v1/messages/batches/msgbatch_1":
		_, _ = io.WriteString(w, f.status)
	case r.Method == http.MethodPost && p == "/v1/messages/batches/msgbatch_1/cancel":
		f.cancelled = true
		_, _ = io.WriteString(w, `{"id":"msgbatch_1","type":"message_batch","processing_status":"canceling"}`)
	case r.Method == http.MethodGet && p == "/v1/messages/batches/msgbatch_1/results":
		f.resultsN++
		f.ranges = append(f.ranges, r.Header.Get("Range")+"|"+r.Header.Get("If-Range"))
		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, f.results[:20])
			return
		}
		_, _ = io.WriteString(w, f.results)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"unexpected"}}`)
	}
}

const (
	endedWithResults = `{"id":"msgbatch_1","type":"message_batch","processing_status":"ended","ended_at":"2020-01-01T00:00:00Z",` +
		`"request_counts":{"succeeded":1,"errored":1,"expired":1},"results_url":"https://api.anthropic.com/v1/messages/batches/msgbatch_1/results"}`
	endedEmpty = `{"id":"msgbatch_1","type":"message_batch","processing_status":"ended","ended_at":"2020-01-01T00:00:00Z",` +
		`"request_counts":{"succeeded":0,"canceled":2},"results_url":"https://api.anthropic.com/v1/messages/batches/msgbatch_1/results"}`
	// Two requests of up to 100 output tokens each.
	createTwo = `{"requests":[{"custom_id":"a","params":{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}},` +
		`{"custom_id":"b","params":{"model":"claude","max_tokens":100,"messages":[{"role":"user","content":"yo"}]}}]}`
)

// balanceBatchEnv is a message-batch environment billed by meter.balance
// over an in-memory ledger: list price 1/10 per input/output token, half
// off for batches.
func balanceBatchEnv(t *testing.T, f *messageBatchFake, mod ...func(*Deps)) (*env, *memBatches, *ledgertest.Memory) {
	t.Helper()
	up := newFake(t, f.handle)
	store := newMemBatches()
	mods := append([]func(*Deps){func(d *Deps) { d.Batches = store }}, mod...)
	e := newEnv(t, "claude", []upSpec{{id: "an", protocol: "anthropic", issuer: "an", fake: up, model: "claude-up", caps: "{batches: true}"}}, mods...)
	e.snap.ModelPrice = func(string) (Price, bool) {
		return Price{Input: 1_000_000, Output: 10_000_000, BatchDiscount: 0.5}, true
	}
	l := ledgertest.New()
	e.chain.Meter = balance.New(l)
	return e, store, l
}

func topup(t *testing.T, l *ledgertest.Memory, subject string, amount int64) {
	t.Helper()
	if _, _, err := l.Credit(context.Background(), ledger.Txn{ID: "topup:" + subject + time.Now().String(), Subject: subject,
		Kind: ledger.Topup, AmountMicro: amount}); err != nil {
		t.Fatal(err)
	}
}

// settledAccount waits for the subject's account to satisfy ok: the relay
// bills batch results after the response body has been sent, so a client
// that has read the whole body can look before billing finished.
func settledAccount(t *testing.T, l *ledgertest.Memory, subject string, ok func(ledger.Account) bool) ledger.Account {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a := account(t, l, subject)
		if ok(a) || time.Now().After(deadline) {
			return a
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func account(t *testing.T, l *ledgertest.Memory, subject string) ledger.Account {
	t.Helper()
	a, err := l.Account(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A batch is refused when the balance does not cover its estimate:
// every request's prompt and output limit at the batch price.
func TestBatchRefusedOnInsufficientBalance(t *testing.T) {
	f := &messageBatchFake{}
	e, store, l := balanceBatchEnv(t, f)
	topup(t, l, "user:u1", 500) // the estimate is about 2×100×5 + prompts
	resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(createTwo))
	if resp.StatusCode != http.StatusPaymentRequired || gjson.GetBytes(out, "error.type").String() != "billing_error" || len(store.objs) != 0 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if a := account(t, l, "user:u1"); a.HeldMicro != 0 || a.BalanceMicro != 500 {
		t.Fatalf("refused batch left %+v", a)
	}
	topup(t, l, "user:u1", 10_000)
	resp, out = e.batch("POST", "/v1/messages/batches", "application/json", []byte(createTwo))
	if resp.StatusCode != 200 {
		t.Fatalf("covered batch: %d %s", resp.StatusCode, out)
	}
	if a := account(t, l, "user:u1"); a.HeldMicro < 1000 || a.BalanceMicro != 10_500 {
		t.Fatalf("hold %+v", a)
	}
}

// A batch that ends with nothing to bill (cancelled before any request
// ran) has its hold released once the relay sees it ended.
func TestBatchHoldReleasedOnCancel(t *testing.T) {
	f := &messageBatchFake{status: endedEmpty}
	e, store, l := balanceBatchEnv(t, f)
	topup(t, l, "user:u1", 10_000)
	if resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(createTwo)); resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, out)
	}
	if a := account(t, l, "user:u1"); a.HeldMicro == 0 {
		t.Fatal("no hold")
	}
	if resp, out := e.batch("POST", "/v1/messages/batches/msgbatch_1/cancel", "", nil); resp.StatusCode != 200 {
		t.Fatalf("cancel: %d %s", resp.StatusCode, out)
	}
	if a := account(t, l, "user:u1"); a.HeldMicro == 0 {
		t.Fatal("hold released while still canceling")
	}
	if resp, out := e.batch("GET", "/v1/messages/batches/msgbatch_1", "", nil); resp.StatusCode != 200 {
		t.Fatalf("get: %d %s", resp.StatusCode, out)
	}
	if a := account(t, l, "user:u1"); a.HeldMicro != 0 || a.BalanceMicro != 10_000 {
		t.Fatalf("after the batch ended empty: %+v", a)
	}
	if o, _ := store.GetBatch(context.Background(), BatchKindMessageBatch, "msgbatch_1"); o.BilledAt.IsZero() {
		t.Fatal("ended batch not closed")
	}
}

// Results read after the balance was spent (the hold lapsed) are still
// charged, below zero, exactly once.
func TestBatchResultsChargedAfterBalanceSpent(t *testing.T) {
	f := &messageBatchFake{status: endedWithResults, results: messageBatchResults}
	e, _, l := balanceBatchEnv(t, f)
	topup(t, l, "user:u1", 10_000)
	if resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(createTwo)); resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, out)
	}
	ctx := context.Background()
	if _, err := l.ExpireHolds(ctx, time.Now().Add(100*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Credit(ctx, ledger.Txn{ID: "spent", Subject: "user:u1", Kind: ledger.Adjustment, AmountMicro: -10_000}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		resp, out := e.batch("GET", "/v1/messages/batches/msgbatch_1/results", "", nil)
		if resp.StatusCode != 200 || string(out) != messageBatchResults {
			t.Fatalf("results: %d %s", resp.StatusCode, out)
		}
	}
	e.p.SettleBatches(ctx)
	if err := e.p.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// One succeeded line: (100×1 + 50×10) / 2.
	if a := settledAccount(t, l, "user:u1", func(a ledger.Account) bool { return a.BalanceMicro == -300 && a.HeldMicro == 0 }); a.BalanceMicro != -300 || a.HeldMicro != 0 {
		t.Fatalf("after reading the results: %+v", a)
	}
	txns, _, err := l.Transactions(ctx, ledger.Query{Subject: "user:u1", Kind: ledger.Charge})
	if err != nil || len(txns) != 1 {
		t.Fatalf("charges %+v %v", txns, err)
	}
}

// Results nobody reads through the relay are fetched and billed by the
// relay once the batch ended; a client range request never meters part
// of the results as the whole.
func TestUnreadBatchResultsBilled(t *testing.T) {
	f := &messageBatchFake{status: endedWithResults, results: messageBatchResults}
	e, store, l := balanceBatchEnv(t, f)
	topup(t, l, "user:u1", 10_000)
	if resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(createTwo)); resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, out)
	}
	ctx := context.Background()
	e.p.SettleBatches(ctx)
	if err := e.p.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	rec := e.waitRecords(e.recordCount())
	if rec.ChargeMicro != 300 || rec.Usage.Output != 50 || rec.Caller.KeyID != "k1" {
		t.Fatalf("background record %+v", rec)
	}
	if a := settledAccount(t, l, "user:u1", func(a ledger.Account) bool { return a.BalanceMicro == 9_700 && a.HeldMicro == 0 }); a.BalanceMicro != 9_700 || a.HeldMicro != 0 {
		t.Fatalf("after the sweep: %+v", a)
	}
	if o, _ := store.GetBatch(ctx, BatchKindMessageBatch, "msgbatch_1"); o.BilledAt.IsZero() {
		t.Fatal("not marked billed")
	}
	// A later sweep and a client read charge nothing more.
	store.checked = nil
	e.p.SettleBatches(ctx)
	_ = e.p.Wait(ctx)
	if resp, _ := e.batch("GET", "/v1/messages/batches/msgbatch_1/results", "", nil); resp.StatusCode != 200 {
		t.Fatalf("read: %d", resp.StatusCode)
	}
	if a := account(t, l, "user:u1"); a.BalanceMicro != 9_700 {
		t.Fatalf("charged again: %+v", a)
	}
}

func TestBatchResultsRangeNotForwarded(t *testing.T) {
	f := &messageBatchFake{status: endedWithResults, results: messageBatchResults}
	e, _, l := balanceBatchEnv(t, f)
	topup(t, l, "user:u1", 10_000)
	if resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(createTwo)); resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, out)
	}
	srv := e.batchServer()
	req, _ := http.NewRequest("GET", srv+"/v1/messages/batches/msgbatch_1/results", nil)
	req.Header.Set("Range", "bytes=0-19")
	req.Header.Set("If-Range", `"etag"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != messageBatchResults || f.ranges[0] != "|" {
		t.Fatalf("%d %q, upstream saw %q", resp.StatusCode, body, f.ranges)
	}
	if a := settledAccount(t, l, "user:u1", func(a ledger.Account) bool { return a.BalanceMicro == 9_700 }); a.BalanceMicro != 9_700 {
		t.Fatalf("not billed in full: %+v", a)
	}
}

func newBatchHTTP(e *env) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := e.caller
		e.p.ServeBatch(w, r, &caller)
	}))
}

// batchServer serves the pipeline's batch APIs as the env's caller.
func (e *env) batchServer() string {
	srv := newBatchHTTP(e)
	e.t.Cleanup(srv.Close)
	return srv.URL
}

// Usage is billed to the batch's creator, not to another key of the same
// user reading the results.
func TestBatchBilledToCreator(t *testing.T) {
	f := &messageBatchFake{status: endedWithResults, results: messageBatchResults}
	settings := func(_ *plugin.Caller, pluginID, key string) plugin.Value {
		if pluginID == balance.ID && key == "payer" {
			return plugin.NewValue(json.RawMessage(`"key"`))
		}
		return plugin.Value{}
	}
	e, _, l := balanceBatchEnv(t, f, func(d *Deps) { d.Settings = settings })
	topup(t, l, "key:k1", 10_000)
	topup(t, l, "key:k5", 10_000)
	if resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(createTwo)); resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, out)
	}
	e.caller = plugin.Caller{KeyID: "k5", UserID: "u1"}
	if resp, _ := e.batch("GET", "/v1/messages/batches/msgbatch_1/results", "", nil); resp.StatusCode != 200 {
		t.Fatalf("read: %d", resp.StatusCode)
	}
	if a, b := account(t, l, "key:k1"), account(t, l, "key:k5"); a.BalanceMicro != 9_700 || a.HeldMicro != 0 || b.BalanceMicro != 10_000 {
		t.Fatalf("creator %+v reader %+v", a, b)
	}
}

// contentPlugin refuses requests that mention "forbidden", like a
// guardrail in block mode; rewrite makes it redact instead.
type contentPlugin struct {
	rewrite bool
	model   string // refused at admission
}

func (p *contentPlugin) Info() plugin.Info {
	return plugin.Info{ID: "middleware.content", Kind: plugin.KindMiddleware, FailPolicy: plugin.FailClosed}
}

func (p *contentPlugin) OnRequest(c *plugin.Call) (*plugin.Reply, error) {
	if c.Model == p.model {
		return &plugin.Reply{Error: ir.Errorf(ir.ErrPermission, "model %s is not allowed", c.Model)}, nil
	}
	req, err := c.Request()
	if err != nil || req == nil {
		return nil, nil
	}
	for i, m := range req.Messages {
		for j, b := range m.Content {
			if strings.Contains(b.Text, "forbidden") {
				if p.rewrite {
					req.Messages[i].Content[j].Text = "[redacted]"
					continue
				}
				return &plugin.Reply{Error: ir.Errorf(ir.ErrPermission, "content blocked")}, nil
			}
		}
	}
	return nil, nil
}

// Request plugins see every request of a batch: a refusal (or a rewrite,
// which a batch cannot carry) stops the batch before it is sent.
func TestBatchRunsRequestPlugins(t *testing.T) {
	f := &messageBatchFake{}
	up := newFake(t, f.handle)
	e, _ := batchEnv(t, "claude", []upSpec{{id: "an", protocol: "anthropic", issuer: "an", fake: up, caps: "{batches: true}"}})
	pl := &contentPlugin{}
	e.chain.Plugins = []plugin.Plugin{pl}
	bad := strings.Replace(createTwo, `"yo"`, `"this is forbidden"`, 1)
	resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(bad))
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(out), "requests.1: content blocked") || up.calls() != 0 {
		t.Fatalf("blocked line: %d %s (calls %d)", resp.StatusCode, out, up.calls())
	}
	pl.rewrite = true
	resp, out = e.batch("POST", "/v1/messages/batches", "application/json", []byte(bad))
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(out), "rewrites requests") || up.calls() != 0 {
		t.Fatalf("rewritten line: %d %s", resp.StatusCode, out)
	}
	pl.model = "claude"
	resp, out = e.batch("POST", "/v1/messages/batches", "application/json", []byte(createTwo))
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(out), "not allowed") {
		t.Fatalf("model refused: %d %s", resp.StatusCode, out)
	}
	pl.model = ""
	if resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(createTwo)); resp.StatusCode != 200 || up.calls() != 1 {
		t.Fatalf("clean batch: %d %s", resp.StatusCode, out)
	}

	// OpenAI uploads are checked line by line as they stream.
	fo := &openAIBatchFake{}
	upo := newFake(t, fo.handle)
	eo, _ := batchEnv(t, "gpt", []upSpec{{id: "a", protocol: "openai-chat", issuer: "o", fake: upo, caps: "{batches: true}"}})
	eo.chain.Plugins = []plugin.Plugin{&contentPlugin{}}
	lines := `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt","messages":[{"role":"user","content":"ok"}]}}
{"custom_id":"b","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt","messages":[{"role":"user","content":"forbidden"}]}}
`
	body, ct := batchUploadBody(t, true, "batch", lines)
	resp, out = eo.batch("POST", "/v1/files", ct, body)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(out), "line 2: content blocked") {
		t.Fatalf("upload: %d %s", resp.StatusCode, out)
	}
}

// results_url points at the configured public URL, not at forwarded
// headers a client can set.
func TestBatchResultsURLUsesPublicURL(t *testing.T) {
	f := &messageBatchFake{status: endedWithResults}
	up := newFake(t, f.handle)
	store := newMemBatches()
	e := newEnv(t, "claude", []upSpec{{id: "an", protocol: "anthropic", issuer: "an", fake: up, caps: "{batches: true}"}},
		func(d *Deps) {
			d.Batches, d.PublicURL = store, func(context.Context) string { return "https://relay.example.com/" }
		})
	if resp, out := e.batch("POST", "/v1/messages/batches", "application/json", []byte(createTwo)); resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, out)
	}
	srv := e.batchServer()
	req, _ := http.NewRequest("GET", srv+"/v1/messages/batches/msgbatch_1", nil)
	req.Header.Set("X-Forwarded-Host", "evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := gjson.GetBytes(out, "results_url").String(); got != "https://relay.example.com/v1/messages/batches/msgbatch_1/results" {
		t.Fatalf("results_url %q", got)
	}
}

// OpenAI: the batch holds the uploaded file's estimate, and its output
// file is billed to the batch, which releases the hold.
func TestOpenAIBatchHoldAndBill(t *testing.T) {
	fa := &openAIBatchFake{output: batchOutput}
	up := newFake(t, fa.handle)
	e, store := batchEnv(t, "gpt", []upSpec{{id: "a", protocol: "openai-chat", issuer: "o", fake: up, model: "gpt-up", caps: "{batches: true}"}})
	e.snap.ModelPrice = func(string) (Price, bool) {
		return Price{Input: 1_000_000, Output: 2_000_000, BatchDiscount: 0.5}, true
	}
	l := ledgertest.New()
	e.chain.Meter = balance.New(l)
	topup(t, l, "user:u1", 20_000)
	lines := `{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt","max_tokens":1000,"messages":[{"role":"user","content":"2+2"}]}}
{"custom_id":"c","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt","messages":[{"role":"user","content":"2+3"}]}}
`
	body, ct := batchUploadBody(t, true, "batch", lines)
	if resp, out := e.batch("POST", "/v1/files", ct, body); resp.StatusCode != 200 {
		t.Fatalf("upload: %d %s", resp.StatusCode, out)
	}
	f, _ := store.GetBatch(context.Background(), BatchKindFile, "file-in1")
	if f.OutputTokens != 1000+batchLineOutput || f.InputTokens == 0 {
		t.Fatalf("file estimate %d in, %d out", f.InputTokens, f.OutputTokens)
	}
	create := []byte(`{"input_file_id":"file-in1","endpoint":"/v1/chat/completions","completion_window":"24h"}`)
	// (1000 + 4096) output tokens at 2 per token, halved: about 5100.
	if resp, out := e.batch("POST", "/v1/batches", "application/json", create); resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, out)
	}
	if a := account(t, l, "user:u1"); a.HeldMicro < 5_096 || a.HeldMicro > 5_200 {
		t.Fatalf("hold %+v", a)
	}
	if resp, out := e.batch("GET", "/v1/batches/batch_1", "", nil); resp.StatusCode != 200 {
		t.Fatalf("get: %d %s", resp.StatusCode, out)
	}
	if resp, out := e.batch("GET", "/v1/files/file-out1/content", "", nil); resp.StatusCode != 200 || string(out) != batchOutput {
		t.Fatalf("content: %d %s", resp.StatusCode, out)
	}
	if a := settledAccount(t, l, "user:u1", func(a ledger.Account) bool { return a.BalanceMicro == 18_000 && a.HeldMicro == 0 }); a.BalanceMicro != 18_000 || a.HeldMicro != 0 {
		t.Fatalf("after billing: %+v", a)
	}
	if o, _ := store.GetBatch(context.Background(), BatchKindBatch, "batch_1"); o.BilledAt.IsZero() || o.HoldID == "" {
		t.Fatalf("batch %+v", o)
	}
	// Too little balance for the estimate: refused before the upstream.
	calls := up.calls()
	if _, _, err := l.Credit(context.Background(), ledger.Txn{ID: "spend", Subject: "user:u1", Kind: ledger.Adjustment, AmountMicro: -15_000}); err != nil {
		t.Fatal(err)
	}
	if resp, out := e.batch("POST", "/v1/batches", "application/json", create); resp.StatusCode != 429 || up.calls() != calls {
		t.Fatalf("over budget: %d %s", resp.StatusCode, out)
	}
}

// A batch for a preset is held and charged at the list price of the member
// that served it, even when neither the preset nor the upstream has a
// price, and its records name that member.
func TestBatchPresetChargedAtMemberPrice(t *testing.T) {
	f := &messageBatchFake{status: endedWithResults, results: messageBatchResults}
	e, store, l := balanceBatchEnv(t, f)
	tb := e.snap.Table
	tb.Models["gpt-mini"] = router.Model{Name: "gpt-mini", Enabled: true, Routes: tb.Models["claude"].Routes}
	delete(tb.Models, "claude")
	tb.Presets = map[string]router.Preset{"auto": {Name: "auto", Models: []string{"gpt-mini"}, Strategy: "in_order"}}
	e.snap.ModelPrice = func(m string) (Price, bool) {
		if m == "gpt-mini" {
			return Price{Input: 1_000_000, Output: 10_000_000, BatchDiscount: 0.5}, true
		}
		return Price{}, false
	}
	e.snap.Price = func(string, string) (Price, bool) { return Price{}, false }
	create := []byte(strings.ReplaceAll(createTwo, `"model":"claude"`, `"model":"auto"`))
	topup(t, l, "user:u1", 10)
	if resp, out := e.batch("POST", "/v1/messages/batches", "application/json", create); resp.StatusCode != http.StatusPaymentRequired || len(store.objs) != 0 {
		t.Fatalf("too little balance: %d %s", resp.StatusCode, out)
	}
	topup(t, l, "user:u1", 9_990)
	if resp, out := e.batch("POST", "/v1/messages/batches", "application/json", create); resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, out)
	}
	if a := account(t, l, "user:u1"); a.HeldMicro < 1000 {
		t.Fatalf("hold %+v", a)
	}
	ctx := context.Background()
	o, _ := store.GetBatch(ctx, BatchKindMessageBatch, "msgbatch_1")
	if o.Model != "auto" || o.ServedModel != "gpt-mini" {
		t.Fatalf("batch object %+v", o)
	}
	if resp, _ := e.batch("GET", "/v1/messages/batches/msgbatch_1/results", "", nil); resp.StatusCode != 200 {
		t.Fatalf("results: %d", resp.StatusCode)
	}
	// One succeeded line: (100×1 + 50×10) / 2.
	if a := settledAccount(t, l, "user:u1", func(a ledger.Account) bool { return a.BalanceMicro == 9_700 && a.HeldMicro == 0 }); a.BalanceMicro != 9_700 || a.HeldMicro != 0 {
		t.Fatalf("after the results: %+v", a)
	}
	rec := e.waitRecords(e.recordCount())
	if a := rec.Attempts[len(rec.Attempts)-1]; rec.Model != "auto" || rec.ChargeMicro != 300 || a.Model != "gpt-mini" || a.Charge == nil || a.Charge.Source != "model" {
		t.Fatalf("record %s charge %d, attempt %+v", rec.Model, rec.ChargeMicro, rec.Attempts)
	}
}
