# ADR-0013: Operators switch features, not plugins; the console has no plugin UI

- Status: accepted
- Date: 2026-11-07

## Context

The relay is built from many small plugins, but operators think in
capabilities ("sell access", "guardrails", "single sign-on"). Exposing
plugins directly means operators must know module ids, the same setting can
end up in two places, and letting plugins serve their own console pages
needs a slot system (CSP, scoped tokens, approvals) that costs more than the
few pages it would carry.

## Decision

- The operator-facing unit is a **feature** (`internal/features`): one
  switch and one settings form, backed by one or more built-in modules, with
  zh/en names and the console pages it shows. Feature state lives in the
  database and hot-applies. Core modules are always on and not listed.
- Plugins are the internal extension mechanism (`pkg/plugin`); plugins
  outside every feature appear as extensions (ADR-0011).
- The console hard-codes its pages and shows them by role and feature state.
  Plugins cannot add console pages, cards or slots.
- There are no run modes. Setup only pre-enables features; everything else
  is an explicit setting. The sell kit is compiled in.
- relay.yaml is bootstrap only (listen, store, state, master keys, egress,
  limits, external plugin dir, logging, shutdown, native, WebSocket bounds).

## Consequences

- Adding a console page for a capability means changing the console, not
  shipping a plugin; third parties cannot extend the console.
- A module setting has one place per layer: the feature's form at the
  global layer, the group, user or key object below it.
