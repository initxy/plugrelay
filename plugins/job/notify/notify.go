// Package notify is the job.notify plugin of the notifications feature: it
// tells operators when something needs them (a credential suspected banned
// or paused for long, quota running low, an upstream paused and back, a
// budget reaching its threshold, a refreshed secret not saved, an
// extension crashing) through email, Feishu/Lark, DingTalk, WeCom bots or a
// generic webhook.
//
// Two ways in, one way out:
//
//   - Conditions: every run (30 s, one node at a time under the core's job
//     lock) the job asks the core what is wrong right now (Backend.Conditions,
//     read from the router's shared live state) and compares it with what was
//     wrong on the last run (kept in the plugin's KV, so a run on another node
//     continues from it). What started is announced; an upstream that is
//     fine again is announced as recovered.
//   - Notices: plugins and the core raise plugin.Notice values (the quota
//     meter on a budget, the extension supervisor on a crash) on any node.
//     Push drops repeats (locally, then cluster-wide with SetNX) and queues
//     the rest in the store as pending entries.
//
// Every announcement is rate-limited per event and object (repeat_hours).
// Each run turns everything new of one event into one message, so twenty
// credentials cooling at once are one message listing them, and delivers
// pending entries to the enabled channels that take the event, retrying a
// failed send twice with backoff. Delivery is best effort: an entry is
// delivered once, with each channel's outcome logged on it.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// ID is the plugin id.
const ID = "job.notify"

func init() { plugin.Register(ID, func() plugin.Plugin { return New() }) }

// Events.
const (
	EventQuarantined   = "credential_quarantined"
	EventCooling       = "credential_cooling"
	EventQuotaLow      = "credential_quota_low"
	EventSecretUnsaved = "credential_secret_unsaved"
	EventPaused        = "upstream_paused"
	EventRecovered     = "upstream_recovered"
	EventBudget        = "budget_reached"
	EventCrashed       = "extension_crashed"
	EventTest          = "test"
)

// Events lists the events a channel can subscribe to, in display order.
var Events = []string{EventQuarantined, EventCooling, EventQuotaLow, EventSecretUnsaved, EventPaused, EventRecovered,
	EventBudget, EventCrashed}

// Channel types.
const (
	TypeEmail    = "email"
	TypeFeishu   = "feishu"
	TypeDingTalk = "dingtalk"
	TypeWeCom    = "wecom"
	TypeWebhook  = "webhook"
)

// Channel is where messages go; URL, Password and Secret are opened.
type Channel struct {
	ID, Name, Type string
	Enabled        bool
	// Events the channel takes; empty is every event.
	Events   []string
	Language string // "zh-CN" or "en"

	SMTPHost string
	SMTPPort int
	TLS      string // "tls", "starttls" or "none"
	Username string
	Password string
	From     string
	To       []string

	URL    string
	Secret string

	Records      bool
	RecordFields []string
}

// Takes reports whether the channel receives event.
func (c Channel) Takes(event string) bool {
	return c.Enabled && (len(c.Events) == 0 || slices.Contains(c.Events, event))
}

// Object is what an entry is about, named for people.
type Object struct {
	Type       string            `json:"type"` // credential, upstream, key, user, group, extension, channel
	ID         string            `json:"id"`
	Name       string            `json:"name,omitempty"`
	ParentID   string            `json:"parent_id,omitempty"` // a credential's upstream
	ParentName string            `json:"parent_name,omitempty"`
	Detail     map[string]string `json:"detail,omitempty"`
}

// Condition is something wrong right now.
type Condition struct {
	Event  string `json:"event"`
	Object Object `json:"object"`
}

func (c Condition) key() string { return c.Event + "|" + c.Object.Type + "|" + c.Object.ID }

// Entry is one message of the log.
type Entry struct {
	ID         string
	At         time.Time
	Event      string
	Objects    []Object
	Deliveries []Delivery
	Done       bool
}

// Delivery is the outcome of sending an entry to one channel.
type Delivery struct {
	ChannelID, ChannelName, ChannelType string
	OK                                  bool
	Error                               string
	Attempts                            int
	At                                  time.Time
}

// Backend is what the job needs from the core.
type Backend interface {
	// Conditions lists what is wrong right now, with raw figures in Detail:
	// "reason", "since", "until" (RFC 3339), "percent".
	Conditions(ctx context.Context) ([]Condition, error)
	// Channels lists every channel, secrets opened.
	Channels(ctx context.Context) ([]Channel, error)
	// Add appends an entry to the log; a Done entry is final.
	Add(ctx context.Context, e *Entry) error
	Pending(ctx context.Context) ([]Entry, error)
	// Finish stores an entry's objects and deliveries and marks it done.
	Finish(ctx context.Context, e *Entry) error
	Drop(ctx context.Context, id string) error
	// Describe names the subject of a notice.
	Describe(ctx context.Context, o plugin.NoticeObject) Object
	// PublicURL is the console's public address; "" when unset.
	PublicURL(ctx context.Context) string
	// Timezone is the site time zone messages show times in.
	Timezone(ctx context.Context) *time.Location
	// Dial connects through the core egress (SSRF guard), for SMTP.
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
}

// BackendHost is implemented by a Host that offers the Backend.
type BackendHost interface {
	NotifyBackend() Backend
}

const (
	every     = 30 * time.Second
	kvActive  = "active"
	queueSize = 256
	// maxObjects bounds the objects stored on one entry.
	maxObjects = 100
	// budgetRepeat keeps a budget threshold from repeating within its
	// period (the period is part of the notice key).
	budgetRepeat = 35 * 24 * time.Hour
)

// Notifier implements plugin.Job.
type Notifier struct {
	h     plugin.Host
	b     Backend
	st    plugin.SharedState
	log   *slog.Logger
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	mu   sync.Mutex
	seen map[string]time.Time // notice keys pushed recently from this node
	q    chan plugin.Notice
	stop context.CancelFunc
	wg   sync.WaitGroup
}

// New returns the plugin.
func New() *Notifier {
	return &Notifier{now: time.Now, sleep: sleepCtx, seen: map[string]time.Time{}, log: slog.New(slog.DiscardHandler)}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func intSetting(key string, def int, label, help plugin.Text) plugin.Setting {
	return plugin.Setting{Key: key, Type: plugin.TypeInt, Default: def, Scopes: plugin.Global, Merge: plugin.Override,
		Label: label, Help: help}
}

// Info implements plugin.Plugin.
func (n *Notifier) Info() plugin.Info {
	return plugin.Info{
		ID:            ID,
		Kind:          plugin.KindJob,
		Version:       "1.0.0",
		Description:   "Sends notifications about credentials, upstreams, budgets and extensions to email, chat bots and webhooks.",
		FailPolicy:    plugin.FailOpen,
		ReportsOffBox: true,
		Settings: []plugin.Setting{
			intSetting("cooling_minutes", 30, plugin.T("Report a paused credential after (minutes)", "凭据暂停多久才提醒（分钟）"),
				plugin.T("A credential paused at least this long is reported; one paused because its quota ran out always is.",
					"凭据暂停时长达到这个值才提醒；因为额度用完而暂停的总会提醒。")),
			intSetting("quota_low_percent", 10, plugin.T("Report remaining quota at or below (%)", "剩余额度低于多少提醒（%）"),
				plugin.T("From the upstream's own quota reading. 0 turns this off.", "按上游自己报告的剩余额度算。0 表示不提醒。")),
			intSetting("budget_percent", 80, plugin.T("Report budgets used from (%)", "额度用到多少提醒（%）"),
				plugin.T("Spend and token budgets of keys, users and groups; at least 50. Using one up is always reported.",
					"适用于密钥、用户和分组的花费与 token 上限，最低 50。用完时总会提醒。")),
			intSetting("repeat_hours", 6, plugin.T("Repeat the same alert at most every (hours)", "同一件事多久内只提醒一次（小时）"),
				plugin.T("For the same thing about the same object; similar alerts at once are sent as one message.",
					"同一对象的同一件事；同时发生的同类提醒会合成一条消息。")),
		},
	}
}

// Init implements plugin.Initializer.
func (n *Notifier) Init(ctx context.Context, h plugin.Host) error {
	bh, ok := h.(BackendHost)
	if !ok {
		return errors.New("notify: the host offers no notification backend")
	}
	sh, ok := h.(plugin.StateHost)
	if !ok {
		return errors.New("notify: the host offers no shared state")
	}
	n.h, n.b, n.st, n.log = h, bh.NotifyBackend(), sh.State(), h.Logger()
	n.q = make(chan plugin.Notice, queueSize)
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	n.stop = cancel
	n.wg.Go(func() { n.drain(wctx) })
	return nil
}

// Close implements plugin.Closer.
func (n *Notifier) Close() error {
	if n.stop != nil {
		n.stop()
		n.wg.Wait()
	}
	return nil
}

// Every implements plugin.Job.
func (n *Notifier) Every() time.Duration { return every }

func (n *Notifier) setting(key string, def int64) int64 {
	if n.h == nil {
		return def
	}
	v := n.h.Settings().Get(key)
	if !v.IsSet() {
		return def
	}
	return v.Int()
}

func (n *Notifier) repeat() time.Duration {
	return time.Duration(max(n.setting("repeat_hours", 6), 0)) * time.Hour
}

// Push takes a notice raised anywhere. It never blocks: a notice is
// dropped when it repeats one this node pushed lately, when it is below
// its threshold, or when the queue is full.
func (n *Notifier) Push(_ context.Context, nt plugin.Notice) {
	if n.q == nil {
		return
	}
	ttl := n.repeat()
	if nt.Event == EventBudget {
		pct, _ := strconv.Atoi(nt.Detail["percent"])
		threshold := int(max(n.setting("budget_percent", 80), 50))
		switch {
		case pct >= 100:
			threshold = 100
		case pct < threshold:
			return
		}
		nt.Key += ":" + strconv.Itoa(threshold)
		ttl = budgetRepeat
	}
	key := nt.Event + "|" + nt.Object.Type + "|" + nt.Object.ID + "|" + nt.Key
	now := n.now()
	n.mu.Lock()
	if until, ok := n.seen[key]; ok && now.Before(until) {
		n.mu.Unlock()
		return
	}
	n.seen[key] = now.Add(min(ttl, time.Hour))
	if len(n.seen) > 10000 {
		for k, until := range n.seen {
			if !now.Before(until) {
				delete(n.seen, k)
			}
		}
	}
	n.mu.Unlock()
	nt.Key = key
	if nt.Detail == nil {
		nt.Detail = map[string]string{}
	}
	nt.Detail["ttl"] = ttl.String()
	select {
	case n.q <- nt:
	default:
		n.log.Warn("notification queue full; notice dropped", "event", nt.Event)
	}
}

// drain stores pushed notices that no node stored lately.
func (n *Notifier) drain(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case nt := <-n.q:
			ttl, _ := time.ParseDuration(nt.Detail["ttl"])
			delete(nt.Detail, "ttl")
			if ok, err := n.first(ctx, "push:"+nt.Key, ttl); err != nil || !ok {
				continue
			}
			o := n.b.Describe(ctx, nt.Object)
			o.Detail = nt.Detail
			if err := n.b.Add(ctx, &Entry{At: n.now(), Event: nt.Event, Objects: []Object{o}}); err != nil {
				n.log.Warn("store notification", "event", nt.Event, "err", err)
			}
		}
	}
}

// first reports whether key was not claimed within ttl, claiming it.
func (n *Notifier) first(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return true, nil
	}
	return n.st.SetNX(ctx, "rl:"+key, []byte("1"), ttl)
}

// Run implements plugin.Job: announce what started or recovered, then
// deliver what is pending.
func (n *Notifier) Run(ctx context.Context) error {
	errs := []error{n.watch(ctx)}
	errs = append(errs, n.deliver(ctx))
	return errors.Join(errs...)
}

// watch compares the conditions with the last run's and queues an entry
// per event for what started (and for upstreams that recovered).
func (n *Notifier) watch(ctx context.Context) error {
	conds, err := n.b.Conditions(ctx)
	if err != nil {
		return err
	}
	cur := map[string]Condition{}
	for _, c := range conds {
		if n.relevant(c) {
			cur[c.key()] = c
		}
	}
	prev := map[string]Condition{}
	stored, ok, err := n.h.KV().Get(ctx, kvActive)
	if err != nil {
		return err
	}
	if ok {
		_ = json.Unmarshal(stored, &prev)
	}
	var fresh []Condition
	for k, c := range cur {
		if _, was := prev[k]; !was {
			fresh = append(fresh, c)
		}
	}
	for k, c := range prev {
		if _, still := cur[k]; !still && c.Event == EventPaused {
			fresh = append(fresh, Condition{Event: EventRecovered, Object: Object{Type: c.Object.Type, ID: c.Object.ID,
				Name: c.Object.Name}})
		}
	}
	slices.SortFunc(fresh, func(a, b Condition) int {
		if a.Event != b.Event {
			return strings.Compare(a.Event, b.Event)
		}
		return strings.Compare(a.Object.Name+a.Object.ID, b.Object.Name+b.Object.ID)
	})
	byEvent := map[string][]Object{}
	var order []string
	for _, c := range fresh {
		if ok, err := n.first(ctx, c.key(), n.repeat()); err != nil {
			return err
		} else if !ok {
			continue
		}
		if _, seen := byEvent[c.Event]; !seen {
			order = append(order, c.Event)
		}
		byEvent[c.Event] = append(byEvent[c.Event], c.Object)
	}
	for _, ev := range order {
		objs := byEvent[ev]
		if len(objs) > maxObjects {
			objs = objs[:maxObjects]
		}
		if err := n.b.Add(ctx, &Entry{At: n.now(), Event: ev, Objects: objs}); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(cur)
	if err != nil || (ok && string(raw) == string(stored)) {
		return err
	}
	return n.h.KV().Put(ctx, kvActive, raw, 0)
}

// relevant applies the thresholds of the settings to a condition.
func (n *Notifier) relevant(c Condition) bool {
	d := c.Object.Detail
	switch c.Event {
	case EventCooling:
		if d["reason"] == string(plugin.CauseQuota) {
			return true
		}
		since, err1 := time.Parse(time.RFC3339, d["since"])
		until, err2 := time.Parse(time.RFC3339, d["until"])
		if err1 != nil || err2 != nil {
			return false
		}
		return until.Sub(since) >= time.Duration(max(n.setting("cooling_minutes", 30), 1))*time.Minute
	case EventQuotaLow:
		limit := n.setting("quota_low_percent", 10)
		pct, err := strconv.ParseFloat(d["percent"], 64)
		return limit > 0 && err == nil && pct <= float64(limit)
	}
	return true
}

// deliver sends every pending entry; entries of one event are merged first.
func (n *Notifier) deliver(ctx context.Context) error {
	pending, err := n.b.Pending(ctx)
	if err != nil || len(pending) == 0 {
		return err
	}
	chans, err := n.b.Channels(ctx)
	if err != nil {
		return err
	}
	base, loc := n.b.PublicURL(ctx), n.b.Timezone(ctx)
	var merged []*Entry
	byEvent := map[string]*Entry{}
	for i := range pending {
		e := &pending[i]
		if m, ok := byEvent[e.Event]; ok && e.Event != EventTest {
			m.Objects = append(m.Objects, e.Objects...)
			if err := n.b.Drop(ctx, e.ID); err != nil {
				return err
			}
			continue
		}
		byEvent[e.Event] = e
		merged = append(merged, e)
	}
	var errs []error
	for _, e := range merged {
		if len(e.Objects) > maxObjects {
			e.Objects = e.Objects[:maxObjects]
		}
		e.Deliveries = []Delivery{}
		for _, ch := range chans {
			if ch.Takes(e.Event) {
				e.Deliveries = append(e.Deliveries, n.send(ctx, ch, Render(e, ch.Language, base, loc)))
			}
		}
		e.Done = true
		errs = append(errs, n.b.Finish(ctx, e))
	}
	return errors.Join(errs...)
}

// Test sends a test message to ch and logs it.
func (n *Notifier) Test(ctx context.Context, ch Channel) Delivery {
	e := &Entry{At: n.now(), Event: EventTest, Objects: []Object{{Type: "channel", ID: ch.ID, Name: ch.Name}}}
	d := n.send(ctx, ch, Render(e, ch.Language, n.b.PublicURL(ctx), n.b.Timezone(ctx)))
	e.Deliveries, e.Done = []Delivery{d}, true
	if err := n.b.Add(ctx, e); err != nil {
		n.log.Warn("log test notification", "err", err)
	}
	return d
}
