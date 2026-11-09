# 管理 API

[English](api.md) | 简体中文

管理 API 的定义在 [`api/openapi.yaml`](../api/openapi.yaml)（OpenAPI 3.1），由 `internal/admin` 在 `/api/v1` 下提供。自带的控制台用的就是它，计费相关页面还会用到售卖套件的插件路由（[sell-kit.md](sell-kit.zh-CN.md#plugin-api)）——没有只给控制台用的接口——所以别的前端或脚本能做控制台能做的一切。`TestOpenAPICoverage`（`internal/admin/openapi_test.go`）保证服务端和定义文件保持一致。

面向客户端的路由——模型 API、批处理、WebSocket 会话、原生直通和 MCP 网关——不在其中，见 [clients.md](clients.zh-CN.md#what-the-relay-serves)。`/mcp` 背后的服务器在这里管理（`/api/v1/mcp/servers`，外加 `/test`）。

各类请求在请求日志里长什么样：MCP 工具调用的协议是 `mcp`，模型是 `<server>__<tool>`，上游是 `mcp:<server>`；内容安全规则的判定结果记作尝试的诊断备注（[插件](plugins.zh-CN.md#guardrails)）；原生请求和 WebSocket 会话见 [clients.md](clients.zh-CN.md#native-passthrough) 和 [clients.md](clients.zh-CN.md#realtime-and-websocket)。Batch API 请求的协议是 `batch`；第一个读取某个批次结果的请求带上这一批的用量、成本和扣费（记在它的第二次尝试上，批处理折扣写在 `charge.batch_discount`）。媒体类请求把计费单位记在 `usage` 里：`images`（附 `image_size`、`image_quality`）、`audio_ms`、`chars` 和 `search_units`。价格规则的单位价格放在 `prices.units`，批处理折扣放在 `prices.batch_discount`（[配置](configuration.zh-CN.md#prices)）。

<a id="request-log-analytics"></a>
## 请求日志分析

- 每条日志都记录发起请求的客户端程序（`client.id`、`name`、`version`），从请求头识别：先看 User-Agent，再看 `X-Title`/`HTTP-Referer` 或 `Originator` 这类来源标识头，再看 SDK 的请求头；都对不上就记为 `other`。识别规则是 `pkg/clientid` 里的一张表（`GET /api/v1/clients` 可以列出）；客户端信息存在日志条目和按小时的汇总里，所以 `/usage/timeseries?group_by=client` 和 `/usage/top?by=client` 不用读日志明细。密钥允许的客户端（`settings.clients`）和分组的 `allowed_clients` 用的也是这些 id，按同一套规则匹配。
- `GET /api/v1/logs/sessions` 接受 `/logs` 的筛选条件，把匹配的条目按 `session_id` 分组（没有会话 id 的条目不算在内）；`from` 和 `to` 都没给时，查最近 30 天。日志条目的 `session` 是它所在整个会话的汇总（轮次、请求数、扣费）。
- `GET /api/v1/usage/failures` 按最终原因统计失败请求（各种尝试失败原因，外加 `not_sent`，表示一个上游都没试）。`/usage/timeseries?bucket=hour` 给出按小时的数据点，用来按查看者所在时区画“星期 × 小时”的分布图。

<a id="request-log-export"></a>
### 导出请求日志

`GET /api/v1/logs/export` 接受 `/logs` 的筛选条件，以流式方式输出 CSV，最新的在前，最多 100,000 行（要更多就缩小时间范围），编码 UTF-8 带 BOM。`lang=zh-CN`（或 `Accept-Language` 以 `zh` 开头）时表头用中文，否则用英文；失败原因和处理方式也用同一种语言写。成员只能导出自己的请求，且没有成本列。各列依次为：时间（UTC，RFC 3339）、请求 id、客户端、客户端版本、来源 IP、密钥、用户、模型、实际提供服务的模型、上游、状态、失败原因、处理方式、尝试次数、输入 / 缓存读 / 缓存写 / 输出 / 推理 token 数、首个 token 耗时（毫秒）、总耗时（毫秒）、扣费、上游成本（仅管理员）、币种（金额以货币单位计）、会话 id。

<a id="authentication"></a>
## 认证

| 调用方 | 怎么认证 | 说明 |
|-|-|-|
| 浏览器 | `POST /api/v1/auth/login`（首次运行时是 `POST /api/v1/setup`）下发的 `pr_session` cookie | HttpOnly、SameSite=Lax，走 HTTPS 时加 Secure。非安全方法还必须带 `X-CSRF-Token`，值等于 `pr_csrf` cookie（双重提交）；跨源的浏览器请求一律拒绝。登录失败按客户端地址限流（IPv6 按 /64），计数通过共享状态后端在各节点间共享。 |
| 浏览器，单点登录 | 同样的 cookie，先访问 `GET /api/v1/auth/sso/{id}/start`，再由 `GET /api/v1/auth/sso/{id}/callback` 设置 | 整页跳转，不是 fetch；出错时跳回 `/login?sso_error=<code>`。`GET /api/v1/auth/providers` 列出登录页提供的登录方式。登录方式的配置与策略：[部署](deployment.zh-CN.md#single-sign-on)。 |
| 浏览器，第二步验证 | `POST /api/v1/auth/login` 返回 202 和可用的验证方式，只设置 `pr_mfa` cookie；`POST /api/v1/auth/mfa`（TOTP、恢复码或通行密钥）设置会话。`POST /api/v1/auth/passkey/options` + `POST /api/v1/auth/passkey` 只用通行密钥登录（登录挑战按地址限流：429 `rate_limited`） | WebAuthn 的二进制字段用 base64url 编码，结构和 `PublicKeyCredential.toJSON()` 一致。策略与设置：[部署](deployment.zh-CN.md#two-step-sign-in-passkeys-and-captcha)。 |
| 脚本 / 运维 | `Authorization: Bearer <admin token>` | 环境变量 `PLUGRELAY_ADMIN_TOKEN`，或在站点设置里创建的令牌（只存哈希）。不受 CSRF 检查约束。这里不接受平台密钥（`pr_…`）。 |
| 本机所有者 | 什么都不用 | 只在以下条件同时满足时生效：站点设置 `security.local_owner_login` 开着（默认开；安装向导选“团队”和“售卖”时会关掉），中转站只监听回环地址，请求来自回环地址、`Host` 是回环主机名、且不带任何转发头。身份等同第一个管理员；`plugrelay init` 不带 `--admin-email` 时创建的就是这个所有者账号。`GET /api/v1/me` 返回 `auth: none`。 |

<a id="roles"></a>
## 角色

`admin` 管理中转站；`member` 是团队成员，在共享上游上用自己的密钥；`user` 是售卖站点的终端用户。接口分为：公开的（安装状态、登录）、仅安装期可用的（还没有管理员时）、任何已登录角色可用的（由处理函数限定非管理员能看到什么），以及仅管理员可用的（`internal/admin/auth.go`）。每次改动都写入审计日志（`GET /api/v1/audit`）。停用一个用户后，他的平台密钥也无法通过认证，直到重新启用。

<a id="conventions"></a>
## 约定

- 金额是带币种的整数微单位（`{"micro": 197000, "currency": "CNY"}` 即 ¥0.197）；token 价格按每百万 token 计。没有倍率或点数：分组的 `price_multiplier` 只有管理员能看到，并且已经算进调用方看到的每个价格和扣费里。设置项是唯一的例外：`unit: money` 的设置（功能、扩展、分组、用户或密钥层）用货币单位，所以任何表单都不会显示微单位。
- 错误响应带 `Error` 请求体；校验错误会列出出错的字段。
- 不设上限的列表（请求日志、审计日志）用不透明的时间游标分页：把 `next_cursor` 原样作为 `cursor` 传回。
- 凭据密钥、SSO 客户端密钥和 MCP 服务器的密文请求头只能写不能读；上游和凭据的状态从路由实时读取。
- 所有测试操作（`POST /api/v1/upstreams/{id}/test`、`/keys/{id}/test`、`/mcp/servers/{id}/test`）都返回 200 和 `{ok, results}`：每个凭据一条结果，密钥或服务器则只有一条；全部通过时 `ok` 为真。
- 功能（`/api/v1/features`）是各项开关和全局设置表单；扩展（`/api/v1/extensions`）是功能之外的插件；分组、用户或密钥层的设置在 `/api/v1/{groups,users,keys}/{id}/settings`。标签、帮助文字和枚举名称都有中英两种；`GET /api/v1/me` 列出已开启的功能和调用方能看到的控制台页面 id。
- 密码登录关闭时（站点设置 `security.password_login`，且提供了单点登录方式），`POST /api/v1/auth/login` 返回 403 `password_login_disabled`；某个地址失败次数达到 `security.login_captcha_after` 后，返回 403 `captcha_required` / `captcha_failed`（需要带 `captcha_token`）。
- `/api/v1/me/` 下的敏感改动（关闭身份验证器 App、重新生成恢复码、通行密钥），以及管理员创建管理员（`POST /api/v1/users` 且 `role: admin`）、修改任意用户的密码或角色、停用用户或重新启用已停用的管理员（`PATCH /api/v1/users/{id}`，包括自己的账号）、删除用户（`DELETE /api/v1/users/{id}`）或重置其两步登录（`DELETE /api/v1/users/{id}/mfa`），除非这个会话在五分钟内登录过或调用过 `POST /api/v1/me/reauth`，否则返回 403 `reauth_required`（运维令牌直接放行）。管理员超过两步登录的宽限期后，仅管理员可用的接口对他返回 403 `mfa_enrollment_required`；在所有用户都能用的接口上，这个管理员按成员对待（只能看自己的密钥和日志）；插件路由（`/api/plugins/{id}/…`）也把他当成员。
- 已有预付余额时，`PATCH /api/v1/settings` 修改 `site.currency` 会返回 409 `balances_exist`，除非带上 `confirm_currency_change: true`。

插件自己的 API 挂在 `/api/plugins/<id>/` 下，每条路由需要什么角色由它自己声明（[plugins.md](plugins.zh-CN.md#built-in-go-plugins)）。编译进来的插件可以把路由标为公开（`plugin.AdminRoute.Public`）：这种路由不需要登录，也收不到 cookie 和身份请求头，由插件自己认证请求——售卖套件的支付回调和注册就是这样（[sell-kit.md](sell-kit.zh-CN.md#plugin-api)）；公开调用按路由和客户端地址限流。标了 `Reauth` 的路由要求控制台会话最近重新验证过身份（否则 403 `reauth_required`）。概览里的 `balance` 只在 `billing` 功能开着时出现。
