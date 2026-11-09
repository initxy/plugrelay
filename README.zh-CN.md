<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/logo-dark.svg">
    <img src="docs/assets/logo.svg" alt="plugrelay" width="300">
  </picture>
</p>

<p align="center"><strong>给编程客户端用的大模型中转站。同协议原样转发，跨协议按能力转换，每次请求走了哪、花了多少都查得到。</strong></p>

<p align="center">
  <a href="https://github.com/initxy/plugrelay/actions/workflows/ci.yml"><img src="https://github.com/initxy/plugrelay/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/initxy/plugrelay/releases"><img src="https://img.shields.io/github/v/release/initxy/plugrelay?include_prereleases&sort=semver" alt="Release"></a>
  <a href="https://pkg.go.dev/github.com/initxy/plugrelay"><img src="https://pkg.go.dev/badge/github.com/initxy/plugrelay.svg" alt="Go Reference"></a>
  <a href="https://goreportcard.com/report/github.com/initxy/plugrelay"><img src="https://goreportcard.com/badge/github.com/initxy/plugrelay" alt="Go Report Card"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue.svg" alt="License"></a>
</p>

<p align="center"><a href="README.md">English</a> | 简体中文</p>

<p align="center"><a href="#quickstart">快速开始</a> · <a href="#screenshots">截图</a> · <a href="#documentation">文档</a> · <a href="docs/architecture.zh-CN.md">架构</a> · <a href="docs/comparison.zh-CN.md">横向对比</a></p>

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/images/overview.zh-CN.dark.webp">
    <img src="docs/images/overview.zh-CN.webp" alt="plugrelay 控制台：总览" width="900">
  </picture>
</p>

plugrelay 是一个开源的大模型中转站。Claude Code、Codex、Gemini CLI、Cursor、Cline、OpenCode 改个 base URL 就能接进来，功能不受影响：

- 客户端和上游协议相同时，请求体原样转发，最多改个模型名，提示词缓存照常命中。
- 协议不同时，先对比请求要用的能力和上游支持的能力，只补差的那几项，丢了什么写进日志。
- 接新上游写一份 YAML 清单就行；私有协议可以用任何语言写外部插件。

<a id="why-it-is-different"></a>
## 和常见中转站的区别

| | 常见做法 | plugrelay |
|-|-|-|
| 协议转换 | 全部转成某一家的格式，或者每两种协议单写一个转换器 | 优先找同协议的上游原样转发；不得不转时只补缺的能力 |
| 接新上游 | 改核心代码，或者只能填个地址 | 兼容的上游写 YAML；私有协议写外部插件 |
| 错误 | 统一成一种格式，`retry-after` 丢了 | 按客户端协议的原生格式返回；上游原文、状态码、`retry-after` 都带回去 |
| 缓存 | 轮询分摊，命中看运气 | 缓存还热就留在原凭据上；跨协议时自动补缓存断点 |
| 思考签名 | 换渠道就报 Invalid signature | 加密后交给客户端保管，下一轮只还给签发它的上游 |
| 凭据 | 号池轮换、伪装官方客户端 | 每把凭据固定出口、限并发；出问题自动冷却或隔离。不做号池，不做伪装 |
| 透明度 | 只能看余额 | 每次请求查得到实际上游、处理方式、缓存读写和上游原始用量 |
| 兼容性 | 等用户报 bug | 拿真实 Claude Code、Codex 录下的请求做测试，另有对真实上游跑的端到端测试 |

<a id="screenshots"></a>
## 截图

<table>
  <tr>
    <td width="50%"><img src="docs/images/log-detail.zh-CN.webp" alt="请求详情"><br><sub><b>请求日志</b>：每次上游尝试走了哪、怎么处理、缓存读写、上游原始用量。</sub></td>
    <td width="50%"><img src="docs/images/analytics.zh-CN.webp" alt="用量分析"><br><sub><b>用量分析</b>：忙闲时段、失败原因，按客户端和模型看花费。</sub></td>
  </tr>
  <tr>
    <td width="50%"><img src="docs/images/upstreams.zh-CN.webp" alt="上游与凭据"><br><sub><b>上游</b>：每把凭据的实时状态，冷却、隔离、熔断、剩余额度。</sub></td>
    <td width="50%"><img src="docs/images/gallery.zh-CN.webp" alt="厂商库"><br><sub><b>厂商库</b>：选一家、填 key、点测试。</sub></td>
  </tr>
</table>

想先点点看：`plugrelay demo` 会起一个带演示数据和假上游的本地实例，账号 `admin@demo.local`，密码 `plugrelay-demo`。

<a id="features"></a>
## 功能

**接入**
- 客户端协议：Anthropic messages、OpenAI responses / chat / completions、Gemini。另外还有向量、生图、语音、重排序、批量任务和 Realtime WebSocket，和对话共用 key、选路、日志和计费（[完整路由表](docs/clients.zh-CN.md#what-the-relay-serves)）。
- 地址写法宽松：带不带 `/v1`、末尾多个斜杠、Azure 的 `api-version` 都认。模型列表按客户端要的格式返回。
- 中转自己报的错用客户端协议的原生格式；流一定以协议规定的结束事件收尾；上游排队时自动发保活。

**转换**
- 每次尝试用最轻的一种处理：原样转发、只改几个字段、完整转换。
- 上游缺请求要用的能力就不参与选路；一个都不剩才返回 400，并写明缺什么。
- 思考签名、加密推理这类上游私有状态，跨协议时加密交给客户端，下一轮只还给签发它的上游。

**选路和凭据**
- 按优先级和权重分流。预设可以把几个模型合成一个名字，出现在客户端的模型列表里，由客户端自己选。
- 缓存还热时会话留在原凭据上；Codex 同一轮的请求固定到同一把凭据。
- 失败原因由上游插件判断：限流冷却到重置时间，疑似封号立刻隔离，内容违规不重试，连续失败熔断。
- 凭据分共享、个人（只给本人用）、服务账号三种。每把凭据固定一个代理，代理不通就失败，不退回直连。

**可查**
- 请求日志列出每次上游尝试：哪个上游、怎么处理的、耗时、成本、缓存读写、上游原始用量。默认不记正文。
- 认得出请求来自哪个客户端，按客户端、模型、会话看花费，可导出 CSV。

**上游**
- [`manifests/`](manifests) 里有 108 份现成清单，覆盖 OpenAI、Anthropic、Gemini、DeepSeek、Kimi、智谱、通义等厂商，Azure、Bedrock、Vertex 等云平台，Groq、硅基流动等推理服务商，OpenRouter 等聚合网关，以及 vLLM、Ollama 等自部署引擎。每份都注明核对过的厂商文档。
- 外部插件通过 stdio 上的 JSON-RPC 和中转通信，附 Go SDK（[插件](docs/plugins.zh-CN.md)）。

**可选功能**
- 限流与额度、路由策略、请求日志、内容安全、计费、自助注册、单点登录、[MCP 网关](docs/mcp.zh-CN.md)、[通知](docs/notifications.zh-CN.md)、价格同步、公开页面、监控指标导出。每项一个开关、一张设置表单（[配置](docs/configuration.zh-CN.md#features)）。
- 想对外收费：预充值余额、兑换码、Stripe 和易支付、订阅套餐、邀请返利，还能从 new-api / one-api 导入数据（[收费功能](docs/sell-kit.zh-CN.md)）。

**部署**
- 一个二进制，控制台打包在里面，界面有中英文和深浅色。
- 数据库用 SQLite（默认）、PostgreSQL 或 MySQL；多节点共享状态用 Redis。
- 登录支持密码、OIDC / GitHub / 飞书单点登录、TOTP 和通行密钥。

<a id="quickstart"></a>
## 快速开始

从源码编译，需要 Go 1.26+ 和 Node 20.19+：

```bash
git clone https://github.com/initxy/plugrelay && cd plugrelay
make web build                 # 产物是 bin/plugrelay，控制台已打包进去
sudo install bin/plugrelay /usr/local/bin/
```

发布第一个版本后，[Releases](https://github.com/initxy/plugrelay/releases) 会提供 linux / darwin / windows 的 amd64 和 arm64 二进制，镜像是 `ghcr.io/initxy/plugrelay`。`go install` 也能装，但不带控制台。

起一个中转，接上 DeepSeek：

```bash
plugrelay init                                    # 生成 relay.yaml、主密钥和 SQLite 数据库
export DEEPSEEK_API_KEY=sk-...
plugrelay upstream add --manifest deepseek --credential-env DEEPSEEK_API_KEY
plugrelay model add --name deepseek-chat --route deepseek:deepseek-chat
plugrelay key create --name me                    # 打印 pr_...，只显示这一次
plugrelay serve                                   # 监听 http://127.0.0.1:8787
```

把客户端指过来：

```bash
plugrelay setup claude-code --key pr_... --model deepseek-chat
plugrelay setup codex --key pr_... --model deepseek-chat
eval "$(plugrelay setup gemini --key pr_... --print)"
```

其他客户端见[接入客户端](docs/clients.zh-CN.md)，团队部署和 Docker 见[部署](docs/deployment.zh-CN.md)。

<a id="architecture"></a>
## 架构

<p align="center"><img src="docs/assets/overview.zh-CN.svg" alt="plugrelay 架构总览" width="900"></p>

所有请求走同一条流程。核心代码里不出现任何客户端或厂商的名字：客户端之间的差异在协议编解码器里处理，上游之间的差异在清单和上游插件里处理。详见[架构](docs/architecture.zh-CN.md)。

<a id="documentation"></a>
## 文档

- [架构](docs/architecture.zh-CN.md)：一次请求怎么走、协议转换、选路、出站规则
- [接入客户端](docs/clients.zh-CN.md)：每个客户端怎么接，走中转会少什么
- [客户端兼容记录](docs/compat/README.zh-CN.md)：真实 Claude Code、Codex 经中转跑任务的结果
- [配置](docs/configuration.zh-CN.md)：relay.yaml、环境变量、分层设置、YAML 导入导出
- [部署](docs/deployment.zh-CN.md)：本机、团队、Docker Compose、安全检查、备份
- [插件](docs/plugins.zh-CN.md)：YAML 清单、外部插件、内置插件、一致性测试
- [MCP 网关](docs/mcp.zh-CN.md) · [通知](docs/notifications.zh-CN.md) · [收费功能](docs/sell-kit.zh-CN.md) · [管理 API](docs/api.zh-CN.md)
- [横向对比](docs/comparison.zh-CN.md)：和 new-api、sub2api、CLIProxyAPI、OpenRouter、LiteLLM 等逐项比，包括我们不如人家的地方
- [术语表](CONTEXT.md) · [架构决策记录](docs/adr/) · [参与贡献](CONTRIBUTING.zh-CN.md) · [安全](SECURITY.md)

<a id="non-goals"></a>
## 不做的事

- **不做订阅号池。**把个人订阅拿给多人共用违反厂商条款，plugrelay 不提供这个功能，也不留接口。
- **不冒充官方客户端。**不伪造 TLS 指纹、请求头、`metadata.user_id` 或系统提示词。个人凭据只给本人用。

<a id="license"></a>
## 许可证

[Apache-2.0](LICENSE)
