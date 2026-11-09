# plugrelay 外部插件协议

[English](PROTOCOL.md) | 简体中文

版本 1。

外部插件是 plugrelay 启动的一个进程，双方用 JSON-RPC 2.0 通信。每个方法对应
`pkg/plugin` 里的一个扩展点；在核心看来，外部插件和内置插件没有区别。本文就是双方的
约定：Go 宿主端在 `internal/extplugin`，插件端由 `sdk/go` 里的 Go SDK 实现。

<a id="1-transport-and-framing"></a>
## 1. 传输与分帧

消息分帧方式和 Language Server Protocol 一样：先是 ASCII 头部，接一个空行，然后是
恰好 `Content-Length` 字节的 UTF-8 JSON 正文。

```
Content-Length: 56\r\n
\r\n
{"jsonrpc":"2.0","id":1,"method":"shutdown","params":{}}
```

- `Content-Length` 必填；头部名不区分大小写；其他头部（比如 `Content-Type`）一律忽略。
- 单条消息最大 64 MiB。
- 双方都会发请求：宿主调用插件的方法，插件调用 `host.*` 方法。请求 id 在发送方内唯一；
  宿主用整数。
- 请求并发处理，响应可能乱序返回。

有两种传输方式，由 `plugin.yaml` 里的 `transport` 选择：

| transport | 协议通道 | 日志 |
|-|-|-|
| `stdio`（默认） | 插件读 stdin、写 stdout | stderr，一行一条日志 |
| `unix` | 插件连接 `PLUGRELAY_SOCKET` 给出的 socket 路径 | stdout 和 stderr |

stdio 模式下，往 stdout 写协议帧以外的任何内容都会弄断连接。

进程在自己的插件目录里启动，环境变量只给最少的一组：`PATH`、`HOME`、`LANG`、
`LC_ALL`、`TMPDIR`、`TZ`、清单里的 `env`，再加上 `PLUGRELAY_PLUGIN_ID`、
`PLUGRELAY_PROTOCOL_VERSION`，`unix` 模式下还有 `PLUGRELAY_SOCKET`。宿主自己的环境
变量不会传下去。

<a id="2-lifecycle"></a>
## 2. 生命周期

```
host                              plugin
 |-- initialize ------------------->|
 |<------------------------ result -|
 |-- method calls / host calls <--->|
 |-- shutdown --------------------->|
 |<------------------------ result -|
 |-- exit (notification) ---------->|   process exits
```

<a id="initialize"></a>
### initialize

参数：

```json
{"protocol_version": 1, "host_version": "0.4.0", "plugin_id": "ip-allowlist",
 "settings": {"allow": ["10.0.0.0/8"]}}
```

`settings` 是清单里声明的设置项在全局层的取值。每个请求合并后的取值随每次调用一起送达
（见 `call`）。

返回：

```json
{"protocol_version": 1,
 "manifest": {"id": "ip-allowlist", "version": "0.1.0"},
 "capabilities": {"points": ["middleware.onRequest"]}}
```

以下情况宿主拒绝握手（并按退避重试）：`protocol_version` 不一致；`manifest.id` 和清单
不一致；`capabilities.points` 非空，但缺少清单列出的某个扩展点。握手必须在
`timeouts.startup`（默认 10s）内完成。

<a id="shutdown-and-exit"></a>
### shutdown 与 exit

`shutdown` 让插件做完手上的活、释放资源，回 `{}`。随后是 `exit` 通知，插件收到后退出。
超过 `timeouts.shutdown`（默认 5s）宿主就直接杀进程。

<a id="supervision"></a>
### 进程看护

- 每次调用都有超时（`timeouts.call`，默认 5s；`provider.send` 用 `timeouts.send`，
  默认 10m；`timeouts.methods` 可按方法单独设），超时也包括排队等 `max_concurrency`
  个并发名额（默认 16）的时间。
- 超时后宿主发 `$/cancelRequest`，并让这次调用失败。
- 进程退出后按指数退避重启（500ms 起，逐次翻倍，最多 30s；连续运行 30s 后退避清零）。
  进程不在的时候，调用立即失败。
- 调用失败、超时或没有回应时，按插件的 `fail_policy` 处理：`closed` 拒绝这个请求，
  `open` 放它继续走。默认值：sink 和 job 是 `open`，其余都是 `closed`。

<a id="cancellation"></a>
### 取消

任何一方都可以发 `$/cancelRequest` `{"id": <request id>}`。接收方能停就停；如果响应
还是发出来了，会被忽略。

<a id="3-common-objects"></a>
## 3. 通用对象

线上传输的时长一律是毫秒（`*_ms`），时间一律是 RFC 3339。头部是"字符串数组"组成的
对象（`{"User-Agent": ["x/1"]}`）。与协议无关的中间类型（`request`、`response`、
`usage`、`tool_call`、`tool_result`、`event`）用 `pkg/ir` 的 JSON 编码；`record` 用
`plugin.Record`，其中的 `ttfb` 和 `duration` 单位是纳秒。

<a id="call"></a>
### call

每个按请求调用的方法都会带上 `call`。

```json
{"id": "req_01H...", "caller": {"key_id": "k1", "key_name": "ci", "user_id": "u1", "group_id": "g1"},
 "protocol": "anthropic", "model": "claude-sonnet", "stream": true,
 "session_id": "s-1", "client_ip": "203.0.113.7",
 "header": {"User-Agent": ["claude-cli/2.0"]},
 "settings": {"allow": ["10.0.0.0/8"]},
 "state": {"n": 1},
 "request": { "...": "ir.Request, only with request_body: true" }}
```

- `header` 里永远不会有 `Authorization`、`X-Api-Key`、`X-Goog-Api-Key`、`Cookie`
  或 `Proxy-Authorization`。
- `settings` 按 Global → Group → User → Key 的顺序为当前调用方合并。合并方式为
  `strictest` 的列表如果在多层都设置了，送达的是每一层都列出的条目；如果各层没有交集，
  送达 `["\u0000deny-all"]`（什么都不匹配）。这时 `setting_layers` 里放着每层自己的
  列表，离调用方最近的一层在最后。条目是模式或范围（CIDR、glob）时，要拿它和每一层的
  列表逐一比对，因为这类条目按字符串求交集是不对的。
- `state` 是本插件在同一请求更早的钩子里作为 `state` 返回的内容，原样带回。

<a id="error"></a>
### error

用在回复、裁决、流错误和拒绝里。

```json
{"kind": "rate_limited", "message": "slow down", "retry_after_ms": 7000,
 "reset_at": "2026-01-01T00:00:00Z", "no_retry": false, "status": 429,
 "code": "rate_limit_exceeded", "tokens_used": 0, "tokens_limit": 0,
 "header": {"Retry-After": ["7"]}}
```

`kind` 取 `ir.ErrorKind` 的值：`invalid_request`、`context_too_long`、
`authentication`、`permission`、`not_found`、`rate_limited`、`quota_exhausted`、
`overloaded`、`content_filtered`、`capability_missing`、`timeout`、`internal`、
`upstream`。客户端收到的是按它自己协议编码后的错误。

<a id="http_request--http_response"></a>
### http_request / http_response

```json
{"method": "POST", "url": "/api/plugins/x/stats?d=1", "host": "relay", "path": "/api/plugins/x/stats",
 "query": "d=1", "header": {}, "remote_addr": "203.0.113.7:5123",
 "body": "text body", "body_base64": "", "timeout_ms": 0}
{"status": 200, "header": {}, "body": "text", "body_base64": ""}
```

正文是合法 UTF-8 就放在 `body` 里，否则放在 `body_base64` 里。

<a id="4-plugin-methods"></a>
## 4. 插件方法

插件实现清单 `points` 里列出的方法。宿主不会调用其他方法；插件对不认识的方法回
`-32601`。

| 方法 | 参数 | 返回 |
|-|-|-|
| `auth.authenticate` | `request`（http_request，含全部头部） | `caller`，或 `null`（交给下一个认证器）；拒绝时回 `-32001` |
| `middleware.onRequest` | `call` | `reply`（`{error}` 或 `{response: ir.Response}`）或 `null`；`state` |
| `middleware.onAttempt` | `call`、`attempt` | `set_header`（字符串对象）；`state` |
| `middleware.onFinish` | `call`、`result` | `{}` |
| `selector.rank` | `call`、`candidates` | `order`：候选项下标 |
| `meter.reserve` | `call`、`estimate_micro` | `reservation` `{id, amount_micro}` 或 `null`；拒绝时回 `-32001` |
| `meter.settle` | `call`、`reservation`、`usage`、`cost_micro` | `{}` |
| `meter.refund` | `call`、`reservation` | `{}` |
| `sink.consume` | `record` | `{}` |
| `job.run` | `{}` | `{}` |
| `admin.handle` | `route`（清单里的路径）、`request`（http_request） | http_response |
| `provider.endpoints` | `upstream` | `endpoints`、`owned_headers` |
| `provider.prepare` | `outgoing` | `header`、`url` |
| `provider.send` | `token`、`outgoing`（带正文）、`request` | 先发若干 `$/event`，再返回 `outcome` 或 `null` |
| `provider.classify` | `outcome` | `verdict` |
| `provider.listModels` | `upstream`、`credential` | `models`（`[]ir.ModelInfo`） |
| `provider.refresh` | `upstream`、`credential` | `secret`、`expires` |
| `provider.quota` | `upstream`、`credential` | `quota` `{remaining, left?, limit?, reset_at?}`（remaining 是比例，-1 表示未知；0 表示这个凭据在 `reset_at` 之前不再使用） |

没有 `middleware.onEvent`：按事件触发的钩子每个流式事件都要跑一次进程往返，所以只开放
给内置插件。

<a id="call-timing"></a>
### 调用时机

每个请求一次：`auth.authenticate`、`middleware.onRequest`、`selector.rank`、
`meter.reserve`，最后是 `middleware.onFinish` 和 `meter.settle`/`meter.refund`。
每次尝试一次（每次重试都会再来一遍）：`middleware.onAttempt`、`provider.prepare`、
`provider.send`、`provider.classify`。前置钩子按配置顺序执行，后置钩子倒序执行；
前置钩子跑过，对应的后置钩子才会跑。`meter.settle` 和 `meter.refund` 对同一个
`call.id` 必须是幂等的。

<a id="middleware"></a>
### middleware

`attempt`：

```json
{"n": 1, "tier": "passthrough", "upstream_model": "gpt-x", "header": {},
 "candidate": {"upstream_id": "u", "endpoint_id": "e", "credential_id": "c", "protocol": "openai-chat",
   "upstream_model": "gpt-x", "priority": 0, "weight": 1, "same_protocol": true, "sticky": false,
   "fidelity": "lossless", "latency_ms": 420, "in_flight": 2, "price_in_micro": 0, "price_out_micro": 0,
   "quota_reset": "2026-01-01T00:00:00Z"}}
```

`set_header` 不能设置凭据类头部；归 provider 管的头部之后会被 provider 覆盖。外部
middleware 改不了请求正文。

`result`（onFinish）：`{"status": 200, "error": null, "usage": {...}, "attempts": 1, "delivered": true}`。

<a id="selector"></a>
### selector

`order` 只能调整顺序：不认识或重复的下标被忽略，没列到的候选项按原顺序排在最后。
调用失败时沿用路由器原来的顺序。

<a id="sink"></a>
### sink

`record` 是一条已结束的请求（`plugin.Record`）；`id` 是中转给这个请求分配的 id。
`fail_policy: closed` 的 sink 就是流水账：`sink.consume` 失败时宿主按退避重试
（100ms、500ms、2s）；遇到分不清是超时还是失败的情况，同一条记录可能再送一次，所以
sink 必须把重复的 `record.id` 视为已存过（插入时忽略重复）。每个流水账 sink 有自己的
队列，慢的只拖累它自己。`open` 的 sink 每条记录最多收到一次，负载过高时可能丢一些。

<a id="provider"></a>
### provider

`upstream`：`{"id", "provider", "endpoints": [plugin.Endpoint], "config": {}}`。
不实现 `provider.endpoints` 时，使用配置里的端点，并采用清单里的
`provider.owned_headers`。

`outgoing`：

```json
{"upstream": {...}, "endpoint": {"id": "e", "protocol": "openai-chat", "base_url": "https://api.example.com", "issuer": "example"},
 "credential": {"id": "c", "upstream_id": "u", "kind": "shared", "secret": "sk-...", "meta": {}},
 "model": "upstream-model", "method": "POST", "url": "https://api.example.com/v1/chat/completions",
 "header": {"Content-Type": ["application/json"]},
 "body": {"...": "provider.send only: the body as JSON, or body_base64"}}
```

`provider.prepare` 返回完整的出站头部（整个替换掉宿主的那份），也可以返回一个新的
绝对 `url`。它声明归自己管的每个头部都必须设置或删除；客户端传来的这些头部的值永远
到不了上游。

`provider.send` 自己完成和上游的交互，并把中间格式的事件以通知的形式流式发回，通知里
带着这次调用的 token：

```json
{"jsonrpc": "2.0", "method": "$/event", "params": {"token": "6f1c...", "event":
  {"kind": "delta", "index": 0, "delta": {"kind": "text", "text": "Hel"}}}}
```

规则：

1. 事件顺序遵循 `ir.Event`：先 `start`，然后每个块依次是 `block_start`、若干
   `delta`、`block_stop`；下标从 0 开始连续编号；`ping`、`status` 和 `usage` 可以
   出现在任何位置。
2. 流以恰好一个 `done` 或 `error` 事件结束，然后返回响应 `{"outcome": null}`。
3. **还没输出任何内容**时出的错不走流：直接返回 `{"outcome": {...}}`（不发事件），
   宿主据此判断并换下一个候选重试。请求根本没发出去时设 `sent: false`。
4. **已经输出内容之后**出的错是一个 `error` 事件，这次尝试不能重试。进程在流中途挂掉
   时由宿主补发这个事件。
5. 事件里的 `error` 对象使用线上的 `error` 格式。

`outcome`（classify 的输入，也是 send 失败时的返回）：

```json
{"endpoint_id": "e", "credential_id": "c", "status": 429, "header": {"Retry-After": ["7"]},
 "body": "{\"error\":...}", "error": "transport error text", "signal": {"kind": "rate_limited", "message": "..."},
 "sent": true}
```

`signal` 是宿主的协议编解码器对错误正文的解读。`verdict`：

```json
{"cause": "rate_limit", "retry": "next", "credential": "cooldown",
 "until": "2026-01-01T00:00:07Z", "model_scope": false,
 "downgrade": {"reasoning.effort": "\"high\""},
 "client_error": {"kind": "rate_limited", "message": "...", "retry_after_ms": 7000},
 "sent": true}
```

- `cause`：`request`、`transient`、`rate_limit`、`quota`、`auth_expired`、
  `banned`、`content`、`unknown`。
- `retry`：`none`、`next`、`downgrade`（按 `downgrade` 里的修改重发到同一个上游：
  gjson 路径 → JSON 值）。
- `credential`：`keep`、`cooldown`（冷却到 `until`）、`refresh`、`quarantine`、
  `manual`。

每个 provider 都必须遵守的裁决规则（由 `pkg/plugin/plugintest` 检查）：内容违规在任何
凭据上都不重试；疑似封号就隔离凭据、不重试；拿不准是这两种中的哪一种时，按隔离、不重试
处理；不是凭据本身的问题就别动它；没发出去的请求可以放心重试。`provider.classify`
调用失败时，宿主保留凭据，只重试没发出去的请求。

<a id="5-host-methods"></a>
## 5. 宿主方法

插件可以调用宿主的这些方法。除 `host.log` 外，每个方法都要在清单的
`permissions.host` 里列出名字，否则回 `-32002`。

| 方法 | 参数 | 返回 |
|-|-|-|
| `host.log` | `level`（`debug`/`info`/`warn`/`error`）、`message`、`attrs` | `{}` |
| `host.kv.get` | `key` | `found`、`value` |
| `host.kv.put` | `key`、`value`、`ttl_ms`（0 = 不过期） | `{}` |
| `host.kv.delete` | `key` | `{}` |
| `host.kv.incr` | `key`、`delta`、`ttl_ms` | `value`（新的整数值） |
| `host.http` | http_request（绝对 `url`，可选 `timeout_ms`、`upstream_id`、`credential_id`） | http_response |

KV 的值是字符串，按插件隔离；二进制数据请先 base64 编码。`host.http` 走核心的出站
客户端，所以 SSRF 防护照样生效。替某个上游凭据发的请求（provider 的 `send`、
`listModels`、`quota`、`refresh`）要带上对应的 `upstream_id` 和 `credential_id`：
这样请求会走这个凭据的出站设置，先用凭据的代理，没有就用上游的代理；代理不可用时直接
失败，不会绕过代理直连。不带这两个字段时请求直连。插件可以自己开连接（宿主拦不住），
但这些连接不受上述规则约束，必须在 `permissions.network` 里声明。

<a id="6-errors"></a>
## 6. 错误

| 错误码 | 含义 | 宿主的处理 |
|-|-|-|
| -32700 | 解析错误 | |
| -32600 | 无效请求（协议版本不支持也用这个） | |
| -32601 | 方法不存在 | 当作没实现 |
| -32602 | 参数无效 | |
| -32603 | 内部错误 | |
| -32000 | 插件错误：处理函数执行失败 | 按失败策略处理 |
| -32001 | 拒绝：插件有意拒绝；`data` 是一个 `error` 对象 | 原样发给客户端；不走失败策略 |
| -32002 | 没有权限（宿主 API 不在 `permissions.host` 里） | |
| -32003 | 未初始化 | |
| -32004 | 宿主 API 不可用（本宿主没配置） | |
| -32800 | 请求已取消 | |

middleware 拒绝请求有两种方式：`reply.error` 或 `-32001`。认证失败
（`kind: authentication`）和计费拒绝（`kind: quota_exhausted`）请用 `-32001`。

<a id="7-versioning"></a>
## 7. 版本

- `protocol_version` 是一个整数，只有不兼容的改动才会加一（删字段或改字段类型、改语义、
  新增必须遵守的行为）。宿主只说一个版本；为别的版本构建的插件握手会失败，并给出明确的
  错误。
- 增量改动不改版本号：新的可选字段、新方法、新的枚举值。双方都忽略不认识的字段；插件对
  不认识的方法回 `-32601`；不认识的枚举值按最保守的含义处理（`cause: unknown`、
  `retry: none`、`credential: keep`）。
- 宿主只会在插件 `points` 里列出的扩展点上调用它，所以新增的扩展点不会落到没列它的插件
  头上。

<a id="8-pluginyaml"></a>
## 8. plugin.yaml

每个插件目录里都有一个 `plugin.yaml`。出现不认识的键就报错。
[`plugin.schema.json`](plugin.schema.json) 是给编辑器用的 JSON Schema；
`internal/extplugin` 里的加载器还会额外检查下面这些跨字段规则。

```yaml
id: ip-allowlist              # [a-z0-9][a-z0-9-]*；核心看到的是 "<kind>.<id>"
name: IP allowlist
version: 0.1.0
description: Reject requests from addresses outside the allowed ranges.
kind: middleware              # 或 kinds: [middleware, admin]
points: [middleware.onRequest]

command: ./ip-allowlist       # 只写名字时在 PATH 里查找；
args: []                      # 相对路径相对本目录解析
env: {}                       # PLUGRELAY_* 开头的名字是保留的
transport: stdio              # stdio | unix

timeouts:                     # Go 时长格式
  startup: 10s
  call: 5s
  send: 10m                   # provider.send
  shutdown: 5s
  methods: {middleware.onRequest: 500ms}
max_concurrency: 16
fail_policy: closed           # closed | open
request_body: false           # middleware 调用里是否带上解码后的请求
reports_off_box: false        # 会把数据发到本机之外时设为 true；测试模式会跳过它
requires: []                  # 离了就没法工作的插件 id，例如 [sink.requestlog]

settings:                     # plugin.Setting；控制台据此渲染表单
  - key: allow
    type: list                # string int float bool duration list secret model upstream group json
    default: []
    scopes: [global, group, key]
    merge: strictest          # override | strictest | union（仅列表）
    description: CIDR ranges allowed to call the relay.
    restart: false
    enum: []                  # 仅 string 类型的设置项

permissions:
  host: [kv, http]            # host.log 之外要用的宿主 API
  network: none               # 展示给管理员看；不强制
  files: none

provider:                     # kind 为 provider 时
  owned_headers: [Authorization]
  issuer: example
job:                          # kind 为 job 时
  every: 1h
admin:                        # kind 为 admin 时；挂载在 /api/plugins/<id>/ 下
  routes:
    - {method: GET, path: /stats, role: admin}    # role: admin | member | ""（任意用户）
```

校验还要求：每个 kind 至少有一个扩展点，且包含它必需的那些（auth：
`auth.authenticate`；meter：三个全要；selector、sink、job、admin：各自唯一的那个方法；
provider：`provider.prepare` 或 `provider.send`）；`middleware.onEvent` 会被拒绝；
`secret` 类型的设置项不能有默认值。
