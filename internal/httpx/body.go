// Package httpx holds small HTTP helpers for the relay's server side:
// bounded body reading with decompression, path matching tolerant of common
// client mistakes, and client IP extraction behind trusted proxies.
package httpx

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Limits bound a request body. Zero means no limit.
type Limits struct {
	MaxCompressed   int64 // bytes on the wire
	MaxDecompressed int64 // bytes after decoding
}

// TooLargeError means the body exceeded a limit (HTTP 413).
type TooLargeError struct {
	Limit        int64
	Decompressed bool // the decoded size, not the wire size, was exceeded
}

func (e *TooLargeError) Error() string {
	if e.Decompressed {
		return fmt.Sprintf("request body exceeds %d bytes after decompression", e.Limit)
	}
	return fmt.Sprintf("request body exceeds %d bytes", e.Limit)
}

// UnsupportedEncodingError means Content-Encoding names a coding we do not
// decode (HTTP 415).
type UnsupportedEncodingError struct{ Encoding string }

func (e *UnsupportedEncodingError) Error() string {
	return fmt.Sprintf("unsupported content-encoding %q", e.Encoding)
}

// BadEncodingError means the body is not valid for its declared coding
// (HTTP 400).
type BadEncodingError struct {
	Encoding string
	Err      error
}

func (e *BadEncodingError) Error() string {
	return fmt.Sprintf("invalid %s body: %v", e.Encoding, e.Err)
}

func (e *BadEncodingError) Unwrap() error { return e.Err }

// Status maps a ReadBody error to an HTTP status.
func Status(err error) int {
	var tl *TooLargeError
	var ue *UnsupportedEncodingError
	var be *BadEncodingError
	switch {
	case err == nil:
		return http.StatusOK
	case errors.As(err, &tl):
		return http.StatusRequestEntityTooLarge
	case errors.As(err, &ue):
		return http.StatusUnsupportedMediaType
	case errors.As(err, &be):
		return http.StatusBadRequest
	}
	return http.StatusBadRequest
}

// maxCodings bounds stacked Content-Encoding values (gzip of gzip ...).
const maxCodings = 2

// ReadBody reads and decodes r.Body (identity, gzip, deflate, zstd, stacked
// in Content-Encoding order) within the limits. On success the request's
// Content-Encoding header is removed since the returned bytes are decoded.
func ReadBody(r *http.Request, lim Limits) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	if lim.MaxCompressed > 0 && r.ContentLength > lim.MaxCompressed {
		return nil, &TooLargeError{Limit: lim.MaxCompressed}
	}
	var codings []string
	for _, v := range r.Header.Values("Content-Encoding") {
		for _, c := range strings.Split(v, ",") {
			c = strings.ToLower(strings.TrimSpace(c))
			if c != "" && c != "identity" {
				codings = append(codings, c)
			}
		}
	}
	if len(codings) > maxCodings {
		return nil, &UnsupportedEncodingError{Encoding: strings.Join(codings, ", ")}
	}
	for _, c := range codings {
		switch c {
		case "gzip", "x-gzip", "deflate", "zstd":
		default:
			return nil, &UnsupportedEncodingError{Encoding: c}
		}
	}

	var src io.Reader = r.Body
	wire := &countingReader{r: r.Body, limit: lim.MaxCompressed}
	if lim.MaxCompressed > 0 {
		src = wire
	}
	var closers []func()
	defer func() {
		for _, c := range closers {
			c()
		}
	}()
	// Decode in reverse order of application.
	for i := len(codings) - 1; i >= 0; i-- {
		dec, closeFn, err := decoder(codings[i], src)
		if err != nil {
			if wire.over {
				return nil, &TooLargeError{Limit: lim.MaxCompressed}
			}
			return nil, &BadEncodingError{Encoding: codings[i], Err: err}
		}
		closers = append(closers, closeFn)
		src = dec
	}
	max := lim.MaxDecompressed
	if len(codings) == 0 && (max <= 0 || (lim.MaxCompressed > 0 && lim.MaxCompressed < max)) {
		max = lim.MaxCompressed
	}
	var buf bytes.Buffer
	reader := src
	if max > 0 {
		reader = io.LimitReader(src, max+1)
	}
	_, err := buf.ReadFrom(reader)
	if wire.over {
		return nil, &TooLargeError{Limit: lim.MaxCompressed}
	}
	if err != nil {
		if len(codings) > 0 {
			return nil, &BadEncodingError{Encoding: codings[0], Err: err}
		}
		return nil, err
	}
	if max > 0 && int64(buf.Len()) > max {
		return nil, &TooLargeError{Limit: max, Decompressed: len(codings) > 0}
	}
	r.Header.Del("Content-Encoding")
	return buf.Bytes(), nil
}

func decoder(coding string, src io.Reader) (io.Reader, func(), error) {
	switch coding {
	case "gzip", "x-gzip":
		z, err := gzip.NewReader(src)
		if err != nil {
			return nil, nil, err
		}
		return z, func() { _ = z.Close() }, nil
	case "deflate":
		// HTTP "deflate" is zlib-wrapped, but some clients send raw deflate.
		br := bufio.NewReader(src)
		if hdr, err := br.Peek(2); err == nil && (uint16(hdr[0])<<8|uint16(hdr[1]))%31 == 0 && hdr[0]&0x0f == 8 {
			z, err := zlib.NewReader(br)
			if err != nil {
				return nil, nil, err
			}
			return z, func() { _ = z.Close() }, nil
		}
		f := flate.NewReader(br)
		return f, func() { _ = f.Close() }, nil
	case "zstd":
		// The window cap bounds decoder memory; output size is bounded by
		// the caller's limit reader.
		z, err := zstd.NewReader(src, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(64<<20))
		if err != nil {
			return nil, nil, err
		}
		return z, z.Close, nil
	}
	return nil, nil, &UnsupportedEncodingError{Encoding: coding}
}

type countingReader struct {
	r     io.Reader
	n     int64
	limit int64
	over  bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.limit > 0 && c.n >= c.limit {
		// Probe one byte to tell "exactly at limit" from "over".
		var one [1]byte
		n, err := c.r.Read(one[:])
		if n > 0 {
			c.over = true
			return 0, errors.New("compressed body too large")
		}
		return 0, err
	}
	if c.limit > 0 && int64(len(p)) > c.limit-c.n {
		p = p[:c.limit-c.n]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
