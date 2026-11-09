package admin

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

type userView struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	DisplayName string     `json:"display_name,omitempty"`
	Email       string     `json:"email,omitempty"`
	Role        store.Role `json:"role"`
	Group       *ref       `json:"group,omitempty"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	LastSeenAt  *time.Time `json:"last_seen_at"`
	KeyCount    int        `json:"key_count"`
	MonthSpend  money      `json:"month_spend"`
	Balance     *money     `json:"balance,omitempty"`
	MFAEnabled  bool       `json:"mfa_enabled"`
}

// mfaUsers is who has a second factor; empty when the store has none.
func (h *Handler) mfaUsers(ctx context.Context) (map[string]bool, error) {
	if m, ok := store.MFAOf(h.d.Store); ok {
		return m.MFAUsers(ctx)
	}
	return map[string]bool{}, nil
}

// The store's login column ("email") holds the username; it is reported
// as the email too when it looks like one.
func (h *Handler) userView(ctx context.Context, u *store.User, n *names, cur string, b *userBalances) (userView, error) {
	keys, err := h.d.Store.ListKeys(ctx, store.KeyFilter{UserID: u.ID})
	if err != nil {
		return userView{}, err
	}
	spent, _, err := h.d.Store.Spent(ctx, "user:"+u.ID, n.month)
	if err != nil {
		return userView{}, err
	}
	v := userView{ID: u.ID, Username: u.Email, DisplayName: u.Name, Role: u.Role, Group: n.group(u.GroupID),
		Status: "active", CreatedAt: u.CreatedAt, KeyCount: len(keys), MonthSpend: money{Micro: spent, Currency: cur},
		Balance: b.of(u)}
	if strings.Contains(u.Email, "@") {
		v.Email = u.Email
	}
	if u.Disabled {
		v.Status = "disabled"
	}
	var last time.Time
	for _, k := range keys {
		if k.LastUsedAt.After(last) {
			last = k.LastUsedAt
		}
	}
	v.LastSeenAt = timePtr(last)
	return v, nil
}

func (h *Handler) listUsers(c *call) error {
	users, err := h.d.Store.ListUsers(c.ctx)
	if err != nil {
		return err
	}
	n, err := h.names(c.ctx)
	if err != nil {
		return err
	}
	cur := h.currency(c.ctx)
	b, err := h.loadUserBalances(c.ctx, cur)
	if err != nil {
		return err
	}
	mfa, err := h.mfaUsers(c.ctx)
	if err != nil {
		return err
	}
	q, gid := c.query("q"), c.query("group_id")
	items := []userView{}
	for i := range users {
		u := &users[i]
		if gid != "" && u.GroupID != gid {
			continue
		}
		if q != "" && !containsFold(u.Email, q) && !containsFold(u.Name, q) && u.ID != q {
			continue
		}
		v, err := h.userView(c.ctx, u, n, cur, b)
		if err != nil {
			return err
		}
		v.MFAEnabled = mfa[u.ID]
		items = append(items, v)
	}
	return c.json(200, map[string]any{"items": items})
}

func (h *Handler) writeUser(c *call, status int, id string) error {
	u, err := h.d.Store.GetUser(c.ctx, id)
	if err != nil {
		return storeErr(err, objUser)
	}
	n, err := h.names(c.ctx)
	if err != nil {
		return err
	}
	cur := h.currency(c.ctx)
	b, err := h.loadUserBalances(c.ctx, cur)
	if err != nil {
		return err
	}
	v, err := h.userView(c.ctx, u, n, cur, b)
	if err != nil {
		return err
	}
	mfa, err := h.mfaUsers(c.ctx)
	if err != nil {
		return err
	}
	v.MFAEnabled = mfa[u.ID]
	return c.json(status, v)
}

func (h *Handler) getUser(c *call) error { return h.writeUser(c, 200, c.r.PathValue("id")) }

func validRole(r store.Role) bool {
	return r == store.RoleAdmin || r == store.RoleMember || r == store.RoleUser
}

func (h *Handler) createUser(c *call) error {
	var in struct {
		Username    string     `json:"username"`
		DisplayName string     `json:"display_name"`
		Email       string     `json:"email"`
		Password    string     `json:"password"`
		Role        store.Role `json:"role"`
		GroupID     string     `json:"group_id"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" {
		bad.add("username", eNotEmpty)
	}
	// The email is the login name (username); a separate one is not stored,
	// so refuse it instead of dropping it silently.
	if in.Email != "" && !strings.EqualFold(strings.TrimSpace(in.Email), strings.TrimSpace(in.Username)) {
		bad.add("email", eEmailIsLogin)
	}
	if in.Password != "" && len(in.Password) < minPassword {
		bad.add("password", eMinLength.with("min", minPassword))
	}
	if !validRole(in.Role) {
		bad.add("role", oneOf("admin", "member", "user"))
	}
	if in.GroupID != "" {
		if _, err := h.d.Store.GetGroup(c.ctx, in.GroupID); err != nil {
			bad.add("group_id", eNoSuch.with("object", objGroup))
		}
	}
	if err := bad.err(); err != nil {
		return err
	}
	if in.Role == store.RoleAdmin {
		if err := h.requireAdminReauth(c); err != nil {
			return err
		}
	}
	u := &store.User{Name: orDefault(in.DisplayName, in.Username), Email: in.Username, Role: in.Role, GroupID: in.GroupID}
	if err := h.d.Store.CreateUser(c.ctx, u, in.Password); err != nil {
		if err := storeErr(err, objUser); err != nil {
			if ae, ok := err.(*apiError); ok && ae.Code == "conflict" {
				return fieldInvalid("username", eAlreadyTaken)
			}
			return err
		}
	}
	c.audit("user.create", "user", u.ID, displayName(u), []change{{Field: "role", To: u.Role}})
	return h.writeUser(c, 201, u.ID)
}

func (h *Handler) updateUser(c *call) error {
	u, err := h.d.Store.GetUser(c.ctx, c.r.PathValue("id"))
	if err != nil {
		return storeErr(err, objUser)
	}
	var in struct {
		DisplayName *string     `json:"display_name"`
		Email       *string     `json:"email"`
		Password    *string     `json:"password"`
		Role        *store.Role `json:"role"`
		GroupID     *string     `json:"group_id"`
		Status      *string     `json:"status"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	before := *u
	var bad invalid
	var changes []change
	if in.DisplayName != nil {
		changes = append(changes, change{Field: "display_name", From: u.Name, To: *in.DisplayName})
		u.Name = *in.DisplayName
	}
	// The email is the login name, which cannot be renamed (identities,
	// audit trails and SSO links refer to it); refuse instead of ignoring.
	if in.Email != nil && !strings.EqualFold(strings.TrimSpace(*in.Email), before.Email) {
		bad.add("email", eLoginImmutable)
	}
	if in.Role != nil {
		if !validRole(*in.Role) {
			bad.add("role", oneOf("admin", "member", "user"))
		}
		changes = append(changes, change{Field: "role", From: u.Role, To: *in.Role})
		u.Role = *in.Role
	}
	if in.GroupID != nil {
		if *in.GroupID != "" {
			if _, err := h.d.Store.GetGroup(c.ctx, *in.GroupID); err != nil {
				bad.add("group_id", eNoSuch.with("object", objGroup))
			}
		}
		changes = append(changes, change{Field: "group_id", From: u.GroupID, To: *in.GroupID})
		u.GroupID = *in.GroupID
	}
	if in.Status != nil {
		if *in.Status != "active" && *in.Status != "disabled" {
			bad.add("status", oneOf("active", "disabled"))
		}
		changes = append(changes, change{Field: "status", To: *in.Status})
		u.Disabled = *in.Status == "disabled"
	}
	if in.Password != nil && len(*in.Password) < minPassword {
		bad.add("password", eMinLength.with("min", minPassword))
	}
	if err := bad.err(); err != nil {
		return err
	}
	// Re-enabling a disabled admin hands out a working admin account, like
	// creating one, so both directions of an admin's status need it.
	if in.Password != nil || (in.Role != nil && *in.Role != before.Role) ||
		(u.Disabled && !before.Disabled) || (!u.Disabled && before.Disabled && before.Role == store.RoleAdmin) {
		if err := h.requireAdminReauth(c); err != nil {
			return err
		}
	}
	if (u.Role != store.RoleAdmin || u.Disabled) && h.isLastAdmin(c.ctx, u.ID) {
		return errConflict(eLastAdminDemote)
	}
	if err := h.d.Store.UpdateUser(c.ctx, u); err != nil {
		return storeErr(err, objUser)
	}
	if in.Password != nil {
		if err := h.d.Store.SetPassword(c.ctx, u.ID, *in.Password); err != nil {
			return storeErr(err, objUser)
		}
		changes = append(changes, change{Field: "password", To: "********"})
	}
	c.audit("user.update", "user", u.ID, displayName(u), changes)
	return h.writeUser(c, 200, u.ID)
}

// isLastAdmin reports whether id is the only enabled admin.
func (h *Handler) isLastAdmin(ctx context.Context, id string) bool {
	users, err := h.d.Store.ListUsers(ctx)
	if err != nil {
		return true
	}
	for _, u := range users {
		if u.ID != id && u.Role == store.RoleAdmin && !u.Disabled {
			return false
		}
	}
	return true
}

func (h *Handler) deleteUser(c *call) error {
	u, err := h.d.Store.GetUser(c.ctx, c.r.PathValue("id"))
	if err != nil {
		return storeErr(err, objUser)
	}
	if u.Role == store.RoleAdmin && h.isLastAdmin(c.ctx, u.ID) {
		return errConflict(eLastAdminDelete)
	}
	if err := h.requireAdminReauth(c); err != nil {
		return err
	}
	// Plugins end what the user has running first (a subscription a
	// payment provider bills); the store ends any subscription left.
	// ?force=true lets the deletion through what they cannot end.
	force := c.r.URL.Query().Get("force") == "true"
	hookCtx := c.ctx
	if force {
		hookCtx = plugin.WithForcedDelete(hookCtx)
	}
	if err := h.d.Runtime.BeforeUserDelete(hookCtx, u.ID); err != nil {
		return errConflict(eUserDelBlocked.with("reason", err.Error()))
	}
	keys, err := h.d.Store.ListKeys(c.ctx, store.KeyFilter{UserID: u.ID})
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := h.d.Store.DeleteKey(c.ctx, k.ID); err != nil {
			return storeErr(err, objKey)
		}
		h.dropKeySettings(c.ctx, k.ID)
	}
	if err := h.d.Store.DeleteUser(c.ctx, u.ID); err != nil {
		return storeErr(err, objUser)
	}
	changes := []change{{Field: "keys_revoked", To: len(keys)}}
	if force {
		changes = append(changes, change{Field: "force", To: true})
	}
	c.audit("user.delete", "user", u.ID, displayName(u), changes)
	return c.noContent()
}

// ---------------------------------------------------------------- groups

type groupView struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Description       string   `json:"description,omitempty"`
	MemberCount       int      `json:"member_count"`
	Models            []string `json:"models"`
	ShowUpstreamNames *bool    `json:"show_upstream_names,omitempty"`
	LogBodies         *bool    `json:"log_bodies,omitempty"`
	PriceMultiplier   float64  `json:"price_multiplier"`
}

type groupInput struct {
	Name              *string   `json:"name"`
	Description       *string   `json:"description"`
	Models            *[]string `json:"models"`
	ShowUpstreamNames *bool     `json:"show_upstream_names"`
	LogBodies         *bool     `json:"log_bodies"`
	PriceMultiplier   *float64  `json:"price_multiplier"`
}

func groupViewOf(g *store.Group, members int) groupView {
	v := groupView{ID: g.ID, Name: g.Name, MemberCount: members, Models: []string{}, PriceMultiplier: g.PriceMultiplier}
	if v.PriceMultiplier <= 0 {
		v.PriceMultiplier = 1
	}
	get := func(k string, dst any) { _ = json.Unmarshal(g.Settings[k], dst) }
	get("description", &v.Description)
	get("models", &v.Models)
	get("show_upstream_names", &v.ShowUpstreamNames)
	get("log_bodies", &v.LogBodies)
	if v.Models == nil {
		v.Models = []string{}
	}
	return v
}

func (h *Handler) memberCounts(ctx context.Context) (map[string]int, error) {
	users, err := h.d.Store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	n := map[string]int{}
	for _, u := range users {
		n[u.GroupID]++
	}
	return n, nil
}

func (h *Handler) listGroups(c *call) error {
	gs, err := h.d.Store.ListGroups(c.ctx)
	if err != nil {
		return err
	}
	counts, err := h.memberCounts(c.ctx)
	if err != nil {
		return err
	}
	items := []groupView{}
	for i := range gs {
		items = append(items, groupViewOf(&gs[i], counts[gs[i].ID]))
	}
	return c.json(200, map[string]any{"items": items})
}

func applyGroupInput(g *store.Group, in groupInput, bad *invalid) {
	if g.Settings == nil {
		g.Settings = map[string]json.RawMessage{}
	}
	set := func(k string, v any) { g.Settings[k], _ = json.Marshal(v) }
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" {
			bad.add("name", eNotEmpty)
		}
		g.Name = strings.TrimSpace(*in.Name)
	}
	if in.Description != nil {
		set("description", *in.Description)
	}
	if in.Models != nil {
		set("models", *in.Models)
	}
	if in.ShowUpstreamNames != nil {
		set("show_upstream_names", *in.ShowUpstreamNames)
	}
	if in.LogBodies != nil {
		set("log_bodies", *in.LogBodies)
	}
	if in.PriceMultiplier != nil {
		if m := *in.PriceMultiplier; m <= 0 || m > 1000 || math.IsNaN(m) {
			bad.add("price_multiplier", eMultiplier)
		}
		g.PriceMultiplier = *in.PriceMultiplier
	}
}

func (h *Handler) createGroup(c *call) error {
	var in groupInput
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	if in.Name == nil {
		bad.add("name", eRequired)
	}
	g := &store.Group{PriceMultiplier: 1}
	applyGroupInput(g, in, &bad)
	if err := bad.err(); err != nil {
		return err
	}
	if err := h.d.Store.PutGroup(c.ctx, g); err != nil {
		return storeErr(err, objGroup)
	}
	c.audit("group.create", "group", g.ID, g.Name, nil)
	return c.json(201, groupViewOf(g, 0))
}

func (h *Handler) updateGroup(c *call) error {
	g, err := h.d.Store.GetGroup(c.ctx, c.r.PathValue("id"))
	if err != nil {
		return storeErr(err, objGroup)
	}
	var in groupInput
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	before := groupViewOf(g, 0)
	applyGroupInput(g, in, &bad)
	if err := bad.err(); err != nil {
		return err
	}
	if err := h.d.Store.PutGroup(c.ctx, g); err != nil {
		return storeErr(err, objGroup)
	}
	counts, err := h.memberCounts(c.ctx)
	if err != nil {
		return err
	}
	after := groupViewOf(g, counts[g.ID])
	c.audit("group.update", "group", g.ID, g.Name, []change{{Field: "group", From: before, To: after}})
	return c.json(200, after)
}

func (h *Handler) deleteGroup(c *call) error {
	id := c.r.PathValue("id")
	g, err := h.d.Store.GetGroup(c.ctx, id)
	if err != nil {
		return storeErr(err, objGroup)
	}
	counts, err := h.memberCounts(c.ctx)
	if err != nil {
		return err
	}
	if counts[id] > 0 {
		return errConflict(eGroupHasMembers)
	}
	if keys, err := h.d.Store.ListKeys(c.ctx, store.KeyFilter{GroupID: id}); err != nil {
		return err
	} else if len(keys) > 0 {
		return errConflict(eGroupHasKeys)
	}
	if err := h.d.Store.DeleteGroup(c.ctx, id); err != nil {
		return storeErr(err, objGroup)
	}
	c.audit("group.delete", "group", id, g.Name, nil)
	return c.noContent()
}
