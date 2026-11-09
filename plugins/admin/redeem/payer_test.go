package redeem

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
	"github.com/initxy/plugrelay/plugins/meter/balance"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger/ledgertest"
)

func redeemAs(p *Plugin, user, body string) (int, map[string]any) {
	req := httptest.NewRequest("POST", "/api/plugins/admin.redeem/redeem", strings.NewReader(body))
	req.Header.Set("X-Plugrelay-User", user)
	rec := httptest.NewRecorder()
	p.postRedeem(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// payer=key: a code is redeemed into the chosen key's account, and that
// key's requests draw on it.
func TestRedeemPayerKey(t *testing.T) {
	l := ledgertest.New()
	l.Payer = "key"
	l.Keys["k1"] = ledgertest.Key{UserID: "u1"}
	l.Keys["kx"] = ledgertest.Key{UserID: "u2"}
	kv := testkit.NewState(nil)
	p := New(l, kv)
	if err := p.Init(t.Context(), &testkit.Host{Store: kv}); err != nil {
		t.Fatal(err)
	}
	code := "RC-AAAA-BBBB-CCCC-DDDD-EEEE"
	if err := l.CreateCodes(t.Context(), []ledger.Code{{Hash: Hash(code), AmountMicro: 1000, MaxUses: 1}}); err != nil {
		t.Fatal(err)
	}
	if c, out := redeemAs(p, "u1", `{"code":"`+code+`"}`); c != http.StatusBadRequest {
		t.Fatalf("without key_id: %d %v", c, out)
	}
	if c, _ := redeemAs(p, "u1", `{"code":"`+code+`","key_id":"kx"}`); c != http.StatusNotFound {
		t.Fatalf("someone else's key: %d", c)
	}
	if c, out := redeemAs(p, "u1", `{"code":"`+code+`","key_id":"k1"}`); c != 200 {
		t.Fatalf("redeem: %d %v", c, out)
	}
	m := balance.New(l)
	call := testkit.NewCall(balance.ID, testkit.CallOpts{ID: "r1", Protocol: "openai-chat", Model: "m",
		Caller: &plugin.Caller{KeyID: "k1", UserID: "u1"}, Settings: testkit.Settings{"payer": "key"}})
	res, err := m.Reserve(call, 400)
	if err != nil {
		t.Fatalf("request refused after the redeem: %v", err)
	}
	if err := m.Settle(call, res, ir.Usage{}, 250); err != nil {
		t.Fatal(err)
	}
	if a, _ := l.Account(t.Context(), "key:k1"); a.BalanceMicro != 750 || a.HeldMicro != 0 {
		t.Fatalf("key account: %+v", a)
	}
	// A second key of the same user does not redeem the code again.
	many := "RC-FFFF-GGGG-HHHH-JJJJ-KKKK"
	if err := l.CreateCodes(t.Context(), []ledger.Code{{Hash: Hash(many), AmountMicro: 1000, MaxUses: 10}}); err != nil {
		t.Fatal(err)
	}
	l.Keys["k2"] = ledgertest.Key{UserID: "u1"}
	if c, out := redeemAs(p, "u1", `{"code":"`+many+`","key_id":"k1"}`); c != 200 {
		t.Fatalf("redeem: %d %v", c, out)
	}
	if c, out := redeemAs(p, "u1", `{"code":"`+many+`","key_id":"k2"}`); c == 200 {
		t.Fatalf("same user, second key: %d %v", c, out)
	}
}

// countingRedeem counts attempts that reach the ledger.
type countingRedeem struct {
	*ledgertest.Memory
	n atomic.Int64
}

func (c *countingRedeem) Redeem(ctx context.Context, hash, subject, actor string, now time.Time) (ledger.Txn, error) {
	c.n.Add(1)
	time.Sleep(time.Millisecond)
	return c.Memory.Redeem(ctx, hash, subject, actor, now)
}

// Concurrent wrong guesses cannot get past failures_per_hour.
func TestRedeemFailureLimitConcurrent(t *testing.T) {
	l := &countingRedeem{Memory: ledgertest.New()}
	kv := testkit.NewState(nil)
	p := New(l, kv)
	if err := p.Init(t.Context(), &testkit.Host{Store: kv, Set: testkit.Settings{"failures_per_hour": 3}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var limited atomic.Int64
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, _ := redeemAs(p, "u1", `{"code":"RC-NOPE"}`); c == http.StatusTooManyRequests {
				limited.Add(1)
			}
		}()
	}
	wg.Wait()
	if l.n.Load() != 3 || limited.Load() != 27 {
		t.Fatalf("%d attempts reached the ledger, %d limited", l.n.Load(), limited.Load())
	}
}
