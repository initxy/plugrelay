// Package webhook is the sink.webhook plugin of the notifications feature:
// it posts finished request records to the generic webhook channels that
// have "push records" on.
//
// Best effort, never on the request path: Consume only queues (Queue
// records; a full queue drops and counts). A worker posts what is queued
// in batches of up to Batch records, at least every Flush, as
// {"type":"records","records":[...]} signed like every webhook body
// (notify.Signature). A failed post is retried twice with backoff, then the
// batch is dropped for that channel. Each record carries the channel's
// field selection; bodies only when "body" is selected.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/job/notify"
)

// ID is the plugin id.
const ID = "sink.webhook"

func init() { plugin.Register(ID, func() plugin.Plugin { return New() }) }

// Tuning.
var (
	Queue        = 2048
	Batch        = 100
	Flush        = 2 * time.Second
	channelCache = 30 * time.Second
)

// DefaultFields are the record fields posted when a channel selects none.
var DefaultFields = []string{"id", "time", "caller", "protocol", "model", "stream", "session_id", "client_ua", "client_ip",
	"status", "error_kind", "error", "attempts", "usage", "cost_micro", "charge_micro", "ttfb", "duration", "retried"}

// Sink implements plugin.Sink.
type Sink struct {
	h   plugin.Host
	b   notify.Backend
	log *slog.Logger
	q   chan *plugin.Record

	stop context.CancelFunc
	wg   sync.WaitGroup

	mu      sync.Mutex
	chans   []notify.Channel
	fetched time.Time

	// Dropped counts records lost to a full queue or failed posts.
	Dropped atomic.Int64
}

// New returns the plugin.
func New() *Sink { return &Sink{log: slog.New(slog.DiscardHandler)} }

// Info implements plugin.Plugin.
func (s *Sink) Info() plugin.Info {
	return plugin.Info{ID: ID, Kind: plugin.KindSink, Version: "1.0.0", FailPolicy: plugin.FailOpen, ReportsOffBox: true,
		Description: "Posts finished request records to webhook channels that ask for them."}
}

// Init implements plugin.Initializer.
func (s *Sink) Init(ctx context.Context, h plugin.Host) error {
	bh, ok := h.(notify.BackendHost)
	if !ok {
		return errors.New("webhook: the host offers no notification backend")
	}
	s.h, s.b, s.log = h, bh.NotifyBackend(), h.Logger()
	s.q = make(chan *plugin.Record, Queue)
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.stop = cancel
	s.wg.Go(func() { s.run(wctx) })
	return nil
}

// Close implements plugin.Closer.
func (s *Sink) Close() error {
	if s.stop != nil {
		s.stop()
		s.wg.Wait()
	}
	return nil
}

// Consume implements plugin.Sink; it never waits.
func (s *Sink) Consume(_ context.Context, r *plugin.Record) error {
	if s.q == nil || len(s.channels(context.Background())) == 0 {
		return nil
	}
	select {
	case s.q <- r:
	default:
		s.Dropped.Add(1)
	}
	return nil
}

// channels are the enabled webhook channels with records on, cached.
func (s *Sink) channels(ctx context.Context) []notify.Channel {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.fetched) < channelCache {
		return s.chans
	}
	s.fetched = time.Now()
	all, err := s.b.Channels(ctx)
	if err != nil {
		s.log.Warn("list webhook channels", "err", err)
		return s.chans
	}
	s.chans = slices.DeleteFunc(all, func(c notify.Channel) bool {
		return !c.Enabled || c.Type != notify.TypeWebhook || !c.Records || c.URL == ""
	})
	return s.chans
}

func (s *Sink) run(ctx context.Context) {
	t := time.NewTicker(Flush)
	defer t.Stop()
	var buf []*plugin.Record
	flush := func() {
		if len(buf) > 0 {
			s.post(ctx, buf)
			buf = nil
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-s.q:
			if buf = append(buf, r); len(buf) >= Batch {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

func (s *Sink) post(ctx context.Context, recs []*plugin.Record) {
	for _, ch := range s.channels(ctx) {
		body, err := json.Marshal(map[string]any{"type": "records", "records": Select(recs, ch.RecordFields)})
		if err != nil {
			continue
		}
		_, err = notify.Retry(ctx, func(ctx context.Context) error {
			return notify.PostWebhook(ctx, s.h.HTTPClient(), ch, "records", body, time.Now())
		})
		if err != nil {
			s.Dropped.Add(int64(len(recs)))
			s.log.Warn("records not delivered to webhook", "channel", ch.Name, "records", len(recs), "err", err)
		}
	}
}

// Select keeps the chosen top-level fields of each record (DefaultFields
// when none are chosen).
func Select(recs []*plugin.Record, fields []string) []map[string]json.RawMessage {
	if len(fields) == 0 {
		fields = DefaultFields
	}
	out := make([]map[string]json.RawMessage, 0, len(recs))
	for _, r := range recs {
		raw, err := json.Marshal(r)
		if err != nil {
			continue
		}
		var all map[string]json.RawMessage
		if json.Unmarshal(raw, &all) != nil {
			continue
		}
		m := map[string]json.RawMessage{}
		for _, f := range fields {
			if v, ok := all[f]; ok {
				m[f] = v
			}
		}
		out = append(out, m)
	}
	return out
}
