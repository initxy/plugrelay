package extplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/sdk/go/pluginsdk"
)

// TestMain doubles as the helper plugin: the manifest under test runs this
// binary with EXTPLUGIN_TEST_HELPER set.
func TestMain(m *testing.M) {
	if os.Getenv("EXTPLUGIN_TEST_HELPER") != "" {
		if err := helperPlugin().Serve(); err != nil {
			fmt.Fprintln(os.Stderr, "helper:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helperPlugin() *pluginsdk.Plugin {
	id := os.Getenv("EXTPLUGIN_TEST_ID")
	p := pluginsdk.New(id, "1.0.0")
	p.OnInit = func(ctx context.Context, settings map[string]json.RawMessage) error {
		fmt.Fprintln(os.Stderr, "helper started")
		return nil
	}
	p.Authenticate(func(ctx context.Context, r *pluginsdk.HTTPRequest) (*plugin.Caller, error) {
		switch http.Header(r.Header).Get("X-Key") {
		case "good":
			return &plugin.Caller{KeyID: "k1", UserID: "u1"}, nil
		case "bad":
			return nil, pluginsdk.Reject(ir.ErrAuthentication, "bad key")
		}
		return nil, nil
	})
	p.OnRequest(func(ctx context.Context, c *pluginsdk.Call) (*pluginsdk.Reply, error) {
		switch c.Model {
		case "deny":
			return pluginsdk.Deny(ir.ErrPermission, "denied by helper"), nil
		case "sleep":
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
			}
		case "crash":
			os.Exit(3)
		case "fail":
			return nil, fmt.Errorf("helper failure")
		case "setting":
			return pluginsdk.Deny(ir.ErrInvalidRequest, c.Setting("greeting").String()), nil
		case "headers":
			return pluginsdk.Deny(ir.ErrInvalidRequest, c.Header.Get("Authorization")+"|"+c.Header.Get("User-Agent")), nil
		}
		return nil, c.SetState(map[string]int{"n": 7})
	})
	p.OnAttempt(func(ctx context.Context, c *pluginsdk.Call, a *pluginsdk.Attempt) (map[string]string, error) {
		return map[string]string{"X-State": string(c.State), "Authorization": "Bearer stolen", "X-Upstream": a.Candidate.UpstreamID}, nil
	})
	p.OnFinish(func(ctx context.Context, c *pluginsdk.Call, r *pluginsdk.Result) error {
		_, err := p.Host().KVIncr(ctx, "finished", 1, 0)
		return err
	})
	p.Rank(func(ctx context.Context, c *pluginsdk.Call, cands []pluginsdk.Candidate) ([]int, error) {
		order := []int{99, -1}
		for i := len(cands) - 1; i >= 1; i-- {
			order = append(order, i, i)
		}
		return order, nil
	})
	p.Meter(
		func(ctx context.Context, c *pluginsdk.Call, est int64) (*pluginsdk.Reservation, error) {
			if est > 100 {
				return nil, pluginsdk.Reject(ir.ErrQuotaExhausted, "balance too low")
			}
			return &pluginsdk.Reservation{ID: "r-" + c.ID, AmountMicro: est}, nil
		},
		func(ctx context.Context, c *pluginsdk.Call, r *pluginsdk.Reservation, u ir.Usage, cost int64) error {
			return p.Host().KVPut(ctx, "settled/"+r.ID, strconv.FormatInt(cost, 10), 0)
		},
		func(ctx context.Context, c *pluginsdk.Call, r *pluginsdk.Reservation) error { return nil },
	)
	p.Consume(func(ctx context.Context, r *plugin.Record) error {
		return p.Host().KVPut(ctx, "record", r.ID, 0)
	})
	p.RunJob(func(ctx context.Context) error {
		resp, err := p.Host().HTTP(ctx, &pluginsdk.HTTPRequest{Method: "GET", URL: os.Getenv("EXTPLUGIN_TEST_URL")})
		if err != nil {
			return err
		}
		return p.Host().KVPut(ctx, "job", resp.Text(), 0)
	})
	p.AdminHandle(func(ctx context.Context, route string, r *pluginsdk.HTTPRequest) (*pluginsdk.HTTPResponse, error) {
		body := route + ":" + r.Method + ":" + *r.Body
		return &pluginsdk.HTTPResponse{Status: 201, Header: http.Header{"X-Plugin": {"yes"}}, Body: &body}, nil
	})
	p.Prepare(func(ctx context.Context, o *pluginsdk.Outgoing) error {
		h := http.Header(o.Header)
		h.Set("Authorization", "Bearer "+o.Credential.Secret)
		h.Del("X-Api-Key")
		o.Header = h
		return nil
	})
	p.Classify(func(ctx context.Context, o *pluginsdk.Outcome) (*pluginsdk.Verdict, error) {
		if o.Status == 429 {
			return &pluginsdk.Verdict{Cause: plugin.CauseRateLimit, Retry: plugin.RetryNext, Credential: plugin.CredCooldown, Sent: o.Sent,
				ClientError: &pluginsdk.Error{Kind: ir.ErrRateLimited, Message: o.Body, RetryAfterMs: 1500}}, nil
		}
		return &pluginsdk.Verdict{Cause: plugin.CauseUnknown, Retry: plugin.RetryNone, Credential: plugin.CredQuarantine, Sent: o.Sent}, nil
	})
	p.Send(func(ctx context.Context, o *pluginsdk.Outgoing, req *ir.Request, emit pluginsdk.Emit) (*pluginsdk.Outcome, error) {
		switch o.Model {
		case "send-429":
			return &pluginsdk.Outcome{Status: 429, Body: "slow down", Sent: true}, nil
		case "send-crash":
			_ = emit(pluginsdk.StartEvent("x", o.Model))
			for _, ev := range pluginsdk.TextBlockEvents(0, "partial") {
				_ = emit(ev)
			}
			time.Sleep(50 * time.Millisecond)
			os.Exit(4)
		case "send-noend":
			return nil, nil
		}
		var body struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(o.Body, &body)
		if err := emit(pluginsdk.StartEvent("resp-1", o.Model)); err != nil {
			return nil, err
		}
		for _, ev := range pluginsdk.TextBlockEvents(0, body.Text) {
			if err := emit(ev); err != nil {
				return nil, err
			}
		}
		return nil, emit(pluginsdk.DoneEvent(ir.StopEndTurn, &ir.Usage{Input: 1, Output: 2}))
	})
	p.ListModels(func(ctx context.Context, u *pluginsdk.Upstream, c *pluginsdk.Credential) ([]ir.ModelInfo, error) {
		return []ir.ModelInfo{{ID: "m1"}}, nil
	})
	return p
}

// ---------------------------------------------------------------- test host

type memKV struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (k *memKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok, nil
}

func (k *memKV) Put(_ context.Context, key string, v []byte, _ time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[key] = v
	return nil
}

func (k *memKV) Delete(_ context.Context, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.m, key)
	return nil
}

func (k *memKV) Incr(_ context.Context, key string, d int64, _ time.Duration) (int64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	n, _ := strconv.ParseInt(string(k.m[key]), 10, 64)
	n += d
	k.m[key] = []byte(strconv.FormatInt(n, 10))
	return n, nil
}

func (k *memKV) get(key string) string {
	v, _, _ := k.Get(context.Background(), key)
	return string(v)
}

type mapSettings map[string]json.RawMessage

func (s mapSettings) Get(key string) plugin.Value { return plugin.NewValue(s[key]) }

type testHost struct {
	kv  *memKV
	log *slog.Logger
}

func (h *testHost) Logger() *slog.Logger     { return h.log }
func (h *testHost) HTTPClient() *http.Client { return http.DefaultClient }
func (h *testHost) ClientFor(*plugin.Upstream, *plugin.Credential) (*http.Client, error) {
	return http.DefaultClient, nil
}
func (h *testHost) KV() plugin.KV             { return h.kv }
func (h *testHost) Settings() plugin.Settings { return mapSettings{} }
func (h *testHost) Usage() plugin.UsageReader { return nil }
func (h *testHost) Version() string           { return "test" }

// logBuffer collects log output for assertions.
type logBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

var _ io.Writer = (*logBuffer)(nil)
