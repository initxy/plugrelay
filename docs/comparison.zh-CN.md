# plugrelay 与同类项目对比

[English](comparison.md) | 简体中文

和大家通常拿来对比的中转、网关项目做一个实事求是的比较。其他项目的情况于
**2026-11-05** 对照各项目自己的仓库或文档核实过；每条说法都带来源标记（[S1] …），
对应的出处见[来源](#sources)。plugrelay 的说法都指向本仓库里的代码或测试，于
**2026-11-08** 对照代码核实过。"unknown" 表示对方项目自己的资料没提，不代表没有这个
功能。

- [概览](#at-a-glance)
- [也调研过的项目](#also-surveyed)
- [协议与转换](#protocols-and-conversion)
- [编程客户端](#coding-clients)
- [提示词缓存粘性](#prompt-cache-stickiness)
- [上游私有的推理状态](#private-reasoning-state)
- [扩展方式](#extension-model)
- [存储与集群](#storage-and-clustering)
- [计费](#billing)
- [控制台](#console)
- [部署与许可证](#deployment-and-license)
- [plugrelay 同样具备的](#what-plugrelay-matches)
- [plugrelay 领先的地方](#where-plugrelay-is-ahead)
- [别人领先的地方](#where-others-are-ahead)
- [plugrelay 已知的不足](#known-gaps-in-plugrelay)
- [来源](#sources)

<a id="at-a-glance"></a>
## 概览

| | star 数 · 许可证 · 最新版本 [S0] | 语言 | 定位 |
|-|-|-|-|
| new-api | 51.5k · AGPL-3.0（另加 §7 署名条款） · v1.0.0-rc.48，2026-11-04 | Go + React | 多租户网关和转售站 [S1] |
| one-api | 37.3k · MIT · v0.6.10，2025-02-02；最后一次推送 2026-01-09 | Go + JS | 密钥管理和二次分发 [S5] |
| sub2api | 45.5k · LGPL-3.0 · v0.2.19，2026-10-28 | Go + Vue | 共享订阅账号额度 [S6] |
| LiteLLM proxy | 62.8k · `enterprise/` 以外为 MIT · v1.108.1，2026-11-05 | Python（+ Rust 内核） | 企业网关，100+ 家服务商 [S9][S10] |
| claude-code-router | 38.0k · MIT · v3.1.3，2026-10-09 | TypeScript、Electron | 本地单用户的编程智能体管控工具 [S16] |
| Portkey gateway | 13.3k · MIT · v1.15.2，2026-01-12 | TypeScript | 带内容护栏的应用网关 [S18] |
| Bifrost | 9.4k · Apache-2.0 · HTTP 网关 `transports/v2.4.0`，2026-10-29（GitHub 上标为 "latest" 的是一个 Helm chart 标签） | Go | 高吞吐应用网关 [S19] |
| OpenRouter | 托管服务，不能自部署 | — | 托管的模型市场 [S20] |
| plugrelay | 首个公开版本 · Apache-2.0（[`LICENSE`](../LICENSE)） | Go + React | 面向编程客户端，自部署 |

<a id="also-surveyed"></a>
## 也调研过的项目

还看过自部署中转 CLIProxyAPI、claude-relay-service、gpt-load、axonhub、octopus 和
CliRelay，以及托管网关 Helicone、Vercel AI Gateway 和 Cloudflare AI Gateway。这里不逐项
对比它们：它们的说法没有对照各自的出处核实过。这些托管网关可以作为上游接在 plugrelay
后面：OpenRouter、Vercel AI Gateway、Cloudflare AI Gateway、Helicone、Portkey（托管版
和自部署版）、LiteLLM、Bifrost、New API 和 One API 都有官方清单（`manifests/`、
`TestManifestConformance`）。

<a id="protocols-and-conversion"></a>
## 协议与转换

对编程客户端来说关键的问题是：说协议 A 的客户端能不能用说协议 B 的上游？同协议的流量
会不会原样不动？

| | 客户端侧协议 | 跨协议转换 | 同协议处理 |
|-|-|-|-|
| new-api | OpenAI chat、Responses、Anthropic messages、Gemini；另有 realtime/WebSocket、图片、音频、embeddings、rerank [S1] | 四种文本协议之间通过 RelayKit 模块互转；"protocol-specific tools and fields may not map exactly"（协议特有的工具和字段可能无法精确对应） [S1] | unknown |
| one-api | OpenAI 格式 [S5] | 每家服务商一个适配器，转成 OpenAI 格式 [S5] | 不适用 |
| sub2api | Anthropic messages、Responses（含 Codex WebSocket 入口）、chat、Gemini；xAI 和 Antigravity 路由 [S6] | 部分桥接，例如 `/v1/messages` 转成 xAI Responses [S6] | unknown |
| LiteLLM | 以 OpenAI 格式为主；也有 `/responses`、`/messages`、embeddings、图片、音频、batches、rerank、A2A [S9] | 全部经过它的 OpenAI 形状接口 [S9] | unknown |
| claude-code-router | OpenAI chat / Responses、Anthropic messages、Gemini generateContent [S16] | 支持，在上述协议之间 [S16] | unknown |
| plugrelay | Anthropic messages、Responses、OpenAI chat、Gemini、旧版 `/v1/completions`；OpenAI 和 Gemini embeddings，OpenAI 图片和音频；Jina/Cohere-v1 形状（`/v1/rerank`）和 Cohere v2（`/v2/rerank`）的 rerank；OpenAI files/batches 和 Anthropic message batches（`pkg/protocol/*`、`internal/pipeline/batch.go`，路由见 [clients.md](clients.zh-CN.md#what-the-relay-serves)） | 先转成与协议无关的中间表示，只对两边能力不同的部分做通用转换步骤；每种转换分为无损 / 有损 / 不建议 / 不可能四档，判为不可能的端点永远不会选中（`internal/convert`、`TestAssessMatrix`、`TestCapabilityFiltering`、`TestCrossAnthropicToChatStream`）；embeddings 支持 OpenAI ⇄ Gemini 互转（`TestEmbeddingsOpenAIToGemini`、`TestEmbeddingsGeminiToOpenAI`）；rerank 在两种客户端形状和 Hugging Face TEI 的 `/rerank` 之间互转（`TestRerankToTEI`）；图片、音频和 batches 只支持同协议 | 优先逐字节直通，其次是只改个别字段的修补档（`TestPassthroughByteExact`、`TestPatchChangesOnlyModel`、`TestSameProtocolPreferred`） |

plugrelay 的媒体路由和 chat 共用密钥、路由、重试、请求日志和计费；模型只会路由到声明了
这个操作的端点（`TestMediaCapabilityMissing`），上传走流式（`TestTranscriptionUploadStreams`、
`TestStreamedUploadNotRetried`），图片按张计费（`TestImagesBilledPerImage`、
`TestPriceMediaUnits`）。rerank 只路由到声明了它的端点（`TestRerankCapabilityMissing`），
按 token 或搜索单位计费（`TestRerankSearchUnitBilling`）；有 19 份清单声明了 rerank
（`manifests/*.yaml`、`TestManifestConformance`）。batch 一直留在创建它的那个上游凭据上，
结果第一次经由中转读取时按价格规则里的 `batch_discount` 计费一次
（`TestOpenAIBatchLifecycle`、`TestAnthropicMessageBatchLifecycle`）；在别处取走的结果
不计费。OpenAI Realtime（`/v1/realtime`）和 Responses WebSocket 模式会中转到声明了它们
的端点，按每个 response 预扣，再按 `response.done` / `response.completed` 里的用量结算，
输入语音转写也算在内；客户端或上游中途断掉的 response 按估算计费
（`TestRealtimeRelaysAndMeters`、`TestResponsesWebSocket`、
`TestWebSocketClientLeavesMidResponse`、`TestRealtimeTranscriptionBilled`）；访问名单和
限流照样生效（`TestWebSocketRequestPlugins`）；除非只有一个上游能服务所有模型，否则
Responses 模式回 `426`；不提供 Gemini Live（[clients.md](clients.zh-CN.md#realtime-and-websocket)）。
还没对接真实的 realtime 上游跑过：那需要付费的 OpenAI 或 Azure OpenAI 账号。

<a id="coding-clients"></a>
## 编程客户端

| | Claude Code | Codex | Gemini CLI | Cursor | Continue / 其他 |
|-|-|-|-|-|-|
| new-api | 支持；渠道亲和规则会透传 Claude CLI 的请求头 [S2] | 支持；透传 Codex 的 session/turn 请求头 [S2] | 提供 Gemini 端点 [S1] | unknown | unknown |
| sub2api | 支持，含 Antigravity 端点 [S6] | 支持；会话粘性需要 nginx 开 `underscores_in_headers` [S6] | Gemini 端点 [S6] | unknown | unknown |
| LiteLLM | 有文档，每天自动跑一份按服务商划分的 Claude Code 兼容性矩阵 [S13] | 有文档 [S14] | 有文档 [S14] | 尽力支持，"reverse engineering their APIs"（靠逆向它的 API）；Cursor CLI 无法指向它 [S14] | unknown |
| claude-code-router | 为 Claude Code、Codex、Grok CLI、Kimi CLI、Kilo Code、OpenCode、Pi 等提供配置模板 [S16] | 支持 [S16] | 不在它的客户端列表里 [S16] | 不在它的客户端列表里 [S16] | — |
| OpenRouter | 有 Anthropic 兼容端点；"only guaranteed to work with the Anthropic first-party provider"（只保证在 Anthropic 官方服务商上可用） [S20] | unknown | unknown | unknown | unknown |
| plugrelay | `plugrelay setup claude-code`；应答 `/api/hello` 探测；`anthropic-beta` 原样转发（`internal/cli/setup.go`、`TestClientConfigWriters`、`TestClaudeCodeRequest`） | `plugrelay setup codex` 写入 provider 和模型目录；用请求头里的回合令牌把同一轮的请求固定在同一个上游凭据上（`TestSetupCodexWritesCatalog`、`TestTurnToken`） | `plugrelay setup gemini`（`TestSetupNewFilesAndGemini`） | 文档说明手动配置（[clients.md](clients.zh-CN.md#cursor)） | Cline、OpenCode（有 setup 命令）、Continue、Copilot、Trae 有文档（[clients.md](clients.zh-CN.md)）；提供 Continue 用的 `/v1/completions` 和中间补全（fill-in-the-middle）（`TestCompletionsFIMPassthrough`、`TestCompletionsToChatStream`） |

plugrelay 的端到端测试对接真实上游调用每种客户端协议（`test/e2e/e2e_test.go`）；另外
可以选择开启，让真的 Claude Code 和 Codex CLI 经由一个真实中转完成一个智能体任务，路由
分两条：同协议路由，以及只有 chat、必须转换的路由（`test/e2e/clients_test.go` 里的
`TestClients`；只有 chat 的那条路由用的是那里定义的测试专用清单 `e2e-chat-only`）。最近
一次在 2026-11-06 运行，4/4 通过；但这些测试是按需跑、只对接一个上游，结果也没有公开发布。
这方面 LiteLLM 按服务商划分的 Claude Code 矩阵 [S13] 领先。大约九次完整的真实运行里出现过
一次 `TestRelay/openai_chat*` 失败，没能复现。

<a id="prompt-cache-stickiness"></a>
## 提示词缓存粘性

在多个凭据或账号之间轮询会丢掉服务商的提示词缓存，这是编程流量走中转更贵的主要原因。

| | 机制 |
|-|-|
| new-api | "渠道亲和"规则，按请求头、上下文值或 JSON 路径匹配，带 TTL，分严格和优先两种模式 [S2] |
| sub2api | 选账号时做会话粘性 [S6] |
| LiteLLM | 可选的 `prompt_caching` 调用前检查，把后续请求路由到写入缓存的那个部署 [S11] |
| claude-code-router | unknown（单用户、本地） |
| plugrelay | 只在上一轮读或写了缓存时保留绑定（5 分钟；上一轮写的是 1 小时缓存则为 1 小时）；每个用户最多固定在 3 个最近用过的凭据上；权重只用来分配新会话；跨协议时，缓存步骤会补上缓存断点或缓存键（[architecture](architecture.zh-CN.md#routing-stickiness-and-failure-verdicts)；`TestStickinessWindow`、`TestUserRecentCredential`、`TestWeightedSplitOnlyForNewSessions`、`TestCachePlacement`、`TestCacheModes`） |

<a id="private-reasoning-state"></a>
## 上游私有的推理状态

thinking 签名、Responses 的 `encrypted_content` 和 Gemini 的 thought 签名只在签发它们的
上游（往往还限定同一个组织）那里有效。发到别处，请求就会失败。

| | 做法 |
|-|-|
| new-api | Gemini 有一个针对函数调用 thought 签名的设置，默认开启 [S3]；通用处理方式 unknown |
| sub2api | 对严格的 Anthropic 上游去掉没有有效签名的 thinking 块，对要求回传的上游保留；按模型名前缀识别模型家族 [S7]；Anthropic 和 Antigravity 的 Claude "cannot be mixed within the same conversation"（不能在同一段对话里混用） [S6] |
| LiteLLM | `encrypted_content_affinity` 把签发它的部署编码进 Responses 条目 id，再把后续请求路由回去，是 2026-02 一次事故之后加的 [S12]；跨协议处理方式 unknown |
| claude-code-router | unknown |
| plugrelay | 所有上游私有状态都用主密钥做 AES-256-GCM 加密封装，标上签发方，只回传给签发方，发往别处时直接静默丢弃；只要客户端协议里有回传字段，跨协议也能用（[architecture](architecture.zh-CN.md#private-state-sealing)；`TestSealedReasoningRoundTrip`、`TestOpaqueHistory`、`TestOpaqueResponseRoundTrip`、`TestTamper`、`TestWrongKey`）。OpenAI chat 没有回传字段，所以走这个协议时状态会丢失 |

<a id="extension-model"></a>
## 扩展方式

| | 怎么扩展 |
|-|-|
| new-api | 用 JavaScript 写任务插件，对接异步图片/视频 API，控制台里有插件市场 [S1]；新的文本上游要在 `relay/` 里写 Go 代码 [S1] |
| one-api | Go 代码 [S5] |
| sub2api | 进程外插件，走 gRPC，打包成 `.s2plugin`，带沙箱化的 UI；目前定义的唯一能力是出站传输，也没有发布官方示例 [S8] |
| LiteLLM | Python 回调和钩子（调用前、内容审核、调用后、流式） [S15]；用 Python 写自定义服务商 [S9] |
| claude-code-router | JavaScript 插件（`plugin.json` + 模块），有包装插件和核心网关插件，还有一份插件市场 schema [S16][S17] |
| Bifrost | Go 和 WASM 中间件插件；文档首页说自定义插件属于开源版 [S23]，README 却说属于企业版 [S19] |
| plugrelay | 八个扩展点（`pkg/plugin/plugin.go` 里的 `Kind`），有三种接入方式：上游用 YAML 清单（[`manifests/`](../manifests) 里有 108 份，`TestOfficialManifestsValidate`、`TestManifestConformance`）；外部进程走 JSON-RPC/stdio，[`sdk/`](../sdk) 里有 Go SDK（`internal/extplugin`、`TestCrashRestart`）；或者内置的 Go 代码。插件不往控制台加 UI；不做插件市场、WASM 插件和签名包，这是有意的决定。详见 [plugins.md](plugins.zh-CN.md) |

<a id="storage-and-clustering"></a>
## 存储与集群

| | 业务数据 | 共享运行时状态 / 集群 |
|-|-|-|
| new-api | SQLite、MySQL ≥ 5.7.8、PostgreSQL ≥ 9.6；日志可单独放一个库，支持 ClickHouse [S1] | 用 Redis 共享限流；多节点共用主库和密钥 [S1] |
| one-api | SQLite、MySQL；PostgreSQL "适配中" [S5] | 多节点靠共享 MySQL + Redis + `SYNC_FREQUENCY` 轮询 [S5] |
| sub2api | 必须 PostgreSQL 15+ [S6] | 必须 Redis 7+ [S6] |
| LiteLLM | PostgreSQL 存密钥、团队、预算、用量 [S15] | Redis；有拆分组件部署用的 Terraform 模块 [S9] |
| claude-code-router | SQLite（`packages/core/src/storage/sqlite-native.ts`） [S17] | 不适用（本地） |
| Bifrost | 配置库和日志库 [S19] | 集群是企业版功能 [S19][S23] |
| plugrelay | SQLite（纯 Go，默认）、PostgreSQL 或 MySQL 8（`internal/store`、`internal/sqldialect`、`TestSQLite`、`TestPostgres`、`TestMySQL`）；支持 YAML 导出/应用（`internal/store/yamlio.go`） | 内存、SQL 或 Redis（`internal/state`、`TestMemory`、`TestRedis`）；路由健康状态在节点间共享，并在控制台实时展示（`TestConsoleShowsRouterLiveState`） |

<a id="billing"></a>
## 计费

| | 计量单位 | 支付 |
|-|-|-|
| new-api | 额度和订阅，按表达式定价 [S1] | Stripe、易支付 EPay、Creem、Waffo 的充值和订阅处理 [S4] |
| one-api | 点数：额度 = 分组倍率 × 模型倍率 × (提示 + 补全 × 补全倍率)；可选按美元显示 [S5] | 只有一个充值链接 [S5] |
| sub2api | token 级成本计算 [S6] | 内置：EasyPay、支付宝、微信支付、Stripe [S6] |
| LiteLLM | 按密钥、用户、团队统计美元花费，带预算 [S9][S15] | 代理里没有（其他地方 unknown） |
| claude-code-router | 每个请求的估算成本，本地限额 [S16] | 没有 |
| plugrelay | 真钱：价格按每百万 token 计，单位是站点货币的百万分之一（`internal/store/types.go` 里的 `PriceRule`）；媒体按张、按音频秒数或按千字符计（`TestPriceMediaUnits`）；每次发出去的尝试都计成本，默认只对最终交付的那次扣费；密钥/用户/分组预算（`plugins/meter/quota`、`TestCostRecordedForFailedSentAttempt`、`TestMeterSettleOrRefundExactlyOnce`、`TestSettleRefundIdempotent`） | 计费和注册功能编译在内、默认关闭（`TestSellKitOffByDefault`）：预付余额，先预扣再确认扣款/释放，并发下也绝不透支（`plugins/meter/balance`、`TestReserveSettleRefund`、`TestConcurrentReserveNeverOverdraws`、`TestLedgerSQLite`、`TestLedgerPostgres`）；兑换码（`plugins/admin/redeem`、`TestBatchAndRedeem`）；Stripe Checkout 充值（`plugins/admin/payment/stripe`、`TestCheckoutAndWebhook`）；人民币站点可用易支付（epay）充值，走支付宝和微信支付（`plugins/admin/payment/epay`、`TestSubmitCheckoutAndNotify`）；订阅套餐，每个周期给一笔金额或 token 额度，可从余额扣、走 Stripe 订阅或走易支付（`plugins/admin/subscriptions`、`TestSubscriptions`、`TestSellKitStripeSubscription`、`TestSellKitEpaySubscription`）；充值优惠券、每日签到和邀请奖励记入流水账，公告（`plugins/admin/rewards`、`plugins/admin/announcements`、`TestCoupons`、`TestCheckIn`、`TestReferrals`、`TestSellKitCheckInOncePerDay`、`TestSellKitCouponAndReferral`、`TestSellKitAnnouncements`）；自助注册（`plugins/admin/signup`、`TestRegister`、`TestInviteRequired`）；new-api / one-api 导入工具，导入后原有密钥照常可用（`internal/importnewapi`、`TestDryRunThenImport`）；真实环境测试：`TestSellKitBalance`。Stripe 和易支付只对假服务器测过。详见 [sell-kit.md](sell-kit.zh-CN.md) |

<a id="console"></a>
## 控制台

| | 控制台 |
|-|-|
| new-api | React；用量看板、插件市场、模型广场、playground；七种语言 [S21] |
| sub2api | Vue 管理后台；一键自更新，可回滚；可用 iframe 嵌入外部系统 [S6] |
| LiteLLM | 管理界面，含密钥、团队、预算、内容护栏、请求日志 [S9] |
| claude-code-router | 桌面应用（macOS、Windows、Linux）外加浏览器界面 [S16] |
| Portkey | 本地日志控制台 [S18] |
| plugrelay | 内置在二进制里，菜单按角色和已开启的功能显示；初始化向导问清中转打算怎么用，然后打开对应功能（`TestSetupSellEnablesSellKit`）；功能页里每个功能一个开关、一张表单，外加扩展（`internal/features`、`TestRegistry`）；总览页按客户端统计花费；从可搜索的服务商列表里添加上游，同一步填凭据（`web/src/features/upstreams`、`TestConsoleCreatesUpstreamWithManifestVariables`）；模型带预设；请求日志记下每次上游尝试，有会话视图和 CSV 导出（`TestSessionsDefaultSpan`、`TestAnalytics`）；用量分析（星期 × 小时分布、按原因统计失败）；playground（`TestPlaygroundEveryProtocol`）；通知；公开的模型价格页和状态页（`TestPublicPages`）；计费功能开启时有计费页面（`web/src/features/billing`）；凭据实时状态（`TestConsoleShowsRouterLiveState`）；单点登录，支持 OIDC（Google、Microsoft Entra、Keycloak、Authentik 或任何 issuer）、GitHub OAuth 和飞书/Lark（`internal/admin/sso_*.go`、`TestSSO`、`TestIdentitiesSQLite`、`TestUserResetPassword`）；TOTP 两步验证，带恢复码、通行密钥（passkey），以及管理员可设的两步验证策略（`internal/admin/mfa*.go`、`TestMFA`）；登录多次失败后要求验证码；只有英文和简体中文，两边的键完全一致（`web/src/i18n/i18n.test.ts`），后端错误也有两种语言（`TestErrorCodesTranslated`）；只使用公开 API（`internal/admin/openapi_test.go`）；`plugrelay demo` 可以起一份预置数据的本地副本（`TestSeed`） |

<a id="deployment-and-license"></a>
## 部署与许可证

| | 部署 | 许可证 |
|-|-|-|
| new-api | Docker、Compose（带 PostgreSQL + Redis）、Electron 桌面封装 [S1] | AGPL-3.0 加署名条款；改过的界面必须保留一个可见链接 [S1] |
| sub2api | 安装脚本 + systemd、Compose、Apple container；需要 PostgreSQL 和 Redis [S6] | LGPL-3.0 [S0] |
| LiteLLM | pip/uv、Docker、Terraform [S9] | `enterprise/` 以外为 MIT [S10]；从 v1.76.0 起 SSO 5 个用户以内免费，超过要企业许可证 [S22] |
| claude-code-router | 桌面安装包、npm CLI、Docker [S16] | MIT [S0] |
| plugrelay | 单个二进制，内嵌控制台；Docker 镜像；Compose（[deployment.md](deployment.zh-CN.md)、[`deploy/`](../deploy)、[`.goreleaser.yaml`](../.goreleaser.yaml)）；用 SQLite 时什么都不用另装 | Apache-2.0 |

<a id="what-plugrelay-matches"></a>
## plugrelay 同样具备的

下面这些控制台和运维功能，这里都有：

- 从可搜索的服务商列表里添加上游，同一步填凭据
  （`web/src/features/upstreams/add-upstream-page.tsx`）；
- 识别每个请求背后的客户端程序，按客户端统计花费，会话视图，日志导出 CSV
  （`pkg/clientid`、`TestDetect`、`TestSessionsDefaultSpan`、[api](api.zh-CN.md#request-log-analytics)）；
- 接入代码片段旁边有 CC Switch 导入链接（`TestCCSwitchLinks`）；
- 通过邮件、飞书/Lark、钉钉、企业微信和 webhook 发通知
  （`TestNotificationsEndToEnd`、[notifications](notifications.zh-CN.md)）；
- 每种客户端协议的模型列表里都有预设
  （[configuration](configuration.zh-CN.md#routing-models-and-presets)）；
- 从 models.dev 同步价格，手动设的价格优先（`TestPriceSync`）；
- playground 走正常的请求链路（`TestPlaygroundEveryProtocol`）；
- 公开的价格页和状态页（`TestPublicPages`）；
- 售卖相关：订单页、优惠券、每日签到、邀请奖励和公告
  （[sell kit](sell-kit.zh-CN.md#coupons-check-in-referrals-announcements)）。

"具备"只是说功能存在；这些功能都还没经过生产流量的检验。

<a id="where-plugrelay-is-ahead"></a>
## plugrelay 领先的地方

范围很窄，只列代码或测试能证明的；成熟度不在其中。

- **跨协议保存上游私有的推理状态。** 对每种有回传字段的客户端协议，都加密封装并只回传给
  签发方（[表格](#private-reasoning-state)）；sub2api 不能在同一段对话里混用不同模型家族
  [S6]，LiteLLM 的修复只覆盖 Responses 条目 id [S12]。
- **转换分档。** 每种转换都定了档，服务不了某个请求的端点永远不会被选中
  （`TestAssessMatrix`、`TestCapabilityFiltering`）；new-api 则提醒字段"may not map
  exactly"（可能无法精确对应） [S1]。
- **同协议流量原样不动。** 优先逐字节直通（`TestPassthroughByteExact`）；其他项目没有说明
  它们是怎么做的。
- **从 new-api 迁过来。** `admin.import-newapi` 读取 new-api / one-api 的数据库，原有的
  `sk-` 密钥照常可用（`TestDryRunThenImport`）。

<a id="where-others-are-ahead"></a>
## 别人领先的地方

- **成熟度和社区。** 上面每个项目都有几千到几万 star、正式发布的版本和生产用户
  [S0][S9]；plugrelay 刚到首个公开版本，还没有生产用户的报告。
- **服务商覆盖面。** LiteLLM 支持 100+ 家服务商 [S9]，Portkey 能路由到 1,600+ 个模型
  [S18]，Bifrost 支持 23+ 家服务商 [S19]；plugrelay 自带 108 份清单（`manifests/`、
  `TestManifestConformance`），每份都在 2026-11 对照厂商文档核对过：其中 23 份对应自部署
  推理服务，5 份对应自部署网关，1 份是通用清单。数量和 LiteLLM 差不多，但有好几份是同一
  厂商的不同地区版本（Kimi、GLM、MiniMax、Qwen），真正对接真实账号跑过的不多。
- **支付和转售。** new-api 提供 Stripe、易支付 EPay、Creem 和 Waffo，支持订阅 [S4]；
  sub2api 提供 EasyPay、支付宝、微信支付和 Stripe [S6]。plugrelay 的计费功能有 Stripe 和
  易支付协议（支付宝、微信支付），支持订阅套餐，但没有 Creem 和 Waffo，而且两种支付通道都只
  对接过假服务器。注册可以选开 Turnstile/hCaptcha 验证，但没有邮箱验证
  （`plugins/admin/signup/signup.go`）。
- **模态。** new-api 支持 realtime [S1]；LiteLLM 支持 batches 和 A2A [S9]。plugrelay 有
  embeddings、图片、音频、rerank 和 batches，但图片、音频和 batches 只支持同协议；realtime
  只中转到 OpenAI 和 Azure，而且只对假服务验证过（测试上游在 `/v1/realtime` 上回 404，所以
  `TestWebSocket` 会跳过；真实运行需要付费的 OpenAI 或 Azure 账号），也没有 Gemini Live。
  图片在真实环境测过；embeddings（OpenAI 和 Gemini 两种形状）、rerank（通用形状和 Cohere v2
  形状）和语音转写只对接本地开源服务 TEI 和 whisper.cpp 真实测过（`TestSelfHostedMedia`，
  [运行记录](compat/2026-11-06-live.md)），没有测过商业 API；语音合成和 batches 只对假服务
  测过，因为测试上游回 404（`TestRelay` 和 `TestRerankAndBatches` 会跳过它们，并说明原因）。
  Voyage、Mixedbread、NVIDIA 和 DashScope 的 rerank 用各自的格式，没有对应的编解码器。
- **企业身份认证。** new-api 有 OAuth/OIDC、通行密钥和两步验证 [S1]；plugrelay 也有 SSO、
  TOTP 两步验证和通行密钥（`TestMFA`），但通行密钥只在测试里对接过软件模拟的认证器，没有在
  真实浏览器和设备上用过。
- **内容护栏和 MCP。** LiteLLM 和 Portkey 有内容护栏框架和 MCP 网关 [S9][S18]；Bifrost 的
  文档首页把内容护栏标为企业版，MCP 网关没标属于哪边（只有 "MCP Tool Filtering" 标了开源）
  [S23]，而它的 README 把两者都列在企业部署下 [S19]；plugrelay 有内置检查（拒绝名单、个人
  信息和密钥检测、大小限制、模型和工具限制、一次 OpenAI 兼容的内容审核调用；
  `middleware.guardrails`）和 MCP 网关（`internal/mcp`），但不集成第三方内容护栏，不支持
  stdio MCP 服务，也不支持对 MCP 服务做 OAuth。MCP 网关对接官方参考服务、用官方
  TypeScript SDK 客户端和 Claude Code 在真实环境测通过（`TestMCPReferenceServer`，
  [运行记录](compat/2026-11-06-live.md)），只覆盖协议版本 2025-11-25；内容护栏只对假服务
  验证过。
- **客户端兼容性证据。** LiteLLM 每天发布一份按服务商划分的 Claude Code 矩阵 [S13]；
  plugrelay 发布带日期的运行记录，用真实的 Claude Code 和 Codex 分别跑一条直通路由和一条
  全程转换路由（[compat](compat/README.zh-CN.md)、`scripts/compat.sh`、`TestClients`；配好
  secret 后有每周一次的工作流），但只对接一个上游，不是多家服务商的矩阵。
- **生产环境的数据库。** new-api 能跑在 MySQL ≥ 5.7.8 上，日志放在单独的库里，ClickHouse 也
  支持 [S1]；plugrelay 的存储能跑在 MySQL 8.0.13+ 上（`TestMySQL`、`TestLedgerMySQL`），但
  没有单独的日志库；MySQL 和 MariaDB 只手动测过，而且是在全新的库上。
- **控制台语言。** new-api 的控制台有七种语言 [S21]；plugrelay 只有英文和简体中文，这是有意
  的决定。
- **扩展。** new-api 的控制台里有插件市场 [S1]，claude-code-router 有插件市场 schema
  [S16][S17]；plugrelay 两者都没有，这是有意的决定。
- **订阅账号。** sub2api 和 claude-code-router 会把 OAuth 订阅账号做成池子并轮换密钥
  [S6][S16]。plugrelay 有意不做订阅账号的 OAuth 号池
  （[README](../README.zh-CN.md#non-goals)）；对想要这个功能的运营者来说，这是个缺口。

<a id="known-gaps-in-plugrelay"></a>
## plugrelay 已知的不足

| 不足 | 现状 |
|-|-|
| AWS Bedrock 和 Google Vertex AI | 已支持（`plugins/provider/bedrock`：SigV4 或 API key，解码 event-stream；`plugins/provider/vertex`：服务账号 OAuth；`manifests/bedrock.yaml`、`vertex-anthropic.yaml`、`vertex-gemini.yaml`），但只对假服务和公开的测试向量验证过（`TestSignatureVerifiesAtUpstream`、`TestStreamDecodesChunks`、`TestTokenLifecycle`），没有对接真实账号；这几份清单都没列模型和价格 |
| 上游额度展示 | 由 `job.quotaprobe` 探测，按凭据展示；`quota_reset` 路由策略会用到它，额度用完的凭据会被搁置到重置时间（`TestQuotaProbeEndToEnd`）；官方清单里只有 OpenRouter 声明了探测（`manifests/openrouter.yaml`、`TestOpenRouterQuotaProbe`），而且这个探测还没在真实的 OpenRouter 账号上跑过 |
| embeddings 和音频 | 已支持（`pkg/protocol/openaiembeddings`、`geminiembeddings`、`openaiaudio`）；embeddings（OpenAI 和 Gemini 客户端）和语音转写只对接 CPU 上运行的 TEI 和 whisper.cpp 真实验证过（`TestSelfHostedMedia`，[运行记录](compat/2026-11-06-live.md)）；语音合成只对假服务测过，也没有跑过任何商业 embeddings 或音频 API |
| 图片和音频跨协议 | 只能直通到同一协议 |
| rerank、batches | 已支持（`pkg/protocol/rerank`、`coherererank`、`teirerank`；`internal/pipeline/batch.go`）；转到 TEI 的 rerank 通过 `/v1/rerank` 和 `/v2/rerank` 真实验证过（`TestSelfHostedMedia`；TEI 不报告用量，所以客户端收到的响应里 token 数为零，日志里按估算计费）；其他 rerank 形状和 batches 只对假上游测过（`TestRerankToTEI`、`TestOpenAIBatchLifecycle`、`TestAnthropicMessageBatchLifecycle`；真实测试上游对这些路由回 404，所以 `TestRerankAndBatches` 会跳过）；batches 只支持同协议，而且只有结果经由中转读取时才计费；Voyage、Mixedbread、NVIDIA、DashScope 和 DeepInfra 原生的 rerank 格式没有编解码器 |
| Realtime / WebSocket | OpenAI Realtime（`/v1/realtime`，OpenAI 和 Azure 清单）和 Responses WebSocket 模式（仅官方 OpenAI；除非只有一个上游能服务所有模型，否则回 `426`，Codex 会退回 HTTP）可以中转，每个 response 预扣并结算，输入语音转写计费，被截断的 response 按估算计费，访问名单和限流照样生效（`internal/pipeline/websocket.go`）；只对假 WebSocket 上游验证过；没有 Gemini Live；音频 token 按模型的 token 单价计费 |
| 支付 | Stripe（充值和订阅）和易支付只对假服务器测过；没有 Creem、Waffo，也没有直接对接支付宝/微信支付的商户 API；更换套餐不按比例折算 |
| 注册滥用 | 注册时、以及密码登录多次失败后，可以选开 Turnstile 或 hCaptcha；通过和不通过两种情况都用它们公开的测试密钥对接真实 siteverify 验证过（`TestCaptchaLive`，[运行记录](compat/2026-11-06-live.md)），服务不可用的情况只对假服务测过（`TestCaptcha`、`TestMFA/*/LoginCaptcha`）；没有邮箱验证 |
| 两步验证、通行密钥 | TOTP、恢复码和通行密钥（`TestMFA`）；WebAuthn 只对软件模拟的认证器验证过，没有在真实浏览器上测过 |
| MCP 网关、内容护栏 | 已内置（`internal/mcp`：对客户端提供 Streamable HTTP，上游支持 Streamable HTTP 和 HTTP+SSE，按密钥授权，`/mcp` 聚合；`middleware.guardrails`，默认关闭）；MCP 网关对接 `@modelcontextprotocol/server-everything`、用官方 TypeScript SDK 客户端和 Claude Code 真实验证过（`TestMCPReferenceServer`），只覆盖协议版本 2025-11-25：2026-07-28 修订版、HTTP+SSE 回退和进度流只对假服务测过（`internal/mcp` 里的测试），内容护栏只对假的内容审核服务测过（`TestModeration`）；不支持 stdio 服务，不支持上游 OAuth |
| 控制台语言 | 只有英文和简体中文（`web/src/i18n`） |
| 订阅账号 | 不做 Claude / ChatGPT 订阅账号的 OAuth 号池，这是有意的决定 |
| 插件市场 | 没有；外部插件要手动装到 `external_plugins.dir` |
| 用 MySQL 做存储 | 设置 `PLUGRELAY_TEST_MYSQL` 后，MySQL 8.0.13+ 会跑同一套存储、流水账和状态测试（`internal/store`、`TestMySQL`、`TestLedgerMySQL`）；CI 在一个全新的 MySQL 8.4 上建表（`internal/store/migrations/mysql/0001_init.sql`），并跑存储、管理后台、importnewapi 和状态测试（`.github/workflows/ci.yml`）；存储测试也在 MariaDB 10.6 上手动跑通过 |
| 成熟度 | 首个公开版本；还没有生产用户的报告 |

<a id="sources"></a>
## 来源

除另有标注外，均于 2026-11-05 核实。

- [S0] 仓库元数据（Bifrost 的发布信息于 2026-11-06 复核），通过 GitHub API 获取（`gh api repos/<owner>/<repo>` 和 `…/releases/latest`）：[new-api](https://github.com/QuantumNous/new-api)、[one-api](https://github.com/songquanpeng/one-api)、[sub2api](https://github.com/Wei-Shaw/sub2api)、[litellm](https://github.com/BerriAI/litellm)、[claude-code-router](https://github.com/musistudio/claude-code-router)、[Portkey gateway](https://github.com/Portkey-AI/gateway)、[bifrost](https://github.com/maximhq/bifrost)
- [S1] new-api README — https://github.com/QuantumNous/new-api/blob/main/README.md
- [S2] new-api 渠道亲和 — https://github.com/QuantumNous/new-api/blob/main/setting/operation_setting/channel_affinity_setting.go
- [S3] new-api Gemini 设置 — https://github.com/QuantumNous/new-api/blob/main/setting/model_setting/gemini.go
- [S4] new-api 支付处理代码（`topup_stripe.go`、`subscription_payment_epay.go`，…） — https://github.com/QuantumNous/new-api/tree/main/controller
- [S5] one-api README — https://github.com/songquanpeng/one-api/blob/main/README.md
- [S6] sub2api README — https://github.com/Wei-Shaw/sub2api/blob/main/README.md
- [S7] sub2api 的 thinking 处理 — https://github.com/Wei-Shaw/sub2api/blob/main/backend/internal/service/thinking_protocol.go
- [S8] sub2api 插件开发指南 — https://github.com/Wei-Shaw/sub2api/blob/main/docs/PLUGIN_DEVELOPMENT.md
- [S9] LiteLLM README — https://github.com/BerriAI/litellm/blob/main/README.md
- [S10] LiteLLM 许可证 — https://github.com/BerriAI/litellm/blob/main/LICENSE
- [S11] LiteLLM 提示词缓存路由 — https://github.com/BerriAI/litellm-docs/blob/main/docs/tutorials/claude_code_prompt_cache_routing.md
- [S12] LiteLLM encrypted-content 事故 — https://github.com/BerriAI/litellm-docs/blob/main/blog/responses_api_encrypted_content_incident/index.md
- [S13] LiteLLM Claude Code 兼容性矩阵 — https://github.com/BerriAI/litellm-docs/blob/main/docs/claude_code_compatibility.md
- [S14] LiteLLM 客户端接入指南：[Codex](https://github.com/BerriAI/litellm-docs/blob/main/docs/proxy/client_setup/codex_cli.md)、[Gemini CLI](https://github.com/BerriAI/litellm-docs/blob/main/docs/tutorials/litellm_gemini_cli.md)、[Cursor](https://github.com/BerriAI/litellm-docs/blob/main/docs/tutorials/cursor_integration.md)
- [S15] LiteLLM [数据库](https://github.com/BerriAI/litellm-docs/blob/main/docs/proxy/db_info.md)和[钩子](https://github.com/BerriAI/litellm-docs/blob/main/docs/proxy/call_hooks.md)
- [S16] claude-code-router README — https://github.com/musistudio/claude-code-router/blob/main/README.md
- [S17] claude-code-router 源码目录（`packages/core/src/plugins`、`packages/core/src/storage`） — https://github.com/musistudio/claude-code-router/tree/main/packages/core/src
- [S18] Portkey gateway README — https://github.com/Portkey-AI/gateway/blob/main/README.md
- [S19] Bifrost README — https://github.com/maximhq/bifrost/blob/main/README.md
- [S20] OpenRouter 的 Claude Code 接入说明 — https://openrouter.ai/docs/guides/guides/claude-code-integration
- [S21] new-api 控制台语言包（`en`、`fr`、`ja`、`ru`、`vi`、`zh`、`zh-TW`；2026-11-06 核实） — https://github.com/QuantumNous/new-api/tree/main/web/src/i18n/locales
- [S22] LiteLLM OIDC SSO 文档（2026-11-06 核实） — https://github.com/BerriAI/litellm-docs/blob/main/docs/proxy/admin_ui_sso.md
- [S23] Bifrost 文档首页（2026-11-06 核实） — https://docs.getbifrost.ai/
