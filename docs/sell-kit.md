# Sell kit

English | [简体中文](sell-kit.zh-CN.md)

Optional plugins for running plugrelay as a paid service: prepaid balance,
redeem codes, payments (Stripe; Alipay and WeChat Pay through an epay
gateway), coupons, subscription plans, check-in and referral rewards,
announcements, self sign-up and a new-api / one-api importer. Every build
includes them; each is off until enabled. Amounts are real money in the site
currency (`site.currency`), stored as integer micro-units. No points, no
ratios.

- [Enable](#enable)
- [Prepaid balance: `meter.balance`](#prepaid-balance-meterbalance)
- [Redeem codes: `admin.redeem`](#redeem-codes-adminredeem)
- [Payments: `admin.payment`](#payments-adminpayment)
- [Subscription plans: `admin.subscriptions`](#subscription-plans-adminsubscriptions)
- [Coupons, check-in, referrals, announcements](#coupons-check-in-referrals-announcements)
- [Sign-up: `admin.signup`](#sign-up-adminsignup)
- [Import from new-api: `admin.import-newapi`](#import-from-new-api-adminimport-newapi)
- [Console](#console)
- [Plugin API](#plugin-api)

## Enable

Two features ([configuration](configuration.md#features)): `billing`
(`meter.balance`, `admin.payment`, `admin.redeem`, `admin.subscriptions`,
`admin.rewards`, `admin.announcements`, `admin.import-newapi`) and `signup`
(`admin.signup`). Choosing "Selling" in the setup wizard turns on `billing`;
otherwise use the console's Features page or `plugrelay apply`:

```yaml
features:
  - {id: billing, enabled: true}
  - {id: signup, enabled: true}
```

Settings are in each feature's form. Secrets (Stripe and epay keys, invite
codes) are sealed with the master key. Code: `plugins/builtin/builtin.go`,
`internal/app/sellkit*.go`, `internal/app/operations.go`.

## Prepaid balance: `meter.balance`

| `payer` | account charged |
|-|-|
| `user` (default) | the key's user; a key without a user pays from its own account (`key:<id>`) |
| `group` | the key's group (one shared balance), else the key's user |
| `key` | the key itself |

Top-ups, redeem codes and the console's balance all use that account. When
it depends on the key (`payer=key`, or `payer=group` with a key outside the
user's group) the console asks which key; the API takes `key_id` and answers
`400 key_required` without it (`GET meter.balance/me/payers` lists the
choices).

Each request is **held** (input estimate plus output limit at the caller's
rate, at least `min_reserve_micro`; refused when available balance plus
credit limit doesn't cover it), then **settled** at exactly the core's charge
as transaction `charge:<request id>`, or **released** if it failed. A
request is never charged twice. A settle that fails is retried and, failing
that, done later by the sweep (once a minute per node) from the request log,
so `meter.balance` requires `sink.requestlog`. Holds older than `hold_ttl`
with no log record (a node died mid-request) are released.

The charge is never cut short: a request that ran past its estimate (no
output limit sent) can take the balance below zero once, and the next one
is refused. Raise `min_reserve_micro` to narrow that, or set a credit limit
to allow it on purpose. With `meter.quota` on too, budgets are checked first.
[Native passthrough](adr/0007-native-passthrough-model.md) runs no plugins
and is never charged: grant it only to keys you don't bill.

[Batches](clients.md#batches) hold their estimate when created (prompt at
one token per three body bytes, plus each request's output limit or 4096, at
the batch price; OpenAI input files are estimated at upload), and are
charged in full to the creator once, when the results are first read, even
below zero. A batch that ends with nothing to bill is released. Batch holds
last three days past `hold_ttl`.

Out of balance, clients get their protocol's own non-retryable error
(`x-should-retry: false` where the protocol has it), saying what is
available and what was needed:

| client protocol | status | body |
|-|-|-|
| Anthropic | 402 | `billing_error` |
| OpenAI chat / responses / completions | 429 | `insufficient_quota` |
| Gemini | 429 | `RESOURCE_EXHAUSTED` |

| setting | default | meaning |
|-|-|-|
| `payer` | `user` | see above (global only) |
| `exempt` | false | serve without a balance; set on a group, user or key for staff |
| `min_reserve_micro` | 0 | smallest hold per request |
| `balance_header` | empty | response header carrying the available balance, e.g. `x-plugrelay-balance` |
| `low_balance_micro` | 0 | send that header only below this amount; 0 = always |
| `hold_ttl` | 6h | release holds that have no request log record after this |

Transaction kinds: `topup`, `redeem`, `charge`, `refund` (an admin returns a
request's charge, once, at most the charge), `adjustment` (admin credit or
debit; never past the credit limit), `subscription` (a plan period paid from
the balance; zero when paid at a provider), `checkin`, `referral`. Plan
allowance draws are `allowance` transactions on a separate subject.

Changing `site.currency` while any balance exists answers `409
balances_exist` unless the request sends `"confirm_currency_change": true`.
Balances are not converted.

## Redeem codes: `admin.redeem`

Admins generate batches of 1–1000 codes with an amount, uses per code, and
optional expiry and batch name. Codes look like
`RC-XXXX-XXXX-XXXX-XXXX-XXXX`, are shown once and stored as SHA-256; case
and dashes are ignored when redeeming. A code counts once per user, even
across keys with `payer=key`. Failed attempts are limited per user
(`failures_per_hour`, default 10).

## Payments: `admin.payment`

Users pick an amount (presets and limits are settings) and a method; only
configured providers that charge in the site currency are offered. The
provider's signed callback credits the balance.

- Return and callback URLs come from `site.public_base_url`, never from
  `Host`. Unset, checkout answers `503 not_configured`.
- Provider errors are logged; users see `502 upstream`.
- Public routes (`webhook`, `return`, sign-up) allow 600 calls per route and
  client address per five minutes (`429 rate_limited`). Behind a reverse
  proxy, list it in `trusted_proxies` ([deployment](deployment.md)), or all
  callers share the proxy's limit and a flood blocks payment notices.
- Each order credits once (transaction `topup:<order>`); a notice that
  credits nothing is acknowledged, and a later one can still credit.

### Stripe

1. In the Stripe dashboard, add the webhook endpoint
   `https://<relay>/api/plugins/admin.payment/webhook/stripe` for
   `checkout.session.completed` and `checkout.session.async_payment_succeeded`.
2. Set `stripe_secret_key` (`sk_…`) and `stripe_webhook_secret` (`whsec_…`).

Checkout charges in the currency's [minor unit](https://docs.stripe.com/currencies)
and refuses amounts or currencies Stripe can't charge; automatic tax and
promotion codes are off. Events are checked against `Stripe-Signature`; one
whose amount or currency differs from the order is logged, not credited.

### epay (易支付: Alipay, WeChat Pay)

For gateways that speak the epay merchant protocol (彩虹易支付 and its
clones; [protocol](https://pay.yzfpay.com/doc/v1_legacy_api.html)). Set
`epay_url` (the base holding `submit.php` and `mapi.php`), `epay_pid`,
`epay_key`, and `epay_types` (default `alipay`, `wxpay`). Nothing is
registered at the gateway: each order carries its own `notify_url`
(`<public URL>/api/plugins/admin.payment/webhook/epay`) and `return_url`
(`…/return/epay`).

- `epay_mode: submit` (default) sends the user to `submit.php`; `mapi`
  creates the order at `mapi.php` and sends the user to the returned
  `payurl` (else `qrcode` or `urlscheme`). The item name is
  `epay_product_name`.
- The notify (GET or POST form) must have a valid MD5 signature, our `pid`,
  `trade_status` `TRADE_SUCCESS` and the order's `money`. The relay answers
  `success` even to duplicates and mismatches (logged, not credited) and
  `fail` (400) to a bad signature. A value containing `&` is refused.
- A valid signed return credits the same way, for when the gateway can't
  reach the relay, then redirects to the wallet page.
- Offered only when `site.currency` is `CNY`; amounts must be whole fen.
- epay has no refund call. Refund in the gateway's merchant console, then
  take the money back by hand: a negative `adjustment` for a top-up, or
  "End now" (and "Mark refunded" if flagged) for a plan.

### Other providers

Implement `payment.Provider` and register it with `payment.RegisterProvider`
from a package imported in `plugins/builtin/builtin.go`, following
`plugins/admin/payment/stripe` or `…/epay`. Its callback is
`/api/plugins/admin.payment/webhook/<name>`.

## Subscription plans: `admin.subscriptions`

A plan has a price per week, month or year, an allowance per period in money
or tokens (input, cached and output), and an overage policy: `block` refuses
with the out-of-credit errors above (saying when it renews), `balance` lets
the prepaid balance pay. Subscriptions copy the plan's terms, so edits apply
to later ones; a disabled plan takes no new subscribers.

A plan may name a group. Subscribers, with their keys in their own group,
move into it while subscribed, so its model allowlist, rate limits and price
multiplier apply ([settings layers](configuration.md#layered-plugin-settings)),
and move back afterwards unless an admin moved them meanwhile. This conflicts with
`payer=group`: a plan group is refused (`409 payer_group`) while groups pay,
and switching to `payer=group` is refused (409) while any plan or live
subscription has a group. If a stored setting already has both, every
`meter.balance` save fails until you change `payer` in the same save, remove
the plan's group, or end those subscriptions.

| method | first period | renewal |
|-|-|-|
| prepaid balance | price taken from the balance; refused if short | from the balance at period end; short ends the subscription |
| Stripe | Checkout in subscription mode | Stripe bills; each `invoice.paid` starts the next period |
| epay | one payment, one period | from the balance (epay has no recurring billing) |

- **One per user.** Subscribing while active answers `409
  already_subscribed`; an unpaid pending one is replaced.
- **Refund due.** A payment for a subscription that can't start (another is
  active, or the user was deleted) starts nothing and is flagged **refund
  due** on the Plans page; a Stripe one is set to cancel. Refund at the
  provider, then "Mark refunded".
- **Cancel** ends at the period end (for Stripe, `cancel_at_period_end`).
  No proration: to switch plans, cancel and subscribe after the period.
- **End now** (admin, e.g. after a refund or chargeback) ends at once and
  cancels at Stripe; the relay refunds nothing. It and "Mark refunded" need a
  recent re-authentication.
- **Deleting a user** ends their subscription first. If that fails (Stripe
  refuses, provider not configured, checkout still pending) the delete
  answers 409: End now first, or `DELETE /api/v1/users/{id}?force=true` to
  end it locally only.
- **Periods** end on the same day next week, month or year, clamped to the
  month's last day (Jan 31 → Feb 28/29). Renewals and expiries run in
  `meter.balance`'s sweep, never on a page view.

**Metering.** A subscriber draws on the allowance before the balance. Holds
work as for the balance, in the plan's unit and capped at what is left. When
it runs out, `block` refuses and `balance` falls back to the balance; with
`balance`, a request bigger than what is left uses the plan only if the
balance covers the rest. Anything past the allowance, including an overrun
under `block`, is charged to the balance (tokens converted at the request's
own rate), below zero if need be; the next top-up pays that debt first.
Token plans cover only token-priced requests: images, speech,
transcription, rerank, requests reporting no tokens, and batches always pay
from the balance. Draws are `allowance` transactions on `sub:<subscription>`,
shown as used and left on the plan page.

**Stripe.** Add a second webhook endpoint
`https://<relay>/api/plugins/admin.subscriptions/webhook/stripe` for
`invoice.paid`, `customer.subscription.updated`,
`customer.subscription.deleted`, `charge.refunded` and
`charge.dispute.created`, and set its signing secret as
`stripe_subscription_webhook_secret` on `admin.payment`. Access follows
`invoice.paid`, [as Stripe advises](https://docs.stripe.com/billing/subscriptions/webhooks);
an invoice that doesn't match the price starts nothing. A subscription
Stripe deletes, or marks `canceled`, `unpaid` or `incomplete_expired`, ends
here; one not renewed three days after its period ends too, until a later
`invoice.paid`. A full refund or a new dispute of an invoice ends the
subscription now; a partial refund or a won dispute changes nothing.

**epay.** A plan bought through epay is an order like a top-up, with its
callbacks under `/api/plugins/admin.subscriptions/`; each order buys one
period.

## Coupons, check-in, referrals, announcements

Each has its own switch in the billing feature's form, off by default.

- **Coupons** (`admin.payment`, `coupons_enabled`): a percent off (optionally
  capped) or a fixed amount off a top-up, with a time window, minimum
  top-up, total uses and uses per user. The discount is rounded down to
  whole cents; the user pays the discounted price and gets the full amount.
  An unpaid use is held for an hour, even after the user cancels, unless the
  provider can expire the checkout (Stripe). An order paid more than an hour
  after checkout gets what was paid, without the discount.
- **Daily check-in** (`admin.rewards`, `checkin_enabled`): once per user per
  day in `site.timezone` (default UTC), a random amount between the lowest
  and highest reward. A second try answers 409 `checked_in`.
- **Referral rewards** (`admin.rewards`, `referral_enabled`): each member
  has an invite code; `/signup?ref=<code>` records the first inviter. On the
  invitee's first paid top-up, the inviter gets a percent of it or a fixed
  amount, up to `referral_cap_micro` in total (0 = no cap). Nothing is paid
  with `payer=key`.
- **Announcements** (`admin.announcements`, `enabled`): Markdown notices
  with an optional time window and audience (everyone, members, admins),
  shown as a dismissible banner. At most 200.

## Sign-up: `admin.signup`

When on, the login page offers "Create an account" and `/signup` registers
an end user (role `user`) by email and password (10+ characters) into
`default_group`. Options: an invite code from `invite_codes`
(`invite_required`), email domains in `allowed_domains`, attempts per
address (`per_ip_per_hour`, default 5), and a captcha (`captcha_provider`:
`turnstile` or `hcaptcha`, with `captcha_site_key` and `captcha_secret`).
Captcha errors: 403 `captcha_required`, 403 `captcha_failed`, 503
`captcha_unavailable` (it fails closed).

Emails are not verified, so `allowed_domains` only filters what people type
(restrict by organization with single sign-on), and SSO `link_by_email`
never links to a self-registered or imported account: the SSO user gets
`account_exists` until an admin links them or deletes the squatter.

## Import from new-api: `admin.import-newapi`

Reads a new-api ([QuantumNous/new-api](https://github.com/QuantumNous/new-api),
schema as of commit `2de51a19`) or one-api database by MySQL, PostgreSQL or
SQLite DSN, or a JSON export
(`{"channels": [...], "tokens": [...], "users": [...], "options": {...}}`
with new-api's column names). Each run starts as a dry run listing what
would change and why. Rows keep their new-api ids (`newapi-c7`, `newapi-u3`,
`newapi-t12`, `newapi-<group>`), so running it again changes nothing.

| new-api | plugrelay |
|-|-|
| channel | an upstream from the matching manifest (OpenAI, Anthropic, Gemini, DeepSeek, Moonshot, Zhipu, Qwen, Volcengine, MiniMax, Mistral, xAI, OpenRouter, Bedrock, Ollama, vLLM, SGLang, Xinference, custom / SiliconFlow as OpenAI-compatible); an OpenAI channel with another base URL becomes OpenAI-compatible with that URL; other types are skipped with the reason |
| channel keys (one per line) | one shared credential each; Bedrock `AK\|SK\|region` becomes `AK:SK` and the region |
| channel models, model mapping, group, priority, weight | models with routes to the upstream model, restricted to those groups |
| group (and `GroupRatio`) | a group with that price multiplier |
| user | a user (every role, root and admin included, → user: promote admins by hand once they have set a new password); login is the email, else the username, marked unverified; disabled stays disabled |
| user quota | an `adjustment` credit: quota ÷ `QuotaPerUnit` USD, times the rate you give when the site currency is not USD |
| token | a platform key of that user, with its expiry, group, model limits and IP allowlist |

Existing `sk-` keys keep working. bcrypt passwords carry over; newer argon2
ones don't, and those users get a warning. Not carried: per-token quota
limits (use a `meter.quota` key budget), logs, top-up history, redemption
codes, and channel types without a manifest (Midjourney, Suno, Azure, …).

Admins only. The DSN can't read the relay's files: MySQL `allowAllFiles` and
`local_infile` are refused; a SQLite file is opened read-only and needs
`"confirm_local_file": true`.

## Console

Pages under `/billing`, shown while billing is on (the sign-up page while
sign-up is on).

| plugin | end users | admins |
|-|-|-|
| `meter.balance` | balance on the overview and the keys page; Wallet: balance and history | Balances: every account, adjust, credit limit, history |
| `admin.payment` | Wallet: top up, with a coupon | Orders: every top-up and plan payment; Coupons |
| `admin.subscriptions` | My plan: current plan, allowance used and left, renewal date, subscribe, cancel, allowance history | Plans: define plans, list subscriptions (refund due flagged), cancel at period end, end now, mark refunded |
| `admin.redeem` | Wallet: redeem a code | Redeem codes: batches, generate, export, disable, who redeemed |
| `admin.rewards` | Wallet: check in, invite link and earnings | — (balance history shows every reward) |
| `admin.announcements` | banner on every page | Announcements |
| `admin.signup` | sign-up page and login link | — |
| `admin.import-newapi` | — | Import from new-api |

## Plugin API

Under `/api/plugins/<id>/` ([admin API](api.md)). `admin` routes need the
admin role; public routes need no login.

| route | who | does |
|-|-|-|
| `GET meter.balance/me?key_id=` | any user | own account (or the key's): balance, available, held, credit limit |
| `GET meter.balance/me/transactions?key_id=&cursor=` | any user | its transactions, newest first |
| `GET meter.balance/me/payers` | any user | `{payer, items: [{subject, key_id?, key_name?}]}`: the accounts to choose from |
| `GET meter.balance/accounts` | admin | every account |
| `GET meter.balance/transactions?subject=&kind=&cursor=` | admin | transactions |
| `POST meter.balance/adjust` | admin | `{subject, amount_micro, note, idempotency_key}` |
| `PUT meter.balance/credit-limit` | admin | `{subject, credit_limit_micro}` |
| `POST meter.balance/refund` | admin | `{request_id, amount_micro?, note}` gives back a charge once |
| `POST admin.redeem/batches` | admin | `{count, amount_micro, max_uses, expires_at, batch}` → the codes, once |
| `GET admin.redeem/batches`, `POST …/batches/disable` | admin | batches summed; disable or enable a whole batch |
| `GET admin.redeem/codes?batch=&uses=1` | admin | codes (last four characters only), with who redeemed each |
| `POST admin.redeem/codes/disable` | admin | `{id, disabled}` |
| `POST admin.redeem/redeem` | any user | `{code, key_id?}` → amount and new balance |
| `GET admin.payment/config` | any user | providers, `methods: [{provider, method?}]`, currency, limits, presets |
| `POST admin.payment/checkout` | any user | `{amount_micro, provider?, method?, key_id?}` → `{order_id, url}` |
| `GET admin.payment/orders?status=&provider=&kind=&user_id=&cursor=` | admin | orders, newest first; the first page carries 30-day totals |
| `GET admin.payment/orders/{id}` | owner, admin | order status |
| `POST admin.payment/orders/{id}/cancel` | owner, admin | cancel a pending order, giving its coupon use back |
| `POST admin.payment/coupon-check` | any user | `{code, amount_micro}` → discount and price, without taking a use |
| `GET`/`POST admin.payment/coupons`, `PUT …/coupons/{id}`, `GET …/coupons/{id}/uses` | admin | coupons |
| `POST`/`GET admin.payment/webhook/{provider}` | public | provider callback (GET for epay) |
| `GET admin.payment/return/{provider}` | public | epay's signed return: credits, then redirects to the wallet page |
| `GET admin.subscriptions/config` | any user | currency, `methods` (balance first) |
| `GET admin.subscriptions/plans?all=1` | any user | enabled plans (`all=1`: admins also get disabled ones) |
| `POST admin.subscriptions/plans` | admin | `{id?, name, description?, price_micro, period, allowance_unit, allowance, overage, group_id?, disabled?}` creates or updates |
| `GET admin.subscriptions/me` | any user | `{subscription, plan}`: the current (else pending) subscription with `used`, `remaining`, `period_end`, `cancel_at_period_end` |
| `POST admin.subscriptions/subscribe` | any user | `{plan_id, provider, method?, key_id?}` → `{subscription_id, status, url?}`; 409 `already_subscribed`, `insufficient_balance`, `payer_group` |
| `POST admin.subscriptions/me/cancel` | any user | `{cancel}`: end at the period end (`false` resumes) |
| `GET admin.subscriptions/me/transactions?cursor=` | any user | allowance grants and draws |
| `GET admin.subscriptions/subscriptions?status=` | admin | subscriptions |
| `POST admin.subscriptions/subscriptions/cancel` | admin | `{id, cancel}` |
| `POST admin.subscriptions/subscriptions/{id}/end` | admin | end now (see above) |
| `POST admin.subscriptions/subscriptions/{id}/refunded` | admin | `{refunded}` (default true): clears the refund-due flag; `false` sets it |
| `POST`/`GET admin.subscriptions/webhook/{provider}`, `GET …/return/{provider}` | public | plan payment callbacks |
| `GET admin.rewards/me` | any user | check-in state and streak; invite code, invited, earned |
| `POST admin.rewards/checkin` | any user | `{key_id?}` → amount and balance; 409 `checked_in` |
| `GET admin.rewards/me/referrals`, `GET …/referrals?inviter_id=` | any user, admin | referrals and their rewards |
| `GET`/`POST admin.announcements/announcements`, `PUT`/`DELETE …/{id}` | admin | announcements |
| `GET admin.announcements/me`, `POST …/me/dismiss` | any user | the live ones for the caller; `{id}` dismisses |
| `GET admin.signup/config` | public | whether invites are required, allowed domains |
| `POST admin.signup/register` | public | `{email, password, name?, invite_code?, referral_code?}` |
| `POST admin.import-newapi/run` | admin | `{driver, dsn, confirm_local_file?}` or `{data}`, `rate?`, `dry_run` (default true) → report |
