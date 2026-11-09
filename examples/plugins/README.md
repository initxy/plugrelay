# Example plugins

English | [简体中文](README.zh-CN.md)

Each directory is a complete external plugin (`plugin.yaml` plus code). Point
plugrelay's plugin directory here or copy one out.

| plugin | language | extension point | what it does |
|-|-|-|-|
| [`ip-allowlist-go`](ip-allowlist-go) | Go | `middleware.onRequest` | rejects client addresses outside the `allow` CIDR list (empty allows all) |
| [`echo-provider-go`](echo-provider-go) | Go | `provider.*` | an upstream that streams back the last user message, for tests and demos |

Build them first; `plugin.yaml` runs the binary next to it:

```sh
(cd examples/plugins/ip-allowlist-go && go build -o ip-allowlist .)
(cd examples/plugins/echo-provider-go && go build -o echo-provider .)
```

`internal/extplugin/examples_test.go` builds both and runs them through the
plugin host; there the echo provider also passes the `pkg/plugin/plugintest`
conformance suite.
