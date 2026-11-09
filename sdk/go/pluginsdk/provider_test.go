package pluginsdk

import (
	"context"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// The PROTOCOL.md methods without another test: provider.endpoints,
// provider.refresh, an error event from provider.send, host.log,
// host.kv.delete, and a text body in an admin.handle response.
func TestProtocolSurface(t *testing.T) {
	p := New("demo", "1.0.0")
	p.Endpoints(func(_ context.Context, u *Upstream) ([]plugin.Endpoint, []string, error) {
		return []plugin.Endpoint{{ID: u.ID + "-chat", Protocol: "openai-chat", BaseURL: "https://api.example.com"}}, []string{"X-Owned"}, nil
	})
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	p.Refresh(func(_ context.Context, u *Upstream, c *Credential) (string, time.Time, error) {
		return "new-" + c.Secret, expires, nil
	})
	p.Send(func(_ context.Context, _ *Outgoing, _ *ir.Request, emit Emit) (*Outcome, error) {
		if err := emit(StartEvent("r1", "m")); err != nil {
			return nil, err
		}
		return nil, emit(ErrorEvent(ir.ErrUpstream, "cut off"))
	})
	p.AdminHandle(func(ctx context.Context, route string, r *HTTPRequest) (*HTTPResponse, error) {
		if err := p.Host().Log(ctx, "info", "deleting", map[string]any{"route": route}); err != nil {
			return nil, err
		}
		if err := p.Host().KVDelete(ctx, "k"); err != nil {
			return nil, err
		}
		return &HTTPResponse{Status: 200, Body: TextBody("deleted")}, nil
	})
	c := serve(t, p)
	c.call("initialize", map[string]any{"protocol_version": 1}, nil)

	r := c.call("provider.endpoints", map[string]any{"upstream": map[string]any{"id": "u"}}, nil)["result"].(map[string]any)
	eps := r["endpoints"].([]any)
	if len(eps) != 1 || eps[0].(map[string]any)["id"] != "u-chat" || r["owned_headers"].([]any)[0] != "X-Owned" {
		t.Fatalf("provider.endpoints %v", r)
	}

	r = c.call("provider.refresh", map[string]any{"upstream": map[string]any{"id": "u"},
		"credential": map[string]any{"id": "c", "secret": "old"}}, nil)["result"].(map[string]any)
	if r["secret"] != "new-old" || r["expires"] != expires.Format(time.RFC3339) {
		t.Fatalf("provider.refresh %v", r)
	}

	var events []map[string]any
	r = c.call("provider.send", map[string]any{"token": "tok", "outgoing": map[string]any{}}, func(m map[string]any) {
		if m["method"] != "$/event" || m["params"].(map[string]any)["token"] != "tok" {
			t.Fatalf("unexpected %v", m)
		}
		events = append(events, m["params"].(map[string]any)["event"].(map[string]any))
	})["result"].(map[string]any)
	if len(events) != 2 || events[1]["kind"] != "error" || events[1]["error"].(map[string]any)["kind"] != "upstream" ||
		events[1]["error"].(map[string]any)["message"] != "cut off" || r["outcome"] != nil {
		t.Fatalf("provider.send events %v result %v", events, r)
	}

	var hostCalls []string
	r = c.call("admin.handle", map[string]any{"route": "/x", "request": map[string]any{}}, func(m map[string]any) {
		params := m["params"].(map[string]any)
		switch m["method"] {
		case "host.log":
			if params["level"] != "info" || params["message"] != "deleting" || params["attrs"].(map[string]any)["route"] != "/x" {
				t.Fatalf("host.log %v", params)
			}
		case "host.kv.delete":
			if params["key"] != "k" {
				t.Fatalf("host.kv.delete %v", params)
			}
		default:
			t.Fatalf("unexpected %v", m)
		}
		hostCalls = append(hostCalls, m["method"].(string))
		c.send(map[string]any{"id": m["id"], "result": map[string]any{}})
	})["result"].(map[string]any)
	if len(hostCalls) != 2 || r["status"] != 200.0 || r["body"] != "deleted" {
		t.Fatalf("admin.handle %v, host calls %v", r, hostCalls)
	}
}
