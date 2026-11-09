package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/initxy/plugrelay/pkg/ir"
)

// maxFormFields bounds the text fields of an upload held in memory.
const maxFormFields = 1 << 20

// upload is a multipart request body that is not read into memory. The
// text fields before the first file are read up front; when they name the
// model, the rest is streamed to the first attempt's upstream as it
// arrives, so only that attempt can use it. When the model comes after a
// file, the body is first copied to a temporary file (bounded by the body
// limit), which every attempt can replay. Each attempt gets the parts
// re-framed under a fresh boundary, the model field set to the upstream
// model, every other part byte for byte.
type upload struct {
	head  []textproto.MIMEHeader // text parts held in memory, in order
	vals  [][]byte
	json  []byte // the fields as a JSON object, for the codec
	model string

	mr    *multipart.Reader // stream mode: the parts after first
	first *multipart.Part   // stream mode: the first file part
	spool string            // spool mode: path of a re-framed copy
	sb    string            // spool mode: its boundary

	audio *audioProbe // the first file as it passed, for its duration

	started atomic.Bool  // stream mode: an attempt began reading the client
	tooBig  atomic.Bool  // the client body exceeded the limit
	limit   atomic.Int64 // that limit
}

var errUploadSpent = errors.New("pipeline: upload already streamed to an earlier attempt")

// readUpload reads the form fields of a multipart body up to the first
// file part, or to the end when the model comes later.
func readUpload(r *http.Request) (*upload, *ir.GwError) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return nil, ir.Errorf(ir.ErrInvalidRequest, "expected a multipart/form-data body")
	}
	if r.Body == nil {
		return nil, ir.Errorf(ir.ErrInvalidRequest, "empty request body")
	}
	u := &upload{audio: &audioProbe{}}
	mr := multipart.NewReader(r.Body, params["boundary"])
	budget := int64(maxFormFields)
	for {
		p, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, u.readError(err)
		}
		if p.FileName() == "" {
			if gw := u.addField(p, &budget); gw != nil {
				return nil, gw
			}
			continue
		}
		if u.model != "" {
			u.mr, u.first = mr, p
			break
		}
		if gw := u.spoolRest(mr, p, &budget); gw != nil {
			u.close()
			return nil, gw
		}
		break
	}
	u.json = u.fieldsJSON()
	return u, nil
}

func (u *upload) addField(p *multipart.Part, budget *int64) *ir.GwError {
	v, err := io.ReadAll(io.LimitReader(p, *budget+1))
	if err != nil {
		return u.readError(err)
	}
	if *budget -= int64(len(v)); *budget < 0 {
		return &ir.GwError{Kind: ir.ErrInvalidRequest, Status: http.StatusRequestEntityTooLarge,
			Message: "form fields exceed 1 MiB"}
	}
	u.head = append(u.head, p.Header)
	u.vals = append(u.vals, v)
	if p.FormName() == "model" && u.model == "" {
		u.model = string(v)
	}
	return nil
}

// spoolRest copies first and every later part to a temporary file,
// collecting the text fields on the way.
func (u *upload) spoolRest(mr *multipart.Reader, first *multipart.Part, budget *int64) *ir.GwError {
	f, err := os.CreateTemp("", "plugrelay-upload-*")
	if err != nil {
		return ir.Errorf(ir.ErrInternal, "upload: %v", err)
	}
	u.spool = f.Name()
	defer f.Close()
	mw := multipart.NewWriter(f)
	u.sb = mw.Boundary()
	for i, h := range u.head {
		if err := writePart(mw, h, bytes.NewReader(u.vals[i])); err != nil {
			return u.readError(err)
		}
	}
	for p := first; ; {
		if p.FileName() == "" {
			if gw := u.addField(p, budget); gw != nil {
				return gw
			}
			if err := writePart(mw, p.Header, bytes.NewReader(u.vals[len(u.vals)-1])); err != nil {
				return u.readError(err)
			}
		} else if err := writePart(mw, p.Header, u.audio.tee(p)); err != nil {
			return u.readError(err)
		}
		var err error
		if p, err = mr.NextRawPart(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return u.readError(err)
		}
	}
	if err := mw.Close(); err != nil {
		return ir.Errorf(ir.ErrInternal, "upload: %v", err)
	}
	return nil
}

func writePart(mw *multipart.Writer, h textproto.MIMEHeader, src io.Reader) error {
	w, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, src)
	return err
}

// readError maps a failure reading the client body: over the limit is a
// 413, anything else a malformed body.
func (u *upload) readError(err error) *ir.GwError {
	var mb *http.MaxBytesError
	if errors.As(err, &mb) {
		u.limit.Store(mb.Limit)
		u.tooBig.Store(true)
		return tooLarge(mb.Limit)
	}
	return ir.Errorf(ir.ErrInvalidRequest, "malformed multipart body: %v", err)
}

func tooLarge(limit int64) *ir.GwError {
	return &ir.GwError{Kind: ir.ErrInvalidRequest, Status: http.StatusRequestEntityTooLarge,
		Message: "request body exceeds " + strconv.FormatInt(limit, 10) + " bytes"}
}

// fieldsJSON renders the text fields as a JSON object (repeated names as
// arrays) plus the first file as {"filename","content_type"}.
func (u *upload) fieldsJSON() []byte {
	obj := map[string]any{}
	add := func(k string, v any) {
		switch cur := obj[k].(type) {
		case nil:
			obj[k] = v
		case []any:
			obj[k] = append(cur, v)
		default:
			obj[k] = []any{cur, v}
		}
	}
	for i, h := range u.head {
		add(formName(h), string(u.vals[i]))
	}
	if u.first != nil {
		add(u.first.FormName(), map[string]string{"filename": u.first.FileName(), "content_type": u.first.Header.Get("Content-Type")})
	}
	b, _ := json.Marshal(obj)
	return b
}

func formName(h textproto.MIMEHeader) string {
	_, params, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	return params["name"]
}

// replayable reports whether every attempt can send the body.
func (u *upload) replayable() bool { return u.first == nil }

// spent reports that a streamed body was consumed by an attempt, so no
// other attempt can send it.
func (u *upload) spent() bool { return !u.replayable() && u.started.Load() }

// body returns the request body of one attempt with the model field set to
// model, and its content type. progress is called on every chunk read from
// the client.
func (u *upload) body(model string, progress func()) (io.ReadCloser, string, error) {
	if u.spent() {
		return nil, "", errUploadSpent
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	run := func() {
		err := u.write(mw, model, progress)
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}
	return &lazyBody{r: pr, start: func() { go run() }, started: &u.started}, mw.FormDataContentType(), nil
}

// write sends the parts: the held fields, then the streamed or spooled ones.
func (u *upload) write(mw *multipart.Writer, model string, progress func()) error {
	if u.spool != "" {
		f, err := os.Open(u.spool)
		if err != nil {
			return err
		}
		defer f.Close()
		return u.copyParts(mw, multipart.NewReader(f, u.sb), nil, model, nil)
	}
	for i, h := range u.head {
		v := u.vals[i]
		if formName(h) == "model" {
			v = []byte(model)
		}
		if err := writePart(mw, h, bytes.NewReader(v)); err != nil {
			return err
		}
	}
	if u.first == nil {
		return nil
	}
	return u.copyParts(mw, u.mr, u.first, model, progress)
}

// copyParts copies first (when set) and every remaining part of mr.
func (u *upload) copyParts(mw *multipart.Writer, mr *multipart.Reader, first *multipart.Part, model string, progress func()) error {
	p := first
	for {
		if p == nil {
			var err error
			if p, err = mr.NextRawPart(); errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return u.sourceError(err)
			}
		}
		var src io.Reader = &progressReader{r: p, progress: progress}
		if p.FileName() == "" && p.FormName() == "model" {
			src = bytes.NewReader([]byte(model))
		} else if p.FileName() != "" && u.spool == "" {
			src = u.audio.tee(src)
		}
		w, err := mw.CreatePart(p.Header)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, src); err != nil {
			return u.sourceError(err)
		}
		p = nil
	}
}

func (u *upload) sourceError(err error) error {
	var mb *http.MaxBytesError
	if errors.As(err, &mb) {
		u.limit.Store(mb.Limit)
		u.tooBig.Store(true)
	}
	return err
}

// audioMs estimates the duration of the uploaded audio file.
func (u *upload) audioMs() int64 {
	if u == nil {
		return 0
	}
	return u.audio.ms()
}

// close removes the spooled copy.
func (u *upload) close() {
	if u != nil && u.spool != "" {
		_ = os.Remove(u.spool)
	}
}

// lazyBody starts producing the body on the first read, so an attempt
// that never sends (a dial failure) consumes nothing of the client body.
type lazyBody struct {
	r       *io.PipeReader
	once    sync.Once
	start   func()
	started *atomic.Bool
}

func (b *lazyBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		b.started.Store(true)
		b.start()
	})
	return b.r.Read(p)
}

func (b *lazyBody) Close() error { return b.r.Close() }

type progressReader struct {
	r        io.Reader
	progress func()
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 && p.progress != nil {
		p.progress()
	}
	return n, err
}
