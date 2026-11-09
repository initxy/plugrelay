# Contributing to plugrelay

English | [简体中文](CONTRIBUTING.zh-CN.md)

Thanks for helping. A normal PR with tests and a green `make check` is
enough. `AGENTS.md`, `docs/gc.md`, `docs/reviewing.md` and `docs/specs/`
describe how the maintainers work with coding agents; you don't need to
follow them.

## Ground rules

- **No client impersonation, no subscription pools.** Patches that forge TLS
  fingerprints, client headers or system prompts, or share subscription
  accounts across users, are declined however good they are.
- **No client or upstream names in core logic.** Client quirks go in the
  protocol codec (`pkg/protocol/<name>`), upstream quirks in a manifest
  (`manifests/`) or provider plugin. A test rejects product names in the
  generic conversion code.
- **Every compatibility bug starts with a failing fixture**, in the codec's
  `testdata/` (e.g. the captured `codex_request.json` and
  `claude_code_request.json`) or in `internal/pipeline/testdata/` for
  end-to-end conversions.

## Development

Go 1.26+, Node 20.19+ (CI uses 24), and Docker if you want the PostgreSQL and
MySQL tests.

```bash
make build            # bin/plugrelay
make test             # Go (race) + console unit tests
make check            # every gate CI runs; must be green before review
PLUGRELAY_TEST_PG=postgres://… make test   # add the PostgreSQL store suite
PLUGRELAY_TEST_MYSQL='root:pw@tcp(127.0.0.1:3306)/plugrelay' make test   # and/or MySQL
# Each -count of the store suite on all three databases takes ~4 min,
# past go test's default 10 min timeout.
go test -race -count=3 -timeout 60m ./internal/store/
PLUGRELAY_E2E_BASE=… PLUGRELAY_E2E_KEY=… make e2e   # live upstream, every client protocol
PLUGRELAY_E2E_CLIENTS=1 …  make e2e                  # also drive real Claude Code / Codex
```

Run the real-client e2e before review on any compatibility change: it is the
only test that catches a client upgrade changing what it sends.

### Demo relay for console work

`make demo` runs a real relay on a throwaway SQLite store, seeded with
upstreams, credentials in every live state, models, presets, users, keys,
billing data and 14 days of request history (`--days`). A fake upstream in
the same process answers every request. It prints the admin login and a demo
key, and deletes the data on exit unless you pass `--dir`,
as in `make demo DEMO_FLAGS='--dir /tmp/prdemo --days 30'`.

Point the console's dev server at it (`web/README.md`). If a page needs data
the seed lacks, add it in `internal/demo/`.

## Adding an upstream

Most upstreams need only a YAML manifest:

1. Copy a close match in `manifests/` and edit endpoints, auth, capabilities
   and error rules (schema: `manifests/schema.json`).
2. `plugrelay manifest validate manifests/<id>.yaml`
3. `go test ./plugins/provider/compatible/ -run TestManifestConformance` runs
   the provider conformance suite on every file in `manifests/`.

For a private protocol, write an external plugin in any language
(`sdk/PROTOCOL.md`; Go SDK in `sdk/go`).

## Pull requests

- One concern per PR; features and refactors go in separate PRs.
- Conventional commit prefixes (`feat:`, `fix:`, `docs:`, `refactor:`,
  `test:`, `chore:`). `feat:` and `fix:` commits go into the release notes
  ([releasing](docs/releasing.md)).
- Explain *why* in the description; the diff shows *what*.
- New behavior needs tests for the main path and at least one failure path.

## Reporting security issues

Don't open public issues for vulnerabilities; see `SECURITY.md`.
