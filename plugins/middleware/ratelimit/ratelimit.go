// Package ratelimit is the middleware.ratelimit plugin: requests per
// minute, tokens per minute and concurrent requests, counted per key (or
// per user or group, see the "subject" setting).
//
// Limits merge Strictest across Global → Group → Key, so a key can only be
// tightened below its group. 0 means unlimited.
//
// Counters live in the shared plugin.SharedState so every node sees the same
// numbers. The minute windows are fixed 60-second buckets; the count used
// for a decision is the current bucket plus the previous bucket weighted by
// how much of it still overlaps the trailing minute. That approximates a
// sliding window with two counters per subject.
//
// A Realtime client event checked as a request (Call.EventIndex) counts
// toward rpm and is held to tpm; the session already holds its
// concurrency slot.
//
// Tokens are counted when a request finishes (from its usage) and enforced
// when the next request starts, so one request can overshoot tpm; the next
// one is refused until the window drains.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// ID is the plugin id.
const ID = "middleware.ratelimit"

func init() { plugin.Register(ID, func() plugin.Plugin { return New(nil) }) }

const (
	window = time.Minute
	// concurrencyTTL bounds how long a crashed node can hold a slot.
	concurrencyTTL = 30 * time.Minute
)

// StateHost is implemented by a Host that shares the cluster state.
type StateHost = plugin.StateHost

// Option configures the plugin.
type Option func(*Limiter)

// WithClock overrides time.Now.
func WithClock(now func() time.Time) Option { return func(l *Limiter) { l.now = now } }

// Limiter implements RequestHook and FinishHook.
type Limiter struct {
	st  plugin.SharedState
	now func() time.Time
}

// New returns the plugin. st may be nil when it will come from the Host.
func New(st plugin.SharedState, opts ...Option) *Limiter {
	l := &Limiter{st: st, now: time.Now}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Info implements plugin.Plugin.
func (l *Limiter) Info() plugin.Info {
	lim := plugin.Global | plugin.Group | plugin.Key
	return plugin.Info{
		ID:          ID,
		Kind:        plugin.KindMiddleware,
		Version:     "1.0.0",
		Description: "Requests per minute, tokens per minute and concurrency limits.",
		FailPolicy:  plugin.FailClosed,
		Settings: []plugin.Setting{
			{Key: "rpm", Type: plugin.TypeInt, Default: 0, Scopes: lim, Merge: plugin.Strictest,
				Label: plugin.T("Requests per minute", "每分钟请求数"),
				Help:  plugin.T("0 means no limit.", "0 表示不限。")},
			{Key: "tpm", Type: plugin.TypeInt, Default: 0, Scopes: lim, Merge: plugin.Strictest,
				Label: plugin.T("Tokens per minute", "每分钟 token 数"),
				Help: plugin.T("Input (cached included) plus output tokens; 0 means no limit.",
					"输入（含缓存）加输出的 token 数；0 表示不限。")},
			{Key: "concurrency", Type: plugin.TypeInt, Default: 0, Scopes: lim, Merge: plugin.Strictest,
				Label: plugin.T("Requests at once", "同时进行的请求数"),
				Help:  plugin.T("0 means no limit.", "0 表示不限。")},
			{Key: "subject", Type: plugin.TypeString, Default: "key", Scopes: plugin.Global | plugin.Group,
				Merge: plugin.Override, Enum: []string{"key", "user", "group"},
				Label: plugin.T("Limits count per", "限额按谁算"),
				Help: plugin.T("Each key on its own, each user across their keys, or the whole group together.",
					"每个密钥各算各的、每个用户的所有密钥合起来算，或者整个分组一起算。"),
				EnumLabels: map[string]plugin.Text{"key": plugin.T("Key", "密钥"), "user": plugin.T("User", "用户"),
					"group": plugin.T("Group", "分组")}},
		},
	}
}

// Init implements plugin.Initializer.
func (l *Limiter) Init(_ context.Context, h plugin.Host) error {
	if l.st == nil {
		if sh, ok := h.(StateHost); ok {
			l.st = sh.State()
		}
	}
	if l.st == nil {
		return errors.New("ratelimit: no state backend: construct with New(state) or provide a Host implementing StateHost")
	}
	return nil
}

// OnRequest implements plugin.RequestHook.
func (l *Limiter) OnRequest(c *plugin.Call) (*plugin.Reply, error) {
	rpm, tpm, conc := c.Setting("rpm").Int(), c.Setting("tpm").Int(), c.Setting("concurrency").Int()
	subj := subject(c)
	c.Set("subject", subj)
	if rpm <= 0 && tpm <= 0 && conc <= 0 {
		return nil, nil
	}
	ctx := c.Context
	now := l.now()
	cur, frac := bucket(now)
	retry := retryAfter(now)

	if tpm > 0 {
		used, err := l.windowCount(ctx, "tpm", subj, cur, frac)
		if err != nil {
			return nil, err
		}
		if used >= tpm {
			return limited(fmt.Sprintf("token rate limit of %d per minute reached", tpm), retry), nil
		}
	}
	if rpm > 0 {
		k := key("rpm", subj, cur)
		n, err := l.st.Incr(ctx, k, 1, 2*window)
		if err != nil {
			return nil, err
		}
		prev, err := l.get(ctx, key("rpm", subj, cur-1))
		if err != nil {
			return nil, err
		}
		if weighted(n, prev, frac) > rpm {
			_, _ = l.st.Incr(ctx, k, -1, 2*window)
			return limited(fmt.Sprintf("request rate limit of %d per minute reached", rpm), retry), nil
		}
		c.Set("rpm_key", k)
	}
	if conc > 0 && c.EventIndex == 0 {
		release, ok, err := l.st.Acquire(ctx, "rl:conc:"+subj, int(conc), concurrencyTTL)
		if err != nil || !ok {
			l.undoRPM(c)
			if err != nil {
				return nil, err
			}
			return limited(fmt.Sprintf("concurrency limit of %d reached", conc), time.Second), nil
		}
		c.Set("release", release)
	}
	return nil, nil
}

// OnFinish implements plugin.FinishHook.
func (l *Limiter) OnFinish(c *plugin.Call, r *plugin.Result) {
	if v, ok := c.Get("release"); ok {
		v.(func())()
	}
	if r == nil {
		return
	}
	tokens := r.Usage.TotalInput() + r.Usage.Output
	if tokens <= 0 {
		return
	}
	subj, _ := c.Get("subject")
	s, _ := subj.(string)
	if s == "" {
		s = subject(c)
	}
	cur, _ := bucket(l.now())
	_, _ = l.st.Incr(context.WithoutCancel(c.Context), key("tpm", s, cur), tokens, 2*window)
}

func (l *Limiter) undoRPM(c *plugin.Call) {
	if v, ok := c.Get("rpm_key"); ok {
		_, _ = l.st.Incr(c.Context, v.(string), -1, 2*window)
	}
}

func (l *Limiter) windowCount(ctx context.Context, kind, subj string, cur int64, frac float64) (int64, error) {
	n, err := l.get(ctx, key(kind, subj, cur))
	if err != nil {
		return 0, err
	}
	prev, err := l.get(ctx, key(kind, subj, cur-1))
	if err != nil {
		return 0, err
	}
	return weighted(n, prev, frac), nil
}

func (l *Limiter) get(ctx context.Context, k string) (int64, error) {
	b, ok, err := l.st.Get(ctx, k)
	if err != nil || !ok {
		return 0, err
	}
	n, _ := strconv.ParseInt(string(b), 10, 64)
	return n, nil
}

// subject names the counter owner. Unauthenticated calls count per source IP.
func subject(c *plugin.Call) string {
	if c.Caller == nil {
		return "ip:" + c.ClientIP
	}
	switch c.Setting("subject").String() {
	case "user":
		if c.Caller.UserID != "" {
			return "user:" + c.Caller.UserID
		}
	case "group":
		if c.Caller.GroupID != "" {
			return "group:" + c.Caller.GroupID
		}
	}
	return "key:" + c.Caller.KeyID
}

// bucket returns the minute bucket of t and the fraction of it elapsed.
func bucket(t time.Time) (int64, float64) {
	ms := t.UnixMilli()
	w := window.Milliseconds()
	return ms / w, float64(ms%w) / float64(w)
}

func weighted(cur, prev int64, frac float64) int64 {
	return cur + int64(float64(prev)*(1-frac))
}

func retryAfter(now time.Time) time.Duration {
	_, frac := bucket(now)
	d := time.Duration((1 - frac) * float64(window)).Round(time.Second)
	if d < time.Second {
		d = time.Second
	}
	return d
}

func key(kind, subj string, b int64) string {
	return "rl:" + kind + ":" + subj + ":" + strconv.FormatInt(b, 10)
}

func limited(msg string, retry time.Duration) *plugin.Reply {
	e := ir.Errorf(ir.ErrRateLimited, "%s", msg)
	e.RetryAfter = retry
	return &plugin.Reply{Error: e}
}
