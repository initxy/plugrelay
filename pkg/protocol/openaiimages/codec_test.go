package openaiimages

import (
	"reflect"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
)

var codec = Codec{}

func TestDecodeRequest(t *testing.T) {
	gen := `{"model":"gpt-image-1","prompt":"a red apple","n":2,"size":"1024x1024","quality":"high"}`
	req, needs, err := codec.DecodeRequest(nil, protocol.Inbound{Op: protocol.OpImage}, []byte(gen))
	if err != nil {
		t.Fatal(err)
	}
	if *req.Units != (ir.MediaUnits{Images: 2, ImageSize: "1024x1024", ImageQuality: "high"}) || needs.Modality != ir.ModalityImageGeneration {
		t.Fatalf("units %+v needs %+v", req.Units, needs)
	}
	// Form fields of an edit upload arrive as strings.
	edit := `{"model":"gpt-image-1","prompt":"x","n":"3","image[]":[{"filename":"a.png"},{"filename":"b.png"}]}`
	req, needs, err = codec.DecodeRequest(nil, protocol.Inbound{Op: protocol.OpImageEdit}, []byte(edit))
	if err != nil || req.Units.Images != 3 || needs.Modality != ir.ModalityImageEdit {
		t.Fatalf("edit %+v %+v %v", req.Units, needs, err)
	}
	if _, err := codec.EncodeRequest(req); err == nil {
		t.Error("image request converted")
	}
}

func TestDecodeResponse(t *testing.T) {
	cases := []struct {
		body string
		want ir.Usage
	}{
		{`{"created":1,"data":[{"b64_json":"aGk="},{"b64_json":"aGk="}],"size":"1536x1024","quality":"high",` +
			`"usage":{"input_tokens":50,"output_tokens":4160,"total_tokens":4210}}`,
			ir.Usage{Images: 2, ImageSize: "1536x1024", ImageQuality: "high", Input: 50, Output: 4160}},
		{`{"created":1,"data":[{"url":"https://x/1.jpeg","size":"1024x1024"}],"usage":{"generated_images":1,"output_tokens":4096}}`,
			ir.Usage{Images: 1, ImageSize: "1024x1024", Output: 4096}},
		{`{"created":1,"data":[{"url":"https://x/1.png"}]}`, ir.Usage{Images: 1}},
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
	if _, err := codec.DecodeResponse([]byte(`{"error":{}}`)); err == nil {
		t.Error("response without data accepted")
	}
}

func TestPathsAndStreams(t *testing.T) {
	if p := codec.UpstreamPath(nil, protocol.Inbound{Op: protocol.OpImageEdit}); p != "/images/edits" {
		t.Error(p)
	}
	if p := codec.UpstreamPath(nil, protocol.Inbound{Op: protocol.OpImage}); p != "/images/generations" {
		t.Error(p)
	}
	if !codec.StreamsBody(protocol.Inbound{}, "text/event-stream; charset=utf-8") || codec.StreamsBody(protocol.Inbound{}, "application/json") {
		t.Error("StreamsBody")
	}
	if !codec.Routes()[1].Upload || codec.Routes()[0].Upload {
		t.Error("only edits are uploads")
	}
}
