package egress

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// Timeouts are the response timers. Zero disables one.
type Timeouts struct {
	// Connect bounds getting a connection (dial, proxy, TLS) when none is
	// pooled. The transport's own dial and handshake limits (10s each)
	// still apply, so only a shorter value has an effect.
	Connect   time.Duration
	FirstByte time.Duration // request sent → first body byte
	Idle      time.Duration // between body reads that return data
	Total     time.Duration // whole attempt
}

// Override returns t with every non-zero field of o replacing t's.
func (t Timeouts) Override(o Timeouts) Timeouts {
	for _, f := range []struct{ dst, src *time.Duration }{
		{&t.Connect, &o.Connect}, {&t.FirstByte, &o.FirstByte}, {&t.Idle, &o.Idle}, {&t.Total, &o.Total},
	} {
		if *f.src > 0 {
			*f.dst = *f.src
		}
	}
	return t
}

// TimeoutError reports which timer fired.
type TimeoutError struct{ Kind string }

func (e *TimeoutError) Error() string { return "egress: " + e.Kind + " timeout" }

// Timeout reports true (net.Error-style).
func (e *TimeoutError) Timeout() bool { return true }

// Is matches the sentinel of the same kind.
func (e *TimeoutError) Is(target error) bool {
	t, ok := target.(*TimeoutError)
	return ok && t.Kind == e.Kind
}

// Sentinels for errors.Is.
var (
	ErrConnectTimeout   = &TimeoutError{Kind: "connect"}
	ErrFirstByteTimeout = &TimeoutError{Kind: "first byte"}
	ErrIdleTimeout      = &TimeoutError{Kind: "idle"}
	ErrTotalTimeout     = &TimeoutError{Kind: "total"}
)

// Watch runs the timers of one attempt. Create it before sending, send the
// request with Watch's context, wrap the response body with Body, and map any
// error with Err. Stop releases the timers.
type Watch struct {
	ctx    context.Context
	cancel context.CancelFunc
	t      Timeouts

	mu      sync.Mutex
	fired   error
	started bool // first byte seen
	conn    *time.Timer
	first   *time.Timer
	idle    *time.Timer
	total   *time.Timer
	stopped bool
}

// Start begins the first-byte and total timers.
func Start(ctx context.Context, t Timeouts) *Watch {
	w := &Watch{t: t}
	w.ctx, w.cancel = context.WithCancel(ctx)
	if t.Connect > 0 {
		w.conn = time.AfterFunc(t.Connect, func() { w.fire(ErrConnectTimeout) })
	}
	if t.FirstByte > 0 {
		w.first = time.AfterFunc(t.FirstByte, func() { w.fire(ErrFirstByteTimeout) })
	}
	if t.Total > 0 {
		w.total = time.AfterFunc(t.Total, func() { w.fire(ErrTotalTimeout) })
	}
	return w
}

// Context is canceled when any timer fires or Stop is called.
func (w *Watch) Context() context.Context { return w.ctx }

func (w *Watch) fire(err error) {
	w.mu.Lock()
	if w.fired == nil && !w.stopped {
		w.fired = err
	}
	w.mu.Unlock()
	w.cancel()
}

// Connected ends the connect timer; call it when the request got a
// connection (httptrace GotConn).
func (w *Watch) Connected() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn != nil {
		w.conn.Stop()
	}
}

// Touch records progress: the first call ends the first-byte timer, every
// call restarts the idle timer. Body calls it on every read with data; call
// it directly for progress seen elsewhere (an upstream keepalive).
func (w *Watch) Touch() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired != nil || w.stopped {
		return
	}
	if !w.started {
		w.started = true
		for _, t := range []*time.Timer{w.conn, w.first} {
			if t != nil {
				t.Stop()
			}
		}
	}
	if w.t.Idle > 0 {
		if w.idle == nil {
			w.idle = time.AfterFunc(w.t.Idle, func() { w.fire(ErrIdleTimeout) })
		} else {
			w.idle.Reset(w.t.Idle)
		}
	}
}

// Fired returns the timer that fired, or nil.
func (w *Watch) Fired() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

// Err maps an error from the request or body to the timeout that caused it;
// other errors pass through unchanged.
func (w *Watch) Err(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	if f := w.Fired(); f != nil {
		return f
	}
	return err
}

// Stop releases the timers and cancels the context.
func (w *Watch) Stop() {
	w.mu.Lock()
	w.stopped = true
	for _, t := range []*time.Timer{w.conn, w.first, w.idle, w.total} {
		if t != nil {
			t.Stop()
		}
	}
	w.mu.Unlock()
	w.cancel()
}

// Body wraps a response body so reads honor the timers.
func (w *Watch) Body(rc io.ReadCloser) *TimedBody { return &TimedBody{rc: rc, w: w} }

// TimedBody is a response body whose reads fail with a *TimeoutError once a
// timer fires. Closing it stops the watch.
type TimedBody struct {
	rc io.ReadCloser
	w  *Watch
}

// Read reads from the body; a fired timer turns the read error into its
// *TimeoutError.
func (b *TimedBody) Read(p []byte) (int, error) {
	if f := b.w.Fired(); f != nil {
		return 0, f
	}
	n, err := b.rc.Read(p)
	if n > 0 {
		b.w.Touch()
	}
	if err != nil {
		err = b.w.Err(err)
	}
	return n, err
}

// Close closes the body and stops the watch.
func (b *TimedBody) Close() error {
	b.w.Stop()
	return b.rc.Close()
}
