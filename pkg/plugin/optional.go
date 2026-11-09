package plugin

import (
	"context"
	"errors"
	"fmt"

	"github.com/initxy/plugrelay/pkg/ir"
)

// Sender is implemented by providers that perform the upstream call
// themselves (external providers with a private protocol) instead of letting
// the core send the HTTP request Prepare built. Events are emitted in neutral
// form; the core encodes them for the client. The returned Outcome describes
// a failure before any event was emitted.
type Sender interface {
	Send(ctx context.Context, o *Outgoing, req *ir.Request, emit func(*ir.Event) error) (*Outcome, error)
}

// Supporter is implemented by adapters whose optional hooks are declared at
// runtime (external plugins): an adapter may satisfy RequestHook in Go while
// the plugin behind it never implemented "middleware.onRequest". The core
// skips hooks for which Supports returns false.
type Supporter interface {
	Supports(method string) bool
}

// ErrPluginFailure marks errors caused by a plugin malfunctioning (crash,
// timeout, protocol error) rather than deliberately rejecting a request.
// The core applies the plugin's FailPolicy to these; a deliberate rejection
// is an *ir.GwError sent to the client as is.
var ErrPluginFailure = errors.New("plugin failure")

// IsFailure reports whether err is a plugin malfunction.
func IsFailure(err error) bool { return errors.Is(err, ErrPluginFailure) }

// ErrPluginStopped marks a plugin failure that retrying cannot fix because
// the plugin was stopped (the relay is shutting down, or an admin turned
// it off). It matches ErrPluginFailure.
var ErrPluginStopped = fmt.Errorf("plugin stopped: %w", ErrPluginFailure)
