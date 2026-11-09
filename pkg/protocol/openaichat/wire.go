package openaichat

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
)

// Round-trip markers kept in ir.Extra. They never reach the wire.
const (
	xRole      = "_role"       // "system"/"developer" of a system block or relocated message
	xCont      = "_cont"       // system block continues the previous system message
	xArray     = "_array"      // content was a part array, not a string
	xMsg       = "_msg"        // unknown message-level fields of a system or tool message
	xMsgCache  = "_msg_cache"  // cache_control sat on the message, not on the part
	xField     = "_field"      // wire field that carried reasoning text ("reasoning" or "none")
	xRefusal   = "_refusal"    // text block came from the refusal field
	xPart      = "_part"       // verbatim content part or tool definition the IR cannot model
	xCall      = "_call"       // raw block is a tool call, not a content part
	xArgsRaw   = "_args_raw"   // tool arguments were not valid JSON; Input holds them as a JSON string
	xMaxTokens = "_max_tokens" // client used max_tokens rather than max_completion_tokens
	xStopStr   = "_stop_str"   // stop was a single string
	xMessage   = "_message"    // response: unknown fields of choices[0].message
	xChoice    = "_choice"     // response: unknown fields of choices[0]
	xFinish    = "_finish"     // response: finish_reason with no neutral equivalent
	xCreated   = "_created"    // response: created timestamp
)

var jsonTrue = json.RawMessage("true")

// field is one member of an ordered JSON object.
type field struct {
	k string
	v any
}

// object is a JSON object that keeps member order.
type object []field

func (o object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := marshal(f.k)
		b.Write(k)
		b.WriteByte(':')
		v, err := marshal(f.v)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (o *object) add(k string, v any) { *o = append(*o, field{k, v}) }

// set replaces k if present, else appends it.
func (o *object) set(k string, v any) {
	for i := range *o {
		if (*o)[i].k == k {
			(*o)[i].v = v
			return
		}
	}
	o.add(k, v)
}

func (o object) has(k string) bool {
	for _, f := range o {
		if f.k == k {
			return true
		}
	}
	return false
}

// marshal is json.Marshal without HTML escaping, so prompts with <, > and &
// keep their bytes.
func marshal(v any) ([]byte, error) {
	if r, ok := v.(json.RawMessage); ok {
		if len(r) == 0 {
			return []byte("null"), nil
		}
		return r, nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func mustRaw(v any) json.RawMessage {
	b, _ := marshal(v)
	return b
}

func isNull(r json.RawMessage) bool {
	r = bytes.TrimSpace(r)
	return len(r) == 0 || string(r) == "null"
}

// rawMap parses a JSON object into its members; anything else yields nil.
func rawMap(r json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(r, &m) != nil {
		return nil
	}
	return m
}

func str(r json.RawMessage) string {
	var s string
	_ = json.Unmarshal(r, &s)
	return s
}

// take removes and returns m[k] when it is present and not null. An explicit
// null stays in m so it is kept as an unknown field.
func take(m map[string]json.RawMessage, k string) (json.RawMessage, bool) {
	v, ok := m[k]
	if !ok || isNull(v) {
		return nil, false
	}
	delete(m, k)
	return v, true
}

// extraFrom records every remaining member of m in an Extra.
func extraFrom(m map[string]json.RawMessage) ir.Extra {
	var e ir.Extra
	for k, v := range m {
		e.Set(Name, k, v)
	}
	return e
}

func mark(e *ir.Extra, k string, v json.RawMessage) { e.Set(Name, k, v) }

func has(e *ir.Extra, k string) bool {
	_, ok := e.Get(Name, k)
	return ok
}

func getStr(e *ir.Extra, k string) string {
	v, _ := e.Get(Name, k)
	return str(v)
}

// restore appends the wire fields of an Extra produced by this codec, in
// sorted order, skipping markers, members already set and names in skip.
func restore(o *object, e ir.Extra, skip ...string) {
	if e.Origin != Name {
		return
	}
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
outer:
	for _, k := range keys {
		if strings.HasPrefix(k, "_") || o.has(k) {
			continue
		}
		for _, s := range skip {
			if k == s {
				continue outer
			}
		}
		o.add(k, e.Fields[k])
	}
}

// restoreRaw merges the JSON object held under marker k into o.
func restoreRaw(o *object, e ir.Extra, k string) {
	if v, ok := e.Get(Name, k); ok {
		mergeRaw(o, v)
	}
}

// mergeRaw adds the members of a JSON object to o, in sorted order, without
// replacing members o already has.
func mergeRaw(o *object, v json.RawMessage) {
	m := rawMap(v)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !o.has(k) {
			o.add(k, m[k])
		}
	}
}

// cacheMark parses a cache_control object.
func cacheMark(r json.RawMessage) *ir.CacheMark {
	m := rawMap(r)
	if m == nil {
		return nil
	}
	return &ir.CacheMark{TTL: str(m["ttl"])}
}

func cacheControl(c *ir.CacheMark) object {
	o := object{{"type", "ephemeral"}}
	if c.TTL != "" {
		o.add("ttl", c.TTL)
	}
	return o
}
