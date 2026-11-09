package settings

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Resolver answers merged setting values for a caller from an immutable set
// of entries. Build a new one when the stored settings change.
type Resolver struct {
	decls   map[string]map[string]plugin.Setting
	values  map[entryKey]json.RawMessage
	box     *secret.Box
	records RecordLayer
}

// RecordLayer supplies the group or key layer of a setting whose home
// there is a field of the group or key record. owned=false means the layer
// comes from the stored entries.
type RecordLayer func(scope plugin.Scope, scopeID, pluginID, key string) (raw json.RawMessage, owned bool)

// WithRecords makes r read record-owned layers through f; stored entries
// for those layers are ignored. Call it before sharing r.
func (r *Resolver) WithRecords(f RecordLayer) *Resolver {
	r.records = f
	return r
}

func (r *Resolver) layer(scope plugin.Scope, scopeID, pluginID, key string) json.RawMessage {
	if r.records != nil {
		if raw, owned := r.records(scope, scopeID, pluginID, key); owned {
			return raw
		}
	}
	return r.values[entryKey{scope, scopeID, pluginID, key}]
}

type entryKey struct {
	scope    plugin.Scope
	scopeID  string
	pluginID string
	key      string
}

// NewResolver indexes entries for the given declarations (plugin id →
// settings). Entries of undeclared plugins or settings are ignored. box
// opens secret settings; with a nil box secret values resolve unset.
func NewResolver(decls map[string][]plugin.Setting, entries []Entry, box *secret.Box) *Resolver {
	r := &Resolver{decls: map[string]map[string]plugin.Setting{}, values: map[entryKey]json.RawMessage{}, box: box}
	for id, list := range decls {
		m := map[string]plugin.Setting{}
		for _, s := range list {
			m[s.Key] = s
		}
		r.decls[id] = m
	}
	for _, e := range entries {
		if _, ok := r.decls[e.PluginID][e.Key]; !ok {
			continue
		}
		r.values[entryKey{e.Scope, e.ScopeID, e.PluginID, e.Key}] = e.Value
	}
	return r
}

// WarnInvalid logs every stored entry whose value fails validation, with
// what the merge makes of it (see LayerValue). Every write path validates,
// so such a row comes from editing the database by hand or from a plugin
// whose declaration changed. A secret's value is never logged.
func (r *Resolver) WarnInvalid(log *slog.Logger) *Resolver {
	for k, raw := range r.values {
		s := r.decls[k.pluginID][k.key]
		if s.Scopes&k.scope == 0 || len(raw) == 0 || isNull(raw) {
			continue
		}
		err := Validate(s, raw)
		if err == nil {
			continue
		}
		effect := "ignored"
		switch got := LayerValue(s, raw); {
		case got == nil:
		case string(got) == string(denyAllLayer):
			effect = "allows nothing"
		default:
			effect = "only its valid entries count"
		}
		reason := err.Error()
		if s.Type == plugin.TypeSecret {
			reason = "not a valid secret" // the error quotes the value
		}
		log.Warn("stored setting value is invalid", "setting", k.pluginID+"/"+k.key, "layer", ScopeName(k.scope),
			"owner", k.scopeID, "effect", effect, "reason", reason)
	}
	return r
}

// Exemptions lists the settings a key is exempt from Strictest for, as
// "<plugin id>/<key>", "<plugin id>/*" or "*".
type Exemptions []string

func (x Exemptions) covers(pluginID, key string) bool {
	for _, e := range x {
		if e == "*" || e == pluginID+"/*" || e == pluginID+"/"+key {
			return true
		}
	}
	return false
}

// Get returns one merged value for caller (nil means global only).
func (r *Resolver) Get(c *plugin.Caller, exempt Exemptions, pluginID, key string) plugin.Value {
	s, ok := r.decls[pluginID][key]
	if !ok {
		return plugin.Value{}
	}
	var l Layered
	l.Global = r.values[entryKey{plugin.Global, "", pluginID, key}]
	if c != nil {
		if c.GroupID != "" {
			l.Group = r.layer(plugin.Group, c.GroupID, pluginID, key)
		}
		if c.UserID != "" {
			l.User = r.layer(plugin.User, c.UserID, pluginID, key)
		}
		if c.KeyID != "" {
			l.Key = r.layer(plugin.Key, c.KeyID, pluginID, key)
		}
	}
	v := Merge(s, l, exempt.covers(pluginID, key))
	if s.Type == plugin.TypeSecret && v.IsSet() {
		return r.open(pluginID, key, v)
	}
	return v
}

func (r *Resolver) open(pluginID, key string, v plugin.Value) plugin.Value {
	sealed := v.String()
	if !secret.IsSealed(sealed) {
		// A declared default is not sealed.
		return v
	}
	if r.box == nil {
		return plugin.Value{}
	}
	pt, err := r.box.Open(sealed, SecretAD(pluginID, key))
	if err != nil {
		return plugin.Value{}
	}
	raw, _ := json.Marshal(string(pt))
	return plugin.NewValue(raw)
}

// For returns the lookup function plugin.NewCall expects, bound to caller.
func (r *Resolver) For(c *plugin.Caller, exempt Exemptions) func(pluginID, key string) plugin.Value {
	return func(pluginID, key string) plugin.Value { return r.Get(c, exempt, pluginID, key) }
}

// Global returns the global-layer settings of one plugin, for Host.Settings.
func (r *Resolver) Global(pluginID string) plugin.Settings {
	return globalSettings{r: r, id: pluginID}
}

type globalSettings struct {
	r  *Resolver
	id string
}

func (g globalSettings) Get(key string) plugin.Value { return g.r.Get(nil, nil, g.id, key) }

// SecretAD is the associated data secret settings are sealed with.
func SecretAD(pluginID, key string) string { return "setting:" + pluginID + "/" + key }

// SealSecret prepares a secret setting value for storage: a JSON string is
// sealed under box and returned as a JSON string again. Values already
// sealed and JSON null pass through.
func SealSecret(box *secret.Box, pluginID, key string, raw json.RawMessage) (json.RawMessage, error) {
	if isNull(raw) {
		return raw, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("setting %s: secret must be a string", key)
	}
	if secret.IsSealed(s) {
		return raw, nil
	}
	if box == nil {
		return nil, fmt.Errorf("setting %s: no master key to seal secret", key)
	}
	sealed, err := box.Seal([]byte(s), SecretAD(pluginID, key))
	if err != nil {
		return nil, fmt.Errorf("setting %s: %w", key, err)
	}
	return json.Marshal(sealed)
}

// Redact replaces a secret value with a fixed marker for display.
func Redact(s plugin.Setting, raw json.RawMessage) json.RawMessage {
	if s.Type != plugin.TypeSecret || len(raw) == 0 || isNull(raw) {
		return raw
	}
	return json.RawMessage(`"` + strings.Repeat("*", 8) + `"`)
}
