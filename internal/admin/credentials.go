package admin

import (
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/egress"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/store"
)

// Credential meta keys owned by the console.
const (
	metaLabel = "label"
	metaHint  = "hint"
)

type credentialState struct {
	Status     string           `json:"status"`
	Reason     string           `json:"reason,omitempty"`
	Until      *time.Time       `json:"until"`
	ModelScope string           `json:"model_scope,omitempty"`
	InFlight   int              `json:"in_flight"`
	Quota      *credentialQuota `json:"quota,omitempty"`
	Attention  *attention       `json:"attention,omitempty"`
}

// credentialQuota is the last upstream quota probe of a credential.
type credentialQuota struct {
	Remaining float64    `json:"remaining"`
	Left      *float64   `json:"left,omitempty"`
	Limit     *float64   `json:"limit,omitempty"`
	ResetAt   *time.Time `json:"reset_at,omitempty"`
	CheckedAt time.Time  `json:"checked_at"`
	Error     string     `json:"error,omitempty"`
}

// attention is why an operator should look at a credential that is still
// in rotation.
type attention struct {
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

func quotaView(q *router.QuotaReading) *credentialQuota {
	if q == nil {
		return nil
	}
	v := &credentialQuota{Remaining: q.Remaining, ResetAt: timePtr(q.ResetAt), CheckedAt: q.At, Error: q.Err}
	if q.Limit > 0 {
		left, limit := q.Left, q.Limit
		v.Left, v.Limit = &left, &limit
	}
	return v
}

type credentialView struct {
	ID             string          `json:"id"`
	UpstreamID     string          `json:"upstream_id"`
	Label          string          `json:"label"`
	Kind           string          `json:"kind"`
	Owner          *ref            `json:"owner,omitempty"`
	SecretHint     string          `json:"secret_hint"`
	Proxy          string          `json:"proxy,omitempty"`
	MaxConcurrency int             `json:"max_concurrency"`
	Enabled        bool            `json:"enabled"`
	ExpiresAt      *time.Time      `json:"expires_at"`
	State          credentialState `json:"state"`
}

// liveState reads a credential's state from the router's live view (hard
// rule 25); the stored status only fills in before the router has seen
// the credential.
func liveState(c *store.Credential, ul router.UpstreamLive) credentialState {
	st := credentialState{Status: "ok"}
	var cl *router.CredLive
	for i := range ul.Credentials {
		if ul.Credentials[i].ID == c.ID {
			cl = &ul.Credentials[i]
		}
	}
	status := router.CredStatus(c.Status)
	reason, until := c.StatusReason, c.StatusUntil
	if cl != nil {
		status, reason, until = cl.Status, cl.Reason, cl.Until
		st.InFlight = cl.InFlight
		st.Quota = quotaView(cl.Quota)
		if cl.Attention != "" {
			st.Attention = &attention{Reason: cl.Attention, Since: cl.AttentionSince}
		}
	}
	switch status {
	case router.StatusCooling:
		st.Status = "cooldown"
	case router.StatusRefreshing:
		st.Status = "refreshing"
	case router.StatusQuarantined:
		st.Status = "quarantined"
	case router.StatusManual:
		st.Status = "manual"
	case router.StatusDisabled:
		st.Status = "disabled"
	}
	if st.Status != "ok" {
		st.Reason, st.Until = reason, timePtr(until)
		return st
	}
	if ul.Breaker == router.BreakerOpen {
		st.Status, st.Until, st.Reason = "circuit_open", timePtr(ul.BreakerOpen), "upstream circuit breaker is open"
		return st
	}
	if cl != nil && len(cl.ModelCooldowns) > 0 {
		mc := cl.ModelCooldowns[0]
		st.Status, st.ModelScope, st.Reason, st.Until = "cooldown", mc.Model, mc.Reason, timePtr(mc.Until)
	}
	return st
}

// CredentialLabel is the name the console shows for a credential.
func CredentialLabel(c *store.Credential) string {
	if s := c.Meta[metaLabel]; s != "" {
		return s
	}
	return c.ID
}

func credentialViewOf(c *store.Credential, ul router.UpstreamLive, n *names) credentialView {
	v := credentialView{ID: c.ID, UpstreamID: c.UpstreamID, Label: CredentialLabel(c), Kind: string(c.Kind),
		SecretHint: c.Meta[metaHint], Proxy: c.Proxy, MaxConcurrency: c.MaxConcurrency,
		Enabled: c.Status != store.CredDisabled, ExpiresAt: timePtr(c.ExpiresAt), State: liveState(c, ul)}
	if v.SecretHint == "" && c.Sealed != "" {
		v.SecretHint = "••••"
	}
	if c.Kind == store.CredPersonal && n != nil {
		v.Owner = n.user(c.OwnerID)
	}
	return v
}

// secretHint masks a secret for display: its first three and last four
// characters.
func secretHint(s string) string {
	if len(s) <= 8 {
		return "••••"
	}
	return s[:3] + "••••" + s[len(s)-4:]
}

func (h *Handler) liveOf(c *call, upstreamID string) router.UpstreamLive {
	for _, ul := range h.d.Runtime.Live(c.ctx).Upstreams {
		if ul.ID == upstreamID {
			return ul
		}
	}
	return router.UpstreamLive{}
}

func (h *Handler) writeCredential(c *call, status int, id string) error {
	cr, err := h.d.Store.GetCredential(c.ctx, id)
	if err != nil {
		return storeErr(err, objCredential)
	}
	n, err := h.names(c.ctx)
	if err != nil {
		return err
	}
	return c.json(status, credentialViewOf(cr, h.liveOf(c, cr.UpstreamID), n))
}

func (h *Handler) listCredentials(c *call) error {
	id := c.r.PathValue("id")
	if _, err := h.d.Store.GetUpstream(c.ctx, id); err != nil {
		return storeErr(err, objUpstream)
	}
	creds, err := h.d.Store.ListCredentials(c.ctx, id)
	if err != nil {
		return err
	}
	n, err := h.names(c.ctx)
	if err != nil {
		return err
	}
	ul := h.liveOf(c, id)
	items := []credentialView{}
	for i := range creds {
		items = append(items, credentialViewOf(&creds[i], ul, n))
	}
	return c.json(200, map[string]any{"items": items})
}

func validKind(k string) bool {
	return k == string(store.CredShared) || k == string(store.CredPersonal) || k == string(store.CredService)
}

func (h *Handler) createCredential(c *call) error {
	upID := c.r.PathValue("id")
	if _, err := h.d.Store.GetUpstream(c.ctx, upID); err != nil {
		return storeErr(err, objUpstream)
	}
	var in struct {
		Label          string `json:"label"`
		Kind           string `json:"kind"`
		OwnerID        string `json:"owner_id"`
		Secret         string `json:"secret"`
		Proxy          string `json:"proxy"`
		MaxConcurrency int    `json:"max_concurrency"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	if strings.TrimSpace(in.Label) == "" {
		bad.add("label", eNotEmpty)
	}
	if !validKind(in.Kind) {
		bad.add("kind", oneOf("shared", "personal", "service"))
	}
	if in.Kind == string(store.CredPersonal) {
		if in.OwnerID == "" {
			bad.add("owner_id", ePersonalOwner)
		} else if _, err := h.d.Store.GetUser(c.ctx, in.OwnerID); err != nil {
			bad.add("owner_id", eNoSuch.with("object", objUser))
		}
	}
	if in.MaxConcurrency < 0 {
		bad.add("max_concurrency", eNotNegative)
	}
	if egress.CheckProxy(in.Proxy) != nil {
		bad.add("proxy", eURLProxy)
	}
	if err := bad.err(); err != nil {
		return err
	}
	cr := &store.Credential{UpstreamID: upID, Kind: store.CredentialKind(in.Kind), OwnerID: in.OwnerID,
		Secret: in.Secret, Proxy: in.Proxy, MaxConcurrency: in.MaxConcurrency, Status: store.CredActive,
		Meta: map[string]string{metaLabel: strings.TrimSpace(in.Label)}}
	if in.Secret != "" {
		cr.Meta[metaHint] = secretHint(in.Secret)
	}
	if err := h.d.Store.PutCredential(c.ctx, cr); err != nil {
		return storeErr(err, objCredential)
	}
	c.audit("credential.create", "credential", cr.ID, cr.Meta[metaLabel], []change{{Field: "upstream_id", To: upID}})
	return h.writeCredential(c, 201, cr.ID)
}

func (h *Handler) getCredential(c *call) error {
	return h.writeCredential(c, 200, c.r.PathValue("id"))
}

func (h *Handler) updateCredential(c *call) error {
	cr, err := h.d.Store.GetCredential(c.ctx, c.r.PathValue("id"))
	if err != nil {
		return storeErr(err, objCredential)
	}
	var in struct {
		Label          *string `json:"label"`
		Secret         *string `json:"secret"`
		Proxy          *string `json:"proxy"`
		MaxConcurrency *int    `json:"max_concurrency"`
		Enabled        *bool   `json:"enabled"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	var bad invalid
	var changes []change
	if cr.Meta == nil {
		cr.Meta = map[string]string{}
	}
	if in.Label != nil {
		if strings.TrimSpace(*in.Label) == "" {
			bad.add("label", eNotEmpty)
		}
		changes = append(changes, change{Field: "label", From: cr.Meta[metaLabel], To: *in.Label})
		cr.Meta[metaLabel] = strings.TrimSpace(*in.Label)
	}
	if in.Secret != nil && *in.Secret != "" {
		cr.Secret = *in.Secret
		cr.Meta[metaHint] = secretHint(*in.Secret)
		changes = append(changes, change{Field: "secret", To: "********"})
	}
	if in.Proxy != nil {
		if egress.CheckProxy(*in.Proxy) != nil {
			bad.add("proxy", eURLProxy)
		}
		changes = append(changes, change{Field: "proxy", From: cr.Proxy, To: *in.Proxy})
		cr.Proxy = *in.Proxy
	}
	if in.MaxConcurrency != nil {
		if *in.MaxConcurrency < 0 {
			bad.add("max_concurrency", eNotNegative)
		}
		changes = append(changes, change{Field: "max_concurrency", From: cr.MaxConcurrency, To: *in.MaxConcurrency})
		cr.MaxConcurrency = *in.MaxConcurrency
	}
	if err := bad.err(); err != nil {
		return err
	}
	if in.Enabled != nil {
		changes = append(changes, change{Field: "enabled", From: cr.Status != store.CredDisabled, To: *in.Enabled})
		if *in.Enabled && cr.Status == store.CredDisabled {
			cr.Status, cr.StatusReason, cr.StatusUntil = store.CredActive, "", time.Time{}
		} else if !*in.Enabled {
			cr.Status, cr.StatusReason = store.CredDisabled, "disabled by an admin"
		}
	}
	if err := h.d.Store.PutCredential(c.ctx, cr); err != nil {
		return storeErr(err, objCredential)
	}
	c.audit("credential.update", "credential", cr.ID, cr.Meta[metaLabel], changes)
	return h.writeCredential(c, 200, cr.ID)
}

func (h *Handler) deleteCredential(c *call) error {
	cr, err := h.d.Store.GetCredential(c.ctx, c.r.PathValue("id"))
	if err != nil {
		return storeErr(err, objCredential)
	}
	if err := h.d.Store.DeleteCredential(c.ctx, cr.ID); err != nil {
		return storeErr(err, objCredential)
	}
	c.audit("credential.delete", "credential", cr.ID, cr.Meta[metaLabel], nil)
	return c.noContent()
}

func (h *Handler) resetCredential(c *call) error {
	cr, err := h.d.Store.GetCredential(c.ctx, c.r.PathValue("id"))
	if err != nil {
		return storeErr(err, objCredential)
	}
	if err := h.d.Runtime.RestoreCredential(c.ctx, cr.UpstreamID, cr.ID); err != nil {
		return err
	}
	if cr.Status != store.CredDisabled && cr.Status != store.CredActive {
		if err := h.d.Store.SetCredentialStatus(c.ctx, cr.ID, store.CredActive, "", time.Time{}); err != nil {
			return storeErr(err, objCredential)
		}
	}
	c.audit("credential.reset", "credential", cr.ID, cr.Meta[metaLabel], []change{{Field: "status", From: cr.Status, To: "active"}})
	return h.writeCredential(c, 200, cr.ID)
}
