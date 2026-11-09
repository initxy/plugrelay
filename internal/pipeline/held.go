package pipeline

import (
	"math"
	"net/http"
)

// heldFrames delays stream frames whose events an event hook holds back
// (plugin.EventHolder). Frames leave in order once every event in them is
// released; an abort or a retry drops them.
type heldFrames struct {
	frames []heldFrame
	seq    int64              // number of the last event run through the hooks
	seen   map[string][]int64 // per holder hook: the events it still holds
	off    bool               // the stream is ending with an error: write through
}

type heldFrame struct {
	b       []byte
	seq     int64
	content bool
	header  http.Header
}

// from is the first event number some hook still holds.
func (h *heldFrames) from() int64 {
	min := int64(math.MaxInt64)
	for _, s := range h.seen {
		if len(s) > 0 && s[0] < min {
			min = s[0]
		}
	}
	return min
}

// hold records that hook id saw the current event and still holds its n
// most recent ones.
func (h *heldFrames) hold(id string, n int) {
	if h.seen == nil {
		h.seen = map[string][]int64{}
	}
	s := append(h.seen[id], h.seq)
	n = max(0, min(n, len(s)))
	h.seen[id] = append(s[:0], s[len(s)-n:]...)
}

// holding reports whether a frame written now would wait.
func (h *heldFrames) holding() bool {
	return !h.off && (len(h.frames) > 0 || h.seq >= h.from())
}

// reset drops held frames and every hook's holds.
func (h *heldFrames) reset() {
	h.frames, h.seen = nil, nil
}

// sendFrame writes one client frame, or queues it while its events are
// held; content frames release the response once written.
func (x *exchange) sendFrame(b []byte, content bool, header http.Header) error {
	h := &x.held
	if !h.holding() {
		return x.writeFrame(b, content, header)
	}
	h.frames = append(h.frames, heldFrame{b: b, seq: h.seq, content: content, header: header})
	return x.flushHeld(false)
}

// flushHeld writes the queued frames no hook holds any more, or all of
// them.
func (x *exchange) flushHeld(all bool) error {
	h := &x.held
	from := h.from()
	for len(h.frames) > 0 && (all || h.frames[0].seq < from) {
		f := h.frames[0]
		h.frames = h.frames[1:]
		if err := x.writeFrame(f.b, f.content, f.header); err != nil {
			return err
		}
	}
	if all {
		h.seen = nil
	}
	return nil
}

func (x *exchange) writeFrame(b []byte, content bool, header http.Header) error {
	if err := x.out.frame(b); err != nil {
		return err
	}
	if content {
		return x.out.release(header)
	}
	return nil
}

// heldWriter is the io.Writer behind a convert-tier encoder: each Write is
// one frame of the event being encoded.
type heldWriter struct {
	x       *exchange
	content *bool // whether the event being encoded is content
}

func (w heldWriter) Write(p []byte) (int, error) {
	if err := w.x.sendFrame(append([]byte(nil), p...), *w.content, nil); err != nil {
		return 0, err
	}
	return len(p), nil
}
