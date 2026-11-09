package admin

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// PlaygroundUserAgent marks playground requests in the request log.
const PlaygroundUserAgent = "plugrelay-playground/1"

// PlaygroundMessage is one turn of a playground conversation.
type PlaygroundMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// PlaygroundRequest is one playground call.
type PlaygroundRequest struct {
	KeyID       string              `json:"key_id,omitempty"`
	Protocol    string              `json:"protocol"`
	Model       string              `json:"model"`
	System      string              `json:"system,omitempty"`
	Messages    []PlaygroundMessage `json:"messages"`
	Temperature *float64            `json:"temperature,omitempty"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Effort      string              `json:"reasoning_effort,omitempty"`
}

// PlaygroundEvent is one server-sent event of a playground answer: start
// (request id), text and reasoning (a piece of the answer), done (the
// outcome).
type PlaygroundEvent struct {
	Type      string    `json:"-"`
	Text      string    `json:"text,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
	OK        bool      `json:"ok,omitempty"`
	Status    int       `json:"status,omitempty"`
	TTFBMs    int64     `json:"ttfb_ms,omitempty"`
	LatencyMs int64     `json:"latency_ms,omitempty"`
	Usage     *ir.Usage `json:"usage,omitempty"`
	Error     string    `json:"error,omitempty"`
}

var playgroundProtocols = []string{"anthropic", "openai-chat", "openai-responses", "gemini"}

const playgroundMaxChars = 200_000

func (h *Handler) playground(c *call) error {
	var in PlaygroundRequest
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	if !slices.Contains(playgroundProtocols, in.Protocol) {
		bad.add("protocol", oneOf(playgroundProtocols...))
	}
	if strings.TrimSpace(in.Model) == "" {
		bad.add("model", eRequired)
	}
	if len(in.Messages) == 0 {
		bad.add("messages", eRequired)
	}
	chars := len(in.System)
	for i, m := range in.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			bad.add("messages."+strconv.Itoa(i)+".role", oneOf("user", "assistant"))
		}
		chars += len(m.Content)
	}
	if chars > playgroundMaxChars {
		bad.add("messages", eLengthRange.with("min", 1, "max", playgroundMaxChars))
	}
	if in.Effort != "" && !slices.Contains([]string{"low", "medium", "high"}, in.Effort) {
		bad.add("reasoning_effort", oneOf("low", "medium", "high"))
	}
	if in.Temperature != nil && (*in.Temperature < 0 || *in.Temperature > 2) {
		bad.add("temperature", eNumberRange.with("min", 0, "max", 2))
	}
	if in.MaxTokens < 0 || in.MaxTokens > 128_000 {
		bad.add("max_tokens", eNumberRange.with("min", 1, "max", 128_000))
	}
	if !c.who.isAdmin() && in.KeyID == "" {
		bad.add("key_id", eRequired)
	}
	if err := bad.err(); err != nil {
		return err
	}
	var caller plugin.Caller
	if in.KeyID != "" {
		k, err := h.ownKey(c, in.KeyID)
		if err != nil {
			return err
		}
		if !k.Usable(h.d.Now()) {
			return fieldInvalid("key_id", eKeyUnusable)
		}
		caller = k.Caller()
	} else {
		// An admin without a key of their own talks as themselves.
		u := c.who.user
		if u == nil {
			return errForbidden(eUserRequired)
		}
		caller = plugin.Caller{KeyName: "playground", UserID: u.ID, GroupID: u.GroupID}
	}

	w := c.w
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	// A long answer outlasts the admin API's write deadline; the relay's
	// own timeouts bound the request instead.
	_ = rc.SetWriteDeadline(time.Time{})
	_ = rc.Flush()
	h.d.Runtime.Playground(c.ctx, caller, in, func(ev PlaygroundEvent) {
		b, _ := json.Marshal(ev)
		_, _ = w.Write([]byte("event: " + ev.Type + "\ndata: " + string(b) + "\n\n"))
		_ = rc.Flush()
	})
	return nil
}
