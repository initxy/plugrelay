package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
)

func TestNotify(t *testing.T) {
	onEveryDB(t, func(t *testing.T, open opener) {
		ctx := context.Background()
		s := open(t, newBox(t))
		n, ok := store.NotifyOf(s)
		if !ok {
			t.Fatal("store has no notifications")
		}
		c := &store.NotifyChannel{Name: "Ops", Type: store.NotifyFeishu, Enabled: true, Events: []string{"upstream_paused"},
			Language: "zh-CN", Secrets: store.NotifySecrets{URL: "https://open.feishu.cn/hook/abc", Secret: "s1"}}
		must(t, n.PutNotifyChannel(ctx, c))
		if c.ID == "" {
			t.Fatal("no id")
		}
		got, err := n.GetNotifyChannel(ctx, c.ID)
		must(t, err)
		if got.Secrets.URL != c.Secrets.URL || got.Secrets.Secret != "s1" || len(got.Events) != 1 || !got.Enabled {
			t.Fatalf("%+v", got)
		}
		got.Config = store.NotifyConfig{Records: true, RecordFields: []string{"id"}}
		got.Secrets = store.NotifySecrets{}
		must(t, n.PutNotifyChannel(ctx, got))
		all, err := n.ListNotifyChannels(ctx)
		must(t, err)
		if len(all) != 1 || all[0].Secrets.URL != "" || !all[0].Config.Records {
			t.Fatalf("%+v", all)
		}
		must(t, n.DeleteNotifyChannel(ctx, c.ID))
		if _, err := n.GetNotifyChannel(ctx, c.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}

		base := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
		for i := range store.NotificationsKept + 3 {
			must(t, n.AddNotification(ctx, &store.Notification{CreatedAt: base.Add(time.Duration(i) * time.Second),
				Event: fmt.Sprintf("e%d", i), Objects: []store.NotifyObject{{Type: "upstream", ID: "u"}}}))
		}
		list, err := n.ListNotifications(ctx, 0)
		must(t, err)
		if len(list) != store.NotificationsKept || list[0].Event != fmt.Sprintf("e%d", store.NotificationsKept+2) {
			t.Fatalf("%d entries, newest %s", len(list), list[0].Event)
		}
		pending, err := n.PendingNotifications(ctx)
		must(t, err)
		if len(pending) != store.NotificationsKept || pending[0].Event != "e3" {
			t.Fatalf("%d pending, oldest %s", len(pending), pending[0].Event)
		}
		p := pending[0]
		p.Deliveries = []store.NotifyDelivery{{ChannelID: "c", OK: true, Attempts: 1, At: time.Now()}}
		must(t, n.FinishNotification(ctx, &p))
		must(t, n.DeleteNotification(ctx, pending[1].ID))
		pending, err = n.PendingNotifications(ctx)
		must(t, err)
		if len(pending) != store.NotificationsKept-2 {
			t.Fatal(len(pending))
		}
		one, err := n.ListNotifications(ctx, 600)
		must(t, err)
		for _, e := range one {
			if e.ID == p.ID && (e.Status != store.NotificationDone || len(e.Deliveries) != 1 || !e.Deliveries[0].OK) {
				t.Fatalf("%+v", e)
			}
		}
	})
}
