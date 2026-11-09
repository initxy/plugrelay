# 收费功能

[English](sell-kit.md) | 简体中文

想把 plugrelay 当收费服务来运营，可以打开这组可选插件：预充值余额、兑换码、在线支付（Stripe；支付宝和微信支付走易支付网关）、优惠券、订阅套餐、签到和邀请奖励、公告、自助注册，以及从 new-api / one-api 导入数据。每个版本都自带这些插件，默认全关。金额都是站点币种（`site.currency`）的真钱，按整数微单位存；没有积分，也没有倍率。

- [启用](#enable)
- [预充值余额：`meter.balance`](#prepaid-balance-meterbalance)
- [兑换码：`admin.redeem`](#redeem-codes-adminredeem)
- [在线支付：`admin.payment`](#payments-adminpayment)
- [订阅套餐：`admin.subscriptions`](#subscription-plans-adminsubscriptions)
- [优惠券、签到、邀请、公告](#coupons-check-in-referrals-announcements)
- [自助注册：`admin.signup`](#sign-up-adminsignup)
- [从 new-api 导入：`admin.import-newapi`](#import-from-new-api-adminimport-newapi)
- [控制台](#console)
- [插件 API](#plugin-api)

<a id="enable"></a>
## 启用

分成两个功能开关（[配置](configuration.zh-CN.md#features)）：`billing`（`meter.balance`、`admin.payment`、`admin.redeem`、`admin.subscriptions`、`admin.rewards`、`admin.announcements`、`admin.import-newapi`）和 `signup`（`admin.signup`）。安装向导里选“对外卖额度”会打开 `billing`；也可以在控制台的功能页打开，或者用 `plugrelay apply`：

```yaml
features:
  - {id: billing, enabled: true}
  - {id: signup, enabled: true}
```

设置在各功能的表单里填。Stripe 和易支付的密钥、邀请码用主密钥加密保存。代码：`plugins/builtin/builtin.go`、`internal/app/sellkit*.go`、`internal/app/operations.go`。

<a id="prepaid-balance-meterbalance"></a>
## 预充值余额：`meter.balance`

| `payer` | 扣谁的钱 |
|-|-|
| `user`（默认） | key 所属用户；没有用户的 key 扣它自己的账户（`key:<id>`） |
| `group` | key 所在分组（全组共用一份余额），没有分组就扣所属用户 |
| `key` | key 自己 |

充值、兑换和控制台显示的余额都落在这个账户上。账户取决于用哪把 key 时（`payer=key`，或者 `payer=group` 而这把 key 不在用户自己的分组里），控制台会让用户选 key；API 要带 `key_id`，不带返回 `400 key_required`（可选项用 `GET meter.balance/me/payers` 查）。

每个请求先**预扣**：输入估算加输出上限，按调用方的费率算，至少 `min_reserve_micro`；可用余额加信用额度不够就拒绝。成功后按核心算出的金额**结算**，记一笔 `charge:<request id>`；失败就**退回**预扣。同一个请求不会扣两次。结算失败会重试，还不行就由后台定时检查（每个节点每分钟最多一次）按请求日志补上，所以 `meter.balance` 依赖 `sink.requestlog`。超过 `hold_ttl` 还没有日志记录的预扣（节点在请求中途挂了）会被释放。

最终扣费不打折扣：请求输出超出估算（客户端没传输出上限）时，余额可能被扣成负数一次，下一个请求就会被拒。调高 `min_reserve_micro` 能缩小这个口子；想允许透支，就给账户设信用额度。同时开着 `meter.quota` 时先查预算再扣余额。[原生直通](adr/0007-native-passthrough-model.md)不跑插件，也就不扣余额，只开给不按余额收费的 key。

[批量任务](clients.zh-CN.md#batches)创建时按批量价预扣估算（请求体每三个字节算一个 token，加上每个请求的输出上限，没写按 4096；OpenAI 的输入文件在上传时估算），结果第一次被读取时向创建者一次性全额扣费，扣成负数也照扣。最后没有可计费内容的批次会退回预扣。批次的预扣比 `hold_ttl` 多留三天。

余额不足时，客户端收到它所用协议自己的“余额不足”错误，不会触发重试（协议支持的带 `x-should-retry: false`），消息里写明可用多少、需要多少：

| 客户端协议 | 状态码 | 响应体 |
|-|-|-|
| Anthropic | 402 | `billing_error` |
| OpenAI chat / responses / completions | 429 | `insufficient_quota` |
| Gemini | 429 | `RESOURCE_EXHAUSTED` |

| 设置 | 默认值 | 作用 |
|-|-|-|
| `payer` | `user` | 见上（只能全局设） |
| `exempt` | false | 不扣余额也放行；可以设在分组、用户或 key 上，给员工用 |
| `min_reserve_micro` | 0 | 每个请求至少预扣多少 |
| `balance_header` | 空 | 用这个响应头返回可用余额，例如 `x-plugrelay-balance` |
| `low_balance_micro` | 0 | 余额低于这个数才发上面的响应头；0 表示每次都发 |
| `hold_ttl` | 6h | 预扣过了这么久还没有请求日志记录就释放 |

余额流水的类型：`topup`（充值）、`redeem`（兑换）、`charge`（请求扣费）、`refund`（管理员退还某个请求的扣费，只能一次，最多退到原额）、`adjustment`（管理员加减余额，减不会超过信用额度）、`subscription`（一个套餐周期；用余额付是套餐价，在支付渠道付记 0）、`checkin`、`referral`。套餐额度的扣减是单独账户上的 `allowance` 流水。

已经有人有余额时，修改 `site.currency` 会返回 `409 balances_exist`，除非请求里带 `"confirm_currency_change": true`。余额不会换算。

<a id="redeem-codes-adminredeem"></a>
## 兑换码：`admin.redeem`

管理员按批生成兑换码：每批 1–1000 个，设金额和每个码能用几次，可选过期时间和批次名。码长这样：`RC-XXXX-XXXX-XXXX-XXXX-XXXX`，只显示一次，库里只存 SHA-256；兑换时不分大小写，短横线可有可无。同一个码每个用户只能用一次，`payer=key` 时换一把 key 也不行。兑换失败按用户限次（`failures_per_hour`，默认 10）。

<a id="payments-adminpayment"></a>
## 在线支付：`admin.payment`

用户选金额（预设金额和上下限在设置里配）和支付方式，只列出已配置且能收站点币种的渠道。渠道带签名的回调到了就给余额入账。

- 回跳地址和回调地址用 `site.public_base_url` 拼，不用请求的 `Host`。没设置时下单返回 `503 not_configured`。
- 渠道的报错写日志，用户只看到 `502 upstream`。
- 公开路由（`webhook`、`return`、注册）按每条路由、每个客户端地址每五分钟 600 次限流（`429 rate_limited`）。前面有反向代理就把它写进 `trusted_proxies`（[部署](deployment.zh-CN.md)），否则所有人共用代理的限额，有人刷一波就能把支付通知堵住。
- 每个订单只入账一次（流水 `topup:<order>`）；没能入账的通知照样回复已收到，之后再来的通知仍然可以入账。

<a id="stripe"></a>
### Stripe

1. 在 Stripe 后台添加 webhook 端点 `https://<relay>/api/plugins/admin.payment/webhook/stripe`，订阅 `checkout.session.completed` 和 `checkout.session.async_payment_succeeded`。
2. 设置 `stripe_secret_key`（`sk_…`）和 `stripe_webhook_secret`（`whsec_…`）。

Checkout 按该币种的[最小单位](https://docs.stripe.com/currencies)收款，Stripe 收不了的金额或币种直接拒绝；自动计税和促销码都关掉了。事件用 `Stripe-Signature` 校验；金额或币种和订单对不上的只写日志，不入账。

<a id="epay-易支付-alipay-wechat-pay"></a>
### 易支付 epay（支付宝、微信支付）

适用于走易支付商户协议的网关（彩虹易支付及各种衍生版；[协议文档](https://pay.yzfpay.com/doc/v1_legacy_api.html)）。设置 `epay_url`（网关根地址，`submit.php` 和 `mapi.php` 在它下面）、`epay_pid`、`epay_key`，以及 `epay_types`（默认 `alipay`、`wxpay`）。网关那边不用登记什么：每个订单自带 `notify_url`（`<public URL>/api/plugins/admin.payment/webhook/epay`）和 `return_url`（`…/return/epay`）。

- `epay_mode: submit`（默认）把用户跳到 `submit.php`；`mapi` 由服务端调 `mapi.php` 下单，再把用户跳到返回的 `payurl`（没有就用 `qrcode` 或 `urlscheme`）。商品名是 `epay_product_name`。
- 异步通知（GET 或 POST 表单）要 MD5 签名对、`pid` 是我们的、`trade_status` 为 `TRADE_SUCCESS`、`money` 等于订单金额。重复通知和对不上的通知也回 `success`（只写日志不入账），签名不对回 `fail`（400）。值里带 `&` 的直接拒绝。
- 用户带着签名参数回跳时，校验通过同样入账（网关连不上中转时有用），然后跳到钱包页。
- 只在 `site.currency` 为 `CNY` 时提供；金额必须是整分。
- 易支付没有退款接口。先在网关的商户后台退款，再在中转里手动扣回：充值就做一笔负数 `adjustment`；套餐就点“立即结束”（被标记的再点“标记为已退款”）。

<a id="other-providers"></a>
### 其他支付渠道

实现 `payment.Provider`，在 `plugins/builtin/builtin.go` 引入的包里用 `payment.RegisterProvider` 注册，照着 `plugins/admin/payment/stripe` 或 `…/epay` 写。回调地址是 `/api/plugins/admin.payment/webhook/<name>`。

<a id="subscription-plans-adminsubscriptions"></a>
## 订阅套餐：`admin.subscriptions`

套餐包括：每周、每月或每年的价格；每个周期的额度，按金额或按 token（输入、缓存、输出）算；以及额度用完怎么办：`block` 直接拒绝，返回和余额不足一样的错误（并写明什么时候刷新），`balance` 改从预充值余额扣。订阅时会复制一份套餐条款，所以改套餐只影响之后的订阅；停用的套餐不再接受新订阅。

套餐可以绑定一个分组。订阅期间用户（连同他放在自己分组里的 key）会移进这个分组，分组的模型白名单、限流和价格倍率随之生效（[分层设置](configuration.zh-CN.md#layered-plugin-settings)），订阅结束再移回去；期间被管理员移走的就不动。这和 `payer=group` 冲突：按分组扣钱时，带分组的套餐会被拒（`409 payer_group`）；只要还有套餐或生效中的订阅带分组，改成 `payer=group` 也会被拒（409）。如果存储里已经两者都有，`meter.balance` 的任何保存都会失败，解法是在同一次保存里改掉 `payer`、去掉套餐的分组，或者结束这些订阅。

| 付款方式 | 第一个周期 | 续费 |
|-|-|-|
| 预充值余额 | 从余额扣套餐价；不够就拒绝 | 周期结束时从余额扣；不够就结束订阅 |
| Stripe | 订阅模式的 Checkout | Stripe 每期扣款；每个 `invoice.paid` 开始下一期 |
| 易支付 | 付一次买一期 | 从余额扣（易支付没有自动续费） |

- **每人一个。** 已有生效中的订阅再订会返回 `409 already_subscribed`；没付钱的待支付订阅会被新的替掉。
- **需要退款。** 订阅开不起来（已有别的订阅在生效，或者用户已删除）但钱到了，就什么都不开，在套餐页标记为**需要退款**；Stripe 订阅会被设为取消。去支付渠道退款，再点“标记为已退款”。
- **取消**在周期结束时生效（Stripe 设 `cancel_at_period_end`）。不按比例折算：想换套餐，先取消，等这期结束再订。
- **立即结束**（管理员用，比如退款或拒付之后）马上结束，并在 Stripe 那边取消；中转不会退钱。它和“标记为已退款”都要求近期重新验证过身份。
- **删除用户**会先结束他的订阅。结束不了（Stripe 拒绝、渠道没配置、订单还在待支付）就返回 409：先点立即结束，或者用 `DELETE /api/v1/users/{id}?force=true` 只在本地结束。
- **周期**到下周、下月或明年的同一天结束，那个月没有这一天就取月底（1 月 31 日 → 2 月 28/29 日）。续费和到期在 `meter.balance` 的后台定时检查里处理，不靠打开页面触发。

**扣额度。** 有订阅的用户先用套餐额度，再用余额。预扣和余额一样，只是换成套餐的单位，最多扣到剩余量。额度用完后，`block` 拒绝，`balance` 改扣余额；`balance` 模式下，请求估算比剩余额度大时，只有余额够付差额才用套餐。超出额度的部分（包括 `block` 模式下的超支）都从余额扣，token 按这个请求自己的单价折成钱，必要时扣成负数；下次充值先还这笔欠款。token 套餐只管按 token 计价的请求：生图、语音、转写、重排序、不报 token 的请求，以及批量任务，都走余额。额度扣减记在 `sub:<subscription>` 上的 `allowance` 流水里，套餐页据此显示用了多少、还剩多少。

**Stripe。** 再添加一个 webhook 端点 `https://<relay>/api/plugins/admin.subscriptions/webhook/stripe`，订阅 `invoice.paid`、`customer.subscription.updated`、`customer.subscription.deleted`、`charge.refunded` 和 `charge.dispute.created`，把它的签名密钥填到 `admin.payment` 的 `stripe_subscription_webhook_secret`。权益跟着 `invoice.paid` 走（[Stripe 的建议](https://docs.stripe.com/billing/subscriptions/webhooks)）；金额和套餐价对不上的发票不开周期。Stripe 删除的订阅，或者状态变成 `canceled`、`unpaid`、`incomplete_expired` 的，这边也结束；周期结束三天还没续上的同样结束，直到再来一个 `invoice.paid`。订阅发票全额退款或者新出现拒付，订阅立即结束；部分退款、赢下的拒付都不改变什么。

**易支付。** 用易支付买套餐就是一个和充值一样的订单，回调地址在 `/api/plugins/admin.subscriptions/` 下；一个订单买一期。

<a id="coupons-check-in-referrals-announcements"></a>
## 优惠券、签到、邀请、公告

每项在 billing 功能的表单里各有一个开关，默认关。

- **优惠券**（`admin.payment`，`coupons_enabled`）：充值时按百分比打折（可以设最多减多少）或直接减一个固定金额，可限定生效时间、最低充值金额、总次数和每人次数。优惠金额向下取整到分；用户付折后价，余额按原价到账。没付款的订单会占着这次使用一小时，用户点了取消也一样，除非渠道能让 checkout 作废（Stripe）。下单超过一小时才付款的，按实付金额入账，不给优惠。
- **每日签到**（`admin.rewards`，`checkin_enabled`）：每人每天一次，按站点时区（`site.timezone`，默认 UTC）算，在最低和最高奖励之间随机给一个金额。当天再签返回 409 `checked_in`。
- **邀请奖励**（`admin.rewards`，`referral_enabled`）：每个成员有一个邀请码，`/signup?ref=<code>` 记下第一个邀请人。被邀请人第一次付费充值时，邀请人拿到实付金额的一个百分比或一笔固定金额，累计不超过 `referral_cap_micro`（0 表示不封顶）。`payer=key` 时没有奖励。
- **公告**（`admin.announcements`，`enabled`）：Markdown 格式，可设起止时间和受众（所有人、成员、管理员），在控制台顶部显示为可关闭的横幅。最多 200 条。

<a id="sign-up-adminsignup"></a>
## 自助注册：`admin.signup`

打开后登录页会出现“注册账号”，`/signup` 用邮箱和密码（至少 10 个字符）注册终端用户（角色 `user`），放进 `default_group`。可选项：要求填 `invite_codes` 里的邀请码（`invite_required`）、只放行 `allowed_domains` 里的邮箱域名、按地址限次（`per_ip_per_hour`，默认 5）、验证码（`captcha_provider` 选 `turnstile` 或 `hcaptcha`，再配 `captcha_site_key` 和 `captcha_secret`）。验证码出错时：没带返回 403 `captcha_required`，没通过返回 403 `captcha_failed`，服务商连不上返回 503 `captcha_unavailable`（连不上就拒绝，不放行）。

邮箱不做验证，所以 `allowed_domains` 只是过滤一下用户填的内容（要按组织限制请用单点登录）；SSO 的 `link_by_email` 也不会把身份关联到自助注册或导入的账号上：SSO 用户会收到 `account_exists`，由管理员手动关联，或者删掉抢注的账号。

<a id="import-from-new-api-adminimport-newapi"></a>
## 从 new-api 导入：`admin.import-newapi`

读取 new-api（[QuantumNous/new-api](https://github.com/QuantumNous/new-api)，表结构以 commit `2de51a19` 为准）或 one-api 的数据库，用 MySQL、PostgreSQL 或 SQLite 的 DSN 连接，也可以给一份 JSON 导出（`{"channels": [...], "tokens": [...], "users": [...], "options": {...}}`，列名和 new-api 一致）。每次都先试运行，列出会改什么、为什么。导入的记录保留 new-api 的 id（`newapi-c7`、`newapi-u3`、`newapi-t12`、`newapi-<group>`），所以重复导入不会有变化。

| new-api | plugrelay |
|-|-|
| 渠道 channel | 用匹配的清单建一个上游（OpenAI、Anthropic、Gemini、DeepSeek、Moonshot、Zhipu、Qwen、Volcengine、MiniMax、Mistral、xAI、OpenRouter、Bedrock、Ollama、vLLM、SGLang、Xinference，custom / SiliconFlow 当作 OpenAI 兼容）；base URL 不是官方地址的 OpenAI 渠道变成用那个地址的 OpenAI 兼容上游；其他类型跳过并说明原因 |
| 渠道密钥（每行一个） | 每个一把共享凭据；Bedrock 的 `AK\|SK\|region` 变成 `AK:SK` 加区域 |
| 渠道的模型、模型映射、分组、优先级、权重 | 模型及其指向上游模型的路由，限定在这些分组里 |
| 分组（以及 `GroupRatio`） | 带这个价格倍率的分组 |
| 用户 | 一个用户（所有角色，包括 root 和管理员，都变成 user：等他们设好新密码后再手动提升为管理员）；登录名用邮箱，没有就用用户名，标为未验证；已禁用的保持禁用 |
| 用户额度 quota | 一笔 `adjustment` 入账：quota ÷ `QuotaPerUnit` 美元，站点货币不是 USD 时再乘以你给的汇率 |
| 令牌 token | 这个用户的一把平台 key，保留过期时间、分组、模型限制和 IP 白名单 |

原来的 `sk-` key 导入后照常能用。bcrypt 密码能沿用；新版 new-api 的 argon2 哈希不行，这些用户会收到提示，需要重设密码。不迁移的：单个令牌的额度限制（改用 `meter.quota` 的 key 预算）、日志、充值记录、兑换码，以及没有对应清单的渠道类型（Midjourney、Suno、Azure 等）。

只有管理员能用。DSN 读不到中转自己的文件：MySQL 的 `allowAllFiles` 和 `local_infile` 一律拒绝；SQLite 文件以只读方式打开，并且要带 `"confirm_local_file": true`。

<a id="console"></a>
## 控制台

`/billing` 下的页面，billing 功能打开时显示（注册页在注册功能打开时显示）。

| 插件 | 终端用户 | 管理员 |
|-|-|-|
| `meter.balance` | 概览页和 key 页上的余额；钱包：余额和流水 | 余额：所有账户、调账、信用额度、流水 |
| `admin.payment` | 钱包：充值，可用优惠券 | 订单：所有充值和套餐付款；优惠券 |
| `admin.subscriptions` | 我的套餐：当前套餐、额度已用和剩余、续费日期、订阅、取消、额度流水 | 套餐：定义套餐、订阅列表（标出需要退款）、到期取消、立即结束、标记为已退款 |
| `admin.redeem` | 钱包：兑换码兑换 | 兑换码：批次、生成、导出、停用、谁兑换了 |
| `admin.rewards` | 钱包：签到、邀请链接和收益 | —（余额流水里能看到每一笔奖励） |
| `admin.announcements` | 每个页面上的横幅 | 公告 |
| `admin.signup` | 注册页和登录页上的链接 | — |
| `admin.import-newapi` | — | 从 new-api 导入 |

<a id="plugin-api"></a>
## 插件 API

路径都在 `/api/plugins/<id>/` 下（[管理 API](api.zh-CN.md)）。`admin` 路由要管理员角色，公开路由不用登录。

| 路由 | 谁能调 | 作用 |
|-|-|-|
| `GET meter.balance/me?key_id=` | 任何用户 | 自己的账户（或那把 key 的）：余额、可用、预扣、信用额度 |
| `GET meter.balance/me/transactions?key_id=&cursor=` | 任何用户 | 它的交易流水，最新的在前 |
| `GET meter.balance/me/payers` | 任何用户 | `{payer, items: [{subject, key_id?, key_name?}]}`：可选的账户 |
| `GET meter.balance/accounts` | 管理员 | 所有账户 |
| `GET meter.balance/transactions?subject=&kind=&cursor=` | 管理员 | 交易流水 |
| `POST meter.balance/adjust` | 管理员 | `{subject, amount_micro, note, idempotency_key}` |
| `PUT meter.balance/credit-limit` | 管理员 | `{subject, credit_limit_micro}` |
| `POST meter.balance/refund` | 管理员 | `{request_id, amount_micro?, note}` 退还一笔扣费，只能一次 |
| `POST admin.redeem/batches` | 管理员 | `{count, amount_micro, max_uses, expires_at, batch}` → 生成的码，只返回这一次 |
| `GET admin.redeem/batches`, `POST …/batches/disable` | 管理员 | 按批次汇总；停用或启用整批 |
| `GET admin.redeem/codes?batch=&uses=1` | 管理员 | 兑换码（只显示最后四个字符），以及每个码是谁兑换的 |
| `POST admin.redeem/codes/disable` | 管理员 | `{id, disabled}` |
| `POST admin.redeem/redeem` | 任何用户 | `{code, key_id?}` → 金额和新余额 |
| `GET admin.payment/config` | 任何用户 | 支付渠道、`methods: [{provider, method?}]`、货币、上下限、预设金额 |
| `POST admin.payment/checkout` | 任何用户 | `{amount_micro, provider?, method?, key_id?}` → `{order_id, url}` |
| `GET admin.payment/orders?status=&provider=&kind=&user_id=&cursor=` | 管理员 | 订单，最新的在前；第一页带最近 30 天的汇总 |
| `GET admin.payment/orders/{id}` | 订单所有者、管理员 | 订单状态 |
| `POST admin.payment/orders/{id}/cancel` | 订单所有者、管理员 | 取消一个待支付订单，退回它占用的优惠券次数 |
| `POST admin.payment/coupon-check` | 任何用户 | `{code, amount_micro}` → 折扣和价格，不占用次数 |
| `GET`/`POST admin.payment/coupons`, `PUT …/coupons/{id}`, `GET …/coupons/{id}/uses` | 管理员 | 优惠券 |
| `POST`/`GET admin.payment/webhook/{provider}` | 公开 | 支付渠道回调（易支付用 GET） |
| `GET admin.payment/return/{provider}` | 公开 | 易支付的签名回跳：入账后重定向到钱包页 |
| `GET admin.subscriptions/config` | 任何用户 | 货币、`methods`（余额排第一） |
| `GET admin.subscriptions/plans?all=1` | 任何用户 | 已启用的套餐（`all=1`：管理员还能拿到已停用的） |
| `POST admin.subscriptions/plans` | 管理员 | `{id?, name, description?, price_micro, period, allowance_unit, allowance, overage, group_id?, disabled?}` 新建或更新 |
| `GET admin.subscriptions/me` | 任何用户 | `{subscription, plan}`：当前订阅（没有就给待支付的），带 `used`、`remaining`、`period_end`、`cancel_at_period_end` |
| `POST admin.subscriptions/subscribe` | 任何用户 | `{plan_id, provider, method?, key_id?}` → `{subscription_id, status, url?}`；409 `already_subscribed`、`insufficient_balance`、`payer_group` |
| `POST admin.subscriptions/me/cancel` | 任何用户 | `{cancel}`：周期结束时结束（`false` 恢复续费） |
| `GET admin.subscriptions/me/transactions?cursor=` | 任何用户 | 额度的发放和扣减 |
| `GET admin.subscriptions/subscriptions?status=` | 管理员 | 订阅列表 |
| `POST admin.subscriptions/subscriptions/cancel` | 管理员 | `{id, cancel}` |
| `POST admin.subscriptions/subscriptions/{id}/end` | 管理员 | 立即结束（见上） |
| `POST admin.subscriptions/subscriptions/{id}/refunded` | 管理员 | `{refunded}`（默认 true）：清掉需要退款标记；`false` 则设上 |
| `POST`/`GET admin.subscriptions/webhook/{provider}`, `GET …/return/{provider}` | 公开 | 套餐付款回调 |
| `GET admin.rewards/me` | 任何用户 | 签到状态和连续天数；邀请码、已邀请人数、已获奖励 |
| `POST admin.rewards/checkin` | 任何用户 | `{key_id?}` → 金额和余额；409 `checked_in` |
| `GET admin.rewards/me/referrals`, `GET …/referrals?inviter_id=` | 任何用户、管理员 | 邀请记录及其奖励 |
| `GET`/`POST admin.announcements/announcements`, `PUT`/`DELETE …/{id}` | 管理员 | 公告 |
| `GET admin.announcements/me`, `POST …/me/dismiss` | 任何用户 | 调用者当前能看到的公告；`{id}` 关闭一条 |
| `GET admin.signup/config` | 公开 | 是否需要邀请码、允许的域名 |
| `POST admin.signup/register` | 公开 | `{email, password, name?, invite_code?, referral_code?}` |
| `POST admin.import-newapi/run` | 管理员 | `{driver, dsn, confirm_local_file?}` 或 `{data}`，`rate?`，`dry_run`（默认 true）→ 报告 |
