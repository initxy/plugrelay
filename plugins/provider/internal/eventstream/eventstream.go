// Package eventstream reads and writes the AWS event stream binary framing
// (application/vnd.amazon.eventstream), used by streaming AWS APIs such as
// Bedrock's InvokeModelWithResponseStream. Format:
// https://smithy.io/2.0/aws/amazon-eventstream.html
//
// A message is
//
//	total length   uint32 big endian, whole message including both CRCs
//	headers length uint32 big endian
//	prelude CRC    CRC-32 (IEEE) of the 8 bytes above
//	headers        name length (1) · name · type (1) · value
//	payload
//	message CRC    CRC-32 (IEEE) of everything before it
package eventstream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"
)

const (
	preludeLen = 12
	crcLen     = 4
	minLen     = preludeLen + crcLen
	// maxHeadersLen is the protocol's limit on the header block.
	maxHeadersLen = 128 << 10
)

// Errors returned by Decoder.Next. Every one of them means the stream is
// unusable from that point on.
var (
	ErrPreludeCRC = errors.New("eventstream: prelude checksum mismatch")
	ErrMessageCRC = errors.New("eventstream: message checksum mismatch")
	ErrTooLarge   = errors.New("eventstream: message exceeds maximum size")
	ErrMalformed  = errors.New("eventstream: malformed message")
)

// Header value types.
const (
	TypeTrue      byte = 0
	TypeFalse     byte = 1
	TypeByte      byte = 2
	TypeShort     byte = 3
	TypeInt       byte = 4
	TypeLong      byte = 5
	TypeBytes     byte = 6
	TypeString    byte = 7
	TypeTimestamp byte = 8
	TypeUUID      byte = 9
)

// Header is one message header. Value is bool, int64 (byte, short, int,
// long), []byte (bytes, uuid), string or time.Time (timestamp).
type Header struct {
	Name  string
	Value any
}

// Message is one decoded frame.
type Message struct {
	Headers []Header
	Payload []byte
}

// Header returns the string value of a header, or "" when it is absent or
// not a string.
func (m *Message) Header(name string) string {
	for _, h := range m.Headers {
		if h.Name == name {
			if s, ok := h.Value.(string); ok {
				return s
			}
		}
	}
	return ""
}

// Decoder reads messages from a stream.
type Decoder struct {
	r   io.Reader
	max int
}

// NewDecoder returns a decoder refusing messages longer than max bytes.
func NewDecoder(r io.Reader, max int) *Decoder { return &Decoder{r: r, max: max} }

// Next returns the next message. It returns io.EOF only at a message
// boundary; a stream cut inside a message is io.ErrUnexpectedEOF.
func (d *Decoder) Next() (*Message, error) {
	var prelude [preludeLen]byte
	if _, err := io.ReadFull(d.r, prelude[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, unexpected(err)
	}
	total := binary.BigEndian.Uint32(prelude[0:4])
	hlen := binary.BigEndian.Uint32(prelude[4:8])
	if crc32.ChecksumIEEE(prelude[:8]) != binary.BigEndian.Uint32(prelude[8:12]) {
		return nil, ErrPreludeCRC
	}
	if uint64(total) > uint64(d.max) {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, total)
	}
	if total < minLen || hlen > maxHeadersLen || uint64(hlen) > uint64(total)-minLen {
		return nil, ErrMalformed
	}
	buf := make([]byte, total)
	copy(buf, prelude[:])
	if _, err := io.ReadFull(d.r, buf[preludeLen:]); err != nil {
		return nil, unexpected(err)
	}
	body := buf[:total-crcLen]
	if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(buf[total-crcLen:]) {
		return nil, ErrMessageCRC
	}
	headers, err := parseHeaders(body[preludeLen : preludeLen+hlen])
	if err != nil {
		return nil, err
	}
	return &Message{Headers: headers, Payload: body[preludeLen+hlen:]}, nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func parseHeaders(b []byte) ([]Header, error) {
	var out []Header
	for len(b) > 0 {
		n := int(b[0])
		if n == 0 || len(b) < 1+n+1 {
			return nil, ErrMalformed
		}
		h := Header{Name: string(b[1 : 1+n])}
		typ := b[1+n]
		b = b[2+n:]
		need := func(k int) ([]byte, error) {
			if len(b) < k {
				return nil, ErrMalformed
			}
			v := b[:k]
			b = b[k:]
			return v, nil
		}
		var v []byte
		var err error
		switch typ {
		case TypeTrue:
			h.Value = true
		case TypeFalse:
			h.Value = false
		case TypeByte:
			if v, err = need(1); err == nil {
				h.Value = int64(int8(v[0]))
			}
		case TypeShort:
			if v, err = need(2); err == nil {
				h.Value = int64(int16(binary.BigEndian.Uint16(v)))
			}
		case TypeInt:
			if v, err = need(4); err == nil {
				h.Value = int64(int32(binary.BigEndian.Uint32(v)))
			}
		case TypeLong:
			if v, err = need(8); err == nil {
				h.Value = int64(binary.BigEndian.Uint64(v))
			}
		case TypeTimestamp:
			if v, err = need(8); err == nil {
				h.Value = time.UnixMilli(int64(binary.BigEndian.Uint64(v))).UTC()
			}
		case TypeUUID:
			if v, err = need(16); err == nil {
				h.Value = append([]byte(nil), v...)
			}
		case TypeBytes, TypeString:
			if v, err = need(2); err != nil {
				break
			}
			if v, err = need(int(binary.BigEndian.Uint16(v))); err != nil {
				break
			}
			if typ == TypeString {
				h.Value = string(v)
			} else {
				h.Value = append([]byte(nil), v...)
			}
		default:
			return nil, fmt.Errorf("%w: header %q has unknown type %d", ErrMalformed, h.Name, typ)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// Encode renders one message. Header values may be string, []byte, bool,
// int64 or time.Time; any other type is an error.
func Encode(headers []Header, payload []byte) ([]byte, error) {
	var hb []byte
	for _, h := range headers {
		if len(h.Name) == 0 || len(h.Name) > 255 {
			return nil, fmt.Errorf("eventstream: header name %q: length must be 1-255", h.Name)
		}
		hb = append(hb, byte(len(h.Name)))
		hb = append(hb, h.Name...)
		switch v := h.Value.(type) {
		case bool:
			if v {
				hb = append(hb, TypeTrue)
			} else {
				hb = append(hb, TypeFalse)
			}
		case int64:
			hb = append(hb, TypeLong)
			hb = binary.BigEndian.AppendUint64(hb, uint64(v))
		case time.Time:
			hb = append(hb, TypeTimestamp)
			hb = binary.BigEndian.AppendUint64(hb, uint64(v.UnixMilli()))
		case string:
			if len(v) > 0xffff {
				return nil, fmt.Errorf("eventstream: header %q too long", h.Name)
			}
			hb = append(hb, TypeString)
			hb = binary.BigEndian.AppendUint16(hb, uint16(len(v)))
			hb = append(hb, v...)
		case []byte:
			if len(v) > 0xffff {
				return nil, fmt.Errorf("eventstream: header %q too long", h.Name)
			}
			hb = append(hb, TypeBytes)
			hb = binary.BigEndian.AppendUint16(hb, uint16(len(v)))
			hb = append(hb, v...)
		default:
			return nil, fmt.Errorf("eventstream: header %q: unsupported value type %T", h.Name, h.Value)
		}
	}
	total := preludeLen + len(hb) + len(payload) + crcLen
	out := make([]byte, 0, total)
	out = binary.BigEndian.AppendUint32(out, uint32(total))
	out = binary.BigEndian.AppendUint32(out, uint32(len(hb)))
	out = binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
	out = append(out, hb...)
	out = append(out, payload...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out)), nil
}
