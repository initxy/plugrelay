# 通知

[English](notifications.md) | 简体中文

出了需要运维处理的事，中转会通过邮件、飞书/Lark、钉钉、企业微信群机器人，或者你自己的 webhook 发消息；webhook 还能接收每一条已完成请求的记录。`notifications` 功能开着时才工作（[配置](configuration.zh-CN.md#features)）。通知渠道和发送记录在控制台的“通知”页面，接口在 `/api/v1/notifications` 下（[管理 API](api.zh-CN.md)）。代码是 `job.notify` 和 `sink.webhook` 两个插件，装配在 `internal/app/notify.go`。

- [事件](#events)
- [什么时候发消息](#when-a-message-is-sent)
- [渠道](#channels)
- [Webhook 请求体与签名](#webhook-bodies-and-signatures)
- [请求记录](#request-records)
- [内网地址](#addresses-inside-your-network)

<a id="events"></a>
## 事件

| 事件 | 什么时候 |
|-|-|
| `credential_quarantined` | 凭据被永久下线：疑似封号、原因不明的拒绝、手动停用 |
| `credential_cooling` | 凭据暂停至少 `cooling_minutes`（30）分钟，或者额度用完 |
| `credential_quota_low` | 上游自己报告的剩余额度不高于 `quota_low_percent`（10 %） |
| `credential_secret_unsaved` | 刷新后的密钥没存下来（同时计入 `plugrelay_credential_secret_save_failures_total`） |
| `upstream_paused` / `upstream_recovered` | 上游熔断 / 恢复 |
| `budget_reached` | key、用户或分组的花费或 token 预算用到 `budget_percent`（80 %），用完时再发一次 |
| `extension_crashed` | 外部插件进程意外退出，正在重启 |

这些阈值都是这个功能的设置项。消息里会写明涉及的上游、凭据、key、用户或分组；设置了站点公网地址时，还附上它在控制台的页面链接。

<a id="when-a-message-is-sent"></a>
## 什么时候发消息

- `job.notify` 每 30 秒跑一次，同一时刻只在一个节点上跑。它拿当前的问题和上次运行时的对比（记录存在插件 KV 里，换节点也能接着跑），只通知有变化的部分。预算和外部插件的通知可能来自任何节点，会在整个集群范围去重。
- 同一对象的同一事件，每 `repeat_hours`（6）小时最多发一次；预算阈值每个预算周期发一次。
- 一次运行里同一事件的新情况合成一条消息（列出十个对象，其余只给数量），所以二十把凭据同时冷却也只发一条。
- 每个渠道只收它订阅的事件（一个都没选表示全收）。发送失败会重试两次（间隔 1 秒、3 秒），接收方明确拒收的不重试。发送记录保留最近 500 条消息和每个渠道的结果。
- 设置了 `PLUGRELAY_TEST_MODE` 时，这两个插件不会启动。

<a id="channels"></a>
## 渠道

| 类型 | 字段 | 说明 |
|-|-|-|
| `email` | SMTP 主机、端口、`tls`（默认 `starttls`，可选 `tls`、`none`）、用户名、密码、发件人、收件人 | 端口填 0 时按 `tls` 取 587 / 465 / 25；填了密码就必须用 TLS，除非服务器在本机 |
| `feishu` | 机器人地址，可选签名密钥 | 飞书和 Lark 的自定义机器人；`timestamp` + `sign` 放在请求体里 |
| `dingtalk` | 机器人地址，可选签名密钥 | `timestamp` + `sign` 放在 URL 里 |
| `wecom` | 机器人地址 | key 就在地址里 |
| `webhook` | 地址，可选密钥，请求记录的开关和字段 | 见下文 |

每个渠道都能设消息语言（`zh-CN` 或 `en`）、开关，还有一个测试按钮（测试消息也会记进发送记录）。地址（里面带着机器人的令牌）、密码和密钥用主密钥加密保存，不会返回；API 只显示打码后的地址。

<a id="webhook-bodies-and-signatures"></a>
## Webhook 请求体与签名

每次推送都是带 JSON 请求体的 `POST`，带这几个请求头：

- `X-Plugrelay-Event`：`notification` 或 `records`
- `X-Plugrelay-Timestamp`：Unix 秒数
- `X-Plugrelay-Signature`：`sha256=` 加十六进制 HMAC-SHA256，用渠道密钥对 `<timestamp>.<raw body>` 签名（设了密钥才有）

通知的请求体包含 `type: "notification"`、`title`、`lines`、`text`（标题和各行拼在一起，和聊天机器人收到的一样）、`link`（没有公网地址时为空）和 `at`。时间戳和你的时钟差太远就拒收，防重放。

<a id="request-records"></a>
## 请求记录

打开请求记录后，webhook 渠道还会收到已完成的请求：`{"type":"records","records":[…]}`，每次最多 100 条，至少每 2 秒推一次。字段就是请求日志的字段（`plugin.Record`），去掉 `body`；每个渠道可以少选几项，也可以加上 `body`（请求日志保存了正文才有）。

只保证尽量送到：每个节点最多排队 2048 条，满了就丢新来的，重试两次仍失败的推送也丢掉。没有补发，别拿它记账；正式记录以请求日志和 `GET /api/v1/logs` 为准。

<a id="addresses-inside-your-network"></a>
## 内网地址

所有发送都走核心出口并经过 SSRF 防护，回环地址和私网地址一律拒绝，SMTP 也一样。要放行内网的 webhook 或邮件服务器，把它的网段加进 `egress.allow_private_cidrs`（[配置](configuration.zh-CN.md#egress)），例如 `PLUGRELAY_EGRESS_ALLOW_PRIVATE_CIDRS=10.20.0.0/16`。
