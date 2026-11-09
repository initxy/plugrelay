// Package publicpages is the admin.public-pages plugin: the settings of the
// public models-and-prices and status pages. The pages themselves are served
// by the core's admin API, which reads these settings while the plugin runs.
package publicpages

import "github.com/initxy/plugrelay/pkg/plugin"

// ID is the plugin id.
const ID = "admin.public-pages"

func init() { plugin.Register(ID, func() plugin.Plugin { return Plugin{} }) }

// Plugin implements plugin.Admin with no routes of its own.
type Plugin struct{}

// Info implements plugin.Plugin.
func (Plugin) Info() plugin.Info {
	return plugin.Info{
		ID:          ID,
		Kind:        plugin.KindAdmin,
		Version:     "1.0.0",
		Description: "Settings of the public models-and-prices and status pages.",
		FailPolicy:  plugin.FailOpen,
		Settings: []plugin.Setting{
			{Key: "price_group", Type: plugin.TypeGroup, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Show the prices of this group", "公开哪个分组的价格"),
				Help: plugin.T("The page lists the models this group may use, at its prices. Empty: every model at list price.",
					"页面列出这个分组能用的模型和它的价格。留空：列出所有模型，按标价显示。")},
			{Key: "show_status", Type: plugin.TypeBool, Default: true, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Publish the status page", "公开服务状态页"),
				Help:  plugin.T("Each model's success rate over the last 24 hours.", "每个模型最近 24 小时的成功率。")},
			{Key: "per_ip_per_minute", Type: plugin.TypeInt, Default: 60, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Page loads per minute from one address", "同一地址每分钟最多打开几次"),
				Help:  plugin.T("Further loads are refused for a while.", "超过后暂时拒绝。")},
		},
	}
}

// Routes implements plugin.Admin.
func (Plugin) Routes() []plugin.AdminRoute { return nil }
