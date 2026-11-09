# ADR-0003: Seal upstream-private state under the master key, bound to its issuer

- Status: accepted
- Date: 2026-11-05

## Context

Thinking signatures, encrypted reasoning, Gemini thought signatures and
compaction blobs are opaque to clients, must come back verbatim next turn,
and are only accepted by the upstream that issued them. Forwarding them to a
different upstream causes errors such as "Invalid signature in thinking
block"; dropping them breaks reasoning continuity.

## Decision

- Model all of it as one thing: `ir.OpaqueState`, tagged with an issuer
  group that the provider declares per endpoint (`plugin.Endpoint.Issuer`).
- When it must cross into a client of another protocol, `internal/seal`
  encrypts issuer, kind and data with AES-256-GCM under a key derived by
  HKDF from the master key, and places it in a field that client echoes back
  (`ir.Capabilities.EchoFields`). Seals start with `ir.SealPrefix`.
- On the way back it is restored only for the same issuer. Anything that
  fails to open, or belongs to another issuer, is dropped silently; the
  request never fails because of it.
- Same-protocol passthrough does not seal; when the issuer changes, the
  codec's `StripOpaque` removes stale state in the patch tier.

## Consequences

- Losing the master key invalidates every seal in flight (conversations
  continue, but lose their private state once). Keys listed in
  `previous_master_keys` still open seals, so the key can rotate without
  that loss.
- Protocols without an echo field (OpenAI chat) cannot carry the state; the
  loss is recorded in the conversion diagnostic.
- Issuer groups are part of every provider's contract; getting one wrong
  either leaks state to a stranger or drops it needlessly.
