package pipeline

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/seal"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
	_ "github.com/initxy/plugrelay/pkg/protocol/anthropic"
	_ "github.com/initxy/plugrelay/pkg/protocol/coherererank"
	_ "github.com/initxy/plugrelay/pkg/protocol/gemini"
	_ "github.com/initxy/plugrelay/pkg/protocol/geminiembeddings"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaiaudio"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaichat"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaicompletions"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaiembeddings"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaiimages"
	_ "github.com/initxy/plugrelay/pkg/protocol/openairesponses"
	_ "github.com/initxy/plugrelay/pkg/protocol/rerank"
	_ "github.com/initxy/plugrelay/pkg/protocol/teirerank"
	"github.com/initxy/plugrelay/plugins/provider/compatible"
	"github.com/initxy/plugrelay/plugins/provider/manifest"
)

// ---------------------------------------------------------------- state

type memItem struct {
	v   []byte
	exp time.Time
}

// memState is a minimal in-memory state.State.
type memState struct {
	mu    sync.Mutex
	items map[string]memItem
	slots map[string]int
}

func newMemState() *memState {
	return &memState{items: map[string]memItem{}, slots: map[string]int{}}
}

func (m *memState) live(key string) (memItem, bool) {
	it, ok := m.items[key]
	if ok && !it.exp.IsZero() && !time.Now().Before(it.exp) {
		delete(m.items, key)
		return memItem{}, false
	}
	return it, ok
}

func exp(ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return time.Now().Add(ttl)
}

func (m *memState) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.live(key)
	return it.v, ok, nil
}

func (m *memState) Set(_ context.Context, key string, v []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[key] = memItem{v: v, exp: exp(ttl)}
	return nil
}

func (m *memState) SetNX(_ context.Context, key string, v []byte, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.live(key); ok {
		return false, nil
	}
	m.items[key] = memItem{v: v, exp: exp(ttl)}
	return true, nil
}

func (m *memState) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, key)
	return nil
}

func (m *memState) Incr(_ context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.live(key)
	var n int64
	if ok {
		n, _ = strconv.ParseInt(string(it.v), 10, 64)
	} else {
		it.exp = exp(ttl)
	}
	n += delta
	it.v = []byte(strconv.FormatInt(n, 10))
	m.items[key] = it
	return n, nil
}

func (m *memState) Acquire(_ context.Context, key string, limit int, _ time.Duration) (func(), bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.slots[key] >= limit {
		return nil, false, nil
	}
	m.slots[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			m.slots[key]--
			m.mu.Unlock()
		})
	}, true, nil
}

func (m *memState) Lock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	ok, _ := m.SetNX(ctx, key, []byte("1"), ttl)
	if !ok {
		return nil, false, nil
	}
	return func() { _ = m.Delete(ctx, key) }, true, nil
}

func (m *memState) Close() error { return nil }

// ---------------------------------------------------------------- fake upstreams

// fakeUpstream is an httptest server speaking one protocol; the handler
// is set per test and every received request is kept.
type fakeUpstream struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	paths   []string
	handle  func(w http.ResponseWriter, r *http.Request, body []byte)
	srv     *httptest.Server
}

func newFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte)) *fakeUpstream {
	f := &fakeUpstream{handle: h}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.headers = append(f.headers, r.Header.Clone())
		f.paths = append(f.paths, r.URL.RequestURI())
		f.mu.Unlock()
		f.handle(w, r, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeUpstream) body(i int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[i]
}

// sseReply streams frames, flushing each, with an optional pause first.
func sseReply(pause time.Duration, frames ...string) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if pause > 0 {
			select {
			case <-time.After(pause):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Request-Id", "req_upstream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			fl.Flush()
		}
	}
}

func jsonReply(status int, body string, header ...string) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func fixture(t *testing.T, name string) string {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---------------------------------------------------------------- environment

// upSpec describes one upstream of a test environment.
type upSpec struct {
	id       string
	protocol string
	issuer   string
	fake     *fakeUpstream
	model    string // upstream model name; defaults to the client model
	priority int
	caps     string // YAML capabilities block for the endpoint
}

type env struct {
	t       *testing.T
	p       *Pipeline
	rt      *router.Router
	snap    *Snapshot
	chain   *Chain
	records *recQueue
	caller  plugin.Caller
}

type recQueue struct {
	mu   sync.Mutex
	recs []*plugin.Record
}

func (q *recQueue) Enqueue(r *plugin.Record) bool {
	q.mu.Lock()
	q.recs = append(q.recs, r)
	q.mu.Unlock()
	return true
}

func (q *recQueue) last(t *testing.T) *plugin.Record {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.recs) == 0 {
		t.Fatal("no record")
	}
	return q.recs[len(q.recs)-1]
}

var testPrice = Price{Input: 3_000_000, Output: 15_000_000, CacheRead: 300_000, CacheWrite5m: 3_750_000, CacheWrite1h: 6_000_000}

func interpreterOf(proto string) string {
	if proto == "anthropic" {
		return "anthropic-compatible"
	}
	if strings.HasPrefix(proto, "gemini") {
		return "gemini"
	}
	return "openai-compatible"
}

func newEnv(t *testing.T, clientModel string, ups []upSpec, mod ...func(*Deps)) *env {
	t.Helper()
	e := &env{t: t, records: &recQueue{}, chain: &Chain{}, caller: plugin.Caller{KeyID: "k1", UserID: "u1"}}
	table := &router.Table{Models: map[string]router.Model{}, Upstreams: map[string]router.Upstream{}}
	snap := &Snapshot{Table: table, Upstreams: map[string]*Upstream{},
		Models: []ir.ModelInfo{{ID: clientModel}, {ID: "other-model"}},
		Price:  func(string, string) (Price, bool) { return testPrice, true }}
	var routes []router.Route
	for _, u := range ups {
		base := u.fake.srv.URL
		if strings.HasPrefix(u.protocol, "openai") {
			base += "/v1"
		}
		authHeader := "auth: {header: Authorization, scheme: Bearer}"
		if u.protocol == "anthropic" {
			authHeader = "auth: {header: x-api-key, scheme: raw}"
		}
		if strings.HasPrefix(u.protocol, "gemini") {
			authHeader = "auth: {header: x-goog-api-key, scheme: raw}"
		}
		caps := ""
		if u.caps != "" {
			caps = "\n    capabilities: " + u.caps
		}
		yml := "id: " + u.id + "\nkind: provider\ninterpreter: " + interpreterOf(u.protocol) + "\n" + authHeader +
			"\nendpoints:\n  - id: main\n    protocol: " + u.protocol + "\n    base_url: \"" + base + "\"\n    issuer: " + u.issuer + caps + "\n"
		m, err := manifest.Parse([]byte(yml))
		if err != nil {
			t.Fatalf("manifest %s: %v\n%s", u.id, err, yml)
		}
		prov := compatible.New(interpreterOf(u.protocol), compatible.WithIndex(compatible.NewIndex(m)))
		cfg := &plugin.Upstream{ID: u.id, Provider: prov.Info().ID, Config: map[string]any{"manifest": u.id}}
		eps, err := prov.Endpoints(cfg)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Endpoints = eps
		table.Upstreams[u.id] = router.Upstream{ID: u.id, Enabled: true, Endpoints: eps,
			Credentials: []router.Cred{{ID: "c1", Kind: router.KindShared, Status: router.StatusActive}}}
		snap.Upstreams[u.id] = &Upstream{Config: cfg, Provider: prov, AllowPrivate: true,
			Credentials: map[string]*plugin.Credential{"c1": {ID: "c1", UpstreamID: u.id, Kind: "shared", Secret: "sk-" + u.id}}}
		model := u.model
		if model == "" {
			model = clientModel
		}
		routes = append(routes, router.Route{UpstreamID: u.id, UpstreamModel: model, Priority: u.priority})
	}
	table.Models[clientModel] = router.Model{Name: clientModel, Enabled: true, Routes: routes}
	e.snap = snap
	e.rt = router.New(newMemState(), router.Options{Secret: []byte("turn-secret")})
	sealer, err := seal.New(bytes.Repeat([]byte{7}, seal.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{
		Snapshot:       func() *Snapshot { return e.snap },
		Router:         e.rt,
		Egress:         egress.New(egress.Options{}),
		Sealer:         sealer,
		Plugins:        func() *Chain { return e.chain },
		Records:        e.records,
		Timeouts:       egress.Timeouts{FirstByte: 5 * time.Second, Idle: 5 * time.Second, Total: 20 * time.Second},
		KeepaliveDelay: 2 * time.Second,
		Keepalive:      time.Second,
	}
	for _, f := range mod {
		f(&d)
	}
	e.p = New(d)
	return e
}

// call sends a client request through the pipeline over a real HTTP
// server and returns the complete response.
func (e *env) call(codecName, path string, body string, header ...string) (*http.Response, []byte) {
	e.t.Helper()
	srv := e.server(codecName, path)
	defer srv.Close()
	resp, err := http.DefaultClient.Do(e.request(srv.URL, codecName, path, body, header...))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func (e *env) request(base, codecName, path, body string, header ...string) *http.Request {
	codec, _ := protocol.Get(codecName)
	route, _ := matchRoute(codec, path)
	req, _ := http.NewRequest(route.Method, base+path, strings.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	return req
}

// server serves one route of one codec through the pipeline.
func (e *env) server(codecName, path string) *httptest.Server {
	e.t.Helper()
	codec, ok := protocol.Get(codecName)
	if !ok {
		e.t.Fatalf("codec %s", codecName)
	}
	route, ok := matchRoute(codec, path)
	if !ok {
		e.t.Fatalf("no route %s on %s", path, codecName)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		caller := e.caller
		e.p.ServeProtocol(w, r, codec, route, &caller, b)
	}))
}

// waitRecords waits until n records were written.
func (e *env) waitRecords(n int) *plugin.Record {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.records.mu.Lock()
		got := len(e.records.recs)
		e.records.mu.Unlock()
		if got >= n {
			return e.records.last(e.t)
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("record %d never written", n)
	return nil
}

func matchRoute(c protocol.Codec, path string) (protocol.Route, bool) {
	p, _, _ := strings.Cut(path, "?")
	for _, r := range c.Routes() {
		if r.Path == p {
			return r, true
		}
		if strings.Contains(r.Path, "{model}") && strings.HasPrefix(p, "/v1beta/models/") && strings.Contains(p, ":") {
			return r, true
		}
	}
	return protocol.Route{}, false
}
