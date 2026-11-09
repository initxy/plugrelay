# How plugrelay compares

English | [简体中文](comparison.zh-CN.md)

plugrelay next to the relays and gateways people usually weigh it against.
Facts about other projects come from their own repositories and docs, checked
on 2026-11-05; each carries a source tag resolved in [Sources](#sources).
Claims about plugrelay point at its code or tests and were checked on
2026-11-08. "unknown" means the project's own material does not say.

[At a glance](#at-a-glance) · [Also surveyed](#also-surveyed) · [Protocols and conversion](#protocols-and-conversion) · [Coding clients](#coding-clients) · [Prompt-cache stickiness](#prompt-cache-stickiness) · [Private reasoning state](#private-reasoning-state) · [Extension model](#extension-model) · [Storage and clustering](#storage-and-clustering) · [Billing](#billing) · [Console](#console) · [Deployment and license](#deployment-and-license) · [What plugrelay matches](#what-plugrelay-matches) · [Where plugrelay is ahead](#where-plugrelay-is-ahead) · [Where others are ahead](#where-others-are-ahead) · [Known gaps in plugrelay](#known-gaps-in-plugrelay) · [Sources](#sources)

## At a glance

| | stars · license · latest release [S0] | language | aimed at |
|-|-|-|-|
| new-api | 51.5k · AGPL-3.0 (+§7 attribution terms) · v1.0.0-rc.48, 2026-11-04 | Go + React | multi-tenant gateway and resale site [S1] |
| one-api | 37.3k · MIT · v0.6.10, 2025-02-02; last push 2026-01-09 | Go + JS | key management and redistribution [S5] |
| sub2api | 45.5k · LGPL-3.0 · v0.2.19, 2026-10-28 | Go + Vue | sharing subscription-account quota [S6] |
| LiteLLM proxy | 62.8k · MIT outside `enterprise/` · v1.108.1, 2026-11-05 | Python (+ Rust core) | enterprise gateway, 100+ providers [S9][S10] |
| claude-code-router | 38.0k · MIT · v3.1.3, 2026-10-09 | TypeScript, Electron | local, single-user control plane for coding agents [S16] |
| Portkey gateway | 13.3k · MIT · v1.15.2, 2026-01-12 | TypeScript | app gateway with guardrails [S18] |
| Bifrost | 9.4k · Apache-2.0 · `transports/v2.4.0`, 2026-10-29 (GitHub's "latest" is a Helm chart tag) | Go | high-throughput app gateway [S19] |
| OpenRouter | hosted only | — | hosted model marketplace [S20] |
| plugrelay | first public release · Apache-2.0 | Go + React | coding clients, self-hosted |

## Also surveyed

Not compared here, because their claims were not checked against their own
sources: CLIProxyAPI, claude-relay-service, gpt-load, axonhub, octopus,
CliRelay, Helicone, Vercel AI Gateway, Cloudflare AI Gateway. Gateways can
sit behind plugrelay as upstreams; [`manifests/`](../manifests) covers
OpenRouter, Vercel, Cloudflare, Helicone, Portkey, LiteLLM, Bifrost, New API
and One API.

## Protocols and conversion

Can a client speaking protocol A use an upstream speaking B, and does
same-protocol traffic stay untouched?

| | client-side protocols | cross-protocol conversion | same-protocol handling |
|-|-|-|-|
| new-api | OpenAI chat, Responses, Anthropic messages, Gemini; realtime, images, audio, embeddings, rerank [S1] | between the four text protocols; "protocol-specific tools and fields may not map exactly" [S1] | unknown |
| one-api | OpenAI format [S5] | per-provider adapters into the OpenAI format [S5] | n/a |
| sub2api | Anthropic messages, Responses (incl. Codex WebSocket), chat, Gemini; xAI and Antigravity routes [S6] | selected bridges, e.g. `/v1/messages` to xAI Responses [S6] | unknown |
| LiteLLM | OpenAI format first; also `/responses`, `/messages`, embeddings, images, audio, batches, rerank, A2A [S9] | everything through its OpenAI-shaped interface [S9] | unknown |
| claude-code-router | OpenAI chat / Responses, Anthropic messages, Gemini [S16] | between those [S16] | unknown |
| plugrelay | Anthropic messages, Responses, OpenAI chat, Gemini, legacy completions; embeddings, images, audio, rerank, batches, Realtime ([routes](clients.md#what-the-relay-serves)) | a neutral IR plus only the steps where capabilities differ; each endpoint graded lossless / lossy / discouraged / impossible, and impossible ones are never routed ([architecture](architecture.md#ir-and-capabilities)). Embeddings and rerank convert across shapes; images, audio and batches don't | byte-exact passthrough first, then a field-level patch ([tiers](architecture.md#processing-tiers)) |

Media, batch and WebSocket routes share keys, routing, the request log and
billing with chat, and go only to endpoints that declare the operation
([clients](clients.md#what-the-relay-serves)).

## Coding clients

| | Claude Code | Codex | Gemini CLI | Cursor | others |
|-|-|-|-|-|-|
| new-api | supported; affinity rules pass Claude CLI headers [S2] | supported; session/turn headers passed through [S2] | Gemini endpoints [S1] | unknown | unknown |
| sub2api | supported, incl. Antigravity [S6] | supported; nginx `underscores_in_headers` needed for sticky sessions [S6] | Gemini endpoints [S6] | unknown | unknown |
| LiteLLM | documented, with a daily Claude Code compatibility matrix per provider [S13] | documented [S14] | documented [S14] | best effort, "reverse engineering their APIs"; Cursor CLI cannot target it [S14] | unknown |
| claude-code-router | profiles for Claude Code, Codex, Grok CLI, Kimi CLI, Kilo Code, OpenCode, Pi and more [S16] | yes [S16] | not listed [S16] | not listed [S16] | — |
| OpenRouter | Anthropic-compatible endpoint, "only guaranteed to work with the Anthropic first-party provider" [S20] | unknown | unknown | unknown | unknown |
| plugrelay | `plugrelay setup claude-code`; `anthropic-beta` forwarded as is | `plugrelay setup codex` writes the provider and model catalog; one turn stays on one credential | `plugrelay setup gemini` | by hand ([clients](clients.md#cursor)) | OpenCode by setup command; Cline, Continue (incl. fill-in-the-middle), Copilot, Trae by hand ([clients](clients.md)) |

plugrelay's opt-in end-to-end test drives the real Claude Code and Codex
through the relay on a small coding task, over a same-protocol route and a
chat-only route that forces conversion. The last run passed 4/4
([compat](compat/README.md)).

## Prompt-cache stickiness

Round-robin across credentials loses the provider's prompt cache, which is
the main reason coding traffic costs more through a relay.

| | mechanism |
|-|-|
| new-api | "channel affinity" rules keyed by header, context value or JSON path, with TTL and strict/prefer modes [S2] |
| sub2api | sticky sessions in account selection [S6] |
| LiteLLM | opt-in `prompt_caching` pre-call check sends follow-ups to the deployment that wrote the cache [S11] |
| claude-code-router | unknown (single-user, local) |
| plugrelay | a session stays on its credential only while the last turn read or wrote the cache (5 min, or 1 h after a 1 h write); each user stays on at most 3 recent credentials; weights split only new sessions; across protocols, breakpoints or a cache key are added ([architecture](architecture.md#routing-stickiness-and-failure-verdicts)) |

## Private reasoning state

Thinking signatures, Responses `encrypted_content` and Gemini thought
signatures are valid only at the upstream (often the organisation) that
issued them. Sent anywhere else, the request fails.

| | approach |
|-|-|
| new-api | a Gemini setting for function-call thought signatures, on by default [S3]; general handling unknown |
| sub2api | strips thinking blocks without a valid signature for strict Anthropic upstreams, keeps them where passback is required; families detected by model-name prefix [S7]; Anthropic and Antigravity Claude "cannot be mixed within the same conversation" [S6] |
| LiteLLM | `encrypted_content_affinity` encodes the issuing deployment into Responses item ids and routes follow-ups back, added after a 2026-02 incident [S12]; cross-protocol handling unknown |
| claude-code-router | unknown |
| plugrelay | any upstream-private state is encrypted (AES-256-GCM, key from the master key), tagged with its issuer, returned only to that issuer and dropped elsewhere. Works across protocols wherever the client protocol has a field to carry it; OpenAI chat has none, so the state is lost there ([architecture](architecture.md#private-state-sealing)) |

## Extension model

| | how you extend it |
|-|-|
| new-api | JavaScript task plugins for async image/video APIs, with a plugin marketplace in the console; new text upstreams are Go code in `relay/` [S1] |
| one-api | Go code [S5] |
| sub2api | out-of-process gRPC plugins packaged as `.s2plugin` with a sandboxed UI; the only capability so far is an outbound transport, and no official example is published [S8] |
| LiteLLM | Python callbacks and hooks (pre-call, moderation, post-call, streaming) [S15]; custom providers in Python [S9] |
| claude-code-router | JavaScript plugins (`plugin.json` + module), wrapper and core gateway plugins, a marketplace schema [S16][S17] |
| Bifrost | Go and WASM middleware plugins; its docs home lists custom plugins as open source [S23], its README as enterprise [S19] |
| plugrelay | eight extension points (`pkg/plugin/plugin.go`), written three ways: a YAML manifest for upstreams (108 ship in [`manifests/`](../manifests)), an external process over JSON-RPC/stdio with a Go SDK, or Go compiled in. Plugins add no console pages; no marketplace, WASM or signed packages, by decision ([plugins](plugins.md)) |

## Storage and clustering

| | business data | shared runtime state / clustering |
|-|-|-|
| new-api | SQLite, MySQL ≥ 5.7.8, PostgreSQL ≥ 9.6; separate log DB incl. ClickHouse [S1] | Redis for shared rate limits; nodes share the main DB and secrets [S1] |
| one-api | SQLite, MySQL; PostgreSQL "适配中" (being adapted) [S5] | shared MySQL + Redis + `SYNC_FREQUENCY` polling [S5] |
| sub2api | PostgreSQL 15+ required [S6] | Redis 7+ required [S6] |
| LiteLLM | PostgreSQL for keys, teams, budgets, usage [S15] | Redis; Terraform modules [S9] |
| claude-code-router | SQLite (`packages/core/src/storage/sqlite-native.ts`) [S17] | n/a (local) |
| Bifrost | config and log stores [S19] | clustering is enterprise [S19][S23] |
| plugrelay | SQLite (default), PostgreSQL or MySQL 8.0.13+ (`internal/store`); YAML export/apply | memory, SQL or Redis (`internal/state`); routing health shared across nodes and shown live in the console |

## Billing

| | unit | payments |
|-|-|-|
| new-api | quota and subscriptions with expression-based pricing [S1] | Stripe, EPay, Creem, Waffo top-ups and subscriptions [S4] |
| one-api | points: quota = group ratio × model ratio × (prompt + completion × completion ratio); optional USD display [S5] | top-up link only [S5] |
| sub2api | token-level cost [S6] | EasyPay, Alipay, WeChat Pay, Stripe [S6] |
| LiteLLM | USD spend per key, user, team, with budgets [S9][S15] | none in the proxy |
| claude-code-router | estimated cost per request, local limits [S16] | none |
| plugrelay | real money: per million tokens in the site currency, per image, audio second or 1k characters for media; every attempt sent upstream is costed, by default only the delivered one is charged; key, user and group budgets | billing feature, off by default: prepaid balance that never overdraws under concurrency, redeem codes, Stripe and epay (Alipay, WeChat Pay) top-ups, subscription plans, coupons, check-in and referral rewards, self sign-up, and a new-api / one-api importer that keeps keys working. Stripe and epay tested against fakes only ([sell kit](sell-kit.md)) |

## Console

| | console |
|-|-|
| new-api | React; usage dashboard, plugin marketplace, model square, playground; seven languages [S21] |
| sub2api | Vue admin dashboard; one-click self-update with rollback; iframe embedding of external systems [S6] |
| LiteLLM | admin UI with keys, teams, budgets, guardrails, request logs [S9] |
| claude-code-router | desktop app (macOS, Windows, Linux) plus browser UI [S16] |
| Portkey | local log console [S18] |
| plugrelay | built into the binary; a setup wizard that turns features on by use case; one switch and one form per feature; provider gallery; request log with every upstream attempt, sessions and CSV export; usage analytics; playground; public pricing and status pages; billing pages; live credential state; SSO (OIDC, GitHub, Feishu/Lark), TOTP, passkeys; English and Simplified Chinese; calls only the public admin API; `plugrelay demo` for a local copy with sample data |

## Deployment and license

| | deployment | license |
|-|-|-|
| new-api | Docker, Compose (with PostgreSQL + Redis), Electron desktop wrapper [S1] | AGPL-3.0 with attribution terms; modified UIs must keep a visible link [S1] |
| sub2api | install script + systemd, Compose, Apple container; needs PostgreSQL and Redis [S6] | LGPL-3.0 [S0] |
| LiteLLM | pip/uv, Docker, Terraform [S9] | MIT outside `enterprise/` [S10]; SSO free for up to 5 users since v1.76.0, then an enterprise license [S22] |
| claude-code-router | desktop installers, npm CLI, Docker [S16] | MIT [S0] |
| plugrelay | one binary with the console built in, Docker image, Compose ([deployment](deployment.md)); with SQLite it needs nothing else | Apache-2.0 |

## What plugrelay matches

Console features the others have are here too: provider gallery, the client
behind each request and spend by client, sessions and CSV export, a CC Switch
link, notifications (email, Feishu/Lark, DingTalk, WeCom, webhook), presets,
price sync from models.dev, a playground, public pricing and status pages,
and orders, coupons, check-in, referrals and announcements for selling. None
has seen production traffic yet.

## Where plugrelay is ahead

Only where code or a test shows it; maturity is not one of them.

- **Private reasoning state across protocols**, returned only to its issuer
  ([table](#private-reasoning-state)). sub2api cannot mix families in one
  conversation [S6]; LiteLLM's fix covers Responses item ids [S12].
- **Graded conversion.** Endpoints that cannot serve a request are never
  routed (`TestAssessMatrix`); new-api warns fields "may not map exactly" [S1].
- **Same-protocol traffic untouched**, byte for byte
  (`TestPassthroughByteExact`); the others don't document theirs.
- **Leaving new-api.** The importer reads a new-api / one-api database and
  keeps its `sk-` keys working ([sell kit](sell-kit.md#import-from-new-api-adminimport-newapi)).

## Where others are ahead

- **Maturity.** Every project above has many releases and production users
  [S0][S9]; plugrelay is at its first release, with none reported.
- **Provider breadth.** LiteLLM covers 100+ providers [S9], Portkey 1,600+
  models [S18], Bifrost 23+ providers [S19]. plugrelay's 108 manifests are
  about LiteLLM's count, but several are regional editions of one vendor
  (Kimi, GLM, MiniMax, Qwen) and few have met a live account.
- **Payments.** new-api has Stripe, EPay, Creem and Waffo [S4]; sub2api
  EasyPay, Alipay, WeChat Pay and Stripe [S6]. plugrelay has Stripe and epay
  only, and sign-up has no email verification.
- **Modalities.** new-api serves realtime [S1], LiteLLM batches and A2A [S9].
  plugrelay sends images, audio and batches only to a same-protocol upstream,
  relays realtime only to OpenAI and Azure, has no Gemini Live, and has no
  codec for Voyage, Mixedbread, NVIDIA, DashScope or DeepInfra rerank.
- **Guardrails and MCP.** LiteLLM and Portkey have guardrail frameworks and
  MCP gateways [S9][S18]; Bifrost's docs home marks guardrails enterprise and
  only "MCP Tool Filtering" open source [S23], its README lists both as
  enterprise [S19]. plugrelay has built-in checks and an MCP gateway, but no
  third-party guardrails, no stdio MCP servers and no OAuth to MCP servers.
- **Client compatibility evidence.** LiteLLM publishes a daily Claude Code
  matrix per provider [S13]; plugrelay's dated runs use one upstream.
- **Databases.** new-api runs on MySQL ≥ 5.7.8 and keeps logs in a separate
  database, ClickHouse included [S1]; plugrelay needs MySQL 8.0.13+
  and has no separate log database.
- **Console languages.** new-api has seven [S21]; plugrelay two, by decision.
- **Marketplaces.** new-api has a plugin marketplace [S1], claude-code-router
  a marketplace schema [S16][S17]; plugrelay has neither, by decision.
- **Subscription accounts.** sub2api and claude-code-router pool OAuth
  subscription accounts [S6][S16]. plugrelay refuses to
  ([non-goals](../README.md#non-goals)); for operators who want it, a gap.

## Known gaps in plugrelay

What exists but has not met the real service:

| area | verified against |
|-|-|
| AWS Bedrock, Google Vertex AI | fakes and published test vectors only; their manifests list no models or prices |
| Upstream quota display | fakes; only the OpenRouter manifest declares a quota probe, not yet run on a live account |
| Embeddings, rerank, transcription | live only against self-hosted TEI and whisper.cpp ([run](compat/2026-11-06-live.md)); no commercial API. TEI reports no rerank usage, so the log bills an estimate |
| Speech, batches | fakes only; the live test upstream answers 404 on these routes |
| Realtime / Responses WebSocket | fake WebSocket upstreams only; a live run needs a paid OpenAI or Azure account. Audio tokens are priced at the model's token rates |
| Stripe, epay | fake servers only; plan changes are not prorated |
| Sign-up and login captcha | Turnstile and hCaptcha live with their test keys; outages against fakes |
| Passkeys | a software authenticator only, not real browsers and devices |
| MCP gateway | live against the official reference server, TypeScript SDK client and Claude Code on protocol 2025-11-25; the 2026-07-28 revision, HTTP+SSE fallback and progress streaming against fakes |
| Guardrails | fake moderation servers only |
| MySQL, MariaDB | CI runs MySQL 8.4 on a fresh database; MariaDB 10.6 by hand |
| Live protocol suite | one `TestRelay/openai_chat*` failure in about nine full runs, not reproduced |

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
