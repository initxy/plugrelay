package store

import (
	"context"
	"encoding/json"
	"time"
)

// Notification channel types.
const (
	NotifyEmail    = "email"
	NotifyFeishu   = "feishu"
	NotifyDingTalk = "dingtalk"
	NotifyWeCom    = "wecom"
	NotifyWebhook  = "webhook"
)

// NotifyChannel is one place notifications go: a mailbox list, a chat bot
// or a webhook.
type NotifyChannel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
	// Events the channel receives; empty means every event.
	Events []string `json:"events"`
	// Language of the messages: "zh-CN" or "en".
	Language string       `json:"language"`
	Config   NotifyConfig `json:"config"`
	// Secrets are sealed under the master key at rest and never returned
	// by the admin API.
	Secrets   NotifySecrets `json:"-"`
	CreatedAt time.Time     `json:"created_at,omitzero"`
	UpdatedAt time.Time     `json:"updated_at,omitzero"`
}

// NotifyConfig holds a channel's settings that are not secret; each type
// uses its own fields.
type NotifyConfig struct {
	SMTPHost string   `json:"smtp_host,omitempty"`
	SMTPPort int      `json:"smtp_port,omitempty"`
	TLS      string   `json:"tls,omitempty"` // "tls", "starttls" or "none"
	Username string   `json:"username,omitempty"`
	From     string   `json:"from,omitempty"`
	To       []string `json:"to,omitempty"`
	// Records also posts finished request records (webhook only).
	Records bool `json:"records,omitempty"`
	// RecordFields are the record fields posted; empty = all but bodies.
	RecordFields []string `json:"record_fields,omitempty"`
}

// NotifySecrets are a channel's secret values: the bot or webhook address
// (it carries the bot's token), the SMTP password and the signing secret.
type NotifySecrets struct {
	URL      string `json:"url,omitempty"`
	Password string `json:"password,omitempty"`
	Secret   string `json:"secret,omitempty"`
}

// Notification states.
const (
	NotificationPending = "pending"
	NotificationDone    = "done"
)

// Notification is one message about one event, possibly about several
// objects at once, with its delivery to each channel.
type Notification struct {
	ID         string           `json:"id"`
	CreatedAt  time.Time        `json:"created_at"`
	Event      string           `json:"event"`
	Status     string           `json:"status"`
	Objects    []NotifyObject   `json:"objects"`
	Deliveries []NotifyDelivery `json:"deliveries"`
}

// NotifyObject is what a notification is about.
type NotifyObject struct {
	Type string `json:"type"` // credential, upstream, key, user, group, extension, channel
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Parent is the upstream of a credential.
	ParentID   string            `json:"parent_id,omitempty"`
	ParentName string            `json:"parent_name,omitempty"`
	Detail     map[string]string `json:"detail,omitempty"`
}

// NotifyDelivery is the outcome of sending a notification to one channel.
type NotifyDelivery struct {
	ChannelID   string    `json:"channel_id"`
	ChannelName string    `json:"channel_name"`
	ChannelType string    `json:"channel_type"`
	OK          bool      `json:"ok"`
	Error       string    `json:"error,omitempty"`
	Attempts    int       `json:"attempts"`
	At          time.Time `json:"at"`
}

// NotificationsKept is how many notifications the log keeps.
const NotificationsKept = 500

// Notify stores notification channels and the notification log. The SQL
// store implements it; get it with NotifyOf.
type Notify interface {
	ListNotifyChannels(ctx context.Context) ([]NotifyChannel, error)
	GetNotifyChannel(ctx context.Context, id string) (*NotifyChannel, error)
	// PutNotifyChannel inserts (empty ID: one is assigned) or replaces a
	// channel, secrets included.
	PutNotifyChannel(ctx context.Context, c *NotifyChannel) error
	DeleteNotifyChannel(ctx context.Context, id string) error

	// AddNotification appends to the log (ID and CreatedAt are assigned
	// when empty) and drops entries past NotificationsKept.
	AddNotification(ctx context.Context, n *Notification) error
	// ListNotifications returns up to limit entries, newest first.
	ListNotifications(ctx context.Context, limit int) ([]Notification, error)
	// PendingNotifications returns undelivered entries, oldest first.
	PendingNotifications(ctx context.Context) ([]Notification, error)
	// FinishNotification stores the deliveries and marks the entry done.
	FinishNotification(ctx context.Context, n *Notification) error
	DeleteNotification(ctx context.Context, id string) error
}

// NotifyOf returns the store's notification storage.
func NotifyOf(s Store) (Notify, bool) {
	n, ok := s.(Notify)
	return n, ok
}

var _ Notify = (*db)(nil)

func notifyAD(id string) string { return "notify_channel:" + id }

const notifyChannelCols = `id, name, type, enabled, events, language, config, secrets, created_at, updated_at`

func (s *db) scanNotifyChannel(sc interface{ Scan(...any) error }) (*NotifyChannel, error) {
	var c NotifyChannel
	var events, config, sealed string
	var created, updated int64
	if err := sc.Scan(&c.ID, &c.Name, &c.Type, &c.Enabled, &events, &c.Language, &config, &sealed, &created, &updated); err != nil {
		return nil, err
	}
	c.CreatedAt, c.UpdatedAt = fromMs(created), fromMs(updated)
	if err := unmarshalOpt(events, &c.Events); err != nil {
		return nil, err
	}
	if err := unmarshalOpt(config, &c.Config); err != nil {
		return nil, err
	}
	if sealed != "" {
		if s.box == nil {
			return nil, ErrNoBox
		}
		pt, err := s.box.Open(sealed, notifyAD(c.ID))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(pt, &c.Secrets); err != nil {
			return nil, err
		}
	}
	if c.Events == nil {
		c.Events = []string{}
	}
	return &c, nil
}

func unmarshalOpt(s string, v any) error {
	if s == "" {
		return nil
	}
	return json.Unmarshal([]byte(s), v)
}

func (s *db) ListNotifyChannels(ctx context.Context) ([]NotifyChannel, error) {
	return list(ctx, s.r(), "list notify channels", `SELECT `+notifyChannelCols+` FROM notify_channels ORDER BY created_at, id`,
		s.scanNotifyChannel)
}

func (s *db) GetNotifyChannel(ctx context.Context, id string) (*NotifyChannel, error) {
	c, err := s.scanNotifyChannel(s.r().row(ctx, `SELECT `+notifyChannelCols+` FROM notify_channels WHERE id = ?`, id))
	return c, wrap("get notify channel", err)
}

func (s *db) PutNotifyChannel(ctx context.Context, c *NotifyChannel) error {
	now := time.Now().UTC().Truncate(time.Millisecond)
	if c.ID == "" {
		c.ID = newID()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	if c.Events == nil {
		c.Events = []string{}
	}
	sealed := ""
	if c.Secrets != (NotifySecrets{}) {
		if s.box == nil {
			return ErrNoBox
		}
		pt, err := json.Marshal(c.Secrets)
		if err != nil {
			return err
		}
		if sealed, err = s.box.Seal(pt, notifyAD(c.ID)); err != nil {
			return err
		}
	}
	events, err := json.Marshal(c.Events)
	if err != nil {
		return err
	}
	config, err := json.Marshal(c.Config)
	if err != nil {
		return err
	}
	_, err = s.r().exec(ctx, `INSERT INTO notify_channels (`+notifyChannelCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET name = excluded.name, type = excluded.type, enabled = excluded.enabled,
  events = excluded.events, language = excluded.language, config = excluded.config, secrets = excluded.secrets,
  updated_at = excluded.updated_at`,
		c.ID, c.Name, c.Type, c.Enabled, string(events), c.Language, string(config), sealed, ms(c.CreatedAt), ms(c.UpdatedAt))
	return wrap("put notify channel", err)
}

func (s *db) DeleteNotifyChannel(ctx context.Context, id string) error {
	res, err := s.r().exec(ctx, `DELETE FROM notify_channels WHERE id = ?`, id)
	return mustAffect("delete notify channel", res, err)
}

const notificationCols = `id, created_at, event, status, objects, deliveries`

func scanNotification(sc interface{ Scan(...any) error }) (*Notification, error) {
	var n Notification
	var created int64
	var objects, deliveries string
	if err := sc.Scan(&n.ID, &created, &n.Event, &n.Status, &objects, &deliveries); err != nil {
		return nil, err
	}
	n.CreatedAt = fromMs(created)
	if err := unmarshalOpt(objects, &n.Objects); err != nil {
		return nil, err
	}
	if err := unmarshalOpt(deliveries, &n.Deliveries); err != nil {
		return nil, err
	}
	if n.Objects == nil {
		n.Objects = []NotifyObject{}
	}
	if n.Deliveries == nil {
		n.Deliveries = []NotifyDelivery{}
	}
	return &n, nil
}

func (s *db) AddNotification(ctx context.Context, n *Notification) error {
	if n.ID == "" {
		n.ID = newID()
	}
	if n.CreatedAt.IsZero() {
		n.CreatedAt = time.Now().UTC().Truncate(time.Millisecond)
	}
	if n.Status == "" {
		n.Status = NotificationPending
	}
	objects, err := json.Marshal(n.Objects)
	if err != nil {
		return err
	}
	deliveries, err := json.Marshal(n.Deliveries)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(r run) error {
		if _, err := r.exec(ctx, `INSERT INTO notifications (`+notificationCols+`) VALUES (?, ?, ?, ?, ?, ?)`,
			n.ID, ms(n.CreatedAt), n.Event, n.Status, string(objects), string(deliveries)); err != nil {
			return wrap("add notification", err)
		}
		// Keep the newest NotificationsKept: find the creation time of the
		// first entry past them and drop everything older.
		var cut int64
		err := r.row(ctx, `SELECT created_at FROM notifications ORDER BY created_at DESC, id DESC LIMIT 1 OFFSET ?`, NotificationsKept).Scan(&cut)
		if isNotFound(err) {
			return nil
		}
		if err != nil {
			return wrap("add notification", err)
		}
		_, err = r.exec(ctx, `DELETE FROM notifications WHERE created_at <= ?`, cut)
		return wrap("add notification", err)
	})
}

func (s *db) ListNotifications(ctx context.Context, limit int) ([]Notification, error) {
	if limit <= 0 || limit > NotificationsKept {
		limit = NotificationsKept
	}
	return list(ctx, s.r(), "list notifications", `SELECT `+notificationCols+` FROM notifications ORDER BY created_at DESC, id DESC LIMIT ?`,
		scanNotification, limit)
}

func (s *db) PendingNotifications(ctx context.Context) ([]Notification, error) {
	return list(ctx, s.r(), "pending notifications", `SELECT `+notificationCols+` FROM notifications WHERE status = ? ORDER BY created_at, id`,
		scanNotification, NotificationPending)
}

func (s *db) FinishNotification(ctx context.Context, n *Notification) error {
	deliveries, err := json.Marshal(n.Deliveries)
	if err != nil {
		return err
	}
	objects, err := json.Marshal(n.Objects)
	if err != nil {
		return err
	}
	n.Status = NotificationDone
	res, err := s.r().exec(ctx, `UPDATE notifications SET status = ?, deliveries = ?, objects = ? WHERE id = ?`,
		n.Status, string(deliveries), string(objects), n.ID)
	return mustAffect("finish notification", res, err)
}

func (s *db) DeleteNotification(ctx context.Context, id string) error {
	_, err := s.r().exec(ctx, `DELETE FROM notifications WHERE id = ?`, id)
	return wrap("delete notification", err)
}
