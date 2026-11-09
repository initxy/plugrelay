package pipeline

import (
	"testing"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/plugin"
)

const rerankResp = `{"id":"rerank-1","model":"bge-up","usage":{"total_tokens":56},"results":[{"index":1,"document":{"text":"b"},"relevance_score":0.99}]}`

// Same protocol: request and response bytes are untouched and the request
// is billed on the upstream's token count.
func TestRerankPassthroughByteExact(t *testing.T) {
	up := newFake(t, jsonReply(200, rerankResp))
	e := newEnv(t, "rr", []upSpec{{id: "v", protocol: "rerank", issuer: "v", fake: up}})
	e.snap.ModelPrice = func(string) (Price, bool) { return Price{Input: 1_000_000}, true }
	body := `{ "query":"q","model":"rr", "documents":["a","b"],"top_n":1 ,"x_vendor":true}`
	resp, out := e.call("rerank", "/v1/rerank", body)
	if resp.StatusCode != 200 || string(out) != rerankResp {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if got := string(up.body(0)); got != body || up.paths[0] != "/rerank" {
		t.Fatalf("upstream got %s at %s", got, up.paths[0])
	}
	rec := e.waitRecords(1)
	if rec.Attempts[0].Tier != plugin.TierPassthrough || rec.Usage.Input != 56 || rec.ChargeMicro != 56 {
		t.Errorf("tier %s usage %+v charge %d", rec.Attempts[0].Tier, rec.Usage, rec.ChargeMicro)
	}
}

// Search units are priced per unit (Cohere bills one per query of up to
// 100 documents); a different upstream model name is patched in.
func TestRerankSearchUnitBilling(t *testing.T) {
	up := newFake(t, jsonReply(200, `{"id":"x","results":[{"index":0,"relevance_score":0.5}],"meta":{"billed_units":{"search_units":2}}}`))
	e := newEnv(t, "rr", []upSpec{{id: "c", protocol: "cohere-rerank", issuer: "c", fake: up, model: "rerank-v3.5"}})
	e.snap.ModelPrice = func(string) (Price, bool) {
		return Price{Units: map[string]int64{plugin.UnitSearch: 2_000}}, true
	}
	resp, out := e.call("cohere-rerank", "/v2/rerank", `{"model":"rr","query":"q","documents":["a"]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if gjson.GetBytes(up.body(0), "model").String() != "rerank-v3.5" {
		t.Errorf("upstream body %s", up.body(0))
	}
	rec := e.waitRecords(1)
	if rec.Attempts[0].Tier != plugin.TierPatch || rec.Usage.SearchUnits != 2 || rec.ChargeMicro != 4_000 {
		t.Errorf("tier %s usage %+v charge %d", rec.Attempts[0].Tier, rec.Usage, rec.ChargeMicro)
	}
	if ch := rec.Attempts[0].Charge; ch == nil || ch.Rates.Units[plugin.UnitSearch] != 2_000 {
		t.Errorf("charge detail %+v", ch)
	}
}

// A common-shape client served by TEI: converted, top_n and documents
// applied by the relay, usage estimated.
func TestRerankToTEI(t *testing.T) {
	up := newFake(t, jsonReply(200, `[{"index":2,"score":0.8},{"index":0,"score":0.3},{"index":1,"score":0.1}]`))
	e := newEnv(t, "rr", []upSpec{{id: "t", protocol: "tei-rerank", issuer: "t", fake: up}})
	resp, out := e.call("rerank", "/v1/rerank", `{"model":"rr","query":"q","documents":["a",{"text":"b","id":7},"c"],"top_n":2}`)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	sent := gjson.ParseBytes(up.body(0))
	if up.paths[0] != "/rerank" || sent.Get("texts.1").String() != "b" || sent.Get("query").String() != "q" || sent.Get("model").Exists() {
		t.Fatalf("upstream got %s at %s", up.body(0), up.paths[0])
	}
	if gjson.GetBytes(out, "results.#").Int() != 2 || gjson.GetBytes(out, "results.0.index").Int() != 2 ||
		gjson.GetBytes(out, "results.0.document.text").String() != "c" || gjson.GetBytes(out, "results.0.relevance_score").Float() != 0.8 {
		t.Errorf("client got %s", out)
	}
	rec := e.waitRecords(1)
	if rec.Attempts[0].Tier != plugin.TierConvert || !rec.Usage.Estimated || rec.Usage.Input == 0 {
		t.Errorf("tier %s usage %+v", rec.Attempts[0].Tier, rec.Usage)
	}
}

// A rerank request is never sent to a text or embeddings endpoint.
func TestRerankCapabilityMissing(t *testing.T) {
	up := newFake(t, jsonReply(200, `{}`))
	e := newEnv(t, "m", []upSpec{{id: "u", protocol: "openai-embeddings", issuer: "u", fake: up}})
	resp, out := e.call("rerank", "/v1/rerank", `{"model":"m","query":"q","documents":["a"]}`)
	if resp.StatusCode != 400 || up.calls() != 0 || !gjson.GetBytes(out, "error.message").Exists() {
		t.Fatalf("%d %s (calls %d)", resp.StatusCode, out, up.calls())
	}
	// A multimodal document only passes through to the same protocol.
	resp, out = e.call("rerank", "/v1/rerank", `{"model":"m","query":"q","documents":[{"image":"https://x/y.png"}]}`)
	if resp.StatusCode < 400 || up.calls() != 0 {
		t.Fatalf("image document: %d %s", resp.StatusCode, out)
	}
}
