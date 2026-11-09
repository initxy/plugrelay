package plugin

import (
	"encoding/json"
	"slices"
	"strconv"
	"time"
)

// Scope is a bit set of configuration layers a setting may be placed on.
// Layers merge in the order Global → Group → User → Key.
type Scope uint8

const (
	Global Scope = 1 << iota
	Group
	User
	Key
)

// Merge says how a lower layer combines with the layers above it.
type Merge string

const (
	// Override: the nearest layer that sets the value wins.
	Override Merge = "override"
	// Strictest: a lower layer may only tighten. For numbers the smaller
	// positive value wins (0 means unlimited); for bools false wins; for
	// lists every layer that sets a non-empty list must allow the entry
	// (see Value.ListAllows); an empty list restricts nothing. An admin may
	// flag one key as an exception.
	Strictest Merge = "strictest"
	// Union: lists only; every layer's entries add up, so a lower layer can
	// add entries (a deny list grows) but never remove one. An exempted key
	// keeps only its own value, as with Strictest.
	Union Merge = "union"
)

// SettingType drives validation and the console form widget.
type SettingType string

const (
	TypeString   SettingType = "string"
	TypeInt      SettingType = "int"
	TypeFloat    SettingType = "float"
	TypeBool     SettingType = "bool"
	TypeDuration SettingType = "duration"
	TypeList     SettingType = "list"     // []string
	TypeSecret   SettingType = "secret"   // stored encrypted, never echoed
	TypeModel    SettingType = "model"    // picker over configured models
	TypeUpstream SettingType = "upstream" // picker over configured upstreams
	TypeGroup    SettingType = "group"    // picker over user groups (a group id)
	TypeJSON     SettingType = "json"
)

// Text is a string shown to people, in the console's two languages.
type Text struct {
	EN string `json:"en"`
	ZH string `json:"zh-CN,omitempty"`
}

// T builds a Text.
func T(en, zh string) Text { return Text{EN: en, ZH: zh} }

// Unit says how a numeric setting is presented.
type Unit string

// UnitMoney marks an integer (or a list of integers) of micro-units of the
// site currency. It is stored in micro-units; the admin API shows and takes
// currency units.
const UnitMoney Unit = "money"

// Setting declares one configuration item a plugin reads.
type Setting struct {
	Key     string      `json:"key"`
	Type    SettingType `json:"type"`
	Default any         `json:"default,omitempty"`
	Scopes  Scope       `json:"scopes"`
	Merge   Merge       `json:"merge"`
	// Label names the setting and Help explains it, for people.
	Label Text `json:"label"`
	Help  Text `json:"help"`
	Unit  Unit `json:"unit,omitempty"`
	// Restart marks settings that only take effect after a restart.
	Restart bool     `json:"restart,omitempty"`
	Enum    []string `json:"enum,omitempty"`
	// EnumLabels names Enum values for people.
	EnumLabels map[string]Text `json:"enum_labels,omitempty"`
}

// Value is a merged setting value.
type Value struct {
	raw json.RawMessage
	set bool
	// layers holds each layer's non-empty list of a Strictest list.
	layers [][]string
}

// DenyAll is the entry a Strictest list's List carries when its layers
// share no entry: it matches nothing, so a reader that treats a non-empty
// list as a restriction denies everything instead of reading the empty
// intersection as "no restriction".
const DenyAll = "\x00deny-all"

// NewStrictList merges list layers under Strictest. Empty layers restrict
// nothing and are dropped. List returns the intersection of the rest, or
// [DenyAll] when they share no entry; ListAllows checks every layer in turn,
// which is what patterns need, since they cannot be intersected as strings.
func NewStrictList(layers [][]string) Value {
	var kept [][]string
	for _, l := range layers {
		if len(l) > 0 {
			kept = append(kept, l)
		}
	}
	set := []string{}
	if len(kept) > 0 {
		set = slices.Clone(kept[0])
		for _, other := range kept[1:] {
			set = slices.DeleteFunc(set, func(s string) bool { return !slices.Contains(other, s) })
		}
		if len(set) == 0 {
			set = []string{DenyAll}
		}
	}
	raw, _ := json.Marshal(set)
	return Value{raw: raw, set: true, layers: kept}
}

// ListLayers returns the non-empty lists that must each allow an entry: the
// layers of a Strictest list, otherwise the value's own list if not empty.
func (v Value) ListLayers() [][]string {
	if v.layers != nil {
		return v.layers
	}
	if l := v.List(); len(l) > 0 {
		return [][]string{l}
	}
	return nil
}

// ListAllows reports whether allowed accepts every list in ListLayers; with
// none (unset or empty) nothing is restricted.
func (v Value) ListAllows(allowed func(list []string) bool) bool {
	for _, l := range v.ListLayers() {
		if !allowed(l) {
			return false
		}
	}
	return true
}

// NewValue wraps a JSON-encoded value.
func NewValue(raw json.RawMessage) Value { return Value{raw: raw, set: len(raw) > 0} }

// IsSet reports whether any layer (or the default) provided a value.
func (v Value) IsSet() bool { return v.set }

// Raw returns the JSON encoding.
func (v Value) Raw() json.RawMessage { return v.raw }

// String returns the value as a string ("" if unset).
func (v Value) String() string {
	var s string
	if json.Unmarshal(v.raw, &s) == nil {
		return s
	}
	return string(v.raw)
}

// Int returns the value as an int64 (0 if unset or not a number).
func (v Value) Int() int64 {
	var n json.Number
	if json.Unmarshal(v.raw, &n) == nil {
		i, err := n.Int64()
		if err == nil {
			return i
		}
		f, _ := n.Float64()
		return int64(f)
	}
	i, _ := strconv.ParseInt(v.String(), 10, 64)
	return i
}

// Float returns the value as a float64.
func (v Value) Float() float64 {
	var f float64
	if json.Unmarshal(v.raw, &f) == nil {
		return f
	}
	f, _ = strconv.ParseFloat(v.String(), 64)
	return f
}

// Bool returns the value as a bool.
func (v Value) Bool() bool {
	var b bool
	_ = json.Unmarshal(v.raw, &b)
	return b
}

// Duration parses "30s"-style strings or a number of seconds.
func (v Value) Duration() time.Duration {
	if d, err := time.ParseDuration(v.String()); err == nil {
		return d
	}
	return time.Duration(v.Float() * float64(time.Second))
}

// List returns a []string value.
func (v Value) List() []string {
	var l []string
	_ = json.Unmarshal(v.raw, &l)
	return l
}

// Settings resolves setting values.
type Settings interface {
	Get(key string) Value
}
