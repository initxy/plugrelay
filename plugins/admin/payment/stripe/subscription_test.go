package stripe_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/initxy/plugrelay/plugins/admin/payment/stripe"
)

func TestVerifySubscriptionEvents(t *testing.T) {
	p := &stripe.Provider{SubscriptionWebhookSecret: "whsec_sub", WebhookSecret: "whsec_topup"}
	now := time.Now()
	verify := func(body string, secret string) (string, string, error) {
		h := http.Header{"Stripe-Signature": {stripe.Sign(secret, []byte(body), now)}}
		ev, err := p.VerifySubscription(h, []byte(body), now)
		return ev.Kind, ev.Ref, err
	}
	// The top-up endpoint's secret does not sign subscription events.
	if _, _, err := verify(`{"id":"evt_0","type":"invoice.paid","data":{"object":{}}}`, "whsec_topup"); err == nil {
		t.Fatal("accepted the top-up endpoint's signature")
	}
	for name, c := range map[string]struct{ body, kind, ref string }{
		// A one-off invoice (no subscription) starts nothing.
		"one-off invoice": {`{"id":"evt_1","type":"invoice.paid","data":{"object":{"object":"invoice","id":"in_1","amount_paid":500,"currency":"usd","parent":null,"lines":{"data":[{"type":"invoiceitem","period":{"start":1,"end":2}}]}}}}`, "", ""},
		// Expanded subscription objects are read by id.
		"expanded":   {`{"id":"evt_2","type":"invoice.paid","data":{"object":{"object":"invoice","id":"in_2","amount_paid":500,"currency":"usd","subscription":{"id":"sub_9"},"lines":{"data":[{"type":"subscription","period":{"start":1,"end":2}}]}}}}`, "paid", "sub_9"},
		"past due":   {`{"id":"evt_3","type":"customer.subscription.updated","data":{"object":{"object":"subscription","id":"sub_9","status":"past_due","cancel_at_period_end":false}}}`, "updated", "sub_9"},
		"unpaid":     {`{"id":"evt_4","type":"customer.subscription.updated","data":{"object":{"object":"subscription","id":"sub_9","status":"unpaid"}}}`, "ended", "sub_9"},
		"other type": {`{"id":"evt_5","type":"invoice.payment_failed","data":{"object":{"object":"invoice","id":"in_5"}}}`, "", ""},
	} {
		kind, ref, err := verify(c.body, "whsec_sub")
		if err != nil || kind != c.kind || ref != c.ref {
			t.Errorf("%s: kind %q ref %q err %v", name, kind, ref, err)
		}
	}
	if err := p.CancelAtPeriodEnd(t.Context(), "sub_1/../x", true); err == nil {
		t.Error("accepted a path in a subscription id")
	}
}
