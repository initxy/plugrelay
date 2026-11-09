package pipeline

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

func wavFile(byteRate, dataLen int) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+dataLen))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, []uint16{1, 1})     // PCM, mono
	binary.Write(&b, binary.LittleEndian, uint32(byteRate/2)) // sample rate
	binary.Write(&b, binary.LittleEndian, uint32(byteRate))   // byte rate
	binary.Write(&b, binary.LittleEndian, []uint16{2, 16})    // block align, bits
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(dataLen))
	b.Write(make([]byte, dataLen))
	return b.Bytes()
}

func oggPage(granule uint64, packet []byte) []byte {
	var b bytes.Buffer
	b.WriteString("OggS")
	b.Write([]byte{0, 2})
	binary.Write(&b, binary.LittleEndian, granule)
	b.Write(make([]byte, 12)) // serial, sequence, checksum
	b.Write([]byte{1, byte(len(packet))})
	b.Write(packet)
	return b.Bytes()
}

func TestAudioDuration(t *testing.T) {
	mp3CBR := append([]byte{0xff, 0xfb, 0x90, 0x64}, make([]byte, 16000-4)...) // MPEG-1 L3 128 kbit/s
	xing := append([]byte{0xff, 0xfb, 0x90, 0xc4}, make([]byte, 17)...)        // mono: 17 bytes side info
	xing = append(xing, "Xing\x00\x00\x00\x01\x00\x00\x00\x64"...)             // 100 frames
	xing = append(xing, make([]byte, 400)...)
	id3 := append([]byte("ID3\x03\x00\x00\x00\x00\x01\x00"), make([]byte, 128)...) // 128-byte tag
	id3 = append(id3, mp3CBR...)

	flac := []byte("fLaC\x00\x00\x00\x22")
	si := make([]byte, 34)
	si[10], si[11], si[12] = 0x0a, 0xc4, 0x40 // 44100 Hz, 1 channel, 16 bits
	binary.BigEndian.PutUint32(si[14:], 441000)
	flac = append(flac, si...)

	opusHead := append([]byte("OpusHead\x01\x01"), 0x38, 0x01) // pre-skip 312
	opusHead = append(opusHead, make([]byte, 8)...)
	opus := append(oggPage(0, opusHead), make([]byte, 200<<10)...)
	opus = append(opus, oggPage(48000*5+312, []byte("x"))...)

	vorbisID := append([]byte("\x01vorbis\x00\x00\x00\x00\x02"), 0x44, 0xac, 0, 0) // 44100 Hz
	vorbisID = append(vorbisID, make([]byte, 16)...)
	vorbis := append(oggPage(0, vorbisID), oggPage(44100*2, []byte("y"))...)

	mvhd := make([]byte, 100)
	copy(mvhd[4:], "mvhd")
	binary.BigEndian.PutUint32(mvhd[20:], 1000) // v0: timescale
	binary.BigEndian.PutUint32(mvhd[24:], 7500) // duration
	mp4 := append([]byte("\x00\x00\x00\x18ftypM4A \x00\x00\x00\x00M4A mp42"), make([]byte, 300<<10)...)
	mp4 = append(mp4, mvhd...) // moov at the end, past the head

	dur := make([]byte, 4)
	binary.BigEndian.PutUint32(dur, math.Float32bits(4200))
	webm := append([]byte{0x1a, 0x45, 0xdf, 0xa3, 0x80, 0x2a, 0xd7, 0xb1, 0x83, 0x0f, 0x42, 0x40, 0x44, 0x89, 0x84}, dur...)
	webm = append(webm, make([]byte, 1000)...)
	webmNoDur := append([]byte{0x1a, 0x45, 0xdf, 0xa3}, make([]byte, 8000-4)...)

	for _, tc := range []struct {
		name string
		file []byte
		want int64
	}{
		{"wav", wavFile(32000, 96000), 3000},
		{"wav streaming size", func() []byte { b := wavFile(32000, 64000); copy(b[40:44], []byte{0xff, 0xff, 0xff, 0xff}); return b }(), 2000},
		// A forged byte rate claims 10 ms; the bytes at floorKbps win.
		{"wav forged byte rate", wavFile(100_000_000, 1_000_000), (1_000_000 + 44) * 8 / floorKbps},
		{"mp3 cbr", mp3CBR, 1000},
		{"mp3 after id3", id3, 1000},
		{"mp3 xing", xing, 100 * 1152 * 1000 / 44100},
		{"flac", flac, 10000},
		{"ogg opus", opus, 5000},
		{"ogg vorbis", vorbis, 2000},
		{"m4a moov at end", mp4, 7500},
		{"webm", webm, 4200},
		{"webm without duration", webmNoDur, 8000 * 8 / fallbackKbps},
		{"unknown", bytes.Repeat([]byte("abc"), 8000), 24000 * 8 / fallbackKbps},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &audioProbe{}
			if _, err := io.Copy(io.Discard, p.tee(bytes.NewReader(tc.file))); err != nil {
				t.Fatal(err)
			}
			if got := p.ms(); got != tc.want {
				t.Fatalf("%d ms, want %d", got, tc.want)
			}
		})
	}
}

func transcribeForm(t *testing.T, fields map[string]string, file []byte) (*bytes.Buffer, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "whisper")
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", "a.wav")
	_, _ = fw.Write(file)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

// Transcripts that carry no duration (text, SRT, VTT, an event stream, a
// bare JSON text) are billed by the uploaded audio's length, estimated.
func TestTranscriptBilledWithoutReportedDuration(t *testing.T) {
	for _, tc := range []struct {
		name, format, ctype, body string
		stream                    bool
	}{
		{"text", "text", "text/plain; charset=utf-8", "hello", false},
		{"srt", "srt", "application/x-subrip", "1\n00:00:00,000 --> 00:00:01,000\nhello\n", false},
		{"vtt", "vtt", "text/vtt", "WEBVTT\n\n00:00.000 --> 00:01.000\nhello\n", false},
		{"bare json", "json", "application/json", `{"text":"hello"}`, false},
		{"stream", "json", "text/event-stream", "data: {\"type\":\"transcript.text.done\",\"text\":\"hello\"}\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				w.Header().Set("Content-Type", tc.ctype)
				_, _ = io.WriteString(w, tc.body)
			})
			e := newEnv(t, "whisper", []upSpec{{id: "o", protocol: "openai-audio", issuer: "o", fake: up}})
			e.snap.ModelPrice = func(string) (Price, bool) { return Price{Units: map[string]int64{plugin.UnitAudioSecond: 100}}, true }
			srv := e.uploadServer("openai-audio", "/v1/audio/transcriptions", 64<<20)
			fields := map[string]string{"response_format": tc.format}
			if tc.stream {
				fields["stream"] = "true"
			}
			body, ct := transcribeForm(t, fields, wavFile(32000, 32000*12)) // 12 s
			resp, err := http.Post(srv.URL+"/v1/audio/transcriptions", ct, body)
			if err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || string(out) != tc.body {
				t.Fatalf("%d %q", resp.StatusCode, out)
			}
			rec := e.waitRecords(1)
			if rec.Usage.AudioMs != 12_000 || !rec.Usage.Estimated || rec.ChargeMicro != 1200 {
				t.Fatalf("usage %+v charge %d", rec.Usage, rec.ChargeMicro)
			}
		})
	}
	// A reported duration wins over the estimate.
	up := newFake(t, jsonReply(200, `{"text":"hi","usage":{"type":"duration","seconds":3}}`))
	e := newEnv(t, "whisper", []upSpec{{id: "o", protocol: "openai-audio", issuer: "o", fake: up}})
	srv := e.uploadServer("openai-audio", "/v1/audio/transcriptions", 64<<20)
	body, ct := transcribeForm(t, nil, wavFile(32000, 32000*12))
	resp, err := http.Post(srv.URL+"/v1/audio/transcriptions", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if rec := e.waitRecords(1); rec.Usage.AudioMs != 3000 || rec.Usage.Estimated {
		t.Fatalf("reported duration: %+v", rec.Usage)
	}
}

// A streamed image response is billed for the images asked for.
func TestImageStreamBilledPerImage(t *testing.T) {
	up := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: image_generation.completed\ndata: {\"b64_json\":\"aGk=\"}\n\n")
	})
	e := newEnv(t, "img", []upSpec{{id: "o", protocol: "openai-images", issuer: "o", fake: up}})
	e.snap.ModelPrice = func(string) (Price, bool) {
		return Price{Units: map[string]int64{plugin.UnitImage: 50_000, "image:1024x1024:hd": 80_000}}, true
	}
	for i, tc := range []struct {
		body   string
		images int64
		charge int64
	}{
		{`{"model":"img","prompt":"apple","stream":true,"n":3,"size":"1024x1024","quality":"hd"}`, 3, 240_000},
		{`{"model":"img","prompt":"apple","stream":true}`, 1, 50_000},
	} {
		resp, _ := e.call("openai-images", "/v1/images/generations", tc.body)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		rec := e.waitRecords(i + 1)
		if rec.Usage.Images != tc.images || !rec.Usage.Estimated || rec.ChargeMicro != tc.charge {
			t.Fatalf("%s: usage %+v charge %d", tc.body, rec.Usage, rec.ChargeMicro)
		}
	}
}

// unitMeter records whether each reserved call was marked unit-priced.
type unitMeter struct{ unit []bool }

func (m *unitMeter) Info() plugin.Info { return plugin.Info{ID: "meter.units", Kind: plugin.KindMeter} }

func (m *unitMeter) Reserve(c *plugin.Call, _ int64) (*plugin.Reservation, error) {
	m.unit = append(m.unit, c.UnitPriced)
	return &plugin.Reservation{ID: c.ID}, nil
}

func (m *unitMeter) Settle(*plugin.Call, *plugin.Reservation, ir.Usage, int64) error { return nil }

func (m *unitMeter) Refund(*plugin.Call, *plugin.Reservation) error { return nil }

// The meter learns that an image request may be priced per unit (a token
// plan does not cover it) and that an embeddings request is not.
func TestMeterSeesUnitPriced(t *testing.T) {
	img := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		_, _ = io.WriteString(w, `{"created":1,"data":[{"b64_json":"aGk="}]}`)
	})
	m := &unitMeter{}
	e := newEnv(t, "img", []upSpec{{id: "o", protocol: "openai-images", issuer: "o", fake: img}})
	e.chain.Meter = m
	if resp, _ := e.call("openai-images", "/v1/images/generations", `{"model":"img","prompt":"apple"}`); resp.StatusCode != 200 {
		t.Fatalf("image: %d", resp.StatusCode)
	}
	emb := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"model":"e","usage":{"prompt_tokens":1,"total_tokens":1}}`)
	})
	e2 := newEnv(t, "emb", []upSpec{{id: "o", protocol: "openai-embeddings", issuer: "o", fake: emb}})
	e2.chain.Meter = m
	if resp, _ := e2.call("openai-embeddings", "/v1/embeddings", `{"model":"emb","input":"hi"}`); resp.StatusCode != 200 {
		t.Fatalf("embeddings: %d", resp.StatusCode)
	}
	if len(m.unit) != 2 || !m.unit[0] || m.unit[1] {
		t.Fatalf("unit-priced per call: %v", m.unit)
	}
}
