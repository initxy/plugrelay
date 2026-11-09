package pipeline

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
)

// failAfter yields n bytes of r, then a broken connection.
type failAfter struct {
	r io.Reader
	n int
}

func (f *failAfter) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("connection reset by peer")
	}
	if len(p) > f.n {
		p = p[:f.n]
	}
	n, err := f.r.Read(p)
	f.n -= n
	return n, err
}

// A body whose model comes after the file is spooled to disk; every way
// that spooling can fail must remove the file.
func TestUploadSpoolRemovedOnFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	form := func(extra func(*multipart.Writer)) ([]byte, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "a.wav")
		fw.Write(bytes.Repeat([]byte("x"), 64<<10))
		if extra != nil {
			extra(mw)
		}
		mw.WriteField("model", "whisper-1")
		mw.Close()
		return buf.Bytes(), mw.FormDataContentType()
	}
	body, ct := form(nil)
	bigFields, bigCT := form(func(mw *multipart.Writer) { mw.WriteField("prompt", strings.Repeat("p", maxFormFields+1)) })
	for _, tc := range []struct {
		name   string
		ct     string
		body   func() io.Reader
		limit  int64
		status int // 0: the kind's default
	}{
		{"over the body limit", ct, func() io.Reader { return bytes.NewReader(body) }, 32 << 10, http.StatusRequestEntityTooLarge},
		{"truncated", ct, func() io.Reader { return bytes.NewReader(body[:40<<10]) }, 1 << 30, 0},
		{"client gone", ct, func() io.Reader { return &failAfter{r: bytes.NewReader(body), n: 40 << 10} }, 1 << 30, 0},
		{"fields too large", bigCT, func() io.Reader { return bytes.NewReader(bigFields) }, 1 << 30, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/audio/transcriptions", tc.body())
			r.Header.Set("Content-Type", tc.ct)
			r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, tc.limit)
			u, gw := readUpload(r)
			if gw == nil || u != nil {
				t.Fatalf("want an error, got upload %v", u)
			}
			if gw.Kind != ir.ErrInvalidRequest || gw.Status != tc.status {
				t.Fatalf("got %s status %d, want %d", gw, gw.Status, tc.status)
			}
			if left, _ := filepath.Glob(filepath.Join(dir, "plugrelay-upload-*")); len(left) != 0 {
				t.Fatalf("spool files left behind: %v", left)
			}
		})
	}
	// The success path keeps the spool until the exchange closes it.
	r := httptest.NewRequest("POST", "/v1/audio/transcriptions", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)
	u, gw := readUpload(r)
	if gw != nil || u.spool == "" {
		t.Fatalf("spool: %v %q", gw, u.spool)
	}
	u.close()
	if left, _ := filepath.Glob(filepath.Join(dir, "plugrelay-upload-*")); len(left) != 0 {
		t.Fatalf("spool kept after close: %v", left)
	}
}
