# 接入客户端

[English](clients.md) | 简体中文

每个客户端都只要改一个 base URL，再填一把平台 key（`pr_…`）。key 用
`plugrelay key create` 创建，或者在控制台的 Keys 页创建（那里还按客户端给出
现成的接入片段）。下面的例子用 `https://relay.example.com`；本地跑的中转站用
`http://127.0.0.1:8787`。真实 Claude Code 和 Codex 走中转站的实测结果（带日期）
见 [compat/](compat/README.zh-CN.md)。

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
- [走中转站后所有客户端都会少掉的东西](#what-every-client-loses-behind-a-relay)

<a id="what-the-relay-serves"></a>
## 中转站提供哪些接口

| 路由 | 协议 | 谁在用 |
|-|-|-|
| `POST /v1/messages`, `POST /v1/messages/count_tokens` | `anthropic` | Claude Code、Cline、Copilot 等 |
| `HEAD`/`GET /api/hello` | `anthropic` | Claude Code 启动时的探活请求；中转站本地直接应答 |
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
| `/v1/files`, `/v1/batches`, `/v1/messages/batches` | 无：见[批量任务](#batches) | OpenAI 和 Anthropic SDK 的批量接口 |
| WebSocket `GET /v1/realtime?model=…` | OpenAI Realtime，逐条中转：见 [realtime](#realtime-and-websocket) | OpenAI Realtime SDK、语音智能体 |
| WebSocket `GET /v1/responses` | Responses 的 WebSocket 模式，中转或回 `426`：见 [realtime](#realtime-and-websocket) | 开了 WebSocket 的 Codex |
| `GET /v1/models`, `GET /v1beta/models` | 全部 | 几乎所有客户端 |
| `POST`/`DELETE` `/mcp`, `/mcp/<server>` | MCP Streamable HTTP | MCP 客户端（Claude Code、Codex、Cursor 等）；见 [MCP 服务](#mcp-servers) |
| `/native/<upstream>/<path>` 上的任意方法 | 无：原样转发 | 调用其他文件接口、微调或厂商独有接口的 SDK；默认关闭，见[原生直通](#native-passthrough) |

路由由各协议的编解码器 codec 声明（每个 `pkg/protocol/<name>` 里的 `Routes()`）。
服务端（`internal/server/routes.go`）能容忍常见的地址写错：同一条路由带不带
`/v1` 或 `/v1beta` 前缀、末尾多不多一个斜杠、带不带 Azure 风格的
`?api-version=`，都能应答。

- **Key** 可以放在 `Authorization: Bearer`、`x-api-key`、`x-goog-api-key` 或
  `?key=` 里（`plugins/auth/apikey`）。
- **模型列表**按请求要的格式返回：带 `?client_version=` 时给 Codex 的模型目录，
  带 `anthropic-version` 头时给 Anthropic 格式，`/v1beta/models` 给 Gemini 格式，
  其余给 OpenAI 的 `{"object":"list","data":[…]}`。只列出这把 key 能用的模型。
- **错误**：中转站自己产生的错误，用客户端协议的原生格式和信号返回（上下文
  超长、限流并带 `retry-after`、额度用完、过载），客户端会像直连厂商时一样去压缩
  上下文、退避重试或停下。同协议上游返回的错误原样透传。
- **模型名**由客户端决定，中转站从不偷换。想要跨模型自动兜底，由管理员定义一个
  [预设 preset](configuration.zh-CN.md#routing-models-and-presets)，客户端在模型
  列表里能看到它，自己明确选用。
- **WebSocket** 升级只在 `/v1/realtime` 和 `/v1/responses` 上提供
  （[realtime](#realtime-and-websocket)）；Responses 模式没法中转时，升级请求会
  收到 `426`，客户端退回 HTTP 流式。
- **多媒体类请求**（embeddings、生图、语音、rerank）和对话共用同一套 key、选路、
  重试、请求日志和计费。只有声明了对应能力（`embeddings`、`image_generation`、
  `image_edit`、`audio_transcription`、`speech`、`rerank`）的上游入口才会接这类
  请求，否则客户端收到 400 `capability_missing`。Embeddings 能在 OpenAI 和 Gemini
  之间互转（多条输入 ⇄ 一个批次，`dimensions` ⇄ `outputDimensionality`；上游返回
  浮点数时由中转站生成 base64；token id 形式的输入只能发给 OpenAI 风格的入口）。
  生图和语音只透传给同协议的上游。
- **Rerank**（`rerank` 能力）能在三种格式之间互转：通用格式（`/v1/rerank`：
  文档可以是字符串或 `{text, …}` 对象，支持 `return_documents`）、Cohere v2
  （`/v2/rerank`：只收字符串，结果里不带文档），以及 Hugging Face TEI 的
  `/rerank`（`{query, texts}` → `[{index, score}]`，只用于上游）。目标协议不支持
  这些字段时，中转站自己保留得分最高的 `top_n` 条，并从请求里把文档补回去；对象
  形式的文档发给只收字符串的上游时，只发它的文本。图片等非文本文档只透传给同协议
  上游。用量取上游报的 token 数或搜索单位（Cohere 的 `meta.billed_units`，或
  OpenRouter、Portkey 发的 `usage.search_units`）；TEI 不报用量，所以 token 数是
  估算的。
- **上传**（`/v1/images/edits`、`/v1/audio/transcriptions`、
  `/v1/audio/translations`）边收边转给上游，大小上限是 `limits.max_body_bytes`
  （超过返回 413）；压缩过的上传返回 415。如果 `model` 字段排在文件后面，中转站
  会先把请求体复制到临时文件，好读出模型名。边收边转的上传一旦开始发送，就不会再
  换上游重试；先复制到临时文件的可以重试。语音合成的音频和事件流响应都是边收边发
  给客户端。

<a id="claude-code"></a>
## Claude Code

协议：Anthropic messages。请求从你自己的机器发出。

```bash
plugrelay setup claude-code --key pr_xxx --url https://relay.example.com
```

会写进 `~/.claude/settings.json`（原文件备份为 `.bak-<time>`）：

```json
{"env": {"ANTHROPIC_BASE_URL": "https://relay.example.com",
         "ANTHROPIC_AUTH_TOKEN": "pr_xxx",
         "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"}}
```

加 `--model M` 还会设置 `ANTHROPIC_MODEL`。手动配置的话，导出同样的环境变量即可
（用 `ANTHROPIC_API_KEY` 也行，它会以 `x-api-key` 发出）。

- 同协议时 `anthropic-beta` 和 `anthropic-version` 原样转发，没有白名单，也就不会
  跟不上新 beta。不支持某个 beta 的上游，在自己的清单里（`unsupported_betas`）把它
  去掉。
- 上游是 Anthropic 时，`system` 数组、`cache_control`（包括 `ttl: 1h`）、
  `thinking`、`context_management` 和工具相关字段一个字节都不改，提示词缓存命中率
  和直连一样。
- "prompt is too long: N tokens > M maximum" 这条报错原样保留，会触发自动压缩上下文。
- 上游不是 Anthropic 时，思考签名会加密保存，只还给签发它的那个上游；不会出现
  "Invalid signature in thinking block"。
- 开了模型发现时，Claude Code 只列出 id 里含 `claude` 或 `anthropic` 的模型。其他
  模型用 `ANTHROPIC_MODEL`（以及小模型/快速模型那几个变量）指定。

<a id="cc-switch"></a>
### CC Switch

在控制台创建或轮换 key 之后，接入面板会马上给 Claude Code、Codex 和 Gemini CLI
各提供一个"导入 CC Switch"的链接（CC Switch 需要完整的 key，只有这时能拿到）。
它是 CC Switch 的 `ccswitch://v1/import` 供应商导入链接，带上中转站的站点名和地址、
key、`apiFormat`（`anthropic`、带 `/v1` 的 `openai_responses`、`gemini_native`），
Claude Code 和 Codex 还带上和接入片段一样的设置（`internal/admin/connect.go`，
`TestCCSwitchLinks`）。

<a id="codex"></a>
## Codex

协议：只用 OpenAI responses。请求从你自己的机器发出。

```bash
plugrelay setup codex --key pr_xxx --model gpt-5.5 --url https://relay.example.com
export PLUGRELAY_API_KEY=pr_xxx   # Codex 从这个变量读 key
```

会把中转站的模型目录存成 `~/.codex/plugrelay-models.json`，在
`~/.codex/config.toml` 里加一个供应商并选中它：

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

- 要像这样用自定义供应商，不要用 `openai_base_url`：用后者的话，Codex 会把中转站
  当成 OpenAI，打开 WebSocket（只有[下文](#realtime-and-websocket)那种情况能中转，
  否则回 `426` 退回 HTTP）、远程压缩上下文和请求压缩。
- 流一定以带用量的 `response.completed` 结束，Codex 自动压缩上下文要靠它；流中途
  断了会报成 `response.failed`。
- 新的输入项类型（`custom_tool_call`、`tool_search_call`、命名空间、压缩记录）同协议
  时原样透传；换到其他上游时，`reasoning.encrypted_content` 里装的是加密后的上游
  私有状态。
- 中转站会返回一个带签名的 `x-codex-turn-state` 头；Codex 在同一轮对话里会把它
  带回来，这样一轮对话固定用同一把凭据（保住提示词缓存）。
- Codex 不会从自定义供应商拉取模型元数据，而是读 `model_catalog_json` 指向的本地
  文件。没有这个文件，Codex 会警告 "Model metadata … not found"，然后用通用设置
  （没有自由格式的 `apply_patch`，上下文窗口用默认值）。`setup codex` 从
  `GET /v1/models?client_version=…` 下载这份目录（Codex 自己的格式，带 ETag）；文件是
  一份快照，加了模型之后要重新跑一次 `setup codex`。目录为空时 Codex 起不来，所以
  key 一个模型都看不到时 setup 会直接失败。
  每个条目都带一段不偏向任何厂商的 `base_instructions` 系统提示词，因为一个模型名
  背后可能是任何一家的模型。

<a id="gemini-cli"></a>
## Gemini CLI

协议：Gemini `generateContent`。Gemini CLI 没有可供 plugrelay 修改的配置文件，
`setup` 直接打印环境变量：

```bash
eval "$(plugrelay setup gemini --key pr_xxx --url https://relay.example.com --print)"
# export GOOGLE_GEMINI_BASE_URL=https://relay.example.com
# export GEMINI_API_KEY=pr_xxx
```

- key 通过 `x-goog-api-key` 传过来。
- 各个 part 上的 `thoughtSignature` 会保留；流的最后一块带 `finishReason`。
- 429 带 Google 风格的 `RetryInfo`；每日额度用完时会明确告知，客户端就不再重试。

<a id="cursor"></a>
## Cursor

协议：OpenAI chat。**请求从 Cursor 的服务器发出**，所以中转站需要一个公网 HTTPS
地址（站点设置 `site.public_base_url`），localhost 和内网地址都不行。在 Cursor 设置
里打开 *Override OpenAI Base URL*，填 `https://relay.example.com/v1` 和平台 key，
再把模型名加进去。

- Tab 补全始终用 Cursor 自己的模型。
- Agent 模式可能走 responses 接口；两种都支持。

<a id="github-copilot-vs-code"></a>
## GitHub Copilot (VS Code)

按模型不同，走 chat、responses 或 messages。在 `chatLanguageModels.json` 里加一个
*Custom Endpoint*，指向中转站，填上平台 key。Copilot 需要 `/v1/models`；不支持工具
调用的模型不会出现在 Agent 模式里。行内补全从不经过中转站。

<a id="cline--roo-code--kilo-code"></a>
## Cline / Roo Code / Kilo Code

OpenAI chat 或 Anthropic messages（Kilo 还支持 responses 和 Gemini）。在扩展设置
里选 OpenAI 兼容或 Anthropic 供应商，填 base URL（OpenAI 兼容填
`https://relay.example.com/v1`，Anthropic 填 `https://relay.example.com`）和 key。

- `max_tokens` 和 `max_completion_tokens` 都认，发给上游时会改成上游要的字段名。
- Roo 会在 OpenAI 风格的消息里带 `cache_control`；中转站把它当作缓存断点，按目标
  上游的缓存方式处理。
- 历史消息里带回来的 `reasoning_content` 会保留（DeepSeek 一类上游在工具调用轮次
  要求必须带上）。

<a id="opencode"></a>
## OpenCode

chat、responses 或 messages。

```bash
plugrelay setup opencode --key pr_xxx --model claude-sonnet-4-5,gpt-5.5 --url https://relay.example.com
```

会在 `~/.config/opencode/opencode.json` 里加一个 `plugrelay` 供应商
（`@ai-sdk/openai-compatible`，base URL 为 `…/v1`），带上列出的模型；用 `/models`
选择。OpenCode 只提供这里列出的模型，所以 `--model` 必填。带 `include_usage` 时，
最后一块会带用量和 `cached_tokens`；`prompt_cache_key` 会转发给上游。

<a id="cherry-studio--continue--jetbrains"></a>
## Cherry Studio / Continue / JetBrains

大多走 OpenAI chat：加一个 OpenAI 兼容供应商，base URL 填
`https://relay.example.com`（带不带 `/v1` 都行），再填 key。Continue 和 JetBrains
的代码补全走 `/v1/completions`（老式的文本补全接口，通常带 `suffix` 做中间填空
fill-in-the-middle）；把补全用的模型也指向同一个 base URL。如果模型路由到的上游
有 completions 入口（OpenAI、Azure OpenAI、DeepSeek、Mistral、vLLM、Ollama、
LM Studio、OpenRouter 以及大多数 OpenAI 兼容清单），请求一个字节不改地透传。带
`suffix` 的请求只发给清单里声明了 `fill_in_middle` 的入口（OpenAI、DeepSeek、
Mistral、Ollama、SiliconFlow）；一个都没有时，客户端收到一个指明缺
`fill_in_middle` 的 400，而不是一个无视光标后代码的补全结果。不带 suffix 时，只支持
chat 的上游也能接：提示词作为一条 user 消息发过去（评级为有损）。多条提示词的批量
请求、token id 形式的提示词、大于 1 的 `n`/`best_of`，以及 `echo`，只有 completions
入口能处理。对话客户端的请求从不会路由到 completions 入口。

<a id="trae"></a>
## Trae

OpenAI chat 或 Anthropic messages（不支持 responses）。Trae 按填的地址原样请求，所以
要填完整路径：`https://relay.example.com/v1/chat/completions`。请求是从你的机器发出
还是从 Trae 的服务器发出，目前没有确认；如果本地中转站连不上，就像 Cursor 那样用
公网地址。

<a id="playground"></a>
## Playground

控制台的 playground（`POST /api/v1/playground`）像客户端一样发一段对话，四种对话
协议任选，用调用者自己的某一把 key（管理员也可以不用 key，以自己的身份对话）。它走
的是正常的请求流程——限额、选路、计量、计费、请求日志都照常——User-Agent 是
`plugrelay-playground/1`，请求日志里会标成 playground（`TestPlaygroundEveryProtocol`）。

<a id="batches"></a>
## 批量任务

OpenAI Batch API（`POST /v1/files` 且 `purpose=batch`、`GET
/v1/files[/{id}[/content]]`、`DELETE /v1/files/{id}`、`POST`/`GET
/v1/batches`、`GET /v1/batches/{id}`、`POST /v1/batches/{id}/cancel`）和 Anthropic
Message Batches（`POST`/`GET /v1/messages/batches`、`GET`/`DELETE
/v1/messages/batches/{id}`、`POST …/cancel`、`GET …/results`），把 SDK 照常指向
中转站就能用（`internal/pipeline/batch.go`）。

- **放在哪**：一个批量任务落在某一把上游凭据上。中转站对输入文件（OpenAI：按第一
  行的 `body.model`）或 message batch（按 `params.model`）像对这个协议的普通请求一样
  选路，发给声明了 `batches` 的入口（官方的 `openai` 和 `anthropic` 清单都声明了），
  并记下每个文件、每个批量任务在哪把凭据上。之后的所有调用——查询、取消、取结果、
  删除——都发给那把凭据，即使路由规则后来改了也一样。只支持同协议：批量任务从不做
  协议转换。
- **一个批量任务只能一个模型**：每一行（每个请求）必须是同一个模型，OpenAI 还要求
  `url` 相同，否则返回 400。路由对应的上游模型名不同时，中转站会改写 `body.model` /
  `params.model`；否则每一行一个字节不改地透传。上传是边收边转给上游的
  （`DefaultBatchMaxBody`，512 MiB），每一行经过时检查，有一行不对就中止上传。
- **Id** 用上游的（`file-…`、`batch_…`、`msgbatch_…`）。只有所有者能看到一个对象——
  创建它的用户，key 不属于任何用户时就是那把 key；其他人一律按厂商的错误格式拿到
  404。列表取自中转站记录的调用者自己的对象，还没结束的会从上游刷新状态。message
  batch 的 `results_url` 会被改写成指向中转站（站点的公网 base URL，没设就用请求进来
  的地址），这样跟着这个链接走的 SDK 不会把平台 key 发给厂商。
- **计费**：结果按每一条成功请求的用量计费，记在批量任务创建者名下，用价格规则里的
  `batch_discount`（[价格](configuration.zh-CN.md#prices)），只计一次。计费时机是：
  第一次通过中转站完整读取结果时（输出文件的内容，或 `…/results`），或者由中转站
  自己读取——它每隔几分钟检查一遍还没计费的批量任务，结束超过十分钟的就去取结果，
  另外在删除操作会让结果读不到之前也会先取。客户端读到一半不读了，也不会漏计：
  剩下的中转站自己读完。批量调用不转发 `Range` 和条件请求头，所以下载总是完整的。
  启用 `meter.balance` 时，创建批量任务会预扣它的估算金额
  （[售卖套件](sell-kit.zh-CN.md#prepaid-balance-meterbalance)）；其他计量插件放行
  时预留为零。
- **请求插件**在创建批量任务时运行（上传输入文件、创建 batch 或 message batch），
  当作这批任务所用模型的一次请求：访问名单、禁用模型、限流照常拒绝，一个批量任务
  算一次请求。读请求体的插件（guardrails 内容审查）还会对批量里的每个请求都跑一遍，
  任何一条被拒，整个批量任务被拒，并指出是哪一行。改写请求体的插件像在同协议调用里
  一样作用到每个请求上：`middleware.redact`、`redact` 模式的 guardrails，以及
  `middleware.param-policy` 的上限，都会在每一行发往上游前写进去。如果请求插件改的是
  解码后的请求，批量任务会被拒，因为每一行是按自己原来的编码发往上游的；单次尝试
  钩子（`plugin.AttemptHook`）不在批量的行上运行。guardrails 的审核接口最多检查一个
  批量任务的前 100 个请求，超出部分按审核调用失败处理（`moderation_fail`）。读取、
  列表、取消、删除不运行任何请求插件。

<a id="realtime-and-websocket"></a>
## Realtime 和 WebSocket

有两种 WebSocket 接口，按消息逐条中转到上游的 WebSocket
（`internal/pipeline/websocket.go`）。Gemini Live（它自己的 WebSocket 协议，路径是
`…/ws/…BidiGenerateContent`）不支持。

- **OpenAI Realtime**：`wss://relay.example.com/v1/realtime?model=…`，用法同 OpenAI 的
  [WebSocket 指南](https://developers.openai.com/api/docs/guides/voice-websockets)
  （正式版接口；beta 客户端带的 `OpenAI-Beta: realtime=v1` 头原样转发，不过 OpenAI
  的迁移说明里让去掉它）。模型和其他请求一样选路，发给声明了 `realtime` 的入口：
  官方 `openai` 清单（`wss://api.openai.com/v1/realtime`）和 `azure-openai`
  （`wss://<resource>.openai.azure.com/openai/v1/realtime?model=<deployment>`，正式版
  路径，不带 `api-version`）。
- **Responses 的 WebSocket 模式**（`/v1/responses`，Codex 把中转站当成 OpenAI 时会用，
  [指南](https://developers.openai.com/api/docs/guides/websocket-mode)）：透传给声明了
  `responses_websocket` 的入口（只有官方 OpenAI）。模型要等第一个 `response.create`
  才知道，而那时上游早就得选好了；Codex 又只在握手返回 `426` 时才退回 HTTP，之后
  再出错不会退。所以只有当某一个这样的上游能服务这把 key 能用的全部模型时，中转站才
  接受升级，否则回 `426`，Codex 在这次会话剩下的时间里就用 HTTP。如果某个
  `response.create` 指定的模型那个上游不提供，会收到一个 `error` 事件（404），这条
  不转发。
- **Key**：`Authorization: Bearer`、其他常用请求头、`?key=`，或者——给没法设请求头的
  浏览器用——子协议 `openai-insecure-api-key.<key>`（大小写不限）。这个 key 会从客户端
  提供的子协议里去掉，从不转发；上游用的是供应商的凭据。交给浏览器的 key，应当是
  本来就可以公开的那种。
- **连上游**：在接受客户端升级之前，先通过核心的统一出口连上游（凭据的固定代理、
  SSRF 防护、连接超时、上游的额外请求头），所以握手格式不对（`Sec-WebSocket-Key`、
  版本 `13`）、路由出错、上游自己握手失败，都会以 OpenAI 格式的 HTTP 错误返回给
  客户端。上游的子协议和响应头（`x-codex-turn-state` 等）随 `101` 一起带回。凭据的
  并发名额在整个会话期间一直占着。
- **请求插件**在指明模型的调用上运行，和[批量任务](#batches)一样：一个 Realtime
  会话在握手时算一次请求，不带请求体（`middleware.access` 名单、`blocked_models`、
  `middleware.ratelimit`——它的并发名额整个会话期间占着，token 在会话结束时计入），
  之后客户端每个带文本的事件，都算作这个会话模型的又一次请求（计入 `rpm`，不再多占
  并发名额）；Responses 模式下每个 `response.create` 算一次带请求体的请求，被拒时
  返回 `error` 事件。访问控制里的来源地址名单和客户端名单在每次握手时也会检查。
  鉴权之前会丢掉 Cookie。
- **改写请求体**：Responses 模式下，每个改写请求体的插件都会像对 patch 档位的 HTTP
  尝试那样改写每个 `response.create`（只改它声明的路径），所以
  `middleware.param-policy` 会给 `max_output_tokens` 和 `reasoning.effort` 设上限、
  删掉参数；单次尝试钩子（`plugin.AttemptHook`）也会运行，可以拒绝这条；它们设的
  请求头会被丢掉，因为连接已经建立了。Realtime 上，`middleware.param-policy` 会在
  `session.update` 和 `response.create` 里压低 `max_output_tokens` /
  `max_response_output_tokens`（包括 `inf`）和 `reasoning.effort`，并删掉它管的参数；
  如果上游建好的会话允许的输出比上限多，中转站会自己给上游发一个 `session.update`，
  对应的 `session.updated` 客户端也会收到。客户端的事件要等上游的 `session.created`
  到了、而且这个 update 发出去之后才放行，所以客户端自己设的更低上限不会被覆盖；
  `response.create` 里的上限会覆盖会话的上限，它只会被压到上限值（上游一直不发
  `session.created` 的情况见 **限制** 一条）。单次尝试钩子不在 Realtime 上运行。
- **内容检查**（[guardrails](plugins.zh-CN.md#guardrails)、`middleware.redact`）读取
  客户端事件里的文本：Realtime 上是 `conversation.item.create`（消息文本、客户端提供
  的转写、函数调用的参数和输出）、`response.create`（`instructions`、`input` 项、工具名）
  和 `session.update`（`instructions`、工具名）；Responses 上是每个 `response.create`，
  按一个 HTTP `/v1/responses` 请求来读。被拦下的事件收到一个 `error` 事件，不转发；
  会话不断开。脱敏只改事件里用户消息的文本，然后再发往上游；其他文本只检查不修改，
  没有改动的事件一个字节不改地转发。每个事件单独检查：一个关键词被拆在两个事件里就
  匹配不到，`max_prompt_chars` 算的也是单个事件的文本，而不是上游那边累积的整段对话。
  guardrails 的审核接口在一个 Realtime 会话里最多检查 `max_moderated_events`（200）个
  事件，只算真的把用户文本送去审核的事件；超出部分按审核调用失败处理
  （`moderation_fail`）。和 HTTP 一样，存储提示词的变量（`session.prompt`、
  `response.prompt`、Responses 上的 `prompt`）不检查。
- **输出检查**：文本和音频转写的增量（`response.output_text.delta`、
  `response.text.delta`、`response.output_audio_transcript.delta`、
  `response.audio_transcript.delta`）和 HTTP 流走同一个检查窗口。`block` 模式下，
  只要一段文本可能是某个匹配的一部分，对应的帧就先压着不发；一旦匹配上，这个响应就
  停掉：客户端收到一个 `error` 事件，这个响应之后的帧、关于它输出项的事件都不会再
  收到，最后收到它的结束事件，其中 `output` 被清空。Realtime 上游会收到
  `response.cancel`；Responses 模式没有取消事件，上游会把响应跑完。两种情况都按用量
  计费。上游的 `error` 事件总会送到客户端，被停掉的响应的也一样。如果是对某个客户端
  事件的回应（带客户端的 `event_id`，或者是 `invalid_request_error`），立即发出，压着
  的输出继续压着；如果点名了某个被压着的响应，或者是别的故障，就结束所有压着的响应：
  把压着的文本当作完整文本检查一遍，匹配上就照上面的办法停掉；没问题就先放出这段
  文本，再发这个错误，这个响应之后的帧（包括结束事件）照常中转和检查。之后的响应
  照常中转。检查结论记在请求日志里：输出和 Responses 的输入记在对应响应的条目上，
  Realtime 的输入记在会话的条目上。
- **不检查的**：音频，两个方向都不查——被停掉的响应，它的语音输出可能已经说出了
  转写被拦下的那些内容。在 guardrails 设置 `block_unscannable_audio`（默认关闭）打开、
  模式为 `block` 并且启用了任一内容规则或审核时，音频输入（`input_audio_buffer.append`、
  `input_audio` 内容）会改为收到 `error` 事件（每个会话判断一次）。和 HTTP 一样，输出
  里的图片、思考过程和函数调用参数不检查；上游对输入音频做的转写也不检查。
- **事件**：客户端的文本事件必须是一个 JSON 对象，而且不能有重复的键（中转站读第一
  个，上游可能读最后一个），否则收到 `error` 事件，不转发。Responses 会话的第一个
  `response.create` 必须指定模型。Realtime 的 `session.update` 不能换模型；它或 beta
  的 `transcription_session.update` 指定的输入转写模型（`audio.input.transcription.model`、
  beta 的 `input_audio_transcription.model`）按请求模型同样把关：这把 key 必须能看到它，
  请求插件必须放行，启用计量时还必须有价格；启用计量时，设置了转写却没指定模型也会
  被拒。不满足就给这个 update 回一个 `error` 事件。在转写会话
  （`intent=transcription`）上，如果启用了计量或模型可见性规则，那么在会话的转写模型
  （指定的，或上游报告的）通过同样的检查之前，音频输入（`input_audio_buffer.append` /
  `commit`）都会收到 `error` 事件。
  `event_id` 不能以 `event_relay_guard_` 开头，这个前缀留给中转站自己的事件用。
- **计费**：每个响应开始前，计量插件先预扣——它的输出上限（`max_output_tokens`；
  Realtime 用会话的 `max_output_tokens` / `max_response_output_tokens`，没有就用
  [`websocket.response_reserve_tokens`](configuration.zh-CN.md#websocket)）按模型的输出
  价，加上它的输入估算（见下）按输入价——所以并发会话的预扣会累加；按 token 计的套餐
  预扣的是同样的输入估算加输出上限的 token 数。会话建立之前被拒，返回 HTTP 错误；
  Responses 的 `response.create` 被拒，返回 `error` 事件；Realtime 上，等正在跑的响应
  结束后，客户端收到一个 `error` 事件（`insufficient_quota`），会话以 `1008` 关闭。每个
  `response.done`（Realtime）或 `response.completed` / `failed` / `incomplete`
  （Responses）按它的 `usage` 结算，并作为一条单独的请求记日志：缓存的 token 按缓存
  读取计，其余输入和输出按模型的 token 价计。Realtime 的音频、文本、图片 token 不分开
  定价，它们的拆分留在原始用量里，所以给 realtime 模型定价时要按它实际的混合比例来。
  输入音频转写（`conversation.item.input_audio_transcription.completed`）按会话报告的
  转写模型（`session.*` 或 `transcription_session.*`）作为一次请求记日志并计费：按它的
  token 价计 token，或者按它的 `audio_second` 单价计秒数。
- **没跑完的响应**：客户端离开时还有响应在跑，上游连接最多再保持 10 秒等它的用量。
  到时仍没结束的响应，或者遇到上游连接断开、会话超时、中转站关闭，都按估算计费，并在
  日志里标为 `estimated` 用量：输出按已经中转的文本和转写增量算；输入在 Responses 上和
  HTTP 请求一样算（文本四个字节算一个 token，一张图算 800 token）；在 Realtime 上算到
  目前为止的整段对话——客户端加入的文本四个字节一个 token，输入音频每秒约 10 token，
  并且至少不低于最近一次上报用量里的输入加输出。
- **日志**：每个会话一条（协议 `openai-realtime` 或 `openai-responses-ws`，状态 `101`，
  时长，双向字节数，响应数、转写数和关闭原因记在诊断备注里），另外每个响应、每次转写
  各一条，带它的用量和扣费。
- **限制**（[配置](configuration.zh-CN.md#websocket)）：会话超过 `websocket.max_session`
  结束（先发 `error` 事件 `session_expired`，再以 `1000` 关闭），或者在
  `websocket.idle_timeout` 内双向都没有消息时结束（`1000`）；Realtime 会话等上游的
  `session.created` 超过 `websocket.session_ready_timeout` 时，收到 `error` 事件
  `session_not_ready`，然后以 `1013` 关闭；
  单条消息超过 `websocket.max_message_bytes` 时以 `1009` 关闭。任何一方关闭，都带着它的
  关闭码转给另一方；上游连接丢失时以 `1014` 关闭客户端，中转站关机时用 `1001`，中转站
  内部故障用 `1011`。

<a id="native-passthrough"></a>
## 原生直通

`/native/<upstream>/<path>` 把任意方法、路径、查询参数和请求体转发给某一个上游，用来
调用 plugrelay 没有编解码器的接口：非批量的文件接口、微调、厂商独有的接口。默认关闭，
要在 relay.yaml 里设置 `native.enabled`，并给 key 授权这个上游
（[配置](configuration.zh-CN.md#native)）；没开时这个路由返回 404，没授权的 key 返回 403。

- **地址**：`<upstream>` 是上游 id。`<path>` 拼在它第一个入口的 base URL 后面，这个
  base URL 通常以 `/v1` 结尾，所以指向 `https://relay.example.com/native/openai` 的 SDK
  请求 `https://relay.example.com/native/openai/files` 时，中转站请求的是
  `https://api.openai.com/v1/files`。用 `<upstream>:<endpoint>` 选别的入口
  （`/native/deepseek:completions/…`）。上游不存在返回 404，已停用返回 403。
- **凭据**：平台 key 放在任何常用请求头里都行（或者 `?key=`，转发时会从查询参数里
  去掉），从不转发，客户端带的其他凭据也一样不转发。中转站从这个上游里挑一把调用者
  能用的凭据（冷却中、被占住、被隔离、已满的凭据跳过；一把都不剩时返回 429 或 503），
  由供应商插件加上它的鉴权。核心出口的规则照样生效：危险请求头丢掉、凭据的固定代理、
  SSRF 防护、上游的额外请求头和超时。
- **授权范围**：给了原生直通权限，等于把这把上游凭据在那个主机上能做的一切都交给了
  调用者，而不只是调模型——对 Vertex 是一个 `cloud-platform` 令牌，对 OpenAI 是整个
  账号的 API（文件、微调、删除）。按 key 单独授权，只给那些你本来就愿意把凭据本身交给
  他们的调用者。解码后是 `.` 或 `..` 的路径段会被拒（400），所以 base URL 的路径前缀
  不会被绕过。
- **请求体和响应体**双向流式传输，不缓存，每个请求上限 `native.max_body_bytes`；响应——
  JSON、二进制或 SSE——带着上游的状态码和响应头返回。
- **不生效的**：请求插件（限流、额度、脱敏、guardrails）和计量。用量未知，所以不计费；
  失败也从不影响凭据的健康状态。`middleware.access` 的 IP 和客户端白名单仍然生效。
- **日志**：记一条请求日志，协议 `native`，处理档位 `passthrough`，带状态码、请求和
  响应字节数、时长；方法和路径记在这次尝试的诊断备注里。

<a id="mcp-servers"></a>
## MCP 服务

中转站的 MCP 网关（[mcp.md](mcp.zh-CN.md)）对每把 key 来说就是一个 Streamable HTTP
服务：`https://relay.example.com/mcp` 提供这把 key 被授权的所有工具，命名为
`<server>__<tool>`；`/mcp/<server>` 只提供一个服务。客户端不用装任何东西，各个服务自己
的凭据留在中转站上。`plugrelay setup` 不会注册它——Claude Code 把用户级 MCP 服务存在
`~/.claude.json` 里，这个文件它自己会改写——所以要手动加。

Claude Code（[文档](https://code.claude.com/docs/en/mcp)，2026-11-06 核对）：

```bash
claude mcp add --transport http plugrelay https://relay.example.com/mcp \
  --header "Authorization: Bearer pr_xxx"
```

或者针对单个项目写 `.mcp.json`（`${VAR}` 会从环境变量展开）：

```json
{"mcpServers": {"plugrelay": {"type": "http", "url": "https://relay.example.com/mcp",
                              "headers": {"Authorization": "Bearer ${PLUGRELAY_API_KEY}"}}}}
```

Claude Code 里这些工具显示为 `mcp__plugrelay__<server>__<tool>`。

Codex（[文档](https://developers.openai.com/codex/mcp)，2026-11-06 核对），写在
`~/.codex/config.toml` 里：

```toml
[mcp_servers.plugrelay]
url = "https://relay.example.com/mcp"
bearer_token_env_var = "PLUGRELAY_API_KEY"
```

其他客户端在它们的"远程 / Streamable HTTP MCP 服务"设置里填同样的地址和
`Authorization` 请求头即可。

<a id="what-every-client-loses-behind-a-relay"></a>
## 走中转站后所有客户端都会少掉的东西

- 必须连官方端点的厂商独有功能：Cursor Tab、Copilot 行内补全、Zed 的编辑预测、
  Claude Code 的细粒度工具流式输出和提示词缓存作用域相关功能。
- Codex 走自定义供应商时：WebSocket、远程压缩上下文和网页搜索扩展都关掉了；本地压缩
  上下文照常可用。
- 上游缺少、而且没有哪个转换步骤能补上的能力（把图片发给只收文本的入口、请求强制要用
  某个服务端工具而没有插件来执行它），会让中转站跳过这个上游；只是"提供"而没强制的
  服务端工具（Codex 的 `web_search`）会被去掉，请求日志里列为已丢弃；如果一个上游都
  不剩，客户端收到一个指明缺少哪项能力的 400——绝不悄悄丢掉。
- 走 OpenAI chat 时，来自另一种协议的上游私有状态带不过去（这个协议没有回传字段）；
  这种情况下跨轮次的思考连贯性会丢失，请求日志会写明。
