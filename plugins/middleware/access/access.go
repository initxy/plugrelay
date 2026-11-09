// Package access is the middleware.access plugin: a group's source-IP
// allowlist and allowed clients, plus the grants for native passthrough and
// the MCP gateway.
//
// The two allowlists are group settings; a key's own IP allowlist and
// clients live on the key record and are enforced by auth.apikey. Clients
// are client ids (pkg/clientid), on the group as on the key. Which models
// a caller may use is the group's and the key's model list, enforced by
// the core. An empty list allows any address or client.
//
// The MCP gateway reads three more: mcp_servers is a grant like
// native_upstreams (empty = none, "*" = all); mcp_tools_allow narrows the
// tools (Strictest) and mcp_tools_deny only grows down the layers (Union),
// both as patterns over "<server>__<tool>". The core enforces them whether
// or not the plugin is enabled.
//
// native_upstreams is a grant, not a restriction: an empty list allows no
// native passthrough, and the nearest layer that sets it wins (Override),
// so a group or a single key can be given access. The core enforces it with
// CheckNative, whether or not the plugin is enabled, since native requests
// run no request hooks.
package access

import (
	"net/http"
	"slices"

	"github.com/initxy/plugrelay/pkg/clientid"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/auth/apikey"
)

// ID is the plugin id.
const ID = "middleware.access"

func init() { plugin.Register(ID, func() plugin.Plugin { return New() }) }

// clientIDs are the ids allowed_clients may list, named by clientNames.
var clientIDs, clientNames = func() ([]string, map[string]plugin.Text) {
	ids := []string{}
	names := map[string]plugin.Text{}
	for _, c := range clientid.Known() {
		ids = append(ids, c.ID)
		names[c.ID] = plugin.T(c.Name, c.Name)
	}
	ids = append(ids, clientid.Other)
	names[clientid.Other] = plugin.T("Other clients", "其他客户端")
	return ids, names
}()

// Access implements plugin.RequestHook.
type Access struct{}

// New returns the plugin.
func New() *Access { return &Access{} }

// Info implements plugin.Plugin.
func (*Access) Info() plugin.Info {
	return plugin.Info{
		ID:          ID,
		Kind:        plugin.KindMiddleware,
		Version:     "1.0.0",
		Description: "Group IP allowlist and allowed clients; native passthrough and MCP grants.",
		FailPolicy:  plugin.FailClosed,
		Settings: []plugin.Setting{
			{Key: "allowed_ips", Type: plugin.TypeList, Scopes: plugin.Group, Merge: plugin.Strictest,
				Label: plugin.T("Allowed source addresses", "允许的来源地址"),
				Help:  plugin.T("IP addresses or ranges (CIDR); empty allows any.", "IP 地址或网段（CIDR）；留空表示不限。")},
			{Key: "allowed_clients", Type: plugin.TypeList, Scopes: plugin.Group, Merge: plugin.Strictest,
				Enum: clientIDs, EnumLabels: clientNames,
				Label: plugin.T("Allowed clients", "允许的客户端"),
				Help:  plugin.T("Client programs, as the request log names them; empty allows any.", "按请求日志里认出的客户端程序来选；留空表示不限。")},
			{Key: SettingNativeUpstreams, Type: plugin.TypeList, Scopes: plugin.Global | plugin.Group | plugin.User | plugin.Key,
				Merge: plugin.Override, Label: plugin.T("Upstreams reachable as-is", "可直连的上游"),
				Help: plugin.T("Upstreams this caller may reach through /native/ untouched; * means all, empty means none. Works only when native passthrough is on in relay.yaml.", "可以通过 /native/ 原样直连的上游；* 表示全部，留空表示都不行。要在 relay.yaml 里开启原样直连才生效。")},
			{Key: SettingMCPServers, Type: plugin.TypeList, Scopes: plugin.Global | plugin.Group | plugin.User | plugin.Key,
				Merge: plugin.Override, Label: plugin.T("MCP servers allowed", "可用的 MCP 服务"),
				Help: plugin.T("* means all, empty means none.", "* 表示全部，留空表示都不能用。")},
			{Key: SettingMCPToolsAllow, Type: plugin.TypeList, Scopes: plugin.Global | plugin.Group | plugin.User | plugin.Key,
				Merge: plugin.Strictest, Label: plugin.T("MCP tools allowed", "可用的 MCP 工具"),
				Help: plugin.T("Written as server__tool, * matches anything; empty allows every tool of the allowed servers.", "写成 服务__工具，可用 * 通配；留空表示允许的服务里所有工具都能用。")},
			{Key: SettingMCPToolsDeny, Type: plugin.TypeList, Scopes: plugin.Global | plugin.Group | plugin.User | plugin.Key,
				Merge: plugin.Union, Label: plugin.T("MCP tools refused", "禁用的 MCP 工具"),
				Help: plugin.T("Written as server__tool, * matches anything, e.g. github__delete_*.", "写成 服务__工具，可用 * 通配，例如 github__delete_*。")},
		},
	}
}

// Setting keys the core reads for native passthrough and the MCP gateway.
const (
	SettingNativeUpstreams = "native_upstreams"
	SettingMCPServers      = "mcp_servers"
	SettingMCPToolsAllow   = "mcp_tools_allow"
	SettingMCPToolsDeny    = "mcp_tools_deny"
)

// CheckNative applies this plugin's settings to a native passthrough
// request: the upstream must be granted in native_upstreams, and the IP and
// client allowlists hold as for model calls. setting reads this plugin's
// settings merged for the caller.
func CheckNative(setting func(key string) plugin.Value, clientIP string, h http.Header, upstreamID string) *ir.GwError {
	grant := setting(SettingNativeUpstreams).List()
	if !slices.Contains(grant, "*") && !slices.Contains(grant, upstreamID) {
		return ir.Errorf(ir.ErrPermission, "native passthrough to upstream %q is not allowed for this key", upstreamID)
	}
	return CheckClient(setting, clientIP, h)
}

// CheckClient applies the source-address and client allowlists, as the
// plugin does for a model call; h is the request's headers, which name its
// client (clientid.Detect). The core runs it on a WebSocket handshake,
// before the model of a Responses session is known.
func CheckClient(setting func(key string) plugin.Value, clientIP string, h http.Header) *ir.GwError {
	if !setting("allowed_ips").ListAllows(func(ips []string) bool { return apikey.IPAllowed(clientIP, ips) }) {
		return ir.Errorf(ir.ErrPermission, "source address is not allowed")
	}
	if !setting("allowed_clients").ListAllows(func(ids []string) bool { return slices.Contains(ids, clientid.Detect(h).ID) }) {
		return ir.Errorf(ir.ErrPermission, "client is not allowed")
	}
	return nil
}

// OnRequest implements plugin.RequestHook.
func (*Access) OnRequest(c *plugin.Call) (*plugin.Reply, error) {
	if gw := CheckClient(c.Setting, c.ClientIP, c.Header); gw != nil {
		return &plugin.Reply{Error: gw}, nil
	}
	return nil, nil
}
