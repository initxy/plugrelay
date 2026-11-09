# MCP gateway

English | [简体中文](mcp.zh-CN.md)

The relay exposes configured MCP servers to clients with the platform key,
so a team configures each server once (URL, credentials) and every client
reaches it through plugrelay with its own key. It runs while the `mcp`
feature is on ([configuration](configuration.md#features)); off, every
`/mcp` path answers 404. Code: `internal/mcp` (package doc), wiring in
`internal/app/mcp.go`.

- [Endpoints](#endpoints)
- [Protocol versions](#protocol-versions)
- [Adding servers](#adding-servers)
- [Access](#access)
- [Logging and limits](#logging-and-limits)
- [Connecting clients](#connecting-clients)
- [Limits of the gateway](#limits-of-the-gateway)

## Endpoints

| path | serves |
|-|-|
| `/mcp` | every tool of every server the key may use, named `<server>__<tool>` |
| `/mcp/<server-id>` | one server: its tools under their own names, plus its resources, prompts and completions passed through |

Both speak MCP's Streamable HTTP transport and take the platform key the
same way the model API does (`Authorization: Bearer pr_…`, `x-api-key`).
A missing or wrong key gets 401; a server the key may not use, or one that
is off, gets 404.

Aggregated names: characters outside `[A-Za-z0-9_-]` become `_`; a name
longer than 64 characters is cut and ends in 8 hex characters of a hash of
server and tool; two tools that still collide are numbered (`_2`, `_3`) in
tool-name order, so names do not change when a server reorders its list.
Server ids have no underscores, so the first `__` always ends the server
id. The MCP spec asks aggregating proxies to prefix a server identifier
([tools, 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/server/tools)).

## Protocol versions

Checked against the MCP specification on 2026-11-06: the current revision
is 2026-07-28; 2025-11-25 and earlier are "legacy"
([versioning](https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning)).

| side | what the gateway speaks |
|-|-|
| client, legacy (2025-03-26, 2025-06-18, 2025-11-25) | `initialize` (the client's version when supported, else 2025-11-25), `Mcp-Session-Id` on the response and required afterwards (400 without, 404 when unknown or another key's), `MCP-Protocol-Version` checked, `DELETE` ends the session, `GET` answers 405 (no server-initiated stream), `ping` ([transports, 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)) |
| client, 2026-07-28 | stateless: `MCP-Protocol-Version` must match `_meta`, `Mcp-Method` the body's `method`, and `Mcp-Name` (plain or `=?base64?…?=`) the `params.name` of `tools/call` and `prompts/get` or the `params.uri` of `resources/read`; a missing or mismatched header gets 400 with `-32020` ([standard request headers](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http#standard-request-headers), [server validation](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http#server-validation)); `server/discover`, results carry `resultType`, list results `ttlMs` and `cacheScope: private`, unknown methods 404 ([Streamable HTTP, 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)) |
| upstream, transport `auto` (default) | Streamable HTTP: `initialize` with 2025-11-25; an error listing 2026-07-28 in `data.supported` switches to the stateless revision (`server/discover`, `Mcp-Method`, `Mcp-Name`, `_meta`); a 400/404/405 without a JSON-RPC body falls back to the HTTP+SSE transport of [2024-11-05](https://modelcontextprotocol.io/specification/2024-11-05/basic/transports) (`GET` stream, `endpoint` event, `message` events) |
| upstream, `streamable-http` / `sse` | that transport only |

Session ids are HMAC-signed tokens of key id, version and issue time, so
any relay node accepts them and nothing is stored; one older than 24 hours
gets 404 and the client initializes again. Each node keeps one connection per
upstream server (session re-opened once when the server answers 404), and
the server's tool list is reused for a minute. JSON-RPC batches are
refused (removed in 2025-06-18). Requests with an `Origin` naming another
host get 403 (DNS rebinding).

A `tools/call` that carries a `progressToken` from a client accepting
`text/event-stream` is answered as SSE: the server's progress
notifications for that call, then the result. Tokens are rewritten on the
way up, so clients sharing a connection never see each other's progress.

## Adding servers

Console: the MCP servers page (add, edit, test). API: `/api/v1/mcp/servers`
([api.md](api.md)); the list is stored in plugin KV `core.mcp`.

| field | meaning |
|-|-|
| `id` | 1–32 lowercase letters, digits, dashes; in `/mcp/<id>` and `<id>__<tool>` |
| `url` | the server's MCP endpoint (its `/sse` URL for the legacy transport) |
| `transport` | `auto`, `streamable-http` or `sse` |
| `headers` | sent as is |
| `secret_headers` | credentials (`Authorization: Bearer …`, `X-Api-Key`), sealed with the master key, never returned; an empty value removes one. Changing `url` to another scheme, host or port is refused unless every stored secret header is given again (or cleared) in the same edit, so secrets never follow a server to a new address unseen |
| `tools_allow` / `tools_deny` | bare tool names or globs everyone is limited to / refused; in every tool glob `*` and `?` also match `/` |
| `proxy`, `allow_private` | fixed egress proxy; allow a private or loopback address (else the SSRF guard refuses it, as for upstreams) |
| `timeout_sec` | per call, default 120 s |

Client headers never reach a server; only the configured ones go out,
through the core egress. `POST /api/v1/mcp/servers/{id}/test` connects
afresh and lists the tools.

## Access

Grants live in the `middleware.access` settings (a core module; global
values in the MCP gateway feature's form, lower layers on the group, user
or key; [configuration](configuration.md#layered-plugin-settings)):

| setting | merge | meaning |
|-|-|-|
| `mcp_servers` | override | server ids the caller may use, `"*"` = all; empty = none |
| `mcp_tools_allow` | strictest | `<server>__<tool>` names or globs the caller is limited to: each layer's list must allow the tool; empty = all granted |
| `mcp_tools_deny` | union | `<server>__<tool>` names or globs refused (`github__delete_*`); a group or key can add, never remove |

```bash
# group eng may use the github and docs servers, but no github__delete_* tool
# (only the settings named change; null removes one)
curl -X PUT "$RELAY/api/v1/groups/eng/settings" \
  -H "Authorization: Bearer $PLUGRELAY_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"settings":{"middleware.access":{"mcp_servers":["github","docs"],"mcp_tools_deny":["github__delete_*"]}}}'
```

A tool the caller may not use is not listed and answers `-32602 unknown
tool`, the same as a tool that does not exist. Tool rules cover tools only:
a key granted a server reaches all of its resources, prompts and
completions on `/mcp/<server-id>`.

## Logging and limits

Every `tools/call` is one request-log record: protocol `mcp`, model
`<server>__<tool>`, upstream `mcp:<server>`, status (200, 404 unknown or
refused tool, 429 limited, 400/502 server errors), duration, and the error;
a result with `isError` is logged as an error text with status 200.
Arguments and result are stored only when `sink.requestlog` body logging is
on for the caller. Calls go through `middleware.ratelimit` when it runs:
the caller's `rpm` and `concurrency` counters are shared with model
requests (`tpm` does not apply). On `/mcp/<server-id>`, `resources/read`
and `prompts/get` are logged and limited the same way, under model
`<server>/<method>`. Listings, `initialize` and completions are not.

## Connecting clients

[clients.md](clients.md#mcp-servers) has the Claude Code and Codex
configuration; `plugrelay setup` does not write it.

## Limits of the gateway

- stdio servers (launched as child processes) are not supported; run them
  behind a Streamable HTTP bridge.
- No `listChanged`, resource subscriptions or the 2026-07-28
  `subscriptions/listen` stream; clients re-list instead (tools are cached
  for a minute).
- A server's own requests (sampling, elicitation, roots) are refused with
  `-32601`; the relay offers clients none of them.
- One shared upstream connection per server and node: a stateful server
  sees every relay client as one.
- OAuth to upstream servers is not handled; give a token in
  `secret_headers`.
