package eventstream

import (
	"bytes"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
	"time"
)

// testdata/ holds the encoded event stream vectors of the AWS SDK for Go
// (Apache-2.0): https://github.com/aws/aws-sdk-go/tree/main/private/protocol/eventstream/testdata

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeOne(t *testing.T, b []byte) *Message {
	t.Helper()
	d := NewDecoder(bytes.NewReader(b), 1<<20)
	m, err := d.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after the only message: %v, want io.EOF", err)
	}
	return m
}

func TestPositiveVectors(t *testing.T) {
	payload := []byte("{'foo':'bar'}")
	t.Run("empty_message", func(t *testing.T) {
		m := decodeOne(t, read(t, "positive/empty_message"))
		if len(m.Headers) != 0 || len(m.Payload) != 0 {
			t.Errorf("got %+v", m)
		}
	})
	t.Run("payload_no_headers", func(t *testing.T) {
		m := decodeOne(t, read(t, "positive/payload_no_headers"))
		if !bytes.Equal(m.Payload, payload) || len(m.Headers) != 0 {
			t.Errorf("got %+v", m)
		}
	})
	t.Run("payload_one_str_header", func(t *testing.T) {
		m := decodeOne(t, read(t, "positive/payload_one_str_header"))
		if m.Header("content-type") != "application/json" || !bytes.Equal(m.Payload, payload) {
			t.Errorf("got %+v", m)
		}
	})
	t.Run("int32_header", func(t *testing.T) {
		m := decodeOne(t, read(t, "positive/int32_header"))
		if len(m.Headers) != 1 || m.Headers[0].Name != "event-type" || m.Headers[0].Value != int64(40972) {
			t.Errorf("got %+v", m.Headers)
		}
	})
	t.Run("all_headers", func(t *testing.T) {
		m := decodeOne(t, read(t, "positive/all_headers"))
		want := []Header{
			{"event-type", int64(40972)},
			{"content-type", "application/json"},
			{"bool false", false},
			{"bool true", true},
			{"byte", int64(-49)},
			{"byte buf", []byte("I'm a little teapot!")},
			{"timestamp", time.UnixMilli(8675309).UTC()},
			{"int16", int64(42)},
			{"int64", int64(42424242)},
			{"uuid", []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
		}
		if !reflect.DeepEqual(m.Headers, want) {
			t.Errorf("headers\n got %#v\nwant %#v", m.Headers, want)
		}
		if !bytes.Equal(m.Payload, payload) {
			t.Errorf("payload %q", m.Payload)
		}
	})
}

func TestNegativeVectors(t *testing.T) {
	for name, want := range map[string]error{
		"corrupted_header_len": ErrPreludeCRC,
		"corrupted_headers":    ErrMessageCRC,
		"corrupted_length":     ErrPreludeCRC,
		"corrupted_payload":    ErrMessageCRC,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewDecoder(bytes.NewReader(read(t, "negative/"+name)), 1<<20).Next()
			if !errors.Is(err, want) {
				t.Errorf("err = %v, want %v", err, want)
			}
		})
	}
}

// Encode reproduces the SDK's bytes for the header types it writes.
func TestEncodeMatchesVectors(t *testing.T) {
	payload := []byte("{'foo':'bar'}")
	for name, msg := range map[string]Message{
		"empty_message":          {},
		"payload_no_headers":     {Payload: payload},
		"payload_one_str_header": {Headers: []Header{{"content-type", "application/json"}}, Payload: payload},
	} {
		got, err := Encode(msg.Headers, msg.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if want := read(t, "positive/"+name); !bytes.Equal(got, want) {
			t.Errorf("%s:\n got %x\nwant %x", name, got, want)
		}
	}
}

func TestStream(t *testing.T) {
	var buf bytes.Buffer
	for i := range 3 {
		b, err := Encode([]Header{{":event-type", "chunk"}, {"n", int64(i)}, {"ok", true}}, []byte{byte('a' + i)})
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
	}
	full := buf.Bytes()
	d := NewDecoder(bytes.NewReader(full), 1<<20)
	for i := range 3 {
		m, err := d.Next()
		if err != nil {
			t.Fatal(err)
		}
		if m.Header(":event-type") != "chunk" || string(m.Payload) != string(rune('a'+i)) || m.Headers[1].Value != int64(i) {
			t.Errorf("message %d: %+v", i, m)
		}
	}
	if _, err := d.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("end: %v", err)
	}

	// Cut inside the prelude and inside the body.
	for _, n := range []int{len(full) - 3, len(full) - 30} {
		d := NewDecoder(bytes.NewReader(full[:n]), 1<<20)
		var err error
		for err == nil {
			_, err = d.Next()
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("cut at %d: %v, want unexpected EOF", n, err)
		}
	}
}

func TestLimits(t *testing.T) {
	b, _ := Encode(nil, bytes.Repeat([]byte("x"), 100))
	if _, err := NewDecoder(bytes.NewReader(b), 64).Next(); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized: %v", err)
	}
	if _, err := NewDecoder(bytes.NewReader(b), len(b)).Next(); err != nil {
		t.Errorf("exactly max: %v", err)
	}
	// A text stream fed to the decoder fails on the prelude checksum.
	if _, err := NewDecoder(bytes.NewReader([]byte("data: {\"type\":\"ping\"}\n\n")), 1<<20).Next(); !errors.Is(err, ErrPreludeCRC) {
		t.Errorf("SSE text: %v", err)
	}
	if _, err := Encode([]Header{{"x", 1.5}}, nil); err == nil {
		t.Error("float header accepted")
	}
}
