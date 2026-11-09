package extplugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// Options configures a Manager.
type Options struct {
	// HostVersion is sent in initialize.
	HostVersion string
	// Host returns the Host for a plugin, keyed by the core-facing id of
	// the manifest's first kind. It backs host.log, host.kv.* and
	// host.http and supplies global-layer settings for initialize. Nil
	// leaves those host APIs unavailable.
	Host func(pluginID string) plugin.Host
	// Logger receives lifecycle events and plugin stderr.
	Logger *slog.Logger
	// BackoffMin and BackoffMax bound the restart delay (defaults 500ms, 30s).
	BackoffMin, BackoffMax time.Duration
	// SocketDir holds unix sockets (default os.TempDir()).
	SocketDir string
	// OnCrash, when set, hears of every unexpected end of a plugin
	// process (by manifest id) before its restart; it must not block.
	OnCrash func(id string, err error)
}

// Status is a plugin's state as shown in the console.
type Status struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Version   string        `json:"version"`
	Kinds     []plugin.Kind `json:"kinds"`
	State     State         `json:"state"`
	PID       int           `json:"pid,omitempty"`
	Restarts  int           `json:"restarts"`
	LastError string        `json:"last_error,omitempty"`
	Since     time.Time     `json:"since"`
	Manifest  *Manifest     `json:"manifest"`
}

// Manager discovers, runs and supervises external plugins and exposes
// each as ordinary pkg/plugin implementations.
type Manager struct {
	opts Options

	mu       sync.Mutex
	order    []string
	plugins  map[string]*instance
	adapters map[string][]plugin.Plugin
}

// NewManager returns an empty Manager.
func NewManager(opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.BackoffMin <= 0 {
		opts.BackoffMin = 500 * time.Millisecond
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = 30 * time.Second
	}
	opts.BackoffMax = max(opts.BackoffMax, opts.BackoffMin)
	return &Manager{opts: opts, plugins: map[string]*instance{}, adapters: map[string][]plugin.Plugin{}}
}

// Load adds every dir/*/plugin.yaml. Invalid manifests are skipped and
// reported in the joined error; valid ones are still added.
func (m *Manager) Load(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name(), ManifestFile)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		man, err := LoadManifest(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := m.Add(man); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Add registers a validated manifest without starting it.
func (m *Manager) Add(man *Manifest) error {
	if err := man.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.plugins[man.ID]; dup {
		return fmt.Errorf("plugin %q already loaded", man.ID)
	}
	in := newInstance(man, &m.opts)
	m.plugins[man.ID] = in
	m.order = append(m.order, man.ID)
	m.adapters[man.ID] = buildAdapters(in)
	return nil
}

func buildAdapters(in *instance) []plugin.Plugin {
	var out []plugin.Plugin
	for _, k := range in.man.Kinds {
		b := base{in: in, kind: k}
		switch k {
		case plugin.KindAuth:
			out = append(out, &Authenticator{b})
		case plugin.KindMiddleware:
			out = append(out, &Middleware{b})
		case plugin.KindSelector:
			out = append(out, &Selector{b})
		case plugin.KindMeter:
			out = append(out, &Meter{b})
		case plugin.KindSink:
			out = append(out, &Sink{b})
		case plugin.KindJob:
			out = append(out, &Job{b})
		case plugin.KindAdmin:
			out = append(out, &Admin{b})
		case plugin.KindProvider:
			out = append(out, &Provider{base: b})
		}
	}
	return out
}

// Start launches the named plugins (all loaded ones when ids is empty)
// and waits until each finished its first handshake or ctx ends. A plugin
// that fails keeps retrying in the background; its error is returned.
func (m *Manager) Start(ctx context.Context, ids ...string) error {
	ins, err := m.pick(ids)
	if err != nil {
		return err
	}
	for _, in := range ins {
		in.start()
	}
	var errs []error
	for _, in := range ins {
		if err := in.waitReady(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Stop shuts the named plugins down (all when ids is empty): shutdown
// request, exit notification, then kill after the manifest's shutdown
// timeout.
func (m *Manager) Stop(ctx context.Context, ids ...string) error {
	ins, err := m.pick(ids)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	errs := make([]error, len(ins))
	for i, in := range ins {
		wg.Go(func() { errs[i] = in.halt(ctx) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (m *Manager) pick(ids []string) ([]*instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(ids) == 0 {
		ids = m.order
	}
	out := make([]*instance, 0, len(ids))
	for _, id := range ids {
		in, ok := m.plugins[id]
		if !ok {
			return nil, fmt.Errorf("plugin %q not loaded", id)
		}
		out = append(out, in)
	}
	return out, nil
}

// List returns every loaded plugin's status in load order.
func (m *Manager) List() []Status {
	ins, _ := m.pick(nil)
	out := make([]Status, 0, len(ins))
	for _, in := range ins {
		out = append(out, in.status())
	}
	return out
}

// Status returns one plugin's status.
func (m *Manager) Status(id string) (Status, bool) {
	m.mu.Lock()
	in, ok := m.plugins[id]
	m.mu.Unlock()
	if !ok {
		return Status{}, false
	}
	return in.status(), true
}

// Plugins returns the adapters of one manifest, one per kind. Each
// satisfies the pkg/plugin interface of its kind; register them with the
// core like built-in plugins.
func (m *Manager) Plugins(id string) []plugin.Plugin {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.adapters[id])
}
