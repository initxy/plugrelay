package openairesponses

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/tidwall/gjson"
)

// object is a JSON object that keeps insertion order, so encoded items read
// like the wire format and verbatim values are never re-marshaled.
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

func (o *object) has(k string) bool { _, ok := o.vals[k]; return ok }

func (o *object) str(k, s string) { o.set(k, jstr(s)) }

func (o *object) num(k string, n int64) { o.set(k, json.RawMessage(strconv.FormatInt(n, 10))) }

func (o *object) boolean(k string, b bool) { o.set(k, json.RawMessage(strconv.FormatBool(b))) }

func (o *object) any(k string, v any) { o.set(k, jany(v)) }

// merge adds the fields of a JSON object that o does not set yet.
func (o *object) merge(rest json.RawMessage) {
	if len(rest) == 0 {
		return
	}
	gjson.ParseBytes(rest).ForEach(func(k, v gjson.Result) bool {
		if !o.has(k.Str) {
			o.set(k.Str, json.RawMessage(v.Raw))
		}
		return true
	})
}

func (o *object) bytes() []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(jstr(k))
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

func (o *object) raw() json.RawMessage { return o.bytes() }

// jany marshals without HTML escaping so text survives byte for byte.
func jany(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return json.RawMessage("null")
	}
	return bytes.TrimRight(b.Bytes(), "\n")
}

func jstr(s string) json.RawMessage { return jany(s) }

func jarray(items []json.RawMessage) json.RawMessage {
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

// restObject returns raw without the known keys, values verbatim and in
// source order; nil when nothing is left.
func restObject(raw string, known ...string) json.RawMessage {
	r := gjson.Parse(raw)
	if !r.IsObject() {
		return nil
	}
	o := newObject()
	r.ForEach(func(k, v gjson.Result) bool {
		for _, n := range known {
			if k.Str == n {
				return true
			}
		}
		o.set(k.Str, json.RawMessage(v.Raw))
		return true
	})
	if len(o.keys) == 0 {
		return nil
	}
	return o.raw()
}

// Extra helpers scoped to this protocol.

func xget(e *ir.Extra, k string) (json.RawMessage, bool) { return e.Get(Name, k) }

func xset(e *ir.Extra, k string, v json.RawMessage) {
	if v != nil {
		e.Set(Name, k, v)
	}
}

func xflag(e *ir.Extra, k string) bool {
	v, ok := xget(e, k)
	return ok && string(v) == "true"
}

func xstring(e *ir.Extra, k string) string {
	v, ok := xget(e, k)
	if !ok {
		return ""
	}
	return gjson.ParseBytes(v).String()
}

var jtrue = json.RawMessage("true")

// jsonField returns a string field of a raw JSON object.
func jsonField(raw json.RawMessage, k string) string { return gjson.GetBytes(raw, k).Str }

func hasField(raw json.RawMessage, k string) bool { return gjson.GetBytes(raw, k).Exists() }
