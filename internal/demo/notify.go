package demo

import (
	"context"
	"time"

	"github.com/initxy/plugrelay/internal/store"
)

// Demo notification channels post to the fake upstream's /hooks/ path, so
// the notifications the relay raises while the demo runs are delivered.
type channelSpec struct {
	name, typ string
	enabled   bool
	body      map[string]any
}

var channelSpecs = []channelSpec{
	{"运维群", "feishu", true, map[string]any{"secret": "demo-signing-secret"}},
	{"值班邮箱", "email", false, map[string]any{"smtp_host": "smtp.demo.local", "smtp_port": 587, "tls": "starttls",
		"username": "alerts@demo.local", "password": "demo", "from": "plugrelay <alerts@demo.local>",
		"to": []string{"oncall@demo.local", "ops@demo.local"}, "events": []string{"credential_quarantined", "upstream_paused", "budget_reached"}}},
	{"计费系统", "webhook", true, map[string]any{"secret": "demo-hmac-key", "records": true, "language": "en",
		"events": []string{"budget_reached"}}},
}

func hookURL(fakeURL, typ string) string { return fakeURL + "/hooks/" + typ }

// seedNotifications turns the feature on, adds the channels and a few days
// of notification history.
func seedNotifications(ctx context.Context, st store.Store, c *api, w *world, fakeURL string, now time.Time) error {
	if err := c.v1("PATCH", "/features/notifications", map[string]any{"enabled": true}, nil); err != nil {
		return err
	}
	ids := map[string]string{}
	for _, s := range channelSpecs {
		body := map[string]any{"type": s.typ, "name": s.name, "enabled": s.enabled}
		for k, v := range s.body {
			body[k] = v
		}
		if s.typ != "email" {
			body["url"] = hookURL(fakeURL, s.typ)
		}
		var out struct {
			ID string `json:"id"`
		}
		if err := c.v1("POST", "/notifications/channels", body, &out); err != nil {
			return err
		}
		ids[s.name] = out.ID
	}
	ns, ok := store.NotifyOf(st)
	if !ok {
		return nil
	}
	sent := func(name, typ string, at time.Time, ok bool, attempts int, errText string) store.NotifyDelivery {
		return store.NotifyDelivery{ChannelID: ids[name], ChannelName: name, ChannelType: typ, OK: ok, Attempts: attempts,
			Error: errText, At: at.Add(2 * time.Second)}
	}
	cred := func(up, key, name, upName string, d map[string]string) store.NotifyObject {
		return store.NotifyObject{Type: "credential", ID: w.cred(up, key), Name: name, ParentID: w.upstreams[up], ParentName: upName, Detail: d}
	}
	type entry struct {
		ago        time.Duration
		event      string
		objects    []store.NotifyObject
		deliveries func(at time.Time) []store.NotifyDelivery
	}
	feishu := func(at time.Time) []store.NotifyDelivery {
		return []store.NotifyDelivery{sent("运维群", "feishu", at, true, 1, "")}
	}
	entries := []entry{
		{3*24*time.Hour + 2*time.Hour, "test", []store.NotifyObject{{Type: "channel", ID: ids["运维群"], Name: "运维群"}}, feishu},
		{2*24*time.Hour + 7*time.Hour, "credential_quota_low", []store.NotifyObject{cred("openai", "team-a", "Project A", "OpenAI",
			map[string]string{"percent": "8"})}, feishu},
		{26 * time.Hour, "upstream_paused", []store.NotifyObject{{Type: "upstream", ID: w.upstreams["bedrock"], Name: "Bedrock us-east-1"}},
			func(at time.Time) []store.NotifyDelivery {
				return []store.NotifyDelivery{sent("运维群", "feishu", at, true, 1, ""),
					sent("值班邮箱", "email", at, false, 3, "dial tcp: lookup smtp.demo.local: no such host")}
			}},
		{25 * time.Hour, "upstream_recovered", []store.NotifyObject{{Type: "upstream", ID: w.upstreams["bedrock"], Name: "Bedrock us-east-1"}}, feishu},
		{9 * time.Hour, "budget_reached", []store.NotifyObject{
			{Type: "key", ID: w.keys[1].id, Name: w.keys[1].spec.name, Detail: map[string]string{"percent": "82", "window": "monthly", "kind": "spend"}},
			{Type: "group", ID: w.groups["vip"], Name: "vip", Detail: map[string]string{"percent": "80", "window": "monthly", "kind": "spend"}},
		}, func(at time.Time) []store.NotifyDelivery {
			return []store.NotifyDelivery{sent("运维群", "feishu", at, true, 1, ""), sent("计费系统", "webhook", at, true, 2, "")}
		}},
		{3 * time.Hour, "credential_cooling", []store.NotifyObject{
			cred("anthropic", "backup", "Backup key", "Anthropic", map[string]string{"reason": "rate_limit",
				"until": now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)}),
			cred("openai", "team-a", "Project A", "OpenAI", map[string]string{"reason": "quota",
				"until": now.Add(20 * time.Hour).UTC().Format(time.RFC3339)}),
		}, feishu},
	}
	for _, e := range entries {
		at := now.Add(-e.ago).UTC().Truncate(time.Millisecond)
		n := &store.Notification{CreatedAt: at, Event: e.event, Status: store.NotificationDone, Objects: e.objects,
			Deliveries: e.deliveries(at)}
		if err := ns.AddNotification(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

// pointChannels moves the demo channels to this run's fake upstream.
func pointChannels(c *api, fakeURL string) error {
	var list struct {
		Items []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"items"`
	}
	if err := c.v1("GET", "/notifications/channels", nil, &list); err != nil {
		return err
	}
	for _, ch := range list.Items {
		if ch.Type == "email" {
			continue
		}
		if err := c.v1("PATCH", "/notifications/channels/"+ch.ID, map[string]any{"url": hookURL(fakeURL, ch.Type)}, nil); err != nil {
			return err
		}
	}
	return nil
}
