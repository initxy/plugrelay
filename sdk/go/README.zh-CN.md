# pluginsdk (Go)

[English](README.md) | 简体中文

用 Go 写 plugrelay 外部进程插件的 SDK：`github.com/initxy/plugrelay/sdk/go/pluginsdk`，就在 plugrelay 模块里。

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

- `Serve` 在 stdio 上收发协议消息；清单设了 `transport: unix` 时改用 `PLUGRELAY_SOCKET` 指定的 unix socket。
- 每个方法一个带类型的注册函数：`Authenticate`、`OnRequest`、`OnAttempt`、`OnFinish`、
  `Rank`、`Meter`、`Consume`、`RunJob`、`AdminHandle`、`Endpoints`、`Prepare`、`Classify`、
  `Send`、`ListModels`、`Refresh`。其余的用 `Handle`（比如 `provider.quota`）。
- 处理函数并发运行。宿主发来 `$/cancelRequest` 或调用超时，它们的 context 就会取消。
- `Host()` 回调 plugrelay：`Log`、`KVGet`/`KVPut`/`KVDelete`/`KVIncr`，以及经中转出站规则发请求的 `HTTP`。
  `kv` 和 `http` 要写在 `permissions.host` 下。
- `Deny(kind, msg)`（作为回复）和 `Reject(kind, msg)`（作为错误）原样交给客户端；其他错误都算插件故障，按 `fail_policy` 处理。
- `Call.SetState` 把一个值交给本插件在同一请求里后面的钩子，它们从 `Call.State` 读。
- provider 用的流辅助函数：`StartEvent`、`TextBlockEvents`、`DoneEvent`、`ErrorEvent`。

stdio 模式下 stdout 专门走协议，千万别往上打印；用 stderr 或 `Host().Log`。

测 provider 用 `pkg/plugin/plugintest`。完整的 provider 例子：`examples/plugins/echo-provider-go`。
协议细节：[`../PROTOCOL.md`](../PROTOCOL.zh-CN.md)。
