# ADR-0011: External plugins speak JSON-RPC 2.0 over stdio, with a Go SDK, as extensions

- Status: accepted
- Date: 2026-11-07

## Context

Private-protocol upstreams, guardrails and integrations must be writable in
any language and must not be able to crash the relay. Options were gRPC,
JSON-RPC over stdio, or WASM. An SDK per language costs upkeep on every
protocol change, and operators switch features rather than individual
plugins (ADR-0013).

## Decision

- An external plugin is a directory with `plugin.yaml` and an executable.
  The host (`internal/extplugin`) starts it and speaks JSON-RPC 2.0 with
  LSP-style `Content-Length` framing over stdio, or a unix socket.
- Methods map one-to-one to the extension points of `pkg/plugin`; to the core
  an external plugin is just another plugin. Per-event stream hooks are not
  offered across the process boundary.
- The host supervises: per-call timeouts and concurrency limits, restart with
  backoff, and the plugin's `fail_policy` on failure.
- The contract is `sdk/PROTOCOL.md`; one SDK ships, for Go (`sdk/go`).
  Other languages implement the protocol directly. No WASM tier.
- External plugins are extensions: never part of a feature, off until an
  admin approves the host APIs they declare and turns them on, one by one
  (`/api/v1/extensions`).

## Consequences

- Any language with JSON and pipes can host a plugin; no codegen or
  toolchain is required, but only Go authors get an SDK.
- Each call costs a process round trip, so hot-path work (per-event
  rewriting, codecs) stays in built-in Go plugins.
- The wire format is a public compatibility surface, versioned separately
  (protocol version 1).
- External processes run with their own OS permissions; installing one means
  trusting it. Operators can turn the tier off (`external_plugins.enabled`).
