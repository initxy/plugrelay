# Sell kit

English | [简体中文](sell-kit.zh-CN.md)

Optional plugins for running plugrelay as a paid service: a prepaid
balance, redeem codes, payments (Stripe; Alipay and WeChat Pay through an
epay gateway) with coupons, subscription plans, daily check-in and
referral rewards, announcements, self-service sign-up and an importer for
new-api / one-api sites. They are compiled into every build and each one
stays off until enabled. Money
is always real money in the site currency (`site.currency`), in integer
micro-units; there are no points or ratios.

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

The plugins are linked in `plugins/builtin/builtin.go` with the other
built-in plugins; the core's side of them is `internal/app/sellkit.go`,
`internal/app/sellkit_subscriptions.go` and `internal/app/operations.go`.
The kit is two features
([configuration](configuration.md#features)): `billing` (`meter.balance`,
`admin.payment`, `admin.redeem`, `admin.subscriptions`,
`admin.import-newapi`, `admin.rewards`, `admin.announcements`) and
`signup` (`admin.signup`). Answering "sell" in
the setup wizard turns `billing` on; otherwise switch them on in the
console's features page or with `plugrelay apply`:

```yaml
features:
  - {id: billing, enabled: true}
  - {id: signup, enabled: true}
```

Settings are set in each feature's form, amounts in the site currency;
secrets (Stripe and epay keys, invite codes) are stored encrypted with the
master key.

## Prepaid balance: `meter.balance`

Every request draws on the balance of its payer:

| `payer` | the account charged |
|-|-|
| `user` (default) | the key's user; a key without a user pays from its own account (`key:<id>`) |
| `group` | the key's group (one shared balance), else the key's user |
| `key` | the key itself |

Top-ups, redeems and the console's balance go to the account the requests
draw on. With `payer=key` (and with `payer=group` when a user's key sits in
another group than the user) the console asks which key; the API takes
`key_id`, which must be one of the caller's keys, and answers
`400 key_required` without one when keys pay. `GET meter.balance/me/payers`
lists the choices. With `payer=key` the overview and user list show no
per-user balance, since there is none.

Requests through `/native/…` ([native passthrough](adr/0007-native-passthrough-model.md))
run no request plugins: they are neither held nor charged against a
balance. Grant native access only to keys you do not bill by balance.

1. **Reserve.** The core prices the request's input estimate and output
   limit at the caller's rate (list price × group multiplier, the same rate
   it bills with). The plugin holds that amount, or `min_reserve_micro` if
   larger, in one database transaction that refuses when the available
   balance (balance − holds) plus the account's credit limit does not cover
   it or is not positive.
2. **Settle.** A delivered response is charged exactly the core's charge
   (`plugin.Charge` on the request log's delivered attempt) and the hold is
   released, as one `charge` transaction whose id is `charge:<request id>`.
3. **Refund.** A request that failed, or a stream the upstream broke, only
   releases its hold.

Settle and refund are idempotent per request id: the hold row is the
marker, whichever runs first wins, and the charge id is unique, so a retry
or a duplicate settle never charges twice (ADR-0012's at-least-once rule).
A settle that still fails after three quick retries (the database was
briefly unreachable) is not lost: the request log record, written
at-least-once after it, carries the same charge, and the sweep (at most
once a minute per node) settles every hold older than a minute whose
request is logged, from the record. That is why `meter.balance` requires
`sink.requestlog`.
The final charge is never cut short: a request whose output ran past its
estimate (a client that sends no output limit) can take the balance below
zero once by the uncovered part, and the next request is refused. Set
`min_reserve_micro` to narrow that, or give an account a credit limit to
allow going below zero on purpose. Holds older than `hold_ttl` (6h; a node
died mid-request) with no request log record are released by the sweep; a
record or settle that arrives within a day after that still charges.

With `meter.quota` on as well, both run: budgets first, then the balance,
and a refusal by either undoes the other's reservation.

[Batches](clients.md#batches) are held and charged across requests:

- **Hold at creation.** Creating a batch (`POST /v1/batches`, `POST
  /v1/messages/batches`) holds its estimate under `batch:<request id>`:
  every request's prompt (a token per three bytes of its body) plus its
  output limit (`max_tokens`, `max_completion_tokens` or
  `max_output_tokens`; 4096 when it names none), at the batch price the
  results will be charged at. An OpenAI input file is estimated when it is
  uploaded. A balance that does not cover it refuses the batch as above.
- **Charge at the results.** When the results are read, by the client or
  by the relay (below), they are charged to the batch's creator in full
  under the same id, also when the hold lapsed or the balance ran out
  meanwhile (it goes below zero, like an overrun), once per batch. A charge
  that fails leaves the batch unbilled for the next read.
- **Release.** A batch that ends with nothing to bill (failed, expired or
  cancelled before any request ran) has its hold released when the relay
  next sees it. Batch holds outlast `hold_ttl` by three days.

Out of balance, clients get their protocol's own "out of credit" signal,
never a retryable one:

| client protocol | status | body |
|-|-|-|
| Anthropic | 402 | `billing_error` (Anthropic's documented billing error type) |
| OpenAI chat / responses / completions | 429 | `insufficient_quota` (what OpenAI returns when credit runs out) |
| Gemini | 429 | `RESOURCE_EXHAUSTED` |

All carry `x-should-retry: false` where the protocol has it, and the message
says what is available and what the request needed.

| setting | default | meaning |
|-|-|-|
| `payer` | `user` | see above (global) |
| `exempt` | false | serve without a balance; set on a group, user or key for staff |
| `min_reserve_micro` | 0 | smallest hold per request |
| `balance_header` | empty | response header with the available balance (decimal), e.g. `x-plugrelay-balance` |
| `low_balance_micro` | 0 | send that header only below this amount; 0 = always |
| `hold_ttl` | 6h | holds without a request log record are released after this |

Every balance change is a transaction: `topup` (payment), `checkin`,
`referral` ([below](#coupons-check-in-referrals-announcements)), `charge`
(request), `refund` (an admin giving back a request's charge, at most once
and at most the charge), `redeem` (code), `adjustment` (admin, positive or
negative, idempotent per form submission), `subscription` (a plan period:
its price when paid from the balance, zero when paid at a provider). Plan
allowances are `allowance` transactions on their own subject
([below](#subscription-plans-adminsubscriptions)). A negative adjustment never
takes an account past its credit limit.

`site.currency` is the unit of every stored amount. Changing it while any
account holds a balance is refused (`409 balances_exist`) unless the
settings request carries `"confirm_currency_change": true`; balances are
not converted, so adjust them by hand afterwards.

## Redeem codes: `admin.redeem`

Admins generate batches (1–1000 codes, amount, uses per code, optional
expiry and batch name). Codes look like `RC-XXXX-XXXX-XXXX-XXXX-XXXX`
(100 random bits); they are shown once, and only their SHA-256 is stored.
Users redeem in the console; case and dashes do not matter. A redeem is one
database transaction (valid, not expired, not disabled, under `max_uses`,
not yet used by this user, then credit), so concurrent redeems never
pass the limit. A code counts once per user, whichever account pays: with
`payer=key` a second key of the same user cannot redeem it again. Failed
attempts are limited per user
(`failures_per_hour`, default 10); each attempt takes a slot before it is
checked, so concurrent guesses cannot get past the limit.

## Payments: `admin.payment`

Users choose an amount (presets and limits in the settings) and a payment
method (only configured providers that charge in the site currency are
offered) and pay at the provider; the provider's signed callback credits
the balance. Two providers are built in.

Return and callback URLs are built from the site's public URL (the site
setting `site.public_base_url`), never from the request's `Host` header;
while it is unset, checkout answers `503 not_configured`. A provider's own error message is logged, and users
get a generic `502 upstream`. The public callback routes of every built-in
plugin (`webhook`, `return`, sign-up) are limited to 600 calls per route
and client address per five minutes (`429 rate_limited`; providers retry
later), so a sign-up flood does not hold back payment notices. Behind a
reverse proxy, list it in `trusted_proxies`
([deployment](deployment.md)): otherwise every call comes from the proxy's
address and shares its limit, and a flood from anyone blocks the payment
notices.

An event id is remembered (a week) only once it credited: a notice that
credits nothing (not paid yet, an amount that does not match the order, an
unknown order) is acknowledged but not remembered, so a later notice under
the same id still credits.

### Stripe

1. In the Stripe dashboard, add a webhook endpoint
   `https://<relay>/api/plugins/admin.payment/webhook/stripe` for
   `checkout.session.completed` and `checkout.session.async_payment_succeeded`.
2. Set `stripe_secret_key` (`sk_…`) and `stripe_webhook_secret` (`whsec_…`).

A top-up is an order (table `payment_orders`, listed on the Orders page
with plan orders); Checkout is created with the order id as
`client_reference_id` and metadata and charged in the currency's minor unit
as [Stripe documents it](https://docs.stripe.com/currencies): cents for
two-decimal currencies; whole units for zero-decimal ones such as JPY;
ISK and UGX as two-decimal amounts ending in `00`; BHD, JOD, KWD, OMR and
TND in thousandths with the last digit 0. An amount with a fraction the
currency cannot charge, or a site currency Stripe does not list, is
refused. Automatic tax and promotion codes are switched off on the
session: the paid total must equal the order. The webhook is public and
authenticated only by the `Stripe-Signature` header: HMAC-SHA256 of
`<t>.<body>` with the endpoint secret against every `v1` value, and `t`
within five minutes, as Stripe documents. An event whose paid amount or
currency differs from the order is logged and not credited. Crediting is
the transaction `topup:<order>`, so a replayed event, a duplicate delivery
or a second event for the same payment credit once.

### epay (易支付: Alipay, WeChat Pay)

Many Chinese gateways (彩虹易支付 and its clones) take Alipay and WeChat Pay
through the epay merchant protocol
([its documentation](https://pay.yzfpay.com/doc/v1_legacy_api.html); field
names checked against the open-source 彩虹易支付 code and new-api's client).
Set `epay_url` (the gateway, under which `submit.php` and `mapi.php` live),
`epay_pid` and `epay_key`; `epay_types` lists the methods
offered (default `alipay`, `wxpay`). Nothing needs to be registered at the
gateway: every order carries its own `notify_url`
(`<public URL>/api/plugins/admin.payment/webhook/epay`) and `return_url`
(`…/return/epay`).

- **Order.** `epay_mode: submit` (default) sends the user to `submit.php`
  with the signed order; `mapi` creates it server-side at `mapi.php` (with
  the client's address) and sends the user to the `payurl` it answers
  (else its `qrcode` or `urlscheme` link). Fields: `pid`, `type`,
  `out_trade_no` (the order id), `notify_url`, `return_url`, `name`
  (`epay_product_name`), `money` in yuan with two decimals, `sign`,
  `sign_type=MD5`.
- **Signature.** Every parameter except `sign`, `sign_type` and empty
  ones, sorted by name, joined as `k=v&k=v` without URL-encoding, the key
  appended directly, MD5 in lowercase hex. Because values are signed
  unencoded, a value containing `&` is refused.
- **Notify.** The gateway calls `notify_url` with GET (POST forms are read
  too). The signature must match, `pid` must be ours and `trade_status`
  `TRADE_SUCCESS`; the order must exist and the paid `money` equal it
  (compared as a number: gateways drop trailing zeros). The reply is
  `success`, also for a duplicate, an unknown order or a mismatched
  amount, which are logged and not credited (and do not stop a later
  `TRADE_SUCCESS` under the same `trade_no` from crediting); a bad
  signature gets `fail` (400). Crediting is `topup:<order>`, so duplicate
  notifies, a replay under another `trade_no` and the user's return credit
  once.
- **Return.** The user comes back to `return_url` with the same signed
  parameters; a valid one credits the order the same way (useful when the
  gateway cannot reach the relay), then redirects to the wallet page.
- **Currency.** epay has no currency field and charges yuan: it is offered
  only when `site.currency` is `CNY`, and a checkout in any other currency
  is refused (no conversion). Amounts must be whole fen.
- **Refunds.** The epay protocol has no refund call and no refund notice:
  refund in the gateway's merchant console, then take the money back in
  the relay by hand — a negative `adjustment` for a top-up; for a plan,
  "End now" on the Plans page (and "Mark refunded" on a flagged one).

### Other providers

Other providers are a `payment.Provider` — `Checkout` (return a URL to send
the user to) and `Verify` (authenticate a callback and say which order it
paid); `FormProvider` for gateways that call back with form parameters
and want their own reply, `MethodProvider` and `CurrencyProvider` to offer
several methods or limit currencies, `RecurringProvider` to bill plans
every period — registered with `payment.RegisterProvider` from a package
imported in `plugins/builtin/builtin.go`; `plugins/admin/payment/stripe` and
`plugins/admin/payment/epay` are the references. Their callback is
`/api/plugins/admin.payment/webhook/<name>`.

## Subscription plans: `admin.subscriptions`

Admins define plans on the Plans page: a price per period (week, month or
year) in the site currency, an allowance per period — money (requests are
counted at what they would be charged) or tokens (input, cached and
output) — and what happens past it: `block` refuses requests with the same
out-of-credit errors as an empty balance (the message says the allowance is
used up and when it renews), `balance` lets the prepaid balance pay. A plan
may name a group: subscribers are moved into it (with their keys that were
in their own group) while subscribed and moved back when the subscription
ends, so the group's model allowlist, rate limits and price multiplier
apply ([settings layers](configuration.md#layered-plugin-settings)). Moving
back happens only for a user still in the plan group (and only the keys
still in it): one an admin moved elsewhere meanwhile stays there. With
`payer=group` a plan group would change which group's balance pays, so
saving a plan with a group, and subscribing to one, is refused (`409
payer_group`) while groups pay, and switching `meter.balance` to
`payer=group` is refused (409) while a plan or a live subscription has a
group, on the settings page as in `plugrelay apply` and the new-api
import; so is turning the billing feature on over such a stored setting.
A disabled plan takes no new subscribers;
existing ones keep it.
Each subscription copies its plan's terms, so editing a plan changes only
later subscriptions.

The check runs on every `meter.balance` save, so a store that holds
`payer=group` together with a plan or live subscription that has a group
refuses every edit. To get out: change `payer` in
the same save, remove the group from the plan, or end the offending
subscriptions (`POST /api/plugins/admin.subscriptions/subscriptions/{id}/end`,
which ends the local subscription even when the payment provider refuses).

A user has one live (pending or active) subscription, enforced in the
database (a unique index, and the user's row locked by every change of
which subscription is live). Subscribing again while one is active is
refused (`409 already_subscribed`); a pending one (a checkout left
unpaid) is ended by subscribing again. A payment that still arrives for a
subscription that cannot start because another is active or its user
was deleted (the unpaid checkout paid after all, a late Stripe
`invoice.paid` for a plan the user left) is recorded (`subscription:<provider>:<id>`) but starts nothing: the
subscription is flagged **refund due** on the Plans page and in the log,
and a Stripe one is set to cancel so it bills no further period. Refund it
at the provider, then "Mark refunded". Paying from the balance never takes
the price in that case. Ways to pay, offered when configured:

| method | first period | renewal |
|-|-|-|
| prepaid balance | the price is taken from the balance (`subscription` transaction), refused when it does not cover it | at the period end, from the balance; a balance that does not cover it ends the subscription |
| Stripe | Checkout in subscription mode | Stripe bills every period; each `invoice.paid` starts the next |
| epay (Alipay, WeChat Pay) | one payment for one period | from the balance as above (epay has no recurring billing) |

Cancelling (the user's plan page, or an admin) ends the subscription at
the period end instead of renewing; for Stripe it is set at Stripe first
(`cancel_at_period_end`). There is no proration: to switch plans, cancel
and subscribe again after the period ends. An admin's **End now** (after a
refund or a chargeback) ends it at once: the allowance stops being drawn
on, the user leaves the plan group, a Stripe subscription is set to cancel
at Stripe first (a refusal there is logged; it ends here anyway); the
money is not refunded by the relay. The core's audit
log records the call with the subscription id. End now and Mark refunded
need a recent re-authentication, as deleting a user does. Deleting a user
ends the user's live subscription first, a Stripe one set to cancel at
Stripe; when it cannot be (Stripe refuses, the provider is not configured,
or a checkout left pending has no Stripe subscription yet),
the deletion is refused (409) so that it does not bill on: End now, then
delete, or delete with `?force=true` (`DELETE /api/v1/users/{id}`), which
ends it here only. A payment that arrives for it later is flagged refund
due as above.

Periods run from their start to the same day of the next week, month or
year; a day the target month lacks is clamped to its last day (Jan 31 plus
a month is Feb 28 or 29, and the next period runs from there). Renewals
and expiries run in `meter.balance`'s sweep (at most once a minute per
node, while requests come in), never on a page view; one subscription that
fails to renew is logged and the others still renew. A balance renewal
decides "renew" or "end for lack of money" in one locked transaction with
the subscription, so a period paid meanwhile (an epay order) is never
ended by it.

**Metering** is `meter.balance`'s, in the same database transactions as
the balance. A user with a current subscription draws on its allowance
first. A request holds its estimate in the plan's unit against what is
left — money: the balance's reservation (input estimate and output limit
at the caller's rate, at least `min_reserve_micro`); tokens: the input
estimate plus the output limit — at least 1 and at most what is left, so
concurrent requests see the allowance taken. With nothing left, `block`
refuses and `balance` sends the request to the balance as without a plan.
A token plan covers requests priced by the token: image generation and
edits, speech, transcription and rerank (which a price may charge per
unit) go to the balance as without a plan, under either policy, and a
request that reports no tokens but costs money pays all of it from the
balance.
With `balance`, a request whose estimate is larger than what is left is
covered only when the payer's available balance (plus credit limit) covers
the rest; otherwise it is held on the balance in full. A request settles
exactly once per request id like a balance hold: its units (money: the
charge; tokens: input, cached and output tokens) come from what is left,
never past the allowance, and the rest is charged to the payer's balance
(`charge:<request>`) as money — tokens converted at the request's own
rate (charge ÷ tokens). That holds for `block` too: a request that ran past
its estimate (a client that sends no output limit, or a low
`min_reserve_micro`) pays the overrun from the balance, below zero if need
be. That negative balance is a debt: the next top-up pays it first, and a
balance renewal needs the price on top of it. Draws are `allowance` transactions on the
subject `sub:<subscription>` (`allowance:<request>`, `balance_after` = what
is left; each period starts with a grant), so the plan page shows used and
left. Failed settles are collected from the request log and stale holds
released by the same sweep as balance holds. Batches are billed to the
balance, never to an allowance.

**Stripe.** In Stripe, add a second webhook endpoint
`https://<relay>/api/plugins/admin.subscriptions/webhook/stripe` for
`invoice.paid`, `customer.subscription.updated`,
`customer.subscription.deleted`, `charge.refunded` and
`charge.dispute.created`, and set its signing secret as
`stripe_subscription_webhook_secret` on admin.payment (keys stay there).
Checkout is created with `mode=subscription`, recurring `price_data` and
the subscription id as `client_reference_id`, `metadata` and
`subscription_data[metadata]`. As Stripe advises
([subscription webhooks](https://docs.stripe.com/billing/subscriptions/webhooks)),
access follows `invoice.paid`: the subscription line's period starts once
per invoice id (`subscription:stripe:<invoice>`), read from both payload
shapes (`parent.subscription_details` since API version
`2025-03-31.basil`, top-level `subscription` before); an invoice whose
paid amount or currency differs from the price is logged and starts
nothing. `customer.subscription.updated` mirrors `cancel_at_period_end` and
ends the subscription when the status is `canceled`, `unpaid` or
`incomplete_expired`; `customer.subscription.deleted` ends it. A Stripe
subscription with no renewal three days after its period ends is ended
locally (a later `invoice.paid` starts it again). Events are signed and
deduplicated as for top-ups. A full refund (`charge.refunded` with
`refunded: true`) or a new dispute (`charge.dispute.created`) of a
subscription invoice ends that subscription now and sets it to cancel at
Stripe; a refund of a payment flagged refund due clears the flag. The
invoice is found as Stripe documents for refunds and disputes: the
charge's `invoice` before `2025-03-31.basil`, else the PaymentIntent's
paid invoice payments (`GET /v1/invoice_payments`), then the invoice's
subscription. A partial refund changes nothing; a dispute that is won
does not start the subscription again — subscribe again.

**epay.** A plan bought through epay is an order like a top-up whose
`notify_url` and `return_url` point at `/api/plugins/admin.subscriptions/`;
the checks are the same, and the period starts once per order
(`subscription:epay:<order>`).

## Coupons, check-in, referrals, announcements

Each has its own switch in the billing feature's form, off by default.

- **Coupons** (`admin.payment`, `coupons_enabled`): codes made on the
  Coupons page take a percent (capped by an amount if set) or a fixed
  amount off a top-up, within a time window, a minimum top-up, a total
  number of uses and uses per user. The provider charges the discounted
  price, rounded down to whole cents, and the full amount is credited. A
  use is taken at checkout in one transaction with the coupon row locked,
  so concurrent checkouts never pass the limits. While unpaid it holds its
  place for an hour, also after the user returns with "canceled"
  (`POST admin.payment/orders/{id}/cancel`), since the provider's page may
  still be paid; it is given back at once only when the provider could
  expire the checkout (Stripe). It is kept once paid, also when the payment
  arrives after a cancel; an order paid more than an hour after checkout is
  credited what was paid, without the discount.
- **Daily check-in** (`admin.rewards`, `checkin_enabled`): once per user
  and day in the site time zone (`site.timezone`, default UTC), a random
  amount between the lowest and highest reward, in whole cents, as the
  transaction `checkin:<user>:<day>`; a second check-in that day changes
  nothing (409 `checked_in`).
- **Referral rewards** (`admin.rewards`, `referral_enabled`): every member
  has an invite code; `/signup?ref=<code>` records the inviter (an invitee
  keeps their first inviter, nobody invites themselves). On the invitee's
  first paid top-up the inviter gets a percent of what was paid or a fixed
  amount, never past `referral_cap_micro` in total (0 = no cap), as
  `referral:<invitee>` to the account the inviter's requests pay from
  (nothing with `payer=key`). The referral row is locked and marked in the
  same transaction as the credit, so duplicate callbacks and concurrent
  top-ups pay once and stay within the cap.
- **Announcements** (`admin.announcements`, `enabled`): Markdown notices
  with an optional start and end and an audience (everyone, members,
  admins), shown as a dismissible banner at the top of the console. They
  live in the plugin's KV, at most 200.

Check-ins and referral rewards are balance changes outside the meter, so
`TestPluginsCannotChangeBalances` lists their ledger calls (`CheckIn`,
`RewardReferral`) with the only plugins that may make them.

## Sign-up: `admin.signup`

Off unless enabled: then the login page offers "Create an account" and
`/signup` registers an end user (role `user`, email marked unverified) by email and password
(at least 10 characters), optionally requiring one of `invite_codes`
(`invite_required`) and an email domain on `allowed_domains` (the address
is not verified, so the domain list only filters what people type; restrict
by organization with single sign-on instead). New users join
`default_group` and log in normally; they create their own keys in the
console. Attempts are limited per client address (`per_ip_per_hour`,
default 5). A captcha is off by default: set `captcha_provider` to
`turnstile` (Cloudflare) or `hcaptcha` with `captcha_site_key` and
`captcha_secret` (a secret setting, sealed), and the form shows the widget
and `/register` verifies its token with the provider's siteverify endpoint
through the core egress — missing token 403 `captcha_required`, rejected 403
`captcha_failed`, provider unreachable 503 `captcha_unavailable` (fails
closed; a provider chosen without both keys answers the same instead of
skipping the check). There is no email verification. Because nobody checked the address,
SSO `link_by_email` never links an identity to a self-registered (or
imported) account: whoever signed up first with someone's address cannot
take over that person's SSO login. The IdP user gets `account_exists`; an
admin links them by hand or deletes the squatting account.

## Import from new-api: `admin.import-newapi`

Reads a new-api ([QuantumNous/new-api](https://github.com/QuantumNous/new-api),
schema as of commit `2de51a19`: `model/channel.go`, `model/token.go`,
`model/user.go`, `model/option.go`) or one-api database — MySQL, PostgreSQL
or SQLite DSN — or a JSON export of the same rows
(`{"channels": [...], "tokens": [...], "users": [...], "options": {...}}`
with new-api's column names). Every run starts as a dry run that lists what
would be created, updated, left alone or skipped and why; nothing is written
until you import. Rows keep their new-api ids (`newapi-c7`, `newapi-u3`,
`newapi-t12`, `newapi-<group>`), so running it again changes nothing that is
already there and credits nothing twice.

| new-api | plugrelay |
|-|-|
| channel | an upstream from the matching manifest (OpenAI, Anthropic, Gemini, DeepSeek, Moonshot, Zhipu, Qwen, Volcengine, MiniMax, Mistral, xAI, OpenRouter, Bedrock, Ollama, vLLM, SGLang, Xinference, custom / SiliconFlow as OpenAI-compatible); an OpenAI channel with another base URL becomes OpenAI-compatible with that URL; other types are skipped with the reason |
| channel keys (one per line) | one shared credential each; Bedrock `AK\|SK\|region` becomes `AK:SK` and the region |
| channel models, model mapping, group, priority, weight | models with routes to the upstream model, restricted to those groups |
| group (and `GroupRatio`) | a group with that price multiplier |
| user | a user (every role, root and admin included, → user: promote admins by hand once they have set a new password); login is the email, else the username, marked unverified; disabled stays disabled |
| user quota | an `adjustment` credit: quota ÷ `QuotaPerUnit` USD, times the rate you give when the site currency is not USD |
| token | a platform key of that user, with its expiry, group, model limits and IP allowlist |

Keys carry over. plugrelay never stores a key, only its SHA-256; new-api
stores tokens in plaintext and clients send `sk-` plus the stored 48
characters, so the import stores SHA-256 of exactly that and clients keep
working. `KeyByToken` accepts that shape besides plugrelay's own `pr_` keys.
Passwords carry over when they are bcrypt (both relays use it); newer
new-api argon2 hashes cannot, and those users get a warning and must have a
new password set. Not carried: per-token quota limits (reported; use a
`meter.quota` key budget), logs, top-up history, redemption codes, and
channel types without a manifest (Midjourney, Suno, Azure, …).

The importer connects to whatever DSN an admin gives it, so only admins can
run it, and the DSN cannot read the relay's own files: a MySQL DSN is
parsed with `allowAllFiles` forced off (a DSN asking for it or for
`local_infile` is refused), and a SQLite source must be a plain path to an
existing file, opened read-only, with `"confirm_local_file": true` (a
checkbox in the console).

## Console

The console has fixed pages under `/billing`, shown while the billing
feature is on (the sign-up page while sign-up is on).

| plugin | end users | admins |
|-|-|-|
| `meter.balance` | balance on the overview and the keys page; Wallet: balance and history | Balances: every account, adjust, credit limit, history |
| `admin.payment` | Wallet: top up, with a coupon | Orders: every top-up and plan payment; Coupons |
| `admin.subscriptions` | My plan: current plan, allowance used and left, renewal date, subscribe, cancel, allowance history | Plans: define plans, list subscriptions (refund due flagged), cancel at period end, end now, mark refunded |
| `admin.redeem` | Wallet: redeem a code | Redeem codes: batches, generate, export, disable, who redeemed |
| `admin.rewards` | Wallet: check in, invite link and earnings | — (the ledger shows every reward) |
| `admin.announcements` | banner on every page | Announcements |
| `admin.signup` | sign-up page and login link | — |
| `admin.import-newapi` | — | Import from new-api |

## Plugin API

Under `/api/plugins/<id>/` ([admin API](api.md)); `admin` routes need the
admin role, public routes need no login.

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
