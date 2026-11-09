package admin

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/store"
)

// change is one changed field in an audit entry.
type change struct {
	Field string `json:"field"`
	From  any    `json:"from,omitempty"`
	To    any    `json:"to,omitempty"`
}

// auditDiff is what store.Audit.Diff holds.
type auditDiff struct {
	ActorName  string   `json:"actor_name,omitempty"`
	TargetName string   `json:"target_name,omitempty"`
	IP         string   `json:"ip,omitempty"`
	Changes    []change `json:"changes,omitempty"`
}

// audit records a mutation. Failures are logged, never returned: the
// change itself already happened.
func (c *call) audit(action, targetType, targetID, targetName string, changes []change) {
	actorID, actorName := c.who.actor()
	diff, _ := json.Marshal(auditDiff{ActorName: actorName, TargetName: targetName, IP: c.h.d.ClientIP(c.r), Changes: changes})
	a := &store.Audit{Time: c.h.d.Now(), Actor: actorID, Action: action, Target: targetType + ":" + targetID, Diff: diff}
	if err := c.h.d.Store.AddAudit(c.ctx, a); err != nil {
		c.h.d.Logger.Error("audit write failed", "action", action, "err", err)
	}
}

// AuditPasswordReset records a password reset done by the CLI on the
// database directly, so the console's audit log shows it.
func AuditPasswordReset(ctx context.Context, st store.Store, u *store.User, passwordLoginEnabled bool) error {
	changes := []change{{Field: "password", To: "********"}, {Field: "method", To: "cli"}}
	if passwordLoginEnabled {
		changes = append(changes, change{Field: "password_login", From: false, To: true})
	}
	diff, err := json.Marshal(auditDiff{ActorName: "command line", TargetName: displayName(u), Changes: changes})
	if err != nil {
		return err
	}
	return st.AddAudit(ctx, &store.Audit{Time: time.Now(), Actor: "cli", Action: "user.update", Target: "user:" + u.ID, Diff: diff})
}

type auditTarget struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type auditEntry struct {
	ID      string      `json:"id"`
	Time    time.Time   `json:"time"`
	Actor   ref         `json:"actor"`
	Action  string      `json:"action"`
	Target  auditTarget `json:"target"`
	IP      string      `json:"ip,omitempty"`
	Changes []change    `json:"changes,omitempty"`
}

func (h *Handler) listAudit(c *call) error {
	limit := c.intQuery("limit", 100, 1, 500)
	q := store.AuditQuery{Actor: c.query("actor_id"), Limit: limit}
	action, ttype := auditActionFilter(c.query("action")), c.query("target_type")
	if id := c.query("target_id"); id != "" {
		if ttype == "" {
			return fieldInvalid("target_id", eNeedsTargetType)
		}
		q.Target = ttype + ":" + id
	}
	if cur := c.query("cursor"); cur != "" {
		ms, err := strconv.ParseInt(cur, 10, 64)
		if err != nil {
			return fieldInvalid("cursor", eCursor)
		}
		q.Before = time.UnixMilli(ms)
	}
	var items []auditEntry
	var last time.Time
	// Action and target type are filtered here; keep reading pages until
	// one is full or the log ends.
	for len(items) < limit {
		page, err := h.d.Store.ListAudit(c.ctx, q)
		if err != nil {
			return err
		}
		for _, a := range page {
			last = a.Time
			e := toAuditEntry(a)
			if action(e.Action) && (ttype == "" || e.Target.Type == ttype) {
				items = append(items, e)
				if len(items) == limit {
					break
				}
			}
		}
		if len(page) < q.Limit || len(items) == limit {
			if len(page) < q.Limit && len(items) < limit {
				last = time.Time{}
			}
			break
		}
		q.Before = last
	}
	var next *string
	if !last.IsZero() && len(items) == limit {
		s := strconv.FormatInt(last.UnixMilli(), 10)
		next = &s
	}
	if items == nil {
		items = []auditEntry{}
	}
	return c.json(200, map[string]any{"items": items, "next_cursor": next})
}

// auditActionFilter matches an action against a comma-separated list of
// actions, where "key.*" stands for every action starting with "key.".
func auditActionFilter(spec string) func(string) bool {
	if spec == "" {
		return func(string) bool { return true }
	}
	parts := strings.Split(spec, ",")
	return func(a string) bool {
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if prefix, ok := strings.CutSuffix(p, "*"); ok && strings.HasPrefix(a, prefix) || p == a {
				return true
			}
		}
		return false
	}
}

func toAuditEntry(a store.Audit) auditEntry {
	var d auditDiff
	_ = json.Unmarshal(a.Diff, &d)
	typ, id, _ := strings.Cut(a.Target, ":")
	name := d.ActorName
	if name == "" {
		name = a.Actor
	}
	return auditEntry{ID: a.ID, Time: a.Time, Actor: ref{ID: a.Actor, Name: name}, Action: a.Action,
		Target: auditTarget{Type: typ, ID: id, Name: d.TargetName}, IP: d.IP, Changes: d.Changes}
}
