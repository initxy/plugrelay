package extplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

var allPoints = []string{
	"auth.authenticate", "middleware.onRequest", "middleware.onAttempt", "middleware.onFinish",
	"selector.rank", "meter.reserve", "meter.settle", "meter.refund",
	"sink.consume", "job.run", "admin.handle", "provider.prepare", "provider.classify",
	"provider.send", "provider.listModels",
}

func helperManifest(t *testing.T) *Manifest {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Manifest{
		ID: "helper", Version: "1.0.0",
		Kinds: []plugin.Kind{
			plugin.KindAuth, plugin.KindMiddleware, plugin.KindSelector, plugin.KindMeter,
			plugin.KindSink, plugin.KindJob, plugin.KindAdmin, plugin.KindProvider,
		},
		Points:      append([]string(nil), allPoints...),
		Command:     exe,
		Env:         map[string]string{"EXTPLUGIN_TEST_HELPER": "1", "EXTPLUGIN_TEST_ID": "helper"},
		Timeouts:    Timeouts{Call: Duration(time.Second), Startup: Duration(5 * time.Second), Shutdown: Duration(2 * time.Second)},
		Settings:    []SettingSpec{{Key: "greeting", Type: plugin.TypeString, Scopes: []string{"global", "key"}}},
		Permissions: Permissions{Host: []string{PermKV, PermHTTP}},
		Provider:    &ProviderSpec{OwnedHeaders: []string{"Authorization", "X-Api-Key"}},
		Job:         &JobSpec{Every: Duration(time.Minute)},
		Admin:       &AdminSpec{Routes: []RouteSpec{{Method: "POST", Path: "/echo", Role: "admin"}}},
		Dir:         t.TempDir(),
	}
}

type fixture struct {
	m    *Manager
	kv   *memKV
	logs *logBuffer
	man  *Manifest
}

func start(t *testing.T, mutate func(*Manifest)) *fixture {
	t.Helper()
	man := helperManifest(t)
	if mutate != nil {
		mutate(man)
	}
	f := &fixture{kv: &memKV{m: map[string][]byte{}}, logs: &logBuffer{}, man: man}
	logger := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.m = NewManager(Options{
		HostVersion: "test", Logger: logger, BackoffMin: 50 * time.Millisecond, BackoffMax: 200 * time.Millisecond,
		Host: func(id string) plugin.Host { return &testHost{kv: f.kv, log: logger} },
	})
	if err := f.m.Add(man); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.m.Stop(ctx)
	})
	return f
}

func adapter[T any](t *testing.T, f *fixture) T {
	t.Helper()
	for _, p := range f.m.Plugins(f.man.ID) {
		if a, ok := p.(T); ok {
			return a
		}
	}
	var zero T
	t.Fatalf("no adapter of type %T", zero)
	return zero
}

func newCall(model string, settings map[string]json.RawMessage) *plugin.Call {
	c := plugin.NewCall(context.Background(), "req-1", func(_, key string) plugin.Value {
		return plugin.NewValue(settings[key])
	}, nil)
	c.Model = model
	c.Header = http.Header{"Authorization": {"Bearer client-key"}, "User-Agent": {"cli/1"}}
	return c.WithPlugin("middleware.helper")
}

func waitState(t *testing.T, f *fixture, want State) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := f.m.Status(f.man.ID)
		if st.State == want {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := f.m.Status(f.man.ID)
	t.Fatalf("state %s, want %s", st.State, want)
	return st
}

func TestHandshakeAndStatus(t *testing.T) {
	f := start(t, nil)
	st, ok := f.m.Status("helper")
	if !ok || st.State != StateRunning || st.PID == 0 {
		t.Fatalf("status %+v", st)
	}
	if len(f.m.List()) != 1 || len(f.m.Plugins("helper")) != 8 {
		t.Fatalf("list %d plugins %d", len(f.m.List()), len(f.m.Plugins("helper")))
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(f.logs.String(), "helper started") {
		if time.Now().After(deadline) {
			t.Fatalf("stderr not captured: %s", f.logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHandshakeRejectsWrongID(t *testing.T) {
	man := helperManifest(t)
	man.Env["EXTPLUGIN_TEST_ID"] = "someone-else"
	m := NewManager(Options{Logger: slog.New(slog.DiscardHandler), BackoffMin: time.Hour})
	if err := m.Add(man); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())
	err := m.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "someone-else") {
		t.Fatalf("err = %v", err)
	}
	if st, _ := m.Status("helper"); st.State != StateBackoff {
		t.Fatalf("state %s", st.State)
	}
}

func TestHandshakeRejectsMissingPoint(t *testing.T) {
	man := helperManifest(t)
	man.Points = append(man.Points, "provider.refresh")
	m := NewManager(Options{Logger: slog.New(slog.DiscardHandler), BackoffMin: time.Hour})
	if err := m.Add(man); err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background())
	if err := m.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "provider.refresh") {
		t.Fatalf("err = %v", err)
	}
}

func TestMiddleware(t *testing.T) {
	f := start(t, nil)
	mw := adapter[*Middleware](t, f)
	if info := mw.Info(); info.ID != "middleware.helper" || info.FailPolicy != plugin.FailClosed || len(info.Settings) != 1 {
		t.Fatalf("info %+v", info)
	}

	reply, err := mw.OnRequest(newCall("deny", nil))
	if err != nil || reply == nil || reply.Error.Kind != ir.ErrPermission {
		t.Fatalf("deny: %v %+v", err, reply)
	}

	reply, _ = mw.OnRequest(newCall("setting", map[string]json.RawMessage{"greeting": json.RawMessage(`"hi"`)}))
	if reply.Error.Message != "hi" {
		t.Fatalf("setting not passed: %q", reply.Error.Message)
	}

	reply, _ = mw.OnRequest(newCall("headers", nil))
	if reply.Error.Message != "|cli/1" {
		t.Fatalf("credential header leaked to plugin: %q", reply.Error.Message)
	}

	c := newCall("ok", nil)
	if reply, err := mw.OnRequest(c); err != nil || reply != nil {
		t.Fatalf("pass: %v %+v", err, reply)
	}
	a := &plugin.Attempt{N: 1, Candidate: plugin.Candidate{UpstreamID: "up1"}, Header: http.Header{}}
	if err := mw.OnAttempt(c, a); err != nil {
		t.Fatal(err)
	}
	if a.Header.Get("X-State") != `{"n":7}` || a.Header.Get("X-Upstream") != "up1" {
		t.Fatalf("attempt header %v", a.Header)
	}
	if a.Header.Get("Authorization") != "" {
		t.Fatal("middleware set a credential header")
	}

	mw.OnFinish(c, &plugin.Result{Status: 200})
	if f.kv.get("finished") != "1" {
		t.Fatalf("onFinish did not reach host kv: %q", f.kv.get("finished"))
	}

	_, err = mw.OnRequest(newCall("fail", nil))
	if !plugin.IsFailure(err) || !errors.Is(err, ErrPlugin) {
		t.Fatalf("plugin error: %v", err)
	}
}

func TestAuth(t *testing.T) {
	f := start(t, nil)
	auth := adapter[*Authenticator](t, f)
	req := func(key string) *http.Request {
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		r.Header.Set("X-Key", key)
		return r
	}
	c, err := auth.Authenticate(req("good"))
	if err != nil || c == nil || c.KeyID != "k1" {
		t.Fatalf("good: %v %+v", err, c)
	}
	c, err = auth.Authenticate(req(""))
	if err != nil || c != nil {
		t.Fatalf("pass-through: %v %+v", err, c)
	}
	_, err = auth.Authenticate(req("bad"))
	var gw *ir.GwError
	if !errors.As(err, &gw) || gw.Kind != ir.ErrAuthentication || plugin.IsFailure(err) {
		t.Fatalf("reject: %v", err)
	}
}

func TestSelectorMeterSinkJobAdmin(t *testing.T) {
	f := start(t, nil)
	cands := []plugin.Candidate{{UpstreamID: "a"}, {UpstreamID: "b"}, {UpstreamID: "c"}}
	got := adapter[*Selector](t, f).Rank(newCall("m", nil), cands)
	if ids := got[0].UpstreamID + got[1].UpstreamID + got[2].UpstreamID; ids != "cba" {
		t.Fatalf("rank %s", ids)
	}

	meter := adapter[*Meter](t, f)
	c := newCall("m", nil)
	r, err := meter.Reserve(c, 50)
	if err != nil || r.ID != "r-req-1" {
		t.Fatalf("reserve %v %+v", err, r)
	}
	if err := meter.Settle(c, r, ir.Usage{Input: 1}, 42); err != nil || f.kv.get("settled/r-req-1") != "42" {
		t.Fatalf("settle %v", err)
	}
	if err := meter.Refund(c, r); err != nil {
		t.Fatal(err)
	}
	_, err = meter.Reserve(c, 500)
	var gw *ir.GwError
	if !errors.As(err, &gw) || gw.Kind != ir.ErrQuotaExhausted {
		t.Fatalf("quota: %v", err)
	}

	sink := adapter[*Sink](t, f)
	if sink.Info().FailPolicy != plugin.FailOpen {
		t.Fatal("sinks default to fail open")
	}
	if err := sink.Consume(context.Background(), &plugin.Record{ID: "rec-9"}); err != nil || f.kv.get("record") != "rec-9" {
		t.Fatalf("sink %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "pong") }))
	defer upstream.Close()
	f.man.Env["EXTPLUGIN_TEST_URL"] = upstream.URL // read by the restarted process below
	job := adapter[*Job](t, f)
	if job.Every() != time.Minute {
		t.Fatal("every")
	}

	admin := adapter[*Admin](t, f)
	routes := admin.Routes()
	if len(routes) != 1 || routes[0].Role != "admin" {
		t.Fatalf("routes %+v", routes)
	}
	rec := httptest.NewRecorder()
	routes[0].Handler(rec, httptest.NewRequest("POST", "/api/plugins/helper/echo", strings.NewReader("hello")))
	if rec.Code != 201 || rec.Body.String() != "/echo:POST:hello" || rec.Header().Get("X-Plugin") != "yes" {
		t.Fatalf("admin %d %q", rec.Code, rec.Body)
	}

	// The job needs the upstream URL in its environment: restart the process.
	ctx := context.Background()
	if err := f.m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := job.Run(ctx); err != nil || f.kv.get("job") != "pong" {
		t.Fatalf("job %v %q", err, f.kv.get("job"))
	}
}

func TestHostPermissions(t *testing.T) {
	f := start(t, func(m *Manifest) { m.Permissions.Host = nil })
	err := adapter[*Sink](t, f).Consume(context.Background(), &plugin.Record{ID: "x"})
	if err == nil || !strings.Contains(err.Error(), "permissions.host") {
		t.Fatalf("kv without permission: %v", err)
	}
}

func TestProvider(t *testing.T) {
	f := start(t, nil)
	p := adapter[*Provider](t, f)
	u := &plugin.Upstream{ID: "up", Endpoints: []plugin.Endpoint{{ID: "e", Protocol: "openai-chat"}}}
	eps, err := p.Endpoints(u)
	if err != nil || len(eps) != 1 {
		t.Fatalf("endpoints %v %v", eps, err)
	}
	for _, a := range f.m.Plugins("helper") {
		s, ok := a.(plugin.Supporter)
		if !ok {
			t.Fatalf("%T does not implement plugin.Supporter", a)
		}
		if s.Supports("provider.refresh") || !s.Supports("provider.send") {
			t.Fatal("Supports must follow the manifest points")
		}
	}
	if _, ok := any(p).(plugin.Sender); !ok {
		t.Fatal("provider adapter is not a plugin.Sender")
	}
	if h := p.OwnedHeaders(u); len(h) != 2 {
		t.Fatalf("owned %v", h)
	}
	req, _ := http.NewRequest("POST", "https://upstream.test/v1/chat/completions", strings.NewReader(`{"text":"hi"}`))
	req.Header.Set("Authorization", "Bearer client")
	req.Header.Set("X-Api-Key", "client")
	o := &plugin.Outgoing{Upstream: u, Endpoint: &eps[0], Credential: &plugin.Credential{ID: "c", Secret: "sk-up"}, Model: "m", Req: req}
	if err := p.Prepare(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer sk-up" || req.Header.Get("X-Api-Key") != "" {
		t.Fatalf("prepare header %v", req.Header)
	}

	v := p.Classify(&plugin.Outcome{Status: 429, Body: []byte("slow"), Sent: true})
	if v.Cause != plugin.CauseRateLimit || v.Retry != plugin.RetryNext || v.ClientErr.RetryAfter != 1500*time.Millisecond {
		t.Fatalf("verdict %+v", v)
	}
	models, err := p.ListModels(context.Background(), u, o.Credential)
	if err != nil || models[0].ID != "m1" {
		t.Fatalf("models %v %v", models, err)
	}
	if _, _, err := p.Refresh(context.Background(), u, o.Credential); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("refresh not implemented: %v", err)
	}
}

func collect(evs *[]*ir.Event) func(*ir.Event) error {
	return func(ev *ir.Event) error { *evs = append(*evs, ev); return nil }
}

func outgoing(model, body string) *plugin.Outgoing {
	req, _ := http.NewRequest("POST", "https://upstream.test/x", bytes.NewReader([]byte(body)))
	return &plugin.Outgoing{Model: model, Req: req, Credential: &plugin.Credential{ID: "c"}}
}

func TestProviderSend(t *testing.T) {
	f := start(t, nil)
	p := adapter[*Provider](t, f)

	var evs []*ir.Event
	out, err := p.Send(context.Background(), outgoing("m", `{"text":"hello"}`), nil, collect(&evs))
	if err != nil || out != nil {
		t.Fatalf("send %v %+v", err, out)
	}
	kinds := ""
	for _, ev := range evs {
		kinds += string(ev.Kind) + ","
	}
	if kinds != "start,block_start,delta,block_stop,done," || evs[2].Delta.Text != "hello" {
		t.Fatalf("events %s", kinds)
	}

	out, err = p.Send(context.Background(), outgoing("send-429", `{}`), nil, collect(&evs))
	if err != nil || out == nil || out.Status != 429 || string(out.Body) != "slow down" {
		t.Fatalf("outcome %v %+v", err, out)
	}

	evs = nil
	if _, err := p.Send(context.Background(), outgoing("send-noend", `{}`), nil, collect(&evs)); err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != ir.EvError {
		t.Fatalf("missing terminal event not repaired: %+v", evs)
	}

	evs = nil
	out, err = p.Send(context.Background(), outgoing("send-crash", `{}`), nil, collect(&evs))
	if err != nil || out != nil {
		t.Fatalf("crash after content must end in-band: %v %+v", err, out)
	}
	if last := evs[len(evs)-1]; last.Kind != ir.EvError {
		t.Fatalf("last event %s", last.Kind)
	}
	waitState(t, f, StateRunning)
}

func TestTimeout(t *testing.T) {
	f := start(t, func(m *Manifest) {
		m.Timeouts.Call = Duration(200 * time.Millisecond)
		m.Timeouts.Methods = map[string]Duration{"middleware.onRequest": Duration(600 * time.Millisecond)}
		m.MaxConcurrency = 1
	})
	mw := adapter[*Middleware](t, f)
	began := time.Now()
	_, err := mw.OnRequest(newCall("sleep", nil))
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, plugin.ErrPluginFailure) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(began); d < 500*time.Millisecond || d > 1500*time.Millisecond {
		t.Fatalf("timeout took %s", d)
	}

	// With one slot, a call waiting behind a slow one times out too.
	done := make(chan error, 1)
	go func() { _, err := mw.OnRequest(newCall("sleep", nil)); done <- err }()
	time.Sleep(50 * time.Millisecond)
	if _, err := adapter[*Meter](t, f).Reserve(newCall("ok", nil), 1); !errors.Is(err, ErrTimeout) {
		t.Fatalf("queued call: %v", err)
	}
	<-done
	// The process survives timeouts.
	if _, err := mw.OnRequest(newCall("ok", nil)); err != nil {
		t.Fatal(err)
	}
}

func TestCrashRestart(t *testing.T) {
	f := start(t, nil)
	mw := adapter[*Middleware](t, f)
	_, err := mw.OnRequest(newCall("crash", nil))
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, plugin.ErrPluginFailure) || !plugin.IsFailure(err) {
		t.Fatalf("crash: %v", err)
	}
	if errors.Is(err, plugin.ErrPluginStopped) {
		t.Fatalf("a crash is retryable, not stopped: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _ := f.m.Status("helper")
		if st.Restarts >= 1 && st.State == StateRunning && st.LastError == "" && st.PID != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status after restart %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := mw.OnRequest(newCall("ok", nil)); err != nil {
		t.Fatalf("after restart: %v", err)
	}
}

func TestDownCallsFailFast(t *testing.T) {
	f := start(t, func(m *Manifest) { m.FailPolicy = plugin.FailOpen })
	mw := adapter[*Middleware](t, f)
	if mw.Info().FailPolicy != plugin.FailOpen {
		t.Fatal("manifest fail_policy not applied")
	}
	if err := f.m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.m.Status("helper"); st.State != StateStopped {
		t.Fatalf("state %s", st.State)
	}
	began := time.Now()
	_, err := mw.OnRequest(newCall("ok", nil))
	if !errors.Is(err, ErrUnavailable) || time.Since(began) > 100*time.Millisecond {
		t.Fatalf("down call: %v after %s", err, time.Since(began))
	}
	// A stopped plugin is not coming back: retrying callers (the ledger
	// sink queue) must be able to tell.
	if !errors.Is(err, ErrStopped) || !errors.Is(err, plugin.ErrPluginStopped) || !plugin.IsFailure(err) {
		t.Fatalf("stopped call: %v", err)
	}
	v := adapter[*Provider](t, f).Classify(&plugin.Outcome{Status: 500, Sent: true})
	if v.Credential != plugin.CredKeep || v.Retry != plugin.RetryNone {
		t.Fatalf("fallback verdict %+v", v)
	}
	if got := adapter[*Selector](t, f).Rank(newCall("m", nil), []plugin.Candidate{{UpstreamID: "a"}, {UpstreamID: "b"}}); got[0].UpstreamID != "a" {
		t.Fatal("selector failure must keep the router order")
	}
}

func TestUnixTransport(t *testing.T) {
	f := start(t, func(m *Manifest) { m.Transport = TransportUnix })
	reply, err := adapter[*Middleware](t, f).OnRequest(newCall("deny", nil))
	if err != nil || reply == nil {
		t.Fatalf("unix: %v", err)
	}
}

func TestLoadDir(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("good", `
id: good
version: 0.1.0
kind: middleware
points: [middleware.onRequest]
command: ./run.sh
fail_policy: closed
settings:
  - {key: allow, type: list, scopes: [global, group], merge: strictest}
permissions: {host: [kv], network: none}
`)
	write("bad", "id: Bad\nversion: 1\nkind: middleware\npoints: [middleware.onEvent]\ncommand: x\n")
	os.MkdirAll(filepath.Join(root, "not-a-plugin"), 0o755)

	m := NewManager(Options{Logger: slog.New(slog.DiscardHandler)})
	err := m.Load(root)
	if err == nil || !strings.Contains(err.Error(), "onEvent") {
		t.Fatalf("load err = %v", err)
	}
	list := m.List()
	if len(list) != 1 || list[0].ID != "good" || list[0].State != StateStopped {
		t.Fatalf("list %+v", list)
	}
	man := list[0].Manifest
	if man.Timeouts.Call.D() != DefaultCallTimeout || man.MaxConcurrency != DefaultMaxConcurrency || man.Transport != TransportStdio {
		t.Fatalf("defaults %+v", man)
	}
	s := man.PluginSettings()[0]
	if s.Scopes != plugin.Global|plugin.Group || s.Merge != plugin.Strictest {
		t.Fatalf("setting %+v", s)
	}
	if _, err := json.Marshal(list); err != nil {
		t.Fatal(err)
	}
}

func TestManifestValidation(t *testing.T) {
	cases := map[string]string{
		"unknown field":  "id: a\nversion: 1\nkind: sink\npoints: [sink.consume]\ncommand: x\ncolour: red\n",
		"onEvent":        "id: a\nversion: 1\nkind: middleware\npoints: [middleware.onEvent]\ncommand: x\n",
		"point kind":     "id: a\nversion: 1\nkind: sink\npoints: [sink.consume, job.run]\ncommand: x\n",
		"meter partial":  "id: a\nversion: 1\nkind: meter\npoints: [meter.reserve]\ncommand: x\n",
		"unknown kind":   "id: a\nversion: 1\nkind: widget\npoints: [widget.run]\ncommand: x\n",
		"job every":      "id: a\nversion: 1\nkind: job\npoints: [job.run]\ncommand: x\n",
		"provider":       "id: a\nversion: 1\nkind: provider\npoints: [provider.classify]\ncommand: x\n",
		"transport":      "id: a\nversion: 1\nkind: sink\npoints: [sink.consume]\ncommand: x\ntransport: tcp\n",
		"setting type":   "id: a\nversion: 1\nkind: sink\npoints: [sink.consume]\ncommand: x\nsettings: [{key: k, type: blob}]\n",
		"secret default": "id: a\nversion: 1\nkind: sink\npoints: [sink.consume]\ncommand: x\nsettings: [{key: k, type: secret, default: s}]\n",
		"permission":     "id: a\nversion: 1\nkind: sink\npoints: [sink.consume]\ncommand: x\npermissions: {host: [fs]}\n",
		"reserved env":   "id: a\nversion: 1\nkind: sink\npoints: [sink.consume]\ncommand: x\nenv: {PLUGRELAY_SOCKET: x}\n",
		"ui section":     "id: a\nversion: 1\nkind: sink\npoints: [sink.consume]\ncommand: x\nui: {slots: []}\n",
		"timeout method": "id: a\nversion: 1\nkind: sink\npoints: [sink.consume]\ncommand: x\ntimeouts: {methods: {job.run: 1s}}\n",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := ParseManifest([]byte(y))
			if err == nil {
				err = m.Validate()
			}
			if err == nil {
				t.Fatal("accepted")
			}
		})
	}
	ok := `
id: a
version: "1"
kinds: [sink, admin]
points: [sink.consume, admin.handle]
command: x
timeouts: {call: 2s, methods: {sink.consume: 9s}}
admin:
  routes:
    - {path: /stats, role: member}
`
	m, err := ParseManifest([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if m.CallTimeout("sink.consume") != 9*time.Second || m.CallTimeout("admin.handle") != 2*time.Second || m.Admin.Routes[0].Method != "GET" {
		t.Fatal("timeouts or defaults")
	}
}

func TestFraming(t *testing.T) {
	var b bytes.Buffer
	if err := writeFrame(&b, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	b.WriteString("Content-Type: application/vscode-jsonrpc; charset=utf-8\r\ncontent-length: 2\r\n\r\n{}")
	r := newConn(&b, io.Discard, nil, nil).r
	for _, want := range []string{`{"a":1}`, `{}`} {
		got, err := readFrame(r)
		if err != nil || string(got) != want {
			t.Fatalf("frame %q %v", got, err)
		}
	}
	if _, err := readFrame(r); err != io.EOF {
		t.Fatalf("eof: %v", err)
	}
	for _, bad := range []string{"\r\n{}", "Content-Length: x\r\n\r\n", "Content-Length: 999999999999\r\n\r\n"} {
		if _, err := readFrame(newConn(strings.NewReader(bad), io.Discard, nil, nil).r); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// A strictest list from several layers reaches the plugin as their
// intersection plus each layer's own list.
func TestWireCallSettingLayers(t *testing.T) {
	b := &base{in: &instance{man: &Manifest{ID: "x", Settings: []SettingSpec{{Key: "allow", Type: plugin.TypeList}, {Key: "rpm", Type: plugin.TypeInt}}}}}
	vals := map[string]plugin.Value{
		"allow": plugin.NewStrictList([][]string{{"10.0.0.0/8"}, {"10.1.0.0/16"}}),
		"rpm":   plugin.NewValue(json.RawMessage(`5`)),
	}
	c := plugin.NewCall(context.Background(), "r", func(_, key string) plugin.Value { return vals[key] }, nil)
	w := b.wireCall(c, false)
	if got := string(w.Settings["allow"]); got != `["\u0000deny-all"]` {
		t.Fatalf("allow %s", got)
	}
	if l := w.SettingLayers["allow"]; len(l) != 2 || l[1][0] != "10.1.0.0/16" || len(w.SettingLayers) != 1 {
		t.Fatalf("layers %v", w.SettingLayers)
	}
}
