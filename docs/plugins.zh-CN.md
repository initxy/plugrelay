# 插件

[English](plugins.md) | 简体中文

凡是策略而不是机制的东西——上游、鉴权、限流、预算、候选排序、日志——都是插件。
插件挂在固定的扩展点上，有三种写法；在核心看来三种完全一样。运维不会一个个开关内置插件，
而是开关功能（feature，见[配置](configuration.zh-CN.md#features)）；不属于任何功能的插件
叫扩展（extension）。术语见 [`CONTEXT.md`](../CONTEXT.md)。插件在请求中所处的位置见
[架构](architecture.zh-CN.md#plugin-system-features-modules-extensions)。

- [选哪种写法](#choosing-a-tier)
- [声明式上游清单](#declarative-upstream-manifests)
- [外部进程插件](#external-process-plugins)
- [内置 Go 插件](#built-in-go-plugins)
- [内容安全 guardrails](#guardrails)
- [调用顺序与故障策略](#call-order-and-failure-policy)
- [一致性测试套件 plugintest](#conformance-suite-plugintest)

<a id="choosing-a-tier"></a>
## 选哪种写法

| 你想…… | 写 | 为什么 |
|-|-|-|
| 接入一个说已支持协议（OpenAI chat 或 responses、Anthropic、Gemini）的上游，有自己的 URL、鉴权头、怪癖和错误码 | **YAML 清单** | 不用写代码；用 JSON Schema 校验 |
| 接入私有协议的上游、调用外部服务的内容审查、IP 白名单、自定义计量、定时任务、管理路由——用任何语言 | **外部进程插件** | 在独立进程里跑，崩了只影响自己；有 Go SDK，其他语言照协议实现 |
| 要处理每一个流式事件、新增一个协议编解码器，或者要开销最低 | **内置 Go 插件** | 编译进程序；只有这种写法能用 `OnEvent` 和编解码器 |

扩展点（`pkg/plugin/points.go`）以及哪种写法能实现：

| 扩展点 | 做什么 | 清单 | 外部 | 内置 |
|-|-|-|-|-|
| `provider` | 一个上游：端点、自己负责的请求头、`Prepare`、`Classify` → 失败判定；可选：模型列表、凭据刷新、额度探测、自己的 `Send` | 可以 | 可以 | 可以 |
| `auth` | 识别调用方 | | 可以 | 可以 |
| `middleware` | `OnRequest`、`OnAttempt`、`OnFinish`、`MutateBody`；`OnEvent` 和 `OnResponse` 仅限内置 | | 可以（没有 `OnEvent`、`OnResponse`） | 可以 |
| `selector` | 给候选重新排序（不能加）；启用后由它排序，取代内置的路由策略 | | 可以 | 可以 |
| `meter` | `Reserve` / `Settle` / `Refund`，都必须幂等 | | 可以 | 可以 |
| `sink` | 异步消费已完成的请求记录 | | 可以 | 可以 |
| `job` | 按计划定时运行 | | 可以 | 可以 |
| `admin` | 在 `/api/plugins/<id>/` 下挂 HTTP 路由 | | 可以 | 可以 |
| （可选，任何类型） | `UserDeleteHook`：删除用户前先结束这个用户正在跑的东西；`SettingsCheck`：拒绝某次插件设置修改（409；控制台保存、`plugrelay apply`、new-api 导入），以及在已存设置不合它要求时拒绝把自己打开；`plugrelay apply` 不加载外部插件，所以外部插件的检查在那里不会运行 | | | 可以 |

协议编解码器不是这套 API 下的插件；它们实现 `pkg/protocol.Codec`，并且编译进程序。

<a id="declarative-upstream-manifests"></a>
## 声明式上游清单

一份清单描述一个上游，由通用 provider 来解释。官方清单放在
[`manifests/`](../manifests)（厂商列表见 [README](../README.zh-CN.md)；
`plugrelay manifest list` 也能列出来），每份文件开头的注释写着它对照过的厂商文档。
自建服务的清单给地址变量起名有两种方式：`server_url` 是不含 `/v1` 的服务器根地址，
用于服务器在 `/v1` 之外还有路由的情况（Anthropic、重排序）；`base_url` 包含 `/v1`。
每份清单的 `variables:` 写明用的是哪个并附说明，控制台会把它们显示成表单字段。
Schema 是 [`manifests/schema.json`](../manifests/schema.json)；Go 类型和每个字段的含义见
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

说明：

- `freeform_tools`（OpenAI 的自定义工具，比如 Codex 的 `apply_patch`）对所有清单默认关闭，
  除非清单声明 `freeform_tools: true`，不管编解码器默认是什么：OpenAI 兼容服务器会拒收这类工具，
  中转改用函数工具发过去。只有 `openai` 和 `azure-openai` 声明了它。
- `{name}` 占位符用上游实例配置里的值填，没有就用变量的默认值；`path` 里的 `{model}`
  是上游模型名（Azure 的部署）。见 `manifests/azure-openai.yaml`。
- 失败判定的原因：`request`、`transient`、`rate_limit`、`quota`、`auth_expired`、`banned`、
  `content`、`unknown`。凭据动作：`keep`、`cooldown`、`cool_until_reset`、`refresh`、
  `quarantine`、`manual`。重试：`none`、`next`、`downgrade`（配合 `downgrade:` 里的改动）。
  信号取 `ir.ErrorKind` 的值（`pkg/ir/error.go`）。会违反硬规则的规则过不了校验——
  比如重试被内容拒绝的请求，或者因为请求本身的错误去改凭据状态。
- 上游 URL 受 SSRF 防护约束；自建端点需要配置 `egress.allow_private_cidrs`
  （[配置](configuration.zh-CN.md#egress)）。

校验和使用：

```bash
plugrelay manifest validate acme.yaml        # 也可以是一个目录
plugrelay manifest list                      # 二进制里自带的官方清单
plugrelay upstream add --manifest deepseek --credential-env DEEPSEEK_API_KEY
plugrelay upstream add --manifest azure-openai --id azure-eu --credential-env AZURE_KEY --var resource=acme-eu
plugrelay upstream add --manifest vllm --var base_url=http://10.0.0.5:8000/v1   # 不带密钥
```

清单设置了 `auth.key_optional: true`（各类自建服务器、LiteLLM 和 Bifrost 网关、自定义的
`openai-compatible`）并且一个凭据都没有的上游是免密钥上游：路由器把它当作一个 id 为 `""`
的共享凭据来路由，不发鉴权头。对这类服务器，不写 `--credential-env`，或者在控制台的添加表单里
不填密钥，就得到一个免密钥上游。其他没有凭据的上游没有路由：删掉云上游的最后一个密钥，它就停了，
绝不会不带鉴权就发出去，控制台会标上「未路由：需要密钥」。存储里解不开密的凭据会以停用状态
（「无法打开」）继续列着，绝不会让它的上游变成免密钥。

`upstream add` 接受官方清单和已经存进数据库的清单。自己写的清单文件通过 `plugrelay apply`
存进去（在 `manifests:` 条目里内联 YAML）；控制台和管理 API 能列出清单
（`GET /api/v1/manifests`），但不能上传。

签发方分组默认就是清单 id；`base_url` 来自变量的端点，每个上游各有自己的签发方分组，
所以两个自建实例绝不会共享私有状态。

<a id="cloud-platforms-bedrock-and-vertex-ai"></a>
### 云平台：Bedrock 和 Vertex AI

它们的鉴权没法写成一个请求头，所以它们的清单指定的是背后由 Go 代码实现的解释器：
`bedrock`（`plugins/provider/bedrock`）和 `vertex`（`plugins/provider/vertex`）。
包注释列出了各自要处理的协议细节差异。对运维来说它们就是普通的清单：

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

- 每个模型的路由，上游模型名填平台上的 id（Bedrock 上是 `us.anthropic.claude-…` 推理配置文件
  或 ARN，Vertex 上是 `claude-…@YYYYMMDD`）。这两份清单都不列模型也不带价格。
- Anthropic API、Bedrock 和 Vertex 互不接受对方的 thinking 签名，Gemini API 和 Vertex Gemini
  也是分开的，所以各自有独立的签发方分组。
- Bedrock：token 计数在本地回答；不提供 Anthropic 服务端工具。调用由 provider 自己发起
  （`plugin.Sender`），走这次尝试的出口（`Outgoing.Client`），所以凭据或上游配置的代理会承载这些调用。
- Vertex：访问令牌按凭据缓存，过期前五分钟换新；遇到 401 就扔掉令牌，由核心刷新凭据。
  换令牌的请求同样走凭据的出口。

<a id="external-process-plugins"></a>
## 外部进程插件

外部插件就是一个目录，里面有一个 `plugin.yaml` 和一个可执行文件。plugrelay 负责启动它，
通过 stdio（LSP 式分帧）或 unix socket 讲 JSON-RPC 2.0，它崩了就带退避重启，
它出错或超时就执行它的 `fail_policy`。核心看到的是 `<kind>.<id>`。

- 契约——分帧、生命周期、每个方法、回调宿主的接口、错误码、完整的 `plugin.yaml` 参考——见
  [`sdk/PROTOCOL.md`](../sdk/PROTOCOL.zh-CN.md)。`sdk/plugin.schema.json` 可以在编辑器里校验
  `plugin.yaml`。
- 打开这种写法要设 `external_plugins.enabled: true`，并把 `external_plugins.dir`
  指向一个放插件目录的目录（[配置](configuration.zh-CN.md#external_plugins)）。
- 安装插件就等于信任它：它以 plugrelay 的系统用户运行，有自己的网络和文件访问权限。
  `plugin.yaml` 里的 `permissions` 会展示给管理员，但不强制执行；唯一的例外是
  `permissions.host`，它管着宿主接口（KV、经核心出站层发 HTTP）。

外部插件属于扩展：一开始是停用的；由管理员批准它声明的宿主接口、打开它并设定顺序
（`PATCH /api/v1/extensions/{id}`、控制台的扩展列表，或 `plugrelay apply` 里的 `plugins:`），
和内置插件排在同一条链里（`internal/app/plugins.go`）。

<a id="sdk-quickstarts"></a>
### SDK 快速上手

Go SDK 是 `github.com/initxy/plugrelay/sdk/go/pluginsdk`
（[README](../sdk/go/README.zh-CN.md)，附完整示例）。其他语言直接实现
[`sdk/PROTOCOL.md`](../sdk/PROTOCOL.zh-CN.md)。[`examples/plugins`](../examples/plugins)
里是能直接跑的参考：[`echo-provider-go`](../examples/plugins/echo-provider-go)，一个把最后一条
用户消息原样流式返回的 provider；[`ip-allowlist-go`](../examples/plugins/ip-allowlist-go)，
一个 `middleware.onRequest` 的 CIDR 白名单。

容易踩的坑：

- stdio 模式下，stdout 上只要出现协议帧以外的东西，连接就断；日志写到 stderr。
- 有意的拒绝用 `deny(...)` / `reject(...)`，会原样交给客户端；其他错误都算插件故障，
  按 `fail_policy` 处理。
- 跨进程没有 `middleware.onEvent`。
- 进程拿到的是一份精简的环境变量（`PATH`、`HOME`、语言区域、`TMPDIR`、`TZ`、清单里的
  `env`、`PLUGRELAY_PLUGIN_ID` 等），不是中转自己的环境。

<a id="built-in-go-plugins"></a>
## 内置 Go 插件

内置插件放在 `plugins/<kind>/<name>`，由 `plugins/builtin` 链接进来。它们只能 import
`pkg/...` 和 `plugins/internal/...`（`plugins/boundary_test.go`），核心服务通过传给 `Init` 的
`plugin.Host` 拿到。

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

- **注册**：在 `init` 里注册；注册只是让插件可用，并不会让它运行。把 import 加到
  `plugins/builtin/builtin.go`，并在 `internal/features` 里给它安个位置：要么是核心模块
  （始终运行），要么是某个功能下的模块（功能打开时运行）；漏了 `TestEveryBuiltinPlaced` 会失败。
  第三方 Go 插件需要一个 import 了它的自定义构建；它属于扩展，单独开关。
- **Info**：`ID` 是 `<kind>.<name>`。`Settings` 声明核心要合并哪些设置
  （[分层设置](configuration.zh-CN.md#layered-plugin-settings)）以及表单显示什么：
  `Label` 和 `Help` 要有中英文，枚举值用 `EnumLabels`，微单位金额用 `Unit: plugin.UnitMoney`
  （管理 API 以货币单位显示和接收）；`TestSettingsLabelled` 要求每个内置设置都写齐这些。
  一个功能会列出它的表单显示哪些模块的哪些设置。`Mutates` 列出插件可以改的请求字段；
  一个都没声明的插件是只读的，这样直通才能保持逐字节一致。`ReportsOffBox` 标记会把数据
  发到本机之外的插件；`PLUGRELAY_TEST_MODE` 下永远不启动它。`Requires` 列出它离不开的插件
  （`meter.quota` 依赖 `sink.requestlog`，它的预算是从后者汇总出来的）：核心会随它一起打开这些插件。
  如果某个依赖没装：故障时拒绝请求（fail closed）的依赖方（比如 `meter.quota`）会让中转起不来，
  或让这次重新加载不生效；其他依赖方会被关掉，并在日志里记下冲突。`FailPolicy: closed`
  的 sink 相当于一本流水账，有自己单独的队列，慢了也不耽误别人：队列满时核心绝不丢它的记录
  （先等，然后由正在结束的那个请求自己把记录写进去），写失败会带退避重试，所以同一个
  `Record.ID` 可能到达不止一次，sink 必须跳过已经存过的 id。关机期限到了仍未送达的记录
  会记日志并计为丢失。其他 sink 在过载时可能丢记录。外部插件在 `plugin.yaml` 里用
  `reports_off_box` 和 `requires` 声明同样的东西。
- **鉴权插件与 `/native`**：`/native` 会把客户端的查询参数和请求头转发出去，只去掉核心知道
  会携带平台密钥的那几个（`Authorization`、`x-api-key`、`x-goog-api-key`、`?key=`）。
  如果外部鉴权插件从别的地方读密钥，就不能和 `/native` 授权一起用，否则那个密钥会被送到上游。
- **Init 失败**：`Init` 失败的插件会被跳过，在控制台里显示为 `crashed`，依赖它的每个插件也一样；
  故障时拒绝请求的插件也不例外（被跳过的 `meter.quota` 不执行任何预算），所以部署后要留意插件状态。
- **共享状态**：`plugin.StateHost.State()` 提供所有节点共享的计数器、名额和锁；
  每个插件的键都有自己的命名空间，一个插件碰不到另一个插件的键。
- **接口**：按需实现 `plugin.RequestHook`、`AttemptHook`、`EventHook`（+ `EventHolder`）、
  `ResponseHook`（完整响应发出之前调用；可以改，也可以拒绝）、`FinishHook`、`BodyMutator`
  （middleware 用）；`Authenticator`、`Selector`、`Meter`（+ `QuotaHeaders`）、`Sink`、
  `Job`、`Admin`、`Provider`（+ `ModelLister`、`Refresher`、`QuotaProber`、`Sender`）。
  `Initializer` 和 `Closer` 用于初始化和收尾。实现了 `plugin.NoticeHost` 的 Host 接受
  `Notify(plugin.Notice)`，供通知功能使用（[通知](notifications.zh-CN.md)）；它从不阻塞。
- **Host**：带作用域的日志器、出站客户端、按插件隔离的 `KV`、全局设置、用量读取、版本号。
  插件从不自己开连接（`plugins/boundary_test.go`）；它从下面三种客户端里取一个，
  全都带 SSRF 防护且不跟随重定向：
  - `Outgoing.Client`——provider 在一次尝试中自己发起的调用（`Prepare` 里换令牌、
    `Sender` 发的请求）：走凭据的固定代理，受这次尝试的计时器约束。
  - `Host.ClientFor(upstream, credential)`——请求之外为某个上游发起的调用（`ListModels`、
    `Quota`、`Refresh`）：走凭据的代理，没有就走上游的代理，超时用上游的设置。代理取自中转的配置，
    而不是传进来的那个凭据对象。
  - `Host.HTTPClient`——直连，用于不属于任何上游的流量；provider 从不使用它。

  配置了的代理挂掉时调用就失败，绝不退回直连（硬规则 15）。外部插件通过带
  `upstream_id` / `credential_id` 的 `host.http` 得到同样的效果
  （[`sdk/PROTOCOL.md`](../sdk/PROTOCOL.zh-CN.md)）。
- **凭据刷新**：遇到 `refresh` 判定时，核心先给这个凭据拿一把集群级别的锁，调用 provider 的
  `Refresher`，存下新的密钥和过期时间，把凭据放回轮换，再用它把这次尝试重发一次。
  没有 `Refresher`（或刷新失败）时，凭据歇一分钟。如果重试几次后存储仍不接受新密钥，
  请求照样用新密钥完成，控制台会把这个凭据标为需要关注（它仍在轮换里），
  `plugrelay_credential_secret_save_failures_total` 计一次数，并且每分钟重试保存；
  保存成功之前，这个节点一重启就会丢掉这个密钥（`internal/pipeline/refresh.go`）。
- **额度探测**：`job.quotaprobe`（始终运行）对每个已启用上游的每个凭据调用
  `QuotaProber.Quota`，间隔取上游的 `quota_probe`，没有就取这个任务的 `interval`（10m），
  同一时刻只在一个节点上跑，走 `Host.ClientFor`。读数是路由器的共享状态：它填进
  `Candidate.QuotaReset` 供 `quota_reset` 路由策略使用，在凭据上显示成额度条，
  `remaining` 为 0 时凭据会被排除到 `reset_at` 为止。探测不了的上游返回 `plugin.ErrNoQuota`。
- **请求日志备注**：`Call.Note(text)` 往给出回应的那次尝试（没有就是最后一次）的诊断信息里加一行；
  在 `OnFinish` 里加就太晚了。实现了 `prometheus.Collector` 的内置插件，在 `sink.metrics`
  运行期间会把它的指标在 `/metrics` 上提供出来。
- **两条路径都要改**：会改请求的插件要实现 `MutateBody`（patch 档，只用 sjson 改声明过的路径），
  还要在 `OnAttempt` 里改 `Call.Request()` 拿到的 IR（convert 档）。可以参考
  `plugins/middleware/parampolicy`。

<a id="guardrails"></a>
## 内容安全 guardrails

`middleware.guardrails`（`plugins/middleware/guardrails`，见包注释）和 `middleware.redact`
一起，在内容安全功能打开时运行（默认关闭）。它检查请求和响应，对内容规则统一套用一种动作。

| 设置 | 合并方式 | 含义 |
|-|-|-|
| `detect` | 并集 | 对请求文本和响应跑的检测器：`email`、`phone`、`credit_card`（Luhn 校验）、`iban`（mod 97 校验）、`cn_id`（18 位居民身份证号，校验位和出生日期）、`us_ssn`（只认已发放的号段）、`secret`（带前缀的 API key、AWS id、GitHub/Slack/Google/GitLab 令牌、bearer 令牌、JWT）、`private_key`（PEM） |
| `deny_keywords`、`deny_patterns` | 并集 | 前者是子串，匹配时忽略大小写、零宽字符和全角 ASCII 形式（其他形近字符不做归一）；后者是 RE2 正则，按原样匹配（写错的正则会让请求按拒绝处理） |
| `action` | 覆盖 | `block`（默认）、`redact` 或 `flag` |
| `check_response` | 覆盖 | 也检查响应（默认开启） |
| `max_prompt_chars` | 取最严 | system、消息、工具调用和工具结果的字符数；`0` = 不限 |
| `blocked_models`、`blocked_tools` | 并集 | 名字或通配符（`pkg/match`）；工具只要被提供或在历史里被调用过就拒绝 |
| `stream_window` | 覆盖，仅全局 | 为了跨分块匹配而保留的流式文本字节数（2048，最多 16384） |
| `block_unscannable_audio` | 覆盖 | 在 `block` 且开了任何内容规则或审核时，拒绝 WebSocket 音频输入，因为规则读不了音频（默认关闭） |
| `moderation_upstream`、`moderation_model`、`moderation_fail` | 覆盖，全局/分组 | 某个上游的 OpenAI 兼容 `/moderations`（路径拼在它第一个端点的 base URL 后面，用它的一个凭据、经核心出站层调用）；`closed`（默认）在调用失败时拒绝请求，`open` 放行并记一条备注 |
| `max_moderated_events` | 取最严，全局/分组 | 一个 Realtime 会话里，其用户文本会送去审核的客户端事件数上限（200）；超过之后审核按失败处理（`moderation_fail`） |

- 读哪些内容：请求方面，读客户端提供的所有文本——system 提示词、每一轮的文本和 thinking、
  工具调用的参数和工具结果（不读图片和文件）；响应方面，读模型的文本（不读工具输入和 thinking）。
  审核只收到用户轮次的文本。检测器偏保守：数字只有在校验和或结构成立时才算命中
  （`plugins/internal/textscan`，和 `middleware.redact` 共用）。
- 管不到的地方：`/native` 直通不跑请求钩子，所以 guardrails（和所有请求类插件一样）看不到它；
  需要 guardrails 把关的密钥不要开它。批处理逐个请求检查（[客户端](clients.zh-CN.md#batches)）。
  WebSocket 会话逐个事件检查，只看文本：客户端事件的文本和输出的文本增量，从不检查音频
  （[客户端](clients.zh-CN.md#realtime-and-websocket)）。
- `block`：客户端收到本协议的内容过滤错误（OpenAI 上是带 `content_filter` 的 400，
  Anthropic 上是 `invalid_request_error`）；被禁的模型和工具返回 403，提示词超长返回 400。
  被拒的完整响应不收费，但它的上游成本照样记录。
- `redact`：命中的内容在请求的用户轮次文本里（patch 和 convert 档，和 `middleware.redact` 一样）
  以及完整响应里（重新编码，直通时也是）替换成 `[REDACTED:<rule>]`。请求里其他位置
  （system 提示词、其他轮次、工具调用和结果）的命中不做修改，记为 `flag`。流一旦有字节发出去就没法
  再脱敏，所以流里也按 `flag` 处理。审核命中没有具体位置，所以 `redact` 时直接拦截。
- 流：文本增量在一个 `stream_window` 字节的滑动窗口里匹配。`block` 时，凡是其文本还可能
  属于某个匹配的帧都先压着，一出现匹配就以本协议的错误事件结束流（如果匹配落在最新字节上、
  后续文本还可能推翻它，比如一串还没完的数字，就等后续文本或内容块结束），
  所以匹配的任何部分都到不了客户端：只有关键词时，只有可能是某个关键词开头的那段尾巴需要等；
  只要有任何正则或检测器，文本就要等到移出窗口，流最多会延迟 `stream_window` 字节。
  这个保证覆盖不超过窗口长度、且在同一个内容块之内的匹配（内容块结束会放行压着的内容）。
  `flag` 时什么都不等，仍然挨着最新字节的匹配，等它完整了（或内容块结束时）再记下。
- 判定记录：在尝试的诊断信息里加一条备注 `guardrails: <action> <phase> <rules>`
  （phase 为 `request`、`response`、`stream`），并计入
  `plugrelay_guardrail_verdicts_total{phase,action,rule}`。在任何尝试之前就被拒的请求
  没有尝试可加备注；它日志条目里的错误会写明是哪些规则。

<a id="call-order-and-failure-policy"></a>
## 调用顺序与故障策略

每种写法规则都一样（`internal/pipeline`、`sdk/PROTOCOL.md`）：

1. 每个请求一次：`auth`、`OnRequest`、selector 的 `Rank`、`meter.Reserve`，
   最后是 `OnFinish` 以及 `meter.Settle` 或 `meter.Refund`。
2. 每次尝试一次，每次重试都会再来：`OnAttempt`，然后是 provider 的
   `Prepare` / `Send` / `Classify`。
3. 前置钩子按配置顺序跑，后置钩子倒序跑；只有前置钩子跑过的插件，它的后置钩子才会跑。
4. 任何前置钩子都可以返回一个 `Reply`；这时后面的链和上游都跳过。
5. `OnEvent` 对每个流式事件运行，仅限内置插件，预算 50 毫秒；它可以放行、丢弃或中止
   （返回 `*ir.GwError` 会以这个错误结束流）。`EventHolder` 还会说明它最近的多少个事件
   暂时不能发给客户端；这些事件在放行后发出，流被中止时则丢掉。`OnResponse` 对完整响应运行一次。
6. 出故障时（崩溃、超时、协议错误——`plugin.IsFailure`）由插件的 `FailPolicy` 决定：
   `closed` 拒绝请求，`open` 继续往下走。默认值：auth、meter 和 middleware 是 closed；
   sink 和 job 是 open。
7. 同一个请求的 `Settle` 和 `Refund` 可能被重复调用，必须只生效一次。

<a id="conformance-suite-plugintest"></a>
## 一致性测试套件 plugintest

`pkg/plugin/plugintest` 让任意 `plugin.Provider` 对着一个按脚本回应的假上游跑一遍，
检查核心依赖的判定和请求头处理：正常、流式、请求头归属（预先埋下的客户端密钥绝不能到达上游）、
流被切断、带 retry-after 的 429、401、账号被封（隔离，不重试）、内容违规（哪里都不重试）、
拿不准（从严）、发送前超时、首字节慢、超大 SSE 事件、错误透传、5xx。

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

`Prepare` 设置的请求头必须列在 `OwnedHeaders` 里，除非 `Prepare` 只在客户端没发这个头时才填
（比如协议版本的默认值）：套件会带着客户端的值再跑一次 `Prepare` 来区分这两种情况。

`UseSend: true` 会测试 provider 自己的 `Send`。`Skip` 和 `Expect` 用来调整那些确实不同的场景。
`manifests/` 里每个文件的每个端点都会在 `plugins/provider/compatible/conformance_test.go`
里对着假上游跑这套测试；Bedrock 用事件流测试数据经 `Send` 跑，Vertex 对着一个假的令牌端点跑
（`plugins/provider/internal/cloudtest`）。外部 provider 在 `internal/extplugin/examples_test.go`
里经宿主跑（echo provider 能通过这套测试）；非 Go 插件没有可在仓库外使用的运行器。
