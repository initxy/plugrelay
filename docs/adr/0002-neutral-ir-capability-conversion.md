# ADR-0002: Neutral IR and capability-driven conversion instead of pairwise converters

- Status: accepted
- Date: 2026-11-05

## Context

The relay speaks four client protocols (Anthropic messages, OpenAI chat,
OpenAI responses, Gemini) and must reach upstreams in any of them. Pairwise
converters grow quadratically and tend to hard-code client or vendor quirks;
converting everything through one vendor's format loses what that format
cannot express (thinking signatures, cache breakpoints, freeform tools).

## Decision

- One codec per protocol maps its wire format to and from a neutral
  intermediate representation, `pkg/ir`, which is the union of what the
  protocols express. Codecs move fields; they never decide what to drop.
  Fields the IR has no slot for are kept verbatim in `Extra`.
- Codecs declare `ir.Capabilities`; each upstream endpoint narrows them.
  Decoding computes the request's `ir.Needs`.
- `internal/convert` compares needs with effective capabilities and runs a
  fixed set of generic steps (cache, tools, private state, parameters,
  content, usage) only where they differ, grading each pairing lossless,
  lossy, discouraged or impossible. Impossible endpoints never enter routing.
- Same-protocol traffic skips conversion: passthrough (bytes untouched) or
  patch (named fields edited with sjson).
- No client or upstream name may appear in conversion logic
  (`TestNoProductNames` in `internal/convert/convert_test.go`).

## Consequences

- A new protocol costs one codec plus a capability declaration; existing
  steps apply to it automatically.
- Changing the IR touches every codec; it is the most expensive surface to
  reshape.
- Fidelity grades and diagnostics are recorded per attempt, so lossy routes
  are visible in the request log rather than silent.
