# 插件

[English](plugins.md) | 简体中文

上游、鉴权、限流、预算、候选排序、日志，都是挂在固定扩展点上的插件，有三种写法，核心一视同仁。
运维开关的是功能，不是一个个内置插件（[配置](configuration.zh-CN.md#features)）；不属于任何功能的插件叫扩展。
插件在请求里什么时候跑见[架构](architecture.zh-CN.md#plugin-system-features-modules-extensions)，
术语见 [`CONTEXT.md`](../CONTEXT.md)。

- [选哪种写法](#choosing-a-tier)
- [声明式上游清单](#declarative-upstream-manifests)
- [外部进程插件](#external-process-plugins)
- [内置 Go 插件](#built-in-go-plugins)
- [内容安全 guardrails](#guardrails)
- [调用顺序与故障策略](#call-order-and-failure-policy)
- [一致性测试套件 plugintest](#conformance-suite-plugintest)

<a id="choosing-a-tier"></a>
## 选哪种写法

| 你想…… | 写 | |
|-|-|-|
| 接一个说 OpenAI chat/responses、Anthropic 或 Gemini 协议的上游，有自己的地址、鉴权、怪癖和错误码 | **YAML 清单** | 不写代码；用 JSON Schema 校验 |
| 接私有协议的上游、调外部服务、做 IP 白名单、计量、定时任务、管理路由，用什么语言都行 | **外部进程插件** | 单独一个进程，崩了只影响自己；有 Go SDK，也可以照协议自己实现 |
| 处理每个流式事件、新增协议编解码器，或者要开销最低 | **内置 Go 插件** | 编译进程序；只有它能用 `OnEvent`、`OnResponse` 和编解码器 |

扩展点（`pkg/plugin/points.go`）：

| 扩展点 | 做什么 | 清单 | 外部 | 内置 |
|-|-|-|-|-|
| `provider` | 一个上游：端点、自己负责的请求头、`Prepare`、`Classify` → 失败判定；可选：模型列表、凭据刷新、额度探测、自己发请求的 `Send` | 可以 | 可以 | 可以 |
| `auth` | 认出调用方 | | 可以 | 可以 |
| `middleware` | `OnRequest`、`OnAttempt`、`OnFinish`、`MutateBody`、`OnEvent`、`OnResponse` | | `OnRequest`、`OnAttempt`（只能改请求头）、`OnFinish`；不能改请求体 | 可以 |
| `selector` | 给候选重新排序，不能加；启用后取代内置的路由策略 | | 可以 | 可以 |
| `meter` | `Reserve` / `Settle` / `Refund`，都要幂等 | | 可以 | 可以 |
| `sink` | 异步接收已完成的请求记录 | | 可以 | 可以 |
| `job` | 定时运行 | | 可以 | 可以 |
| `admin` | 在 `/api/plugins/<id>/` 下挂 HTTP 路由 | | 可以 | 可以 |
| 任何类型（可选） | `UserDeleteHook`：删用户前先结束这个用户正在跑的东西；`SettingsCheck`：拒绝某次设置修改（409，来自控制台、`plugrelay apply` 或 new-api 导入），已存设置不合要求时也不让自己的插件打开 | | | 可以 |

协议编解码器不是插件：它们实现 `pkg/protocol.Codec`，编译进程序。

<a id="declarative-upstream-manifests"></a>
## 声明式上游清单

一份清单描述一个上游，由通用 provider 执行。官方清单在 [`manifests/`](../manifests)
（`plugrelay manifest list` 可列出），每份开头都注明对照过的厂商文档。Schema 见
[`manifests/schema.json`](../manifests/schema.json)，各字段含义见
`plugins/provider/manifest/manifest.go`。

```yaml
id: acme                      # 唯一；也是默认的签发方分组
kind: provider                # 只有这一种
name: ACME AI
name_zh: ACME 智能           # 可选，控制台里的中文名
category: vendor              # 展示分组：vendor | cloud | inference | gateway | self_hosted | custom
homepage: https://docs.acme.example
interpreter: openai-compatible   # 端点的默认解释器：openai-compatible、
                                 # anthropic-compatible、gemini、openai、anthropic
variables:                    # 每个实例各自的取值：下面的 {region}
  region: {default: us, description: Deployment region, description_zh: 部署区域}
auth:
  header: Authorization       # 凭据放在哪
  scheme: Bearer              # Bearer | raw；或者 query_param: key
  key_optional: false         # true：服务器可以不带密钥运行（见下文免密钥）
owned_headers: [X-Acme-Project]  # 客户端发来的同名头会被丢掉
credential_headers:           # 请求头 ← 凭据元数据里的值
  X-Acme-Org: org
capabilities:                 # 共享默认值，合并到各编解码器自己的能力之上
  cache: keyed                # explicit | keyed | implicit | none
  max_tokens_field: max_completion_tokens
  unsupported_params: [service_tier]
endpoints:
  - id: chat                  # 默认就是协议名
    protocol: openai-chat     # anthropic | openai-chat | openai-responses | openai-completions | gemini
    base_url: https://{region}.api.acme.example/v1   # OpenAI 风格包含 /v1
    capabilities:
      reasoning: reasoning_content   # 推理文本所在字段，工具轮次会回传
  - id: anthropic
    protocol: anthropic
    base_url: https://{region}.api.acme.example/anthropic  # Anthropic/Gemini：不带 /v1
    interpreter: anthropic-compatible
    unsupported_betas: [context-1m-2025-08-07]       # 从 anthropic-beta 里去掉
    issuer: acme              # issuer 相同的端点共享私有状态
  - id: completions           # 老式 /completions，给编辑器自动补全用
    protocol: openai-completions
    base_url: https://{region}.api.acme.example/v1
    capabilities:
      fill_in_middle: true    # 接受 suffix（不声明就是关闭）
errors:                       # 按顺序检查，先于内置规则
  - when: {status: 402}
    verdict: {cause: quota, credential: cool_until_reset, retry: next, signal: quota_exhausted}
  - when: {status: 400, message_contains: "maximum context length"}
    verdict: {cause: request, signal: context_too_long}
models:
  endpoint: chat
  list: /models               # 相对端点的 base URL；或者 static: [...]
pricing:                      # 按每百万 token 计；也可以是清单旁边的文件路径
  currency: USD
  models:
    acme-large: {input: 2.0, cache_read: 0.2, output: 8.0}
quota:                        # 可选的额度探测，由 job.quotaprobe 执行
  path: /dashboard/billing
  remaining_fraction: data.remaining   # 或者 remaining + limit（金额）
  reset: data.reset_at                 # unix 秒数或 RFC 3339
  reset_period: data.period            # reset 为空时使用：daily | weekly | monthly（UTC 00:00）
```

| 项目 | 规则 |
|-|-|
| 占位符 | `{name}` 取上游实例配置里的值，没有就用变量默认值；`path` 里的 `{model}` 是上游模型名（Azure 的部署，见 `manifests/azure-openai.yaml`） |
| 自建服务的地址 | `base_url` 含 `/v1`；`server_url` 是服务器根地址，用于在 `/v1` 之外还有路由的服务器（Anthropic、重排序）。控制台把 `variables:` 显示成表单字段 |
| 失败判定 | 原因 `request`、`transient`、`rate_limit`、`quota`、`auth_expired`、`banned`、`content`、`unknown`；凭据动作 `keep`、`cooldown`、`cool_until_reset`、`refresh`、`quarantine`、`manual`；重试 `none`、`next`、`downgrade`（配合 `downgrade:` 里的改动）；信号取 `ir.ErrorKind` 的值（`pkg/ir/error.go`）。违反硬规则的规则（比如重试被内容拒绝的请求、因请求本身出错去动凭据）过不了校验 |
| `freeform_tools` | OpenAI 自定义工具（比如 Codex 的 `apply_patch`）默认改成函数工具发出，除非清单声明 `freeform_tools: true`；只有 `openai` 和 `azure-openai` 声明了 |
| 内网地址 | 被 SSRF 防护挡住；自建端点要配 `egress.allow_private_cidrs`（[配置](configuration.zh-CN.md#egress)） |
| 签发方分组 | 默认是清单 id；`base_url` 来自变量的端点每个上游单独一组，所以两个自建实例绝不共享私有状态 |

```bash
plugrelay manifest validate acme.yaml        # 也可以是一个目录
plugrelay manifest list                      # 二进制里自带的官方清单
plugrelay upstream add --manifest deepseek --credential-env DEEPSEEK_API_KEY
plugrelay upstream add --manifest azure-openai --id azure-eu --credential-env AZURE_KEY --var resource=acme-eu
plugrelay upstream add --manifest vllm --var base_url=http://10.0.0.5:8000/v1   # 不带密钥
```

`upstream add` 只认官方清单和已经存进数据库的清单。自己写的清单用 `plugrelay apply` 存
（`manifests:` 条目里内联 YAML）；控制台和 `GET /api/v1/manifests` 能列出清单，但不能上传。

**免密钥上游。** 清单设了 `auth.key_optional: true`（各类自建服务器、LiteLLM 和 Bifrost 网关、
自定义的 `openai-compatible`），上游又一个凭据都没有时，路由器把它当成一个 id 为 `""` 的共享凭据，
不发鉴权头；不写 `--credential-env`、或在控制台不填密钥就是这种。其他没有凭据的上游不参与路由
（控制台显示「未路由：需要密钥」），绝不会不带鉴权发出去。解不开密的凭据以停用状态列着
（「无法打开」），也不会让上游变成免密钥。

<a id="cloud-platforms-bedrock-and-vertex-ai"></a>
### 云平台：Bedrock 和 Vertex AI

这两家的鉴权没法写成一个请求头，所以清单用的是 Go 写的解释器 `bedrock` 和 `vertex`
（`plugins/provider/<name>`，协议细节差异见包注释）。除此之外就是普通清单：

| 清单 | 变量 | 凭据内容 | 签发方 |
|-|-|-|-|
| `bedrock` | `region`（默认 `us-east-1`） | `ACCESS_KEY_ID:SECRET_ACCESS_KEY[:SESSION_TOKEN]`，或同样内容的 JSON，或一个 Bedrock API key | `bedrock` |
| `vertex-anthropic` | `project`（默认取密钥里的 `project_id`）、`location`（默认 `global`） | 服务账号密钥 JSON | `vertex-anthropic` |
| `vertex-gemini` | 同上 | 服务账号密钥 JSON | `vertex-gemini` |

```bash
plugrelay upstream add --manifest bedrock --credential-env BEDROCK_KEYS --var region=us-west-2
export VERTEX_KEY="$(cat service-account.json)"
plugrelay upstream add --manifest vertex-anthropic --credential-env VERTEX_KEY --var location=us-east5
```

两份清单都不列模型、不带价格：每个模型路由的上游模型名填平台上的 id（Bedrock 上是
`us.anthropic.claude-…` 推理配置文件或 ARN，Vertex 上是 `claude-…@YYYYMMDD`）。签发方各自独立，
因为 Anthropic API、Bedrock 和 Vertex 互不认对方的 thinking 签名（Gemini API 和 Vertex Gemini
也分开）。Bedrock 的 token 计数在本地算，不提供 Anthropic 服务端工具，请求由 provider 自己发
（`plugin.Sender`），走 `Outgoing.Client`。Vertex 按凭据缓存访问令牌，过期前五分钟换新，遇到 401
就扔掉令牌、由核心刷新凭据；换令牌的请求同样走凭据的出口。

<a id="external-process-plugins"></a>
## 外部进程插件

外部插件就是一个目录，里面有 `plugin.yaml` 和一个可执行文件。plugrelay 负责启动它，通过 stdio
（LSP 式分帧）或 unix socket 讲 JSON-RPC 2.0，崩了就带退避重启，出错或超时就按它的 `fail_policy`
处理。核心看到的是 `<kind>.<id>`。

| | |
|-|-|
| 协议 | 消息格式和完整的 `plugin.yaml` 说明见 [`sdk/PROTOCOL.md`](../sdk/PROTOCOL.zh-CN.md)；`sdk/plugin.schema.json` 可以在编辑器里校验 `plugin.yaml` |
| 打开 | 设 `external_plugins.enabled: true` 和 `external_plugins.dir`（[配置](configuration.zh-CN.md#external_plugins)）。插件装上后默认停用；由管理员批准它申请的宿主接口、打开它、排好顺序，和内置插件排在同一条链里（`PATCH /api/v1/extensions/{id}`、控制台的扩展列表，或 `plugrelay apply` 里的 `plugins:`） |
| 信任 | 它以 plugrelay 的系统用户运行，自己能上网、能读写文件。只有 `permissions.host`（KV、经核心出站层发 HTTP）真正生效，其他 `permissions` 只是展示给管理员看 |

<a id="sdk-quickstarts"></a>
### SDK 快速上手

Go SDK 是 `github.com/initxy/plugrelay/sdk/go/pluginsdk`（[README](../sdk/go/README.zh-CN.md)，
附完整示例）。其他语言直接照 [`sdk/PROTOCOL.md`](../sdk/PROTOCOL.zh-CN.md) 实现。
[`examples/plugins`](../examples/plugins) 里有能直接跑的例子：
[`echo-provider-go`](../examples/plugins/echo-provider-go) 把最后一条用户消息流式返回；
[`ip-allowlist-go`](../examples/plugins/ip-allowlist-go) 是一个 `middleware.onRequest` 的 CIDR 白名单。

| 容易踩的坑 | |
|-|-|
| stdout | stdio 模式下 stdout 上只要出现协议帧以外的东西，连接就断；日志写 stderr |
| 报错 | `deny(...)` / `reject(...)` 原样交给客户端；其他错误都算插件故障，按 `fail_policy` 处理 |
| 做不了的事 | 没有 `middleware.onEvent`；middleware 不能改请求体 |
| 环境变量 | 只给一小份（`PATH`、`HOME`、语言区域、`TMPDIR`、`TZ`、清单里的 `env`、`PLUGRELAY_PLUGIN_ID` 等），不是中转自己的环境 |
| `plugin.yaml` | `reports_off_box` 和 `requires` 的作用同[下文](#built-in-go-plugins)的 `ReportsOffBox`、`Requires` |
| `plugrelay apply` | 不加载外部插件，所以检查不了它们的设置 |

<a id="built-in-go-plugins"></a>
## 内置 Go 插件

内置插件放在 `plugins/<kind>/<name>`，只能 import `pkg/...` 和 `plugins/internal/...`
（`plugins/boundary_test.go`），核心服务从传给 `Init` 的 `plugin.Host` 拿。

```go
package quiet

import "github.com/initxy/plugrelay/pkg/plugin"

const ID = "middleware.quiet"

func init() { plugin.Register(ID, func() plugin.Plugin { return &Quiet{} }) }

type Quiet struct{}

func (*Quiet) Info() plugin.Info {
	return plugin.Info{
		ID:   ID,
		Kind: plugin.KindMiddleware,
		Settings: []plugin.Setting{
			{Key: "rpm", Type: plugin.TypeInt, Default: 0,
				Scopes: plugin.Global | plugin.Group | plugin.Key, Merge: plugin.Strictest,
				Label: plugin.T("Requests per minute", "每分钟请求数"),
				Help:  plugin.T("0 means no limit.", "0 表示不限。")},
		},
		// Mutates: []string{"max_tokens"}, // 本插件可以改的 gjson 路径
		FailPolicy: plugin.FailClosed,
	}
}

func (*Quiet) OnRequest(c *plugin.Call) (*plugin.Reply, error) {
	_ = c.Setting("rpm").Int() // 已经按 Global → Group → User → Key 合并好
	return nil, nil            // 返回 nil 表示继续往下走
}
```

**注册**只是让插件可用，不会让它运行。把 import 加到 `plugins/builtin/builtin.go`，再在
`internal/features` 里给它安个位置：核心模块（始终运行）或某个功能下的模块
（漏了 `TestEveryBuiltinPlaced` 会失败）。第三方 Go 插件要自己编一个 import 了它的版本，算扩展。

| `plugin.Info` 字段 | 含义 |
|-|-|
| `ID` | `<kind>.<name>` |
| `Settings` | 核心要合并哪些设置（[分层设置](configuration.zh-CN.md#layered-plugin-settings)）、表单显示什么：`Label`/`Help` 要有中英文，枚举值用 `EnumLabels`，微单位金额用 `Unit: plugin.UnitMoney`（管理 API 按货币单位收发）。`TestSettingsLabelled` 检查每个内置设置。功能自己决定表单显示哪些设置 |
| `Mutates` | 可以改的请求字段；一个都不声明就是只读，直通才能逐字节一致 |
| `FailPolicy` | `closed` 或 `open`；各类插件的默认值见[架构](architecture.zh-CN.md#plugin-system-features-modules-extensions) |
| `ReportsOffBox` | 会把数据发到本机之外；`PLUGRELAY_TEST_MODE` 下不启动 |
| `Requires` | 离不开的插件，随它一起打开（`meter.quota` 的预算从 `sink.requestlog` 汇总）。缺了某个依赖时，出错就拒绝请求的插件（`FailPolicy: closed`）会让中转起不来或这次重新加载不生效；其他插件被关掉，日志里记下冲突 |

| 接口 | |
|-|-|
| middleware | `plugin.RequestHook`、`AttemptHook`、`EventHook`（+ `EventHolder`）、`ResponseHook`（完整响应发出前调用，可以改也可以拒）、`FinishHook`、`BodyMutator` |
| 其他扩展点 | `Authenticator`、`Selector`、`Meter`（+ `QuotaHeaders`，批量任务用 `DeferredMeter`）、`Sink`、`Job`、`Admin` |
| provider | `Provider`（+ `ModelLister`、`Refresher`、`QuotaProber`、`Sender`） |
| 初始化和收尾 | `Initializer`、`Closer`；实现了 `plugin.NoticeHost` 的 Host 接受 `Notify(plugin.Notice)`，供[通知](notifications.zh-CN.md)功能用，从不阻塞 |

**Host 和出站。** Host 提供带作用域的日志器、按插件隔离的 `KV`、全局设置、用量查询、版本号和出站客户端；
`plugin.StateHost.State()` 另外提供所有节点共享的计数器、名额和锁，每个插件的键互不相通。插件从不自己开连接
（`plugins/boundary_test.go`），只用下面三种客户端，都带 SSRF 防护、不跟随重定向：

| 客户端 | 用于 | 怎么出去 |
|-|-|-|
| `Outgoing.Client` | provider 在一次尝试中自己发的请求（`Prepare` 里换令牌、`Sender` 发的请求） | 凭据固定的代理，受这次尝试的计时器约束 |
| `Host.ClientFor(upstream, credential)` | 请求之外为上游发的请求（`ListModels`、`Quota`、`Refresh`） | 凭据的代理，没有就用上游的（取自中转配置，不看传进来的对象）；超时用上游的设置 |
| `Host.HTTPClient` | 不属于任何上游的流量；provider 不用它 | 直连 |

配置了的代理挂了，调用就失败，绝不退回直连（硬规则 15）。外部插件用带 `upstream_id` / `credential_id`
的 `host.http` 得到同样效果。

| 事项 | 要知道的 |
|-|-|
| 不能丢记录的 sink | `FailPolicy: closed` 的 sink 相当于流水账，有自己单独的队列，慢了只耽误自己。队列满时核心先等，再由正在结束的请求自己写；写失败带退避重试，所以同一个 `Record.ID` 可能来不止一次，存过的要跳过。关机期限到了还没送到的记录记日志、算丢失。其他 sink 过载时可能丢记录 |
| 凭据刷新 | 遇到 `refresh` 判定，核心给这个凭据加一把集群级的锁，调 `Refresher`，存下新密钥和过期时间，把凭据放回轮换，再用它重发这次尝试一次。没有 `Refresher` 或刷新失败，凭据歇一分钟。存储几次都不收新密钥时，请求照样用新密钥完成，控制台把凭据标成需要关注（仍在轮换），`plugrelay_credential_secret_save_failures_total` 计数，每分钟重试保存；存进去之前这个节点一重启就丢了（`internal/pipeline/refresh.go`） |
| 额度探测 | `job.quotaprobe`（始终运行）对每个已启用上游的每个凭据调 `QuotaProber.Quota`，间隔取上游的 `quota_probe`，没有就取任务的 `interval`（10m），同一时刻只在一个节点跑，走 `Host.ClientFor`。结果填进 `Candidate.QuotaReset` 供 `quota_reset` 策略用，也显示成凭据上的额度条；`remaining` 为 0 时凭据排除到 `reset_at`。探测不了就返回 `plugin.ErrNoQuota` |
| 改请求 | 实现 `MutateBody`（patch 档，用 sjson 只改声明过的路径），同时在 `OnAttempt` 里改 `Call.Request()` 拿到的中间格式（convert 档）；参考 `plugins/middleware/parampolicy` |
| 备注 | `Call.Note(text)` 往给出回应的那次尝试（没有就是最后一次）的诊断里加一行；在 `OnFinish` 里加就晚了 |
| 指标 | 实现了 `prometheus.Collector` 的内置插件，在 `sink.metrics` 运行时由 `/metrics` 提供 |
| `Init` 失败 | 插件被跳过，显示为 `crashed`，依赖它的插件同样跳过，`FailPolicy: closed` 的也不例外（被跳过的 `meter.quota` 不管任何预算）。部署后要看一眼插件状态 |
| 鉴权和 `/native` | `/native` 只去掉核心知道会带平台密钥的头（`Authorization`、`x-api-key`、`x-goog-api-key`、`?key=`）。鉴权插件如果从别处读密钥，就不能和 `/native` 授权一起用，否则密钥会被送到上游 |

<a id="guardrails"></a>
## 内容安全 guardrails

`middleware.guardrails`（`plugins/middleware/guardrails`，见包注释）和 `middleware.redact`
一起，在内容安全功能打开时运行（默认关闭）。它检查请求和响应，所有内容规则共用一种动作。

| 设置 | 合并方式 | 含义 |
|-|-|-|
| `detect` | 并集 | `email`、`phone`、`credit_card`（Luhn 校验）、`iban`（mod 97 校验）、`cn_id`（18 位居民身份证号，查校验位和出生日期）、`us_ssn`（只认已发放的号段）、`secret`（带前缀的 API key、AWS id、GitHub/Slack/Google/GitLab 令牌、bearer 令牌、JWT）、`private_key`（PEM） |
| `deny_keywords`、`deny_patterns` | 并集 | 前者是子串，忽略大小写、零宽字符和全角 ASCII（其他形近字不处理）；后者是 RE2 正则，按原样匹配（正则写错会让请求被拒） |
| `action` | 覆盖 | `block`（默认）、`redact` 或 `flag` |
| `check_response` | 覆盖 | 也检查响应（默认开） |
| `max_prompt_chars` | 取最严 | system、消息、工具调用和结果的总字符数；`0` 不限 |
| `blocked_models`、`blocked_tools` | 并集 | 名字或通配符（`pkg/match`）；工具只要被提供、或历史里调用过就拒绝 |
| `stream_window` | 覆盖，仅全局 | 为了跨分块匹配保留的流式文本字节数（2048，最多 16384） |
| `block_unscannable_audio` | 覆盖 | 在 `block` 且开了任何规则或审核时，拒收 WebSocket 音频输入，因为规则读不了音频（默认关） |
| `moderation_upstream`、`moderation_model`、`moderation_fail` | 覆盖，全局/分组 | 调某个上游的 OpenAI 兼容 `/moderations`（拼在它第一个端点的 base URL 后面，用它的一个凭据，经核心出站层）；模型默认 `omni-moderation-latest`；调用失败时 `closed`（默认）拒绝，`open` 放行并记一条备注 |
| `max_moderated_events` | 取最严，全局/分组 | 一个 Realtime 会话里最多送审多少个客户端事件的用户文本（200）；超过后审核按失败处理 |

| 项目 | 行为 |
|-|-|
| 读哪些内容 | 请求：客户端给的所有文本——system 提示词、每轮的文本和 thinking、工具调用参数和工具结果（不读图片和文件）。响应：模型的文本（不读工具输入和 thinking）。审核只看用户轮次。检测器偏保守，校验和或结构对得上才算命中（`plugins/internal/textscan`，和 `middleware.redact` 共用） |
| 管不到的 | `/native` 不跑请求钩子，需要 guardrails 把关的密钥别开它。批量任务逐个请求检查（[客户端](clients.zh-CN.md#batches)）；WebSocket 会话逐个事件检查，只看文本（[客户端](clients.zh-CN.md#realtime-and-websocket)） |
| `block` | 返回本协议的内容过滤错误（OpenAI 上是带 `content_filter` 的 400，Anthropic 上是 `invalid_request_error`）；被禁的模型和工具返回 403，提示词超长返回 400。被拒的完整响应不收费，但上游成本照记 |
| `redact` | 请求里用户轮次的文本（patch 和 convert 档）和完整响应（重新编码，直通时也是）里的命中换成 `[REDACTED:<rule>]`。请求其他位置和流里的命中按 `flag` 处理。审核命中没有具体位置，直接拦截 |
| 流 | 在 `stream_window` 字节的滑动窗口里匹配。`block` 时，还可能属于某个匹配的文本先压着，一确定命中就以本协议的错误事件结束流，匹配的任何部分都到不了客户端。只有关键词时，只压可能是关键词开头的那段尾巴；有任何正则或检测器时，输出最多延迟 `stream_window` 字节。能保证的范围：不超过窗口长度、且在同一个内容块里的匹配（内容块结束会放行压着的内容）。`flag` 时什么都不压 |
| 判定记录 | 在尝试的诊断里加备注 `guardrails: <action> <phase> <rules>`（phase 为 `request`、`response`、`stream`），并计入 `plugrelay_guardrail_verdicts_total{phase,action,rule}`。还没开始尝试就被拒的请求，日志条目的错误里写明是哪些规则 |

<a id="call-order-and-failure-policy"></a>
## 调用顺序与故障策略

钩子顺序、`Reply` 直接回复、`FailPolicy` 默认值，三种写法都一样，见
[架构](architecture.zh-CN.md#plugin-system-features-modules-extensions)。另外：每次重试都会再跑
`OnAttempt` 和 provider 的 `Prepare` / `Send` / `Classify`。`OnEvent` 每个事件限 50 毫秒，
返回放行、丢弃或中止（返回 `*ir.GwError` 会用这个错误结束流）；`EventHolder` 还能说明最近多少个事件
先别发给客户端，放行后再发，流被中止就丢掉。只有插件出故障（崩溃、超时、协议错误——`plugin.IsFailure`）
才按 `FailPolicy` 处理；有意返回的 `*ir.GwError` 原样交给客户端。`Settle` 和 `Refund` 可能被重复调用，
必须只生效一次。

<a id="conformance-suite-plugintest"></a>
## 一致性测试套件 plugintest

`pkg/plugin/plugintest` 让任意 `plugin.Provider` 对着一个按脚本回应的假上游跑一遍，检查核心依赖的判定和请求头处理：
正常、流式、请求头归属（预先埋的客户端密钥绝不能到上游）、流被切断、带 retry-after 的 429、401、
账号被封（隔离，不重试）、内容违规（哪里都不重试）、拿不准（从严）、发送前超时、首字节慢、超大 SSE 事件、
错误透传、5xx。

```go
func TestConformance(t *testing.T) {
	plugintest.RunProvider(t, plugintest.ProviderSuite{
		Protocol: "openai-chat",
		New: func(baseURL string) (plugin.Provider, *plugin.Upstream, *plugin.Credential) {
			u := &plugin.Upstream{ID: "up", Endpoints: []plugin.Endpoint{{ID: "e", Protocol: "openai-chat", BaseURL: baseURL}}}
			return myprovider.New(), u, &plugin.Credential{ID: "c", Secret: "sk-test"}
		},
	})
}
```

| | |
|-|-|
| 自己负责的请求头 | `Prepare` 设置的头必须列进 `OwnedHeaders`，除非 `Prepare` 只在客户端没发时才填（比如协议版本的默认值）；套件会带上客户端的值再跑一次 `Prepare` 来区分 |
| 选项 | `UseSend: true` 测 provider 自己的 `Send`；`Skip` 和 `Expect` 用来调整确实不一样的场景 |
| 仓库里怎么用 | `manifests/` 每个文件的每个端点都跑（`plugins/provider/compatible/conformance_test.go`）；Bedrock 用事件流测试数据经 `Send` 跑，Vertex 对着假的令牌端点跑（`plugins/provider/internal/cloudtest`）；echo provider 经宿主跑（`internal/extplugin/examples_test.go`）。非 Go 插件没有能在仓库外用的运行器 |
