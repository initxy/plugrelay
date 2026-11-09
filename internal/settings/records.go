package settings

import (
	"encoding/json"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// recordSetting is a plugin setting whose group or key layer is a field of
// the group or key record. The record is its only home on that layer: no
// settings row is stored there (the admin API and `plugrelay apply` refuse
// one), and the resolver reads the record.
type recordSetting struct {
	scope         plugin.Scope
	pluginID, key string
	field         string
	micro         bool // the field is money ({"micro": n}); the setting takes n
}

var recordSettings = []recordSetting{
	{plugin.Key, "meter.quota", "key_monthly_micro", "budget_monthly", true},
	{plugin.Key, "meter.quota", "key_total_micro", "budget_total", true},
	{plugin.Key, "middleware.ratelimit", "rpm", "rpm", false},
	{plugin.Key, "middleware.ratelimit", "concurrency", "max_concurrency", false},
	{plugin.Group, "sink.requestlog", "body_logging", "log_bodies", false},
}

func findRecord(scope plugin.Scope, pluginID, key string) (recordSetting, bool) {
	for _, r := range recordSettings {
		if r.scope == scope && r.pluginID == pluginID && r.key == key {
			return r, true
		}
	}
	return recordSetting{}, false
}

// RecordOwned reports whether a setting's layer at scope is a field of the
// group or key record, and names that field.
func RecordOwned(scope plugin.Scope, pluginID, key string) (field string, owned bool) {
	r, ok := findRecord(scope, pluginID, key)
	return r.field, ok
}

// FromRecord reports whether a setting's layer at scope is a record field,
// and returns that field of fields (the record's settings) in the setting's
// form; nil when the record leaves it unset.
func FromRecord(scope plugin.Scope, pluginID, key string, fields map[string]json.RawMessage) (json.RawMessage, bool) {
	r, ok := findRecord(scope, pluginID, key)
	if !ok {
		return nil, false
	}
	raw := fields[r.field]
	if !r.micro || len(raw) == 0 {
		return raw, true
	}
	var m struct {
		Micro int64 `json:"micro"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Micro <= 0 {
		return nil, true
	}
	out, _ := json.Marshal(m.Micro)
	return out, true
}
