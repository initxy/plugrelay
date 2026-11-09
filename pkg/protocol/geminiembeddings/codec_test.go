package geminiembeddings

import (
	"bytes"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
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

func TestSniffActions(t *testing.T) {
	for _, tc := range []struct {
		path, action string
		ok           bool
	}{
		{"/v1beta/models/gemini-embedding-001:embedContent", ActionEmbed, true},
		{"/v1beta/models/gemini-embedding-001:batchEmbedContents", ActionBatch, true},
		{"/v1beta/models/gemini-embedding-001:generateContent", "", false},
	} {
		r := httptest.NewRequest("POST", tc.path, nil)
		in, err := codec.Sniff(r, codec.Routes()[0], []byte(`{}`))
		if (err == nil) != tc.ok || (tc.ok && (in.Action != tc.action || in.Model != "gemini-embedding-001" || in.Op != protocol.OpEmbed)) {
			t.Errorf("%s: %+v %v", tc.path, in, err)
		}
	}
}

func TestDecodeRequestGolden(t *testing.T) {
	in := protocol.Inbound{Model: "gemini-embedding-001", Action: ActionBatch}
	req, needs, err := codec.DecodeRequest(nil, in, fixture(t, "request_batch.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := &ir.Embed{Inputs: []ir.EmbedInput{{Text: "first\nline two"}, {Text: "second"}}, Dimensions: 768,
		TaskType: "RETRIEVAL_DOCUMENT", Title: "Doc", Batch: true}
	if !reflect.DeepEqual(req.Embed, want) || needs.Modality != ir.ModalityEmbeddings {
		t.Fatalf("embed %+v needs %+v", req.Embed, needs)
	}
	one := `{"content":{"parts":[{"text":"hi"}]},"outputDimensionality":3}`
	req, _, err = codec.DecodeRequest(nil, protocol.Inbound{Model: "m", Action: ActionEmbed}, []byte(one))
	if err != nil || req.Embed.Batch || len(req.Embed.Inputs) != 1 || req.Embed.Dimensions != 3 {
		t.Fatalf("single %+v %v", req.Embed, err)
	}
	// Non-text content cannot be converted.
	img := `{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}}`
	if _, _, err := codec.DecodeRequest(nil, protocol.Inbound{Action: ActionEmbed}, []byte(img)); err == nil {
		t.Error("image part decoded")
	}
}

func TestEncodeRequest(t *testing.T) {
	req := &ir.Request{Model: "gemini-embedding-001", Embed: &ir.Embed{Inputs: []ir.EmbedInput{{Text: "a"}, {Text: "b"}}, Dimensions: 256}}
	out, err := codec.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	r := gjson.ParseBytes(out)
	if r.Get("requests.#").Int() != 2 || r.Get("requests.1.model").String() != "models/gemini-embedding-001" ||
		r.Get("requests.1.content.parts.0.text").String() != "b" || r.Get("requests.0.outputDimensionality").Int() != 256 {
		t.Fatalf("encoded %s", out)
	}
	if p := codec.UpstreamPath(req, protocol.Inbound{}); p != "/v1beta/models/gemini-embedding-001:batchEmbedContents" {
		t.Errorf("path %s", p)
	}
	if p := codec.UpstreamPath(req, protocol.Inbound{Action: ActionEmbed}); p != "/v1beta/models/gemini-embedding-001:embedContent" {
		t.Errorf("same-protocol path %s", p)
	}
	req.Embed.Inputs = []ir.EmbedInput{{Tokens: []int64{1}}}
	if _, err := codec.EncodeRequest(req); err == nil {
		t.Error("token ids encoded")
	}
}

func TestResponseGolden(t *testing.T) {
	resp, err := codec.DecodeResponse(fixture(t, "response_batch.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resp.Embeddings, [][]float64{{0.1, -0.2, 0.3}, {0.4, 0.5, -0.6}}) {
		t.Fatalf("resp %+v", resp)
	}
	out, _ := codec.EncodeResponse(&ir.Request{Embed: &ir.Embed{Batch: true}}, resp)
	if !bytes.Equal(out, fixture(t, "response_batch.json")) {
		t.Errorf("batch %s", out)
	}
	one := &ir.Response{Embeddings: [][]float64{{1, 2}}}
	out, _ = codec.EncodeResponse(&ir.Request{Embed: &ir.Embed{}}, one)
	if string(out) != `{"embedding":{"values":[1,2]}}` {
		t.Errorf("single %s", out)
	}
	resp, err = codec.DecodeResponse([]byte(`{"embedding":{"values":[1,2]},"usageMetadata":{"promptTokenCount":4}}`))
	if err != nil || len(resp.Embeddings) != 1 || resp.Usage.Input != 4 {
		t.Fatalf("single response %+v %v", resp, err)
	}
}

func TestSetModelByteExact(t *testing.T) {
	in := fixture(t, "request_batch.json")
	out, err := codec.SetModel(in, "gemini-embedding-002")
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.ReplaceAll(in, []byte("models/gemini-embedding-001"), []byte("models/gemini-embedding-002"))
	if !bytes.Equal(out, want) {
		t.Fatalf("got %s", out)
	}
	plain := []byte(`{"content":{"parts":[{"text":"x"}]}}`)
	if out, _ := codec.SetModel(plain, "m"); !bytes.Equal(out, plain) {
		t.Errorf("model added to %s", out)
	}
}
