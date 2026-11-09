package pipeline

import (
	"errors"
	"fmt"
	"testing"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Every meter call site (request, batch gate, batch hold, WebSocket
// response) maps a refusal the same way.
func TestMeterRefusal(t *testing.T) {
	quota := &ir.GwError{Kind: ir.ErrQuotaExhausted, Message: "balance too low"}
	cases := []struct {
		name    string
		err     error
		kind    ir.ErrorKind
		msg     string
		noRetry bool
	}{
		{"quota", quota, ir.ErrQuotaExhausted, "balance too low", true},
		{"rate limit", &ir.GwError{Kind: ir.ErrRateLimited, Message: "slow down"}, ir.ErrRateLimited, "slow down", false},
		{"failure", fmt.Errorf("db down: %w", plugin.ErrPluginFailure), ir.ErrInternal, "metering unavailable", false},
		{"failure carrying a rejection", fmt.Errorf("%w: %w", plugin.ErrPluginFailure, quota), ir.ErrInternal, "metering unavailable", false},
		{"plain error", errors.New("secret internal detail"), ir.ErrInternal, "metering unavailable", false},
	}
	for _, c := range cases {
		gw := meterRefusal(c.err)
		if gw.Kind != c.kind || gw.Message != c.msg || gw.NoRetry != c.noRetry {
			t.Errorf("%s: got %+v", c.name, gw)
		}
	}
	if quota.NoRetry {
		t.Error("meterRefusal changed the meter's own error")
	}
}
