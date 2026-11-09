package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// KVNamespace is the plugin KV namespace holding the server list.
const KVNamespace = "core.mcp"

const serversKey = "servers"

// Transports of an upstream MCP server.
const (
	TransportAuto       = "auto"            // Streamable HTTP, else the legacy HTTP+SSE transport
	TransportStreamable = "streamable-http" // MCP 2025-03-26 and later
	TransportSSE        = "sse"             // the HTTP+SSE transport of 2024-11-05
)

// DefaultTimeout bounds one call to an upstream server.
const DefaultTimeout = 2 * time.Minute

// Server is one configured upstream MCP server. Secret header values are
// sealed in storage and never returned by the admin API.
type Server struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	URL       string `json:"url"`
	Transport string `json:"transport,omitempty"`
	Enabled   bool   `json:"enabled"`
	// Headers are sent as is; SecretHeaders (name → sealed value) carry
	// credentials. Client headers never reach the server.
	Headers       map[string]string `json:"headers,omitempty"`
	SecretHeaders map[string]string `json:"secret_headers,omitempty"`
	// ToolsAllow and ToolsDeny filter the server's tools for everyone
	// (names or glob patterns, case-insensitive); empty allow = all.
	ToolsAllow []string `json:"tools_allow,omitempty"`
	ToolsDeny  []string `json:"tools_deny,omitempty"`
	// Proxy is the fixed egress ("" = direct); AllowPrivate lets the
	// server live on a private or loopback address.
	Proxy        string `json:"proxy,omitempty"`
	AllowPrivate bool   `json:"allow_private,omitempty"`
	// TimeoutSec bounds one call; 0 = DefaultTimeout.
	TimeoutSec int `json:"timeout_sec,omitempty"`
}

func (s *Server) timeout() time.Duration {
	if s.TimeoutSec > 0 {
		return time.Duration(s.TimeoutSec) * time.Second
	}
	return DefaultTimeout
}

// View is a server as the admin API shows it: secret header names only.
type View struct {
	Server
	SecretHeaders []string `json:"secret_headers"`
}

// ViewOf hides a server's secret values.
func ViewOf(s Server) View {
	v := View{Server: s, SecretHeaders: []string{}}
	for k := range s.SecretHeaders {
		v.SecretHeaders = append(v.SecretHeaders, k)
	}
	sort.Strings(v.SecretHeaders)
	v.Server.SecretHeaders = nil
	return v
}

// Input is a create or update body. On update, a nil field keeps the stored
// value; in SecretHeaders an empty value removes that header.
type Input struct {
	ID            *string           `json:"id"`
	Name          *string           `json:"name"`
	URL           *string           `json:"url"`
	Transport     *string           `json:"transport"`
	Enabled       *bool             `json:"enabled"`
	Headers       map[string]string `json:"headers"`
	SecretHeaders map[string]string `json:"secret_headers"`
	ToolsAllow    []string          `json:"tools_allow"`
	ToolsDeny     []string          `json:"tools_deny"`
	Proxy         *string           `json:"proxy"`
	AllowPrivate  *bool             `json:"allow_private"`
	TimeoutSec    *int              `json:"timeout_sec"`
}

// FieldError is a refused field: Code is a stable id for the wording
// (callers translate it), Message the English text.
type FieldError struct{ Field, Code, Message string }

func (e *FieldError) Error() string { return e.Field + ": " + e.Message }

// Errors of the registry.
var (
	ErrNotFound = errors.New("mcp server not found")
	ErrExists   = errors.New("an mcp server with this id already exists")
)

// IDPattern is the shape of a server id: it appears in URLs and, with
// "__", in aggregated tool names, so it has no underscores.
var IDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

var headerName = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// headers a server may not be configured to receive: framing and the
// protocol's own.
var reservedHeaders = []string{"host", "content-length", "content-type", "accept", "connection", "transfer-encoding",
	"mcp-session-id", "mcp-protocol-version", "mcp-method", "mcp-name", "last-event-id"}

// Registry stores the server list in plugin KV.
type Registry struct {
	kv  plugin.KV
	box *secret.Box

	// edit serializes read-modify-write on this node; edits on several
	// nodes at once are last-writer-wins, like other plugin KV settings.
	edit   sync.Mutex
	mu     sync.Mutex
	cached []Server
	at     time.Time
}

// NewRegistry returns a registry over kv; box seals secret headers (nil
// refuses them).
func NewRegistry(kv plugin.KV, box *secret.Box) *Registry { return &Registry{kv: kv, box: box} }

// cacheFor is how long a node trusts its copy of the list; edits on the
// same node apply at once.
const cacheFor = 2 * time.Second

// List returns the stored servers, secrets still sealed.
func (r *Registry) List(ctx context.Context) ([]Server, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cached != nil && time.Since(r.at) < cacheFor {
		return slices.Clone(r.cached), nil
	}
	b, ok, err := r.kv.Get(ctx, serversKey)
	if err != nil {
		return nil, err
	}
	out := []Server{}
	if ok {
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, fmt.Errorf("mcp: stored servers: %w", err)
		}
	}
	r.cached, r.at = out, time.Now()
	return slices.Clone(out), nil
}

// Get returns one server, secrets sealed.
func (r *Registry) Get(ctx context.Context, id string) (*Server, error) {
	all, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}
	return nil, ErrNotFound
}

func (r *Registry) save(ctx context.Context, all []Server) error {
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	if err := r.kv.Put(ctx, serversKey, b, 0); err != nil {
		return err
	}
	r.cached, r.at = slices.Clone(all), time.Now()
	return nil
}

// Create stores a new server.
func (r *Registry) Create(ctx context.Context, in Input) (*Server, error) {
	s := Server{Enabled: true, Transport: TransportAuto}
	if in.ID == nil || !IDPattern.MatchString(*in.ID) {
		return nil, &FieldError{"id", "id_format", "must be 1-32 lowercase letters, digits or dashes, starting with a letter or digit"}
	}
	s.ID = *in.ID
	if err := r.apply(&s, in); err != nil {
		return nil, err
	}
	r.lockedEdit()
	defer r.unlockEdit()
	all, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(all, func(o Server) bool { return o.ID == s.ID }) {
		return nil, ErrExists
	}
	if err := r.save(ctx, append(all, s)); err != nil {
		return nil, err
	}
	return &s, nil
}

// Update edits a stored server; the id cannot change.
func (r *Registry) Update(ctx context.Context, id string, in Input) (*Server, error) {
	r.lockedEdit()
	defer r.unlockEdit()
	all, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(all, func(o Server) bool { return o.ID == id })
	if i < 0 {
		return nil, ErrNotFound
	}
	if in.ID != nil && *in.ID != id {
		return nil, &FieldError{"id", "immutable", "cannot be changed"}
	}
	s := all[i]
	s.Headers, s.SecretHeaders = maps.Clone(s.Headers), maps.Clone(s.SecretHeaders)
	if err := r.apply(&s, in); err != nil {
		return nil, err
	}
	all[i] = s
	if err := r.save(ctx, all); err != nil {
		return nil, err
	}
	return &s, nil
}

// Delete removes a server.
func (r *Registry) Delete(ctx context.Context, id string) error {
	r.lockedEdit()
	defer r.unlockEdit()
	all, err := r.List(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(all, func(o Server) bool { return o.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	return r.save(ctx, slices.Delete(all, i, i+1))
}

func (r *Registry) lockedEdit() { r.edit.Lock(); r.mu.Lock(); r.cached = nil; r.mu.Unlock() }
func (r *Registry) unlockEdit() { r.edit.Unlock() }

// origin is the scheme and host of a server URL, lower-cased with default
// ports dropped; "" when it does not parse.
func origin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

func hasFold(m map[string]string, key string) bool {
	for k := range m {
		if http.CanonicalHeaderKey(k) == key {
			return true
		}
	}
	return false
}

func (r *Registry) apply(s *Server, in Input) error {
	if in.Name != nil {
		s.Name = strings.TrimSpace(*in.Name)
	}
	oldOrigin := origin(s.URL)
	if in.URL != nil {
		s.URL = strings.TrimSpace(*in.URL)
	}
	u, err := url.Parse(s.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return &FieldError{"url", "url_no_credentials", "must be an http(s) URL without credentials"}
	}
	if oldOrigin != "" && origin(s.URL) != oldOrigin {
		// Stored secrets were entered for the old address; repointing the
		// server must not ship them to a new one unseen.
		for k := range s.SecretHeaders {
			if !hasFold(in.SecretHeaders, k) {
				return &FieldError{"secret_headers." + k, "secret_reenter", "the server address changed: enter this secret again, or clear it"}
			}
		}
	}
	if in.Transport != nil {
		s.Transport = *in.Transport
	}
	if s.Transport == "" {
		s.Transport = TransportAuto
	}
	if !slices.Contains([]string{TransportAuto, TransportStreamable, TransportSSE}, s.Transport) {
		return &FieldError{"transport", "transport", "must be auto, streamable-http or sse"}
	}
	if in.Enabled != nil {
		s.Enabled = *in.Enabled
	}
	if in.Headers != nil {
		s.Headers = map[string]string{}
		for k, v := range in.Headers {
			if err := checkHeader(k, v); err != nil {
				return &FieldError{"headers." + k, "header_invalid", err.Error()}
			}
			s.Headers[http.CanonicalHeaderKey(k)] = v
		}
	}
	for k, v := range in.SecretHeaders {
		ck := http.CanonicalHeaderKey(k)
		if v == "" {
			delete(s.SecretHeaders, ck)
			continue
		}
		if err := checkHeader(k, v); err != nil {
			return &FieldError{"secret_headers." + k, "header_invalid", err.Error()}
		}
		if secret.IsSealed(v) {
			return &FieldError{"secret_headers." + k, "invalid", "invalid"}
		}
		if r.box == nil {
			return &FieldError{"secret_headers." + k, "no_master_key", "no master key is configured, so secrets cannot be stored"}
		}
		sealed, err := r.box.Seal([]byte(v), secretAD(s.ID, ck))
		if err != nil {
			return err
		}
		if s.SecretHeaders == nil {
			s.SecretHeaders = map[string]string{}
		}
		s.SecretHeaders[ck] = sealed
	}
	if in.ToolsAllow != nil {
		s.ToolsAllow = clean(in.ToolsAllow)
	}
	if in.ToolsDeny != nil {
		s.ToolsDeny = clean(in.ToolsDeny)
	}
	if in.Proxy != nil {
		s.Proxy = strings.TrimSpace(*in.Proxy)
	}
	if err := egress.CheckProxy(s.Proxy); err != nil {
		return &FieldError{"proxy", "proxy", err.Error()}
	}
	if in.AllowPrivate != nil {
		s.AllowPrivate = *in.AllowPrivate
	}
	if in.TimeoutSec != nil {
		if *in.TimeoutSec < 0 || *in.TimeoutSec > 3600 {
			return &FieldError{"timeout_sec", "timeout_range", "must be between 0 and 3600"}
		}
		s.TimeoutSec = *in.TimeoutSec
	}
	return nil
}

func checkHeader(k, v string) error {
	if !headerName.MatchString(k) {
		return errors.New("invalid header name")
	}
	if slices.Contains(reservedHeaders, strings.ToLower(k)) {
		return errors.New("this header is set by the relay")
	}
	if strings.ContainsAny(v, "\r\n") {
		return errors.New("invalid header value")
	}
	return nil
}

func clean(l []string) []string {
	out := []string{}
	for _, s := range l {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func secretAD(id, header string) string { return "mcp:" + id + ":" + strings.ToLower(header) }

// open returns the server's headers with secrets decrypted.
func (r *Registry) open(s *Server) (http.Header, error) {
	h := http.Header{}
	for k, v := range s.Headers {
		h.Set(k, v)
	}
	for k, v := range s.SecretHeaders {
		if r.box == nil {
			return nil, errors.New("no master key to open secret headers")
		}
		b, err := r.box.Open(v, secretAD(s.ID, k))
		if err != nil {
			return nil, fmt.Errorf("secret header %s: %w", k, err)
		}
		h.Set(k, string(b))
	}
	return h, nil
}
