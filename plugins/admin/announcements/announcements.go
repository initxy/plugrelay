// Package announcements is the admin.announcements plugin (sell kit):
// operators write short Markdown notices with a start and end time and an
// audience; signed-in people see the live ones as a banner they can
// dismiss. Notices and who dismissed which live in the plugin's KV.
package announcements

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/sellkit/ledger"
)

// ID is the plugin id.
const ID = "admin.announcements"

func init() { plugin.Register(ID, func() plugin.Plugin { return New(nil) }) }

// Audiences.
const (
	Everyone = "all"
	Members  = "members" // everyone who is not an admin
	Admins   = "admins"
)

// Limits on what an operator writes.
const (
	maxItems = 200
	maxTitle = 120
	maxBody  = 10_000
)

// Announcement is one notice. Body is Markdown; zero times are open.
type Announcement struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Audience  string    `json:"audience"`
	StartsAt  time.Time `json:"starts_at,omitzero"`
	EndsAt    time.Time `json:"ends_at,omitzero"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// live reports whether a is shown at now.
func (a Announcement) live(now time.Time) bool {
	return (a.StartsAt.IsZero() || !now.Before(a.StartsAt)) && (a.EndsAt.IsZero() || now.Before(a.EndsAt))
}

func (a Announcement) state(now time.Time) string {
	switch {
	case !a.StartsAt.IsZero() && now.Before(a.StartsAt):
		return "scheduled"
	case !a.EndsAt.IsZero() && !now.Before(a.EndsAt):
		return "ended"
	}
	return "live"
}

func (a Announcement) shownTo(role string) bool {
	switch a.Audience {
	case Admins:
		return role == "admin"
	case Members:
		return role != "admin"
	}
	return true
}

// Plugin implements plugin.Admin.
type Plugin struct {
	kv       plugin.KV
	settings plugin.Settings
	now      func() time.Time
	mu       sync.Mutex // edits of the list
}

var _ plugin.Admin = (*Plugin)(nil)

// New returns the plugin; a nil KV is taken from the Host in Init.
func New(kv plugin.KV) *Plugin { return &Plugin{kv: kv, now: time.Now} }

// Info implements plugin.Plugin.
func (p *Plugin) Info() plugin.Info {
	return plugin.Info{ID: ID, Kind: plugin.KindAdmin, Version: "1.0.0",
		Description: "Announcements: notices with a time window and an audience, shown as a banner in the console.",
		Settings: []plugin.Setting{
			{Key: "enabled", Type: plugin.TypeBool, Default: false, Scopes: plugin.Global, Merge: plugin.Override,
				Label: plugin.T("Announcements", "公告"),
				Help: plugin.T("Notices you write on the Announcements page appear as a banner at the top of the console while they run.",
					"在「公告」页写的通知，在生效时间内显示在控制台顶部。")},
		}}
}

// Init implements plugin.Initializer.
func (p *Plugin) Init(_ context.Context, h plugin.Host) error {
	if p.kv == nil {
		p.kv = h.KV()
	}
	p.settings = h.Settings()
	return nil
}

// Routes implements plugin.Admin.
func (p *Plugin) Routes() []plugin.AdminRoute {
	return []plugin.AdminRoute{
		{Method: http.MethodGet, Path: "/announcements", Handler: p.getAll, Role: "admin"},
		{Method: http.MethodPost, Path: "/announcements", Handler: p.post, Role: "admin"},
		{Method: http.MethodPut, Path: "/announcements/{id}", Handler: p.put, Role: "admin"},
		{Method: http.MethodDelete, Path: "/announcements/{id}", Handler: p.delete, Role: "admin"},
		{Method: http.MethodGet, Path: "/me", Handler: p.getMine},
		{Method: http.MethodPost, Path: "/me/dismiss", Handler: p.postDismiss},
	}
}

func (p *Plugin) enabled() bool { return p.settings != nil && p.settings.Get("enabled").Bool() }

func (p *Plugin) load(ctx context.Context) ([]Announcement, error) {
	b, ok, err := p.kv.Get(ctx, "items")
	if err != nil || !ok {
		return []Announcement{}, err
	}
	var out []Announcement
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *Plugin) save(ctx context.Context, items []Announcement) error {
	b, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return p.kv.Put(ctx, "items", b, 0)
}

type view struct {
	Announcement
	State string `json:"state"`
}

func (p *Plugin) getAll(w http.ResponseWriter, r *http.Request) {
	items, err := p.load(r.Context())
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	now := p.now()
	out := make([]view, 0, len(items))
	for _, a := range items {
		out = append(out, view{Announcement: a, State: a.state(now)})
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"items": out, "enabled": p.enabled()})
}

type input struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Audience string `json:"audience"`
	StartsAt string `json:"starts_at"`
	EndsAt   string `json:"ends_at"`
}

// parse checks in and copies it onto a.
func (in input) parse(w http.ResponseWriter, a *Announcement) bool {
	bad := func(code, msg string) bool {
		ledger.WriteError(w, http.StatusBadRequest, code, msg)
		return false
	}
	title, body := strings.TrimSpace(in.Title), strings.TrimSpace(in.Body)
	if title == "" || utf8.RuneCountInString(title) > maxTitle {
		return bad("announcement_title", "the title must be 1 to 120 characters")
	}
	if utf8.RuneCountInString(body) > maxBody {
		return bad("announcement_body", "the text must be at most 10000 characters")
	}
	if in.Audience == "" {
		in.Audience = Everyone
	}
	if !slices.Contains([]string{Everyone, Members, Admins}, in.Audience) {
		return bad("announcement_audience", "audience is all, members or admins")
	}
	var start, end time.Time
	for _, f := range []struct {
		raw string
		dst *time.Time
	}{{in.StartsAt, &start}, {in.EndsAt, &end}} {
		if f.raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, f.raw)
		if err != nil {
			return bad("announcement_time", "times are RFC 3339")
		}
		*f.dst = t.UTC()
	}
	if !start.IsZero() && !end.IsZero() && !end.After(start) {
		return bad("announcement_window", "an announcement must end after it starts")
	}
	a.Title, a.Body, a.Audience, a.StartsAt, a.EndsAt = title, body, in.Audience, start, end
	return true
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "ann_" + hex.EncodeToString(b)
}

func (p *Plugin) post(w http.ResponseWriter, r *http.Request) {
	var in input
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	now := p.now().UTC().Truncate(time.Second)
	a := Announcement{ID: newID(), CreatedAt: now, UpdatedAt: now}
	a.CreatedBy, _ = ledger.Who(r)
	if !in.parse(w, &a) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx := r.Context()
	items, err := p.load(ctx)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	if len(items) >= maxItems {
		ledger.WriteError(w, http.StatusConflict, "announcements_full", "delete old announcements first (at most 200)")
		return
	}
	items = append([]Announcement{a}, items...)
	if err := p.save(ctx, items); err != nil {
		ledger.Fail(w, err)
		return
	}
	ledger.WriteJSON(w, http.StatusCreated, view{Announcement: a, State: a.state(p.now())})
}

func idOf(r *http.Request) string { return r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:] }

func (p *Plugin) put(w http.ResponseWriter, r *http.Request) {
	var in input
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx := r.Context()
	items, err := p.load(ctx)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	i := slices.IndexFunc(items, func(a Announcement) bool { return a.ID == idOf(r) })
	if i < 0 {
		ledger.Fail(w, ledger.ErrNotFound)
		return
	}
	a := items[i]
	if !in.parse(w, &a) {
		return
	}
	a.UpdatedAt = p.now().UTC().Truncate(time.Second)
	items[i] = a
	if err := p.save(ctx, items); err != nil {
		ledger.Fail(w, err)
		return
	}
	ledger.WriteJSON(w, http.StatusOK, view{Announcement: a, State: a.state(p.now())})
}

func (p *Plugin) delete(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx := r.Context()
	items, err := p.load(ctx)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	n := len(items)
	items = slices.DeleteFunc(items, func(a Announcement) bool { return a.ID == idOf(r) })
	if len(items) == n {
		ledger.Fail(w, ledger.ErrNotFound)
		return
	}
	if err := p.save(ctx, items); err != nil {
		ledger.Fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *Plugin) dismissed(ctx context.Context, uid string) ([]string, error) {
	b, ok, err := p.kv.Get(ctx, "dismissed:"+uid)
	if err != nil || !ok {
		return nil, err
	}
	var ids []string
	return ids, json.Unmarshal(b, &ids)
}

// getMine lists the live announcements for the caller, newest first,
// without the ones they dismissed.
func (p *Plugin) getMine(w http.ResponseWriter, r *http.Request) {
	uid, role := ledger.Who(r)
	if !p.enabled() {
		ledger.WriteJSON(w, http.StatusOK, map[string]any{"items": []Announcement{}})
		return
	}
	ctx := r.Context()
	items, err := p.load(ctx)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	gone, err := p.dismissed(ctx, uid)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	now := p.now()
	out := []Announcement{}
	for _, a := range items {
		if a.live(now) && a.shownTo(role) && !slices.Contains(gone, a.ID) {
			a.CreatedBy = ""
			out = append(out, a)
		}
	}
	ledger.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (p *Plugin) postDismiss(w http.ResponseWriter, r *http.Request) {
	uid, _ := ledger.Who(r)
	if uid == "" {
		ledger.WriteError(w, http.StatusForbidden, "forbidden", "a console user is required")
		return
	}
	var in struct {
		ID string `json:"id"`
	}
	if !ledger.ReadJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	items, err := p.load(ctx)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	gone, err := p.dismissed(ctx, uid)
	if err != nil {
		ledger.Fail(w, err)
		return
	}
	// Keep only ids that still exist, so the list never outgrows the notices.
	keep := []string{in.ID}
	for _, id := range gone {
		if id != in.ID && slices.ContainsFunc(items, func(a Announcement) bool { return a.ID == id }) {
			keep = append(keep, id)
		}
	}
	b, _ := json.Marshal(keep)
	if err := p.kv.Put(ctx, "dismissed:"+uid, b, 0); err != nil {
		ledger.Fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
