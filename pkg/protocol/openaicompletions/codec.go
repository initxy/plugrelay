// Package openaicompletions implements the legacy OpenAI text completions
// wire protocol ("openai-completions"): POST /v1/completions on the client
// side and the same endpoint upstream. Editors use it for inline
// completion, often with a suffix (fill-in-the-middle).
//
// The protocol takes one text prompt and returns text, so it declares
// ir.Capabilities.PromptOnly: a conversation from another protocol is never
// routed to it. A request decodes to a single user text message; a suffix
// sets ir.Needs.FillInMiddle, which only endpoints declaring
// fill_in_middle can serve. Requests the IR cannot express faithfully —
// several prompts, token-id prompts, n or best_of above 1, echo — fail to
// decode, which limits them to same-protocol passthrough.
//
// Fields without an IR slot (logprobs, seed, logit_bias, user, ...) are kept
// in ir.Extra under their wire names and restored only for this protocol.
// Keys starting with "_" are this codec's round-trip markers and never
// reach the wire. Errors and model lists use the OpenAI shapes of package
// openaichat.
package openaicompletions

import (
	"encoding/json"
	"net/http"
	"unicode/utf8"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/protocol/openaichat"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Name is the protocol id of this codec.
const Name = "openai-completions"

// Codec is the text completions codec. The zero value is ready to use.
type Codec struct{}

func init() { protocol.Register(Codec{}) }

var _ protocol.Codec = Codec{}

// openai renders errors and model lists; both are shared with chat.
var openai = openaichat.Codec{}

// Name returns "openai-completions".
func (Codec) Name() string { return Name }

// Capabilities reports what text completions can express: a prompt, an
// optional suffix and sampling parameters.
func (Codec) Capabilities() ir.Capabilities {
	return ir.Capabilities{
		Cache:          ir.CacheImplicit,
		MaxTokensField: "max_tokens",
		FillInMiddle:   true,
		PromptOnly:     true,
	}
}

// Routes lists the completions endpoint.
func (Codec) Routes() []protocol.Route {
	return []protocol.Route{{Method: http.MethodPost, Path: "/v1/completions", Op: protocol.OpGenerate}}
}

// Sniff reads model, stream flag and session hints without a full decode.
func (Codec) Sniff(r *http.Request, route protocol.Route, body []byte) (protocol.Inbound, error) {
	in := protocol.Inbound{Op: route.Op}
	if r != nil {
		in.Session.SessionID = r.Header.Get("session-id")
		if in.Session.SessionID == "" {
			in.Session.SessionID = r.Header.Get("x-session-id")
		}
	}
	if route.Op != protocol.OpGenerate {
		return in, nil
	}
	if !gjson.ValidBytes(body) {
		return in, ir.Errorf(ir.ErrInvalidRequest, "request body is not valid JSON")
	}
	res := gjson.GetManyBytes(body, "model", "stream", "user")
	in.Model = res[0].String()
	in.Stream = res[1].Bool()
	in.Session.UserTag = res[2].String()
	return in, nil
}

// EncodeError renders a relay-generated error in the OpenAI error format.
func (Codec) EncodeError(e *ir.GwError) (int, http.Header, []byte) { return openai.EncodeError(e) }

// DecodeError reads an OpenAI-style upstream error.
func (Codec) DecodeError(status int, header http.Header, body []byte) *ir.GwError {
	return openai.DecodeError(status, header, body)
}

// EncodeModels renders the OpenAI list format.
func (Codec) EncodeModels(r *http.Request, models []ir.ModelInfo) (string, []byte) {
	return openai.EncodeModels(r, models)
}

// CountTokens answers with a local estimate as {"input_tokens":n}.
func (Codec) CountTokens(req *ir.Request) ([]byte, error) {
	return json.Marshal(map[string]int{"input_tokens": estimateTokens(promptText(req))})
}

// estimateTokens is a rough count of about four characters per token.
func estimateTokens(s string) int { return (utf8.RuneCountInString(s) + 3) / 4 }

// UpstreamPath returns "/completions"; upstream base URLs include /v1.
func (Codec) UpstreamPath(*ir.Request, protocol.Inbound) string { return "/completions" }

// SetModel rewrites the top-level model field, leaving every other byte intact.
func (Codec) SetModel(body []byte, model string) ([]byte, error) {
	return sjson.SetBytes(body, "model", model)
}

// StripOpaque is a no-op: completions carry no upstream-private state.
func (Codec) StripOpaque(body []byte) ([]byte, int, error) { return body, 0, nil }
