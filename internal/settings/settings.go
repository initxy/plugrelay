// Package settings merges plugin settings across the configuration layers
// Global → Group → User → Key, validates values against their plugin's
// declaration, seals secret values, and converts money settings between
// stored micro-units and currency units.
//
// Each plugin.Setting declares its own merge rule. Override: the nearest
// layer that sets a value wins. Strictest: a lower layer may only tighten
// what the layers above allow (for lists every layer's non-empty list must
// allow an entry); an admin can exempt a single key, letting
// that key's own value stand. Union (lists only): every layer's entries
// add up, so a lower layer can grow a deny list but never shrink it.
package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// Entry is one stored setting value on one layer. ScopeID names the group,
// user or key the value is attached to; it is empty for Global.
type Entry struct {
	Scope    plugin.Scope    `json:"scope"`
	ScopeID  string          `json:"scope_id,omitempty"`
	PluginID string          `json:"plugin_id"`
	Key      string          `json:"key"`
	Value    json.RawMessage `json:"value"`
}

// Layers order, nearest last.
var layerOrder = [...]plugin.Scope{plugin.Global, plugin.Group, plugin.User, plugin.Key}

// ScopeName returns the storage name of a single scope: "global", "group",
// "user" or "key".
func ScopeName(s plugin.Scope) string {
	switch s {
	case plugin.Global:
		return "global"
	case plugin.Group:
		return "group"
	case plugin.User:
		return "user"
	case plugin.Key:
		return "key"
	}
	return ""
}

// ParseScope is the inverse of ScopeName.
func ParseScope(name string) (plugin.Scope, error) {
	for _, s := range layerOrder {
		if ScopeName(s) == name {
			return s, nil
		}
	}
	return 0, fmt.Errorf("settings: unknown scope %q", name)
}

// ScopeNames lists the names of every scope set in s, in merge order.
func ScopeNames(s plugin.Scope) []string {
	var out []string
	for _, l := range layerOrder {
		if s&l != 0 {
			out = append(out, ScopeName(l))
		}
	}
	return out
}

// Layered holds one setting's raw value on each layer; nil means the layer
// does not set it.
type Layered struct {
	Global, Group, User, Key json.RawMessage
}

func (l Layered) at(s plugin.Scope) json.RawMessage {
	switch s {
	case plugin.Global:
		return l.Global
	case plugin.Group:
		return l.Group
	case plugin.User:
		return l.User
	case plugin.Key:
		return l.Key
	}
	return nil
}

// Merge combines a setting's layers under its declared rule. Layers the
// setting may not be placed on are ignored. A value that fails validation
// is ignored too, except where ignoring it would loosen a restriction (see
// LayerValue). exempt lets the key layer's own value win over Strictest.
// When no layer sets a value the declared default is returned.
func Merge(s plugin.Setting, l Layered, exempt bool) plugin.Value {
	var vals []json.RawMessage
	var keyVal json.RawMessage
	for _, sc := range layerOrder {
		if s.Scopes&sc == 0 {
			continue
		}
		raw := LayerValue(s, l.at(sc))
		if raw == nil {
			continue
		}
		vals = append(vals, raw)
		if sc == plugin.Key {
			keyVal = raw
		}
	}
	if len(vals) == 0 {
		return defaultValue(s)
	}
	if (s.Merge != plugin.Strictest && s.Merge != plugin.Union) || (exempt && keyVal != nil) {
		return plugin.NewValue(vals[len(vals)-1])
	}
	if s.Merge == plugin.Union {
		return plugin.NewValue(union(vals))
	}
	if s.Type == plugin.TypeList {
		layers := make([][]string, len(vals))
		for i, v := range vals {
			layers[i] = plugin.NewValue(v).List()
		}
		return plugin.NewStrictList(layers)
	}
	return plugin.NewValue(strictest(s.Type, vals))
}

// denyAllLayer is the list layer that allows nothing.
var denyAllLayer, _ = json.Marshal([]string{plugin.DenyAll})

// LayerValue is what one layer's stored value counts as in Merge; nil when
// the layer does not count (unset, null, or invalid and ignored). An
// invalid layer of a Strictest list is an allowlist nobody passes
// ([plugin.DenyAll]): a typo must not lift the restriction. An invalid
// layer of a Union list (a deny list) keeps the strings it holds that are
// valid entries, a lone string counting as a one-entry list. Any other
// invalid layer is ignored (Resolver.WarnInvalid logs it).
func LayerValue(s plugin.Setting, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || isNull(raw) {
		return nil
	}
	if Validate(s, raw) == nil {
		return raw
	}
	if s.Type != plugin.TypeList {
		return nil
	}
	switch s.Merge {
	case plugin.Strictest:
		return denyAllLayer
	case plugin.Union:
		var items []any
		if json.Unmarshal(raw, &items) != nil {
			var one string
			if json.Unmarshal(raw, &one) != nil {
				return nil
			}
			items = []any{one}
		}
		var l []string
		for _, it := range items {
			if e, ok := it.(string); ok && (len(s.Enum) == 0 || slices.Contains(s.Enum, e)) {
				l = append(l, e)
			}
		}
		if len(l) == 0 {
			return nil
		}
		out, _ := json.Marshal(l)
		return out
	}
	return nil
}

func defaultValue(s plugin.Setting) plugin.Value {
	if s.Default == nil {
		return plugin.Value{}
	}
	raw, err := json.Marshal(s.Default)
	if err != nil {
		return plugin.Value{}
	}
	return plugin.NewValue(raw)
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// strictest picks the tightest of already-validated values.
func strictest(t plugin.SettingType, vals []json.RawMessage) json.RawMessage {
	switch t {
	case plugin.TypeInt, plugin.TypeFloat, plugin.TypeDuration:
		// Smallest positive wins; 0 (or unparsable) means unlimited.
		var best json.RawMessage
		bestN := math.Inf(1)
		for _, v := range vals {
			n := magnitude(t, v)
			if n > 0 && n < bestN {
				best, bestN = v, n
			}
		}
		if best == nil {
			return vals[len(vals)-1]
		}
		return best
	case plugin.TypeBool:
		for _, v := range vals {
			if !plugin.NewValue(v).Bool() {
				return v
			}
		}
		return vals[len(vals)-1]
	}
	// Strings, pickers and JSON have no order; the nearest layer wins.
	return vals[len(vals)-1]
}

// union joins list values in layer order, without duplicates.
func union(vals []json.RawMessage) json.RawMessage {
	out := []string{}
	for _, v := range vals {
		for _, e := range plugin.NewValue(v).List() {
			if !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
	}
	raw, _ := json.Marshal(out)
	return raw
}

func magnitude(t plugin.SettingType, raw json.RawMessage) float64 {
	v := plugin.NewValue(raw)
	if t == plugin.TypeDuration {
		return float64(v.Duration())
	}
	return v.Float()
}

// Validate checks a JSON value against a setting's type, enum and, for
// Strictest numbers, sign. JSON null is accepted and means "unset".
func Validate(s plugin.Setting, raw json.RawMessage) error {
	if !json.Valid(raw) {
		return fmt.Errorf("setting %s: invalid JSON", s.Key)
	}
	if isNull(raw) {
		return nil
	}
	fail := func(want string) error {
		return fmt.Errorf("setting %s: want %s, got %s", s.Key, want, abbreviate(raw))
	}
	switch s.Type {
	case plugin.TypeString, plugin.TypeModel, plugin.TypeUpstream, plugin.TypeGroup, plugin.TypeSecret:
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return fail("a string")
		}
		if len(s.Enum) > 0 && !slices.Contains(s.Enum, str) {
			return fmt.Errorf("setting %s: %q is not one of %s", s.Key, str, strings.Join(s.Enum, ", "))
		}
	case plugin.TypeInt:
		var n json.Number
		if !isNumber(raw) || json.Unmarshal(raw, &n) != nil {
			return fail("an integer")
		}
		i, err := n.Int64()
		if err != nil {
			return fail("an integer")
		}
		if s.Merge == plugin.Strictest && i < 0 {
			return fmt.Errorf("setting %s: must be >= 0 (0 = unlimited)", s.Key)
		}
	case plugin.TypeFloat:
		var f float64
		if json.Unmarshal(raw, &f) != nil {
			return fail("a number")
		}
		if s.Merge == plugin.Strictest && f < 0 {
			return fmt.Errorf("setting %s: must be >= 0 (0 = unlimited)", s.Key)
		}
	case plugin.TypeBool:
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return fail("true or false")
		}
	case plugin.TypeDuration:
		var str string
		var f float64
		var d time.Duration
		switch {
		case json.Unmarshal(raw, &str) == nil:
			var err error
			if d, err = time.ParseDuration(str); err != nil {
				return fail(`a duration like "30s"`)
			}
		case json.Unmarshal(raw, &f) == nil:
			d = time.Duration(f * float64(time.Second))
		default:
			return fail(`a duration like "30s" or seconds`)
		}
		if d < 0 {
			return fmt.Errorf("setting %s: duration must not be negative", s.Key)
		}
	case plugin.TypeList:
		var l []string
		if json.Unmarshal(raw, &l) != nil {
			return fail("a list of strings")
		}
		if len(s.Enum) > 0 {
			for _, item := range l {
				if !slices.Contains(s.Enum, item) {
					return fmt.Errorf("setting %s: %q is not one of %s", s.Key, item, strings.Join(s.Enum, ", "))
				}
			}
		}
	case plugin.TypeJSON:
	default:
		return fmt.Errorf("setting %s: unknown type %q", s.Key, s.Type)
	}
	return nil
}

func isNumber(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && (t[0] == '-' || (t[0] >= '0' && t[0] <= '9'))
}

func abbreviate(raw json.RawMessage) string {
	if len(raw) > 40 {
		return string(raw[:40]) + "…"
	}
	return string(raw)
}
