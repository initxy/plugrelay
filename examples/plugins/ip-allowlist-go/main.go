// Command ip-allowlist is an external middleware plugin that rejects
// requests whose client address is outside the CIDR ranges of its "allow"
// setting. An empty list allows every address.
package main

import (
	"context"
	"log"
	"net/netip"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/sdk/go/pluginsdk"
)

func main() {
	p := pluginsdk.New("ip-allowlist", "0.1.0")
	p.OnRequest(func(ctx context.Context, c *pluginsdk.Call) (*pluginsdk.Reply, error) {
		return check(c.Setting("allow").List(), c.ClientIP), nil
	})
	if err := p.Serve(); err != nil {
		log.Fatal(err)
	}
}

// check returns the refusal for ip, or nil when ranges allow it.
func check(ranges []string, ip string) *pluginsdk.Reply {
	if len(ranges) == 0 {
		return nil
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return pluginsdk.Deny(ir.ErrPermission, "client address is unknown")
	}
	addr = addr.Unmap()
	for _, r := range ranges {
		pfx, err := parse(r)
		if err != nil {
			// stderr reaches the relay's log.
			log.Printf("ignoring invalid CIDR in allow: %q", r)
			continue
		}
		if pfx.Contains(addr) {
			return nil
		}
	}
	return pluginsdk.Deny(ir.ErrPermission, "address "+addr.String()+" is not allowed to use this relay")
}

// parse reads a CIDR, or a bare IP as a single-address range.
func parse(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "/") {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), nil
	}
	pfx, err := netip.ParsePrefix(s)
	return pfx.Masked(), err
}
