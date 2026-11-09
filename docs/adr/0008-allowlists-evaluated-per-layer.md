# ADR-0008: Allowlist settings are evaluated per layer

- Status: accepted
- Date: 2026-11-06

## Context

Access settings such as `allowed_models`, `allowed_ips`, `allowed_clients`
and `mcp_tools_allow` are set per group, user and key and merge Strictest.
An empty list means "no restriction". Merging two non-empty lists by string
intersection fails in two ways: patterns (globs, CIDRs) cannot be
intersected as strings, and an empty intersection would read as "no
restriction", so narrowing at a lower layer would open everything.

## Decision

- An allowlist setting allows a value only if every layer that sets a
  non-empty list allows it (`plugin.Value.ListAllows`, `ListLayers`). An
  empty list at a lower layer never lifts a restriction set above it.
- Where a single merged list must be produced (readers of the merged
  value, external plugins), layers without a common entry merge to a
  sentinel that matches nothing (`DenyAll`), never to empty.
- External plugins also receive each layer's list (`setting_layers`) and
  should evaluate per layer; the console's effective view shows the layers
  and names the one that leaves nothing allowed.

## Consequences

- Lists whose empty value means "nothing" (`mcp_servers`,
  `native_upstreams`) are grants, merged Override, and unaffected.
- A new allowlist setting must be declared as a strict list
  (`plugin.NewStrictList`) to get these semantics.
