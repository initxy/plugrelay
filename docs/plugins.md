# Plugins

English | [简体中文](plugins.zh-CN.md)

Upstreams, auth, rate limits, budgets, candidate ordering and logging are all
plugins on fixed extension points, written in one of three ways that the core
treats alike. Operators switch features, not single built-in plugins
([configuration](configuration.md#features)); a plugin outside any feature is
an extension. Where plugins run: [architecture](architecture.md#plugin-system-features-modules-extensions).
Terms: [`CONTEXT.md`](../CONTEXT.md).

- [Choosing a tier](#choosing-a-tier)
- [Declarative upstream manifests](#declarative-upstream-manifests)
- [External process plugins](#external-process-plugins)
- [Built-in Go plugins](#built-in-go-plugins)
- [Guardrails](#guardrails)
- [Call order and failure policy](#call-order-and-failure-policy)
- [Conformance suite: plugintest](#conformance-suite-plugintest)

## Choosing a tier

| you want to… | write | |
|-|-|-|
| add an upstream that speaks OpenAI chat/responses, Anthropic or Gemini, with its own URL, auth, quirks and error codes | a **YAML manifest** | no code; checked by JSON Schema |
| add a private-protocol upstream, a call to an outside service, an IP allowlist, a meter, a job, admin routes — in any language | an **external process plugin** | own process, own crash domain; Go SDK or the wire protocol |
| touch every streamed event, add a protocol codec, or run with the least overhead | a **built-in Go plugin** | compiled in; the only tier with `OnEvent`, `OnResponse` and codecs |

Extension points (`pkg/plugin/points.go`):

| point | does | manifest | external | built-in |
|-|-|-|-|-|
| `provider` | an upstream: endpoints, owned headers, `Prepare`, `Classify` → verdict; optionally model list, credential refresh, quota probe, its own `Send` | yes | yes | yes |
| `auth` | identify the caller | | yes | yes |
| `middleware` | `OnRequest`, `OnAttempt`, `OnFinish`, `MutateBody`, `OnEvent`, `OnResponse` | | `OnRequest`, `OnAttempt` (headers only), `OnFinish`; no body edits | yes |
| `selector` | reorder candidates, never add; an enabled one replaces the built-in routing strategy | | yes | yes |
| `meter` | `Reserve` / `Settle` / `Refund`, all idempotent | | yes | yes |
| `sink` | consume finished request records, asynchronously | | yes | yes |
| `job` | run on a schedule | | yes | yes |
| `admin` | HTTP routes under `/api/plugins/<id>/` | | yes | yes |
| any kind, optional | `UserDeleteHook` ends what a user has running before the user is deleted; `SettingsCheck` refuses a settings change (409, from the console, `plugrelay apply` or new-api import) and refuses to switch its plugin on over stored settings it would reject | | | yes |

Protocol codecs are not plugins: they implement `pkg/protocol.Codec` and are
compiled in.

## Declarative upstream manifests

A manifest describes one upstream; a generic provider runs it. Official ones
are in [`manifests/`](../manifests) (`plugrelay manifest list`), each citing
the vendor docs it was checked against. Schema:
[`manifests/schema.json`](../manifests/schema.json); field meanings:
`plugins/provider/manifest/manifest.go`.

```yaml
id: acme                      # unique; also the default issuer group
kind: provider                # the only kind
name: ACME AI
name_zh: ACME 智能           # optional console name in Chinese
category: vendor              # gallery group: vendor | cloud | inference | gateway | self_hosted | custom
homepage: https://docs.acme.example
interpreter: openai-compatible   # default for endpoints: openai-compatible,
                                 # anthropic-compatible, gemini, openai, anthropic
variables:                    # per-instance values: {region} below
  region: {default: us, description: Deployment region, description_zh: 部署区域}
auth:
  header: Authorization       # where the credential goes
  scheme: Bearer              # Bearer | raw; or query_param: key
  key_optional: false         # true: the server may run without a key (keyless below)
owned_headers: [X-Acme-Project]  # client headers of these names are dropped
credential_headers:           # header ← credential meta value
  X-Acme-Org: org
capabilities:                 # shared defaults, merged over each codec's own
  cache: keyed                # explicit | keyed | implicit | none
  max_tokens_field: max_completion_tokens
  unsupported_params: [service_tier]
endpoints:
  - id: chat                  # defaults to the protocol name
    protocol: openai-chat     # anthropic | openai-chat | openai-responses | openai-completions | gemini
    base_url: https://{region}.api.acme.example/v1   # OpenAI style includes /v1
    capabilities:
      reasoning: reasoning_content   # reasoning text field, sent back on tool turns
  - id: anthropic
    protocol: anthropic
    base_url: https://{region}.api.acme.example/anthropic  # Anthropic/Gemini: no /v1
    interpreter: anthropic-compatible
    unsupported_betas: [context-1m-2025-08-07]       # removed from anthropic-beta
    issuer: acme              # endpoints with the same issuer share private state
  - id: completions           # legacy /completions for editor autocomplete
    protocol: openai-completions
    base_url: https://{region}.api.acme.example/v1
    capabilities:
      fill_in_middle: true    # accepts suffix (off unless declared)
errors:                       # checked in order, before the built-in rules
  - when: {status: 402}
    verdict: {cause: quota, credential: cool_until_reset, retry: next, signal: quota_exhausted}
  - when: {status: 400, message_contains: "maximum context length"}
    verdict: {cause: request, signal: context_too_long}
models:
  endpoint: chat
  list: /models               # relative to the endpoint's base URL; or static: [...]
pricing:                      # per million tokens; or a file path next to the manifest
  currency: USD
  models:
    acme-large: {input: 2.0, cache_read: 0.2, output: 8.0}
quota:                        # optional probe, run by job.quotaprobe
  path: /dashboard/billing
  remaining_fraction: data.remaining   # or remaining + limit (amounts)
  reset: data.reset_at                 # unix seconds or RFC 3339
  reset_period: data.period            # used when reset is empty: daily | weekly | monthly (00:00 UTC)
```

| topic | rule |
|-|-|
| placeholders | `{name}` comes from the upstream's config, else the variable's default; `{model}` in `path` is the upstream model name (Azure deployments, `manifests/azure-openai.yaml`) |
| self-hosted address | `base_url` includes `/v1`; `server_url` is the server root, for servers with routes outside `/v1` (Anthropic, rerank). The console shows `variables:` as form fields |
| verdicts | causes `request`, `transient`, `rate_limit`, `quota`, `auth_expired`, `banned`, `content`, `unknown`; credential `keep`, `cooldown`, `cool_until_reset`, `refresh`, `quarantine`, `manual`; retry `none`, `next`, `downgrade` (with `downgrade:` edits); signals are `ir.ErrorKind` (`pkg/ir/error.go`). Rules that break the hard rules (retrying a content rejection, touching the credential on a request error) fail validation |
| `freeform_tools` | OpenAI custom tools (Codex's `apply_patch`) are sent as function tools unless the manifest declares `freeform_tools: true`; only `openai` and `azure-openai` do |
| private addresses | blocked by the SSRF guard; self-hosted endpoints need `egress.allow_private_cidrs` ([configuration](configuration.md#egress)) |
| issuer groups | default to the manifest id; an endpoint whose `base_url` comes from a variable gets its own group per upstream, so two self-hosted instances never share private state |

```bash
plugrelay manifest validate acme.yaml        # or a directory
plugrelay manifest list                      # official manifests in the binary
plugrelay upstream add --manifest deepseek --credential-env DEEPSEEK_API_KEY
plugrelay upstream add --manifest azure-openai --id azure-eu --credential-env AZURE_KEY --var resource=acme-eu
plugrelay upstream add --manifest vllm --var base_url=http://10.0.0.5:8000/v1   # no key
```

`upstream add` takes official manifests and ones already in the database.
Store your own with `plugrelay apply` (`manifests:` entries, YAML inline); the
console and `GET /api/v1/manifests` list manifests but cannot upload one.

**Keyless upstreams.** With `auth.key_optional: true` (self-hosted servers,
the LiteLLM and Bifrost gateways, the custom `openai-compatible`) and no
credentials, an upstream is routed as one shared credential with id `""` and
no auth header; leave out `--credential-env` or the console key to get one.
Any other upstream without credentials is not routed ("not routed: needs a
key"), never sent unauthenticated. A credential that cannot be decrypted is
listed as disabled ("cannot be opened") and never makes its upstream keyless.

### Cloud platforms: Bedrock and Vertex AI

Their auth is not a header, so their manifests use Go interpreters, `bedrock`
and `vertex` (`plugins/provider/<name>`; package docs list the wire
differences). Otherwise they are ordinary manifests:

| manifest | variables | credential secret | issuer |
|-|-|-|-|
| `bedrock` | `region` (default `us-east-1`) | `ACCESS_KEY_ID:SECRET_ACCESS_KEY[:SESSION_TOKEN]`, the same as JSON, or a Bedrock API key | `bedrock` |
| `vertex-anthropic` | `project` (default: the key's `project_id`), `location` (default `global`) | service account key JSON | `vertex-anthropic` |
| `vertex-gemini` | as above | service account key JSON | `vertex-gemini` |

```bash
plugrelay upstream add --manifest bedrock --credential-env BEDROCK_KEYS --var region=us-west-2
export VERTEX_KEY="$(cat service-account.json)"
plugrelay upstream add --manifest vertex-anthropic --credential-env VERTEX_KEY --var location=us-east5
```

No model list or prices: set each route's upstream model name to the platform
id (`us.anthropic.claude-…` profiles or ARNs on Bedrock, `claude-…@YYYYMMDD` on
Vertex). Separate issuers, because Anthropic API, Bedrock and Vertex reject
each other's thinking signatures (Gemini API and Vertex Gemini are kept apart
too). Bedrock counts tokens locally, offers no Anthropic server tools and
sends calls itself (`plugin.Sender`) through `Outgoing.Client`. Vertex caches
access tokens per credential, renews them five minutes before expiry and drops
one on a 401 so the core refreshes the credential; the token exchange uses the
credential's egress.

## External process plugins

An external plugin is a directory with a `plugin.yaml` and an executable.
plugrelay starts it, talks JSON-RPC 2.0 over stdio (LSP-style framing) or a
unix socket, restarts it with backoff after a crash, and applies its
`fail_policy` when it fails or times out. The core sees it as `<kind>.<id>`.

| | |
|-|-|
| contract | wire protocol and full `plugin.yaml` reference in [`sdk/PROTOCOL.md`](../sdk/PROTOCOL.md); `sdk/plugin.schema.json` validates `plugin.yaml` in editors |
| enabling | `external_plugins.enabled: true` and `external_plugins.dir` ([configuration](configuration.md#external_plugins)). Each plugin starts disabled; an admin approves its host APIs, turns it on and orders it in the same chain as built-in plugins (`PATCH /api/v1/extensions/{id}`, the console's Extensions list, or `plugins:` in `plugrelay apply`) |
| trust | it runs as plugrelay's OS user with its own network and file access. Only `permissions.host` (KV, HTTP through the core egress) is enforced; other `permissions` are just shown to the admin |

### SDK quickstarts

Go: `github.com/initxy/plugrelay/sdk/go/pluginsdk` ([README](../sdk/go/README.md),
with a full example). Other languages implement [`sdk/PROTOCOL.md`](../sdk/PROTOCOL.md).
Runnable [`examples/plugins`](../examples/plugins):
[`echo-provider-go`](../examples/plugins/echo-provider-go) streams back the
last user message; [`ip-allowlist-go`](../examples/plugins/ip-allowlist-go)
is a `middleware.onRequest` CIDR allowlist.

| things that bite | |
|-|-|
| stdout | in stdio mode anything on stdout but protocol frames breaks the connection; log to stderr |
| errors | `deny(...)` / `reject(...)` reach the client as is; any other error is a plugin failure and `fail_policy` applies |
| limits | no `middleware.onEvent`; middleware cannot change the request body |
| environment | minimal (`PATH`, `HOME`, locale, `TMPDIR`, `TZ`, the manifest's `env`, `PLUGRELAY_PLUGIN_ID`, …), not the relay's |
| `plugin.yaml` | `reports_off_box` and `requires` work like `ReportsOffBox` and `Requires` [below](#built-in-go-plugins) |
| `plugrelay apply` | does not load external plugins, so it cannot check their settings |

## Built-in Go plugins

Built-in plugins live in `plugins/<kind>/<name>`, import only `pkg/...` and
`plugins/internal/...` (`plugins/boundary_test.go`), and get core services
from the `plugin.Host` passed to `Init`.

```go
package quiet

import "github.com/initxy/plugrelay/pkg/plugin"

const ID = "middleware.quiet"

func init() { plugin.Register(ID, func() plugin.Plugin { return &Quiet{} }) }

type Quiet struct{}

func (*Quiet) Info() plugin.Info {
	return plugin.Info{
		ID:   ID,
		Kind: plugin.KindMiddleware,
		Settings: []plugin.Setting{
			{Key: "rpm", Type: plugin.TypeInt, Default: 0,
				Scopes: plugin.Global | plugin.Group | plugin.Key, Merge: plugin.Strictest,
				Label: plugin.T("Requests per minute", "每分钟请求数"),
				Help:  plugin.T("0 means no limit.", "0 表示不限。")},
		},
		// Mutates: []string{"max_tokens"}, // gjson paths this plugin may edit
		FailPolicy: plugin.FailClosed,
	}
}

func (*Quiet) OnRequest(c *plugin.Call) (*plugin.Reply, error) {
	_ = c.Setting("rpm").Int() // already merged Global → Group → User → Key
	return nil, nil            // nil reply: carry on
}
```

**Registering** makes a plugin available, not running. Import it in
`plugins/builtin/builtin.go` and place it in `internal/features` as a core
module (always runs) or a feature's module (`TestEveryBuiltinPlaced` checks).
A third-party Go plugin needs a custom build and is an extension.

| `plugin.Info` field | meaning |
|-|-|
| `ID` | `<kind>.<name>` |
| `Settings` | what the core merges ([layered settings](configuration.md#layered-plugin-settings)) and forms show: `Label`/`Help` in English and Chinese, `EnumLabels` for enums, `Unit: plugin.UnitMoney` for micro-units (the admin API uses currency units). `TestSettingsLabelled` checks every built-in setting. A feature picks which settings its form shows |
| `Mutates` | request fields it may edit; none = read-only, which keeps passthrough byte-exact |
| `FailPolicy` | `closed` or `open`; per-kind defaults in [architecture](architecture.md#plugin-system-features-modules-extensions) |
| `ReportsOffBox` | sends data off the box; never started under `PLUGRELAY_TEST_MODE` |
| `Requires` | plugins it cannot work without, turned on with it (`meter.quota` sums budgets from `sink.requestlog`). If one is missing, a fail-closed dependent stops startup or the reload; any other is turned off and the conflict logged |

| interfaces | |
|-|-|
| middleware | `plugin.RequestHook`, `AttemptHook`, `EventHook` (+ `EventHolder`), `ResponseHook` (edit or refuse a complete response before it is sent), `FinishHook`, `BodyMutator` |
| other points | `Authenticator`, `Selector`, `Meter` (+ `QuotaHeaders`, `DeferredMeter` for batches), `Sink`, `Job`, `Admin` |
| providers | `Provider` (+ `ModelLister`, `Refresher`, `QuotaProber`, `Sender`) |
| lifecycle | `Initializer`, `Closer`; a Host implementing `plugin.NoticeHost` takes `Notify(plugin.Notice)` for [notifications](notifications.md) and never blocks |

**Host and egress.** The Host gives a scoped logger, a plugin-scoped `KV`,
global settings, a usage reader, the version and egress clients;
`plugin.StateHost.State()` adds counters, slots and locks shared by all nodes,
namespaced per plugin. A plugin never opens its own connections
(`plugins/boundary_test.go`); it uses one of these, all with the SSRF guard and
no redirects:

| client | for | egress |
|-|-|-|
| `Outgoing.Client` | a provider's own calls during an attempt (token exchange in `Prepare`, a `Sender`'s request) | the credential's fixed proxy, the attempt's timers |
| `Host.ClientFor(upstream, credential)` | upstream calls outside a request (`ListModels`, `Quota`, `Refresh`) | the credential's proxy, else the upstream's (from relay config, not the struct passed in); the upstream's timeouts |
| `Host.HTTPClient` | traffic of no upstream; never for providers | direct |

A configured proxy that is down fails the call, with no fallback to direct
(hard rule 15). External plugins get the same from `host.http` with
`upstream_id` / `credential_id`.

| topic | what to know |
|-|-|
| ledger sinks | a sink with `FailPolicy: closed` has its own queue, so a slow one delays only itself. On a full queue the core waits, then writes from the finishing request; failed writes are retried with backoff, so skip a `Record.ID` already stored. Records undelivered at the shutdown deadline are logged as lost. Other sinks may drop records under load |
| credential refresh | on a `refresh` verdict the core locks the credential cluster-wide, calls `Refresher`, stores the new secret and expiry, returns the credential to rotation and resends the attempt once. Without a `Refresher`, or if it fails, the credential sits out a minute. If the store keeps refusing the new secret, the request still uses it, the console flags the credential (still in rotation), `plugrelay_credential_secret_save_failures_total` counts it and the save is retried every minute; until it lands, restarting that node loses it (`internal/pipeline/refresh.go`) |
| quota probe | `job.quotaprobe` (always on) calls `QuotaProber.Quota` for each credential of every enabled upstream, every upstream `quota_probe` or else the job's `interval` (10m), one node at a time, through `Host.ClientFor`. The result fills `Candidate.QuotaReset` for the `quota_reset` strategy and the credential's quota bar; `remaining` of 0 keeps the credential out until `reset_at`. Return `plugin.ErrNoQuota` if you cannot probe |
| editing requests | implement `MutateBody` (patch tier, sjson on declared paths) and edit the IR from `Call.Request()` in `OnAttempt` (convert tier); see `plugins/middleware/parampolicy` |
| notes | `Call.Note(text)` adds a line to the diagnostic of the attempt that answered (else the last); `OnFinish` is too late |
| metrics | a built-in `prometheus.Collector` is served on `/metrics` while `sink.metrics` runs |
| `Init` failures | the plugin is skipped and shown as `crashed`, with every plugin that requires it, fail-closed ones included (a skipped `meter.quota` enforces no budget). Check plugin status after deploys |
| auth and `/native` | `/native` strips only the headers the core knows carry the platform key (`Authorization`, `x-api-key`, `x-goog-api-key`, `?key=`). An auth plugin reading the key from elsewhere must not be combined with `/native` grants, or the key reaches the upstream |

## Guardrails

`middleware.guardrails` (`plugins/middleware/guardrails`, see its package doc)
runs with `middleware.redact` while the content-safety feature is on (off by
default). It checks requests and responses and applies one action to all
content rules.

| setting | merge | meaning |
|-|-|-|
| `detect` | union | `email`, `phone`, `credit_card` (Luhn), `iban` (mod 97), `cn_id` (18-digit resident ID, check digit and birth date), `us_ssn` (issued ranges only), `secret` (prefixed API keys, AWS ids, GitHub/Slack/Google/GitLab tokens, bearer tokens, JWTs), `private_key` (PEM) |
| `deny_keywords`, `deny_patterns` | union | substrings, ignoring case, zero-width characters and full-width ASCII (no other look-alikes); RE2 expressions as written (an invalid one fails the request closed) |
| `action` | override | `block` (default), `redact` or `flag` |
| `check_response` | override | also check responses (default on) |
| `max_prompt_chars` | strictest | characters of system, messages, tool calls and results; `0` = unlimited |
| `blocked_models`, `blocked_tools` | union | names or globs (`pkg/match`); a tool is refused when offered or called in the history |
| `stream_window` | override, global | bytes of streamed text kept to match across chunks (2048, max 16384) |
| `block_unscannable_audio` | override | with `block` and any rule or moderation on, refuse WebSocket audio input, which rules cannot read (default off) |
| `moderation_upstream`, `moderation_model`, `moderation_fail` | override, global/group | call an upstream's OpenAI-compatible `/moderations` (appended to its first endpoint's base URL, with one of its credentials, through the core egress); model default `omni-moderation-latest`; if the call fails, `closed` (default) refuses, `open` lets it through with a note |
| `max_moderated_events` | strictest, global/group | Realtime client events per session whose user text is moderated (200); past that, moderation counts as failed |

| topic | behaviour |
|-|-|
| what is read | requests: all client text — system prompts, each turn's text and thinking, tool call arguments and results (not images or files). Responses: model text (not tool input or thinking). Moderation sees user turns only. Detectors match only when a checksum or structure holds (`plugins/internal/textscan`, shared with `middleware.redact`) |
| not covered | `/native` runs no request hooks, so keep it off for keys guardrails must hold. Batches are checked per request ([clients](clients.md#batches)); WebSocket sessions per event, text only ([clients](clients.md#realtime-and-websocket)) |
| `block` | the protocol's content-filter error (400 `content_filter` on OpenAI, `invalid_request_error` on Anthropic); 403 for blocked models and tools, 400 for an oversized prompt. A refused complete response is not charged; its upstream cost is still recorded |
| `redact` | matches become `[REDACTED:<rule>]` in the request's user-turn text (patch and convert tiers) and in complete responses (re-encoded, even in passthrough). Elsewhere in the request, and in streams, it acts as `flag`. A moderation hit has no span, so it is blocked |
| streams | matched in a sliding window of `stream_window` bytes. On `block`, text that could still be part of a match is held and the stream ends with the protocol's error event once a match is certain, so no part of it reaches the client. Keywords alone hold only a tail that could start one; any pattern or detector delays output by up to `stream_window` bytes. Covered: matches no longer than the window inside one content block (a block's end releases what is held). On `flag` nothing is held |
| verdicts | an attempt note `guardrails: <action> <phase> <rules>` (phase `request`, `response`, `stream`) and `plugrelay_guardrail_verdicts_total{phase,action,rule}`. A request refused before any attempt names the rules in its log entry's error |

## Call order and failure policy

Hook order, `Reply` short-circuits and `FailPolicy` defaults are the same for
every tier: see [architecture](architecture.md#plugin-system-features-modules-extensions).
Also: `OnAttempt` and the provider's `Prepare` / `Send` / `Classify` run again
on every retry. `OnEvent` gets 50 ms per event and returns pass, drop or abort
(an `*ir.GwError` ends the stream with it); an `EventHolder` also names how many
of its latest events to hold back until released (dropped if the stream
aborts). Only malfunctions (crash, timeout, protocol error — `plugin.IsFailure`)
trigger `FailPolicy`; a deliberate `*ir.GwError` reaches the client as is.
`Settle` and `Refund` may be called again and must apply once.

## Conformance suite: plugintest

`pkg/plugin/plugintest` runs a `plugin.Provider` against a scripted upstream
and checks the verdicts and header handling the core relies on: normal, stream,
header ownership (a planted client secret never reaches the upstream), cut
stream, 429 with retry-after, 401, suspended account (quarantine, no retry),
content violation (no retry anywhere), uncertain (conservative), timeout before
send, slow first byte, oversized SSE event, error passthrough, 5xx.

```go
func TestConformance(t *testing.T) {
	plugintest.RunProvider(t, plugintest.ProviderSuite{
		Protocol: "openai-chat",
		New: func(baseURL string) (plugin.Provider, *plugin.Upstream, *plugin.Credential) {
			u := &plugin.Upstream{ID: "up", Endpoints: []plugin.Endpoint{{ID: "e", Protocol: "openai-chat", BaseURL: baseURL}}}
			return myprovider.New(), u, &plugin.Credential{ID: "c", Secret: "sk-test"}
		},
	})
}
```

| | |
|-|-|
| owned headers | a header `Prepare` sets must be in `OwnedHeaders`, unless `Prepare` fills it only when the client sent none (a protocol version default); the suite reruns `Prepare` with a client value to tell them apart |
| options | `UseSend: true` tests the provider's own `Send`; `Skip` and `Expect` adjust scenarios that really differ |
| in this repo | every endpoint of every `manifests/` file (`plugins/provider/compatible/conformance_test.go`); Bedrock through `Send` with event-stream fixtures and Vertex against a fake token endpoint (`plugins/provider/internal/cloudtest`); the echo provider through the host (`internal/extplugin/examples_test.go`). Non-Go plugins have no runner outside the repo |
