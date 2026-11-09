package anthropic

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/tidwall/gjson"
)

// name is the codec id and the Origin of every Extra this codec writes.
const name = "anthropic"

// Extra keys starting with restPrefix are codec-private: they hold the
// sub-fields of a partly modelled object (thinking, cache_control, an image
// source, ...) that the IR has no slot for, as a JSON object merged back over
// the encoded object. They are never wire fields themselves.
const restPrefix = "$"

// Codec-private markers recording wire shapes the IR normalises away.
const (
	markShape    = "$shape"    // "string": content/system was a bare string
	markNoThink  = "$nothink"  // Reasoning came only from output_config
	shapeString  = `"string"`  // value of markShape
	shapeEmptyAr = `"empty"`   // value of markShape: an explicit empty array
	trueJSON     = `true`      // value of boolean markers
	nullJSON     = `null`      // JSON null
	emptyObject  = `{}`        // JSON empty object
	ephemeral    = "ephemeral" // the only cache_control type
)

// marshal encodes v without HTML escaping, so text such as "<system-reminder>"
// keeps its source bytes.
func marshal(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return json.RawMessage(nullJSON)
	}
	return json.RawMessage(bytes.TrimRight(b.Bytes(), "\n"))
}

// object builds a JSON object with keys in insertion order.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func newObject() *object { return &object{vals: map[string]json.RawMessage{}} }

func (o *object) set(k string, v json.RawMessage) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) setv(k string, v any) { o.set(k, marshal(v)) }

func (o *object) has(k string) bool { _, ok := o.vals[k]; return ok }

// extra appends the verbatim fields of e this codec produced, skipping keys
// already written and codec-private markers.
func (o *object) extra(e ir.Extra) {
	if e.Origin != name {
		return
	}
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		if !strings.HasPrefix(k, restPrefix) && !o.has(k) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i] == "type" || keys[j] == "type" {
			return keys[i] == "type"
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		o.set(k, e.Fields[k])
	}
}

// rest overlays the leftover sub-fields stored under key in e.
func (o *object) rest(e ir.Extra, key string) {
	raw, ok := e.Get(name, key)
	if !ok {
		return
	}
	gjson.ParseBytes(raw).ForEach(func(k, v gjson.Result) bool {
		o.set(k.String(), json.RawMessage(v.Raw))
		return true
	})
}

func (o *object) bytes() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(marshal(k))
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// array joins raw JSON values.
func array(items []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, it := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(it)
	}
	b.WriteByte(']')
	return b.Bytes()
}

// leftovers collects the members of obj not named in known into a JSON
// object, or returns nil when there are none.
func leftovers(obj gjson.Result, known ...string) json.RawMessage {
	o := newObject()
	obj.ForEach(func(k, v gjson.Result) bool {
		for _, n := range known {
			if k.String() == n {
				return true
			}
		}
		o.set(k.String(), json.RawMessage(v.Raw))
		return true
	})
	if len(o.keys) == 0 {
		return nil
	}
	return o.bytes()
}

// keepRest stores the leftovers of obj under "$"+field.
func keepRest(e *ir.Extra, field string, obj gjson.Result, known ...string) {
	if rest := leftovers(obj, known...); rest != nil {
		e.Set(name, restPrefix+field, rest)
	}
}

// unquote returns the string held by a JSON string literal.
func unquote(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func raw(r gjson.Result) json.RawMessage { return json.RawMessage(r.Raw) }
