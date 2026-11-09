# 接入客户端

[English](clients.md) | 简体中文

每个客户端只要改 base URL，再填一把平台 key（`pr_…`）。key 用 `plugrelay key create`
创建，或者在控制台的 Keys 页创建，那里还按客户端给出现成的接入配置。下面的例子用
`https://relay.example.com`；本地跑的中转站是 `http://127.0.0.1:8787`。真实 Claude Code
和 Codex 走中转站的实测记录见 [compat/](compat/README.zh-CN.md)。

- [中转站提供哪些接口](#what-the-relay-serves)
- [Claude Code](#claude-code) · [Codex](#codex) · [Gemini CLI](#gemini-cli) ·
  [Cursor](#cursor) · [GitHub Copilot (VS Code)](#github-copilot-vs-code) ·
  [Cline / Roo Code / Kilo Code](#cline--roo-code--kilo-code) ·
  [OpenCode](#opencode) · [Cherry Studio / Continue / JetBrains](#cherry-studio--continue--jetbrains) ·
  [Trae](#trae)
- [试用对话 Playground](#playground)
- [批量任务](#batches)
- [Realtime 和 WebSocket](#realtime-and-websocket)
- [原生直通](#native-passthrough)
- [MCP 服务](#mcp-servers)
- [走中转站后少掉的功能](#what-every-client-loses-behind-a-relay)

<a id="what-the-relay-serves"></a>
## 中转站提供哪些接口

| 路由 | 协议 | 谁在用 |
|-|-|-|
| `POST /v1/messages`, `POST /v1/messages/count_tokens` | `anthropic` | Claude Code、Cline、Copilot 等 |
| `HEAD`/`GET /api/hello` | `anthropic` | Claude Code 启动时的探活；中转站直接应答 |
| `POST /v1/responses`, `/v1/responses/compact`, `/v1/responses/input_tokens` | `openai-responses` | Codex、Copilot、OpenCode、Continue |
| `POST /v1/chat/completions` | `openai-chat` | Cursor、Cline、OpenCode、Cherry Studio 等 |
| `POST /v1/completions` | `openai-completions` | Continue 和 JetBrains 的代码补全 |
| `POST /v1beta/models/{model}:generateContent` / `:streamGenerateContent` / `:countTokens` | `gemini` | Gemini CLI、Kilo、Cherry Studio |
| `POST /v1/embeddings` | `openai-embeddings` | OpenAI SDK、RAG 工具、Cherry Studio |
| `POST /v1beta/models/{model}:embedContent` / `:batchEmbedContents` | `gemini-embeddings` | Google GenAI SDK |
| `POST /v1/images/generations`, `/v1/images/edits`（multipart） | `openai-images` | OpenAI SDK、生图工具 |
| `POST /v1/audio/transcriptions`, `/v1/audio/translations`（multipart）, `/v1/audio/speech` | `openai-audio` | OpenAI SDK、语音工具 |
| `POST /v1/rerank` | `rerank`（Jina、vLLM、Cohere v1 的格式） | RAG 框架、Jina/Cohere SDK |
| `POST /v2/rerank` | `cohere-rerank` | Cohere SDK（v2） |
| `/v1/files`, `/v1/batches`, `/v1/messages/batches` | 无，见[批量任务](#batches) | OpenAI 和 Anthropic SDK 的批量接口 |
| WebSocket `GET /v1/realtime?model=…` | OpenAI Realtime，见 [realtime](#realtime-and-websocket) | OpenAI Realtime SDK、语音智能体 |
| WebSocket `GET /v1/responses` | Responses 的 WebSocket 模式，或回 `426`，见 [realtime](#realtime-and-websocket) | 开了 WebSocket 的 Codex |
| `GET /v1/models`, `GET /v1beta/models` | 全部 | 几乎所有客户端 |
| `POST`/`DELETE` `/mcp`, `/mcp/<server>` | MCP Streamable HTTP | MCP 客户端，见 [MCP 服务](#mcp-servers) |
| `/native/<upstream>/<path>` 上的任意方法 | 无，原样转发 | 其他厂商接口；默认关闭，见[原生直通](#native-passthrough) |

路由由各协议编解码器的 `Routes()` 声明（`pkg/protocol/<name>`）。每条路由不带 `/v1` 或
`/v1beta` 前缀、末尾多一个斜杠、带 Azure 的 `?api-version=`，都照样能用
（`internal/server/routes.go`）。

| | |
|-|-|
| Key | 放在 `Authorization: Bearer`、`x-api-key`、`x-goog-api-key` 或 `?key=` 里都行（`plugins/auth/apikey`） |
| 模型列表 | 带 `?client_version=` 给 Codex 的模型目录，带 `anthropic-version` 头给 Anthropic 格式，`/v1beta/models` 给 Gemini 格式，其余给 OpenAI 格式。只列这把 key 能用的模型 |
| 错误 | 中转站自己的错误用客户端协议的格式和信号返回（上下文超长、`retry-after`、额度用完、过载）；同协议上游的错误原样返回 |
| 模型名 | 从不偷换。想跨模型自动兜底，由管理员定义一个[预设 preset](configuration.zh-CN.md#routing-models-and-presets)，客户端按名字选用 |

**多媒体请求**（embeddings、生图、语音、rerank）和对话共用 key、选路、重试、日志和计费。
只有声明了对应能力（`embeddings`、`image_generation`、`image_edit`、
`audio_transcription`、`speech`、`rerank`）的上游入口才接，否则返回 400
`capability_missing`。

- Embeddings 能在 OpenAI 和 Gemini 之间互转（`dimensions` ⇄ `outputDimensionality`，
  需要时由中转站生成 base64）。token id 形式的输入只能发给 OpenAI 风格的入口。
- 生图和语音只发给同协议的上游。
- Rerank 能在 `/v1/rerank`、Cohere v2 的 `/v2/rerank`（只收字符串）和 Hugging Face TEI
  的 `/rerank`（只用于上游）之间互转。目标不支持 `top_n` 或 `return_documents` 时由
  中转站自己处理。非文本文档只发给同协议上游。用量取上游报的 token 数或搜索单位；
  TEI 不报，按估算。
- 上传（`/v1/images/edits`、`/v1/audio/transcriptions`、`/v1/audio/translations`）
  边收边转，上限 `limits.max_body_bytes`（超过回 413）；压缩过的上传回 415。`model`
  字段排在文件后面时，先把请求体存到临时文件。边收边转的上传一旦开始发送就不再换上游重试，
  存过临时文件的可以。

<a id="claude-code"></a>
## Claude Code

Anthropic messages，请求从你自己的机器发出。

```bash
plugrelay setup claude-code --key pr_xxx --url https://relay.example.com
```

会写进 `~/.claude/settings.json`（原文件备份成 `.bak-<time>`）：

```json
{"env": {"ANTHROPIC_BASE_URL": "https://relay.example.com",
         "ANTHROPIC_AUTH_TOKEN": "pr_xxx",
         "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"}}
```

加 `--model M[,small]` 还会设置 `ANTHROPIC_MODEL` 和 `ANTHROPIC_DEFAULT_HAIKU_MODEL`
（取第二个名字，没有就也用 `M`）。手动配置就导出同样的环境变量，用 `ANTHROPIC_API_KEY` 也行。

- `anthropic-beta` 和 `anthropic-version` 原样转发。不支持某个 beta 的上游在自己的
  清单里（`unsupported_betas`）把它去掉。
- 上游是 Anthropic 时请求体一个字节不改（`system`、带 `ttl: 1h` 的 `cache_control`、
  `thinking`、工具），提示词缓存命中率和直连一样。
- "prompt is too long" 报错原样保留，所以会触发自动压缩上下文。
- 上游不是 Anthropic 时，思考签名加密后交给客户端，只还给签发它的上游，不会出现
  "Invalid signature in thinking block"。
- 开了模型发现时，Claude Code 只列 id 里含 `claude` 或 `anthropic` 的模型，其他模型用
  `ANTHROPIC_MODEL` 指定。

<a id="cc-switch"></a>
### CC Switch

在控制台创建或轮换 key 之后，接入面板会给 Claude Code、Codex 和 Gemini CLI 各提供一个
"导入 CC Switch"链接（`ccswitch://v1/import`），带上中转站的名称和地址、key，以及和接入
配置一样的设置（`internal/admin/connect.go`）。

<a id="codex"></a>
## Codex

只用 OpenAI responses，请求从你自己的机器发出。

```bash
plugrelay setup codex --key pr_xxx --model gpt-5.5 --url https://relay.example.com
export PLUGRELAY_API_KEY=pr_xxx   # Codex 从这个变量读 key
```

会把中转站的模型目录存成 `~/.codex/plugrelay-models.json`，并在 `~/.codex/config.toml`
里加一个供应商并选中它：

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

- 用自定义供应商，别用 `openai_base_url`。用后者的话 Codex 把中转站当成 OpenAI，会打开
  WebSocket（见[下文](#realtime-and-websocket)）、远程压缩上下文和请求压缩。
- Codex 只从 `model_catalog_json` 指向的文件读模型信息；没有这个文件会警告
  "Model metadata … not found"，然后用通用设置（没有自由格式的 `apply_patch`，上下文
  窗口用默认值）。文件是快照，加了模型要重跑 `setup codex`。key 一个模型都看不到时
  setup 直接失败，因为目录为空 Codex 起不来。
- 流一定以带用量的 `response.completed` 结束，Codex 自动压缩上下文要靠它。
- 中转站返回一个带签名的 `x-codex-turn-state` 头，Codex 会带回来，这样同一轮对话固定在
  同一把凭据上，提示词缓存不丢。

<a id="gemini-cli"></a>
## Gemini CLI

Gemini `generateContent`。Gemini CLI 没有配置文件可改，所以 setup 直接打印环境变量：

```bash
eval "$(plugrelay setup gemini --key pr_xxx --url https://relay.example.com --print)"
# export GOOGLE_GEMINI_BASE_URL=https://relay.example.com
# export GEMINI_API_KEY=pr_xxx
```

`thoughtSignature` 会保留。429 带 Google 风格的 `RetryInfo`；每日额度用完会明确告知，
客户端就不再重试。

<a id="cursor"></a>
## Cursor

OpenAI chat。**请求从 Cursor 的服务器发出**，所以中转站要有公网 HTTPS 地址（站点设置
`site.public_base_url`），localhost 和内网都不行。在 Cursor 设置里打开
*Override OpenAI Base URL*，填 `https://relay.example.com/v1` 和 key，再加上模型名。
Tab 补全始终用 Cursor 自己的模型。

<a id="github-copilot-vs-code"></a>
## GitHub Copilot (VS Code)

在 `chatLanguageModels.json` 里加一个 *Custom Endpoint*，指向中转站并填上 key。Copilot
按模型走 chat、responses 或 messages，需要 `/v1/models`；不支持工具调用的模型不会出现在
Agent 模式里。行内补全不经过中转站。

<a id="cline--roo-code--kilo-code"></a>
## Cline / Roo Code / Kilo Code

选 OpenAI 兼容供应商（base URL 填 `https://relay.example.com/v1`）或 Anthropic 供应商
（填 `https://relay.example.com`），再填 key。Kilo 还能用 responses 和 Gemini。
`max_tokens` / `max_completion_tokens`、Roo 放在 OpenAI 消息里的 `cache_control`、历史里的
`reasoning_content`（DeepSeek 一类上游需要）都能正确处理。

<a id="opencode"></a>
## OpenCode

```bash
plugrelay setup opencode --key pr_xxx --model claude-sonnet-4-5,gpt-5.5 --url https://relay.example.com
```

会在 `~/.config/opencode/opencode.json` 里加一个 `plugrelay` 供应商
（`@ai-sdk/openai-compatible`，`…/v1`），带上这些模型，用 `/models` 选。OpenCode 只提供
这里列出的模型，所以 `--model` 必填。

<a id="cherry-studio--continue--jetbrains"></a>
## Cherry Studio / Continue / JetBrains

加一个 OpenAI 兼容供应商，base URL 填 `https://relay.example.com`（带不带 `/v1` 都行），
再填 key。

Continue 和 JetBrains 的代码补全走 `/v1/completions`，把补全模型也指向同一个地址。

- 上游有 completions 入口时（OpenAI、DeepSeek、vLLM、Ollama、OpenRouter 和大多数
  OpenAI 兼容清单），请求一个字节不改地转过去。
- 带 `suffix` 的请求（光标前后都给，补中间那段）只发给声明了 `fill_in_middle` 的入口
  （OpenAI、DeepSeek、Mistral、Ollama、SiliconFlow）。一个都没有时返回 400 并指明缺它，
  不会给一个无视光标后代码的补全。
- 不带 suffix 时，只支持 chat 的上游也能接，提示词作为一条 user 消息发过去（有损）。
  多条提示词、token id 形式的提示词、大于 1 的 `n`/`best_of` 和 `echo` 必须有
  completions 入口。

<a id="trae"></a>
## Trae

OpenAI chat 或 Anthropic messages。Trae 按填的地址原样请求，所以要填完整路径：
`https://relay.example.com/v1/chat/completions`。请求从你的机器还是 Trae 的服务器发出，
目前没确认；本地中转站连不上的话，就像 Cursor 那样用公网地址。

<a id="playground"></a>
## Playground

控制台的试用对话（`POST /api/v1/playground`）用你的某把 key 发一段对话，四种对话协议任选
（管理员可以不用 key）。它走正常的请求流程——限额、选路、计费、请求日志都照常——
User-Agent 是 `plugrelay-playground/1`。

<a id="batches"></a>
## 批量任务

OpenAI Batch API（`purpose=batch` 的 `/v1/files`、`/v1/batches`）和 Anthropic Message
Batches（`/v1/messages/batches`，包括 `…/cancel` 和 `…/results`），把 SDK 照常指向中转站
就能用（`internal/pipeline/batch.go`）。

- **放在哪**：按模型选路（OpenAI 看第一行的 `body.model`，Anthropic 看 `params.model`），
  发给声明了 `batches` 的入口（官方 `openai` 和 `anthropic` 清单）。中转站记下每个文件、
  每个批次在哪把凭据上，之后的调用都发过去，路由改了也一样。只支持同协议。
- **一批一个模型**：每一行必须是同一个模型（OpenAI 还要求 `url` 相同），否则 400。除了
  改模型名，每一行原样转发。上传边收边转，上限 512 MiB（`DefaultBatchMaxBody`）；有一行
  不对就中止上传。
- **Id 和权限**：id 用上游的。只有创建者（创建它的用户；key 不属于用户时就是这把 key）
  能看到，其他人拿到 404。message batch 的 `results_url` 改成指向中转站，所以 SDK 不会
  把平台 key 发给厂商。
- **计费**：每条成功请求的用量按价格规则的 `batch_discount`
  （[价格](configuration.zh-CN.md#prices)）记在创建者名下，只计一次。时机是结果第一次被
  完整读取：客户端读，或者中转站自己读——结束超过十分钟、或者马上要被删的批次，中转站会
  去取结果。启用 `meter.balance` 时，创建批次先预扣估算金额
  （[收费功能](sell-kit.zh-CN.md#prepaid-balance-meterbalance)）。
- **请求插件**：创建批次算这个模型的一次请求，访问名单、禁用模型、限流照常生效。
  guardrails 检查每一行（审核接口只查前 100 行），有一行被拒整批被拒，并指出是哪一行。
  `middleware.redact`、`redact` 模式的 guardrails 和 `middleware.param-policy` 会改写每一行。
  只能改解码后请求的插件会让批次被拒；单次尝试钩子不运行。读取、列表、取消、删除不跑插件。

<a id="realtime-and-websocket"></a>
## Realtime 和 WebSocket

两种 WebSocket 接口按消息逐条转到上游的 WebSocket（`internal/pipeline/websocket.go`）。
不支持 Gemini Live。

| | OpenAI Realtime | Responses 的 WebSocket 模式 |
|-|-|-|
| 地址 | `wss://relay.example.com/v1/realtime?model=…`（[指南](https://developers.openai.com/api/docs/guides/voice-websockets)） | `wss://relay.example.com/v1/responses`（[指南](https://developers.openai.com/api/docs/guides/websocket-mode)） |
| 上游 | 声明了 `realtime` 的入口：`openai`、`azure-openai` | 声明了 `responses_websocket` 的入口：只有官方 OpenAI |
| 模型 | 取自地址，照常选路 | 取自第一个 `response.create`。Codex 只在握手回 `426` 时才退回 HTTP，所以只有当一个这样的上游能服务这把 key 的全部模型时，中转站才接受升级，否则回 `426` |
| 算一次请求的 | 握手，以及每个带文本的客户端事件 | 每个 `response.create` |

- **Key**：常用请求头、`?key=`，浏览器可以用子协议 `openai-insecure-api-key.<key>`。
  key 从不转发。交给浏览器的 key 要是公开了也无所谓的那种。
- **握手**：先通过核心出站规则连上游，再接受客户端的升级，所以选路出错、上游握手失败都会
  以普通 HTTP 错误返回。凭据的并发名额整个会话都占着。
- **插件**：访问名单、`blocked_models`、`middleware.ratelimit` 在握手和每次请求时检查；
  会话中被拒返回 `error` 事件。`middleware.param-policy` 会压低 `response.create` 和
  Realtime `session.update` 里的输出 token 上限和 `reasoning.effort`。
- **内容检查**（[guardrails](plugins.zh-CN.md#guardrails)、`middleware.redact`）逐个读客户端
  事件的文本，以及输出的文本和转写增量。客户端事件被拦下时收到 `error` 事件，会话不断；
  输出被拦下时停掉这个响应。音频从不检查；开了 `block_unscannable_audio` 就直接拒绝音频
  输入。guardrails 的审核接口每个 Realtime 会话最多查 `max_moderated_events`（200）个事件。
- **事件**：客户端事件必须是一个 JSON 对象，不能有重复的键。Realtime 的 `session.update`
  不能换模型，它指定的转写模型要和请求模型过同样的检查。以 `event_relay_guard_` 开头的
  `event_id` 留给中转站自己用。
- **计费**：每个响应开跑前先预扣（它的输出上限，没有就用
  [`websocket.response_reserve_tokens`](configuration.zh-CN.md#websocket)，加上输入估算），
  再按它的 `usage` 结算，单独记一条日志。Realtime 的音频、文本、图片 token 不分开定价。
  输入音频转写按转写模型记一次请求。会话结束时还没跑完的响应按估算计费（用量标为
  `estimated`）。会话中余额不足时：Realtime 等正在跑的响应结束后给客户端发 `insufficient_quota`，再以 `1008`
  关闭会话；Responses 则给这个 `response.create` 回 `error` 事件。
- **日志**：每个会话一条（协议 `openai-realtime` 或 `openai-responses-ws`，状态 `101`），
  另外每个响应、每次转写各一条。
- **限制**（[配置](configuration.zh-CN.md#websocket)）：`websocket.max_session`、
  `websocket.idle_timeout`、`websocket.session_ready_timeout`、`websocket.max_message_bytes`
  （超过以 `1009` 关闭）。一方关闭时带着关闭码转给另一方；上游断开时以 `1014` 关闭客户端，
  中转站关机用 `1001`。

<a id="native-passthrough"></a>
## 原生直通

`/native/<upstream>/<path>` 把任意方法、路径、查询参数和请求体转给某一个上游，用来调
plugrelay 没有编解码器的接口：非批量的文件接口、微调、厂商独有接口。默认关闭，要设置
`native.enabled` 并给 key 授权这个上游（[配置](configuration.zh-CN.md#native)）：没开时
返回 404，没授权返回 403。

- **地址**：`<path>` 拼在这个上游第一个入口的 base URL 后面。SDK 指向
  `https://relay.example.com/native/openai` 再请求 `…/files`，实际到达
  `https://api.openai.com/v1/files`。用 `<upstream>:<endpoint>` 选别的入口
  （`/native/deepseek:completions/…`）。
- **凭据**：平台 key（任一常用请求头或 `?key=`）从不转发；中转站从这个上游里挑一把能用的
  凭据（一把都没有时回 429 或 503），由供应商插件加上鉴权。核心出站规则照常生效。
- **谨慎授权**：等于把这把凭据在那个主机上能做的一切交给调用者——对 OpenAI 是整个账号的
  API，对 Vertex 是一个 `cloud-platform` 令牌。只授权给你愿意直接交出凭据的调用者。
  路径里的 `.` 和 `..` 会被拒（400）。
- **不生效的**：请求插件（限流、额度、脱敏、guardrails）和计费；失败不影响凭据健康状态。
  `middleware.access` 的 IP 和客户端白名单仍然生效。请求体和响应体双向流式传输，上限
  `native.max_body_bytes`。
- **日志**：协议 `native`，处理方式 `passthrough`，带状态码、字节数和时长。

<a id="mcp-servers"></a>
## MCP 服务

中转站的 [MCP 网关](mcp.zh-CN.md)对每把 key 是一个 Streamable HTTP 服务：
`https://relay.example.com/mcp` 提供这把 key 被授权的所有工具，名字是 `<server>__<tool>`；
`/mcp/<server>` 只提供一个服务。各服务的凭据留在中转站上。`plugrelay setup` 不会注册它
（Claude Code 自己会改写 `~/.claude.json`），要手动加。

Claude Code（[文档](https://code.claude.com/docs/en/mcp)，2026-11-06 核对）：

```bash
claude mcp add --transport http plugrelay https://relay.example.com/mcp \
  --header "Authorization: Bearer pr_xxx"
```

或者按项目写 `.mcp.json`（`${VAR}` 从环境变量取）：

```json
{"mcpServers": {"plugrelay": {"type": "http", "url": "https://relay.example.com/mcp",
                              "headers": {"Authorization": "Bearer ${PLUGRELAY_API_KEY}"}}}}
```

工具显示为 `mcp__plugrelay__<server>__<tool>`。

Codex（[文档](https://developers.openai.com/codex/mcp)，2026-11-06 核对），写在
`~/.codex/config.toml` 里：

```toml
[mcp_servers.plugrelay]
url = "https://relay.example.com/mcp"
bearer_token_env_var = "PLUGRELAY_API_KEY"
```

其他客户端填同样的地址和 `Authorization` 请求头即可。

<a id="what-every-client-loses-behind-a-relay"></a>
## 走中转站后少掉的功能

- 必须连厂商官方端点的功能：Cursor Tab、Copilot 行内补全、Zed 的编辑预测、Claude Code
  的细粒度工具流式输出和提示词缓存作用域相关功能。
- Codex 走自定义供应商时没有 WebSocket、远程压缩上下文和网页搜索扩展；本地压缩照常可用。
- 没有上游具备、也没有转换能补上的能力（把图片发给只收文本的入口、强制要用某个服务端工具
  却没人执行），客户端收到指明缺什么的 400。只是提供而没强制的服务端工具（Codex 的
  `web_search`）会被去掉，日志里记为已丢弃。
- 走 OpenAI chat 时，另一种协议的上游私有状态带不过去，跨轮次的思考连贯性会丢，请求日志
  会写明。
