# ADR-0012: Request records are an at-least-once ledger; requirements turn on with their dependent

- Status: accepted
- Date: 2026-11-07

## Context

Budgets (`meter.quota`) are summed from request records written by
`sink.requestlog`, and the charge is settled before the record is queued.
A record lost on overload, a failed write or a restart is therefore a
request that was charged but never counted. Logs, metrics and traces can
afford to lose records under overload; the ledger cannot. A plugin can also
depend on another (`Requires`) — the quota meter on the request log — and
must not run with its dependency off.

## Decision

- A sink whose `FailPolicy` is `closed` is a ledger sink. Each ledger sink
  has its own lane and worker (`internal/pipeline/sinks.go`); a full lane
  makes `Enqueue` wait briefly, then deliver in the calling goroutine.
  Ledger records are never dropped for lack of room; other sinks share a
  best-effort lane that drops and counts.
- Failed ledger deliveries are retried with backoff, except when the
  plugin is stopped (`plugin.ErrPluginStopped`). Delivery is therefore
  at-least-once: ledger sinks must treat a repeated `Record.ID` as already
  written (the store inserts with `ON CONFLICT (id) DO NOTHING`).
- Shutdown order: stop accepting connections, wait for in-flight exchanges
  to finish accounting (`Pipeline.Wait`, `shutdown.drain`), drain the sink
  queue under a deadline, then stop external plugins. Records still
  undelivered at the deadline are logged and counted as lost
  (`plugrelay_sink_ledger_records_total{outcome="lost"}`).
- A plugin may declare `Requires`; the requirement is turned on with it.
  A fail-closed plugin whose requirement is not installed refuses to start
  or reload, and a feature or extension change that would cause it is
  refused (409) before it is stored.

## Consequences

- Third-party ledger sinks must be idempotent on `Record.ID` and may be
  called concurrently (worker and inline path).
- A slow ledger sink slows request completion (bounded by `LedgerWait`
  and the delivery timeout), never the client's response body.
- Process managers must allow `shutdown.grace + shutdown.drain` plus the
  queue deadline before killing (compose ships `stop_grace_period: 120s`).
- No requirement can be switched off under its dependent: the built-in
  ones are core modules or modules of the same feature.
