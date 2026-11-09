package openaicompletions

import (
	"bytes"
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/initxy/plugrelay/pkg/ir"
)

// Round-trip markers kept in ir.Extra. They never reach the wire.
const (
	xPromptArr = "_prompt_array" // prompt was a one-element array
	xStopStr   = "_stop_str"     // stop was a single string
	xCreated   = "_created"      // response: created timestamp
	xChoice    = "_choice"       // response: unknown fields of choices[0], logprobs included
	xFinish    = "_finish"       // response: finish_reason with no neutral equivalent
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

// take removes and returns m[k] when it is present and not null.
func take(m map[string]json.RawMessage, k string) (json.RawMessage, bool) {
	v, ok := m[k]
	if !ok || isNull(v) {
		return nil, false
	}
	delete(m, k)
	return v, true
}

func mark(e *ir.Extra, k string, v json.RawMessage) { e.Set(Name, k, v) }

func has(e *ir.Extra, k string) bool {
	_, ok := e.Get(Name, k)
	return ok
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
	for _, k := range keys {
		if !strings.HasPrefix(k, "_") && !o.has(k) && !slices.Contains(skip, k) {
			o.add(k, e.Fields[k])
		}
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
