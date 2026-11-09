package state

import (
	"context"
	"fmt"
	"hash/maphash"
	"strconv"
	"sync"
	"time"
)

const memShards = 64

// Memory is the in-process backend for a single node.
type Memory struct {
	seed   maphash.Seed
	shards [memShards]memShard
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
}

type memShard struct {
	mu    sync.Mutex
	kv    map[string]memEntry
	slots map[string]map[string]time.Time
}

type memEntry struct {
	v   []byte
	exp time.Time // zero = no expiry
}

func (e memEntry) live(now time.Time) bool { return e.exp.IsZero() || now.Before(e.exp) }

// NewMemory returns an in-memory State that purges expired entries every
// sweepEvery until closed.
func NewMemory(sweepEvery time.Duration) *Memory {
	m := &Memory{seed: maphash.MakeSeed(), stop: make(chan struct{}), done: make(chan struct{})}
	for i := range m.shards {
		m.shards[i].kv = map[string]memEntry{}
		m.shards[i].slots = map[string]map[string]time.Time{}
	}
	if sweepEvery <= 0 {
		sweepEvery = time.Minute
	}
	go m.janitor(sweepEvery)
	return m
}

func (m *Memory) shard(key string) *memShard {
	return &m.shards[maphash.String(m.seed, key)%memShards]
}

func expiry(now time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}

// Get implements State.
func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.kv[key]
	if !ok || !e.live(time.Now()) {
		return nil, false, nil
	}
	return append([]byte(nil), e.v...), true, nil
}

// Set implements State.
func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kv[key] = memEntry{v: append([]byte(nil), value...), exp: expiry(time.Now(), ttl)}
	return nil
}

// SetNX implements State.
func (m *Memory) SetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if e, ok := s.kv[key]; ok && e.live(now) {
		return false, nil
	}
	s.kv[key] = memEntry{v: append([]byte(nil), value...), exp: expiry(now, ttl)}
	return true, nil
}

// Delete implements State.
func (m *Memory) Delete(_ context.Context, key string) error {
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.kv, key)
	return nil
}

// Incr implements State. Counters are stored as decimal text, as in redis.
func (m *Memory) Incr(_ context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	e, ok := s.kv[key]
	var n int64
	if ok && e.live(now) {
		var err error
		if n, err = strconv.ParseInt(string(e.v), 10, 64); err != nil {
			return 0, fmt.Errorf("state: incr %s: value is not an integer", key)
		}
	} else {
		e = memEntry{exp: expiry(now, ttl)}
	}
	n += delta
	e.v = strconv.AppendInt(nil, n, 10)
	s.kv[key] = e
	return n, nil
}

// Acquire implements State.
func (m *Memory) Acquire(_ context.Context, key string, limit int, ttl time.Duration) (func(), bool, error) {
	s := m.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	held := s.slots[key]
	for tok, exp := range held {
		if !now.Before(exp) {
			delete(held, tok)
		}
	}
	if len(held) >= limit {
		return nil, false, nil
	}
	if held == nil {
		held = map[string]time.Time{}
		s.slots[key] = held
	}
	tok := newToken()
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	held[tok] = now.Add(ttl)
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if h := s.slots[key]; h != nil {
				delete(h, tok)
				if len(h) == 0 {
					delete(s.slots, key)
				}
			}
		})
	}, true, nil
}

// Lock implements State.
func (m *Memory) Lock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	tok := []byte(newToken())
	ok, err := m.SetNX(ctx, key, tok, ttl)
	if err != nil || !ok {
		return nil, false, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			s := m.shard(key)
			s.mu.Lock()
			defer s.mu.Unlock()
			if e, ok := s.kv[key]; ok && string(e.v) == string(tok) {
				delete(s.kv, key)
			}
		})
	}, true, nil
}

func (m *Memory) janitor(every time.Duration) {
	defer close(m.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.sweep(time.Now())
		}
	}
}

func (m *Memory) sweep(now time.Time) {
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.Lock()
		for k, e := range s.kv {
			if !e.live(now) {
				delete(s.kv, k)
			}
		}
		for k, held := range s.slots {
			for tok, exp := range held {
				if !now.Before(exp) {
					delete(held, tok)
				}
			}
			if len(held) == 0 {
				delete(s.slots, k)
			}
		}
		s.mu.Unlock()
	}
}

// Close stops the janitor.
func (m *Memory) Close() error {
	m.once.Do(func() { close(m.stop) })
	<-m.done
	return nil
}
