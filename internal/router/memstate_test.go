package router

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// clock is a manual test clock shared by the fake state and the router.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type memItem struct {
	v   []byte
	exp time.Time
}

// memState is a minimal state.State for tests.
type memState struct {
	mu    sync.Mutex
	clk   *clock
	items map[string]memItem
	slots map[string]int
}

func newMemState(c *clock) *memState {
	return &memState{clk: c, items: map[string]memItem{}, slots: map[string]int{}}
}

func (m *memState) live(key string) (memItem, bool) {
	it, ok := m.items[key]
	if ok && !it.exp.IsZero() && !m.clk.Now().Before(it.exp) {
		delete(m.items, key)
		return memItem{}, false
	}
	return it, ok
}

func (m *memState) exp(ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return m.clk.Now().Add(ttl)
}

func (m *memState) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.live(key)
	return it.v, ok, nil
}

func (m *memState) Set(_ context.Context, key string, v []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[key] = memItem{v: v, exp: m.exp(ttl)}
	return nil
}

func (m *memState) SetNX(_ context.Context, key string, v []byte, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.live(key); ok {
		return false, nil
	}
	m.items[key] = memItem{v: v, exp: m.exp(ttl)}
	return true, nil
}

func (m *memState) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, key)
	return nil
}

func (m *memState) Incr(_ context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.live(key)
	var n int64
	if ok {
		n, _ = strconv.ParseInt(string(it.v), 10, 64)
	} else {
		it.exp = m.exp(ttl)
	}
	n += delta
	it.v = []byte(strconv.FormatInt(n, 10))
	m.items[key] = it
	return n, nil
}

func (m *memState) Acquire(_ context.Context, key string, limit int, _ time.Duration) (func(), bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.slots[key] >= limit {
		return nil, false, nil
	}
	m.slots[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			m.slots[key]--
			m.mu.Unlock()
		})
	}, true, nil
}

func (m *memState) Lock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	ok, _ := m.SetNX(ctx, key, []byte("1"), ttl)
	if !ok {
		return nil, false, nil
	}
	return func() { _ = m.Delete(ctx, key) }, true, nil
}

func (m *memState) Close() error { return nil }
