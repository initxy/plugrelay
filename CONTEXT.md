# plugrelay domain glossary

Stable terms used across code and docs. Each entry points at the code that
defines it; read the code for details. How the pieces fit together:
`docs/architecture.md`.

## Codec / protocol

One wire protocol, both directions: client side (decode requests, encode
responses, streams, errors, model lists) and upstream side. Registered by
name: `anthropic`, `openai-chat`, `openai-responses`, `openai-completions`,
`gemini`, and the media codecs `openai-embeddings`, `gemini-embeddings`,
`openai-images`, `openai-audio`, `rerank`, `cohere-rerank`, `tei-rerank`
(upstream only).
Contract: `pkg/protocol/codec.go`; one package per codec under `pkg/protocol/`.

## IR

The protocol-neutral request/response/event model every codec maps to.
Union of what the protocols express; anything without a slot rides in
`Extra` verbatim. `pkg/ir/`

## Capabilities

What a protocol can express, or what an endpoint supports (cache mode, tool
kinds, reasoning style, echo fields, server state, context size, output-limit
field…). Effective capabilities = codec ∩ endpoint. `pkg/ir/capability.go`

## Needs

What a decoded request requires from an endpoint (breakpoints, freeform
tools, images, opaque history, server state…). Computed by the codec while
decoding; used to grade endpoints. `pkg/ir/capability.go`

## Fidelity

Grade of converting one request for one endpoint: lossless, lossy,
discouraged (excluded unless allowed), impossible (never routed). Rules in
the package doc of `internal/convert/convert.go`.

## Tier

Processing level of one attempt: `passthrough` (body bytes untouched),
`patch` (named fields edited with sjson), `convert` (IR decode → generic
steps → encode). Lowest possible tier wins. `pkg/plugin/call.go`

## Upstream / endpoint

An upstream is a configured provider instance with credentials; an endpoint
is one protocol entry of it (protocol, base URL, capabilities, issuer).
One upstream may expose several endpoints. `pkg/plugin/points.go`

## Manifest / interpreter

Declarative YAML describing an upstream (endpoints, auth, owned headers,
capabilities, error rules, models, prices, quota probe). Executed by a
generic provider named by the endpoint's interpreter. Types:
`plugins/provider/manifest/manifest.go`; official ones in `manifests/`.

## Issuer group

Set of endpoints that accept each other's upstream-private state. Declared
per endpoint, defaults to the manifest id. `plugin.Endpoint.Issuer` in
`pkg/plugin/points.go`

## Opaque state / seal

Upstream-private data a client must echo back (thinking signatures,
encrypted reasoning, thought signatures, compaction). Crossing protocols it is
sealed (AES-256-GCM under a master-key-derived key, issuer bound) and only
restored for the same issuer; failures drop it silently. `internal/seal/seal.go`

## Credential kinds

`shared` (pooled), `personal` (routable only for its owner), `service`.
Durable status set by operators vs. live status (cooldown, quarantine,
breaker) held in state. `internal/router/table.go`, `internal/router/health.go`

## Candidate

One way to serve a request: upstream + endpoint + credential + upstream
model, with priority, weight, sticky flag and fidelity. Selectors may only
reorder candidates. `plugin.Candidate` in `pkg/plugin/points.go`

## Preset

A client-facing name served by one of several models, picked in order,
cheapest or fastest. Listed like a model; used only when a client asks for
it by name; shares the model namespace. `store.Preset`,
`router.Table.Presets`

## Routing strategy

How planned candidates are ranked: `priority_weight`, `cheapest`,
`fastest`, `quota_reset`; one setting of the always-on `selector.routing`
(global or per group). `plugins/selector/routing`

## Plan / plan error

The router's ordered candidates for a request. When none remain the plan
error kind (`no_route`, `impossible`, `cooling`, `busy`, `exhausted`) picks
the client error. `internal/router/plan.go`, `internal/router/errors.go`

## Verdict

A provider's structured judgment of a failed attempt: cause, retry mode
(none / next / downgrade), credential action, cooldown scope, client error,
and whether the request was sent. The core only executes it.
`pkg/plugin/points.go`

## Session key / binding

Session id resolved per request (client header → body cache key or user tag
→ hash of key, model and conversation prefix; sub-agent id appended). A
binding pins a session to a candidate while its prompt cache is warm.
`internal/pipeline/doc.go`, `internal/router/session.go`

## Turn token

Relay-signed response header a client echoes within one turn, pinning the
next request to the same candidate. `internal/router/token.go`

## Snapshot

Immutable copy of routing-relevant business data, rebuilt when the store's
version counter changes and swapped atomically; requests in flight keep the
old one. `internal/store/snapshot.go`, `internal/pipeline/deps.go`

## Store / state

Store: durable business data (SQLite, PostgreSQL or MySQL). State: shared, losable
runtime data (memory, Redis, or SQL) — bindings, cooldowns, counters, locks.
`internal/store/store.go`, `internal/state/state.go`

## Settings layers

Plugin settings merge Global → Group → User → Key. Each setting declares its
merge: `override` (nearest wins), `strictest` (lower layers only tighten;
a key may be exempted) or `union` (lists add up: deny lists only grow).
A few group and key layers are fields of the group or key record, never
stored settings (`settings.RecordOwned`).
`pkg/plugin/settings.go`, `internal/settings/`

## Platform key

Client credential `pr_…` (or `sk-…` carried over by the new-api import);
stored as SHA-256 hash plus last four characters.
The key record holds the key's own limits (budget, rpm, concurrency, models,
IP allowlist, clients, expiry); `auth.apikey` and the modules read them there.
`internal/secret/keys.go`, `plugins/auth/apikey/apikey.go`

## Master key

32 random bytes (base64) from relay.yaml, env or a key file. HKDF subkeys
encrypt credential secrets, secret settings and seals; previous master keys
only open seals. Losing it makes stored credentials unreadable.
`internal/secret/secret.go`, `internal/seal/seal.go`

## Feature / module / extension

A feature is what an operator switches: one switch and one settings form
over built-in modules (plugins), plus the console pages it adds by stable
page id; state in the store, default in the registry. Core modules always
run. Any other plugin is an extension, switched on its own.
`internal/features`, `App.states` in `internal/app/plugins.go`

## Plugin kinds and tiers

Extension points: provider, auth, middleware, selector, meter, sink, job,
admin. Written as a YAML manifest (providers only), an external process,
or built-in Go; all look the same to the core. `pkg/plugin/plugin.go`

## FailPolicy / Mutates

FailPolicy: `closed` rejects the request when a plugin malfunctions, `open`
carries on; a `closed` sink is a ledger sink (records never dropped,
delivered at least once — ADR-0012). Mutates: gjson paths a plugin may edit;
a plugin declaring none is read-only, which keeps passthrough byte-exact.
Requires: plugin ids a plugin cannot run without. `pkg/plugin/plugin.go`

## Record / attempt

A finished request as handed to sinks: caller, protocol, model, status, every
attempt (tier, verdict cause, usage, cost, diagnostic), cost vs. charge.
`plugin.Record` in `pkg/plugin/points.go`

## Client id

The client program behind a request (`claude-code`, `codex`, `sdk-python`,
… or `other`), named from its headers by one ordered rules table; stored on
the record and in the hourly roll-ups; a key's and a group's allowed clients
are these ids.
Distinct from the client protocol.
`pkg/clientid`

## Usage buckets

Five non-overlapping counts — uncached input, cache read, 5-minute cache
write, 1-hour cache write, output — plus reasoning (part of output) and the
upstream's raw usage. `pkg/ir/response.go`

## Cost / charge

Cost: operator's upstream spend, recorded for every attempt that reached an
upstream. Charge: what the caller is billed, only for the delivered attempt:
the served model's list price (else that attempt's cost rate) × the caller's group
price multiplier, recorded on the attempt as `plugin.Charge`. Money is integer
micro-units. `pkg/plugin/points.go`, `internal/pipeline/cost.go`

## Native passthrough

`/native/<upstream>/<path>`: raw forward for APIs outside the IR (non-batch
files, fine-tuning, vendor-only). Off by default, granted per key; no plugins, no
metering. ADR-0007, `internal/pipeline/native.go`.

## WebSocket session

`/v1/realtime` (endpoint capability `realtime`) or the Responses WebSocket
mode (`responses_websocket`): one upstream WebSocket per client WebSocket,
the credential's slot held for the session, each finished response metered
and logged as its own request under the session's id.
`internal/pipeline/websocket.go`, [docs/clients.md](docs/clients.md#realtime-and-websocket).

## MCP gateway

`/mcp/<server>` and `/mcp` (all granted servers, tools named
`<server>__<tool>`): configured upstream MCP servers behind the platform
key while the `mcp` feature is on, granted per key in `middleware.access`
settings; tool calls logged as protocol `mcp`. `internal/mcp`, [docs/mcp.md](docs/mcp.md).

## Batch object

A file or batch created through the batch APIs, recorded with the upstream
credential it lives on and its owner, so later calls reach that credential
and nobody else sees it. Ids are the upstream's.
`internal/pipeline/batch.go`, `internal/store/batches.go`
