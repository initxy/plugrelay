# 配置

[English](configuration.md) | 简体中文

plugrelay 的配置分三类，每类只有一个存放位置：

| 是什么 | 放在哪 | 谁来写 |
|-|-|-|
| 进程怎么启动：监听地址、可信代理、主密钥、数据库、状态后端、出口、各项上限、外部插件目录、日志、停机、原生直通、WebSocket 限制 | `relay.yaml` + `PLUGRELAY_*` 环境变量 | 你手工写；plugrelay 从不改它 |
| 业务数据：上游、凭据、模型和路由、预设、分组、价格、开了哪些功能、扩展、清单、站点设置（公网地址、站点名称、币种、时区、登录规则） | 存储（SQLite、PostgreSQL 或 MySQL） | 控制台、管理 API、`plugrelay upstream/model/preset/key/apply` |
| 模块设置，按 全局 → 分组 → 用户 → 密钥 分层 | 存储 | 功能的表单（全局层）、分组/用户/密钥各自的设置，或 `plugrelay apply` |

权威定义在 `internal/config/config.go`（relay.yaml）、`internal/store/yamlio.go`（业务数据 YAML）和 `pkg/plugin/settings.go`（设置）。带注释的示例见 [`examples/relay.yaml`](../examples/relay.yaml)。

- [relay.yaml 参考](#relayyaml-reference)
- [功能](#features)
- [相对路径相对于哪里](#where-relative-paths-resolve)
- [路由、模型和预设](#routing-models-and-presets)
- [分层的插件设置](#layered-plugin-settings)
- [用 YAML 管理业务数据](#business-data-as-yaml)

<a id="relayyaml-reference"></a>
## relay.yaml 参考

未知的键会报错，所以拼错的键在启动时就会失败。所有问题一次性全部报出来，并带上字段路径（`trusted_proxies[2]: want a CIDR or an IP`）。可以用 `plugrelay doctor` 检查配置文件。

配置文件默认是 `./relay.yaml`，除非用 `--config FILE` 或 `PLUGRELAY_CONFIG` 另行指定。

<a id="top-level"></a>
### 顶层

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `listen` | `127.0.0.1:8787` | `PLUGRELAY_LISTEN` | `host:port`。除非有反向代理或团队需要访问，否则保持回环地址。只监听回环地址时，本机打开控制台不用登录（站点设置 `security.local_owner_login`，见 [API 认证](api.zh-CN.md)）。 |
| `trusted_proxies` | `[]` | `PLUGRELAY_TRUSTED_PROXIES`（逗号分隔） | 中转站前面的反向代理的 CIDR 或 IP。只有 TCP 对端是其中之一时，才从 `X-Forwarded-For` 取客户端地址（从右往左找，跳过可信的中间代理），没有就用 `X-Real-IP`。留空时忽略所有转发头。取到的地址用于密钥和 `middleware.access` 的 IP 白名单、按 IP 限流、请求日志、管理员登录限流和审计日志；本机所有者的判断始终用 TCP 对端。 |
| `master_key` | 空 | `PLUGRELAY_MASTER_KEY` | 32 个随机字节，base64 编码（标准或 URL 字母表均可）。用来加密凭据、密文设置和加密保存的私有状态。它和 `master_key_file` 二选一。 |
| `master_key_file` | 空（`plugrelay init` 会写出 `master.key`） | `PLUGRELAY_MASTER_KEY_FILE` | 存放主密钥的文件。设置其中任一环境变量会清掉另一个字段。 |
| `previous_master_keys` | `[]` | `PLUGRELAY_PREVIOUS_MASTER_KEYS`（逗号分隔） | 被 `master_key` 替换掉的旧主密钥，格式相同。它们只能解开换密钥之前加密的私有状态；已存的凭据和密文设置仍然需要保存时用的那把密钥（[更换主密钥](deployment.zh-CN.md#master-key-rotation)）。 |

没有主密钥时，需要保存密钥类数据的命令都会失败（`store: no master key configured`）。主密钥丢了，已存的凭据就读不出来了（[备份](deployment.zh-CN.md#backups)）。

<a id="store"></a>
### store

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `store.driver` | `sqlite` | `PLUGRELAY_STORE_DRIVER` | `sqlite`（纯 Go，单节点）、`postgres` 或 `mysql`（MySQL 8.0.13+ 或 MariaDB 10.6+）。 |
| `store.dsn` | `plugrelay.db` | `PLUGRELAY_STORE_DSN` | SQLite：文件路径（或 `file:` URI）。PostgreSQL：`postgres://user:pass@host:5432/db?sslmode=…`。MySQL：go-sql-driver 格式的 DSN，`user:pass@tcp(host:3306)/db`（数据库必须已存在；`tls=true` 等参数原样透传，但会强制使用 utf8mb4 和二进制排序规则、按匹配行计数、会话级 `sql_mode` 和 READ COMMITTED，并拒绝 `LOAD DATA LOCAL`）。必填。 |
| `store.max_conns` | `0` | `PLUGRELAY_STORE_MAX_CONNS` | 每个节点打开的数据库连接数；`0` 表示 8（SQLite）或 20（PostgreSQL、MySQL）。节点数 × 这个值要小于数据库的 `max_connections`。 |

迁移脚本内嵌在程序里，打开存储时自动执行。

<a id="state"></a>
### state

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `state.driver` | `memory` | `PLUGRELAY_STATE_DRIVER` | `memory`（单节点，重启就丢）、`sql`（单节点，存在存储数据库里，重启不丢）、`redis`（多个节点同时服务时必须用它）。 |
| `state.url` | 空 | `PLUGRELAY_STATE_URL` | `redis://user:pass@host:6379/0`。用 `redis` 时必填。 |

状态丢了只会损失提示词缓存命中和计数器，不会影响正确性。

<a id="external_plugins"></a>
### external_plugins

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `external_plugins.enabled` | `false` | `PLUGRELAY_EXTERNAL_PLUGINS_ENABLED` | 打开外部进程插件这一层。关着时不会启动任何插件进程。 |
| `external_plugins.dir` | `plugins` | `PLUGRELAY_EXTERNAL_PLUGINS_DIR` | 扫描插件目录的位置（每个插件目录里有一个 `plugin.yaml`）。开启时必填。 |

见 [plugins.md](plugins.zh-CN.md#external-process-plugins)。

<a id="log"></a>
### log

| 字段 | 默认值 | 环境变量 | 取值 |
|-|-|-|-|
| `log.level` | `info` | `PLUGRELAY_LOG_LEVEL` | `debug`、`info`、`warn`、`error` |
| `log.format` | `text` | `PLUGRELAY_LOG_FORMAT` | `text`、`json` |

<a id="egress"></a>
### egress

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `egress.allow_private_cidrs` | `[]` | `PLUGRELAY_EGRESS_ALLOW_PRIVATE_CIDRS`（逗号分隔） | 为防 SSRF，私网和回环地址默认屏蔽；自建上游（vLLM、Ollama、LM Studio）所在的网段写在这里。 |
| `egress.default_timeouts.first_byte` | `2m` | `PLUGRELAY_EGRESS_FIRST_BYTE_TIMEOUT` | 等上游响应第一个字节的时间 |
| `egress.default_timeouts.idle` | `5m` | `PLUGRELAY_EGRESS_IDLE_TIMEOUT` | 流式响应两个分块之间的间隔 |
| `egress.default_timeouts.total` | `30m` | `PLUGRELAY_EGRESS_TOTAL_TIMEOUT` | 整次尝试；不能比另外两个短 |

时长可以写 Go 格式（`90s`、`2m`），也可以写秒数。三个值都必须大于零。

每个上游都可以在存储里覆盖这些设置（控制台：上游 → 高级；管理 API：上游的 `headers`、`proxy`、`timeouts`）：

- `timeouts.connect_ms / first_byte_ms / idle_ms / total_ms` 逐项替换默认值；填 0 保持默认。连接超时涵盖建连、代理和 TLS，只能比内置的 10 秒建连和握手上限更短。
- `proxy` 是这个上游下没设代理的凭据所用的出口；凭据自己设了代理时以凭据为准。和凭据代理一样，代理不通时这次尝试直接失败，绝不退回直连。
- `headers` 会加到每个请求上。服务商自己管的请求头（认证和账号相关的）永远不会被覆盖；凭据、分帧、逐跳和客户端地址类的请求头一律拒绝（`egress.SetExtraHeaders`）。

<a id="limits"></a>
### limits

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `limits.max_body_bytes` | `33554432`（32 MiB） | `PLUGRELAY_LIMITS_MAX_BODY_BYTES` | 请求体发来时的大小；流式上传也受它限制 |
| `limits.max_decompressed_bytes` | `134217728`（128 MiB） | `PLUGRELAY_LIMITS_MAX_DECOMPRESSED_BYTES` | gzip/zstd 解压后的大小；必须 ≥ `max_body_bytes` |

<a id="shutdown"></a>
### shutdown

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `shutdown.grace` | `30s` | `PLUGRELAY_SHUTDOWN_GRACE` | 开始停机（SIGINT 或 SIGTERM）后，进行中的请求还能继续跑多久；到时间就关掉它们的连接 |
| `shutdown.drain` | `60s` | `PLUGRELAY_SHUTDOWN_DRAIN` | 之后中转站再等多久，让被切断的请求结算额度、让排队中的记录交给各个记录接收插件 sink（如请求日志）；记录队列至少还会多等 10 秒。超时仍未送达的记录会写日志并计为丢失。`0` 表示用默认值 |

进程管理器在杀掉中转站之前，等待时间必须比这两者之和更长：见[优雅停机](deployment.zh-CN.md#graceful-shutdown)。

<a id="console"></a>
### console

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `console.enabled` | 开 | `PLUGRELAY_CONSOLE_ENABLED` | 提供网页控制台和管理 API |

关掉控制台后，`/api/v1` 下的管理 API 仍然提供，只是不再提供网页（`internal/app/app.go`）。

单点登录方式不在 relay.yaml 里：它们是业务数据，在控制台里设置（[部署](deployment.zh-CN.md#single-sign-on)）；公网地址（站点设置 `site.public_base_url`）、两步登录策略和登录验证码（站点设置 `security.*`，[部署](deployment.zh-CN.md#two-step-sign-in-passkeys-and-captcha)）也一样。

<a id="native"></a>
### native

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `native.enabled` | `false` | `PLUGRELAY_NATIVE_ENABLED` | 提供 [`/native/<upstream>/<path>`](clients.zh-CN.md#native-passthrough)；关着时这个路由返回 404 |
| `native.max_body_bytes` | `536870912`（512 MiB） | `PLUGRELAY_NATIVE_MAX_BODY_BYTES` | 单个原生请求体的大小（上传是流式转发的，不缓存在内存里）；`0` 表示用默认值 |

只打开它并不会放行任何人：每个密钥还需要 `middleware.access` 的 `native_upstreams` 设置（上游 id 列表，`*` 表示全部；留空表示都不行），可以设在全局、分组、用户或密钥层。和其他访问列表不同，它是一种授权，合并方式是 `override`——最近一层设的值生效——所以可以单独放行一个分组或一个密钥。

<a id="websocket"></a>
### websocket

[WebSocket 会话](clients.zh-CN.md#realtime-and-websocket)（`/v1/realtime`、Responses 的 WebSocket 模式）的限制。

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `websocket.max_session` | `30m` | `PLUGRELAY_WEBSOCKET_MAX_SESSION` | 会话最长持续时间，到时关闭；最多 `30m`，即一个并发名额（凭据的和 `middleware.ratelimit` 的）的有效期 |
| `websocket.idle_timeout` | `5m` | `PLUGRELAY_WEBSOCKET_IDLE_TIMEOUT` | 双向都没有消息往来超过这么久就关闭 |
| `websocket.session_ready_timeout` | `10s` | `PLUGRELAY_WEBSOCKET_SESSION_READY_TIMEOUT` | 在 Realtime 上，如果改写请求体的插件或转写检查需要上游的 `session.created`，客户端事件会先等它；等了这么久还没来就关闭会话（先发 `error` 事件 `session_not_ready`，再以 `1013` 关闭） |
| `websocket.max_message_bytes` | `16777216`（16 MiB） | `PLUGRELAY_WEBSOCKET_MAX_MESSAGE_BYTES` | 任一方向单条消息的大小；超过就以 `1009` 关闭会话 |
| `websocket.response_reserve_tokens` | `4096` | `PLUGRELAY_WEBSOCKET_RESPONSE_RESERVE_TOKENS` | 响应自己没设上限时，计量模块为它预留的输出 token 数（[计费](clients.zh-CN.md#realtime-and-websocket)） |

<a id="mcp-gateway"></a>
### MCP 网关

`/mcp` 没有 relay.yaml 字段：`mcp` 功能开着时网关就运行；MCP 服务器是业务数据，在控制台或 `/api/v1/mcp/servers` 里设置；密钥通过 `middleware.access` 的 `mcp_servers`、`mcp_tools_allow` 和 `mcp_tools_deny` 设置获得使用权（[mcp.md](mcp.zh-CN.md#access)）。

<a id="other-environment-variables"></a>
### 其他环境变量

| 变量 | 谁读取 | 含义 |
|-|-|-|
| `PLUGRELAY_CONFIG` | 所有命令 | relay.yaml 的路径（等同 `--config`） |
| `PLUGRELAY_DATA_DIR` | 所有命令 | `--data-dir` 的默认值（Docker 镜像设为 `/data`） |
| `PLUGRELAY_ADMIN_PASSWORD` | `plugrelay init`、`plugrelay user reset-password` | 管理员密码，这样它不会留在 shell 历史里 |
| `PLUGRELAY_API_KEY` | `plugrelay setup`；Codex | 平台密钥；`setup codex` 会让 Codex 从这里读取 |
| `PLUGRELAY_ADMIN_TOKEN` | `serve` | 访问管理 API 和 `/metrics` 的运维 Bearer 令牌 |
| `PLUGRELAY_TEST_MODE` | `serve` | 设成任何值，都会去掉所有往本机以外上报数据的插件 |
| `PLUGRELAY_TEST_PG`、`PLUGRELAY_TEST_MYSQL`、`PLUGRELAY_E2E_BASE`、`PLUGRELAY_E2E_KEY`、`PLUGRELAY_E2E_MODEL` | 仅测试 | 存储、状态和管理后台测试用的 PostgreSQL URL / MySQL DSN（`root:pw@tcp(127.0.0.1:3306)/db`；每个测试自己建库、自己删库，所以这个用户需要 `CREATE`/`DROP` 权限）；`make e2e` 用的真实 OpenAI 兼容上游（带 `/v1` 的 base URL、密钥、模型——默认 `gpt-5.4`） |
| `PLUGRELAY_E2E_CLIENTS`、`PLUGRELAY_E2E_CHAT_MODEL` | 仅测试 | 设为 `1` 时，还会让本机安装的 Claude Code 和 Codex CLI 经中转站完成一个写代码并运行的任务，分别走主模型和一条只支持 chat 的路由（默认 `deepseek-chat`）；配置通过 `plugrelay setup` 写到一个临时 home 目录（`test/e2e/clients_test.go`） |
| `PLUGRELAY_E2E_IMAGE_MODEL`、`_EMBED_MODEL`、`_TTS_MODEL`、`_STT_MODEL`、`_RERANK_MODEL`、`_REALTIME_MODEL`、`_BATCH_WAIT` | 仅测试 | `make e2e` 里媒体、重排和实时用例在同一上游上用的模型，以及等批处理完成的时长（默认 0：只测创建、查询和取消） |
| `PLUGRELAY_E2E_TEI_EMBED`、`_TEI_RERANK`、`_STT_BASE`；`PLUGRELAY_E2E_MCP`（`_MCP_EVERYTHING`、`_MCP_SDK`）；`PLUGRELAY_E2E_CAPTCHA` | 仅测试 | 需要手动开启的真实环境测试：本地 TEI 和 whisper 服务（`selfhosted_test.go`）；设为 `1` 时用官方参考服务器和 SDK 测 MCP 网关，版本可覆盖（`mcp_test.go`）；设为 `1` 时对 Turnstile 和 hCaptcha 真实的 siteverify 校验验证码（`captcha_test.go`） |
| `PLUGRELAY_COMPAT_UPSTREAM`、`_DATE`、`_EVENTS` | `scripts/compat.sh` | 兼容性报告里的上游名称和日期；要保留的 `go test -json` 事件文件，或用来重新生成报告的事件文件 |

每个测试变量的默认值和细节，写在 `test/e2e/` 里对应文件开头的注释中。

<a id="features"></a>
## 功能

运行哪些模块由 `internal/features` 决定（这是功能注册表：名称、模块、默认值、每个表单显示哪些设置、各自增加哪些控制台页面）；功能的开关状态存在存储里，下次重新加载时生效（`internal/app/plugins.go` 里的 `App.states`）：

- **核心模块**始终运行：`auth.apikey`、`middleware.access`、`selector.routing`、`sink.requestlog`、`job.requestlog-retention`、`job.modelsync`、`job.quotaprobe`，以及上游用到的每个 `provider.*`。
- **功能**开着时运行它的模块。打开一个功能会连带打开它的模块所依赖的东西；如果某个改动会留下中转站跑不起来的配置，就拒绝（409）。可以在控制台里切换，也可以用 `PATCH /api/v1/features/{id}`，或 `plugrelay apply` 里的 `features:`。
- **扩展**（外部进程插件、自定义构建里的 Go 插件）默认关闭，逐个开关（`/api/v1/extensions`，或 `plugrelay apply` 里的 `plugins:`）；`apply` 会拒绝指向内置模块的 `plugins:` 条目。

| id | 默认 | 模块 | 作用 |
|-|-|-|-|
| `limits` | 开 | `middleware.ratelimit`、`meter.quota`、`middleware.param-policy` | 按密钥、用户或分组限制每分钟请求数/token 数和并发；花费和 token 预算（按天和按月的周期从站点时区的零点算起，站点设置 `site.timezone`，默认 UTC）；输出 token 和推理强度的上限，剔除指定参数 |
| `routing` | 始终 | （核心的 `selector.routing`、`job.modelsync`、`job.quotaprobe`） | [路由策略](#routing-models-and-presets)，以及两个定时任务的默认间隔 |
| `request_log` | 始终 | （核心的 `sink.requestlog`、`job.requestlog-retention`） | 是否保存请求和响应正文（`body_logging`，默认关），以及日志保留多少天（`retention_days`，0 表示永久） |
| `content_safety` | 关 | `middleware.guardrails`、`middleware.redact` | 黑名单、个人信息和密钥检测、提示词大小、禁用的模型和工具、内容审核（[插件](plugins.zh-CN.md#guardrails)）；在用户文本发出前打码 |
| `billing` | 关 | `meter.balance`、`admin.payment`、`admin.redeem`、`admin.subscriptions`、`admin.import-newapi`、`admin.rewards`、`admin.announcements` | 预付余额、支付（Stripe、易支付 epay）和优惠券、兑换码、订阅套餐、签到和邀请奖励、公告、从 new-api 导入（[售卖套件](sell-kit.zh-CN.md)） |
| `signup` | 关 | `admin.signup` | 公开注册，可选邀请码和邮箱域名限制 |
| `sso` | 关 | — | 单点登录（[部署](deployment.zh-CN.md#single-sign-on)）：关着时不提供任何登录方式，密码登录始终可用 |
| `mcp` | 关 | — | [MCP 网关](mcp.zh-CN.md)；关着时 `/mcp` 返回 404 |
| `notifications` | 关 | `job.notify`、`sink.webhook` | 通过邮件、聊天机器人或 webhook 发告警，并把请求记录推送到 webhook（[通知](notifications.zh-CN.md)） |
| `price_sync` | 关 | `job.pricesync` | [从 models.dev 获取价格建议](#price-sync) |
| `public_pages` | 关 | `admin.public-pages` | 免登录的模型价格页（`/public/models`）和状态页（`/public/status`），登录页上有链接；只展示一个分组的模型和价格（`price_group`，留空表示全部模型、按标价）；只有 `site.show_upstream_names` 开着时才显示上游名称；结果缓存一分钟，访问按地址限流（`per_ip_per_minute`） |
| `metrics` | 开 | `sink.metrics` | Prometheus 指标，前缀 `plugrelay_`，在 `/metrics`（设置了 `PLUGRELAY_ADMIN_TOKEN` 时需要它才能访问） |

安装向导选“售卖”时会打开 `billing`。没安装的扩展 id 会导致这个插件失败（`plugin … is not installed`）；`plugrelay doctor` 和功能页上的重新加载错误会显示出来。

<a id="where-relative-paths-resolve"></a>
## 相对路径相对于哪里

`master_key_file`、普通的 SQLite `store.dsn` 和 `external_plugins.dir` 如果给了 `--data-dir`（或 `PLUGRELAY_DATA_DIR`），就相对于它解析，否则相对于配置文件所在目录——而不是当前工作目录（`internal/cli/config.go`）。没设 `--data-dir` 时，`plugrelay init` 在配置文件旁边写不带路径的文件名，这样数据目录整个搬走也能用。

<a id="routing-models-and-presets"></a>
## 路由、模型和预设

- **路由**属于某个模型：上游、上游的模型名、优先级、权重，以及哪些分组可以用（`plugrelay model add --route`，管理 API 和 YAML 里模型的 `routes`）。上游本身没有模型映射。
- **路由策略**：`selector.routing` 的 `strategy` 设置，可以全局设，也可以按分组设——`priority_weight`（默认：先看优先级，再把新会话按权重分配）、`cheapest`（按上游价格：有价格规则用规则，否则用清单里的标价）、`fastest`（最近的首字节耗时）或 `quota_reset`（额度最先重置的优先）。不管哪种策略，会话粘住的候选始终排在最前。启用了外部选路插件时由它来排序。
- **预设**：一个名称、按顺序排列的候选模型，以及挑选策略（`in_order`、`cheapest` 或 `fastest`；`plugrelay preset add`、`/presets`）。只要调用方能用预设里的某个模型，这个预设就会出现在每种客户端协议的模型列表里；按预设名请求时，从调用方能用的模型中按它的挑选策略（取代路由策略）选一个来服务，失败就换下一个。按名字请求的模型永远不会被换成别的模型。请求日志里，请求的模型记为预设名，同时记下实际提供服务的模型。模型和预设共用一个命名空间。
- **按上游设置的定时任务**（上游上的管理 API 字段）：`model_sync` 控制是否对这个上游运行 `job.modelsync`（默认：清单里列了模型就开）；`quota_probe` 是 `job.quotaprobe` 检查这个上游凭据的频率（至少 `1m`），也可以填 `off` 或 `0`；留空则用这个任务的 `interval`。
- **调用方能用哪些模型**：分组的 `models`（名称或通配符），再由密钥自己的 `models` 进一步收窄。

<a id="layered-plugin-settings"></a>
## 分层的插件设置

插件声明自己读取哪些设置（`pkg/plugin/settings.go` 里的 `plugin.Setting`）：键、类型、默认值、可以设在哪些层，以及各层怎么合并。核心每个请求合并一次；插件用 `Call.Setting(key)` 读取合并后的值。

- **层**：全局 → 分组 → 用户 → 密钥（一个密钥最多属于一个用户和一个分组）。
- **`override`**：最近一层设的值生效。
- **`strictest`**：下层只能收紧。数字：取较小的正数，`0` 表示不限。布尔值：`false` 优先。列表（白名单，空表示不限制）：每个设了非空列表的层都必须允许这个条目，所以通配符逐层收窄，空的层放不开任何东西，没有共同条目的几层合在一起什么都不允许。对象的设置（`GET /api/v1/groups/{id}/settings`，用户和密钥同理）会在这类字段下列出每一层的列表，并指出是哪一层导致什么都不允许；外部插件从 `setting_layers` 拿到各层的值（[协议](../sdk/PROTOCOL.zh-CN.md)）。管理员可以让某个密钥不受某个 strictest 设置约束。
- **`union`**（只用于列表）：各层的条目累加，所以下层能扩充黑名单，但不能缩小；被豁免的密钥只保留自己那一层的列表。
- **无效值**：控制台、管理 API 和 `plugrelay apply` 都会拒绝不符合设置定义的值，所以存储里的无效值只可能来自手工改过的数据库，或者后来改了声明的插件。这样的层如果属于 strictest 列表，就什么都不允许；如果属于 union 列表（黑名单），保留其中有效的字符串条目（单独一个字符串算一条）；其他设置则忽略这一层，当作没设，所以无效的 strictest 数字会退回到其他层或默认值（`settings.LayerValue`）。加载配置时，每一条无效记录都会打一条警告日志，写明设置、层和所属对象。
- **类型**：`string int float bool duration list secret model upstream json`。`secret` 类型的值用主密钥加密，永不回显；`model` 和 `upstream` 在控制台里显示为选择器。金额类设置（`unit: money`）以微单位存储，管理 API 以货币单位显示和接收。
- **重启**：标了 `restart` 的设置要重启后才生效；控制台会标出来。
- **由记录本身决定的层**：密钥的预算（`meter.quota` 的 `key_monthly_micro`、`key_total_micro`）、`rpm` 和并发（`middleware.ratelimit`）来自密钥记录自己的字段，分组的 `body_logging`（`sink.requestlog`）来自分组的 `log_bodies`；管理 API 和 `plugrelay apply` 都不会把这些层存成设置（`settings.RecordOwned`）。合并规则照样在其上生效。
- **每一层在哪里设**：全局层在功能（或扩展）的表单里，`PATCH /api/v1/features/{id}`；分组、用户或密钥层在对应对象上，`PUT /api/v1/{groups,users,keys}/{id}/settings`。

合并和校验的代码在 `internal/settings`。

<a id="business-data-as-yaml"></a>
## 用 YAML 管理业务数据

`plugrelay export` 导出业务数据；`plugrelay apply` 在一个事务里写入（有则更新，无则新建）。文档里没提到的行保持不动。用户、密钥、请求日志和用量从不包含在内。

```bash
plugrelay export -o relay-data.yaml                 # 不导出凭据的密钥
plugrelay export -o full.yaml --with-secrets        # 和密钥本身一样敏感
plugrelay apply -f relay-data.yaml --dry-run --json # 只显示会新建/更新什么，不写入
cat team.yaml | plugrelay apply -f -
```

文档结构（`internal/store/yamlio.go` 里的 `store.Document`）：

```yaml
groups:
  - {id: vip, name: VIP, settings: {}, price_multiplier: 1.5}
upstreams:
  - id: deepseek
    provider: provider.openai-compatible   # 清单对应的 provider.<interpreter>
    manifest: deepseek           # 官方或已存的清单 id
    config: {}                   # 清单变量、issuer 覆盖
    enabled: true                # 默认 true
credentials:
  - id: ds-team-1
    upstream: deepseek
    kind: shared                 # shared | personal | service
    owner: ""                    # 用户 id，personal 时必填
    secret: sk-...               # 明文；省略则保留已存的密钥
    proxy: ""                    # 固定出口；"" = 直连
    max_concurrency: 8
models:
  - name: deepseek-chat
    routes:                      # 省略则保留已存的路由；[] 删除全部路由
      - {upstream: deepseek, upstream_model: deepseek-chat, priority: 10, weight: 1, groups: []}
presets:
  - {name: auto-coding, models: [claude-sonnet-4-5, deepseek-chat], strategy: in_order}  # in_order | cheapest | fastest
prices:                          # 每百万 token 的微单位；units：按单位计价
  - {model: deepseek-chat, input: 270000, cache_read: 70000, output: 1100000, currency: USD}
  - {model: gpt-image-1, units: {image: 40000, "image:1024x1024:high": 167000}, currency: USD}
  - {model: gpt-5.4-mini, input: 750000, output: 4500000, batch_discount: 0.5, currency: USD}
features:
  - {id: billing, enabled: true}           # internal/features 里的 id
plugins:                                   # 只能是扩展
  - {id: ip-allowlist, enabled: true, order: 1010}  # examples/plugins/ip-allowlist-go
settings:
  - {scope: group, scope_id: vip, plugin: middleware.ratelimit, key: rpm, value: 600}
manifests:
  - {id: my-upstream, yaml: "id: my-upstream\nkind: provider\n..."}
```

导出时密文设置仍然用主密钥加密，所以这样的文档只能应用到主密钥相同的存储上。

<a id="prices"></a>
### 价格

所有价格都用站点币种，单位是每百万 token 的微单位。媒体类操作另外在 `units` 里设单位价格，单位是每单位的微单位：

| 键 | 按什么计 | 从哪里计量 |
|-|-|-|
| `image` | 每生成一张图 | 响应里的图片数；事件流响应按请求的 `n` 计费（默认 1） |
| `image:<size>`、`image:<size>:<quality>` | 指定尺寸（和质量）的每张图；最具体的键优先 | 响应里的 `size` / `quality`，没有就看请求 |
| `audio_second` | 每秒输入音频 | 转写结果的 `usage.seconds` 或 `duration`；转写结果两者都没有、也没有 token 用量时（纯文本、SRT、VTT、只有 `text` 的 JSON、事件流），按上传文件的时长：从 WAV、MP3、FLAC、Ogg、MP4/M4A 或 WebM 的文件头读取，但不少于按 1536 kbit/s 换算的字节时长（文件头是客户端给的；48 kHz 16 位立体声 PCM 是最密的常见格式），读不出来就按 64 kbit/s 换算 |
| `1k_chars` | 每 1000 个输入字符 | 语音合成的 `input` |
| `search_unit` | 每个重排搜索单位 | Cohere 的 `meta.billed_units.search_units`；其他重排上游按 token 计费（按输入单价） |

响应读不出用量时会用估算值顶替，请求日志里会标为估算。上游报告了 token 时（按 token 计费的图像和转写模型、嵌入的输入），token 单价另外叠加计算。清单里只有 token 标价，所以单位价格只能来自价格规则。

`batch_discount`（0 到小于 1）是通过[批处理 API](clients.zh-CN.md#batches) 提供的用量减免的比例：0.5 表示批处理按规则单价的一半计费。它作用于给这次批处理定价的那条规则——扣费看模型的规则，成本看上游模型的规则——默认是 0（全价），清单标价也是如此；OpenAI 和 Anthropic 的批处理都是半价，所以在它们的规则上设 0.5。

- 一次尝试的**成本**（运营方的支出）：先找这个上游加这个上游模型的规则，再找这个上游的规则，再找名字和上游模型相同的模型规则，最后用上游清单里的标价（`pricing:`，只在其币种和站点币种相同时使用）。
- **扣费**（调用方付的钱）：用实际提供服务的模型的规则（如果是预设，先用成员模型的规则，没有再用预设的），没有就用最终交付那次尝试的成本单价，再乘以调用方所在分组的 `price_multiplier`（没有分组时为 1）。每个单价乘完后四舍五入到微单位，请求总额也四舍五入。调用方只看到最终价格；请求日志按每一档列出 token 数 × 单价，管理员还能看到倍率。花费预算（`meter.quota`）统计的是扣费。

<a id="price-sync"></a>
#### 价格同步

`price_sync` 功能开着时，`job.pricesync` 每隔 `interval` 通过核心出口下载 [models.dev](https://models.dev) 的目录（`url`），并在里面查找每个模型：先在路由所属的服务商下按上游模型名找，找不到再在模型厂商的条目下按上游模型名和模型名找，查找时去掉厂商或地区前缀（`plugins/job/pricesync`）。站点币种不是美元时，美元价格按 `usd_rate` 换算。找到的结果按模型存下来（`store.PriceSync`），在价格页上作为建议显示：

- 运营方接受之前什么都不变（接受后模型会有自己的规则，来源记为“price sync”）；拒绝过的价格不会再建议，换了价格才会再提；
- 设为跟随 models.dev 的模型会自动采用新价格，但前提是它自己的规则不存在、或者本来就来自同步：手工填的价格永远优先（`TestPriceSync`）；
- 目录里的上下文窗口和能力信息，会补上模型自身信息在目录页上缺的部分。
