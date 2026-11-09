# Notifications

English | [简体中文](notifications.zh-CN.md)

The relay tells operators when something needs them, through email,
Feishu/Lark, DingTalk and WeCom group bots, or a webhook of their own; a
webhook can also receive every finished request record. It runs while the
`notifications` feature is on ([configuration](configuration.md#features));
channels and the log of what was sent are on the console's Notifications
page and under `/api/v1/notifications` ([api.md](api.md)). Code: the
`job.notify` and `sink.webhook` plugins (package docs), wiring in
`internal/app/notify.go`.

- [Events](#events)
- [When a message is sent](#when-a-message-is-sent)
- [Channels](#channels)
- [Webhook bodies and signatures](#webhook-bodies-and-signatures)
- [Request records](#request-records)
- [Addresses inside your network](#addresses-inside-your-network)

## Events

| event | when | from |
|-|-|-|
| `credential_quarantined` | a credential is taken out for good: suspected ban, unclear refusal, manual hold | router live state |
| `credential_cooling` | a credential pauses for at least `cooling_minutes` (30), or because its quota ran out | router live state |
| `credential_quota_low` | the upstream's own quota reading is at or below `quota_low_percent` (10 %) | quota probe readings |
| `credential_secret_unsaved` | a refreshed secret could not be stored (also `plugrelay_credential_secret_save_failures_total`) | router live state |
| `upstream_paused` / `upstream_recovered` | the upstream breaker opens / closes again | router live state |
| `budget_reached` | a key, user or group spend or token budget reaches `budget_percent` (80 %), and again when used up | `meter.quota` on each reservation (`plugin.NoticeHost`) |
| `extension_crashed` | an external plugin process ended unexpectedly and is restarting | the extension supervisor |

The thresholds are the feature's settings. Messages name the objects
(upstream / credential, key, user, group) and link to their console page
when the site's public URL is set; without it the link is left out.

## When a message is sent

- `job.notify` runs every 30 s on one node at a time (the core's job lock).
  It compares what is wrong now with what was wrong on its last run, kept in
  its plugin KV, so another node continues where the last one stopped; only
  changes are announced.
- Budget and extension notices are raised on any node; repeats are dropped
  on that node, then cluster-wide (shared state), and the rest wait in the
  store until the next run.
- The same event about the same object is sent at most once per
  `repeat_hours` (6); a budget threshold once per budget period.
- Everything new of one event in one run is one message listing its
  objects (ten shown, the rest counted), so twenty credentials cooling at
  once are one message.
- Each channel gets the events it subscribed to (none listed = all). A
  failed send is retried twice (1 s, 3 s) unless the receiver refused it;
  then the message stays undelivered for that channel. The log keeps the
  last 500 messages with every channel's outcome.
- `PLUGRELAY_TEST_MODE` never starts the feature's plugins (hard rule 22).

## Channels

| type | fields | notes |
|-|-|-|
| `email` | SMTP host, port, `tls` (`starttls` default, `tls`, `none`), user, password, from, to | port 0 = 587 / 465 / 25 by `tls`; a password needs TLS unless the server is local |
| `feishu` | bot address, optional signing secret | Feishu and Lark custom bots; signed as `timestamp` + `sign` in the body |
| `dingtalk` | bot address, optional signing secret | signed as `timestamp` + `sign` in the URL |
| `wecom` | bot address | the key is in the address |
| `webhook` | address, optional secret, records switch and fields | see below |

Each channel has a message language (`zh-CN` or `en`), an on/off switch and
a test action that sends a test message (and logs it). Addresses (they
carry the bots' tokens), passwords and secrets are sealed with the master
key and never returned; the API shows a masked address.

## Webhook bodies and signatures

Every post is `POST` with a JSON body and these headers:

- `X-Plugrelay-Event`: `notification` or `records`
- `X-Plugrelay-Timestamp`: Unix seconds
- `X-Plugrelay-Signature`: `sha256=` + hex HMAC-SHA256 under the channel's
  secret over `<timestamp>.<raw body>` (only when a secret is set)

A notification body has `type: "notification"`, `title`, `lines`, `text`
(both together, as chat bots get it), `link` (empty without a public URL)
and `at`. Reject a timestamp far from your clock to stop replays.

## Request records

With records on, a webhook channel also receives finished requests:
`{"type":"records","records":[…]}`, up to 100 per post, at least every 2 s.
Delivery is best effort: records wait in a queue of 2048 on each node; a
full queue drops new ones, and a post still failing after two retries is
dropped. There is no replay, so do not use it as a ledger (the request log
and `GET /api/v1/logs` are the record). Fields are the request log's
(`plugin.Record`) minus `body`; pick fewer, or add `body`, per channel
(bodies exist only while the request log keeps them).

## Addresses inside your network

Every send goes through the core egress with the SSRF guard: loopback and
private addresses are refused, for SMTP too. Allow an internal webhook or
mail server by listing its range in `egress.allow_private_cidrs`
([configuration](configuration.md#egress)), e.g.
`PLUGRELAY_EGRESS_ALLOW_PRIVATE_CIDRS=10.20.0.0/16`.
