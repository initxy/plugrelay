# MCP gateway

English | [简体中文](mcp.zh-CN.md)

Configure each MCP server once (URL, credentials) and every client reaches it
through plugrelay with its own platform key. The gateway runs while the `mcp`
feature is on ([configuration](configuration.md#features)); off, every `/mcp`
path answers 404. Code: `internal/mcp`, wired in `internal/app/mcp.go`.

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
| `/mcp/<server-id>` | one server: its tools under their own names, plus its resources, prompts and completions |

Both speak Streamable HTTP and take the platform key like the model API
(`Authorization: Bearer pr_…` or `x-api-key`): 401 for a bad key, 404 for a
server the key may not use or that is off.

In aggregated names, characters outside `[A-Za-z0-9_-]` become `_`; names
over 64 characters are cut and end in an 8-hex-digit hash; remaining
collisions get `_2`, `_3` in tool-name order. Server ids have no
underscores, so the first `__` ends the server id.

## Protocol versions

Checked against the MCP specification on 2026-11-06: the current revision is
2026-07-28; 2025-11-25 and earlier are legacy
([versioning](https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning)).

| side | what the gateway speaks |
|-|-|
| client, legacy (2025-03-26, 2025-06-18, 2025-11-25) | `initialize` (the client's version if supported, else 2025-11-25); `Mcp-Session-Id` returned and required afterwards (400 without, 404 if unknown or another key's); `MCP-Protocol-Version` checked; `DELETE` ends the session; `GET` answers 405 (no server-initiated stream); `ping` ([transports, 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)) |
| client, 2026-07-28 | stateless: `MCP-Protocol-Version` must match `_meta`, `Mcp-Method` the body's `method`, and `Mcp-Name` (plain or `=?base64?…?=`) the `params.name` of `tools/call` and `prompts/get` or the `params.uri` of `resources/read`; missing or mismatched gets 400 with `-32020` ([standard request headers](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http#standard-request-headers), [server validation](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http#server-validation)); `server/discover`; results carry `resultType`, list results `ttlMs` and `cacheScope: private`; unknown methods 404 ([Streamable HTTP, 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)) |
| upstream, transport `auto` (default) | Streamable HTTP, `initialize` with 2025-11-25; an error listing 2026-07-28 in `data.supported` switches to the stateless revision; a 400/404/405 without a JSON-RPC body falls back to the HTTP+SSE transport of [2024-11-05](https://modelcontextprotocol.io/specification/2024-11-05/basic/transports) |
| upstream, `streamable-http` / `sse` | that transport only |

- Session ids are signed, not stored, so any node accepts them; after 24
  hours they get 404 and the client initializes again.
- Each node keeps one connection per upstream server and caches its tool
  list for a minute.
- JSON-RPC batches are refused (removed in 2025-06-18); an `Origin` naming
  another host gets 403.
- A `tools/call` with a `progressToken` from a client accepting
  `text/event-stream` gets SSE: the server's progress notifications, then
  the result. Clients sharing a connection never see each other's progress.

## Adding servers

Console: the MCP servers page. API: `/api/v1/mcp/servers` ([api.md](api.md)),
stored in plugin KV `core.mcp`.

| field | meaning |
|-|-|
| `id` | 1–32 lowercase letters, digits, dashes; used in `/mcp/<id>` and `<id>__<tool>` |
| `url` | the server's MCP endpoint (its `/sse` URL for the legacy transport) |
| `transport` | `auto`, `streamable-http` or `sse` |
| `headers` | sent as is |
| `secret_headers` | credentials (`Authorization: Bearer …`, `X-Api-Key`), sealed, never returned; an empty value removes one. Moving `url` to another scheme, host or port requires re-entering or clearing them in the same edit |
| `tools_allow` / `tools_deny` | tool names or globs everyone is limited to / refused; `*` and `?` also match `/` |
| `proxy`, `allow_private` | fixed egress proxy; allow a private or loopback address (refused by the SSRF guard otherwise) |
| `timeout_sec` | per call, default 120 s |

Only the configured headers go out, never the client's.
`POST /api/v1/mcp/servers/{id}/test` connects afresh and lists the tools.

## Access

Grants are `middleware.access` settings: global values in the MCP gateway
feature's form, overrides on the group, user or key
([configuration](configuration.md#layered-plugin-settings)).

| setting | merge | meaning |
|-|-|-|
| `mcp_servers` | override | server ids the caller may use, `"*"` = all; empty = none |
| `mcp_tools_allow` | strictest | `<server>__<tool>` names or globs the caller is limited to; every layer's list must allow the tool; empty = all granted |
| `mcp_tools_deny` | union | `<server>__<tool>` names or globs refused (`github__delete_*`); lower layers can add, never remove |

```bash
# group eng may use the github and docs servers, but no github__delete_* tool
# (only the settings named change; null removes one)
curl -X PUT "$RELAY/api/v1/groups/eng/settings" \
  -H "Authorization: Bearer $PLUGRELAY_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"settings":{"middleware.access":{"mcp_servers":["github","docs"],"mcp_tools_deny":["github__delete_*"]}}}'
```

A tool the caller may not use is hidden and answers `-32602 unknown tool`.
Tool rules cover only tools: a key granted a server gets all its resources,
prompts and completions on `/mcp/<server-id>`.

## Logging and limits

Each `tools/call` is one request-log entry: protocol `mcp`, model
`<server>__<tool>`, upstream `mcp:<server>`, duration, error and status (404
unknown or refused tool, 429 limited, 400/502 server error; an `isError`
result is 200 with the error text). Arguments and results are kept only when
`sink.requestlog` body logging is on for the caller. With
`middleware.ratelimit` running, calls share the caller's `rpm` and
`concurrency` with model requests (`tpm` doesn't apply).
`resources/read` and `prompts/get` on `/mcp/<server-id>` are logged and
limited too, as model `<server>/<method>`; listings, `initialize` and
completions are not.

## Connecting clients

Claude Code and Codex configuration is in [clients.md](clients.md#mcp-servers);
`plugrelay setup` does not write it.

## Limits of the gateway

- No stdio servers; run them behind a Streamable HTTP bridge.
- No `listChanged`, resource subscriptions or the 2026-07-28
  `subscriptions/listen` stream; clients re-list (tools are cached for a
  minute).
- Server-initiated requests (sampling, elicitation, roots) are refused with
  `-32601`.
- One shared upstream connection per server and node: a stateful server sees
  all relay clients as one.
- No OAuth to upstream servers; put a token in `secret_headers`.
