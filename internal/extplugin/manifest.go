package extplugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/initxy/plugrelay/pkg/plugin"
)

// ManifestFile is the manifest's file name inside a plugin directory.
const ManifestFile = "plugin.yaml"

// Defaults applied by Validate when a manifest leaves a field empty.
const (
	DefaultStartupTimeout  = 10 * time.Second
	DefaultCallTimeout     = 5 * time.Second
	DefaultSendTimeout     = 10 * time.Minute
	DefaultShutdownTimeout = 5 * time.Second
	DefaultMaxConcurrency  = 16
)

// Transport is how the host and the plugin process exchange messages.
type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportUnix  Transport = "unix"
)

// Methods maps every protocol method a plugin may implement to its
// extension point. middleware.onEvent is absent on purpose: per-event hooks
// are only offered to built-in plugins.
var Methods = map[string]plugin.Kind{
	"auth.authenticate":    plugin.KindAuth,
	"middleware.onRequest": plugin.KindMiddleware,
	"middleware.onAttempt": plugin.KindMiddleware,
	"middleware.onFinish":  plugin.KindMiddleware,
	"selector.rank":        plugin.KindSelector,
	"meter.reserve":        plugin.KindMeter,
	"meter.settle":         plugin.KindMeter,
	"meter.refund":         plugin.KindMeter,
	"sink.consume":         plugin.KindSink,
	"job.run":              plugin.KindJob,
	"admin.handle":         plugin.KindAdmin,
	"provider.endpoints":   plugin.KindProvider,
	"provider.prepare":     plugin.KindProvider,
	"provider.send":        plugin.KindProvider,
	"provider.classify":    plugin.KindProvider,
	"provider.listModels":  plugin.KindProvider,
	"provider.refresh":     plugin.KindProvider,
	"provider.quota":       plugin.KindProvider,
}

// required lists methods a kind cannot work without.
var required = map[plugin.Kind][]string{
	plugin.KindAuth:     {"auth.authenticate"},
	plugin.KindSelector: {"selector.rank"},
	plugin.KindMeter:    {"meter.reserve", "meter.settle", "meter.refund"},
	plugin.KindSink:     {"sink.consume"},
	plugin.KindJob:      {"job.run"},
	plugin.KindAdmin:    {"admin.handle"},
}

// Host APIs a manifest may request under permissions.host.
const (
	PermLog  = "log"
	PermKV   = "kv"
	PermHTTP = "http"
)

// Duration is a time.Duration written as "5s" in YAML and JSON.
type Duration time.Duration

// UnmarshalYAML accepts Go duration strings.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Manifest is a parsed plugin.yaml.
type Manifest struct {
	// ID is the bare plugin name; each implemented kind is exposed to the
	// core as "<kind>.<id>".
	ID          string        `yaml:"id" json:"id"`
	Name        string        `yaml:"name" json:"name"`
	Version     string        `yaml:"version" json:"version"`
	Description string        `yaml:"description" json:"description,omitempty"`
	Kind        plugin.Kind   `yaml:"kind" json:"-"`
	Kinds       []plugin.Kind `yaml:"kinds" json:"kinds"`
	// Points lists the protocol methods implemented, e.g. "middleware.onRequest".
	Points []string `yaml:"points" json:"points"`

	Command   string            `yaml:"command" json:"command"`
	Args      []string          `yaml:"args" json:"args,omitempty"`
	Env       map[string]string `yaml:"env" json:"env,omitempty"`
	Transport Transport         `yaml:"transport" json:"transport"`

	Timeouts       Timeouts          `yaml:"timeouts" json:"timeouts"`
	MaxConcurrency int               `yaml:"max_concurrency" json:"max_concurrency"`
	FailPolicy     plugin.FailPolicy `yaml:"fail_policy" json:"fail_policy,omitempty"`
	// RequestBody includes the decoded request in middleware calls.
	RequestBody bool `yaml:"request_body" json:"request_body,omitempty"`
	// ReportsOffBox declares that the plugin sends data off the box
	// (plugin.Info.ReportsOffBox); test mode never runs it.
	ReportsOffBox bool `yaml:"reports_off_box" json:"reports_off_box,omitempty"`
	// Requires lists plugin ids this plugin cannot work without
	// (plugin.Info.Requires).
	Requires []string `yaml:"requires" json:"requires,omitempty"`

	Settings    []SettingSpec `yaml:"settings" json:"settings,omitempty"`
	Permissions Permissions   `yaml:"permissions" json:"permissions"`

	Provider *ProviderSpec `yaml:"provider" json:"provider,omitempty"`
	Job      *JobSpec      `yaml:"job" json:"job,omitempty"`
	Admin    *AdminSpec    `yaml:"admin" json:"admin,omitempty"`

	// Dir is the directory holding plugin.yaml; commands resolve against it.
	Dir string `yaml:"-" json:"dir"`
}

// Timeouts bound the plugin's lifecycle and calls.
type Timeouts struct {
	Startup  Duration `yaml:"startup" json:"startup"`
	Call     Duration `yaml:"call" json:"call"`
	Send     Duration `yaml:"send" json:"send"`
	Shutdown Duration `yaml:"shutdown" json:"shutdown"`
	// Methods overrides Call for individual methods.
	Methods map[string]Duration `yaml:"methods" json:"methods,omitempty"`
}

// SettingSpec is the YAML form of plugin.Setting.
type SettingSpec struct {
	Key         string             `yaml:"key" json:"key"`
	Type        plugin.SettingType `yaml:"type" json:"type"`
	Default     any                `yaml:"default" json:"default,omitempty"`
	Scopes      []string           `yaml:"scopes" json:"scopes"`
	Merge       plugin.Merge       `yaml:"merge" json:"merge"`
	Description string             `yaml:"description" json:"description,omitempty"`
	Restart     bool               `yaml:"restart" json:"restart,omitempty"`
	Enum        []string           `yaml:"enum" json:"enum,omitempty"`
}

// Permissions is what the plugin asks for; the console shows it before an
// admin enables the plugin.
type Permissions struct {
	// Host lists host APIs the plugin may call: log, kv, http.
	Host []string `yaml:"host" json:"host,omitempty"`
	// Network describes, for humans, which hosts the process itself
	// contacts. The host cannot enforce it.
	Network string `yaml:"network" json:"network,omitempty"`
	// Files describes, for humans, which paths the process touches.
	Files string `yaml:"files" json:"files,omitempty"`
}

// ProviderSpec holds provider defaults.
type ProviderSpec struct {
	// OwnedHeaders is used until provider.endpoints reports per-upstream ones.
	OwnedHeaders []string `yaml:"owned_headers" json:"owned_headers,omitempty"`
	// Issuer is the default issuer group of the plugin's endpoints.
	Issuer string `yaml:"issuer" json:"issuer,omitempty"`
}

// JobSpec schedules job.run.
type JobSpec struct {
	Every Duration `yaml:"every" json:"every"`
}

// AdminSpec lists routes mounted under /api/plugins/<id>/.
type AdminSpec struct {
	Routes []RouteSpec `yaml:"routes" json:"routes"`
}

// RouteSpec is one admin route.
type RouteSpec struct {
	Method string `yaml:"method" json:"method"`
	Path   string `yaml:"path" json:"path"`
	Role   string `yaml:"role" json:"role,omitempty"`
}

var (
	idPattern         = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	settingKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
	envKeyPattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

var settingTypes = []plugin.SettingType{
	plugin.TypeString, plugin.TypeInt, plugin.TypeFloat, plugin.TypeBool, plugin.TypeDuration,
	plugin.TypeList, plugin.TypeSecret, plugin.TypeModel, plugin.TypeUpstream, plugin.TypeGroup, plugin.TypeJSON,
}

var scopeNames = map[string]plugin.Scope{
	"global": plugin.Global, "group": plugin.Group, "user": plugin.User, "key": plugin.Key,
}

var kinds = []plugin.Kind{
	plugin.KindProvider, plugin.KindAuth, plugin.KindMiddleware, plugin.KindSelector,
	plugin.KindMeter, plugin.KindSink, plugin.KindJob, plugin.KindAdmin,
}

// LoadManifest reads and validates dir/plugin.yaml (or the file itself
// when path names a file).
func LoadManifest(p string) (*Manifest, error) {
	file := p
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		file = filepath.Join(p, ManifestFile)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	abs, err := filepath.Abs(filepath.Dir(file))
	if err != nil {
		return nil, err
	}
	m.Dir = abs
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return m, nil
}

// ParseManifest decodes YAML strictly (unknown keys are errors). It does
// not validate; call Validate after setting Dir.
func ParseManifest(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks the manifest and fills defaults. All problems are
// reported together.
func (m *Manifest) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !idPattern.MatchString(m.ID) {
		bad("id %q must match %s", m.ID, idPattern)
	}
	if m.Name == "" {
		m.Name = m.ID
	}
	if m.Version == "" {
		bad("version is required")
	}
	if m.Kind != "" {
		if len(m.Kinds) > 0 {
			bad("set kind or kinds, not both")
		}
		m.Kinds = []plugin.Kind{m.Kind}
		m.Kind = ""
	}
	if len(m.Kinds) == 0 {
		bad("kinds is required")
	}
	for _, k := range m.Kinds {
		if !slices.Contains(kinds, k) {
			bad("unknown kind %q", k)
		}
	}
	if hasDup(m.Kinds) {
		bad("duplicate kind")
	}

	if len(m.Points) == 0 {
		bad("points is required")
	}
	for _, p := range m.Points {
		if p == "middleware.onEvent" {
			bad("middleware.onEvent is not offered to external plugins; per-event hooks must be built in")
			continue
		}
		k, ok := Methods[p]
		if !ok {
			bad("unknown point %q", p)
			continue
		}
		if !slices.Contains(m.Kinds, k) {
			bad("point %q needs kind %q", p, k)
		}
	}
	if hasDup(m.Points) {
		bad("duplicate point")
	}
	for _, k := range m.Kinds {
		n := 0
		for _, p := range m.Points {
			if Methods[p] == k {
				n++
			}
		}
		if n == 0 {
			bad("kind %q has no points", k)
		}
		for _, r := range required[k] {
			if !slices.Contains(m.Points, r) {
				bad("kind %q requires point %q", k, r)
			}
		}
	}
	if m.has(plugin.KindProvider) && !m.Implements("provider.prepare") && !m.Implements("provider.send") {
		bad("a provider implements provider.prepare, provider.send or both")
	}

	if m.Command == "" {
		bad("command is required")
	}
	for k := range m.Env {
		if !envKeyPattern.MatchString(k) || strings.HasPrefix(k, "PLUGRELAY_") {
			bad("env %q is not allowed", k)
		}
	}
	switch m.Transport {
	case "":
		m.Transport = TransportStdio
	case TransportStdio, TransportUnix:
	default:
		bad("transport %q must be stdio or unix", m.Transport)
	}

	t := &m.Timeouts
	setDefault(&t.Startup, DefaultStartupTimeout)
	setDefault(&t.Call, DefaultCallTimeout)
	setDefault(&t.Send, DefaultSendTimeout)
	setDefault(&t.Shutdown, DefaultShutdownTimeout)
	for meth, d := range t.Methods {
		if !m.Implements(meth) {
			bad("timeouts.methods: %q is not an implemented point", meth)
		}
		if d <= 0 {
			bad("timeouts.methods: %q must be positive", meth)
		}
	}
	if t.Startup < 0 || t.Call < 0 || t.Send < 0 || t.Shutdown < 0 {
		bad("timeouts must be positive")
	}
	if m.MaxConcurrency == 0 {
		m.MaxConcurrency = DefaultMaxConcurrency
	}
	if m.MaxConcurrency < 0 {
		bad("max_concurrency must be positive")
	}
	switch m.FailPolicy {
	case "", plugin.FailOpen, plugin.FailClosed:
	default:
		bad("fail_policy %q must be open or closed", m.FailPolicy)
	}

	seen := map[string]bool{}
	for i := range m.Settings {
		s := &m.Settings[i]
		if !settingKeyPattern.MatchString(s.Key) {
			bad("settings[%d]: key %q must match %s", i, s.Key, settingKeyPattern)
		}
		if seen[s.Key] {
			bad("settings: duplicate key %q", s.Key)
		}
		seen[s.Key] = true
		if s.Type == "" {
			s.Type = plugin.TypeString
		}
		if !slices.Contains(settingTypes, s.Type) {
			bad("settings %q: unknown type %q", s.Key, s.Type)
		}
		if len(s.Scopes) == 0 {
			s.Scopes = []string{"global"}
		}
		for _, sc := range s.Scopes {
			if _, ok := scopeNames[sc]; !ok {
				bad("settings %q: unknown scope %q", s.Key, sc)
			}
		}
		switch s.Merge {
		case "":
			s.Merge = plugin.Override
		case plugin.Override, plugin.Strictest:
		case plugin.Union:
			if s.Type != plugin.TypeList {
				bad("settings %q: merge union needs type list", s.Key)
			}
		default:
			bad("settings %q: merge %q must be override, strictest or union", s.Key, s.Merge)
		}
		if len(s.Enum) > 0 && s.Type != plugin.TypeString {
			bad("settings %q: enum needs type string", s.Key)
		}
		if s.Type == plugin.TypeSecret && s.Default != nil {
			bad("settings %q: a secret cannot have a default", s.Key)
		}
		if _, err := json.Marshal(s.Default); err != nil {
			bad("settings %q: default is not JSON-encodable: %v", s.Key, err)
		}
	}

	for _, p := range m.Permissions.Host {
		if p != PermLog && p != PermKV && p != PermHTTP {
			bad("permissions.host: unknown host API %q", p)
		}
	}

	if m.Provider != nil && !m.has(plugin.KindProvider) {
		bad("provider section needs kind provider")
	}
	if m.has(plugin.KindJob) && (m.Job == nil || m.Job.Every <= 0) {
		bad("kind job needs a positive job.every")
	}
	if m.Job != nil && !m.has(plugin.KindJob) {
		bad("job section needs kind job")
	}
	if m.has(plugin.KindAdmin) && (m.Admin == nil || len(m.Admin.Routes) == 0) {
		bad("kind admin needs admin.routes")
	}
	if m.Admin != nil {
		if !m.has(plugin.KindAdmin) {
			bad("admin section needs kind admin")
		}
		for i, r := range m.Admin.Routes {
			if r.Method == "" {
				m.Admin.Routes[i].Method = "GET"
			}
			if !strings.HasPrefix(r.Path, "/") || strings.Contains(r.Path, "..") {
				bad("admin.routes[%d]: path %q must be absolute and clean", i, r.Path)
			}
			switch r.Role {
			case "", "admin", "member":
			default:
				bad("admin.routes[%d]: role %q must be admin, member or empty", i, r.Role)
			}
		}
	}
	return errors.Join(errs...)
}

// Implements reports whether the manifest lists method in points.
func (m *Manifest) Implements(method string) bool { return slices.Contains(m.Points, method) }

// PluginID is the core-facing id for one of the manifest's kinds.
func (m *Manifest) PluginID(k plugin.Kind) string { return string(k) + "." + m.ID }

// PluginSettings converts the declared settings to plugin.Setting.
func (m *Manifest) PluginSettings() []plugin.Setting {
	out := make([]plugin.Setting, 0, len(m.Settings))
	for _, s := range m.Settings {
		var sc plugin.Scope
		for _, n := range s.Scopes {
			sc |= scopeNames[n]
		}
		out = append(out, plugin.Setting{
			Key: s.Key, Type: s.Type, Default: s.Default, Scopes: sc, Merge: s.Merge,
			Help: plugin.Text{EN: s.Description}, Restart: s.Restart, Enum: s.Enum,
		})
	}
	return out
}

// CallTimeout is the per-call timeout for method.
func (m *Manifest) CallTimeout(method string) time.Duration {
	if d, ok := m.Timeouts.Methods[method]; ok {
		return d.D()
	}
	if method == "provider.send" {
		return m.Timeouts.Send.D()
	}
	return m.Timeouts.Call.D()
}

// FailPolicyFor is the effective policy for a kind.
func (m *Manifest) FailPolicyFor(k plugin.Kind) plugin.FailPolicy {
	if m.FailPolicy != "" {
		return m.FailPolicy
	}
	switch k {
	case plugin.KindSink, plugin.KindJob:
		return plugin.FailOpen
	}
	return plugin.FailClosed
}

func (m *Manifest) has(k plugin.Kind) bool { return slices.Contains(m.Kinds, k) }

func (m *Manifest) allowed(perm string) bool {
	return perm == PermLog || slices.Contains(m.Permissions.Host, perm)
}

func setDefault(d *Duration, v time.Duration) {
	if *d == 0 {
		*d = Duration(v)
	}
}

func hasDup[T comparable](s []T) bool {
	seen := make(map[T]bool, len(s))
	for _, v := range s {
		if seen[v] {
			return true
		}
		seen[v] = true
	}
	return false
}
