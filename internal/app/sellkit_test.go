package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/config"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/admin/payment/stripe"
	"github.com/initxy/plugrelay/plugins/admin/redeem"
)

type sellRelay struct {
	t    *testing.T
	a    *App
	st   store.Store
	l    store.Ledger
	base string
	key  string
}

// newSellRelay runs a relay with meter.balance on, a priced model "m"
// served by a fake chat upstream and a model "broken" whose upstream fails.
func newSellRelay(t *testing.T, plugins ...string) *sellRelay {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mk, _ := secret.GenerateMasterKey()
	cfg := config.Defaults()
	cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
	cfg.MasterKey = mk
	// Fake payment providers listen on loopback.
	cfg.Egress.AllowPrivateCIDRs = []string{"127.0.0.0/8"}
	a, err := New(ctx, &cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	st := a.Store()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	up := fakeChat(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom","type":"server_error"}}`)
	}))
	t.Cleanup(bad.Close)
	must(st.CreateUser(ctx, &store.User{ID: "admin", Name: "admin", Email: "admin", Role: store.RoleAdmin}, "correct-horse-1"))
	must(st.CreateUser(ctx, &store.User{ID: "u1", Name: "Ann", Email: "ann@example.com", Role: store.RoleUser}, "correct-horse-2"))
	for id, u := range map[string]string{"fake": up.URL, "bad": bad.URL} {
		must(st.PutUpstream(ctx, &store.Upstream{ID: id, Provider: "provider.openai-compatible", Enabled: true,
			Config: map[string]any{"manifest": "vllm", "base_url": u + "/v1", configAllowPrivate: true}}))
		must(st.PutCredential(ctx, &store.Credential{ID: id + "-c", UpstreamID: id, Kind: store.CredShared, Secret: "sk-upstream",
			Status: store.CredActive}))
	}
	for m, upID := range map[string]string{"m": "fake", "broken": "bad"} {
		must(st.PutModel(ctx, &store.Model{Name: m, Enabled: true}))
		must(st.ReplaceRoutes(ctx, m, []store.Route{{Model: m, UpstreamID: upID, UpstreamModel: "fake-model", Weight: 1}}))
		// 1 USD per million input tokens, 2 per million output tokens.
		must(st.PutPrice(ctx, &store.PriceRule{Model: m, Input: 1_000_000, Output: 2_000_000, Currency: "USD"}))
	}
	must(st.PutFeature(ctx, store.FeatureState{ID: "billing", Enabled: true}))
	if slices.Contains(plugins, "admin.signup") {
		must(st.PutFeature(ctx, store.FeatureState{ID: "signup", Enabled: true}))
	}
	// A shared relay: the console on loopback still needs a login.
	must(st.KV("core.site").Put(ctx, "settings", []byte(`{"security.local_owner_login":false,"site.public_base_url":"https://relay.example.com"}`), 0))
	key, err := st.CreateKey(ctx, &store.Key{Name: "ann", UserID: "u1"})
	must(err)
	must(a.Reload(ctx))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	go func() { _ = a.ServeListener(ctx, ln) }()
	l, _ := store.LedgerOf(st)
	return &sellRelay{t: t, a: a, st: st, l: l, base: "http://" + ln.Addr().String(), key: key}
}

func (r *sellRelay) post(path, body string, hdr map[string]string) (int, http.Header, string) {
	r.t.Helper()
	req, _ := http.NewRequest("POST", r.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func (r *sellRelay) chat(model string) (int, string) {
	code, _, body := r.post("/v1/chat/completions", `{"model":"`+model+`","max_tokens":100,"stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer " + r.key})
	return code, body
}

func (r *sellRelay) account() store.BalanceAccount {
	a, err := r.l.Account(context.Background(), "user:u1")
	if err != nil {
		r.t.Fatal(err)
	}
	return *a
}

func TestSellKitInsufficientBalancePerProtocol(t *testing.T) {
	r := newSellRelay(t)
	for _, c := range []struct {
		name, path, body string
		hdr              map[string]string
		status           int
		want             string
	}{
		{"anthropic", "/v1/messages", `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"x-api-key": r.key, "anthropic-version": "2023-06-01"}, 402, `"type":"billing_error"`},
		{"openai-chat", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"Authorization": "Bearer " + r.key}, 429, `"code":"insufficient_quota"`},
		{"openai-responses", "/v1/responses", `{"model":"m","input":"hi"}`,
			map[string]string{"Authorization": "Bearer " + r.key}, 429, `insufficient_quota`},
		{"gemini", "/v1beta/models/m:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			map[string]string{"x-goog-api-key": r.key}, 429, `RESOURCE_EXHAUSTED`},
	} {
		code, h, body := r.post(c.path, c.body, c.hdr)
		if code != c.status || !strings.Contains(body, c.want) || !strings.Contains(body, "balance is too low") {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
		if c.name != "gemini" && h.Get("X-Should-Retry") != "false" {
			t.Errorf("%s: retry hint %q", c.name, h.Get("X-Should-Retry"))
		}
	}
	if a := r.account(); a.HeldMicro != 0 || a.BalanceMicro != 0 {
		t.Fatalf("refused requests changed the balance: %+v", a)
	}
}

func TestSellKitChargeAndRefund(t *testing.T) {
	r := newSellRelay(t)
	ctx := context.Background()
	if _, _, err := r.l.Credit(ctx, &store.BalanceTxn{ID: "topup:test", Subject: "user:u1", Kind: store.TxnTopup, AmountMicro: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if code, body := r.chat("m"); code != 200 {
		t.Fatalf("chat: %d %s", code, body)
	}
	// 5 input tokens × 1 USD/M + 2 output tokens × 2 USD/M = 9 micro-units.
	var a store.BalanceAccount
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if a = r.account(); a.BalanceMicro != 1_000_000 && a.HeldMicro == 0 {
			break
		}
	}
	if a.BalanceMicro != 1_000_000-9 || a.HeldMicro != 0 {
		t.Fatalf("after a charged request: %+v", a)
	}
	txns, _, err := r.l.Transactions(ctx, store.TxnQuery{Subject: "user:u1", Kind: store.TxnCharge})
	if err != nil || len(txns) != 1 || txns[0].AmountMicro != -9 || txns[0].Note != "m" {
		t.Fatalf("charge rows: %+v %v", txns, err)
	}
	// The request log carries the same charge.
	var rec *struct{ charge int64 }
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && rec == nil; time.Sleep(20 * time.Millisecond) {
		if got, err := r.st.GetRecord(ctx, txns[0].RequestID); err == nil {
			rec = &struct{ charge int64 }{got.ChargeMicro}
		}
	}
	if rec == nil || rec.charge != 9 {
		t.Fatalf("request log charge: %+v", rec)
	}

	// The upstream fails: the hold is released, nothing is charged.
	if code, _ := r.chat("broken"); code < 500 {
		t.Fatalf("broken upstream answered %d", code)
	}
	time.Sleep(100 * time.Millisecond)
	if a := r.account(); a.BalanceMicro != 1_000_000-9 || a.HeldMicro != 0 {
		t.Fatalf("after a failed request: %+v", a)
	}
}

func TestSellKitSignupOffByDefault(t *testing.T) {
	r := newSellRelay(t)
	body := `{"email":"new@example.com","password":"correct-horse-3"}`
	if code, _, out := r.post("/api/plugins/admin.signup/register", body, nil); code/100 == 2 {
		t.Fatalf("sign-up served while off: %d %s", code, out)
	}
	if us, _ := r.st.ListUsers(context.Background()); len(us) != 2 {
		t.Fatalf("users: %d", len(us))
	}

	on := newSellRelay(t, "admin.signup", "admin.payment")
	if code, _, out := on.post("/api/plugins/admin.signup/register", body, nil); code != http.StatusCreated {
		t.Fatalf("sign-up when enabled: %d %s", code, out)
	}
	u, err := on.st.Authenticate(context.Background(), "new@example.com", "correct-horse-3")
	if err != nil || u.Role != store.RoleUser {
		t.Fatalf("new account: %+v %v", u, err)
	}
	// Public routes get no console identity, even when a client forges one.
	sig := stripe.Sign("whsec_x", []byte(`{}`), time.Now())
	code, _, out := on.post("/api/plugins/admin.payment/webhook/stripe", `{}`, map[string]string{"Stripe-Signature": sig,
		"X-Plugrelay-User": "admin"})
	if code != http.StatusNotFound || !strings.Contains(out, "not configured") {
		t.Fatalf("webhook without stripe settings: %d %s", code, out)
	}
	// Non-public plugin routes still need a login.
	if code, _, _ := on.post("/api/plugins/admin.payment/checkout", `{"amount_micro":5000000}`, map[string]string{"X-Plugrelay-User": "u1"}); code != http.StatusUnauthorized {
		t.Fatalf("checkout without login: %d", code)
	}
}

// TestSetupSellEnablesSellKit: answering "sell" in the setup wizard turns
// the sell-kit plugins on, except sign-up and the new-api import.
func TestSetupSellEnablesSellKit(t *testing.T) {
	for _, usage := range []string{"myself", "sell"} {
		t.Run(usage, func(t *testing.T) {
			mk, _ := secret.GenerateMasterKey()
			cfg := config.Defaults()
			cfg.Store.DSN = filepath.Join(t.TempDir(), "relay.db")
			cfg.MasterKey = mk
			a, err := New(context.Background(), &cfg, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(a.closeAll)
			srv := httptest.NewServer(a.Handler().Handler())
			t.Cleanup(srv.Close)
			body := `{"admin_username":"admin","admin_password":"correct-horse-1","usage":"` + usage + `","listen_public":false}`
			resp, err := http.Post(srv.URL+"/api/v1/setup", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("setup: %d %s", resp.StatusCode, b)
			}
			if err := a.Reload(context.Background()); err != nil {
				t.Fatal(err)
			}
			enabled := map[string]bool{}
			for _, e := range a.entries() {
				enabled[e.Info.ID] = e.Enabled
			}
			sell := usage == "sell"
			for _, id := range []string{"meter.balance", "admin.redeem", "admin.payment", "admin.subscriptions", "admin.import-newapi"} {
				if enabled[id] != sell {
					t.Errorf("%s enabled = %v, want %v", id, enabled[id], sell)
				}
			}
			if enabled["admin.signup"] {
				t.Error("setup turned self sign-up on")
			}
		})
	}
}

// TestSellKitUserListBalance: with meter.balance running, the console's
// user list carries each user's available balance from the ledger.
func TestSellKitUserListBalance(t *testing.T) {
	const token = "admin-test-token"
	t.Setenv(EnvAdminToken, token)
	r := newSellRelay(t)
	ctx := context.Background()
	if _, _, err := r.l.Credit(ctx, &store.BalanceTxn{ID: "t1", Subject: "user:u1", Kind: store.TxnTopup, AmountMicro: 3_250_000}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", r.base+"/api/v1/users", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Items []struct {
			ID      string
			Balance *struct {
				Micro    int64
				Currency string
			}
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET /users: %d %v", resp.StatusCode, err)
	}
	got := map[string]int64{}
	for _, u := range out.Items {
		if u.Balance == nil {
			t.Fatalf("user %s has no balance with meter.balance on", u.ID)
		}
		got[u.ID] = u.Balance.Micro
	}
	if got["u1"] != 3_250_000 || got["admin"] != 0 {
		t.Fatalf("balances = %v", got)
	}
}

// consolePost calls a plugin route as console user uid (session cookie).
func (r *sellRelay) consolePost(uid, path, body string) (int, string) {
	r.t.Helper()
	tok, err := r.st.CreateSession(context.Background(), uid, time.Hour)
	if err != nil {
		r.t.Fatal(err)
	}
	method := "POST"
	if body == "" {
		method = "GET"
	}
	req, _ := http.NewRequest(method, r.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "pr_session", Value: tok})
	req.AddCookie(&http.Cookie{Name: "pr_csrf", Value: "csrf-1"})
	req.Header.Set("X-CSRF-Token", "csrf-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (r *sellRelay) setPayer(mode string) {
	r.t.Helper()
	ctx := context.Background()
	if err := r.st.PutSetting(ctx, store.SettingRow{Scope: plugin.Global, PluginID: "meter.balance", Key: "payer",
		Value: json.RawMessage(`"` + mode + `"`)}); err != nil {
		r.t.Fatal(err)
	}
	if err := r.a.Reload(ctx); err != nil {
		r.t.Fatal(err)
	}
}

// payer=key: a redeem in the console credits the key the user picked, and
// that key's requests are charged from it.
func TestSellKitPayerKeyTopupIsCharged(t *testing.T) {
	r := newSellRelay(t, "admin.redeem")
	r.setPayer("key")
	ctx := context.Background()
	keys, err := r.st.ListKeys(ctx, store.KeyFilter{UserID: "u1"})
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys: %v %v", keys, err)
	}
	k1 := keys[0].ID
	if _, err := r.st.CreateKey(ctx, &store.Key{Name: "ci", UserID: "u1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.CreateKey(ctx, &store.Key{ID: "admin-key", Name: "ops", UserID: "admin"}); err != nil {
		t.Fatal(err)
	}
	const code = "RC-AAAA-BBBB-CCCC-DDDD-EEEE"
	if err := r.l.CreateRedeemCodes(ctx, []store.RedeemCode{{CodeHash: redeem.Hash(code), AmountMicro: 1_000_000, MaxUses: 5}}); err != nil {
		t.Fatal(err)
	}
	if c, body := r.consolePost("u1", "/api/plugins/admin.redeem/redeem", `{"code":"`+code+`"}`); c != http.StatusBadRequest || !strings.Contains(body, "key_required") {
		t.Fatalf("redeem without key_id: %d %s", c, body)
	}
	if c, body := r.consolePost("u1", "/api/plugins/admin.redeem/redeem", `{"code":"`+code+`","key_id":"admin-key"}`); c != http.StatusNotFound {
		t.Fatalf("redeem into another user's key: %d %s", c, body)
	}
	if c, body := r.consolePost("u1", "/api/plugins/admin.redeem/redeem", `{"code":"`+code+`","key_id":"`+k1+`"}`); c != 200 {
		t.Fatalf("redeem: %d %s", c, body)
	}
	if c, body := r.consolePost("u1", "/api/plugins/meter.balance/me/payers", ""); c != 200 || strings.Count(body, `"key_id"`) != 2 {
		t.Fatalf("payers: %d %s", c, body)
	}
	if c, body := r.chat("m"); c != 200 {
		t.Fatalf("chat after the redeem: %d %s", c, body)
	}
	var a *store.BalanceAccount
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if a, _ = r.l.Account(ctx, "key:"+k1); a.HeldMicro == 0 && a.BalanceMicro != 1_000_000 {
			break
		}
	}
	if a.BalanceMicro != 1_000_000-9 || a.HeldMicro != 0 {
		t.Fatalf("key account after a request: %+v", a)
	}
	if u := r.account(); u.BalanceMicro != 0 {
		t.Fatalf("user account touched: %+v", u)
	}
}

// payer=group: a key moved to another group pays from that group, and a
// top-up for that key goes there too.
func TestSellKitPayerGroupFollowsKey(t *testing.T) {
	r := newSellRelay(t)
	r.setPayer("group")
	ctx := context.Background()
	for _, g := range []string{"ga", "gb"} {
		if err := r.st.PutGroup(ctx, &store.Group{ID: g, Name: g, PriceMultiplier: 1}); err != nil {
			t.Fatal(err)
		}
	}
	u, _ := r.st.GetUser(ctx, "u1")
	u.GroupID = "ga"
	if err := r.st.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	keys, _ := r.st.ListKeys(ctx, store.KeyFilter{UserID: "u1"})
	k := keys[0]
	k.GroupID = "gb"
	if err := r.st.UpdateKey(ctx, &k); err != nil {
		t.Fatal(err)
	}
	l := (&host{a: r.a}).Ledger()
	if s, err := l.PayerOf(ctx, "u1", ""); err != nil || s != "group:ga" {
		t.Fatalf("own account: %q %v", s, err)
	}
	if s, err := l.PayerOf(ctx, "u1", k.ID); err != nil || s != "group:gb" {
		t.Fatalf("key's account: %q %v", s, err)
	}
	ps, err := l.Payers(ctx, "u1")
	if err != nil || len(ps) != 2 || ps[1].Subject != "group:gb" || ps[1].KeyID != k.ID {
		t.Fatalf("payers: %+v %v", ps, err)
	}
}
