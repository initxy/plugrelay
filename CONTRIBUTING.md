# Contributing to plugrelay

English | [简体中文](CONTRIBUTING.zh-CN.md)

Thanks for helping. This page is the short version; `AGENTS.md` holds the
working rules both humans and coding agents follow.

## Ground rules

- **No client impersonation, no subscription pools.** Patches that forge TLS
  fingerprints, client headers or system prompts, or that share subscription
  accounts across users, are declined regardless of quality.
- **No client or upstream names in core logic.** Client quirks belong to the
  protocol codec (`pkg/protocol/<name>`), upstream quirks to a provider
  manifest (`manifests/`) or plugin. Generic conversion code is checked by a
  test for product names.
- **Every compatibility bug starts with a failing fixture** under the codec's
  `testdata/` (`pkg/protocol/<name>/testdata/`, e.g. the captured
  `codex_request.json` and `claude_code_request.json`) or
  `internal/pipeline/testdata/` for end-to-end conversions.

`AGENTS.md`, `docs/gc.md`, `docs/reviewing.md` and `docs/specs/` describe
the maintainers' agent-assisted workflow. Contributors don't need to follow
it: a normal PR with tests and `make check` green is enough.

## Development

Requirements: Go 1.26+, Node 20.19+ (CI uses 24), and optionally Docker for PostgreSQL and MySQL tests.

```bash
make build            # bin/plugrelay
make test             # Go (race) + console unit tests
make check            # every gate CI runs — must be green before review
PLUGRELAY_TEST_PG=postgres://… make test   # include the PostgreSQL store suite
PLUGRELAY_TEST_MYSQL='root:pw@tcp(127.0.0.1:3306)/plugrelay' make test   # and/or the MySQL one
# Repeated race runs of the store on all three databases take ~4 min per
# -count, past go test's default 10 min timeout: raise it.
go test -race -count=3 -timeout 60m ./internal/store/
PLUGRELAY_E2E_BASE=… PLUGRELAY_E2E_KEY=… make e2e   # live upstream, every client protocol
PLUGRELAY_E2E_CLIENTS=1 …  make e2e                  # also drive real Claude Code / Codex
```

### Demo relay for console work

`make demo` (`plugrelay demo`) runs a real relay on a throwaway SQLite store
seeded with upstreams, credentials in every live state, models, presets,
users, keys, billing data and `--days` (default 14) of request history; an
in-process fake upstream answers every request. It prints the admin login
and a demo key; data is deleted on exit unless `--dir` is given.
`make demo DEMO_FLAGS='--dir /tmp/prdemo --days 30'` keeps it between runs.
Point the console's dev server at it (see `web/README.md`). The seed lives
in `internal/demo/`; extend it there when a page needs data it lacks.

A compatibility change should be checked with the real-client run before
review: it is the only test that catches a client upgrade changing what it
sends.

## Adding an upstream

Most upstreams need only a YAML manifest:

1. Copy a close match in `manifests/` and edit endpoints, auth, capabilities
   and error rules. The schema is `manifests/schema.json`.
2. `plugrelay manifest validate manifests/<id>.yaml`.
3. Run the provider conformance suite (`pkg/plugin/plugintest`) through the
   manifest test, which covers every file in `manifests/` automatically.

Private protocols: write an external plugin in any language
(`sdk/PROTOCOL.md`; Go SDK in `sdk/go`).

## Pull requests

- One concern per PR; feature work and refactors go in separate PRs.
- Conventional commit prefixes (`feat:`, `fix:`, `docs:`, `refactor:`,
  `test:`, `chore:`) — `feat:` and `fix:` commits are listed under the
  release notes ([releasing](docs/releasing.md)).
- Explain *why* in the description; the diff already shows *what*.
- New behavior needs tests for the critical path and at least one failure path.

## Reporting security issues

Please do not open public issues for vulnerabilities — see `SECURITY.md`.
