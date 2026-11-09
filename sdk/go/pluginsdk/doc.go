// Package pluginsdk writes plugrelay external process plugins in Go.
//
// A plugin registers one handler per protocol method it implements and
// calls Serve, which speaks JSON-RPC 2.0 over Content-Length framed stdio
// (or the unix socket in PLUGRELAY_SOCKET) as specified in sdk/PROTOCOL.md:
//
//	p := pluginsdk.New("ip-allowlist", "0.1.0")
//	p.OnRequest(func(ctx context.Context, c *pluginsdk.Call) (*pluginsdk.Reply, error) {
//		if c.ClientIP == "10.0.0.1" {
//			return pluginsdk.Deny(ir.ErrPermission, "blocked"), nil
//		}
//		return nil, nil
//	})
//	log.Fatal(p.Serve())
//
// Handlers run concurrently, each with a context the host cancels through
// $/cancelRequest. Never write to stdout in stdio mode: it carries the
// protocol. Use stderr or Host().Log.
package pluginsdk
