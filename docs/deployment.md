# Deployment

English | [简体中文](deployment.zh-CN.md)

One binary serves every shape. Pick the smallest that fits:

| shape | listen | store / state | console | typical |
|-|-|-|-|-|
| local, one person | `127.0.0.1:8787` | SQLite / memory or sql | on; on loopback it needs no login | a laptop |
| team, one node | loopback behind a TLS proxy | SQLite, PostgreSQL or MySQL / sql or Redis | on, login required | a small VM |
| team or sell, several nodes | behind a load balancer | PostgreSQL or MySQL / Redis (required) | on | containers |

Every setting below is documented in [configuration.md](configuration.md).

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

A local server that takes no key (Ollama, vLLM, …) is added without
`--credential-env`; it is then keyless ([plugins](plugins.md#declarative-upstream-manifests)).
Any other upstream needs a credential before it is routed.

`init` without `--admin-email` creates an owner account with a random
password; while the relay listens on loopback only, the console and admin
API accept loopback requests without a login (the local-owner rule,
[API auth](api.md)) — unless the request carries `X-Forwarded-For`,
`Forwarded` or `X-Real-IP`, which mark it as relayed by a proxy. On a
loopback listener, first-run setup in the console also only answers
`localhost` or loopback-IP `Host` names. Clients: [clients.md](clients.md).

`init` flags for the console account:

- `--admin-email E` creates an admin instead of the owner account and turns
  the local-owner rule off. Required when `--listen` is not loopback (or
  pass `--no-admin`).
- `--admin-password P` is that admin's password. It ends up in shell
  history: prefer `PLUGRELAY_ADMIN_PASSWORD`, or leave both unset and type
  it on stdin. There is no default password.
- `--admin-name N` is the admin's display name (default `admin`).
- `--no-admin` creates neither an admin nor the owner account; the
  console's first-run setup then creates the admin, open to whoever reaches
  it first.

## Team behind a TLS reverse proxy

```bash
PLUGRELAY_ADMIN_PASSWORD=... plugrelay init \
  --admin-email ops@example.com --public-url https://relay.example.com \
  --config /etc/plugrelay/relay.yaml
plugrelay serve --config /etc/plugrelay/relay.yaml
```

Keep `listen` on `127.0.0.1:8787` and terminate TLS in a proxy. The proxy
must not buffer: streams are forwarded event by event, keepalives arrive
every 15 s while an upstream is busy, and a single response can run for many
minutes (attempt total timeout 30 min). Forward the original `Host` header
and set `X-Forwarded-For` — the local-owner rule only applies to loopback
`Host` names without forwarding headers, and a proxy that rewrites `Host` to
`127.0.0.1` and sends neither would, with the local-owner rule on, let every
visitor act as the owner. Create the admin with `init` as above (which also
turns that rule off): behind the proxy the console's first-run setup refuses
the public `Host`.

Caddy (buffering is already off for streamed responses; `flush_interval -1`
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

Cursor calls from its own servers, so a team that uses Cursor needs this
public HTTPS entry and the public URL (site setting `site.public_base_url`)
set.

By default the client address is the TCP peer, so behind a proxy every
request appears to come from the proxy. List the proxy in `trusted_proxies`
([configuration](configuration.md#top-level)) so IP allowlists, per-IP
limits, logs and login throttling see the real client:

```yaml
trusted_proxies: [127.0.0.1, "::1"]   # the proxy above, on the same host
```

The proxy must overwrite or append to `X-Forwarded-For` as the nginx example
does (`$proxy_add_x_forwarded_for`; Caddy does by default). Never list
addresses that untrusted clients can connect from: a trusted peer can claim
any client address. In Docker, list the proxy container's address or
network, not the whole host.

Probes: `GET /healthz` (process up) and `GET /readyz` (snapshot loaded, store
reachable). Prometheus: `GET /metrics`, protected by
`PLUGRELAY_ADMIN_TOKEN` when that is set.

## Docker and Compose with PostgreSQL and Redis

The image (`Dockerfile`, distroless, non-root) runs
`plugrelay serve --config /data/relay.yaml`. A missing relay.yaml means
defaults, so the container can be configured with `PLUGRELAY_*` variables
alone.

[`deploy/docker-compose.yml`](../deploy/docker-compose.yml) runs plugrelay with
PostgreSQL and Redis, configured entirely by environment:

```bash
cp deploy/.env.example deploy/.env      # set PLUGRELAY_MASTER_KEY, POSTGRES_PASSWORD
docker compose -f deploy/docker-compose.yml up -d
```

- The relay port is published on `127.0.0.1:8787`; put the proxy above in
  front of it.
- On first start the log says `first run: open the setup wizard`; open
  `/setup` (through the proxy or an SSH tunnel) to create the admin. Do it
  right away — until an admin exists, the wizard is open to whoever reaches
  it first.
  Then set the public URL under Settings.
- CLI commands run inside the container with the same environment:

  ```bash
  docker compose -f deploy/docker-compose.yml exec plugrelay plugrelay upstream list
  docker compose -f deploy/docker-compose.yml exec plugrelay plugrelay key create --name ci
  ```

- Upstream API keys for `upstream add --credential-env` must be in the
  container's environment; adding credentials in the console avoids that.
- More than one relay node: run several `plugrelay` replicas against the same
  PostgreSQL and Redis. State must be Redis — memory and sql state are
  per-node.
- MySQL instead of PostgreSQL: the comment at the top of the compose file
  shows the service and the two store variables to swap.
- `deploy/.env` holds the master key; keep it out of version control and
  back it up.

### Single container with SQLite

Mount a host directory owned by the container user and point the store at
it. Generate the master key into a file first and keep that file: it is the
only way to read stored credentials later.

```bash
openssl rand -base64 32 > master.key && chmod 600 master.key
docker run -d --name plugrelay -p 127.0.0.1:8787:8787 --stop-timeout 120 \
  --user "$(id -u):$(id -g)" -v "$PWD/data:/data" \
  -e PLUGRELAY_LISTEN=0.0.0.0:8787 \
  -e PLUGRELAY_MASTER_KEY="$(cat master.key)" \
  -e PLUGRELAY_STORE_DSN=/data/plugrelay.db -e PLUGRELAY_STATE_DRIVER=sql \
  ghcr.io/initxy/plugrelay:latest
```

Before the first tagged release there is no published image: build one with
`docker build -t plugrelay .` in a checkout and run `plugrelay` instead of
`ghcr.io/initxy/plugrelay:latest`.

## Single sign-on

The console can sign people in through an OpenID Connect provider or an
OAuth provider. Turn the `sso` feature on first: while it is off no provider
is offered and password sign-in always works. Providers are configured in
the console under **System → Sign-in providers** (or
`POST /api/v1/sso/providers`) and stored in the database; the client secret is sealed with the master key. Calls to the provider (discovery, keys, token, user info) go through the core egress and its SSRF guard. Code:
`internal/admin/sso_*.go`.

Prerequisites: the *Public base URL* site setting (`site.public_base_url`)
must be set unless the relay listens on loopback only and is opened at a
loopback name — the redirect URI is built from it, never from the request's
`Host`. Each provider's redirect URI is shown in the console:

```text
https://relay.example.com/api/v1/auth/sso/<provider id>/callback
```

How a sign-in works: the login page links to `/api/v1/auth/sso/<id>/start`,
which redirects to the provider with PKCE (S256), a one-time `state` bound to
the browser by the `pr_sso` cookie, and (OIDC) a `nonce`. The callback redeems
the code; for OIDC it verifies the ID token signature against the provider's
JWKS (RS/PS/ES 256–512, keys cached for an hour and refetched when an unknown
`kid` appears) and checks `iss`, `aud`, `azp`, `exp`, `nbf`, `iat` and
`nonce`. Then the identity maps to a user, in this order:

1. an identity already linked to a user (`provider`, `subject`);
2. with **Link existing users by email**, a user whose email equals the
   provider's *verified* email (case-insensitive). Admin accounts are skipped
   unless **Also link admin accounts** (`link_admins`) is on too;
3. with **Create users on first sign-in**, a new user with the default role
   (`user` or `member`, never `admin`) and the group from the first matching
   group rule (`@domain` of the verified email, or a claim such as `groups`
   with a value), else the default group. GitHub providers take domain rules
   only and Feishu/Lark domain or `tenant_key` rules: their other profile
   fields are typed in by the user.

An email counts as verified when the provider says so. *Trust email*
(`trust_email`) only fills in for a provider that says nothing; an explicit
`email_verified: false` stays unverified. Feishu/Lark providers need allowed
tenants before *trust email* can be on. Links are scoped to the provider's
issuer, tenant and client ID: changing any of them unlinks everyone who
signed in with it, and they are matched afresh on their next sign-in.

Otherwise the login page shows why (`/login?sso_error=<code>`). Allowed email
domains need a verified email; allowed tenants match the Entra `tid` or the
Feishu/Lark `tenant_key`. A successful sign-in sets the same session and CSRF
cookies as a password login and writes `auth.login` (plus `user.create` and
`user.identity.link` when those happen) to the audit log. Failures count
against the same per-address limit as wrong passwords; starting more than 60
sign-ins in five minutes from one address is refused too.

### Provider setup

| preset | at the provider | in plugrelay |
|-|-|-|
| `google` | Google Cloud console → APIs & Services → Credentials → OAuth client ID, type *Web application*; add the redirect URI. | client ID and secret. Allowed domains such as `example.com` keep personal Gmail accounts out. |
| `microsoft` | Entra admin center → App registrations → New registration, platform *Web*, redirect URI; Certificates & secrets → new client secret. Optional claims: add `email` to the ID token. | tenant = directory (tenant) ID or a verified domain for one organization; `organizations` admits any work account (then list tenant IDs under allowed tenants). In a single-tenant app the email is trusted; multi-tenant needs `xms_edov` or *trust email*. |
| `keycloak` | Realm → Clients → Create, OpenID Connect, *Client authentication* on, *Standard flow* on; valid redirect URI = the URI above. | issuer `https://<host>/realms/<realm>`. Map groups with a *Group Membership* mapper (claim `groups`, full path off) to use group rules. |
| `authentik` | Applications → Providers → OAuth2/OpenID Provider, client type *Confidential*, redirect URI; create an application for it. | issuer `https://<host>/application/o/<application slug>/`. |
| `oidc` | any provider with `/.well-known/openid-configuration` under its issuer. | issuer URL exactly as the provider publishes it. |
| `github` | Settings → Developer settings → OAuth Apps → New OAuth App; callback URL = the URI above. GitHub is OAuth 2.0, not OIDC: plugrelay reads `/user` and `/user/emails`. | client ID and secret. Only a *verified primary* email counts; the subject is the numeric GitHub user id. |
| `feishu` / `lark` | open.feishu.cn / open.larksuite.com → developer console → create a self-built app; Security Settings → add the redirect URL; enable web login; for email add the scope `contact:user.email:readonly`. | App ID and App Secret as client ID and secret. The subject is `open_id`. Feishu/Lark contact emails are entered by tenant admins and never confirmed by the user, so they are not treated as verified unless **Trust the provider's email** is on; restrict sign-in with allowed tenants (`tenant_key`) instead. |

### Turning password sign-in off

Once SSO works, password sign-in can be switched off (site setting
`security.password_login`). The relay refuses
to switch it off unless an enabled provider exists and the admin doing it has
signed in through one (an operator token may always do it), and it refuses to
disable or delete the last enabled provider while passwords are off.
Break-glass paths that always work:

- `PLUGRELAY_ADMIN_TOKEN` (or a stored admin token):
  `curl -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"values":{"security.password_login": true}}' https://relay.example.com/api/v1/settings`
- on the relay host, against the database:
  `PLUGRELAY_ADMIN_PASSWORD='new long password' plugrelay user reset-password --user ops@example.com`
  sets the password, ends that user's sessions and turns password sign-in
  back on.

## Two-step sign-in, passkeys and captcha

Each console user turns these on for themselves on the **Security** page
(user menu → Security). Code: `internal/admin/mfa*.go`, `totp.go`,
`webauthn.go`, `captcha.go`; factors live in the store (`user_totp`,
`user_recovery_codes`, `user_passkeys`).

- **Authenticator app (TOTP).** RFC 6238 with SHA-1, 6 digits, 30-second
  steps, one step of clock drift either way. The secret is sealed with the
  master key; the QR code is drawn in the browser. A code is good once: its
  time step is spent on first use, across all nodes.
- **Recovery codes.** Ten one-time codes, shown once when the first factor
  is turned on and whenever they are regenerated; stored hashed.
- **Passkeys (WebAuthn).** Registration with attestation `none` (the
  attestation statement is not checked), ES256, EdDSA and RS256 keys, user
  verification preferred. A passkey works as the second step after a
  password and, with user verification, as a passwordless sign-in ("Sign in
  with a passkey"), which is refused while password sign-in is off. The RP
  ID is the host of the *Public base URL* setting and the only accepted
  origin is that URL's origin; without one, only a loopback-only relay opened
  at a loopback name offers passkeys, others refuse them
  (`public_url_missing`). A signature counter that does
  not grow is refused and logged as a possible cloned authenticator.
  Implemented on the standard library, no WebAuthn dependency.

With a factor on, `POST /api/v1/auth/login` answers 202 and sets only the
`pr_mfa` cookie (five minutes, five answers); the session cookie comes after
the second step (`POST /api/v1/auth/mfa`). Wrong codes count against the
login limiter per address and per account. Single sign-on skips the second
step — the identity provider usually enforces its own MFA — unless
`security.require_2fa_sso` is on; then the callback sends the browser to
`/login?mfa=1`.

Turning the authenticator app off, new recovery codes, adding or deleting
a passkey, and an admin creating an admin, changing a user's password or
role, disabling, re-enabling a disabled admin or deleting a user, or
resetting their two-step sign-in need a sign-in or a re-confirmation (password, code
or passkey) within the last five minutes (`POST /api/v1/me/reauth`).

Site settings (**Settings → Security**):

| key | default | meaning |
|-|-|-|
| `security.require_2fa_admins` | off | admins must turn on an authenticator app or a passkey |
| `security.require_2fa_grace_days` | 7 | days from when the rule turned on (or the account was created) before admin operations answer 403 `mfa_enrollment_required`; the admin's own pages stay open to enroll, with no admin powers on them. Admins who can only sign in by SSO are exempt unless `security.require_2fa_sso` is on |
| `security.require_2fa_sso` | off | ask SSO sign-ins for the second step too |
| `security.login_captcha_after` | 0 (off) | ask for a captcha on the password login after this many failures from an address within five minutes |
| `security.captcha_provider` | `off` | `turnstile` (Cloudflare) or `hcaptcha` |
| `security.captcha_site_key`, `security.captcha_secret` | | the provider's keys; the secret is sealed with the master key |

The captcha token is verified server-side with the provider's siteverify
endpoint through the core egress; when the provider cannot be reached the
login fails closed (503 `captcha_unavailable`). Saving a provider without
its site key and secret is refused; a captcha that is on but cannot run
(for example, a secret sealed under another master key) also answers 503
`captcha_unavailable` instead of skipping the check. The console's
Content-Security-Policy allows a provider's script and frame origins only
while that provider is chosen (here or for sign-up).
Sign-up has its own captcha settings ([sell kit](sell-kit.md#sign-up-adminsignup)).

Locked out: an admin removes a user's factors in the user drawer
(`DELETE /api/v1/users/{id}/mfa`, audited as `user.mfa_reset`), or on the
relay host `plugrelay user reset-2fa --user ops@example.com` does it against
the database (also audited).

## Graceful shutdown

On SIGTERM or SIGINT the relay stops accepting connections and then:

1. lets in-flight requests run for up to `shutdown.grace` (30 s), then
   closes their connections;
2. waits up to `shutdown.drain` (60 s) for the cut-off requests to settle
   quota and for queued records to reach the sinks — the record queue gets
   at least 10 s even when the requests used the whole drain;
3. stops external plugins (up to 10 s) and closes the store.

Records not written by then are logged and counted
(`plugrelay_sink_ledger_records_total{outcome="lost"}`). Whatever runs the
relay must wait longer than grace + drain + 30 s before it kills it — 120 s
with the defaults:

- Docker Compose: `stop_grace_period` (set to `120s` in
  [`deploy/docker-compose.yml`](../deploy/docker-compose.yml)); `docker run`
  needs `--stop-timeout 120` (its default is 10 s).
- Kubernetes: `terminationGracePeriodSeconds: 120` in the pod spec (the
  default is 30 s). A `preStop` hook counts against the same budget.
- systemd: `TimeoutStopSec=120`.

Raising `shutdown.grace` or `shutdown.drain` raises that figure with it.

## Security checklist

- [ ] TLS in front of anything that is not loopback; `listen` stays on
      loopback behind the proxy, or the published port is bound to
      `127.0.0.1`.
- [ ] `security.local_owner_login` off for anything shared (setup's "team"
      and "sell" answers and `init --admin-email` turn it off) — on, it
      trusts loopback requests without a login.
- [ ] The proxy forwards the original `Host`.
- [ ] `trusted_proxies` lists only your proxies, or is empty when nothing
      sits in front of the relay.
- [ ] Setup wizard completed immediately after the first start; no default
      admin exists, so nobody else should reach `/setup` first.
- [ ] Master key generated by `plugrelay init` or `openssl rand -base64 32`,
      file mode 0600 or a secret store, backed up separately from the
      database.
- [ ] `PLUGRELAY_ADMIN_TOKEN` long and random if set; it grants full admin.
- [ ] Two-step sign-in: `security.require_2fa_admins` on for shared relays;
      the public URL set so passkeys work; recovery codes stored by each admin.
- [ ] Public sign-up or a login page open to the internet: a captcha
      configured (`security.login_captcha_after`, `admin.signup`
      `captcha_provider`).
- [ ] Single sign-on: the public URL set; allowed domains or tenants set for
      providers anyone can sign up at (Google, GitHub, Microsoft
      `organizations`, Feishu/Lark); *trust email* only when the provider's
      emails are really controlled.
- [ ] `egress.allow_private_cidrs` lists only the ranges self-hosted upstreams
      need — it opens the SSRF guard for them.
- [ ] External plugins off (`external_plugins.enabled: false`) unless you
      trust every plugin in the directory; they run with the relay's OS user.
- [ ] Request bodies are not logged (`body_logging` and every group's `log_bodies` off, the default) unless
      users have agreed to it.
- [ ] Only upstream accounts you own and whose terms allow your use (resale
      in particular). plugrelay does not pool subscriptions or impersonate
      clients.
- [ ] Release artifacts verified: checksums, cosign signatures, SBOM
      (`.goreleaser.yaml`).

## Backups

| what | how | note |
|-|-|-|
| master key | copy `master.key` / the secret | without it, credential secrets and secret settings in any backup are unreadable |
| SQLite | `sqlite3 plugrelay.db ".backup 'plugrelay-$(date +%F).db'"` while running | the database runs in WAL mode; copy with `.backup`, not `cp` |
| PostgreSQL | `pg_dump -Fc plugrelay > plugrelay.dump` | includes the request log |
| MySQL | `mysqldump --single-transaction plugrelay > plugrelay.sql` | includes the request log |
| business data only | `plugrelay export -o relay-data.yaml` (`--with-secrets` for credentials) | portable YAML; no users, keys or logs; restore with `plugrelay apply -f` |
| state | nothing | memory/Redis state is disposable: losing it costs cache hits and counters |

## Upgrading

1. Read the release notes on [GitHub Releases](https://github.com/initxy/plugrelay/releases)
   ([how they are written](releasing.md)).
2. Back up the database and the master key.
3. Replace the binary or image tag and restart. Migrations are embedded and
   applied when the store opens; a downgrade after a migration needs the
   backup.
4. `plugrelay doctor` checks config, master key, database and listen address.
5. Several nodes: upgrade one node first; the others keep serving.

On PostgreSQL and MySQL migrations run under a database lock, so several
nodes may start at once. MySQL commits each DDL statement on its own: a
migration that fails halfway leaves its finished statements behind and
needs the backup (or a manual cleanup) before the next start.

## Master key rotation

Put the old key in `previous_master_keys` (or
`PLUGRELAY_PREVIOUS_MASTER_KEYS`) and the new one in `master_key`. Seals
made under the old key still open, so conversations keep their private state
([ADR-0003](adr/0003-seal-private-state-with-master-key.md)). Nothing stored
is re-encrypted: every secret in the database (credentials, secret settings
such as the captcha and payment keys, SSO, MCP and notification secrets,
authenticator-app secrets) stays readable only with the key it was saved
under. After the change, re-enter them; users re-enroll their authenticator
app.
