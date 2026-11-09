package anthropic

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func init() { protocol.Register(Codec{}) }

// Codec is the Anthropic Messages protocol. It is stateless.
type Codec struct{}

var _ protocol.Codec = Codec{}

// Name returns "anthropic".
func (Codec) Name() string { return name }

// Capabilities reports everything the Messages API can express.
func (Codec) Capabilities() ir.Capabilities {
	return ir.Capabilities{
		Cache:         ir.CacheExplicit,
		Cache1h:       true,
		FunctionTools: true,
		ServerTools: []string{
			"web_search_20250305", "web_fetch_20250910",
			"code_execution_20250522", "code_execution_20250825",
		},
		DeferredTools:  true,
		Reasoning:      true,
		ReasoningStyle: "budget",
		Images:         true,
		Documents:      true,
		ToolResultImgs: true,
		EchoFields:     []string{kindRedacted, kindSignature},
		MaxTokensField: "max_tokens",
	}
}

// Routes lists the client-facing endpoints. A "?beta=true" query on
// /v1/messages is accepted and ignored by the HTTP server's path matching.
func (Codec) Routes() []protocol.Route {
	return []protocol.Route{
		{Method: http.MethodPost, Path: "/v1/messages", Op: protocol.OpGenerate},
		{Method: http.MethodPost, Path: "/v1/messages/count_tokens", Op: protocol.OpCountTokens},
		{Method: http.MethodGet, Path: "/v1/models", Op: protocol.OpModels},
		{Method: http.MethodHead, Path: "/api/hello", Op: protocol.OpHello},
		{Method: http.MethodGet, Path: "/api/hello", Op: protocol.OpHello},
	}
}

// Session hint headers.
const (
	headerSessionID = "X-Claude-Code-Session-Id"
	headerAgentID   = "X-Claude-Code-Agent-Id"
)

// Sniff reads the model, stream flag and session hints.
//
// Session hints come from the session and agent id headers and from
// metadata.user_id, which is kept as UserTag. When user_id is itself a JSON
// object with a session_id, or a legacy "..._session_<id>" string, that id
// fills SessionID if no header did.
func (Codec) Sniff(r *http.Request, route protocol.Route, body []byte) (protocol.Inbound, error) {
	in := protocol.Inbound{Op: route.Op}
	if r != nil {
		in.Session.SessionID = r.Header.Get(headerSessionID)
		in.Session.AgentID = r.Header.Get(headerAgentID)
	}
	if route.Op != protocol.OpGenerate && route.Op != protocol.OpCountTokens {
		return in, nil
	}
	if !gjson.ValidBytes(body) {
		return in, &ir.GwError{Kind: ir.ErrInvalidRequest, Message: errInvalidJSON.Error()}
	}
	root := gjson.ParseBytes(body)
	in.Model = root.Get("model").String()
	in.Stream = root.Get("stream").Bool()
	if uid := root.Get("metadata.user_id").String(); uid != "" {
		in.Session.UserTag = uid
		if in.Session.SessionID == "" {
			in.Session.SessionID = sessionFromUserID(uid)
		}
	}
	in.Session.PrefixHash = prefixHash(root)
	return in, nil
}

func sessionFromUserID(uid string) string {
	if gjson.Valid(uid) {
		return gjson.Get(uid, "session_id").String()
	}
	if i := strings.LastIndex(uid, "_session_"); i >= 0 {
		return uid[i+len("_session_"):]
	}
	return ""
}

// prefixHash hashes the conversation's stable start: every system block and
// the first user message, each with cache_control removed. Text blocks of the
// first user message that begin with a tag-like wrapper ("<system-reminder>",
// "<environment>", ...) are skipped: clients inject per-turn context that way
// and it changes while the conversation does not. A string-form first message
// starting with such a wrapper is skipped the same way.
func prefixHash(root gjson.Result) string {
	h := sha256.New()
	n := 0
	write := func(b gjson.Result) {
		s := b.Raw
		if b.IsObject() {
			if out, err := sjson.Delete(s, "cache_control"); err == nil {
				s = out
			}
		}
		h.Write([]byte(s))
		h.Write([]byte{0})
		n++
	}
	sys := root.Get("system")
	if sys.IsArray() {
		sys.ForEach(func(_, b gjson.Result) bool { write(b); return true })
	} else if sys.Exists() {
		write(sys)
	}
	h.Write([]byte{1})
	for _, m := range root.Get("messages").Array() {
		if m.Get("role").String() != string(ir.RoleUser) {
			continue
		}
		c := m.Get("content")
		if c.IsArray() {
			c.ForEach(func(_, b gjson.Result) bool {
				if !(b.Get("type").String() == wireText && injected(b.Get("text").String())) {
					write(b)
				}
				return true
			})
		} else if !injected(c.String()) {
			write(c)
		}
		break
	}
	if n == 0 {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// injected reports whether text starts with an XML-like opening tag.
func injected(text string) bool {
	t := strings.TrimLeft(text, " \t\r\n")
	if len(t) < 2 || t[0] != '<' {
		return false
	}
	c := t[1]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// EncodeModels renders the Anthropic model list format.
func (Codec) EncodeModels(_ *http.Request, models []ir.ModelInfo) (string, []byte) {
	data := make([]any, 0, len(models))
	for _, m := range models {
		display := m.DisplayName
		if display == "" {
			display = m.ID
		}
		data = append(data, map[string]any{
			"type":         "model",
			"id":           m.ID,
			"display_name": display,
			"created_at":   time.Unix(m.Created, 0).UTC().Format(time.RFC3339),
		})
	}
	out := map[string]any{"data": data, "has_more": false, "first_id": nil, "last_id": nil}
	if len(models) > 0 {
		out["first_id"], out["last_id"] = models[0].ID, models[len(models)-1].ID
	}
	return "application/json", marshal(out)
}

// CountTokens answers count_tokens with a local estimate.
func (Codec) CountTokens(req *ir.Request) ([]byte, error) {
	return marshal(map[string]int{"input_tokens": estimateTokens(req)}), nil
}

// UpstreamPath returns the messages or count_tokens path.
func (Codec) UpstreamPath(_ *ir.Request, in protocol.Inbound) string {
	if in.Op == protocol.OpCountTokens {
		return "/v1/messages/count_tokens"
	}
	return "/v1/messages"
}

// SetModel rewrites only the model value, leaving every other byte intact.
func (Codec) SetModel(body []byte, model string) ([]byte, error) {
	return sjson.SetBytes(body, "model", model)
}

// StripOpaque deletes thinking and redacted_thinking blocks from assistant
// messages, leaving every other byte intact. It returns how many blocks it
// removed. An assistant message left with no content is deleted when its
// neighbours still alternate roles without it; otherwise it gets a single
// placeholder text block, since the API rejects empty content and empty text.
func (Codec) StripOpaque(body []byte) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return body, 0, &ir.GwError{Kind: ir.ErrInvalidRequest, Message: errInvalidJSON.Error()}
	}
	removed := 0
	msgs := gjson.GetBytes(body, "messages").Array()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Get("role").String() != string(ir.RoleAssistant) {
			continue
		}
		blocks := msgs[i].Get("content").Array()
		kept := len(blocks)
		for j := len(blocks) - 1; j >= 0; j-- {
			t := blocks[j].Get("type").String()
			if t != wireThinking && t != wireRedactedThinking {
				continue
			}
			out, err := sjson.DeleteBytes(body, pathf(i, j))
			if err != nil {
				return body, removed, err
			}
			body = out
			removed++
			kept--
		}
		if kept > 0 || len(blocks) == 0 {
			continue
		}
		var err error
		if canDropMessage(body, i) {
			body, err = sjson.DeleteBytes(body, "messages."+strconv.Itoa(i))
		} else {
			body, err = sjson.SetRawBytes(body, "messages."+strconv.Itoa(i)+".content", []byte(placeholderContent))
		}
		if err != nil {
			return body, removed, err
		}
	}
	return body, removed, nil
}

// placeholderContent replaces an assistant message emptied by StripOpaque.
const placeholderContent = `[{"type":"text","text":"(no content)"}]`

// canDropMessage reports whether removing message i keeps the history valid:
// non-empty, starting with a user turn, and alternating roles across the gap.
func canDropMessage(body []byte, i int) bool {
	msgs := gjson.GetBytes(body, "messages").Array()
	role := func(k int) string { return msgs[k].Get("role").String() }
	switch {
	case len(msgs) < 2:
		return false
	case i == 0:
		return role(1) == string(ir.RoleUser)
	case i == len(msgs)-1:
		return true
	}
	return role(i-1) != role(i+1)
}

func pathf(i, j int) string {
	return "messages." + strconv.Itoa(i) + ".content." + strconv.Itoa(j)
}
