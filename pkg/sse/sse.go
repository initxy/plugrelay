// Package sse is plugrelay's single Server-Sent Events reader and writer.
//
// The reader enforces a maximum event size and fails loudly instead of
// silently dropping oversized events. Upstreams are always asked for SSE
// framing (Gemini via alt=sse), so every stream decoder consumes Event.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// DefaultMaxEventSize bounds one event's data. Large tool inputs and base64
// images arrive as single events, so this is generous.
const DefaultMaxEventSize = 32 << 20

// ErrEventTooLarge is returned when an event exceeds the reader's limit.
var ErrEventTooLarge = errors.New("sse: event exceeds maximum size")

// Event is one SSE event.
type Event struct {
	Name string // "event:" field; empty for unnamed events
	Data []byte // joined "data:" lines
	ID   string
	// Comment is set for comment-only frames (": keepalive"), which carry no data.
	Comment bool
}

// Reader parses an SSE stream.
type Reader struct {
	r   *bufio.Reader
	max int
}

// NewReader returns a Reader with the given max event size (0 = default).
func NewReader(r io.Reader, max int) *Reader {
	if max <= 0 {
		max = DefaultMaxEventSize
	}
	return &Reader{r: bufio.NewReaderSize(r, 64<<10), max: max}
}

// Next returns the next event, or io.EOF at a clean end of input.
func (rd *Reader) Next() (Event, error) {
	var ev Event
	var data bytes.Buffer
	hasData, hasField := false, false
	for {
		line, err := rd.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && (hasData || hasField) {
				ev.Data = data.Bytes()
				return ev, nil
			}
			return Event{}, err
		}
		if len(line) == 0 {
			if hasData || hasField {
				ev.Data = data.Bytes()
				return ev, nil
			}
			continue
		}
		if line[0] == ':' {
			if !hasData && !hasField {
				return Event{Comment: true}, nil
			}
			continue
		}
		field, value := line, []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], line[i+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
		}
		switch string(field) {
		case "event":
			ev.Name = string(value)
			hasField = true
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			if data.Len()+len(value) > rd.max {
				return Event{}, fmt.Errorf("%w (%d bytes)", ErrEventTooLarge, rd.max)
			}
			data.Write(value)
			hasData = true
		case "id":
			ev.ID = string(value)
			hasField = true
		case "retry":
			hasField = true
		}
	}
}

// lineOverhead is the room a line gets beyond the data limit for its field
// name ("data: "), so data of exactly the limit still fits on one line.
const lineOverhead = 16

func (rd *Reader) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := rd.r.ReadLine()
		if err != nil {
			if len(buf) > 0 && errors.Is(err, io.EOF) {
				return buf, nil
			}
			return nil, err
		}
		if len(buf)+len(chunk) > rd.max+lineOverhead {
			return nil, fmt.Errorf("%w (%d bytes)", ErrEventTooLarge, rd.max)
		}
		if !isPrefix && buf == nil {
			return chunk, nil
		}
		buf = append(buf, chunk...)
		if !isPrefix {
			return buf, nil
		}
	}
}

// Writer writes SSE frames to a client and flushes after each one.
type Writer interface {
	// Event writes "event: name" (omitted when empty) and data.
	Event(name string, data []byte) error
	// Comment writes a comment line, used as a keepalive.
	Comment(text string) error
	// Raw writes bytes verbatim (passthrough).
	Raw(p []byte) error
}

type httpWriter struct {
	w io.Writer
	f http.Flusher
}

// NewWriter wraps an io.Writer; if it is an http.Flusher every frame is flushed.
func NewWriter(w io.Writer) Writer {
	f, _ := w.(http.Flusher)
	return &httpWriter{w: w, f: f}
}

func (h *httpWriter) flush() {
	if h.f != nil {
		h.f.Flush()
	}
}

func (h *httpWriter) Event(name string, data []byte) error {
	var b bytes.Buffer
	if name != "" {
		b.WriteString("event: ")
		b.WriteString(name)
		b.WriteByte('\n')
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		b.WriteString("data: ")
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	if _, err := h.w.Write(b.Bytes()); err != nil {
		return err
	}
	h.flush()
	return nil
}

func (h *httpWriter) Comment(text string) error {
	if _, err := io.WriteString(h.w, ": "+text+"\n\n"); err != nil {
		return err
	}
	h.flush()
	return nil
}

func (h *httpWriter) Raw(p []byte) error {
	if _, err := h.w.Write(p); err != nil {
		return err
	}
	h.flush()
	return nil
}
