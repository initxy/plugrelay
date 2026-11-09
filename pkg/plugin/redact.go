package plugin

import (
	"net/url"
	"strings"
)

// RedactURLError returns err with the query of any wrapped *url.Error cut
// off. Go's HTTP client names the full request URL in transport errors
// ("Get \"https://host/v1/x?key=…\": connection refused"), and a provider
// may put its credential in the query (manifest auth.query_param), so
// anything that logs, stores or returns a transport error must pass it
// through here first. Errors without a URL are returned unchanged.
//
// Only the message is redacted: errors.As on the result still reaches the
// original *url.Error, so callers must not log its URL field themselves.
// Every *url.Error in the chain is redacted, including errors.Join branches.
func RedactURLError(err error) error {
	if ue, ok := err.(*url.Error); err == nil || (ok && ue == nil) {
		return err
	}
	msg := err.Error()
	changed := false
	for _, ue := range urlErrors(err) {
		if ue == nil || !strings.Contains(ue.URL, "?") {
			continue
		}
		cp := *ue
		cp.URL = cp.URL[:strings.IndexByte(cp.URL, '?')] + "?…"
		if ue == err {
			return &cp
		}
		msg, changed = strings.ReplaceAll(msg, ue.Error(), cp.Error()), true
	}
	if !changed {
		return err
	}
	return &redactedError{msg: msg, err: err}
}

// urlErrors collects every *url.Error in err's chain, following both
// Unwrap() error and Unwrap() []error (errors.Join).
func urlErrors(err error) []*url.Error {
	var out []*url.Error
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if ue, ok := e.(*url.Error); ok {
			out = append(out, ue)
			if ue == nil {
				return
			}
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, x := range u.Unwrap() {
				walk(x)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	return out
}

// redactedError keeps the original chain for errors.Is/As while printing
// the redacted message.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }
