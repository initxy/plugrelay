# Client compatibility record

English | [简体中文](README.zh-CN.md)

Real coding clients driven through the relay against a live upstream. Each
row below is one client on one route in one run of `scripts/compat.sh`; the
linked report has every case of that run, protocol tests included.

- **What a run does.** [`TestClients`](../../test/e2e/clients_test.go) builds
  the relay, sets up each client with `plugrelay setup` in a throwaway home
  (your own `~/.claude` and `~/.codex` are not touched), and asks it to write
  `answer.py`, run it and report the output. [`TestRelay`](../../test/e2e/e2e_test.go)
  calls every client protocol directly.
- **Routes.** *responses upstream* uses the generic OpenAI-compatible
  manifest: Codex's requests pass through, Claude Code's are converted.
  *chat-only upstream* offers only Chat Completions, so every request is
  converted.
- **Pass** means the program printed 42, the client reported it, and every
  request in the log succeeded through the expected upstream.

## Running it

```bash
export PLUGRELAY_E2E_BASE=https://llm.example.com/v1   # OpenAI-compatible, with /v1
export PLUGRELAY_E2E_KEY=...                           # environment only
export PLUGRELAY_E2E_MODEL=...      PLUGRELAY_E2E_CHAT_MODEL=...   # optional
export PLUGRELAY_COMPAT_UPSTREAM="name for the report" # the URL is never written
scripts/compat.sh                    # writes docs/compat/<date>.md, updates this table
```

Needs `go`, `jq`, `python3`, and `claude` and `codex` on `PATH` (a missing
client is recorded as skipped). The [`compat.yml`](../../.github/workflows/compat.yml)
workflow runs the same script weekly or on demand when the repository has
the `PLUGRELAY_E2E_BASE` and `PLUGRELAY_E2E_KEY` secrets, and uploads the
report as an artifact for a maintainer to commit.

## Other live checks

Opt-in e2e tests against real services; each skips with a reason when not
set up. The last run's output is in [2026-11-06-live](2026-11-06-live.md).

| test | service | turn on |
|-|-|-|
| `TestMCPReferenceServer` ([mcp_test.go](../../test/e2e/mcp_test.go)) | `@modelcontextprotocol/server-everything` over Streamable HTTP, the official TypeScript SDK client, Claude Code | `PLUGRELAY_E2E_MCP=1` (needs node, npm) |
| `TestCaptchaLive` ([captcha_test.go](../../test/e2e/captcha_test.go)) | Cloudflare Turnstile and hCaptcha siteverify with their published test keys | `PLUGRELAY_E2E_CAPTCHA=1` |
| `TestSelfHostedMedia` ([selfhosted_test.go](../../test/e2e/selfhosted_test.go)) | Hugging Face TEI (embeddings, rerank), whisper.cpp server (transcription) | `PLUGRELAY_E2E_TEI_EMBED`, `PLUGRELAY_E2E_TEI_RERANK`, `PLUGRELAY_E2E_STT_BASE` |

Starting the local servers (CPU, models fetched from Hugging Face):

```bash
docker run -d --network host -v $PWD/hf:/data ghcr.io/huggingface/text-embeddings-inference:cpu-1.8 \
  --model-id BAAI/bge-small-en-v1.5 --port 18090 --hostname 127.0.0.1
docker run -d --network host -v $PWD/hf:/data ghcr.io/huggingface/text-embeddings-inference:cpu-1.8 \
  --model-id BAAI/bge-reranker-base --port 18091 --hostname 127.0.0.1
# whisper.cpp built from source; ffmpeg with libflite makes the test's speech
sh models/download-ggml-model.sh base.en
build/bin/whisper-server -m models/ggml-base.en.bin --port 18094 --inference-path /v1/audio/transcriptions
export PLUGRELAY_E2E_TEI_EMBED=http://127.0.0.1:18090 PLUGRELAY_E2E_TEI_RERANK=http://127.0.0.1:18091 \
  PLUGRELAY_E2E_STT_BASE=http://127.0.0.1:18094/v1
```

## Runs

<!-- compat:rows: scripts/compat.sh inserts each run under the header, newest first -->
| date | client | version | route | upstream model | result | duration | relay | report |
|-|-|-|-|-|-|-|-|-|
| 2026-11-06 | claude-code | 2.1.319 (Claude Code) | responses upstream | gpt-5.4 | pass | 8s | plugrelay uncommitted tree | [report](2026-11-06.md) |
| 2026-11-06 | claude-code | 2.1.319 (Claude Code) | chat-only upstream | deepseek-v4.1-flash | pass | 2m41s | plugrelay uncommitted tree | [report](2026-11-06.md) |
| 2026-11-06 | codex | codex-cli 0.166.0 | responses upstream | gpt-5.4 | pass | 8s | plugrelay uncommitted tree | [report](2026-11-06.md) |
| 2026-11-06 | codex | codex-cli 0.166.0 | chat-only upstream | deepseek-v4.1-flash | pass | 2m26s | plugrelay uncommitted tree | [report](2026-11-06.md) |
