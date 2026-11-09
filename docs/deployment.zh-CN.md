# 部署

[English](deployment.md) | 简体中文

同一个二进制文件能支撑所有部署形态。选能满足需求的最小那种：

| 形态 | 监听 | 存储 / 状态 | 控制台 | 典型环境 |
|-|-|-|-|-|
| 本机，单人使用 | `127.0.0.1:8787` | SQLite / memory 或 sql | 开；在回环地址上不用登录 | 笔记本 |
| 团队，单节点 | 回环地址，前面放 TLS 代理 | SQLite、PostgreSQL 或 MySQL / sql 或 Redis | 开，需要登录 | 一台小虚拟机 |
| 团队或售卖，多节点 | 负载均衡后面 | PostgreSQL 或 MySQL / Redis（必须） | 开 | 容器 |

下文提到的每项设置都在 [configuration.md](configuration.zh-CN.md) 里有说明。

- [本机单人使用](#local-single-user)
- [团队使用，放在 TLS 反向代理后面](#team-behind-a-tls-reverse-proxy)
- [用 Docker 和 Compose 搭配 PostgreSQL、Redis](#docker-and-compose-with-postgresql-and-redis)
- [单点登录](#single-sign-on)
- [两步登录、通行密钥和验证码](#two-step-sign-in-passkeys-and-captcha)
- [优雅停机](#graceful-shutdown)
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

不需要密钥的本地服务（Ollama、vLLM 等）添加时不带 `--credential-env`，这样它就是免密钥的（[插件](plugins.zh-CN.md#declarative-upstream-manifests)）。其他上游都要先有凭据，才会被路由到。

`init` 不带 `--admin-email` 时，会创建一个随机密码的所有者账号；只要中转站只监听回环地址，控制台和管理 API 就接受来自回环地址的免登录请求（本机所有者规则，见 [API 认证](api.zh-CN.md)）——除非请求带了 `X-Forwarded-For`、`Forwarded` 或 `X-Real-IP`，这些头说明请求是代理转发来的。在回环监听上，控制台的首次安装也只响应 `Host` 为 `localhost` 或回环 IP 的请求。客户端接入见 [clients.md](clients.zh-CN.md)。

`init` 里和控制台账号相关的参数：

- `--admin-email E` 创建一个管理员而不是所有者账号，同时关掉本机所有者规则。`--listen` 不是回环地址时必须带上（或者传 `--no-admin`）。
- `--admin-password P` 是这个管理员的密码。它会留在 shell 历史里：最好用 `PLUGRELAY_ADMIN_PASSWORD`，或者两个都不设，从标准输入敲进去。没有默认密码。
- `--admin-name N` 是管理员的显示名（默认 `admin`）。
- `--no-admin` 既不创建管理员，也不创建所有者账号；之后由控制台的首次安装来创建管理员，谁先打开谁就能建。

<a id="team-behind-a-tls-reverse-proxy"></a>
## 团队使用，放在 TLS 反向代理后面

```bash
PLUGRELAY_ADMIN_PASSWORD=... plugrelay init \
  --admin-email ops@example.com --public-url https://relay.example.com \
  --config /etc/plugrelay/relay.yaml
plugrelay serve --config /etc/plugrelay/relay.yaml
```

`listen` 保持 `127.0.0.1:8787`，在代理上终结 TLS。代理不能缓冲：流式响应是一个事件一个事件转发的，上游忙的时候每 15 秒有一次保活，单个响应可能持续很多分钟（单次尝试的总超时是 30 分钟）。要转发原始的 `Host` 请求头，并设置 `X-Forwarded-For`——本机所有者规则只适用于 `Host` 是回环主机名且不带转发头的请求；如果代理把 `Host` 改写成 `127.0.0.1`，又两个头都不发，那么在本机所有者规则开着时，每个访问者都能以所有者身份操作。像上面那样用 `init` 创建管理员（这同时会关掉这个规则）：在代理后面，控制台的首次安装会拒绝公网 `Host`。

Caddy（对流式响应本来就不缓冲；`flush_interval -1` 只是写明）：

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
        proxy_read_timeout 35m;         # 要大于出口的总超时
        proxy_send_timeout 35m;
    }
}
```

Cursor 是从它自己的服务器发起调用的，所以用 Cursor 的团队需要这个公网 HTTPS 入口，并设置公网地址（站点设置 `site.public_base_url`）。

默认情况下客户端地址就是 TCP 对端，所以放在代理后面时，所有请求看起来都来自代理。把代理写进 `trusted_proxies`（[配置](configuration.zh-CN.md#top-level)），IP 白名单、按 IP 限流、日志和登录限流才能看到真实的客户端：

```yaml
trusted_proxies: [127.0.0.1, "::1"]   # 上面那个代理，和中转站在同一台机器上
```

代理必须像 nginx 示例那样覆盖或追加 `X-Forwarded-For`（`$proxy_add_x_forwarded_for`；Caddy 默认就会）。不可信客户端能连进来的地址绝不能写进去：可信对端可以冒充任何客户端地址。在 Docker 里，写代理容器的地址或网络，不要写整个宿主机。

探针：`GET /healthz`（进程在跑）和 `GET /readyz`（快照已加载、存储可达）。Prometheus：`GET /metrics`，设置了 `PLUGRELAY_ADMIN_TOKEN` 时需要它才能访问。

<a id="docker-and-compose-with-postgresql-and-redis"></a>
## 用 Docker 和 Compose 搭配 PostgreSQL、Redis

镜像（`Dockerfile`，distroless，非 root）运行 `plugrelay serve --config /data/relay.yaml`。没有 relay.yaml 时用默认值，所以容器可以只靠 `PLUGRELAY_*` 环境变量来配置。

[`deploy/docker-compose.yml`](../deploy/docker-compose.yml) 把 plugrelay 和 PostgreSQL、Redis 一起跑起来，全部用环境变量配置：

```bash
cp deploy/.env.example deploy/.env      # 设置 PLUGRELAY_MASTER_KEY、POSTGRES_PASSWORD
docker compose -f deploy/docker-compose.yml up -d
```

- 中转站的端口发布在 `127.0.0.1:8787`；前面放上文的代理。
- 第一次启动时日志会显示 `first run: open the setup wizard`；打开 `/setup`（通过代理或 SSH 隧道）创建管理员。马上去做——管理员建好之前，谁先打开向导谁就能建。然后在“设置”里填公网地址。
- CLI 命令在容器里用同样的环境变量运行：

  ```bash
  docker compose -f deploy/docker-compose.yml exec plugrelay plugrelay upstream list
  docker compose -f deploy/docker-compose.yml exec plugrelay plugrelay key create --name ci
  ```

- `upstream add --credential-env` 用到的上游 API 密钥必须在容器的环境变量里；在控制台里添加凭据就不用这样。
- 多个中转节点：让几个 `plugrelay` 副本连同一个 PostgreSQL 和 Redis。状态必须用 Redis——memory 和 sql 状态都是每个节点各自一份。
- 用 MySQL 代替 PostgreSQL：compose 文件开头的注释里写了要换的服务和两个存储变量。
- `deploy/.env` 里有主密钥；别放进版本控制，并且要备份。

<a id="single-container-with-sqlite"></a>
### 单容器加 SQLite

挂载一个属于容器用户的宿主机目录，把存储指向它。先把主密钥生成到一个文件里并保管好：以后要读出已存的凭据，只能靠它。

```bash
openssl rand -base64 32 > master.key && chmod 600 master.key
docker run -d --name plugrelay -p 127.0.0.1:8787:8787 --stop-timeout 120 \
  --user "$(id -u):$(id -g)" -v "$PWD/data:/data" \
  -e PLUGRELAY_LISTEN=0.0.0.0:8787 \
  -e PLUGRELAY_MASTER_KEY="$(cat master.key)" \
  -e PLUGRELAY_STORE_DSN=/data/plugrelay.db -e PLUGRELAY_STATE_DRIVER=sql \
  ghcr.io/initxy/plugrelay:latest
```

第一个正式版本发布之前没有公开镜像：在代码仓库里用 `docker build -t plugrelay .` 自己构建，然后用 `plugrelay` 代替 `ghcr.io/initxy/plugrelay:latest`。

<a id="single-sign-on"></a>
## 单点登录

控制台可以通过 OpenID Connect 或 OAuth 身份提供方让用户登录。先打开 `sso` 功能：它关着时不提供任何登录方式，密码登录始终可用。登录方式在控制台的 **系统 → 登录方式** 里配置（或 `POST /api/v1/sso/providers`），存在数据库里；客户端密钥用主密钥加密。对身份提供方的调用（发现、公钥、令牌、用户信息）都走核心出口和它的 SSRF 防护。代码：`internal/admin/sso_*.go`。

前提：必须设置站点设置 *公网地址*（`site.public_base_url`），除非中转站只监听回环地址、且通过回环主机名打开——回调地址由它生成，从不取请求的 `Host`。每个登录方式的回调地址都会在控制台里显示：

```text
https://relay.example.com/api/v1/auth/sso/<provider id>/callback
```

登录流程：登录页链接到 `/api/v1/auth/sso/<id>/start`，它跳转到身份提供方，带上 PKCE（S256）、一个通过 `pr_sso` cookie 绑定到当前浏览器的一次性 `state`，以及（OIDC 时）`nonce`。回调时用授权码换令牌；OIDC 还会用提供方的 JWKS 校验 ID 令牌签名（RS/PS/ES 256–512，公钥缓存一小时，出现未知 `kid` 时重新拉取），并检查 `iss`、`aud`、`azp`、`exp`、`nbf`、`iat` 和 `nonce`。然后按以下顺序把身份对应到用户：

1. 已经关联到某个用户的身份（`provider`、`subject`）；
2. 开了 **按邮箱关联已有用户** 时，邮箱与提供方给出的*已验证*邮箱相同（不区分大小写）的用户。管理员账号会跳过，除非 **也关联管理员账号**（`link_admins`）也开着；
3. 开了 **首次登录时创建用户** 时，新建一个用户，角色取默认角色（`user` 或 `member`，绝不会是 `admin`），分组取第一条匹配的分组规则（已验证邮箱的 `@domain`，或 `groups` 这类声明加上某个值），都不匹配就用默认分组。GitHub 登录方式只接受域名规则，飞书/Lark 只接受域名或 `tenant_key` 规则：它们的其他资料字段是用户自己填的。

提供方说邮箱已验证，才算已验证。*信任邮箱*（`trust_email`）只在提供方什么都没说时补位；明确给出 `email_verified: false` 的仍然算未验证。飞书/Lark 登录方式必须先设置允许的租户，才能打开*信任邮箱*。关联关系限定在提供方的 issuer、租户和客户端 ID 上：改了其中任何一个，所有用它登录过的人都会被解除关联，下次登录时重新匹配。

其他情况下登录页会显示原因（`/login?sso_error=<code>`）。允许的邮箱域名要求邮箱已验证；允许的租户匹配 Entra 的 `tid` 或飞书/Lark 的 `tenant_key`。登录成功后设置的会话和 CSRF cookie 与密码登录相同，并向审计日志写入 `auth.login`（发生建用户和关联身份时，还会写 `user.create` 和 `user.identity.link`）。登录失败和密码输错计入同一个按地址的限额；同一地址五分钟内发起超过 60 次登录也会被拒。

<a id="provider-setup"></a>
### 配置登录方式

| 预设 | 在身份提供方那边 | 在 plugrelay 里 |
|-|-|-|
| `google` | Google Cloud 控制台 → APIs & Services → Credentials → OAuth client ID，类型选 *Web application*；添加回调地址。 | 客户端 ID 和密钥。设置允许的域名（如 `example.com`）可以挡住个人 Gmail 账号。 |
| `microsoft` | Entra 管理中心 → App registrations → New registration，平台选 *Web*，填回调地址；Certificates & secrets → 新建客户端密钥。可选声明：给 ID 令牌加上 `email`。 | 租户填目录（租户）ID 或某个组织的已验证域名；填 `organizations` 则接受任何工作账号（这时在允许的租户里列出租户 ID）。单租户应用信任邮箱；多租户需要 `xms_edov` 或打开*信任邮箱*。 |
| `keycloak` | Realm → Clients → Create，选 OpenID Connect，打开 *Client authentication* 和 *Standard flow*；有效回调地址填上面的地址。 | issuer 填 `https://<host>/realms/<realm>`。要用分组规则，就加一个 *Group Membership* 映射器（声明 `groups`，关闭 full path）。 |
| `authentik` | Applications → Providers → OAuth2/OpenID Provider，客户端类型选 *Confidential*，填回调地址；再为它建一个 application。 | issuer 填 `https://<host>/application/o/<application slug>/`。 |
| `oidc` | 任何在 issuer 下提供 `/.well-known/openid-configuration` 的身份提供方。 | issuer URL 和提供方发布的完全一致。 |
| `github` | Settings → Developer settings → OAuth Apps → New OAuth App；回调地址填上面的地址。GitHub 用的是 OAuth 2.0，不是 OIDC：plugrelay 读取 `/user` 和 `/user/emails`。 | 客户端 ID 和密钥。只认*已验证的主*邮箱；subject 是 GitHub 用户的数字 id。 |
| `feishu` / `lark` | open.feishu.cn / open.larksuite.com → 开发者后台 → 创建企业自建应用；安全设置 → 添加重定向 URL；开启网页登录；要拿邮箱就加上权限 `contact:user.email:readonly`。 | App ID 和 App Secret 分别填作客户端 ID 和密钥。subject 是 `open_id`。飞书/Lark 的通讯录邮箱是租户管理员录入的，用户从没确认过，所以除非打开 **信任提供方的邮箱**，否则不算已验证；应该改用允许的租户（`tenant_key`）来限制登录。 |

<a id="turning-password-sign-in-off"></a>
### 关闭密码登录

SSO 能用以后，可以关掉密码登录（站点设置 `security.password_login`）。只有存在已启用的登录方式、且执行操作的管理员本人是通过它登录的（运维令牌随时都可以），中转站才允许关闭；密码登录关着时，也不允许停用或删除最后一个已启用的登录方式。以下应急途径始终可用：

- `PLUGRELAY_ADMIN_TOKEN`（或已存的管理员令牌）：
  `curl -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"values":{"security.password_login": true}}' https://relay.example.com/api/v1/settings`
- 在中转站所在机器上，直接对数据库操作：
  `PLUGRELAY_ADMIN_PASSWORD='new long password' plugrelay user reset-password --user ops@example.com`
  会设置密码、结束这个用户的所有会话，并重新打开密码登录。

<a id="two-step-sign-in-passkeys-and-captcha"></a>
## 两步登录、通行密钥和验证码

每个控制台用户在 **安全** 页面（用户菜单 → 安全）里自己开启这些功能。代码：`internal/admin/mfa*.go`、`totp.go`、`webauthn.go`、`captcha.go`；验证因素存在存储里（`user_totp`、`user_recovery_codes`、`user_passkeys`）。

- **身份验证器 App（TOTP）。** RFC 6238，SHA-1，6 位数字，30 秒一个时间步，前后各容忍一个时间步的时钟偏差。密钥用主密钥加密；二维码在浏览器里画。每个验证码只能用一次：首次使用后它所在的时间步就作废，所有节点都认。
- **恢复码。** 十个一次性验证码，第一次开启验证因素时和每次重新生成时显示一次；只存哈希。
- **通行密钥（WebAuthn）。** 注册时 attestation 为 `none`（不校验 attestation 声明），支持 ES256、EdDSA 和 RS256 密钥，用户验证设为 preferred。通行密钥可以作为密码之后的第二步；带用户验证时也可以免密码登录（“用通行密钥登录”），但密码登录关闭时不允许这种方式。RP ID 是 *公网地址* 设置中的主机，唯一接受的 origin 就是这个地址的 origin；没有设置时，只有只监听回环地址、且通过回环主机名打开的中转站才提供通行密钥，其余情况一律拒绝（`public_url_missing`）。签名计数器没有增长的请求会被拒绝，并记录为可能被克隆的认证器。基于标准库实现，不依赖 WebAuthn 库。

开启了验证因素后，`POST /api/v1/auth/login` 返回 202，只设置 `pr_mfa` cookie（有效五分钟，最多答五次）；完成第二步（`POST /api/v1/auth/mfa`）后才下发会话 cookie。输错验证码按地址和按账号计入登录限流。单点登录跳过第二步——身份提供方通常有自己的 MFA——除非开了 `security.require_2fa_sso`；这时回调会把浏览器送到 `/login?mfa=1`。

关闭身份验证器 App、重新生成恢复码、添加或删除通行密钥，以及管理员创建管理员、修改用户的密码或角色、停用用户、重新启用被停用的管理员、删除用户、重置其两步登录，都要求五分钟内登录过或重新确认过身份（密码、验证码或通行密钥）（`POST /api/v1/me/reauth`）。

站点设置（**设置 → 安全**）：

| 键 | 默认值 | 含义 |
|-|-|-|
| `security.require_2fa_admins` | 关 | 管理员必须开启身份验证器 App 或通行密钥 |
| `security.require_2fa_grace_days` | 7 | 从规则开启（或账号创建）起过这么多天后，管理员操作返回 403 `mfa_enrollment_required`；管理员自己的页面仍然可以打开以便完成设置，但在这些页面上没有管理员权限。只能通过 SSO 登录的管理员不受此限，除非开了 `security.require_2fa_sso` |
| `security.require_2fa_sso` | 关 | SSO 登录也要求第二步 |
| `security.login_captcha_after` | 0（关） | 同一地址五分钟内密码登录失败达到这个次数后，要求填验证码 |
| `security.captcha_provider` | `off` | `turnstile`（Cloudflare）或 `hcaptcha` |
| `security.captcha_site_key`、`security.captcha_secret` | | 验证码服务商的密钥；secret 用主密钥加密 |

验证码令牌在服务端通过核心出口调用服务商的 siteverify 接口校验；服务商连不上时登录直接失败（503 `captcha_unavailable`）。保存服务商时如果没填 site key 和 secret 会被拒绝；验证码开着却跑不起来（例如 secret 是用另一把主密钥加密的）也会返回 503 `captcha_unavailable`，而不是跳过检查。控制台的 Content-Security-Policy 只在选中某个服务商时（在这里或注册设置里）才放行它的脚本和 frame 来源。注册有自己的验证码设置（[售卖套件](sell-kit.zh-CN.md#sign-up-adminsignup)）。

登不进去了：管理员可以在用户详情侧栏里清除这个用户的验证因素（`DELETE /api/v1/users/{id}/mfa`，审计记为 `user.mfa_reset`），或者在中转站所在机器上用 `plugrelay user reset-2fa --user ops@example.com` 直接对数据库操作（同样会记审计）。

<a id="graceful-shutdown"></a>
## 优雅停机

收到 SIGTERM 或 SIGINT 后，中转站停止接受新连接，然后：

1. 让进行中的请求最多再跑 `shutdown.grace`（30 秒），然后关掉它们的连接；
2. 最多等待 `shutdown.drain`（60 秒），让被切断的请求结算额度，让排队的记录交给各个记录接收插件 sink——即使请求用完了整个 drain 时间，记录队列也至少还有 10 秒；
3. 停止外部插件（最多 10 秒），关闭存储。

到这时还没写入的记录会写日志并计数（`plugrelay_sink_ledger_records_total{outcome="lost"}`）。运行中转站的程序在杀掉它之前，等待时间必须超过 grace + drain + 30 秒——默认值下是 120 秒：

- Docker Compose：`stop_grace_period`（[`deploy/docker-compose.yml`](../deploy/docker-compose.yml) 里设为 `120s`）；`docker run` 需要 `--stop-timeout 120`（默认 10 秒）。
- Kubernetes：在 pod spec 里设 `terminationGracePeriodSeconds: 120`（默认 30 秒）。`preStop` 钩子也占用同一个时间预算。
- systemd：`TimeoutStopSec=120`。

调大 `shutdown.grace` 或 `shutdown.drain`，这个数也要跟着调大。

<a id="security-checklist"></a>
## 安全检查清单

- [ ] 凡是不在回环地址上的，前面都要有 TLS；在代理后面时 `listen` 保持回环地址，或者发布的端口绑定在 `127.0.0.1`。
- [ ] 凡是共享使用的，都关掉 `security.local_owner_login`（安装向导选“团队”“售卖”和 `init --admin-email` 都会关掉它）——它开着时会信任来自回环地址的免登录请求。
- [ ] 代理转发原始的 `Host`。
- [ ] `trusted_proxies` 只列你自己的代理；中转站前面什么都没有时留空。
- [ ] 第一次启动后立刻完成安装向导；系统没有默认管理员，别让别人抢先打开 `/setup`。
- [ ] 主密钥由 `plugrelay init` 或 `openssl rand -base64 32` 生成，文件权限 0600 或放在密钥管理系统里，并且和数据库分开备份。
- [ ] 如果设置了 `PLUGRELAY_ADMIN_TOKEN`，它要足够长且随机；它拥有完整的管理员权限。
- [ ] 两步登录：共享的中转站打开 `security.require_2fa_admins`；设置公网地址，通行密钥才能用；每个管理员保存好自己的恢复码。
- [ ] 开放了公开注册或登录页对公网开放：配置验证码（`security.login_captcha_after`，以及 `admin.signup` 的 `captcha_provider`）。
- [ ] 单点登录：设置公网地址；任何人都能注册的身份提供方（Google、GitHub、Microsoft `organizations`、飞书/Lark）要设置允许的域名或租户；只有提供方的邮箱确实受控时才打开*信任邮箱*。
- [ ] `egress.allow_private_cidrs` 只列自建上游需要的网段——它会为这些网段打开 SSRF 防护的口子。
- [ ] 外部插件保持关闭（`external_plugins.enabled: false`），除非你信任目录里的每一个插件；它们以中转站的操作系统用户身份运行。
- [ ] 不记录请求正文（`body_logging` 和每个分组的 `log_bodies` 都关着，这是默认值），除非用户同意。
- [ ] 只用你自己拥有、且条款允许你这样用（尤其是转售）的上游账号。plugrelay 不做订阅池化，也不冒充客户端。
- [ ] 校验发布产物：校验和、cosign 签名、SBOM（`.goreleaser.yaml`）。

<a id="backups"></a>
## 备份

| 备份什么 | 怎么备份 | 说明 |
|-|-|-|
| 主密钥 | 复制 `master.key` / 密钥本身 | 没有它，任何备份里的凭据密钥和密文设置都读不出来 |
| SQLite | 运行中执行 `sqlite3 plugrelay.db ".backup 'plugrelay-$(date +%F).db'"` | 数据库运行在 WAL 模式下；用 `.backup` 复制，不要用 `cp` |
| PostgreSQL | `pg_dump -Fc plugrelay > plugrelay.dump` | 包含请求日志 |
| MySQL | `mysqldump --single-transaction plugrelay > plugrelay.sql` | 包含请求日志 |
| 只备份业务数据 | `plugrelay export -o relay-data.yaml`（要凭据就加 `--with-secrets`） | 可移植的 YAML；不含用户、密钥和日志；用 `plugrelay apply -f` 恢复 |
| 状态 | 不用备份 | memory/Redis 状态丢了无所谓：只损失缓存命中和计数器 |

<a id="upgrading"></a>
## 升级

1. 阅读 [GitHub Releases](https://github.com/initxy/plugrelay/releases) 上的发布说明（[发布说明怎么写](releasing.md)）。
2. 备份数据库和主密钥。
3. 替换二进制文件或镜像标签，然后重启。迁移脚本内嵌在程序里，打开存储时自动执行；迁移之后要降级，得用备份恢复。
4. `plugrelay doctor` 检查配置、主密钥、数据库和监听地址。
5. 多节点：先升级一个节点，其他节点继续服务。

在 PostgreSQL 和 MySQL 上，迁移在数据库锁下执行，所以多个节点可以同时启动。MySQL 的每条 DDL 语句都会单独提交：迁移中途失败时，已经执行完的语句会留下来，下次启动前需要用备份恢复（或手工清理）。

<a id="master-key-rotation"></a>
## 更换主密钥

把旧密钥放进 `previous_master_keys`（或 `PLUGRELAY_PREVIOUS_MASTER_KEYS`），新密钥放进 `master_key`。用旧密钥加密的内容仍然能解开，所以会话的私有状态不会丢（[ADR-0003](adr/0003-seal-private-state-with-master-key.md)）。已存的东西不会重新加密：数据库里的每一项密钥（凭据、密文设置如验证码和支付密钥、SSO、MCP 和通知的密钥、身份验证器 App 的密钥）都只能用保存时的那把密钥读出。换完之后要把它们重新填一遍；用户要重新绑定身份验证器 App。
