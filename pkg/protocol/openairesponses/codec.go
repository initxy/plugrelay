// Package openairesponses implements the OpenAI Responses wire protocol
// ("openai-responses") for both sides of the relay: decoding client requests
// to IR and rendering responses, streams, errors and model lists back, and
// encoding IR for responses-API upstreams and decoding what they return.
//
// Item types the IR cannot model (tool_search_call, web search and shell
// calls, item references, new types) travel as raw blocks holding the item
// verbatim, so a same-protocol round trip keeps them. Namespace tools are
// flattened into their member tools tagged with ir.Tool.Namespace and
// regrouped on encoding. Encrypted
// reasoning and compaction items become opaque state; values carrying the
// relay seal prefix are flagged Sealed for the pipeline to unseal.
package openairesponses

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Name is the protocol id.
const Name = "openai-responses"

// TurnStateHeader is the response header whose value clients echo back on
// later requests of the same turn.
const TurnStateHeader = "x-codex-turn-state"

// serverToolTypes are the provider-executed tool types the protocol can
// carry; any other non-function, non-custom tool travels verbatim too.
var serverToolTypes = []string{
	"web_search", "web_search_preview", "file_search", "code_interpreter",
	"image_generation", "computer_use_preview", "mcp", "local_shell",
	"tool_search", "namespace",
}

// Codec is the OpenAI Responses codec. The zero value is ready to use.
type Codec struct{}

func init() { protocol.Register(Codec{}) }

var (
	_ protocol.Codec        = Codec{}
	_ protocol.StickyHeader = Codec{}
)

// Name returns "openai-responses".
func (Codec) Name() string { return Name }

// StickyHeaderName names the same-turn sticky token header.
func (Codec) StickyHeaderName() string { return TurnStateHeader }

// Capabilities is everything the protocol can express.
func (Codec) Capabilities() ir.Capabilities {
	return ir.Capabilities{
		Cache:          ir.CacheKeyed,
		FunctionTools:  true,
		FreeformTools:  true,
		ServerTools:    append([]string(nil), serverToolTypes...),
		Namespaces:     true,
		DeferredTools:  true,
		Reasoning:      true,
		ReasoningStyle: "effort",
		Images:         true,
		Documents:      true,
		ToolResultImgs: true,
		EchoFields:     []string{kindEncrypted},
		ServerState:    true,
		Compaction:     true,
		MaxTokensField: "max_output_tokens",
	}
}

// Routes lists the client-facing endpoints. The compact route carries
// Action "compact" through Sniff so UpstreamPath can target it.
func (Codec) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/v1/responses", Op: protocol.OpGenerate},
		{Method: http.MethodPost, Path: "/v1/responses/compact", Op: protocol.OpGenerate},
		{Method: http.MethodPost, Path: "/v1/responses/input_tokens", Op: protocol.OpCountTokens},
		{Method: http.MethodGet, Path: "/v1/models", Op: protocol.OpModels},
	}
}

// UpgradeRequired is the answer to a WebSocket upgrade on /v1/responses:
// 426 tells clients to fall back to HTTP streaming.
func UpgradeRequired() (status int, body []byte) {
	return http.StatusUpgradeRequired, errorBody(errInfo{
		typ:     "invalid_request_error",
		code:    "websocket_not_supported",
		message: "WebSocket transport is not supported; use HTTP streaming.",
	}, nil)
}

// Sniff reads the routing facts from a raw request cheaply.
func (Codec) Sniff(r *http.Request, route protocol.Route, body []byte) (protocol.Inbound, error) {
	in := protocol.Inbound{Op: route.Op}
	if strings.HasSuffix(route.Path, "/compact") {
		in.Action = "compact"
	}
	if r != nil {
		h := r.Header
		in.Session.SessionID = firstHeader(h, "session-id", "session_id")
		if in.Session.SessionID == "" {
			in.Session.SessionID = firstHeader(h, "thread-id", "thread_id")
		}
		in.Session.AgentID = h.Get("x-openai-subagent")
		in.Session.TurnState = h.Get(TurnStateHeader)
	}
	if route.Op == protocol.OpModels || len(body) == 0 {
		return in, nil
	}
	if !gjson.ValidBytes(body) {
		return in, ir.Errorf(ir.ErrInvalidRequest, "request body is not valid JSON")
	}
	res := gjson.GetManyBytes(body, "model", "stream", "prompt_cache_key", "user", "safety_identifier")
	in.Model = res[0].Str
	in.Stream = res[1].Bool() && route.Op == protocol.OpGenerate
	in.Session.CacheKey = res[2].Str
	in.Session.UserTag = res[3].Str
	if in.Session.UserTag == "" {
		in.Session.UserTag = res[4].Str
	}
	in.Session.PrefixHash = prefixHash(body)
	return in, nil
}

func firstHeader(h http.Header, names ...string) string {
	for _, n := range names {
		if v := h.Get(n); v != "" {
			return v
		}
	}
	return ""
}

// prefixHash hashes instructions, the leading developer/system items and the
// first user message. Text parts that start with an environment-context tag
// change every session and are left out.
func prefixHash(body []byte) string {
	h := sha256.New()
	n := 0
	write := func(s string) {
		if s == "" || strings.HasPrefix(strings.TrimSpace(s), "<environment_context>") {
			return
		}
		h.Write([]byte(s))
		h.Write([]byte{0})
		n++
	}
	write(gjson.GetBytes(body, "instructions").Str)
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		write(input.Str)
	}
	for _, it := range input.Array() {
		system := isSystemMessage(it)
		user := !system && (it.Get("type").Str == itemMessage || it.Get("type").Str == "") && it.Get("role").Str == "user"
		if !system && !user {
			break
		}
		c := it.Get("content")
		if c.Type == gjson.String {
			write(c.Str)
		}
		for _, p := range c.Array() {
			write(p.Get("text").Str)
		}
		if user {
			break
		}
	}
	if n == 0 {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// UpstreamPath is the path to call on an upstream base URL.
func (Codec) UpstreamPath(req *ir.Request, in protocol.Inbound) string {
	switch {
	case in.Op == protocol.OpCountTokens:
		return "/responses/input_tokens"
	case in.Action == "compact":
		return "/responses/compact"
	}
	return "/responses"
}

// SetModel rewrites the model name in place.
func (Codec) SetModel(body []byte, model string) ([]byte, error) {
	return sjson.SetBytes(body, "model", model)
}

// StripOpaque removes reasoning and compaction items from the input array,
// leaving every other byte untouched.
func (Codec) StripOpaque(body []byte) ([]byte, int, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, 0, nil
	}
	var drop []int
	i := 0
	input.ForEach(func(_, v gjson.Result) bool {
		switch v.Get("type").Str {
		case itemReasoning, itemCompaction:
			drop = append(drop, i)
		}
		i++
		return true
	})
	out := body
	for k := len(drop) - 1; k >= 0; k-- {
		var err error
		if out, err = sjson.DeleteBytes(out, "input."+strconv.Itoa(drop[k])); err != nil {
			return body, 0, err
		}
	}
	return out, len(drop), nil
}

// CountTokens answers a token-count request with a local estimate.
func (Codec) CountTokens(req *ir.Request) ([]byte, error) {
	o := newObject()
	o.str("object", "response.input_tokens")
	o.num("input_tokens", int64(estimateTokens(req)))
	return o.bytes(), nil
}

// estimateTokens is a rough size: four bytes of text per token, a fixed
// cost per image.
func estimateTokens(req *ir.Request) int {
	if req == nil {
		return 0
	}
	chars, images := 0, 0
	var walk func(bs []ir.Block)
	walk = func(bs []ir.Block) {
		for _, b := range bs {
			chars += len(b.Text)
			switch b.Type {
			case ir.BlockImage:
				images++
			case ir.BlockDocument:
				if b.Media != nil {
					chars += len(b.Media.Data) / 2
				}
			case ir.BlockRaw:
				if raw, ok := xget(&b.Extra, xRaw); ok {
					chars += len(raw)
				}
			}
			if b.Tool != nil {
				chars += len(b.Tool.Name) + len(b.Tool.Input) + len(b.Tool.Text)
			}
			if b.Result != nil {
				walk(b.Result.Content)
			}
		}
	}
	walk(req.System)
	for _, m := range req.Messages {
		chars += 16
		walk(m.Content)
	}
	for _, t := range req.Tools {
		chars += len(t.Name) + len(t.Description) + len(t.InputSchema) + len(t.Grammar)
		if raw, ok := xget(&t.Extra, xToolRaw); ok {
			chars += len(raw)
		}
	}
	return chars/4 + images*800
}
