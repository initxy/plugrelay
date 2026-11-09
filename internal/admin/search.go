package admin

import (
	"strings"

	"github.com/initxy/plugrelay/internal/features"
	"github.com/initxy/plugrelay/internal/store"
)

type searchHit struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	score    int
}

// score ranks a match: exact id, prefix, then substring.
func score(q string, fields ...string) int {
	best := 0
	for i, f := range fields {
		lf := strings.ToLower(f)
		s := 0
		switch {
		case lf == q:
			s = 100
		case strings.HasPrefix(lf, q):
			s = 60
		case strings.Contains(lf, q):
			s = 30
		}
		if s > 0 && i == 0 {
			s += 5
		}
		best = max(best, s)
	}
	return best
}

func (h *Handler) search(c *call) error {
	q := strings.ToLower(strings.TrimSpace(c.query("q")))
	if q == "" {
		return fieldInvalid("q", eNotEmpty)
	}
	limit := c.intQuery("limit", 20, 1, 50)
	admin := c.who.isAdmin()
	var hits []searchHit
	add := func(typ, id, title, sub string, s int) {
		if s > 0 {
			hits = append(hits, searchHit{Type: typ, ID: id, Title: title, Subtitle: sub, score: s})
		}
	}
	f := store.KeyFilter{}
	if !admin {
		f.UserID = c.who.userID()
	}
	keys, err := h.d.Store.ListKeys(c.ctx, f)
	if err != nil {
		return err
	}
	for _, k := range keys {
		add("key", k.ID, k.Name, "pr_…"+k.Last4, score(q, k.Name, k.ID, k.Last4))
	}
	ms, err := h.d.Store.ListModels(c.ctx)
	if err != nil {
		return err
	}
	allowed := h.callerModelFilter(c)
	for _, m := range ms {
		if admin || (m.Enabled && allowed(m.Name)) {
			add("model", m.Name, m.Name, m.Info.DisplayName, score(q, m.Name, m.Info.DisplayName))
		}
	}
	if admin {
		ups, err := h.d.Store.ListUpstreams(c.ctx)
		if err != nil {
			return err
		}
		for i := range ups {
			u := &ups[i]
			name, _ := u.Config[cfgName].(string)
			add("upstream", u.ID, UpstreamName(u), u.Provider, score(q, u.ID, name, u.Provider))
		}
		users, err := h.d.Store.ListUsers(c.ctx)
		if err != nil {
			return err
		}
		for _, u := range users {
			add("user", u.ID, u.Email, u.Name, score(q, u.Email, u.Name, u.ID))
		}
		for _, f := range features.All() {
			add("feature", f.ID, f.Name.EN, f.Description.EN, score(q, f.ID, f.Name.EN, f.Name.ZH))
		}
		for _, e := range h.d.Runtime.Plugins() {
			if isExtension(&e) {
				add("extension", e.Info.ID, e.Info.ID, e.Info.Description, score(q, e.Info.ID))
			}
		}
	}
	if len(q) >= 4 {
		rq := store.RecordQuery{RequestID: q, Limit: 5}
		if !admin {
			rq.UserID = c.who.userID()
		}
		if page, err := h.d.Store.QueryRecords(c.ctx, rq); err == nil {
			for _, r := range page.Records {
				add("request", r.ID, r.ID, r.Model, 90)
			}
		}
	}
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].score > hits[j-1].score; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	if hits == nil {
		hits = []searchHit{}
	}
	return c.json(200, map[string]any{"items": hits})
}
