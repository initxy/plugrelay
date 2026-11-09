# ADR-0001: Go, with a small public `pkg/` API and everything else internal

- Status: accepted
- Date: 2026-11-05

## Context

plugrelay is a long-running network relay that streams SSE, holds many
concurrent connections and ships as one binary for laptops, servers and
containers. Third parties need to reuse parts of it — protocol codecs, the
plugin API and its conformance suite — without depending on the relay's
internals, which must stay free to change.

## Decision

- Write plugrelay in Go (module `github.com/initxy/plugrelay`, Go 1.26), a
  single static binary with the console embedded via `go:embed` (`web/embed.go`).
- Public, versioned packages live under `pkg/`: `pkg/ir` (neutral format),
  `pkg/sse` (the only SSE parser), `pkg/protocol` with one sub-package per
  codec, `pkg/plugin` with the `pkg/plugin/plugintest` conformance suite, and
  the small helpers plugins share (`pkg/clientid`, `pkg/match`,
  `pkg/captcha`).
- Everything else is under `internal/`, which Go forbids other modules to
  import. Built-in plugins live in `plugins/` and see the core through
  `pkg/plugin`.

## Consequences

- Anything in `pkg/` is a compatibility promise; moving it back out is a
  breaking change for importers.
- Third-party Go plugins compile into a custom binary that imports
  `pkg/plugin`; they cannot reach `internal/`.
- Go would let plugins inside this module import `internal/`;
  `plugins/boundary_test.go` fails the build when one does, so built-in
  plugins stay on the same contract as third-party ones.
