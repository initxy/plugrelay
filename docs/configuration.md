# Configuration

English | [简体中文](configuration.zh-CN.md)

Three kinds of configuration, each with one home:

| what | where | written by |
|-|-|-|
| how the process starts (listen address, master key, database, limits, …) | `relay.yaml` + `PLUGRELAY_*` env vars | you; plugrelay never writes it |
| business data: upstreams, credentials, models, presets, groups, prices, features, extensions, manifests, site settings | the store (SQLite, PostgreSQL or MySQL) | console, admin API, `plugrelay upstream/model/preset/key/apply` |
| module settings, layered Global → Group → User → Key | the store | feature forms, each object's settings, `plugrelay apply` |

Source of truth: `internal/config/config.go` (relay.yaml),
`internal/store/yamlio.go` (business data YAML), `pkg/plugin/settings.go`
(settings). Annotated sample: [`examples/relay.yaml`](../examples/relay.yaml).

- [relay.yaml reference](#relayyaml-reference)
- [Features](#features)
- [Where relative paths resolve](#where-relative-paths-resolve)
- [Routing, models and presets](#routing-models-and-presets)
- [Layered plugin settings](#layered-plugin-settings)
- [Business data as YAML](#business-data-as-yaml)

## relay.yaml reference

The file is `./relay.yaml` unless `--config FILE` or `PLUGRELAY_CONFIG` says
otherwise; a missing file means all defaults. Unknown keys are errors, and
every problem is reported at once with its path
(`trusted_proxies[2]: want a CIDR or an IP`). `plugrelay doctor` checks a file.

Durations take Go syntax (`90s`, `2m`) or a number of seconds.

### Top level

| field | default | env | meaning |
|-|-|-|-|
| `listen` | `127.0.0.1:8787` | `PLUGRELAY_LISTEN` | `host:port`. On loopback only, the console on this machine needs no login (site setting `security.local_owner_login`, [API auth](api.md)). |
| `trusted_proxies` | `[]` | `PLUGRELAY_TRUSTED_PROXIES` (comma-separated) | CIDRs or IPs of your reverse proxies. Only when the TCP peer is one of them is the client address read from `X-Forwarded-For` (right to left, skipping trusted hops), else `X-Real-IP`. Used by IP allowlists, per-IP limits, request logs, login throttling and the audit log. The local-owner check always uses the TCP peer. |
| `master_key` | empty | `PLUGRELAY_MASTER_KEY` | 32 random bytes, base64 (standard or URL). Encrypts credentials, secret settings and sealed private state. Set this or `master_key_file`, not both. |
| `master_key_file` | empty (`init` writes `master.key`) | `PLUGRELAY_MASTER_KEY_FILE` | file holding the master key. Setting either env var clears the other field. |
| `previous_master_keys` | `[]` | `PLUGRELAY_PREVIOUS_MASTER_KEYS` (comma-separated) | old master keys. They only open private state sealed before the change, not stored secrets ([rotation](deployment.md#master-key-rotation)). |
| `store.driver` | `sqlite` | `PLUGRELAY_STORE_DRIVER` | `sqlite` (single node), `postgres`, or `mysql` (MySQL 8.0.13+ / MariaDB 10.6+). Migrations run when the store opens. |
| `store.dsn` | `plugrelay.db` | `PLUGRELAY_STORE_DSN` | required. SQLite: a path or `file:` URI. PostgreSQL: `postgres://user:pass@host:5432/db?sslmode=…`. MySQL: `user:pass@tcp(host:3306)/db`; the database must exist, options like `tls=true` pass through, utf8mb4, `sql_mode` and READ COMMITTED are forced, `LOAD DATA LOCAL` is refused. |
| `store.max_conns` | `0` | `PLUGRELAY_STORE_MAX_CONNS` | connections per node; `0` = 8 (SQLite) or 20. Keep nodes × this under the server's `max_connections`. |
| `state.driver` | `memory` | `PLUGRELAY_STATE_DRIVER` | runtime state: `memory` (single node, lost on restart), `sql` (single node, kept in the store), `redis` (required with more than one node). Losing it costs cache hits and counters, never correctness. |
| `state.url` | empty | `PLUGRELAY_STATE_URL` | `redis://user:pass@host:6379/0`; required for `redis` |
| `log.level` | `info` | `PLUGRELAY_LOG_LEVEL` | `debug`, `info`, `warn`, `error` |
| `log.format` | `text` | `PLUGRELAY_LOG_FORMAT` | `text`, `json` |
| `limits.max_body_bytes` | `33554432` (32 MiB) | `PLUGRELAY_LIMITS_MAX_BODY_BYTES` | request body as sent, streamed uploads included |
| `limits.max_decompressed_bytes` | `134217728` (128 MiB) | `PLUGRELAY_LIMITS_MAX_DECOMPRESSED_BYTES` | after gzip/zstd; at least `max_body_bytes` |
| `shutdown.grace` | `30s` | `PLUGRELAY_SHUTDOWN_GRACE` | how long in-flight requests keep running after SIGINT/SIGTERM ([graceful shutdown](deployment.md#graceful-shutdown)) |
| `shutdown.drain` | `60s` | `PLUGRELAY_SHUTDOWN_DRAIN` | then how long to wait for quota settlement and queued records; `0` = default |
| `console.enabled` | on | `PLUGRELAY_CONSOLE_ENABLED` | serve the web console; the admin API under `/api/v1` stays on either way |

Without a master key, storing a secret fails (`store: no master key
configured`); losing it makes stored credentials unreadable
([backups](deployment.md#backups)). The public URL, single sign-on,
two-step sign-in, captcha and [MCP servers](mcp.md) live in the store, not
in relay.yaml.

### external_plugins

| field | default | env | meaning |
|-|-|-|-|
| `external_plugins.enabled` | `false` | `PLUGRELAY_EXTERNAL_PLUGINS_ENABLED` | off: no plugin process is ever started |
| `external_plugins.dir` | `plugins` | `PLUGRELAY_EXTERNAL_PLUGINS_DIR` | scanned for plugin directories with a `plugin.yaml` ([plugins](plugins.md#external-process-plugins)) |

### egress

| field | default | env | meaning |
|-|-|-|-|
| `egress.allow_private_cidrs` | `[]` | `PLUGRELAY_EGRESS_ALLOW_PRIVATE_CIDRS` (comma-separated) | private and loopback addresses are blocked (SSRF guard); list CIDRs here for self-hosted upstreams such as vLLM or Ollama |
| `egress.default_timeouts.first_byte` | `2m` | `PLUGRELAY_EGRESS_FIRST_BYTE_TIMEOUT` | until the first response byte |
| `egress.default_timeouts.idle` | `5m` | `PLUGRELAY_EGRESS_IDLE_TIMEOUT` | between two stream chunks |
| `egress.default_timeouts.total` | `30m` | `PLUGRELAY_EGRESS_TOTAL_TIMEOUT` | whole attempt; positive and not shorter than the other two |

Each upstream can override these (console: upstream → Advanced; or fields on
the upstream in the admin API):

- `timeouts.connect_ms / first_byte_ms / idle_ms / total_ms`, field by field;
  0 keeps the default. Connect covers dial, proxy and TLS, capped at 10 s.
- `proxy`: egress for its credentials that set none; a credential's own
  proxy wins. A dead proxy fails the attempt, never falls back to direct.
- `headers`: added to every request. Headers the provider owns are never
  overridden; credential, framing, hop-by-hop and client-address headers are
  rejected (`egress.SetExtraHeaders`).

### native

| field | default | env | meaning |
|-|-|-|-|
| `native.enabled` | `false` | `PLUGRELAY_NATIVE_ENABLED` | serve [`/native/<upstream>/<path>`](clients.md#native-passthrough); off answers 404 |
| `native.max_body_bytes` | `536870912` (512 MiB) | `PLUGRELAY_NATIVE_MAX_BODY_BYTES` | one request body (uploads stream, not buffered); `0` = default |

Turning it on grants nothing. Each key also needs the `middleware.access`
setting `native_upstreams` (upstream ids, `*` for all, empty = none). It
merges `override`, so one group or one key can be let in.

### websocket

Limits of [WebSocket sessions](clients.md#realtime-and-websocket)
(`/v1/realtime`, Responses WebSocket mode).

| field | default | env | meaning |
|-|-|-|-|
| `websocket.max_session` | `30m` | `PLUGRELAY_WEBSOCKET_MAX_SESSION` | session lifetime; at most `30m`, the lifetime of a concurrency slot |
| `websocket.idle_timeout` | `5m` | `PLUGRELAY_WEBSOCKET_IDLE_TIMEOUT` | closed after this long with no message either way |
| `websocket.session_ready_timeout` | `10s` | `PLUGRELAY_WEBSOCKET_SESSION_READY_TIMEOUT` | Realtime: how long client events may wait for the upstream's `session.created` when plugins need it; then `session_not_ready` and close `1013` |
| `websocket.max_message_bytes` | `16777216` (16 MiB) | `PLUGRELAY_WEBSOCKET_MAX_MESSAGE_BYTES` | one message either way; larger closes with `1009` |
| `websocket.response_reserve_tokens` | `4096` | `PLUGRELAY_WEBSOCKET_RESPONSE_RESERVE_TOKENS` | output tokens reserved for a response that sets no cap |

### Other environment variables

| variable | read by | meaning |
|-|-|-|
| `PLUGRELAY_CONFIG` | every command | path of relay.yaml (`--config`) |
| `PLUGRELAY_DATA_DIR` | every command | default `--data-dir`; `/data` in the Docker image |
| `PLUGRELAY_ADMIN_PASSWORD` | `init`, `user reset-password` | admin password, kept out of shell history |
| `PLUGRELAY_API_KEY` | `plugrelay setup`, Codex | platform key; `setup codex` points Codex at it |
| `PLUGRELAY_ADMIN_TOKEN` | `serve` | operator Bearer token for the admin API and `/metrics` |
| `PLUGRELAY_TEST_MODE` | the relay | any value drops every plugin that sends data off the box |
| `PLUGRELAY_TEST_PG`, `PLUGRELAY_TEST_MYSQL`, `PLUGRELAY_E2E_*` | tests | test databases and live upstreams for `make e2e`; each is documented at the top of its file in `test/e2e/` |
| `PLUGRELAY_COMPAT_UPSTREAM`, `_DATE`, `_EVENTS` | `scripts/compat.sh` | labels and events file of the compatibility report |

## Features

`internal/features` lists what can run; on/off state lives in the store and
applies on the next reload (`App.states` in `internal/app/plugins.go`).

- **Core modules** always run: `auth.apikey`, `middleware.access`,
  `selector.routing`, `sink.requestlog`, `job.requestlog-retention`,
  `job.modelsync`, `job.quotaprobe`, and each `provider.*` in use.
- **Features** below run their modules while on, pulling in what they
  require; a change that leaves an unrunnable setup is refused (409). Switch
  them in the console, `PATCH /api/v1/features/{id}`, or `features:` in `apply`.
- **Extensions** (external plugins, Go plugins in a custom build) start off,
  one switch each (`/api/v1/extensions`, `plugins:` in `apply`). A missing
  one fails with `plugin … is not installed` (see `plugrelay doctor`).

| id | default | modules | what it does |
|-|-|-|-|
| `limits` | on | `middleware.ratelimit`, `meter.quota`, `middleware.param-policy` | per-minute requests/tokens and concurrency per key, user or group; spend and token budgets (days start at midnight in `site.timezone`, default UTC); caps on output tokens and reasoning effort; stripped parameters |
| `routing` | always | (core) | the [routing strategy](#routing-models-and-presets) and model-sync / quota-probe intervals |
| `request_log` | always | (core) | keep request/response text (`body_logging`, off) and for how many days (`retention_days`, 0 = forever) |
| `content_safety` | off | `middleware.guardrails`, `middleware.redact` | deny lists, PII and secret detectors, prompt size, blocked models and tools, moderation ([plugins](plugins.md#guardrails)); masking text before it leaves |
| `billing` | off | `meter.balance`, `admin.payment`, `admin.redeem`, `admin.subscriptions`, `admin.import-newapi`, `admin.rewards`, `admin.announcements` | prepaid balance, Stripe and EasyPay, coupons, redeem codes, plans, check-in and referral rewards, announcements, new-api import ([sell kit](sell-kit.md)); the setup wizard's "Selling" answer turns it on |
| `signup` | off | `admin.signup` | public sign-up, optionally by invite code or email domain |
| `sso` | off | — | [single sign-on](deployment.md#single-sign-on); off, no provider is offered and passwords always work |
| `mcp` | off | — | the [MCP gateway](mcp.md); off, `/mcp` answers 404 |
| `notifications` | off | `job.notify`, `sink.webhook` | alerts by email, chat bots or webhook; request records to a webhook ([notifications](notifications.md)) |
| `price_sync` | off | `job.pricesync` | [price suggestions from models.dev](#price-sync) |
| `public_pages` | off | `admin.public-pages` | `/public/models` and `/public/status` without sign-in; prices of one group (`price_group`, empty = list price); upstream names only with `site.show_upstream_names`; cached a minute, limited per address (`per_ip_per_minute`) |
| `metrics` | on | `sink.metrics` | Prometheus metrics (`plugrelay_` prefix) at `/metrics`, behind `PLUGRELAY_ADMIN_TOKEN` when set |

## Where relative paths resolve

`master_key_file`, a plain SQLite `store.dsn` and `external_plugins.dir`
resolve against `--data-dir` (`PLUGRELAY_DATA_DIR`) if set, else the config
file's directory, never the working directory (`internal/cli/config.go`).
`plugrelay init` writes bare file names, so the directory stays portable.

## Routing, models and presets

- **Routes** belong to a model: upstream, upstream model name, priority,
  weight, allowed groups (`plugrelay model add --route`, the model's
  `routes`). Upstreams have no model map.
- **Strategy**: `selector.routing` setting `strategy`, global or per group.
  `priority_weight` (default; priority, then weighted split of new sessions),
  `cheapest` (price rule, else manifest list price), `fastest` (recent time
  to first byte), `quota_reset` (soonest reset first). Sticky candidates
  always stay first. An enabled external selector plugin ranks instead.
- **Presets**: a name, ordered models and a strategy (`in_order`,
  `cheapest`, `fastest`; `plugrelay preset add`, `/presets`). A preset shows
  in model lists to callers allowed one of its models. A request for it is
  served by an allowed member picked by the preset's strategy, falling
  through to the next on failure. A model asked for by name is never
  swapped. The request log shows both the preset and the serving model.
  Models and presets share one namespace.
- **Per-upstream jobs** (upstream fields): `model_sync` turns
  `job.modelsync` on or off (default on when the manifest lists models);
  `quota_probe` is the probe interval (at least `1m`), `off` or `0`; empty
  uses the job's `interval`.
- **Allowed models**: the group's `models` (names or globs), narrowed by the
  key's `models`.

## Layered plugin settings

Each plugin declares its settings (`plugin.Setting` in
`pkg/plugin/settings.go`): key, type, default, allowed layers, merge rule.
The core merges them per request (`internal/settings`); the plugin reads
`Call.Setting(key)`.

Layers run Global → Group → User → Key; a key has at most one user and one
group. Set the global layer in the feature's or extension's form
(`PATCH /api/v1/features/{id}`), the others on the object
(`PUT /api/v1/{groups,users,keys}/{id}/settings`).

| merge | rule |
|-|-|
| `override` | the nearest layer that sets a value wins |
| `strictest` | lower layers only tighten. Numbers: smallest positive wins, `0` = unlimited. Booleans: `false` wins. Allowlists (empty = no limit): an entry must pass every non-empty layer, so layers with nothing in common allow nothing. An admin can exempt a key. |
| `union` | lists add up: a lower layer can grow a deny list, never shrink it. An exempted key keeps only its own list. |

- `GET /api/v1/{groups,users,keys}/{id}/settings` shows each layer's
  allowlist and which layer blocks everything. External plugins get the
  layers in `setting_layers` ([protocol](../sdk/PROTOCOL.md)).
- **Invalid stored values** (hand-edited database, changed plugin; the
  console, API and `apply` refuse them on input): a strictest list allows
  nothing, a union list keeps its valid strings, anything else counts as
  unset (`settings.LayerValue`). Each is logged as a warning on load.
- **Types**: `string int float bool duration list secret model upstream json`.
  `secret` is encrypted under the master key and never echoed. `unit: money`
  is stored in micro-units, shown in currency units. Settings marked
  `restart` apply after a restart.
- **Record-owned layers**: a key's budgets (`key_monthly_micro`,
  `key_total_micro`), `rpm` and concurrency come from the key record; a
  group's `body_logging` from its `log_bodies`. The API and `apply` don't
  store those layers (`settings.RecordOwned`); merge rules still apply.

## Business data as YAML

`plugrelay export` writes business data; `plugrelay apply` upserts it in one
transaction and leaves unmentioned rows alone. Users, keys, request logs and
usage are never included.

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

Secret settings stay sealed in exports, so the document only applies to a
store with the same master key.

### Prices

Prices are in the site currency, in micro-units per million tokens. Media
adds per-unit prices in `units`:

| key | per | metered from |
|-|-|-|
| `image` | image generated | images in the response; a streamed response bills the `n` requested (default 1) |
| `image:<size>`, `image:<size>:<quality>` | image of that size/quality; most specific wins | `size` / `quality` of the response, else the request |
| `audio_second` | second of input audio | transcript `usage.seconds` or `duration`; without those or token usage, the upload's length from its WAV/MP3/FLAC/Ogg/MP4/WebM header, but never less than its size at 1536 kbit/s; with no readable header, its size at 64 kbit/s |
| `1k_chars` | 1000 input characters | speech `input` |
| `search_unit` | rerank search unit | Cohere's `meta.billed_units.search_units`; other rerank upstreams bill input tokens |

Token rates apply on top when the upstream reports tokens. Usage guessed for
an unreadable response is marked estimated. Manifests carry only token list
prices, so unit prices need price rules.

`batch_discount` (0 to below 1) is taken off [batch](clients.md#batches)
usage of the rule that prices it. Default 0, list prices included. OpenAI
and Anthropic bill batches at half price, so set 0.5 on their rules.

- **Cost** (your spend) of an attempt: first match of a rule for upstream +
  upstream model, for the upstream, a model rule named like the upstream
  model, then the manifest's `pricing:` (only in the site currency).
- **Charge** (what the caller pays): the serving model's rule (for a preset,
  the member's, else the preset's), else the attempt's cost rate, times the
  group's `price_multiplier` (1 without a group). Rates and totals round
  half up to a micro-unit. Callers see only final prices; admins also see
  the multiplier. Spend budgets count charges.

#### Price sync

With `price_sync` on, `job.pricesync` fetches the [models.dev](https://models.dev)
catalog (`url`, every `interval`, through the core egress), matches each model
by upstream model name under the route's provider, else under the model
maker with vendor or region prefixes removed, and converts dollars with
`usd_rate` (`plugins/job/pricesync`). Matches show as suggestions on the
prices tab. Nothing changes until an operator accepts one; a rejected price
is not suggested again. A model set to follow models.dev updates itself, but
only while its rule is missing or came from sync: a hand-typed price always
wins. Catalog context window and capabilities fill gaps on the catalog pages.
