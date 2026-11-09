package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/initxy/plugrelay/internal/store"
)

func TestNotifyChannels(t *testing.T) {
	e := newEnv(t, sqliteStore, false)
	c := e.setup()
	const base = "/api/v1/notifications/channels"

	code, out := c.do("POST", base, map[string]any{"type": "email", "name": "Ops", "from": "nope", "to": []string{}})
	if code != 422 || !strings.Contains(fieldCodes(out), "from:email_address") || !strings.Contains(fieldCodes(out), "to:not_empty") ||
		!strings.Contains(fieldCodes(out), "smtp_host:required") {
		t.Fatalf("%d %v", code, out)
	}
	code, out = c.do("POST", base, map[string]any{"type": "feishu", "name": "Bot", "url": "ftp://x", "events": []string{"nope"}})
	if code != 422 || !strings.Contains(fieldCodes(out), "url:url_http") || !strings.Contains(fieldCodes(out), "events.0:one_of") {
		t.Fatalf("%d %v", code, out)
	}

	bot := c.must(201, "POST", base, map[string]any{"type": "feishu", "name": "Bot", "secret": "sig",
		"url": "https://open.feishu.cn/open-apis/bot/v2/hook/0123456789abcdef", "events": []string{"upstream_paused"}})
	id := bot["id"].(string)
	if bot["url"] != "https://open.feishu.cn/••••cdef" || bot["has_secret"] != true || bot["language"] != "zh-CN" || bot["enabled"] != true {
		t.Fatalf("%v", bot)
	}
	// Leaving the address out keeps it; an empty secret removes it.
	upd := c.must(200, "PATCH", base+"/"+id, map[string]any{"name": "Bot 2", "secret": ""})
	if upd["has_secret"] != false || upd["url"] != bot["url"] {
		t.Fatalf("%v", upd)
	}
	ns, _ := store.NotifyOf(e.st)
	stored, _ := ns.GetNotifyChannel(context.Background(), id)
	if stored.Secrets.URL != "https://open.feishu.cn/open-apis/bot/v2/hook/0123456789abcdef" || stored.Name != "Bot 2" {
		t.Fatalf("%+v", stored)
	}
	if code, out := c.do("PATCH", base+"/"+id, map[string]any{"type": "email"}); code != 422 || !strings.Contains(fieldCodes(out), "type:immutable") {
		t.Fatalf("%d %v", code, out)
	}

	list := c.must(200, "GET", base, nil)
	if items := list["items"].([]any); len(items) != 1 || list["public_url_set"] != false || len(list["events"].([]any)) != 8 {
		t.Fatalf("%v", list)
	}

	test := c.must(200, "POST", base+"/"+id+"/test", nil)
	if test["ok"] != true {
		t.Fatalf("%v", test)
	}
	e.rt.notifyOff = true
	if code, out := c.do("POST", base+"/"+id+"/test", nil); code != 409 || errCode(out) != "notifications_off" {
		t.Fatalf("%d %v", code, out)
	}

	hook := c.must(201, "POST", base, map[string]any{"type": "webhook", "name": "Records", "url": "https://hooks.example.com/x",
		"records": true, "record_fields": []string{"id", "body"}, "language": "en"})
	if hook["records"] != true || len(hook["record_fields"].([]any)) != 2 {
		t.Fatalf("%v", hook)
	}
	if code, out := c.do("POST", base, map[string]any{"type": "webhook", "name": "x", "url": "https://h.example", "record_fields": []string{"secret"}}); code != 422 ||
		!strings.Contains(fieldCodes(out), "record_fields.0:one_of") {
		t.Fatalf("%d %v", code, out)
	}

	if err := ns.AddNotification(context.Background(), &store.Notification{Event: "upstream_paused",
		Objects: []store.NotifyObject{{Type: "upstream", ID: "u"}}}); err != nil {
		t.Fatal(err)
	}
	log := c.must(200, "GET", "/api/v1/notifications?limit=10", nil)
	if items := log["items"].([]any); len(items) != 1 {
		t.Fatalf("%v", log)
	}
	if code, _ := c.do("GET", "/api/v1/notifications?limit=0", nil); code != 422 {
		t.Fatal(code)
	}
	c.must(204, "DELETE", base+"/"+id, nil)
	if code, out := c.do("DELETE", base+"/"+id, nil); code != 404 || errCode(out) != "not_found" {
		t.Fatalf("%d %v", code, out)
	}
}

// fieldCodes lists a 422's fields as "path:code" pairs.
func fieldCodes(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	fs, _ := e["fields"].([]any)
	var parts []string
	for _, f := range fs {
		m, _ := f.(map[string]any)
		p, _ := m["path"].(string)
		code, _ := m["code"].(string)
		parts = append(parts, p+":"+code)
	}
	return strings.Join(parts, " ")
}
