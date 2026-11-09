package app

import (
	"errors"
	"net/http"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// chainMeters adds next to the meter chain cur (nil, a single meter or a
// meterChain).
func chainMeters(cur, next plugin.Meter) plugin.Meter {
	switch c := cur.(type) {
	case nil:
		return next
	case *meterChain:
		return &meterChain{meters: append(append([]plugin.Meter(nil), c.meters...), next)}
	}
	return &meterChain{meters: []plugin.Meter{cur, next}}
}

// meterChain runs several meters (budgets and a prepaid balance) as one:
// Reserve in chain order, undone in reverse when a later meter refuses;
// Settle and Refund on every meter. Each meter sees its own settings.
type meterChain struct{ meters []plugin.Meter }

var (
	_ plugin.Meter        = (*meterChain)(nil)
	_ plugin.QuotaHeaders = (*meterChain)(nil)
	_ plugin.Supporter    = (*meterChain)(nil)
)

func (m *meterChain) Info() plugin.Info {
	ids := make([]string, len(m.meters))
	for i, x := range m.meters {
		ids[i] = x.Info().ID
	}
	return plugin.Info{ID: strings.Join(ids, "+"), Kind: plugin.KindMeter, FailPolicy: plugin.FailClosed}
}

func (m *meterChain) Supports(method string) bool {
	if method != "meter.headers" {
		return true
	}
	for _, x := range m.meters {
		if _, ok := x.(plugin.QuotaHeaders); ok && supports(x, method) {
			return true
		}
	}
	return false
}

func supports(p any, method string) bool {
	s, ok := p.(plugin.Supporter)
	return !ok || s.Supports(method)
}

const chainKey = "meter-chain/reservations"

func (m *meterChain) Reserve(c *plugin.Call, estimate int64) (*plugin.Reservation, error) {
	self := m.Info().ID
	held := make([]*plugin.Reservation, len(m.meters))
	for i, x := range m.meters {
		var res *plugin.Reservation
		if supports(x, "meter.reserve") {
			var err error
			res, err = x.Reserve(c.WithPlugin(x.Info().ID), estimate)
			if err != nil {
				for j := i - 1; j >= 0; j-- {
					if supports(m.meters[j], "meter.refund") {
						_ = m.meters[j].Refund(c.WithPlugin(m.meters[j].Info().ID), held[j])
					}
				}
				c.WithPlugin(self)
				return nil, err
			}
		}
		if res == nil {
			res = &plugin.Reservation{ID: c.ID}
		}
		held[i] = res
	}
	c.WithPlugin(self).Set(chainKey, held)
	return &plugin.Reservation{ID: c.ID, AmountMicro: max(estimate, 0)}, nil
}

func (m *meterChain) held(c *plugin.Call) []*plugin.Reservation {
	v, _ := c.WithPlugin(m.Info().ID).Get(chainKey)
	held, _ := v.([]*plugin.Reservation)
	return held
}

func (m *meterChain) Settle(c *plugin.Call, _ *plugin.Reservation, u ir.Usage, charge int64) error {
	held := m.held(c)
	var errs []error
	for i, x := range m.meters {
		if i < len(held) && supports(x, "meter.settle") {
			errs = append(errs, x.Settle(c.WithPlugin(x.Info().ID), held[i], u, charge))
		}
	}
	c.WithPlugin(m.Info().ID)
	return errors.Join(errs...)
}

func (m *meterChain) Refund(c *plugin.Call, _ *plugin.Reservation) error {
	held := m.held(c)
	var errs []error
	for i, x := range m.meters {
		if i < len(held) && supports(x, "meter.refund") {
			errs = append(errs, x.Refund(c.WithPlugin(x.Info().ID), held[i]))
		}
	}
	c.WithPlugin(m.Info().ID)
	return errors.Join(errs...)
}

func (m *meterChain) Headers(c *plugin.Call) http.Header {
	out := http.Header{}
	for _, x := range m.meters {
		if q, ok := x.(plugin.QuotaHeaders); ok && supports(x, "meter.headers") {
			for k, v := range q.Headers(c.WithPlugin(x.Info().ID)) {
				out[k] = v
			}
		}
	}
	c.WithPlugin(m.Info().ID)
	return out
}
