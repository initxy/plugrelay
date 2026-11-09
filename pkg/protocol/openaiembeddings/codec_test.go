package openaiembeddings

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/protocol/internal/media"
	"github.com/tidwall/gjson"
)

var codec = Codec{}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(b)
}

func TestRegistered(t *testing.T) {
	c, ok := protocol.Get(Name)
	if !ok {
		t.Fatal("not registered")
	}
	caps := c.Capabilities()
	if !caps.MediaOnly || !caps.Embeddings || !caps.EmbedTokens || caps.Serves(ir.ModalityText) || caps.Serves(ir.ModalitySpeech) {
		t.Fatalf("caps %+v", caps)
	}
}

func TestDecodeRequestGolden(t *testing.T) {
	req, needs, err := codec.DecodeRequest(nil, protocol.Inbound{}, fixture(t, "request_batch.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := &ir.Embed{Inputs: []ir.EmbedInput{{Text: "first text"}, {Text: "second text"}}, Dimensions: 256,
		Base64: true, User: "u-1", Batch: true}
	if !reflect.DeepEqual(req.Embed, want) || req.Model != "text-embedding-3-small" {
		t.Fatalf("embed %+v", req.Embed)
	}
	if needs.Modality != ir.ModalityEmbeddings || needs.EmbedTokens || needs.InputTokensEst == 0 {
		t.Fatalf("needs %+v", needs)
	}
	if raw, ok := req.Extra.Get(Name, "extra_field"); !ok || string(raw) != `{"keep":true}` {
		t.Fatalf("extra %v", req.Extra)
	}
	// Re-encoded for this protocol: floats are asked for, the rest kept.
	out, err := codec.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	r := gjson.ParseBytes(out)
	if r.Get("encoding_format").Exists() || r.Get("dimensions").Int() != 256 || r.Get("input.#").Int() != 2 ||
		r.Get("user").String() != "u-1" || !r.Get("extra_field.keep").Bool() {
		t.Fatalf("encoded %s", out)
	}
}

func TestTokenInputs(t *testing.T) {
	cases := []struct {
		body  string
		want  []ir.EmbedInput
		batch bool
	}{
		{`{"model":"m","input":"one"}`, []ir.EmbedInput{{Text: "one"}}, false},
		{`{"model":"m","input":[1,2,3]}`, []ir.EmbedInput{{Tokens: []int64{1, 2, 3}}}, false},
		{`{"model":"m","input":[[1,2],[3]]}`, []ir.EmbedInput{{Tokens: []int64{1, 2}}, {Tokens: []int64{3}}}, true},
	}
	for _, tc := range cases {
		req, needs, err := codec.DecodeRequest(nil, protocol.Inbound{}, []byte(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(req.Embed.Inputs, tc.want) || req.Embed.Batch != tc.batch {
			t.Errorf("%s: %+v", tc.body, req.Embed)
		}
		tokens := tc.want[0].Tokens != nil
		if needs.EmbedTokens != tokens {
			t.Errorf("%s: EmbedTokens %v", tc.body, needs.EmbedTokens)
		}
		out, _ := codec.EncodeRequest(req)
		if got := gjson.GetBytes(out, "input").Raw; !jsonSame(got, gjson.Get(tc.body, "input").Raw) {
			t.Errorf("%s: input re-encoded as %s", tc.body, got)
		}
	}
	for _, bad := range []string{`{"model":"m","input":[]}`, `{"model":"m"}`, `{"model":"m","input":[1,"x"]}`,
		`{"model":"m","input":"x","encoding_format":"int8"}`} {
		if _, _, err := codec.DecodeRequest(nil, protocol.Inbound{}, []byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func jsonSame(a, b string) bool {
	var x, y any
	_ = json.Unmarshal([]byte(a), &x)
	_ = json.Unmarshal([]byte(b), &y)
	return reflect.DeepEqual(x, y)
}

func TestResponseGolden(t *testing.T) {
	resp, err := codec.DecodeResponse(fixture(t, "response_float.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := [][]float64{{0.0023064255, -0.009327292}, {0.5, -0.25}}
	if !reflect.DeepEqual(resp.Embeddings, want) || resp.Usage.Input != 8 {
		t.Fatalf("resp %+v", resp)
	}
	// Floats render as they came.
	out, err := codec.EncodeResponse(&ir.Request{Embed: &ir.Embed{}}, resp)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "data.0.embedding").Raw; got != "[0.0023064255,-0.009327292]" {
		t.Errorf("floats %s", got)
	}
	if gjson.GetBytes(out, "usage.prompt_tokens").Int() != 8 || gjson.GetBytes(out, "data.1.index").Int() != 1 {
		t.Errorf("response %s", out)
	}
	// The client asked for base64: the relay encodes float32 LE.
	out, _ = codec.EncodeResponse(&ir.Request{Embed: &ir.Embed{Base64: true}}, resp)
	s := gjson.GetBytes(out, "data.1.embedding").String()
	back, err := media.DecodeBase64(s)
	if err != nil || !reflect.DeepEqual(back, []float64{0.5, -0.25}) {
		t.Fatalf("base64 %q → %v, %v", s, back, err)
	}
	// And base64 from an upstream decodes back to floats.
	b64 := `{"object":"list","data":[{"object":"embedding","index":0,"embedding":"` + s + `"}],"model":"m"}`
	resp, err = codec.DecodeResponse([]byte(b64))
	if err != nil || !reflect.DeepEqual(resp.Embeddings[0], []float64{0.5, -0.25}) {
		t.Fatalf("decoded base64 %+v %v", resp, err)
	}
}

func TestSetModelByteExact(t *testing.T) {
	in := fixture(t, "request_batch.json")
	out, err := codec.SetModel(in, "other")
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(in, []byte(`"text-embedding-3-small"`), []byte(`"other"`), 1)
	if !bytes.Equal(out, want) {
		t.Fatalf("got %s", out)
	}
}
