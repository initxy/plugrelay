# Admin API

English | [简体中文](api.zh-CN.md)

The management API is described in [`api/openapi.yaml`](../api/openapi.yaml)
(OpenAPI 3.1) and served under `/api/v1` by `internal/admin`. The bundled
console uses it and, on the billing pages, the sell kit's plugin routes
([sell-kit.md](sell-kit.md#plugin-api)) — there are no console-only
endpoints — so any other front end or script can do everything the console
does.
`TestOpenAPICoverage` (`internal/admin/openapi_test.go`) keeps the server and
the spec in step.

The client-facing routes — model APIs, batches, WebSocket sessions, native
passthrough and the MCP gateway — are not part of it; see
[clients.md](clients.md#what-the-relay-serves). The servers behind `/mcp` are
managed here (`/api/v1/mcp/servers`, plus `/test`).

How requests appear in the request log: MCP tool calls with protocol `mcp`,
model `<server>__<tool>` and upstream `mcp:<server>`; guardrail verdicts as
attempt diagnostic notes ([plugins](plugins.md#guardrails)); native requests
and WebSocket sessions as described in
[clients.md](clients.md#native-passthrough) and
[clients.md](clients.md#realtime-and-websocket). Batch API requests appear
with protocol `batch`; the one that first reads a batch's results carries
its usage, cost and charge (a second attempt with the batch discount in
`charge.batch_discount`). Media requests record their billable units in
`usage`: `images` (with `image_size`, `image_quality`), `audio_ms`, `chars`
and `search_units`. Pricing rules take unit prices in `prices.units` and a
batch discount in `prices.batch_discount`
([configuration](configuration.md#prices)).

## Request log analytics

- Every log entry names its client program (`client.id`, `name`, `version`)
  from the request headers — User-Agent first, then attribution headers
  such as `X-Title`/`HTTP-Referer` or `Originator`, then SDK headers;
  anything else is `other`. The rules are one table in
  `pkg/clientid` (`GET /api/v1/clients` lists it); the client is
  stored on the entry and in the hourly roll-ups, so
  `/usage/timeseries?group_by=client` and `/usage/top?by=client` read no
  log rows. A key's allowed clients (`settings.clients`) and a group's
  `allowed_clients` are these ids too, matched by the same rules.
- `GET /api/v1/logs/sessions` takes the `/logs` filters and groups the
  matching entries by `session_id` (entries without one are left out),
  over the last 30 days when neither `from` nor `to` is given; a
  log entry's `session` sums its whole session (turn, requests, charge).
- `GET /api/v1/usage/failures` counts failed requests by final cause (the
  attempt causes, plus `not_sent` when no upstream was tried).
  `/usage/timeseries?bucket=hour` gives hourly points for a
  weekday × hour view in the viewer's time zone.

### Request log export

`GET /api/v1/logs/export` takes the `/logs` filters and streams CSV, newest
first, at most 100,000 rows (narrow the time range for more), UTF-8 with a
byte-order mark. Headers are Chinese for `lang=zh-CN` (or an
`Accept-Language` starting with `zh`), else English; failure causes and
handling are written in the same language. Members get their own requests
and no cost column. Columns, in order: time (UTC, RFC 3339), request id,
client, client version, source IP, key, user, model, served-by model,
upstream, status, failure cause, handling, attempts, input / cache read /
cache write / output / reasoning tokens, first token (ms), total time (ms),
charge, upstream cost (admins), currency (amounts in currency units),
session id.

## Authentication

| caller | how | notes |
|-|-|-|
| browser | `pr_session` cookie from `POST /api/v1/auth/login` (or `POST /api/v1/setup` on first run) | HttpOnly, SameSite=Lax, Secure over HTTPS. Unsafe methods must also send `X-CSRF-Token` equal to the `pr_csrf` cookie (double submit); cross-origin browser requests are rejected. Failed logins are rate limited per client address (per /64 for IPv6), counted across nodes through the shared state backend. |
| browser, single sign-on | the same cookies, set by `GET /api/v1/auth/sso/{id}/callback` after `GET /api/v1/auth/sso/{id}/start` | full-page redirects, not fetches; errors come back as `/login?sso_error=<code>`. `GET /api/v1/auth/providers` lists what the login page offers. Provider setup and policy: [deployment](deployment.md#single-sign-on). |
| browser, second step | `POST /api/v1/auth/login` answers 202 with the methods and sets only the `pr_mfa` cookie; `POST /api/v1/auth/mfa` (TOTP, recovery code or passkey) sets the session. `POST /api/v1/auth/passkey/options` + `POST /api/v1/auth/passkey` sign in with a passkey alone (login challenges are rate limited per address: 429 `rate_limited`) | binary WebAuthn fields are base64url in the shape of `PublicKeyCredential.toJSON()`. Policy and settings: [deployment](deployment.md#two-step-sign-in-passkeys-and-captcha). |
| script / operator | `Authorization: Bearer <admin token>` | the `PLUGRELAY_ADMIN_TOKEN` environment variable, or a token created in site settings (stored hashed). Exempt from CSRF. Platform keys (`pr_…`) are refused here. |
| local owner | nothing | only while the site setting `security.local_owner_login` is on (the default; the setup wizard turns it off for "team" and "sell") and the relay listens on loopback only, for requests from loopback whose `Host` is a loopback name and that carry no forwarding headers. Acts as the first admin; `plugrelay init` without `--admin-email` creates this owner account. `GET /api/v1/me` reports `auth: none`. |

## Roles

`admin` manages the relay; `member` is a team member with own keys on shared
upstreams; `user` is an end user of a selling site. Operations are public
(setup status, login), setup-only (while no admin exists), any authenticated
role (handlers scope what non-admins see), or admin-only
(`internal/admin/auth.go`). Every change is written to the audit log
(`GET /api/v1/audit`). Disabling a user also stops their platform keys from
authenticating, until the user is enabled again.

## Conventions

- Money is integer micro-units with a currency (`{"micro": 197000,
  "currency": "CNY"}` is ¥0.197); token prices are per million tokens. No
  ratios or points: a group's `price_multiplier` is admin-only and already
  included in every price and charge a caller sees. Settings are the one
  exception: a setting with `unit: money` (features, extensions, a group's,
  user's or key's layer) is in currency units, so no form shows
  micro-units.
- Errors carry an `Error` body; validation errors list the offending fields.
- Unbounded lists (request logs, audit) page with an opaque time cursor:
  pass `next_cursor` back as `cursor`.
- Credential secrets, SSO client secrets and MCP servers' secret headers
  are write-only; upstream and
  credential state is read live from the router.
- Every test action (`POST /api/v1/upstreams/{id}/test`, `/keys/{id}/test`,
  `/mcp/servers/{id}/test`) answers 200 with `{ok, results}`: one result per
  credential, or one for a key or server; `ok` when every result passed.
- Features (`/api/v1/features`) are the switches and global settings forms;
  extensions (`/api/v1/extensions`) are plugins outside them; a group's,
  user's or key's layer is `/api/v1/{groups,users,keys}/{id}/settings`.
  Labels, help and enum names come in English and Chinese; `GET /api/v1/me`
  lists the features that are on and the console page ids the caller sees.
- `POST /api/v1/auth/login` answers 403 `password_login_disabled` while
  password sign-in is switched off (site setting `security.password_login`,
  with a single sign-on provider offered), and 403
  `captcha_required` / `captcha_failed` once an address has failed
  `security.login_captcha_after` times (send `captcha_token`).
- Sensitive changes under `/api/v1/me/` (authenticator app off, new
  recovery codes, passkeys), and an admin creating an admin
  (`POST /api/v1/users` with `role: admin`), changing any user's password
  or role, disabling them or re-enabling a disabled admin
  (`PATCH /api/v1/users/{id}`, own account included), deleting a user (`DELETE /api/v1/users/{id}`) or resetting
  their two-step sign-in (`DELETE /api/v1/users/{id}/mfa`), answer 403
  `reauth_required` unless the session signed in or called
  `POST /api/v1/me/reauth` within five minutes (operator tokens pass).
  Admin-only operations answer 403 `mfa_enrollment_required` to an admin
  past the two-step sign-in grace period, and on routes open to every user
  that admin is treated as a member (own keys and logs only); plugin
  routes (`/api/plugins/{id}/…`) see them as a member too.
- `PATCH /api/v1/settings` changing `site.currency` while prepaid balances
  exist answers 409 `balances_exist` unless `confirm_currency_change: true`.

Plugin-owned APIs mount under `/api/plugins/<id>/` with the role each route
declares ([plugins.md](plugins.md#built-in-go-plugins)). A compiled-in plugin
may mark a route public (`plugin.AdminRoute.Public`): it is served without a
login and without cookies or identity headers, and authenticates the request
itself — the sell kit's payment webhooks and sign-up
([sell-kit.md](sell-kit.md#plugin-api)); public calls are limited per route
and client address. A route marked `Reauth` needs a recent
re-authentication of the console session (403 `reauth_required`). The overview's `balance` appears
only while the `billing` feature is on.
