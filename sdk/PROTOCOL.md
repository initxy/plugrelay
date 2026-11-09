# plugrelay external plugin protocol

English | [简体中文](PROTOCOL.zh-CN.md)

Version 1.

An external plugin is a process plugrelay starts and talks to with
JSON-RPC 2.0. Every method maps to one extension point of `pkg/plugin`; to the
core, an external plugin is indistinguishable from a built-in one. This
document is the contract; the Go host lives in `internal/extplugin`, and the
Go SDK in `sdk/go` implements the plugin side.

## 1. Transport and framing

Messages are framed like the Language Server Protocol: an ASCII header block,
an empty line, then a UTF-8 JSON body of exactly `Content-Length` bytes.

```
Content-Length: 56\r\n
\r\n
{"jsonrpc":"2.0","id":1,"method":"shutdown","params":{}}
```

- `Content-Length` is required; header names are case-insensitive; other
  headers (such as `Content-Type`) are ignored.
- One message is at most 64 MiB.
- Both sides send requests: the host calls plugin methods, the plugin calls
  `host.*` methods. Request ids are unique per sender; the host uses integers.
- Requests are served concurrently; responses may arrive in any order.

Two transports, chosen by `transport` in `plugin.yaml`:

| transport | protocol channel | logs |
|-|-|-|
| `stdio` (default) | the plugin reads stdin, writes stdout | stderr, one log line per line |
| `unix` | the plugin connects to the socket path in `PLUGRELAY_SOCKET` | stdout and stderr |

In stdio mode, writing anything but protocol frames to stdout breaks the
connection.

The process starts in its plugin directory with a minimal environment:
`PATH`, `HOME`, `LANG`, `LC_ALL`, `TMPDIR`, `TZ`, the manifest's `env`, plus
`PLUGRELAY_PLUGIN_ID`, `PLUGRELAY_PROTOCOL_VERSION` and, for `unix`,
`PLUGRELAY_SOCKET`. The host's own environment is not inherited.

## 2. Lifecycle

```
host                              plugin
 |-- initialize ------------------->|
 |<------------------------ result -|
 |-- method calls / host calls <--->|
 |-- shutdown --------------------->|
 |<------------------------ result -|
 |-- exit (notification) ---------->|   process exits
```

### initialize

Params:

```json
{"protocol_version": 1, "host_version": "0.4.0", "plugin_id": "ip-allowlist",
 "settings": {"allow": ["10.0.0.0/8"]}}
```

`settings` holds the global-layer values of the settings the manifest
declares. Per-request merged values arrive with each call (see `call`).

Result:

```json
{"protocol_version": 1,
 "manifest": {"id": "ip-allowlist", "version": "0.1.0"},
 "capabilities": {"points": ["middleware.onRequest"]}}
```

The host rejects the handshake (and retries with backoff) when
`protocol_version` differs, `manifest.id` differs from the manifest, or
`capabilities.points` is non-empty and lacks a point the manifest lists. The
handshake must finish within `timeouts.startup` (default 10s).

### shutdown and exit

`shutdown` asks the plugin to finish in-flight work and release resources; it
answers `{}`. The `exit` notification follows; the plugin then exits. The
host kills the process after `timeouts.shutdown` (default 5s).

### Supervision

- Each call has a timeout (`timeouts.call`, default 5s; `timeouts.send`,
  default 10m, for `provider.send`; `timeouts.methods` per method) that also
  covers waiting for one of `max_concurrency` slots (default 16).
- On timeout the host sends `$/cancelRequest` and fails the call.
- A process that exits is restarted with exponential backoff (500ms doubling
  to 30s, reset after 30s of uptime). While it is down, calls fail at once.
- A failed, timed-out or unanswered call is resolved by the plugin's
  `fail_policy`: `closed` rejects the request, `open` lets it continue.
  Defaults: `open` for sink and job, `closed` for everything else.

### Cancellation

`$/cancelRequest` `{"id": <request id>}` may be sent by either side. The
receiver stops work if it can; the response, if still sent, is ignored.

## 3. Common objects

All durations on the wire are milliseconds (`*_ms`) and all times RFC 3339.
Headers are objects of string arrays (`{"User-Agent": ["x/1"]}`). Neutral
types (`request`, `response`, `usage`, `tool_call`, `tool_result`, `event`)
use the JSON encoding of `pkg/ir`; `record` uses `plugin.Record`, whose
`ttfb` and `duration` are nanoseconds.

### call

Sent as `call` with every request-scoped method.

```json
{"id": "req_01H...", "caller": {"key_id": "k1", "key_name": "ci", "user_id": "u1", "group_id": "g1"},
 "protocol": "anthropic", "model": "claude-sonnet", "stream": true,
 "session_id": "s-1", "client_ip": "203.0.113.7",
 "header": {"User-Agent": ["claude-cli/2.0"]},
 "settings": {"allow": ["10.0.0.0/8"]},
 "state": {"n": 1},
 "request": { "...": "ir.Request, only with request_body: true" }}
```

- `header` never contains `Authorization`, `X-Api-Key`, `X-Goog-Api-Key`,
  `Cookie` or `Proxy-Authorization`.
- `settings` are merged Global → Group → User → Key for this caller. A
  `strictest` list set on more than one layer arrives as the entries every
  layer lists, or `["\u0000deny-all"]` (matches nothing) when they share
  none; `setting_layers` then holds each layer's own list, nearest last.
  Check an entry against every layer's list when entries are patterns or
  ranges (CIDRs, globs), since those do not intersect as strings.
- `state` is whatever this plugin returned as `state` from an earlier hook of
  the same request.

### error

Used for replies, verdicts, stream errors and rejections.

```json
{"kind": "rate_limited", "message": "slow down", "retry_after_ms": 7000,
 "reset_at": "2026-01-01T00:00:00Z", "no_retry": false, "status": 429,
 "code": "rate_limit_exceeded", "tokens_used": 0, "tokens_limit": 0,
 "header": {"Retry-After": ["7"]}}
```

`kind` is an `ir.ErrorKind`: `invalid_request`, `context_too_long`,
`authentication`, `permission`, `not_found`, `rate_limited`,
`quota_exhausted`, `overloaded`, `content_filtered`, `capability_missing`,
`timeout`, `internal`, `upstream`. The client receives it encoded in its own
protocol.

### http_request / http_response

```json
{"method": "POST", "url": "/api/plugins/x/stats?d=1", "host": "relay", "path": "/api/plugins/x/stats",
 "query": "d=1", "header": {}, "remote_addr": "203.0.113.7:5123",
 "body": "text body", "body_base64": "", "timeout_ms": 0}
{"status": 200, "header": {}, "body": "text", "body_base64": ""}
```

A body travels in `body` when it is valid UTF-8, otherwise in `body_base64`.

## 4. Plugin methods

A plugin implements the methods its manifest lists in `points`. The host never
calls others; a plugin answers unknown methods with `-32601`.

| method | params | result |
|-|-|-|
| `auth.authenticate` | `request` (http_request, all headers) | `caller` or `null` (let the next authenticator try); reject with `-32001` |
| `middleware.onRequest` | `call` | `reply` (`{error}` or `{response: ir.Response}`) or `null`; `state` |
| `middleware.onAttempt` | `call`, `attempt` | `set_header` (object of strings); `state` |
| `middleware.onFinish` | `call`, `result` | `{}` |
| `selector.rank` | `call`, `candidates` | `order`: candidate indexes |
| `meter.reserve` | `call`, `estimate_micro` | `reservation` `{id, amount_micro}` or `null`; reject with `-32001` |
| `meter.settle` | `call`, `reservation`, `usage`, `cost_micro` | `{}` |
| `meter.refund` | `call`, `reservation` | `{}` |
| `sink.consume` | `record` | `{}` |
| `job.run` | `{}` | `{}` |
| `admin.handle` | `route` (manifest path), `request` (http_request) | http_response |
| `provider.endpoints` | `upstream` | `endpoints`, `owned_headers` |
| `provider.prepare` | `outgoing` | `header`, `url` |
| `provider.send` | `token`, `outgoing` (with body), `request` | `outcome` or `null`, after `$/event`s |
| `provider.classify` | `outcome` | `verdict` |
| `provider.listModels` | `upstream`, `credential` | `models` (`[]ir.ModelInfo`) |
| `provider.refresh` | `upstream`, `credential` | `secret`, `expires` |
| `provider.quota` | `upstream`, `credential` | `quota` `{remaining, left?, limit?, reset_at?}` (remaining: fraction, -1 unknown; 0 keeps the credential out until `reset_at`) |

There is no `middleware.onEvent`: per-event hooks cost a process round trip
per streamed event and are only available to built-in plugins.

### Call timing

Per request: `auth.authenticate`, `middleware.onRequest`, `selector.rank`,
`meter.reserve`, then `middleware.onFinish` and `meter.settle`/`meter.refund`.
Per attempt (again on every retry): `middleware.onAttempt`,
`provider.prepare`, `provider.send`, `provider.classify`. Pre-hooks run in
configuration order, post-hooks in reverse, and a post-hook runs only if its
pre-hook ran. `meter.settle` and `meter.refund` must be idempotent per
`call.id`.

### middleware

`attempt`:

```json
{"n": 1, "tier": "passthrough", "upstream_model": "gpt-x", "header": {},
 "candidate": {"upstream_id": "u", "endpoint_id": "e", "credential_id": "c", "protocol": "openai-chat",
   "upstream_model": "gpt-x", "priority": 0, "weight": 1, "same_protocol": true, "sticky": false,
   "fidelity": "lossless", "latency_ms": 420, "in_flight": 2, "price_in_micro": 0, "price_out_micro": 0,
   "quota_reset": "2026-01-01T00:00:00Z"}}
```

`set_header` may not set credential headers; provider-owned headers are
overwritten later by the provider. External middleware cannot change the
request body.

`result` (onFinish): `{"status": 200, "error": null, "usage": {...}, "attempts": 1, "delivered": true}`.

### selector

`order` may only reorder: unknown or repeated indexes are ignored and missing
candidates keep their order at the end. On failure the router's order stands.

### sink

`record` is one finished request (`plugin.Record`); `id` is the relay's
request id. A sink with `fail_policy: closed` is a ledger: the host retries a
failed `sink.consume` with backoff (100ms, 500ms, 2s) and may deliver the same
record again after a timeout it could not tell from a failure, so it must
treat a repeated `record.id` as already stored (an insert that ignores
duplicates). Each ledger sink has its own queue, so a slow one only delays
itself. `open` sinks get each record at most once and may lose some under
overload.

### provider

`upstream`: `{"id", "provider", "endpoints": [plugin.Endpoint], "config": {}}`.
Without `provider.endpoints`, the configured endpoints are used and
`provider.owned_headers` from the manifest applies.

`outgoing`:

```json
{"upstream": {...}, "endpoint": {"id": "e", "protocol": "openai-chat", "base_url": "https://api.example.com", "issuer": "example"},
 "credential": {"id": "c", "upstream_id": "u", "kind": "shared", "secret": "sk-...", "meta": {}},
 "model": "upstream-model", "method": "POST", "url": "https://api.example.com/v1/chat/completions",
 "header": {"Content-Type": ["application/json"]},
 "body": {"...": "provider.send only: the body as JSON, or body_base64"}}
```

`provider.prepare` returns the complete outgoing header (it replaces the
host's) and optionally a new absolute `url`. It must set or delete every
header it lists as owned; client values of owned headers never reach the
upstream.

`provider.send` performs the upstream exchange itself and streams neutral
events as notifications carrying the call's token:

```json
{"jsonrpc": "2.0", "method": "$/event", "params": {"token": "6f1c...", "event":
  {"kind": "delta", "index": 0, "delta": {"kind": "text", "text": "Hel"}}}}
```

Rules:

1. Events follow `ir.Event` ordering: `start`, then per block `block_start`,
   `delta`s, `block_stop`; indexes dense from 0; `ping`, `status` and `usage`
   anywhere.
2. The stream ends with exactly one `done` or `error` event, then the
   response `{"outcome": null}`.
3. A failure **before any content** is not streamed: respond
   `{"outcome": {...}}` (no events) so the host can classify and retry on
   another candidate. Set `sent: false` when the request never left.
4. A failure **after content** is an `error` event; the attempt cannot be
   retried. If the process dies mid-stream the host emits it.
5. Event `error` objects use the wire `error` form.

`outcome` (classify input and send failure):

```json
{"endpoint_id": "e", "credential_id": "c", "status": 429, "header": {"Retry-After": ["7"]},
 "body": "{\"error\":...}", "error": "transport error text", "signal": {"kind": "rate_limited", "message": "..."},
 "sent": true}
```

`signal` is the host codec's reading of the error body. `verdict`:

```json
{"cause": "rate_limit", "retry": "next", "credential": "cooldown",
 "until": "2026-01-01T00:00:07Z", "model_scope": false,
 "downgrade": {"reasoning.effort": "\"high\""},
 "client_error": {"kind": "rate_limited", "message": "...", "retry_after_ms": 7000},
 "sent": true}
```

- `cause`: `request`, `transient`, `rate_limit`, `quota`, `auth_expired`,
  `banned`, `content`, `unknown`.
- `retry`: `none`, `next`, `downgrade` (resend to the same upstream with
  `downgrade` edits: gjson path → JSON value).
- `credential`: `keep`, `cooldown` (until `until`), `refresh`, `quarantine`,
  `manual`.

Verdict rules every provider must follow (checked by `pkg/plugin/plugintest`):
content violations are never retried on any credential; a suspected suspension
quarantines without retry; when unsure between the two, quarantine without
retry; failures that are not the credential's fault leave it alone; a request
that was not sent is safe to retry. If `provider.classify` fails, the host
keeps the credential and retries only unsent requests.

## 5. Host methods

The plugin may call these on the host. Each except `host.log` requires its
name under `permissions.host` in the manifest; otherwise `-32002`.

| method | params | result |
|-|-|-|
| `host.log` | `level` (`debug`/`info`/`warn`/`error`), `message`, `attrs` | `{}` |
| `host.kv.get` | `key` | `found`, `value` |
| `host.kv.put` | `key`, `value`, `ttl_ms` (0 = no expiry) | `{}` |
| `host.kv.delete` | `key` | `{}` |
| `host.kv.incr` | `key`, `delta`, `ttl_ms` | `value` (new integer) |
| `host.http` | http_request (absolute `url`, optional `timeout_ms`, `upstream_id`, `credential_id`) | http_response |

KV values are strings, scoped to the plugin; base64-encode binary data.
`host.http` goes through the core's egress client, so its SSRF guard
applies. A call made for an upstream credential (a provider's `send`,
`listModels`, `quota`, `refresh`) passes that `upstream_id` and
`credential_id`: the request then takes the credential's egress, its proxy
or else the upstream's, and fails rather than going direct when that proxy
is down. Without them the request goes direct. A plugin is free to open its own connections (the host
cannot prevent it), but those bypass those rules and must be declared in
`permissions.network`.

## 6. Errors

| code | meaning | host behaviour |
|-|-|-|
| -32700 | parse error | |
| -32600 | invalid request (also: unsupported protocol version) | |
| -32601 | method not found | treated as not implemented |
| -32602 | invalid params | |
| -32603 | internal error | |
| -32000 | plugin error: the handler failed | fail policy applies |
| -32001 | rejected: a deliberate refusal; `data` is an `error` object | sent to the client as is; no fail policy |
| -32002 | permission denied (host API not in `permissions.host`) | |
| -32003 | not initialized | |
| -32004 | host API unavailable (not configured on this host) | |
| -32800 | request cancelled | |

A middleware may refuse either way: `reply.error` or `-32001`. Use
`-32001` for authentication (`kind: authentication`) and meter refusals
(`kind: quota_exhausted`).

## 7. Versioning

- `protocol_version` is a single integer, bumped only for incompatible
  changes (removed or retyped fields, changed semantics, new required
  behaviour). The host speaks exactly one version; a plugin built for another
  fails its handshake with a clear error.
- Additive changes keep the version: new optional fields, new methods, new
  enum values. Both sides ignore unknown fields; plugins answer unknown
  methods with `-32601`; unknown enum values degrade to the most conservative
  meaning (`cause: unknown`, `retry: none`, `credential: keep`).
- A plugin is only called on the points it lists in `points`, so a new
  extension point never reaches a plugin that does not list it.

## 8. plugin.yaml

Every plugin directory holds a `plugin.yaml`. Unknown keys are errors.
[`plugin.schema.json`](plugin.schema.json) is its JSON Schema for editors;
the loader in `internal/extplugin` also enforces the cross-field rules below.

```yaml
id: ip-allowlist              # [a-z0-9][a-z0-9-]*; the core sees "<kind>.<id>"
name: IP allowlist
version: 0.1.0
description: Reject requests from addresses outside the allowed ranges.
kind: middleware              # or kinds: [middleware, admin]
points: [middleware.onRequest]

command: ./ip-allowlist       # a bare name is looked up in PATH;
args: []                      # a relative path resolves against this directory
env: {}                       # PLUGRELAY_* names are reserved
transport: stdio              # stdio | unix

timeouts:                     # Go durations
  startup: 10s
  call: 5s
  send: 10m                   # provider.send
  shutdown: 5s
  methods: {middleware.onRequest: 500ms}
max_concurrency: 16
fail_policy: closed           # closed | open
request_body: false           # include the decoded request in middleware calls
reports_off_box: false        # true if it sends data off the box; test mode skips it
requires: []                  # plugin ids it cannot work without, e.g. [sink.requestlog]

settings:                     # plugin.Setting; the console renders a form
  - key: allow
    type: list                # string int float bool duration list secret model upstream group json
    default: []
    scopes: [global, group, key]
    merge: strictest          # override | strictest | union (lists only)
    description: CIDR ranges allowed to call the relay.
    restart: false
    enum: []                  # string settings only

permissions:
  host: [kv, http]            # host APIs beyond host.log
  network: none               # shown to the admin; not enforced
  files: none

provider:                     # kind provider
  owned_headers: [Authorization]
  issuer: example
job:                          # kind job
  every: 1h
admin:                        # kind admin; mounted under /api/plugins/<id>/
  routes:
    - {method: GET, path: /stats, role: admin}    # role: admin | member | "" (any user)
```

Validation also requires: every kind has at least one point and its required
ones (auth: `auth.authenticate`; meter: all three; selector, sink, job,
admin: their single method; provider: `provider.prepare` or `provider.send`);
`middleware.onEvent` is rejected; a `secret` setting has no default.
