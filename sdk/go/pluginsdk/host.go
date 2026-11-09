package pluginsdk

import (
	"context"
	"time"
)

// Host calls back into plugrelay. Each API except Log must be listed under
// permissions.host in plugin.yaml.
type Host struct{ p *Plugin }

// Log writes to the host log. Level is debug, info, warn or error.
func (h *Host) Log(ctx context.Context, level, message string, attrs map[string]any) error {
	return h.p.request(ctx, "host.log", map[string]any{"level": level, "message": message, "attrs": attrs}, nil)
}

// KVGet reads a plugin-scoped key.
func (h *Host) KVGet(ctx context.Context, key string) (value string, found bool, err error) {
	var res struct {
		Found bool   `json:"found"`
		Value string `json:"value"`
	}
	err = h.p.request(ctx, "host.kv.get", map[string]any{"key": key}, &res)
	return res.Value, res.Found, err
}

// KVPut writes a key; ttl 0 keeps it forever.
func (h *Host) KVPut(ctx context.Context, key, value string, ttl time.Duration) error {
	return h.p.request(ctx, "host.kv.put", map[string]any{"key": key, "value": value, "ttl_ms": ttl.Milliseconds()}, nil)
}

// KVDelete removes a key.
func (h *Host) KVDelete(ctx context.Context, key string) error {
	return h.p.request(ctx, "host.kv.delete", map[string]any{"key": key}, nil)
}

// KVIncr atomically adds delta and returns the new value.
func (h *Host) KVIncr(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	var res struct {
		Value int64 `json:"value"`
	}
	err := h.p.request(ctx, "host.kv.incr", map[string]any{"key": key, "delta": delta, "ttl_ms": ttl.Milliseconds()}, &res)
	return res.Value, err
}

// HTTP sends a request through the host's egress client.
func (h *Host) HTTP(ctx context.Context, req *HTTPRequest) (*HTTPResponse, error) {
	var res HTTPResponse
	if err := h.p.request(ctx, "host.http", req, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
