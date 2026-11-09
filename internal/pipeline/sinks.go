package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// SinkQueue delivers records to sinks off the request path, in lanes that
// each have their own worker:
//
//   - Every sink whose FailPolicy is FailClosed gets a ledger lane of its
//     own. Their records are the billing ledger (budgets are summed from
//     them), so a record is never dropped: when a sink's lane is full,
//     Enqueue waits for room up to SinkQueueOptions.LedgerWait and then
//     delivers the record to that sink itself, in the calling goroutine. A
//     failed delivery is retried with backoff, so ledger sinks must treat a
//     repeated Record.ID as already stored. One slow or broken ledger sink
//     never holds up another.
//   - One best-effort lane feeds every other sink (logs, metrics, traces).
//     When it is full, records are dropped and counted, never waited for.
//
// The sinks a record goes to are fixed when it is enqueued. Enqueue is safe
// after Close: ledger records are then delivered synchronously and
// best-effort records are dropped.
type SinkQueue struct {
	sinks func() []plugin.Sink
	o     SinkQueueOptions
	best  *lane

	// ctx bounds every delivery; Shutdown cancels it when its deadline
	// passes, which stops retries and abandons what is still queued.
	ctx     context.Context
	abandon context.CancelFunc

	// mu orders Enqueue against Shutdown: senders hold it shared, Shutdown
	// exclusively, so no send can hit a closed channel.
	mu     sync.RWMutex
	closed bool
	// lanesMu guards ledger, the lane of each FailClosed sink by id.
	lanesMu sync.Mutex
	ledger  map[string]*lane

	dropped, waited, inline, failed, lost atomic.Int64
}

// SinkQueueOptions tunes a SinkQueue; zero fields take the defaults below.
type SinkQueueOptions struct {
	// Size is the capacity of each lane (1024).
	Size int
	// Timeout bounds one Consume call (10s).
	Timeout time.Duration
	// LedgerWait bounds how long Enqueue waits for room in a full ledger
	// lane before delivering the record itself (2s).
	LedgerWait time.Duration
	// Retry is the backoff between delivery attempts to a FailClosed sink;
	// its length is the number of retries (100ms, 500ms, 2s).
	Retry []time.Duration
	// CloseTimeout bounds Close (30s); see Shutdown.
	CloseTimeout time.Duration
	Logger       *slog.Logger
}

// DefaultLedgerRetry is the default backoff for ledger sink retries.
var DefaultLedgerRetry = []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second}

// abandonGrace is how long Shutdown still waits, after cancelling
// deliveries at its deadline, for workers to notice.
const abandonGrace = time.Second

// SinkQueueStats counts what happened to records.
type SinkQueueStats struct {
	// Dropped best-effort records (lane full, queue closed, or abandoned
	// at the Shutdown deadline). Records are only counted when at least one
	// best-effort sink was enabled.
	Dropped int64
	// LedgerWaited counts ledger deliveries that found their lane full and
	// waited; LedgerInline those whose wait ran out (or that came after
	// Close) and were delivered in the calling goroutine.
	LedgerWaited, LedgerInline int64
	// LedgerFailed counts deliveries to a FailClosed sink that still failed
	// after every retry (or failed in a way a retry cannot fix); those
	// records are lost to that sink.
	LedgerFailed int64
	// LedgerLost counts ledger records still queued or being retried when
	// the Shutdown deadline passed.
	LedgerLost int64
}

// delivery is one record bound for the sinks of one lane.
type delivery struct {
	sinks []plugin.Sink
	r     *plugin.Record
}

type lane struct {
	q      *SinkQueue
	id     string // sink id of a ledger lane; "" for the best-effort lane
	ch     chan delivery
	done   chan struct{}
	queued atomic.Int64 // records sent and not yet finished
	// abandoned counts records the worker skipped or cut off after the
	// Shutdown deadline. gaveUp is set by whoever reports them as lost:
	// the worker when it ends, or Shutdown when it stops waiting for it.
	abandoned atomic.Int64
	gaveUp    atomic.Bool
}

// NewSinkQueue starts a queue feeding the sinks that sinks returns at
// enqueue time.
func NewSinkQueue(sinks func() []plugin.Sink, o SinkQueueOptions) *SinkQueue {
	if o.Size <= 0 {
		o.Size = 1024
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.LedgerWait <= 0 {
		o.LedgerWait = 2 * time.Second
	}
	if o.Retry == nil {
		o.Retry = DefaultLedgerRetry
	}
	if o.CloseTimeout <= 0 {
		o.CloseTimeout = 30 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	q := &SinkQueue{sinks: sinks, o: o, ledger: map[string]*lane{}}
	q.ctx, q.abandon = context.WithCancel(context.Background())
	q.best = q.newLane("")
	return q
}

func (q *SinkQueue) newLane(id string) *lane {
	l := &lane{q: q, id: id, ch: make(chan delivery, q.o.Size), done: make(chan struct{})}
	go l.run()
	return l
}

// ledgerLane returns the lane of a FailClosed sink, starting it on first
// use. Callers hold q.mu shared and checked q.closed.
func (q *SinkQueue) ledgerLane(id string) *lane {
	q.lanesMu.Lock()
	defer q.lanesMu.Unlock()
	l := q.ledger[id]
	if l == nil {
		l = q.newLane(id)
		q.ledger[id] = l
	}
	return l
}

// Enqueue implements RecordQueue. It reports false when the best-effort
// lane dropped the record; ledger sinks always get it. It returns at once
// unless a ledger lane is full, and then within LedgerWait plus one
// synchronous delivery per full lane.
func (q *SinkQueue) Enqueue(r *plugin.Record) bool {
	var ledger, best []plugin.Sink
	if q.sinks != nil {
		for _, s := range q.sinks() {
			if s.Info().EffectiveFailPolicy() == plugin.FailClosed {
				ledger = append(ledger, s)
			} else {
				best = append(best, s)
			}
		}
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		for _, s := range ledger {
			q.inline.Add(1)
			q.deliverInline(s, r)
		}
		if len(best) > 0 {
			q.dropped.Add(1)
			return false
		}
		return true
	}
	var full []plugin.Sink
	for _, s := range ledger {
		if !q.ledgerLane(s.Info().ID).trySend(delivery{[]plugin.Sink{s}, CopyRecord(r)}) {
			full = append(full, s)
		}
	}
	for _, s := range full {
		q.enqueueLedger(s, r)
	}
	if len(best) == 0 {
		return true
	}
	if q.best.trySend(delivery{best, CopyRecord(r)}) {
		return true
	}
	q.dropped.Add(1)
	return false
}

func (l *lane) trySend(d delivery) bool {
	l.queued.Add(1)
	select {
	case l.ch <- d:
		return true
	default:
		l.queued.Add(-1)
		return false
	}
}

func (q *SinkQueue) enqueueLedger(s plugin.Sink, r *plugin.Record) {
	l := q.ledgerLane(s.Info().ID)
	q.waited.Add(1)
	t := time.NewTimer(q.o.LedgerWait)
	defer t.Stop()
	l.queued.Add(1)
	select {
	case l.ch <- delivery{[]plugin.Sink{s}, CopyRecord(r)}:
	case <-t.C:
		l.queued.Add(-1)
		q.inline.Add(1)
		q.o.Logger.Warn("ledger sink queue full; delivering the record synchronously", "plugin", s.Info().ID, "request", r.ID)
		q.deliverInline(s, r)
	}
}

// Stats returns the queue's counters since start.
func (q *SinkQueue) Stats() SinkQueueStats {
	return SinkQueueStats{Dropped: q.dropped.Load(), LedgerWaited: q.waited.Load(),
		LedgerInline: q.inline.Load(), LedgerFailed: q.failed.Load(), LedgerLost: q.lost.Load()}
}

// Close is Shutdown bounded by SinkQueueOptions.CloseTimeout.
func (q *SinkQueue) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), q.o.CloseTimeout)
	defer cancel()
	_ = q.Shutdown(ctx)
}

// Shutdown stops queueing and drains every lane until ctx is done. Enqueue
// calls blocked on a full ledger lane finish first; later ones deliver
// synchronously (see SinkQueue). When ctx ends first, deliveries in
// progress are cancelled, retries stop, and the records still queued are
// logged and counted as lost (LedgerLost, Dropped; approximate when a
// sink ignores its context); Shutdown then returns within about a second
// and reports ctx's error.
func (q *SinkQueue) Shutdown(ctx context.Context) error {
	// Arm the deadline before taking the lock: an Enqueue delivering a
	// ledger record inline holds the read lock, and only cancelling q.ctx
	// cuts that delivery (and its retries) short.
	stop := context.AfterFunc(ctx, q.abandon)
	defer stop()
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.best.ch)
		for _, l := range q.ledger {
			close(l.ch)
		}
	}
	lanes := []*lane{q.best}
	for _, l := range q.ledger {
		lanes = append(lanes, l)
	}
	q.mu.Unlock()
	if q.ctx.Err() != nil {
		return context.DeadlineExceeded // an earlier Shutdown already gave up
	}

	all := make(chan struct{})
	go func() {
		for _, l := range lanes {
			<-l.done
		}
		close(all)
	}()
	select {
	case <-all:
		return nil
	case <-ctx.Done():
	}
	q.abandon()
	select {
	case <-all:
	case <-time.After(abandonGrace):
		for _, l := range lanes {
			select {
			case <-l.done:
				continue
			default:
			}
			if !l.gaveUp.Swap(true) {
				l.lose(l.queued.Load()+l.abandoned.Load(), "still delivering")
			}
		}
	}
	return ctx.Err()
}

// lose counts n records of this lane as lost at shutdown.
func (l *lane) lose(n int64, why string) {
	if n <= 0 {
		return
	}
	if l.id == "" {
		l.q.dropped.Add(n)
		l.q.o.Logger.Warn("shutdown deadline passed; best-effort records dropped", "records", n, "why", why)
		return
	}
	l.q.lost.Add(n)
	l.q.o.Logger.Error("shutdown deadline passed; ledger records lost to this sink", "plugin", l.id, "records", n, "why", why)
}

// CopyRecord returns a copy of r that shares no mutable slices with it, so
// one sink rewriting the charge or usage cannot change what another records.
func CopyRecord(r *plugin.Record) *plugin.Record {
	cp := *r
	cp.Attempts = slices.Clone(r.Attempts)
	cp.Usage.Raw = slices.Clone(r.Usage.Raw)
	return &cp
}

func (l *lane) run() {
	defer close(l.done)
	for d := range l.ch {
		if l.q.ctx.Err() != nil || !l.deliver(d) {
			l.abandoned.Add(1)
		}
		l.queued.Add(-1)
	}
	if !l.gaveUp.Swap(true) {
		l.lose(l.abandoned.Load(), "still queued")
	}
}

// deliver reports false when the Shutdown deadline cut a ledger delivery
// off.
func (l *lane) deliver(d delivery) bool {
	for _, s := range d.sinks {
		if l.id == "" {
			_ = l.q.consume(s, CopyRecord(d.r), false)
			continue
		}
		if !l.q.deliverLedger(s, d.r) {
			return false
		}
	}
	return true
}

func (q *SinkQueue) deliverInline(s plugin.Sink, r *plugin.Record) {
	if !q.deliverLedger(s, r) {
		q.lost.Add(1)
		q.o.Logger.Error("shutdown deadline passed; ledger record lost to this sink", "plugin", s.Info().ID, "request", r.ID)
	}
}

// deliverLedger hands r to one FailClosed sink, retrying with backoff. It
// reports false when the Shutdown deadline cut it off; a delivery that
// failed for good is logged and counted here.
func (q *SinkQueue) deliverLedger(s plugin.Sink, r *plugin.Record) bool {
	o := q.o
	for try := 0; ; try++ {
		err := q.consume(s, CopyRecord(r), true)
		if err == nil {
			return true
		}
		if q.ctx.Err() != nil {
			return false
		}
		if permanent(err) || try >= len(o.Retry) {
			q.failed.Add(1)
			o.Logger.Error("ledger sink failed; record lost to it", "plugin", s.Info().ID, "request", r.ID,
				"tries", try+1, "err", err)
			return true
		}
		o.Logger.Warn("ledger sink failed; retrying", "plugin", s.Info().ID, "request", r.ID, "err", err)
		t := time.NewTimer(o.Retry[try])
		select {
		case <-t.C:
		case <-q.ctx.Done():
			t.Stop()
		}
	}
}

// permanent reports a sink failure that retrying cannot fix: a panic, a
// stopped plugin, or a plugin that does not implement sink.consume.
func permanent(err error) bool {
	return errors.Is(err, errSinkPanic) || errors.Is(err, plugin.ErrPluginStopped) || errors.Is(err, errors.ErrUnsupported)
}

// errSinkPanic stands for a recovered sink panic; it is not retried.
var errSinkPanic = errors.New("sink panicked")

func (q *SinkQueue) consume(s plugin.Sink, r *plugin.Record, ledger bool) (err error) {
	defer func() {
		if v := recover(); v != nil {
			q.o.Logger.Error("sink panicked", "plugin", s.Info().ID, "panic", v)
			err = errSinkPanic
		}
	}()
	ctx, cancel := context.WithTimeout(q.ctx, q.o.Timeout)
	defer cancel()
	if err := s.Consume(ctx, r); err != nil {
		if !ledger {
			q.o.Logger.Warn("sink failed", "plugin", s.Info().ID, "request", r.ID, "err", err)
		}
		return err
	}
	return nil
}
