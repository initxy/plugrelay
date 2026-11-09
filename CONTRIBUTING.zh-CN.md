# 参与 plugrelay 开发

[English](CONTRIBUTING.md) | 简体中文

感谢帮忙。提一个带测试、`make check` 全绿的普通 PR 就够了。`AGENTS.md`、`docs/gc.md`、
`docs/reviewing.md` 和 `docs/specs/` 写的是维护者和编程智能体一起干活的流程，你不必照做。

<a id="ground-rules"></a>
## 基本规则

- **不冒充客户端，不做订阅号池。** 伪造 TLS 指纹、客户端请求头或系统提示词的补丁，以及
  让多个用户共用订阅账号的补丁，写得再好也不合并。
- **核心逻辑里不出现客户端或上游的名字。** 客户端的特殊行为放在协议编解码器里
  （`pkg/protocol/<name>`），上游的特殊行为放在清单（`manifests/`）或 provider 插件里。
  有测试专门检查通用转换代码里有没有产品名。
- **每个兼容性 bug 都先写一个会失败的测试样本**，放在编解码器的 `testdata/` 下（比如抓包
  得到的 `codex_request.json` 和 `claude_code_request.json`），端到端转换的样本放在
  `internal/pipeline/testdata/`。

<a id="development"></a>
## 开发

需要 Go 1.26+、Node 20.19+（CI 用 24）；想跑 PostgreSQL 和 MySQL 测试的话再装个 Docker。

```bash
make build            # bin/plugrelay
make test             # Go（开 race）+ 控制台单元测试
make check            # CI 跑的全部关卡，提交评审前必须全绿
PLUGRELAY_TEST_PG=postgres://… make test   # 加上 PostgreSQL 存储测试
PLUGRELAY_TEST_MYSQL='root:pw@tcp(127.0.0.1:3306)/plugrelay' make test   # 和/或 MySQL
# 在三种数据库上跑存储测试，每个 -count 约 4 分钟，
# 会超过 go test 默认的 10 分钟超时。
go test -race -count=3 -timeout 60m ./internal/store/
PLUGRELAY_E2E_BASE=… PLUGRELAY_E2E_KEY=… make e2e   # 真实上游，覆盖每种客户端协议
PLUGRELAY_E2E_CLIENTS=1 …  make e2e                  # 再驱动真的 Claude Code / Codex
```

兼容性相关的改动，提交评审前要用真实客户端跑一遍端到端测试：客户端升级后发的请求变了，
只有它能发现。

<a id="demo-relay-for-console-work"></a>
### 开发控制台用的演示中转

`make demo` 在一个用完即弃的 SQLite 存储上跑一个真正的中转，预置了上游、处于各种运行状态
的凭据、模型、预设、用户、密钥、计费数据和 14 天的请求记录（`--days` 可改）。同一进程里
有个假上游应答所有请求。启动后打印管理员登录信息和一个演示密钥；不加 `--dir` 的话退出时
数据会删掉，想留着就这样：`make demo DEMO_FLAGS='--dir /tmp/prdemo --days 30'`。

把控制台的开发服务器指向它（见 `web/README.md`）。某个页面要的数据预置里没有，就去
`internal/demo/` 里补。

<a id="adding-an-upstream"></a>
## 新增上游

大多数上游只要一份 YAML 清单：

1. 在 `manifests/` 里复制一份最接近的，改端点、认证、能力和错误规则（格式定义在
   `manifests/schema.json`）。
2. `plugrelay manifest validate manifests/<id>.yaml`
3. `go test ./plugins/provider/compatible/ -run TestManifestConformance` 会对 `manifests/`
   下的每个文件跑 provider 一致性测试。

私有协议就用任意语言写一个外部插件（`sdk/PROTOCOL.md`；Go SDK 在 `sdk/go`）。

<a id="pull-requests"></a>
## Pull request

- 一个 PR 只做一件事；新功能和重构分开提。
- 用 Conventional Commits 前缀（`feat:`、`fix:`、`docs:`、`refactor:`、`test:`、
  `chore:`）。`feat:` 和 `fix:` 提交会列进发布说明（[发布流程](docs/releasing.md)）。
- 描述里讲清楚*为什么*；*改了什么* diff 里看得到。
- 新行为要给主路径写测试，并至少覆盖一条失败路径。

<a id="reporting-security-issues"></a>
## 报告安全问题

漏洞请不要开公开 issue，见 `SECURITY.md`。
