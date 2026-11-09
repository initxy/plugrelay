# 售卖套件

[English](sell-kit.md) | 简体中文

一组可选插件，用来把 plugrelay 当成收费服务来运营：预付费余额、兑换码、支付（Stripe；
支付宝和微信支付走易支付 epay 网关）和优惠券、订阅套餐、每日签到和邀请奖励、公告、
自助注册，以及一个从 new-api / one-api 站点迁移数据的导入工具。每个版本都编译了这些
插件，每个插件在启用之前都是关着的。钱永远是站点货币（`site.currency`）计的真钱，以
整数的百万分之一单位（micro）存储；没有积分，也没有倍率换算。

- [启用](#enable)
- [预付费余额：`meter.balance`](#prepaid-balance-meterbalance)
- [兑换码：`admin.redeem`](#redeem-codes-adminredeem)
- [支付：`admin.payment`](#payments-adminpayment)
- [订阅套餐：`admin.subscriptions`](#subscription-plans-adminsubscriptions)
- [优惠券、签到、邀请、公告](#coupons-check-in-referrals-announcements)
- [注册：`admin.signup`](#sign-up-adminsignup)
- [从 new-api 导入：`admin.import-newapi`](#import-from-new-api-adminimport-newapi)
- [控制台](#console)
- [插件 API](#plugin-api)

<a id="enable"></a>
## 启用

这些插件和其他内置插件一起在 `plugins/builtin/builtin.go` 里链接进来；核心这边对应的
代码是 `internal/app/sellkit.go`、`internal/app/sellkit_subscriptions.go` 和
`internal/app/operations.go`。整套功能分成两个功能开关
（[配置](configuration.zh-CN.md#features)）：`billing`（`meter.balance`、
`admin.payment`、`admin.redeem`、`admin.subscriptions`、`admin.import-newapi`、
`admin.rewards`、`admin.announcements`）和 `signup`（`admin.signup`）。在安装向导里
选 "sell" 会打开 `billing`；否则在控制台的功能页打开，或者用 `plugrelay apply`：

```yaml
features:
  - {id: billing, enabled: true}
  - {id: signup, enabled: true}
```

设置在各个功能自己的表单里填，金额都按站点货币；密钥类设置（Stripe 和易支付的密钥、
邀请码）用主密钥加密存储。

<a id="prepaid-balance-meterbalance"></a>
## 预付费余额：`meter.balance`

每个请求都从它的付款方余额里扣：

| `payer` | 扣谁的账户 |
|-|-|
| `user`（默认） | key 所属的用户；不属于任何用户的 key 从它自己的账户（`key:<id>`）扣 |
| `group` | key 所在的分组（一个共享余额），没有分组就扣 key 所属用户 |
| `key` | key 自己 |

充值、兑换和控制台上显示的余额，都对应请求实际扣费的那个账户。`payer=key` 时（以及
`payer=group` 而用户的某把 key 不在用户自己的分组里时），控制台会问是哪把 key；API 用
`key_id` 参数，必须是调用者自己的 key，由 key 付款时不带它会返回 `400 key_required`。
`GET meter.balance/me/payers` 列出可选的账户。`payer=key` 时，概览页和用户列表不显示
每个用户的余额，因为根本没有。

走 `/native/…`（[原生直通](adr/0007-native-passthrough-model.md)）的请求不运行任何
请求插件：既不预扣，也不从余额扣费。所以原生直通权限只给那些不靠余额计费的 key。

1. **预扣。** 核心按调用者的费率（标价 × 分组倍率，和最终计费用的是同一个费率）给
   请求的输入估算和输出上限算价。插件在一个数据库事务里预扣这个金额（如果
   `min_reserve_micro` 更大就扣它）；当可用余额（余额 − 预扣）加上账户的信用额度不够
   或者不是正数时，事务拒绝。
2. **结算。** 成功返回的响应按核心算出的金额精确扣费（请求日志里那次成功尝试上的
   `plugin.Charge`），同时释放预扣，合成一笔 `charge` 交易，id 是 `charge:<request id>`。
3. **退回。** 失败的请求，或者被上游中途断掉的流，只释放预扣。

结算和退回对每个请求 id 都是幂等的：预扣记录本身就是标记，谁先执行谁生效，而扣费 id
是唯一的，所以重试或重复结算都不会扣两次（ADR-0012 的"至少一次"规则）。结算如果快速
重试三次还是失败（数据库短暂连不上），也不会丢：之后"至少写一次"的请求日志记录里带着
同一笔扣费，后台清扫（每个节点最多每分钟一次）会根据记录，把所有超过一分钟、且请求
已经记了日志的预扣结算掉。这就是 `meter.balance` 依赖 `sink.requestlog` 的原因。
最终扣费从不打折扣：如果请求的输出超出了估算（客户端没传输出上限），没覆盖的那部分
可以让余额一次性变成负数，下一个请求就会被拒。设置 `min_reserve_micro` 可以缩小这个
口子；想有意允许透支，就给账户设信用额度。超过 `hold_ttl`（6h；节点在请求中途挂了）
且没有请求日志记录的预扣，会被清扫释放；之后一天内到达的记录或结算仍然会扣费。

同时开着 `meter.quota` 时两个都运行：先查预算，再查余额，任何一个拒绝都会撤销另一个
的预留。

[批量任务](clients.zh-CN.md#batches)的预扣和扣费跨越多个请求：

- **创建时预扣。** 创建批量任务（`POST /v1/batches`、`POST /v1/messages/batches`）时，
  以 `batch:<request id>` 预扣它的估算：每个请求的提示词（请求体每三个字节算一个 token）
  加上它的输出上限（`max_tokens`、`max_completion_tokens` 或 `max_output_tokens`；都没写
  按 4096 算），按将来结果计费用的批量价格算。OpenAI 的输入文件在上传时就估算。余额
  不够时批量任务按上面的方式被拒。
- **取结果时扣费。** 结果被读取时（客户端读，或者中转站自己读，见下），以同一个 id 向
  批量任务创建者全额扣费，即使预扣已经过期、或者余额在此期间用完了也照扣（余额变成
  负数，和输出超支一样），每个批量任务只扣一次。扣费失败的话，这批任务保持未计费，
  等下次读取时再扣。
- **释放。** 结束时没有可计费内容的批量任务（失败、过期，或者在任何请求跑之前就取消
  了），中转站下次看到它时释放预扣。批量任务的预扣比 `hold_ttl` 多保留三天。

余额用完时，客户端收到的是它所用协议自己的"余额不足"信号，永远不是可重试的那种：

| 客户端协议 | 状态码 | 响应体 |
|-|-|-|
| Anthropic | 402 | `billing_error`（Anthropic 文档里的计费错误类型） |
| OpenAI chat / responses / completions | 429 | `insufficient_quota`（OpenAI 余额用完时返回的就是这个） |
| Gemini | 429 | `RESOURCE_EXHAUSTED` |

协议支持 `x-should-retry` 的都带上 `x-should-retry: false`，消息里会写明可用多少、这个
请求需要多少。

| 设置 | 默认值 | 含义 |
|-|-|-|
| `payer` | `user` | 见上（全局） |
| `exempt` | false | 不需要余额也放行；给员工用，可以设在分组、用户或 key 上 |
| `min_reserve_micro` | 0 | 每个请求最少预扣多少 |
| `balance_header` | 空 | 在响应头里返回可用余额（十进制），例如 `x-plugrelay-balance` |
| `low_balance_micro` | 0 | 只在余额低于这个数时才发上面那个头；0 = 总是发 |
| `hold_ttl` | 6h | 没有请求日志记录的预扣，过了这个时间就释放 |

每次余额变动都是一笔交易：`topup`（支付充值）、`checkin`、`referral`
（[见下](#coupons-check-in-referrals-announcements)）、`charge`（请求扣费）、`refund`
（管理员退还某个请求的扣费，最多一次，最多退到扣费金额）、`redeem`（兑换码）、
`adjustment`（管理员调账，可正可负，每次表单提交幂等）、`subscription`（一个套餐周期：
用余额付款时是套餐价，在支付渠道付款时为零）。套餐额度是记在单独账户主体上的
`allowance` 交易（[见下](#subscription-plans-adminsubscriptions)）。负数调账永远不会让
账户超出它的信用额度。

`site.currency` 是所有存储金额的单位。只要还有账户有余额，修改它就会被拒
（`409 balances_exist`），除非设置请求里带上 `"confirm_currency_change": true`；余额
不会换算，改完之后要手动调整。

<a id="redeem-codes-adminredeem"></a>
## 兑换码：`admin.redeem`

管理员成批生成兑换码（每批 1–1000 个，指定金额、每个码可用次数，可选过期时间和批次
名）。兑换码长这样：`RC-XXXX-XXXX-XXXX-XXXX-XXXX`（100 位随机数）；只显示一次，库里
只存它的 SHA-256。用户在控制台兑换，大小写和短横线都无所谓。一次兑换是一个数据库
事务（有效、没过期、没停用、没超过 `max_uses`、这个用户没用过，然后入账），所以并发
兑换也不会超过次数上限。一个码对每个用户只算一次，不管是哪个账户付款：`payer=key`
时，同一个用户的第二把 key 不能再兑一次。失败尝试按用户限次（`failures_per_hour`，
默认 10）；每次尝试在检查之前先占一个名额，所以并发猜码也绕不过这个限制。

<a id="payments-adminpayment"></a>
## 支付：`admin.payment`

用户选一个金额（预设金额和上下限在设置里配）和一种支付方式（只列出已配置、且用站点
货币收款的支付渠道），到支付渠道那边付款；渠道带签名的回调通知给余额入账。内置两个
支付渠道。

回跳地址和回调地址都用站点的公网地址（站点设置 `site.public_base_url`）拼出来，从不
用请求里的 `Host` 头；没设置时，下单接口返回 `503 not_configured`。支付渠道自己的错误
信息记在日志里，用户只看到一个通用的 `502 upstream`。所有内置插件的公开回调路由
（`webhook`、`return`、注册）按每条路由、每个客户端地址每五分钟 600 次限流
（`429 rate_limited`；支付渠道会稍后重试），这样注册洪水不会挤掉支付通知。如果前面有
反向代理，要把它写进 `trusted_proxies`（[部署](deployment.zh-CN.md)）：否则所有调用都
来自代理的地址、共用同一个限额，任何人刷一波都能把支付通知堵住。

事件 id 只在真正入账之后才记住（保留一周）：没有入账的通知（还没付款、金额和订单
对不上、订单不存在）会被确认收到，但不记住，所以之后用同一个 id 来的通知仍然能入账。

<a id="stripe"></a>
### Stripe

1. 在 Stripe 控制台添加一个 webhook 端点
   `https://<relay>/api/plugins/admin.payment/webhook/stripe`，订阅
   `checkout.session.completed` 和 `checkout.session.async_payment_succeeded`。
2. 设置 `stripe_secret_key`（`sk_…`）和 `stripe_webhook_secret`（`whsec_…`）。

一次充值就是一个订单（表 `payment_orders`，和套餐订单一起列在订单页上）。创建
Checkout 时把订单 id 放进 `client_reference_id` 和 metadata，按货币的最小单位收款，
规则照 [Stripe 的文档](https://docs.stripe.com/currencies)：两位小数的货币按分；
JPY 这类零位小数的货币按整数单位；ISK 和 UGX 按两位小数的金额且末尾必须是 `00`；BHD、
JOD、KWD、OMR 和 TND 按千分之一且最后一位是 0。金额里有这个货币收不了的零头，或者站点
货币不在 Stripe 的列表里，都会被拒。Checkout 会话上关掉了自动计税和促销码：实付总额
必须等于订单金额。webhook 是公开的，只靠 `Stripe-Signature` 头鉴权：用端点密钥对
`<t>.<body>` 做 HMAC-SHA256，和每个 `v1` 值比对，并且 `t` 必须在五分钟以内，和 Stripe
文档写的一样。实付金额或币种和订单不一致的事件只记日志，不入账。入账是交易
`topup:<order>`，所以重放的事件、重复投递、同一笔付款的第二个事件都只入账一次。

<a id="epay-易支付-alipay-wechat-pay"></a>
### 易支付 epay（支付宝、微信支付）

很多国内网关（彩虹易支付及其各种克隆版）通过易支付商户协议收支付宝和微信支付
（[协议文档](https://pay.yzfpay.com/doc/v1_legacy_api.html)；字段名对照过开源的
彩虹易支付代码和 new-api 的客户端）。设置 `epay_url`（网关地址，`submit.php` 和
`mapi.php` 都在它下面）、`epay_pid` 和 `epay_key`；`epay_types` 列出提供的支付方式
（默认 `alipay`、`wxpay`）。不需要在网关那边登记任何东西：每个订单都自带
`notify_url`（`<public URL>/api/plugins/admin.payment/webhook/epay`）和 `return_url`
（`…/return/epay`）。

- **下单。** `epay_mode: submit`（默认）把用户带着签好名的订单送到 `submit.php`；`mapi`
  则在服务端调 `mapi.php` 下单（带上客户端地址），然后把用户送到它返回的 `payurl`
  （没有就用 `qrcode` 或 `urlscheme` 链接）。字段：`pid`、`type`、`out_trade_no`（订单
  id）、`notify_url`、`return_url`、`name`（`epay_product_name`）、`money`（单位元，两位
  小数）、`sign`、`sign_type=MD5`。
- **签名。** 除 `sign`、`sign_type` 和空值以外的所有参数，按参数名排序，拼成
  `k=v&k=v`，不做 URL 编码，直接把密钥接在后面，取 MD5 的小写十六进制。因为签名用的
  是未编码的值，含 `&` 的值会被拒。
- **异步通知。** 网关用 GET 调 `notify_url`（POST 表单也能读）。签名必须对，`pid` 必须
  是我们的，`trade_status` 必须是 `TRADE_SUCCESS`；订单必须存在，实付 `money` 必须等于
  订单金额（按数值比较：网关会去掉末尾的零）。回复 `success`，重复通知、订单不存在、
  金额对不上也一样回 `success`，这几种只记日志不入账（也不影响之后同一个 `trade_no` 的
  `TRADE_SUCCESS` 正常入账）；签名不对回 `fail`（400）。入账是 `topup:<order>`，所以重复
  通知、换了 `trade_no` 的重放，以及用户回跳，都只入账一次。
- **回跳。** 用户带着同样的签名参数回到 `return_url`；参数有效就同样给订单入账（网关
  连不上中转站时很有用），然后重定向到钱包页。
- **货币。** 易支付没有币种字段，收的是人民币：只在 `site.currency` 为 `CNY` 时提供，
  其他货币下单一律拒绝（不做换算）。金额必须是整分。
- **退款。** 易支付协议没有退款接口，也没有退款通知：先在网关的商户后台退款，再在
  中转站里手动把钱扣回来——充值的话做一笔负数 `adjustment`；套餐的话在套餐页点
  "立即结束"（被标记的那条再点"标记为已退款"）。

<a id="other-providers"></a>
### 其他支付渠道

其他支付渠道实现 `payment.Provider` 接口——`Checkout`（返回一个 URL，把用户送过去）和
`Verify`（验证回调，并说明它付的是哪个订单）；回调用表单参数、要求自定义回复的网关
实现 `FormProvider`；要提供多种支付方式或限制币种，实现 `MethodProvider` 和
`CurrencyProvider`；要按周期自动扣套餐费，实现 `RecurringProvider`。实现好之后，在
`plugins/builtin/builtin.go` 引入的某个包里调 `payment.RegisterProvider` 注册；
参考实现是 `plugins/admin/payment/stripe` 和 `plugins/admin/payment/epay`。它们的回调
地址是 `/api/plugins/admin.payment/webhook/<name>`。

<a id="subscription-plans-adminsubscriptions"></a>
## 订阅套餐：`admin.subscriptions`

管理员在套餐页定义套餐：每个周期（周、月或年）的价格（站点货币），每个周期的额度——
按金额（请求按它本来要扣的钱计）或按 token（输入、缓存和输出）——以及用完之后怎么办：
`block` 拒绝请求，错误和余额为空时一样是"余额不足"（消息里写明额度已用完以及什么时候
刷新），`balance` 让预付费余额来付。套餐可以指定一个分组：订阅期间用户会被移进这个
分组（连同原本在用户自己分组里的 key），订阅结束时移回去，这样分组的模型白名单、限流
和价格倍率就会生效（[设置的层级](configuration.zh-CN.md#layered-plugin-settings)）。只有
仍在套餐分组里的用户才会被移回（key 也只移回仍在里面的）：管理员在此期间把人移到别处
的，就留在那里。`payer=group` 时，套餐分组会改变由哪个分组的余额付款，所以在分组付款
期间，保存带分组的套餐、订阅这样的套餐都会被拒（`409 payer_group`）；反过来，只要有
套餐或生效中的订阅带分组，把 `meter.balance` 改成 `payer=group` 也会被拒（409），设置页、
`plugrelay apply` 和 new-api 导入都一样；在这样一份已存储的设置上打开 billing 功能也会
被拒。停用的套餐不再接受新订阅，已有的订阅照常保留。
每个订阅会复制一份套餐当时的条款，所以修改套餐只影响之后的订阅。

每次保存 `meter.balance` 都会做这个检查，所以如果存储里已经同时有 `payer=group` 和一个
带分组的套餐或生效中订阅，任何修改都会被拒。解决办法：在同一次保存里把 `payer` 改掉，
或者把套餐上的分组去掉，或者结束那些有问题的订阅
（`POST /api/plugins/admin.subscriptions/subscriptions/{id}/end`，即使支付渠道拒绝，
本地订阅也会结束）。

一个用户只能有一个生效中（待支付或已生效）的订阅，由数据库保证（一个唯一索引，并且
每次改变哪个订阅生效时都锁住用户那一行）。已有生效中的订阅时再订阅会被拒
（`409 already_subscribed`）；待支付的订阅（下了单没付钱）在再次订阅时被结束。如果一个
订阅因为另一个订阅已经生效、或者用户已被删除而无法开始，但钱还是到了（没付的那单最后
还是付了，或者用户已经退订的套餐又来了一个迟到的 Stripe `invoice.paid`），这笔付款会被
记下来（`subscription:<provider>:<id>`），但不会开始任何订阅：这个订阅在套餐页和日志里
被标记为**需要退款**，如果是 Stripe 订阅还会被设为取消，不再扣下一个周期。去支付渠道
退款，然后点"标记为已退款"。这种情况下用余额付款永远不会扣走套餐价。支付方式（配置了
才显示）：

| 方式 | 第一个周期 | 续费 |
|-|-|-|
| 预付费余额 | 从余额扣套餐价（`subscription` 交易），余额不够则拒绝 | 周期结束时从余额扣；余额不够就结束订阅 |
| Stripe | 订阅模式的 Checkout | Stripe 每个周期扣款；每个 `invoice.paid` 开始下一个周期 |
| 易支付（支付宝、微信支付） | 付一次买一个周期 | 和上面一样从余额扣（易支付没有自动续费） |

取消（用户在自己的套餐页，或管理员操作）会让订阅在周期结束时结束，不再续费；Stripe
订阅会先在 Stripe 那边设置（`cancel_at_period_end`）。不按比例折算：要换套餐，先取消，
等周期结束后再订新的。管理员的**立即结束**（退款或拒付之后用）会马上结束订阅：额度不再
可用，用户离开套餐分组，Stripe 订阅先在 Stripe 那边设为取消（那边拒绝的话记日志，这边
照样结束）；钱不会由中转站退。核心的审计日志会记下这次调用和订阅 id。"立即结束"和
"标记为已退款"都需要近期重新验证过身份，和删除用户一样。删除用户时会先结束这个用户的
生效中订阅，Stripe 订阅设为在 Stripe 那边取消；如果做不到（Stripe 拒绝、支付渠道没配置，
或者一个待支付的订单还没有对应的 Stripe 订阅），删除会被拒（409），免得它继续扣费：先
"立即结束"再删除，或者带 `?force=true` 删除（`DELETE /api/v1/users/{id}`），这样只在本地
结束。之后再到账的付款，按上面的方式标记为需要退款。

周期从开始那天算到下一周、下个月或下一年的同一天；目标月份没有那一天时取该月最后
一天（1 月 31 日加一个月是 2 月 28 或 29 日，下一个周期从那天开始算）。续费和到期在
`meter.balance` 的后台清扫里处理（每个节点最多每分钟一次，有请求进来时才跑），从不在
打开页面时处理；某个订阅续费失败会记日志，其他订阅照常续费。用余额续费时，"续费"还是
"因余额不足结束"是和订阅一起在一个加锁的事务里决定的，所以在此期间已经付过的周期
（一笔易支付订单）不会被它结束。

**计量**由 `meter.balance` 负责，和余额在同一批数据库事务里。有当前订阅的用户先用套餐
额度。请求按套餐的单位，从剩余额度里预扣它的估算——金额套餐：和余额一样的预留（输入
估算和输出上限按调用者费率算，至少 `min_reserve_micro`）；token 套餐：输入估算加输出
上限——至少 1，最多是剩下的量，这样并发请求能看到额度已被占用。额度用完时，`block`
拒绝请求，`balance` 把请求交给余额，和没有套餐时一样。
token 套餐只覆盖按 token 计价的请求：生图和图片编辑、语音合成、转写和 rerank（价格可以
按次计）都和没有套餐时一样走余额，两种策略都是；一个不报 token 但要花钱的请求，全部从
余额付。
`balance` 策略下，估算比剩余额度大的请求，只有在付款方可用余额（加信用额度）够付剩下
那部分时才由套餐覆盖；否则整个请求都在余额上预扣。每个请求按请求 id 只结算一次，和
余额预扣一样：它的用量（金额套餐：扣费金额；token 套餐：输入、缓存和输出 token）从
剩余额度里扣，永远不超过额度，剩下的作为钱从付款方余额里扣（`charge:<request>`）——
token 按这个请求自己的费率（扣费金额 ÷ token 数）换算成钱。`block` 也是这样：超出估算
的请求（客户端没传输出上限，或者 `min_reserve_micro` 设得太低）超出部分从余额付，必要
时余额变成负数。这个负数余额就是欠款：下一次充值先还它，用余额续费时要在它之上再付
套餐价。额度的扣减是记在账户主体 `sub:<subscription>` 上的 `allowance` 交易
（`allowance:<request>`，`balance_after` = 剩余量；每个周期以一笔发放开始），所以套餐页
能显示用了多少、还剩多少。结算失败的从请求日志里补收，过期的预扣由和余额预扣同一个
后台清扫释放。批量任务计入余额，从不计入套餐额度。

**Stripe。** 在 Stripe 里再添加一个 webhook 端点
`https://<relay>/api/plugins/admin.subscriptions/webhook/stripe`，订阅
`invoice.paid`、`customer.subscription.updated`、`customer.subscription.deleted`、
`charge.refunded` 和 `charge.dispute.created`，并把它的签名密钥设为 admin.payment 上的
`stripe_subscription_webhook_secret`（其他密钥也都留在那里）。Checkout 用
`mode=subscription` 创建，带按周期计费的 `price_data`，订阅 id 放进
`client_reference_id`、`metadata` 和 `subscription_data[metadata]`。按 Stripe 的建议
（[订阅 webhook](https://docs.stripe.com/billing/subscriptions/webhooks)），权益跟着
`invoice.paid` 走：订阅这条记录的周期按发票 id 只开始一次
（`subscription:stripe:<invoice>`），两种 payload 结构都能读（API 版本
`2025-03-31.basil` 起是 `parent.subscription_details`，之前是顶层的 `subscription`）；
实付金额或币种和套餐价不一致的发票只记日志，不开始任何周期。
`customer.subscription.updated` 同步 `cancel_at_period_end`，状态为 `canceled`、`unpaid`
或 `incomplete_expired` 时结束订阅；`customer.subscription.deleted` 结束订阅。Stripe
订阅在周期结束三天后还没续费的，本地就结束（之后再来的 `invoice.paid` 会让它重新开始）。
事件的签名校验和去重和充值一样。订阅发票的全额退款（`charge.refunded` 且
`refunded: true`）或新的拒付（`charge.dispute.created`）会让对应订阅立即结束，并在
Stripe 那边设为取消；对一笔已标记需要退款的付款退款，会清掉这个标记。发票的查找方式照
Stripe 文档里退款和拒付的写法：`2025-03-31.basil` 之前用 charge 的 `invoice`，之后用
PaymentIntent 的已付发票付款记录（`GET /v1/invoice_payments`），再从发票找到订阅。部分
退款不做任何改变；赢下来的拒付不会让订阅重新开始——需要重新订阅。

**易支付。** 通过易支付购买的套餐是一个和充值一样的订单，只是 `notify_url` 和
`return_url` 指向 `/api/plugins/admin.subscriptions/`；校验相同，每个订单只开始一个
周期（`subscription:epay:<order>`）。

<a id="coupons-check-in-referrals-announcements"></a>
## 优惠券、签到、邀请、公告

每一项在 billing 功能的表单里都有自己的开关，默认关闭。

- **优惠券**（`admin.payment`，`coupons_enabled`）：在优惠券页创建的码，可以给充值打折
  （按百分比，设了金额的话最多减这么多）或直接减一个固定金额，可以限定生效时间段、
  最低充值金额、总使用次数和每人使用次数。支付渠道按折后价收款，向下取整到分，入账按
  原价全额。下单时在一个锁住优惠券记录的事务里占用一次使用，所以并发下单也不会超过
  限制。没付款时这次使用会保留一小时，用户点"取消"回来之后也一样
  （`POST admin.payment/orders/{id}/cancel`），因为支付渠道的页面可能还会被付掉；只有
  支付渠道能让 checkout 过期时（Stripe），才会马上把这次使用还回去。付款之后这次使用就
  算用掉了，取消之后才到账的付款也一样；下单超过一小时才付款的订单，按实付金额入账，
  不给折扣。
- **每日签到**（`admin.rewards`，`checkin_enabled`）：每个用户在站点时区（`site.timezone`，
  默认 UTC）每天一次，奖励在最低和最高奖励之间随机取一个整分的金额，记为交易
  `checkin:<user>:<day>`；当天再签到不会有任何变化（409 `checked_in`）。
- **邀请奖励**（`admin.rewards`，`referral_enabled`）：每个成员都有一个邀请码；
  `/signup?ref=<code>` 会记下邀请人（被邀请人只认第一个邀请人，自己不能邀请自己）。被
  邀请人第一次付费充值时，邀请人得到实付金额的一个百分比或一笔固定金额，累计不超过
  `referral_cap_micro`（0 = 不封顶），以 `referral:<invitee>` 记到邀请人请求扣费的那个
  账户（`payer=key` 时没有奖励）。邀请记录和入账在同一个事务里加锁并标记，所以重复
  回调和并发充值都只发一次奖励，也不会超过上限。
- **公告**（`admin.announcements`，`enabled`）：Markdown 格式的通知，可选开始和结束时间，
  可选受众（所有人、成员、管理员），以可关闭的横幅显示在控制台顶部。存在插件的 KV 里，
  最多 200 条。

签到和邀请奖励是计量插件之外的余额变动，所以 `TestPluginsCannotChangeBalances` 列出了
它们的账本调用（`CheckIn`、`RewardReferral`），以及唯一允许调用它们的插件。

<a id="sign-up-adminsignup"></a>
## 注册：`admin.signup`

默认关闭。打开后，登录页会出现"注册账号"，`/signup` 用邮箱和密码（至少 10 个字符）
注册一个终端用户（角色 `user`，邮箱标为未验证）；可以要求填写 `invite_codes` 里的某个
邀请码（`invite_required`），以及邮箱域名必须在 `allowed_domains` 里（地址并没有验证，
所以域名名单只是过滤一下大家填的内容；要按组织限制，请改用单点登录）。新用户加入
`default_group`，正常登录，在控制台里自己创建 key。注册尝试按客户端地址限次
（`per_ip_per_hour`，默认 5）。验证码默认关闭：把 `captcha_provider` 设成 `turnstile`
（Cloudflare）或 `hcaptcha`，再配上 `captcha_site_key` 和 `captcha_secret`（密钥类设置，
加密存储），表单就会显示验证组件，`/register` 会通过核心出口调用验证码服务商的
siteverify 接口校验它的 token——没带 token 返回 403 `captcha_required`，校验不通过返回
403 `captcha_failed`，服务商连不上返回 503 `captcha_unavailable`（连不上就拒绝；选了
服务商但两个密钥没配全时也同样返回，而不是跳过检查）。没有邮箱验证。正因为没人验证过
邮箱地址，SSO 的 `link_by_email` 永远不会把身份关联到自助注册（或导入）的账号上：先拿
别人邮箱注册的人，抢不走那个人的 SSO 登录。IdP 那边的用户会收到 `account_exists`；由
管理员手动关联，或者删掉那个占位的账号。

<a id="import-from-new-api-adminimport-newapi"></a>
## 从 new-api 导入：`admin.import-newapi`

读取 new-api（[QuantumNous/new-api](https://github.com/QuantumNous/new-api)，表结构以
commit `2de51a19` 为准：`model/channel.go`、`model/token.go`、`model/user.go`、
`model/option.go`）或 one-api 的数据库——MySQL、PostgreSQL 或 SQLite 的 DSN——或者同样
这些行的 JSON 导出（`{"channels": [...], "tokens": [...], "users": [...], "options": {...}}`，
列名和 new-api 一致）。每次运行都先是试运行，列出哪些会新建、更新、保持不变或跳过，
以及原因；点导入之前什么都不写。记录保留 new-api 的 id（`newapi-c7`、`newapi-u3`、
`newapi-t12`、`newapi-<group>`），所以再跑一次，已有的东西不会变，也不会重复入账。

| new-api | plugrelay |
|-|-|
| 渠道 channel | 用匹配的清单建一个上游（OpenAI、Anthropic、Gemini、DeepSeek、Moonshot、Zhipu、Qwen、Volcengine、MiniMax、Mistral、xAI、OpenRouter、Bedrock、Ollama、vLLM、SGLang、Xinference，custom / SiliconFlow 当作 OpenAI 兼容）；base URL 不是官方地址的 OpenAI 渠道变成用那个地址的 OpenAI 兼容上游；其他类型跳过并说明原因 |
| 渠道密钥（每行一个） | 每个一把共享凭据；Bedrock 的 `AK\|SK\|region` 变成 `AK:SK` 加区域 |
| 渠道的模型、模型映射、分组、优先级、权重 | 模型及其指向上游模型的路由，限定在这些分组里 |
| 分组（以及 `GroupRatio`） | 带这个价格倍率的分组 |
| 用户 | 一个用户（所有角色，包括 root 和管理员，都变成 user：等他们设好新密码后再手动提升为管理员）；登录名用邮箱，没有就用用户名，标为未验证；已禁用的保持禁用 |
| 用户额度 quota | 一笔 `adjustment` 入账：quota ÷ `QuotaPerUnit` 美元，站点货币不是 USD 时再乘以你给的汇率 |
| 令牌 token | 这个用户的一把平台 key，保留过期时间、分组、模型限制和 IP 白名单 |

Key 能直接沿用。plugrelay 从不存 key 本身，只存它的 SHA-256；new-api 明文存令牌，客户端
发的是 `sk-` 加上存的那 48 个字符，所以导入时就存这串完整内容的 SHA-256，客户端照常能用。
`KeyByToken` 除了 plugrelay 自己的 `pr_` key，也接受这种格式。密码是 bcrypt 的可以沿用
（两边都用 bcrypt）；新版 new-api 的 argon2 哈希沿用不了，这些用户会收到警告，必须重新
设密码。不迁移的：单个令牌的额度限制（会在报告里列出；改用 `meter.quota` 的 key 预算）、
日志、充值记录、兑换码，以及没有对应清单的渠道类型（Midjourney、Suno、Azure 等）。

导入工具会连接管理员给的任何 DSN，所以只有管理员能运行，而且 DSN 读不到中转站自己的
文件：MySQL DSN 解析时强制关闭 `allowAllFiles`（DSN 里要求开它或要求 `local_infile` 的
直接拒绝）；SQLite 来源必须是一个指向已存在文件的普通路径，以只读方式打开，并且要带
`"confirm_local_file": true`（控制台里是一个勾选框）。

<a id="console"></a>
## 控制台

控制台在 `/billing` 下有固定的几个页面，billing 功能打开时显示（注册页在注册功能打开时
显示）。

| 插件 | 终端用户 | 管理员 |
|-|-|-|
| `meter.balance` | 概览页和 key 页上的余额；钱包：余额和流水 | 余额：所有账户、调账、信用额度、流水 |
| `admin.payment` | 钱包：充值，可用优惠券 | 订单：所有充值和套餐付款；优惠券 |
| `admin.subscriptions` | 我的套餐：当前套餐、额度已用和剩余、续费日期、订阅、取消、额度流水 | 套餐：定义套餐、订阅列表（标出需要退款）、到期取消、立即结束、标记为已退款 |
| `admin.redeem` | 钱包：兑换码兑换 | 兑换码：批次、生成、导出、停用、谁兑换了 |
| `admin.rewards` | 钱包：签到、邀请链接和收益 | —（账本里能看到每一笔奖励） |
| `admin.announcements` | 每个页面上的横幅 | 公告 |
| `admin.signup` | 注册页和登录页上的链接 | — |
| `admin.import-newapi` | — | 从 new-api 导入 |

<a id="plugin-api"></a>
## 插件 API

路径都在 `/api/plugins/<id>/` 下（[管理 API](api.zh-CN.md)）；`admin` 路由需要管理员
角色，公开路由不需要登录。

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
