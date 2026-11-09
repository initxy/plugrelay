package router

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

func TestPlanUpstream(t *testing.T) {
	tb := baseTable()
	tb.Upstreams["a"] = Upstream{ID: "a", Enabled: true, Endpoints: []plugin.Endpoint{ep("a-chat", "openai_chat"), ep("a-msg", "anthropic")},
		Credentials: []Cred{{ID: "a1", Kind: KindShared}, {ID: "a2", Kind: KindShared}, {ID: "mine", Kind: KindPersonal, OwnerID: "u1"},
			{ID: "theirs", Kind: KindPersonal, OwnerID: "u2"}, {ID: "off", Kind: KindShared, Status: StatusDisabled}}}
	tb.Upstreams["d"] = Upstream{ID: "d", Enabled: false, Endpoints: []plugin.Endpoint{ep("d-chat", "openai_chat")}, Credentials: shared("d1")}
	r, clk, _ := newRouter(t, tb)
	me := plugin.Caller{KeyID: "k1", UserID: "u1"}

	p, err := r.PlanUpstream(ctx, "a", "a-msg", me)
	if err != nil {
		t.Fatal(err)
	}
	got := ups(p)
	slices.Sort(got)
	if !slices.Equal(got, []string{"a/a-msg/a1", "a/a-msg/a2", "a/a-msg/mine"}) {
		t.Fatalf("candidates %v", got)
	}
	for _, c := range p.Candidates {
		if c.Protocol != "anthropic" || c.UpstreamModel != "" || !c.SameProtocol {
			t.Errorf("candidate %+v", c)
		}
	}

	for _, tc := range []struct{ up, ep string }{{"nope", "x"}, {"a", "nope"}, {"d", "d-chat"}} {
		if _, err := r.PlanUpstream(ctx, tc.up, tc.ep, me); !errors.Is(err, &PlanError{Kind: ErrNoRoute}) {
			t.Errorf("%s/%s: %v", tc.up, tc.ep, err)
		}
	}

	// Cooling credentials are skipped; when all cool, the error says so.
	for _, id := range []string{"a1", "a2", "mine"} {
		_ = r.Report(ctx, plugin.Candidate{UpstreamID: "a", EndpointID: "a-chat", CredentialID: id},
			plugin.Verdict{Cause: plugin.CauseQuota, Credential: plugin.CredCooldown, Until: clk.Now().Add(time.Minute)}, "")
	}
	var pe *PlanError
	if _, err := r.PlanUpstream(ctx, "a", "a-chat", me); !errors.As(err, &pe) || pe.Kind != ErrCooling || pe.RetryAt.IsZero() {
		t.Fatalf("all cooling: %v", err)
	}
	clk.Advance(2 * time.Minute)
	if _, err := r.PlanUpstream(ctx, "a", "a-chat", me); err != nil {
		t.Fatalf("after cooldown: %v", err)
	}
}
