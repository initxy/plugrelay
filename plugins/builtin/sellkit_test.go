package builtin

import (
	"testing"

	"github.com/initxy/plugrelay/internal/features"
	"github.com/initxy/plugrelay/pkg/plugin"
)

var sellKit = []string{"meter.balance", "admin.redeem", "admin.payment", "admin.subscriptions", "admin.signup", "admin.import-newapi",
	"admin.rewards", "admin.announcements"}

func TestSellKitOffByDefault(t *testing.T) {
	for _, id := range sellKit {
		if _, ok := plugin.Lookup(id); !ok {
			t.Errorf("%s not registered", id)
			continue
		}
		if f, ok := features.Of(id); !ok || f.Default || features.IsCore(id) {
			t.Errorf("%s must belong to a feature that is off by default", id)
		}
	}
}
