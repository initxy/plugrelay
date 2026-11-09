// Package openaichat implements the OpenAI Chat Completions wire protocol
// ("openai-chat") for both the client side (POST /v1/chat/completions,
// GET /v1/models) and the upstream side.
//
// The protocol has no field a client echoes back verbatim, so upstream-private
// state (thinking signatures, encrypted reasoning) produced by another
// protocol cannot be sealed into a chat response and is lost; Capabilities
// therefore declares no EchoFields. Visible reasoning travels as
// reasoning_content and is kept. reasoning_details arrays are carried
// verbatim between chat endpoints only.
//
// Fields the IR has no slot for are kept in ir.Extra under their wire names.
// Keys starting with "_" in that Extra are this codec's own round-trip
// markers (which role a system block came from, whether content was a string
// or a part array, ...) and are never written to the wire.
package openaichat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"unicode/utf8"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Name is the protocol id of this codec.
const Name = "openai-chat"

// Codec is the OpenAI Chat Completions codec. The zero value is ready to use.
type Codec struct{}

func init() { protocol.Register(Codec{}) }

var _ protocol.Codec = Codec{}

// Name returns "openai-chat".
func (Codec) Name() string { return Name }

// Capabilities reports what chat completions can express.
func (Codec) Capabilities() ir.Capabilities {
	return ir.Capabilities{
		Cache:          ir.CacheKeyed,
		FunctionTools:  true,
		FreeformTools:  true,
		Reasoning:      true,
		ReasoningStyle: "effort",
		Images:         true,
		Documents:      true,
		MaxTokensField: "max_completion_tokens",
	}
}

// Routes lists the chat completions and model list endpoints.
func (Codec) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/v1/chat/completions", Op: protocol.OpGenerate},
		{Method: http.MethodGet, Path: "/v1/models", Op: protocol.OpModels},
	}
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
	res := gjson.GetManyBytes(body, "model", "stream", "prompt_cache_key", "user", "safety_identifier")
	in.Model = res[0].String()
	in.Stream = res[1].Bool()
	in.Session.CacheKey = res[2].String()
	in.Session.UserTag = res[3].String()
	if in.Session.UserTag == "" {
		in.Session.UserTag = res[4].String()
	}
	in.Session.PrefixHash = prefixHash(body)
	return in, nil
}

// prefixHash hashes the system/developer messages and the first user message.
// Text parts contribute only their text so string and part-array forms of the
// same prompt hash alike; other parts contribute their JSON minus cache_control.
func prefixHash(body []byte) string {
	h := sha256.New()
	seen := false
	gjson.GetBytes(body, "messages").ForEach(func(_, m gjson.Result) bool {
		role := m.Get("role").String()
		switch role {
		case "system", "developer", "user":
		default:
			return true
		}
		h.Write([]byte(role))
		h.Write([]byte{0})
		c := m.Get("content")
		if c.IsArray() {
			c.ForEach(func(_, p gjson.Result) bool {
				if p.Get("type").String() == "text" {
					h.Write([]byte(p.Get("text").String()))
				} else {
					raw, _ := sjson.Delete(p.Raw, "cache_control")
					h.Write([]byte(raw))
				}
				h.Write([]byte{0})
				return true
			})
		} else {
			h.Write([]byte(c.String()))
			h.Write([]byte{0})
		}
		h.Write([]byte{1})
		seen = true
		return role != "user"
	})
	if !seen {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// EncodeModels renders the OpenAI list format.
func (Codec) EncodeModels(_ *http.Request, models []ir.ModelInfo) (string, []byte) {
	type entry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	list := struct {
		Object string  `json:"object"`
		Data   []entry `json:"data"`
	}{Object: "list", Data: make([]entry, 0, len(models))}
	for _, m := range models {
		owner := m.OwnedBy
		if owner == "" {
			owner = "system"
		}
		list.Data = append(list.Data, entry{ID: m.ID, Object: "model", Created: m.Created, OwnedBy: owner})
	}
	b, _ := json.Marshal(list)
	return "application/json", b
}

// CountTokens answers with a local estimate as {"input_tokens":n}.
func (Codec) CountTokens(req *ir.Request) ([]byte, error) {
	return json.Marshal(map[string]int{"input_tokens": estimateTokens(req)})
}

// estimateTokens is a rough count: about four characters per token, plus a
// flat cost per image or document.
func estimateTokens(req *ir.Request) int {
	chars, media := 0, 0
	var walk func([]ir.Block)
	walk = func(bs []ir.Block) {
		for _, b := range bs {
			chars += utf8.RuneCountInString(b.Text)
			if b.Media != nil {
				media++
			}
			if b.Tool != nil {
				chars += len(b.Tool.Name) + len(b.Tool.Input) + utf8.RuneCountInString(b.Tool.Text)
			}
			if b.Result != nil {
				walk(b.Result.Content)
			}
		}
	}
	walk(req.System)
	for _, m := range req.Messages {
		walk(m.Content)
		chars += 4
	}
	for _, t := range req.Tools {
		chars += len(t.Name) + len(t.Description) + len(t.InputSchema) + len(t.Grammar)
	}
	return (chars+3)/4 + media*765
}

// UpstreamPath returns "/chat/completions"; upstream base URLs include /v1.
func (Codec) UpstreamPath(*ir.Request, protocol.Inbound) string { return "/chat/completions" }

// SetModel rewrites the top-level model field, leaving every other byte intact.
func (Codec) SetModel(body []byte, model string) ([]byte, error) {
	return sjson.SetBytes(body, "model", model)
}

// StripOpaque removes reasoning_details from assistant messages. Visible
// reasoning_content is model output the client saw and stays.
func (Codec) StripOpaque(body []byte) ([]byte, int, error) {
	var paths []string
	gjson.GetBytes(body, "messages").ForEach(func(k, m gjson.Result) bool {
		if m.Get("role").String() == "assistant" && m.Get("reasoning_details").Exists() {
			paths = append(paths, "messages."+k.String()+".reasoning_details")
		}
		return true
	})
	out := body
	for _, p := range paths {
		var err error
		if out, err = sjson.DeleteBytes(out, p); err != nil {
			return body, 0, err
		}
	}
	return out, len(paths), nil
}
