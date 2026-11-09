# pluginsdk (Go)

English | [简体中文](README.zh-CN.md)

Write plugrelay external process plugins in Go. The package lives in the
plugrelay module: `github.com/initxy/plugrelay/sdk/go/pluginsdk`.

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

- `Serve` speaks the protocol on stdio, or on the unix socket in
  `PLUGRELAY_SOCKET` when the manifest sets `transport: unix`.
- One typed registration per method: `Authenticate`, `OnRequest`,
  `OnAttempt`, `OnFinish`, `Rank`, `Meter`, `Consume`,
  `RunJob`, `AdminHandle`, `Endpoints`, `Prepare`, `Classify`, `Send`,
  `ListModels`, `Refresh`; `Handle` for anything else (`provider.quota`).
- Handlers run concurrently; their context is cancelled when the host sends
  `$/cancelRequest` or times the call out.
- `Host()` calls back into plugrelay: `Log`, `KVGet`/`KVPut`/`KVDelete`/
  `KVIncr`, `HTTP` (through the relay's egress rules). Declare `kv` and
  `http` under `permissions.host`.
- `Deny(kind, msg)` (a reply) and `Reject(kind, msg)` (an error) are
  deliberate refusals that reach the client as is; any other error is a
  plugin failure and the host applies the plugin's `fail_policy`.
- `Call.SetState` hands a value to this plugin's later hooks of the same
  request (`Call.State`).
- Stream helpers for providers: `StartEvent`, `TextBlockEvents`,
  `DoneEvent`, `ErrorEvent`.

Never print to stdout in stdio mode: it carries the protocol. Use stderr or
`Host().Log`.

Test a provider with `pkg/plugin/plugintest`; see
`examples/plugins/echo-provider-go` for a complete provider and
[`../PROTOCOL.md`](../PROTOCOL.md) for the wire contract.
