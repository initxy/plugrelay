# How plugrelay compares

English | [简体中文](comparison.zh-CN.md)

An honest comparison with the relays and gateways people usually weigh it
against. Competitor facts were checked on **2026-11-05** against each
project's own repository or documentation; every claim carries a source tag
([S1] …) resolved in [Sources](#sources). plugrelay claims point at code or a
test in this repository and were checked against the code on
**2026-11-08**. "unknown" means the
project's own material does not say, not that the feature is missing.

- [At a glance](#at-a-glance)
- [Also surveyed](#also-surveyed)
- [Protocols and conversion](#protocols-and-conversion)
- [Coding clients](#coding-clients)
- [Prompt-cache stickiness](#prompt-cache-stickiness)
- [Private reasoning state](#private-reasoning-state)
- [Extension model](#extension-model)
- [Storage and clustering](#storage-and-clustering)
- [Billing](#billing)
- [Console](#console)
- [Deployment and license](#deployment-and-license)
- [What plugrelay matches](#what-plugrelay-matches)
- [Where plugrelay is ahead](#where-plugrelay-is-ahead)
- [Where others are ahead](#where-others-are-ahead)
- [Known gaps in plugrelay](#known-gaps-in-plugrelay)
- [Sources](#sources)

## At a glance

| | stars · license · latest release [S0] | language | aimed at |
|-|-|-|-|
| new-api | 51.5k · AGPL-3.0 (+§7 attribution terms) · v1.0.0-rc.48, 2026-11-04 | Go + React | multi-tenant gateway and resale site [S1] |
| one-api | 37.3k · MIT · v0.6.10, 2025-02-02; last push 2026-01-09 | Go + JS | key management and redistribution [S5] |
| sub2api | 45.5k · LGPL-3.0 · v0.2.19, 2026-10-28 | Go + Vue | sharing subscription-account quota [S6] |
| LiteLLM proxy | 62.8k · MIT outside `enterprise/` · v1.108.1, 2026-11-05 | Python (+ Rust core) | enterprise gateway, 100+ providers [S9][S10] |
| claude-code-router | 38.0k · MIT · v3.1.3, 2026-10-09 | TypeScript, Electron | local, single-user control plane for coding agents [S16] |
| Portkey gateway | 13.3k · MIT · v1.15.2, 2026-01-12 | TypeScript | app gateway with guardrails [S18] |
| Bifrost | 9.4k · Apache-2.0 · HTTP gateway `transports/v2.4.0`, 2026-10-29 (GitHub's "latest" is a Helm chart tag) | Go | high-throughput app gateway [S19] |
| OpenRouter | hosted service, not self-hostable | — | hosted model marketplace [S20] |
| plugrelay | first public release · Apache-2.0 ([`LICENSE`](../LICENSE)) | Go + React | coding clients, self-hosted |

## Also surveyed

Also looked at: the self-hosted relays CLIProxyAPI, claude-relay-service,
gpt-load, axonhub, octopus and CliRelay, and the hosted gateways Helicone,
Vercel AI Gateway and Cloudflare AI Gateway. They are not
compared feature by feature here: their claims were not checked against
their own sources. The hosted gateways can sit behind plugrelay as
upstreams: official manifests exist for OpenRouter, Vercel AI Gateway,
Cloudflare AI Gateway, Helicone, Portkey (hosted and self-hosted), LiteLLM,
Bifrost, New API and One API (`manifests/`, `TestManifestConformance`).

## Protocols and conversion

The question that matters for coding clients: can a client speaking protocol A
use an upstream speaking protocol B, and does same-protocol traffic stay
untouched?

| | client-side protocols | cross-protocol conversion | same-protocol handling |
|-|-|-|-|
| new-api | OpenAI chat, Responses, Anthropic messages, Gemini; plus realtime/WebSocket, images, audio, embeddings, rerank [S1] | between the four text protocols via its RelayKit module; "protocol-specific tools and fields may not map exactly" [S1] | unknown |
| one-api | OpenAI format [S5] | per-provider adapters into the OpenAI format [S5] | n/a |
| sub2api | Anthropic messages, Responses (incl. Codex WebSocket ingress), chat, Gemini; xAI and Antigravity routes [S6] | selected bridges, e.g. `/v1/messages` converted to xAI Responses [S6] | unknown |
| LiteLLM | OpenAI format first; also `/responses`, `/messages`, embeddings, images, audio, batches, rerank, A2A [S9] | everything through its OpenAI-shaped interface [S9] | unknown |
| claude-code-router | OpenAI chat / Responses, Anthropic messages, Gemini generateContent [S16] | yes, between those [S16] | unknown |
| plugrelay | Anthropic messages, Responses, OpenAI chat, Gemini, legacy `/v1/completions`; OpenAI and Gemini embeddings, OpenAI images and audio; rerank in the Jina/Cohere-v1 shape (`/v1/rerank`) and Cohere v2 (`/v2/rerank`); OpenAI files/batches and Anthropic message batches (`pkg/protocol/*`, `internal/pipeline/batch.go`, routes in [clients.md](clients.md#what-the-relay-serves)) | neutral IR plus only the generic steps whose capabilities differ; each conversion graded lossless / lossy / discouraged / impossible, impossible endpoints never routed (`internal/convert`, `TestAssessMatrix`, `TestCapabilityFiltering`, `TestCrossAnthropicToChatStream`); embeddings convert OpenAI ⇄ Gemini (`TestEmbeddingsOpenAIToGemini`, `TestEmbeddingsGeminiToOpenAI`); rerank converts between its two client shapes and Hugging Face TEI's `/rerank` (`TestRerankToTEI`); images, audio and batches same-protocol only | byte-exact passthrough preferred, then a field-level patch tier (`TestPassthroughByteExact`, `TestPatchChangesOnlyModel`, `TestSameProtocolPreferred`) |

plugrelay's media routes share keys, routing, retries, the request log and
billing with chat; a model is routed only to endpoints declaring the operation
(`TestMediaCapabilityMissing`), uploads are streamed
(`TestTranscriptionUploadStreams`, `TestStreamedUploadNotRetried`) and images
are billed per image (`TestImagesBilledPerImage`, `TestPriceMediaUnits`).
Rerank is routed only to endpoints declaring it (`TestRerankCapabilityMissing`)
and billed per token or search unit (`TestRerankSearchUnitBilling`); 19
manifests declare it (`manifests/*.yaml`, `TestManifestConformance`).
Batches stay on the upstream credential that created them and are billed
once, at the price rule's `batch_discount`, when their results are first
read through the relay (`TestOpenAIBatchLifecycle`,
`TestAnthropicMessageBatchLifecycle`); results fetched elsewhere are not
billed. OpenAI Realtime (`/v1/realtime`) and the Responses WebSocket mode
are relayed to endpoints declaring them, held for per response and billed
from `response.done` / `response.completed` usage, input transcription
included, with an estimate for a response the client or upstream cut off
(`TestRealtimeRelaysAndMeters`, `TestResponsesWebSocket`,
`TestWebSocketClientLeavesMidResponse`, `TestRealtimeTranscriptionBilled`);
access lists and rate limits apply (`TestWebSocketRequestPlugins`); the
Responses mode answers `426` unless one upstream serves every model, and
Gemini Live is not served ([clients.md](clients.md#realtime-and-websocket)).
Not yet run against a live realtime upstream: that needs a paid OpenAI or
Azure OpenAI account.

## Coding clients

| | Claude Code | Codex | Gemini CLI | Cursor | Continue / others |
|-|-|-|-|-|-|
| new-api | supported; channel-affinity rules pass Claude CLI headers [S2] | supported; Codex session/turn headers passed through [S2] | Gemini endpoints served [S1] | unknown | unknown |
| sub2api | supported, incl. Antigravity endpoint [S6] | supported; nginx `underscores_in_headers` needed for sticky sessions [S6] | Gemini endpoints [S6] | unknown | unknown |
| LiteLLM | documented, with a daily automated Claude Code compatibility matrix per provider [S13] | documented [S14] | documented [S14] | best effort, "reverse engineering their APIs"; Cursor CLI cannot target it [S14] | unknown |
| claude-code-router | profiles for Claude Code, Codex, Grok CLI, Kimi CLI, Kilo Code, OpenCode, Pi and more [S16] | yes [S16] | not in its client list [S16] | not in its client list [S16] | — |
| OpenRouter | Anthropic-compatible endpoint; "only guaranteed to work with the Anthropic first-party provider" [S20] | unknown | unknown | unknown | unknown |
| plugrelay | `plugrelay setup claude-code`; `/api/hello` probe answered; `anthropic-beta` forwarded verbatim (`internal/cli/setup.go`, `TestClientConfigWriters`, `TestClaudeCodeRequest`) | `plugrelay setup codex` writes provider and model catalog; same-turn header pinning (`TestSetupCodexWritesCatalog`, `TestTurnToken`) | `plugrelay setup gemini` (`TestSetupNewFilesAndGemini`) | documented by hand ([clients.md](clients.md#cursor)) | Cline, OpenCode (setup command), Continue, Copilot, Trae documented ([clients.md](clients.md)); Continue's `/v1/completions` and fill-in-the-middle served (`TestCompletionsFIMPassthrough`, `TestCompletionsToChatStream`) |

plugrelay's end-to-end tests call every client protocol against a live
upstream (`test/e2e/e2e_test.go`) and, opt-in, drive the real Claude Code and
Codex CLIs through a live relay on an agentic task, over a same-protocol
route and a chat-only route that forces conversion (`TestClients` in
`test/e2e/clients_test.go`; the chat-only route uses the test-only
`e2e-chat-only` manifest defined there). The
last run, on 2026-11-06, passed 4/4, but they run on demand against one
upstream and the results are not published; LiteLLM's per-provider Claude
Code matrix [S13] is ahead here. One `TestRelay/openai_chat*` failure in
about nine full live runs was seen and not reproduced.

## Prompt-cache stickiness

Round-robin across credentials or accounts loses the provider's prompt cache,
which is the main reason coding traffic costs more through a relay.

| | mechanism |
|-|-|
| new-api | "channel affinity" rules keyed by header, context value or JSON path, with TTL and strict/prefer modes [S2] |
| sub2api | sticky sessions in account selection [S6] |
| LiteLLM | opt-in `prompt_caching` pre-call check routes follow-ups to the deployment that wrote the cache [S11] |
| claude-code-router | unknown (single-user, local) |
| plugrelay | binding kept only while the last turn read or wrote the cache (5 min, 1 h after a 1 h write); each user kept on ≤ 3 recent credentials; weights only split new sessions; across protocols the cache step adds breakpoints or a cache key ([architecture](architecture.md#routing-stickiness-and-failure-verdicts); `TestStickinessWindow`, `TestUserRecentCredential`, `TestWeightedSplitOnlyForNewSessions`, `TestCachePlacement`, `TestCacheModes`) |

## Private reasoning state

Thinking signatures, Responses `encrypted_content` and Gemini thought
signatures are only valid at the upstream (and often the organisation) that
issued them. Sending them elsewhere fails the request.

| | approach |
|-|-|
| new-api | a Gemini setting for function-call thought signatures, on by default [S3]; general handling unknown |
| sub2api | strips thinking blocks without a valid signature for Anthropic-strict upstreams, keeps them for upstreams that require passback; families detected by model-name prefix [S7]; Anthropic and Antigravity Claude "cannot be mixed within the same conversation" [S6] |
| LiteLLM | `encrypted_content_affinity` encodes the issuing deployment into Responses item ids and routes follow-ups back, added after a 2026-02 incident [S12]; cross-protocol handling unknown |
| claude-code-router | unknown |
| plugrelay | any upstream-private state is sealed with AES-256-GCM under the master key, tagged with its issuer, returned only to that issuer and silently dropped elsewhere; works across protocols wherever the client protocol has an echo field ([architecture](architecture.md#private-state-sealing); `TestSealedReasoningRoundTrip`, `TestOpaqueHistory`, `TestOpaqueResponseRoundTrip`, `TestTamper`, `TestWrongKey`). OpenAI chat has no echo field, so the state is lost there |

## Extension model

| | how you extend it |
|-|-|
| new-api | JavaScript task plugins for async image/video APIs, with a plugin marketplace in the console [S1]; new text upstreams are Go code in `relay/` [S1] |
| one-api | Go code [S5] |
| sub2api | out-of-process plugins over gRPC packaged as `.s2plugin` with a sandboxed UI; the only capability defined so far is an outbound transport, and no official example is published [S8] |
| LiteLLM | Python callbacks and hooks (pre-call, moderation, post-call, streaming) [S15]; custom providers in Python [S9] |
| claude-code-router | JavaScript plugins (`plugin.json` + module), wrapper and core gateway plugins, a marketplace schema [S16][S17] |
| Bifrost | Go and WASM middleware plugins; its docs home lists custom plugins as open source [S23], its README as enterprise [S19] |
| plugrelay | eight extension points (`Kind` in `pkg/plugin/plugin.go`) reachable three ways: YAML manifest for upstreams (108 in [`manifests/`](../manifests), `TestOfficialManifestsValidate`, `TestManifestConformance`), external process over JSON-RPC/stdio with a Go SDK in [`sdk/`](../sdk) (`internal/extplugin`, `TestCrashRestart`), or built-in Go. Plugins add no console UI; there is no plugin marketplace, no WASM plugins and no signed packages, by decision. Details: [plugins.md](plugins.md) |

## Storage and clustering

| | business data | shared runtime state / clustering |
|-|-|-|
| new-api | SQLite, MySQL ≥ 5.7.8, PostgreSQL ≥ 9.6; separate log DB incl. ClickHouse [S1] | Redis for shared rate limits; multi-node shares the main DB and secrets [S1] |
| one-api | SQLite, MySQL; PostgreSQL "适配中" (being adapted) [S5] | multi-node via shared MySQL + Redis + `SYNC_FREQUENCY` polling [S5] |
| sub2api | PostgreSQL 15+ required [S6] | Redis 7+ required [S6] |
| LiteLLM | PostgreSQL for keys, teams, budgets, usage [S15] | Redis; Terraform modules for a componentised stack [S9] |
| claude-code-router | SQLite (`packages/core/src/storage/sqlite-native.ts`) [S17] | n/a (local) |
| Bifrost | config and log stores [S19] | clustering is an enterprise feature [S19][S23] |
| plugrelay | SQLite (pure Go, default), PostgreSQL or MySQL 8 (`internal/store`, `internal/sqldialect`, `TestSQLite`, `TestPostgres`, `TestMySQL`); YAML export/apply (`internal/store/yamlio.go`) | memory, SQL or Redis (`internal/state`, `TestMemory`, `TestRedis`); routing health shared across nodes and shown live in the console (`TestConsoleShowsRouterLiveState`) |

## Billing

| | unit | payments |
|-|-|-|
| new-api | quota and subscriptions with expression-based pricing [S1] | Stripe, EPay, Creem, Waffo top-up and subscription handlers [S4] |
| one-api | points: quota = group ratio × model ratio × (prompt + completion × completion ratio); optional USD display [S5] | top-up link only [S5] |
| sub2api | token-level cost calculation [S6] | built in: EasyPay, Alipay, WeChat Pay, Stripe [S6] |
| LiteLLM | USD spend per key, user, team, with budgets [S9][S15] | none in the proxy (unknown otherwise) |
| claude-code-router | estimated cost per request, local limits [S16] | none |
| plugrelay | real money: prices per million tokens in micro-units of the site currency (`PriceRule` in `internal/store/types.go`), per image, audio second or 1k characters for media (`TestPriceMediaUnits`); every sent attempt costed, by default only the delivered one charged; key/user/group budgets (`plugins/meter/quota`, `TestCostRecordedForFailedSentAttempt`, `TestMeterSettleOrRefundExactlyOnce`, `TestSettleRefundIdempotent`) | billing and sign-up features, compiled in and off by default (`TestSellKitOffByDefault`): prepaid balance with hold → capture / release that never overdraws under concurrency (`plugins/meter/balance`, `TestReserveSettleRefund`, `TestConcurrentReserveNeverOverdraws`, `TestLedgerSQLite`, `TestLedgerPostgres`), redeem codes (`plugins/admin/redeem`, `TestBatchAndRedeem`), Stripe Checkout top-ups (`plugins/admin/payment/stripe`, `TestCheckoutAndWebhook`), epay (易支付) top-ups for Alipay and WeChat Pay on CNY sites (`plugins/admin/payment/epay`, `TestSubmitCheckoutAndNotify`), subscription plans with a money or token allowance per period, paid from the balance, by Stripe subscriptions or through epay (`plugins/admin/subscriptions`, `TestSubscriptions`, `TestSellKitStripeSubscription`, `TestSellKitEpaySubscription`), coupons on top-ups, daily check-in and referral rewards in the ledger, announcements (`plugins/admin/rewards`, `plugins/admin/announcements`, `TestCoupons`, `TestCheckIn`, `TestReferrals`, `TestSellKitCheckInOncePerDay`, `TestSellKitCouponAndReferral`, `TestSellKitAnnouncements`), self-service sign-up (`plugins/admin/signup`, `TestRegister`, `TestInviteRequired`), and a new-api / one-api importer that keeps keys working (`internal/importnewapi`, `TestDryRunThenImport`); live: `TestSellKitBalance`. Stripe and epay are tested against fake servers only. Details: [sell-kit.md](sell-kit.md) |

## Console

| | console |
|-|-|
| new-api | React; usage dashboard, plugin marketplace, model square, playground; seven languages [S21] |
| sub2api | Vue admin dashboard; one-click self-update with rollback; iframe embedding of external systems [S6] |
| LiteLLM | admin UI with keys, teams, budgets, guardrails, request logs [S9] |
| claude-code-router | desktop app (macOS, Windows, Linux) plus browser UI [S16] |
| Portkey | local log console [S18] |
| plugrelay | built into the binary, one menu by role and enabled features; setup wizard that asks how the relay will be used and turns features on (`TestSetupSellEnablesSellKit`); features page with one switch and one form each, plus extensions (`internal/features`, `TestRegistry`); overview with spend by client; upstreams added from a searchable provider gallery with credentials in the same step (`web/src/features/upstreams`, `TestConsoleCreatesUpstreamWithManifestVariables`); models with presets; request log with every upstream attempt, sessions and CSV export (`TestSessionsDefaultSpan`, `TestAnalytics`); usage analytics (weekday × hour, failures by cause); playground (`TestPlaygroundEveryProtocol`); notifications; public models-and-prices and status pages (`TestPublicPages`); billing pages while that feature is on (`web/src/features/billing`); live credential state (`TestConsoleShowsRouterLiveState`); single sign-on via OIDC (Google, Microsoft Entra, Keycloak, Authentik or any issuer), GitHub OAuth and Feishu/Lark (`internal/admin/sso_*.go`, `TestSSO`, `TestIdentitiesSQLite`, `TestUserResetPassword`); TOTP 2FA with recovery codes, passkeys and an admin 2FA policy (`internal/admin/mfa*.go`, `TestMFA`), login captcha after repeated failures; English and Simplified Chinese only, with identical keys (`web/src/i18n/i18n.test.ts`) and backend errors in both (`TestErrorCodesTranslated`); only public APIs (`internal/admin/openapi_test.go`); `plugrelay demo` for a seeded local copy (`TestSeed`) |

## Deployment and license

| | deployment | license |
|-|-|-|
| new-api | Docker, Compose (with PostgreSQL + Redis), Electron desktop wrapper [S1] | AGPL-3.0 with attribution terms; modified UIs must keep a visible link [S1] |
| sub2api | install script + systemd, Compose, Apple container; needs PostgreSQL and Redis [S6] | LGPL-3.0 [S0] |
| LiteLLM | pip/uv, Docker, Terraform [S9] | MIT outside `enterprise/` [S10]; SSO free for up to 5 users since v1.76.0, beyond that an enterprise license [S22] |
| claude-code-router | desktop installers, npm CLI, Docker [S16] | MIT [S0] |
| plugrelay | single binary with embedded console, Docker image, Compose ([deployment.md](deployment.md), [`deploy/`](../deploy), [`.goreleaser.yaml`](../.goreleaser.yaml)); SQLite needs nothing else | Apache-2.0 |

## What plugrelay matches

Console and operations features, all present here:

- adding an upstream from a searchable provider list with its credentials
  in the same step (`web/src/features/upstreams/add-upstream-page.tsx`);
- the client program behind each request, spend by client, a session view
  and CSV export of the log (`pkg/clientid`, `TestDetect`,
  `TestSessionsDefaultSpan`, [api](api.md#request-log-analytics));
- a CC Switch import link next to the connect snippets (`TestCCSwitchLinks`);
- notifications by email, Feishu/Lark, DingTalk, WeCom and webhook
  (`TestNotificationsEndToEnd`, [notifications](notifications.md));
- presets in every client protocol's model list
  ([configuration](configuration.md#routing-models-and-presets));
- price sync from models.dev, manual prices winning (`TestPriceSync`);
- a playground on the normal request path (`TestPlaygroundEveryProtocol`);
- public pricing and status pages (`TestPublicPages`);
- on the selling side: an orders page, coupons, daily check-in, referral
  rewards and announcements ([sell kit](sell-kit.md#coupons-check-in-referrals-announcements)).

Matching means the feature exists; none of these has seen production
traffic.

## Where plugrelay is ahead

Narrow, and only where the code or a test shows it; maturity is not among
them.

- **Private reasoning state across protocols.** Sealed and returned only to
  its issuer for every client protocol with an echo field
  ([table](#private-reasoning-state)); sub2api cannot mix families in one
  conversation [S6] and LiteLLM's fix covers Responses item ids [S12].
- **Graded conversion.** Every conversion is graded and endpoints that cannot
  serve a request are never routed (`TestAssessMatrix`,
  `TestCapabilityFiltering`); new-api warns that fields "may not map
  exactly" [S1].
- **Same-protocol traffic untouched.** Byte-exact passthrough is preferred
  (`TestPassthroughByteExact`); the others do not document theirs.
- **Leaving new-api.** `admin.import-newapi` reads a new-api / one-api
  database and keeps its `sk-` keys working (`TestDryRunThenImport`).

## Where others are ahead

- **Maturity and community.** Every project above has thousands to tens of
  thousands of stars, releases and production users [S0][S9]; plugrelay is
  at its first public release, with no production users reported yet.
- **Provider breadth.** LiteLLM covers 100+ providers [S9], Portkey routes to
  1,600+ models [S18], Bifrost 23+ providers [S19]; plugrelay ships 108
  manifests (`manifests/`, `TestManifestConformance`), each checked against
  the vendor's docs in 2026-11: 23 for self-hosted inference servers, five
  for self-hosted gateways and one generic. That is about LiteLLM's count,
  but several are regional editions of one vendor (Kimi, GLM, MiniMax,
  Qwen) and few have been run against a live account.
- **Payments and resale.** new-api offers Stripe, EPay, Creem and Waffo with
  subscriptions [S4]; sub2api EasyPay, Alipay, WeChat Pay and Stripe [S6].
  plugrelay's billing feature has Stripe and the epay protocol (Alipay,
  WeChat Pay) with subscription plans but no Creem or Waffo, and both
  gateways have only met fake servers. Sign-up has an
  optional Turnstile/hCaptcha check but no email verification
  (`plugins/admin/signup/signup.go`).
- **Modalities.** new-api serves realtime [S1]; LiteLLM batches and A2A
  [S9]. plugrelay has embeddings, images, audio, rerank and batches, but images, audio and batches only within one protocol;
  realtime is relayed to OpenAI and Azure only, verified against fakes (the
  test upstream answers 404 on `/v1/realtime`, so `TestWebSocket` skips; a
  live run needs a paid OpenAI or Azure account),
  and there is no Gemini Live. Images passed live; embeddings (OpenAI and Gemini shapes), rerank
  (common and Cohere v2 shapes) and transcription passed live only against
  local open-source servers, TEI and whisper.cpp (`TestSelfHostedMedia`,
  [run](compat/2026-11-06-live.md)), not a commercial API; speech and
  batches only against fakes, because the test upstream answers 404
  (`TestRelay` and `TestRerankAndBatches` skip them, saying so).
  Voyage, Mixedbread, NVIDIA and DashScope rerank use their own shapes and
  have no codec.
- **Enterprise identity.** new-api has OAuth/OIDC, passkeys and 2FA [S1];
  plugrelay has SSO, TOTP 2FA and passkeys too (`TestMFA`), but its passkeys
  have only met a software authenticator in tests, not real browsers and
  devices.
- **Guardrails and MCP.** LiteLLM and Portkey have guardrail frameworks and
  MCP gateways [S9][S18]; Bifrost's docs home marks guardrails enterprise
  and does not mark its MCP gateway either way (only "MCP Tool Filtering" is
  labelled open source) [S23], while its README lists both under enterprise
  deployments [S19]; plugrelay
  has built-in checks (deny lists, PII and secret detectors, size, model and tool
  limits, an OpenAI-compatible moderation call; `middleware.guardrails`)
  and an MCP gateway (`internal/mcp`) but no third-party guardrail
  integrations, no stdio MCP servers and no OAuth to MCP servers. The MCP
  gateway passed live against the official reference server with the
  official TypeScript SDK client and Claude Code (`TestMCPReferenceServer`,
  [run](compat/2026-11-06-live.md)), on protocol 2025-11-25 only;
  guardrails are verified against fakes only.
- **Client compatibility evidence.** LiteLLM publishes a daily Claude Code
  matrix per provider [S13]; plugrelay publishes dated runs of real Claude
  Code and Codex on a passthrough and a fully converted route
  ([compat](compat/README.md), `scripts/compat.sh`, `TestClients`; a weekly
  workflow when its secrets are set), but against one upstream, not a matrix
  of providers.
- **Databases in production.** new-api runs on MySQL ≥ 5.7.8 and keeps logs
  in a separate database, ClickHouse included [S1]; plugrelay's store runs on
  MySQL 8.0.13+ (`TestMySQL`, `TestLedgerMySQL`) but has no separate log
  database; MySQL and MariaDB are tested by hand only, on fresh databases.
- **Console languages.** new-api's console has seven languages [S21];
  plugrelay's has English and Simplified Chinese, by decision.
- **Extensions.** new-api has a plugin marketplace in its console [S1] and
  claude-code-router a marketplace schema [S16][S17]; plugrelay has
  neither, by decision.
- **Subscription accounts.** sub2api and claude-code-router pool OAuth
  subscription accounts and rotate keys [S6][S16]. plugrelay refuses
  subscription-account OAuth pooling by decision
  ([README](../README.md#non-goals)); for operators who want it, that is a
  gap.

## Known gaps in plugrelay

| gap | status |
|-|-|
| AWS Bedrock and Google Vertex AI | served (`plugins/provider/bedrock`: SigV4 or API key, event-stream decoding; `plugins/provider/vertex`: service-account OAuth; `manifests/bedrock.yaml`, `vertex-anthropic.yaml`, `vertex-gemini.yaml`), but verified only against fakes and published test vectors (`TestSignatureVerifiesAtUpstream`, `TestStreamDecodesChunks`, `TestTokenLifecycle`), not a live account; neither manifest lists models or prices |
| Upstream quota display | probed by `job.quotaprobe` and shown per credential, used by the `quota_reset` routing strategy and to rest used-up credentials until the reset (`TestQuotaProbeEndToEnd`); of the official manifests only OpenRouter declares a probe (`manifests/openrouter.yaml`, `TestOpenRouterQuotaProbe`), and that probe has not been run against a live OpenRouter account |
| Embeddings and audio | served (`pkg/protocol/openaiembeddings`, `geminiembeddings`, `openaiaudio`); embeddings (OpenAI and Gemini clients) and transcription verified live only against TEI and whisper.cpp on CPU (`TestSelfHostedMedia`, [run](compat/2026-11-06-live.md)); speech only against fakes, and no commercial embeddings or audio API has been run |
| Images and audio across protocols | passthrough to the same protocol only |
| Rerank, batches | served (`pkg/protocol/rerank`, `coherererank`, `teirerank`; `internal/pipeline/batch.go`), rerank to TEI verified live through `/v1/rerank` and `/v2/rerank` (`TestSelfHostedMedia`; TEI reports no usage, so the client's response shows zero tokens while the log bills an estimate), other rerank shapes and batches against fake upstreams only (`TestRerankToTEI`, `TestOpenAIBatchLifecycle`, `TestAnthropicMessageBatchLifecycle`; `TestRerankAndBatches` skips on the live test upstream, which 404s these routes); batches same-protocol only and billed only when results are read through the relay; Voyage, Mixedbread, NVIDIA, DashScope and DeepInfra-native rerank shapes have no codec |
| Realtime / WebSocket | OpenAI Realtime (`/v1/realtime`, OpenAI and Azure manifests) and the Responses WebSocket mode (official OpenAI only; `426` unless one upstream serves every model, so Codex falls back to HTTP) relayed with a meter hold and settlement per response, input transcription billed, cut-off responses estimated, access lists and rate limits applied (`internal/pipeline/websocket.go`), verified against fake WebSocket upstreams only; no Gemini Live; audio tokens priced at the model's token rates |
| Payments | Stripe (top-ups and subscriptions) and epay tested against fake servers only; no Creem, Waffo or direct Alipay/WeChat Pay merchant APIs; plan changes are not prorated |
| Sign-up abuse | optional Turnstile or hCaptcha on sign-up and on the password login after failures, pass and fail verified live against Turnstile's and hCaptcha's real siteverify with their published test keys (`TestCaptchaLive`, [run](compat/2026-11-06-live.md)), outages against fakes (`TestCaptcha`, `TestMFA/*/LoginCaptcha`); no email verification |
| 2FA, passkeys | TOTP, recovery codes and passkeys (`TestMFA`); WebAuthn checked against a software authenticator, not a real browser |
| MCP gateway, guardrails | built in (`internal/mcp`: Streamable HTTP for clients, Streamable HTTP and HTTP+SSE upstreams, per-key grants, `/mcp` aggregation; `middleware.guardrails`, off by default), the MCP gateway verified live against `@modelcontextprotocol/server-everything` with the official TypeScript SDK client and Claude Code (`TestMCPReferenceServer`), on protocol 2025-11-25 only: the 2026-07-28 revision, the HTTP+SSE fallback and progress streaming against fakes only (`internal/mcp` tests), guardrails against fake moderation servers only (`TestModeration`); no stdio servers, no upstream OAuth |
| Console languages | English and Simplified Chinese only (`web/src/i18n`) |
| Subscription accounts | no OAuth pooling of Claude / ChatGPT subscription accounts, by decision |
| Plugin marketplace | none; external plugins are installed by hand into `external_plugins.dir` |
| MySQL as the store | MySQL 8.0.13+ runs the same store, ledger and state suites when `PLUGRELAY_TEST_MYSQL` is set (`internal/store`, `TestMySQL`, `TestLedgerMySQL`); CI runs the schema (`internal/store/migrations/mysql/0001_init.sql`) and the store, admin, importnewapi and state suites on a fresh MySQL 8.4 (`.github/workflows/ci.yml`); the store suite also passed by hand on MariaDB 10.6 |
| Maturity | first public release; no production users reported yet |

## Sources

All checked 2026-11-05 unless marked otherwise.

- [S0] Repository metadata (Bifrost releases re-checked 2026-11-06) via the GitHub API (`gh api repos/<owner>/<repo>` and `…/releases/latest`): [new-api](https://github.com/QuantumNous/new-api), [one-api](https://github.com/songquanpeng/one-api), [sub2api](https://github.com/Wei-Shaw/sub2api), [litellm](https://github.com/BerriAI/litellm), [claude-code-router](https://github.com/musistudio/claude-code-router), [Portkey gateway](https://github.com/Portkey-AI/gateway), [bifrost](https://github.com/maximhq/bifrost)
- [S1] new-api README — https://github.com/QuantumNous/new-api/blob/main/README.md
- [S2] new-api channel affinity — https://github.com/QuantumNous/new-api/blob/main/setting/operation_setting/channel_affinity_setting.go
- [S3] new-api Gemini settings — https://github.com/QuantumNous/new-api/blob/main/setting/model_setting/gemini.go
- [S4] new-api payment handlers (`topup_stripe.go`, `subscription_payment_epay.go`, …) — https://github.com/QuantumNous/new-api/tree/main/controller
- [S5] one-api README — https://github.com/songquanpeng/one-api/blob/main/README.md
- [S6] sub2api README — https://github.com/Wei-Shaw/sub2api/blob/main/README.md
- [S7] sub2api thinking handling — https://github.com/Wei-Shaw/sub2api/blob/main/backend/internal/service/thinking_protocol.go
- [S8] sub2api plugin guide — https://github.com/Wei-Shaw/sub2api/blob/main/docs/PLUGIN_DEVELOPMENT.md
- [S9] LiteLLM README — https://github.com/BerriAI/litellm/blob/main/README.md
- [S10] LiteLLM license — https://github.com/BerriAI/litellm/blob/main/LICENSE
- [S11] LiteLLM prompt-cache routing — https://github.com/BerriAI/litellm-docs/blob/main/docs/tutorials/claude_code_prompt_cache_routing.md
- [S12] LiteLLM encrypted-content incident — https://github.com/BerriAI/litellm-docs/blob/main/blog/responses_api_encrypted_content_incident/index.md
- [S13] LiteLLM Claude Code compatibility matrix — https://github.com/BerriAI/litellm-docs/blob/main/docs/claude_code_compatibility.md
- [S14] LiteLLM client guides: [Codex](https://github.com/BerriAI/litellm-docs/blob/main/docs/proxy/client_setup/codex_cli.md), [Gemini CLI](https://github.com/BerriAI/litellm-docs/blob/main/docs/tutorials/litellm_gemini_cli.md), [Cursor](https://github.com/BerriAI/litellm-docs/blob/main/docs/tutorials/cursor_integration.md)
- [S15] LiteLLM [database](https://github.com/BerriAI/litellm-docs/blob/main/docs/proxy/db_info.md) and [hooks](https://github.com/BerriAI/litellm-docs/blob/main/docs/proxy/call_hooks.md)
- [S16] claude-code-router README — https://github.com/musistudio/claude-code-router/blob/main/README.md
- [S17] claude-code-router source tree (`packages/core/src/plugins`, `packages/core/src/storage`) — https://github.com/musistudio/claude-code-router/tree/main/packages/core/src
- [S18] Portkey gateway README — https://github.com/Portkey-AI/gateway/blob/main/README.md
- [S19] Bifrost README — https://github.com/maximhq/bifrost/blob/main/README.md
- [S20] OpenRouter, Claude Code integration — https://openrouter.ai/docs/guides/guides/claude-code-integration
- [S21] new-api console locales (`en`, `fr`, `ja`, `ru`, `vi`, `zh`, `zh-TW`; checked 2026-11-06) — https://github.com/QuantumNous/new-api/tree/main/web/src/i18n/locales
- [S22] LiteLLM OIDC SSO docs (checked 2026-11-06) — https://github.com/BerriAI/litellm-docs/blob/main/docs/proxy/admin_ui_sso.md
- [S23] Bifrost docs home (checked 2026-11-06) — https://docs.getbifrost.ai/
