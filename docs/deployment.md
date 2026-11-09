# Deployment

English | [简体中文](deployment.zh-CN.md)

One binary for every setup. Pick the smallest that fits:

| setup | listen | store / state | console |
|-|-|-|-|
| one person, a laptop | `127.0.0.1:8787` | SQLite / memory or sql | no login on loopback |
| a team, one VM | loopback behind a TLS proxy | SQLite, PostgreSQL or MySQL / sql or Redis | login required |
| several nodes | behind a load balancer | PostgreSQL or MySQL / Redis (required) | login required |

Settings are explained in [configuration.md](configuration.md).

- [Local single-user](#local-single-user)
- [Team behind a TLS reverse proxy](#team-behind-a-tls-reverse-proxy)
- [Docker and Compose with PostgreSQL and Redis](#docker-and-compose-with-postgresql-and-redis)
- [Single sign-on](#single-sign-on)
- [Two-step sign-in, passkeys and captcha](#two-step-sign-in-passkeys-and-captcha)
- [Graceful shutdown](#graceful-shutdown)
- [Security checklist](#security-checklist)
- [Backups](#backups)
- [Upgrading](#upgrading)
- [Master key rotation](#master-key-rotation)

## Local single-user

```bash
plugrelay init                      # relay.yaml, master.key (0600), plugrelay.db, local owner
export DEEPSEEK_API_KEY=sk-...
plugrelay upstream add --manifest deepseek --credential-env DEEPSEEK_API_KEY
plugrelay model add --name deepseek-chat --route deepseek:deepseek-chat
plugrelay key create --name me      # prints pr_... once
plugrelay serve
plugrelay setup claude-code --key pr_...
```

A local server that needs no key (Ollama, vLLM, …) is added without
`--credential-env` ([plugins](plugins.md#declarative-upstream-manifests)).
Other upstreams are not routed until they have a credential.

Without `--admin-email`, `init` creates a local owner account. While the
relay listens on loopback only, the console and admin API let loopback
requests in without a login ([API auth](api.md)), unless the request carries
`X-Forwarded-For`, `Forwarded` or `X-Real-IP`. First-run setup on a loopback
listener only answers a `localhost` or loopback-IP `Host`.

`init` flags for the console account:

| flag | effect |
|-|-|
| `--admin-email E` | create an admin instead of the local owner, and turn the local-owner rule off. Required when `--listen` is not loopback (or pass `--no-admin`). |
| `--admin-password P` | the admin's password. It lands in shell history: prefer `PLUGRELAY_ADMIN_PASSWORD`, or type it on stdin. No default. |
| `--admin-name N` | display name (default `admin`) |
| `--no-admin` | create no account; the console's first-run setup creates the admin, open to whoever gets there first |

## Team behind a TLS reverse proxy

```bash
PLUGRELAY_ADMIN_PASSWORD=... plugrelay init \
  --admin-email ops@example.com --public-url https://relay.example.com \
  --config /etc/plugrelay/relay.yaml
plugrelay serve --config /etc/plugrelay/relay.yaml
```

Keep `listen` on `127.0.0.1:8787` and terminate TLS in the proxy. The proxy
must:

- not buffer: streams go out event by event, keepalives every 15 s, and one
  response can run up to the 30-minute attempt timeout;
- forward the original `Host` and set `X-Forwarded-For`. A proxy that
  rewrites `Host` to `127.0.0.1` without forwarding headers would let every
  visitor in as the local owner if that rule were on. Creating the admin
  with `init --admin-email` turns it off; behind the proxy, first-run setup
  refuses the public `Host` anyway.

Caddy (streamed responses are not buffered by default; `flush_interval -1`
makes it explicit):

```caddyfile
relay.example.com {
	request_body {
		max_size 32MB
	}
	reverse_proxy 127.0.0.1:8787 {
		flush_interval -1
		transport http {
			read_timeout 35m
			write_timeout 35m
		}
	}
}
```

nginx:

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name relay.example.com;
    ssl_certificate     /etc/letsencrypt/live/relay.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/relay.example.com/privkey.pem;

    client_max_body_size 32m;           # limits.max_body_bytes

    location / {
        proxy_pass http://127.0.0.1:8787;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        proxy_buffering off;            # SSE: forward each event at once
        proxy_request_buffering off;
        proxy_cache off;
        gzip off;
        proxy_read_timeout 35m;         # > egress total timeout
        proxy_send_timeout 35m;
    }
}
```

Cursor calls from its own servers, so Cursor users need this public HTTPS
entry and the public URL (`site.public_base_url`) set.

Behind a proxy every request seems to come from the proxy. List it in
[`trusted_proxies`](configuration.md#top-level) so allowlists, per-IP limits,
logs and login throttling see the real client:

```yaml
trusted_proxies: [127.0.0.1, "::1"]   # the proxy above, on the same host
```

The proxy must overwrite or append `X-Forwarded-For` (nginx above does;
Caddy does by default). Never list addresses untrusted clients can connect
from: a trusted peer can claim any client address. In Docker, list the
proxy container's address or network, not the whole host.

Probes: `GET /healthz` (process up), `GET /readyz` (config loaded, store
reachable). Prometheus: `GET /metrics`, protected by `PLUGRELAY_ADMIN_TOKEN`
when set.

## Docker and Compose with PostgreSQL and Redis

The image (`Dockerfile`, distroless, non-root) runs
`plugrelay serve --config /data/relay.yaml`. Without a relay.yaml it uses
defaults, so `PLUGRELAY_*` variables alone can configure it.

[`deploy/docker-compose.yml`](../deploy/docker-compose.yml) runs plugrelay with
PostgreSQL and Redis, all from environment:

```bash
cp deploy/.env.example deploy/.env      # set PLUGRELAY_MASTER_KEY, POSTGRES_PASSWORD
docker compose -f deploy/docker-compose.yml up -d
```

- The port is published on `127.0.0.1:8787`; put the proxy above in front.
- First start logs `first run: open the setup wizard`. Open `/setup`
  (through the proxy or an SSH tunnel) right away and create the admin:
  until then anyone who reaches it can. Then set the public URL in Settings.
- CLI commands run in the container with the same environment:

  ```bash
  docker compose -f deploy/docker-compose.yml exec plugrelay plugrelay upstream list
  docker compose -f deploy/docker-compose.yml exec plugrelay plugrelay key create --name ci
  ```

- `upstream add --credential-env` needs the key in the container's
  environment; adding credentials in the console avoids that.
- Several nodes: run more `plugrelay` replicas on the same PostgreSQL and
  Redis. State must be Redis.
- MySQL: the comment at the top of the compose file shows the service and
  the two store variables to change.
- `deploy/.env` holds the master key: keep it out of version control and
  back it up.

### Single container with SQLite

Generate the master key into a file and keep it; it is the only way to read
stored credentials later. Mount a host directory the container user owns.

```bash
openssl rand -base64 32 > master.key && chmod 600 master.key
docker run -d --name plugrelay -p 127.0.0.1:8787:8787 --stop-timeout 120 \
  --user "$(id -u):$(id -g)" -v "$PWD/data:/data" \
  -e PLUGRELAY_LISTEN=0.0.0.0:8787 \
  -e PLUGRELAY_MASTER_KEY="$(cat master.key)" \
  -e PLUGRELAY_STORE_DSN=/data/plugrelay.db -e PLUGRELAY_STATE_DRIVER=sql \
  ghcr.io/initxy/plugrelay:latest
```

Until the first tagged release no image is published: run
`docker build -t plugrelay .` in a checkout and use `plugrelay` as the image.

## Single sign-on

The console can sign people in through OpenID Connect or OAuth providers.
Turn the `sso` feature on, then add providers under **System → Sign-in
providers** (or `POST /api/v1/sso/providers`). The client secret is sealed
with the master key; calls to the provider go through the core egress and its
SSRF guard. Code: `internal/admin/sso_*.go`.

The redirect URI is built from the public URL (`site.public_base_url`),
never from the request's `Host`, so set it unless the relay is loopback-only
and opened at a loopback name. The console shows each provider's URI:

```text
https://relay.example.com/api/v1/auth/sso/<provider id>/callback
```

Sign-in uses PKCE (S256), a one-time `state` bound to the browser by the
`pr_sso` cookie and, for OIDC, a `nonce`; ID tokens are checked against the
provider's JWKS (RS/PS/ES 256–512, cached an hour). The identity maps to a
user, first match wins:

1. an identity already linked (`provider`, `subject`);
2. with **Link existing users by email**, a user with the same *verified*
   email. Admins only with **Also link admin accounts** (`link_admins`);
3. with **Create users on first sign-in**, a new user with the default role
   (never `admin`), in the group of the first matching rule (`@domain` of
   the verified email, or a claim such as `groups`), else the default
   group. GitHub takes domain rules only; Feishu/Lark domain or `tenant_key`.

An email counts as verified when the provider says so. *Trust email*
(`trust_email`) fills in only when the provider says nothing; Feishu/Lark
needs allowed tenants first. Allowed tenants match the Entra `tid` or the
Feishu/Lark `tenant_key`. Links are tied to the provider's issuer, tenant and
client ID; changing one unlinks its users until they sign in again.

Errors show on the login page (`/login?sso_error=<code>`). Sign-ins are
audited (`auth.login`, `user.create`, `user.identity.link`). Failures count
toward the wrong-password limit per address, and more than 60 sign-in starts
in five minutes from one address are refused.

### Provider setup

| preset | at the provider | in plugrelay |
|-|-|-|
| `google` | Google Cloud console → APIs & Services → Credentials → OAuth client ID, type *Web application*; add the redirect URI. | client ID and secret. Allowed domains such as `example.com` keep personal Gmail accounts out. |
| `microsoft` | Entra admin center → App registrations → New registration, platform *Web*, redirect URI; Certificates & secrets → new client secret. Optional claims: add `email` to the ID token. | tenant = directory (tenant) ID or a verified domain for one organization; `organizations` admits any work account (then list tenant IDs under allowed tenants). Single-tenant apps trust the email; multi-tenant needs `xms_edov` or *trust email*. |
| `keycloak` | Realm → Clients → Create, OpenID Connect, *Client authentication* on, *Standard flow* on; valid redirect URI = the URI above. | issuer `https://<host>/realms/<realm>`. For group rules add a *Group Membership* mapper (claim `groups`, full path off). |
| `authentik` | Applications → Providers → OAuth2/OpenID Provider, client type *Confidential*, redirect URI; create an application for it. | issuer `https://<host>/application/o/<application slug>/`. |
| `oidc` | any provider with `/.well-known/openid-configuration` under its issuer. | issuer URL exactly as published. |
| `github` | Settings → Developer settings → OAuth Apps → New OAuth App; callback URL = the URI above. OAuth 2.0, not OIDC: plugrelay reads `/user` and `/user/emails`. | client ID and secret. Only a *verified primary* email counts; the subject is the numeric user id. |
| `feishu` / `lark` | open.feishu.cn / open.larksuite.com → developer console → self-built app; Security Settings → redirect URL; enable web login; for email add scope `contact:user.email:readonly`. | App ID and App Secret as client ID and secret; subject is `open_id`. Tenant admins type in contact emails and users never confirm them, so they are unverified unless **Trust the provider's email** is on. Restrict with allowed tenants (`tenant_key`) instead. |

### Turning password sign-in off

Once SSO works, turn passwords off with site setting
`security.password_login`. The relay refuses unless an enabled provider
exists and the admin signed in through one (an operator token always may),
and it won't disable or delete the last enabled provider while passwords are
off. Ways back in that always work:

- `PLUGRELAY_ADMIN_TOKEN` (or a stored admin token):
  `curl -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"values":{"security.password_login": true}}' https://relay.example.com/api/v1/settings`
- on the relay host, against the database:
  `PLUGRELAY_ADMIN_PASSWORD='new long password' plugrelay user reset-password --user ops@example.com`
  sets the password, ends that user's sessions and turns password sign-in
  back on.

## Two-step sign-in, passkeys and captcha

Each user turns these on under user menu → **Security**. Code:
`internal/admin/mfa*.go`, `totp.go`, `webauthn.go`, `captcha.go`; factors are
stored in `user_totp`, `user_recovery_codes`, `user_passkeys`.

| factor | details |
|-|-|
| authenticator app (TOTP) | RFC 6238, SHA-1, 6 digits, 30 s steps, ±1 step drift. Secret sealed with the master key; QR drawn in the browser. Each code works once, across all nodes. |
| recovery codes | ten one-time codes, shown once when the first factor is added or when regenerated; stored hashed |
| passkeys (WebAuthn) | attestation `none`, ES256 / EdDSA / RS256, user verification preferred. Works as the second step and, with user verification, as passwordless sign-in (refused while passwords are off). RP ID is the public URL's host, the only allowed origin is its origin; without a public URL only a loopback-only relay opened at a loopback name offers them (`public_url_missing`). A counter that doesn't grow is refused and logged as a possible clone. Standard library only. |

With a factor on, `POST /api/v1/auth/login` answers 202 with only the
`pr_mfa` cookie (five minutes, five tries); the session comes after
`POST /api/v1/auth/mfa`. Wrong codes count toward the login limiter per
address and per account. SSO skips the second step (the provider usually
has its own MFA) unless `security.require_2fa_sso` is on, which sends the
browser to `/login?mfa=1`.

Sensitive changes need a sign-in or re-confirmation within five minutes
(`POST /api/v1/me/reauth`): removing the authenticator app, new recovery
codes, passkey changes; for admins, creating an admin, changing a user's
password or role, disabling or deleting a user, re-enabling a disabled
admin, resetting someone's two-step sign-in.

Site settings (**Settings → Security**):

| key | default | meaning |
|-|-|-|
| `security.require_2fa_admins` | off | admins must add an authenticator app or passkey |
| `security.require_2fa_grace_days` | 7 | days after the rule turns on (or the account is created) before admin operations answer 403 `mfa_enrollment_required`; the admin can still open their own pages to enroll, without admin powers. SSO-only admins are exempt unless `security.require_2fa_sso` is on |
| `security.require_2fa_sso` | off | SSO sign-ins also need the second step |
| `security.login_captcha_after` | 0 (off) | captcha on password login after this many failures from one address in five minutes |
| `security.captcha_provider` | `off` | `turnstile` (Cloudflare) or `hcaptcha` |
| `security.captcha_site_key`, `security.captcha_secret` | | the provider's keys; the secret is sealed with the master key |

The captcha token is checked server-side through the core egress. If the
provider can't be reached, or the captcha is on but can't run (say, a secret
sealed under another master key), login fails with 503
`captcha_unavailable` instead of skipping the check. A provider can't be
saved without both keys. The console's Content-Security-Policy allows a
provider's scripts only while it is selected. Sign-up has its own captcha
settings ([sell kit](sell-kit.md#sign-up-adminsignup)).

Locked out: an admin removes a user's factors in the user drawer
(`DELETE /api/v1/users/{id}/mfa`, audited as `user.mfa_reset`), or on the
relay host run `plugrelay user reset-2fa --user ops@example.com` (also
audited).

## Graceful shutdown

On SIGTERM or SIGINT the relay stops accepting connections, then:

1. lets in-flight requests run up to `shutdown.grace` (30 s), then closes
   them;
2. waits up to `shutdown.drain` (60 s) for those requests to settle quota and
   for queued records to reach the request log; the queue always gets at
   least 10 s more;
3. stops external plugins (up to 10 s) and closes the store.

Records still unwritten are logged and counted
(`plugrelay_sink_ledger_records_total{outcome="lost"}`). The process manager
must wait longer than grace + drain + 30 s before killing it: 120 s with the
defaults, more if you raise either.

| runner | setting | its default |
|-|-|-|
| Docker Compose | `stop_grace_period: 120s` (set in [`deploy/docker-compose.yml`](../deploy/docker-compose.yml)) | 10 s |
| `docker run` | `--stop-timeout 120` | 10 s |
| Kubernetes | `terminationGracePeriodSeconds: 120`; a `preStop` hook counts against it | 30 s |
| systemd | `TimeoutStopSec=120` | |

## Security checklist

- [ ] TLS in front of anything not on loopback; behind the proxy, `listen` or the published port stays on `127.0.0.1`.
- [ ] `security.local_owner_login` off for anything shared (the setup wizard's "A team" and "Selling" answers and `init --admin-email` do this).
- [ ] The proxy forwards the original `Host`; `trusted_proxies` lists only your proxies, or is empty.
- [ ] Setup wizard finished right after first start; there is no default admin.
- [ ] Master key from `plugrelay init` or `openssl rand -base64 32`, mode 0600 or in a secret store, backed up apart from the database.
- [ ] `PLUGRELAY_ADMIN_TOKEN`, if set, is long and random; it is full admin.
- [ ] Shared relays: `security.require_2fa_admins` on, public URL set (for passkeys), each admin keeps their recovery codes.
- [ ] Public sign-up or an internet-facing login: captcha on (`security.login_captcha_after`, `admin.signup` `captcha_provider`).
- [ ] SSO: public URL set; allowed domains or tenants for providers anyone can join (Google, GitHub, Microsoft `organizations`, Feishu/Lark); *trust email* only when you control the provider's emails.
- [ ] `egress.allow_private_cidrs` lists only what self-hosted upstreams need; it opens the SSRF guard.
- [ ] External plugins off unless you trust every plugin in the directory; they run as the relay's OS user.
- [ ] Bodies not logged (`body_logging` and every group's `log_bodies` off, the default) unless users agreed.
- [ ] Only upstream accounts you own and whose terms allow your use, resale in particular.
- [ ] Release artifacts verified: checksums, cosign signatures, SBOM (`.goreleaser.yaml`).

## Backups

| what | how | note |
|-|-|-|
| master key | copy `master.key` / the secret | without it, secrets in any backup are unreadable |
| SQLite | `sqlite3 plugrelay.db ".backup 'plugrelay-$(date +%F).db'"` while running | WAL mode: use `.backup`, not `cp` |
| PostgreSQL | `pg_dump -Fc plugrelay > plugrelay.dump` | includes the request log |
| MySQL | `mysqldump --single-transaction plugrelay > plugrelay.sql` | includes the request log |
| business data only | `plugrelay export -o relay-data.yaml` (`--with-secrets` for credentials) | portable YAML without users, keys or logs; restore with `plugrelay apply -f` |
| state | nothing | disposable: losing it costs cache hits and counters |

## Upgrading

1. Read the [release notes](https://github.com/initxy/plugrelay/releases)
   ([how they are written](releasing.md)).
2. Back up the database and the master key.
3. Swap the binary or image tag and restart. Migrations run when the store
   opens; downgrading past a migration needs the backup.
4. Run `plugrelay doctor` (config, master key, database, listen address).
5. With several nodes, upgrade one first; the rest keep serving.

On PostgreSQL and MySQL, migrations hold a database lock, so nodes may start
together. MySQL commits each DDL statement separately: a migration that fails
halfway leaves part of itself behind and needs the backup or a manual
cleanup before the next start.

## Master key rotation

Put the old key in `previous_master_keys` (`PLUGRELAY_PREVIOUS_MASTER_KEYS`)
and the new one in `master_key`. Private state sealed under the old key still
opens, so conversations keep working
([ADR-0003](adr/0003-seal-private-state-with-master-key.md)). Nothing stored
is re-encrypted: credentials, secret settings (captcha, payment keys), SSO,
MCP and notification secrets and authenticator-app secrets stay readable
only with the key they were saved under. Re-enter them after the change;
users re-enroll their authenticator app.
