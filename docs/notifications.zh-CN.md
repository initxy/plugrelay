# 通知

[English](notifications.md) | 简体中文

出了需要运维处理的事，中转站会通过邮件、飞书/Lark、钉钉、企业微信群机器人，或你自己的 webhook 告诉你；webhook 还可以接收每一条已完成请求的记录。`notifications` 功能开着时才运行（[配置](configuration.zh-CN.md#features)）；通知渠道和发送记录在控制台的“通知”页面，以及 `/api/v1/notifications` 下（[api.md](api.zh-CN.md)）。代码：`job.notify` 和 `sink.webhook` 两个插件（见包文档），接线在 `internal/app/notify.go`。

- [事件](#events)
- [什么时候发消息](#when-a-message-is-sent)
- [渠道](#channels)
- [Webhook 请求体与签名](#webhook-bodies-and-signatures)
- [请求记录](#request-records)
- [内网地址](#addresses-inside-your-network)

<a id="events"></a>
## 事件

| 事件 | 什么时候 | 来源 |
|-|-|-|
| `credential_quarantined` | 凭据被永久下线：疑似封号、原因不明的拒绝、手动挂起 | 路由的实时状态 |
| `credential_cooling` | 凭据暂停至少 `cooling_minutes`（30）分钟，或因额度用完而暂停 | 路由的实时状态 |
| `credential_quota_low` | 上游自己报告的剩余额度不高于 `quota_low_percent`（10 %） | 额度探测的读数 |
| `credential_secret_unsaved` | 刷新后的密钥没能保存下来（同时计入 `plugrelay_credential_secret_save_failures_total`） | 路由的实时状态 |
| `upstream_paused` / `upstream_recovered` | 上游熔断器断开 / 重新闭合 | 路由的实时状态 |
| `budget_reached` | 密钥、用户或分组的花费或 token 预算用到 `budget_percent`（80 %），用完时再发一次 | `meter.quota` 每次预占额度时（`plugin.NoticeHost`） |
| `extension_crashed` | 某个外部插件进程意外退出，正在重启 | 扩展进程管理器 |

这些阈值都是这个功能的设置项。消息里会写明涉及的对象（上游 / 凭据、密钥、用户、分组），设置了站点公网地址时还会附上它们在控制台的页面链接；没设置就不带链接。

<a id="when-a-message-is-sent"></a>
## 什么时候发消息

- `job.notify` 每 30 秒运行一次，同一时刻只在一个节点上跑（靠核心的任务锁）。它把当前有问题的东西和上次运行时记下的（存在它的插件 KV 里）做对比，所以换个节点也能接着上一个节点的进度；只通知发生变化的部分。
- 预算和扩展的通知在任何节点上都可能产生；重复的先在本节点去掉，再在整个集群范围去掉（靠共享状态），剩下的存进存储，等下次运行时发出。
- 同一对象的同一事件，每 `repeat_hours`（6）小时最多发一次；预算阈值每个预算周期发一次。
- 一次运行里同一事件的所有新情况合成一条消息，列出涉及的对象（显示十个，其余只给数量），所以二十个凭据同时进入冷却也只发一条。
- 每个渠道只收它订阅的事件（一个都没选表示全部）。发送失败会重试两次（间隔 1 秒、3 秒），除非接收方明确拒收；之后这条消息在这个渠道上就算未送达。发送记录保留最近 500 条消息以及每个渠道的结果。
- 设置了 `PLUGRELAY_TEST_MODE` 时，这个功能的插件一律不启动（硬性规则 22）。

<a id="channels"></a>
## 渠道

| 类型 | 字段 | 说明 |
|-|-|-|
| `email` | SMTP 主机、端口、`tls`（默认 `starttls`，可选 `tls`、`none`）、用户名、密码、发件人、收件人 | 端口填 0 时按 `tls` 取 587 / 465 / 25；配了密码就必须用 TLS，除非服务器在本机 |
| `feishu` | 机器人地址，可选签名密钥 | 飞书和 Lark 的自定义机器人；签名以 `timestamp` + `sign` 放在请求体里 |
| `dingtalk` | 机器人地址，可选签名密钥 | 签名以 `timestamp` + `sign` 放在 URL 里 |
| `wecom` | 机器人地址 | key 就在地址里 |
| `webhook` | 地址，可选密钥，请求记录开关及字段 | 见下文 |

每个渠道都有消息语言（`zh-CN` 或 `en`）、启用开关，以及一个测试按钮，点了会发一条测试消息（也会记进发送记录）。地址（里面带着机器人的令牌）、密码和密钥都用主密钥加密保存，永不返回；API 只显示打码后的地址。

<a id="webhook-bodies-and-signatures"></a>
## Webhook 请求体与签名

每次推送都是带 JSON 请求体的 `POST`，并带上这些请求头：

- `X-Plugrelay-Event`：`notification` 或 `records`
- `X-Plugrelay-Timestamp`：Unix 秒数
- `X-Plugrelay-Signature`：`sha256=` 加上十六进制的 HMAC-SHA256，密钥是这个渠道的密钥，签名内容是 `<timestamp>.<raw body>`（只有设置了密钥时才有）

通知的请求体包含 `type: "notification"`、`title`、`lines`、`text`（前两者拼在一起，和聊天机器人收到的一样）、`link`（没有公网地址时为空）和 `at`。时间戳和你的时钟相差太远就拒收，以防重放。

<a id="request-records"></a>
## 请求记录

打开请求记录后，webhook 渠道还会收到已完成的请求：`{"type":"records","records":[…]}`，每次推送最多 100 条，至少每 2 秒推一次。投递尽力而为：每个节点上记录在一个长度 2048 的队列里排队；队列满了就丢弃新来的，重试两次仍失败的推送也会丢弃。没有补发，所以别拿它当流水账（请求日志和 `GET /api/v1/logs` 才是正式记录）。字段就是请求日志的字段（`plugin.Record`），去掉 `body`；每个渠道可以只选其中一部分，或者加上 `body`（只有请求日志保存了正文时才有正文）。

<a id="addresses-inside-your-network"></a>
## 内网地址

所有发送都走核心出口，并经过 SSRF 防护：回环地址和私网地址一律拒绝，SMTP 也一样。要放行内网的 webhook 或邮件服务器，把它所在的网段写进 `egress.allow_private_cidrs`（[配置](configuration.zh-CN.md#egress)），例如 `PLUGRELAY_EGRESS_ALLOW_PRIVATE_CIDRS=10.20.0.0/16`。
