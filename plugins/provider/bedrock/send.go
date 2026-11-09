package bedrock

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/pkg/protocol/anthropic"
	"github.com/initxy/plugrelay/pkg/sse"
	"github.com/initxy/plugrelay/plugins/provider/internal/eventstream"
	"github.com/tidwall/gjson"
)

const maxErrorBody = 64 << 10

// MaxMessageSize bounds one event stream message, matching the core's SSE
// event limit (a chunk carries one base64-encoded Anthropic event).
const MaxMessageSize = sse.DefaultMaxEventSize

var codec = anthropic.Codec{}

// Send implements plugin.Sender. It sends the request Prepare built and
// turns the answer into IR events. A failure before any event is an
// Outcome the core classifies; after the first event it ends the stream
// with EvError.
func (p *Provider) Send(ctx context.Context, o *plugin.Outgoing, _ *ir.Request, emit func(*ir.Event) error) (*plugin.Outcome, error) {
	outcome := func(oc plugin.Outcome) *plugin.Outcome {
		oc.Upstream, oc.Endpoint, oc.Credential = o.Upstream, o.Endpoint, o.Credential
		return &oc
	}
	client, err := p.AttemptClient(o)
	if err != nil {
		return outcome(plugin.Outcome{Err: err}), nil
	}
	var wrote atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wrote.Store(true) }}
	resp, err := client.Do(o.Req.WithContext(httptrace.WithClientTrace(ctx, trace)))
	if err != nil {
		return outcome(plugin.Outcome{Err: plugin.RedactURLError(err), Sent: wrote.Load()}), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return outcome(plugin.Outcome{Status: resp.StatusCode, Header: resp.Header, Body: body,
			Signal: DecodeError(resp.StatusCode, resp.Header, body), Sent: true}), nil
	}
	s := &sender{emit: emit}
	if strings.HasSuffix(o.Req.URL.Path, "/invoke-with-response-stream") {
		err = s.stream(resp)
	} else {
		err = s.full(resp)
	}
	if s.failure != nil {
		s.failure.Header = mergeHeader(resp.Header, s.failure.Header)
		return outcome(*s.failure), nil
	}
	return nil, err
}

// sender emits the events of one response.
type sender struct {
	emit    func(*ir.Event) error
	emitted bool
	// failure is set when the upstream failed before any event.
	failure *plugin.Outcome
}

func (s *sender) send(evs ...ir.Event) error {
	for i := range evs {
		s.emitted = true
		if err := s.emit(&evs[i]); err != nil {
			return err
		}
	}
	return nil
}

// fail reports a failure: as an Outcome when nothing was emitted, else as
// the stream's closing EvError.
func (s *sender) fail(status int, code string, body []byte, gw *ir.GwError) error {
	if !s.emitted {
		h := http.Header{}
		if code != "" {
			h.Set("X-Amzn-Errortype", code)
		}
		s.failure = &plugin.Outcome{Status: status, Header: h, Body: body, Signal: gw, Sent: true}
		return nil
	}
	return s.send(ir.Event{Kind: ir.EvError, Err: gw})
}

func (s *sender) full(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxMessageSize+1))
	if err == nil && len(body) > MaxMessageSize {
		err = fmt.Errorf("bedrock: response exceeds %d bytes", MaxMessageSize)
	}
	if err != nil {
		s.failure = &plugin.Outcome{Status: 0, Err: err, Sent: true}
		return nil
	}
	r, err := codec.DecodeResponse(body)
	if err != nil {
		var gw *ir.GwError
		if !errors.As(err, &gw) {
			gw = &ir.GwError{Kind: ir.ErrUpstream, Message: err.Error()}
		}
		return s.fail(http.StatusBadGateway, "", body, gw)
	}
	return s.send(ResponseEvents(r)...)
}

// ResponseEvents renders a complete response as the event sequence of a
// stream: start, each block whole, done.
func ResponseEvents(r *ir.Response) []ir.Event {
	u := r.Usage
	evs := []ir.Event{{Kind: ir.EvStart, ID: r.ID, Model: r.Model, Usage: &u}}
	for i := range r.Content {
		b := r.Content[i]
		evs = append(evs, ir.Event{Kind: ir.EvBlockStart, Index: i, Block: &b}, ir.Event{Kind: ir.EvBlockStop, Index: i})
	}
	return append(evs, ir.Event{Kind: ir.EvDone, StopReason: r.StopReason, StopSeq: r.StopSeq, Usage: &u})
}

// stream decodes event stream messages. Each "chunk" event's payload is
// {"bytes": base64(<Anthropic stream event JSON>)}; exceptions arrive as
// messages of type "exception" named by :exception-type.
func (s *sender) stream(resp *http.Response) error {
	dec := eventstream.NewDecoder(resp.Body, MaxMessageSize)
	sd := codec.NewStreamDecoder()
	for {
		msg, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return s.fail(0, "", nil, &ir.GwError{Kind: ir.ErrUpstream, Message: "bedrock stream: " + err.Error()})
		}
		switch msg.Header(":message-type") {
		case "event":
			if msg.Header(":event-type") != "chunk" {
				continue
			}
			ev, err := chunkEvent(msg.Payload)
			if err != nil {
				return s.fail(0, "", msg.Payload, &ir.GwError{Kind: ir.ErrUpstream, Message: err.Error()})
			}
			evs, err := sd.Feed(ev)
			if err != nil {
				return s.fail(0, "", msg.Payload, &ir.GwError{Kind: ir.ErrUpstream, Message: err.Error()})
			}
			if err := s.send(evs...); err != nil {
				return err
			}
			if len(evs) > 0 && (evs[len(evs)-1].Kind == ir.EvDone || evs[len(evs)-1].Kind == ir.EvError) {
				return nil
			}
		case "exception":
			code := exceptionCode(msg.Header(":exception-type"))
			status := exceptionStatus(code)
			return s.fail(status, code, msg.Payload, DecodeError(status, codeHeader(code), msg.Payload))
		case "error":
			code := msg.Header(":error-code")
			status := exceptionStatus(code)
			body := []byte(fmt.Sprintf(`{"message":%q}`, msg.Header(":error-message")))
			return s.fail(status, code, body, DecodeError(status, codeHeader(code), body))
		}
	}
	evs, complete := sd.Finish()
	if err := s.send(evs...); err != nil {
		return err
	}
	if !complete {
		return s.fail(0, "", nil, &ir.GwError{Kind: ir.ErrUpstream, Message: "bedrock stream ended before message_stop"})
	}
	return nil
}

func chunkEvent(payload []byte) (sse.Event, error) {
	b64 := gjson.GetBytes(payload, "bytes")
	if b64.Type != gjson.String {
		return sse.Event{}, errors.New("bedrock stream: chunk without bytes")
	}
	data, err := base64.StdEncoding.DecodeString(b64.Str)
	if err != nil {
		return sse.Event{}, fmt.Errorf("bedrock stream: chunk bytes: %w", err)
	}
	return sse.Event{Name: gjson.GetBytes(data, "type").String(), Data: data}, nil
}

// exceptionCode turns ":exception-type" (throttlingException) into the
// error type of the HTTP API (ThrottlingException).
func exceptionCode(t string) string {
	if t == "" {
		return "InternalServerException"
	}
	return strings.ToUpper(t[:1]) + t[1:]
}

// exceptionStatus is the HTTP status Bedrock documents for an error type.
func exceptionStatus(code string) int {
	switch strings.ToLower(code) {
	case "throttlingexception":
		return http.StatusTooManyRequests
	case "serviceunavailableexception":
		return http.StatusServiceUnavailable
	case "validationexception":
		return http.StatusBadRequest
	case "modeltimeoutexception":
		return http.StatusRequestTimeout
	case "modelstreamerrorexception", "modelerrorexception":
		return http.StatusFailedDependency
	case "accessdeniedexception":
		return http.StatusForbidden
	case "resourcenotfoundexception":
		return http.StatusNotFound
	case "modelnotreadyexception":
		return http.StatusTooManyRequests
	}
	return http.StatusInternalServerError
}

func codeHeader(code string) http.Header { return http.Header{"X-Amzn-Errortype": {code}} }

func mergeHeader(base, over http.Header) http.Header {
	out := base.Clone()
	if out == nil {
		out = http.Header{}
	}
	for k, vs := range over {
		out[k] = vs
	}
	return out
}

// DecodeError reads a Bedrock error: the type from X-Amzn-ErrorType
// ("ThrottlingException:http://internal.amazon.com/..."), the message from
// the JSON body. Code is the bare type, which manifest error rules match.
func DecodeError(status int, h http.Header, body []byte) *ir.GwError {
	code, _, _ := strings.Cut(h.Get("X-Amzn-Errortype"), ":")
	msg := ""
	for _, k := range []string{"message", "Message", "error.message"} {
		if r := gjson.GetBytes(body, k); r.Type == gjson.String && r.Str != "" {
			msg = r.Str
			break
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	kind := ir.ErrUpstream
	switch strings.ToLower(code) {
	case "throttlingexception":
		kind = ir.ErrRateLimited
	case "servicequotaexceededexception":
		kind = ir.ErrRateLimited
	case "validationexception":
		kind = ir.ErrInvalidRequest
		switch l := strings.ToLower(msg); {
		case strings.Contains(l, "too long") || strings.Contains(l, "too many tokens"):
			kind = ir.ErrContextTooLong
		case strings.Contains(l, "content filtering"):
			kind = ir.ErrContentFiltered
		}
	case "accessdeniedexception":
		kind = ir.ErrPermission
	case "unrecognizedclientexception", "invalidsignatureexception", "expiredtokenexception",
		"incompletesignatureexception", "missingauthenticationtokenexception":
		kind = ir.ErrAuthentication
	case "resourcenotfoundexception":
		kind = ir.ErrNotFound
	case "modeltimeoutexception":
		kind = ir.ErrTimeout
	case "modelnotreadyexception", "serviceunavailableexception":
		kind = ir.ErrOverloaded
	}
	if code == "" {
		switch status {
		case http.StatusTooManyRequests:
			kind = ir.ErrRateLimited
		case http.StatusBadRequest:
			kind = ir.ErrInvalidRequest
		case http.StatusUnauthorized:
			kind = ir.ErrAuthentication
		case http.StatusForbidden:
			kind = ir.ErrPermission
		case http.StatusNotFound:
			kind = ir.ErrNotFound
		}
	}
	return &ir.GwError{Kind: kind, Message: msg, Code: code, Upstream: body}
}
