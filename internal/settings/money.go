package settings

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// microPerUnit is how many stored micro-units one currency unit is.
const microPerUnit = 1_000_000

// ToDisplay renders a stored value for people: a plugin.UnitMoney setting
// becomes currency units (a number, or a list of numbers). Other values are
// returned decoded as they are.
func ToDisplay(s plugin.Setting, raw json.RawMessage) any {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return nil
	}
	if s.Unit != plugin.UnitMoney {
		return v
	}
	unit := func(x any) any {
		var n float64
		switch t := x.(type) {
		case float64:
			n = t
		case string:
			f, err := strconv.ParseFloat(t, 64)
			if err != nil {
				return x
			}
			n = f
		default:
			return x
		}
		return n / microPerUnit
	}
	if l, ok := v.([]any); ok {
		out := make([]any, len(l))
		for i, x := range l {
			out[i] = unit(x)
		}
		return out
	}
	return unit(v)
}

// FromDisplay is the inverse of ToDisplay: it turns currency units into
// the stored micro-units (rounded to the nearest one) for a
// plugin.UnitMoney setting, and returns other values unchanged.
func FromDisplay(s plugin.Setting, raw json.RawMessage) (json.RawMessage, error) {
	if s.Unit != plugin.UnitMoney || string(raw) == "null" {
		return raw, nil
	}
	micro := func(x any) (int64, error) {
		var f float64
		switch t := x.(type) {
		case float64:
			f = t
		case string:
			var err error
			if f, err = strconv.ParseFloat(t, 64); err != nil {
				return 0, fmt.Errorf("must be an amount")
			}
		default:
			return 0, fmt.Errorf("must be an amount")
		}
		if f < 0 || math.IsNaN(f) || math.IsInf(f, 0) || f > math.MaxInt64/microPerUnit {
			return 0, fmt.Errorf("must be an amount of at least 0")
		}
		return int64(math.Round(f * microPerUnit)), nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("must be an amount")
	}
	if s.Type == plugin.TypeList {
		l, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("must be a list of amounts")
		}
		out := make([]string, len(l))
		for i, x := range l {
			n, err := micro(x)
			if err != nil {
				return nil, err
			}
			out[i] = strconv.FormatInt(n, 10)
		}
		return json.Marshal(out)
	}
	n, err := micro(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(n)
}
