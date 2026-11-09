package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
)

type fakeBackend struct {
	mu      sync.Mutex
	conds   []Condition
	chans   []Channel
	entries []*Entry
	n       int
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (b *fakeBackend) Conditions(context.Context) ([]Condition, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.conds, nil
}
func (b *fakeBackend) Channels(context.Context) ([]Channel, error) { return b.chans, nil }
func (b *fakeBackend) Add(_ context.Context, e *Entry) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n++
	c := *e
	c.ID = string(rune('a' + b.n))
	e.ID = c.ID
	b.entries = append(b.entries, &c)
	return nil
}
func (b *fakeBackend) Pending(context.Context) ([]Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Entry
	for _, e := range b.entries {
		if !e.Done {
			out = append(out, *e)
		}
	}
	return out, nil
}
func (b *fakeBackend) Finish(_ context.Context, e *Entry) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, x := range b.entries {
		if x.ID == e.ID {
			c := *e
			b.entries[i] = &c
		}
	}
	return nil
}
func (b *fakeBackend) Drop(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, x := range b.entries {
		if x.ID == id {
			b.entries = append(b.entries[:i], b.entries[i+1:]...)
			break
		}
	}
	return nil
}
func (b *fakeBackend) Describe(_ context.Context, o plugin.NoticeObject) Object {
	return Object{Type: o.Type, ID: o.ID, Name: "name-" + o.ID}
}
func (b *fakeBackend) PublicURL(context.Context) string        { return "https://relay.example.com/" }
func (b *fakeBackend) Timezone(context.Context) *time.Location { return time.UTC }
func (b *fakeBackend) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return b.dial(ctx, network, addr)
}

func (b *fakeBackend) done() []*Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*Entry
	for _, e := range b.entries {
		if e.Done {
			out = append(out, e)
		}
	}
	return out
}

type testHost struct {
	testkit.Host
	st *testkit.State
	b  *fakeBackend
}

func (h *testHost) State() plugin.SharedState { return h.st }
func (h *testHost) NotifyBackend() Backend    { return h.b }

func newNotifier(t *testing.T, b *fakeBackend, set testkit.Settings) (*Notifier, *testkit.Clock) {
	t.Helper()
	clock := testkit.NewClock(time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC))
	st := testkit.NewState(clock.Now)
	h := &testHost{Host: testkit.Host{Set: set, Store: testkit.NewState(clock.Now)}, st: st, b: b}
	n := New()
	n.now = clock.Now
	n.sleep = func(context.Context, time.Duration) error { return nil }
	if err := n.Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	return n, clock
}

func cred(id, event string, d map[string]string) Condition {
	return Condition{Event: event, Object: Object{Type: "credential", ID: id, Name: id, ParentID: "up", ParentName: "Up", Detail: d}}
}

type hook struct {
	mu     sync.Mutex
	bodies [][]byte
	heads  []http.Header
	fail   int
}

func (k *hook) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		k.mu.Lock()
		defer k.mu.Unlock()
		if k.fail > 0 {
			k.fail--
			w.WriteHeader(503)
			return
		}
		k.bodies = append(k.bodies, body)
		k.heads = append(k.heads, r.Header.Clone())
		w.Write([]byte(`{"code":0,"errcode":0}`))
	}))
	t.Cleanup(s.Close)
	return s
}

// Twenty credentials cooling at once are one message; the same condition
// on the next run is not announced again, and an upstream that recovers is.
func TestWatchBatchesAndRecovers(t *testing.T) {
	k := &hook{fail: 1}
	srv := k.server(t)
	b := &fakeBackend{chans: []Channel{{ID: "w", Name: "hook", Type: TypeWebhook, Enabled: true, URL: srv.URL, Secret: "s3"}}}
	n, clock := newNotifier(t, b, testkit.Settings{"cooling_minutes": 30})
	now := clock.Now()
	for i := range 20 {
		b.conds = append(b.conds, cred(string(rune('A'+i)), EventCooling, map[string]string{"reason": "rate_limit",
			"since": now.Format(time.RFC3339), "until": now.Add(time.Hour).Format(time.RFC3339)}))
	}
	b.conds = append(b.conds,
		cred("short", EventCooling, map[string]string{"reason": "rate_limit", "since": now.Format(time.RFC3339),
			"until": now.Add(time.Minute).Format(time.RFC3339)}),
		Condition{Event: EventPaused, Object: Object{Type: "upstream", ID: "up", Name: "Up"}})
	if err := n.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := b.done()
	if len(done) != 2 {
		t.Fatalf("%d entries: %+v", len(done), done)
	}
	byEvent := map[string]*Entry{}
	for _, e := range done {
		byEvent[e.Event] = e
	}
	if c := byEvent[EventCooling]; c == nil || len(c.Objects) != 20 || len(c.Deliveries) != 1 || !c.Deliveries[0].OK || c.Deliveries[0].Attempts < 1 {
		t.Fatalf("cooling entry %+v", c)
	}
	if len(k.bodies) != 2 {
		t.Fatalf("%d posts", len(k.bodies))
	}
	for i, body := range k.bodies {
		h := k.heads[i]
		if h.Get("X-Plugrelay-Signature") != Signature("s3", h.Get("X-Plugrelay-Timestamp"), body) {
			t.Fatal("signature does not verify")
		}
	}

	if err := n.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.done()) != 2 {
		t.Fatal("a standing condition was announced again")
	}
	b.mu.Lock()
	b.conds = b.conds[:20]
	b.mu.Unlock()
	if err := n.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := b.done()[len(b.done())-1]
	if last.Event != EventRecovered || last.Objects[0].ID != "up" {
		t.Fatalf("%+v", last)
	}
	// Paused again within repeat_hours: rate-limited.
	b.mu.Lock()
	b.conds = append(b.conds, Condition{Event: EventPaused, Object: Object{Type: "upstream", ID: "up", Name: "Up"}})
	b.mu.Unlock()
	if err := n.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := b.done()[len(b.done())-1].Event; got != EventRecovered {
		t.Fatalf("paused again within the repeat window was sent: %s", got)
	}
}

func TestQuotaLowThreshold(t *testing.T) {
	b := &fakeBackend{conds: []Condition{
		cred("a", EventQuotaLow, map[string]string{"percent": "8"}),
		cred("b", EventQuotaLow, map[string]string{"percent": "40"}),
	}}
	n, _ := newNotifier(t, b, testkit.Settings{"quota_low_percent": 10})
	if err := n.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := b.done()
	if len(d) != 1 || len(d[0].Objects) != 1 || d[0].Objects[0].ID != "a" || len(d[0].Deliveries) != 0 {
		t.Fatalf("%+v", d)
	}
}

// Budget notices below the threshold are dropped, repeats are dropped, and
// what is left is queued and delivered as one message per event.
func TestPushBudget(t *testing.T) {
	b := &fakeBackend{}
	n, _ := newNotifier(t, b, testkit.Settings{"budget_percent": 80})
	ctx := context.Background()
	push := func(id, pct string) {
		n.Push(ctx, plugin.Notice{Event: EventBudget, Object: plugin.NoticeObject{Type: "key", ID: id}, Key: "spend:monthly:2026-11-01",
			Detail: map[string]string{"percent": pct, "window": "monthly", "kind": "spend"}})
	}
	push("k1", "60")
	push("k1", "85")
	push("k1", "86")
	push("k2", "90")
	push("k1", "100")
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.mu.Lock()
		got := len(b.entries)
		b.mu.Unlock()
		if got == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d entries queued, want 3", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := n.Run(ctx); err != nil {
		t.Fatal(err)
	}
	d := b.done()
	if len(d) != 1 || len(d[0].Objects) != 3 {
		t.Fatalf("%+v", d)
	}
	m := Render(d[0], "zh-CN", "https://relay.example.com", time.UTC)
	if !strings.Contains(m.Title, "3 个额度") || !strings.Contains(m.Text(), "密钥name-k1：每月花费上限已用完") ||
		m.Link != "https://relay.example.com/keys" {
		t.Fatalf("%+v\n%s", m, m.Text())
	}
}

func TestRender(t *testing.T) {
	e := &Entry{Event: EventQuarantined, Objects: []Object{{Type: "credential", ID: "c1", Name: "main", ParentID: "openai",
		ParentName: "OpenAI", Detail: map[string]string{"reason": "banned"}}}}
	en := Render(e, "en", "", time.UTC)
	if en.Title != "[plugrelay] A credential looks suspended and was taken out of rotation" ||
		en.Lines[0] != "OpenAI / main: account looks suspended" || en.Link != "" {
		t.Fatalf("%+v", en)
	}
	zh := Render(e, "zh-CN", "https://r.example/", time.UTC)
	if zh.Link != "https://r.example/upstreams?id=openai" || !strings.Contains(zh.Markdown(), "[在控制台查看](https://r.example/upstreams?id=openai)") {
		t.Fatalf("%+v", zh)
	}
	var many []Object
	for range 12 {
		many = append(many, Object{Type: "upstream", ID: "u"})
	}
	if m := Render(&Entry{Event: EventPaused, Objects: many}, "en", "", time.UTC); len(m.Lines) != shownLines+1 || !strings.Contains(m.Title, "12 upstreams") {
		t.Fatalf("%+v", m)
	}
}

func TestBotSigning(t *testing.T) {
	k := &hook{}
	srv := k.server(t)
	now := time.Unix(1700000000, 0)
	m := Message{Title: "t", Lines: []string{"l"}}
	if err := sendFeishu(context.Background(), http.DefaultClient, Channel{URL: srv.URL, Secret: "sec"}, m, now); err != nil {
		t.Fatal(err)
	}
	var fb map[string]any
	_ = json.Unmarshal(k.bodies[0], &fb)
	if fb["timestamp"] != "1700000000" || fb["sign"] != hmacB64("1700000000\nsec", "") || fb["msg_type"] != "text" {
		t.Fatalf("%v", fb)
	}
	var gotURL string
	ding := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.RawQuery
		w.Write([]byte(`{"errcode":310000,"errmsg":"sign not match"}`))
	}))
	defer ding.Close()
	err := sendDingTalk(context.Background(), http.DefaultClient, Channel{URL: ding.URL + "/robot/send?access_token=x", Secret: "sec"}, m, now)
	var p permanent
	if err == nil || !strings.Contains(err.Error(), "sign not match") || !errorsAs(err, &p) {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(gotURL, "access_token=x") || !strings.Contains(gotURL, "timestamp=1700000000000") || !strings.Contains(gotURL, "sign=") {
		t.Fatal(gotURL)
	}
}

func errorsAs(err error, p *permanent) bool {
	pp, ok := err.(permanent)
	if ok {
		*p = pp
	}
	return ok
}

// A minimal SMTP server: the mail arrives with an encoded subject and the
// recipients given.
func TestSendMail(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		w := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		w("220 test")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					w("250 ok")
					got <- data.String()
					continue
				}
				data.WriteString(line)
				continue
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				w("250 test")
			case strings.HasPrefix(cmd, "DATA"):
				inData = true
				w("354 go")
			case strings.HasPrefix(cmd, "QUIT"):
				w("221 bye")
				return
			default:
				data.WriteString(cmd + "\n")
				w("250 ok")
			}
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	ch := Channel{Type: TypeEmail, SMTPHost: host, TLS: "none", From: "Relay <relay@example.com>", To: []string{"ops@example.com"}}
	ch.SMTPPort = atoi(port)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	if err := SendMail(context.Background(), dial, ch, "[plugrelay] 测试", "第一行\n第二行"); err != nil {
		t.Fatal(err)
	}
	mail := <-got
	for _, want := range []string{"MAIL FROM:<RELAY@EXAMPLE.COM>", "RCPT TO:<OPS@EXAMPLE.COM>", "Subject: =?utf-8?q?", "Content-Transfer-Encoding: base64"} {
		if !strings.Contains(mail, want) {
			t.Fatalf("missing %q in\n%s", want, mail)
		}
	}
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func TestTestMessageIsLogged(t *testing.T) {
	k := &hook{}
	srv := k.server(t)
	b := &fakeBackend{}
	n, _ := newNotifier(t, b, testkit.Settings{})
	d := n.Test(context.Background(), Channel{ID: "c", Name: "Ops", Type: TypeWeCom, URL: srv.URL, Language: "en"})
	if !d.OK || len(b.done()) != 1 || b.done()[0].Event != EventTest {
		t.Fatalf("%+v %+v", d, b.entries)
	}
	if !strings.Contains(string(k.bodies[0]), `Test message: channel \"Ops\" works`) {
		t.Fatal(string(k.bodies[0]))
	}
}
