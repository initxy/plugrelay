# 配置

[English](configuration.md) | 简体中文

配置分三类，每类只放在一个地方：

| 是什么 | 放在哪 | 谁来写 |
|-|-|-|
| 进程怎么启动（监听地址、主密钥、数据库、各项上限……） | `relay.yaml` + `PLUGRELAY_*` 环境变量 | 你手写；plugrelay 从不改它 |
| 业务数据：上游、凭据、模型、预设、分组、价格、功能开关、扩展、清单、站点设置 | 存储（SQLite、PostgreSQL 或 MySQL） | 控制台、管理 API、`plugrelay upstream/model/preset/key/apply` |
| 模块设置，按 全局 → 分组 → 用户 → 密钥 分层 | 存储 | 功能表单、各对象自己的设置、`plugrelay apply` |

以代码为准：`internal/config/config.go`（relay.yaml）、`internal/store/yamlio.go`（业务数据 YAML）、`pkg/plugin/settings.go`（设置）。带注释的示例：[`examples/relay.yaml`](../examples/relay.yaml)。

- [relay.yaml 参考](#relayyaml-reference)
- [功能](#features)
- [相对路径从哪算起](#where-relative-paths-resolve)
- [路由、模型和预设](#routing-models-and-presets)
- [分层设置](#layered-plugin-settings)
- [用 YAML 管理业务数据](#business-data-as-yaml)

<a id="relayyaml-reference"></a>
## relay.yaml 参考

配置文件默认是 `./relay.yaml`，可以用 `--config FILE` 或 `PLUGRELAY_CONFIG` 指定；文件不存在就全用默认值。不认识的键直接报错，所有问题一次报完并带字段路径（`trusted_proxies[2]: want a CIDR or an IP`）。用 `plugrelay doctor` 检查配置文件。

时长可以写 Go 格式（`90s`、`2m`），也可以写秒数。

<a id="top-level"></a>
### 顶层

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `listen` | `127.0.0.1:8787` | `PLUGRELAY_LISTEN` | `host:port`。只监听回环地址时，本机打开控制台不用登录（站点设置 `security.local_owner_login`，见 [API 认证](api.zh-CN.md)）。 |
| `trusted_proxies` | `[]` | `PLUGRELAY_TRUSTED_PROXIES`（逗号分隔） | 你的反向代理的 CIDR 或 IP。只有 TCP 对端是其中之一，才从 `X-Forwarded-For` 取客户端地址（从右往左，跳过可信代理），没有就用 `X-Real-IP`。IP 白名单、按 IP 限流、请求日志、登录限流和审计日志都用这个地址。判断本机所有者始终看 TCP 对端。 |
| `master_key` | 空 | `PLUGRELAY_MASTER_KEY` | 32 个随机字节，base64（标准或 URL 字母表）。用来加密凭据、密文设置和私有状态。和 `master_key_file` 二选一。 |
| `master_key_file` | 空（`init` 会写出 `master.key`） | `PLUGRELAY_MASTER_KEY_FILE` | 存主密钥的文件。设了其中一个环境变量，另一个字段就会被清掉。 |
| `previous_master_keys` | `[]` | `PLUGRELAY_PREVIOUS_MASTER_KEYS`（逗号分隔） | 换下来的旧主密钥。只用来解开换钥前加密的私有状态，解不开已存的密文（[换主密钥](deployment.zh-CN.md#master-key-rotation)）。 |
| `store.driver` | `sqlite` | `PLUGRELAY_STORE_DRIVER` | `sqlite`（单节点）、`postgres` 或 `mysql`（MySQL 8.0.13+ / MariaDB 10.6+）。打开存储时自动跑数据库迁移。 |
| `store.dsn` | `plugrelay.db` | `PLUGRELAY_STORE_DSN` | 必填。SQLite：文件路径或 `file:` URI。PostgreSQL：`postgres://user:pass@host:5432/db?sslmode=…`。MySQL：`user:pass@tcp(host:3306)/db`，库要先建好；`tls=true` 这类参数原样传下去，utf8mb4、`sql_mode` 和 READ COMMITTED 强制设定，拒绝 `LOAD DATA LOCAL`。 |
| `store.max_conns` | `0` | `PLUGRELAY_STORE_MAX_CONNS` | 每个节点的连接数；`0` 表示 SQLite 8 个、其他 20 个。节点数 × 这个值要小于数据库的 `max_connections`。 |
| `state.driver` | `memory` | `PLUGRELAY_STATE_DRIVER` | 运行时状态：`memory`（单节点，重启就丢）、`sql`（单节点，存在业务库里）、`redis`（多节点必须用）。丢了只损失缓存命中和计数，不影响正确性。 |
| `state.url` | 空 | `PLUGRELAY_STATE_URL` | `redis://user:pass@host:6379/0`；用 `redis` 时必填 |
| `log.level` | `info` | `PLUGRELAY_LOG_LEVEL` | `debug`、`info`、`warn`、`error` |
| `log.format` | `text` | `PLUGRELAY_LOG_FORMAT` | `text`、`json` |
| `limits.max_body_bytes` | `33554432`（32 MiB） | `PLUGRELAY_LIMITS_MAX_BODY_BYTES` | 请求体原始大小，流式上传也算 |
| `limits.max_decompressed_bytes` | `134217728`（128 MiB） | `PLUGRELAY_LIMITS_MAX_DECOMPRESSED_BYTES` | gzip/zstd 解压后的大小；不能小于 `max_body_bytes` |
| `shutdown.grace` | `30s` | `PLUGRELAY_SHUTDOWN_GRACE` | 收到 SIGINT/SIGTERM 后，进行中的请求还能跑多久（[平滑停机](deployment.zh-CN.md#graceful-shutdown)） |
| `shutdown.drain` | `60s` | `PLUGRELAY_SHUTDOWN_DRAIN` | 之后再等多久，让额度结算完、排队的记录写完；`0` 表示默认值 |
| `console.enabled` | 开 | `PLUGRELAY_CONSOLE_ENABLED` | 是否提供网页控制台；`/api/v1` 下的管理 API 不受影响 |

没有主密钥时，存密文会失败（`store: no master key configured`）；主密钥丢了，已存的凭据就读不出来了（[备份](deployment.zh-CN.md#backups)）。公网地址、单点登录、两步登录、验证码和 [MCP 服务](mcp.zh-CN.md) 都存在业务库里，不在 relay.yaml。

<a id="external_plugins"></a>
### external_plugins

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `external_plugins.enabled` | `false` | `PLUGRELAY_EXTERNAL_PLUGINS_ENABLED` | 关着就不会启动任何插件进程 |
| `external_plugins.dir` | `plugins` | `PLUGRELAY_EXTERNAL_PLUGINS_DIR` | 在这里找带 `plugin.yaml` 的插件目录（[插件](plugins.zh-CN.md#external-process-plugins)） |

<a id="egress"></a>
### egress

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `egress.allow_private_cidrs` | `[]` | `PLUGRELAY_EGRESS_ALLOW_PRIVATE_CIDRS`（逗号分隔） | 为防 SSRF，默认不许连内网和回环地址；自建上游（vLLM、Ollama 等）的网段写在这里 |
| `egress.default_timeouts.first_byte` | `2m` | `PLUGRELAY_EGRESS_FIRST_BYTE_TIMEOUT` | 等上游响应第一个字节 |
| `egress.default_timeouts.idle` | `5m` | `PLUGRELAY_EGRESS_IDLE_TIMEOUT` | 流里两个数据块之间 |
| `egress.default_timeouts.total` | `30m` | `PLUGRELAY_EGRESS_TOTAL_TIMEOUT` | 整次尝试；要大于 0，且不短于前两项 |

每个上游可以单独覆盖（控制台：上游 → 高级；或管理 API 里上游的字段）：

- `timeouts.connect_ms / first_byte_ms / idle_ms / total_ms`，逐项覆盖，0 表示用默认值。连接超时包括拨号、代理和 TLS，最长 10 秒。
- `proxy`：这个上游下没设代理的凭据都走它；凭据自己的代理优先。代理不通时这次尝试直接失败，不会改成直连。
- `headers`：每个请求都加上。provider 自己管的头不会被覆盖；凭据、传输控制（如 `Content-Length`）、逐跳（hop-by-hop）和客户端地址相关的头一律拒绝（`egress.SetExtraHeaders`）。

<a id="native"></a>
### native

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `native.enabled` | `false` | `PLUGRELAY_NATIVE_ENABLED` | 提供 [`/native/<upstream>/<path>`](clients.zh-CN.md#native-passthrough)；关着返回 404 |
| `native.max_body_bytes` | `536870912`（512 MiB） | `PLUGRELAY_NATIVE_MAX_BODY_BYTES` | 单个请求体上限（上传是流式转发，不缓存）；`0` 表示默认值 |

打开它本身不放行任何人。每把密钥还要有 `middleware.access` 的 `native_upstreams` 设置（上游 id 列表，`*` 表示全部，空表示都不行）。它按 `override` 合并，所以可以只给某个分组或某把密钥放行。

<a id="websocket"></a>
### websocket

[WebSocket 会话](clients.zh-CN.md#realtime-and-websocket)（`/v1/realtime`、Responses 的 WebSocket 模式）的限制。

| 字段 | 默认值 | 环境变量 | 含义 |
|-|-|-|-|
| `websocket.max_session` | `30m` | `PLUGRELAY_WEBSOCKET_MAX_SESSION` | 会话最长多久；不超过 `30m`，也就是一个并发名额的有效期 |
| `websocket.idle_timeout` | `5m` | `PLUGRELAY_WEBSOCKET_IDLE_TIMEOUT` | 双向都没消息这么久就关掉 |
| `websocket.session_ready_timeout` | `10s` | `PLUGRELAY_WEBSOCKET_SESSION_READY_TIMEOUT` | Realtime：插件需要时，客户端事件要等上游的 `session.created`，最多等这么久；超时发 `session_not_ready` 并以 `1013` 关闭 |
| `websocket.max_message_bytes` | `16777216`（16 MiB） | `PLUGRELAY_WEBSOCKET_MAX_MESSAGE_BYTES` | 单条消息上限（双向）；超了以 `1009` 关闭 |
| `websocket.response_reserve_tokens` | `4096` | `PLUGRELAY_WEBSOCKET_RESPONSE_RESERVE_TOKENS` | 响应没设输出上限时，先预扣这么多输出 token |

<a id="other-environment-variables"></a>
### 其他环境变量

| 变量 | 谁读 | 含义 |
|-|-|-|
| `PLUGRELAY_CONFIG` | 所有命令 | relay.yaml 路径（同 `--config`） |
| `PLUGRELAY_DATA_DIR` | 所有命令 | `--data-dir` 的默认值；Docker 镜像里是 `/data` |
| `PLUGRELAY_ADMIN_PASSWORD` | `init`、`user reset-password` | 管理员密码，免得留在 shell 历史里 |
| `PLUGRELAY_API_KEY` | `plugrelay setup`、Codex | 平台密钥；`setup codex` 让 Codex 从这里读 |
| `PLUGRELAY_ADMIN_TOKEN` | `serve` | 运维用的 Bearer token，可访问管理 API 和 `/metrics` |
| `PLUGRELAY_TEST_MODE` | 中转进程 | 设了任何值，就去掉所有会往外发数据的插件 |
| `PLUGRELAY_TEST_PG`、`PLUGRELAY_TEST_MYSQL`、`PLUGRELAY_E2E_*` | 测试 | 测试数据库和 `make e2e` 用的真实上游；每个变量的说明在 `test/e2e/` 对应文件开头 |
| `PLUGRELAY_COMPAT_UPSTREAM`、`_DATE`、`_EVENTS` | `scripts/compat.sh` | 兼容性报告里的标注和事件文件 |

<a id="features"></a>
## 功能

能跑哪些东西由 `internal/features` 列出；开关状态存在业务库里，下次重载时生效（`internal/app/plugins.go` 的 `App.states`）。

- **核心模块**始终运行：`auth.apikey`、`middleware.access`、`selector.routing`、`sink.requestlog`、`job.requestlog-retention`、`job.modelsync`、`job.quotaprobe`，以及上游用到的 `provider.*`。
- **功能**（下表）打开时运行它的模块，并顺带打开它依赖的东西；改完会跑不起来的改动直接拒绝（409）。在控制台、`PATCH /api/v1/features/{id}` 或 `apply` 的 `features:` 里开关。
- **扩展**（外部插件、自定义构建里的 Go 插件）默认关闭，一个个单独开（`/api/v1/extensions`、`apply` 的 `plugins:`）。没装的扩展会报 `plugin … is not installed`（`plugrelay doctor` 能看到）。

| id | 默认 | 模块 | 作用 |
|-|-|-|-|
| `limits` | 开 | `middleware.ratelimit`、`meter.quota`、`middleware.param-policy` | 按密钥、用户或分组限制每分钟请求数/token 数和并发；花费和 token 额度（按天算时从站点时区 `site.timezone` 的零点起，默认 UTC）；输出 token 和思考强度上限；去掉指定参数 |
| `routing` | 始终开 | （核心） | [路由策略](#routing-models-and-presets)，以及模型同步、额度探测的间隔 |
| `request_log` | 始终开 | （核心） | 是否保存请求和响应正文（`body_logging`，默认关）、保留几天（`retention_days`，0 表示永久） |
| `content_safety` | 关 | `middleware.guardrails`、`middleware.redact` | 敏感词、个人信息和密钥识别、提示词长度、禁用模型和工具、内容审核（[插件](plugins.zh-CN.md#guardrails)）；发出前遮掉用户文本里的敏感内容 |
| `billing` | 关 | `meter.balance`、`admin.payment`、`admin.redeem`、`admin.subscriptions`、`admin.import-newapi`、`admin.rewards`、`admin.announcements` | 预付余额、Stripe 和易支付、优惠券、兑换码、订阅套餐、签到和邀请返利、公告、new-api 数据导入（[收费功能](sell-kit.zh-CN.md)）；初始化向导里选「对外卖额度」会打开它 |
| `signup` | 关 | `admin.signup` | 公开注册，可要求邀请码或限定邮箱域名 |
| `sso` | 关 | — | [单点登录](deployment.zh-CN.md#single-sign-on)；关着不显示任何登录方式，密码登录始终可用 |
| `mcp` | 关 | — | [MCP 网关](mcp.zh-CN.md)；关着 `/mcp` 返回 404 |
| `notifications` | 关 | `job.notify`、`sink.webhook` | 通过邮件、聊天机器人或 Webhook 发提醒；把请求记录推到 Webhook（[通知](notifications.zh-CN.md)） |
| `price_sync` | 关 | `job.pricesync` | [从 models.dev 拿价格建议](#price-sync) |
| `public_pages` | 关 | `admin.public-pages` | 不用登录的 `/public/models` 和 `/public/status`；显示某个分组的价格（`price_group`，空表示原价）；只有开了 `site.show_upstream_names` 才显示上游名；结果缓存一分钟，按地址限流（`per_ip_per_minute`） |
| `metrics` | 开 | `sink.metrics` | 在 `/metrics` 提供 Prometheus 指标（前缀 `plugrelay_`），设了 `PLUGRELAY_ADMIN_TOKEN` 就要带它访问 |

<a id="where-relative-paths-resolve"></a>
## 相对路径从哪算起

`master_key_file`、普通的 SQLite `store.dsn` 和 `external_plugins.dir` 如果是相对路径，设了 `--data-dir`（`PLUGRELAY_DATA_DIR`）就相对它，否则相对配置文件所在目录，不看当前工作目录（`internal/cli/config.go`）。`plugrelay init` 只写文件名，所以整个目录搬走也能用。

<a id="routing-models-and-presets"></a>
## 路由、模型和预设

- **路由**挂在模型下：上游、上游模型名、优先级、权重、允许的分组（`plugrelay model add --route`，或模型的 `routes`）。上游自己没有模型映射。
- **路由策略**：`selector.routing` 的 `strategy` 设置，可以全局设，也可以按分组设。`priority_weight`（默认；先看优先级，新会话再按权重分）、`cheapest`（价格规则，没有就用清单里的标价）、`fastest`（最近的首字节耗时）、`quota_reset`（额度最先重置的优先）。粘性候选始终排最前。开了外部选路插件时由它排序。
- **预设**：一个名字、一串按顺序的模型和挑选策略（`in_order`、`cheapest`、`fastest`；`plugrelay preset add`、`/presets`）。调用方只要能用其中一个模型，就能在模型列表里看到这个预设。请求预设时，按预设的策略从调用方能用的成员里挑一个，失败就换下一个。直接点名的模型不会被换成别的。请求日志里同时记预设名和实际服务的模型。模型和预设共用一套名字。
- **按上游的定时任务**（上游的字段）：`model_sync` 控制这个上游跑不跑 `job.modelsync`（清单里列了模型就默认开）；`quota_probe` 是额度探测间隔（至少 `1m`），`off` 或 `0` 表示不探测，留空用任务的 `interval`。
- **能用哪些模型**：分组的 `models`（名字或通配符），再被密钥自己的 `models` 收窄。

<a id="layered-plugin-settings"></a>
## 分层设置

每个插件声明自己读哪些设置（`pkg/plugin/settings.go` 的 `plugin.Setting`）：键、类型、默认值、能设在哪几层、各层怎么合并。核心每个请求合并一次（`internal/settings`），插件用 `Call.Setting(key)` 读合并后的值。

层级是 全局 → 分组 → 用户 → 密钥；一把密钥最多属于一个用户、一个分组。全局层在功能或扩展的表单里设（`PATCH /api/v1/features/{id}`），其他层在对应对象上设（`PUT /api/v1/{groups,users,keys}/{id}/settings`）。

| 合并方式 | 规则 |
|-|-|
| `override` | 离得最近、设了值的那层说了算 |
| `strictest` | 下层只能收紧。数字：取最小的正数，`0` 表示不限。布尔：`false` 优先。白名单（空表示不限）：每个非空层都放行的才算放行，所以几层没有交集就什么都不放行。管理员可以让某把密钥不受它约束。 |
| `union` | 各层的列表相加：下层能往黑名单里加，不能删。不受约束的密钥只用自己那份。 |

- `GET /api/v1/{groups,users,keys}/{id}/settings` 会列出各层的白名单，并指出是哪一层把所有东西都挡住了。外部插件从 `setting_layers` 拿到各层的值（[协议](../sdk/PROTOCOL.md)）。
- **存进去的值不合法**（手改了数据库，或插件改了声明；控制台、API 和 `apply` 录入时就会拒绝）：strictest 列表什么都不放行，union 列表保留其中合法的字符串，其他设置当作没设（`settings.LayerValue`）。加载配置时每条都会打一行警告。
- **类型**：`string int float bool duration list secret model upstream json`。`secret` 用主密钥加密，从不回显。金额设置（`unit: money`）按微单位存，显示时换成货币单位。标了 `restart` 的设置要重启才生效。
- **由记录本身决定的层**：密钥的额度（`key_monthly_micro`、`key_total_micro`）、`rpm` 和并发取自密钥记录本身的字段，分组的 `body_logging` 取自分组的 `log_bodies`。API 和 `apply` 都不存这几层（`settings.RecordOwned`），合并规则照常生效。

<a id="business-data-as-yaml"></a>
## 用 YAML 管理业务数据

`plugrelay export` 导出业务数据；`plugrelay apply` 在一个事务里写入（有则更新、无则新建），文档里没提到的行不动。用户、密钥、请求日志和用量从不包含在内。

```bash
plugrelay export -o relay-data.yaml                 # 不含凭据的密钥
plugrelay export -o full.yaml --with-secrets        # 和密钥本身一样敏感
plugrelay apply -f relay-data.yaml --dry-run --json # 只看会新建/更新什么，不写入
cat team.yaml | plugrelay apply -f -
```

文档结构（`internal/store/yamlio.go` 的 `store.Document`）：

```yaml
groups:
  - {id: vip, name: VIP, settings: {}, price_multiplier: 1.5}
upstreams:
  - id: deepseek
    provider: provider.openai-compatible   # 清单用的 provider.<interpreter>
    manifest: deepseek           # 官方或已存的清单 id
    config: {}                   # 清单变量、覆盖 issuer
    enabled: true                # 默认 true
credentials:
  - id: ds-team-1
    upstream: deepseek
    kind: shared                 # shared | personal | service
    owner: ""                    # 用户 id，personal 必填
    secret: sk-...               # 明文；不写就保留已存的密钥
    proxy: ""                    # 固定出口；"" 表示直连
    max_concurrency: 8
models:
  - name: deepseek-chat
    routes:                      # 不写就保留已存的路由；[] 表示删掉
      - {upstream: deepseek, upstream_model: deepseek-chat, priority: 10, weight: 1, groups: []}
presets:
  - {name: auto-coding, models: [claude-sonnet-4-5, deepseek-chat], strategy: in_order}  # in_order | cheapest | fastest
prices:                          # 每百万 token 的微单位价格；units 按单位计价
  - {model: deepseek-chat, input: 270000, cache_read: 70000, output: 1100000, currency: USD}
  - {model: gpt-image-1, units: {image: 40000, "image:1024x1024:high": 167000}, currency: USD}
  - {model: gpt-5.4-mini, input: 750000, output: 4500000, batch_discount: 0.5, currency: USD}
features:
  - {id: billing, enabled: true}           # internal/features 里的 id
plugins:                                   # 只放扩展
  - {id: ip-allowlist, enabled: true, order: 1010}  # examples/plugins/ip-allowlist-go
settings:
  - {scope: group, scope_id: vip, plugin: middleware.ratelimit, key: rpm, value: 600}
manifests:
  - {id: my-upstream, yaml: "id: my-upstream\nkind: provider\n..."}
```

导出的密文设置仍用主密钥加密，所以这份文档只能导入到主密钥相同的存储里。

<a id="prices"></a>
### 价格

价格用站点币种，以「微单位」（百万分之一个货币单位）计，token 价格按每百万 token 算。图片、音频等按次计费的价格写在 `units` 里：

| 键 | 计价单位 | 用量从哪来 |
|-|-|-|
| `image` | 每生成一张图 | 响应里的图片数；流式响应按请求的 `n` 计（默认 1） |
| `image:<size>`、`image:<size>:<quality>` | 某个尺寸（和质量）的一张图；越具体的键越优先 | 响应里的 `size` / `quality`，没有就看请求 |
| `audio_second` | 输入音频每秒 | 转写结果的 `usage.seconds` 或 `duration`；这些和 token 用量都没有时，读上传文件的 WAV/MP3/FLAC/Ogg/MP4/WebM 头算时长，但不少于按 1536 kbit/s 折算的时长；读不出文件头就按 64 kbit/s 折算 |
| `1k_chars` | 每 1000 个输入字符 | 语音合成的 `input` |
| `search_unit` | 每个重排搜索单位 | Cohere 的 `meta.billed_units.search_units`；其他重排上游按输入 token 计 |

上游报了 token 时，token 价格另外照算。响应读不出用量时是估出来的，请求日志里会标「估算」。清单只带 token 标价，按次计费的价格要靠价格规则。

`batch_discount`（0 到小于 1）是[批量任务](clients.zh-CN.md#batches)用量在对应价格规则上打的折扣，默认 0（原价），清单标价也一样。OpenAI 和 Anthropic 的批量任务都是半价，给它们的规则设 0.5。

- **成本**（你付给上游的钱）：依次找「上游 + 上游模型」的规则、上游的规则、和上游模型同名的模型规则，最后用上游清单里的 `pricing:`（币种和站点一致才用）。
- **收费**（调用方付的钱）：实际服务的模型的规则（预设就用成员的，没有再用预设的），没有就按这次尝试的成本价，再乘调用方分组的 `price_multiplier`（没分组为 1）。单价和总额都四舍五入到微单位。调用方只看到最终价格，管理员还能看到倍率。花费额度按收费算。

<a id="price-sync"></a>
#### 价格同步

开了 `price_sync` 后，`job.pricesync` 每隔 `interval` 通过核心的出站通道下载 [models.dev](https://models.dev) 的目录（`url`），先在路由所属厂商下按上游模型名找，找不到再去模型原厂下找（去掉厂商或地区前缀），美元价格用 `usd_rate` 换算（`plugins/job/pricesync`）。找到的价格作为建议显示在价格页。运营采纳之前什么都不改；拒绝过的价格不再提示。设成「跟随 models.dev」的模型会自动更新，但只限它还没有价格规则、或规则本来就来自同步：手填的价格永远优先。目录里的上下文长度和能力会补到模型目录页上缺的信息。
