# 参与 plugrelay 开发

[English](CONTRIBUTING.md) | 简体中文

感谢帮忙。这里是简版；人和编程智能体共同遵守的工作规则写在 `AGENTS.md` 里。

<a id="ground-rules"></a>
## 基本规则

- **不冒充客户端，不做订阅号池。** 伪造 TLS 指纹、客户端请求头或系统提示词的补丁，
  以及让多个用户共用订阅账号的补丁，不管写得多好都不会合并。
- **核心逻辑里不出现客户端或上游的名字。** 客户端的特殊行为归协议编解码器
  （`pkg/protocol/<name>`）处理，上游的特殊行为归 provider 清单（`manifests/`）或插件处理。
  通用转换代码里有没有产品名，有测试专门检查。
- **每个兼容性 bug 都先写一个会失败的测试样本**，放在编解码器的 `testdata/` 下
  （`pkg/protocol/<name>/testdata/`，比如抓包得到的 `codex_request.json` 和
  `claude_code_request.json`），端到端转换的样本放在 `internal/pipeline/testdata/`。

`AGENTS.md`、`docs/gc.md`、`docs/reviewing.md` 和 `docs/specs/` 描述的是维护者借助
智能体干活的流程。贡献者不必照做：提一个带测试、`make check` 全绿的普通 PR 就够了。

<a id="development"></a>
## 开发

环境要求：Go 1.26+、Node 20.19+（CI 用的是 24）；跑 PostgreSQL 和 MySQL 测试的话可以装 Docker（可选）。

```bash
make build            # bin/plugrelay
make test             # Go（开 race）+ 控制台单元测试
make check            # CI 跑的全部关卡——提交评审前必须全绿
PLUGRELAY_TEST_PG=postgres://… make test   # 加上 PostgreSQL 存储测试
PLUGRELAY_TEST_MYSQL='root:pw@tcp(127.0.0.1:3306)/plugrelay' make test   # 和/或 MySQL 存储测试
# 在三种数据库上用 race 反复跑存储测试，每个 -count 大约 4 分钟，
# 会超过 go test 默认的 10 分钟超时：把超时调大。
go test -race -count=3 -timeout 60m ./internal/store/
PLUGRELAY_E2E_BASE=… PLUGRELAY_E2E_KEY=… make e2e   # 真实上游，覆盖每种客户端协议
PLUGRELAY_E2E_CLIENTS=1 …  make e2e                  # 再驱动真的 Claude Code / Codex
```

<a id="demo-relay-for-console-work"></a>
### 开发控制台用的演示中转

`make demo`（即 `plugrelay demo`）在一个用完即弃的 SQLite 存储上跑一个真正的中转，里面
预置了上游、处于各种运行状态的凭据、模型、预设、用户、密钥、计费数据，以及 `--days`
天（默认 14）的请求记录；进程内的假上游会应答所有请求。启动后打印管理员登录信息和一个
演示密钥；不加 `--dir` 的话，退出时数据会被删掉。
`make demo DEMO_FLAGS='--dir /tmp/prdemo --days 30'` 可以在多次运行之间保留数据。
把控制台的开发服务器指向它（见 `web/README.md`）。预置数据的代码在 `internal/demo/`；
某个页面需要的数据没有时，就在那里补。

兼容性相关的改动，提交评审前要用真实客户端跑一遍：只有这项测试能发现客户端升级后发出的
请求变了。

<a id="adding-an-upstream"></a>
## 新增上游

大多数上游只需要一份 YAML 清单：

1. 在 `manifests/` 里复制一份最接近的，改端点、认证、能力和错误规则。schema 是
   `manifests/schema.json`。
2. `plugrelay manifest validate manifests/<id>.yaml`。
3. 通过清单测试跑 provider 一致性测试（`pkg/plugin/plugintest`）；清单测试会自动覆盖
   `manifests/` 下的每个文件。

私有协议：用任意语言写一个外部插件（`sdk/PROTOCOL.md`；Go SDK 在 `sdk/go`）。

<a id="pull-requests"></a>
## Pull request

- 一个 PR 只做一件事；新功能和重构分开提。
- 使用 Conventional Commits 前缀（`feat:`、`fix:`、`docs:`、`refactor:`、`test:`、
  `chore:`）——`feat:` 和 `fix:` 提交会列进发布说明（[发布流程](docs/releasing.md)）。
- 描述里讲清楚*为什么*；*改了什么* diff 已经看得到。
- 新行为要为关键路径写测试，并至少覆盖一条失败路径。

<a id="reporting-security-issues"></a>
## 报告安全问题

请不要为漏洞开公开 issue——见 `SECURITY.md`。
