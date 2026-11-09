# Configuration

English | [简体中文](configuration.zh-CN.md)

plugrelay has three kinds of configuration, each with one home:

| what | where | who writes it |
|-|-|-|
| how the process starts: listen address, trusted proxies, master keys, database, state backend, egress, limits, external plugin directory, logging, shutdown, native passthrough, WebSocket bounds | `relay.yaml` + `PLUGRELAY_*` environment variables | you, by hand; plugrelay never writes it |
| business data: upstreams, credentials, models and routes, presets, groups, prices, which features are on, extensions, manifests, site settings (public URL, site name, currency, time zone, sign-in rules) | the store (SQLite, PostgreSQL or MySQL) | console, admin API, `plugrelay upstream/model/preset/key/apply` |
| module settings layered Global → Group → User → Key | the store | a feature's form (global), a group's, user's or key's own settings, or `plugrelay apply` |

The authoritative definitions are `internal/config/config.go` (relay.yaml),
`internal/store/yamlio.go` (business data YAML) and `pkg/plugin/settings.go`
(settings). An annotated sample is [`examples/relay.yaml`](../examples/relay.yaml).

- [relay.yaml reference](#relayyaml-reference)
- [Features](#features)
- [Where relative paths resolve](#where-relative-paths-resolve)
- [Routing, models and presets](#routing-models-and-presets)
- [Layered plugin settings](#layered-plugin-settings)
- [Business data as YAML](#business-data-as-yaml)

## relay.yaml reference

Unknown keys are errors, so typos fail at startup. Every problem is reported
at once with its field path (`trusted_proxies[2]: want a CIDR or an IP`). Check a file with
`plugrelay doctor`.

The config file is `./relay.yaml` unless `--config FILE` or
`PLUGRELAY_CONFIG` says otherwise.

### Top level

| field | default | env | meaning |
|-|-|-|-|
| `listen` | `127.0.0.1:8787` | `PLUGRELAY_LISTEN` | `host:port`. Keep loopback unless a reverse proxy or a team needs access. On a loopback-only listener the console on this machine needs no login (site setting `security.local_owner_login`, [API auth](api.md)). |
| `trusted_proxies` | `[]` | `PLUGRELAY_TRUSTED_PROXIES` (comma-separated) | CIDRs or IPs of reverse proxies in front of the relay. Only when the TCP peer is one of them is the client address taken from `X-Forwarded-For` (walked right to left, skipping trusted hops) or else `X-Real-IP`. Empty: forwarding headers are ignored. Feeds key and `middleware.access` IP allowlists, per-IP rate limits, request logs, admin login throttling and the audit log; the local-owner check always uses the TCP peer. |
| `master_key` | empty | `PLUGRELAY_MASTER_KEY` | 32 random bytes, base64 (standard or URL alphabet). Encrypts credentials, secret settings and sealed private state. Set this or `master_key_file`, not both. |
| `master_key_file` | empty (`plugrelay init` writes `master.key`) | `PLUGRELAY_MASTER_KEY_FILE` | File holding the master key. Setting either env variable clears the other field. |
| `previous_master_keys` | `[]` | `PLUGRELAY_PREVIOUS_MASTER_KEYS` (comma-separated) | Master keys `master_key` replaced, in the same format. They only open private state sealed before the change; stored credentials and secret settings still need the key they were saved under ([rotation](deployment.md#master-key-rotation)). |

Without a master key, commands that store secrets fail (`store: no master
key configured`). Losing the key makes stored credentials unreadable
([backups](deployment.md#backups)).

### store

| field | default | env | meaning |
|-|-|-|-|
| `store.driver` | `sqlite` | `PLUGRELAY_STORE_DRIVER` | `sqlite` (pure Go, single node), `postgres`, or `mysql` (MySQL 8.0.13+ or MariaDB 10.6+). |
| `store.dsn` | `plugrelay.db` | `PLUGRELAY_STORE_DSN` | SQLite: a file path (or `file:` URI). PostgreSQL: `postgres://user:pass@host:5432/db?sslmode=…`. MySQL: a go-sql-driver DSN, `user:pass@tcp(host:3306)/db` (the database must exist; `tls=true` etc. pass through, while utf8mb4 with binary collation, matched-row counts, a session `sql_mode` and READ COMMITTED are forced, and `LOAD DATA LOCAL` is refused). Required. |
| `store.max_conns` | `0` | `PLUGRELAY_STORE_MAX_CONNS` | Open database connections per node; `0` = 8 (SQLite) or 20 (PostgreSQL, MySQL). Keep nodes × this under the server's `max_connections`. |

Migrations are embedded and applied when the store opens.

### state

| field | default | env | meaning |
|-|-|-|-|
| `state.driver` | `memory` | `PLUGRELAY_STATE_DRIVER` | `memory` (single node, lost on restart), `sql` (single node, kept in the store database across restarts), `redis` (required when more than one node serves traffic). |
| `state.url` | empty | `PLUGRELAY_STATE_URL` | `redis://user:pass@host:6379/0`. Required for `redis`. |

Losing state costs prompt-cache hits and counters, never correctness.

### external_plugins

| field | default | env | meaning |
|-|-|-|-|
| `external_plugins.enabled` | `false` | `PLUGRELAY_EXTERNAL_PLUGINS_ENABLED` | Turn the external process tier on. Off means no plugin process is ever started. |
| `external_plugins.dir` | `plugins` | `PLUGRELAY_EXTERNAL_PLUGINS_DIR` | Directory scanned for plugin directories (each with a `plugin.yaml`). Required when enabled. |

See [plugins.md](plugins.md#external-process-plugins).

### log

| field | default | env | values |
|-|-|-|-|
| `log.level` | `info` | `PLUGRELAY_LOG_LEVEL` | `debug`, `info`, `warn`, `error` |
| `log.format` | `text` | `PLUGRELAY_LOG_FORMAT` | `text`, `json` |

### egress

| field | default | env | meaning |
|-|-|-|-|
| `egress.allow_private_cidrs` | `[]` | `PLUGRELAY_EGRESS_ALLOW_PRIVATE_CIDRS` (comma-separated) | Private and loopback addresses are blocked to prevent SSRF; list ranges here for self-hosted upstreams (vLLM, Ollama, LM Studio). |
| `egress.default_timeouts.first_byte` | `2m` | `PLUGRELAY_EGRESS_FIRST_BYTE_TIMEOUT` | until the first byte of the upstream response |
| `egress.default_timeouts.idle` | `5m` | `PLUGRELAY_EGRESS_IDLE_TIMEOUT` | between two chunks of a stream |
| `egress.default_timeouts.total` | `30m` | `PLUGRELAY_EGRESS_TOTAL_TIMEOUT` | whole attempt; must not be shorter than the other two |

Durations accept Go syntax (`90s`, `2m`) or a number of seconds. All three must
be positive.

Each upstream can override these in the store (console: upstream → Advanced;
admin API: `headers`, `proxy`, `timeouts` on the upstream):

- `timeouts.connect_ms / first_byte_ms / idle_ms / total_ms` replace the
  defaults field by field; 0 keeps the default. Connect covers dial, proxy and
  TLS and can only be shorter than the built-in 10 s dial and handshake limits.
- `proxy` is the egress of the upstream's credentials that set none; a
  credential's own proxy wins. As with a credential proxy, a dead proxy fails
  the attempt and never falls back to direct.
- `headers` are added to every request. Names the provider owns (its auth and
  account headers) are never overridden, and credential, framing, hop-by-hop
  and client-address headers are rejected (`egress.SetExtraHeaders`).

### limits

| field | default | env | meaning |
|-|-|-|-|
| `limits.max_body_bytes` | `33554432` (32 MiB) | `PLUGRELAY_LIMITS_MAX_BODY_BYTES` | request body as sent; also bounds streamed uploads |
| `limits.max_decompressed_bytes` | `134217728` (128 MiB) | `PLUGRELAY_LIMITS_MAX_DECOMPRESSED_BYTES` | after gzip/zstd decompression; must be ≥ `max_body_bytes` |

### shutdown

| field | default | env | meaning |
|-|-|-|-|
| `shutdown.grace` | `30s` | `PLUGRELAY_SHUTDOWN_GRACE` | how long in-flight requests keep running once shutdown starts (SIGINT or SIGTERM); their connections are then closed |
| `shutdown.drain` | `60s` | `PLUGRELAY_SHUTDOWN_DRAIN` | how long the relay then waits for cut-off requests to settle quota and for queued records to reach the sinks (the request log); the queue always gets at least 10 s more. Records still undelivered after it are logged and counted as lost. `0` means the default |

The process manager must wait longer than both together before it kills the
relay: see [graceful shutdown](deployment.md#graceful-shutdown).

### console

| field | default | env | meaning |
|-|-|-|-|
| `console.enabled` | on | `PLUGRELAY_CONSOLE_ENABLED` | serve the web console and admin API |

With the console off, the admin API under `/api/v1` is still served; only the
web pages are not (`internal/app/app.go`).

Single sign-on providers are not in relay.yaml: they are business data set
in the console ([deployment](deployment.md#single-sign-on)), and so are the
public URL (site setting `site.public_base_url`), the two-step sign-in
policy and the login captcha (site settings `security.*`,
[deployment](deployment.md#two-step-sign-in-passkeys-and-captcha)).

### native

| field | default | env | meaning |
|-|-|-|-|
| `native.enabled` | `false` | `PLUGRELAY_NATIVE_ENABLED` | serve [`/native/<upstream>/<path>`](clients.md#native-passthrough); off, the route answers 404 |
| `native.max_body_bytes` | `536870912` (512 MiB) | `PLUGRELAY_NATIVE_MAX_BODY_BYTES` | one native request body (uploads stream through, they are not buffered); `0` means the default |

Switching it on grants nothing by itself: each key also needs the
`middleware.access` setting `native_upstreams` (a list of upstream ids, `*`
for all; empty = none) on the global, group, user or key layer. Unlike the
other access lists it is a grant merged `override` — the nearest layer that
sets it wins — so one group or one key can be let in.

### websocket

Limits of the [WebSocket sessions](clients.md#realtime-and-websocket)
(`/v1/realtime`, the Responses WebSocket mode).

| field | default | env | meaning |
|-|-|-|-|
| `websocket.max_session` | `30m` | `PLUGRELAY_WEBSOCKET_MAX_SESSION` | a session is closed after this long; at most `30m`, the lifetime of a concurrency slot (the credential's and `middleware.ratelimit`'s) |
| `websocket.idle_timeout` | `5m` | `PLUGRELAY_WEBSOCKET_IDLE_TIMEOUT` | closed when no message crossed either way for this long |
| `websocket.session_ready_timeout` | `10s` | `PLUGRELAY_WEBSOCKET_SESSION_READY_TIMEOUT` | on Realtime, client events wait for the upstream's `session.created` when body mutators or the transcription check need it; without it this long the session is closed (`error` event `session_not_ready`, then `1013`) |
| `websocket.max_message_bytes` | `16777216` (16 MiB) | `PLUGRELAY_WEBSOCKET_MAX_MESSAGE_BYTES` | one message in either direction; a larger one closes the session with `1009` |
| `websocket.response_reserve_tokens` | `4096` | `PLUGRELAY_WEBSOCKET_RESPONSE_RESERVE_TOKENS` | output tokens the meter holds for a response that sets no cap of its own ([billing](clients.md#realtime-and-websocket)) |

### MCP gateway

`/mcp` has no relay.yaml fields: the gateway runs while the `mcp` feature
is on, MCP servers are business data set in the console or
`/api/v1/mcp/servers`, and keys get them through the `middleware.access`
settings `mcp_servers`, `mcp_tools_allow` and `mcp_tools_deny`
([mcp.md](mcp.md#access)).

### Other environment variables

| variable | read by | meaning |
|-|-|-|
| `PLUGRELAY_CONFIG` | every command | path of relay.yaml (same as `--config`) |
| `PLUGRELAY_DATA_DIR` | every command | default for `--data-dir` (the Docker images set it to `/data`) |
| `PLUGRELAY_ADMIN_PASSWORD` | `plugrelay init`, `plugrelay user reset-password` | admin password, so it stays out of shell history |
| `PLUGRELAY_API_KEY` | `plugrelay setup`; Codex | platform key; `setup codex` makes Codex read it from here |
| `PLUGRELAY_ADMIN_TOKEN` | `serve` | operator Bearer token for the admin API and `/metrics` |
| `PLUGRELAY_TEST_MODE` | `serve` | any value drops every plugin that reports data off the box |
| `PLUGRELAY_TEST_PG`, `PLUGRELAY_TEST_MYSQL`, `PLUGRELAY_E2E_BASE`, `PLUGRELAY_E2E_KEY`, `PLUGRELAY_E2E_MODEL` | tests only | PostgreSQL URL / MySQL DSN (`root:pw@tcp(127.0.0.1:3306)/db`; each test creates and drops its own database, so the user needs `CREATE`/`DROP`) for the store, state and admin suites; live OpenAI-compatible upstream (base URL with `/v1`, key, model — default `gpt-5.4`) for `make e2e` |
| `PLUGRELAY_E2E_CLIENTS`, `PLUGRELAY_E2E_CHAT_MODEL` | tests only | `1` also drives the installed Claude Code and Codex CLIs through the relay on a write-and-run task, over the main model and a chat-only route (default `deepseek-chat`); config goes to a temp home via `plugrelay setup` (`test/e2e/clients_test.go`) |
| `PLUGRELAY_E2E_IMAGE_MODEL`, `_EMBED_MODEL`, `_TTS_MODEL`, `_STT_MODEL`, `_RERANK_MODEL`, `_REALTIME_MODEL`, `_BATCH_WAIT` | tests only | models for the media, rerank and realtime cases of `make e2e` on the same upstream, and how long to wait for a batch to finish (default 0: create, get and cancel only) |
| `PLUGRELAY_E2E_TEI_EMBED`, `_TEI_RERANK`, `_STT_BASE`; `PLUGRELAY_E2E_MCP` (`_MCP_EVERYTHING`, `_MCP_SDK`); `PLUGRELAY_E2E_CAPTCHA` | tests only | opt-in live suites: local TEI and whisper servers (`selfhosted_test.go`); `1` runs the MCP gateway against the official reference server and SDK, versions overridable (`mcp_test.go`); `1` checks captchas against Turnstile's and hCaptcha's real siteverify (`captcha_test.go`) |
| `PLUGRELAY_COMPAT_UPSTREAM`, `_DATE`, `_EVENTS` | `scripts/compat.sh` | upstream name and date in the compatibility report; a `go test -json` events file to keep, or to re-render the report from |

Defaults and details of each test variable are in the comment at the top of
its file in `test/e2e/`.

## Features

What runs is decided by `internal/features` (the registry: names, modules,
defaults, the settings each form shows, the console pages each adds), with
feature states in the store, applied on the next reload
(`App.states` in `internal/app/plugins.go`):

- **Core modules** always run: `auth.apikey`, `middleware.access`,
  `selector.routing`, `sink.requestlog`, `job.requestlog-retention`,
  `job.modelsync`, `job.quotaprobe`, and every `provider.*` an upstream uses.
- **Features** run their modules while on. Turning one on turns on what its
  modules require; a change that would leave a configuration the relay
  cannot run is refused (409). Switch them in the console, with
  `PATCH /api/v1/features/{id}`, or `features:` in `plugrelay apply`.
- **Extensions** (external process plugins, Go plugins of a custom build)
  start off and are switched one by one (`/api/v1/extensions`, `plugins:`
  in `plugrelay apply`); `apply` refuses `plugins:` entries for built-in
  modules.

| id | default | modules | what it does |
|-|-|-|-|
| `limits` | on | `middleware.ratelimit`, `meter.quota`, `middleware.param-policy` | requests/tokens per minute and concurrency per key, user or group; spend and token budgets (days and months start at midnight in the site time zone, site setting `site.timezone`, default UTC); caps on output tokens and reasoning effort, stripped parameters |
| `routing` | always | (core `selector.routing`, `job.modelsync`, `job.quotaprobe`) | the [routing strategy](#routing-models-and-presets) and the two jobs' default intervals |
| `request_log` | always | (core `sink.requestlog`, `job.requestlog-retention`) | whether request and response text is kept (`body_logging`, off) and for how many days entries stay (`retention_days`, 0 = forever) |
| `content_safety` | off | `middleware.guardrails`, `middleware.redact` | deny lists, PII and secret detectors, prompt size, blocked models and tools, moderation ([plugins](plugins.md#guardrails)); masking user text before it leaves |
| `billing` | off | `meter.balance`, `admin.payment`, `admin.redeem`, `admin.subscriptions`, `admin.import-newapi`, `admin.rewards`, `admin.announcements` | prepaid balance, payments (Stripe, epay) and coupons, redeem codes, subscription plans, check-in and referral rewards, announcements, new-api import ([sell kit](sell-kit.md)) |
| `signup` | off | `admin.signup` | public sign-up with optional invite codes and email domains |
| `sso` | off | — | single sign-on ([deployment](deployment.md#single-sign-on)): while off no provider is offered and password sign-in always works |
| `mcp` | off | — | the [MCP gateway](mcp.md); off, `/mcp` answers 404 |
| `notifications` | off | `job.notify`, `sink.webhook` | alerts by email, chat bots or webhook, and request records pushed to a webhook ([notifications](notifications.md)) |
| `price_sync` | off | `job.pricesync` | [price suggestions from models.dev](#price-sync) |
| `public_pages` | off | `admin.public-pages` | models-and-prices (`/public/models`) and status (`/public/status`) pages without sign-in, linked from the login page; one group's models and prices (`price_group`, empty = all at list price); upstream names only while `site.show_upstream_names` is on; answers cached for a minute, loads limited per address (`per_ip_per_minute`) |
| `metrics` | on | `sink.metrics` | Prometheus metrics, prefixed `plugrelay_`, at `/metrics` (behind `PLUGRELAY_ADMIN_TOKEN` when set) |

The setup wizard's "sell" answer turns `billing` on. An extension id that is
not installed fails that plugin (`plugin … is not installed`); `plugrelay
doctor` and the reload error on the features page show it.

## Where relative paths resolve

`master_key_file`, a plain SQLite `store.dsn` and `external_plugins.dir`
resolve against `--data-dir` (or `PLUGRELAY_DATA_DIR`) when given, otherwise against the directory of
the config file — not the working directory (`internal/cli/config.go`).
`plugrelay init` writes bare file names next to the config unless
`--data-dir` is set, so a data directory stays portable.

## Routing, models and presets

- **Routes** belong to a model: upstream, upstream model name, priority,
  weight, and the groups that may use it (`plugrelay model add --route`, the
  model's `routes` in the admin API and YAML). An upstream has no model map
  of its own.
- **Routing strategy**: the setting `strategy` of `selector.routing`, global
  or per group — `priority_weight` (default: priority, then the weighted
  split of new sessions), `cheapest` (upstream price: price rule, else the
  manifest's list price), `fastest` (recent time to first byte) or
  `quota_reset` (soonest quota reset first). Sticky candidates stay first
  whatever the strategy. An enabled external selector plugin ranks instead.
- **Presets**: a name, candidate models in order, and a pick strategy
  (`in_order`, `cheapest` or `fastest`; `plugrelay preset add`, `/presets`).
  A preset is listed in every client protocol's model list to callers who
  may use one of its models; asked for by name, it is served by one of its
  models the caller may use, picked by its strategy (in place of the routing
  strategy), with the next one tried when one fails. A model asked for by
  name is never swapped for another. The request log shows the preset as the
  requested model and the model that served it. Models and presets share one
  namespace.
- **Per upstream jobs** (admin API fields on the upstream): `model_sync`
  turns `job.modelsync` on or off for the upstream (default: on when its
  manifest lists models); `quota_probe` is how often `job.quotaprobe` checks
  its credentials (at least `1m`), `off` or `0`; empty uses the job's
  `interval`.
- **Which models a caller may use**: the group's `models` (names or globs),
  narrowed by the key's own `models`.

## Layered plugin settings

Plugins declare the settings they read (`plugin.Setting` in
`pkg/plugin/settings.go`): key, type, default, the layers it may be set on,
and how layers merge. The core merges per request; a plugin reads the merged
value with `Call.Setting(key)`.

- **Layers**: Global → Group → User → Key (a key belongs to at most one user
  and one group).
- **`override`**: the nearest layer that sets a value wins.
- **`strictest`**: a lower layer can only tighten. Numbers: the smaller
  positive value wins, `0` means unlimited. Booleans: `false` wins. Lists
  (allowlists, where empty means no restriction): every layer that sets a
  non-empty list must allow the entry, so globs narrow per layer, an empty
  layer lifts nothing, and layers sharing no entry allow nothing. An
  object's settings (`GET /api/v1/groups/{id}/settings`, likewise users and
  keys) list each layer's list under such a field and name the layer that
  leaves nothing allowed; external plugins get the
  layers in `setting_layers` ([protocol](../sdk/PROTOCOL.md)). An admin
  may exempt one key from a strictest setting.
- **`union`** (lists only): every layer's entries add up, so a lower layer
  can grow a deny list but never shrink it; an exempted key keeps only its
  own list.
- **Invalid values**: the console, the admin API and `plugrelay apply`
  refuse a value that does not fit its setting, so a stored one comes from
  a hand-edited database or a plugin whose declaration changed. Such a layer of a strictest list allows nothing; of
  a union list (a deny list) it keeps its valid string entries (a lone
  string counts as one); of any other setting it is ignored, as if unset,
  so an invalid strictest number falls back to the other layers or the
  default (`settings.LayerValue`). Each invalid row is logged as a warning
  naming the setting, layer and owner when the configuration loads.
- **Types**: `string int float bool duration list secret model upstream json`.
  `secret` values are encrypted under the master key and never echoed back;
  `model` and `upstream` render as pickers in the console. A money setting
  (`unit: money`) is stored in micro-units and shown and taken by the admin
  API in currency units.
- **Restart**: settings marked `restart` take effect only after a restart;
  the console labels them.
- **Record-owned layers**: a key's budgets (`meter.quota`
  `key_monthly_micro`, `key_total_micro`), `rpm` and concurrency
  (`middleware.ratelimit`) come from the key record's own fields, and a
  group's `body_logging` (`sink.requestlog`) from the group's `log_bodies`;
  neither the admin API nor `plugrelay apply` stores those layers
  (`settings.RecordOwned`). The merge rules still apply on top.
- **Where each layer is set**: the global layer in the feature's (or
  extension's) form, `PATCH /api/v1/features/{id}`; a group's, user's or
  key's layer on that object, `PUT /api/v1/{groups,users,keys}/{id}/settings`.

The merge and validation code is `internal/settings`.

## Business data as YAML

`plugrelay export` writes business data; `plugrelay apply` upserts it in one
transaction. Rows a document does not mention are left alone. Users, keys,
request logs and usage are never part of it.

```bash
plugrelay export -o relay-data.yaml                 # credentials' secrets left out
plugrelay export -o full.yaml --with-secrets        # as sensitive as the keys themselves
plugrelay apply -f relay-data.yaml --dry-run --json # show creates/updates without writing
cat team.yaml | plugrelay apply -f -
```

Document shape (`store.Document` in `internal/store/yamlio.go`):

```yaml
groups:
  - {id: vip, name: VIP, settings: {}, price_multiplier: 1.5}
upstreams:
  - id: deepseek
    provider: provider.openai-compatible   # provider.<interpreter> of the manifest
    manifest: deepseek           # official or stored manifest id
    config: {}                   # manifest variables, issuer override
    enabled: true                # default true
credentials:
  - id: ds-team-1
    upstream: deepseek
    kind: shared                 # shared | personal | service
    owner: ""                    # user id, required for personal
    secret: sk-...               # plaintext; omit to keep the stored secret
    proxy: ""                    # fixed egress; "" = direct
    max_concurrency: 8
models:
  - name: deepseek-chat
    routes:                      # omit to keep stored routes; [] removes them
      - {upstream: deepseek, upstream_model: deepseek-chat, priority: 10, weight: 1, groups: []}
presets:
  - {name: auto-coding, models: [claude-sonnet-4-5, deepseek-chat], strategy: in_order}  # in_order | cheapest | fastest
prices:                          # micro-units per million tokens; units: per unit
  - {model: deepseek-chat, input: 270000, cache_read: 70000, output: 1100000, currency: USD}
  - {model: gpt-image-1, units: {image: 40000, "image:1024x1024:high": 167000}, currency: USD}
  - {model: gpt-5.4-mini, input: 750000, output: 4500000, batch_discount: 0.5, currency: USD}
features:
  - {id: billing, enabled: true}           # ids of internal/features
plugins:                                   # extensions only
  - {id: ip-allowlist, enabled: true, order: 1010}  # examples/plugins/ip-allowlist-go
settings:
  - {scope: group, scope_id: vip, plugin: middleware.ratelimit, key: rpm, value: 600}
manifests:
  - {id: my-upstream, yaml: "id: my-upstream\nkind: provider\n..."}
```

Secret settings stay sealed under the master key in exports, so such a
document only applies to a store with the same master key.

### Prices

All prices are in the site currency, in micro-units per million tokens.
Media operations add unit prices in `units`, in micro-units per unit:

| key | per | metered from |
|-|-|-|
| `image` | image generated | images in the response; an event-stream response is billed for the `n` asked for (default 1) |
| `image:<size>`, `image:<size>:<quality>` | image of that size (and quality); the most specific key wins | `size` / `quality` of the response, else the request |
| `audio_second` | second of input audio | transcript `usage.seconds` or `duration`; when the transcript has neither and no token usage (text, SRT, VTT, a bare JSON `text`, an event stream), the uploaded file's length: read from a WAV, MP3, FLAC, Ogg, MP4/M4A or WebM header but never less than its bytes at 1536 kbit/s (the header is the client's; 48 kHz 16-bit stereo PCM is the densest common format), else its bytes at 64 kbit/s |
| `1k_chars` | 1000 input characters | speech `input` |
| `search_unit` | rerank search unit | Cohere's `meta.billed_units.search_units`; other rerank upstreams bill tokens (input rate) |

Usage that stands in for an unreadable response is marked estimated in the
request log. Token rates apply on top when the upstream reports tokens (image and
transcription models that bill tokens, embeddings input). Manifests carry
token list prices only, so unit prices come from price rules.

`batch_discount` (0 to below 1) is the fraction taken off usage served
through a [batch API](clients.md#batches): 0.5 bills batches at half the
rule's rates. It applies to the rule that prices the batch — the model's
rule for the charge, the upstream model's for the cost — and defaults to 0
(full price), also for manifest list prices; OpenAI and Anthropic bill
batches at half price, so set 0.5 on their rules.

- **Cost** (operator spend) of an attempt: a rule for its upstream and
  upstream model, then for the upstream, then a model rule named like the
  upstream model, then the list price in the upstream's manifest (`pricing:`,
  used only when its currency is the site currency).
- **Charge** (what the caller pays): the rule of the model that served the
  request (for a preset, the member's, else the preset's), else the
  delivered attempt's cost rate, times the caller's group `price_multiplier`
  (1 without a group). Each rate is multiplied and rounded half up to a
  micro-unit, and the request total is rounded half up. Callers see only the
  resulting prices; the request log shows tokens × unit price per bucket, and
  admins also see the multiplier. Spend budgets (`meter.quota`) count charges.

#### Price sync

While the `price_sync` feature is on, `job.pricesync` downloads the
[models.dev](https://models.dev) catalog (`url`, every `interval`, through the
core egress) and finds each model there: under a route's own provider by the
upstream model name, else under the model makers' entries by the upstream
names and the model's name, with a vendor or region prefix taken off
(`plugins/job/pricesync`). Dollar prices are converted with `usd_rate` when
the site currency is not USD. What it finds is stored per model
(`store.PriceSync`) and shown on the prices tab as suggestions:

- nothing changes until an operator accepts one (the model gets its own rule,
  source "price sync"); a rejected price is not suggested again, a different
  one is;
- a model set to follow models.dev takes new prices by itself, but only while
  its own rule is missing or came from sync: a price typed in by hand always
  wins (`TestPriceSync`);
- the catalog's context window and capabilities fill what a model's own
  information lacks on the catalog pages.
