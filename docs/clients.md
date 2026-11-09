# Connecting clients

English | [简体中文](clients.zh-CN.md)

Every client changes one base URL and uses a platform key (`pr_…`) from
`plugrelay key create` or the console's Keys page (which also shows a ready
connect snippet per client). Examples below use `https://relay.example.com`;
for a local relay use `http://127.0.0.1:8787`. Dated results of real Claude
Code and Codex runs through a relay: [compat/](compat/README.md).

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
| WebSocket `GET /v1/realtime?model=…` | OpenAI Realtime, relayed: [realtime](#realtime-and-websocket) | OpenAI Realtime SDKs, voice agents |
| WebSocket `GET /v1/responses` | Responses WebSocket mode, relayed or `426`: [realtime](#realtime-and-websocket) | Codex with WebSocket on |
| `GET /v1/models`, `GET /v1beta/models` | all | almost every client |
| `POST`/`DELETE` `/mcp`, `/mcp/<server>` | MCP Streamable HTTP | MCP clients (Claude Code, Codex, Cursor…); see [MCP servers](#mcp-servers) |
| any method on `/native/<upstream>/<path>` | none: forwarded verbatim | SDKs calling other files, fine-tuning or vendor-only APIs; off by default, see [native passthrough](#native-passthrough) |

Routes come from the codecs (`Routes()` in each `pkg/protocol/<name>`); the
server (`internal/server/routes.go`) is tolerant of the usual URL mistakes: the
same route answers with or without the `/v1` or `/v1beta` prefix, with a
trailing slash, and with Azure-style `?api-version=`.

- **Keys** are accepted from `Authorization: Bearer`, `x-api-key`,
  `x-goog-api-key` or `?key=` (`plugins/auth/apikey`).
- **Model lists** are rendered in the format the request asks for: Codex's
  catalog when `?client_version=` is present, Anthropic's when an
  `anthropic-version` header is sent, Gemini's on `/v1beta/models`, otherwise
  OpenAI's `{"object":"list","data":[…]}`. Only models the key may use are
  listed.
- **Errors** the relay produces itself use the client protocol's native
  format and signals (context too long, rate limited with `retry-after`,
  quota exhausted, overloaded), so clients compact, back off or stop as they
  would against the vendor. Same-protocol upstream errors pass through
  verbatim.
- **Model names** are chosen by the client and never swapped. For automatic
  cross-model fallback, an admin defines a
  [preset](configuration.md#routing-models-and-presets), which clients see in
  their model list and select explicitly.
- **WebSocket** upgrades are served on `/v1/realtime` and `/v1/responses`
  only ([realtime](#realtime-and-websocket)); when the Responses mode cannot
  be relayed the upgrade gets `426`, and clients fall back to HTTP streaming.
- **Media operations** (embeddings, images, audio, rerank) go through the same keys,
  routing, retries, request log and billing as chat. A model is served only
  by endpoints declaring the operation (`embeddings`, `image_generation`,
  `image_edit`, `audio_transcription`, `speech`, `rerank`); otherwise the client gets
  400 `capability_missing`. Embeddings convert between OpenAI and Gemini
  (several inputs ⇄ one batch, `dimensions` ⇄ `outputDimensionality`, base64
  produced by the relay when the upstream returns floats; token-id input
  only reaches an OpenAI-style endpoint). Images and audio are only passed
  through to the same protocol.
- **Rerank** (`rerank` capability) converts between the common shape
  (`/v1/rerank`: documents as strings or `{text, …}` objects,
  `return_documents`), Cohere's v2 (`/v2/rerank`: strings only, no documents
  in the results) and Hugging Face TEI's `/rerank` (`{query, texts}` →
  `[{index, score}]`, an upstream-only protocol). Going to a protocol that
  lacks them, the relay keeps the best `top_n` and fills in the documents
  from the request; object documents reach a string-only upstream as their
  text. Image or other non-text documents only pass through to the same
  protocol. Usage is the upstream's tokens or search units (Cohere's
  `meta.billed_units`, or `usage.search_units` as OpenRouter and Portkey
  send); TEI reports none, so tokens are estimated.
- **Uploads** (`/v1/images/edits`, `/v1/audio/transcriptions`,
  `/v1/audio/translations`) are streamed to the upstream as they arrive,
  bounded by `limits.max_body_bytes` (413 above it); compressed uploads get
  415. When the `model` field comes after the file, the relay first copies
  the body to a temporary file so it can read the model. A streamed upload
  is not retried on another upstream once it began sending; a copied one
  is. Speech audio and event-stream responses are copied to the client as
  they arrive.

## Claude Code

Protocol: Anthropic messages. Requests come from your machine.

```bash
plugrelay setup claude-code --key pr_xxx --url https://relay.example.com
```

writes into `~/.claude/settings.json` (backup kept as `.bak-<time>`):

```json
{"env": {"ANTHROPIC_BASE_URL": "https://relay.example.com",
         "ANTHROPIC_AUTH_TOKEN": "pr_xxx",
         "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"}}
```

`--model M` also sets `ANTHROPIC_MODEL`. By hand, export the same variables
(`ANTHROPIC_API_KEY` works too; it is sent as `x-api-key`).

- `anthropic-beta` and `anthropic-version` are forwarded verbatim on same
  protocol — no allowlist to fall behind. Upstreams that reject a beta drop
  it through their manifest (`unsupported_betas`).
- The `system` array, `cache_control` (including `ttl: 1h`), `thinking`,
  `context_management` and tool fields pass byte-exact on an Anthropic
  upstream, so prompt-cache hit rates match a direct connection.
- "prompt is too long: N tokens > M maximum" is preserved, which triggers
  auto-compaction.
- On a non-Anthropic upstream thinking signatures are sealed and restored
  only for the issuing upstream; no "Invalid signature in thinking block".
- Claude Code only lists models whose id contains `claude` or `anthropic`
  when model discovery is on. Pick others with `ANTHROPIC_MODEL` (and the
  small/fast model variables).

### CC Switch

The console's connect panel offers an "Import to CC Switch" link for Claude
Code, Codex and Gemini CLI right after a key is created or rotated (CC Switch
needs the full key). It is CC Switch's `ccswitch://v1/import` provider link:
the relay's site name and address, the key, `apiFormat` (`anthropic`,
`openai_responses` with `/v1`, `gemini_native`) and, for Claude Code and
Codex, the same settings as the snippets (`internal/admin/connect.go`,
`TestCCSwitchLinks`).

## Codex

Protocol: OpenAI responses only. Requests come from your machine.

```bash
plugrelay setup codex --key pr_xxx --model gpt-5.5 --url https://relay.example.com
export PLUGRELAY_API_KEY=pr_xxx   # Codex reads the key from here
```

saves the relay's model catalog as `~/.codex/plugrelay-models.json`, adds a
provider to `~/.codex/config.toml` and selects it:

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

- Use a custom provider like this one, not `openai_base_url`: with the
  latter Codex treats the relay as OpenAI and turns on WebSocket (relayed
  only in the case [below](#realtime-and-websocket), else `426` and HTTP),
  remote compaction and request compression.
- Streams always end with `response.completed` carrying usage, which Codex
  needs for auto-compaction; a stream cut is reported as `response.failed`.
- New input item types (`custom_tool_call`, `tool_search_call`, namespaces,
  compaction) pass verbatim on same protocol; `reasoning.encrypted_content`
  carries sealed private state on other upstreams.
- The relay returns a signed `x-codex-turn-state` header; Codex echoes it
  within a turn, which pins the turn to one credential (prompt cache).
- Codex does not fetch model metadata from a custom provider; it reads it
  from the local file named by `model_catalog_json`. Without it Codex warns
  "Model metadata … not found" and falls back to generic settings (no
  freeform `apply_patch`, default context window). `setup codex` downloads
  the catalog from `GET /v1/models?client_version=…` (Codex's own format, with
  ETag); the file is a snapshot, so re-run `setup codex` after adding models.
  Codex will not start with an empty catalog, so setup fails when the key
  sees no model.
  Each entry carries a vendor-neutral `base_instructions` system prompt, since
  the model behind a slug may be any vendor's.

## Gemini CLI

Protocol: Gemini `generateContent`. Gemini CLI has no config file plugrelay
edits; `setup` prints the environment:

```bash
eval "$(plugrelay setup gemini --key pr_xxx --url https://relay.example.com --print)"
# export GOOGLE_GEMINI_BASE_URL=https://relay.example.com
# export GEMINI_API_KEY=pr_xxx
```

- The key arrives as `x-goog-api-key`.
- `thoughtSignature` on parts is kept; the last stream chunk carries
  `finishReason`.
- 429s carry Google-style `RetryInfo`; an exhausted daily quota is reported so
  the client stops retrying.

## Cursor

Protocol: OpenAI chat. **Requests come from Cursor's servers**, so the relay
needs a public HTTPS URL (site setting `site.public_base_url`); localhost and private networks do not
work. In Cursor settings, enable *Override OpenAI Base URL*, enter
`https://relay.example.com/v1` and the platform key, and add the model names.

- Tab completion always uses Cursor's own models.
- Agent mode may use the responses API; both are served.

## GitHub Copilot (VS Code)

Uses chat, responses or messages depending on the model. Add a *Custom
Endpoint* in `chatLanguageModels.json` pointing at the relay with the platform
key. Copilot needs `/v1/models`; models without tool support are not offered
in Agent mode. Inline completions never go through the relay.

## Cline / Roo Code / Kilo Code

OpenAI chat or Anthropic messages (Kilo also responses and Gemini). In the
extension settings choose an OpenAI-compatible or Anthropic provider, set the
base URL (`https://relay.example.com/v1` for OpenAI-compatible,
`https://relay.example.com` for Anthropic) and the key.

- Both `max_tokens` and `max_completion_tokens` are accepted and renamed for
  the upstream.
- Roo sends `cache_control` inside OpenAI-style messages; it is treated as a
  cache breakpoint and handled by the target's cache mode.
- `reasoning_content` sent back in history is kept (DeepSeek-style upstreams
  require it on tool turns).

## OpenCode

Chat, responses or messages.

```bash
plugrelay setup opencode --key pr_xxx --model claude-sonnet-4-5,gpt-5.5 --url https://relay.example.com
```

adds a `plugrelay` provider (`@ai-sdk/openai-compatible`, base URL
`…/v1`) with the listed models to `~/.config/opencode/opencode.json`; pick
them with `/models`. OpenCode only offers models listed there, so `--model` is
required. With `include_usage`, the last chunk carries usage and
`cached_tokens`; `prompt_cache_key` is forwarded.

## Cherry Studio / Continue / JetBrains

Mostly OpenAI chat; add an OpenAI-compatible provider with base URL
`https://relay.example.com` (with or without `/v1` — both work) and the key.
Continue's and JetBrains' code completion use `/v1/completions` (legacy text
completions, usually with a `suffix` for fill-in-the-middle); point the
autocomplete model at the same base URL. A model routed to an upstream with
a completions endpoint (OpenAI, Azure OpenAI, DeepSeek, Mistral, vLLM,
Ollama, LM Studio, OpenRouter and most other OpenAI-compatible manifests) is passed through
byte for byte. A request with a `suffix` only goes to an endpoint whose
manifest declares `fill_in_middle` (OpenAI, DeepSeek, Mistral, Ollama,
SiliconFlow); with none, the
client gets a 400 naming `fill_in_middle` rather than a completion that
ignores the code after the cursor. Without a suffix, a chat-only upstream can
serve it: the prompt is sent as one user message (graded lossy). Prompt
batches, token-id prompts, `n`/`best_of` above 1 and `echo` only work against
a completions endpoint. Chat clients are never routed to a completions
endpoint.

## Trae

OpenAI chat or Anthropic messages (no responses). Trae uses the URL as
entered, so give the full path: `https://relay.example.com/v1/chat/completions`.
Whether requests leave from your machine or Trae's servers is not confirmed;
if a local relay does not work, use a public URL as for Cursor.

## Playground

The console's playground (`POST /api/v1/playground`) sends a conversation as
a client would, in any of the four chat protocols, with one of the caller's
keys (admins may use none and talk as themselves). It runs the normal
request path — limits, routing, metering, billing, the request log — with
User-Agent `plugrelay-playground/1`, which the log shows as the playground
(`TestPlaygroundEveryProtocol`).

## Batches

The OpenAI Batch API (`POST /v1/files` with `purpose=batch`, `GET
/v1/files[/{id}[/content]]`, `DELETE /v1/files/{id}`, `POST`/`GET
/v1/batches`, `GET /v1/batches/{id}`, `POST /v1/batches/{id}/cancel`) and
Anthropic Message Batches (`POST`/`GET /v1/messages/batches`, `GET`/`DELETE
/v1/messages/batches/{id}`, `POST …/cancel`, `GET …/results`) work with an
SDK pointed at the relay as usual (`internal/pipeline/batch.go`).

- **Placement**: a batch lives on one upstream credential. The relay routes
  the input file (OpenAI: by the first line's `body.model`) or the message
  batch (by `params.model`) like a request of that protocol, to an endpoint
  declaring `batches` (the official `openai` and `anthropic` manifests do),
  and records which credential holds each file and batch. Every later call
  — get, cancel, results, delete — goes to that credential, even when
  routing has changed since. Same protocol only: a batch is never converted.
- **One model per batch**: every line (every request) must name the same
  model and, for OpenAI, the same `url`; otherwise 400. When the route's
  upstream model has another name, the relay rewrites `body.model` /
  `params.model`; otherwise lines pass byte for byte. Uploads stream to the
  upstream (`DefaultBatchMaxBody`, 512 MiB); a line is checked as it passes,
  so a bad line aborts the upload.
- **Ids** are the upstream's (`file-…`, `batch_…`, `msgbatch_…`). Only the
  owner sees an object — the user that created it, or the key when it has no
  user; everyone else gets 404 in the vendor's error format. Lists come from
  the relay's records of the caller's objects, refreshed from the upstream
  while not finished. A message batch's `results_url` is rewritten to the
  relay (the site's public base URL, else the address the request reached), so
  SDKs that follow it never send the platform key to the vendor.
- **Billing**: the results are billed to the batch's creator from the
  usage of each successful line, at the price rule's `batch_discount`
  ([prices](configuration.md#prices)), once: when first read whole
  through the relay (the output file's content, or `…/results`), or by the
  relay itself, which checks unbilled batches every few minutes and fetches
  the results of one that ended over ten minutes ago, or right before a
  delete would make them unreadable. A client that stops reading early does
  not stop the metering: the relay reads the rest. `Range` and conditional
  headers are not forwarded on batch calls, so a download is always whole.
  With `meter.balance`, creating a batch holds its estimate
  ([sell kit](sell-kit.md#prepaid-balance-meterbalance)); other meters
  admit the caller with a zero reservation.
- **Request plugins** run when a batch is created (uploading an input
  file, creating a batch or a message batch) as for one request of the
  batch's model: access lists, blocked models and rate limits refuse as
  usual, and a batch counts as one request. Plugins that read the request
  body (guardrails) also run on every request of the batch, and any
  refusal refuses the whole batch, naming the line. Body edits apply to
  every request as on a same-protocol call: `middleware.redact`, a
  guardrail in `redact` mode and `middleware.param-policy` caps are written
  into each line before it goes upstream. A request plugin that rewrites
  the decoded request instead refuses the batch, since lines go upstream in
  their own encoding; attempt hooks (`plugin.AttemptHook`) do not run on
  batch lines. Guardrails moderation checks at most the first 100 requests
  of a batch; past them it counts as a failed moderation call
  (`moderation_fail`). Reading, listing, cancelling and deleting run no
  request plugins.

## Realtime and WebSocket

Two WebSocket APIs are relayed message for message to an upstream WebSocket
(`internal/pipeline/websocket.go`). Gemini Live (its own WebSocket
protocol on `…/ws/…BidiGenerateContent`) is not served.

- **OpenAI Realtime**: `wss://relay.example.com/v1/realtime?model=…`, as in
  OpenAI's [WebSocket guide](https://developers.openai.com/api/docs/guides/voice-websockets)
  (GA interface; a beta client's `OpenAI-Beta: realtime=v1` header is
  forwarded unchanged, though OpenAI's migration notes say to drop it).
  The model is routed like any other to an endpoint declaring `realtime`:
  the official `openai` manifest (`wss://api.openai.com/v1/realtime`) and
  `azure-openai` (`wss://<resource>.openai.azure.com/openai/v1/realtime?model=<deployment>`,
  the GA path without `api-version`).
- **Responses WebSocket mode** (`/v1/responses`, used by Codex when it treats
  the relay as OpenAI, [guide](https://developers.openai.com/api/docs/guides/websocket-mode)):
  passed through to an endpoint declaring `responses_websocket` (official
  OpenAI only). The model arrives only with the first `response.create`,
  after the upstream must be chosen, and Codex falls back to HTTP only on a
  `426` handshake, not on a later error. So the relay accepts the upgrade
  only when one such upstream serves every model the key may use, and
  answers `426` otherwise; Codex then uses HTTP for the rest of its session.
  A `response.create` naming a model that upstream does not serve gets an
  `error` event (404) and is not forwarded.
- **Keys**: `Authorization: Bearer`, the other usual headers, `?key=`, or —
  for browsers, which cannot set headers — the subprotocol
  `openai-insecure-api-key.<key>` (any letter case). The key is removed
  from the offered subprotocols and never forwarded; the provider's
  credential is used upstream. Hand browsers a key meant to be exposed.
- **Upstream**: dialed through the core egress (the credential's proxy, the
  SSRF guard, the connect timeout, the upstream's extra headers) before the
  client's upgrade is accepted, so a malformed handshake (`Sec-WebSocket-Key`,
  version `13`), routing errors and the upstream's own handshake errors
  reach the client as HTTP errors in OpenAI's format. The upstream's
  subprotocol and response headers (`x-codex-turn-state`…) come back on the
  `101`. The credential's concurrency slot is held for the whole session.
- **Request plugins** run on a call that names the model, as for a
  [batch](#batches): a Realtime session is one request at the handshake,
  with no body (`middleware.access` lists, `blocked_models`,
  `middleware.ratelimit` with its concurrency slot held for the session and
  its tokens counted when it ends), and each client event carrying text is
  one more request of the session's model (counted toward `rpm`, no extra
  concurrency slot); in the Responses mode each `response.create` is one
  request with its body, refused with an `error` event. The access
  source-address and client lists are also checked at every handshake.
  Cookies are dropped before authentication.
- **Body edits**: in the Responses mode every body mutator edits each
  `response.create` as on a patch-tier HTTP attempt (only the paths it
  declares), so `middleware.param-policy` caps `max_output_tokens` and
  `reasoning.effort` and strips parameters, and attempt hooks
  (`plugin.AttemptHook`) run and may refuse it; headers they set are
  dropped, the connection being open already. On Realtime,
  `middleware.param-policy` lowers `max_output_tokens` /
  `max_response_output_tokens` (`inf` included) and `reasoning.effort` in
  `session.update` and `response.create` and strips its parameters there;
  when the session the upstream created allows more output than the cap,
  the relay sends the upstream a `session.update` of its own, whose
  `session.updated` the client also gets. Client events wait until the
  upstream's `session.created` and that update went out, so a lower limit
  the client sets is never overwritten; a `response.create` limit, which
  overrides the session's, is lowered only to the cap (see **Limits** for
  an upstream that never sends `session.created`). Attempt hooks do not
  run on Realtime.
- **Content checks** ([guardrails](plugins.md#guardrails),
  `middleware.redact`) read the text of client events: on Realtime,
  `conversation.item.create` (message text, transcripts the client
  supplies, function call arguments and outputs), `response.create`
  (`instructions`, `input` items, tool names) and `session.update`
  (`instructions`, tool names); on Responses, each `response.create`, read
  as an HTTP `/v1/responses` request. A blocked event gets an `error` event
  and is not forwarded; the session stays open. Redaction edits user
  message text in the event before it goes upstream; other text is checked,
  never edited, and an event with no edit is forwarded byte for byte. Each
  event is checked on its own: a keyword split across two events is not
  matched, and `max_prompt_chars` counts one event's text, not the
  conversation the upstream keeps. Guardrails moderation checks at most
  `max_moderated_events` (200) events of a Realtime session, counting only
  events whose user text it sent to moderation; past them it counts as a
  failed moderation call (`moderation_fail`). As on HTTP, the
  variables of a stored prompt (`session.prompt`, `response.prompt`,
  `prompt` on Responses) are not checked.
- **Output checks**: text and audio transcript deltas
  (`response.output_text.delta`, `response.text.delta`,
  `response.output_audio_transcript.delta`,
  `response.audio_transcript.delta`) go through the same window as HTTP
  streams. On `block`, frames wait while their text could be part of a
  match, and a match stops the response: the client gets an `error` event,
  none of the response's later frames or events about its output items,
  then its terminal event with `output` emptied. A Realtime upstream is
  sent `response.cancel`; the Responses mode has no cancel event, so the
  upstream finishes the response. Either way it is billed from its usage.
  An upstream `error` event always reaches the client, a stopped
  response's included. One answering a client event (it carries the
  client's `event_id`, or is an `invalid_request_error`) goes at once and
  held output goes on. One naming a held response, or another failure,
  ends the held responses: their held text is checked as complete and
  stopped as above on a match; clean, it is released ahead of the error
  and the response's later frames, its terminal event included, are
  relayed and checked as before. Later responses are relayed.
  Verdicts are noted on the response's log entry (output, Responses input)
  or the session's (Realtime input).
- **Not checked**: audio, in either direction — spoken output of a stopped
  response may already carry what its transcript was stopped for. With the
  guardrails setting `block_unscannable_audio` (default off), `block` and
  any content rule or moderation on, audio input
  (`input_audio_buffer.append`, `input_audio` content) gets an `error`
  event instead (decided once per session). As on HTTP, images, reasoning
  and function call arguments in the output are not checked; nor is the
  upstream's transcription of input audio.
- **Events**: a client text event must be one JSON object with no repeated
  key (the relay reads the first, an upstream may read the last), else it
  gets an `error` event and is not forwarded. The first `response.create`
  of a Responses session must name a model. A Realtime `session.update`
  may not switch the model, and an input transcription model it or a beta
  `transcription_session.update` names (`audio.input.transcription.model`,
  beta `input_audio_transcription.model`) is admitted like a request model:
  the key must see it, the request plugins must let it through, and with a
  meter it must have a price; with a meter, a transcription set without a
  model is refused too. Otherwise the update gets an `error` event. On a
  transcription session (`intent=transcription`) with a meter or model
  visibility rules, input audio (`input_audio_buffer.append` / `commit`)
  gets an `error` event until the session's transcription model, as named
  or as the upstream reports it, passes the same check.
  An `event_id` may not start with `event_relay_guard_`, which the relay
  uses for its own events.
- **Billing**: the meter holds for each response before it runs — its
  output cap (`max_output_tokens`; Realtime: the session's
  `max_output_tokens` / `max_response_output_tokens`, else
  [`websocket.response_reserve_tokens`](configuration.md#websocket)) at
  the model's output price, plus its input estimate (below) at the input
  price — so the holds of concurrent sessions add up; a token plan holds
  the same input estimate plus output cap in tokens. A refusal before the session is an
  HTTP error; on a Responses `response.create` an `error` event; on
  Realtime the client gets an `error` event (`insufficient_quota`) once the
  running response is done and the session closes with `1008`. Each
  `response.done` (Realtime) or `response.completed` / `failed` /
  `incomplete` (Responses) is settled from its `usage` and logged as its
  own request: cached tokens as cache reads, the rest of the input and the
  output at the model's token prices. Realtime audio, text and image tokens
  are not priced apart; their split stays in the raw usage, so price a
  realtime model for its mix. Input audio transcription
  (`conversation.item.input_audio_transcription.completed`) is logged and
  billed as a request of the transcription model the session reports
  (`session.*` or `transcription_session.*`):
  tokens at its token prices, or seconds at its `audio_second` rate.
- **Unfinished responses**: when the client leaves with a response running,
  the upstream stays open up to 10 s for its usage. A response still open
  then, or when the upstream connection is lost, the session times out or
  the relay shuts down, is billed from an estimate and logged with
  `estimated` usage: output from the text and transcript deltas relayed;
  input on Responses as for an HTTP request (text at four bytes a token,
  an image as 800 tokens); on Realtime the conversation so far — text the
  client added at four bytes a token and input audio at about 10 tokens a
  second, raised to at least the last reported usage's input plus output.
- **Logged**: one entry per session (protocol `openai-realtime` or
  `openai-responses-ws`, status `101`, duration, bytes each way, responses,
  transcriptions and how it closed in the diagnostic notes) plus one per
  response and transcription with its usage and charge.
- **Limits** ([configuration](configuration.md#websocket)): a session ends
  after `websocket.max_session` (an `error` event `session_expired`, then
  `1000`) or `websocket.idle_timeout` without a message either way (`1000`);
  a Realtime session waiting on the upstream's `session.created` past
  `websocket.session_ready_timeout` gets an `error` event
  `session_not_ready`, then `1013`;
  a message over `websocket.max_message_bytes` closes it with `1009`. A close
  from either side is passed to the other with its code; a lost upstream
  connection closes the client with `1014`, a relay shutdown with `1001`, a
  relay fault with `1011`.

## Native passthrough

`/native/<upstream>/<path>` forwards any method, path, query and body to one
upstream, for the APIs plugrelay has no codec for: non-batch files,
fine-tuning, vendor-only endpoints. It is off until `native.enabled` is set in
relay.yaml and the key is granted the upstream
([configuration](configuration.md#native)); until then the route is 404, and
a key without the grant gets 403.

- **Addressing**: `<upstream>` is the upstream id. `<path>` is appended to
  the base URL of its first endpoint, which usually ends in `/v1`, so an SDK
  pointed at `https://relay.example.com/native/openai` calls
  `https://relay.example.com/native/openai/files` and the relay calls
  `https://api.openai.com/v1/files`. `<upstream>:<endpoint>` picks another
  endpoint (`/native/deepseek:completions/…`). Unknown upstream: 404;
  disabled: 403.
- **Credentials**: the platform key goes in any of the usual headers (or
  `?key=`, which is stripped from the forwarded query) and is never
  forwarded, nor is any other client credential. The relay picks a
  credential of that upstream the caller may use (cooling, held, quarantined
  and full credentials are skipped; 429 or 503 when none is left) and the
  provider adds its auth. The core egress rules hold: dangerous headers
  dropped, the credential's fixed proxy, the SSRF guard, the upstream's
  extra headers and timeouts.
- **Scope of a grant**: native access hands the caller everything the
  upstream's credential may do on that host, not just model calls — for
  Vertex that is a `cloud-platform` token, for OpenAI the whole account API
  (files, fine-tuning, deletions). Grant it per key, to callers you would
  give the credential itself. Path segments that decode to `.` or `..` are
  refused (400), so a base URL's path prefix holds.
- **Bodies** stream both ways without buffering, up to
  `native.max_body_bytes` per request; the response — JSON, binary or SSE —
  comes back with the upstream's status and headers.
- **Not applied**: request plugins (rate limits, quotas, redaction,
  guardrails) and metering. Usage is unknown, so nothing is billed, and
  failures never change a credential's health. The `middleware.access` IP and
  client allowlists still apply.
- **Logged** as one request-log entry with protocol `native`, tier
  `passthrough`, status, request and response bytes and duration; the
  method and path are in the attempt's diagnostic notes.

## MCP servers

The relay's MCP gateway ([mcp.md](mcp.md)) is one Streamable HTTP server per
key: `https://relay.example.com/mcp` offers every tool the key was granted,
named `<server>__<tool>`; `/mcp/<server>` offers one server. Nothing is
installed on the client, and the servers' own credentials stay on the relay.
`plugrelay setup` does not register it — Claude Code keeps user-level MCP
servers in `~/.claude.json`, a file it rewrites itself — so add it by hand.

Claude Code ([docs](https://code.claude.com/docs/en/mcp), checked
2026-11-06):

```bash
claude mcp add --transport http plugrelay https://relay.example.com/mcp \
  --header "Authorization: Bearer pr_xxx"
```

or, for a project, `.mcp.json` (`${VAR}` is expanded from the environment):

```json
{"mcpServers": {"plugrelay": {"type": "http", "url": "https://relay.example.com/mcp",
                              "headers": {"Authorization": "Bearer ${PLUGRELAY_API_KEY}"}}}}
```

Claude Code shows the tools as `mcp__plugrelay__<server>__<tool>`.

Codex ([docs](https://developers.openai.com/codex/mcp), checked
2026-11-06), in `~/.codex/config.toml`:

```toml
[mcp_servers.plugrelay]
url = "https://relay.example.com/mcp"
bearer_token_env_var = "PLUGRELAY_API_KEY"
```

Other clients take the same URL and `Authorization` header in their
"remote / Streamable HTTP MCP server" settings.

## What every client loses behind a relay

- Vendor-only features that require the official endpoint: Cursor Tab,
  Copilot inline completion, Zed edit prediction, Claude Code's fine-grained
  tool streaming and prompt-caching scope features.
- Codex over a custom provider: WebSocket, remote compaction and web-search
  extensions are off; local compaction works.
- Capabilities an upstream lacks and no conversion step can bridge (images to
  a text-only endpoint, a server tool the request forces and no plugin
  executes) make the relay skip that upstream; server tools that are merely
  offered (Codex's `web_search`) are removed and the request log lists them
  as dropped; if none is left the client gets a 400 naming the missing
  capability — never a silent drop.
- Over OpenAI chat, upstream-private state from another protocol cannot be
  carried (the protocol has no echo field); reasoning continuity across turns
  is lost there and the request log says so.
