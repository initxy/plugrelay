package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// Decode parses one manifest document without validating it. Unknown
// fields are errors.
func Decode(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty manifest")
		}
		return nil, err
	}
	return &m, nil
}

// Parse decodes, fills defaults and validates a manifest. A pricing file
// reference is left unresolved; use Load to resolve it.
func Parse(data []byte) (*Manifest, error) {
	m, err := Decode(data)
	if err != nil {
		return nil, err
	}
	m.Normalize()
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// Load reads the manifest at name from fsys, resolves a pricing file
// relative to it, fills defaults and validates.
func Load(fsys fs.FS, name string) (*Manifest, error) {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	m, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if m.Pricing != nil && m.Pricing.File != "" {
		pf := path.Join(path.Dir(name), m.Pricing.File)
		pdata, err := fs.ReadFile(fsys, pf)
		if err != nil {
			return nil, fmt.Errorf("%s: pricing: %w", name, err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(pdata))
		dec.KnownFields(true)
		var p Pricing
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("%s: %w", pf, err)
		}
		p.File = m.Pricing.File
		m.Pricing = &p
	}
	m.Normalize()
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return m, nil
}

// Normalize fills defaults: endpoint ids, issuers and auth schemes.
func (m *Manifest) Normalize() {
	for i := range m.Endpoints {
		e := &m.Endpoints[i]
		if e.ID == "" {
			e.ID = e.Protocol
		}
		// A user-supplied base URL says nothing about who runs the service,
		// so such endpoints get a per-upstream issuer from the provider.
		if e.Issuer == "" && !strings.Contains(e.BaseURL, "{") {
			e.Issuer = m.ID
		}
		if e.Auth != nil {
			normAuth(e.Auth)
		}
	}
	normAuth(&m.Auth)
}

func normAuth(a *Auth) {
	if a.Scheme == "" && a.Header != "" {
		if http1Canon(a.Header) == "Authorization" {
			a.Scheme = SchemeBearer
		} else {
			a.Scheme = SchemeRaw
		}
	}
}

// strictDecode decodes a node rejecting unknown fields; yaml.Node.Decode
// does not honour the outer decoder's KnownFields.
func strictDecode(n *yaml.Node, v any) error {
	raw, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	return nil
}
