# ADR-0004: Two-level pluggable storage — durable store and shared runtime state

- Status: accepted
- Date: 2026-11-05

## Context

The same binary must run on a laptop with zero dependencies and as a
multi-node team or resale deployment. Business data (keys, upstreams,
credentials, models, usage) must be durable and transactional; routing state
(session bindings, cooldowns, in-flight slots, rate windows) is hot,
short-lived and must be shared by every node.

## Decision

- `internal/store` holds durable business data behind one interface, with
  SQLite (pure Go, modernc, the default), PostgreSQL (pgx) and MySQL drivers
  over `database/sql` and embedded versioned migrations.
- `internal/state` holds shared runtime state behind a small KV/counter/slot/
  lock interface, with memory, Redis and SQL (reusing the store database)
  backends. Losing it costs cache hits and counters, never correctness.
- `relay.yaml` is bootstrap only and never written. Routing-relevant writes
  bump a version counter; the core rebuilds an immutable snapshot and swaps
  it atomically.
- Request logs reach storage through `sink.*` plugins, so they can live
  elsewhere.

## Consequences

- Every store change ships one migration per driver (queries shared through
  `internal/sqldialect`, ADR-0009) and must pass the same suite on each
  (`internal/store/store_test.go`).
- Multi-node deployments require PostgreSQL or MySQL plus Redis; SQLite and the
  memory backend are single-node only.
- Components that read routing data never query the database on the request
  path; they read the current snapshot.
