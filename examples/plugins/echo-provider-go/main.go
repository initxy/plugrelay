// Command echo-provider is an external provider plugin that answers every
// request by streaming back the last user message. It never contacts an
// upstream, which makes it a handy fake for end-to-end tests.
//
// Upstream config:
//
//	prefix:     text prepended to the echo (default "")
//	chunk_size: characters per text delta (default 16)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/sdk/go/pluginsdk"
)

func main() {
	p := pluginsdk.New("echo", "0.1.0")
	p.Prepare(func(ctx context.Context, o *pluginsdk.Outgoing) error {
		h := http.Header(o.Header)
		if o.Credential != nil && o.Credential.Secret != "" {
			h.Set("Authorization", "Bearer "+o.Credential.Secret)
		} else {
			h.Del("Authorization")
		}
		o.Header = h
		return nil
	})
	p.Send(send)
	p.Classify(func(ctx context.Context, o *pluginsdk.Outcome) (*pluginsdk.Verdict, error) {
		return classify(o), nil
	})
	p.ListModels(func(ctx context.Context, u *pluginsdk.Upstream, c *pluginsdk.Credential) ([]ir.ModelInfo, error) {
		return []ir.ModelInfo{{ID: "echo", DisplayName: "Echo", OwnedBy: "plugrelay"}}, nil
	})
	if err := p.Serve(); err != nil {
		log.Fatal(err)
	}
}

func send(ctx context.Context, o *pluginsdk.Outgoing, req *ir.Request, emit pluginsdk.Emit) (*pluginsdk.Outcome, error) {
	text := lastUserText(req, o.Body)
	prefix, chunk := "", 16
	if o.Upstream != nil {
		if s, ok := o.Upstream.Config["prefix"].(string); ok {
			prefix = s
		}
		if n, ok := o.Upstream.Config["chunk_size"].(float64); ok && n > 0 {
			chunk = int(n)
		}
	}
	text = prefix + text

	if err := emit(pluginsdk.StartEvent(fmt.Sprintf("echo-%d", time.Now().UnixNano()), o.Model)); err != nil {
		return nil, err
	}
	if err := emit(&pluginsdk.Event{Event: ir.Event{Kind: ir.EvBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}}}); err != nil {
		return nil, err
	}
	for rest := text; rest != ""; {
		cut := byteOffset(rest, chunk)
		if err := emit(&pluginsdk.Event{Event: ir.Event{Kind: ir.EvDelta, Index: 0, Delta: &ir.Delta{Kind: ir.DeltaText, Text: rest[:cut]}}}); err != nil {
			return nil, err
		}
		rest = rest[cut:]
	}
	if err := emit(&pluginsdk.Event{Event: ir.Event{Kind: ir.EvBlockStop, Index: 0}}); err != nil {
		return nil, err
	}
	in := int64(utf8.RuneCountInString(text)/4 + 1)
	return nil, emit(pluginsdk.DoneEvent(ir.StopEndTurn, &ir.Usage{Input: in, Output: in, Estimated: true}))
}

func byteOffset(s string, runes int) int {
	n := 0
	for i := range s {
		if n == runes {
			return i
		}
		n++
	}
	return len(s)
}

// lastUserText reads the decoded request when the host sends one, else the
// raw body in the openai-chat or anthropic shape.
func lastUserText(req *ir.Request, body json.RawMessage) string {
	if req != nil {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			m := req.Messages[i]
			if m.Role != ir.RoleUser {
				continue
			}
			var parts []string
			for _, b := range m.Content {
				if b.Type == ir.BlockText {
					parts = append(parts, b.Text)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, "\n")
			}
		}
	}
	var raw struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return ""
	}
	for i := len(raw.Messages) - 1; i >= 0; i-- {
		m := raw.Messages[i]
		if m.Role != "user" {
			continue
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			return s
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &blocks) == nil {
			var parts []string
			for _, b := range blocks {
				if b.Type == "text" {
					parts = append(parts, b.Text)
				}
			}
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

// classify applies the conservative rules every provider follows; the echo
// provider never fails on its own, but it may sit behind a real upstream in
// conformance runs.
func classify(o *pluginsdk.Outcome) *pluginsdk.Verdict {
	v := &pluginsdk.Verdict{Sent: o.Sent, Retry: plugin.RetryNone, Credential: plugin.CredKeep, ClientError: o.Signal}
	if v.ClientError == nil {
		v.ClientError = &pluginsdk.Error{Kind: ir.ErrUpstream, Message: "upstream request failed", Status: o.Status}
	}
	body := strings.ToLower(o.Body)
	switch {
	case !o.Sent:
		v.Cause, v.Retry = plugin.CauseTransient, plugin.RetryNext
	case o.Status < 400:
		v.Cause = plugin.CauseTransient
	case o.Status == http.StatusTooManyRequests:
		v.Cause, v.Retry, v.Credential = plugin.CauseRateLimit, plugin.RetryNext, plugin.CredCooldown
		if o.Signal != nil && o.Signal.RetryAfterMs > 0 {
			v.Until = time.Now().Add(time.Duration(o.Signal.RetryAfterMs) * time.Millisecond)
		}
	case o.Status == http.StatusUnauthorized:
		v.Cause, v.Retry, v.Credential = plugin.CauseAuthExpired, plugin.RetryNext, plugin.CredRefresh
	case o.Status == http.StatusForbidden:
		// Suspension and content refusals look alike; quarantine either way.
		v.Cause, v.Credential = plugin.CauseUnknown, plugin.CredQuarantine
		for _, w := range []string{"suspended", "deactivated", "disabled"} {
			if strings.Contains(body, w) {
				v.Cause = plugin.CauseBanned
			}
		}
	case o.Signal != nil && o.Signal.Kind == ir.ErrContentFiltered:
		v.Cause = plugin.CauseContent
	case o.Status < 500:
		v.Cause = plugin.CauseRequest
	default:
		v.Cause, v.Retry = plugin.CauseTransient, plugin.RetryNext
	}
	return v
}
