// Package requestlog is the sink.requestlog plugin, which stores the
// request log users and admins browse in the console, and its companion
// job.requestlog-retention, which deletes old entries.
//
// Request and response text is never stored unless body_logging is on
// (hard rule 22: content is off by default). body_logging merges Strictest,
// so a group can switch it off under a global "on" but never on under a
// global "off".
//
// Wiring: New(writer) / NewRetention(purger), or let the registered
// factories take them in Init from a Host implementing WriterHost /
// PurgerHost. A Host implementing CallerSettings lets body_logging be
// resolved per group; otherwise the global value applies.
package requestlog

import (
	"context"
	"errors"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// IDs of the two plugins in this package.
const (
	ID          = "sink.requestlog"
	RetentionID = "job.requestlog-retention"
)

func init() {
	plugin.Register(ID, func() plugin.Plugin { return New(nil) })
	plugin.Register(RetentionID, func() plugin.Plugin { return NewRetention(nil) })
}

// RecordWriter persists one request record.
type RecordWriter interface {
	InsertRecord(ctx context.Context, r *plugin.Record) error
}

// Purger deletes request records older than before.
type Purger interface {
	Purge(ctx context.Context, before time.Time) error
}

// WriterHost is implemented by a Host that hands the sink its writer.
type WriterHost interface {
	RecordWriter() RecordWriter
}

// PurgerHost is implemented by a Host that hands the job its purger.
type PurgerHost interface {
	RecordPurger() Purger
}

// CallerSettings is implemented by a Host that can merge a plugin's
// settings for a given caller (Global → Group → User → Key).
type CallerSettings interface {
	SettingsFor(pluginID string, c plugin.Caller) plugin.Settings
}

// Sink implements plugin.Sink.
type Sink struct {
	w        RecordWriter
	global   plugin.Settings
	byCaller CallerSettings
}

// New returns the sink. w may be nil when it will come from the Host.
func New(w RecordWriter) *Sink { return &Sink{w: w} }

// Info implements plugin.Plugin.
func (s *Sink) Info() plugin.Info {
	return plugin.Info{
		ID:          ID,
		Kind:        plugin.KindSink,
		Version:     "1.0.0",
		Description: "Request log: upstream, model, tier, diagnostics, raw usage and error verdict per request.",
		// Closed: these records are the billing ledger, so the core never
		// drops them when its sink queue is full.
		FailPolicy: plugin.FailClosed,
		Settings: []plugin.Setting{
			{Key: "body_logging", Type: plugin.TypeBool, Default: false, Scopes: plugin.Global | plugin.Group,
				Merge: plugin.Strictest, Label: plugin.T("Keep request and reply text", "保存请求和回复的正文"),
				Help: plugin.T("Off by default; only token counts, cost and timing are kept. A group can turn it off for itself but not on.", "默认不保存，只记 token 数、费用和耗时。分组可以单独关掉，但不能在这里关着时自己打开。")},
		},
	}
}

// Init implements plugin.Initializer.
func (s *Sink) Init(_ context.Context, h plugin.Host) error {
	if s.w == nil {
		if wh, ok := h.(WriterHost); ok {
			s.w = wh.RecordWriter()
		}
	}
	if s.w == nil {
		return errors.New("requestlog: no RecordWriter: construct with New(w) or provide a Host implementing WriterHost")
	}
	s.global = h.Settings()
	s.byCaller, _ = h.(CallerSettings)
	return nil
}

// Consume implements plugin.Sink. The record passed in is not modified.
func (s *Sink) Consume(ctx context.Context, r *plugin.Record) error {
	if r.Body != nil && !s.bodyLogging(r) {
		cp := *r
		cp.Body = nil
		r = &cp
	}
	return s.w.InsertRecord(ctx, r)
}

func (s *Sink) bodyLogging(r *plugin.Record) bool {
	var set plugin.Settings = s.global
	if s.byCaller != nil {
		set = s.byCaller.SettingsFor(ID, r.Caller)
	}
	return set != nil && set.Get("body_logging").Bool()
}

// Retention implements plugin.Job.
type Retention struct {
	p   Purger
	set plugin.Settings
	now func() time.Time
}

// NewRetention returns the retention job. p may be nil when it will come
// from the Host.
func NewRetention(p Purger) *Retention { return &Retention{p: p, now: time.Now} }

// DefaultRetentionDays applies when retention_days is unset: keep everything.
const DefaultRetentionDays = 0

// Info implements plugin.Plugin.
func (j *Retention) Info() plugin.Info {
	return plugin.Info{
		ID:          RetentionID,
		Kind:        plugin.KindJob,
		Version:     "1.0.0",
		Description: "Deletes request log entries older than the retention period.",
		FailPolicy:  plugin.FailOpen,
		Settings: []plugin.Setting{
			{Key: "retention_days", Type: plugin.TypeInt, Default: DefaultRetentionDays, Scopes: plugin.Global,
				Merge: plugin.Override, Label: plugin.T("Keep the request log for (days)", "请求日志保留天数"),
				Help: plugin.T("Older entries are deleted; the usage totals stay. 0 keeps everything.", "更早的记录会被删除，用量统计保留；0 表示一直保留。")},
		},
	}
}

// Init implements plugin.Initializer.
func (j *Retention) Init(_ context.Context, h plugin.Host) error {
	if j.p == nil {
		if ph, ok := h.(PurgerHost); ok {
			j.p = ph.RecordPurger()
		}
	}
	if j.p == nil {
		return errors.New("requestlog: no Purger: construct with NewRetention(p) or provide a Host implementing PurgerHost")
	}
	j.set = h.Settings()
	return nil
}

// Every implements plugin.Job.
func (j *Retention) Every() time.Duration { return 6 * time.Hour }

// Run implements plugin.Job.
func (j *Retention) Run(ctx context.Context) error {
	days := int64(DefaultRetentionDays)
	if j.set != nil {
		if v := j.set.Get("retention_days"); v.IsSet() {
			days = v.Int()
		}
	}
	if days <= 0 {
		return nil
	}
	return j.p.Purge(ctx, j.now().AddDate(0, 0, -int(days)))
}
