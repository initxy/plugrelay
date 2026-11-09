# Connecting clients

English | [简体中文](clients.zh-CN.md)

Every client changes its base URL and uses a platform key (`pr_…`) from
`plugrelay key create` or the console's Keys page, which also shows a connect
snippet per client. Examples use `https://relay.example.com`; a local relay is
`http://127.0.0.1:8787`. Dated runs of real Claude Code and Codex through a
relay: [compat/](compat/README.md).

- [What the relay serves](#what-the-relay-serves)
- [Claude Code](#claude-code) · [Codex](#codex) · [Gemini CLI](#gemini-cli) ·
  [Cursor](#cursor) · [GitHub Copilot (VS Code)](#github-copilot-vs-code) ·
  [Cline / Roo Code / Kilo Code](#cline--roo-code--kilo-code) ·
  [OpenCode](#opencode) · [Cherry Studio / Continue / JetBrains](#cherry-studio--continue--jetbrains) ·
  [Trae](#trae)
- [Playground](#playground)
- [Batches](#batches)
- [Realtime and WebSocket](#realtime-and-websocket)
- [Native passthrough](#native-passthrough)
- [MCP servers](#mcp-servers)
- [What every client loses behind a relay](#what-every-client-loses-behind-a-relay)

## What the relay serves

| route | protocol | used by |
|-|-|-|
| `POST /v1/messages`, `POST /v1/messages/count_tokens` | `anthropic` | Claude Code, Cline, Copilot, … |
| `HEAD`/`GET /api/hello` | `anthropic` | Claude Code start-up probe; answered locally |
| `POST /v1/responses`, `/v1/responses/compact`, `/v1/responses/input_tokens` | `openai-responses` | Codex, Copilot, OpenCode, Continue |
| `POST /v1/chat/completions` | `openai-chat` | Cursor, Cline, OpenCode, Cherry Studio, … |
| `POST /v1/completions` | `openai-completions` | Continue and JetBrains code completion |
| `POST /v1beta/models/{model}:generateContent` / `:streamGenerateContent` / `:countTokens` | `gemini` | Gemini CLI, Kilo, Cherry Studio |
| `POST /v1/embeddings` | `openai-embeddings` | OpenAI SDKs, RAG tools, Cherry Studio |
| `POST /v1beta/models/{model}:embedContent` / `:batchEmbedContents` | `gemini-embeddings` | Google GenAI SDKs |
| `POST /v1/images/generations`, `/v1/images/edits` (multipart) | `openai-images` | OpenAI SDKs, image tools |
| `POST /v1/audio/transcriptions`, `/v1/audio/translations` (multipart), `/v1/audio/speech` | `openai-audio` | OpenAI SDKs, voice tools |
| `POST /v1/rerank` | `rerank` (Jina, vLLM, Cohere v1 shape) | RAG frameworks, Jina/Cohere SDKs |
| `POST /v2/rerank` | `cohere-rerank` | Cohere SDKs (v2) |
| `/v1/files`, `/v1/batches`, `/v1/messages/batches` | none: [batches](#batches) | OpenAI and Anthropic SDKs' batch APIs |
| WebSocket `GET /v1/realtime?model=…` | OpenAI Realtime: [realtime](#realtime-and-websocket) | OpenAI Realtime SDKs, voice agents |
| WebSocket `GET /v1/responses` | Responses WebSocket mode, or `426`: [realtime](#realtime-and-websocket) | Codex with WebSocket on |
| `GET /v1/models`, `GET /v1beta/models` | all | almost every client |
| `POST`/`DELETE` `/mcp`, `/mcp/<server>` | MCP Streamable HTTP | MCP clients: [MCP servers](#mcp-servers) |
| any method on `/native/<upstream>/<path>` | none, forwarded verbatim | other vendor APIs; off by default: [native passthrough](#native-passthrough) |

Routes come from each codec's `Routes()` (`pkg/protocol/<name>`). Every route
also answers without the `/v1` or `/v1beta` prefix, with a trailing slash, and
with Azure's `?api-version=` (`internal/server/routes.go`).

| | |
|-|-|
| Keys | `Authorization: Bearer`, `x-api-key`, `x-goog-api-key` or `?key=` (`plugins/auth/apikey`) |
| Model lists | Codex's catalog with `?client_version=`, Anthropic's with an `anthropic-version` header, Gemini's on `/v1beta/models`, otherwise OpenAI's. Only models the key may use |
| Errors | the relay's own errors use the client protocol's format and signals (context too long, `retry-after`, quota exhausted, overloaded); same-protocol upstream errors pass through verbatim |
| Model names | never swapped. For cross-model fallback an admin defines a [preset](configuration.md#routing-models-and-presets), which clients pick by name |

**Media** (embeddings, images, audio, rerank) shares keys, routing, retries,
logs and billing with chat. Only endpoints declaring the operation
(`embeddings`, `image_generation`, `image_edit`, `audio_transcription`,
`speech`, `rerank`) serve it; otherwise 400 `capability_missing`.

- Embeddings convert between OpenAI and Gemini (`dimensions` ⇄
  `outputDimensionality`; base64 made by the relay when needed). Token-id
  input only reaches an OpenAI-style endpoint.
- Images and audio are same-protocol only.
- Rerank converts between `/v1/rerank`, Cohere v2 `/v2/rerank` (strings only)
  and Hugging Face TEI's `/rerank` (upstream only). When the target lacks
  `top_n` or `return_documents`, the relay applies them itself. Non-text
  documents are same-protocol only. Usage is the upstream's tokens or search
  units; TEI reports none, so tokens are estimated.
- Uploads (`/v1/images/edits`, `/v1/audio/transcriptions`,
  `/v1/audio/translations`) stream to the upstream, capped by
  `limits.max_body_bytes` (413); compressed uploads get 415. If `model` comes
  after the file, the body is first copied to a temp file. A streamed upload
  is not retried on another upstream once it began sending; a copied one is.

## Claude Code

Anthropic messages, sent from your machine.

```bash
plugrelay setup claude-code --key pr_xxx --url https://relay.example.com
```

writes into `~/.claude/settings.json` (old file kept as `.bak-<time>`):

```json
{"env": {"ANTHROPIC_BASE_URL": "https://relay.example.com",
         "ANTHROPIC_AUTH_TOKEN": "pr_xxx",
         "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"}}
```

`--model M[,small]` also sets `ANTHROPIC_MODEL` and
`ANTHROPIC_DEFAULT_HAIKU_MODEL` (the second name, or `M` again). By hand,
export the same variables; `ANTHROPIC_API_KEY` also works.

- `anthropic-beta` and `anthropic-version` are forwarded verbatim. An upstream
  that rejects a beta drops it in its manifest (`unsupported_betas`).
- On an Anthropic upstream the body passes byte-exact (`system`,
  `cache_control` including `ttl: 1h`, `thinking`, tools), so prompt-cache
  hits match a direct connection.
- "prompt is too long" errors are kept, so auto-compaction triggers.
- On other upstreams thinking signatures are sealed and returned only to the
  upstream that issued them: no "Invalid signature in thinking block".
- With model discovery on, Claude Code lists only ids containing `claude` or
  `anthropic`; set others with `ANTHROPIC_MODEL`.

### CC Switch

Right after a key is created or rotated, the console's connect panel offers an
"Import to CC Switch" link (`ccswitch://v1/import`) for Claude Code, Codex and
Gemini CLI. It carries the relay's name and address, the key and the same
settings as the snippets (`internal/admin/connect.go`).

## Codex

OpenAI responses only, sent from your machine.

```bash
plugrelay setup codex --key pr_xxx --model gpt-5.5 --url https://relay.example.com
export PLUGRELAY_API_KEY=pr_xxx   # Codex reads the key from here
```

saves the relay's model catalog as `~/.codex/plugrelay-models.json` and adds
and selects a provider in `~/.codex/config.toml`:

```toml
model_provider = "plugrelay"
model_catalog_json = "/home/you/.codex/plugrelay-models.json"
model = "gpt-5.5"

[model_providers.plugrelay]
name = "plugrelay"
base_url = "https://relay.example.com/v1"
env_key = "PLUGRELAY_API_KEY"
wire_api = "responses"
```

- Use a custom provider, not `openai_base_url`. With the latter Codex treats
  the relay as OpenAI and turns on WebSocket ([below](#realtime-and-websocket)),
  remote compaction and request compression.
- Codex reads model metadata only from the `model_catalog_json` file; without
  it you get "Model metadata … not found" and generic defaults (no freeform
  `apply_patch`, default context window). The file is a snapshot: re-run
  `setup codex` after adding models. Setup fails when the key sees no model,
  since Codex will not start with an empty catalog.
- Streams always end with `response.completed` carrying usage, which
  auto-compaction needs.
- The relay returns a signed `x-codex-turn-state` header; Codex echoes it, so
  one turn stays on one credential and keeps its prompt cache.

## Gemini CLI

Gemini `generateContent`. There is no config file to edit, so setup prints
the environment:

```bash
eval "$(plugrelay setup gemini --key pr_xxx --url https://relay.example.com --print)"
# export GOOGLE_GEMINI_BASE_URL=https://relay.example.com
# export GEMINI_API_KEY=pr_xxx
```

`thoughtSignature` is kept. 429s carry Google-style `RetryInfo`, and an
exhausted daily quota is reported so the client stops retrying.

## Cursor

OpenAI chat. **Requests come from Cursor's servers**, so the relay needs a
public HTTPS URL (site setting `site.public_base_url`); localhost and private
networks don't work. In Cursor settings enable *Override OpenAI Base URL*,
enter `https://relay.example.com/v1` and the key, and add the model names.
Tab completion always uses Cursor's own models.

## GitHub Copilot (VS Code)

Add a *Custom Endpoint* in `chatLanguageModels.json` pointing at the relay
with the key. Copilot uses chat, responses or messages per model and needs
`/v1/models`; models without tool support are not offered in Agent mode.
Inline completions never go through the relay.

## Cline / Roo Code / Kilo Code

Choose an OpenAI-compatible provider (base URL `https://relay.example.com/v1`)
or an Anthropic one (`https://relay.example.com`) and set the key. Kilo also
speaks responses and Gemini. `max_tokens` / `max_completion_tokens`, Roo's
`cache_control` in OpenAI messages and `reasoning_content` in history (needed
by DeepSeek-style upstreams) are all handled.

## OpenCode

```bash
plugrelay setup opencode --key pr_xxx --model claude-sonnet-4-5,gpt-5.5 --url https://relay.example.com
```

adds a `plugrelay` provider (`@ai-sdk/openai-compatible`, `…/v1`) with those
models to `~/.config/opencode/opencode.json`; pick them with `/models`.
`--model` is required because OpenCode only offers models listed there.

## Cherry Studio / Continue / JetBrains

Add an OpenAI-compatible provider with base URL `https://relay.example.com`
(with or without `/v1`) and the key.

Continue's and JetBrains' code completion use `/v1/completions`; point the
autocomplete model at the same URL.

- An upstream with a completions endpoint (OpenAI, DeepSeek, vLLM, Ollama,
  OpenRouter and most OpenAI-compatible manifests) gets the request byte for
  byte.
- A request with a `suffix` (fill-in-the-middle) only goes to an endpoint
  declaring `fill_in_middle` (OpenAI, DeepSeek, Mistral, Ollama,
  SiliconFlow). With none, the client gets a 400 naming it rather than a
  completion that ignores the code after the cursor.
- Without a suffix, a chat-only upstream can serve it as one user message
  (lossy). Prompt batches, token-id prompts, `n`/`best_of` > 1 and `echo`
  need a completions endpoint.

## Trae

OpenAI chat or Anthropic messages. Trae uses the URL as entered, so give the
full path: `https://relay.example.com/v1/chat/completions`. It is not
confirmed whether requests come from your machine or Trae's servers; if a
local relay fails, use a public URL as for Cursor.

## Playground

The console's playground (`POST /api/v1/playground`) sends a conversation in
any of the four chat protocols with one of your keys (admins may use none).
It takes the normal request path — limits, routing, billing, the request log
— with User-Agent `plugrelay-playground/1`.

## Batches

The OpenAI Batch API (`/v1/files` with `purpose=batch`, `/v1/batches`) and
Anthropic Message Batches (`/v1/messages/batches`, including `…/cancel` and
`…/results`) work with an SDK pointed at the relay (`internal/pipeline/batch.go`).

- **Placement.** A batch is routed by its model (OpenAI: the first line's
  `body.model`; Anthropic: `params.model`) to an endpoint declaring `batches`
  (the official `openai` and `anthropic` manifests). The relay records which
  credential holds each file and batch, and every later call goes there even
  if routing changes. Same protocol only.
- **One model per batch.** Every line must name the same model (and, for
  OpenAI, the same `url`), else 400. Lines pass byte for byte except for a
  model-name rewrite. Uploads stream, up to 512 MiB (`DefaultBatchMaxBody`);
  a bad line aborts the upload.
- **Ids and access.** Ids are the upstream's. Only the creator (the user, or
  the key when it has no user) sees an object; others get 404. A message
  batch's `results_url` points at the relay, so SDKs never send the platform
  key to the vendor.
- **Billing.** Each successful line's usage is billed once to the creator at
  the price rule's `batch_discount` ([prices](configuration.md#prices)),
  when the results are first read whole — by the client, or by the relay,
  which fetches results of batches that ended over ten minutes ago or are
  about to be deleted. With `meter.balance` creating a batch holds its
  estimate ([sell kit](sell-kit.md#prepaid-balance-meterbalance)).
- **Request plugins.** Creating a batch counts as one request of its model:
  access lists, blocked models and rate limits apply. Guardrails check every
  line (moderation: the first 100) and one refusal refuses the batch, naming
  the line. `middleware.redact`, guardrails `redact` and
  `middleware.param-policy` edit each line. Plugins that only rewrite the
  decoded request refuse the batch; attempt hooks don't run. Reading,
  listing, cancelling and deleting run no plugins.

## Realtime and WebSocket

Two WebSocket APIs are relayed message for message to an upstream WebSocket
(`internal/pipeline/websocket.go`). Gemini Live is not served.

| | OpenAI Realtime | Responses WebSocket mode |
|-|-|-|
| URL | `wss://relay.example.com/v1/realtime?model=…` ([guide](https://developers.openai.com/api/docs/guides/voice-websockets)) | `wss://relay.example.com/v1/responses` ([guide](https://developers.openai.com/api/docs/guides/websocket-mode)) |
| Upstream | endpoints declaring `realtime`: `openai`, `azure-openai` | endpoints declaring `responses_websocket`: official OpenAI only |
| Model | from the URL, routed as usual | from the first `response.create`. Since Codex falls back to HTTP only on a `426` handshake, the relay accepts the upgrade only when one such upstream serves every model the key may use; otherwise `426` |
| Counted as a request | the handshake, and each client event with text | each `response.create` |

- **Keys**: the usual headers, `?key=`, or for browsers the subprotocol
  `openai-insecure-api-key.<key>`. It is never forwarded. Give browsers a key
  you are fine exposing.
- **Handshake**: the upstream is dialed through the core egress before the
  client's upgrade is accepted, so routing and upstream handshake errors
  reach the client as normal HTTP errors. The credential's concurrency slot is
  held for the whole session.
- **Plugins**: access lists, `blocked_models` and `middleware.ratelimit` run
  at the handshake and on each counted request; refusals inside a session
  are `error` events. `middleware.param-policy` caps output tokens and
  `reasoning.effort` in `response.create` and Realtime `session.update`.
- **Content checks** ([guardrails](plugins.md#guardrails),
  `middleware.redact`) read the text of each client event on its own and the
  output text and transcript deltas. A blocked client event gets an `error`
  event and the session stays open; a blocked output stops that response.
  Audio is never checked; with `block_unscannable_audio` audio input is
  refused instead. Guardrails moderation covers at most
  `max_moderated_events` (200) events per Realtime session.
- **Events**: a client event must be one JSON object with no repeated key.
  A Realtime `session.update` may not switch the model, and a transcription
  model it names must pass the same checks as a request model. `event_id`s
  starting `event_relay_guard_` are reserved.
- **Billing**: each response is held before it runs (its output cap, else
  [`websocket.response_reserve_tokens`](configuration.md#websocket), plus an
  input estimate) and settled from its `usage` as its own log entry. Realtime
  audio, text and image tokens are not priced apart. Input audio transcription
  is billed as a request of the transcription model. A response still running
  when the session ends is billed from an estimate (`estimated` usage).
  Out of balance mid-session: on Realtime the client gets
  `insufficient_quota` after the running response and the session closes
  with `1008`; on Responses that `response.create` gets an `error` event.
- **Logged**: one entry per session (protocol `openai-realtime` or
  `openai-responses-ws`, status `101`) plus one per response and
  transcription.
- **Limits** ([configuration](configuration.md#websocket)):
  `websocket.max_session`, `websocket.idle_timeout`,
  `websocket.session_ready_timeout` and `websocket.max_message_bytes`
  (`1009`). Closes pass through with their code; a lost upstream closes the
  client with `1014`, a relay shutdown with `1001`.

## Native passthrough

`/native/<upstream>/<path>` forwards any method, path, query and body to one
upstream, for APIs plugrelay has no codec for: non-batch files, fine-tuning,
vendor-only endpoints. It is off until `native.enabled` is set and the key is
granted the upstream ([configuration](configuration.md#native)): 404 while
off, 403 without the grant.

- **Addressing**: `<path>` is appended to the base URL of the upstream's
  first endpoint. An SDK pointed at `https://relay.example.com/native/openai`
  calling `…/files` reaches `https://api.openai.com/v1/files`.
  `<upstream>:<endpoint>` picks another endpoint
  (`/native/deepseek:completions/…`).
- **Credentials**: the platform key (any usual header, or `?key=`) is never
  forwarded; the relay picks a usable credential of that upstream (429 or 503
  when none) and the provider adds its auth. Core egress rules still hold.
- **Grant with care**: it hands the caller everything the credential can do
  on that host — for OpenAI the whole account API, for Vertex a
  `cloud-platform` token. Grant it only to callers you would give the
  credential. `.` and `..` path segments get 400.
- **Not applied**: request plugins (rate limits, quotas, redaction,
  guardrails) and billing; failures don't affect credential health. The
  `middleware.access` IP and client allowlists still apply. Bodies stream
  both ways up to `native.max_body_bytes`.
- **Logged** with protocol `native`, tier `passthrough`, status, bytes and
  duration.

## MCP servers

The relay's [MCP gateway](mcp.md) is one Streamable HTTP server per key:
`https://relay.example.com/mcp` offers every granted tool as
`<server>__<tool>`; `/mcp/<server>` offers one server. The servers'
credentials stay on the relay. `plugrelay setup` does not register it
(Claude Code rewrites `~/.claude.json` itself), so add it by hand.

Claude Code ([docs](https://code.claude.com/docs/en/mcp), checked
2026-11-06):

```bash
claude mcp add --transport http plugrelay https://relay.example.com/mcp \
  --header "Authorization: Bearer pr_xxx"
```

or per project in `.mcp.json` (`${VAR}` comes from the environment):

```json
{"mcpServers": {"plugrelay": {"type": "http", "url": "https://relay.example.com/mcp",
                              "headers": {"Authorization": "Bearer ${PLUGRELAY_API_KEY}"}}}}
```

Tools show up as `mcp__plugrelay__<server>__<tool>`.

Codex ([docs](https://developers.openai.com/codex/mcp), checked
2026-11-06), in `~/.codex/config.toml`:

```toml
[mcp_servers.plugrelay]
url = "https://relay.example.com/mcp"
bearer_token_env_var = "PLUGRELAY_API_KEY"
```

Other clients take the same URL and `Authorization` header.

## What every client loses behind a relay

- Features tied to the vendor's endpoint: Cursor Tab, Copilot inline
  completion, Zed edit prediction, Claude Code's fine-grained tool streaming
  and prompt-caching scope features.
- Codex over a custom provider: no WebSocket, remote compaction or web-search
  extensions; local compaction works.
- A capability no upstream has and no conversion can add (images to a
  text-only endpoint, a forced server tool nobody executes) gets a 400 naming
  it. Server tools that are only offered (Codex's `web_search`) are removed and
  logged as dropped.
- Over OpenAI chat, private state from another protocol can't be carried, so
  reasoning continuity across turns is lost; the request log says so.
