# pluginsdk (Go)

[English](README.md) | 简体中文

用 Go 写 plugrelay 的外部进程插件。这个包就在 plugrelay 模块里：
`github.com/initxy/plugrelay/sdk/go/pluginsdk`。

```go
package main

import (
	"context"
	"log"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/sdk/go/pluginsdk"
)

func main() {
	p := pluginsdk.New("no-shouting", "0.1.0") // 必须和 plugin.yaml 里的 id 一致
	p.OnRequest(func(ctx context.Context, c *pluginsdk.Call) (*pluginsdk.Reply, error) {
		if c.Request != nil && len(c.Request.Messages) > 0 {
			last := c.Request.Messages[len(c.Request.Messages)-1]
			for _, b := range last.Content {
				if b.Type == ir.BlockText && len(b.Text) > 8 && b.Text == strings.ToUpper(b.Text) {
					return pluginsdk.Deny(ir.ErrInvalidRequest, "please do not shout"), nil
				}
			}
		}
		return nil, nil
	})
	log.Fatal(p.Serve())
}
```

```yaml
# plugin.yaml
id: no-shouting
version: 0.1.0
kind: middleware
points: [middleware.onRequest]
command: ./no-shouting
request_body: true
```

<a id="what-you-get"></a>
## 提供了什么

- `Serve` 在 stdio 上讲协议；清单设置了 `transport: unix` 时，改在 `PLUGRELAY_SOCKET`
  指定的 unix socket 上讲。
- 每个方法一个带类型的注册函数：`Authenticate`、`OnRequest`、`OnAttempt`、`OnFinish`、
  `Rank`、`Meter`、`Consume`、`RunJob`、`AdminHandle`、`Endpoints`、`Prepare`、`Classify`、
  `Send`、`ListModels`、`Refresh`；其他方法用 `Handle`（比如 `provider.quota`）。
- 处理函数并发运行；宿主发来 `$/cancelRequest` 或调用超时时，它们的 context 会被取消。
- `Host()` 用来回调 plugrelay：`Log`、`KVGet`/`KVPut`/`KVDelete`/`KVIncr`、`HTTP`
  （遵守中转的出站规则）。要在 `permissions.host` 下声明 `kv` 和 `http`。
- `Deny(kind, msg)`（作为回复返回）和 `Reject(kind, msg)`（作为错误返回）是有意的拒绝，
  会原样交给客户端；其他错误都算插件故障，宿主按插件的 `fail_policy` 处理。
- `Call.SetState` 把一个值交给本插件在同一个请求里后续的钩子（用 `Call.State` 取）。
- 给 provider 用的流辅助函数：`StartEvent`、`TextBlockEvents`、`DoneEvent`、`ErrorEvent`。

stdio 模式下千万别往 stdout 打印：那上面跑的是协议。用 stderr 或 `Host().Log`。

测 provider 用 `pkg/plugin/plugintest`；完整的 provider 示例见
`examples/plugins/echo-provider-go`，协议契约（消息格式和交互规则）见
[`../PROTOCOL.md`](../PROTOCOL.zh-CN.md)。
