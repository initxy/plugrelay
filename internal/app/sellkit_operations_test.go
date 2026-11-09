package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/admin/payment/stripe"
)

func (r *sellRelay) pluginSettings(id string, kv map[string]any) {
	r.t.Helper()
	ctx := context.Background()
	for k, v := range kv {
		raw, _ := json.Marshal(v)
		if err := r.st.PutSetting(ctx, store.SettingRow{Scope: plugin.Global, PluginID: id, Key: k, Value: raw}); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := r.a.Reload(ctx); err != nil {
		r.t.Fatal(err)
	}
}

func checkoutEvent(id, order string, cents int64) []byte {
	b, _ := json.Marshal(map[string]any{"id": id, "object": "event", "type": "checkout.session.completed", "data": map[string]any{
		"object": map[string]any{"object": "checkout.session", "id": "cs_test_1", "client_reference_id": order,
			"metadata": map[string]string{"order_id": order}, "payment_status": "paid", "amount_total": cents, "currency": "usd"}}})
	return b
}

// A daily check-in pays once per day, also when clicked many times at once.
func TestSellKitCheckInOncePerDay(t *testing.T) {
	r := newSellRelay(t)
	if code, out := r.json("u1", "/api/plugins/admin.rewards/checkin", `{}`); code != http.StatusNotFound || !strings.Contains(toJSON(out), "checkin_off") {
		t.Fatalf("check-in while off: %d %v", code, out)
	}
	r.pluginSettings("admin.rewards", map[string]any{"checkin_enabled": true, "checkin_min_micro": 200_000, "checkin_max_micro": 200_000})
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for range 5 {
		wg.Go(func() {
			code, _ := r.json("u1", "/api/plugins/admin.rewards/checkin", `{}`)
			mu.Lock()
			codes[code]++
			mu.Unlock()
		})
	}
	wg.Wait()
	if codes[http.StatusOK] != 1 || codes[http.StatusConflict] != 4 {
		t.Fatalf("answers %v, want one 200 and four 409", codes)
	}
	if a := r.account(); a.BalanceMicro != 200_000 {
		t.Fatalf("balance %d after checking in", a.BalanceMicro)
	}
	code, me := r.json("u1", "/api/plugins/admin.rewards/me", "")
	ci, _ := me["checkin"].(map[string]any)
	if code != 200 || ci["done_today"] != true || ci["streak"] != float64(1) {
		t.Fatalf("me: %d %v", code, me)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A coupon lowers what the provider charges, the full amount is credited,
// and the inviter of the payer is rewarded once, however often the
// provider calls back.
func TestSellKitCouponAndReferral(t *testing.T) {
	fake := newFakeStripeSubs(t)
	r := newSellRelay(t, "admin.signup")
	ctx := context.Background()
	if err := r.st.CreateUser(ctx, &store.User{ID: "inviter", Name: "Ivy", Email: "ivy@example.com", Role: store.RoleUser}, "correct-horse-3"); err != nil {
		t.Fatal(err)
	}
	r.paySettings(map[string]any{"stripe_secret_key": "sk_test_123", "stripe_webhook_secret": "whsec_topups",
		"coupons_enabled": true})
	r.pluginSettings("admin.rewards", map[string]any{"referral_enabled": true, "referral_mode": "percent", "referral_percent": 10.0,
		"referral_cap_micro": 50_000_000})

	_, me := r.json("inviter", "/api/plugins/admin.rewards/me", "")
	ref, _ := me["referral"].(map[string]any)
	code, _ := ref["code"].(string)
	if code == "" {
		t.Fatalf("no referral code: %v", me)
	}
	c, _, body := r.post("/api/plugins/admin.signup/register", `{"email":"new@example.com","password":"correct-horse-9","referral_code":"`+code+`"}`, nil)
	if c != http.StatusCreated {
		t.Fatalf("register: %d %s", c, body)
	}
	var reg struct {
		UserID string `json:"user_id"`
	}
	_ = json.Unmarshal([]byte(body), &reg)

	if c, out := r.json("admin", "/api/plugins/admin.payment/coupons", `{"code":"spring","percent":20,"max_uses":1}`); c != http.StatusCreated {
		t.Fatalf("coupon: %d %v", c, out)
	}
	if c, out := r.json(reg.UserID, "/api/plugins/admin.payment/coupon-check", `{"code":"SPRING","amount_micro":50000000}`); c != 200 ||
		out["discount"].(map[string]any)["micro"] != float64(10_000_000) {
		t.Fatalf("coupon check: %d %v", c, out)
	}
	c, out := r.json(reg.UserID, "/api/plugins/admin.payment/checkout", `{"amount_micro":50000000,"coupon":"spring"}`)
	if c != http.StatusCreated {
		t.Fatalf("checkout: %d %v", c, out)
	}
	order := out["order_id"].(string)
	fake.mu.Lock()
	charged := fake.sessions[len(fake.sessions)-1].Get("line_items[0][price_data][unit_amount]")
	fake.mu.Unlock()
	if charged != "4000" {
		t.Fatalf("Stripe charges %s cents, want 4000", charged)
	}
	if c, out := r.json("u1", "/api/plugins/admin.payment/checkout", `{"amount_micro":50000000,"coupon":"spring"}`); c != http.StatusUnprocessableEntity || !strings.Contains(toJSON(out), "coupon_used_up") {
		t.Fatalf("second use: %d %v", c, out)
	}

	ev := checkoutEvent("evt_1", order, 4000)
	for range 2 {
		c, _, body := r.post("/api/plugins/admin.payment/webhook/stripe", string(ev), map[string]string{"Stripe-Signature": stripe.Sign("whsec_topups", ev, time.Now())})
		if c != 200 {
			t.Fatalf("webhook: %d %s", c, body)
		}
	}
	// Another event for the same order, as Stripe may send.
	ev2 := checkoutEvent("evt_2", order, 4000)
	if c, _, body := r.post("/api/plugins/admin.payment/webhook/stripe", string(ev2), map[string]string{"Stripe-Signature": stripe.Sign("whsec_topups", ev2, time.Now())}); c != 200 {
		t.Fatalf("webhook: %d %s", c, body)
	}
	a, _ := r.l.Account(ctx, "user:"+reg.UserID)
	if a.BalanceMicro != 50_000_000 {
		t.Fatalf("invitee credited %d, want the full 50", a.BalanceMicro)
	}
	inv, _ := r.l.Account(ctx, "user:inviter")
	if inv.BalanceMicro != 4_000_000 {
		t.Fatalf("inviter got %d, want 10%% of the 40 paid", inv.BalanceMicro)
	}
	_, me = r.json("inviter", "/api/plugins/admin.rewards/me", "")
	if ref := me["referral"].(map[string]any); ref["invited"] != float64(1) || ref["earned"].(map[string]any)["micro"] != float64(4_000_000) {
		t.Fatalf("inviter's page: %v", ref)
	}

	c, list := r.adminCall("GET", "/api/plugins/admin.payment/orders?status=paid", "")
	items, _ := list["items"].([]any)
	if c != 200 || len(items) != 1 || items[0].(map[string]any)["coupon_code"] != "SPRING" {
		t.Fatalf("orders: %d %v", c, list)
	}
	if tot := list["totals"].(map[string]any); tot["paid_micro"] != float64(40_000_000) {
		t.Fatalf("totals: %v", tot)
	}
}

// A checkout the user walks away from gives its coupon back once Stripe
// has expired the session.
func TestSellKitCanceledOrderFreesCoupon(t *testing.T) {
	f := newFakeStripeSubs(t)
	r := newSellRelay(t)
	r.paySettings(map[string]any{"stripe_secret_key": "sk_test_123", "stripe_webhook_secret": "whsec_topups",
		"coupons_enabled": true})
	if c, out := r.json("admin", "/api/plugins/admin.payment/coupons", `{"code":"ONCE","amount_micro":1000000,"max_uses":1}`); c != http.StatusCreated {
		t.Fatalf("coupon: %d %v", c, out)
	}
	_, out := r.json("u1", "/api/plugins/admin.payment/checkout", `{"amount_micro":5000000,"coupon":"once"}`)
	order, _ := out["order_id"].(string)
	if c, out := r.json("u1", "/api/plugins/admin.payment/orders/"+order+"/cancel", `{}`); c != 200 || out["status"] != "canceled" {
		t.Fatalf("cancel: %d %v", c, out)
	}
	if c, out := r.json("u1", "/api/plugins/admin.payment/checkout", `{"amount_micro":5000000,"coupon":"once"}`); c != http.StatusCreated {
		t.Fatalf("checkout after the cancel: %d %v", c, out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.expired) != 1 || f.sessions[0].Get("expires_at") == "" {
		t.Fatalf("expired %v, first session %v", f.expired, f.sessions[0])
	}
}

func TestSellKitAnnouncements(t *testing.T) {
	r := newSellRelay(t)
	r.pluginSettings("admin.announcements", map[string]any{"enabled": true})
	c, out := r.json("admin", "/api/plugins/admin.announcements/announcements",
		`{"title":"Maintenance","body":"**Tonight** 22:00","audience":"members"}`)
	if c != http.StatusCreated {
		t.Fatalf("create: %d %v", c, out)
	}
	id := out["id"].(string)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if c, out := r.json("admin", "/api/plugins/admin.announcements/announcements", `{"title":"Later","starts_at":"`+future+`"}`); c != http.StatusCreated || out["state"] != "scheduled" {
		t.Fatalf("scheduled: %d %v", c, out)
	}
	_, mine := r.json("u1", "/api/plugins/admin.announcements/me", "")
	if items := mine["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != id {
		t.Fatalf("member sees %v", mine)
	}
	if _, adm := r.json("admin", "/api/plugins/admin.announcements/me", ""); len(adm["items"].([]any)) != 0 {
		t.Fatalf("admin sees a members-only notice: %v", adm)
	}
	if c, _ := r.consolePost("u1", "/api/plugins/admin.announcements/me/dismiss", `{"id":"`+id+`"}`); c != http.StatusNoContent {
		t.Fatalf("dismiss: %d", c)
	}
	if _, mine := r.json("u1", "/api/plugins/admin.announcements/me", ""); len(mine["items"].([]any)) != 0 {
		t.Fatalf("dismissed notice still shown: %v", mine)
	}
	if c, _ := r.consolePost("u1", "/api/plugins/admin.announcements/announcements", `{"title":"x"}`); c != http.StatusForbidden {
		t.Fatalf("member wrote an announcement: %d", c)
	}
}
