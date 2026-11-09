package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

func fixed(s ...plugin.Sink) func() []plugin.Sink { return func() []plugin.Sink { return s } }

type blockingSink struct {
	gate chan struct{}
	mu   sync.Mutex
	got  []string
}

func (s *blockingSink) Info() plugin.Info { return plugin.Info{ID: "sink.test", Kind: plugin.KindSink} }
func (s *blockingSink) Consume(_ context.Context, r *plugin.Record) error {
	<-s.gate
	s.mu.Lock()
	s.got = append(s.got, r.ID)
	s.mu.Unlock()
	return nil
}

func TestSinkQueueBoundedAndDrains(t *testing.T) {
	s := &blockingSink{gate: make(chan struct{})}
	q := NewSinkQueue(fixed(s), SinkQueueOptions{Size: 2})
	accepted := 0
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if q.Enqueue(&plugin.Record{ID: id}) {
			accepted++
		}
	}
	if q.Stats().Dropped == 0 || accepted+int(q.Stats().Dropped) != 5 {
		t.Fatalf("accepted %d dropped %d", accepted, q.Stats().Dropped)
	}
	close(s.gate)
	q.Close()
	if len(s.got) != accepted {
		t.Errorf("consumed %v of %d", s.got, accepted)
	}
}

// rewritingSink tampers with every record it is handed.
type rewritingSink struct{}

func (rewritingSink) Info() plugin.Info {
	return plugin.Info{ID: "sink.rewrite", Kind: plugin.KindSink}
}
func (rewritingSink) Consume(_ context.Context, r *plugin.Record) error {
	r.ChargeMicro, r.CostMicro, r.Usage.Output = 0, 0, 0
	r.Attempts[0].CostMicro = 0
	return nil
}

type keepingSink struct{ got []plugin.Record }

func (*keepingSink) Info() plugin.Info { return plugin.Info{ID: "sink.keep", Kind: plugin.KindSink} }
func (s *keepingSink) Consume(_ context.Context, r *plugin.Record) error {
	s.got = append(s.got, *r)
	return nil
}

// TestSinkCannotChangeCharge pins hard rule 23 for sinks: what a caller is
// charged comes from the meter's settlement; a sink rewriting the record it
// was handed changes nothing another sink (the request log) records.
func TestSinkCannotChangeCharge(t *testing.T) {
	keep := &keepingSink{}
	q := NewSinkQueue(fixed(rewritingSink{}, keep), SinkQueueOptions{Size: 1})
	q.Enqueue(&plugin.Record{ID: "r", ChargeMicro: 700, CostMicro: 500, Usage: ir.Usage{Output: 9},
		Attempts: []plugin.AttemptRecord{{CostMicro: 500}}})
	q.Close()
	if len(keep.got) != 1 {
		t.Fatalf("got %d records", len(keep.got))
	}
	r := keep.got[0]
	if r.ChargeMicro != 700 || r.CostMicro != 500 || r.Usage.Output != 9 || r.Attempts[0].CostMicro != 500 {
		t.Errorf("record changed by an earlier sink: %+v", r)
	}
}

// ledgerSink is a FailClosed sink: its records are the billing ledger.
type ledgerSink struct{ blockingSink }

func (s *ledgerSink) Info() plugin.Info {
	return plugin.Info{ID: "sink.ledger", Kind: plugin.KindSink, FailPolicy: plugin.FailClosed}
}

// TestSinkQueueNeverDropsLedger fills a tiny queue behind a stalled ledger
// sink and a stalled log sink: the log sink loses records, the ledger sink
// gets every one once it resumes.
func TestSinkQueueNeverDropsLedger(t *testing.T) {
	ledger := &ledgerSink{blockingSink{gate: make(chan struct{})}}
	logs := &blockingSink{gate: make(chan struct{})}
	q := NewSinkQueue(fixed(ledger, logs), SinkQueueOptions{Size: 1, LedgerWait: time.Minute})
	const n = 6
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range n {
			q.Enqueue(&plugin.Record{ID: string(rune('a' + i))})
		}
	}()
	close(logs.gate) // logs drain; the ledger lane stays stalled and Enqueue waits
	close(ledger.gate)
	<-done
	q.Close()
	if len(ledger.got) != n {
		t.Fatalf("ledger got %d of %d records: %v", len(ledger.got), n, ledger.got)
	}
	if len(logs.got)+int(q.Stats().Dropped) != n {
		t.Fatalf("logs got %d, dropped %d, want %d total", len(logs.got), q.Stats().Dropped, n)
	}
}

// TestSinkQueueEnqueueAfterClose: a request finishing after shutdown closed
// the queue still reaches the ledger, synchronously, instead of panicking
// on a closed channel.
func TestSinkQueueEnqueueAfterClose(t *testing.T) {
	ledger := &ledgerSink{blockingSink{gate: make(chan struct{})}}
	logs := &blockingSink{gate: make(chan struct{})}
	close(ledger.gate)
	close(logs.gate)
	q := NewSinkQueue(fixed(ledger, logs), SinkQueueOptions{})
	q.Close()
	if q.Enqueue(&plugin.Record{ID: "late"}) {
		t.Error("best-effort lane took a record after Close")
	}
	if len(ledger.got) != 1 || len(logs.got) != 0 {
		t.Fatalf("ledger %v logs %v", ledger.got, logs.got)
	}
	if st := q.Stats(); st.LedgerInline != 1 || st.Dropped != 1 {
		t.Fatalf("stats %+v", st)
	}
}

// TestSinkQueueCloseWhileLedgerFull closes the queue while an Enqueue waits
// on a full ledger lane: Close waits for it, nothing panics, nothing is lost.
func TestSinkQueueCloseWhileLedgerFull(t *testing.T) {
	ledger := &ledgerSink{blockingSink{gate: make(chan struct{})}}
	q := NewSinkQueue(fixed(ledger), SinkQueueOptions{Size: 1, LedgerWait: time.Minute})
	const n = 4
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { q.Enqueue(&plugin.Record{ID: string(rune('a' + i))}) })
	}
	for q.Stats().LedgerWaited == 0 {
		time.Sleep(time.Millisecond)
	}
	closed := make(chan struct{})
	go func() { q.Close(); close(closed) }()
	time.Sleep(20 * time.Millisecond)
	close(ledger.gate)
	wg.Wait()
	<-closed
	if len(ledger.got) != n {
		t.Fatalf("ledger got %v", ledger.got)
	}
}

// firstBlocksSink stalls its first record until gate closes.
type firstBlocksSink struct {
	gate  chan struct{}
	calls atomic.Int32
	mu    sync.Mutex
	got   []string
}

func (s *firstBlocksSink) Info() plugin.Info {
	return plugin.Info{ID: "sink.ledger", Kind: plugin.KindSink, FailPolicy: plugin.FailClosed}
}
func (s *firstBlocksSink) Consume(_ context.Context, r *plugin.Record) error {
	if s.calls.Add(1) == 1 {
		<-s.gate
	}
	s.mu.Lock()
	s.got = append(s.got, r.ID)
	s.mu.Unlock()
	return nil
}

// TestSinkQueueLedgerWaitBounded: behind a stalled ledger worker, Enqueue
// waits at most LedgerWait, then writes the record itself.
func TestSinkQueueLedgerWaitBounded(t *testing.T) {
	s := &firstBlocksSink{gate: make(chan struct{})}
	q := NewSinkQueue(fixed(s), SinkQueueOptions{Size: 1, LedgerWait: 20 * time.Millisecond})
	q.Enqueue(&plugin.Record{ID: "a"}) // taken by the worker, which stalls
	for s.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	q.Enqueue(&plugin.Record{ID: "b"}) // fills the lane
	start := time.Now()
	q.Enqueue(&plugin.Record{ID: "c"}) // waits, then delivers inline
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Enqueue blocked %v", d)
	}
	s.mu.Lock()
	inline := append([]string(nil), s.got...)
	s.mu.Unlock()
	if len(inline) != 1 || inline[0] != "c" {
		t.Fatalf("delivered before the worker resumed: %v", inline)
	}
	if st := q.Stats(); st.LedgerWaited != 1 || st.LedgerInline != 1 {
		t.Fatalf("stats %+v", st)
	}
	close(s.gate)
	q.Close()
	if len(s.got) != 3 {
		t.Fatalf("got %v", s.got)
	}
}

// flakySink is a ledger sink failing its first fails calls.
type flakySink struct {
	fails int32
	calls atomic.Int32
	ok    atomic.Int32
}

func (s *flakySink) Info() plugin.Info {
	return plugin.Info{ID: "sink.flaky", Kind: plugin.KindSink, FailPolicy: plugin.FailClosed}
}
func (s *flakySink) Consume(context.Context, *plugin.Record) error {
	if s.calls.Add(1) <= s.fails {
		return errors.New("database is locked")
	}
	s.ok.Add(1)
	return nil
}

// TestLedgerSinkRetried: a failing ledger write is retried with backoff; one
// that keeps failing is given up after the last retry and counted.
func TestLedgerSinkRetried(t *testing.T) {
	retry := []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Run("recovers", func(t *testing.T) {
		s := &flakySink{fails: 2}
		q := NewSinkQueue(fixed(s), SinkQueueOptions{Retry: retry})
		q.Enqueue(&plugin.Record{ID: "r"})
		q.Close()
		if s.calls.Load() != 3 || s.ok.Load() != 1 || q.Stats().LedgerFailed != 0 {
			t.Fatalf("calls %d ok %d stats %+v", s.calls.Load(), s.ok.Load(), q.Stats())
		}
	})
	t.Run("gives up", func(t *testing.T) {
		s := &flakySink{fails: 100}
		q := NewSinkQueue(fixed(s), SinkQueueOptions{Retry: retry})
		q.Enqueue(&plugin.Record{ID: "r"})
		q.Close()
		if s.calls.Load() != int32(len(retry)+1) || q.Stats().LedgerFailed != 1 {
			t.Fatalf("calls %d stats %+v", s.calls.Load(), q.Stats())
		}
	})
	t.Run("best-effort not retried", func(t *testing.T) {
		s := &failingLogSink{}
		q := NewSinkQueue(fixed(s), SinkQueueOptions{Retry: retry})
		q.Enqueue(&plugin.Record{ID: "r"})
		q.Close()
		if s.calls.Load() != 1 {
			t.Fatalf("calls %d", s.calls.Load())
		}
	})
}

type failingLogSink struct{ calls atomic.Int32 }

func (*failingLogSink) Info() plugin.Info { return plugin.Info{ID: "sink.logs", Kind: plugin.KindSink} }
func (s *failingLogSink) Consume(context.Context, *plugin.Record) error {
	s.calls.Add(1)
	return errors.New("down")
}

// brokenLedgerSink is a FailClosed sink whose every write fails after a
// short delay.
type brokenLedgerSink struct {
	calls atomic.Int32
	err   error
}

func (*brokenLedgerSink) Info() plugin.Info {
	return plugin.Info{ID: "sink.broken", Kind: plugin.KindSink, FailPolicy: plugin.FailClosed}
}
func (s *brokenLedgerSink) Consume(ctx context.Context, _ *plugin.Record) error {
	s.calls.Add(1)
	if s.err != nil {
		return s.err
	}
	select {
	case <-time.After(5 * time.Millisecond):
	case <-ctx.Done():
	}
	return errors.New("database is down")
}

// TestLedgerSinksIsolated: a FailClosed sink that always fails (and is
// retried with the default backoff) does not delay another FailClosed sink,
// and Close gives up on it at its deadline, counting what it lost.
func TestLedgerSinksIsolated(t *testing.T) {
	broken := &brokenLedgerSink{}
	good := &ledgerSink{blockingSink{gate: make(chan struct{})}}
	close(good.gate)
	q := NewSinkQueue(fixed(broken, good), SinkQueueOptions{CloseTimeout: 200 * time.Millisecond})
	const n = 20
	start := time.Now()
	for i := range n {
		q.Enqueue(&plugin.Record{ID: string(rune('a' + i))})
	}
	for {
		good.mu.Lock()
		got := len(good.got)
		good.mu.Unlock()
		if got == n {
			break
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("good ledger sink got %d of %d records behind a broken one", got, n)
		}
		time.Sleep(time.Millisecond)
	}
	began := time.Now()
	q.Close()
	if d := time.Since(began); d > 200*time.Millisecond+abandonGrace {
		t.Fatalf("Close took %v", d)
	}
	st := q.Stats()
	if st.LedgerLost+st.LedgerFailed != n || st.LedgerLost == 0 {
		t.Fatalf("stats %+v, want %d records lost or failed", st, n)
	}
	if st.Dropped != 0 {
		t.Fatalf("dropped %d with no best-effort sink", st.Dropped)
	}
}

// TestLedgerSinkStoppedNotRetried: a plugin that reports it was stopped
// fails the record at once instead of retrying against a dead process.
func TestLedgerSinkStoppedNotRetried(t *testing.T) {
	s := &brokenLedgerSink{err: fmt.Errorf("sink.consume: %w", plugin.ErrPluginStopped)}
	q := NewSinkQueue(fixed(s), SinkQueueOptions{Retry: []time.Duration{time.Hour}})
	q.Enqueue(&plugin.Record{ID: "r"})
	began := time.Now()
	q.Close()
	if s.calls.Load() != 1 || q.Stats().LedgerFailed != 1 || time.Since(began) > time.Second {
		t.Fatalf("calls %d stats %+v after %v", s.calls.Load(), q.Stats(), time.Since(began))
	}
}

// TestSinkQueueShutdownSinkIgnoresContext: a ledger sink stuck in Consume
// past its context does not hold Shutdown beyond its deadline; the records
// behind it are counted as lost.
func TestSinkQueueShutdownSinkIgnoresContext(t *testing.T) {
	s := &firstBlocksSink{gate: make(chan struct{})}
	defer close(s.gate)
	q := NewSinkQueue(fixed(s), SinkQueueOptions{})
	for _, id := range []string{"a", "b", "c"} {
		q.Enqueue(&plugin.Record{ID: id})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	began := time.Now()
	_ = q.Shutdown(ctx)
	if d := time.Since(began); d > 50*time.Millisecond+abandonGrace+500*time.Millisecond {
		t.Fatalf("Shutdown took %v", d)
	}
	if st := q.Stats(); st.LedgerLost != 3 {
		t.Fatalf("stats %+v", st)
	}
}

// TestSinkQueueNoBestEffortSinks: with only ledger sinks, nothing counts as
// dropped, before or after Close.
func TestSinkQueueNoBestEffortSinks(t *testing.T) {
	ledger := &ledgerSink{blockingSink{gate: make(chan struct{})}}
	close(ledger.gate)
	q := NewSinkQueue(fixed(ledger), SinkQueueOptions{})
	if !q.Enqueue(&plugin.Record{ID: "a"}) {
		t.Fatal("reported a drop")
	}
	q.Close()
	if !q.Enqueue(&plugin.Record{ID: "b"}) {
		t.Fatal("reported a drop after Close")
	}
	if st := q.Stats(); st.Dropped != 0 || len(ledger.got) != 2 {
		t.Fatalf("stats %+v got %v", st, ledger.got)
	}
}

// stuckLedgerSink is a FailClosed sink whose Consume hangs until its
// context ends, then fails.
type stuckLedgerSink struct{ calls atomic.Int64 }

func (s *stuckLedgerSink) Info() plugin.Info {
	return plugin.Info{ID: "sink.stuck", Kind: plugin.KindSink, FailPolicy: plugin.FailClosed}
}
func (s *stuckLedgerSink) Consume(ctx context.Context, _ *plugin.Record) error {
	s.calls.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

// TestSinkQueueShutdownCutsInlineDelivery: an Enqueue delivering inline
// (its ledger lane full) holds the queue's read lock; Shutdown's deadline
// must still cancel it instead of waiting out its timeouts and retries.
func TestSinkQueueShutdownCutsInlineDelivery(t *testing.T) {
	s := &stuckLedgerSink{}
	q := NewSinkQueue(fixed(s), SinkQueueOptions{Size: 1, LedgerWait: 10 * time.Millisecond})
	q.Enqueue(&plugin.Record{ID: "a"}) // worker takes it and hangs
	for s.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	q.Enqueue(&plugin.Record{ID: "b"}) // fills the lane
	inline := make(chan struct{})
	go func() { defer close(inline); q.Enqueue(&plugin.Record{ID: "c"}) }() // waits, then delivers inline
	for s.calls.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	began := time.Now()
	_ = q.Shutdown(ctx)
	if d := time.Since(began); d > 50*time.Millisecond+abandonGrace+500*time.Millisecond {
		t.Fatalf("Shutdown took %v with an inline delivery in progress", d)
	}
	select {
	case <-inline:
	case <-time.After(2 * time.Second):
		t.Fatal("inline delivery was not cancelled")
	}
}
