package router

import (
	"fmt"
	"strings"
	"time"
)

// PlanErrorKind tells the pipeline which client error to produce when no
// candidate is left.
type PlanErrorKind string

// Plan error kinds.
const (
	// ErrNoRoute: the model is unknown, disabled, or has no route the caller
	// may use.
	ErrNoRoute PlanErrorKind = "no_route"
	// ErrImpossible: every reachable endpoint lacks a capability the request
	// needs (Missing lists them), or is discouraged and not allowed.
	ErrImpossible PlanErrorKind = "impossible"
	// ErrCooling: every usable credential or upstream is cooling down;
	// RetryAt is the earliest recovery.
	ErrCooling PlanErrorKind = "cooling"
	// ErrBusy: everything is at its concurrency cap.
	ErrBusy PlanErrorKind = "busy"
	// ErrExhausted: every candidate was already tried (Query.Exclude), or
	// the rest is quarantined/disabled.
	ErrExhausted PlanErrorKind = "exhausted"
)

// PlanError is returned by Plan when no candidate remains.
type PlanError struct {
	Kind    PlanErrorKind
	Model   string
	Missing []string  // ErrImpossible
	RetryAt time.Time // ErrCooling; zero when unknown
	// Discouraged is set for ErrImpossible when at least one endpoint was
	// only discouraged, so retrying with AllowDiscouraged could succeed.
	Discouraged bool
}

func (e *PlanError) Error() string {
	switch e.Kind {
	case ErrImpossible:
		if len(e.Missing) > 0 {
			return fmt.Sprintf("model %q: no endpoint supports %s", e.Model, strings.Join(e.Missing, ", "))
		}
		return fmt.Sprintf("model %q: no endpoint can serve this request", e.Model)
	case ErrCooling:
		if !e.RetryAt.IsZero() {
			return fmt.Sprintf("model %q: all credentials cooling down until %s", e.Model, e.RetryAt.UTC().Format(time.RFC3339))
		}
		return fmt.Sprintf("model %q: all credentials cooling down", e.Model)
	case ErrBusy:
		return fmt.Sprintf("model %q: all credentials at capacity", e.Model)
	case ErrExhausted:
		return fmt.Sprintf("model %q: no untried candidate left", e.Model)
	default:
		return fmt.Sprintf("model %q: no route", e.Model)
	}
}

// Is lets errors.Is(err, &PlanError{Kind: k}) match on kind.
func (e *PlanError) Is(target error) bool {
	t, ok := target.(*PlanError)
	return ok && (t.Kind == "" || t.Kind == e.Kind)
}
