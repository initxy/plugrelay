// Package importnewapi reads a new-api (QuantumNous/new-api) or one-api
// database and maps it onto plugrelay: channels become upstreams with one
// credential per key, their model lists become models and routes, users
// and groups carry over, tokens become platform keys, and user quota
// becomes balance.
//
// Schema: new-api model/channel.go, model/token.go, model/user.go and
// model/option.go at commit 2de51a19e35a602a0041d380b29be7c9bcd1e0e5 (the
// tables channels, tokens, users, options; one-api shares these columns).
//
// Keys: new-api stores tokens in plaintext and clients send "sk-" + key,
// so the import stores SHA-256("sk-" + key), the same hash plugrelay keeps
// for its own keys; clients keep their keys. Passwords are bcrypt in both
// relays and carry over; argon2 hashes (newer new-api) do not and those
// users must reset their password. new-api admins are imported as users
// and every imported address as unverified. Rows deleted in new-api are
// skipped.
package importnewapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/plugins/admin/importnewapi"
)

// Channel, Token, User are the new-api columns read.
type Channel struct {
	ID           int64  `json:"id"`
	Type         int    `json:"type"`
	Key          string `json:"key"`
	Status       int    `json:"status"`
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`
	Models       string `json:"models"`
	Group        string `json:"group"`
	ModelMapping string `json:"model_mapping"`
	Priority     int64  `json:"priority"`
	Weight       int64  `json:"weight"`
}

type Token struct {
	ID                 int64  `json:"id"`
	UserID             int64  `json:"user_id"`
	Key                string `json:"key"`
	Status             int    `json:"status"`
	Name               string `json:"name"`
	ExpiredTime        int64  `json:"expired_time"`
	RemainQuota        int64  `json:"remain_quota"`
	UnlimitedQuota     bool   `json:"unlimited_quota"`
	ModelLimitsEnabled bool   `json:"model_limits_enabled"`
	ModelLimits        string `json:"model_limits"`
	AllowIPs           string `json:"allow_ips"`
	Group              string `json:"group"`
	Deleted            bool   `json:"deleted,omitempty"`
}

type User struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Role        int    `json:"role"`
	Status      int    `json:"status"`
	Email       string `json:"email"`
	Quota       int64  `json:"quota"`
	Group       string `json:"group"`
	Deleted     bool   `json:"deleted,omitempty"`
}

// Dump is everything read from new-api. The JSON export has this shape;
// options is the options table as key → value.
type Dump struct {
	Channels []Channel         `json:"channels"`
	Tokens   []Token           `json:"tokens"`
	Users    []User            `json:"users"`
	Options  map[string]string `json:"options"`
}

// Load reads src.
func Load(ctx context.Context, src importnewapi.Source) (*Dump, error) {
	if len(src.Data) > 0 {
		var d Dump
		if err := json.Unmarshal(src.Data, &d); err != nil {
			return nil, fmt.Errorf("import: JSON export: %w", err)
		}
		return &d, nil
	}
	db, q, err := open(src)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("import: connect: %w", err)
	}
	return read(ctx, db, q)
}

// open connects to the source database. The DSN is admin input, but the
// relay must not be turned into a file reader: MySQL never sends local
// files (LOAD DATA LOCAL), and SQLite opens one existing file read-only,
// only when the admin confirms that this host's file is meant.
func open(src importnewapi.Source) (*sql.DB, string, error) {
	switch src.Driver {
	case "mysql":
		cfg, err := mysql.ParseDSN(src.DSN)
		if err != nil {
			return nil, "", fmt.Errorf("import: mysql DSN: %w", err)
		}
		if cfg.AllowAllFiles {
			return nil, "", errors.New("import: allowAllFiles is not allowed in the DSN")
		}
		for k := range cfg.Params {
			if strings.Contains(strings.ToLower(strings.ReplaceAll(k, "-", "_")), "local_infile") {
				return nil, "", errors.New("import: local_infile is not allowed in the DSN")
			}
		}
		cfg.AllowAllFiles = false
		conn, err := mysql.NewConnector(cfg)
		if err != nil {
			return nil, "", fmt.Errorf("import: open: %w", err)
		}
		return sql.OpenDB(conn), "`", nil
	case "postgres":
		db, err := sql.Open("pgx", src.DSN)
		if err != nil {
			return nil, "", fmt.Errorf("import: open: %w", err)
		}
		return db, `"`, nil
	case "sqlite":
		if !src.ConfirmLocalFile {
			return nil, "", errors.New("import: a SQLite source is a file on the relay's host; set confirm_local_file to read it")
		}
		path := strings.TrimPrefix(src.DSN, "file:")
		if strings.ContainsAny(path, "?#") || path == "" || path == ":memory:" {
			return nil, "", errors.New("import: the SQLite source must be a plain file path, without URI options")
		}
		path = filepath.Clean(path)
		if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
			return nil, "", fmt.Errorf("import: %s is not a readable database file", path)
		}
		db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro")
		if err != nil {
			return nil, "", fmt.Errorf("import: open: %w", err)
		}
		return db, `"`, nil
	}
	return nil, "", fmt.Errorf("import: driver must be mysql, postgres or sqlite")
}

func read(ctx context.Context, db *sql.DB, q string) (*Dump, error) {
	g, k := q+"group"+q, q+"key"+q
	d := &Dump{Options: map[string]string{}}
	rows, err := db.QueryContext(ctx, `SELECT id, type, `+k+`, status, name, base_url, models, `+g+`, model_mapping, priority, weight FROM channels`)
	if err != nil {
		return nil, fmt.Errorf("import: channels: %w", err)
	}
	for rows.Next() {
		var c Channel
		var name, base, models, group, mapping sql.NullString
		var prio, weight sql.NullInt64
		var typ, status sql.NullInt64
		if err := rows.Scan(&c.ID, &typ, &c.Key, &status, &name, &base, &models, &group, &mapping, &prio, &weight); err != nil {
			rows.Close()
			return nil, fmt.Errorf("import: channels: %w", err)
		}
		c.Type, c.Status = int(typ.Int64), int(status.Int64)
		c.Name, c.BaseURL, c.Models, c.Group, c.ModelMapping = name.String, base.String, models.String, group.String, mapping.String
		c.Priority, c.Weight = prio.Int64, weight.Int64
		d.Channels = append(d.Channels, c)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `SELECT id, user_id, `+k+`, status, name, expired_time, remain_quota, unlimited_quota,
model_limits_enabled, model_limits, allow_ips, `+g+`, deleted_at FROM tokens`)
	if err != nil {
		return nil, fmt.Errorf("import: tokens: %w", err)
	}
	for rows.Next() {
		var t Token
		var name, limits, ips, group sql.NullString
		var status, exp, remain sql.NullInt64
		var unlimited, limited sql.NullBool
		var deleted any
		if err := rows.Scan(&t.ID, &t.UserID, &t.Key, &status, &name, &exp, &remain, &unlimited, &limited, &limits, &ips, &group, &deleted); err != nil {
			rows.Close()
			return nil, fmt.Errorf("import: tokens: %w", err)
		}
		t.Status, t.Name, t.ExpiredTime, t.RemainQuota = int(status.Int64), name.String, exp.Int64, remain.Int64
		t.UnlimitedQuota, t.ModelLimitsEnabled, t.ModelLimits = unlimited.Bool, limited.Bool, limits.String
		t.AllowIPs, t.Group, t.Deleted = ips.String, group.String, deleted != nil
		d.Tokens = append(d.Tokens, t)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `SELECT id, username, password, display_name, role, status, email, quota, `+g+`, deleted_at FROM users`)
	if err != nil {
		return nil, fmt.Errorf("import: users: %w", err)
	}
	for rows.Next() {
		var u User
		var pw, display, email, group sql.NullString
		var role, status, quota sql.NullInt64
		var deleted any
		if err := rows.Scan(&u.ID, &u.Username, &pw, &display, &role, &status, &email, &quota, &group, &deleted); err != nil {
			rows.Close()
			return nil, fmt.Errorf("import: users: %w", err)
		}
		u.Password, u.DisplayName, u.Email, u.Group = pw.String, display.String, email.String, group.String
		u.Role, u.Status, u.Quota, u.Deleted = int(role.Int64), int(status.Int64), quota.Int64, deleted != nil
		d.Users = append(d.Users, u)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `SELECT `+k+`, value FROM options`)
	if err != nil {
		return nil, fmt.Errorf("import: options: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var v sql.NullString
		if err := rows.Scan(&key, &v); err != nil {
			return nil, fmt.Errorf("import: options: %w", err)
		}
		d.Options[key] = v.String
	}
	return d, rows.Err()
}

// target is the manifest a channel type maps to. baseVar names the
// manifest variable taking the channel's base URL (v1: it includes /v1).
type target struct {
	manifest string
	baseVar  string
	v1       bool
}

// channelTypes maps new-api constant/channel.go types to manifests.
var channelTypes = map[int]target{
	1:  {manifest: "openai"},
	4:  {manifest: "ollama", baseVar: "base_url", v1: true},
	8:  {manifest: "openai-compatible", baseVar: "base_url", v1: true},
	14: {manifest: "anthropic"},
	17: {manifest: "qwen-dashscope"},
	20: {manifest: "openrouter"},
	24: {manifest: "gemini"},
	25: {manifest: "moonshot-kimi"},
	26: {manifest: "zhipu-glm"},
	33: {manifest: "bedrock"},
	35: {manifest: "minimax"},
	40: {manifest: "openai-compatible", baseVar: "base_url", v1: true}, // SiliconFlow
	42: {manifest: "mistral"},
	43: {manifest: "deepseek"},
	45: {manifest: "volcengine-ark"},
	47: {manifest: "xinference", baseVar: "server_url"},
	48: {manifest: "xai"},
	62: {manifest: "vllm", baseVar: "base_url", v1: true},
	63: {manifest: "sglang", baseVar: "server_url"},
}

// officialBase is each type's default base URL in new-api; a channel with
// another base URL is a proxy and imports as openai-compatible when the
// target manifest cannot take one.
var officialBase = map[int]string{1: "https://api.openai.com", 14: "https://api.anthropic.com",
	24: "https://generativelanguage.googleapis.com", 40: "https://api.siliconflow.cn"}

// ManifestLookup resolves a manifest name to its provider id.
type ManifestLookup func(name string) (provider string, official, ok bool)

// Options tunes an import.
type Options struct {
	Manifests ManifestLookup
	// Rate is site currency units per USD of new-api quota (0 = 1).
	Rate     float64
	Currency string
	// Apply writes the mapped store document; nil is the store's own
	// Apply. The relay passes one that runs the plugins' settings checks.
	Apply func(ctx context.Context, doc []byte, dryRun bool) (*store.ApplyResult, error)
}

// Importer runs imports against a store.
type Importer struct {
	Store store.Store
	Opts  func() Options
}

var _ importnewapi.Importer = Importer{}

// ImportNewAPI implements importnewapi.Importer.
func (im Importer) ImportNewAPI(ctx context.Context, src importnewapi.Source, dryRun bool) (*importnewapi.Report, error) {
	d, err := Load(ctx, src)
	if err != nil {
		return nil, err
	}
	o := im.Opts()
	o.Rate = src.Rate
	return Run(ctx, im.Store, d, o, dryRun)
}

func groupID(name string) string { return "newapi-" + name }

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// plan is what Run writes.
type plan struct {
	doc      store.Document
	users    []store.User
	hashes   map[string]string // user id -> bcrypt hash
	keys     []store.Key
	balances []store.BalanceTxn
	rep      *importnewapi.Report
}

func (p *plan) add(kind, source, id, op, reason string) {
	p.rep.Items = append(p.rep.Items, importnewapi.Item{Kind: kind, Source: source, ID: id, Op: op, Reason: reason})
	if op != "skip" {
		p.rep.Counts[kind]++
	} else {
		p.rep.Counts["skipped"]++
	}
}

func (p *plan) warn(format string, a ...any) {
	p.rep.Warnings = append(p.rep.Warnings, fmt.Sprintf(format, a...))
}

// Run maps d and applies it (or, with dryRun, reports what it would do).
// Re-running is safe: rows are keyed by their new-api ids and balance
// credits by user, so nothing is created or credited twice.
func Run(ctx context.Context, st store.Store, d *Dump, o Options, dryRun bool) (*importnewapi.Report, error) {
	p := &plan{hashes: map[string]string{}, rep: &importnewapi.Report{DryRun: dryRun, Counts: map[string]int{},
		Items: []importnewapi.Item{}, Warnings: []string{}}}
	p.groups(d)
	p.channels(d, o)
	users := p.mapUsers(d, o)
	p.tokens(d, users)

	doc, err := json.Marshal(p.doc)
	if err != nil {
		return nil, err
	}
	apply := o.Apply
	if apply == nil {
		apply = st.Apply
	}
	res, err := apply(ctx, doc, dryRun)
	if err != nil {
		return nil, fmt.Errorf("import: %w", err)
	}
	ops := map[string]string{}
	for _, c := range res.Changes {
		ops[c.Kind+"|"+c.ID] = c.Op
	}
	for i := range p.rep.Items {
		it := &p.rep.Items[i]
		if op, ok := ops[it.Kind+"|"+it.ID]; ok && it.Op == "create" {
			it.Op = op
		}
	}
	if err := p.writeRows(ctx, st, dryRun); err != nil {
		return nil, err
	}
	return p.rep, nil
}

func (p *plan) groups(d *Dump) {
	ratios := map[string]float64{}
	_ = json.Unmarshal([]byte(d.Options["GroupRatio"]), &ratios)
	names := map[string]bool{}
	for _, u := range d.Users {
		if u.Group != "" && !u.Deleted {
			names[u.Group] = true
		}
	}
	for _, t := range d.Tokens {
		if t.Group != "" && !t.Deleted {
			names[t.Group] = true
		}
	}
	for _, c := range d.Channels {
		for _, g := range splitList(c.Group) {
			names[g] = true
		}
	}
	for g := range ratios {
		names[g] = true
	}
	sorted := make([]string, 0, len(names))
	for g := range names {
		sorted = append(sorted, g)
	}
	sort.Strings(sorted)
	for _, g := range sorted {
		m := ratios[g]
		if m <= 0 {
			m = 1
		}
		p.doc.Groups = append(p.doc.Groups, store.DocGroup{ID: groupID(g), Name: g, PriceMultiplier: m})
		p.add("group", g, groupID(g), "create", "")
	}
}

func (p *plan) channels(d *Dump, o Options) {
	routes := map[string][]store.DocRoute{}
	for _, c := range d.Channels {
		src := strconv.FormatInt(c.ID, 10)
		id := fmt.Sprintf("newapi-c%d", c.ID)
		t, ok := channelTypes[c.Type]
		if !ok {
			p.add("upstream", src, id, "skip", fmt.Sprintf("channel type %d has no plugrelay manifest; add it by hand", c.Type))
			continue
		}
		base := strings.TrimRight(c.BaseURL, "/")
		if def, known := officialBase[c.Type]; base != "" && known && base != def && t.baseVar == "" {
			t = target{manifest: "openai-compatible", baseVar: "base_url", v1: true}
			if c.Type == 14 || c.Type == 24 {
				p.add("upstream", src, id, "skip", "a "+map[int]string{14: "Claude", 24: "Gemini"}[c.Type]+
					" channel through a proxy base URL; add it by hand with that URL")
				continue
			}
		}
		provider, official, ok := o.Manifests(t.manifest)
		if !ok {
			p.add("upstream", src, id, "skip", "manifest "+t.manifest+" is not available")
			continue
		}
		cfg := map[string]any{"manifest": t.manifest}
		if t.baseVar != "" {
			if base == "" {
				base = officialBase[c.Type]
			}
			if base == "" {
				p.add("upstream", src, id, "skip", "no base URL")
				continue
			}
			if t.v1 && !strings.HasSuffix(base, "/v1") {
				base += "/v1"
			}
			cfg[t.baseVar] = base
		} else if base != "" && base != officialBase[c.Type] {
			p.warn("channel %d (%s): base URL %s not carried over; %s uses its own", c.ID, c.Name, base, t.manifest)
		}
		enabled := c.Status == 1
		srcKind := store.SourceCustom
		if official {
			srcKind = store.SourceOfficial
		}
		u := store.DocUpstream{ID: id, Provider: provider, Manifest: t.manifest, Config: cfg, Enabled: &enabled, Source: srcKind}
		keys := splitList(strings.ReplaceAll(c.Key, "\r", ""))
		if len(keys) == 0 {
			p.add("upstream", src, id, "skip", "channel has no key")
			continue
		}
		p.doc.Upstreams = append(p.doc.Upstreams, u)
		p.add("upstream", src, id, "create", c.Name)
		for i, k := range keys {
			cid := fmt.Sprintf("%s-k%d", id, i+1)
			if c.Type == 33 { // AK|SK|region
				parts := strings.Split(k, "|")
				if len(parts) >= 2 {
					k = parts[0] + ":" + parts[1]
				}
				if len(parts) >= 3 && parts[2] != "" {
					cfg["region"] = parts[2]
				}
			}
			p.doc.Credentials = append(p.doc.Credentials, store.DocCredential{ID: cid, Upstream: id, Kind: store.CredShared, Secret: k})
			p.add("credential", src, cid, "create", "")
		}
		var mapping map[string]string
		_ = json.Unmarshal([]byte(c.ModelMapping), &mapping)
		var groups []string
		for _, g := range splitList(c.Group) {
			groups = append(groups, groupID(g))
		}
		for _, m := range splitList(c.Models) {
			um := m
			if v := mapping[m]; v != "" {
				um = v
			}
			routes[m] = append(routes[m], store.DocRoute{Upstream: id, UpstreamModel: um, Priority: int(c.Priority),
				Weight: int(max(c.Weight, 1)), Groups: groups})
		}
	}
	names := make([]string, 0, len(routes))
	for m := range routes {
		names = append(names, m)
	}
	sort.Strings(names)
	for _, m := range names {
		p.doc.Models = append(p.doc.Models, store.DocModel{Name: m, Routes: routes[m]})
		p.add("model", "", m, "create", fmt.Sprintf("%d routes", len(routes[m])))
	}
}

func bcryptHash(h string) bool {
	return strings.HasPrefix(h, "$2a$") || strings.HasPrefix(h, "$2b$") || strings.HasPrefix(h, "$2y$")
}

// mapUsers returns new-api user id -> plugrelay user.
func (p *plan) mapUsers(d *Dump, o Options) map[int64]store.User {
	out := map[int64]store.User{}
	perUnit := 500000.0
	if v, err := strconv.ParseFloat(d.Options["QuotaPerUnit"], 64); err == nil && v > 0 {
		perUnit = v
	}
	rate := o.Rate
	if rate <= 0 {
		rate = 1
		if o.Currency != "" && !strings.EqualFold(o.Currency, "USD") {
			p.warn("new-api quota is in USD and the site currency is %s; balances use 1 USD = 1 %s unless a rate is given",
				o.Currency, o.Currency)
		}
	}
	for _, u := range d.Users {
		src := strconv.FormatInt(u.ID, 10)
		id := fmt.Sprintf("newapi-u%d", u.ID)
		if u.Deleted {
			p.add("user", src, id, "skip", "deleted in new-api")
			continue
		}
		login := strings.TrimSpace(u.Email)
		if login == "" {
			login = u.Username
		}
		name := u.DisplayName
		if name == "" {
			name = u.Username
		}
		// new-api admins come over as plain users: their password (or
		// a weak one) was never under this relay's rules, so an admin
		// promotes them by hand after they set a new one.
		if u.Role >= 10 {
			p.warn("user %s was an admin in new-api and is imported as a user; make them admin by hand once they have set a new password", login)
		}
		// Nobody verified new-api's addresses for this relay: SSO
		// link_by_email must not link to them.
		su := store.User{ID: id, Name: name, Email: login, Role: store.RoleUser, Disabled: u.Status != 1, EmailUnverified: true}
		if u.Group != "" {
			su.GroupID = groupID(u.Group)
		}
		if bcryptHash(u.Password) {
			p.hashes[id] = u.Password
		} else if u.Password != "" {
			p.warn("user %s: password hash is not bcrypt; set a new password for this user", login)
		}
		out[u.ID] = su
		p.users = append(p.users, su)
		p.add("user", src, id, "create", login)
		if u.Quota > 0 {
			micro := int64(math.Round(float64(u.Quota) / perUnit * rate * 1e6))
			p.balances = append(p.balances, store.BalanceTxn{ID: "import:newapi:" + id, Subject: "user:" + id,
				Kind: store.TxnAdjustment, AmountMicro: micro, Note: fmt.Sprintf("new-api quota %d", u.Quota), Actor: "import"})
			p.add("balance", src, "user:"+id, "create", fmt.Sprintf("%d micro-units", micro))
		}
	}
	return out
}

func (p *plan) tokens(d *Dump, users map[int64]store.User) {
	for _, t := range d.Tokens {
		src := strconv.FormatInt(t.ID, 10)
		id := fmt.Sprintf("newapi-t%d", t.ID)
		u, ok := users[t.UserID]
		switch {
		case t.Deleted:
			p.add("key", src, id, "skip", "deleted in new-api")
			continue
		case !ok:
			p.add("key", src, id, "skip", "its user was not imported")
			continue
		}
		plain := secret.ImportedKeyPrefix + strings.TrimPrefix(t.Key, secret.ImportedKeyPrefix)
		if !secret.ValidImportedKeyFormat(plain) {
			p.add("key", src, id, "skip", "not a 48-character new-api key")
			continue
		}
		k := store.Key{ID: id, Name: t.Name, Hash: secret.HashKey(plain), Last4: secret.Last4(plain), UserID: u.ID,
			GroupID: u.GroupID, Disabled: t.Status != 1, Settings: map[string]json.RawMessage{}}
		if t.Group != "" {
			k.GroupID = groupID(t.Group)
		}
		if t.ExpiredTime > 0 {
			k.ExpiresAt = time.Unix(t.ExpiredTime, 0).UTC()
		}
		if t.ModelLimitsEnabled {
			if ms := splitList(t.ModelLimits); len(ms) > 0 {
				k.Settings["models"], _ = json.Marshal(ms)
			}
		}
		if ips := splitList(strings.ReplaceAll(t.AllowIPs, " ", "\n")); len(ips) > 0 {
			k.Settings["ip_allowlist"], _ = json.Marshal(ips)
		}
		if !t.UnlimitedQuota {
			p.warn("token %d (%s): its own quota limit is not carried over; set the key's budget if needed", t.ID, t.Name)
		}
		p.keys = append(p.keys, k)
		p.add("key", src, id, "create", t.Name+" …"+k.Last4)
	}
}

// writeRows writes users, keys and balances, which Apply does not cover;
// on a dry run it only marks rows that already exist.
func (p *plan) writeRows(ctx context.Context, st store.Store, dryRun bool) error {
	ri, ok := store.RowImporterOf(st)
	l, lok := store.LedgerOf(st)
	if !ok || !lok {
		return errors.New("import: the store cannot import users and keys")
	}
	existingUsers := map[string]bool{}
	byEmail := map[string]string{}
	if us, err := st.ListUsers(ctx); err == nil {
		for _, u := range us {
			existingUsers[u.ID] = true
			if u.Email != "" {
				byEmail[strings.ToLower(u.Email)] = u.ID
			}
		}
	}
	existingKeys := map[string]bool{}
	if ks, err := st.ListKeys(ctx, store.KeyFilter{}); err == nil {
		for _, k := range ks {
			existingKeys[k.ID] = true
		}
	}
	set := func(kind, id, op, reason string) {
		for i := range p.rep.Items {
			if it := &p.rep.Items[i]; it.Kind == kind && it.ID == id {
				if op == "skip" && it.Op != "skip" {
					p.rep.Counts[kind]--
					p.rep.Counts["skipped"]++
				}
				it.Op, it.Reason = op, reason
			}
		}
	}
	skipped := map[string]bool{}
	for _, u := range p.users {
		if other, taken := byEmail[strings.ToLower(u.Email)]; u.Email != "" && taken && other != u.ID {
			set("user", u.ID, "skip", "login "+u.Email+" is already used by user "+other)
			skipped[u.ID] = true
			continue
		}
		if u.Email != "" {
			byEmail[strings.ToLower(u.Email)] = u.ID // a later user of the batch with this email conflicts
		}
		if existingUsers[u.ID] {
			set("user", u.ID, "unchanged", u.Email)
			continue
		}
		if dryRun {
			continue
		}
		if _, err := ri.ImportUser(ctx, &u, p.hashes[u.ID]); err != nil {
			return fmt.Errorf("import: user %s: %w", u.Email, err)
		}
	}
	for _, k := range p.keys {
		if skipped[k.UserID] {
			set("key", k.ID, "skip", "its user was not imported")
			continue
		}
		if existingKeys[k.ID] {
			set("key", k.ID, "unchanged", k.Name)
			continue
		}
		if dryRun {
			continue
		}
		if _, err := ri.ImportKey(ctx, &k); err != nil {
			return fmt.Errorf("import: key %s: %w", k.Name, err)
		}
	}
	for _, b := range p.balances {
		uid := strings.TrimPrefix(b.Subject, "user:")
		if skipped[uid] {
			set("balance", b.Subject, "skip", "its user was not imported")
			continue
		}
		if _, err := l.GetTxn(ctx, b.ID); err == nil {
			set("balance", b.Subject, "unchanged", "credited by an earlier import")
			continue
		}
		if dryRun {
			continue
		}
		if _, _, err := l.Credit(ctx, &b); err != nil {
			return fmt.Errorf("import: balance of %s: %w", b.Subject, err)
		}
	}
	return nil
}
