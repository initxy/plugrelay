package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/initxy/plugrelay/internal/store"
)

// maxBody bounds admin request bodies.
const maxBody = 4 << 20

// call is one admin request in flight.
type call struct {
	h   *Handler
	w   http.ResponseWriter
	r   *http.Request
	ctx context.Context
	who *identity
}

// apiError is rendered as the OpenAPI Error body: a code and its params
// (see errtext.go) plus the English message.
type apiError struct {
	Status  int            `json:"-"`
	Code    string         `json:"code"`
	Params  map[string]any `json:"params,omitempty"`
	Message string         `json:"message"`
	Fields  []fieldError   `json:"fields,omitempty"`
}

type fieldError struct {
	Path    string         `json:"path"`
	Code    string         `json:"code"`
	Params  map[string]any `json:"params,omitempty"`
	Message string         `json:"message"`
}

func (e *apiError) Error() string { return e.Message }

func newError(status int, t worded) *apiError {
	w := t.wording()
	return &apiError{Status: status, Code: w.code, Params: w.params, Message: w.message}
}

func errNotFound(t worded) *apiError     { return newError(http.StatusNotFound, t) }
func errConflict(t worded) *apiError     { return newError(http.StatusConflict, t) }
func errForbidden(t worded) *apiError    { return newError(http.StatusForbidden, t) }
func errUnauthorized(t worded) *apiError { return newError(http.StatusUnauthorized, t) }
func errBadRequest(t worded) *apiError   { return newError(http.StatusBadRequest, t) }
func errUpstream(t worded) *apiError     { return newError(http.StatusBadGateway, t) }

// invalid collects field errors for a 422.
type invalid []fieldError

func (v *invalid) add(path string, t worded) {
	w := t.wording()
	*v = append(*v, fieldError{Path: path, Code: w.code, Params: w.params, Message: w.message})
}

// addErr adds a validator's error: its wording when it has one, else its
// text as the reason of value_rejected.
func (v *invalid) addErr(path string, err error) {
	var w worded
	if errors.As(err, &w) {
		v.add(path, w)
		return
	}
	v.add(path, eValueRejected.with("reason", err.Error()))
}

func (v invalid) err() error {
	if len(v) == 0 {
		return nil
	}
	e := newError(http.StatusUnprocessableEntity, eInvalid.with("count", len(v)))
	if len(v) == 1 {
		e.Message = v[0].Path + ": " + v[0].Message
	}
	e.Fields = v
	return e
}

func fieldInvalid(path string, t worded) error {
	var v invalid
	v.add(path, t)
	return v.err()
}

// storeErr maps store errors to API errors about obj.
func storeErr(err error, obj errValue) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return errNotFound(eNotFound.with("object", obj))
	case errors.Is(err, store.ErrConflict):
		return errConflict(eConflict.with("object", obj))
	case errors.Is(err, store.ErrNoBox):
		return errConflict(eNoMasterKey)
	}
	return err
}

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = newError(http.StatusInternalServerError, eInternal)
	}
	body := map[string]any{"error": ae}
	if id := w.Header().Get("X-Request-Id"); id != "" {
		body = map[string]any{"error": struct {
			*apiError
			RequestID string `json:"request_id"`
		}{ae, id}}
	}
	writeJSON(w, ae.Status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (c *call) json(status int, v any) error { writeJSON(c.w, status, v); return nil }

func (c *call) noContent() error { c.w.WriteHeader(http.StatusNoContent); return nil }

// decode reads a JSON body into v; an empty body leaves v as is.
func (c *call) decode(v any) error {
	data, err := io.ReadAll(io.LimitReader(c.r.Body, maxBody+1))
	if err != nil {
		return errBadRequest(eBodyUnreadable)
	}
	if len(data) > maxBody {
		return newError(http.StatusRequestEntityTooLarge, eBodyTooLarge)
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return errBadRequest(eInvalidJSON.with("reason", err.Error()))
	}
	return nil
}

func (c *call) query(name string) string { return c.r.URL.Query().Get(name) }

func (c *call) intQuery(name string, def, lo, hi int) int {
	n, err := strconv.Atoi(c.query(name))
	if err != nil {
		return def
	}
	return max(lo, min(hi, n))
}

func (c *call) timeQuery(name string) (time.Time, error) {
	v := c.query(name)
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fieldInvalid(name, eDateTime)
	}
	return t, nil
}
