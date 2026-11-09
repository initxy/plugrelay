package demo

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// reply is what the fake upstream answers every request with.
const reply = "Hello from the plugrelay demo upstream. No real model was called."

// fakeUpstream answers the Anthropic Messages, OpenAI Chat Completions,
// OpenAI Responses and Bedrock InvokeModel calls the demo upstreams make,
// streamed or not, with a fixed reply and plausible usage. It ignores
// credentials.
func fakeUpstream() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if r.Method == http.MethodPost && p == "/v1/checkout/sessions" {
			fakeCheckout(w, r)
			return
		}
		if r.Method == http.MethodGet && p == catalogPath {
			serveCatalog(w)
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(p, "/models") {
			writeJSON(w, map[string]any{"object": "list", "data": []any{}})
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		var in struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &in)
		inTok := max(len(body)/4, 1)
		outTok := len(reply) / 4
		switch {
		case strings.HasPrefix(p, "/hooks/"):
			writeJSON(w, map[string]any{"code": 0, "errcode": 0})
		case strings.HasSuffix(p, "/invoke"):
			writeJSON(w, anthropicMessage(in.Model, inTok, outTok))
		case strings.HasSuffix(p, "/messages"):
			if in.Stream {
				anthropicStream(w, in.Model, inTok, outTok)
			} else {
				writeJSON(w, anthropicMessage(in.Model, inTok, outTok))
			}
		case strings.HasSuffix(p, "/chat/completions"):
			if in.Stream {
				chatStream(w, in.Model, inTok, outTok)
			} else {
				writeJSON(w, chatCompletion(in.Model, inTok, outTok))
			}
		case strings.HasSuffix(p, "/responses"):
			if in.Stream {
				responsesStream(w, in.Model, inTok, outTok)
			} else {
				writeJSON(w, response(in.Model, "completed", inTok, outTok, true))
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"error":{"type":"not_found_error","message":"the demo upstream does not serve %s"}}`, p)
		}
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type sse struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSE(w http.ResponseWriter) *sse {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	f, _ := w.(http.Flusher)
	return &sse{w: w, f: f}
}

func (s *sse) send(event string, v any) {
	b, _ := json.Marshal(v)
	if event != "" {
		_, _ = fmt.Fprintf(s.w, "event: %s\n", event)
	}
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", b)
	if s.f != nil {
		s.f.Flush()
	}
}

func (s *sse) raw(line string) {
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", line)
	if s.f != nil {
		s.f.Flush()
	}
}

// chunks splits the reply into a few stream deltas.
func chunks() []string {
	words := strings.SplitAfter(reply, " ")
	var out []string
	for i := 0; i < len(words); i += 3 {
		out = append(out, strings.Join(words[i:min(i+3, len(words))], ""))
	}
	return out
}

func anthropicMessage(model string, in, out int) map[string]any {
	return map[string]any{
		"id": "msg_demo", "type": "message", "role": "assistant", "model": model,
		"content":     []any{map[string]any{"type": "text", "text": reply}},
		"stop_reason": "end_turn", "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0},
	}
}

func anthropicStream(w http.ResponseWriter, model string, in, out int) {
	s := newSSE(w)
	s.send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_demo", "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": in, "output_tokens": 1, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0}}})
	s.send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	for _, c := range chunks() {
		s.send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": c}})
	}
	s.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	s.send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": out}})
	s.send("message_stop", map[string]any{"type": "message_stop"})
}

func chatCompletion(model string, in, out int) map[string]any {
	return map[string]any{
		"id": "chatcmpl-demo", "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": reply}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out},
	}
}

func chatStream(w http.ResponseWriter, model string, in, out int) {
	s := newSSE(w)
	created := time.Now().Unix()
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": "chatcmpl-demo", "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	for i, c := range chunks() {
		d := map[string]any{"content": c}
		if i == 0 {
			d["role"] = "assistant"
		}
		s.send("", chunk(d, nil))
	}
	s.send("", chunk(map[string]any{}, "stop"))
	s.send("", map[string]any{"id": "chatcmpl-demo", "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{}, "usage": map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}})
	s.raw("[DONE]")
}

func message(done bool) map[string]any {
	status, content := "in_progress", []any{}
	if done {
		status = "completed"
		content = []any{map[string]any{"type": "output_text", "text": reply, "annotations": []any{}}}
	}
	return map[string]any{"type": "message", "id": "msg_demo", "status": status, "role": "assistant", "content": content}
}

func response(model, status string, in, out int, done bool) map[string]any {
	r := map[string]any{"id": "resp_demo", "object": "response", "created_at": time.Now().Unix(), "status": status,
		"model": model, "output": []any{}}
	if done {
		r["output"] = []any{message(true)}
		r["usage"] = map[string]any{"input_tokens": in, "input_tokens_details": map[string]any{"cached_tokens": 0},
			"output_tokens": out, "output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": in + out}
	}
	return r
}

func responsesStream(w http.ResponseWriter, model string, in, out int) {
	s := newSSE(w)
	seq := 0
	send := func(typ string, fields map[string]any) {
		fields["type"], fields["sequence_number"] = typ, seq
		seq++
		s.send(typ, fields)
	}
	part := func(text string) map[string]any {
		return map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	}
	send("response.created", map[string]any{"response": response(model, "in_progress", in, out, false)})
	send("response.in_progress", map[string]any{"response": response(model, "in_progress", in, out, false)})
	send("response.output_item.added", map[string]any{"output_index": 0, "item": message(false)})
	send("response.content_part.added", map[string]any{"item_id": "msg_demo", "output_index": 0, "content_index": 0, "part": part("")})
	for _, c := range chunks() {
		send("response.output_text.delta", map[string]any{"item_id": "msg_demo", "output_index": 0, "content_index": 0, "delta": c})
	}
	send("response.output_text.done", map[string]any{"item_id": "msg_demo", "output_index": 0, "content_index": 0, "text": reply})
	send("response.content_part.done", map[string]any{"item_id": "msg_demo", "output_index": 0, "content_index": 0, "part": part(reply)})
	send("response.output_item.done", map[string]any{"output_index": 0, "item": message(true)})
	send("response.completed", map[string]any{"response": response(model, "completed", in, out, true)})
}
