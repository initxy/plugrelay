package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/settings"
	"github.com/initxy/plugrelay/pkg/ir"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// Role is a console user's role.
type Role string

const (
	RoleAdmin  Role = "admin"  // manages everything
	RoleMember Role = "member" // team member: own keys, shared upstreams
	RoleUser   Role = "user"   // end user of a selling site: own keys and usage
)

// User is a console account.
type User struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	Role     Role   `json:"role"`
	GroupID  string `json:"group_id,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	// EmailUnverified marks an address nobody vouched for (self-service
	// sign-up, import): SSO link_by_email never links to such an account.
	EmailUnverified bool      `json:"email_unverified,omitempty"`
	CreatedAt       time.Time `json:"created_at,omitzero"`
	UpdatedAt       time.Time `json:"updated_at,omitzero"`

	passwordHash string
}

// Group is a plan or team template most settings hang on.
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Settings are core per-group settings (model allowlist, data
	// retention…); plugin settings live in SettingRow.
	Settings        map[string]json.RawMessage `json:"settings,omitempty"`
	PriceMultiplier float64                    `json:"price_multiplier"`
	CreatedAt       time.Time                  `json:"created_at,omitzero"`
	UpdatedAt       time.Time                  `json:"updated_at,omitzero"`
}

// Key is a platform API key. Only its hash is stored.
type Key struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Hash    string `json:"-"`
	Last4   string `json:"last4"`
	UserID  string `json:"user_id,omitempty"`
	GroupID string `json:"group_id,omitempty"`
	// Settings are core per-key overrides such as "models" (allowlist) and
	// "ip_allowlist".
	Settings map[string]json.RawMessage `json:"settings,omitempty"`
	// Exceptions lists settings this key is exempt from Strictest for.
	Exceptions settings.Exemptions `json:"exceptions,omitempty"`
	Disabled   bool                `json:"disabled,omitempty"`
	ExpiresAt  time.Time           `json:"expires_at,omitzero"`
	CreatedAt  time.Time           `json:"created_at,omitzero"`
	LastUsedAt time.Time           `json:"last_used_at,omitzero"`
}

// Usable reports whether the key may authenticate at now.
func (k *Key) Usable(now time.Time) bool {
	return !k.Disabled && (k.ExpiresAt.IsZero() || now.Before(k.ExpiresAt))
}

// Caller returns the identity a request with this key carries.
func (k *Key) Caller() plugin.Caller {
	return plugin.Caller{KeyID: k.ID, KeyName: k.Name, UserID: k.UserID, GroupID: k.GroupID}
}

// KeyFilter narrows ListKeys; empty fields match everything.
type KeyFilter struct {
	UserID  string
	GroupID string
}

// Source says where an upstream or manifest came from.
type Source string

const (
	SourceOfficial Source = "official" // shipped with plugrelay
	SourceCustom   Source = "custom"   // added by an admin
)

// Upstream is a configured upstream instance.
type Upstream struct {
	ID        string            `json:"id"`
	Provider  string            `json:"provider"`
	Manifest  string            `json:"manifest,omitempty"`
	Endpoints []plugin.Endpoint `json:"endpoints,omitempty"`
	Config    map[string]any    `json:"config,omitempty"`
	Enabled   bool              `json:"enabled"`
	Source    Source            `json:"source,omitempty"`
	CreatedAt time.Time         `json:"created_at,omitzero"`
	UpdatedAt time.Time         `json:"updated_at,omitzero"`
}

// Plugin converts to the form providers receive.
func (u *Upstream) Plugin() plugin.Upstream {
	return plugin.Upstream{ID: u.ID, Provider: u.Provider, Endpoints: u.Endpoints, Config: u.Config}
}

// CredentialKind is who a credential belongs to.
type CredentialKind string

const (
	CredShared   CredentialKind = "shared"   // API key usable by everyone routed to it
	CredPersonal CredentialKind = "personal" // usable only by OwnerID's keys
	CredService  CredentialKind = "service"  // service account
)

// CredentialStatus is a credential's durable rotation state. Short
// cooldowns live in package state; these are the ones that must survive.
type CredentialStatus string

const (
	CredActive      CredentialStatus = "active"
	CredCooling     CredentialStatus = "cooling"
	CredRefreshing  CredentialStatus = "refreshing"
	CredQuarantined CredentialStatus = "quarantined"
	CredManual      CredentialStatus = "manual"
	CredDisabled    CredentialStatus = "disabled"
)

// Credential is a secret used to call an upstream.
type Credential struct {
	ID         string         `json:"id"`
	UpstreamID string         `json:"upstream_id"`
	Kind       CredentialKind `json:"kind"`
	// OwnerID is the user a personal credential belongs to (hard rule 18).
	OwnerID string `json:"owner_id,omitempty"`
	// Secret is plaintext to store on Put; reads leave it empty.
	Secret string `json:"-"`
	// Sealed is the stored ciphertext; Open decrypts it.
	Sealed         string            `json:"-"`
	Proxy          string            `json:"proxy,omitempty"`
	MaxConcurrency int               `json:"max_concurrency,omitempty"`
	Status         CredentialStatus  `json:"status"`
	StatusReason   string            `json:"status_reason,omitempty"`
	StatusUntil    time.Time         `json:"status_until,omitzero"`
	Meta           map[string]string `json:"meta,omitempty"`
	ExpiresAt      time.Time         `json:"expires_at,omitzero"`
	CreatedAt      time.Time         `json:"created_at,omitzero"`
	UpdatedAt      time.Time         `json:"updated_at,omitzero"`
}

// CredentialAD is the associated data a credential secret is sealed with,
// so ciphertexts cannot be moved between rows.
func CredentialAD(id string) string { return "credential:" + id }

// Open decrypts the credential into the form providers receive.
func (c *Credential) Open(box *secret.Box) (plugin.Credential, error) {
	pc := plugin.Credential{ID: c.ID, UpstreamID: c.UpstreamID, Kind: string(c.Kind), OwnerID: c.OwnerID,
		Proxy: c.Proxy, MaxConcurrency: c.MaxConcurrency, Meta: c.Meta}
	if c.Sealed == "" {
		return pc, nil
	}
	if box == nil {
		return pc, ErrNoBox
	}
	pt, err := box.Open(c.Sealed, CredentialAD(c.ID))
	if err != nil {
		return pc, fmt.Errorf("store: open credential %s: %w", c.ID, err)
	}
	pc.Secret = string(pt)
	return pc, nil
}

// Model is a client-facing model name.
type Model struct {
	Name    string       `json:"name"`
	Info    ir.ModelInfo `json:"info"`
	Enabled bool         `json:"enabled"`
	// ManualDisabled is set when an admin turned the model off; model sync
	// must not turn it back on (hard rule 21).
	ManualDisabled bool      `json:"manual_disabled,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitzero"`
	UpdatedAt      time.Time `json:"updated_at,omitzero"`
}

// PresetStrategy says how a preset picks among its candidate models.
type PresetStrategy string

// Preset pick strategies.
const (
	PresetInOrder  PresetStrategy = "in_order" // first candidate with a usable route
	PresetCheapest PresetStrategy = "cheapest" // lowest upstream price first
	PresetFastest  PresetStrategy = "fastest"  // lowest recent time to first byte first
)

// PresetStrategies lists the valid strategies.
var PresetStrategies = []PresetStrategy{PresetInOrder, PresetCheapest, PresetFastest}

// Preset is a client-facing name that serves one of several models. Its
// name never equals a model's.
type Preset struct {
	Name string `json:"name"`
	// Models are the candidate model names, in preference order.
	Models    []string       `json:"models"`
	Strategy  PresetStrategy `json:"strategy"`
	CreatedAt time.Time      `json:"created_at,omitzero"`
	UpdatedAt time.Time      `json:"updated_at,omitzero"`
}

// Route sends a model to an upstream. (Model, UpstreamID, UpstreamModel)
// is unique.
type Route struct {
	Model         string `json:"model"`
	UpstreamID    string `json:"upstream_id"`
	UpstreamModel string `json:"upstream_model"`
	// Priority: higher is tried first. Weight splits new sessions within a
	// priority.
	Priority int `json:"priority"`
	Weight   int `json:"weight"`
	// Groups restricts the route to keys of these groups; empty = all.
	Groups []string `json:"groups,omitempty"`
}

// PriceRule prices tokens per million in micro-units of Currency (media
// units per unit). A rule
// matches on Model, or on UpstreamID+UpstreamModel for operator cost;
// unused match fields are empty.
type PriceRule struct {
	Model         string `json:"model,omitempty"`
	UpstreamID    string `json:"upstream_id,omitempty"`
	UpstreamModel string `json:"upstream_model,omitempty"`
	Input         int64  `json:"input"`
	CacheRead     int64  `json:"cache_read"`
	CacheWrite5m  int64  `json:"cache_write_5m"`
	CacheWrite1h  int64  `json:"cache_write_1h"`
	Output        int64  `json:"output"`
	Currency      string `json:"currency,omitempty"`
	// Units prices media units in micro-units per unit, keyed by
	// plugin.Unit* ("image", "image:<size>[:<quality>]", "audio_second",
	// "1k_chars").
	Units map[string]int64 `json:"units,omitempty"`
	// BatchDiscount is the fraction taken off usage served through a
	// batch API (0.5 = half price); 0 charges batches at full price.
	BatchDiscount float64 `json:"batch_discount,omitempty"`
	// Source is PriceManual or PriceSynced; price sync only ever replaces
	// a synced rule by itself.
	Source    string    `json:"source,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// PluginConfig is the database part of a plugin's configuration; relay.yaml
// entries take precedence for plugins listed there.
type PluginConfig struct {
	PluginID string         `json:"plugin_id"`
	Enabled  bool           `json:"enabled"`
	Order    int            `json:"order"`
	Config   map[string]any `json:"config,omitempty"`
}

// FeatureState is a feature an operator switched on or off.
type FeatureState struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// SettingRow is one stored layered setting value.
type SettingRow = settings.Entry

// SettingFilter narrows ListSettings; zero fields match everything.
type SettingFilter struct {
	Scope    plugin.Scope
	ScopeID  string
	PluginID string
}

// Manifest is a declarative provider manifest.
type Manifest struct {
	ID        string    `json:"id"`
	YAML      string    `json:"yaml"`
	Source    Source    `json:"source,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// Audit is one admin action.
type Audit struct {
	ID     string          `json:"id"`
	Time   time.Time       `json:"time"`
	Actor  string          `json:"actor"`
	Action string          `json:"action"`
	Target string          `json:"target,omitempty"`
	Diff   json.RawMessage `json:"diff,omitempty"`
}

// AuditQuery pages the audit log newest first.
type AuditQuery struct {
	Before time.Time // exclusive; zero = now
	Actor  string
	Target string
	Limit  int // default 100
}

// RecordQuery filters the request log. Zero fields match everything.
type RecordQuery struct {
	From, To   time.Time // [From, To)
	KeyID      string
	UserID     string
	GroupID    string
	Model      string
	UpstreamID string
	// StatusClass is 2, 4 or 5 for 2xx/4xx/5xx; -1 matches requests that
	// never got a status (0).
	StatusClass int
	// Failed matches requests without a 2xx status.
	Failed bool
	// Retried matches requests the relay sent to more than one attempt.
	Retried bool
	// Tier is the handling tier of the last attempt.
	Tier      string
	SessionID string
	// Client is a pkg/clientid id.
	Client string
	// Cause matches requests whose final failure cause is this
	// (FailureSeries' causes, not_sent included).
	Cause string
	// RequestID matches ids starting with this text.
	RequestID string
	Cursor    string // from a previous page's Next
	Limit     int    // default 50, max 500
}

// RecordBucket is one bucket of RecordHistogram.
type RecordBucket struct {
	Time     time.Time
	Requests int64
	Failed   int64 // without a 2xx status
}

// RecordPage is one page of QueryRecords.
type RecordPage struct {
	Records []*plugin.Record `json:"records"`
	// Next continues after the last record; empty at the end.
	Next string `json:"next,omitempty"`
}

// Session is one session of SessionPage: the requests of one session id
// that the query matched.
type Session struct {
	ID          string
	First       time.Time
	Last        time.Time
	Requests    int64
	Failed      int64
	KeyID       string
	KeyName     string
	UserID      string
	Client      string
	Models      []string // distinct, most used first
	Input       int64    // all input buckets
	CacheRead   int64
	ChargeMicro int64
	CostMicro   int64
}

// SessionList is one page of SessionPage, newest last request first.
type SessionList struct {
	Sessions []Session
	Next     string
}

// SessionTotal sums one session up to and including one request.
type SessionTotal struct {
	Requests    int64 // in the session
	Turn        int64 // position of the request (1-based)
	ChargeMicro int64
	First, Last time.Time
}

// CausePoint is one bucket of FailureSeries.
type CausePoint struct {
	Time   time.Time
	Cause  string
	Failed int64
}

// UsageQuery reads usage rollups.
type UsageQuery struct {
	From, To time.Time // [From, To), aligned down to hours
	// Bucket is a multiple of an hour; default one hour.
	Bucket     time.Duration
	KeyID      string
	UserID     string
	GroupID    string
	Model      string
	UpstreamID string
	Client     string
	// By splits series: "", "key", "user", "group", "model", "upstream"
	// or "client".
	By string
}

// UsagePoint is one bucket of a usage series.
type UsagePoint struct {
	Time         time.Time `json:"time"`
	Series       string    `json:"series,omitempty"`
	Requests     int64     `json:"requests"`
	Errors       int64     `json:"errors"`
	Input        int64     `json:"input"`
	CacheRead    int64     `json:"cache_read"`
	CacheWrite5m int64     `json:"cache_write_5m"`
	CacheWrite1h int64     `json:"cache_write_1h"`
	Output       int64     `json:"output"`
	Reasoning    int64     `json:"reasoning"`
	CostMicro    int64     `json:"cost_micro"`
	ChargeMicro  int64     `json:"charge_micro"`
}

// Snapshot is a consistent read of everything routing needs. Credential
// secrets stay sealed; open them with Credential.Open while building the
// routing table.
type Snapshot struct {
	Version     int64
	Groups      []Group
	Upstreams   []Upstream
	Credentials []Credential
	Models      []Model
	Presets     []Preset
	Routes      []Route
	Prices      []PriceRule
	Plugins     []PluginConfig
	Features    []FeatureState
	Settings    []SettingRow
	Manifests   []Manifest
}
