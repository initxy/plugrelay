package pipeline

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/initxy/plugrelay/pkg/sse"
)

// errClientGone is returned by writes after the client went away or the
// response was closed.
var errClientGone = errors.New("pipeline: client connection closed")

// clientOut is the client side of a streaming response, shared by every
// attempt of the request and by the keepalive loop.
//
// Frames written before the first content event are held: a retry on
// another candidate discards them, so the client never sees two stream
// openings. Keepalives bypass the hold and may commit the response early.
type clientOut struct {
	x  *exchange
	mu sync.Mutex
	rw http.ResponseWriter
	fl http.Flusher

	committed bool // status line sent
	released  bool // content flowing; hold flushed
	closed    bool
	broken    bool // a write to the client failed
	hold      bytes.Buffer
	last      time.Time   // last write to the client
	ka        []byte      // the client protocol's keepalive frame
	base      http.Header // headers of every committed response
	sticky    http.Header // headers of the current candidate
	stop      chan struct{}
	stopOnce  sync.Once
}

func newClientOut(x *exchange) *clientOut {
	fl, _ := x.w.(http.Flusher)
	o := &clientOut{x: x, rw: x.w, fl: fl, last: time.Now(), stop: make(chan struct{}), base: x.baseHeader()}
	var buf bytes.Buffer
	_ = x.codec.NewStreamEncoder(sse.NewWriter(&buf), x.req).Keepalive()
	o.ka = buf.Bytes()
	return o
}

// commitLocked sends the status line with the stream headers plus extra.
func (o *clientOut) commitLocked(extra http.Header) {
	if o.committed {
		return
	}
	h := o.rw.Header()
	for k, vs := range extra {
		h[k] = vs
	}
	for _, src := range []http.Header{o.base, o.sticky} {
		for k, vs := range src {
			h[k] = vs
		}
	}
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "text/event-stream")
	}
	h.Set("Cache-Control", "no-cache")
	h.Del("Content-Length")
	o.rw.WriteHeader(http.StatusOK)
	o.committed = true
}

func (o *clientOut) writeLocked(p []byte) error {
	if o.broken {
		return errClientGone
	}
	if _, err := o.rw.Write(p); err != nil {
		o.broken = true
		return errClientGone
	}
	if o.fl != nil {
		o.fl.Flush()
	}
	o.last = time.Now()
	return nil
}

// frame writes one complete frame: held before release, sent after.
func (o *clientOut) frame(p []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return errClientGone
	}
	if !o.released {
		o.hold.Write(p)
		return nil
	}
	return o.writeLocked(p)
}

// release commits (with header when not yet committed) and flushes the
// held frames; later frames go straight out.
func (o *clientOut) release(header http.Header) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.released {
		return nil
	}
	if o.closed {
		return errClientGone
	}
	o.commitLocked(header)
	o.released = true
	if o.hold.Len() == 0 {
		return nil
	}
	err := o.writeLocked(o.hold.Bytes())
	o.hold.Reset()
	return err
}

// discard drops held frames before a retry.
func (o *clientOut) discard() {
	o.mu.Lock()
	o.hold.Reset()
	o.mu.Unlock()
}

// state reports whether the response is committed and whether content
// was released.
func (o *clientOut) state() (committed, released bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.committed, o.released
}

// httpError writes a plain HTTP response if nothing was committed yet.
func (o *clientOut) httpError(status int, h http.Header, body []byte) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.committed {
		return false
	}
	o.closed = true
	o.committed = true
	writePlain(o.rw, status, h, body)
	return true
}

// setSticky sets the per-candidate headers sent if the response commits.
func (o *clientOut) setSticky(h http.Header) {
	o.mu.Lock()
	o.sticky = h
	o.mu.Unlock()
}

// close ends keepalives; held frames are dropped.
func (o *clientOut) close() {
	o.mu.Lock()
	o.closed = true
	o.mu.Unlock()
	o.stopOnce.Do(func() { close(o.stop) })
}

// keepalive commits the response after delay without upstream output, then
// writes the protocol's keepalive whenever the client saw nothing for every.
func (o *clientOut) keepalive(delay, every time.Duration) {
	t := time.NewTimer(delay)
	defer t.Stop()
	for {
		select {
		case <-o.stop:
			return
		case <-o.x.ctx.Done():
			return
		case <-t.C:
		}
		o.mu.Lock()
		if o.closed {
			o.mu.Unlock()
			return
		}
		wait := every
		if !o.committed {
			o.commitLocked(nil)
			_ = o.writeLocked(o.ka)
		} else if idle := time.Since(o.last); idle >= every {
			_ = o.writeLocked(o.ka)
		} else {
			wait = every - idle
		}
		o.mu.Unlock()
		t.Reset(wait)
	}
}

// liveWriter writes frames straight to the client, committing if needed;
// used for relay-generated stream errors.
type liveWriter struct{ o *clientOut }

func (l liveWriter) Write(p []byte) (int, error) {
	l.o.mu.Lock()
	defer l.o.mu.Unlock()
	l.o.commitLocked(nil)
	if err := l.o.writeLocked(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func writePlain(w http.ResponseWriter, status int, h http.Header, body []byte) {
	dst := w.Header()
	for k, vs := range h {
		dst[k] = vs
	}
	// The record is enqueued after this returns, and the ledger lane may
	// make that wait. A known length plus a flush lets the client finish
	// reading now instead of at the end of the handler.
	dst.Del("Content-Length")
	if status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified {
		dst.Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
	_ = http.NewResponseController(w).Flush()
}
