// Package store is plugrelay's durable business database: users, groups,
// sign-in identities and second factors, platform keys, upstreams and
// credentials, models, routes and presets, prices and price sync,
// features, plugin configuration and layered settings, declarative
// manifests, batch objects, the request log with its hourly usage
// rollups, the audit log, notifications, plugin KV, and the sell kit's
// balances, subscriptions, payment orders, coupons and referrals.
//
// One implementation runs on SQLite (pure Go, the default), PostgreSQL and
// MySQL through database/sql; package sqldialect rewrites the shared
// queries per driver and each driver has its own migrations. Times are
// stored as unix milliseconds and JSON as TEXT (SQLite), JSONB or TEXT
// (PostgreSQL) or LONGTEXT (MySQL). The schema is embedded, versioned
// migrations (migrations/<driver>/NNNN_name.sql) applied on Open.
//
// Every write to data that routing depends on bumps a version counter in
// the same transaction, so the core can poll Version cheaply and rebuild
// its read-only routing snapshot from Snapshot only when something changed.
// Request log inserts, key last-used touches, sessions and plugin KV do not
// bump it.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/state"
	"github.com/initxy/plugrelay/pkg/plugin"
)

var (
	// ErrNotFound is returned when the addressed row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is returned when a write violates a uniqueness rule.
	ErrConflict = errors.New("store: conflict")
	// ErrInvalidLogin is returned by Authenticate for an unknown email, a
	// wrong password or a disabled user alike.
	ErrInvalidLogin = errors.New("store: invalid login")
	// ErrNoBox is returned when a secret must be sealed but the store was
	// opened without a master key.
	ErrNoBox = errors.New("store: no master key configured")
)

// Config opens a store.
type Config struct {
	// Driver is "sqlite", "postgres" or "mysql".
	Driver string
	// DSN is a file path or "file:" URI for sqlite (WAL, busy timeout and
	// immediate transactions are added unless the URI sets its own query),
	// a postgres:// URL, or a go-sql-driver DSN for mysql
	// (user:pass@tcp(host:3306)/db; sqldialect.OpenMySQL forces the options
	// the queries need).
	DSN string
	// Secrets seals credential secrets on write. Without it, writes that
	// carry a secret fail with ErrNoBox.
	Secrets *secret.Box
	// MaxConns caps open database connections; 0 means 8 for sqlite and
	// 20 for postgres and mysql (several nodes share the server's
	// max_connections).
	MaxConns int
	// TouchFlushEvery is how often buffered key last-used times are
	// written; defaults to 30s.
	TouchFlushEvery time.Duration
}

// Store is the business database. All methods are safe for concurrent use.
//
// Put methods create or replace a row by its natural id; an empty ID on
// rows with generated ids is filled in. List methods return rows in a
// stable order.
type Store interface {
	// Users and console sessions.
	CreateUser(ctx context.Context, u *User, password string) error
	UpdateUser(ctx context.Context, u *User) error
	SetPassword(ctx context.Context, userID, password string) error
	GetUser(ctx context.Context, id string) (*User, error)
	ListUsers(ctx context.Context) ([]User, error)
	// DeleteUser deletes the user and ends the user's live subscriptions.
	DeleteUser(ctx context.Context, id string) error
	Authenticate(ctx context.Context, email, password string) (*User, error)
	// CreateSession returns a new opaque token; only its hash is stored.
	CreateSession(ctx context.Context, userID string, ttl time.Duration) (token string, err error)
	// SessionUser resolves a live session of an enabled user.
	SessionUser(ctx context.Context, token string) (*User, error)
	DeleteSession(ctx context.Context, token string) error
	// UserByEmail finds a user by email, ignoring case.
	UserByEmail(ctx context.Context, email string) (*User, error)

	// External sign-in identities; (provider, subject) is unique.
	LinkIdentity(ctx context.Context, i *Identity) error
	GetIdentity(ctx context.Context, provider, subject string) (*Identity, error)
	// ListIdentities lists one user's identities, or all for "".
	ListIdentities(ctx context.Context, userID string) ([]Identity, error)
	TouchIdentity(ctx context.Context, id string, at time.Time) error
	DeleteIdentity(ctx context.Context, id string) error

	PutGroup(ctx context.Context, g *Group) error
	GetGroup(ctx context.Context, id string) (*Group, error)
	ListGroups(ctx context.Context) ([]Group, error)
	DeleteGroup(ctx context.Context, id string) error

	// CreateKey generates a platform key, stores its hash and returns the
	// plaintext, which is never retrievable again.
	CreateKey(ctx context.Context, k *Key) (plaintext string, err error)
	// UpdateKey changes everything but the hash and timestamps.
	UpdateKey(ctx context.Context, k *Key) error
	GetKey(ctx context.Context, id string) (*Key, error)
	// KeyByToken looks up a key by its plaintext. Disabled or expired keys
	// are returned as found; the caller decides.
	KeyByToken(ctx context.Context, plaintext string) (*Key, error)
	ListKeys(ctx context.Context, f KeyFilter) ([]Key, error)
	DeleteKey(ctx context.Context, id string) error
	// TouchKey records a use in memory; uses are written in batches.
	TouchKey(id string, at time.Time)
	// FlushTouches writes buffered key uses now.
	FlushTouches(ctx context.Context) error

	PutUpstream(ctx context.Context, u *Upstream) error
	GetUpstream(ctx context.Context, id string) (*Upstream, error)
	ListUpstreams(ctx context.Context) ([]Upstream, error)
	// DeleteUpstream also deletes the upstream's credentials and routes.
	DeleteUpstream(ctx context.Context, id string) error

	// PutCredential stores c; a non-empty c.Secret is sealed and replaces
	// the stored secret, an empty one keeps it.
	PutCredential(ctx context.Context, c *Credential) error
	GetCredential(ctx context.Context, id string) (*Credential, error)
	// ListCredentials lists one upstream's credentials, or all for "".
	ListCredentials(ctx context.Context, upstreamID string) ([]Credential, error)
	DeleteCredential(ctx context.Context, id string) error
	SetCredentialStatus(ctx context.Context, id string, status CredentialStatus, reason string, until time.Time) error
	// SetCredentialSecret replaces a secret after a refresh.
	SetCredentialSecret(ctx context.Context, id, secret string, expiresAt time.Time) error

	PutModel(ctx context.Context, m *Model) error
	GetModel(ctx context.Context, name string) (*Model, error)
	ListModels(ctx context.Context) ([]Model, error)
	// DeleteModel also deletes the model's routes.
	DeleteModel(ctx context.Context, name string) error
	// ReplaceRoutes sets the full route list of one model.
	ReplaceRoutes(ctx context.Context, model string, routes []Route) error
	// ListRoutes lists one model's routes, or all for "".
	ListRoutes(ctx context.Context, model string) ([]Route, error)

	// Presets share the model namespace: PutModel and PutPreset refuse a
	// name the other holds (ErrConflict).
	PutPreset(ctx context.Context, p *Preset) error
	GetPreset(ctx context.Context, name string) (*Preset, error)
	ListPresets(ctx context.Context) ([]Preset, error)
	DeletePreset(ctx context.Context, name string) error

	PutPrice(ctx context.Context, p *PriceRule) error
	ListPrices(ctx context.Context) ([]PriceRule, error)
	DeletePrice(ctx context.Context, model, upstreamID, upstreamModel string) error

	// Price sync: what the public model catalog says per model.
	ListPriceSync(ctx context.Context) ([]PriceSync, error)
	// SavePriceCatalog stores the catalog's view of models (keeping the
	// operator's follow and rejected decisions) and applies it to followed
	// models whose price was not entered by hand.
	SavePriceCatalog(ctx context.Context, rows []PriceSync) error
	// SetPriceFollow switches following for a model and applies the current
	// suggestion when allowed.
	SetPriceFollow(ctx context.Context, model string, follow bool) error
	// AcceptPriceSuggestion makes the suggestion the model's own price
	// (source sync); RejectPriceSuggestion stops suggesting these prices.
	// Both answer ErrNotFound when the model has no catalog match.
	AcceptPriceSuggestion(ctx context.Context, model string) error
	RejectPriceSuggestion(ctx context.Context, model string) error

	PutPluginConfig(ctx context.Context, p *PluginConfig) error
	ListPluginConfigs(ctx context.Context) ([]PluginConfig, error)
	DeletePluginConfig(ctx context.Context, pluginID string) error

	// Feature states: a feature without one is at its default.
	PutFeature(ctx context.Context, f FeatureState) error
	ListFeatures(ctx context.Context) ([]FeatureState, error)

	// PutSetting stores one layered setting value. Validation against the
	// plugin's declaration is the caller's job (settings.Validate).
	PutSetting(ctx context.Context, e SettingRow) error
	DeleteSetting(ctx context.Context, scope plugin.Scope, scopeID, pluginID, key string) error
	ListSettings(ctx context.Context, f SettingFilter) ([]SettingRow, error)

	PutManifest(ctx context.Context, m *Manifest) error
	GetManifest(ctx context.Context, id string) (*Manifest, error)
	ListManifests(ctx context.Context) ([]Manifest, error)
	DeleteManifest(ctx context.Context, id string) error

	// InsertRecords stores finished requests and folds them into the hourly
	// rollups in one transaction. Records already stored (same ID) are
	// skipped, so a sink may retry a batch.
	InsertRecords(ctx context.Context, recs []*plugin.Record) error
	// QueryRecords pages through the request log newest first. Bodies are
	// not included; use GetRecord.
	QueryRecords(ctx context.Context, q RecordQuery) (*RecordPage, error)
	GetRecord(ctx context.Context, id string) (*plugin.Record, error)
	// RecordHistogram counts the log rows q matches (Cursor and Limit
	// ignored) per bucket, epoch-aligned; empty buckets are left out.
	RecordHistogram(ctx context.Context, q RecordQuery, bucket time.Duration) ([]RecordBucket, error)
	// TTFBQuantile is the p-quantile (0..1, nearest rank) of time to first
	// byte over the rows q matches that got a first byte; false when none did.
	TTFBQuantile(ctx context.Context, q RecordQuery, p float64) (time.Duration, bool, error)
	// SessionPage groups the log rows q matches by session id (rows
	// without one are left out), newest last request first; q.Cursor
	// continues from a previous page's Next.
	SessionPage(ctx context.Context, q RecordQuery) (*SessionList, error)
	// SessionTotalAt sums session id up to the request (at, id). userID,
	// when set, restricts the sum to that user's requests.
	SessionTotalAt(ctx context.Context, session, userID string, at time.Time, id string) (SessionTotal, error)
	// FailureSeries counts the failed requests q matches per bucket and
	// final cause: an attempt's cause, not_sent when no attempt was
	// made, unknown otherwise.
	FailureSeries(ctx context.Context, q RecordQuery, bucket time.Duration) ([]CausePoint, error)
	// PruneRecords deletes log rows older than before; rollups stay.
	PruneRecords(ctx context.Context, before time.Time) (int64, error)

	// Spent implements plugin.UsageReader over rollups plus the log for the
	// partial first hour. It sums ChargeMicro, what the subject is billed.
	Spent(ctx context.Context, subject string, since time.Time) (chargeMicro int64, requests int64, err error)
	// UsageSeries reads rollups bucketed by q.Bucket.
	UsageSeries(ctx context.Context, q UsageQuery) ([]UsagePoint, error)

	AddAudit(ctx context.Context, a *Audit) error
	ListAudit(ctx context.Context, q AuditQuery) ([]Audit, error)

	// KV returns durable key-value storage scoped to one plugin.
	KV(pluginID string) plugin.KV
	// PurgeExpired deletes expired sessions and plugin KV entries.
	PurgeExpired(ctx context.Context) error

	// Version returns the change counter; it grows on every routing-relevant
	// write.
	Version(ctx context.Context) (int64, error)
	// Snapshot reads everything routing needs in one consistent transaction.
	Snapshot(ctx context.Context) (*Snapshot, error)

	// Export writes business data as YAML (see Document). Credential
	// secrets are included in plaintext only when opts.Secrets is set.
	Export(ctx context.Context, opts ExportOptions) ([]byte, error)
	// Apply upserts the business data in a YAML Document in one
	// transaction and reports what changed; with dryRun nothing is written.
	// Rows not mentioned are left alone.
	Apply(ctx context.Context, doc []byte, dryRun bool) (*ApplyResult, error)

	// State opens a durable state.State on this database (driver "sql").
	State(ctx context.Context) (state.State, error)

	// Close flushes buffered touches and closes the database.
	Close() error
}

var _ plugin.UsageReader = Store(nil)
