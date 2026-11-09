//go:build e2e

package e2e

// Embeddings, rerank and transcription against real open-source servers
// running locally on CPU, added through their shipped manifests:
//
//	PLUGRELAY_E2E_TEI_EMBED   Hugging Face TEI root URL serving an embedding model
//	                          (e.g. BAAI/bge-small-en-v1.5), without /v1
//	PLUGRELAY_E2E_TEI_RERANK  TEI root URL serving a reranker (e.g. BAAI/bge-reranker-base)
//	PLUGRELAY_E2E_STT_BASE    OpenAI-compatible base URL with /v1 serving
//	                          /audio/transcriptions (e.g. whisper.cpp's whisper-server
//	                          started with --inference-path /v1/audio/transcriptions)
//
// Each part skips, saying so, when its variable is unset or the server does
// not answer. docs/compat/README.md has the commands that start them.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSelfHostedMedia(t *testing.T) {
	embedURL, rerankURL := os.Getenv("PLUGRELAY_E2E_TEI_EMBED"), os.Getenv("PLUGRELAY_E2E_TEI_RERANK")
	sttBase := os.Getenv("PLUGRELAY_E2E_STT_BASE")
	if embedURL == "" && rerankURL == "" && sttBase == "" {
		t.Skip("set PLUGRELAY_E2E_TEI_EMBED, PLUGRELAY_E2E_TEI_RERANK and/or PLUGRELAY_E2E_STT_BASE to run against local servers")
	}
	alive := func(u string) bool {
		if u == "" {
			return false
		}
		c := http.Client{Timeout: 5 * time.Second}
		resp, err := c.Get(u)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	}
	// The relay's own upstream is unused here; the servers take no key, and
	// a placeholder credential is sent and ignored.
	r := startWith(t, "http://127.0.0.1:9/v1", []string{
		"PLUGRELAY_ADMIN_TOKEN=" + e2eAdminToken, "PLUGRELAY_E2E_KEY=unused", "E2E_LOCAL_KEY=none"})
	prices := "prices:\n"
	add := func(manifest, id, base, name, upstreamModel string) {
		r.run("upstream", "add", "--manifest", manifest, "--id", id, "--credential-env", "E2E_LOCAL_KEY", "--var", "base_url="+base)
		r.run("model", "add", "--name", name, "--route", id+":"+upstreamModel)
		// 100 USD per million tokens and 0.01 USD per audio second, so a
		// handful of tokens is a visible charge.
		prices += "  - {model: " + name + ", input: 100000000, output: 100000000, units: {audio_second: 10000}, currency: USD}\n"
	}
	haveEmbed, haveRerank, haveSTT := alive(embedURL), alive(rerankURL), alive(sttBase)
	if haveEmbed {
		add("tei", "tei-embed", strings.TrimRight(embedURL, "/"), "bge-small", "BAAI/bge-small-en-v1.5")
	}
	if haveRerank {
		add("tei", "tei-rerank", strings.TrimRight(rerankURL, "/"), "bge-reranker", "BAAI/bge-reranker-base")
	}
	if haveSTT {
		add("openai-compatible", "stt", strings.TrimRight(sttBase, "/"), "whisper-local", "whisper-1")
	}
	doc := filepath.Join(t.TempDir(), "prices.yaml")
	_ = os.WriteFile(doc, []byte(prices), 0o600)
	if haveEmbed || haveRerank || haveSTT {
		r.run("apply", "-f", doc)
		time.Sleep(3 * time.Second) // the next reload picks up upstreams and prices
	}

	t.Run("openai embeddings", func(t *testing.T) {
		if !haveEmbed {
			t.Skip("PLUGRELAY_E2E_TEI_EMBED unset or not answering")
		}
		docs := []string{"The cat sat on the mat.", "A kitten rested on the rug.", "Stock markets fell sharply today."}
		var out struct {
			Object string `json:"object"`
			Data   []struct {
				Index     int       `json:"index"`
				Embedding []float64 `json:"embedding"`
			} `json:"data"`
			Model string `json:"model"`
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
				TotalTokens  int `json:"total_tokens"`
			} `json:"usage"`
		}
		since := time.Now()
		decode(t, r.post(t, "/v1/embeddings", map[string]any{"model": "bge-small", "input": docs}, nil), &out)
		if out.Object != "list" || len(out.Data) != 3 || out.Usage.PromptTokens == 0 {
			t.Fatalf("embeddings: object %q, %d vectors, usage %+v", out.Object, len(out.Data), out.Usage)
		}
		for i, d := range out.Data {
			if d.Index != i || len(d.Embedding) != 384 {
				t.Fatalf("vector %d: index %d, %d dims, want 384", i, d.Index, len(d.Embedding))
			}
		}
		near, far := cosine(out.Data[0].Embedding, out.Data[1].Embedding), cosine(out.Data[0].Embedding, out.Data[2].Embedding)
		if near <= far {
			t.Fatalf("cat/kitten %.3f not closer than cat/stocks %.3f", near, far)
		}
		t.Logf("3 vectors × 384, usage %+v, cos(cat,kitten)=%.3f cos(cat,stocks)=%.3f", out.Usage, near, far)
		checkCharge(t, r, "bge-small", since)

		// base64: the upstream's floats, packed by the relay or the server.
		var b64 struct {
			Data []struct {
				Embedding string `json:"embedding"`
			} `json:"data"`
		}
		decode(t, r.post(t, "/v1/embeddings", map[string]any{"model": "bge-small", "input": docs[0], "encoding_format": "base64"}, nil), &b64)
		raw, err := base64.StdEncoding.DecodeString(b64.Data[0].Embedding)
		if err != nil || len(raw) != 4*384 {
			t.Fatalf("base64 vector: %d bytes (%v), want %d", len(raw), err, 4*384)
		}
		v0 := math.Float32frombits(binary.LittleEndian.Uint32(raw))
		if math.Abs(float64(v0)-out.Data[0].Embedding[0]) > 1e-4 {
			t.Fatalf("base64 vector starts %v, float vector %v", v0, out.Data[0].Embedding[0])
		}
	})

	t.Run("gemini embedContent", func(t *testing.T) {
		if !haveEmbed {
			t.Skip("PLUGRELAY_E2E_TEI_EMBED unset or not answering")
		}
		h := http.Header{"X-Goog-Api-Key": {r.key}}
		var one struct {
			Embedding struct {
				Values []float64 `json:"values"`
			} `json:"embedding"`
		}
		decode(t, r.post(t, "/v1beta/models/bge-small:embedContent", map[string]any{
			"content": map[string]any{"parts": []any{map[string]any{"text": "The cat sat on the mat."}}}}, h), &one)
		if len(one.Embedding.Values) != 384 {
			t.Fatalf("embedContent: %d values, want 384", len(one.Embedding.Values))
		}
		var batch struct {
			Embeddings []struct {
				Values []float64 `json:"values"`
			} `json:"embeddings"`
		}
		req := func(text string) map[string]any {
			return map[string]any{"model": "models/bge-small", "content": map[string]any{"parts": []any{map[string]any{"text": text}}}}
		}
		decode(t, r.post(t, "/v1beta/models/bge-small:batchEmbedContents", map[string]any{
			"requests": []any{req("The cat sat on the mat."), req("Stock markets fell sharply today.")}}, h), &batch)
		if len(batch.Embeddings) != 2 || len(batch.Embeddings[1].Values) != 384 {
			t.Fatalf("batchEmbedContents: %+v", len(batch.Embeddings))
		}
		if c := cosine(one.Embedding.Values, batch.Embeddings[0].Values); c < 0.999 {
			t.Fatalf("same text, different vectors: cos %.4f", c)
		}
		t.Logf("embedContent 384 values; batchEmbedContents 2 × 384, first matches embedContent")
	})

	rerankDocs := []string{"Berlin is the capital of Germany.", "Paris is the capital of France.", "Bananas are rich in potassium."}
	const rerankQuery = "What is the capital of France?"

	t.Run("rerank", func(t *testing.T) {
		if !haveRerank {
			t.Skip("PLUGRELAY_E2E_TEI_RERANK unset or not answering")
		}
		var out struct {
			Results []struct {
				Index    int     `json:"index"`
				Score    float64 `json:"relevance_score"`
				Document *struct {
					Text string `json:"text"`
				} `json:"document"`
			} `json:"results"`
			Usage map[string]any `json:"usage"`
		}
		since := time.Now()
		decode(t, r.post(t, "/v1/rerank", map[string]any{"model": "bge-reranker", "query": rerankQuery,
			"documents": rerankDocs, "top_n": 2, "return_documents": true}, nil), &out)
		if len(out.Results) != 2 || out.Results[0].Index != 1 || out.Results[0].Score < out.Results[1].Score {
			t.Fatalf("results %+v", out.Results)
		}
		if out.Results[0].Document == nil || out.Results[0].Document.Text != rerankDocs[1] {
			t.Fatalf("document not filled in: %+v", out.Results[0].Document)
		}
		t.Logf("top 2: #%d %.4f %q, #%d %.4f; usage %v", out.Results[0].Index, out.Results[0].Score,
			out.Results[0].Document.Text, out.Results[1].Index, out.Results[1].Score, out.Usage)
		checkCharge(t, r, "bge-reranker", since)
	})

	t.Run("cohere v2 rerank", func(t *testing.T) {
		if !haveRerank {
			t.Skip("PLUGRELAY_E2E_TEI_RERANK unset or not answering")
		}
		var out struct {
			ID      string `json:"id"`
			Results []struct {
				Index    int             `json:"index"`
				Score    float64         `json:"relevance_score"`
				Document json.RawMessage `json:"document"`
			} `json:"results"`
			Meta map[string]any `json:"meta"`
		}
		decode(t, r.post(t, "/v2/rerank", map[string]any{"model": "bge-reranker", "query": rerankQuery,
			"documents": rerankDocs, "top_n": 3}, nil), &out)
		if len(out.Results) != 3 || out.Results[0].Index != 1 {
			t.Fatalf("results %+v", out.Results)
		}
		for i := 1; i < len(out.Results); i++ {
			if out.Results[i].Score > out.Results[i-1].Score {
				t.Fatalf("not sorted by score: %+v", out.Results)
			}
		}
		if len(out.Results[0].Document) != 0 {
			t.Fatalf("Cohere v2 results carry no document, got %s", out.Results[0].Document)
		}
		t.Logf("order %d,%d,%d scores %.4f,%.4f,%.4f meta %v", out.Results[0].Index, out.Results[1].Index, out.Results[2].Index,
			out.Results[0].Score, out.Results[1].Score, out.Results[2].Score, out.Meta)
	})

	t.Run("audio transcription", func(t *testing.T) {
		if !haveSTT {
			t.Skip("PLUGRELAY_E2E_STT_BASE unset or not answering")
		}
		wav := spokenWAV(t, "The quick brown fox jumps over the lazy dog.")
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("model", "whisper-local")
		fw, _ := mw.CreateFormFile("file", "fox.wav")
		_, _ = fw.Write(wav)
		_ = mw.WriteField("response_format", "json")
		_ = mw.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/v1/audio/transcriptions", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+r.key)
		since := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", resp.StatusCode, body)
		}
		var out struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(body, &out)
		if !strings.Contains(strings.ToLower(out.Text), "quick brown fox") {
			t.Fatalf("transcript %q", out.Text)
		}
		t.Logf("transcript %q", strings.TrimSpace(out.Text))
		row := checkLogged(t, r, "whisper-local", since)
		var d struct {
			Usage struct {
				AudioMs int64 `json:"audio_ms"`
			} `json:"usage"`
		}
		r.admin(t, "GET", "/api/v1/logs/"+row.ID, nil, &d)
		// 10000 micro-USD per audio second = 10 per millisecond.
		if d.Usage.AudioMs < 1000 || row.Charge.Micro != d.Usage.AudioMs*10 {
			t.Fatalf("audio %d ms, charge %d micro-USD; want 10 per ms", d.Usage.AudioMs, row.Charge.Micro)
		}
		t.Logf("request log: %d ms of audio, charged %d micro-USD", d.Usage.AudioMs, row.Charge.Micro)
	})
}

// spokenWAV synthesizes text to 16 kHz mono WAV with ffmpeg's flite
// filter, or skips when that is unavailable.
func spokenWAV(t *testing.T, text string) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH; no speech to transcribe")
	}
	out := filepath.Join(t.TempDir(), "speech.wav")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "flite=text='"+text+"':voice=slt", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot synthesize speech (built without libflite?): %v %s", err, msg)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	return dot / math.Sqrt(na*nb)
}

type logRow struct {
	ID     string    `json:"id"`
	Time   time.Time `json:"time"`
	Model  string    `json:"model"`
	Status int       `json:"status"`
	Charge struct {
		Micro int64 `json:"micro"`
	} `json:"charge"`
}

// checkLogged waits for a successful request-log record of model since the
// given time and returns it.
func checkLogged(t *testing.T, r *relay, model string, since time.Time) logRow {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var page struct {
			Items []logRow `json:"items"`
		}
		r.admin(t, "GET", "/api/v1/logs?limit=50", nil, &page)
		for _, it := range page.Items {
			if !it.Time.Before(since.Truncate(time.Millisecond)) && it.Model == model && it.Status == http.StatusOK {
				return it
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no successful log record for %s since %s: %+v", model, since.Format(time.RFC3339), page.Items)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// checkCharge asserts the request was logged with input tokens and a
// charge at the test price.
func checkCharge(t *testing.T, r *relay, model string, since time.Time) {
	t.Helper()
	row := checkLogged(t, r, model, since)
	var d struct {
		Usage struct {
			Input  int64 `json:"input"`
			Output int64 `json:"output"`
		} `json:"usage"`
		Billing struct {
			Note string `json:"note"`
		} `json:"billing"`
	}
	r.admin(t, "GET", "/api/v1/logs/"+row.ID, nil, &d)
	if d.Usage.Input == 0 || row.Charge.Micro != (d.Usage.Input+d.Usage.Output)*100 {
		t.Fatalf("usage %+v, charge %d micro-USD; want 100 per token", d.Usage, row.Charge.Micro)
	}
	t.Logf("request log: usage %+v, charged %d micro-USD %s", d.Usage, row.Charge.Micro, d.Billing.Note)
}
