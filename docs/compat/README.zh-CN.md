# 客户端兼容性记录

[English](README.md) | 简体中文

真实编程客户端经由中转、对接真实上游的运行结果。每一行是 `scripts/compat.sh` 某一次
运行里一个客户端在一条路由上的结果；链接的报告里有那次运行的全部用例，协议测试也在内。

- **一次运行做什么。** [`TestClients`](../../test/e2e/clients_test.go) 先构建中转，
  在一个用完即弃的 home 目录里用 `plugrelay setup` 配好每个客户端（用户自己的
  `~/.claude` 和 `~/.codex` 绝不会动），然后让客户端写一个 `answer.py`、运行它并报告
  输出；测试检查这个文件、它的输出和请求日志。[`TestRelay`](../../test/e2e/e2e_test.go)
  则直接调用每种客户端协议。
- **路由。** *responses upstream*：用通用的 OpenAI 兼容清单，Codex 的 Responses 请求
  直通，Claude Code 的 Anthropic 请求要转换。*chat-only upstream*：清单里只有 Chat
  Completions，两个客户端的每个请求都要转换。
- **pass** 的意思是：程序跑起来了，打印出 42，客户端报告了这个结果，而且日志里每个请求
  都经由预期的上游成功完成。

<a id="running-it"></a>
## 怎么跑

```bash
export PLUGRELAY_E2E_BASE=https://llm.example.com/v1   # OpenAI 兼容，带 /v1
export PLUGRELAY_E2E_KEY=...                           # 只放在环境变量里
export PLUGRELAY_E2E_MODEL=...      PLUGRELAY_E2E_CHAT_MODEL=...   # 可选
export PLUGRELAY_COMPAT_UPSTREAM="name for the report" # 报告里绝不会写出 URL
scripts/compat.sh                    # 写出 docs/compat/<date>.md，并更新本表
```

`claude` 和 `codex` 要在 `PATH` 里（缺哪个就记为跳过），另外还要 `go`、`jq` 和
`python3`。定时工作流 [`compat.yml`](../../.github/workflows/compat.yml) 跑的是同一个
脚本：仓库配了 `PLUGRELAY_E2E_BASE` 和 `PLUGRELAY_E2E_KEY` 这两个 secret 时
（可选变量 `PLUGRELAY_E2E_MODEL`、`PLUGRELAY_E2E_CHAT_MODEL`、
`PLUGRELAY_COMPAT_UPSTREAM`），每周跑一次，也可以手动触发；报告作为 artifact 上传，
由维护者提交进仓库。

<a id="other-live-checks"></a>
## 其他真实环境检查

对接真实服务的端到端测试，需要主动开启；没配好时各自跳过并说明原因。最近一次运行的
输出在带日期的报告里。

| 测试 | 服务 | 开启方式 |
|-|-|-|
| `TestMCPReferenceServer`（[mcp_test.go](../../test/e2e/mcp_test.go)） | 走 Streamable HTTP 的 `@modelcontextprotocol/server-everything`、官方 TypeScript SDK 客户端、Claude Code | `PLUGRELAY_E2E_MCP=1`（需要 node、npm） |
| `TestCaptchaLive`（[captcha_test.go](../../test/e2e/captcha_test.go)） | Cloudflare Turnstile 和 hCaptcha 的 siteverify，用它们公开的测试密钥 | `PLUGRELAY_E2E_CAPTCHA=1` |
| `TestSelfHostedMedia`（[selfhosted_test.go](../../test/e2e/selfhosted_test.go)） | Hugging Face TEI（embeddings、rerank）、whisper.cpp 服务（语音转写） | `PLUGRELAY_E2E_TEI_EMBED`、`PLUGRELAY_E2E_TEI_RERANK`、`PLUGRELAY_E2E_STT_BASE` |

启动这些本地服务（CPU 运行，模型从 Hugging Face 下载）：

```bash
docker run -d --network host -v $PWD/hf:/data ghcr.io/huggingface/text-embeddings-inference:cpu-1.8 \
  --model-id BAAI/bge-small-en-v1.5 --port 18090 --hostname 127.0.0.1
docker run -d --network host -v $PWD/hf:/data ghcr.io/huggingface/text-embeddings-inference:cpu-1.8 \
  --model-id BAAI/bge-reranker-base --port 18091 --hostname 127.0.0.1
# whisper.cpp 从源码构建；带 libflite 的 ffmpeg 用来生成测试用的语音
sh models/download-ggml-model.sh base.en
build/bin/whisper-server -m models/ggml-base.en.bin --port 18094 --inference-path /v1/audio/transcriptions
export PLUGRELAY_E2E_TEI_EMBED=http://127.0.0.1:18090 PLUGRELAY_E2E_TEI_RERANK=http://127.0.0.1:18091 \
  PLUGRELAY_E2E_STT_BASE=http://127.0.0.1:18094/v1
```

<a id="runs"></a>
## 运行记录

`scripts/compat.sh` 只往[英文版](README.md#runs)追加新的运行记录，最新结果以英文版为准。

<!-- compat:rows: scripts/compat.sh inserts each run under the header, newest first -->
| 日期 | 客户端 | 版本 | 路由 | 上游模型 | 结果 | 耗时 | 中转版本 | 报告 |
|-|-|-|-|-|-|-|-|-|
| 2026-11-06 | claude-code | 2.1.319 (Claude Code) | responses upstream | gpt-5.4 | pass | 8s | plugrelay uncommitted tree | [report](2026-11-06.md) |
| 2026-11-06 | claude-code | 2.1.319 (Claude Code) | chat-only upstream | deepseek-v4.1-flash | pass | 2m41s | plugrelay uncommitted tree | [report](2026-11-06.md) |
| 2026-11-06 | codex | codex-cli 0.166.0 | responses upstream | gpt-5.4 | pass | 8s | plugrelay uncommitted tree | [report](2026-11-06.md) |
| 2026-11-06 | codex | codex-cli 0.166.0 | chat-only upstream | deepseek-v4.1-flash | pass | 2m26s | plugrelay uncommitted tree | [report](2026-11-06.md) |
