# ADR-0006: Plugins reach upstreams only through core egress clients

- Status: accepted
- Date: 2026-11-05

## Context

Upstream traffic must honour the credential's fixed proxy (never a direct
fallback), the per-upstream timeouts and extra headers, and the SSRF
guard. Providers that send requests themselves (Bedrock's event-stream,
Vertex's OAuth token exchange) and background calls (model sync, quota
probes, external plugins' `host.http`) would otherwise each need their own
client, and a proxied credential could silently go direct.

## Decision

- Every upstream request is made with a client the core builds:
  `plugin.Outgoing.Client` for a request attempt (bound to its timers) and
  `Host.ClientFor(upstream, credential)` for calls outside a request. The
  proxy is looked up from the current snapshot, so a plugin cannot drop it
  by passing a bare credential. External plugins get the same through
  `host.http` with `upstream_id` / `credential_id`.
- Providers do not construct HTTP clients or dial themselves;
  `plugins/boundary_test.go` fails the build if they do.
- Transport errors pass `plugin.RedactURLError` before they are logged,
  stored or returned, since a URL query may carry a credential.

## Consequences

- Proxy, timeout and SSRF rules have one implementation (`internal/egress`)
  and hold for third-party plugins too. The core's own outbound calls
  (single sign-on, captcha, MCP servers) use the same SSRF-guarded egress.
- A provider needing a new transport feature must get it into the egress
  client rather than work around it.
