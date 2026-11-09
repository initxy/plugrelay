package admin

import (
	"net/mail"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/plugins/job/notify"
	"github.com/initxy/plugrelay/plugins/sink/webhook"
)

// Notification channels and the notification log (the notifications
// feature). Channel secrets (bot and webhook addresses, the SMTP password,
// signing secrets) are sealed in the store and never returned.

// NotifyTest is the outcome of sending a test message to a channel.
type NotifyTest struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	Attempts  int    `json:"attempts"`
	LatencyMS int64  `json:"latency_ms"`
}

// Passed reports whether the message went out.
func (t NotifyTest) Passed() bool { return t.OK }

var notifyTypes = []string{store.NotifyEmail, store.NotifyFeishu, store.NotifyDingTalk, store.NotifyWeCom, store.NotifyWebhook}

var notifyTLS = []string{"starttls", "tls", "none"}

func (h *Handler) notifyStore() (store.Notify, error) {
	n, ok := store.NotifyOf(h.d.Store)
	if !ok {
		return nil, errNotFound(eNoSuchEndpoint)
	}
	return n, nil
}

type notifyChannelView struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	Enabled      bool      `json:"enabled"`
	Events       []string  `json:"events"`
	Language     string    `json:"language"`
	URL          string    `json:"url,omitempty"` // masked
	HasSecret    bool      `json:"has_secret"`
	HasPassword  bool      `json:"has_password"`
	SMTPHost     string    `json:"smtp_host,omitempty"`
	SMTPPort     int       `json:"smtp_port,omitempty"`
	TLS          string    `json:"tls,omitempty"`
	Username     string    `json:"username,omitempty"`
	From         string    `json:"from,omitempty"`
	To           []string  `json:"to"`
	Records      bool      `json:"records"`
	RecordFields []string  `json:"record_fields"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func notifyChannelViewOf(c *store.NotifyChannel) notifyChannelView {
	v := notifyChannelView{ID: c.ID, Name: c.Name, Type: c.Type, Enabled: c.Enabled, Events: c.Events, Language: c.Language,
		URL: maskURL(c.Secrets.URL), HasSecret: c.Secrets.Secret != "", HasPassword: c.Secrets.Password != "",
		SMTPHost: c.Config.SMTPHost, SMTPPort: c.Config.SMTPPort, TLS: c.Config.TLS, Username: c.Config.Username,
		From: c.Config.From, To: c.Config.To, Records: c.Config.Records, RecordFields: c.Config.RecordFields,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	if v.To == nil {
		v.To = []string{}
	}
	if v.RecordFields == nil {
		v.RecordFields = []string{}
	}
	return v
}

// maskURL keeps a bot address recognisable without its token: scheme,
// host and the last four characters.
func maskURL(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "••••"
	}
	tail := s[max(len(s)-4, 0):]
	return u.Scheme + "://" + u.Host + "/••••" + tail
}

type notifyChannelInput struct {
	Name         *string   `json:"name"`
	Type         string    `json:"type"`
	Enabled      *bool     `json:"enabled"`
	Events       *[]string `json:"events"`
	Language     *string   `json:"language"`
	URL          *string   `json:"url"`
	Secret       *string   `json:"secret"`
	SMTPHost     *string   `json:"smtp_host"`
	SMTPPort     *int      `json:"smtp_port"`
	TLS          *string   `json:"tls"`
	Username     *string   `json:"username"`
	Password     *string   `json:"password"`
	From         *string   `json:"from"`
	To           *[]string `json:"to"`
	Records      *bool     `json:"records"`
	RecordFields *[]string `json:"record_fields"`
}

func setIfGiven[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}

func trimAll(in []string) []string {
	out := []string{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// apply merges in over c and validates the result. A secret left out (or
// sent empty, for the address and the password) keeps the stored one; an
// empty signing secret clears it.
func (in notifyChannelInput) apply(c *store.NotifyChannel) error {
	setIfGiven(&c.Name, in.Name)
	c.Name = strings.TrimSpace(c.Name)
	setIfGiven(&c.Enabled, in.Enabled)
	setIfGiven(&c.Events, in.Events)
	setIfGiven(&c.Language, in.Language)
	cfg := &c.Config
	setIfGiven(&cfg.SMTPHost, in.SMTPHost)
	cfg.SMTPHost = strings.TrimSpace(cfg.SMTPHost)
	setIfGiven(&cfg.SMTPPort, in.SMTPPort)
	setIfGiven(&cfg.TLS, in.TLS)
	setIfGiven(&cfg.Username, in.Username)
	setIfGiven(&cfg.From, in.From)
	cfg.From = strings.TrimSpace(cfg.From)
	setIfGiven(&cfg.To, in.To)
	cfg.To = trimAll(cfg.To)
	setIfGiven(&cfg.Records, in.Records)
	setIfGiven(&cfg.RecordFields, in.RecordFields)
	if in.URL != nil && strings.TrimSpace(*in.URL) != "" {
		c.Secrets.URL = strings.TrimSpace(*in.URL)
	}
	if in.Password != nil && *in.Password != "" {
		c.Secrets.Password = *in.Password
	}
	setIfGiven(&c.Secrets.Secret, in.Secret)
	if c.Language == "" {
		c.Language = "zh-CN"
	}

	var bad invalid
	if c.Name == "" || len([]rune(c.Name)) > 64 {
		bad.add("name", eLengthRange.with("min", 1, "max", 64))
	}
	if !slices.Contains(notifyTypes, c.Type) {
		bad.add("type", oneOf(notifyTypes...))
	}
	if c.Language != "zh-CN" && c.Language != "en" {
		bad.add("language", oneOf("zh-CN", "en"))
	}
	for i, e := range c.Events {
		if !slices.Contains(notify.Events, e) {
			bad.add("events."+strconv.Itoa(i), oneOf(notify.Events...))
		}
	}
	if c.Type == store.NotifyEmail {
		if c.Config.SMTPHost == "" {
			bad.add("smtp_host", eRequired)
		}
		if c.Config.SMTPPort < 0 || c.Config.SMTPPort > 65535 {
			bad.add("smtp_port", eNumberRange.with("min", 0, "max", 65535))
		}
		if c.Config.TLS == "" {
			c.Config.TLS = "starttls"
		}
		if !slices.Contains(notifyTLS, c.Config.TLS) {
			bad.add("tls", oneOf(notifyTLS...))
		}
		if _, err := mail.ParseAddress(c.Config.From); err != nil {
			bad.add("from", eEmailAddress)
		}
		if len(c.Config.To) == 0 {
			bad.add("to", eNotEmpty)
		}
		for i, to := range c.Config.To {
			if _, err := mail.ParseAddress(to); err != nil {
				bad.add("to."+strconv.Itoa(i), eEmailAddress)
			}
		}
		c.Secrets.URL, c.Secrets.Secret = "", ""
	} else {
		if u, err := url.Parse(c.Secrets.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			bad.add("url", eURLHTTP)
		}
		c.Config = store.NotifyConfig{Records: c.Config.Records, RecordFields: c.Config.RecordFields}
		c.Secrets.Password = ""
	}
	if c.Type == store.NotifyWeCom {
		c.Secrets.Secret = ""
	}
	if c.Type != store.NotifyWebhook {
		c.Config.Records, c.Config.RecordFields = false, nil
	}
	fields := append(slices.Clone(webhook.DefaultFields), "body")
	for i, f := range c.Config.RecordFields {
		if !slices.Contains(fields, f) {
			bad.add("record_fields."+strconv.Itoa(i), oneOf(fields...))
		}
	}
	return bad.err()
}

func (h *Handler) listNotifyChannels(c *call) error {
	ns, err := h.notifyStore()
	if err != nil {
		return err
	}
	all, err := ns.ListNotifyChannels(c.ctx)
	if err != nil {
		return storeErr(err, objNotifyChannel)
	}
	items := make([]notifyChannelView, 0, len(all))
	for i := range all {
		items = append(items, notifyChannelViewOf(&all[i]))
	}
	return c.json(200, map[string]any{"items": items, "events": notify.Events,
		"record_fields": webhook.DefaultFields, "public_url_set": PublicURL(c.ctx, h.d.Store) != ""})
}

func (h *Handler) createNotifyChannel(c *call) error {
	ns, err := h.notifyStore()
	if err != nil {
		return err
	}
	var in notifyChannelInput
	if err := c.decode(&in); err != nil {
		return err
	}
	ch := &store.NotifyChannel{Type: in.Type, Enabled: true, Events: []string{}}
	if err := in.apply(ch); err != nil {
		return err
	}
	if err := ns.PutNotifyChannel(c.ctx, ch); err != nil {
		return storeErr(err, objNotifyChannel)
	}
	c.audit("notify.channel.create", "notify_channel", ch.ID, ch.Name, []change{{Field: "type", To: ch.Type}})
	return c.json(201, notifyChannelViewOf(ch))
}

func (h *Handler) updateNotifyChannel(c *call) error {
	ns, err := h.notifyStore()
	if err != nil {
		return err
	}
	ch, err := ns.GetNotifyChannel(c.ctx, c.r.PathValue("id"))
	if err != nil {
		return storeErr(err, objNotifyChannel)
	}
	old := *ch
	var in notifyChannelInput
	if err := c.decode(&in); err != nil {
		return err
	}
	if in.Type != "" && in.Type != ch.Type {
		return fieldInvalid("type", eImmutable)
	}
	if err := in.apply(ch); err != nil {
		return err
	}
	if err := ns.PutNotifyChannel(c.ctx, ch); err != nil {
		return storeErr(err, objNotifyChannel)
	}
	var changes []change
	if old.Enabled != ch.Enabled {
		changes = append(changes, change{Field: "enabled", From: old.Enabled, To: ch.Enabled})
	}
	if old.Name != ch.Name {
		changes = append(changes, change{Field: "name", From: old.Name, To: ch.Name})
	}
	if old.Secrets != ch.Secrets {
		changes = append(changes, change{Field: "secrets", To: "(changed)"})
	}
	c.audit("notify.channel.update", "notify_channel", ch.ID, ch.Name, changes)
	return c.json(200, notifyChannelViewOf(ch))
}

func (h *Handler) deleteNotifyChannel(c *call) error {
	ns, err := h.notifyStore()
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	if err := ns.DeleteNotifyChannel(c.ctx, id); err != nil {
		return storeErr(err, objNotifyChannel)
	}
	c.audit("notify.channel.delete", "notify_channel", id, "", nil)
	return c.noContent()
}

func (h *Handler) testNotifyChannel(c *call) error {
	ns, err := h.notifyStore()
	if err != nil {
		return err
	}
	ch, err := ns.GetNotifyChannel(c.ctx, c.r.PathValue("id"))
	if err != nil {
		return storeErr(err, objNotifyChannel)
	}
	res, on := h.d.Runtime.TestNotifyChannel(c.ctx, ch)
	if !on {
		return errConflict(eNotifyOff)
	}
	return c.json(200, testOutcome([]NotifyTest{res}))
}

func (h *Handler) listNotifications(c *call) error {
	ns, err := h.notifyStore()
	if err != nil {
		return err
	}
	limit := 100
	if s := c.r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > store.NotificationsKept {
			return fieldInvalid("limit", eNumberRange.with("min", 1, "max", store.NotificationsKept))
		}
		limit = n
	}
	items, err := ns.ListNotifications(c.ctx, limit)
	if err != nil {
		return err
	}
	if items == nil {
		items = []store.Notification{}
	}
	return c.json(200, map[string]any{"items": items})
}
