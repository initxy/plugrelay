package extplugin

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Sentinel causes carried by *CallError. Every CallError also matches
// plugin.ErrPluginFailure, so the core applies the plugin's FailPolicy.
var (
	// ErrUnavailable: the process is not running (starting, crashed,
	// backing off, stopped) or died during the call.
	ErrUnavailable = errors.New("plugin unavailable")
	// ErrTimeout: the call exceeded its timeout, including time spent
	// waiting for a concurrency slot.
	ErrTimeout = errors.New("plugin call timed out")
	// ErrPlugin: the plugin answered with an error or a malformed result.
	ErrPlugin = errors.New("plugin error")
	// ErrStopped: the process was stopped and is not restarting. It also
	// matches ErrUnavailable and plugin.ErrPluginStopped, so callers that
	// retry know to give up.
	ErrStopped error = stoppedError{}
	// ErrNotImplemented: the plugin does not implement the method. It also
	// matches errors.ErrUnsupported.
	ErrNotImplemented = fmt.Errorf("plugin method not implemented: %w", errors.ErrUnsupported)
)

type stoppedError struct{}

func (stoppedError) Error() string   { return "plugin stopped" }
func (stoppedError) Unwrap() []error { return []error{ErrUnavailable, plugin.ErrPluginStopped} }

// CallError is a failed call to an external plugin. Deliberate refusals are
// not CallErrors: they come back as *ir.GwError.
type CallError struct {
	Plugin  string
	Method  string
	Code    int
	Message string
	Data    json.RawMessage
	Err     error
}

func (e *CallError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("plugin %s %s: %v: %s", e.Plugin, e.Method, e.Err, e.Message)
	}
	return fmt.Sprintf("plugin %s %s: %v", e.Plugin, e.Method, e.Err)
}

// Unwrap exposes both the specific cause and plugin.ErrPluginFailure.
func (e *CallError) Unwrap() []error { return []error{e.Err, plugin.ErrPluginFailure} }

// fromRPC maps a JSON-RPC error to the error an adapter returns.
func fromRPC(pluginID, method string, re *rpcError) error {
	switch re.Code {
	case CodeRejected:
		var we wireError
		if len(re.Data) > 0 && json.Unmarshal(re.Data, &we) == nil && we.Kind != "" {
			if we.Message == "" {
				we.Message = re.Message
			}
			return we.gw()
		}
		return &ir.GwError{Kind: ir.ErrPermission, Message: re.Message}
	case CodeMethodNotFound:
		return &CallError{Plugin: pluginID, Method: method, Code: re.Code, Message: re.Message, Err: ErrNotImplemented}
	}
	return &CallError{Plugin: pluginID, Method: method, Code: re.Code, Message: re.Message, Data: re.Data, Err: ErrPlugin}
}
