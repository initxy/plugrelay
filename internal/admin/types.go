package admin

import (
	"context"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/match"
)

// ref points at another object with a display name.
type ref struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// money is an integer amount of micro-units of a currency.
type money struct {
	Micro    int64  `json:"micro"`
	Currency string `json:"currency"`
}

// names resolves user and group ids to display names within one request.
type names struct {
	users  map[string]*store.User
	groups map[string]string
	// month is the start of this month in the site time zone.
	month time.Time
}

func (h *Handler) names(ctx context.Context) (*names, error) {
	n := &names{users: map[string]*store.User{}, groups: map[string]string{},
		month: monthStart(h.d.Now(), Timezone(ctx, h.d.Store))}
	us, err := h.d.Store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range us {
		n.users[us[i].ID] = &us[i]
	}
	gs, err := h.d.Store.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range gs {
		n.groups[g.ID] = g.Name
	}
	return n, nil
}

func (n *names) user(id string) *ref {
	if id == "" {
		return nil
	}
	if u, ok := n.users[id]; ok {
		return &ref{ID: id, Name: displayName(u)}
	}
	return &ref{ID: id, Name: id}
}

func (n *names) group(id string) *ref {
	if id == "" {
		return nil
	}
	if g, ok := n.groups[id]; ok {
		return &ref{ID: id, Name: g}
	}
	return &ref{ID: id, Name: id}
}

func monthStart(now time.Time, loc *time.Location) time.Time {
	y, m, _ := now.In(loc).Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, loc)
}

func dayStart(now time.Time, loc *time.Location) time.Time {
	y, m, d := now.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// globMatch reports whether a group's model list allows name, as routing
// does (match.Any); an empty list allows everything.
func globMatch(name string, patterns []string) bool {
	return len(patterns) == 0 || match.Any(name, patterns)
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
