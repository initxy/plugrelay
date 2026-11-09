package sse

import (
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// readAll returns every event of input, failing on any error but io.EOF.
func readAll(t *testing.T, input string, max int) []Event {
	t.Helper()
	rd := NewReader(strings.NewReader(input), max)
	var out []Event
	for {
		ev, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, ev)
	}
}

func eventsEqual(a, b Event) bool {
	return a.Name == b.Name && a.ID == b.ID && a.Comment == b.Comment && bytes.Equal(a.Data, b.Data)
}

func TestReader(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []Event
	}{
		{
			name:  "single data line",
			input: "data: hello\n\n",
			want:  []Event{{Data: []byte("hello")}},
		},
		{
			name:  "multi-line data is joined with newlines",
			input: "data: a\ndata: b\ndata:c\n\n",
			want:  []Event{{Data: []byte("a\nb\nc")}},
		},
		{
			name:  "event and id fields",
			input: "event: message_start\nid: 42\ndata: {}\n\n",
			want:  []Event{{Name: "message_start", ID: "42", Data: []byte("{}")}},
		},
		{
			name:  "only one leading space is stripped",
			input: "data:  two\n\n",
			want:  []Event{{Data: []byte(" two")}},
		},
		{
			name:  "field without data still ends an event",
			input: "event: ping\n\nretry: 100\n\n",
			want:  []Event{{Name: "ping", Data: []byte{}}, {Data: []byte{}}},
		},
		{
			name:  "comment frame before an event",
			input: ": keepalive\n\ndata: x\n\n",
			want:  []Event{{Comment: true}, {Data: []byte("x")}},
		},
		{
			name:  "comment inside an event is ignored",
			input: "data: a\n: note\ndata: b\n\n",
			want:  []Event{{Data: []byte("a\nb")}},
		},
		{
			name:  "unknown fields are ignored",
			input: "foo: bar\ndata: x\n\n",
			want:  []Event{{Data: []byte("x")}},
		},
		{
			name:  "CRLF line endings",
			input: "event: e\r\ndata: a\r\ndata: b\r\n\r\ndata: c\r\n\r\n",
			want:  []Event{{Name: "e", Data: []byte("a\nb")}, {Data: []byte("c")}},
		},
		{
			name:  "extra blank lines between events",
			input: "\n\ndata: a\n\n\n\ndata: b\n\n",
			want:  []Event{{Data: []byte("a")}, {Data: []byte("b")}},
		},
		{
			name:  "final event without trailing blank line",
			input: "data: a\n\ndata: last",
			want:  []Event{{Data: []byte("a")}, {Data: []byte("last")}},
		},
		{
			name:  "final event ending in a single newline",
			input: "event: done\ndata: [DONE]\n",
			want:  []Event{{Name: "done", Data: []byte("[DONE]")}},
		},
		{
			name:  "empty input",
			input: "",
			want:  nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := readAll(t, c.input, 0)
			if len(got) != len(c.want) {
				t.Fatalf("got %d events %+v, want %d %+v", len(got), got, len(c.want), c.want)
			}
			for i := range got {
				if !eventsEqual(got[i], c.want[i]) {
					t.Errorf("event %d: got %+v (data %q), want %+v (data %q)", i, got[i], got[i].Data, c.want[i], c.want[i].Data)
				}
			}
		})
	}
}

func TestReaderLongLine(t *testing.T) {
	// Longer than the bufio buffer, so ReadLine returns it in pieces.
	long := strings.Repeat("x", 200<<10)
	got := readAll(t, "data: "+long+"\n\n", 0)
	if len(got) != 1 || string(got[0].Data) != long {
		t.Fatalf("long line not reassembled: %d events", len(got))
	}
}

func TestReaderMaxEventSize(t *testing.T) {
	const max = 100
	t.Run("data of exactly the limit fits", func(t *testing.T) {
		data := strings.Repeat("a", max)
		got := readAll(t, "data: "+data+"\n\n", max)
		if len(got) != 1 || string(got[0].Data) != data {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("multi-line data of exactly the limit fits", func(t *testing.T) {
		// 49 + "\n" + 50 = 100 bytes.
		a, b := strings.Repeat("a", 49), strings.Repeat("b", 50)
		got := readAll(t, "data: "+a+"\ndata: "+b+"\n\n", max)
		if len(got) != 1 || string(got[0].Data) != a+"\n"+b {
			t.Fatalf("got %+v", got)
		}
	})
	for name, input := range map[string]string{
		"one byte over on one line":  "data: " + strings.Repeat("a", max+1) + "\n\n",
		"one byte over across lines": "data: " + strings.Repeat("a", 50) + "\ndata: " + strings.Repeat("b", 50) + "\n\n",
		"huge line":                  "data: " + strings.Repeat("a", 10*max) + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			rd := NewReader(strings.NewReader(input), max)
			_, err := rd.Next()
			if !errors.Is(err, ErrEventTooLarge) {
				t.Fatalf("err %v, want ErrEventTooLarge", err)
			}
		})
	}
	t.Run("events after a large one are independent", func(t *testing.T) {
		a := strings.Repeat("a", max)
		got := readAll(t, "data: "+a+"\n\ndata: "+a+"\n\n", max)
		if len(got) != 2 {
			t.Fatalf("got %d events", len(got))
		}
	})
}

func TestReaderTransportError(t *testing.T) {
	boom := errors.New("connection reset")
	rd := NewReader(io.MultiReader(strings.NewReader("data: a\n\n"), &failReader{err: boom}), 0)
	if ev, err := rd.Next(); err != nil || string(ev.Data) != "a" {
		t.Fatalf("first event: %+v %v", ev, err)
	}
	if _, err := rd.Next(); !errors.Is(err, boom) {
		t.Fatalf("err %v, want the transport error", err)
	}
}

type failReader struct{ err error }

func (f *failReader) Read([]byte) (int, error) { return 0, f.err }

func TestWriter(t *testing.T) {
	cases := []struct {
		name  string
		write func(Writer) error
		want  string
	}{
		{"named event", func(w Writer) error { return w.Event("message_start", []byte(`{"a":1}`)) },
			"event: message_start\ndata: {\"a\":1}\n\n"},
		{"unnamed event", func(w Writer) error { return w.Event("", []byte("[DONE]")) },
			"data: [DONE]\n\n"},
		{"multi-line data becomes several data lines", func(w Writer) error { return w.Event("", []byte("a\nb")) },
			"data: a\ndata: b\n\n"},
		{"empty data", func(w Writer) error { return w.Event("ping", nil) },
			"event: ping\ndata: \n\n"},
		{"comment", func(w Writer) error { return w.Comment("keepalive") },
			": keepalive\n\n"},
		{"raw is verbatim", func(w Writer) error { return w.Raw([]byte("event: x\r\ndata: y\r\n\r\n")) },
			"event: x\r\ndata: y\r\n\r\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if err := c.write(NewWriter(rec)); err != nil {
				t.Fatal(err)
			}
			if got := rec.Body.String(); got != c.want {
				t.Errorf("wrote %q, want %q", got, c.want)
			}
			if !rec.Flushed {
				t.Error("frame was not flushed")
			}
		})
	}
}

func TestWriterRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf) // not a Flusher: writes must still succeed
	if err := w.Event("delta", []byte("line1\nline2")); err != nil {
		t.Fatal(err)
	}
	if err := w.Comment("ka"); err != nil {
		t.Fatal(err)
	}
	if err := w.Event("", []byte("end")); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, buf.String(), 0)
	want := []Event{{Name: "delta", Data: []byte("line1\nline2")}, {Comment: true}, {Data: []byte("end")}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if !eventsEqual(got[i], want[i]) {
			t.Errorf("event %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestWriterError(t *testing.T) {
	boom := errors.New("client gone")
	w := NewWriter(&failWriter{err: boom})
	for name, err := range map[string]error{
		"event":   w.Event("e", []byte("d")),
		"comment": w.Comment("c"),
		"raw":     w.Raw([]byte("r")),
	} {
		if !errors.Is(err, boom) {
			t.Errorf("%s: err %v, want the write error", name, err)
		}
	}
}

type failWriter struct{ err error }

func (f *failWriter) Write([]byte) (int, error) { return 0, f.err }
