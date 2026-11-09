// Package builtin links every built-in plugin into the binary and gives
// their chain order.
//
// Importing it (blank import is enough) registers the plugins, the sell
// kit's included; which of them run is the core's job (internal/features).
//
// Test mode (hard rule 22): when the relay runs in test mode, the core
// must not start any plugin that reports data off the box. It does so by
// passing the enabled list through ForTestMode before starting plugins, so
// a stored config cannot switch one back on.
package builtin

import (
	"slices"

	"github.com/initxy/plugrelay/pkg/plugin"

	_ "github.com/initxy/plugrelay/plugins/admin/announcements"
	_ "github.com/initxy/plugrelay/plugins/admin/importnewapi"
	_ "github.com/initxy/plugrelay/plugins/admin/payment"
	_ "github.com/initxy/plugrelay/plugins/admin/payment/epay"
	_ "github.com/initxy/plugrelay/plugins/admin/payment/stripe"
	_ "github.com/initxy/plugrelay/plugins/admin/publicpages"
	_ "github.com/initxy/plugrelay/plugins/admin/redeem"
	_ "github.com/initxy/plugrelay/plugins/admin/rewards"
	_ "github.com/initxy/plugrelay/plugins/admin/signup"
	_ "github.com/initxy/plugrelay/plugins/admin/subscriptions"
	_ "github.com/initxy/plugrelay/plugins/auth/apikey"
	_ "github.com/initxy/plugrelay/plugins/job/modelsync"
	_ "github.com/initxy/plugrelay/plugins/job/notify"
	_ "github.com/initxy/plugrelay/plugins/job/pricesync"
	_ "github.com/initxy/plugrelay/plugins/job/quotaprobe"
	_ "github.com/initxy/plugrelay/plugins/meter/balance"
	_ "github.com/initxy/plugrelay/plugins/meter/quota"
	_ "github.com/initxy/plugrelay/plugins/middleware/access"
	_ "github.com/initxy/plugrelay/plugins/middleware/guardrails"
	_ "github.com/initxy/plugrelay/plugins/middleware/parampolicy"
	_ "github.com/initxy/plugrelay/plugins/middleware/ratelimit"
	_ "github.com/initxy/plugrelay/plugins/middleware/redact"
	_ "github.com/initxy/plugrelay/plugins/provider/bedrock"
	_ "github.com/initxy/plugrelay/plugins/provider/compatible"
	_ "github.com/initxy/plugrelay/plugins/provider/vertex"
	_ "github.com/initxy/plugrelay/plugins/selector/routing"
	_ "github.com/initxy/plugrelay/plugins/sink/metrics"
	_ "github.com/initxy/plugrelay/plugins/sink/requestlog"
	_ "github.com/initxy/plugrelay/plugins/sink/webhook"
)

// Order returns the request-path, record and job plugins in chain order;
// built-in plugins not listed run after them, by id. Providers are not
// listed: they run whenever an upstream uses them. Whether a plugin runs is
// decided by internal/features.
func Order() []string {
	return []string{
		"auth.apikey",
		"middleware.access",
		"middleware.ratelimit",
		"middleware.param-policy",
		"selector.routing",
		"meter.quota",
		"sink.requestlog",
		"sink.metrics",
		"job.modelsync",
		"job.quotaprobe",
		"job.pricesync",
	}
}

// ExternalReportingIDs returns the compiled-in plugins whose Info sets
// ReportsOffBox: the notifications feature's job.notify and sink.webhook,
// and admin.payment and admin.subscriptions, which call payment providers.
// sink.requestlog writes to the relay's own store and sink.metrics is only
// scraped.
func ExternalReportingIDs() []string {
	out := []string{}
	for _, id := range plugin.Registered() {
		if f, ok := plugin.Lookup(id); ok && f().Info().ReportsOffBox {
			out = append(out, id)
		}
	}
	return out
}

// ForTestMode returns enabled without the plugins that report off the box.
// external lists additional ids to drop (external-process or private
// plugins that declared themselves reporting); the built-in ones are always
// dropped.
func ForTestMode(enabled []string, external ...string) []string {
	drop := append(ExternalReportingIDs(), external...)
	return slices.DeleteFunc(slices.Clone(enabled), func(id string) bool { return slices.Contains(drop, id) })
}
