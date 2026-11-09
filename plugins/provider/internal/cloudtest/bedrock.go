package cloudtest

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin/plugintest"
	"github.com/initxy/plugrelay/plugins/provider/internal/eventstream"
)

// BedrockEvents are the Anthropic stream events of the Bedrock fixtures,
// in order; the last one carries Bedrock's invocation metrics.
var BedrockEvents = []string{
	`{"type":"message_start","message":{"id":"msg_bdrk_1","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":3,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
	`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":3,"outputTokenCount":2,"invocationLatency":120,"firstByteLatency":80}}`,
}

// BedrockChunk frames one Anthropic event the way InvokeModelWithResponseStream
// does: a "chunk" event whose JSON payload holds the event base64-encoded.
func BedrockChunk(event string) string {
	payload, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(event))})
	b, err := eventstream.Encode([]eventstream.Header{
		{Name: ":event-type", Value: "chunk"},
		{Name: ":content-type", Value: "application/json"},
		{Name: ":message-type", Value: "event"},
	}, payload)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// BedrockException frames a modeled exception inside a stream.
func BedrockException(typ, message string) string {
	payload, _ := json.Marshal(map[string]string{"message": message})
	b, err := eventstream.Encode([]eventstream.Header{
		{Name: ":exception-type", Value: typ},
		{Name: ":content-type", Value: "application/json"},
		{Name: ":message-type", Value: "exception"},
	}, payload)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func bedrockErr(status int, typ, msg string, kv ...string) plugintest.ErrorFixture {
	h := http.Header{}
	h.Set("X-Amzn-Requestid", "req-"+typ)
	if typ != "" {
		h.Set("X-Amzn-Errortype", typ+":http://internal.amazon.com/coral/com.amazon.bedrock/")
	}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	body, _ := json.Marshal(map[string]string{"message": msg})
	return plugintest.ErrorFixture{Status: status, Header: h, Body: string(body),
		Signal: ir.GwError{Kind: ir.ErrUpstream, Message: msg, Code: typ}}
}

// BedrockFixtures is the plugintest traffic of Bedrock Runtime. The
// request is the Messages body the core builds; Prepare turns it into an
// InvokeModel call.
func BedrockFixtures() plugintest.Fixtures {
	stream := make([]string, len(BedrockEvents))
	for i, ev := range BedrockEvents {
		stream[i] = BedrockChunk(ev)
	}
	return plugintest.Fixtures{
		Path:     "/v1/messages",
		Request:  `{"model":"{model}","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
		Response: `{"id":"msg_bdrk_1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":2}}`,
		Stream:   stream,
		Errors: map[string]plugintest.ErrorFixture{
			plugintest.ScenarioRateLimit:        bedrockErr(429, "ThrottlingException", "Too many requests, please wait before trying again.", "Retry-After", "7"),
			plugintest.ScenarioUnauthorized:     bedrockErr(403, "UnrecognizedClientException", "The security token included in the request is invalid."),
			plugintest.ScenarioSuspended:        bedrockErr(403, "AccessDeniedException", "Your AWS account has been suspended."),
			plugintest.ScenarioContent:          bedrockErr(400, "ValidationException", "Output blocked by content filtering policy"),
			plugintest.ScenarioUncertain:        bedrockErr(403, "", "Request not allowed"),
			plugintest.ScenarioErrorPassthrough: bedrockErr(400, "ValidationException", "Malformed input request: #: required key [max_tokens] not found, please reformat your input and try again."),
			plugintest.ScenarioServerError:      bedrockErr(500, "InternalServerException", "The server encountered an error processing your request."),
		},
	}
}
