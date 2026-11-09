# MCP 网关

[English](mcp.md) | 简体中文

每个 MCP 服务器只需配置一次（地址、凭据），所有客户端用各自的平台 key 经 plugrelay 访问。`mcp` 功能开着时网关才工作（[配置](configuration.zh-CN.md#features)）；关掉后所有 `/mcp` 路径返回 404。代码在 `internal/mcp`，装配在 `internal/app/mcp.go`。

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
| `/mcp` | 这把 key 能用的所有服务器的全部工具，命名为 `<server>__<tool>` |
| `/mcp/<server-id>` | 单个服务器：工具保留原名，资源、提示词和补全也一并转发 |

两个端点都走 Streamable HTTP，平台 key 的传法和模型 API 一样（`Authorization: Bearer pr_…` 或 `x-api-key`）：key 不对返回 401，无权使用或已停用的服务器返回 404。

合并后的工具名里，`[A-Za-z0-9_-]` 以外的字符换成 `_`；超过 64 个字符的截断，末尾接 8 位十六进制哈希；还重名的按工具名排序加 `_2`、`_3`。服务器 id 不含下划线，第一个 `__` 之前就是服务器 id。

<a id="protocol-versions"></a>
## 协议版本

2026-11-06 对照 MCP 规范核对过：当前版本是 2026-07-28，2025-11-25 及更早的算旧版（[versioning](https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning)）。

| 哪一端 | 网关怎么处理 |
|-|-|
| 客户端，旧版（2025-03-26、2025-06-18、2025-11-25） | `initialize`（客户端的版本受支持就用，否则用 2025-11-25）；响应带 `Mcp-Session-Id`，之后必须带上（不带返回 400，未知或属于别的 key 返回 404）；校验 `MCP-Protocol-Version`；`DELETE` 结束会话；`GET` 返回 405（不提供服务器主动推送的流）；支持 `ping`（[transports, 2025-11-25](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports)） |
| 客户端，2026-07-28 | 无状态：`MCP-Protocol-Version` 必须和 `_meta` 一致，`Mcp-Method` 和请求体的 `method` 一致，`Mcp-Name`（明文或 `=?base64?…?=`）和 `tools/call`、`prompts/get` 的 `params.name` 或 `resources/read` 的 `params.uri` 一致；缺失或不一致返回 400，错误码 `-32020`（[standard request headers](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http#standard-request-headers)、[server validation](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http#server-validation)）；支持 `server/discover`；结果带 `resultType`，列表结果带 `ttlMs` 和 `cacheScope: private`；未知方法返回 404（[Streamable HTTP, 2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)） |
| 上游，传输方式 `auto`（默认） | 先用 Streamable HTTP 以 2025-11-25 发 `initialize`；返回的错误在 `data.supported` 里列出 2026-07-28 时，切到无状态版本；返回 400/404/405 且没有 JSON-RPC 响应体时，退回 [2024-11-05](https://modelcontextprotocol.io/specification/2024-11-05/basic/transports) 的 HTTP+SSE 传输 |
| 上游，`streamable-http` / `sse` | 只用指定的传输 |

- 会话 id 是签名令牌，不用存储，任何节点都认；超过 24 小时返回 404，客户端重新 initialize 即可。
- 每个节点对每个上游服务器只保持一条连接，工具列表缓存一分钟。
- 不接受 JSON-RPC 批量请求（2025-06-18 已去掉）；`Origin` 指向别的主机的请求返回 403。
- `tools/call` 带了 `progressToken` 且客户端接受 `text/event-stream` 时用 SSE 回复：先推服务器的进度通知，最后是结果。共用连接的客户端看不到彼此的进度。

<a id="adding-servers"></a>
## 添加服务器

控制台在 MCP 服务器页面操作；API 是 `/api/v1/mcp/servers`（[管理 API](api.zh-CN.md)），存在插件 KV `core.mcp` 里。

| 字段 | 含义 |
|-|-|
| `id` | 1–32 个小写字母、数字或短横线；用在 `/mcp/<id>` 和 `<id>__<tool>` 里 |
| `url` | 服务器的 MCP 地址（旧版传输填它的 `/sse` 地址） |
| `transport` | `auto`、`streamable-http` 或 `sse` |
| `headers` | 原样发送 |
| `secret_headers` | 凭据（`Authorization: Bearer …`、`X-Api-Key`），加密保存，不会返回；值留空即删除。把 `url` 改到别的协议、主机或端口时，必须在同一次编辑里重填或清空它们 |
| `tools_allow` / `tools_deny` | 对所有人生效的工具白名单 / 黑名单，填工具原名或通配符；`*` 和 `?` 也匹配 `/` |
| `proxy`、`allow_private` | 固定出口代理；允许私网或回环地址（否则会被 SSRF 防护拒绝） |
| `timeout_sec` | 单次调用超时，默认 120 秒 |

发出去的只有配置的请求头，客户端的不会转过去。`POST /api/v1/mcp/servers/{id}/test` 会新建一条连接并列出工具。

<a id="access"></a>
## 访问权限

授权写在 `middleware.access` 设置里：全局值在 MCP 网关功能的表单里填，分组、用户或 key 上可以再覆盖（[配置](configuration.zh-CN.md#layered-plugin-settings)）。

| 设置 | 合并方式 | 含义 |
|-|-|-|
| `mcp_servers` | override | 能用的服务器 id，`"*"` 表示全部；留空表示都不能用 |
| `mcp_tools_allow` | strictest | 只能用的 `<server>__<tool>` 名称或通配符；每一层的列表都得放行；留空表示已授权的都能用 |
| `mcp_tools_deny` | union | 禁用的 `<server>__<tool>` 名称或通配符（`github__delete_*`）；下层只能加，不能删 |

```bash
# eng 分组可以用 github 和 docs 两个服务器，但不能用任何 github__delete_* 工具
# （只改动列出的设置；值为 null 表示删除这个设置）
curl -X PUT "$RELAY/api/v1/groups/eng/settings" \
  -H "Authorization: Bearer $PLUGRELAY_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"settings":{"middleware.access":{"mcp_servers":["github","docs"],"mcp_tools_deny":["github__delete_*"]}}}'
```

没权限的工具不会列出来，调用时返回 `-32602 unknown tool`。工具规则只管工具：key 被授权某个服务器后，就能在 `/mcp/<server-id>` 上用它的全部资源、提示词和补全。

<a id="logging-and-limits"></a>
## 日志与限流

每次 `tools/call` 记一条请求日志：协议 `mcp`，模型 `<server>__<tool>`，上游 `mcp:<server>`，耗时、错误信息和状态码（404 是工具不存在或被拒，429 是被限流，400/502 是服务器出错；结果带 `isError` 的按 200 记并写上错误文本）。只有对这个调用方开了 `sink.requestlog` 的正文记录，才保存参数和结果。`middleware.ratelimit` 在运行时，调用和模型请求共用调用方的 `rpm` 和 `concurrency`（`tpm` 不管）。`/mcp/<server-id>` 上的 `resources/read` 和 `prompts/get` 也照样记日志、限流，模型记作 `<server>/<method>`；列表、`initialize` 和补全不记也不限。

<a id="connecting-clients"></a>
## 接入客户端

Claude Code 和 Codex 的配置见[接入客户端](clients.zh-CN.md#mcp-servers)；`plugrelay setup` 不会写这部分。

<a id="limits-of-the-gateway"></a>
## 网关的局限

- 不支持 stdio 服务器；请放到 Streamable HTTP 桥接后面。
- 不支持 `listChanged`、资源订阅和 2026-07-28 的 `subscriptions/listen` 流；客户端需要重新拉列表（工具缓存一分钟）。
- 服务器主动发起的请求（sampling、elicitation、roots）一律返回 `-32601`。
- 每个服务器在每个节点上只有一条共用连接：有状态的服务器会把所有中转客户端看成同一个。
- 不处理到上游服务器的 OAuth；请把令牌填进 `secret_headers`。
