package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/clientid"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// testClients are the clients a key test can speak as (the connect guide's).
var testClients = []string{"claude-code", "codex", "gemini-cli", "cursor", "cline", "opencode", "curl"}

// keySettings is the API form of a key's own settings.
type keySettings struct {
	BudgetMonthly  *money     `json:"budget_monthly,omitempty"`
	BudgetTotal    *money     `json:"budget_total,omitempty"`
	Models         []string   `json:"models,omitempty"`
	Clients        []string   `json:"clients,omitempty"`
	IPAllowlist    []string   `json:"ip_allowlist,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	MaxConcurrency int        `json:"max_concurrency,omitempty"`
	RPM            int        `json:"rpm,omitempty"`
	Exception      bool       `json:"exception,omitempty"`
}

type keyView struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Prefix     string      `json:"prefix"`
	Last4      string      `json:"last4"`
	Owner      *ref        `json:"owner,omitempty"`
	Group      *ref        `json:"group,omitempty"`
	Status     string      `json:"status"`
	CreatedAt  time.Time   `json:"created_at"`
	LastUsedAt *time.Time  `json:"last_used_at"`
	MonthSpend money       `json:"month_spend"`
	Settings   keySettings `json:"settings"`
}

func readKeySettings(k *store.Key) keySettings {
	var s keySettings
	get := func(name string, v any) { _ = json.Unmarshal(k.Settings[name], v) }
	get("budget_monthly", &s.BudgetMonthly)
	get("budget_total", &s.BudgetTotal)
	get("models", &s.Models)
	get("clients", &s.Clients)
	get("ip_allowlist", &s.IPAllowlist)
	get("max_concurrency", &s.MaxConcurrency)
	get("rpm", &s.RPM)
	s.ExpiresAt = timePtr(k.ExpiresAt)
	s.Exception = slices.Contains(k.Exceptions, "*")
	return s
}

// writeKeySettings stores s on k after validating it. The key record is
// the only home of these limits: the modules enforcing them read the key
// layer from it (settings.FromRecord).
func writeKeySettings(k *store.Key, s keySettings, admin bool, bad *invalid) {
	put := func(name string, v any, set bool) {
		if !set {
			delete(k.Settings, name)
			return
		}
		raw, _ := json.Marshal(v)
		k.Settings[name] = raw
	}
	if k.Settings == nil {
		k.Settings = map[string]json.RawMessage{}
	}
	for i, ip := range s.IPAllowlist {
		if _, _, err := net.ParseCIDR(ip); err != nil && net.ParseIP(ip) == nil {
			bad.add(fmt.Sprintf("settings.ip_allowlist.%d", i), eIPOrCIDR)
		}
	}
	for i, cl := range s.Clients {
		if clientid.Name(cl) == "" {
			bad.add(fmt.Sprintf("settings.clients.%d", i), eUnknownClient)
		}
	}
	if s.MaxConcurrency < 0 {
		bad.add("settings.max_concurrency", eNotNegative)
	}
	if s.RPM < 0 {
		bad.add("settings.rpm", eNotNegative)
	}
	if s.BudgetMonthly != nil && s.BudgetMonthly.Micro < 0 {
		bad.add("settings.budget_monthly.micro", eNotNegative)
	}
	if s.BudgetTotal != nil && s.BudgetTotal.Micro < 0 {
		bad.add("settings.budget_total.micro", eNotNegative)
	}
	if s.Exception && !admin {
		bad.add("settings.exception", eAdminOnlyExcept)
	}
	put("budget_monthly", s.BudgetMonthly, s.BudgetMonthly != nil)
	put("budget_total", s.BudgetTotal, s.BudgetTotal != nil)
	put("models", s.Models, len(s.Models) > 0)
	put("clients", s.Clients, len(s.Clients) > 0)
	put("ip_allowlist", s.IPAllowlist, len(s.IPAllowlist) > 0)
	put("max_concurrency", s.MaxConcurrency, s.MaxConcurrency > 0)
	put("rpm", s.RPM, s.RPM > 0)
	k.ExpiresAt = time.Time{}
	if s.ExpiresAt != nil {
		k.ExpiresAt = *s.ExpiresAt
	}
	if admin {
		k.Exceptions = nil
		if s.Exception {
			k.Exceptions = []string{"*"}
		}
	}
}

func (h *Handler) dropKeySettings(ctx context.Context, keyID string) {
	rows, err := h.d.Store.ListSettings(ctx, store.SettingFilter{Scope: plugin.Key, ScopeID: keyID})
	if err != nil {
		return
	}
	for _, r := range rows {
		_ = h.d.Store.DeleteSetting(ctx, plugin.Key, keyID, r.PluginID, r.Key)
	}
}

func (h *Handler) keyView(ctx context.Context, k *store.Key, n *names, cur string) (keyView, error) {
	spent, _, err := h.d.Store.Spent(ctx, "key:"+k.ID, n.month)
	if err != nil {
		return keyView{}, err
	}
	s := readKeySettings(k)
	status := "active"
	switch {
	case k.Disabled:
		status = "disabled"
	case !k.ExpiresAt.IsZero() && !h.d.Now().Before(k.ExpiresAt):
		status = "expired"
	case s.BudgetMonthly != nil && s.BudgetMonthly.Micro > 0 && spent >= s.BudgetMonthly.Micro:
		status = "over_budget"
	}
	v := keyView{ID: k.ID, Name: k.Name, Prefix: secret.KeyPrefix, Last4: k.Last4, Owner: n.user(k.UserID),
		Group: n.group(k.GroupID), Status: status, CreatedAt: k.CreatedAt, LastUsedAt: timePtr(k.LastUsedAt),
		MonthSpend: money{Micro: spent, Currency: cur}, Settings: s}
	return v, nil
}

// ownKey loads a key the caller may manage.
func (h *Handler) ownKey(c *call, id string) (*store.Key, error) {
	k, err := h.d.Store.GetKey(c.ctx, id)
	if err != nil {
		return nil, storeErr(err, objKey)
	}
	if !c.who.isAdmin() && k.UserID != c.who.userID() {
		return nil, errNotFound(eNotFound.with("object", objKey))
	}
	return k, nil
}

func (h *Handler) listKeys(c *call) error {
	f := store.KeyFilter{UserID: c.query("user_id")}
	if !c.who.isAdmin() {
		f.UserID = c.who.userID()
	}
	keys, err := h.d.Store.ListKeys(c.ctx, f)
	if err != nil {
		return err
	}
	n, err := h.names(c.ctx)
	if err != nil {
		return err
	}
	cur := h.currency(c.ctx)
	q, status := c.query("q"), c.query("status")
	items := []keyView{}
	for i := range keys {
		k := &keys[i]
		if q != "" && !containsFold(k.Name, q) && !strings.HasSuffix(k.Last4, q) && k.ID != q {
			continue
		}
		v, err := h.keyView(c.ctx, k, n, cur)
		if err != nil {
			return err
		}
		if status != "" && v.Status != status {
			continue
		}
		items = append(items, v)
	}
	return c.json(200, map[string]any{"items": items})
}

func (h *Handler) getKey(c *call) error {
	k, err := h.ownKey(c, c.r.PathValue("id"))
	if err != nil {
		return err
	}
	n, err := h.names(c.ctx)
	if err != nil {
		return err
	}
	v, err := h.keyView(c.ctx, k, n, h.currency(c.ctx))
	if err != nil {
		return err
	}
	return c.json(200, v)
}

func (h *Handler) createKey(c *call) error {
	var in struct {
		Name     string      `json:"name"`
		UserID   string      `json:"user_id"`
		GroupID  string      `json:"group_id"`
		Settings keySettings `json:"settings"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 64 {
		bad.add("name", eLengthRange.with("min", 1, "max", 64))
	}
	k := &store.Key{Name: in.Name, UserID: c.who.userID()}
	if c.who.isAdmin() {
		if in.UserID != "" {
			k.UserID = in.UserID
		}
		k.GroupID = in.GroupID
	} else if in.UserID != "" && in.UserID != c.who.userID() {
		bad.add("user_id", eOwnKeysOnly)
	}
	if k.UserID != "" {
		u, err := h.d.Store.GetUser(c.ctx, k.UserID)
		if err != nil {
			bad.add("user_id", eNoSuch.with("object", objUser))
		} else if k.GroupID == "" {
			k.GroupID = u.GroupID
		}
	}
	if k.GroupID != "" {
		if _, err := h.d.Store.GetGroup(c.ctx, k.GroupID); err != nil {
			bad.add("group_id", eNoSuch.with("object", objGroup))
		}
	}
	writeKeySettings(k, in.Settings, c.who.isAdmin(), &bad)
	if err := bad.err(); err != nil {
		return err
	}
	plain, err := h.d.Store.CreateKey(c.ctx, k)
	if err != nil {
		return storeErr(err, objKey)
	}
	c.audit("key.create", "key", k.ID, k.Name, nil)
	return h.keyCreated(c, 201, k, plain)
}

func (h *Handler) keyCreated(c *call, status int, k *store.Key, plain string) error {
	n, err := h.names(c.ctx)
	if err != nil {
		return err
	}
	v, err := h.keyView(c.ctx, k, n, h.currency(c.ctx))
	if err != nil {
		return err
	}
	return c.json(status, map[string]any{"key": v, "secret": plain})
}

func (h *Handler) updateKey(c *call) error {
	k, err := h.ownKey(c, c.r.PathValue("id"))
	if err != nil {
		return err
	}
	var in struct {
		Name     *string      `json:"name"`
		Enabled  *bool        `json:"enabled"`
		GroupID  *string      `json:"group_id"`
		Settings *keySettings `json:"settings"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	before := readKeySettings(k)
	var bad invalid
	var changes []change
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" || len(name) > 64 {
			bad.add("name", eLengthRange.with("min", 1, "max", 64))
		}
		changes = append(changes, change{Field: "name", From: k.Name, To: name})
		k.Name = name
	}
	if in.Enabled != nil {
		changes = append(changes, change{Field: "enabled", From: !k.Disabled, To: *in.Enabled})
		k.Disabled = !*in.Enabled
	}
	if in.GroupID != nil {
		if !c.who.isAdmin() {
			bad.add("group_id", eAdminOnlyMove)
		} else if *in.GroupID != "" {
			if _, err := h.d.Store.GetGroup(c.ctx, *in.GroupID); err != nil {
				bad.add("group_id", eNoSuch.with("object", objGroup))
			}
		}
		changes = append(changes, change{Field: "group_id", From: k.GroupID, To: *in.GroupID})
		k.GroupID = *in.GroupID
	}
	if in.Settings != nil {
		if !c.who.isAdmin() {
			in.Settings.Exception = before.Exception
		}
		writeKeySettings(k, *in.Settings, c.who.isAdmin(), &bad)
		changes = append(changes, change{Field: "settings", From: before, To: in.Settings})
	}
	if err := bad.err(); err != nil {
		return err
	}
	if err := h.d.Store.UpdateKey(c.ctx, k); err != nil {
		return storeErr(err, objKey)
	}
	c.audit("key.update", "key", k.ID, k.Name, changes)
	return h.getKey(c)
}

func (h *Handler) deleteKey(c *call) error {
	k, err := h.ownKey(c, c.r.PathValue("id"))
	if err != nil {
		return err
	}
	if err := h.d.Store.DeleteKey(c.ctx, k.ID); err != nil {
		return storeErr(err, objKey)
	}
	h.dropKeySettings(c.ctx, k.ID)
	c.audit("key.delete", "key", k.ID, k.Name, nil)
	return c.noContent()
}

// rotateKey replaces the key with a new one carrying the same settings;
// the old secret stops working at once.
func (h *Handler) rotateKey(c *call) error {
	old, err := h.ownKey(c, c.r.PathValue("id"))
	if err != nil {
		return err
	}
	nk := &store.Key{Name: old.Name, UserID: old.UserID, GroupID: old.GroupID, Settings: old.Settings,
		Exceptions: old.Exceptions, Disabled: old.Disabled, ExpiresAt: old.ExpiresAt}
	plain, err := h.d.Store.CreateKey(c.ctx, nk)
	if err != nil {
		return storeErr(err, objKey)
	}
	rows, err := h.d.Store.ListSettings(c.ctx, store.SettingFilter{Scope: plugin.Key, ScopeID: old.ID})
	if err != nil {
		return err
	}
	for _, r := range rows {
		r.ScopeID = nk.ID
		if err := h.d.Store.PutSetting(c.ctx, r); err != nil {
			return err
		}
	}
	if err := h.d.Store.DeleteKey(c.ctx, old.ID); err != nil {
		return storeErr(err, objKey)
	}
	h.dropKeySettings(c.ctx, old.ID)
	c.audit("key.rotate", "key", nk.ID, nk.Name, []change{{Field: "id", From: old.ID, To: nk.ID}})
	return h.keyCreated(c, 200, nk, plain)
}

// clientProtocol is the wire protocol a client speaks to the relay.
func clientProtocol(client string) string {
	switch client {
	case "claude-code":
		return "anthropic"
	case "codex":
		return "openai-responses"
	case "gemini-cli":
		return "gemini"
	}
	return "openai-chat"
}

func (h *Handler) testKey(c *call) error {
	k, err := h.ownKey(c, c.r.PathValue("id"))
	if err != nil {
		return err
	}
	var in struct {
		Client string `json:"client"`
		Model  string `json:"model"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if in.Client != "" && !slices.Contains(testClients, in.Client) {
		return fieldInvalid("client", eUnknownClient)
	}
	res := h.d.Runtime.TestKey(c.ctx, k, clientProtocol(in.Client), in.Model)
	return c.json(200, testOutcome([]TestResult{res}))
}
