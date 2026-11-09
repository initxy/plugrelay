# ADR-0009: One SQL dialect layer for SQLite, PostgreSQL and MySQL

- Status: accepted
- Date: 2026-11-06

## Context

The store and the SQL state driver run on SQLite, PostgreSQL and MySQL 8 /
MariaDB. Their differences (upsert syntax,
`RETURNING`, integer division, identifier quoting, row-count semantics,
advisory locks) would otherwise spread `if mysql` branches through every
query.

## Decision

- Queries are written once in the SQLite/PostgreSQL form; `internal/sqldialect`
  rewrites them for MySQL and owns connection setup (DSN hardening, SQL
  mode, isolation level, `ClientFoundRows`), migration locking and the
  "duplicate key means exists" rule. Business code holds no dialect checks.
- No `RETURNING`: writes read back inside the same transaction, with
  `SELECT … FOR UPDATE` where a row is claimed, on every driver.
- Each migration exists for all three drivers; `TestMigrationsPerDriver`
  fails when one is missing. Client-supplied indexed strings are truncated
  or hashed to the column width the same way on every driver.

## Consequences

- PostgreSQL takes an extra round trip where `RETURNING` would have saved
  one.
- MySQL DDL is not transactional: a migration that fails half way leaves
  partial schema ([deployment](../deployment.md)).
- A new query must pass the shared store/state suites on all three drivers
  (`PLUGRELAY_TEST_PG`, `PLUGRELAY_TEST_MYSQL`; CI sets both).
