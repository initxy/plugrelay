package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	_ "github.com/initxy/plugrelay/pkg/protocol/anthropic"
	_ "github.com/initxy/plugrelay/pkg/protocol/gemini"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaichat"
	_ "github.com/initxy/plugrelay/pkg/protocol/openaicompletions"
	_ "github.com/initxy/plugrelay/pkg/protocol/openairesponses"
	"github.com/initxy/plugrelay/pkg/sse"
)

// canonical is one call's usage: 100 uncached input, 40 read from cache, 20
// and 10 written to the 5-minute and 1-hour caches, 50 output of which 15
// are reasoning. Every codec below reports exactly this call.
var canonical = ir.Usage{Input: 100, CacheRead: 40, CacheWrite5m: 20, CacheWrite1h: 10, Output: 50, Reasoning: 15}

const (
	totalInput = 170
	totalCache = 30
)

type usageCase struct {
	codec string
	// response and stream are the upstream's wire form of canonical.
	response string
	stream   []sse.Event
	// want is canonical as far as the protocol can express it; decoding
	// the upstream payloads and decoding the codec's own client encoding
	// of canonical must both yield it.
	want ir.Usage
	// client are gjson paths into EncodeResponse(canonical) and their values.
	client map[string]int64
}

func ev(name, data string) sse.Event { return sse.Event{Name: name, Data: []byte(data)} }

var usageCases = []usageCase{
	{
		codec: "anthropic",
		response: `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":100,"cache_read_input_tokens":40,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10},"output_tokens":50}}`,
		stream: []sse.Event{
			ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],
				"usage":{"input_tokens":100,"cache_read_input_tokens":40,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":10},"output_tokens":1}}}`),
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
			ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":50}}`),
			ev("message_stop", `{"type":"message_stop"}`),
		},
		// The wire has no reasoning count; input_tokens excludes cache.
		want: ir.Usage{Input: 100, CacheRead: 40, CacheWrite5m: 20, CacheWrite1h: 10, Output: 50},
		client: map[string]int64{
			"usage.input_tokens": 100, "usage.cache_read_input_tokens": 40, "usage.cache_creation_input_tokens": totalCache,
			"usage.cache_creation.ephemeral_5m_input_tokens": 20, "usage.cache_creation.ephemeral_1h_input_tokens": 10,
			"usage.output_tokens": 50,
		},
	},
	{
		codec: "openai-chat",
		response: `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":170,"completion_tokens":50,"total_tokens":220,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":30},"completion_tokens_details":{"reasoning_tokens":15}}}`,
		stream: []sse.Event{
			ev("", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`),
			ev("", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
			ev("", `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],
				"usage":{"prompt_tokens":170,"completion_tokens":50,"total_tokens":220,"prompt_tokens_details":{"cached_tokens":40,"cache_write_tokens":30},"completion_tokens_details":{"reasoning_tokens":15}}}`),
			ev("", `[DONE]`),
		},
		// One cache-write count: both cache lifetimes land in the 5-minute bucket.
		want: ir.Usage{Input: 100, CacheRead: 40, CacheWrite5m: totalCache, Output: 50, Reasoning: 15},
		client: map[string]int64{
			"usage.prompt_tokens": totalInput, "usage.prompt_tokens_details.cached_tokens": 40,
			"usage.prompt_tokens_details.cache_write_tokens": totalCache,
			"usage.completion_tokens":                        50, "usage.completion_tokens_details.reasoning_tokens": 15, "usage.total_tokens": 220,
		},
	},
	{
		codec: "openai-completions",
		response: `{"id":"cmpl-1","object":"text_completion","created":1,"model":"m","choices":[{"text":"hi","index":0,"logprobs":null,"finish_reason":"stop"}],
			"usage":{"prompt_tokens":170,"completion_tokens":50,"total_tokens":220,"prompt_tokens_details":{"cached_tokens":40}}}`,
		stream: []sse.Event{
			ev("", `{"id":"cmpl-1","object":"text_completion","created":1,"model":"m","choices":[{"text":"hi","index":0,"logprobs":null,"finish_reason":null}]}`),
			ev("", `{"id":"cmpl-1","object":"text_completion","created":1,"model":"m","choices":[{"text":"","index":0,"logprobs":null,"finish_reason":"stop"}]}`),
			ev("", `{"id":"cmpl-1","object":"text_completion","created":1,"model":"m","choices":[],
				"usage":{"prompt_tokens":170,"completion_tokens":50,"total_tokens":220,"prompt_tokens_details":{"cached_tokens":40}}}`),
			ev("", `[DONE]`),
		},
		// No cache-write or reasoning count: cache writes are uncached input.
		want: ir.Usage{Input: 130, CacheRead: 40, Output: 50},
		client: map[string]int64{
			"usage.prompt_tokens": totalInput, "usage.prompt_tokens_details.cached_tokens": 40,
			"usage.completion_tokens": 50, "usage.total_tokens": 220,
		},
	},
	{
		codec: "openai-responses",
		response: `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m",
			"output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","annotations":[],"text":"hi"}]}],
			"usage":{"input_tokens":170,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":30},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":15},"total_tokens":220}}`,
		stream: []sse.Event{
			ev("response.created", `{"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"m","output":[]}}`),
			ev("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","status":"in_progress","role":"assistant","content":[]}}`),
			ev("response.content_part.added", `{"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"msg_1","part":{"type":"output_text","annotations":[],"text":""}}`),
			ev("response.output_text.delta", `{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_1","delta":"hi"}`),
			ev("response.completed", `{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"m",
				"usage":{"input_tokens":170,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":30},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":15},"total_tokens":220}}}`),
		},
		want: ir.Usage{Input: 100, CacheRead: 40, CacheWrite5m: totalCache, Output: 50, Reasoning: 15},
		client: map[string]int64{
			"usage.input_tokens": totalInput, "usage.input_tokens_details.cached_tokens": 40,
			"usage.input_tokens_details.cache_write_tokens": totalCache,
			"usage.output_tokens":                           50, "usage.output_tokens_details.reasoning_tokens": 15, "usage.total_tokens": 220,
		},
	},
	{
		codec: "gemini",
		response: `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],"modelVersion":"m","responseId":"r1",
			"usageMetadata":{"promptTokenCount":170,"cachedContentTokenCount":40,"candidatesTokenCount":35,"thoughtsTokenCount":15,"totalTokenCount":220}}`,
		stream: []sse.Event{
			ev("", `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"index":0}],"modelVersion":"m","responseId":"r1"}`),
			ev("", `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP","index":0}],"modelVersion":"m","responseId":"r1",
				"usageMetadata":{"promptTokenCount":170,"cachedContentTokenCount":40,"candidatesTokenCount":35,"thoughtsTokenCount":15,"totalTokenCount":220}}`),
		},
		// No cache-write count: written tokens are ordinary prompt tokens.
		want: ir.Usage{Input: 100 + totalCache, CacheRead: 40, Output: 50, Reasoning: 15},
		client: map[string]int64{
			"usageMetadata.promptTokenCount": totalInput, "usageMetadata.cachedContentTokenCount": 40,
			"usageMetadata.candidatesTokenCount": 35, "usageMetadata.thoughtsTokenCount": 15, "usageMetadata.totalTokenCount": 220,
		},
	},
}

// bucketSet is ir.Usage without Raw, so it compares with ==.
type bucketSet struct{ Input, CacheRead, CacheWrite5m, CacheWrite1h, Output, Reasoning int64 }

func buckets(u ir.Usage) bucketSet {
	return bucketSet{u.Input, u.CacheRead, u.CacheWrite5m, u.CacheWrite1h, u.Output, u.Reasoning}
}

// TestUsageBucketsAcrossCodecs pins hard rule 12: every codec maps its
// upstream usage into the same non-overlapping buckets, so TotalInput (input
// + cache read + cache writes), cache reads and output agree whichever
// protocol reported the call, and each codec renders the buckets back in its
// client's convention.
func TestUsageBucketsAcrossCodecs(t *testing.T) {
	if n := len(protocol.All()); n != len(usageCases) {
		t.Fatalf("%d codecs registered, %d covered", n, len(usageCases))
	}
	for _, tc := range usageCases {
		t.Run(tc.codec, func(t *testing.T) {
			c, ok := protocol.Get(tc.codec)
			if !ok {
				t.Fatalf("codec %s not registered", tc.codec)
			}
			if tc.want.TotalInput() != totalInput || tc.want.CacheRead != canonical.CacheRead || tc.want.Output != canonical.Output {
				t.Fatalf("want %+v disagrees with canonical on total input, cache read or output", tc.want)
			}

			resp, err := c.DecodeResponse([]byte(tc.response))
			if err != nil {
				t.Fatal(err)
			}
			if got := buckets(resp.Usage); got != buckets(tc.want) {
				t.Errorf("DecodeResponse usage = %+v, want %+v", got, tc.want)
			}
			if len(resp.Usage.Raw) == 0 || !json.Valid(resp.Usage.Raw) {
				t.Errorf("DecodeResponse dropped raw usage: %q", resp.Usage.Raw)
			}

			d := c.NewStreamDecoder()
			var last *ir.Usage
			for _, e := range tc.stream {
				evs, err := d.Feed(e)
				if err != nil {
					t.Fatal(err)
				}
				for _, x := range evs {
					if x.Usage != nil {
						u := *x.Usage
						last = &u
					}
				}
			}
			if _, complete := d.Finish(); !complete {
				t.Error("stream not complete")
			}
			if got := buckets(d.Usage()); got != buckets(tc.want) {
				t.Errorf("stream decoder usage = %+v, want %+v", got, tc.want)
			}
			if last == nil || buckets(*last) != buckets(tc.want) {
				t.Errorf("last usage event = %+v, want %+v", last, tc.want)
			}

			out, err := c.EncodeResponse(&ir.Request{Model: "m"}, &ir.Response{
				ID: "x", Model: "m", Content: []ir.Block{{Type: ir.BlockText, Text: "hi"}},
				StopReason: ir.StopEndTurn, Usage: canonical,
			})
			if err != nil {
				t.Fatal(err)
			}
			for path, n := range tc.client {
				if v := gjson.GetBytes(out, path); !v.Exists() || v.Int() != n {
					t.Errorf("client %s = %s, want %d", path, v.Raw, n)
				}
			}
			back, err := c.DecodeResponse(out)
			if err != nil {
				t.Fatal(err)
			}
			if got := buckets(back.Usage); got != buckets(tc.want) {
				t.Errorf("round trip usage = %+v, want %+v (client body %s)", got, tc.want, out)
			}
		})
	}
}
