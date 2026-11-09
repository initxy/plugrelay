//go:build e2e

// Package e2e drives a real plugrelay binary against a live
// OpenAI-compatible upstream: build, init, add the upstream, create a key,
// serve, then call every client protocol and expect text back, plus the
// raw /native passthrough.
//
//	PLUGRELAY_E2E_BASE   upstream base URL including /v1 (required)
//	PLUGRELAY_E2E_KEY    upstream API key (required)
//	PLUGRELAY_E2E_MODEL  upstream model (default gpt-5.4)
//	PLUGRELAY_E2E_IMAGE_MODEL  image model (default ark/seedream-5.0-flash)
//	PLUGRELAY_E2E_EMBED_MODEL  embeddings model (default text-embedding-3-small)
//	PLUGRELAY_E2E_TTS_MODEL    speech model (default tts-1)
//	PLUGRELAY_E2E_STT_MODEL    transcription model (default whisper-1)
//
// Media cases first ask the upstream directly whether it serves the
// operation and skip, saying so, when it answers 404.
//
// Run with `make e2e`. The test skips when BASE or KEY is unset.
package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/sse"
)

const defaultModel = "gpt-5.4"

// prompt asks for a short, predictable answer; the checks only require
// non-empty text, since models phrase it differently.
const prompt = "Reply with the single word: pong"

type relay struct {
	t     *testing.T
	bin   string
	env   []string
	base  string // http://host:port of the running relay
	key   string // platform key
	keyID string
}

func TestRelay(t *testing.T) {
	upBase, upKey := os.Getenv("PLUGRELAY_E2E_BASE"), os.Getenv("PLUGRELAY_E2E_KEY")
	if upBase == "" || upKey == "" {
		t.Skip("set PLUGRELAY_E2E_BASE and PLUGRELAY_E2E_KEY to run against a live upstream")
	}
	model := os.Getenv("PLUGRELAY_E2E_MODEL")
	if model == "" {
		model = defaultModel
	}
	media := map[string]string{
		"image": envOr("PLUGRELAY_E2E_IMAGE_MODEL", "ark/seedream-5.0-flash"),
		"embed": envOr("PLUGRELAY_E2E_EMBED_MODEL", "text-embedding-3-small"),
		"tts":   envOr("PLUGRELAY_E2E_TTS_MODEL", "tts-1"),
		"stt":   envOr("PLUGRELAY_E2E_STT_MODEL", "whisper-1"),
	}
	r := start(t, upBase, model, media["image"], media["embed"], media["tts"], media["stt"])
	up := upstream{base: upBase, key: upKey}

	t.Run("openai chat", func(t *testing.T) {
		resp := r.post(t, "/v1/chat/completions", map[string]any{
			"model": model, "messages": []any{map[string]any{"role": "user", "content": prompt}},
		}, nil)
		var out struct {
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
		}
		decode(t, resp, &out)
		if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
			t.Fatalf("no text: %+v", out)
		}
	})

	t.Run("openai chat stream", func(t *testing.T) {
		resp := r.post(t, "/v1/chat/completions", map[string]any{
			"model": model, "stream": true, "messages": []any{map[string]any{"role": "user", "content": prompt}},
		}, nil)
		text := streamText(t, resp, func(ev sse.Event) string {
			var c struct {
				Choices []struct {
					Delta struct{ Content string } `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal(ev.Data, &c) != nil || len(c.Choices) == 0 {
				return ""
			}
			return c.Choices[0].Delta.Content
		})
		if strings.TrimSpace(text) == "" {
			t.Fatal("stream carried no text")
		}
	})

	t.Run("responses stream", func(t *testing.T) {
		resp := r.post(t, "/v1/responses", map[string]any{"model": model, "stream": true, "input": prompt}, nil)
		text := streamText(t, resp, func(ev sse.Event) string {
			var d struct {
				Type  string `json:"type"`
				Delta string `json:"delta"`
			}
			if json.Unmarshal(ev.Data, &d) != nil || d.Type != "response.output_text.delta" {
				return ""
			}
			return d.Delta
		})
		if strings.TrimSpace(text) == "" {
			t.Fatal("stream carried no text")
		}
	})

	// The upstream speaks only OpenAI protocols, so this exercises the
	// Anthropic Messages ↔ OpenAI conversion end to end.
	t.Run("anthropic messages stream", func(t *testing.T) {
		resp := r.post(t, "/v1/messages", map[string]any{
			"model": model, "max_tokens": 1024, "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": prompt}},
		}, http.Header{"Anthropic-Version": {"2023-06-01"}})
		var names []string
		text := streamText(t, resp, func(ev sse.Event) string {
			names = append(names, ev.Name)
			var d struct {
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if ev.Name != "content_block_delta" || json.Unmarshal(ev.Data, &d) != nil || d.Delta.Type != "text_delta" {
				return ""
			}
			return d.Delta.Text
		})
		if strings.TrimSpace(text) == "" {
			t.Fatalf("stream carried no text; events %v", names)
		}
		if len(names) == 0 || names[0] != "message_start" || names[len(names)-1] != "message_stop" {
			t.Fatalf("event sequence %v", names)
		}
	})

	// Legacy completions (Continue / JetBrains autocomplete). The upstream
	// may lack /completions, in which case the relay falls back to chat.
	t.Run("openai completions", func(t *testing.T) {
		resp := r.post(t, "/v1/completions", map[string]any{"model": model, "prompt": prompt, "max_tokens": 20}, nil)
		var out struct {
			Object  string `json:"object"`
			Choices []struct {
				Text string `json:"text"`
			} `json:"choices"`
		}
		decode(t, resp, &out)
		if out.Object != "text_completion" || len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Text) == "" {
			t.Fatalf("completion %+v", out)
		}
	})

	t.Run("images", func(t *testing.T) {
		body := map[string]any{"model": media["image"], "prompt": "a red apple on a white table", "n": 1, "size": "1024x1024"}
		up.require(t, "/images/generations")
		var out struct {
			Data []struct {
				URL     string `json:"url"`
				B64JSON string `json:"b64_json"`
			} `json:"data"`
		}
		decode(t, r.post(t, "/v1/images/generations", body, nil), &out)
		if len(out.Data) == 0 || (out.Data[0].URL == "" && out.Data[0].B64JSON == "") {
			t.Fatalf("no image: %+v", out)
		}
	})

	t.Run("embeddings", func(t *testing.T) {
		body := map[string]any{"model": media["embed"], "input": []string{"hello", "world"}, "encoding_format": "base64"}
		up.require(t, "/embeddings")
		var out struct {
			Data []struct {
				Embedding string `json:"embedding"`
			} `json:"data"`
		}
		decode(t, r.post(t, "/v1/embeddings", body, nil), &out)
		if len(out.Data) != 2 || out.Data[0].Embedding == "" {
			t.Fatalf("embeddings %+v", out)
		}
	})

	t.Run("audio speech", func(t *testing.T) {
		body := map[string]any{"model": media["tts"], "input": "pong", "voice": "alloy"}
		up.require(t, "/audio/speech")
		resp := r.post(t, "/v1/audio/speech", body, nil)
		audio, _ := io.ReadAll(resp.Body)
		if len(audio) == 0 {
			t.Fatal("no audio")
		}
	})

	t.Run("audio transcription", func(t *testing.T) {
		up.require(t, "/audio/transcriptions")
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("model", media["stt"])
		fw, _ := mw.CreateFormFile("file", "silence.wav")
		_, _ = fw.Write(silentWAV())
		_ = mw.Close()
		req, _ := http.NewRequest(http.MethodPost, r.base+"/v1/audio/transcriptions", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+r.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			t.Fatalf("status %d: %s", resp.StatusCode, msg)
		}
	})

	// Raw passthrough: off for keys until granted, then forwarded as is.
	t.Run("native passthrough", func(t *testing.T) {
		if code := r.get(t, "/native/e2e/models", nil); code != http.StatusForbidden {
			t.Fatalf("before grant: status %d, want 403", code)
		}
		grant := filepath.Join(t.TempDir(), "grant.yaml")
		_ = os.WriteFile(grant, []byte("settings:\n  - {scope: global, plugin: middleware.access, key: native_upstreams, value: [\"*\"]}\n"), 0o600)
		r.run("apply", "-f", grant)
		var models struct {
			Data []json.RawMessage `json:"data"`
		}
		deadline := time.Now().Add(20 * time.Second) // the grant applies on the next reload
		for r.get(t, "/native/e2e/models", &models) != http.StatusOK {
			if time.Now().After(deadline) {
				t.Fatal("native still refused after the grant")
			}
			time.Sleep(500 * time.Millisecond)
		}
		if len(models.Data) == 0 {
			t.Fatal("upstream model list came back empty")
		}
	})
}

// get sends a GET with the platform key, decodes a 200 body into v when
// v is not nil, and returns the status.
func (r *relay) get(t *testing.T, path string, v any) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, r.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+r.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return resp.StatusCode
}

// upstream is the live upstream, asked directly whether it serves a
// media operation.
type upstream struct{ base, key string }

// require skips the test when the upstream answers 404 or 405 on path:
// the operation is not served there, so the relay cannot be checked. The
// probe sends an empty object, which a serving upstream rejects (400)
// without doing any billable work.
func (u upstream) require(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(u.base, "/")+path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+u.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		t.Skipf("the upstream does not serve %s (status %d); not checked live", path, resp.StatusCode)
	}
}

// silentWAV is one second of 16 kHz mono 16-bit silence.
func silentWAV() []byte {
	const rate, n = 16000, 16000
	var b bytes.Buffer
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	le(uint32(36 + 2*n))
	b.WriteString("WAVEfmt ")
	le(uint32(16))
	le(uint16(1))
	le(uint16(1))
	le(uint32(rate))
	le(uint32(2 * rate))
	le(uint16(2))
	le(uint16(16))
	b.WriteString("data")
	le(uint32(2 * n))
	b.Write(make([]byte, 2*n))
	return b.Bytes()
}

// start builds plugrelay, sets up a relay for this machine in a temp dir pointing
// at the upstream, and serves it until the test ends. Every model is
// routed to the upstream under its own name.
func start(t *testing.T, upBase string, models ...string) *relay {
	t.Helper()
	return startWith(t, upBase, nil, models...)
}

// startWith is start with extraEnv entries added to the relay's
// environment.
func startWith(t *testing.T, upBase string, extraEnv []string, models ...string) *relay {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "plugrelay")
	build := exec.Command("go", "build", "-o", bin, "./cmd/plugrelay")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	addr := freeAddr(t)
	r := &relay{t: t, bin: bin, base: "http://" + addr}
	// The key reaches the CLI through the environment only, never argv.
	r.env = append(os.Environ(),
		"PLUGRELAY_CONFIG="+filepath.Join(dir, "relay.yaml"),
		"PLUGRELAY_DATA_DIR="+dir,
		// The SSRF guard blocks loopback and private upstreams such as a
		// local test relay; open it for this throwaway instance only.
		"PLUGRELAY_EGRESS_ALLOW_PRIVATE_CIDRS=127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16",
		// /native is off by default; turn it on so the suite can grant it.
		"PLUGRELAY_NATIVE_ENABLED=true",
	)
	r.env = append(r.env, extraEnv...)
	r.run("init", "--listen", addr)
	r.run("upstream", "add", "--manifest", "openai-compatible", "--id", "e2e",
		"--credential-env", "PLUGRELAY_E2E_KEY", "--var", "base_url="+upBase)
	for _, model := range models {
		r.run("model", "add", "--name", model, "--route", "e2e:"+model)
	}
	var k struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(r.run("key", "create", "--name", "e2e", "--json")), &k); err != nil || k.Key == "" {
		t.Fatalf("key create: %v", err)
	}
	r.key, r.keyID = k.Key, k.ID

	ctx, cancel := context.WithCancel(context.Background())
	var logs bytes.Buffer
	serve := exec.CommandContext(ctx, bin, "serve")
	serve.Env, serve.Stdout, serve.Stderr = r.env, &logs, &logs
	serve.Cancel = func() error { return serve.Process.Signal(os.Interrupt) }
	serve.WaitDelay = 10 * time.Second
	if err := serve.Start(); err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = serve.Wait()
		if t.Failed() {
			t.Logf("serve output:\n%s", logs.String())
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(r.base + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay not ready: %v\n%s", err, logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// run executes a CLI command and returns its stdout.
func (r *relay) run(args ...string) string {
	r.t.Helper()
	cmd := exec.Command(r.bin, args...)
	cmd.Env = r.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		r.t.Fatalf("plugrelay %s: %v\n%s%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// post sends a JSON request with the platform key and fails on a non-200.
func (r *relay) post(t *testing.T, path string, body any, h http.Header) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.key)
	for k, vs := range h {
		req.Header[k] = vs
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		t.Fatalf("POST %s: status %d: %s", path, resp.StatusCode, msg)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// streamText reads an SSE response to the end, concatenating what text
// returns for each event.
func streamText(t *testing.T, resp *http.Response, text func(sse.Event) string) string {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q, want text/event-stream", ct)
	}
	var b strings.Builder
	rd := sse.NewReader(resp.Body, 0)
	for {
		ev, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return b.String()
		}
		if err != nil {
			t.Fatalf("stream: %v (text so far %q)", err, b.String())
		}
		if !ev.Comment {
			b.WriteString(text(ev))
		}
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}
