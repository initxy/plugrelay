package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/pipeline"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol"
	"github.com/initxy/plugrelay/pkg/sse"
)

// playgroundBody builds a streaming request in the client protocol.
func playgroundBody(req admin.PlaygroundRequest) (path string, body []byte) {
	type msg = map[string]any
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	switch req.Protocol {
	case "anthropic":
		b := msg{"model": req.Model, "max_tokens": maxTokens, "stream": true}
		var ms []any
		for _, m := range req.Messages {
			ms = append(ms, msg{"role": m.Role, "content": m.Content})
		}
		b["messages"] = ms
		if req.System != "" {
			b["system"] = req.System
		}
		if req.Temperature != nil {
			b["temperature"] = *req.Temperature
		}
		if req.Effort != "" {
			b["thinking"] = msg{"type": "adaptive"}
			b["output_config"] = msg{"effort": req.Effort}
		}
		body, _ = json.Marshal(b)
		return "/v1/messages", body
	case "openai-responses":
		var in []any
		for _, m := range req.Messages {
			in = append(in, msg{"role": m.Role, "content": m.Content})
		}
		b := msg{"model": req.Model, "input": in, "stream": true, "max_output_tokens": maxTokens, "store": false}
		if req.System != "" {
			b["instructions"] = req.System
		}
		if req.Temperature != nil {
			b["temperature"] = *req.Temperature
		}
		if req.Effort != "" {
			b["reasoning"] = msg{"effort": req.Effort, "summary": "auto"}
		}
		body, _ = json.Marshal(b)
		return "/v1/responses", body
	case "gemini":
		var contents []any
		for _, m := range req.Messages {
			role := "user"
			if m.Role == "assistant" {
				role = "model"
			}
			contents = append(contents, msg{"role": role, "parts": []any{msg{"text": m.Content}}})
		}
		gc := msg{"maxOutputTokens": maxTokens}
		if req.Temperature != nil {
			gc["temperature"] = *req.Temperature
		}
		if req.Effort != "" {
			gc["thinkingConfig"] = msg{"thinkingLevel": req.Effort, "includeThoughts": true}
		}
		b := msg{"contents": contents, "generationConfig": gc}
		if req.System != "" {
			b["systemInstruction"] = msg{"parts": []any{msg{"text": req.System}}}
		}
		body, _ = json.Marshal(b)
		return "/v1beta/models/" + url.PathEscape(req.Model) + ":streamGenerateContent?alt=sse", body
	}
	var ms []any
	if req.System != "" {
		ms = append(ms, msg{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		ms = append(ms, msg{"role": m.Role, "content": m.Content})
	}
	b := msg{"model": req.Model, "messages": ms, "stream": true, "max_completion_tokens": maxTokens,
		"stream_options": msg{"include_usage": true}}
	if req.Temperature != nil {
		b["temperature"] = *req.Temperature
	}
	if req.Effort != "" {
		b["reasoning_effort"] = req.Effort
	}
	body, _ = json.Marshal(b)
	return "/v1/chat/completions", body
}

// pipeWriter is the client side of a playground request: the pipeline
// writes its response into a pipe the playground reads as it arrives.
type pipeWriter struct {
	header http.Header
	w      *io.PipeWriter
	once   sync.Once
	status chan int
}

func (p *pipeWriter) Header() http.Header { return p.header }

func (p *pipeWriter) WriteHeader(code int) {
	p.once.Do(func() { p.status <- code })
}

func (p *pipeWriter) Write(b []byte) (int, error) {
	p.WriteHeader(http.StatusOK)
	return p.w.Write(b)
}

func (p *pipeWriter) Flush() {}

// playground sends one conversation through the live relay as caller,
// exactly like a client request (logged and billed), and reports what the
// stream carries.
func (a *App) playground(ctx context.Context, caller plugin.Caller, req admin.PlaygroundRequest, emit func(admin.PlaygroundEvent)) {
	start := time.Now()
	codec, ok := protocol.Get(req.Protocol)
	if !ok {
		emit(admin.PlaygroundEvent{Type: "done", Error: "unknown protocol " + req.Protocol})
		return
	}
	var route protocol.Route
	for _, rt := range codec.Routes() {
		if rt.Op == protocol.OpGenerate {
			route = rt
			break
		}
	}
	path, body := playgroundBody(req)
	id := newID()
	hr := httptest.NewRequestWithContext(pipeline.WithRequestID(ctx, id), http.MethodPost, path, bytes.NewReader(body))
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "text/event-stream")
	hr.Header.Set("User-Agent", admin.PlaygroundUserAgent)
	hr.RemoteAddr = "127.0.0.1:0"

	pr, pw := io.Pipe()
	w := &pipeWriter{header: http.Header{}, w: pw, status: make(chan int, 1)}
	go func() {
		a.pipe.ServeProtocol(w, hr, codec, route, &caller, body)
		w.WriteHeader(http.StatusInternalServerError)
		_ = pw.Close()
	}()
	defer func() { _ = pr.Close() }()

	status := <-w.status
	done := admin.PlaygroundEvent{Type: "done", Status: status, RequestID: id}
	emit(admin.PlaygroundEvent{Type: "start", RequestID: done.RequestID})
	finish := func() {
		done.LatencyMs = time.Since(start).Milliseconds()
		emit(done)
	}
	if status/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(pr, 64<<10))
		if e := codec.DecodeError(status, w.header, b); e != nil && e.Message != "" {
			done.Error = e.Message
		} else {
			done.Error = strings.TrimSpace(string(b))
		}
		finish()
		return
	}
	dec := codec.NewStreamDecoder()
	rd := sse.NewReader(pr, 0)
	handle := func(evs []ir.Event) {
		for _, ev := range evs {
			switch {
			case ev.Kind == ir.EvDelta && ev.Delta != nil && ev.Delta.Text != "" &&
				(ev.Delta.Kind == ir.DeltaText || ev.Delta.Kind == ir.DeltaThinking):
				if done.TTFBMs == 0 {
					done.TTFBMs = max(1, time.Since(start).Milliseconds())
				}
				t := "text"
				if ev.Delta.Kind == ir.DeltaThinking {
					t = "reasoning"
				}
				emit(admin.PlaygroundEvent{Type: t, Text: ev.Delta.Text})
			case ev.Kind == ir.EvError && ev.Err != nil:
				done.Error = ev.Err.Message
			}
		}
	}
	for {
		ev, err := rd.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && done.Error == "" && ctx.Err() == nil {
				done.Error = err.Error()
			}
			break
		}
		if ev.Comment {
			continue
		}
		evs, err := dec.Feed(ev)
		if err != nil {
			done.Error = err.Error()
			break
		}
		handle(evs)
	}
	evs, _ := dec.Finish()
	handle(evs)
	u := dec.Usage()
	done.Usage = &u
	done.OK = done.Error == ""
	finish()
}
