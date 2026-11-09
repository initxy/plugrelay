# 管理 API

[English](api.md) | 简体中文

管理 API 在 `/api/v1` 下，由 `internal/admin` 提供，定义在 [`api/openapi.yaml`](../api/openapi.yaml)（OpenAPI 3.1；`TestOpenAPICoverage` 保证两边一致）。控制台只用这套 API 和收费功能的[插件路由](sell-kit.zh-CN.md#plugin-api)，所以控制台能做的，脚本都能做。

给客户端用的路由（模型 API、批量任务、WebSocket、原生直通、`/mcp`）见[接入客户端](clients.zh-CN.md#what-the-relay-serves)。`/mcp` 背后的 MCP 服务器在这里管理（`/api/v1/mcp/servers`）。

其他请求在请求日志里怎么记：

| 请求 | 日志里的样子 |
|-|-|
| MCP 工具调用 | 协议 `mcp`，模型 `<server>__<tool>`，上游 `mcp:<server>` |
| 内容安全判定 | 记在尝试的诊断备注里（[插件](plugins.zh-CN.md#guardrails)） |
| 原生请求、WebSocket | 见[原生直通](clients.zh-CN.md#native-passthrough)、[WebSocket](clients.zh-CN.md#realtime-and-websocket) |
| Batch API | 协议 `batch`；第一次读取某批结果的那个请求带上这批的用量和扣费（批量折扣记在第二次尝试的 `charge.batch_discount`） |
| 媒体类 | 计费单位记在 `usage`：`images`（附 `image_size`、`image_quality`）、`audio_ms`、`chars`、`search_units` |

按单位计的价格写在 `prices.units`，批量折扣写在 `prices.batch_discount`（[配置](configuration.zh-CN.md#prices)）。

<a id="request-log-analytics"></a>
## 请求日志分析

- 每条日志记下发请求的客户端程序（`client.id`、`name`、`version`）：先看 User-Agent，再看 `X-Title`/`HTTP-Referer`、`Originator` 这类来源头，再看 SDK 的请求头，都对不上记 `other`。识别规则是 `pkg/clientid` 里的一张表（`GET /api/v1/clients`）。按小时的汇总里也存了客户端，所以 `/usage/timeseries?group_by=client` 和 `/usage/top?by=client` 不用读日志明细。key 的 `settings.clients` 和分组的 `allowed_clients` 用的是同一套 id。
- `GET /api/v1/logs/sessions` 接受 `/logs` 的筛选条件，按 `session_id` 分组（默认查最近 30 天）。日志条目的 `session` 是整个会话的汇总（轮次、请求数、扣费）。
- `GET /api/v1/usage/failures` 按最终原因统计失败请求，一个上游都没试的记 `not_sent`。`/usage/timeseries?bucket=hour` 给“星期 × 小时”分布图用，按查看者的时区算。

<a id="request-log-export"></a>
### 导出请求日志

`GET /api/v1/logs/export` 接受 `/logs` 的筛选条件，流式输出 CSV，新的在前，最多 100,000 行，UTF-8 带 BOM。`lang=zh-CN`（或 `Accept-Language` 以 `zh` 开头）时表头、失败原因和处理方式用中文，否则用英文。成员只能导出自己的请求，没有成本列。列依次为：时间（UTC，RFC 3339）、请求 id、客户端、客户端版本、来源 IP、key、用户、模型、实际服务的模型、上游、状态、失败原因、处理方式、尝试次数、输入 / 缓存读 / 缓存写 / 输出 / 推理 token 数、首 token 耗时（毫秒）、总耗时（毫秒）、扣费、上游成本（仅管理员）、币种、会话 id。

<a id="authentication"></a>
## 认证

| 调用方 | 怎么认证 | 说明 |
|-|-|-|
| 浏览器 | `POST /api/v1/auth/login`（首次运行是 `POST /api/v1/setup`）下发的 `pr_session` cookie | HttpOnly、SameSite=Lax，走 HTTPS 时加 Secure。非安全方法还要带 `X-CSRF-Token`，值等于 `pr_csrf` cookie；跨源请求一律拒绝。登录失败按地址限流（IPv6 按 /64），多节点共用计数。 |
| 浏览器，单点登录 | 同样的 cookie，先访问 `GET /api/v1/auth/sso/{id}/start`，再由 `GET /api/v1/auth/sso/{id}/callback` 设置 | 整页跳转；出错跳回 `/login?sso_error=<code>`。`GET /api/v1/auth/providers` 列出登录页提供的方式。配置见[部署](deployment.zh-CN.md#single-sign-on)。 |
| 浏览器，第二步验证 | `POST /api/v1/auth/login` 返回 202 和可用的验证方式，只设 `pr_mfa`；`POST /api/v1/auth/mfa`（TOTP、恢复码或通行密钥）设置会话。只用通行密钥登录：`POST /api/v1/auth/passkey/options` + `POST /api/v1/auth/passkey`（按地址限流，429 `rate_limited`） | WebAuthn 的二进制字段用 base64url，结构同 `PublicKeyCredential.toJSON()`。策略见[部署](deployment.zh-CN.md#two-step-sign-in-passkeys-and-captcha)。 |
| 脚本 / 运维 | `Authorization: Bearer <admin token>` | 环境变量 `PLUGRELAY_ADMIN_TOKEN`，或站点设置里创建的令牌（只存哈希）。不做 CSRF 检查。平台 key（`pr_…`）不能用在这里。 |
| 本机所有者 | 不用认证 | 要同时满足：`security.local_owner_login` 开着（默认开；安装向导选“团队用”或“对外卖额度”会关掉）、中转只监听回环地址、请求来自回环地址且 `Host` 是回环主机名、不带转发头。身份等同第一个管理员（`plugrelay init` 不带 `--admin-email` 时建的就是这个账号）；`GET /api/v1/me` 返回 `auth: none`。 |

`POST /api/v1/auth/login` 的登录错误：`security.password_login` 关闭（且提供了单点登录）时返回 403 `password_login_disabled`；某个地址失败次数达到 `security.login_captcha_after` 后返回 403 `captcha_required` / `captcha_failed`（要带 `captcha_token`）。

<a id="roles"></a>
## 角色

`admin` 管理中转；`member` 是团队成员，在共享上游上用自己的 key；`user` 是收费站点的终端用户。每个接口要么公开，要么只在安装期可用（还没有管理员时），要么任何已登录角色都能用（由处理函数决定非管理员能看到什么），要么只有管理员能用（`internal/admin/auth.go`）。所有改动都写进审计日志（`GET /api/v1/audit`）。停用的用户，他的平台 key 也跟着失效，直到重新启用。

下面这些操作要求这个会话五分钟内登录过或调用过 `POST /api/v1/me/reauth`，否则返回 403 `reauth_required`（运维令牌不受限）：`/api/v1/me/` 下的敏感改动（关闭身份验证器 App、重新生成恢复码、通行密钥）；创建管理员（`POST /api/v1/users` 且 `role: admin`）；改用户的密码或角色、停用用户、重新启用已停用的管理员（`PATCH /api/v1/users/{id}`，包括自己）；`DELETE /api/v1/users/{id}`；`DELETE /api/v1/users/{id}/mfa`。

管理员过了两步登录的宽限期还没设置，仅管理员可用的接口对他返回 403 `mfa_enrollment_required`，其他接口（包括插件路由）把他当成员。

<a id="conventions"></a>
## 约定

- 金额是带币种的整数微单位（`{"micro": 197000, "currency": "CNY"}` 即 ¥0.197）；token 价格按每百万 token 计。没有倍率或点数：分组的 `price_multiplier` 只有管理员看得到，并且已经算进调用方看到的每个价格和扣费。例外是 `unit: money` 的设置项，用的是货币单位。
- 错误响应是 `Error` 结构；校验错误会列出出错的字段。
- 不设上限的列表（请求日志、审计日志）用游标分页：把 `next_cursor` 原样作为 `cursor` 传回。
- 凭据密钥、SSO 客户端密钥和 MCP 的密文请求头只能写不能读。上游和凭据的状态从路由实时读取。
- 测试操作（`POST /api/v1/upstreams/{id}/test`、`/keys/{id}/test`、`/mcp/servers/{id}/test`）都返回 200 和 `{ok, results}`：每个凭据一条结果，key 或服务器只有一条。
- `/api/v1/features` 是各功能的开关和全局设置表单；`/api/v1/extensions` 是功能之外的插件；分组、用户或 key 这一层的设置在 `/api/v1/{groups,users,keys}/{id}/settings`。标签和帮助文字有中英两种。`GET /api/v1/me` 列出已开启的功能和调用方能看到的控制台页面。
- 已有预充值余额时，`PATCH /api/v1/settings` 修改 `site.currency` 返回 409 `balances_exist`，除非带上 `confirm_currency_change: true`。
- 概览里的 `balance` 只在 `billing` 功能开着时出现。

插件自己的 API 挂在 `/api/plugins/<id>/` 下，每条路由要什么角色由插件声明（[插件](plugins.zh-CN.md#built-in-go-plugins)）。编译进来的插件可以把路由标成公开（`plugin.AdminRoute.Public`）：不用登录，也收不到 cookie 和身份请求头，由插件自己认证（收费功能的支付回调和注册就是这样），按路由和地址限流。标了 `Reauth` 的路由要求近期重新验证过身份（否则 403 `reauth_required`）。
