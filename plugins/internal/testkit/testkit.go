// Package testkit holds small fakes shared by the built-in plugin tests: an
// in-memory plugin.SharedState that is also a plugin.KV, a plugin.Host, a
// controllable clock and a helper to build a plugin.Call with fixed
// settings.
package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Clock is a manually advanced clock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock starts a clock at t.
func NewClock(t time.Time) *Clock { return &Clock{now: t} }

// Now returns the current fake time.
func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

type entry struct {
	v   []byte
	exp time.Time
}

// State is an in-memory plugin.SharedState (and plugin.KV) driven by a
// clock.
type State struct {
	mu    sync.Mutex
	clock func() time.Time
	kv    map[string]entry
	slots map[string]int
}

// NewState returns an empty State; clock may be nil for wall time.
func NewState(clock func() time.Time) *State {
	if clock == nil {
		clock = time.Now
	}
	return &State{clock: clock, kv: map[string]entry{}, slots: map[string]int{}}
}

func (s *State) live(k string) (entry, bool) {
	e, ok := s.kv[k]
	if !ok || (!e.exp.IsZero() && !s.clock().Before(e.exp)) {
		return entry{}, false
	}
	return e, true
}

func (s *State) exp(ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return s.clock().Add(ttl)
}

// Get implements plugin.SharedState.
func (s *State) Get(_ context.Context, key string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.live(key)
	return e.v, ok, nil
}

// Set implements plugin.SharedState.
func (s *State) Set(_ context.Context, key string, v []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kv[key] = entry{v: v, exp: s.exp(ttl)}
	return nil
}

// Put implements plugin.KV.
func (s *State) Put(ctx context.Context, key string, v []byte, ttl time.Duration) error {
	return s.Set(ctx, key, v, ttl)
}

// SetNX implements plugin.SharedState.
func (s *State) SetNX(_ context.Context, key string, v []byte, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.live(key); ok {
		return false, nil
	}
	s.kv[key] = entry{v: v, exp: s.exp(ttl)}
	return true, nil
}

// Delete implements plugin.SharedState.
func (s *State) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.kv, key)
	return nil
}

// Incr implements plugin.SharedState.
func (s *State) Incr(_ context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.live(key)
	var n int64
	if ok {
		var err error
		if n, err = strconv.ParseInt(string(e.v), 10, 64); err != nil {
			return 0, fmt.Errorf("not an integer: %s", key)
		}
	} else {
		e = entry{exp: s.exp(ttl)}
	}
	n += delta
	e.v = []byte(strconv.FormatInt(n, 10))
	s.kv[key] = e
	return n, nil
}

// Acquire implements plugin.SharedState. Slots do not expire in the fake.
func (s *State) Acquire(_ context.Context, key string, limit int, _ time.Duration) (func(), bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slots[key] >= limit {
		return nil, false, nil
	}
	s.slots[key]++
	var once sync.Once
	return func() {
		once.Do(func() { s.mu.Lock(); s.slots[key]--; s.mu.Unlock() })
	}, true, nil
}

// Held reports the slots taken under key.
func (s *State) Held(key string) int { s.mu.Lock(); defer s.mu.Unlock(); return s.slots[key] }

// Lock implements plugin.SharedState.
func (s *State) Lock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	ok, _ := s.SetNX(ctx, "lock:"+key, []byte("1"), ttl)
	if !ok {
		return nil, false, nil
	}
	return func() { _ = s.Delete(ctx, "lock:"+key) }, true, nil
}

// Settings maps setting keys to values for one plugin. A plugin.Value is
// returned as is (for merged Strictest lists); anything else is JSON-encoded.
type Settings map[string]any

// Get implements plugin.Settings.
func (s Settings) Get(key string) plugin.Value {
	v, ok := s[key]
	if !ok {
		return plugin.Value{}
	}
	if pv, ok := v.(plugin.Value); ok {
		return pv
	}
	b, _ := json.Marshal(v)
	return plugin.NewValue(b)
}

// CallOpts describes a fake request.
type CallOpts struct {
	ID       string
	Caller   *plugin.Caller
	Protocol string
	Model    string
	ClientIP string
	Header   http.Header
	Settings Settings
	Request  *ir.Request
}

// NewCall builds a Call bound to pluginID through plugin.NewCall.
func NewCall(pluginID string, o CallOpts) *plugin.Call {
	if o.ID == "" {
		o.ID = "req-1"
	}
	var req func() (*ir.Request, error)
	if o.Request != nil {
		req = func() (*ir.Request, error) { return o.Request, nil }
	}
	c := plugin.NewCall(context.Background(), o.ID, func(_, key string) plugin.Value { return o.Settings.Get(key) }, req)
	c.Caller, c.Protocol, c.Model, c.ClientIP = o.Caller, o.Protocol, o.Model, o.ClientIP
	c.Header = o.Header
	if c.Header == nil {
		c.Header = http.Header{}
	}
	return c.WithPlugin(pluginID)
}

// Host is a configurable plugin.Host.
type Host struct {
	Set   Settings
	Store *State
	Use   plugin.UsageReader
	// Public is the relay's public URL (a site setting); "" when unset.
	Public string
}

// PublicURL is the relay's public URL, as the core's host reports it.
func (h *Host) PublicURL(context.Context) string { return h.Public }

// Logger implements plugin.Host.
func (h *Host) Logger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// HTTPClient implements plugin.Host.
func (h *Host) HTTPClient() *http.Client { return http.DefaultClient }

// ClientFor implements plugin.Host with a direct client.
func (h *Host) ClientFor(*plugin.Upstream, *plugin.Credential) (*http.Client, error) {
	return http.DefaultClient, nil
}

// KV implements plugin.Host.
func (h *Host) KV() plugin.KV { return h.Store }

// Settings implements plugin.Host.
func (h *Host) Settings() plugin.Settings { return h.Set }

// Usage implements plugin.Host.
func (h *Host) Usage() plugin.UsageReader { return h.Use }

// Version implements plugin.Host.
func (h *Host) Version() string { return "test" }
