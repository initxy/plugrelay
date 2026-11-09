package plugintest

import (
	"net/http"
	"strconv"

	"github.com/initxy/plugrelay/pkg/ir"
)

// ErrorFixture is one scripted upstream error.
type ErrorFixture struct {
	Status int
	Header http.Header
	Body   string
	// Signal is what the protocol's codec reads from Body; the suite puts
	// it in Outcome.Signal as the core would.
	Signal ir.GwError
}

// Fixtures is the scripted traffic of one protocol family.
type Fixtures struct {
	// Path is the request path used when the endpoint has no Path
	// override; "{model}" is substituted. StreamPath is used for streams
	// (same as Path when empty); StreamQuery is appended to streams.
	Path, StreamPath string
	StreamQuery      string
	// Request is a minimal JSON body; "{model}" is substituted.
	Request string
	// Response is a complete non-streamed 200 body.
	Response string
	// Stream lists complete SSE frames ("event: x\ndata: y\n\n").
	Stream []string
	// Errors is keyed by scenario name.
	Errors map[string]ErrorFixture
}

// FixturesFor returns the built-in fixtures for a protocol (codec name).
func FixturesFor(protocol string) (Fixtures, bool) {
	switch protocol {
	case "openai-chat":
		return openAIChat, true
	case "openai-responses":
		return openAIResponses, true
	case "openai-completions":
		return openAICompletions, true
	case "anthropic":
		return anthropic, true
	case "gemini":
		return gemini, true
	case "openai-embeddings":
		return openAIEmbeddings, true
	case "openai-images":
		return openAIImages, true
	case "openai-audio":
		return openAIAudio, true
	case "rerank":
		return rerankFx, true
	case "cohere-rerank":
		f := rerankFx
		f.Path = "/v2/rerank"
		f.Response = `{"id":"r1","results":[{"index":0,"relevance_score":0.9}],"meta":{"billed_units":{"search_units":1}}}`
		return f, true
	case "tei-rerank":
		f := rerankFx
		f.Path, f.Request, f.Response = "/rerank", `{"query":"q","texts":["a"]}`, `[{"index":0,"score":0.9}]`
		return f, true
	case "gemini-embeddings":
		g := gemini
		g.Path, g.StreamPath, g.StreamQuery, g.Stream = "/v1beta/models/{model}:batchEmbedContents", "", "", nil
		g.Request = `{"requests":[{"model":"models/{model}","content":{"parts":[{"text":"hello"}]}}]}`
		g.Response = `{"embeddings":[{"values":[0.1,-0.2]}]}`
		return g, true
	}
	return Fixtures{}, false
}

// Media protocols have no event streams; their stream scenarios are skipped.
var (
	openAIEmbeddings = Fixtures{
		Path:     "/v1/embeddings",
		Request:  `{"model":"{model}","input":"hello"}`,
		Response: `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,-0.2]}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`,
		Errors:   openAIErrors(),
	}
	openAIImages = Fixtures{
		Path:     "/v1/images/generations",
		Request:  `{"model":"{model}","prompt":"a cat"}`,
		Response: `{"created":1,"data":[{"b64_json":"aGk="}]}`,
		Errors:   openAIErrors(),
	}
	rerankFx = Fixtures{
		Path:     "/v1/rerank",
		Request:  `{"model":"{model}","query":"q","documents":["a"]}`,
		Response: `{"model":"m","results":[{"index":0,"relevance_score":0.9}],"usage":{"total_tokens":3}}`,
		Errors:   openAIErrors(),
	}
	openAIAudio = Fixtures{
		Path:     "/v1/audio/speech",
		Request:  `{"model":"{model}","input":"hello","voice":"alloy"}`,
		Response: `{}`,
		Errors:   openAIErrors(),
	}
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func openAIErr(typ, code, msg string) string {
	return `{"error":{"message":"` + msg + `","type":"` + typ + `","param":null,"code":"` + code + `"}}`
}

func openAIErrors() map[string]ErrorFixture {
	return map[string]ErrorFixture{
		ScenarioRateLimit: {Status: 429, Header: hdr("Retry-After", "7", "X-Request-Id", "req_rl"),
			Body:   openAIErr("requests", "rate_limit_exceeded", "Rate limit reached for requests"),
			Signal: ir.GwError{Kind: ir.ErrRateLimited, Message: "Rate limit reached for requests", Code: "rate_limit_exceeded"}},
		ScenarioUnauthorized: {Status: 401, Header: hdr("X-Request-Id", "req_401"),
			Body:   openAIErr("invalid_request_error", "invalid_api_key", "Incorrect API key provided"),
			Signal: ir.GwError{Kind: ir.ErrAuthentication, Message: "Incorrect API key provided", Code: "invalid_api_key"}},
		ScenarioSuspended: {Status: 403, Header: hdr("X-Request-Id", "req_403"),
			Body:   openAIErr("invalid_request_error", "account_deactivated", "Your account has been deactivated. Please contact support."),
			Signal: ir.GwError{Kind: ir.ErrPermission, Message: "Your account has been deactivated. Please contact support.", Code: "account_deactivated"}},
		ScenarioContent: {Status: 400, Header: hdr("X-Request-Id", "req_cv"),
			Body:   openAIErr("invalid_request_error", "content_policy_violation", "Your request was rejected as a result of our safety system."),
			Signal: ir.GwError{Kind: ir.ErrContentFiltered, Message: "Your request was rejected as a result of our safety system.", Code: "content_policy_violation"}},
		ScenarioUncertain: {Status: 403, Header: hdr("X-Request-Id", "req_unc"),
			Body:   openAIErr("forbidden", "", "Request not allowed"),
			Signal: ir.GwError{Kind: ir.ErrPermission, Message: "Request not allowed"}},
		ScenarioErrorPassthrough: {Status: 400, Header: hdr("X-Request-Id", "req_bad"),
			Body:   openAIErr("invalid_request_error", "invalid_value", "Invalid value for 'temperature': expected a number between 0 and 2."),
			Signal: ir.GwError{Kind: ir.ErrInvalidRequest, Message: "Invalid value for 'temperature': expected a number between 0 and 2.", Code: "invalid_value"}},
		ScenarioServerError: {Status: 500, Header: hdr("X-Request-Id", "req_500"),
			Body:   openAIErr("server_error", "", "The server had an error while processing your request."),
			Signal: ir.GwError{Kind: ir.ErrUpstream, Message: "The server had an error while processing your request."}},
	}
}

var openAIChat = Fixtures{
	Path:     "/v1/chat/completions",
	Request:  `{"model":"{model}","messages":[{"role":"user","content":"hello"}]}`,
	Response: `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	Stream: []string{
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n",
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"}}]}\n\n",
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n",
		"data: [DONE]\n\n",
	},
	Errors: openAIErrors(),
}

var openAICompletions = Fixtures{
	Path:     "/v1/completions",
	Request:  `{"model":"{model}","prompt":"hello","max_tokens":8}`,
	Response: `{"id":"cmpl-1","object":"text_completion","created":1,"model":"m","choices":[{"text":"Hello","index":0,"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	Stream: []string{
		"data: {\"id\":\"cmpl-1\",\"object\":\"text_completion\",\"choices\":[{\"text\":\"Hel\",\"index\":0,\"logprobs\":null,\"finish_reason\":null}]}\n\n",
		"data: {\"id\":\"cmpl-1\",\"object\":\"text_completion\",\"choices\":[{\"text\":\"lo\",\"index\":0,\"logprobs\":null,\"finish_reason\":\"stop\"}]}\n\n",
		"data: {\"id\":\"cmpl-1\",\"object\":\"text_completion\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n",
		"data: [DONE]\n\n",
	},
	Errors: openAIErrors(),
}

var openAIResponses = Fixtures{
	Path:     "/v1/responses",
	Request:  `{"model":"{model}","input":"hello"}`,
	Response: `{"id":"resp_1","object":"response","status":"completed","model":"m","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
	Stream: []string{
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n",
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"Hel\"}\n\n",
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"lo\"}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n",
	},
	Errors: openAIErrors(),
}

func anthropicErr(typ, msg string) string {
	return `{"type":"error","error":{"type":"` + typ + `","message":"` + msg + `"},"request_id":"req_x"}`
}

var anthropic = Fixtures{
	Path:     "/v1/messages",
	Request:  `{"model":"{model}","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
	Response: `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
	Stream: []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	},
	Errors: map[string]ErrorFixture{
		ScenarioRateLimit: {Status: 429, Header: hdr("Retry-After", "7", "Request-Id", "req_rl", "Anthropic-Ratelimit-Requests-Remaining", "0"),
			Body:   anthropicErr("rate_limit_error", "Number of request tokens has exceeded your per-minute rate limit"),
			Signal: ir.GwError{Kind: ir.ErrRateLimited, Message: "Number of request tokens has exceeded your per-minute rate limit", Code: "rate_limit_error"}},
		ScenarioUnauthorized: {Status: 401, Header: hdr("Request-Id", "req_401"),
			Body:   anthropicErr("authentication_error", "invalid x-api-key"),
			Signal: ir.GwError{Kind: ir.ErrAuthentication, Message: "invalid x-api-key", Code: "authentication_error"}},
		ScenarioSuspended: {Status: 403, Header: hdr("Request-Id", "req_403"),
			Body:   anthropicErr("permission_error", "This organization has been disabled."),
			Signal: ir.GwError{Kind: ir.ErrPermission, Message: "This organization has been disabled.", Code: "permission_error"}},
		ScenarioContent: {Status: 400, Header: hdr("Request-Id", "req_cv"),
			Body:   anthropicErr("invalid_request_error", "Output blocked by content filtering policy"),
			Signal: ir.GwError{Kind: ir.ErrContentFiltered, Message: "Output blocked by content filtering policy", Code: "invalid_request_error"}},
		ScenarioUncertain: {Status: 403, Header: hdr("Request-Id", "req_unc"),
			Body:   anthropicErr("permission_error", "Request not allowed"),
			Signal: ir.GwError{Kind: ir.ErrPermission, Message: "Request not allowed", Code: "permission_error"}},
		ScenarioErrorPassthrough: {Status: 400, Header: hdr("Request-Id", "req_bad"),
			Body:   anthropicErr("invalid_request_error", "max_tokens: Field required"),
			Signal: ir.GwError{Kind: ir.ErrInvalidRequest, Message: "max_tokens: Field required", Code: "invalid_request_error"}},
		ScenarioServerError: {Status: 529, Header: hdr("Request-Id", "req_529"),
			Body:   anthropicErr("overloaded_error", "Overloaded"),
			Signal: ir.GwError{Kind: ir.ErrOverloaded, Message: "Overloaded", Code: "overloaded_error"}},
	},
}

func geminiErr(code int, status, msg string) string {
	return `{"error":{"code":` + strconv.Itoa(code) + `,"message":"` + msg + `","status":"` + status + `"}}`
}

var gemini = Fixtures{
	Path:        "/v1beta/models/{model}:generateContent",
	StreamPath:  "/v1beta/models/{model}:streamGenerateContent",
	StreamQuery: "alt=sse",
	Request:     `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
	Response:    `{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`,
	Stream: []string{
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hel\"}]}}]}\n\n",
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"lo\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n",
	},
	Errors: map[string]ErrorFixture{
		ScenarioRateLimit: {Status: 429, Header: hdr("Retry-After", "7"),
			Body:   geminiErr(429, "RESOURCE_EXHAUSTED", "Resource has been exhausted (e.g. check quota)."),
			Signal: ir.GwError{Kind: ir.ErrRateLimited, Message: "Resource has been exhausted (e.g. check quota).", Code: "RESOURCE_EXHAUSTED"}},
		ScenarioUnauthorized: {Status: 401,
			Body:   geminiErr(401, "UNAUTHENTICATED", "API key not valid. Please pass a valid API key."),
			Signal: ir.GwError{Kind: ir.ErrAuthentication, Message: "API key not valid. Please pass a valid API key.", Code: "UNAUTHENTICATED"}},
		ScenarioSuspended: {Status: 403,
			Body:   geminiErr(403, "PERMISSION_DENIED", "Permission denied: Consumer has been suspended."),
			Signal: ir.GwError{Kind: ir.ErrPermission, Message: "Permission denied: Consumer has been suspended.", Code: "PERMISSION_DENIED"}},
		ScenarioContent: {Status: 400,
			Body:   geminiErr(400, "INVALID_ARGUMENT", "The prompt was blocked due to safety reasons."),
			Signal: ir.GwError{Kind: ir.ErrContentFiltered, Message: "The prompt was blocked due to safety reasons.", Code: "INVALID_ARGUMENT"}},
		ScenarioUncertain: {Status: 403,
			Body:   geminiErr(403, "PERMISSION_DENIED", "The caller does not have permission"),
			Signal: ir.GwError{Kind: ir.ErrPermission, Message: "The caller does not have permission", Code: "PERMISSION_DENIED"}},
		ScenarioErrorPassthrough: {Status: 400, Header: hdr("X-Request-Id", "req_bad"),
			Body:   geminiErr(400, "INVALID_ARGUMENT", "Invalid value at 'generation_config.temperature'"),
			Signal: ir.GwError{Kind: ir.ErrInvalidRequest, Message: "Invalid value at 'generation_config.temperature'", Code: "INVALID_ARGUMENT"}},
		ScenarioServerError: {Status: 503,
			Body:   geminiErr(503, "UNAVAILABLE", "The model is overloaded. Please try again later."),
			Signal: ir.GwError{Kind: ir.ErrOverloaded, Message: "The model is overloaded. Please try again later.", Code: "UNAVAILABLE"}},
	},
}
