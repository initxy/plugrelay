# Plugins

English | [简体中文](plugins.zh-CN.md)

Everything that is policy rather than mechanism — upstreams, authentication,
rate limits, budgets, ordering of candidates, logging — is a plugin. Plugins
attach to fixed extension points and are written in one of three ways; to the
core all three look the same. Operators do not switch built-in plugins one by
one: they switch features ([configuration](configuration.md#features)); a
plugin outside them is an extension. Terms: [`CONTEXT.md`](../CONTEXT.md). Where
plugins sit in a request: [architecture](architecture.md#plugin-system-features-modules-extensions).

- [Choosing a tier](#choosing-a-tier)
- [Declarative upstream manifests](#declarative-upstream-manifests)
- [External process plugins](#external-process-plugins)
- [Built-in Go plugins](#built-in-go-plugins)
- [Guardrails](#guardrails)
- [Call order and failure policy](#call-order-and-failure-policy)
- [Conformance suite: plugintest](#conformance-suite-plugintest)

## Choosing a tier

| you want to… | write | why |
|-|-|-|
| add an upstream that speaks a supported protocol (OpenAI chat or responses, Anthropic, Gemini) with its own URL, auth header, quirks and error codes | a **YAML manifest** | no code; validated by JSON Schema |
| add an upstream with a private protocol, a guardrail calling an external service, an IP allowlist, a custom meter, a job, admin routes — in any language | an **external process plugin** | runs out of process with its own crash domain; a Go SDK, and the protocol for any other language |
| touch every streamed event, add a protocol codec, or need the lowest overhead | a **built-in Go plugin** | compiled in; the only tier with `OnEvent` and codecs |

Extension points (`pkg/plugin/points.go`) and the tiers that can implement
them:

| point | does | manifest | external | built-in |
|-|-|-|-|-|
| `provider` | an upstream: endpoints, owned headers, `Prepare`, `Classify` → verdict; optional model list, credential refresh, quota probe, own `Send` | yes | yes | yes |
| `auth` | identify the caller | | yes | yes |
| `middleware` | `OnRequest`, `OnAttempt`, `OnFinish`, `MutateBody`; `OnEvent` and `OnResponse` built-in only | | yes (no `OnEvent`, `OnResponse`) | yes |
| `selector` | reorder candidates (never add); an enabled one ranks instead of the built-in routing strategy | | yes | yes |
| `meter` | `Reserve` / `Settle` / `Refund`, all idempotent | | yes | yes |
| `sink` | consume finished request records asynchronously | | yes | yes |
| `job` | run on a schedule | | yes | yes |
| `admin` | mount HTTP routes under `/api/plugins/<id>/` | | yes | yes |
| (optional, any kind) | `UserDeleteHook`: end what a user has running before the user is deleted; `SettingsCheck`: refuse a plugin settings change (409; console save, `plugrelay apply`, new-api import), and turning itself on over stored settings it would refuse; external plugins are not loaded by `plugrelay apply`, so theirs do not run there | | | yes |

Protocol codecs are not plugins of this API; they implement
`pkg/protocol.Codec` and are compiled in.

## Declarative upstream manifests

A manifest describes one upstream; a generic provider interprets it. Official
manifests live in [`manifests/`](../manifests) (the vendor list is in the
[README](../README.md); `plugrelay manifest list` prints them), each with the
vendor docs it was checked against in its header comment. Self-hosted ones
name their address variable one of two ways: `server_url` is the server root
without `/v1`, used when the server also has routes outside `/v1` (Anthropic,
rerank); `base_url` includes `/v1`. Each manifest's `variables:` says which,
with a description, and the console shows them as form fields. The schema is
[`manifests/schema.json`](../manifests/schema.json); the Go types and every
field's meaning are in `plugins/provider/manifest/manifest.go`.

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

Notes:

- `freeform_tools` (OpenAI custom tools such as Codex's `apply_patch`) is off
  for every manifest unless it declares `freeform_tools: true`, whatever the
  codec's default: OpenAI-compatible servers reject them, and the relay sends
  them as function tools instead. Only `openai` and `azure-openai` declare it.
- `{name}` placeholders are filled from the upstream instance's config or the
  variable's default; `{model}` in `path` is the upstream model name (Azure
  deployments). See `manifests/azure-openai.yaml`.
- Verdict causes: `request`, `transient`, `rate_limit`, `quota`,
  `auth_expired`, `banned`, `content`, `unknown`. Credential actions: `keep`,
  `cooldown`, `cool_until_reset`, `refresh`, `quarantine`, `manual`. Retry:
  `none`, `next`, `downgrade` (with `downgrade:` edits). Signals are
  `ir.ErrorKind` values (`pkg/ir/error.go`). Validation refuses rules that
  would break the hard rules — e.g. retrying a content rejection or changing
  the credential for a request error.
- Upstream URLs are subject to the SSRF guard; self-hosted endpoints need
  `egress.allow_private_cidrs` ([configuration](configuration.md#egress)).

Validate and use:

```bash
plugrelay manifest validate acme.yaml        # or a directory
plugrelay manifest list                      # official manifests in the binary
plugrelay upstream add --manifest deepseek --credential-env DEEPSEEK_API_KEY
plugrelay upstream add --manifest azure-openai --id azure-eu --credential-env AZURE_KEY --var resource=acme-eu
plugrelay upstream add --manifest vllm --var base_url=http://10.0.0.5:8000/v1   # no key
```

An upstream is keyless when its manifest sets `auth.key_optional: true`
(the self-hosted servers, the LiteLLM and Bifrost gateways, the custom
`openai-compatible`) and it has no credentials at all: the router routes it
as one shared credential with id `""` and sends no auth header. Leaving out
`--credential-env`, or the key in the console's add form for such a server,
gives one. Every other upstream with no credentials has no route: deleting
a cloud upstream's last key stops it, it never goes out unauthenticated,
and the console marks it "not routed: needs a key". A stored credential that cannot be
decrypted stays listed as disabled ("cannot be opened") and never makes its
upstream keyless.

`upstream add` takes official manifests and manifests already stored in the
database. A manifest file of your own is stored through `plugrelay apply`
(`manifests:` entries with the YAML inline); the console and the admin API
list manifests (`GET /api/v1/manifests`) but cannot upload one.

Issuer groups default to the manifest id; an endpoint whose `base_url` comes
from a variable gets its own issuer group per upstream, so two self-hosted
instances never share private state.

### Cloud platforms: Bedrock and Vertex AI

Their authentication cannot be written as a header, so their manifests name
interpreters backed by Go code: `bedrock` (`plugins/provider/bedrock`) and
`vertex` (`plugins/provider/vertex`). The package docs list the wire
differences each one handles. To the operator they are ordinary manifests:

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

- Point each model's route at the platform's id as the upstream model
  name (`us.anthropic.claude-…` inference profiles or ARNs on Bedrock,
  `claude-…@YYYYMMDD` on Vertex). Neither manifest lists models or prices.
- Anthropic API, Bedrock and Vertex never accept each other's thinking
  signatures, and the Gemini API and Vertex Gemini are kept apart too, so
  each has its own issuer group.
- Bedrock: token counting is answered locally; Anthropic server tools are
  not offered there. Calls are made by the provider itself (`plugin.Sender`)
  through the attempt's egress (`Outgoing.Client`), so the credential's or
  upstream's proxy carries them.
- Vertex: access tokens are cached per credential and replaced five minutes
  before they expire; a 401 drops the token and the core refreshes the
  credential. The token exchange takes the credential's egress too.

## External process plugins

An external plugin is a directory with a `plugin.yaml` and an executable.
plugrelay starts it, speaks JSON-RPC 2.0 over stdio (LSP-style framing) or a
unix socket, restarts it with backoff if it crashes, and applies its
`fail_policy` when it fails or times out. The core sees it as
`<kind>.<id>`.

- The contract — framing, lifecycle, every method, host calls, error codes,
  the full `plugin.yaml` reference — is [`sdk/PROTOCOL.md`](../sdk/PROTOCOL.md).
  `sdk/plugin.schema.json` validates `plugin.yaml` in editors.
- Turn the tier on with `external_plugins.enabled: true` and point
  `external_plugins.dir` at a directory of plugin directories
  ([configuration](configuration.md#external_plugins)).
- Installing a plugin means trusting it: it runs with plugrelay's OS user and
  its own network and file access. `permissions` in `plugin.yaml` are shown to
  the admin but not enforced, except `permissions.host`, which gates host APIs
  (KV, HTTP through the core egress).

External plugins are extensions: they start disabled; an admin approves the
host APIs they declare, turns them on and sets their order
(`PATCH /api/v1/extensions/{id}`, the console's Extensions list, or
`plugins:` in `plugrelay apply`), in the same chain as built-in plugins
(`internal/app/plugins.go`).

### SDK quickstarts

The Go SDK is `github.com/initxy/plugrelay/sdk/go/pluginsdk`
([README](../sdk/go/README.md), with a complete example). Other languages
implement [`sdk/PROTOCOL.md`](../sdk/PROTOCOL.md) directly. The
[`examples/plugins`](../examples/plugins) are runnable references:
[`echo-provider-go`](../examples/plugins/echo-provider-go), a provider that
streams back the last user message, and
[`ip-allowlist-go`](../examples/plugins/ip-allowlist-go), a
`middleware.onRequest` CIDR allowlist.

Rules that bite:

- In stdio mode, anything but protocol frames on stdout breaks the
  connection; log to stderr.
- A deliberate refusal is `deny(...)` / `reject(...)` and goes to the client
  as is; any other error is a plugin failure and the `fail_policy` applies.
- There is no `middleware.onEvent` across the process boundary.
- The process gets a minimal environment (`PATH`, `HOME`, locale, `TMPDIR`,
  `TZ`, the manifest's `env`, `PLUGRELAY_PLUGIN_ID`, …), not the relay's.

## Built-in Go plugins

Built-in plugins live in `plugins/<kind>/<name>` and are linked by
`plugins/builtin`. They import only `pkg/...` and `plugins/internal/...`
(`plugins/boundary_test.go`) and receive core services through the
`plugin.Host` passed to `Init`.

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

- **Register** from `init`; registering a plugin makes it available, not
  running. Add the import to `plugins/builtin/builtin.go` and place it in
  `internal/features`: a core module (always runs) or a module of a feature
  (runs while the feature is on); `TestEveryBuiltinPlaced` fails otherwise.
  A third-party Go plugin needs a custom build that imports it; it is an
  extension, switched on its own.
- **Info**: `ID` is `<kind>.<name>`. `Settings` declares what the core
  merges ([layered settings](configuration.md#layered-plugin-settings)) and
  what forms show: `Label` and `Help` in English and Chinese, `EnumLabels`
  for enum values, `Unit: plugin.UnitMoney` for micro-units (shown and taken
  in currency units by the admin API); `TestSettingsLabelled` requires them
  on every built-in setting. A feature lists which of its modules' settings
  its form shows. `Mutates` lists the request fields the plugin may edit; a
  plugin declaring none is read-only, which keeps passthrough byte-exact.
  `ReportsOffBox` marks a plugin that sends
  data off the box; `PLUGRELAY_TEST_MODE` never starts it. `Requires` lists
  plugins it cannot work without (`meter.quota` requires `sink.requestlog`,
  which its budgets are summed from): the core turns them on with it. If
  one is not installed, a dependent that fails closed (such as `meter.quota`) stops the relay from starting, or the
  reload from applying; any other dependent is turned off and the conflict
  logged. A sink with `FailPolicy: closed` is a ledger with a queue of its
  own, so a slow one delays nobody else: the core never drops its records
  when that queue is full (it waits, then writes the record from the
  finishing request itself) and retries failed writes with backoff, so the
  same `Record.ID` may arrive more than once and the sink must skip an id it
  already stored. Records still undelivered when the shutdown deadline
  passes are logged and counted as lost. Other sinks may lose records under
  overload. External plugins declare the same with
  `reports_off_box` and `requires` in `plugin.yaml`.
- **Authenticators and `/native`**: `/native` forwards the client's query
  and headers minus the ones the core knows carry the platform key
  (`Authorization`, `x-api-key`, `x-goog-api-key`, `?key=`). An external
  auth plugin that reads the key from anywhere else must not be combined
  with `/native` grants, or that key reaches the upstream.
- **Init failures**: a plugin whose `Init` fails is skipped and shown as
  `crashed` in the console, and so is every plugin that requires it; this
  holds for fail-closed plugins too (a skipped `meter.quota` enforces no
  budget), so watch plugin status after a deploy.
- **Shared state**: `plugin.StateHost.State()` gives counters, slots and
  locks shared by every node; each plugin's keys are namespaced, so one
  plugin cannot touch another's.
- **Interfaces**: implement any of `plugin.RequestHook`, `AttemptHook`,
  `EventHook` (+ `EventHolder`), `ResponseHook` (a complete response before it is sent; may
  edit it or refuse it), `FinishHook`, `BodyMutator` (middleware); `Authenticator`,
  `Selector`, `Meter` (+ `QuotaHeaders`), `Sink`, `Job`, `Admin`,
  `Provider` (+ `ModelLister`, `Refresher`, `QuotaProber`, `Sender`).
  `Initializer` and `Closer` for setup and teardown. A Host implementing
  `plugin.NoticeHost` takes `Notify(plugin.Notice)` for the notifications
  feature ([notifications](notifications.md)); it never blocks.
- **Host**: scoped logger, egress clients, plugin-scoped `KV`, global
  settings, usage reader, version. A plugin never opens its own
  connections (`plugins/boundary_test.go`); it takes one of three clients,
  all with the SSRF guard and no redirects:
  - `Outgoing.Client` — a provider's own calls during an attempt (a token
    exchange in `Prepare`, a `Sender`'s request): the credential's fixed
    proxy under the attempt's timers.
  - `Host.ClientFor(upstream, credential)` — calls for an upstream outside a
    request (`ListModels`, `Quota`, `Refresh`): the credential's proxy, else
    the upstream's, with the upstream's timeouts. The proxy comes from the
    relay's configuration, not from the credential passed in.
  - `Host.HTTPClient` — direct, for traffic that belongs to no upstream;
    providers never use it.

  A configured proxy that is down fails the call; nothing falls back to a
  direct connection (hard rule 15). External plugins get the same through
  `host.http` with `upstream_id` / `credential_id`
  ([`sdk/PROTOCOL.md`](../sdk/PROTOCOL.md)).
- **Credential refresh**: on a `refresh` verdict the core takes a
  cluster-wide lock for the credential, calls the provider's `Refresher`,
  stores the new secret and expiry, returns the credential to rotation and
  resends the attempt once with it. Without a `Refresher` (or when the
  refresh fails) the credential sits out for a minute. If the store refuses
  the new secret after a few retries, the request is still served with it,
  the credential is flagged for attention in the console (it stays in
  rotation), `plugrelay_credential_secret_save_failures_total` counts it and
  the save is retried every minute; until it lands, a restart of that node
  loses the secret (`internal/pipeline/refresh.go`).
- **Quota probe**: `job.quotaprobe` (always running) calls `QuotaProber.Quota`
  for every credential of an enabled upstream, once per its interval (the
  upstream's `quota_probe`, else the job's `interval`, 10m), on one node at a
  time, through `Host.ClientFor`. The reading is shared router state: it
  fills `Candidate.QuotaReset` for the `quota_reset` routing strategy, shows as a quota
  bar on the credential, and a `remaining` of 0 keeps the credential out until
  `reset_at`. Return `plugin.ErrNoQuota` for an upstream you cannot probe.
- **Request log notes**: `Call.Note(text)` adds a line to the diagnostic of
  the attempt that answered (else the last one); notes made in `OnFinish`
  come too late. A built-in plugin that is a `prometheus.Collector` has its
  series served on `/metrics` while `sink.metrics` runs.
- **Two-path edits**: a plugin that changes requests implements
  `MutateBody` (patch tier, sjson on declared paths only) and edits the IR
  from `Call.Request()` in `OnAttempt` (convert tier).
  `plugins/middleware/parampolicy` is the reference.

## Guardrails

`middleware.guardrails` (`plugins/middleware/guardrails`, package doc) runs
with `middleware.redact` while the content-safety feature is on (off by
default). It checks requests and responses and
applies one action to content rules.

| setting | merge | meaning |
|-|-|-|
| `detect` | union | detectors over request text and responses: `email`, `phone`, `credit_card` (Luhn), `iban` (mod 97), `cn_id` (18-digit resident ID, check digit and birth date), `us_ssn` (issued ranges only), `secret` (prefixed API keys, AWS ids, GitHub/Slack/Google/GitLab tokens, bearer tokens, JWTs), `private_key` (PEM) |
| `deny_keywords`, `deny_patterns` | union | substrings, matched ignoring case, zero-width characters and full-width ASCII forms (other look-alikes are not folded); RE2 expressions, matched as written (an invalid one fails the request closed) |
| `action` | override | `block` (default), `redact` or `flag` |
| `check_response` | override | also check responses (default on) |
| `max_prompt_chars` | strictest | characters of system, messages, tool calls and results; `0` = unlimited |
| `blocked_models`, `blocked_tools` | union | names or globs (`pkg/match`); tools are refused when offered or called in the history |
| `stream_window` | override, global | bytes of streamed text kept for matching across chunks (2048, at most 16384) |
| `block_unscannable_audio` | override | with `block` and any content rule or moderation on, refuse WebSocket audio input, which the rules cannot read (default off) |
| `moderation_upstream`, `moderation_model`, `moderation_fail` | override, global/group | an upstream's OpenAI-compatible `/moderations` (path appended to its first endpoint's base URL, called with one of its credentials through the core egress); `closed` (default) refuses the request when the call fails, `open` lets it through and notes it |
| `max_moderated_events` | strictest, global/group | Realtime client events of one session whose user text is sent to moderation (200); past them moderation counts as failed (`moderation_fail`) |

- What is read: on requests, all text the client supplies — system
  prompts, every turn's text and thinking, tool calls' arguments and tool
  results (not images or files); on responses, the model's text (not tool
  input or thinking). Moderation gets the user turns' text only. Detectors
  are conservative: numbers count only when their checksum or structure
  holds (`plugins/internal/textscan`, shared with `middleware.redact`).
- Not covered: `/native` passthrough runs no request hooks, so guardrails
  (like every request plugin) never see it; keep it off for keys
  guardrails must hold. Batches are checked request by request
  ([clients](clients.md#batches)). WebSocket sessions are checked event by
  event, text only: client events' text and output text deltas, never
  audio ([clients](clients.md#realtime-and-websocket)).
- `block`: the client gets its protocol's content-filter error (400 with
  `content_filter` on OpenAI, `invalid_request_error` on Anthropic);
  blocked models and tools get 403, an oversized prompt 400. A refused
  complete response is not charged; its upstream cost is still recorded.
- `redact`: matches become `[REDACTED:<rule>]` in the user turns' text of
  the request (patch and convert tiers, like `middleware.redact`) and in
  complete responses (re-encoded, even in passthrough). A match anywhere
  else in the request (system prompt, other turns, tool calls and results)
  is not edited: it is noted as `flag`. A stream cannot be redacted once bytes
  are out, so there it is `flag`. A moderation hit has no span, so `redact`
  blocks it.
- Streams: text deltas are matched in a sliding window of `stream_window`
  bytes. On `block`, frames whose text could still be part of a match are
  held back and the stream stops with the protocol's error event as soon as
  a match appears (a match at the newest byte that more text could undo,
  such as a number that goes on, waits for that text or the block's end),
  so no part of a match reaches the client: with keywords
  only, just a tail that could start a keyword waits; with any pattern or
  detector, text waits until it leaves the window, delaying the stream by up
  to `stream_window` bytes. The guarantee covers matches no longer than the
  window and within one content block (a block's end releases what is
  held). On `flag` nothing waits, and a match still touching the newest byte
  is noted once it is complete (or at the block's end).
- Verdicts: an attempt diagnostic note `guardrails: <action> <phase>
  <rules>` (phase `request`, `response`, `stream`) and
  `plugrelay_guardrail_verdicts_total{phase,action,rule}`. A request refused
  before any attempt has no attempt to note on; its log entry's error names
  the rules.

## Call order and failure policy

Same rules for every tier (`internal/pipeline`, `sdk/PROTOCOL.md`):

1. Once per request: `auth`, `OnRequest`, selector `Rank`, `meter.Reserve`,
   then `OnFinish` and `meter.Settle` or `meter.Refund`.
2. Once per attempt, again on every retry: `OnAttempt`, then the provider's
   `Prepare` / `Send` / `Classify`.
3. Pre-hooks run in configured order, post-hooks in reverse; a post-hook runs
   only for a plugin whose pre-hook ran.
4. Any pre-hook may return a `Reply`; the chain and the upstream are skipped.
5. `OnEvent` runs per streamed event, built-in only, within a 50 ms budget;
   it may pass, drop, or abort (returning an `*ir.GwError` ends the stream
   with that error). An `EventHolder` also says how many of its latest
   events must not reach the client yet; they go out once released, or are
   dropped when the stream is aborted. `OnResponse` runs once on a complete
   response.
6. On malfunction (crash, timeout, protocol error — `plugin.IsFailure`) the
   plugin's `FailPolicy` decides: `closed` rejects the request, `open`
   carries on. Defaults: closed for auth, meter and middleware; open for
   sinks and jobs.
7. `Settle` and `Refund` may be called again for the same request and must
   apply once.

## Conformance suite: plugintest

`pkg/plugin/plugintest` runs any `plugin.Provider` against a scripted upstream
and checks the verdicts and header handling the core relies on: normal,
stream, header ownership (a planted client secret must never reach the
upstream), cut stream, 429 with retry-after, 401, suspended account
(quarantine, no retry), content violation (no retry anywhere), uncertain
(conservative), timeout before send, slow first byte, oversized SSE event,
error passthrough, 5xx.

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

A header `Prepare` sets must be listed in `OwnedHeaders`, unless `Prepare`
only fills it when the client sent none (a protocol version default): the
suite re-runs `Prepare` with a client value to tell the two apart.

`UseSend: true` exercises a provider's own `Send`. `Skip` and `Expect` adapt
scenarios that genuinely differ. Every endpoint of every file in `manifests/`
runs the suite in `plugins/provider/compatible/conformance_test.go`, pointed
at the fake upstream; Bedrock runs it through `Send` with event stream
fixtures, Vertex against a fake token endpoint (`plugins/provider/internal/cloudtest`). External providers run through the host in
`internal/extplugin/examples_test.go` (the echo provider passes the suite);
there is no out-of-tree runner for non-Go plugins.
