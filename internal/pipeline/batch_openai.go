package pipeline

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/initxy/plugrelay/pkg/ir"
)

// openAIBatchEndpoints maps a batch line's url to the protocol it speaks.
var openAIBatchEndpoints = map[string]string{
	"/v1/chat/completions": "openai-chat",
	"/v1/responses":        "openai-responses",
	"/v1/embeddings":       "openai-embeddings",
	"/v1/completions":      "openai-completions",
}

func (b *batchReq) serveOpenAI() {
	rt, m := b.route, b.r.Method
	switch {
	case rt.resource == "files" && rt.id == "" && m == http.MethodPost:
		b.uploadFile()
	case rt.resource == "files" && rt.id == "" && m == http.MethodGet:
		b.listFiles()
	case rt.resource == "files" && rt.action == "" && (m == http.MethodGet || m == http.MethodDelete):
		b.fileCall()
	case rt.resource == "files" && rt.action == "content" && m == http.MethodGet:
		b.fileContent()
	case rt.resource == "batches" && rt.id == "" && m == http.MethodPost:
		b.createBatch()
	case rt.resource == "batches" && rt.id == "" && m == http.MethodGet:
		b.listOpenAIBatches()
	case rt.resource == "batches" && rt.action == "" && m == http.MethodGet,
		rt.resource == "batches" && rt.action == "cancel" && m == http.MethodPost:
		b.batchCall()
	default:
		b.fail(&ir.GwError{Kind: ir.ErrInvalidRequest, Status: http.StatusMethodNotAllowed, Message: "method not allowed"})
	}
}

// uploadFile streams a batch input file to the credential its first
// line's model routes to. Every line must name the same url and model;
// when the upstream model has another name, each line's body.model is
// rewritten, otherwise lines pass byte for byte.
func (b *batchReq) uploadFile() {
	mt, params, err := mime.ParseMediaType(b.r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" || b.r.Body == nil {
		b.fail(ir.Errorf(ir.ErrInvalidRequest, "expected a multipart/form-data body"))
		return
	}
	u := &upload{}
	mr := multipart.NewReader(http.MaxBytesReader(b.w, b.r.Body, b.p.d.BatchMaxBody), params["boundary"])
	budget := int64(maxFormFields)
	var file *multipart.Part
	for file == nil {
		p, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			b.fail(u.readError(err))
			return
		}
		if p.FileName() != "" {
			file = p
			break
		}
		if gw := u.addField(p, &budget); gw != nil {
			b.fail(gw)
			return
		}
	}
	purpose := fieldValue(u, "purpose")
	switch {
	case file == nil:
		b.fail(ir.Errorf(ir.ErrInvalidRequest, "file is required"))
		return
	case purpose != "" && purpose != "batch":
		b.fail(ir.Errorf(ir.ErrInvalidRequest, "only purpose=batch files are served by the relay (got %q)", purpose))
		return
	}
	br := bufio.NewReaderSize(file, 64<<10)
	first, err := readLine(br, int(b.p.d.MaxResponseBody))
	if err != nil && !errors.Is(err, io.EOF) {
		b.fail(u.readError(err))
		return
	}
	lineURL := gjson.GetBytes(first, "url").String()
	model := gjson.GetBytes(first, "body.model").String()
	proto, ok := openAIBatchEndpoints[lineURL]
	if !ok {
		b.fail(ir.Errorf(ir.ErrInvalidRequest, "line 1: url %q is not a batch endpoint", lineURL))
		return
	}
	t, gw := b.place(model, proto)
	if gw != nil {
		b.fail(gw)
		return
	}
	content, ok := b.admit(model)
	if !ok {
		return
	}
	hooks, gw := b.lineHooks(content, t, proto, lineURL, model)
	if gw != nil {
		b.fail(gw)
		return
	}
	if !b.gate() {
		return
	}
	lw := &lineCheck{url: lineURL, model: model, upstream: t.cand.UpstreamModel, proto: proto, hooks: hooks}
	pr, pw := io.Pipe()
	defer pr.Close()
	mw := multipart.NewWriter(pw)
	var mu sync.Mutex
	var bad *ir.GwError
	firstErr := err
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := b.writeUpload(mw, u, file, br, first, firstErr, mr, lw, &purpose)
		var gw *ir.GwError
		if errors.As(err, &gw) {
			mu.Lock()
			bad = gw
			mu.Unlock()
		}
		pw.CloseWithError(err)
	}()
	resp, err := b.send(t, http.MethodPost, "/files", pr, http.Header{"Content-Type": {mw.FormDataContentType()}})
	// The upstream answered: the lines (and their estimate) are final.
	_ = pr.Close()
	<-done
	mu.Lock()
	failed := bad
	mu.Unlock()
	if failed == nil && u.tooBig.Load() {
		failed = tooLarge(u.limit.Load())
	}
	if failed != nil {
		if resp != nil {
			resp.Body.Close()
		}
		b.fail(failed)
		return
	}
	if err != nil {
		b.upstreamFailed(err)
		return
	}
	defer resp.Body.Close()
	body, err := readBounded(resp.Body, b.p.d.MaxResponseBody)
	if err != nil {
		b.upstreamFailed(err)
		return
	}
	if id := gjson.GetBytes(body, "id").String(); ok2xx(resp.StatusCode) && id != "" {
		b.save(b.owner(&BatchObject{Kind: BatchKindFile, ID: id, Protocol: proto, Model: model, Role: roleInput, Object: body,
			InputTokens: lw.in, OutputTokens: lw.out}, t))
	}
	b.reply(resp, body)
}

// writeUpload re-frames the parts for the upstream: held fields, the file
// line by line, then the remaining parts.
func (b *batchReq) writeUpload(mw *multipart.Writer, u *upload, file *multipart.Part, br *bufio.Reader, line []byte, lerr error,
	mr *multipart.Reader, lw *lineCheck, purpose *string) error {
	for i, h := range u.head {
		if err := writePart(mw, h, bytes.NewReader(u.vals[i])); err != nil {
			return err
		}
	}
	fw, err := mw.CreatePart(file.Header)
	if err != nil {
		return err
	}
	max := int(b.p.d.MaxResponseBody)
	for {
		if len(line) > 0 {
			out, gw := lw.check(line)
			if gw != nil {
				return gw
			}
			if _, err := fw.Write(out); err != nil {
				return err
			}
		}
		if lerr != nil {
			if errors.Is(lerr, io.EOF) {
				break
			}
			return u.sourceError(lerr)
		}
		line, lerr = readLine(br, max)
	}
	for {
		p, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return u.sourceError(err)
		}
		if p.FileName() == "" {
			v, err := io.ReadAll(io.LimitReader(p, maxFormFields))
			if err != nil {
				return u.sourceError(err)
			}
			if p.FormName() == "purpose" {
				*purpose = string(v)
			}
			if err := writePart(mw, p.Header, bytes.NewReader(v)); err != nil {
				return err
			}
			continue
		}
		if err := writePart(mw, p.Header, p); err != nil {
			return u.sourceError(err)
		}
	}
	if *purpose != "batch" {
		return ir.Errorf(ir.ErrInvalidRequest, "purpose must be \"batch\" (got %q)", *purpose)
	}
	return mw.Close()
}

func fieldValue(u *upload, name string) string {
	for i, h := range u.head {
		if formName(h) == name {
			return string(u.vals[i])
		}
	}
	return ""
}

// readLine reads one line including its newline; a line longer than max
// is an error.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := br.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > max {
			return nil, ir.Errorf(ir.ErrInvalidRequest, "a batch line exceeds %d bytes", max)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return out, err
	}
}

// lineCheck validates batch input lines, runs the content plugins on
// them, sums their token estimate and renames their model.
type lineCheck struct {
	url, model, upstream, proto string
	hooks                       *lineHooks
	n                           int
	in, out                     int64 // token estimate so far
}

func (c *lineCheck) check(line []byte) ([]byte, *ir.GwError) {
	c.n++
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return line, nil
	}
	if !gjson.ValidBytes(trimmed) {
		return nil, ir.Errorf(ir.ErrInvalidRequest, "line %d is not valid JSON", c.n)
	}
	if gw := duplicateKey(gjson.ParseBytes(trimmed), "line "+strconv.Itoa(c.n)); gw != nil {
		return nil, gw
	}
	if u := gjson.GetBytes(trimmed, "url").String(); u != c.url {
		return nil, ir.Errorf(ir.ErrInvalidRequest, "line %d: every request of a batch must use url %q (got %q)", c.n, c.url, u)
	}
	body := gjson.GetBytes(trimmed, "body")
	if m := body.Get("model").String(); m != c.model {
		return nil, ir.Errorf(ir.ErrInvalidRequest, "line %d: every request of a batch must use model %q (got %q)", c.n, c.model, m)
	}
	checked, gw := c.hooks.check("line "+strconv.Itoa(c.n), []byte(body.Raw))
	if gw != nil {
		return nil, gw
	}
	li, lo := lineTokens(gjson.ParseBytes(checked), c.proto)
	c.in, c.out = c.in+li, c.out+lo
	out := line
	var err error
	if !bytes.Equal(checked, []byte(body.Raw)) {
		if out, err = sjson.SetRawBytes(trimmed, "body", checked); err != nil {
			return nil, ir.Errorf(ir.ErrInvalidRequest, "line %d: %v", c.n, err)
		}
		out = append(out, '\n')
	}
	if c.upstream == "" || c.upstream == c.model {
		return out, nil
	}
	if out, err = sjson.SetBytes(bytes.TrimSpace(out), "body.model", c.upstream); err != nil {
		return nil, ir.Errorf(ir.ErrInvalidRequest, "line %d: %v", c.n, err)
	}
	return append(out, '\n'), nil
}

// batchLineOutput is the output limit assumed for a batch request that
// names none, for holding its estimate.
const batchLineOutput = 4096

// lineTokens estimates one batch request: a token per three bytes of its
// body (generous for English, close for CJK) and its output limit.
func lineTokens(body gjson.Result, proto string) (in, out int64) {
	in = int64(len(body.Raw))/3 + 1
	if proto == "openai-embeddings" {
		return in, 0
	}
	for _, f := range []string{"max_completion_tokens", "max_output_tokens", "max_tokens"} {
		if v := body.Get(f); v.Exists() && v.Int() > 0 {
			return in, v.Int()
		}
	}
	return in, batchLineOutput
}

// fileCall retrieves or deletes one file.
func (b *batchReq) fileCall() {
	o, ok := b.lookup(BatchKindFile, b.route.id, "file")
	if !ok {
		return
	}
	t, gw := b.targetOf(o)
	if gw != nil {
		b.fail(gw)
		return
	}
	var parent *BatchObject
	if b.r.Method == http.MethodDelete && o.Role == roleOutput {
		// Deleting the output file makes the results unreadable.
		parent = b.billTarget(o)
		if parent != nil && parent.Kind == BatchKindBatch && gjson.GetBytes(parent.Object, "output_file_id").String() == o.ID {
			b.collectFirst(parent)
		} else {
			parent = nil
		}
	}
	resp, body, err := b.sendJSON(t, b.r.Method, "/files/"+url.PathEscape(o.ID), nil)
	if err != nil {
		b.upstreamFailed(err)
		return
	}
	if ok2xx(resp.StatusCode) {
		if b.r.Method == http.MethodDelete {
			if parent != nil && parent.BilledAt.IsZero() {
				b.closeUnbilled(parent)
			}
			_ = b.store.DeleteBatch(b.ctx(), o.Kind, o.ID)
		} else {
			b.refresh(o, body)
		}
	}
	b.reply(resp, body)
}

// fileContent downloads a file; a batch's output file is billed on its
// first complete read.
func (b *batchReq) fileContent() {
	o, ok := b.lookup(BatchKindFile, b.route.id, "file")
	if !ok {
		return
	}
	t, gw := b.targetOf(o)
	if gw != nil {
		b.fail(gw)
		return
	}
	var bill *BatchObject
	if o.Role == roleOutput {
		bill = b.billTarget(o)
	}
	b.detach = bill != nil && bill.BilledAt.IsZero()
	resp, err := b.send(t, http.MethodGet, "/files/"+url.PathEscape(o.ID)+"/content", nil, nil)
	if err != nil {
		b.upstreamFailed(err)
		return
	}
	b.streamResults(bill, resp)
}

// billTarget is what an output file's results are billed to: its batch,
// or the file itself when the batch is not on record; nil when the store
// cannot tell, leaving the results to SettleBatches.
func (b *batchReq) billTarget(f *BatchObject) *BatchObject {
	if f.BatchID == "" {
		return f
	}
	o, err := b.store.GetBatch(b.ctx(), BatchKindBatch, f.BatchID)
	if err != nil {
		b.p.d.Logger.Warn("batch of an output file not read; its results are billed later", "file", f.ID, "err", err)
		return nil
	}
	if o == nil {
		return f
	}
	return o
}

// listFiles lists the caller's files from the relay's records.
func (b *batchReq) listFiles() {
	q := b.r.URL.Query()
	objs, more, err := b.store.ListBatches(b.ctx(), BatchQuery{Kind: BatchKindFile, UserID: b.caller.UserID, KeyID: b.caller.KeyID,
		After: q.Get("after"), Limit: listLimit(b.r, 100, 10000)})
	if err != nil {
		b.fail(ir.Errorf(ir.ErrInternal, "batch store: %v", err))
		return
	}
	purpose := q.Get("purpose")
	data := []any{}
	for _, o := range objs {
		if purpose != "" && purpose != o.Role {
			continue
		}
		if len(o.Object) > 0 {
			data = append(data, o.Object)
		} else {
			data = append(data, map[string]any{"id": o.ID, "object": "file", "purpose": o.Role, "created_at": o.CreatedAt.Unix()})
		}
	}
	b.replyJSON(listPage(objs, data, more, "list"))
}

func listPage(objs []BatchObject, data []any, more bool, object string) map[string]any {
	out := map[string]any{"data": data, "has_more": more, "first_id": nil, "last_id": nil}
	if object != "" {
		out["object"] = object
	}
	if len(objs) > 0 {
		out["first_id"], out["last_id"] = objs[0].ID, objs[len(objs)-1].ID
	}
	return out
}

// createBatch starts a batch on the credential holding its input file.
func (b *batchReq) createBatch() {
	body, gw := b.readJSON()
	if gw != nil {
		b.fail(gw)
		return
	}
	fileID := gjson.GetBytes(body, "input_file_id").String()
	f, ok := b.lookup(BatchKindFile, fileID, "file")
	if !ok {
		return
	}
	if ep := gjson.GetBytes(body, "endpoint").String(); openAIBatchEndpoints[ep] != f.Protocol {
		b.fail(ir.Errorf(ir.ErrInvalidRequest, "endpoint %q does not match the requests of file %s", ep, f.ID))
		return
	}
	t, gw := b.targetOf(f)
	if gw != nil {
		b.fail(gw)
		return
	}
	if _, ok := b.admit(f.Model); !ok {
		return
	}
	o := b.owner(&BatchObject{Kind: BatchKindBatch, Protocol: f.Protocol, Model: f.Model}, t)
	hold, ok := b.hold(o, f.InputTokens, f.OutputTokens)
	if !ok {
		return
	}
	resp, out, err := b.sendJSON(t, http.MethodPost, "/batches", body)
	if err != nil {
		b.release(hold)
		b.upstreamFailed(err)
		return
	}
	if id := gjson.GetBytes(out, "id").String(); ok2xx(resp.StatusCode) && id != "" {
		o.ID, o.Object, o.HoldID = id, out, hold
		b.save(o)
		b.noteFiles(o, out)
		b.ended(o, out)
	} else {
		b.release(hold)
	}
	b.reply(resp, out)
}

// batchCall retrieves or cancels one batch.
func (b *batchReq) batchCall() {
	o, ok := b.lookup(BatchKindBatch, b.route.id, "batch")
	if !ok {
		return
	}
	t, gw := b.targetOf(o)
	if gw != nil {
		b.fail(gw)
		return
	}
	path := "/batches/" + url.PathEscape(o.ID)
	method := http.MethodGet
	if b.route.action == "cancel" {
		path, method = path+"/cancel", http.MethodPost
	}
	resp, out, err := b.sendJSON(t, method, path, nil)
	if err != nil {
		b.upstreamFailed(err)
		return
	}
	if ok2xx(resp.StatusCode) {
		b.refresh(o, out)
		b.noteFiles(o, out)
		b.ended(o, out)
	}
	b.reply(resp, out)
}

// noteFiles records a batch's output and error files on its credential,
// so they can be downloaded (and the output billed).
func (b *batchReq) noteFiles(o *BatchObject, obj []byte) {
	for role, field := range map[string]string{roleOutput: "output_file_id", roleError: "error_file_id"} {
		id := gjson.GetBytes(obj, field).String()
		if id == "" {
			continue
		}
		if cur, err := b.store.GetBatch(b.ctx(), BatchKindFile, id); err != nil || cur != nil {
			continue
		}
		f := *o
		f.Kind, f.ID, f.Role, f.BatchID, f.Object = BatchKindFile, id, role, o.ID, nil
		f.BilledAt, f.CreatedAt = time.Time{}, b.p.d.Now()
		b.save(&f)
	}
}

// listOpenAIBatches lists the caller's batches, refreshing those not
// finished yet.
func (b *batchReq) listOpenAIBatches() {
	objs, more, err := b.store.ListBatches(b.ctx(), BatchQuery{Kind: BatchKindBatch, UserID: b.caller.UserID, KeyID: b.caller.KeyID,
		After: b.r.URL.Query().Get("after"), Limit: listLimit(b.r, 20, 100)})
	if err != nil {
		b.fail(ir.Errorf(ir.ErrInternal, "batch store: %v", err))
		return
	}
	data := b.refreshAll(objs, func(o *BatchObject) string { return "/batches/" + url.PathEscape(o.ID) },
		map[string]bool{"completed": true, "failed": true, "expired": true, "cancelled": true}, "status")
	b.replyJSON(listPage(objs, data, more, "list"))
}

// refreshAll returns each object as the upstream reports it now, or as
// last stored when it is in a final state or the upstream does not
// answer. At most eight calls run at once.
func (b *batchReq) refreshAll(objs []BatchObject, path func(*BatchObject) string, final map[string]bool, statusField string) []any {
	raws := make([]json.RawMessage, len(objs))
	b.quiet = true
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := range objs {
		o := &objs[i]
		raws[i] = o.Object
		if final[gjson.GetBytes(o.Object, statusField).String()] {
			continue
		}
		t, gw := b.targetOf(o)
		if gw != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			resp, out, err := b.sendJSON(t, http.MethodGet, path(o), nil)
			if err != nil || !ok2xx(resp.StatusCode) || !gjson.ValidBytes(out) {
				return
			}
			b.refresh(o, out)
			if o.Kind == BatchKindBatch {
				b.noteFiles(o, out)
			}
			b.ended(o, out)
			raws[i] = out
		}()
	}
	wg.Wait()
	data := make([]any, len(objs))
	for i := range objs {
		if len(raws[i]) == 0 {
			data[i] = map[string]string{"id": objs[i].ID}
			continue
		}
		data[i] = json.RawMessage(b.present(&objs[i], raws[i]))
	}
	return data
}

// readJSON reads a bounded JSON request body.
func (b *batchReq) readJSON() ([]byte, *ir.GwError) {
	if b.r.Body == nil {
		return nil, ir.Errorf(ir.ErrInvalidRequest, "empty request body")
	}
	body, err := io.ReadAll(http.MaxBytesReader(b.w, b.r.Body, b.p.d.BatchMaxBody))
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			return nil, tooLarge(mb.Limit)
		}
		return nil, ir.Errorf(ir.ErrInvalidRequest, "reading the request body: %v", err)
	}
	if !gjson.ValidBytes(body) {
		return nil, ir.Errorf(ir.ErrInvalidRequest, "request body is not valid JSON")
	}
	return body, nil
}
