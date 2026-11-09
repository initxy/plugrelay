// Package config loads relay.yaml, the hand-written bootstrap file: where to
// listen, which database and state backend to use, the master key. Business
// data (upstreams, keys, models, which features run) lives in the store,
// never here, and plugrelay never writes this file.
//
// Every field can be overridden by a PLUGRELAY_* environment variable; see
// Env for the list.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/initxy/plugrelay/internal/secret"
)

// Config is the parsed relay.yaml.
type Config struct {
	Listen string `yaml:"listen"`
	// TrustedProxies are the reverse proxies (CIDRs or bare IPs) whose
	// X-Forwarded-For / X-Real-IP headers name the client. Empty means
	// headers are never trusted and the socket peer is the client.
	TrustedProxies []string `yaml:"trusted_proxies"`
	MasterKey      string   `yaml:"master_key"`
	MasterKeyFile  string   `yaml:"master_key_file"`
	// PreviousMasterKeys are master keys replaced by master_key (base64,
	// as master_key). They only open private state sealed before the
	// rotation; stored secrets (credentials, secret settings) still need
	// the key they were saved under.
	PreviousMasterKeys []string        `yaml:"previous_master_keys"`
	Store              Store           `yaml:"store"`
	State              State           `yaml:"state"`
	ExternalPlugins    ExternalPlugins `yaml:"external_plugins"`
	Log                Log             `yaml:"log"`
	Egress             Egress          `yaml:"egress"`
	Limits             Limits          `yaml:"limits"`
	Console            Console         `yaml:"console"`
	Shutdown           Shutdown        `yaml:"shutdown"`
	Native             Native          `yaml:"native"`
	WebSocket          WebSocket       `yaml:"websocket"`
}

// WebSocket bounds the WebSocket sessions of /v1/realtime and the Responses
// WebSocket mode.
type WebSocket struct {
	// MaxSession ends a session after this long.
	MaxSession Duration `yaml:"max_session"`
	// IdleTimeout ends a session with no message in either direction.
	IdleTimeout Duration `yaml:"idle_timeout"`
	// SessionReadyTimeout bounds the wait for a Realtime upstream's
	// session.created while client events wait for it.
	SessionReadyTimeout Duration `yaml:"session_ready_timeout"`
	// MaxMessageBytes bounds one message in either direction.
	MaxMessageBytes int64 `yaml:"max_message_bytes"`
	// ResponseReserveTokens is the output the meter holds for a response
	// that sets no cap of its own.
	ResponseReserveTokens int64 `yaml:"response_reserve_tokens"`
}

// MaxWebSocketSession is the longest websocket.max_session: the lifetime of
// a credential's and a rate limit's concurrency slot.
const MaxWebSocketSession = 30 * time.Minute

// Store selects the business database.
type Store struct {
	Driver string `yaml:"driver"` // sqlite | postgres | mysql
	DSN    string `yaml:"dsn"`
	// MaxConns caps the store's open connections; 0 = 8 (sqlite) / 20 (postgres, mysql).
	MaxConns int64 `yaml:"max_conns"`
}

// State selects the shared runtime state backend.
type State struct {
	Driver string `yaml:"driver"` // memory | redis | sql
	URL    string `yaml:"url"`
}

// ExternalPlugins controls out-of-process plugins.
type ExternalPlugins struct {
	Enabled bool   `yaml:"enabled"`
	Dir     string `yaml:"dir"`
}

// Log configures the process logger.
type Log struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // text | json
}

// Egress configures the core's outbound HTTP client.
type Egress struct {
	AllowPrivateCIDRs []string `yaml:"allow_private_cidrs"`
	DefaultTimeouts   Timeouts `yaml:"default_timeouts"`
}

// Timeouts are the three upstream timeouts.
type Timeouts struct {
	FirstByte Duration `yaml:"first_byte"`
	Idle      Duration `yaml:"idle"`
	Total     Duration `yaml:"total"`
}

// Limits bound client request bodies.
type Limits struct {
	MaxBodyBytes         int64 `yaml:"max_body_bytes"`
	MaxDecompressedBytes int64 `yaml:"max_decompressed_bytes"`
}

// Native switches /native/<upstream>/<path> passthrough on. Off, the route
// does not exist (404); on, each key still needs a grant in the
// middleware.access setting native_upstreams.
type Native struct {
	Enabled bool `yaml:"enabled"`
	// MaxBodyBytes bounds one request body (uploads); 0 means 512 MiB.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
}

// Shutdown bounds a graceful stop.
type Shutdown struct {
	// Grace is how long in-flight requests may keep running once shutdown
	// starts; their connections are closed after it.
	Grace Duration `yaml:"grace"`
	// Drain is how long the relay then waits for requests cut off by the
	// closed connections to finish their accounting (settlement, request
	// log) and for the record queue to reach the sinks; the queue always
	// gets at least 10s more. 0 means the default.
	Drain Duration `yaml:"drain"`
}

// DefaultDrain is the default Shutdown.Drain.
const DefaultDrain = 60 * time.Second

// DrainTimeout resolves Shutdown.Drain.
func (c Config) DrainTimeout() time.Duration {
	if c.Shutdown.Drain > 0 {
		return c.Shutdown.Drain.D()
	}
	return DefaultDrain
}

// Console toggles the admin web console.
type Console struct {
	// Enabled defaults to on.
	Enabled *bool `yaml:"enabled"`
}

// ConsoleEnabled resolves the console default (on).
func (c Config) ConsoleEnabled() bool {
	return c.Console.Enabled == nil || *c.Console.Enabled
}

// Duration is a time.Duration written as "30s" in YAML.
type Duration time.Duration

// UnmarshalYAML accepts "30s"-style strings or a number of seconds.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := parseDuration(n.Value)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML writes the duration as a string.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func parseDuration(s string) (time.Duration, error) {
	if v, err := time.ParseDuration(s); err == nil {
		return v, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	return 0, fmt.Errorf("invalid duration %q", s)
}

// Defaults returns the configuration used for anything relay.yaml omits.
func Defaults() Config {
	return Config{
		Listen: "127.0.0.1:8787",
		Store:  Store{Driver: "sqlite", DSN: "plugrelay.db"},
		State:  State{Driver: "memory"},
		Log:    Log{Level: "info", Format: "text"},
		Egress: Egress{DefaultTimeouts: Timeouts{
			FirstByte: Duration(2 * time.Minute),
			Idle:      Duration(5 * time.Minute),
			Total:     Duration(30 * time.Minute),
		}},
		Limits:          Limits{MaxBodyBytes: 32 << 20, MaxDecompressedBytes: 128 << 20},
		ExternalPlugins: ExternalPlugins{Dir: "plugins"},
		Shutdown:        Shutdown{Grace: Duration(30 * time.Second), Drain: Duration(DefaultDrain)},
		WebSocket: WebSocket{MaxSession: Duration(30 * time.Minute), IdleTimeout: Duration(5 * time.Minute),
			SessionReadyTimeout: Duration(10 * time.Second), MaxMessageBytes: 16 << 20, ResponseReserveTokens: 4096},
	}
}

// LookupEnv reads an environment variable; os.LookupEnv fits.
type LookupEnv func(string) (string, bool)

// Load reads relay.yaml at path (a missing file means all defaults),
// applies environment overrides and validates the result.
func Load(path string, env LookupEnv) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg, err := Parse(data, env)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes YAML over the defaults, applies environment overrides and
// validates. Unknown fields are errors so typos do not pass silently.
func Parse(data []byte, env LookupEnv) (*Config, error) {
	cfg := Defaults()
	if len(bytes.TrimSpace(data)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parse: %w", err)
		}
	}
	if env != nil {
		if err := applyEnv(&cfg, env); err != nil {
			return nil, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Env lists the environment variables Parse honours, mapped to the field
// path each one overrides.
var Env = map[string]string{
	"PLUGRELAY_LISTEN":                            "listen",
	"PLUGRELAY_TRUSTED_PROXIES":                   "trusted_proxies",
	"PLUGRELAY_MASTER_KEY":                        "master_key",
	"PLUGRELAY_MASTER_KEY_FILE":                   "master_key_file",
	"PLUGRELAY_PREVIOUS_MASTER_KEYS":              "previous_master_keys",
	"PLUGRELAY_STORE_DRIVER":                      "store.driver",
	"PLUGRELAY_STORE_DSN":                         "store.dsn",
	"PLUGRELAY_STORE_MAX_CONNS":                   "store.max_conns",
	"PLUGRELAY_STATE_DRIVER":                      "state.driver",
	"PLUGRELAY_STATE_URL":                         "state.url",
	"PLUGRELAY_EXTERNAL_PLUGINS_ENABLED":          "external_plugins.enabled",
	"PLUGRELAY_EXTERNAL_PLUGINS_DIR":              "external_plugins.dir",
	"PLUGRELAY_LOG_LEVEL":                         "log.level",
	"PLUGRELAY_LOG_FORMAT":                        "log.format",
	"PLUGRELAY_EGRESS_ALLOW_PRIVATE_CIDRS":        "egress.allow_private_cidrs",
	"PLUGRELAY_EGRESS_FIRST_BYTE_TIMEOUT":         "egress.default_timeouts.first_byte",
	"PLUGRELAY_EGRESS_IDLE_TIMEOUT":               "egress.default_timeouts.idle",
	"PLUGRELAY_EGRESS_TOTAL_TIMEOUT":              "egress.default_timeouts.total",
	"PLUGRELAY_LIMITS_MAX_BODY_BYTES":             "limits.max_body_bytes",
	"PLUGRELAY_LIMITS_MAX_DECOMPRESSED_BYTES":     "limits.max_decompressed_bytes",
	"PLUGRELAY_CONSOLE_ENABLED":                   "console.enabled",
	"PLUGRELAY_SHUTDOWN_GRACE":                    "shutdown.grace",
	"PLUGRELAY_SHUTDOWN_DRAIN":                    "shutdown.drain",
	"PLUGRELAY_NATIVE_ENABLED":                    "native.enabled",
	"PLUGRELAY_NATIVE_MAX_BODY_BYTES":             "native.max_body_bytes",
	"PLUGRELAY_WEBSOCKET_MAX_SESSION":             "websocket.max_session",
	"PLUGRELAY_WEBSOCKET_IDLE_TIMEOUT":            "websocket.idle_timeout",
	"PLUGRELAY_WEBSOCKET_SESSION_READY_TIMEOUT":   "websocket.session_ready_timeout",
	"PLUGRELAY_WEBSOCKET_MAX_MESSAGE_BYTES":       "websocket.max_message_bytes",
	"PLUGRELAY_WEBSOCKET_RESPONSE_RESERVE_TOKENS": "websocket.response_reserve_tokens",
}

func applyEnv(c *Config, env LookupEnv) error {
	var errs []error
	str := func(name string, dst *string) {
		if v, ok := env(name); ok {
			*dst = v
		}
	}
	boolean := func(name string, set func(bool)) {
		if v, ok := env(name); ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s (%s): %w", name, Env[name], err))
				return
			}
			set(b)
		}
	}
	dur := func(name string, dst *Duration) {
		if v, ok := env(name); ok {
			d, err := parseDuration(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s (%s): %w", name, Env[name], err))
				return
			}
			*dst = Duration(d)
		}
	}
	integer := func(name string, dst *int64) {
		if v, ok := env(name); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s (%s): %w", name, Env[name], err))
				return
			}
			*dst = n
		}
	}
	str("PLUGRELAY_LISTEN", &c.Listen)
	// An env master key replaces whatever relay.yaml says, inline or file.
	if v, ok := env("PLUGRELAY_MASTER_KEY"); ok {
		c.MasterKey, c.MasterKeyFile = v, ""
	}
	if v, ok := env("PLUGRELAY_MASTER_KEY_FILE"); ok {
		c.MasterKeyFile, c.MasterKey = v, ""
	}
	str("PLUGRELAY_STORE_DRIVER", &c.Store.Driver)
	str("PLUGRELAY_STORE_DSN", &c.Store.DSN)
	integer("PLUGRELAY_STORE_MAX_CONNS", &c.Store.MaxConns)
	str("PLUGRELAY_STATE_DRIVER", &c.State.Driver)
	str("PLUGRELAY_STATE_URL", &c.State.URL)
	boolean("PLUGRELAY_EXTERNAL_PLUGINS_ENABLED", func(b bool) { c.ExternalPlugins.Enabled = b })
	str("PLUGRELAY_EXTERNAL_PLUGINS_DIR", &c.ExternalPlugins.Dir)
	str("PLUGRELAY_LOG_LEVEL", &c.Log.Level)
	str("PLUGRELAY_LOG_FORMAT", &c.Log.Format)
	list := func(name string, dst *[]string) {
		if v, ok := env(name); ok {
			*dst = nil
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					*dst = append(*dst, p)
				}
			}
		}
	}
	list("PLUGRELAY_TRUSTED_PROXIES", &c.TrustedProxies)
	list("PLUGRELAY_PREVIOUS_MASTER_KEYS", &c.PreviousMasterKeys)
	list("PLUGRELAY_EGRESS_ALLOW_PRIVATE_CIDRS", &c.Egress.AllowPrivateCIDRs)
	dur("PLUGRELAY_EGRESS_FIRST_BYTE_TIMEOUT", &c.Egress.DefaultTimeouts.FirstByte)
	dur("PLUGRELAY_EGRESS_IDLE_TIMEOUT", &c.Egress.DefaultTimeouts.Idle)
	dur("PLUGRELAY_EGRESS_TOTAL_TIMEOUT", &c.Egress.DefaultTimeouts.Total)
	integer("PLUGRELAY_LIMITS_MAX_BODY_BYTES", &c.Limits.MaxBodyBytes)
	integer("PLUGRELAY_LIMITS_MAX_DECOMPRESSED_BYTES", &c.Limits.MaxDecompressedBytes)
	boolean("PLUGRELAY_CONSOLE_ENABLED", func(b bool) { c.Console.Enabled = &b })
	dur("PLUGRELAY_SHUTDOWN_GRACE", &c.Shutdown.Grace)
	dur("PLUGRELAY_SHUTDOWN_DRAIN", &c.Shutdown.Drain)
	boolean("PLUGRELAY_NATIVE_ENABLED", func(b bool) { c.Native.Enabled = b })
	integer("PLUGRELAY_NATIVE_MAX_BODY_BYTES", &c.Native.MaxBodyBytes)
	dur("PLUGRELAY_WEBSOCKET_MAX_SESSION", &c.WebSocket.MaxSession)
	dur("PLUGRELAY_WEBSOCKET_IDLE_TIMEOUT", &c.WebSocket.IdleTimeout)
	dur("PLUGRELAY_WEBSOCKET_SESSION_READY_TIMEOUT", &c.WebSocket.SessionReadyTimeout)
	integer("PLUGRELAY_WEBSOCKET_MAX_MESSAGE_BYTES", &c.WebSocket.MaxMessageBytes)
	integer("PLUGRELAY_WEBSOCKET_RESPONSE_RESERVE_TOKENS", &c.WebSocket.ResponseReserveTokens)
	if len(errs) > 0 {
		return fmt.Errorf("environment: %w", errors.Join(errs...))
	}
	return nil
}

// FieldError is a validation error at a YAML field path such as
// "trusted_proxies[2]".
type FieldError struct {
	Path string
	Msg  string
}

func (e *FieldError) Error() string { return e.Path + ": " + e.Msg }

// Validate checks the whole configuration and returns every problem found,
// joined, each as a *FieldError.
func (c *Config) Validate() error {
	var errs []error
	bad := func(path, format string, args ...any) {
		errs = append(errs, &FieldError{Path: path, Msg: fmt.Sprintf(format, args...)})
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		bad("listen", "want host:port, got %q", c.Listen)
	}
	for i, p := range c.TrustedProxies {
		if _, err := ParsePrefix(p); err != nil {
			bad(fmt.Sprintf("trusted_proxies[%d]", i), "want a CIDR or an IP, got %q", p)
		}
	}
	if c.MasterKey != "" && c.MasterKeyFile != "" {
		bad("master_key", "set master_key or master_key_file, not both")
	}
	if c.MasterKey != "" {
		if _, err := secret.ParseMasterKey(c.MasterKey); err != nil {
			bad("master_key", "%v", err)
		}
	}
	for i, k := range c.PreviousMasterKeys {
		if _, err := secret.ParseMasterKey(k); err != nil {
			bad(fmt.Sprintf("previous_master_keys[%d]", i), "%v", err)
		}
	}
	switch c.Store.Driver {
	case "sqlite", "postgres", "mysql":
	default:
		bad("store.driver", "want sqlite, postgres or mysql, got %q", c.Store.Driver)
	}
	if c.Store.DSN == "" {
		bad("store.dsn", "required")
	}
	if c.Store.MaxConns < 0 || c.Store.MaxConns > 10_000 {
		bad("store.max_conns", "want 0 (default) to 10000, got %d", c.Store.MaxConns)
	}
	switch c.State.Driver {
	case "memory", "sql":
	case "redis":
		if c.State.URL == "" {
			bad("state.url", "required for the redis driver")
		}
	default:
		bad("state.driver", "want memory, redis or sql, got %q", c.State.Driver)
	}
	if c.ExternalPlugins.Enabled && c.ExternalPlugins.Dir == "" {
		bad("external_plugins.dir", "required when external plugins are enabled")
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.Log.Level) {
		bad("log.level", "want debug, info, warn or error, got %q", c.Log.Level)
	}
	if !slices.Contains([]string{"text", "json"}, c.Log.Format) {
		bad("log.format", "want text or json, got %q", c.Log.Format)
	}
	for i, cidr := range c.Egress.AllowPrivateCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			bad(fmt.Sprintf("egress.allow_private_cidrs[%d]", i), "invalid CIDR %q", cidr)
		}
	}
	t := c.Egress.DefaultTimeouts
	for _, d := range []struct {
		path string
		v    Duration
	}{{"first_byte", t.FirstByte}, {"idle", t.Idle}, {"total", t.Total}} {
		if d.v <= 0 {
			bad("egress.default_timeouts."+d.path, "must be positive")
		}
	}
	if t.Total > 0 && (t.FirstByte > t.Total || t.Idle > t.Total) {
		bad("egress.default_timeouts.total", "must not be shorter than first_byte or idle")
	}
	if c.Shutdown.Grace <= 0 {
		bad("shutdown.grace", "must be positive")
	}
	if c.Shutdown.Drain < 0 {
		bad("shutdown.drain", "must not be negative")
	}
	if c.Limits.MaxBodyBytes <= 0 {
		bad("limits.max_body_bytes", "must be positive")
	}
	if c.Limits.MaxDecompressedBytes < c.Limits.MaxBodyBytes {
		bad("limits.max_decompressed_bytes", "must be at least max_body_bytes")
	}
	if c.Native.MaxBodyBytes < 0 {
		bad("native.max_body_bytes", "must not be negative")
	}
	ws := c.WebSocket
	for _, d := range []struct {
		path string
		v    Duration
	}{{"max_session", ws.MaxSession}, {"idle_timeout", ws.IdleTimeout}, {"session_ready_timeout", ws.SessionReadyTimeout}} {
		if d.v <= 0 {
			bad("websocket."+d.path, "must be positive")
		}
	}
	if ws.MaxSession > Duration(MaxWebSocketSession) {
		bad("websocket.max_session", "must be at most %s, the lifetime of a concurrency slot", MaxWebSocketSession)
	}
	if ws.MaxMessageBytes <= 0 {
		bad("websocket.max_message_bytes", "must be positive")
	}
	if ws.ResponseReserveTokens <= 0 {
		bad("websocket.response_reserve_tokens", "must be positive")
	}
	return errors.Join(errs...)
}

// ParsePrefix parses a CIDR, or a bare IP as a single-address prefix.
func ParsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// TrustedProxyPrefixes returns TrustedProxies parsed; Validate has already
// rejected malformed entries, which are skipped here.
func (c *Config) TrustedProxyPrefixes() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range c.TrustedProxies {
		if p, err := ParsePrefix(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// LoadMasterKey returns the master key bytes from master_key or
// master_key_file.
func (c *Config) LoadMasterKey() ([]byte, error) {
	k, err := secret.LoadMasterKey(c.MasterKey, c.MasterKeyFile)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return k, nil
}

// LoadPreviousMasterKeys returns the bytes of previous_master_keys.
func (c *Config) LoadPreviousMasterKeys() ([][]byte, error) {
	var out [][]byte
	for i, s := range c.PreviousMasterKeys {
		k, err := secret.ParseMasterKey(s)
		if err != nil {
			return nil, fmt.Errorf("config: previous_master_keys[%d]: %w", i, err)
		}
		out = append(out, k)
	}
	return out, nil
}
