// Package media holds what the codecs of the media operations (embeddings,
// images, audio, rerank) share: they have no event streams, they sniff only
// the model, embeddings travel as floats or as base64 of little-endian
// float32, and the rerank protocols share request decoding and usage.
package media

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/sse"
)

// ErrUnsupported is returned for an operation the protocol does not have.
var ErrUnsupported = errors.New("protocol: operation not part of this protocol")

// ErrNoStream is returned when a stream is fed to a protocol without event
// streams.
var ErrNoStream = errors.New("protocol: this operation has no event stream")

// NoStreamEncoder is the stream encoder of a protocol without event
// streams. Only an error event is ever written, as a JSON data line.
type NoStreamEncoder struct {
	W    sse.Writer
	done bool
}

// Encode writes an error event; everything else is ignored.
func (e *NoStreamEncoder) Encode(ev *ir.Event) error {
	if e.done || ev == nil {
		return nil
	}
	if ev.Kind == ir.EvError || ev.Kind == ir.EvDone {
		e.done = true
	}
	if ev.Kind == ir.EvError && ev.Err != nil && e.W != nil {
		return e.W.Event("", []byte(`{"error":{"message":`+quote(ev.Err.Message)+`}}`))
	}
	return nil
}

// Keepalive writes nothing.
func (e *NoStreamEncoder) Keepalive() error { return nil }

// Usage is always zero.
func (e *NoStreamEncoder) Usage() ir.Usage { return ir.Usage{} }

// NoStreamDecoder is the stream decoder of a protocol without event
// streams: nothing it is fed is an event, and a stream is never complete.
type NoStreamDecoder struct{}

// Feed ignores the event.
func (NoStreamDecoder) Feed(sse.Event) ([]ir.Event, error) { return nil, ErrNoStream }

// Finish reports an incomplete stream.
func (NoStreamDecoder) Finish() ([]ir.Event, bool) { return nil, false }

// Usage is always zero.
func (NoStreamDecoder) Usage() ir.Usage { return ir.Usage{} }

// EncodeBase64 renders a vector as base64 of little-endian float32.
func EncodeBase64(v []float64) string {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(float32(f)))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// DecodeBase64 parses base64 of little-endian float32.
func DecodeBase64(s string) ([]float64, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b)%4 != 0 {
		return nil, errors.New("embedding: base64 length is not a multiple of 4")
	}
	out := make([]float64, len(b)/4)
	for i := range out {
		out[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:])))
	}
	return out, nil
}

// EstimateTokens is a rough count of about four bytes per token.
func EstimateTokens(chars int) int { return (chars + 3) / 4 }

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
