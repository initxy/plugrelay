package state

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is the shared backend for multi-node deployments. Slot leases and
// lock checks run as Lua scripts and read the server clock, so node clock
// skew does not matter. Requires Redis 5 or later.
type Redis struct {
	c      redis.UniversalClient
	prefix string
}

var (
	incrScript = redis.NewScript(`
local existed = redis.call('EXISTS', KEYS[1])
local v = redis.call('INCRBY', KEYS[1], ARGV[1])
if existed == 0 and tonumber(ARGV[2]) > 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return v`)

	acquireScript = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local ttl = tonumber(ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[1]) then
  return 0
end
redis.call('ZADD', KEYS[1], now + ttl, ARGV[3])
if redis.call('PTTL', KEYS[1]) < ttl then
  redis.call('PEXPIRE', KEYS[1], ttl)
end
return 1`)

	unlockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`)
)

// OpenRedis connects to url and pings it. prefix namespaces every key
// ("plugrelay:" when empty).
func OpenRedis(ctx context.Context, url, prefix string) (*Redis, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("state: redis url: %w", err)
	}
	c := redis.NewClient(opt)
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("state: redis ping: %w", err)
	}
	if prefix == "" {
		prefix = "plugrelay:"
	}
	return &Redis{c: c, prefix: prefix}, nil
}

func (r *Redis) k(key string) string { return r.prefix + key }

// Get implements State.
func (r *Redis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	v, err := r.c.Get(ctx, r.k(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("state: redis get: %w", err)
	}
	return v, true, nil
}

// Set implements State.
func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl < 0 {
		ttl = 0
	}
	if err := r.c.Set(ctx, r.k(key), value, ttl).Err(); err != nil {
		return fmt.Errorf("state: redis set: %w", err)
	}
	return nil
}

// SetNX implements State.
func (r *Redis) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	if ttl < 0 {
		ttl = 0
	}
	ok, err := r.c.SetNX(ctx, r.k(key), value, ttl).Result()
	if err != nil {
		return false, fmt.Errorf("state: redis setnx: %w", err)
	}
	return ok, nil
}

// Delete implements State.
func (r *Redis) Delete(ctx context.Context, key string) error {
	if err := r.c.Del(ctx, r.k(key)).Err(); err != nil {
		return fmt.Errorf("state: redis del: %w", err)
	}
	return nil
}

// Incr implements State.
func (r *Redis) Incr(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	n, err := incrScript.Run(ctx, r.c, []string{r.k(key)}, delta, ttl.Milliseconds()).Int64()
	if err != nil {
		return 0, fmt.Errorf("state: redis incr: %w", err)
	}
	return n, nil
}

// Acquire implements State.
func (r *Redis) Acquire(ctx context.Context, key string, limit int, ttl time.Duration) (func(), bool, error) {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	tok := newToken()
	k := r.k("slots:" + key)
	n, err := acquireScript.Run(ctx, r.c, []string{k}, limit, ttl.Milliseconds(), tok).Int64()
	if err != nil {
		return nil, false, fmt.Errorf("state: redis acquire: %w", err)
	}
	if n == 0 {
		return nil, false, nil
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			c, cancel := releaseCtx(ctx)
			defer cancel()
			_ = r.c.ZRem(c, k, tok).Err()
		})
	}, true, nil
}

// Lock implements State.
func (r *Redis) Lock(ctx context.Context, key string, ttl time.Duration) (func(), bool, error) {
	tok := newToken()
	ok, err := r.SetNX(ctx, key, []byte(tok), ttl)
	if err != nil || !ok {
		return nil, false, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			c, cancel := releaseCtx(ctx)
			defer cancel()
			_ = unlockScript.Run(c, r.c, []string{r.k(key)}, tok).Err()
		})
	}, true, nil
}

// Close closes the client.
func (r *Redis) Close() error { return r.c.Close() }
