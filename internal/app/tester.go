package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/internal/pipeline"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/state"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// testTimeouts bound one test request.
var testTimeouts = egress.Timeouts{FirstByte: 60 * time.Second, Idle: 60 * time.Second, Total: 90 * time.Second}

// probe builds a tiny generate request in a protocol.
func probe(proto, model string) (path string, body []byte) {
	switch proto {
	case "anthropic":
		body, _ = json.Marshal(map[string]any{"model": model, "max_tokens": 16,
			"messages": []any{map[string]any{"role": "user", "content": "ping"}}})
		return "/v1/messages", body
	case "openai-responses":
		body, _ = json.Marshal(map[string]any{"model": model, "input": "ping", "max_output_tokens": 16})
		return "/v1/responses", body
	case "gemini":
		body, _ = json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user",
			"parts": []any{map[string]any{"text": "ping"}}}}, "generationConfig": map[string]any{"maxOutputTokens": 16}})
		return "/v1beta/models/" + model + ":generateContent", body
	}
	body, _ = json.Marshal(map[string]any{"model": model, "max_tokens": 16,
		"messages": []any{map[string]any{"role": "user", "content": "ping"}}})
	return "/v1/chat/completions", body
}

// send runs a probe through p and reports the outcome.
func send(ctx context.Context, p *pipeline.Pipeline, proto, model string, caller *plugin.Caller) (admin.TestResult, string) {
	res := admin.TestResult{Model: model}
	codec, ok := protocol.Get(proto)
	if !ok {
		res.Error = "unknown protocol " + proto
		return res, ""
	}
	path, body := probe(proto, model)
	var route protocol.Route
	for _, rt := range codec.Routes() {
		if rt.Op == protocol.OpGenerate {
			route = rt
			break
		}
	}
	id := newID()
	req := httptest.NewRequestWithContext(pipeline.WithRequestID(ctx, id), http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "plugrelay-test")
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	start := time.Now()
	p.ServeProtocol(rec, req, codec, route, caller, body)
	res.LatencyMs = time.Since(start).Milliseconds()
	res.Status = rec.Code
	res.OK = rec.Code/100 == 2
	if !res.OK {
		msg := strings.TrimSpace(rec.Body.String())
		if len(msg) > 2000 {
			msg = msg[:2000] + "…"
		}
		res.Error = msg
	}
	res.RequestID = id
	return res, id
}

// notChat marks model ids that cannot answer a chat probe.
var notChat = []string{"embed", "rerank", "tts", "whisper", "transcribe", "dall-e", "image", "audio", "moderation", "speech"}

// testModel picks the first listed model that looks like a chat model.
func testModel(ms []ir.ModelInfo) string {
	for _, m := range ms {
		id := strings.ToLower(m.ID)
		if !slices.ContainsFunc(notChat, func(s string) bool { return strings.Contains(id, s) }) {
			return m.ID
		}
	}
	return ""
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// testUpstream sends a probe to one upstream per credential. Each probe
// runs through its own pipeline and router over the live configuration of
// that single upstream, so a failed test never cools down a credential in
// the live router; the request is still logged.
func (a *App) testUpstream(ctx context.Context, upstreamID string, req admin.TestRequest) []admin.TestResult {
	// An upstream or credential the console has just stored is tested
	// before the next poll would load it.
	if err := a.Reload(ctx); err != nil {
		a.log.Warn("reload before upstream test failed", "err", err)
	}
	rt := a.runtime()
	fail := func(msg string) []admin.TestResult {
		return []admin.TestResult{{UpstreamID: upstreamID, Model: req.Model, Error: msg}}
	}
	up := rt.snap.Upstreams[upstreamID]
	tu, ok := rt.snap.Table.Upstreams[upstreamID]
	if up == nil || !ok || len(tu.Endpoints) == 0 {
		return fail("the upstream is misconfigured; check its provider and config")
	}
	ep := tu.Endpoints[0]
	if req.EndpointID != "" {
		i := slices.IndexFunc(tu.Endpoints, func(e plugin.Endpoint) bool { return e.ID == req.EndpointID })
		if i < 0 {
			return fail("no endpoint " + req.EndpointID)
		}
		ep = tu.Endpoints[i]
	}
	model := req.Model
	if model == "" {
		for _, r := range rt.store.Routes {
			if r.UpstreamID == upstreamID {
				model = r.UpstreamModel
				break
			}
		}
	}
	if model == "" {
		// A new upstream has no routes yet: ask it which models it serves.
		ms, err := runtimeAPI{a}.RemoteModels(ctx, upstreamID)
		if err != nil {
			return fail("no model to test with, and listing the upstream's models failed: " + err.Error())
		}
		model = testModel(ms)
	}
	if model == "" {
		return fail("no model to test with: pass one or route a model to this upstream")
	}
	if tu.Keyless() {
		return []admin.TestResult{a.probeCredential(ctx, rt, up, tu, ep, nil, model)}
	}
	if len(tu.Credentials) == 0 {
		return fail("the upstream has no credential: add its API key")
	}
	var out []admin.TestResult
	for _, c := range tu.Credentials {
		if req.CredentialID != "" && c.ID != req.CredentialID {
			continue
		}
		if c.Status == router.StatusDisabled {
			continue
		}
		res := a.probeCredential(ctx, rt, up, tu, ep, &c, model)
		out = append(out, res)
	}
	if len(out) == 0 {
		return fail("no enabled credential to test")
	}
	return out
}

// probeCredential tests one credential; c nil tests a keyless upstream.
func (a *App) probeCredential(ctx context.Context, rt *runtime, up *pipeline.Upstream, tu router.Upstream,
	ep plugin.Endpoint, c *router.Cred, model string) admin.TestResult {
	cfg := *up.Config
	cfg.Endpoints = []plugin.Endpoint{ep}
	one := &pipeline.Upstream{Config: &cfg, Provider: up.Provider, AllowPrivate: up.AllowPrivate,
		Timeouts: up.Timeouts, Proxy: up.Proxy, Headers: up.Headers, Credentials: map[string]*plugin.Credential{}, Keyless: c == nil}
	var creds []router.Cred
	if c != nil {
		one.Credentials[c.ID] = up.Credentials[c.ID]
		rc := *c
		rc.Status, rc.StatusUntil, rc.Reason = router.StatusActive, time.Time{}, ""
		creds = []router.Cred{rc}
	}
	table := &router.Table{
		Models: map[string]router.Model{model: {Name: model, Enabled: true,
			Routes: []router.Route{{UpstreamID: tu.ID, UpstreamModel: model}}}},
		Upstreams: map[string]router.Upstream{tu.ID: {ID: tu.ID, Enabled: true, Endpoints: []plugin.Endpoint{ep},
			Credentials: creds, KeyOptional: tu.KeyOptional}},
	}
	snap := &pipeline.Snapshot{Table: table, Upstreams: map[string]*pipeline.Upstream{tu.ID: one}, Price: rt.snap.Price,
		AllowDiscouraged: true}
	mem := state.NewMemory(time.Minute)
	defer mem.Close()
	p := pipeline.New(pipeline.Deps{
		Snapshot: func() *pipeline.Snapshot { return snap },
		Router:   router.New(mem, router.Options{}),
		Egress:   a.egress,
		Sealer:   a.sealer,
		Records:  a.queue,
		Logger:   a.log.With("component", "upstream-test"),
		Timeouts: testTimeouts,
	})
	res, _ := send(ctx, p, ep.Protocol, model, &plugin.Caller{KeyName: "upstream test"})
	res.UpstreamID, res.EndpointID = tu.ID, ep.ID
	if c != nil {
		res.CredentialID = c.ID
	}
	return res
}

// testKey sends a probe through the live relay as the key's caller.
func (a *App) testKey(ctx context.Context, k *store.Key, proto, model string) admin.TestResult {
	caller := k.Caller()
	if model == "" {
		snap := a.runtime().snap
		for _, m := range snap.Models {
			if snap.Visible == nil || snap.Visible(&caller, m.ID) {
				model = m.ID
				break
			}
		}
	}
	if model == "" {
		return admin.TestResult{Error: "no model is available to this key"}
	}
	if !k.Usable(time.Now()) {
		return admin.TestResult{Model: model, Status: http.StatusUnauthorized, Error: "the key is disabled or expired"}
	}
	res, _ := send(ctx, a.pipe, proto, model, &caller)
	return res
}
