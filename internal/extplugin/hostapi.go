package extplugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// maxHTTPBody bounds a host.http response body.
const maxHTTPBody = 32 << 20

// handleHost serves requests the plugin sends to the host.
func (in *instance) handleHost(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "host.log":
		return in.hostLog(params)
	case "host.kv.get", "host.kv.put", "host.kv.delete", "host.kv.incr":
		if !in.man.allowed(PermKV) {
			return nil, &rpcError{Code: CodePermissionDenied, Message: "permissions.host does not include kv"}
		}
		return in.hostKV(ctx, method, params)
	case "host.http":
		if !in.man.allowed(PermHTTP) {
			return nil, &rpcError{Code: CodePermissionDenied, Message: "permissions.host does not include http"}
		}
		return in.hostHTTP(ctx, params)
	case "$/cancelRequest", "$/progress":
		return nil, nil
	}
	return nil, &rpcError{Code: CodeMethodNotFound, Message: "unknown host method " + method}
}

func invalid(err error) error { return &rpcError{Code: CodeInvalidParams, Message: err.Error()} }

func (in *instance) hostLog(params json.RawMessage) (any, error) {
	var p struct {
		Level   string         `json:"level"`
		Message string         `json:"message"`
		Attrs   map[string]any `json:"attrs"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	lvl := slog.LevelInfo
	switch p.Level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	log := in.log
	if in.host != nil && in.host.Logger() != nil {
		log = in.host.Logger()
	}
	args := make([]any, 0, 2*len(p.Attrs))
	for k, v := range p.Attrs {
		args = append(args, k, v)
	}
	log.Log(context.Background(), lvl, p.Message, args...)
	return struct{}{}, nil
}

func (in *instance) hostKV(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if in.host == nil || in.host.KV() == nil {
		return nil, &rpcError{Code: CodeHostUnavailable, Message: "host KV is not configured"}
	}
	kv := in.host.KV()
	var p struct {
		Key   string `json:"key"`
		Value string `json:"value"`
		Delta int64  `json:"delta"`
		TTLMs int64  `json:"ttl_ms"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	if p.Key == "" {
		return nil, invalid(fmt.Errorf("key is required"))
	}
	ttl := time.Duration(p.TTLMs) * time.Millisecond
	var err error
	switch method {
	case "host.kv.get":
		v, ok, gerr := kv.Get(ctx, p.Key)
		if gerr == nil {
			return map[string]any{"found": ok, "value": string(v)}, nil
		}
		err = gerr
	case "host.kv.put":
		err = kv.Put(ctx, p.Key, []byte(p.Value), ttl)
	case "host.kv.delete":
		err = kv.Delete(ctx, p.Key)
	case "host.kv.incr":
		n, ierr := kv.Incr(ctx, p.Key, p.Delta, ttl)
		if ierr == nil {
			return map[string]int64{"value": n}, nil
		}
		err = ierr
	}
	if err != nil {
		return nil, &rpcError{Code: CodeInternalError, Message: err.Error()}
	}
	return struct{}{}, nil
}

// hostHTTP sends a request through the core egress so the SSRF guard
// applies to plugins too; with an upstream (and credential) id it takes
// that credential's egress, proxy included.
func (in *instance) hostHTTP(ctx context.Context, params json.RawMessage) (any, error) {
	if in.host == nil || in.host.HTTPClient() == nil {
		return nil, &rpcError{Code: CodeHostUnavailable, Message: "host HTTP client is not configured"}
	}
	var p wireHTTPRequest
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalid(err)
	}
	if p.Method == "" {
		p.Method = http.MethodGet
	}
	body, err := decodeBody(p.Body, p.BodyBase64)
	if err != nil {
		return nil, invalid(err)
	}
	if p.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(p.TimeoutMs)*time.Millisecond)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, p.Method, p.URL, bytes.NewReader(body))
	if err != nil {
		return nil, invalid(err)
	}
	for k, vs := range p.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	client := in.host.HTTPClient()
	if p.UpstreamID != "" {
		var cred *plugin.Credential
		if p.CredentialID != "" {
			cred = &plugin.Credential{ID: p.CredentialID, UpstreamID: p.UpstreamID}
		}
		if client, err = in.host.ClientFor(&plugin.Upstream{ID: p.UpstreamID}, cred); err != nil {
			return nil, &rpcError{Code: CodeInternalError, Message: err.Error()}
		}
	} else if p.CredentialID != "" {
		return nil, invalid(errors.New("credential_id needs upstream_id"))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &rpcError{Code: CodeInternalError, Message: plugin.RedactURLError(err).Error()}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody+1))
	if err != nil {
		return nil, &rpcError{Code: CodeInternalError, Message: err.Error()}
	}
	if len(data) > maxHTTPBody {
		return nil, &rpcError{Code: CodeInternalError, Message: "response body exceeds limit"}
	}
	out := wireHTTPResponse{Status: resp.StatusCode, Header: resp.Header}
	setBody(data, &out.Body, &out.BodyBase64)
	return out, nil
}

func decodeBody(text *string, b64 string) ([]byte, error) {
	if b64 != "" {
		return base64.StdEncoding.DecodeString(b64)
	}
	if text != nil {
		return []byte(*text), nil
	}
	return nil, nil
}

func setBody(data []byte, text **string, b64 *string) {
	if len(data) == 0 {
		return
	}
	if utf8.Valid(data) {
		s := string(data)
		*text = &s
		return
	}
	*b64 = base64.StdEncoding.EncodeToString(data)
}
