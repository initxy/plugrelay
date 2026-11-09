# MCP 网关

[English](mcp.md) | 简体中文

中转站把配置好的 MCP 服务器开放给持平台密钥的客户端：团队只需把每个服务器配置一次（地址、凭据），每个客户端都用自己的密钥经 plugrelay 访问它。`mcp` 功能开着时网关才运行（[配置](configuration.zh-CN.md#features)）；关掉后所有 `/mcp` 路径都返回 404。代码：`internal/mcp`（见包文档），接线在 `internal/app/mcp.go`。

- [端点](#endpoints)
- [协议版本](#protocol-versions)
- [添加服务器](#adding-servers)
- [访问权限](#access)
- [日志与限流](#logging-and-limits)
- [接入客户端](#connecting-clients)
- [网关的局限](#limits-of-the-gateway)

<a id="endpoints"></a>
## 端点

| 路径 | 提供什么 |
|-|-|
| `/mcp` | 这个密钥能用的所有服务器的全部工具，命名为 `<server>__<tool>` |
| `/mcp/<server-id>` | 单个服务器：工具保留原名，资源、提示词和补全也原样转发 |

两个端点都走 MCP 的 Streamable HTTP 传输，平台密钥的传法和模型 API 一样（`Authorization: Bearer pr_…`、`x-api-key`）。没带密钥或密钥不对返回 401；密钥无权使用的服务器、或已停用的服务器返回 404。

聚合后的工具名：`[A-Za-z0-9_-]` 以外的字符换成 `_`；超过 64 个字符的名字会被截断，末尾接上服务器名加工具名哈希的 8 位十六进制；处理后仍然重名的工具按工具名排序依次编号（`_2`、`_3`），所以服务器调整列表顺序时名字不会变。服务器 id 里没有下划线，所以第一个 `__` 一定是服务器 id 的结尾。MCP 规范要求做聚合的代理给工具名加上服务器标识前缀（[tools, 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/server/tools)）。

<a id="protocol-versions"></a>
## 协议版本

2026-11-06 对照 MCP 规范核对过：当前修订版是 2026-07-28；2025-11-25 及更早的版本属于“旧版”（legacy）（[versioning](https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning)）。

| 哪一端 | 网关怎么说话 |
|-|-|
| 客户端，旧版（2025-03-26、2025-06-18、2025-11-25） | `initialize`（客户端的版本受支持就用它，否则用 2025-11-25）；响应里带 `Mcp-Session-Id`，之后每次请求都必须带上（不带返回 400，未知或属于别的密钥返回 404）；校验 `MCP-Protocol-Version`；`DELETE` 结束会话；`GET` 返回 405（不提供服务器主动推送的流）；支持 `ping`（[transports, 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)） |
| 客户端，2026-07-28 | 无状态：`MCP-Protocol-Version` 必须和 `_meta` 一致，`Mcp-Method` 必须和请求体的 `method` 一致，`Mcp-Name`（明文或 `=?base64?…?=`）必须和 `tools/call`、`prompts/get` 的 `params.name` 或 `resources/read` 的 `params.uri` 一致；缺头或不一致返回 400，错误码 `-32020`（[standard request headers](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http#standard-request-headers)、[server validation](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http#server-validation)）；支持 `server/discover`，结果带 `resultType`，列表结果带 `ttlMs` 和 `cacheScope: private`，未知方法返回 404（[Streamable HTTP, 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)） |
| 上游，传输方式 `auto`（默认） | Streamable HTTP：先用 2025-11-25 发 `initialize`；如果返回的错误在 `data.supported` 里列出了 2026-07-28，就切到无状态修订版（`server/discover`、`Mcp-Method`、`Mcp-Name`、`_meta`）；如果返回 400/404/405 且没有 JSON-RPC 响应体，就退回 [2024-11-05](https://modelcontextprotocol.io/specification/2024-11-05/basic/transports) 的 HTTP+SSE 传输（`GET` 流、`endpoint` 事件、`message` 事件） |
| 上游，`streamable-http` / `sse` | 只用指定的那种传输 |

会话 id 是用 HMAC 签名的令牌，里面是密钥 id、协议版本和签发时间，所以任何一个中转节点都认得它，也不用存任何东西；超过 24 小时的会话 id 返回 404，客户端重新 initialize 即可。每个节点对每个上游服务器只保持一条连接（服务器返回 404 时重开一次会话），服务器的工具列表缓存一分钟。JSON-RPC 批量请求一律拒绝（2025-06-18 已去掉这一特性）。`Origin` 指向别的主机的请求返回 403（防 DNS 重绑定）。

如果 `tools/call` 带了 `progressToken`，且客户端接受 `text/event-stream`，就用 SSE 回复：先推送服务器为这次调用发的进度通知，最后是结果。令牌在发往上游时会被改写，所以共用同一条连接的客户端不会看到彼此的进度。

<a id="adding-servers"></a>
## 添加服务器

控制台：MCP 服务器页面（添加、编辑、测试）。API：`/api/v1/mcp/servers`（[api.md](api.zh-CN.md)）；列表存在插件 KV `core.mcp` 里。

| 字段 | 含义 |
|-|-|
| `id` | 1–32 个小写字母、数字或短横线；用在 `/mcp/<id>` 和 `<id>__<tool>` 里 |
| `url` | 服务器的 MCP 端点（旧版传输填它的 `/sse` 地址） |
| `transport` | `auto`、`streamable-http` 或 `sse` |
| `headers` | 原样发送 |
| `secret_headers` | 凭据（`Authorization: Bearer …`、`X-Api-Key`），用主密钥加密保存，永不返回；值留空即删除这一项。把 `url` 改到别的协议、主机或端口时，必须在同一次编辑里把所有已存的密文请求头重新填一遍（或清掉），否则拒绝修改，这样密文不会在没人留意的情况下跟着服务器发到新地址 |
| `tools_allow` / `tools_deny` | 所有人都只能用 / 都不能用的工具，填工具原名或通配符；所有工具通配符里的 `*` 和 `?` 都能匹配 `/` |
| `proxy`、`allow_private` | 固定的出口代理；允许私网或回环地址（否则 SSRF 防护会拒绝，和上游的规则一样） |
| `timeout_sec` | 单次调用超时，默认 120 秒 |

客户端的请求头不会转给服务器；发出去的只有配置好的那些，并且走核心出口。`POST /api/v1/mcp/servers/{id}/test` 会重新建一条连接并列出工具。

<a id="access"></a>
## 访问权限

授权放在 `middleware.access` 设置里（核心模块；全局值在 MCP 网关功能的表单里填，下层在分组、用户或密钥上设；[配置](configuration.zh-CN.md#layered-plugin-settings)）：

| 设置 | 合并方式 | 含义 |
|-|-|-|
| `mcp_servers` | override | 调用方能用的服务器 id，`"*"` 表示全部；留空表示一个都不能用 |
| `mcp_tools_allow` | strictest | 调用方只能用的 `<server>__<tool>` 名称或通配符：每一层设的列表都得放行这个工具；留空表示已授权的全部可用 |
| `mcp_tools_deny` | union | 禁用的 `<server>__<tool>` 名称或通配符（`github__delete_*`）；分组或密钥只能往里加，不能删 |

```bash
# eng 分组可以用 github 和 docs 两个服务器，但不能用任何 github__delete_* 工具
# （只改动列出的设置；值为 null 表示删除这个设置）
curl -X PUT "$RELAY/api/v1/groups/eng/settings" \
  -H "Authorization: Bearer $PLUGRELAY_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"settings":{"middleware.access":{"mcp_servers":["github","docs"],"mcp_tools_deny":["github__delete_*"]}}}'
```

调用方无权使用的工具不会出现在列表里，调用时返回 `-32602 unknown tool`，和工具不存在时一样。工具规则只管工具：密钥一旦被授权某个服务器，就能在 `/mcp/<server-id>` 上访问它的全部资源、提示词和补全。

<a id="logging-and-limits"></a>
## 日志与限流

每次 `tools/call` 记一条请求日志：协议 `mcp`，模型 `<server>__<tool>`，上游 `mcp:<server>`，状态码（200；404 表示工具不存在或被拒；429 表示被限流；400/502 表示服务器出错），耗时，以及错误信息；带 `isError` 的结果按状态 200 记录，并写上错误文本。只有对这个调用方开启了 `sink.requestlog` 的正文记录时，才会保存参数和结果。`middleware.ratelimit` 在运行时，调用也要过它：调用方的 `rpm` 和 `concurrency` 计数和模型请求共用（`tpm` 不适用）。在 `/mcp/<server-id>` 上，`resources/read` 和 `prompts/get` 也同样记日志、同样限流，模型记作 `<server>/<method>`。列表、`initialize` 和补全不记也不限。

<a id="connecting-clients"></a>
## 接入客户端

Claude Code 和 Codex 的配置写在 [clients.md](clients.zh-CN.md#mcp-servers)；`plugrelay setup` 不会替你写这部分。

<a id="limits-of-the-gateway"></a>
## 网关的局限

- 不支持 stdio 服务器（以子进程方式启动的那种）；请把它们放到一个 Streamable HTTP 桥接后面。
- 不支持 `listChanged`、资源订阅，也不支持 2026-07-28 的 `subscriptions/listen` 流；客户端需要重新拉列表（工具缓存一分钟）。
- 服务器主动发起的请求（sampling、elicitation、roots）一律用 `-32601` 拒绝；中转站不向客户端提供其中任何一项。
- 每个服务器在每个节点上只有一条共用的上游连接：有状态的服务器会把所有中转客户端看成同一个。
- 不处理到上游服务器的 OAuth；请把令牌填在 `secret_headers` 里。
