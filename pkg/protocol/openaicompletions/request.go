package openaicompletions

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/protocol"
)

// sameProtocolOnly is the reason given for requests only an endpoint of
// this protocol can serve verbatim.
const sameProtocolOnly = " is only served by an openai-completions endpoint"

// DecodeRequest parses a completions request into IR: the prompt becomes
// one user text message. Batches, token-id prompts, n/best_of above 1 and
// echo have no faithful IR form and fail, which keeps them on same-protocol
// passthrough.
func (Codec) DecodeRequest(_ *http.Request, _ protocol.Inbound, body []byte) (*ir.Request, ir.Needs, error) {
	top := rawMap(body)
	if top == nil {
		return nil, ir.Needs{}, ir.Errorf(ir.ErrInvalidRequest, "request body must be a JSON object")
	}
	bad := func(f string) (*ir.Request, ir.Needs, error) {
		return nil, ir.Needs{}, ir.Errorf(ir.ErrInvalidRequest, "invalid %s", f)
	}
	only := func(what string) (*ir.Request, ir.Needs, error) {
		return nil, ir.Needs{}, ir.Errorf(ir.ErrInvalidRequest, "%s%s", what, sameProtocolOnly)
	}
	req := &ir.Request{}
	if v, ok := take(top, "model"); ok {
		req.Model = str(v)
	}
	if v, ok := take(top, "stream"); ok && json.Unmarshal(v, &req.Stream) != nil {
		return bad("stream")
	}
	prompt := ""
	if v, ok := take(top, "prompt"); ok {
		var list []json.RawMessage
		switch {
		case json.Unmarshal(v, &prompt) == nil:
		case json.Unmarshal(v, &list) == nil:
			if len(list) != 1 || json.Unmarshal(list[0], &prompt) != nil {
				return only("a prompt list or token-id prompt")
			}
			mark(&req.Extra, xPromptArr, jsonTrue)
		default:
			return bad("prompt")
		}
	}
	req.Messages = []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: prompt}}}}
	for _, k := range []string{"n", "best_of"} {
		if v, ok := top[k]; ok && !isNull(v) {
			var n int
			if json.Unmarshal(v, &n) != nil {
				return bad(k)
			}
			if n > 1 {
				return only(k + " above 1")
			}
		}
	}
	if v, ok := top["echo"]; ok && string(v) == "true" {
		return only("echo")
	}
	var needs ir.Needs
	if v, ok := top["suffix"]; ok && !isNull(v) {
		var s string
		if json.Unmarshal(v, &s) != nil {
			return bad("suffix")
		}
		needs.FillInMiddle = s != ""
	}
	if v, ok := take(top, "max_tokens"); ok && json.Unmarshal(v, &req.MaxOutputTokens) != nil {
		return bad("max_tokens")
	}
	for _, n := range []struct {
		k string
		p any
	}{{"temperature", &req.Temperature}, {"top_p", &req.TopP}, {"top_k", &req.TopK}} {
		if v, ok := take(top, n.k); ok && json.Unmarshal(v, n.p) != nil {
			return bad(n.k)
		}
	}
	if v, ok := take(top, "stop"); ok {
		var s string
		if json.Unmarshal(v, &s) == nil {
			req.Stop = []string{s}
			mark(&req.Extra, xStopStr, jsonTrue)
		} else if json.Unmarshal(v, &req.Stop) != nil {
			return bad("stop")
		}
	}
	for k, v := range top {
		req.Extra.Set(Name, k, v)
	}
	needs.InputTokensEst = estimateTokens(prompt)
	return req, needs, nil
}

// promptText joins the text of the request's user messages, the only
// content a prompt-only protocol can carry.
func promptText(req *ir.Request) string {
	var b strings.Builder
	for _, m := range req.Messages {
		for _, bl := range m.Content {
			if bl.Type == ir.BlockText {
				b.WriteString(bl.Text)
			}
		}
	}
	return b.String()
}

// singlePrompt reports whether req is one user message of text only.
func singlePrompt(req *ir.Request) bool {
	if len(req.System) > 0 || len(req.Tools) > 0 || len(req.Messages) != 1 || req.Messages[0].Role != ir.RoleUser {
		return false
	}
	for _, b := range req.Messages[0].Content {
		if b.Type != ir.BlockText {
			return false
		}
	}
	return true
}

// EncodeRequest renders IR as a completions request. Conversion never
// routes another protocol's request here (Capabilities.PromptOnly); a
// conversation that arrives anyway is refused rather than flattened into a
// prompt. Streaming requests always ask for a usage chunk.
func (Codec) EncodeRequest(req *ir.Request) ([]byte, error) {
	if !singlePrompt(req) {
		return nil, ir.Errorf(ir.ErrCapabilityMissing, "openai-completions takes a single text prompt, not a conversation")
	}
	var prompt any = promptText(req)
	if has(&req.Extra, xPromptArr) {
		prompt = []any{prompt}
	}
	o := object{{"model", req.Model}, {"prompt", prompt}}
	if v, ok := req.Extra.Get(Name, "suffix"); ok {
		o.add("suffix", v)
	}
	if req.MaxOutputTokens != nil {
		o.add("max_tokens", *req.MaxOutputTokens)
	}
	if req.Temperature != nil {
		o.add("temperature", *req.Temperature)
	}
	if req.TopP != nil {
		o.add("top_p", *req.TopP)
	}
	if req.TopK != nil {
		o.add("top_k", *req.TopK)
	}
	if len(req.Stop) == 1 && has(&req.Extra, xStopStr) {
		o.add("stop", req.Stop[0])
	} else if len(req.Stop) > 0 {
		o.add("stop", req.Stop)
	}
	if req.Stream {
		o.add("stream", true)
		so := object{}
		if v, ok := req.Extra.Get(Name, "stream_options"); ok {
			mergeRaw(&so, v)
		}
		for i := range so {
			if so[i].k == "include_usage" {
				so = append(so[:i], so[i+1:]...)
				break
			}
		}
		so = append(object{{"include_usage", true}}, so...)
		o.add("stream_options", so)
	}
	restore(&o, req.Extra, "stream_options")
	return marshal(o)
}
