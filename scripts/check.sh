#!/usr/bin/env bash
# check — every automated gate in one command; CI runs exactly this. check-docs stays last.
set -euo pipefail
cd "$(dirname "$0")/.."
GO=${GO:-go}

scripts/lint.sh
# PLUGRELAY_TEST_PG (postgres:// URL) and PLUGRELAY_TEST_MYSQL (go-sql-driver
# DSN), when set, add the PostgreSQL and MySQL store, state and admin suites.
# -timeout: the store suites on three databases take minutes under -race.
$GO test -race -count=1 -timeout 30m ./...
(cd web && npm run --silent test && npm run --silent build)
$GO build -o /dev/null ./cmd/plugrelay
scripts/check-docs.sh
