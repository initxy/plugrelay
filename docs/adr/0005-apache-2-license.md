# ADR-0005: License under Apache-2.0

- Status: accepted
- Date: 2026-11-05

## Context

plugrelay targets individual developers, teams and companies that embed or
resell it. The choice was between a permissive license and a network
copyleft (AGPL-3.0).

## Decision

License the whole repository, including `pkg/`, the SDKs and the console,
under Apache-2.0 (`LICENSE`).

## Consequences

- Companies can adopt, embed and extend plugrelay, including in closed
  products, with an explicit patent grant.
- Modified closed-source forks are allowed; changing to a copyleft license
  later would need every contributor's consent for existing code.
