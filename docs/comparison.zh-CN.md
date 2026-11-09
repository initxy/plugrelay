# plugrelay 与同类项目对比

[English](comparison.md) | 简体中文

把 plugrelay 和大家常拿来比的中转、网关放在一起看。其他项目的情况来自它们自己的仓库和
文档，2026-11-05 核实，每条都带来源标记，出处见[来源](#sources)。plugrelay 的说法指向
本仓库的代码或测试，2026-11-08 核实。「不详」表示对方自己的资料没写。

[概览](#at-a-glance) · [也看过的项目](#also-surveyed) · [协议与转换](#protocols-and-conversion) · [编程客户端](#coding-clients) · [提示词缓存粘性](#prompt-cache-stickiness) · [上游私有的推理状态](#private-reasoning-state) · [扩展方式](#extension-model) · [存储与集群](#storage-and-clustering) · [计费](#billing) · [控制台](#console) · [部署与许可证](#deployment-and-license) · [plugrelay 也有的](#what-plugrelay-matches) · [plugrelay 领先的地方](#where-plugrelay-is-ahead) · [别人领先的地方](#where-others-are-ahead) · [plugrelay 已知的不足](#known-gaps-in-plugrelay) · [来源](#sources)

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
| Bifrost | 9.4k · Apache-2.0 · `transports/v2.4.0`，2026-10-29（GitHub 上标为 "latest" 的是一个 Helm chart 标签） | Go | 高吞吐应用网关 [S19] |
| OpenRouter | 只有托管版 | — | 托管的模型市场 [S20] |
| plugrelay | 首个公开版本 · Apache-2.0 | Go + React | 面向编程客户端，自部署 |

<a id="also-surveyed"></a>
## 也看过的项目

下面这些看过但没放进对比，因为它们的说法没有对照各自的出处核实：CLIProxyAPI、
claude-relay-service、gpt-load、axonhub、octopus、CliRelay、Helicone、Vercel AI
Gateway、Cloudflare AI Gateway。网关类可以当上游接在 plugrelay 后面，
[`manifests/`](../manifests) 里有 OpenRouter、Vercel、Cloudflare、Helicone、Portkey、
LiteLLM、Bifrost、New API 和 One API 的清单。

<a id="protocols-and-conversion"></a>
## 协议与转换

说协议 A 的客户端能不能用说协议 B 的上游？同协议的流量是不是原样不动？

| | 客户端侧协议 | 跨协议转换 | 同协议处理 |
|-|-|-|-|
| new-api | OpenAI chat、Responses、Anthropic messages、Gemini；另有 realtime、图片、音频、embeddings、rerank [S1] | 四种文本协议之间互转；"protocol-specific tools and fields may not map exactly"（协议特有的工具和字段可能对不上） [S1] | 不详 |
| one-api | OpenAI 格式 [S5] | 每家服务商一个适配器，转成 OpenAI 格式 [S5] | 不适用 |
| sub2api | Anthropic messages、Responses（含 Codex WebSocket）、chat、Gemini；xAI 和 Antigravity 路由 [S6] | 部分桥接，例如 `/v1/messages` 转成 xAI Responses [S6] | 不详 |
| LiteLLM | 以 OpenAI 格式为主；也有 `/responses`、`/messages`、embeddings、图片、音频、batches、rerank、A2A [S9] | 全部走它那套 OpenAI 格式的接口 [S9] | 不详 |
| claude-code-router | OpenAI chat / Responses、Anthropic messages、Gemini [S16] | 上述协议之间互转 [S16] | 不详 |
| plugrelay | Anthropic messages、Responses、OpenAI chat、Gemini、老的 completions；embeddings、图片、音频、rerank、batches、Realtime（[全部路由](clients.zh-CN.md#what-the-relay-serves)） | 先转成中间格式（IR），只跑能力有差别的那几步；每个端点评为无损 / 有损 / 不推荐 / 不可能，不可能的绝不路由（[架构](architecture.zh-CN.md#ir-and-capabilities)）。embeddings 和 rerank 能跨格式转换，图片、音频和 batches 不能 | 优先逐字节直通，其次只改个别字段（[处理方式](architecture.zh-CN.md#processing-tiers)） |

媒体、batch 和 WebSocket 路由和对话共用密钥、路由、请求日志和计费，只发给声明了该操作的
端点（[客户端](clients.zh-CN.md#what-the-relay-serves)）。

<a id="coding-clients"></a>
## 编程客户端

| | Claude Code | Codex | Gemini CLI | Cursor | 其他 |
|-|-|-|-|-|-|
| new-api | 支持；渠道亲和规则会透传 Claude CLI 的请求头 [S2] | 支持；透传 session/turn 请求头 [S2] | 提供 Gemini 端点 [S1] | 不详 | 不详 |
| sub2api | 支持，含 Antigravity [S6] | 支持；要粘性会话得给 nginx 开 `underscores_in_headers` [S6] | 提供 Gemini 端点 [S6] | 不详 | 不详 |
| LiteLLM | 有文档，每天自动按服务商跑一遍 Claude Code 兼容性矩阵 [S13] | 有文档 [S14] | 有文档 [S14] | 尽力而为，"reverse engineering their APIs"（靠逆向它们的 API）；Cursor CLI 没法指向它 [S14] | 不详 |
| claude-code-router | 有 Claude Code、Codex、Grok CLI、Kimi CLI、Kilo Code、OpenCode、Pi 等客户端的配置 [S16] | 支持 [S16] | 不在客户端列表里 [S16] | 不在客户端列表里 [S16] | — |
| OpenRouter | 提供 Anthropic 兼容端点，"only guaranteed to work with the Anthropic first-party provider"（只保证 Anthropic 官方服务商可用） [S20] | 不详 | 不详 | 不详 | 不详 |
| plugrelay | `plugrelay setup claude-code`；`anthropic-beta` 原样转发 | `plugrelay setup codex` 写好服务商和模型目录；同一轮的请求留在同一个凭据上 | `plugrelay setup gemini` | 手动配置（[客户端](clients.zh-CN.md#cursor)） | OpenCode 有 setup 命令；Cline、Continue（含代码补全 fill-in-the-middle）、Copilot、Trae 手动配置（[客户端](clients.zh-CN.md)） |

plugrelay 有一项需要手动开启的端到端测试：让真的 Claude Code 和 Codex 经过中转完成一个
小编程任务，一条路由同协议，一条只有 chat、逼着每个请求都转换。最近一次 4/4 通过
（[兼容性记录](compat/README.zh-CN.md)）。

<a id="prompt-cache-stickiness"></a>
## 提示词缓存粘性

在多个凭据间轮询会丢掉服务商的提示词缓存，这是编程流量走中转更贵的主要原因。

| | 做法 |
|-|-|
| new-api | 「渠道亲和」规则，按请求头、上下文值或 JSON 路径匹配，带 TTL 和严格/优先两种模式 [S2] |
| sub2api | 选账号时用粘性会话 [S6] |
| LiteLLM | 可选的 `prompt_caching` 调用前检查，把后续请求发回写入缓存的那个部署 [S11] |
| claude-code-router | 不详（本地单用户） |
| plugrelay | 只有上一轮读写过缓存，会话才继续留在原凭据上（5 分钟；写过 1 小时缓存则 1 小时）；每个用户最多固定在 3 个最近用过的凭据上；权重只用来分配新会话；跨协议时补上缓存断点或缓存键（[架构](architecture.zh-CN.md#routing-stickiness-and-failure-verdicts)） |

<a id="private-reasoning-state"></a>
## 上游私有的推理状态

思考签名、Responses 的 `encrypted_content` 和 Gemini 的 thought signature 只在签发它的
上游（往往还限同一个组织）有效，发到别处请求就会失败。

| | 做法 |
|-|-|
| new-api | Gemini 有个函数调用 thought signature 的开关，默认打开 [S3]；其他情况不详 |
| sub2api | 对严格的 Anthropic 上游去掉签名无效的 thinking 块，对要求回传的上游保留；按模型名前缀判断模型家族 [S7]；Anthropic 和 Antigravity 的 Claude "cannot be mixed within the same conversation"（不能在同一段对话里混用） [S6] |
| LiteLLM | `encrypted_content_affinity` 把签发的部署编进 Responses 条目 id，后续请求据此发回原处，是 2026-02 一次事故后加的 [S12]；跨协议情况不详 |
| claude-code-router | 不详 |
| plugrelay | 任何上游私有状态都加密（AES-256-GCM，密钥由主密钥派生）并记下签发方，只发回签发方，到别处就丢掉。只要客户端协议有字段能带它，跨协议也行；OpenAI chat 没有这种字段，状态在那里会丢（[架构](architecture.zh-CN.md#private-state-sealing)） |

<a id="extension-model"></a>
## 扩展方式

| | 怎么扩展 |
|-|-|
| new-api | 给异步图片/视频 API 用的 JavaScript 任务插件，控制台里有插件市场；新的文本上游要在 `relay/` 里写 Go 代码 [S1] |
| one-api | 写 Go 代码 [S5] |
| sub2api | 进程外的 gRPC 插件，打包成 `.s2plugin`，带沙箱化的界面；目前只定义了出站传输这一种能力，官方没发布示例 [S8] |
| LiteLLM | Python 回调和钩子（调用前、审核、调用后、流式） [S15]；自定义服务商用 Python 写 [S9] |
| claude-code-router | JavaScript 插件（`plugin.json` + 模块），有包装型和核心网关型插件，还有插件市场的格式定义 [S16][S17] |
| Bifrost | Go 和 WASM 中间件插件；文档首页说自定义插件是开源的 [S23]，README 却列为企业版 [S19] |
| plugrelay | 八个扩展点（`pkg/plugin/plugin.go`），三种写法：上游用 YAML 清单（[`manifests/`](../manifests) 自带 108 份），用任意语言写外部进程、走 stdio 上的 JSON-RPC（有 Go SDK），或者直接用 Go 编译进去。插件不往控制台加页面；没有插件市场、WASM 插件和签名包，这是有意的（[插件](plugins.zh-CN.md)） |

<a id="storage-and-clustering"></a>
## 存储与集群

| | 业务数据 | 共享的运行时状态 / 集群 |
|-|-|-|
| new-api | SQLite、MySQL ≥ 5.7.8、PostgreSQL ≥ 9.6；日志可以单独放一个库，支持 ClickHouse [S1] | 用 Redis 共享限流；多节点共用主库和密钥 [S1] |
| one-api | SQLite、MySQL；PostgreSQL「适配中」 [S5] | 共用 MySQL + Redis，再加 `SYNC_FREQUENCY` 轮询 [S5] |
| sub2api | 必须用 PostgreSQL 15+ [S6] | 必须用 Redis 7+ [S6] |
| LiteLLM | PostgreSQL 存密钥、团队、预算、用量 [S15] | Redis；有 Terraform 模块 [S9] |
| claude-code-router | SQLite（`packages/core/src/storage/sqlite-native.ts`） [S17] | 不适用（本地） |
| Bifrost | 配置库和日志库 [S19] | 集群是企业版功能 [S19][S23] |
| plugrelay | SQLite（默认）、PostgreSQL 或 MySQL 8.0.13+（`internal/store`）；能导出/导入 YAML | 内存、SQL 或 Redis（`internal/state`）；路由健康状态多节点共享，控制台里看到的是实时状态 |

<a id="billing"></a>
## 计费

| | 计价单位 | 支付 |
|-|-|-|
| new-api | 额度和订阅，定价可以写表达式 [S1] | Stripe、易支付、Creem、Waffo 的充值和订阅 [S4] |
| one-api | 点数：额度 = 分组倍率 × 模型倍率 × (提示 + 补全 × 补全倍率)；可以按美元显示 [S5] | 只有一个充值链接 [S5] |
| sub2api | 按 token 算成本 [S6] | 易支付、支付宝、微信支付、Stripe [S6] |
| LiteLLM | 按密钥、用户、团队统计美元花费，带预算 [S9][S15] | 代理本身没有 |
| claude-code-router | 每个请求估算成本，本地限额 [S16] | 无 |
| plugrelay | 按站点货币实价计费：每百万 token 一个价，媒体按张、按音频秒数或每千字符；每次发到上游的尝试都算成本，默认只向调用方收成功交付的那一次；密钥、用户、分组三级预算 | 计费功能，默认关闭：并发下也不会透支的预付费余额、兑换码、Stripe 和易支付（支付宝、微信支付）充值、订阅套餐、优惠券、签到和邀请奖励、自助注册，以及能让原有密钥继续可用的 new-api / one-api 导入工具。Stripe 和易支付只对假服务测过（[收费功能](sell-kit.zh-CN.md)） |

<a id="console"></a>
## 控制台

| | 控制台 |
|-|-|
| new-api | React；用量看板、插件市场、模型广场、在线试用；七种语言 [S21] |
| sub2api | Vue 管理后台；一键自升级，可回滚；可以用 iframe 嵌入外部系统 [S6] |
| LiteLLM | 管理界面，有密钥、团队、预算、内容护栏、请求日志 [S9] |
| claude-code-router | 桌面应用（macOS、Windows、Linux）加浏览器界面 [S16] |
| Portkey | 本地日志控制台 [S18] |
| plugrelay | 内置在二进制里；首次设置时按用途打开相应功能；每个功能一个开关一张表单；厂商库；请求日志列出每次上游尝试，带会话视图和 CSV 导出；用量分析；在线试用；公开的价格页和状态页；计费页面；凭据的实时状态；单点登录（OIDC、GitHub、飞书）、TOTP、通行密钥；中英文两种语言；只调用公开的管理 API；`plugrelay demo` 起一个带示例数据的本地副本 |

<a id="deployment-and-license"></a>
## 部署与许可证

| | 部署 | 许可证 |
|-|-|-|
| new-api | Docker、Compose（带 PostgreSQL + Redis）、Electron 桌面封装 [S1] | AGPL-3.0 加署名条款；改过的界面必须保留一个看得见的链接 [S1] |
| sub2api | 安装脚本 + systemd、Compose、Apple container；需要 PostgreSQL 和 Redis [S6] | LGPL-3.0 [S0] |
| LiteLLM | pip/uv、Docker、Terraform [S9] | `enterprise/` 以外为 MIT [S10]；从 v1.76.0 起单点登录 5 个用户以内免费，再多要企业许可证 [S22] |
| claude-code-router | 桌面安装包、npm 命令行、Docker [S16] | MIT [S0] |
| plugrelay | 一个内置控制台的二进制，有 Docker 镜像和 Compose（[部署](deployment.zh-CN.md)）；用 SQLite 时不依赖别的 | Apache-2.0 |

<a id="what-plugrelay-matches"></a>
## plugrelay 也有的

上面那些中转有的控制台功能，这里也有：从厂商库添加上游并顺手填凭据，识别每个请求
来自哪个客户端并按客户端统计花费，会话视图和 CSV 导出，CC Switch 导入链接，通知（邮件、
飞书、钉钉、企业微信、webhook），预设，从 models.dev 同步价格，在线试用，公开的价格页和
状态页，以及收费用的订单、优惠券、签到、邀请奖励和公告。这些都还没经过生产流量。

<a id="where-plugrelay-is-ahead"></a>
## plugrelay 领先的地方

只列代码或测试能证明的；成熟度不在其中。

- **跨协议保住上游私有的推理状态**，只发回签发方（[表格](#private-reasoning-state)）。
  sub2api 不能在同一段对话里混用不同模型家族 [S6]；LiteLLM 的修复只管 Responses 条目
  id [S12]。
- **转换有评级。** 满足不了请求的端点绝不路由（`TestAssessMatrix`）；new-api 自己提醒
  字段"may not map exactly"（可能对不上） [S1]。
- **同协议流量一个字节都不改**（`TestPassthroughByteExact`）；其他项目没写自己怎么处理。
- **从 new-api 迁过来。** 导入工具读取 new-api / one-api 的数据库，原有的 `sk-` 密钥继续
  可用（[收费功能](sell-kit.zh-CN.md#import-from-new-api-adminimport-newapi)）。

<a id="where-others-are-ahead"></a>
## 别人领先的地方

- **成熟度。** 上面每个项目都发过很多版本、有生产用户 [S0][S9]；plugrelay 刚发第一版，
  还没有生产用户的报告。
- **服务商覆盖。** LiteLLM 支持 100+ 家服务商 [S9]，Portkey 能路由到 1,600+ 个模型
  [S18]，Bifrost 23+ 家 [S19]。plugrelay 的 108 份清单数量和 LiteLLM 差不多，但有几份
  是同一家的不同地区版（Kimi、GLM、MiniMax、Qwen），真账号跑过的不多。
- **支付。** new-api 有 Stripe、易支付、Creem 和 Waffo [S4]；sub2api 有易支付、支付宝、
  微信支付和 Stripe [S6]。plugrelay 只有 Stripe 和易支付，注册也不验证邮箱。
- **模态。** new-api 支持 realtime [S1]，LiteLLM 支持 batches 和 A2A [S9]。plugrelay 的
  图片、音频和 batches 只发给同协议的上游，realtime 只转发给 OpenAI 和 Azure，没有
  Gemini Live，Voyage、Mixedbread、NVIDIA、DashScope 和 DeepInfra 自己格式的 rerank
  也没有对应的编解码器。
- **内容护栏和 MCP。** LiteLLM 和 Portkey 有护栏框架和 MCP 网关 [S9][S18]；Bifrost
  文档首页把护栏标为企业版，只有 "MCP Tool Filtering" 标为开源 [S23]，README 则把两者都
  列为企业版 [S19]。plugrelay 有内置检查和 MCP 网关，但不能接第三方护栏，不支持 stdio
  方式的 MCP 服务，也不能用 OAuth 连 MCP 服务。
- **客户端兼容性证据。** LiteLLM 每天按服务商发布 Claude Code 兼容性矩阵 [S13]；
  plugrelay 按日期发布的运行记录只对一个上游。
- **数据库。** new-api 能跑在 MySQL ≥ 5.7.8 上，日志放在单独的库里，ClickHouse 也行
  [S1]；plugrelay 要 MySQL 8.0.13+，也没有单独的日志库。
- **控制台语言。** new-api 有七种 [S21]；plugrelay 两种，这是有意的。
- **插件市场。** new-api 控制台里有插件市场 [S1]，claude-code-router 有插件市场的格式
  定义 [S16][S17]；plugrelay 都没有，这是有意的。
- **订阅账号。** sub2api 和 claude-code-router 能把 OAuth 订阅账号凑成池子用 [S6][S16]。
  plugrelay 不做（[不做的事](../README.zh-CN.md#non-goals)）；对想要这个的运营方来说，
  这就是缺口。

<a id="known-gaps-in-plugrelay"></a>
## plugrelay 已知的不足

功能有了，但还没在真实服务上验证过的：

| 方面 | 验证到什么程度 |
|-|-|
| AWS Bedrock、Google Vertex AI | 只对假服务和官方公布的测试向量验证过；清单里没列模型和价格 |
| 上游额度显示 | 只对假服务验证过；官方清单里只有 OpenRouter 声明了额度探测，还没在真账号上跑过 |
| embeddings、rerank、语音转写 | 只在自部署的 TEI 和 whisper.cpp 上真实跑过（[运行记录](compat/2026-11-06-live.md)），没跑过商用 API。TEI 不报告 rerank 用量，日志里按估算计费 |
| 语音合成、batches | 只对假服务验证过；用来真实测试的上游在这些路由上返回 404 |
| Realtime / Responses WebSocket | 只对假的 WebSocket 上游验证过；真实测试需要付费的 OpenAI 或 Azure 账号。音频 token 按模型的 token 单价计 |
| Stripe、易支付 | 只对假服务验证过；更换套餐不按比例折算 |
| 注册和登录验证码 | Turnstile 和 hCaptcha 用官方测试密钥真实跑过；服务故障只用假服务模拟过 |
| 通行密钥 | 只用软件模拟的认证器测过，没在真实浏览器和设备上测 |
| MCP 网关 | 对官方参考服务、官方 TypeScript SDK 客户端和 Claude Code 真实跑过，只限 2025-11-25 版协议；2026-07-28 修订版、HTTP+SSE 回退和进度推送只对假服务测过 |
| 内容护栏 | 只对假的审核服务验证过 |
| MySQL、MariaDB | CI 在全新的 MySQL 8.4 上跑；MariaDB 10.6 手动跑过 |
| 真实上游协议测试 | 约九次完整运行里出现过一次 `TestRelay/openai_chat*` 失败，没能复现 |

<a id="sources"></a>
## 来源

除非另有标注，均于 2026-11-05 核实。

- [S0] 仓库元数据（Bifrost 的版本于 2026-11-06 复查），通过 GitHub API 获取（`gh api repos/<owner>/<repo>` 和 `…/releases/latest`）：[new-api](https://github.com/QuantumNous/new-api)、[one-api](https://github.com/songquanpeng/one-api)、[sub2api](https://github.com/Wei-Shaw/sub2api)、[litellm](https://github.com/BerriAI/litellm)、[claude-code-router](https://github.com/musistudio/claude-code-router)、[Portkey gateway](https://github.com/Portkey-AI/gateway)、[bifrost](https://github.com/maximhq/bifrost)
- [S1] new-api README — https://github.com/QuantumNous/new-api/blob/main/README.md
- [S2] new-api 渠道亲和 — https://github.com/QuantumNous/new-api/blob/main/setting/operation_setting/channel_affinity_setting.go
- [S3] new-api Gemini 设置 — https://github.com/QuantumNous/new-api/blob/main/setting/model_setting/gemini.go
- [S4] new-api 支付处理代码（`topup_stripe.go`、`subscription_payment_epay.go` 等） — https://github.com/QuantumNous/new-api/tree/main/controller
- [S5] one-api README — https://github.com/songquanpeng/one-api/blob/main/README.md
- [S6] sub2api README — https://github.com/Wei-Shaw/sub2api/blob/main/README.md
- [S7] sub2api 的 thinking 处理 — https://github.com/Wei-Shaw/sub2api/blob/main/backend/internal/service/thinking_protocol.go
- [S8] sub2api 插件开发指南 — https://github.com/Wei-Shaw/sub2api/blob/main/docs/PLUGIN_DEVELOPMENT.md
- [S9] LiteLLM README — https://github.com/BerriAI/litellm/blob/main/README.md
- [S10] LiteLLM 许可证 — https://github.com/BerriAI/litellm/blob/main/LICENSE
- [S11] LiteLLM 提示词缓存路由 — https://github.com/BerriAI/litellm-docs/blob/main/docs/tutorials/claude_code_prompt_cache_routing.md
- [S12] LiteLLM encrypted-content 事故 — https://github.com/BerriAI/litellm-docs/blob/main/blog/responses_api_encrypted_content_incident/index.md
- [S13] LiteLLM Claude Code 兼容性矩阵 — https://github.com/BerriAI/litellm-docs/blob/main/docs/claude_code_compatibility.md
- [S14] LiteLLM 客户端指南：[Codex](https://github.com/BerriAI/litellm-docs/blob/main/docs/proxy/client_setup/codex_cli.md)、[Gemini CLI](https://github.com/BerriAI/litellm-docs/blob/main/docs/tutorials/litellm_gemini_cli.md)、[Cursor](https://github.com/BerriAI/litellm-docs/blob/main/docs/tutorials/cursor_integration.md)
- [S15] LiteLLM [数据库](https://github.com/BerriAI/litellm-docs/blob/main/docs/proxy/db_info.md)和[钩子](https://github.com/BerriAI/litellm-docs/blob/main/docs/proxy/call_hooks.md)
- [S16] claude-code-router README — https://github.com/musistudio/claude-code-router/blob/main/README.md
- [S17] claude-code-router 源码目录（`packages/core/src/plugins`、`packages/core/src/storage`） — https://github.com/musistudio/claude-code-router/tree/main/packages/core/src
- [S18] Portkey gateway README — https://github.com/Portkey-AI/gateway/blob/main/README.md
- [S19] Bifrost README — https://github.com/maximhq/bifrost/blob/main/README.md
- [S20] OpenRouter，Claude Code 集成 — https://openrouter.ai/docs/guides/guides/claude-code-integration
- [S21] new-api 控制台语言包（`en`、`fr`、`ja`、`ru`、`vi`、`zh`、`zh-TW`；2026-11-06 核实） — https://github.com/QuantumNous/new-api/tree/main/web/src/i18n/locales
- [S22] LiteLLM OIDC 单点登录文档（2026-11-06 核实） — https://github.com/BerriAI/litellm-docs/blob/main/docs/proxy/admin_ui_sso.md
- [S23] Bifrost 文档首页（2026-11-06 核实） — https://docs.getbifrost.ai/
