// Package state holds short-lived runtime state that every relay node must
// agree on: session bindings, credential cooldowns, in-flight counters,
// rate-limit windows and locks. A single node uses the in-memory backend,
// or the SQL backend in the store's database to keep the state across
// restarts; a cluster points all nodes at the same Redis.
//
// Durable business data (keys, upstreams, usage) lives in package store, not
// here. Everything in State may be lost on restart of its backend at the cost
// of cache hits and counters, never of correctness.
package state

import "github.com/initxy/plugrelay/pkg/plugin"

// State is the shared runtime state backend. Its method set is the public
// plugin.SharedState plus Close, so compiled-in plugins see the same backend
// without importing this package.
type State interface {
	plugin.SharedState
	Close() error
}
