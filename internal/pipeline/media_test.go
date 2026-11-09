package pipeline

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/protocol/openaiembeddings"
)

const embedResp = `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.0023064255,-0.009327292]}],"model":"emb-up","usage":{"prompt_tokens":5,"total_tokens":5}}`

// Same protocol: request and response bytes are untouched, usage is the
// upstream's, the request is billed on input tokens.
func TestEmbeddingsPassthroughByteExact(t *testing.T) {
	up := newFake(t, jsonReply(200, embedResp))
	e := newEnv(t, "emb", []upSpec{{id: "o", protocol: "openai-embeddings", issuer: "o", fake: up}})
	e.snap.ModelPrice = func(string) (Price, bool) { return Price{Input: 20_000}, true }
	body := `{ "input" : ["a", "b"],"model":"emb",  "encoding_format":"float","x_vendor":1 }`
	resp, out := e.call("openai-embeddings", "/v1/embeddings", body)
	if resp.StatusCode != 200 || string(out) != embedResp {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if got := string(up.body(0)); got != body {
		t.Fatalf("upstream got %s", got)
	}
	if up.paths[0] != "/v1/embeddings" {
		t.Errorf("path %s", up.paths[0])
	}
	rec := e.waitRecords(1)
	if rec.Attempts[0].Tier != plugin.TierPassthrough || rec.Usage.Input != 5 || rec.ChargeMicro != 0 {
		// 5 tokens × 0.02/M rounds to 0 micro-units.
		t.Errorf("tier %s usage %+v charge %d", rec.Attempts[0].Tier, rec.Usage, rec.ChargeMicro)
	}
}

// An OpenAI client reaches a Gemini embeddings endpoint: the inputs go as
// one batch with outputDimensionality, base64 is encoded by the relay and
// the missing usage is estimated.
func TestEmbeddingsOpenAIToGemini(t *testing.T) {
	up := newFake(t, jsonReply(200, `{"embeddings":[{"values":[0.5,-0.25]},{"values":[1,2]}]}`))
	e := newEnv(t, "emb", []upSpec{{id: "g", protocol: "gemini-embeddings", issuer: "g", fake: up, model: "gemini-embedding-001"}})
	resp, out := e.call("openai-embeddings", "/v1/embeddings",
		`{"model":"emb","input":["first input text","second"],"dimensions":2,"encoding_format":"base64"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if up.paths[0] != "/v1beta/models/gemini-embedding-001:batchEmbedContents" {
		t.Errorf("path %s", up.paths[0])
	}
	sent := gjson.ParseBytes(up.body(0))
	if sent.Get("requests.#").Int() != 2 || sent.Get("requests.0.outputDimensionality").Int() != 2 ||
		sent.Get("requests.1.content.parts.0.text").String() != "second" || sent.Get("requests.0.model").String() != "models/gemini-embedding-001" {
		t.Fatalf("upstream body %s", up.body(0))
	}
	if up.headers[0].Get("X-Goog-Api-Key") != "sk-g" {
		t.Errorf("auth %v", up.headers[0])
	}
	r, err := openaiembeddings.Codec{}.DecodeResponse(out)
	if err != nil || !reflect.DeepEqual(r.Embeddings, [][]float64{{0.5, -0.25}, {1, 2}}) {
		t.Fatalf("client got %s (%v)", out, err)
	}
	if gjson.GetBytes(out, "data.0.embedding").Type != gjson.String {
		t.Errorf("not base64: %s", out)
	}
	rec := e.waitRecords(1)
	if rec.Attempts[0].Tier != plugin.TierConvert || !rec.Usage.Estimated || rec.Usage.Input == 0 {
		t.Errorf("tier %s usage %+v", rec.Attempts[0].Tier, rec.Usage)
	}
}

// A Gemini client reaches an OpenAI endpoint; each action keeps its shape.
func TestEmbeddingsGeminiToOpenAI(t *testing.T) {
	up := newFake(t, jsonReply(200, `{"object":"list","data":[{"object":"embedding","index":1,"embedding":[3,4]},{"object":"embedding","index":0,"embedding":[1,2]}],"model":"m","usage":{"prompt_tokens":7,"total_tokens":7}}`))
	e := newEnv(t, "gemini-embedding-001", []upSpec{{id: "o", protocol: "openai-embeddings", issuer: "o", fake: up, model: "text-embedding-3-small"}})
	resp, out := e.call("gemini-embeddings", "/v1beta/models/gemini-embedding-001:batchEmbedContents",
		`{"requests":[{"model":"models/gemini-embedding-001","content":{"parts":[{"text":"a"}]},"outputDimensionality":64},{"model":"models/gemini-embedding-001","content":{"parts":[{"text":"b"}]}}]}`)
	if resp.StatusCode != 200 || string(out) != `{"embeddings":[{"values":[1,2]},{"values":[3,4]}]}` {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	sent := gjson.ParseBytes(up.body(0))
	if sent.Get("model").String() != "text-embedding-3-small" || sent.Get("input.#").Int() != 2 || sent.Get("dimensions").Int() != 64 ||
		sent.Get("encoding_format").Exists() {
		t.Fatalf("upstream body %s", up.body(0))
	}
	if rec := e.waitRecords(1); rec.Usage.Input != 7 {
		t.Errorf("usage %+v", rec.Usage)
	}

	up.handle = jsonReply(200, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[9,8]}],"model":"m"}`)
	resp, out = e.call("gemini-embeddings", "/v1beta/models/gemini-embedding-001:embedContent", `{"content":{"parts":[{"text":"one"}]}}`)
	if resp.StatusCode != 200 || string(out) != `{"embedding":{"values":[9,8]}}` {
		t.Fatalf("single: %d %s", resp.StatusCode, out)
	}
	if got := gjson.GetBytes(up.body(1), "input").Raw; got != `"one"` {
		t.Errorf("single input %s", got)
	}
}

// Operations no endpoint of the model serves are refused with 400
// capability_missing in the client's format, without calling upstream.
func TestMediaCapabilityMissing(t *testing.T) {
	cases := []struct {
		name, upProto, codec, path, body, missing string
	}{
		{"token ids to gemini", "gemini-embeddings", "openai-embeddings", "/v1/embeddings", `{"model":"m","input":[1,2,3]}`, "embed_tokens"},
		{"chat to an embeddings model", "openai-embeddings", "openai-chat", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "text_generation"},
		{"embeddings to a chat model", "openai-chat", "openai-embeddings", "/v1/embeddings", `{"model":"m","input":"hi"}`, "embeddings"},
		{"speech to an images endpoint", "openai-images", "openai-audio", "/v1/audio/speech", `{"model":"m","input":"hi","voice":"x"}`, "speech"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFake(t, jsonReply(200, `{}`))
			e := newEnv(t, "m", []upSpec{{id: "u", protocol: tc.upProto, issuer: "u", fake: up}})
			resp, out := e.call(tc.codec, tc.path, tc.body)
			if resp.StatusCode != 400 || !strings.Contains(string(out), tc.missing) || up.calls() != 0 {
				t.Fatalf("%d %s (calls %d)", resp.StatusCode, out, up.calls())
			}
			if rec := e.waitRecords(1); rec.ErrorKind != "capability_missing" {
				t.Errorf("record %s", rec.ErrorKind)
			}
		})
	}
	// An endpoint can narrow its protocol's operations.
	up := newFake(t, jsonReply(200, `{}`))
	e := newEnv(t, "m", []upSpec{{id: "u", protocol: "openai-audio", issuer: "u", fake: up, caps: "{speech: false}"}})
	if resp, out := e.call("openai-audio", "/v1/audio/speech", `{"model":"m","input":"hi","voice":"x"}`); resp.StatusCode != 400 {
		t.Fatalf("speech on a transcription-only endpoint: %d %s", resp.StatusCode, out)
	}
}

// Images are billed per image at the most specific size/quality rate,
// times the group multiplier; tokens the upstream reports add on.
func TestImagesBilledPerImage(t *testing.T) {
	up := newFake(t, jsonReply(200, `{"created":1,"data":[{"b64_json":"aGk="},{"b64_json":"aGk="}],"usage":{"input_tokens":10,"output_tokens":0}}`))
	e := newEnv(t, "img", []upSpec{{id: "o", protocol: "openai-images", issuer: "o", fake: up}})
	e.snap.Price = func(string, string) (Price, bool) {
		return Price{Units: map[string]int64{plugin.UnitImage: 40_000}}, true
	}
	e.snap.ModelPrice = func(string) (Price, bool) {
		return Price{Input: 5_000_000, Units: map[string]int64{plugin.UnitImage: 50_000, "image:1024x1024": 60_000, "image:1024x1024:hd": 80_000}}, true
	}
	e.snap.Multiplier = func(string) float64 { return 1.5 }
	e.caller.GroupID = "g"
	m := &estimateMeter{}
	e.chain.Meter = m
	body := `{"model":"img","prompt":"a red apple","n":2,"size":"1024x1024","quality":"hd"}`
	resp, out := e.call("openai-images", "/v1/images/generations", body)
	if resp.StatusCode != 200 || string(up.body(0)) != body || up.paths[0] != "/v1/images/generations" {
		t.Fatalf("%d %s → %s %s", resp.StatusCode, out, up.paths[0], up.body(0))
	}
	rec := e.waitRecords(1)
	if rec.Usage.Images != 2 || rec.Usage.ImageSize != "1024x1024" || rec.Usage.ImageQuality != "hd" {
		t.Fatalf("usage %+v", rec.Usage)
	}
	// 2 images × 80000 × 1.5 + 10 input tokens × 7_500_000 per million
	if rec.ChargeMicro != 240_075 || m.charged != 240_075 {
		t.Errorf("charge %d settled %d", rec.ChargeMicro, m.charged)
	}
	if rec.CostMicro != 80_000 {
		t.Errorf("cost %d, want 2 × 40000", rec.CostMicro)
	}
	if ch := rec.Attempts[0].Charge; ch == nil || ch.Rates.Units["image:1024x1024:hd"] != 120_000 {
		t.Errorf("charge detail %+v", ch)
	}
	if m.estimate < 240_000 {
		t.Errorf("reservation %d does not cover the images asked for", m.estimate)
	}
}

// Speech audio is copied to the client as it arrives and billed per 1000
// characters of input.
func TestSpeechStreamsAudio(t *testing.T) {
	chunks := [][]byte{[]byte("ID3\x00\x01"), bytes.Repeat([]byte{0xff, 0xfb}, 40_000), []byte("tail")}
	up := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "audio/mpeg")
		for _, c := range chunks {
			_, _ = w.Write(c)
			w.(http.Flusher).Flush()
		}
	})
	e := newEnv(t, "tts", []upSpec{{id: "o", protocol: "openai-audio", issuer: "o", fake: up}})
	e.snap.ModelPrice = func(string) (Price, bool) { return Price{Units: map[string]int64{plugin.UnitKChars: 15_000}}, true }
	input := strings.Repeat("ab", 750) // 1500 characters
	resp, out := e.call("openai-audio", "/v1/audio/speech", `{"model":"tts","input":"`+input+`","voice":"alloy"}`)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/mpeg" || !bytes.Equal(out, bytes.Join(chunks, nil)) {
		t.Fatalf("%d %s, %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(out))
	}
	rec := e.waitRecords(1)
	if rec.Usage.Chars != 1500 || rec.ChargeMicro != 22_500 {
		t.Errorf("usage %+v charge %d", rec.Usage, rec.ChargeMicro)
	}
}

// ---------------------------------------------------------------- uploads

// uploadServer serves an upload route like the HTTP server does: the body
// is bounded but not read.
func (e *env) uploadServer(codecName, path string, limit int64) *httptest.Server {
	codec, _ := protocol.Get(codecName)
	route, ok := matchRoute(codec, path)
	if !ok || !route.Upload {
		e.t.Fatalf("%s is not an upload route", path)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		caller := e.caller
		e.p.ServeProtocol(w, r, codec, route, &caller, nil)
	}))
	e.t.Cleanup(srv.Close)
	return srv
}

// upstreamForm is what a fake upstream received in a multipart body.
type upstreamForm struct {
	fields map[string]string
	files  map[string][]byte
	order  []string
}

func readForm(t *testing.T, r *http.Request, onFirstFileBytes func()) (upstreamForm, error) {
	f := upstreamForm{fields: map[string]string{}, files: map[string][]byte{}}
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return f, err
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return f, nil
		}
		if err != nil {
			return f, err
		}
		f.order = append(f.order, p.FormName())
		if p.FileName() == "" {
			b, _ := io.ReadAll(p)
			f.fields[p.FormName()] = string(b)
			continue
		}
		var buf bytes.Buffer
		chunk := make([]byte, 1024)
		n, err := io.ReadFull(p, chunk)
		buf.Write(chunk[:n])
		if onFirstFileBytes != nil && n > 0 {
			onFirstFileBytes()
		}
		if err == nil {
			if _, err := io.Copy(&buf, p); err != nil {
				return f, err
			}
		}
		f.files[p.FileName()] = buf.Bytes()
	}
}

// A transcription upload reaches the upstream while the client is still
// sending it, with the model field set to the upstream model, and is billed
// per second of audio the upstream reports.
func TestTranscriptionUploadStreams(t *testing.T) {
	gotFirst := make(chan struct{})
	var once sync.Once
	var got upstreamForm
	var formErr error
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, formErr = readForm(t, r, func() { once.Do(func() { close(gotFirst) }) })
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"hello","usage":{"type":"duration","seconds":12.5}}`)
	}))
	defer upSrv.Close()
	e := newEnv(t, "whisper", []upSpec{{id: "o", protocol: "openai-audio", issuer: "o", fake: &fakeUpstream{srv: upSrv}, model: "whisper-up"}})
	e.snap.ModelPrice = func(string) (Price, bool) { return Price{Units: map[string]int64{plugin.UnitAudioSecond: 100}}, true }
	srv := e.uploadServer("openai-audio", "/v1/audio/transcriptions", 64<<20)

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	audio := bytes.Repeat([]byte("RIFF-audio-"), 50_000) // 550 kB
	stalled := make(chan error, 1)
	go func() {
		_ = mw.WriteField("model", "whisper")
		_ = mw.WriteField("response_format", "json")
		fw, _ := mw.CreateFormFile("file", "speech.wav")
		_, _ = fw.Write(audio[:4096])
		// The rest is held back until the upstream has the start: a relay
		// that buffered the whole upload would never get here.
		select {
		case <-gotFirst:
		case <-time.After(5 * time.Second):
			stalled <- io.ErrNoProgress
			_ = pw.CloseWithError(io.ErrNoProgress)
			return
		}
		_, _ = fw.Write(audio[4096:])
		_ = mw.WriteField("language", "en")
		_ = mw.Close()
		_ = pw.Close()
		stalled <- nil
	}()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/audio/transcriptions", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err := <-stalled; err != nil {
		t.Fatal("the upstream saw nothing before the client finished: the upload was buffered")
	}
	if resp.StatusCode != 200 || gjson.GetBytes(out, "text").String() != "hello" {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if formErr != nil || got.fields["model"] != "whisper-up" || got.fields["language"] != "en" ||
		!bytes.Equal(got.files["speech.wav"], audio) || !reflect.DeepEqual(got.order, []string{"model", "response_format", "file", "language"}) {
		t.Fatalf("upstream form %v fields %v order %v, %d file bytes", formErr, got.fields, got.order, len(got.files["speech.wav"]))
	}
	rec := e.waitRecords(1)
	if rec.Usage.AudioMs != 12_500 || rec.ChargeMicro != 1250 || rec.Attempts[0].Tier != plugin.TierPatch {
		t.Errorf("usage %+v charge %d tier %s", rec.Usage, rec.ChargeMicro, rec.Attempts[0].Tier)
	}
}

func multipartBody(t *testing.T, modelFirst bool, file []byte) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if modelFirst {
		_ = mw.WriteField("model", "whisper")
	}
	fw, _ := mw.CreateFormFile("file", "a.mp3")
	_, _ = fw.Write(file)
	if !modelFirst {
		_ = mw.WriteField("model", "whisper")
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

// failThenServe answers 500 to the first call (after reading the body) and
// 200 afterwards, recording each form.
type formUpstream struct {
	mu    sync.Mutex
	forms []upstreamForm
	srv   *httptest.Server
}

func newFormUpstream(t *testing.T, status int) *formUpstream {
	f := &formUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		form, _ := readForm(t, r, nil)
		f.mu.Lock()
		f.forms = append(f.forms, form)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == 200 {
			_, _ = io.WriteString(w, `{"text":"ok"}`)
		} else {
			_, _ = io.WriteString(w, `{"error":{"message":"boom","type":"server_error"}}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// A model field after the file makes the relay spool the body first; the
// spooled copy is replayed to the next candidate after a failure.
func TestUploadModelAfterFileSpooledAndRetried(t *testing.T) {
	bad, good := newFormUpstream(t, 500), newFormUpstream(t, 200)
	e := newEnv(t, "whisper", []upSpec{
		{id: "bad", protocol: "openai-audio", issuer: "b", fake: &fakeUpstream{srv: bad.srv}, priority: 10},
		{id: "good", protocol: "openai-audio", issuer: "g", fake: &fakeUpstream{srv: good.srv}, model: "whisper-2"},
	})
	srv := e.uploadServer("openai-audio", "/v1/audio/transcriptions", 64<<20)
	file := bytes.Repeat([]byte{1, 2, 3}, 10_000)
	body, ct := multipartBody(t, false, file)
	resp, err := http.Post(srv.URL+"/v1/audio/transcriptions", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(bad.forms) != 1 || len(good.forms) != 1 {
		t.Fatalf("%d %s (bad %d, good %d)", resp.StatusCode, out, len(bad.forms), len(good.forms))
	}
	g := good.forms[0]
	if g.fields["model"] != "whisper-2" || !bytes.Equal(g.files["a.mp3"], file) || bad.forms[0].fields["model"] != "whisper" {
		t.Fatalf("forms %v / %v", bad.forms[0].fields, g.fields)
	}
}

// A streamed upload has gone to the first upstream; a failure there ends
// the request, since the body cannot be sent again.
func TestStreamedUploadNotRetried(t *testing.T) {
	bad, good := newFormUpstream(t, 500), newFormUpstream(t, 200)
	e := newEnv(t, "whisper", []upSpec{
		{id: "bad", protocol: "openai-audio", issuer: "b", fake: &fakeUpstream{srv: bad.srv}, priority: 10},
		{id: "good", protocol: "openai-audio", issuer: "g", fake: &fakeUpstream{srv: good.srv}},
	})
	srv := e.uploadServer("openai-audio", "/v1/audio/transcriptions", 64<<20)
	body, ct := multipartBody(t, true, []byte("audio"))
	resp, err := http.Post(srv.URL+"/v1/audio/transcriptions", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 500 || !strings.Contains(string(out), "boom") || len(good.forms) != 0 {
		t.Fatalf("%d %s (good called %d times)", resp.StatusCode, out, len(good.forms))
	}
}

// An upload over the body limit is a 413, whether the limit is hit while
// reading the form fields, while spooling, or mid-stream to the upstream.
func TestUploadTooLarge(t *testing.T) {
	for _, modelFirst := range []bool{true, false} {
		up := newFormUpstream(t, 200)
		e := newEnv(t, "whisper", []upSpec{{id: "o", protocol: "openai-audio", issuer: "o", fake: &fakeUpstream{srv: up.srv}}})
		srv := e.uploadServer("openai-audio", "/v1/audio/transcriptions", 8<<10)
		body, ct := multipartBody(t, modelFirst, bytes.Repeat([]byte("x"), 64<<10))
		// No Content-Length: the limit is only found while reading.
		req, _ := http.NewRequest("POST", srv.URL+"/v1/audio/transcriptions", io.MultiReader(body))
		req.Header.Set("Content-Type", ct)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(out), "exceeds") {
			t.Fatalf("model first %v: %d %s", modelFirst, resp.StatusCode, out)
		}
		if rec := e.waitRecords(1); len(rec.Attempts) > 1 {
			t.Errorf("retried: %d attempts", len(rec.Attempts))
		}
	}
	// Not multipart at all.
	e := newEnv(t, "whisper", []upSpec{{id: "o", protocol: "openai-audio", issuer: "o", fake: newFake(t, jsonReply(200, `{}`))}})
	srv := e.uploadServer("openai-audio", "/v1/audio/transcriptions", 8<<10)
	resp, err := http.Post(srv.URL+"/v1/audio/transcriptions", "application/json", strings.NewReader(`{"model":"whisper"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("JSON body on an upload route: %d", resp.StatusCode)
	}
}
