package notify

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Message is an entry worded for one language.
type Message struct {
	Title string
	Lines []string
	// Link opens the console page of the objects; "" without a public URL.
	Link     string
	LinkText string
}

// shownLines is how many objects a message lists before "and n more".
const shownLines = 10

type words struct {
	one, many string // English singular and plural titles, %d is the count
	zh        string // Chinese title, %d is the count
}

var titles = map[string]words{
	EventQuarantined: {"A credential looks suspended and was taken out of rotation",
		"%d credentials look suspended and were taken out of rotation", "%d 个凭据疑似被封，已停止使用"},
	EventCooling: {"A credential is paused for a while", "%d credentials are paused for a while",
		"%d 个凭据暂停使用较长时间"},
	EventQuotaLow: {"A credential is running low on quota", "%d credentials are running low on quota",
		"%d 个凭据剩余额度不多"},
	EventSecretUnsaved: {"A credential's refreshed secret could not be saved",
		"%d credentials' refreshed secrets could not be saved", "%d 个凭据刷新后的密钥没能保存"},
	EventPaused: {"An upstream keeps failing and was paused", "%d upstreams keep failing and were paused",
		"%d 个上游连续失败，已暂停转发"},
	EventRecovered: {"An upstream works again", "%d upstreams work again", "%d 个上游已恢复"},
	EventBudget:    {"A budget reached its alert level", "%d budgets reached their alert level", "%d 个额度用到了提醒线"},
	EventCrashed: {"An extension crashed and is being restarted", "%d extensions crashed and are being restarted",
		"%d 个扩展意外退出，正在重启"},
}

var reasons = map[string][2]string{
	"rate_limit":   {"rate limited", "触发限流"},
	"quota":        {"quota used up", "额度用完"},
	"auth_expired": {"sign-in expired", "授权过期"},
	"banned":       {"account looks suspended", "账号疑似被封"},
	"unknown":      {"unclear refusal, treated as suspended", "拒绝原因不明，按被封处理"},
	"manual":       {"held for an operator", "等待人工处理"},
}

var windows = map[string][2]string{
	"total":   {"in total", "累计"},
	"daily":   {"per day", "每天"},
	"monthly": {"per month", "每月"},
}

var objectNames = map[string][2]string{
	"key":       {"Key", "密钥"},
	"user":      {"User", "用户"},
	"group":     {"Group", "分组"},
	"extension": {"Extension", "扩展"},
	"upstream":  {"Upstream", "上游"},
}

// Render words an entry for a channel language ("zh-CN" or "en"); base is
// the console's public URL ("" leaves the link out); times are shown in loc.
func Render(e *Entry, lang, base string, loc *time.Location) Message {
	zh := strings.HasPrefix(lang, "zh")
	pick := func(en, z string) string {
		if zh {
			return z
		}
		return en
	}
	var m Message
	n := len(e.Objects)
	switch {
	case e.Event == EventTest:
		name := ""
		if n > 0 {
			name = e.Objects[0].Name
		}
		m.Title = pick(fmt.Sprintf("Test message: channel %q works", name), fmt.Sprintf("测试消息：通知渠道「%s」可以正常收到", name))
		m.Lines = []string{pick("Sent from the plugrelay console.", "这条消息由 plugrelay 控制台发出。")}
	default:
		w, ok := titles[e.Event]
		if !ok {
			w = words{e.Event, e.Event + " (%d)", e.Event + "（%d）"}
		}
		switch {
		case zh:
			m.Title = fmt.Sprintf(w.zh, n)
		case n == 1:
			m.Title = w.one
		default:
			m.Title = fmt.Sprintf(w.many, n)
		}
		for i, o := range e.Objects {
			if i == shownLines {
				m.Lines = append(m.Lines, pick(fmt.Sprintf("… and %d more", n-shownLines), fmt.Sprintf("……还有 %d 个", n-shownLines)))
				break
			}
			m.Lines = append(m.Lines, line(e.Event, o, zh, loc))
		}
	}
	m.Title = "[plugrelay] " + m.Title
	if base != "" {
		m.Link = strings.TrimRight(base, "/") + link(e)
		m.LinkText = pick("Open in the console", "在控制台查看")
	}
	return m
}

func line(event string, o Object, zh bool, loc *time.Location) string {
	pick := func(en, z string) string {
		if zh {
			return z
		}
		return en
	}
	two := func(m map[string][2]string, k string) string {
		v, ok := m[k]
		if !ok {
			return k
		}
		return pick(v[0], v[1])
	}
	name := o.Name
	if name == "" {
		name = o.ID
	}
	switch o.Type {
	case "credential":
		if o.ParentName != "" || o.ParentID != "" {
			name = firstOf(o.ParentName, o.ParentID) + " / " + name
		}
	case "channel":
	default:
		if t, ok := objectNames[o.Type]; ok && o.Type != "upstream" {
			name = pick(t[0]+" ", t[1]) + name
		}
	}
	d := o.Detail
	clock := func(t time.Time) string { return t.In(loc).Format("01-02 15:04 MST") }
	var why []string
	switch event {
	case EventQuarantined, EventCooling:
		if r := d["reason"]; r != "" {
			why = append(why, two(reasons, r))
		}
		if t, err := time.Parse(time.RFC3339, d["until"]); err == nil && event == EventCooling {
			why = append(why, pick("back at "+clock(t), clock(t)+" 恢复"))
		}
	case EventQuotaLow:
		why = append(why, pick(d["percent"]+"% left", "剩 "+d["percent"]+"%"))
		if t, err := time.Parse(time.RFC3339, d["reset"]); err == nil {
			why = append(why, pick("resets "+clock(t), clock(t)+" 重置"))
		}
	case EventBudget:
		kind := pick("spend", "花费")
		if d["kind"] == "token" {
			kind = "token"
		}
		if pct, _ := strconv.Atoi(d["percent"]); pct >= 100 {
			why = append(why, pick(fmt.Sprintf("%s budget %s used up", kind, two(windows, d["window"])),
				fmt.Sprintf("%s%s上限已用完", two(windows, d["window"]), kind)))
		} else {
			why = append(why, pick(fmt.Sprintf("%s%% of the %s budget %s used", d["percent"], kind, two(windows, d["window"])),
				fmt.Sprintf("%s%s上限已用 %s%%", two(windows, d["window"]), kind, d["percent"])))
		}
	case EventCrashed:
		if e := d["error"]; e != "" {
			why = append(why, e)
		}
	}
	if len(why) == 0 {
		return name
	}
	return name + pick(": ", "：") + strings.Join(why, pick(", ", "，"))
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// link is the console path for an entry's objects: the object's page when
// there is one object (or one shared page), else the list page.
func link(e *Entry) string {
	one, list := "", ""
	for i, o := range e.Objects {
		p, l := objectLink(o)
		if i == 0 {
			one, list = p, l
			continue
		}
		if p != one {
			one = ""
		}
		if l != list {
			list = "/notifications"
		}
	}
	switch {
	case one != "":
		return one
	case list != "":
		return list
	}
	return "/notifications"
}

func objectLink(o Object) (page, list string) {
	q := url.QueryEscape
	switch o.Type {
	case "credential":
		return "/upstreams?id=" + q(o.ParentID), "/upstreams"
	case "upstream":
		return "/upstreams?id=" + q(o.ID), "/upstreams"
	case "key":
		return "/keys?id=" + q(o.ID), "/keys"
	case "user":
		return "/users?id=" + q(o.ID), "/users"
	case "group":
		return "/users?tab=groups&group=" + q(o.ID), "/users?tab=groups"
	case "extension":
		return "/features?tab=extensions&id=" + q(o.ID), "/features?tab=extensions"
	}
	return "/notifications", "/notifications"
}

// Text is the message as plain text.
func (m Message) Text() string {
	var b strings.Builder
	b.WriteString(m.Title)
	b.WriteString("\n")
	for _, l := range m.Lines {
		b.WriteString("\n- " + l)
	}
	if m.Link != "" {
		b.WriteString("\n\n" + m.LinkText + " " + m.Link)
	}
	return b.String()
}

// Markdown is the message in the Markdown dialect of DingTalk and WeCom.
func (m Message) Markdown() string {
	var b strings.Builder
	b.WriteString("### " + m.Title + "\n")
	for _, l := range m.Lines {
		b.WriteString("\n- " + l)
	}
	if m.Link != "" {
		b.WriteString("\n\n[" + m.LinkText + "](" + m.Link + ")")
	}
	return b.String()
}
