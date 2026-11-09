# Admin API

English | [简体中文](api.zh-CN.md)

The management API lives under `/api/v1`, served by `internal/admin` and
described in [`api/openapi.yaml`](../api/openapi.yaml) (OpenAPI 3.1;
`TestOpenAPICoverage` keeps the two in step). The console uses only this API
and the sell kit's [plugin routes](sell-kit.md#plugin-api), so a script can
do anything the console does.

Client-facing routes (model APIs, batches, WebSocket, native passthrough,
`/mcp`) are listed in [clients.md](clients.md#what-the-relay-serves). The MCP
servers behind `/mcp` are managed here (`/api/v1/mcp/servers`).

How other traffic shows up in the request log:

| traffic | in the log |
|-|-|
| MCP tool calls | protocol `mcp`, model `<server>__<tool>`, upstream `mcp:<server>` |
| guardrail verdicts | attempt diagnostic notes ([plugins](plugins.md#guardrails)) |
| native requests, WebSocket | see [native passthrough](clients.md#native-passthrough), [WebSocket](clients.md#realtime-and-websocket) |
| Batch API | protocol `batch`; the request that first reads a batch's results carries its usage and charge (the batch discount in `charge.batch_discount` on a second attempt) |
| media | billable units in `usage`: `images` (with `image_size`, `image_quality`), `audio_ms`, `chars`, `search_units` |

Unit prices go in `prices.units` and the batch discount in
`prices.batch_discount` ([configuration](configuration.md#prices)).

## Request log analytics

- Each entry names its client program (`client.id`, `name`, `version`),
  read from User-Agent, then attribution headers (`X-Title`/`HTTP-Referer`,
  `Originator`), then SDK headers; anything else is `other`. The rules are a
  table in `pkg/clientid` (`GET /api/v1/clients`). The client is stored in
  the hourly roll-ups, so `/usage/timeseries?group_by=client` and
  `/usage/top?by=client` read no log rows. A key's `settings.clients` and a
  group's `allowed_clients` use the same ids.
- `GET /api/v1/logs/sessions` takes the `/logs` filters and groups entries
  by `session_id` (default: last 30 days). An entry's `session` sums its
  whole session (turns, requests, charge).
- `GET /api/v1/usage/failures` counts failed requests by final cause, plus
  `not_sent` when no upstream was tried. `/usage/timeseries?bucket=hour`
  feeds a weekday × hour chart in the viewer's time zone.

### Request log export

`GET /api/v1/logs/export` takes the `/logs` filters and streams CSV, newest
first, at most 100,000 rows, UTF-8 with a byte-order mark. Headers, failure
causes and handling are in Chinese for `lang=zh-CN` (or `Accept-Language:
zh…`), else English. Members get only their own requests and no cost column.
Columns: time (UTC, RFC 3339), request id, client, client version, source IP,
key, user, model, served-by model, upstream, status, failure cause, handling,
attempts, input / cache read / cache write / output / reasoning tokens, first
token (ms), total time (ms), charge, upstream cost (admins), currency, session
id.

## Authentication

| caller | how | notes |
|-|-|-|
| browser | `pr_session` cookie from `POST /api/v1/auth/login` (or `POST /api/v1/setup` on first run) | HttpOnly, SameSite=Lax, Secure over HTTPS. Unsafe methods also send `X-CSRF-Token` equal to the `pr_csrf` cookie; cross-origin requests are rejected. Failed logins are limited per address (per /64 for IPv6), across nodes. |
| browser, single sign-on | the same cookies, set by `GET /api/v1/auth/sso/{id}/callback` after `GET /api/v1/auth/sso/{id}/start` | full-page redirects; errors return to `/login?sso_error=<code>`. `GET /api/v1/auth/providers` lists the login options. Setup: [deployment](deployment.md#single-sign-on). |
| browser, second step | `POST /api/v1/auth/login` answers 202 with the methods and sets only `pr_mfa`; `POST /api/v1/auth/mfa` (TOTP, recovery code or passkey) sets the session. Passkey-only: `POST /api/v1/auth/passkey/options` + `POST /api/v1/auth/passkey` (429 `rate_limited` per address) | WebAuthn binary fields are base64url, shaped like `PublicKeyCredential.toJSON()`. Policy: [deployment](deployment.md#two-step-sign-in-passkeys-and-captcha). |
| script / operator | `Authorization: Bearer <admin token>` | `PLUGRELAY_ADMIN_TOKEN`, or a token created in site settings (stored hashed). No CSRF check. Platform keys (`pr_…`) are refused. |
| local owner | nothing | only while `security.local_owner_login` is on (default; the setup wizard turns it off for "A team" and "Selling"), the relay listens on loopback only, and the request comes from loopback with a loopback `Host` and no forwarding headers. Acts as the first admin (the account `plugrelay init` creates without `--admin-email`); `GET /api/v1/me` reports `auth: none`. |

Sign-in errors from `POST /api/v1/auth/login`: 403 `password_login_disabled`
while `security.password_login` is off (with an SSO provider offered); 403
`captcha_required` / `captcha_failed` once an address has failed
`security.login_captcha_after` times (send `captcha_token`).

## Roles

`admin` manages the relay; `member` is a team member with their own keys on
shared upstreams; `user` is an end user of a selling site. Each operation is
public, setup-only (while no admin exists), open to any signed-in role
(handlers limit what non-admins see), or admin-only
(`internal/admin/auth.go`). Every change goes to the audit log
(`GET /api/v1/audit`). A disabled user's platform keys stop working until
the user is enabled again.

These answer 403 `reauth_required` unless the session signed in or called
`POST /api/v1/me/reauth` in the last five minutes (operator tokens pass):
sensitive changes under `/api/v1/me/` (turning off the authenticator app, new
recovery codes, passkeys); creating an admin (`POST /api/v1/users` with
`role: admin`); changing a user's password or role, disabling them, or
re-enabling a disabled admin (`PATCH /api/v1/users/{id}`, own account
included); `DELETE /api/v1/users/{id}`; `DELETE /api/v1/users/{id}/mfa`.

An admin past the two-step sign-in grace period gets 403
`mfa_enrollment_required` on admin-only operations and is treated as a member
elsewhere, plugin routes included.

## Conventions

- Money is integer micro-units with a currency (`{"micro": 197000,
  "currency": "CNY"}` is ¥0.197); token prices are per million tokens. No
  ratios or points: a group's `price_multiplier` is admin-only and already in
  every price and charge a caller sees. Exception: settings with `unit:
  money` are in currency units.
- Errors have an `Error` body; validation errors list the fields.
- Unbounded lists (request logs, audit) page with a cursor: pass
  `next_cursor` back as `cursor`.
- Credential secrets, SSO client secrets and MCP secret headers are
  write-only. Upstream and credential state is read live from the router.
- Test actions (`POST /api/v1/upstreams/{id}/test`, `/keys/{id}/test`,
  `/mcp/servers/{id}/test`) answer 200 with `{ok, results}`: one result per
  credential, or one for a key or server.
- `/api/v1/features` holds the feature switches and global settings forms;
  `/api/v1/extensions` the plugins outside them; a group's, user's or key's
  layer is `/api/v1/{groups,users,keys}/{id}/settings`. Labels and help come
  in English and Chinese. `GET /api/v1/me` lists the features that are on and
  the console pages the caller sees.
- `PATCH /api/v1/settings` changing `site.currency` while prepaid balances
  exist answers 409 `balances_exist` unless `confirm_currency_change: true`.
- The overview's `balance` appears only while the `billing` feature is on.

Plugin APIs mount under `/api/plugins/<id>/` with the role each route
declares ([plugins](plugins.md#built-in-go-plugins)). A compiled-in plugin
may mark a route public (`plugin.AdminRoute.Public`): no login, no cookies
or identity headers, the plugin authenticates the call itself (the sell
kit's payment webhooks and sign-up), and calls are limited per route and
address. A route marked `Reauth` needs a recent re-authentication (403
`reauth_required`).
