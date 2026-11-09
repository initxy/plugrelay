package balance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger/ledgertest"
)

func (m *Meter) get(t *testing.T, target string) (int, map[string]any) {
	t.Helper()
	path, _, _ := strings.Cut(target, "?")
	for _, rt := range m.Routes() {
		if rt.Method == http.MethodGet && rt.Path == path {
			req := httptest.NewRequest(http.MethodGet, "/api/plugins/meter.balance"+target, nil)
			req.Header.Set("X-Plugrelay-User", "u1")
			rec := httptest.NewRecorder()
			rt.Handler(rec, req)
			var out map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			return rec.Code, out
		}
	}
	t.Fatalf("no route %s", path)
	return 0, nil
}

// payer=key: the console shows and tops up the account of one of the
// user's keys, the one its requests draw on.
func TestPayerKeyConsoleAccounts(t *testing.T) {
	l := ledgertest.New()
	l.Payer = "key"
	l.Keys["k1"] = ledgertest.Key{UserID: "u1", Name: "laptop"}
	l.Keys["k2"] = ledgertest.Key{UserID: "u1", Name: "ci"}
	l.Keys["kx"] = ledgertest.Key{UserID: "someone-else"}
	m := New(l)
	if err := m.Init(t.Context(), &testkit.Host{Set: testkit.Settings{"payer": "key"}}); err != nil {
		t.Fatal(err)
	}
	topup(t, l, "key:k1", 1000)
	c := testkit.NewCall(ID, testkit.CallOpts{ID: "r1", Protocol: "openai-chat", Model: "m",
		Caller: &plugin.Caller{KeyID: "k1", UserID: "u1"}, Settings: testkit.Settings{"payer": "key"}})
	res, err := m.Reserve(c, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Settle(c, res, ir.Usage{}, 70); err != nil {
		t.Fatal(err)
	}

	if code, out := m.get(t, "/me"); code != http.StatusBadRequest || !strings.Contains(out["error"].(map[string]any)["code"].(string), "key_required") {
		t.Fatalf("/me without a key: %d %v", code, out)
	}
	if code, _ := m.get(t, "/me?key_id=kx"); code != http.StatusNotFound {
		t.Fatalf("/me for another user's key: %d", code)
	}
	code, out := m.get(t, "/me?key_id=k1")
	if code != 200 || out["subject"] != "key:k1" || out["balance"].(map[string]any)["micro"].(float64) != 930 {
		t.Fatalf("/me?key_id=k1: %d %v", code, out)
	}
	code, out = m.get(t, "/me/payers")
	items, _ := out["items"].([]any)
	if code != 200 || out["payer"] != "key" || len(items) != 2 || items[0].(map[string]any)["subject"] != "key:k1" {
		t.Fatalf("/me/payers: %d %v", code, out)
	}
}

// failingCapture is a ledger whose Capture fails while down is set: a
// database blip during settle.
type failingCapture struct {
	*ledgertest.Memory
	down atomic.Bool
}

func (f *failingCapture) Capture(ctx context.Context, id string, charge int64, note string) (bool, error) {
	if f.down.Load() {
		return false, errors.New("connection reset")
	}
	return f.Memory.Capture(ctx, id, charge, note)
}

// A settle that fails is not lost: the sweep charges it from the request
// log record, once, instead of releasing the hold unbilled.
func TestSettleFailureChargedFromRecord(t *testing.T) {
	mem := ledgertest.New()
	l := &failingCapture{Memory: mem}
	m := New(l)
	topup(t, mem, "user:u1", 1000)
	c := call("r1", "openai-chat", nil)
	res, err := m.Reserve(c, 300)
	if err != nil {
		t.Fatal(err)
	}
	l.down.Store(true)
	if err := m.Settle(c, res, ir.Usage{}, 120); err == nil {
		t.Fatal("settle during the blip succeeded")
	}
	l.down.Store(false)
	// The core logs the request with its charge (at-least-once).
	mem.Record("r1", 120)
	now := time.Now()
	// Within the grace period the hold is left to in-flight retries.
	m.runSweep(now, 6*time.Hour)
	if a, _ := mem.Account(t.Context(), "user:u1"); a.HeldMicro != 300 {
		t.Fatalf("settled inside the grace period: %+v", a)
	}
	for range 2 {
		m.runSweep(now.Add(2*settleGrace), 6*time.Hour)
	}
	if a, _ := mem.Account(t.Context(), "user:u1"); a.BalanceMicro != 880 || a.HeldMicro != 0 {
		t.Fatalf("after the sweep: %+v", a)
	}
	// The late in-process retry does not charge again.
	if err := m.Settle(c, res, ir.Usage{}, 120); err != nil {
		t.Fatal(err)
	}
	if a, _ := mem.Account(t.Context(), "user:u1"); a.BalanceMicro != 880 {
		t.Fatalf("charged twice: %+v", a)
	}
}
