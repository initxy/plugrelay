# 示例插件

[English](README.md) | 简体中文

每个目录都是一个完整的外部插件：一个 `plugin.yaml` 加上代码。把 plugrelay 的插件目录指到这里，
或者拷一个出去用。

| 插件 | 语言 | 扩展点 | 做什么 |
|-|-|-|-|
| [`ip-allowlist-go`](ip-allowlist-go) | Go | `middleware.onRequest` | 拒绝不在 `allow` CIDR 列表里的客户端地址 |
| [`echo-provider-go`](echo-provider-go) | Go | `provider.*` | 一个把最后一条用户消息流式返回的上游，用于测试和演示 |

用之前先逐个编译；`plugin.yaml` 运行的是它旁边的二进制：

```sh
(cd examples/plugins/ip-allowlist-go && go build -o ip-allowlist .)
(cd examples/plugins/echo-provider-go && go build -o echo-provider .)
```

`internal/extplugin/examples_test.go` 会编译这两个插件，并通过插件宿主启动它们；
echo provider 还在那里通过了 `pkg/plugin/plugintest` 一致性测试套件。
