# Notifications

English | [简体中文](notifications.zh-CN.md)

The relay tells operators when something needs them, by email, Feishu/Lark,
DingTalk or WeCom group bot, or your own webhook; a webhook can also receive
every finished request. It runs while the `notifications` feature is on
([configuration](configuration.md#features)). Channels and the sent log are
on the console's Notifications page and under `/api/v1/notifications`
([api.md](api.md)). Code: the `job.notify` and `sink.webhook` plugins, wired
in `internal/app/notify.go`.

- [Events](#events)
- [When a message is sent](#when-a-message-is-sent)
- [Channels](#channels)
- [Webhook bodies and signatures](#webhook-bodies-and-signatures)
- [Request records](#request-records)
- [Addresses inside your network](#addresses-inside-your-network)

## Events

| event | when |
|-|-|
| `credential_quarantined` | a credential is taken out for good: suspected ban, unclear refusal, manual hold |
| `credential_cooling` | a credential pauses for at least `cooling_minutes` (30), or its quota ran out |
| `credential_quota_low` | the upstream's own quota reading is at or below `quota_low_percent` (10 %) |
| `credential_secret_unsaved` | a refreshed secret could not be stored (also counted in `plugrelay_credential_secret_save_failures_total`) |
| `upstream_paused` / `upstream_recovered` | the upstream's breaker opens / closes |
| `budget_reached` | a key, user or group spend or token budget reaches `budget_percent` (80 %), and again when used up |
| `extension_crashed` | an external plugin process died and is restarting |

The thresholds are the feature's settings. Messages name the upstream,
credential, key, user or group, with a link to its console page when the
site's public URL is set.

## When a message is sent

- `job.notify` runs every 30 s on one node at a time. It compares what is
  wrong now with its last run (kept in plugin KV, so any node can take over)
  and announces only changes. Budget and extension notices can come from any
  node and are deduplicated across the cluster.
- The same event for the same object is sent at most once per
  `repeat_hours` (6); a budget threshold once per budget period.
- One run's new cases of one event become one message (ten objects listed,
  the rest counted), so twenty credentials cooling at once is one message.
- Each channel gets the events it subscribed to (none selected = all). A
  failed send is retried twice (after 1 s and 3 s) unless the receiver
  refused it. The sent log keeps the last 500 messages with each channel's
  result.
- With `PLUGRELAY_TEST_MODE` set, these plugins never start.

## Channels

| type | fields | notes |
|-|-|-|
| `email` | SMTP host, port, `tls` (`starttls` default, `tls`, `none`), user, password, from, to | port 0 = 587 / 465 / 25 by `tls`; a password needs TLS unless the server is local |
| `feishu` | bot address, optional signing secret | Feishu and Lark custom bots; `timestamp` + `sign` in the body |
| `dingtalk` | bot address, optional signing secret | `timestamp` + `sign` in the URL |
| `wecom` | bot address | the key is in the address |
| `webhook` | address, optional secret, records switch and fields | see below |

Each channel has a message language (`zh-CN` or `en`), an on/off switch and a
test button (the test is logged too). Addresses (they carry bot tokens),
passwords and secrets are sealed with the master key and never returned; the
API shows a masked address.

## Webhook bodies and signatures

Every post is a JSON `POST` with these headers:

- `X-Plugrelay-Event`: `notification` or `records`
- `X-Plugrelay-Timestamp`: Unix seconds
- `X-Plugrelay-Signature`: `sha256=` + hex HMAC-SHA256 of
  `<timestamp>.<raw body>` with the channel's secret (only when a secret is
  set)

A notification body has `type: "notification"`, `title`, `lines`, `text`
(title and lines together, as chat bots get it), `link` (empty without a
public URL) and `at`. Reject timestamps far from your clock to stop replays.

## Request records

With records on, a webhook channel also gets finished requests:
`{"type":"records","records":[…]}`, up to 100 per post, at least every 2 s.
Fields are the request log's (`plugin.Record`) without `body`; each channel
can pick fewer or add `body` (present only while the request log keeps
bodies).

Delivery is best effort: each node queues up to 2048 records and drops new
ones when full, and a post still failing after two retries is dropped. There
is no replay, so don't use it for billing; the request log and
`GET /api/v1/logs` are the record.

## Addresses inside your network

Every send goes through the core egress with the SSRF guard, so loopback and
private addresses are refused, SMTP included. To allow an internal webhook
or mail server, add its range to `egress.allow_private_cidrs`
([configuration](configuration.md#egress)), e.g.
`PLUGRELAY_EGRESS_ALLOW_PRIVATE_CIDRS=10.20.0.0/16`.
