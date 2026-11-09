package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// jsonObject is a JSON object that keeps its members' order, so editing a
// client's config file changes only the keys plugrelay owns.
type jsonObject []jsonMember

type jsonMember struct {
	Key   string
	Value json.RawMessage
}

// parseObject decodes a JSON object; empty input is an empty object.
func parseObject(data []byte) (jsonObject, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return jsonObject{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	obj := jsonObject{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		obj.set(key, raw)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON object")
	}
	return obj, nil
}

func (o jsonObject) get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// set replaces key in place or appends it.
func (o *jsonObject) set(key string, v json.RawMessage) {
	for i := range *o {
		if (*o)[i].Key == key {
			(*o)[i].Value = v
			return
		}
	}
	*o = append(*o, jsonMember{Key: key, Value: v})
}

// setValue marshals v and sets it.
func (o *jsonObject) setValue(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	o.set(key, raw)
	return nil
}

// child returns the object at key, or an empty one when absent.
func (o jsonObject) child(key string) (jsonObject, error) {
	raw, ok := o.get(key)
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return jsonObject{}, nil
	}
	c, err := parseObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", key, err)
	}
	return c, nil
}

func (o jsonObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.Key)
		b.Write(k)
		b.WriteByte(':')
		if err := json.Compact(&b, m.Value); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// encode writes the object indented with two spaces and a final newline.
func (o jsonObject) encode() ([]byte, error) {
	compact, err := o.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := json.Indent(&b, compact, "", "  "); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
}
