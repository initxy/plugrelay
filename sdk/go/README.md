# pluginsdk (Go)

English | [简体中文](README.zh-CN.md)

Go SDK for plugrelay external process plugins:
`github.com/initxy/plugrelay/sdk/go/pluginsdk`, part of the plugrelay module.

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
	p := pluginsdk.New("no-shouting", "0.1.0") // must match plugin.yaml id
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

## What you get

- `Serve` talks the protocol on stdio, or on the unix socket in
  `PLUGRELAY_SOCKET` when the manifest sets `transport: unix`.
- A typed register function per method: `Authenticate`, `OnRequest`,
  `OnAttempt`, `OnFinish`, `Rank`, `Meter`, `Consume`, `RunJob`,
  `AdminHandle`, `Endpoints`, `Prepare`, `Classify`, `Send`, `ListModels`,
  `Refresh`. `Handle` covers the rest (`provider.quota`).
- Handlers run concurrently. Their context is cancelled on `$/cancelRequest`
  or when the host times the call out.
- `Host()` calls back into plugrelay: `Log`, `KVGet`/`KVPut`/`KVDelete`/
  `KVIncr`, and `HTTP` through the relay's egress. `kv` and `http` must be
  listed under `permissions.host`.
- `Deny(kind, msg)` (a reply) and `Reject(kind, msg)` (an error) reach the
  client as is. Any other error is a plugin failure and `fail_policy` applies.
- `Call.SetState` passes a value to this plugin's later hooks of the same
  request, which read it from `Call.State`.
- Stream helpers for providers: `StartEvent`, `TextBlockEvents`, `DoneEvent`,
  `ErrorEvent`.

In stdio mode stdout carries the protocol, so never print to it; use stderr
or `Host().Log`.

Test a provider with `pkg/plugin/plugintest`. Full provider:
`examples/plugins/echo-provider-go`. Wire contract:
[`../PROTOCOL.md`](../PROTOCOL.md).
