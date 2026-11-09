package app

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/initxy/plugrelay/internal/admin"
	"github.com/initxy/plugrelay/internal/router"
	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/job/notify"
)

// The notifications feature's view of the core: what is wrong right now,
// the channels, the log, and the egress its senders use.
var (
	_ notify.BackendHost = (*host)(nil)
	_ plugin.NoticeHost  = (*host)(nil)
)

func (h *host) NotifyBackend() notify.Backend { return notifyBackend{h.a} }

// Notify hands a notice to the notifier while the feature runs.
func (h *host) Notify(ctx context.Context, n plugin.Notice) { h.a.notice(ctx, n) }

func (a *App) notifier() *notify.Notifier {
	if j, ok := a.runtime().job(notify.ID).(*notify.Notifier); ok {
		return j
	}
	return nil
}

func (a *App) notice(ctx context.Context, n plugin.Notice) {
	if nt := a.notifier(); nt != nil {
		nt.Push(ctx, n)
	}
}

// onExtensionCrash is the extension supervisor's crash callback.
func (a *App) onExtensionCrash(id string, err error) {
	d := map[string]string{}
	if err != nil {
		d["error"] = err.Error()
	}
	a.notice(context.Background(), plugin.Notice{Event: notify.EventCrashed,
		Object: plugin.NoticeObject{Type: "extension", ID: id}, Detail: d})
}

func (r runtimeAPI) TestNotifyChannel(ctx context.Context, c *store.NotifyChannel) (admin.NotifyTest, bool) {
	nt := r.a.notifier()
	if nt == nil {
		return admin.NotifyTest{}, false
	}
	start := time.Now()
	d := nt.Test(ctx, channelOf(c))
	return admin.NotifyTest{OK: d.OK, Error: d.Error, Attempts: d.Attempts, LatencyMS: time.Since(start).Milliseconds()}, true
}

type notifyBackend struct{ a *App }

var _ notify.Backend = notifyBackend{}

func (b notifyBackend) store() (store.Notify, error) {
	n, ok := store.NotifyOf(b.a.store)
	if !ok {
		return nil, fmt.Errorf("notify: the store keeps no notifications")
	}
	return n, nil
}

// Conditions reads the router's live state, shared by every node.
func (b notifyBackend) Conditions(ctx context.Context) ([]notify.Condition, error) {
	rt := b.a.runtime()
	ups := map[string]*store.Upstream{}
	for i := range rt.store.Upstreams {
		ups[rt.store.Upstreams[i].ID] = &rt.store.Upstreams[i]
	}
	creds := map[string]*store.Credential{}
	for i := range rt.store.Credentials {
		creds[rt.store.Credentials[i].ID] = &rt.store.Credentials[i]
	}
	now := time.Now()
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	var out []notify.Condition
	for _, ul := range b.a.router.Live(ctx).Upstreams {
		u := ups[ul.ID]
		if !ul.Enabled || u == nil {
			continue
		}
		upName := admin.UpstreamName(u)
		if ul.Breaker != router.BreakerClosed {
			out = append(out, notify.Condition{Event: notify.EventPaused, Object: notify.Object{Type: "upstream", ID: ul.ID, Name: upName}})
		}
		for _, cl := range ul.Credentials {
			name := cl.ID
			if c := creds[cl.ID]; c != nil {
				name = admin.CredentialLabel(c)
			}
			obj := func(d map[string]string) notify.Object {
				return notify.Object{Type: "credential", ID: cl.ID, Name: name, ParentID: ul.ID, ParentName: upName, Detail: d}
			}
			switch cl.Status {
			case router.StatusQuarantined, router.StatusManual:
				out = append(out, notify.Condition{Event: notify.EventQuarantined, Object: obj(map[string]string{"reason": cl.Reason})})
			case router.StatusCooling:
				since, reason := cl.Since, cl.Reason
				if since.IsZero() {
					since = now
				}
				if reason == router.ReasonQuotaProbe {
					reason = string(plugin.CauseQuota)
				}
				out = append(out, notify.Condition{Event: notify.EventCooling, Object: obj(map[string]string{
					"reason": reason, "since": stamp(since), "until": stamp(cl.Until)})})
			}
			if q := cl.Quota; q != nil && q.Remaining >= 0 && q.Err == "" {
				out = append(out, notify.Condition{Event: notify.EventQuotaLow, Object: obj(map[string]string{
					"percent": strconv.FormatFloat(q.Remaining*100, 'f', 0, 64), "reset": stamp(q.ResetAt)})})
			}
			if cl.Attention != "" {
				out = append(out, notify.Condition{Event: notify.EventSecretUnsaved, Object: obj(nil)})
			}
		}
	}
	return out, nil
}

func channelOf(c *store.NotifyChannel) notify.Channel {
	return notify.Channel{ID: c.ID, Name: c.Name, Type: c.Type, Enabled: c.Enabled, Events: c.Events, Language: c.Language,
		SMTPHost: c.Config.SMTPHost, SMTPPort: c.Config.SMTPPort, TLS: c.Config.TLS, Username: c.Config.Username,
		Password: c.Secrets.Password, From: c.Config.From, To: c.Config.To, URL: c.Secrets.URL, Secret: c.Secrets.Secret,
		Records: c.Config.Records, RecordFields: c.Config.RecordFields}
}

func (b notifyBackend) Channels(ctx context.Context) ([]notify.Channel, error) {
	ns, err := b.store()
	if err != nil {
		return nil, err
	}
	all, err := ns.ListNotifyChannels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]notify.Channel, len(all))
	for i := range all {
		out[i] = channelOf(&all[i])
	}
	return out, nil
}

func toStoreEntry(e *notify.Entry) *store.Notification {
	n := &store.Notification{ID: e.ID, CreatedAt: e.At, Event: e.Event, Status: store.NotificationPending,
		Objects: make([]store.NotifyObject, len(e.Objects)), Deliveries: make([]store.NotifyDelivery, len(e.Deliveries))}
	if e.Done {
		n.Status = store.NotificationDone
	}
	for i, o := range e.Objects {
		n.Objects[i] = store.NotifyObject(o)
	}
	for i, d := range e.Deliveries {
		n.Deliveries[i] = store.NotifyDelivery(d)
	}
	return n
}

func (b notifyBackend) Add(ctx context.Context, e *notify.Entry) error {
	ns, err := b.store()
	if err != nil {
		return err
	}
	n := toStoreEntry(e)
	if err := ns.AddNotification(ctx, n); err != nil {
		return err
	}
	e.ID, e.At = n.ID, n.CreatedAt
	return nil
}

func (b notifyBackend) Pending(ctx context.Context) ([]notify.Entry, error) {
	ns, err := b.store()
	if err != nil {
		return nil, err
	}
	rows, err := ns.PendingNotifications(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]notify.Entry, len(rows))
	for i, n := range rows {
		e := notify.Entry{ID: n.ID, At: n.CreatedAt, Event: n.Event, Objects: make([]notify.Object, len(n.Objects))}
		for j, o := range n.Objects {
			e.Objects[j] = notify.Object(o)
		}
		out[i] = e
	}
	return out, nil
}

func (b notifyBackend) Finish(ctx context.Context, e *notify.Entry) error {
	ns, err := b.store()
	if err != nil {
		return err
	}
	return ns.FinishNotification(ctx, toStoreEntry(e))
}

func (b notifyBackend) Drop(ctx context.Context, id string) error {
	ns, err := b.store()
	if err != nil {
		return err
	}
	return ns.DeleteNotification(ctx, id)
}

// Describe names the subject of a notice for people.
func (b notifyBackend) Describe(ctx context.Context, o plugin.NoticeObject) notify.Object {
	out := notify.Object{Type: o.Type, ID: o.ID}
	st := b.a.store
	switch o.Type {
	case "key":
		if k, err := st.GetKey(ctx, o.ID); err == nil {
			out.Name = k.Name
		}
	case "user":
		if u, err := st.GetUser(ctx, o.ID); err == nil {
			out.Name = u.Name
			if out.Name == "" {
				out.Name = u.Email
			}
		}
	case "group":
		if g, err := st.GetGroup(ctx, o.ID); err == nil {
			out.Name = g.Name
		}
	case "extension":
		if b.a.ext != nil {
			if s, ok := b.a.ext.Status(o.ID); ok && s.Name != "" {
				out.Name = s.Name
			}
		}
	}
	return out
}

func (b notifyBackend) PublicURL(ctx context.Context) string { return admin.PublicURL(ctx, b.a.store) }

func (b notifyBackend) Timezone(ctx context.Context) *time.Location {
	return admin.Timezone(ctx, b.a.store)
}

func (b notifyBackend) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return b.a.egress.Dial(ctx, network, addr)
}
