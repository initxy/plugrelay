package rerank_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/protocol/coherererank"
	"github.com/initxy/plugrelay/pkg/protocol/rerank"
	"github.com/initxy/plugrelay/pkg/protocol/teirerank"
)

var (
	jina   protocol.Codec = rerank.Codec{}
	cohere protocol.Codec = coherererank.Codec{}
	tei    protocol.Codec = teirerank.Codec{}
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(b)
}

// golden compares JSON semantically (map keys come out sorted).
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	want := read(t, name)
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("%s: got invalid JSON %s", name, got)
	}
	_ = json.Unmarshal(want, &b)
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	if !bytes.Equal(ga, gb) {
		t.Errorf("%s:\n got  %s\n want %s", name, ga, gb)
	}
}

func decode(t *testing.T, c protocol.Codec, body []byte) (*ir.Request, ir.Needs) {
	t.Helper()
	req, needs, err := c.DecodeRequest(nil, protocol.Inbound{}, body)
	if err != nil {
		t.Fatal(err)
	}
	return req, needs
}

func TestDecodeRequest(t *testing.T) {
	req, needs := decode(t, jina, read(t, "request_jina.json"))
	rr := req.Rerank
	if needs.Modality != ir.ModalityRerank || needs.InputTokensEst == 0 {
		t.Errorf("needs %+v", needs)
	}
	if rr.Query != "capital of France" || len(rr.Documents) != 3 || rr.TopN != 2 || rr.ReturnDocuments == nil || !*rr.ReturnDocuments {
		t.Fatalf("rerank %+v", rr)
	}
	if rr.Documents[1].Text != "Paris is the capital of France." || gjson.GetBytes(rr.Documents[1].Object, "id").String() != "doc-7" ||
		rr.Documents[0].Object != nil {
		t.Errorf("documents %+v", rr.Documents)
	}
	if _, ok := req.Extra.Fields["truncation"]; !ok || req.Extra.Origin != rerank.Name {
		t.Errorf("extra %+v", req.Extra)
	}
	for _, bad := range []string{
		`{"model":"m","query":"q","documents":[{"image":"https://x/y.png"}]}`,
		`{"model":"m","query":{"content":[]},"documents":["a"]}`,
		`{"model":"m","query":"q","documents":[]}`,
		`{"model":"m","query":"q"}`,
	} {
		if _, _, err := jina.DecodeRequest(nil, protocol.Inbound{}, []byte(bad)); err == nil {
			t.Errorf("decoded %s", bad)
		}
	}
}

func TestResponsesAndUsage(t *testing.T) {
	r, err := jina.DecodeResponse(read(t, "response_vllm.json"))
	if err != nil || len(r.Ranks) != 2 || r.Ranks[0].Index != 1 || r.Usage.Input != 56 || r.Model != "BAAI/bge-reranker-base" {
		t.Fatalf("vllm %+v %v", r, err)
	}
	r, err = cohere.DecodeResponse(read(t, "response_cohere_v2.json"))
	if err != nil || r.Ranks[0].Index != 1 || r.Usage.SearchUnits != 1 || r.Usage.Input != 0 {
		t.Fatalf("cohere %+v %v", r, err)
	}
	golden(t, "golden_cohere_to_cohere_client.json", must(cohere.EncodeResponse(&ir.Request{Rerank: &ir.Rerank{}}, r)))
	r, _ = cohere.DecodeResponse(read(t, "response_cohere_tokens.json"))
	if r.Usage.Input != 192 || r.Usage.SearchUnits != 0 {
		t.Errorf("cohere tokens %+v", r.Usage)
	}
	r, err = tei.DecodeResponse(read(t, "response_tei.json"))
	if err != nil || len(r.Ranks) != 3 || r.Ranks[2].Index != 0 || !r.Usage.IsZero() {
		t.Fatalf("tei %+v %v", r, err)
	}
	if _, err := tei.DecodeResponse([]byte(`{"error":"x"}`)); err == nil {
		t.Error("tei accepted an object")
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// A common-shape client served by TEI: texts only, every one scored; the
// client gets the best top_n with its documents (objects as sent).
func TestJinaToTEI(t *testing.T) {
	req, _ := decode(t, jina, read(t, "request_jina.json"))
	golden(t, "golden_jina_to_tei.json", must(tei.EncodeRequest(req)))
	resp, err := tei.DecodeResponse(read(t, "response_tei.json"))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "golden_tei_to_jina_client.json", must(jina.EncodeResponse(req, resp)))
}

// Cohere v2 takes strings only and has no return_documents; its results
// come back to a common-shape client with the documents filled in.
func TestJinaToCohere(t *testing.T) {
	req, _ := decode(t, jina, read(t, "request_jina.json"))
	golden(t, "golden_jina_to_cohere.json", must(cohere.EncodeRequest(req)))
	resp, _ := cohere.DecodeResponse(read(t, "response_cohere_v2.json"))
	out := must(jina.EncodeResponse(req, resp))
	if gjson.GetBytes(out, "results.0.document.text").String() != "Paris is the capital of France." ||
		gjson.GetBytes(out, "results.1.document.text").String() != "France borders Spain." ||
		gjson.GetBytes(out, "meta.billed_units.search_units").Int() != 1 {
		t.Errorf("client got %s", out)
	}
	off := false
	req.Rerank.ReturnDocuments = &off
	if out := must(jina.EncodeResponse(req, resp)); gjson.GetBytes(out, "results.0.document").Exists() {
		t.Errorf("documents returned when turned off: %s", out)
	}
}

// A Cohere v2 client served by a common-shape upstream: return_documents
// is not invented, and the documents the upstream returns are not shown.
func TestCohereToJina(t *testing.T) {
	req, _ := decode(t, cohere, []byte(`{"model":"rerank-v3.5","query":"q","documents":["a","b"],"top_n":1,"max_tokens_per_doc":512,"priority":1}`))
	body := must(jina.EncodeRequest(req))
	if gjson.GetBytes(body, "return_documents").Exists() || gjson.GetBytes(body, "priority").Exists() ||
		gjson.GetBytes(body, "max_tokens_per_doc").Int() != 512 || gjson.GetBytes(body, "documents.1").String() != "b" {
		t.Errorf("upstream body %s", body)
	}
	resp, _ := jina.DecodeResponse(read(t, "response_vllm.json"))
	out := must(cohere.EncodeResponse(req, resp))
	if gjson.GetBytes(out, "results.#").Int() != 1 || gjson.GetBytes(out, "results.0.document").Exists() ||
		gjson.GetBytes(out, "meta.billed_units.input_tokens").Int() != 56 {
		t.Errorf("client got %s", out)
	}
}

// Usage in the places vendors put it outside the common shape.
func TestUsageVariants(t *testing.T) {
	for _, c := range []struct {
		name, body  string
		input, unit int64
	}{
		{"siliconflow", `{"results":[],"tokens":{"input_tokens":42,"output_tokens":0}}`, 42, 0},
		{"openrouter", `{"results":[],"usage":{"total_tokens":30,"search_units":2,"cost":0.001}}`, 30, 2},
		{"prompt 0 next to the total", `{"results":[],"usage":{"prompt_tokens":0,"total_tokens":25}}`, 25, 0},
		{"prompt only", `{"results":[],"usage":{"prompt_tokens":0}}`, 0, 0},
		{"portkey", `{"results":[],"usage":{"search_units":1}}`, 0, 1},
		{"cohere", `{"results":[],"meta":{"billed_units":{"search_units":3}}}`, 0, 3},
	} {
		resp, err := jina.DecodeResponse([]byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if resp.Usage.Input != c.input || resp.Usage.SearchUnits != c.unit {
			t.Errorf("%s: input %d, search units %d", c.name, resp.Usage.Input, resp.Usage.SearchUnits)
		}
	}
}

func TestSetModelAndErrors(t *testing.T) {
	body := []byte(`{ "query":"q", "model" : "a","documents":["x"]}`)
	out, err := jina.SetModel(body, "b")
	if err != nil || string(out) != `{ "query":"q", "model" : "b","documents":["x"]}` {
		t.Errorf("SetModel %s %v", out, err)
	}
	if out, _ := tei.SetModel(body, "b"); !bytes.Equal(out, body) {
		t.Errorf("tei SetModel changed the body")
	}
	_, _, eb := cohere.EncodeError(ir.Errorf(ir.ErrInvalidRequest, "bad"))
	if string(eb) != `{"message":"bad"}` {
		t.Errorf("cohere error %s", eb)
	}
	if e := tei.DecodeError(413, nil, []byte(`{"error":"Input validation error","error_type":"Validation"}`)); e.Message != "Input validation error" {
		t.Errorf("tei error %+v", e)
	}
	if e := cohere.DecodeError(429, nil, []byte(`{"message":"too many"}`)); e.Kind != ir.ErrRateLimited || e.Message != "too many" {
		t.Errorf("cohere error %+v", e)
	}
	if len(tei.Routes()) != 0 || jina.Routes()[0].Path != "/v1/rerank" || cohere.Routes()[0].Path != "/v2/rerank" {
		t.Error("routes")
	}
}
