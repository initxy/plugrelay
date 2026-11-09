// Package gemini implements the Gemini API wire protocol (generateContent,
// streamGenerateContent with alt=sse, countTokens and the model list) as a
// protocol.Codec registered under the name "gemini".
//
// Tool call ids: Gemini function calls and responses may omit "id" and pair
// by name and order instead. When decoding, a missing id is synthesized as
// "call_<name>_<n>" (n counts calls across the request, so ids are
// deterministic and unique); a functionResponse without id answers the
// oldest unanswered call of the same name. Ids synthesized while decoding a
// response carry a per-response salt ("call_<name>_<salt>_<n>") so they stay
// unique across turns. Encoders omit ids of that shape, so a round trip
// through this protocol does not invent ids, and take a tool result's
// function name from the matching tool_use in history.
//
// Thought signatures: a part's thoughtSignature becomes the Opaque state of
// the block decoded from that part (Kind "thought_signature"). A part that
// carries only a signature becomes a BlockOpaque, and opaque state is encoded
// back to clients as {"text":"","thoughtSignature":...}: the same shape the
// upstream API itself emits at the end of a stream, so clients that keep
// real responses in history echo it back.
package gemini

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/sse"
)

// Name is the protocol id of this codec.
const Name = "gemini"

// Path actions this codec serves.
const (
	ActionGenerate    = "generateContent"
	ActionStream      = "streamGenerateContent"
	ActionCountTokens = "countTokens"
)

// opaqueKind is the OpaqueState.Kind for a part's thoughtSignature.
const opaqueKind = "thought_signature"

// Server tool types, keyed by their tools[] entry name.
var serverToolTypes = map[string]string{
	"googleSearch":          "google_search",
	"googleSearchRetrieval": "google_search_retrieval",
	"urlContext":            "url_context",
	"codeExecution":         "code_execution",
}

func init() { protocol.Register(Codec{}) }

// Codec is the Gemini API codec. The zero value is ready to use.
type Codec struct{}

var _ protocol.Codec = Codec{}

// Name implements protocol.Codec.
func (Codec) Name() string { return Name }

// Capabilities implements protocol.Codec.
func (Codec) Capabilities() ir.Capabilities {
	return ir.Capabilities{
		Cache:          ir.CacheImplicit,
		FunctionTools:  true,
		ServerTools:    []string{"google_search", "url_context", "code_execution"},
		Reasoning:      true,
		ReasoningStyle: "budget",
		Images:         true,
		Documents:      true,
		ToolResultImgs: true,
		EchoFields:     []string{opaqueKind},
		MaxTokensField: "maxOutputTokens",
	}
}

// Routes implements protocol.Codec. The one POST route serves all three
// actions; Sniff sets the Op from the action.
func (Codec) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/v1beta/models/{model}:{action}", Op: protocol.OpGenerate},
		{Method: http.MethodGet, Path: "/v1beta/models", Op: protocol.OpModels},
	}
}

// Sniff implements protocol.Codec.
func (Codec) Sniff(r *http.Request, route protocol.Route, body []byte) (protocol.Inbound, error) {
	if route.Op == protocol.OpModels {
		return protocol.Inbound{Op: protocol.OpModels}, nil
	}
	model, action, ok := ParsePath(r.URL.Path)
	if !ok {
		return protocol.Inbound{}, ir.Errorf(ir.ErrNotFound, "unrecognized Gemini path %q", r.URL.Path)
	}
	in := protocol.Inbound{Model: model, Action: action}
	switch action {
	case ActionGenerate:
		in.Op = protocol.OpGenerate
	case ActionStream:
		in.Op, in.Stream = protocol.OpGenerate, true
	case ActionCountTokens:
		in.Op = protocol.OpCountTokens
	default:
		return protocol.Inbound{}, ir.Errorf(ir.ErrNotFound, "action %q is not supported", action)
	}
	in.Session.PrefixHash = prefixHash(requestBody(body))
	return in, nil
}

// ParsePath extracts the model id and action from a path such as
// "/v1beta/models/gemini-2.5-pro:streamGenerateContent". Model ids may
// contain "/" and "."; the action is whatever follows the last ":".
func ParsePath(p string) (model, action string, ok bool) {
	if u, err := url.PathUnescape(p); err == nil {
		p = u
	}
	i := strings.Index(p, "/models/")
	if i >= 0 {
		p = p[i+len("/models/"):]
	} else if after, found := strings.CutPrefix(strings.TrimPrefix(p, "/"), "models/"); found {
		p = after
	} else {
		return "", "", false
	}
	j := strings.LastIndexByte(p, ':')
	if j <= 0 || j == len(p)-1 {
		return "", "", false
	}
	return p[:j], p[j+1:], true
}

// requestBody unwraps a countTokens body of the generateContentRequest form.
func requestBody(body []byte) []byte {
	if w := gjson.GetBytes(body, "generateContentRequest"); w.IsObject() {
		return []byte(w.Raw)
	}
	return body
}

// prefixHash hashes the text of systemInstruction and the first user content.
func prefixHash(body []byte) string {
	h := sha256.New()
	gjson.GetBytes(body, "systemInstruction.parts.#.text").ForEach(func(_, v gjson.Result) bool {
		h.Write([]byte(v.String()))
		h.Write([]byte{0})
		return true
	})
	h.Write([]byte{1})
	found := false
	gjson.GetBytes(body, "contents").ForEach(func(_, c gjson.Result) bool {
		if r := c.Get("role").String(); r != "" && r != "user" {
			return true
		}
		c.Get("parts").ForEach(func(_, p gjson.Result) bool {
			if t := p.Get("text"); t.Exists() && !p.Get("thought").Bool() {
				h.Write([]byte(t.String()))
				h.Write([]byte{0})
			}
			return true
		})
		found = true
		return false
	})
	if !found && !gjson.GetBytes(body, "systemInstruction").Exists() {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// UpstreamPath implements protocol.Codec.
func (Codec) UpstreamPath(req *ir.Request, in protocol.Inbound) string {
	model := strings.TrimPrefix(req.Model, "models/")
	switch {
	case in.Op == protocol.OpCountTokens:
		return "/v1beta/models/" + model + ":" + ActionCountTokens
	case req.Stream:
		return "/v1beta/models/" + model + ":" + ActionStream + "?alt=sse"
	default:
		return "/v1beta/models/" + model + ":" + ActionGenerate
	}
}

// SetModel implements protocol.Codec. The model lives in the path.
func (Codec) SetModel(body []byte, _ string) ([]byte, error) { return body, nil }

// StripOpaque implements protocol.Codec. It removes every thoughtSignature
// from history; parts left with no content are removed, and so are contents
// left with no parts, since the API rejects empty parts.
func (Codec) StripOpaque(body []byte) ([]byte, int, error) {
	prefix := ""
	if gjson.GetBytes(body, "generateContentRequest").IsObject() {
		prefix = "generateContentRequest."
	}
	contents := gjson.GetBytes(body, prefix+"contents").Array()
	removed := 0
	out := body
	var err error
	for ci := len(contents) - 1; ci >= 0; ci-- {
		parts := contents[ci].Get("parts").Array()
		kept := len(parts)
		for pi := len(parts) - 1; pi >= 0; pi-- {
			p := parts[pi]
			if !p.Get("thoughtSignature").Exists() {
				continue
			}
			removed++
			path := prefix + "contents." + itoa(ci) + ".parts." + itoa(pi)
			if signatureOnly(p) {
				out, err = sjson.DeleteBytes(out, path)
				kept--
			} else {
				out, err = sjson.DeleteBytes(out, path+".thoughtSignature")
			}
			if err != nil {
				return body, 0, err
			}
		}
		if kept == 0 && len(parts) > 0 {
			if out, err = sjson.DeleteBytes(out, prefix+"contents."+itoa(ci)); err != nil {
				return body, 0, err
			}
		}
	}
	return out, removed, nil
}

// signatureOnly reports whether a part has nothing but a signature (and an
// empty text and/or thought flag).
func signatureOnly(p gjson.Result) bool {
	only := true
	p.ForEach(func(k, v gjson.Result) bool {
		switch k.String() {
		case "thoughtSignature", "thought":
		case "text":
			if v.String() != "" {
				only = false
			}
		default:
			only = false
		}
		return only
	})
	return only
}

// NewStreamEncoder implements protocol.Codec.
func (Codec) NewStreamEncoder(w sse.Writer, req *ir.Request) protocol.StreamEncoder {
	return newStreamEncoder(w, req)
}

// NewStreamDecoder implements protocol.Codec.
func (Codec) NewStreamDecoder() protocol.StreamDecoder { return newStreamDecoder() }

func itoa(i int) string { return strconv.Itoa(i) }
