package openaiaudio

import (
	"reflect"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
)

var codec = Codec{}

func TestDecodeRequest(t *testing.T) {
	req, needs, err := codec.DecodeRequest(nil, protocol.Inbound{Op: protocol.OpSpeech},
		[]byte(`{"model":"tts-1","input":"héllo wörld","voice":"alloy"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Units == nil || req.Units.Chars != 11 || needs.Modality != ir.ModalitySpeech {
		t.Fatalf("speech %+v %+v", req.Units, needs)
	}
	req, needs, err = codec.DecodeRequest(nil, protocol.Inbound{Op: protocol.OpTranscribe},
		[]byte(`{"model":"whisper-1","file":{"filename":"a.mp3"}}`))
	if err != nil || req.Model != "whisper-1" || needs.Modality != ir.ModalityTranscription {
		t.Fatalf("transcription %+v %+v %v", req, needs, err)
	}
}

func TestDecodeResponseUsage(t *testing.T) {
	cases := []struct {
		body string
		want ir.Usage
	}{
		{`{"text":"hi","usage":{"type":"duration","seconds":3}}`, ir.Usage{AudioMs: 3000}},
		{`{"task":"transcribe","duration":3.52,"text":"hi","segments":[]}`, ir.Usage{AudioMs: 3520}},
		{`{"text":"hi","usage":{"type":"tokens","input_tokens":14,"output_tokens":45,"total_tokens":59}}`, ir.Usage{Input: 14, Output: 45}},
	}
	for _, tc := range cases {
		r, err := codec.DecodeResponse([]byte(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		r.Usage.Raw = nil
		if !reflect.DeepEqual(r.Usage, tc.want) {
			t.Errorf("%s: %+v", tc.body, r.Usage)
		}
	}
	// Plain text transcripts carry nothing billable.
	if _, err := codec.DecodeResponse([]byte("hello there\n")); err == nil {
		t.Error("text transcript decoded")
	}
}

func TestRoutesAndPaths(t *testing.T) {
	for _, rt := range codec.Routes() {
		if rt.Upload != (rt.Op != protocol.OpSpeech) {
			t.Errorf("%s upload=%v", rt.Path, rt.Upload)
		}
		if p := codec.UpstreamPath(nil, protocol.Inbound{Op: rt.Op}); "/v1"+p != rt.Path {
			t.Errorf("%s upstream %s", rt.Path, p)
		}
	}
	if !codec.StreamsBody(protocol.Inbound{Op: protocol.OpSpeech}, "audio/mpeg") ||
		codec.StreamsBody(protocol.Inbound{Op: protocol.OpTranscribe}, "application/json") ||
		!codec.StreamsBody(protocol.Inbound{Op: protocol.OpTranscribe}, "text/event-stream") {
		t.Error("StreamsBody")
	}
}
