package compatible

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/initxy/plugrelay/manifests"
	"github.com/initxy/plugrelay/plugins/provider/manifest"
)

// Index is a concurrency-safe set of manifests keyed by id. The core keeps
// one, starting from the official manifests, and puts console-defined
// manifests into it; changes take effect on the next request.
type Index struct {
	mu sync.RWMutex
	m  map[string]*manifest.Manifest
}

// NewIndex returns an index holding ms. Later duplicates replace earlier ones.
func NewIndex(ms ...*manifest.Manifest) *Index {
	x := &Index{m: make(map[string]*manifest.Manifest, len(ms))}
	for _, m := range ms {
		x.m[m.ID] = m
	}
	return x
}

// Get returns the manifest with id.
func (x *Index) Get(id string) (*manifest.Manifest, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	m, ok := x.m[id]
	return m, ok
}

// IDs returns every manifest id, sorted.
func (x *Index) IDs() []string {
	x.mu.RLock()
	defer x.mu.RUnlock()
	ids := make([]string, 0, len(x.m))
	for id := range x.m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Put validates m and adds or replaces it.
func (x *Index) Put(m *manifest.Manifest) error {
	m.Normalize()
	if err := m.Validate(); err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.m[m.ID] = m
	return nil
}

// Delete removes a manifest.
func (x *Index) Delete(id string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.m, id)
}

// PricingFileSuffix marks YAML files that are pricing tables referenced by
// a manifest, not manifests.
const PricingFileSuffix = ".prices.yaml"

// LoadDir loads and validates every *.yaml / *.yml manifest under fsys
// (pricing files excluded). All failures are reported, each prefixed with
// its file; duplicate ids are an error.
func LoadDir(fsys fs.FS) (*Index, error) {
	x := NewIndex()
	from := map[string]string{}
	var errs []error
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		ext := path.Ext(name)
		if d.IsDir() || (ext != ".yaml" && ext != ".yml") || strings.HasSuffix(name, PricingFileSuffix) {
			return nil
		}
		m, err := manifest.Load(fsys, name)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		if prev, dup := from[m.ID]; dup {
			errs = append(errs, fmt.Errorf("%s: id %q already defined in %s", name, m.ID, prev))
			return nil
		}
		from[m.ID] = name
		x.m[m.ID] = m
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return x, nil
}

var (
	defaultOnce  sync.Once
	defaultIndex *Index
)

// Default returns the process-wide index, initialised with the official
// manifests embedded in package manifests. The embedded set is validated
// by tests, so a failure here is a build defect and panics.
func Default() *Index {
	defaultOnce.Do(func() {
		x, err := LoadDir(manifests.FS)
		if err != nil {
			panic("compatible: official manifests: " + err.Error())
		}
		defaultIndex = x
	})
	return defaultIndex
}
