# 部署

[English](deployment.md) | 简体中文

各种部署都用同一个二进制文件。挑够用的最小那种：

| 场景 | 监听 | 存储 / 状态 | 控制台 |
|-|-|-|-|
| 一个人，一台笔记本 | `127.0.0.1:8787` | SQLite / memory 或 sql | 回环地址上免登录 |
| 一个团队，一台虚拟机 | 回环地址，前面放 TLS 代理 | SQLite、PostgreSQL 或 MySQL / sql 或 Redis | 要登录 |
| 多个节点 | 负载均衡后面 | PostgreSQL 或 MySQL / Redis（必须） | 要登录 |

各项设置的说明见[配置](configuration.zh-CN.md)。

- [本机单人使用](#local-single-user)
- [团队使用：放在 TLS 反向代理后面](#team-behind-a-tls-reverse-proxy)
- [Docker 和 Compose（PostgreSQL + Redis）](#docker-and-compose-with-postgresql-and-redis)
- [单点登录](#single-sign-on)
- [两步登录、通行密钥和验证码](#two-step-sign-in-passkeys-and-captcha)
- [平滑停机](#graceful-shutdown)
- [安全检查清单](#security-checklist)
- [备份](#backups)
- [升级](#upgrading)
- [更换主密钥](#master-key-rotation)

<a id="local-single-user"></a>
## 本机单人使用

```bash
plugrelay init                      # 生成 relay.yaml、master.key (0600)、plugrelay.db 和本机所有者账号
export DEEPSEEK_API_KEY=sk-...
plugrelay upstream add --manifest deepseek --credential-env DEEPSEEK_API_KEY
plugrelay model add --name deepseek-chat --route deepseek:deepseek-chat
plugrelay key create --name me      # 只打印一次 pr_...
plugrelay serve
plugrelay setup claude-code --key pr_...
```

不需要密钥的本地服务（Ollama、vLLM 等）添加时不带 `--credential-env`（[插件](plugins.zh-CN.md#declarative-upstream-manifests)）。其他上游要先有凭据才会被选中。

`init` 不带 `--admin-email` 时会创建本机所有者账号。只要中转只监听回环地址，控制台和管理 API 就放行本机的免登录请求（[API 认证](api.zh-CN.md)），除非请求带了 `X-Forwarded-For`、`Forwarded` 或 `X-Real-IP`。回环监听下，首次安装页也只响应 `Host` 为 `localhost` 或回环 IP 的请求。

`init` 里和控制台账号有关的参数：

| 参数 | 作用 |
|-|-|
| `--admin-email E` | 创建管理员而不是本机所有者，同时关掉本机免登录。`--listen` 不是回环地址时必须带（或者传 `--no-admin`）。 |
| `--admin-password P` | 管理员密码。会留在 shell 历史里，最好用 `PLUGRELAY_ADMIN_PASSWORD`，或者在标准输入里敲。没有默认密码。 |
| `--admin-name N` | 显示名（默认 `admin`） |
| `--no-admin` | 不建任何账号，由控制台首次安装来建管理员，谁先打开谁就能建 |

<a id="team-behind-a-tls-reverse-proxy"></a>
## 团队使用：放在 TLS 反向代理后面

```bash
PLUGRELAY_ADMIN_PASSWORD=... plugrelay init \
  --admin-email ops@example.com --public-url https://relay.example.com \
  --config /etc/plugrelay/relay.yaml
plugrelay serve --config /etc/plugrelay/relay.yaml
```

`listen` 保持 `127.0.0.1:8787`，TLS 在代理上终结。代理要做到：

- 不缓冲：流式响应逐个事件转发，上游忙时每 15 秒一次保活，单个响应最长可以跑到单次尝试的 30 分钟总超时；
- 转发原始 `Host`，并设置 `X-Forwarded-For`。如果代理把 `Host` 改成 `127.0.0.1` 又不带转发头，而本机免登录还开着，每个访问者都会被当成所有者。用 `init --admin-email` 建管理员会关掉这条规则；在代理后面，首次安装页本来也会拒绝公网 `Host`。

Caddy（流式响应默认就不缓冲，`flush_interval -1` 只是写明白）：

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

nginx：

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

        proxy_buffering off;            # SSE：每个事件立即转发
        proxy_request_buffering off;
        proxy_cache off;
        gzip off;
        proxy_read_timeout 35m;         # 大于出站总超时
        proxy_send_timeout 35m;
    }
}
```

Cursor 是从它自己的服务器发请求的，所以用 Cursor 的团队需要这个公网 HTTPS 入口，并设好公网地址（站点设置 `site.public_base_url`）。

放在代理后面时，所有请求看起来都来自代理。把代理写进 [`trusted_proxies`](configuration.zh-CN.md#top-level)，IP 白名单、按 IP 限流、日志和登录限流才能看到真实客户端：

```yaml
trusted_proxies: [127.0.0.1, "::1"]   # 上面那个代理，和中转在同一台机器上
```

代理必须覆盖或追加 `X-Forwarded-For`（上面的 nginx 配置会这样做，Caddy 默认就会）。不可信客户端能连进来的地址绝不能写进去：可信对端可以冒充任何客户端地址。在 Docker 里写代理容器的地址或网段，不要写整台宿主机。

健康检查：`GET /healthz`（进程活着）、`GET /readyz`（配置已加载、数据库连得上）。Prometheus：`GET /metrics`，设了 `PLUGRELAY_ADMIN_TOKEN` 就要带它访问。

<a id="docker-and-compose-with-postgresql-and-redis"></a>
## Docker 和 Compose（PostgreSQL + Redis）

镜像（`Dockerfile`，distroless，非 root）运行 `plugrelay serve --config /data/relay.yaml`。没有 relay.yaml 就用默认值，所以只靠 `PLUGRELAY_*` 环境变量就能配好容器。

[`deploy/docker-compose.yml`](../deploy/docker-compose.yml) 把 plugrelay 和 PostgreSQL、Redis 一起跑起来，全部用环境变量配置：

```bash
cp deploy/.env.example deploy/.env      # 填好 PLUGRELAY_MASTER_KEY、POSTGRES_PASSWORD
docker compose -f deploy/docker-compose.yml up -d
```

- 端口只发布在 `127.0.0.1:8787`，前面放上面那样的代理。
- 第一次启动时日志会提示 `first run: open the setup wizard`。马上（通过代理或 SSH 隧道）打开 `/setup` 建管理员：建好之前谁先打开谁就能建。然后在「设置」里填公网地址。
- 命令行在容器里用同一套环境变量执行：

  ```bash
  docker compose -f deploy/docker-compose.yml exec plugrelay plugrelay upstream list
  docker compose -f deploy/docker-compose.yml exec plugrelay plugrelay key create --name ci
  ```

- `upstream add --credential-env` 要求上游 API 密钥在容器的环境变量里；在控制台里加凭据就不用。
- 多个节点：多跑几个 `plugrelay` 副本，连同一个 PostgreSQL 和 Redis。状态必须用 Redis。
- 换成 MySQL：compose 文件开头的注释写了要换的服务和两个存储变量。
- `deploy/.env` 里有主密钥，不要提交到版本库，记得备份。

<a id="single-container-with-sqlite"></a>
### 单容器 + SQLite

先把主密钥生成到文件里并保管好，以后读已存的凭据全靠它。挂载一个归容器用户所有的宿主机目录。

```bash
openssl rand -base64 32 > master.key && chmod 600 master.key
docker run -d --name plugrelay -p 127.0.0.1:8787:8787 --stop-timeout 120 \
  --user "$(id -u):$(id -g)" -v "$PWD/data:/data" \
  -e PLUGRELAY_LISTEN=0.0.0.0:8787 \
  -e PLUGRELAY_MASTER_KEY="$(cat master.key)" \
  -e PLUGRELAY_STORE_DSN=/data/plugrelay.db -e PLUGRELAY_STATE_DRIVER=sql \
  ghcr.io/initxy/plugrelay:latest
```

第一个正式版本发布前还没有公开镜像：在源码目录里 `docker build -t plugrelay .`，镜像名用 `plugrelay`。

<a id="single-sign-on"></a>
## 单点登录

控制台可以通过 OpenID Connect 或 OAuth 账号登录。先打开 `sso` 功能，再到 **系统 → 登录方式** 添加（或 `POST /api/v1/sso/providers`）。client secret 用主密钥加密；对登录服务的请求都走核心的出站通道和它的 SSRF 防护。代码：`internal/admin/sso_*.go`。

回调地址由公网地址（`site.public_base_url`）拼出来，从不看请求的 `Host`，所以除非中转只监听回环地址、而且用回环地址打开，否则必须设好。控制台会显示每个登录方式的回调地址：

```text
https://relay.example.com/api/v1/auth/sso/<provider id>/callback
```

登录用 PKCE（S256）、一次性的 `state`（由 `pr_sso` cookie 绑定到浏览器），OIDC 另带 `nonce`；ID token 用登录服务的 JWKS 验签（RS/PS/ES 256–512，缓存一小时）。之后按下面的顺序找对应的用户，先匹配上的算：

1. 已经绑定过的身份（`provider`、`subject`）；
2. 开了 **按邮箱关联已有用户** 时，邮箱和登录服务给的*已验证*邮箱相同的用户。管理员账号要另开 **也关联管理员账号**（`link_admins`）；
3. 开了 **首次登录时创建用户** 时，新建一个默认角色的用户（不会是 `admin`），分组取第一条匹配的规则（已验证邮箱的 `@域名`，或 `groups` 这类声明的值），都不匹配就进默认分组。GitHub 只支持按域名，飞书/Lark 支持按域名或 `tenant_key`。

登录服务说邮箱已验证才算已验证。*信任邮箱*（`trust_email`）只在登录服务什么都没说时补上；飞书/Lark 要先设允许的租户才能开它。允许的租户对应 Entra 的 `tid` 或飞书/Lark 的 `tenant_key`。绑定关系跟登录服务的 issuer、租户和 client ID 挂钩，改了其中任何一个，原来的用户都会解绑，下次登录时重新匹配。

出错时登录页会显示原因（`/login?sso_error=<code>`）。登录会记审计日志（`auth.login`、`user.create`、`user.identity.link`）。失败次数和密码输错共用每个地址的限额；同一地址五分钟内发起超过 60 次登录也会被拒。

<a id="provider-setup"></a>
### 各家登录服务怎么配

| 预设 | 在登录服务那边 | 在 plugrelay 里 |
|-|-|-|
| `google` | Google Cloud 控制台 → APIs & Services → Credentials → OAuth client ID，类型选 *Web application*，加上回调地址。 | client ID 和 secret。允许的域名设成 `example.com` 这类，就能挡住个人 Gmail 账号。 |
| `microsoft` | Entra 管理中心 → App registrations → New registration，平台选 *Web*，填回调地址；Certificates & secrets → 新建 client secret。Optional claims 里给 ID token 加上 `email`。 | tenant 填目录（租户）ID 或某个组织的已验证域名；填 `organizations` 则任何工作账号都能登（这时要在允许的租户里列出租户 ID）。单租户应用信任邮箱；多租户需要 `xms_edov` 或开*信任邮箱*。 |
| `keycloak` | Realm → Clients → Create，选 OpenID Connect，打开 *Client authentication* 和 *Standard flow*，有效回调地址填上面那个。 | issuer 为 `https://<host>/realms/<realm>`。要按分组规则分组，加一个 *Group Membership* mapper（声明名 `groups`，关掉 full path）。 |
| `authentik` | Applications → Providers → OAuth2/OpenID Provider，client type 选 *Confidential*，填回调地址；再为它建一个 application。 | issuer 为 `https://<host>/application/o/<application slug>/`。 |
| `oidc` | 任何在 issuer 下有 `/.well-known/openid-configuration` 的服务。 | issuer 地址照登录服务公布的原样填。 |
| `github` | Settings → Developer settings → OAuth Apps → New OAuth App，callback URL 填上面的地址。GitHub 是 OAuth 2.0 而不是 OIDC：plugrelay 读 `/user` 和 `/user/emails`。 | client ID 和 secret。只认*已验证的主邮箱*；subject 是 GitHub 的数字用户 id。 |
| `feishu` / `lark` | open.feishu.cn / open.larksuite.com → 开发者后台 → 创建企业自建应用；安全设置 → 添加重定向 URL；开启网页登录；要拿邮箱就加 `contact:user.email:readonly` 权限。 | App ID 和 App Secret 分别当 client ID 和 secret；subject 是 `open_id`。飞书/Lark 的通讯录邮箱由租户管理员填写、用户从不确认，所以除非开了 **信任登录服务提供的邮箱**，否则不算已验证；改用允许的租户（`tenant_key`）来限制登录。 |

<a id="turning-password-sign-in-off"></a>
### 关闭密码登录

单点登录跑通后，可以用站点设置 `security.password_login` 关掉密码登录。前提是有启用中的登录方式，而且操作的管理员正是通过它登录的（运维 token 不受这个限制）；密码登录关着时，也不能停用或删除最后一个启用中的登录方式。下面两条路始终能用：

- 用 `PLUGRELAY_ADMIN_TOKEN`（或存储里的管理员 token）：
  `curl -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"values":{"security.password_login": true}}' https://relay.example.com/api/v1/settings`
- 在中转所在的机器上直接改数据库：
  `PLUGRELAY_ADMIN_PASSWORD='new long password' plugrelay user reset-password --user ops@example.com`
  会设置密码、让这个用户的所有会话下线，并重新打开密码登录。

<a id="two-step-sign-in-passkeys-and-captcha"></a>
## 两步登录、通行密钥和验证码

每个用户在用户菜单 → **安全** 里自己开。代码：`internal/admin/mfa*.go`、`totp.go`、`webauthn.go`、`captcha.go`；数据存在 `user_totp`、`user_recovery_codes`、`user_passkeys` 表里。

| 方式 | 说明 |
|-|-|
| 验证器应用（TOTP） | RFC 6238，SHA-1，6 位，30 秒一步，前后各容忍一步时钟偏差。密钥用主密钥加密；二维码在浏览器里画。每个码只能用一次，所有节点都算。 |
| 恢复码 | 十个一次性码，开第一种验证方式时和重新生成时各显示一次；存的是哈希 |
| 通行密钥（WebAuthn） | attestation 为 `none`，支持 ES256 / EdDSA / RS256，优先要求用户验证。可以当密码之后的第二步；带用户验证时也能免密码登录（密码登录关着时不行）。RP ID 是公网地址的主机名，只接受这个地址的 origin；没设公网地址时，只有只监听回环地址、并用回环地址打开的中转才提供通行密钥，其他情况一律拒绝（`public_url_missing`）。签名计数不增长就拒绝，并记为疑似被复制的验证器。只用标准库实现。 |

开了第二步后，`POST /api/v1/auth/login` 返回 202，只设 `pr_mfa` cookie（五分钟内有效，最多答五次）；第二步（`POST /api/v1/auth/mfa`）通过后才给会话 cookie。输错的码按地址和按账号计入登录限流。单点登录默认跳过第二步（登录服务通常自带多因素验证），除非开了 `security.require_2fa_sso`，那时回调会把浏览器送到 `/login?mfa=1`。

敏感操作要求五分钟内登录过或重新确认过（密码、验证码或通行密钥，`POST /api/v1/me/reauth`）：关掉验证器应用、重新生成恢复码、增删通行密钥；管理员还包括新建管理员、改用户密码或角色、停用或删除用户、重新启用被停用的管理员、重置别人的两步登录。

站点设置（**设置 → 安全**）：

| 键 | 默认值 | 含义 |
|-|-|-|
| `security.require_2fa_admins` | 关 | 管理员必须开验证器应用或通行密钥 |
| `security.require_2fa_grace_days` | 7 | 从规则打开（或账号创建）起过这么多天，管理操作就返回 403 `mfa_enrollment_required`；管理员自己的页面仍能打开，用来补开，但没有管理权限。只能用单点登录的管理员不受约束，除非开了 `security.require_2fa_sso` |
| `security.require_2fa_sso` | 关 | 单点登录也要第二步 |
| `security.login_captcha_after` | 0（关） | 同一地址五分钟内密码登录失败这么多次后要求验证码 |
| `security.captcha_provider` | `off` | `turnstile`（Cloudflare）或 `hcaptcha` |
| `security.captcha_site_key`、`security.captcha_secret` | | 验证码服务的密钥；secret 用主密钥加密 |

验证码 token 由服务端通过核心的出站通道去验证码服务核验。连不上验证码服务，或者验证码开着却跑不起来（比如 secret 是用别的主密钥加密的），登录都会失败并返回 503 `captcha_unavailable`，不会跳过检查。没填全 site key 和 secret 的验证码服务存不进去。控制台的 Content-Security-Policy 只在选中某家验证码服务时才放行它的脚本。注册有自己的验证码设置（[收费功能](sell-kit.zh-CN.md#sign-up-adminsignup)）。

被锁在外面：管理员在用户详情里清掉这个用户的验证方式（`DELETE /api/v1/users/{id}/mfa`，审计记为 `user.mfa_reset`），或者在中转所在的机器上运行 `plugrelay user reset-2fa --user ops@example.com`（同样记审计）。

<a id="graceful-shutdown"></a>
## 平滑停机

收到 SIGTERM 或 SIGINT 后，中转不再接新连接，然后：

1. 让进行中的请求最多再跑 `shutdown.grace`（30 秒），然后断开；
2. 最多再等 `shutdown.drain`（60 秒），让这些请求结算额度、排队的记录写进请求日志；记录队列至少还有 10 秒；
3. 停掉外部插件（最多 10 秒），关闭存储。

这时还没写进去的记录会打日志并计数（`plugrelay_sink_ledger_records_total{outcome="lost"}`）。进程管理器要等够 grace + drain + 30 秒才能强杀：默认值下是 120 秒，调大任一项就跟着加。

| 运行方式 | 设置 | 它的默认值 |
|-|-|-|
| Docker Compose | `stop_grace_period: 120s`（[`deploy/docker-compose.yml`](../deploy/docker-compose.yml) 里已设） | 10 秒 |
| `docker run` | `--stop-timeout 120` | 10 秒 |
| Kubernetes | `terminationGracePeriodSeconds: 120`；`preStop` 钩子的时间也算在里面 | 30 秒 |
| systemd | `TimeoutStopSec=120` | |

<a id="security-checklist"></a>
## 安全检查清单

- [ ] 不是只在回环地址上跑的，前面一定要有 TLS；放在代理后面时，`listen` 或发布的端口只绑 `127.0.0.1`。
- [ ] 只要是共用的，就关掉 `security.local_owner_login`（初始化向导选「团队用」「对外卖额度」或 `init --admin-email` 都会关）。
- [ ] 代理转发原始 `Host`；`trusted_proxies` 只写你自己的代理，前面没有代理就留空。
- [ ] 第一次启动后马上完成初始化向导；系统没有默认管理员。
- [ ] 主密钥用 `plugrelay init` 或 `openssl rand -base64 32` 生成，文件权限 0600 或放进密钥管理服务，和数据库分开备份。
- [ ] 设了 `PLUGRELAY_ADMIN_TOKEN` 的话，要够长够随机；它等于完整的管理员权限。
- [ ] 共用的中转：打开 `security.require_2fa_admins`，设好公网地址（通行密钥要用），每个管理员保管好自己的恢复码。
- [ ] 开放注册或登录页对公网开放：配好验证码（`security.login_captcha_after`、`admin.signup` 的 `captcha_provider`）。
- [ ] 单点登录：设好公网地址；对谁都能注册的登录服务（Google、GitHub、Microsoft `organizations`、飞书/Lark）设好允许的域名或租户；只有登录服务的邮箱确实可控时才开*信任邮箱*。
- [ ] `egress.allow_private_cidrs` 只写自建上游需要的网段，它会对这些网段放开 SSRF 防护。
- [ ] 除非信得过目录里的每个插件，否则不开外部插件；插件以中转的系统用户身份运行。
- [ ] 不记录请求正文（`body_logging` 和每个分组的 `log_bodies` 都关着，这是默认值），除非用户同意。
- [ ] 只用你自己的上游账号，并确认对方条款允许你这样用，尤其是转售。
- [ ] 校验发布产物：校验和、cosign 签名、SBOM（`.goreleaser.yaml`）。

<a id="backups"></a>
## 备份

| 内容 | 怎么做 | 说明 |
|-|-|-|
| 主密钥 | 复制 `master.key` 或那串密钥 | 没有它，任何备份里的密文都读不出来 |
| SQLite | 运行中执行 `sqlite3 plugrelay.db ".backup 'plugrelay-$(date +%F).db'"` | 数据库是 WAL 模式，要用 `.backup`，不能直接 `cp` |
| PostgreSQL | `pg_dump -Fc plugrelay > plugrelay.dump` | 包含请求日志 |
| MySQL | `mysqldump --single-transaction plugrelay > plugrelay.sql` | 包含请求日志 |
| 只备份业务数据 | `plugrelay export -o relay-data.yaml`（要凭据就加 `--with-secrets`） | 可移植的 YAML，不含用户、密钥和日志；用 `plugrelay apply -f` 恢复 |
| 状态 | 不用备份 | 丢了只损失缓存命中和计数 |

<a id="upgrading"></a>
## 升级

1. 读 [GitHub Releases](https://github.com/initxy/plugrelay/releases) 上的发布说明（[怎么写的](releasing.md)）。
2. 备份数据库和主密钥。
3. 换掉二进制或镜像标签，重启。打开存储时自动跑迁移；迁移过之后再降级，只能用备份恢复。
4. 运行 `plugrelay doctor`，检查配置、主密钥、数据库和监听地址。
5. 多个节点时先升一个，其他节点照常服务。

在 PostgreSQL 和 MySQL 上，迁移会加数据库锁，所以多个节点可以同时启动。MySQL 的每条 DDL 都单独提交：迁移做到一半失败，已执行的语句不会回滚，下次启动前要用备份恢复或手工清理。

<a id="master-key-rotation"></a>
## 更换主密钥

把旧密钥放进 `previous_master_keys`（`PLUGRELAY_PREVIOUS_MASTER_KEYS`），新密钥放进 `master_key`。用旧密钥加密的私有状态仍能解开，所以进行中的对话不受影响（[ADR-0003](adr/0003-seal-private-state-with-master-key.md)）。已存的东西不会重新加密：凭据、密文设置（验证码、支付密钥等）、单点登录、MCP 和通知的密钥、验证器应用的密钥，都只能用当初保存时的主密钥读。换完之后把它们重新填一遍；用户要重新绑定验证器应用。
