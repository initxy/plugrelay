package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
	"github.com/initxy/plugrelay/plugins/job/notify"
)

type backend struct {
	notify.Backend
	chans []notify.Channel
}

func (b backend) Channels(context.Context) ([]notify.Channel, error) { return b.chans, nil }
func (b backend) Dial(context.Context, string, string) (net.Conn, error) {
	return nil, nil
}

type host struct {
	testkit.Host
	b backend
}

func (h *host) NotifyBackend() notify.Backend { return h.b }

func TestRecordsPosted(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]json.RawMessage
	var sigOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			Type    string                       `json:"type"`
			Records []map[string]json.RawMessage `json:"records"`
		}
		_ = json.Unmarshal(body, &in)
		mu.Lock()
		got = append(got, in.Records...)
		sigOK = in.Type == "records" && r.Header.Get("X-Plugrelay-Signature") == notify.Signature("k", r.Header.Get("X-Plugrelay-Timestamp"), body)
		mu.Unlock()
	}))
	defer srv.Close()
	Flush = 20 * time.Millisecond
	s := New()
	h := &host{b: backend{chans: []notify.Channel{
		{ID: "w", Type: notify.TypeWebhook, Enabled: true, Records: true, URL: srv.URL, Secret: "k"},
		{ID: "off", Type: notify.TypeWebhook, Enabled: false, Records: true, URL: srv.URL},
		{ID: "events", Type: notify.TypeWebhook, Enabled: true, URL: srv.URL},
	}}}
	if err := s.Init(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"r1", "r2"} {
		_ = s.Consume(context.Background(), &plugin.Record{ID: id, Model: "m", Status: 200,
			Body: &plugin.RecordBody{Request: json.RawMessage(`{"secret":"text"}`)}})
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || !sigOK {
		t.Fatalf("%d records, signature ok %v", len(got), sigOK)
	}
	if _, ok := got[0]["body"]; ok {
		t.Fatal("body posted without being selected")
	}
	if string(got[0]["id"]) != `"r1"` || string(got[0]["model"]) != `"m"` {
		t.Fatalf("%v", got[0])
	}
}

func TestSelect(t *testing.T) {
	r := &plugin.Record{ID: "r", Model: "m", Body: &plugin.RecordBody{Request: json.RawMessage(`{"a":1}`)}}
	out := Select([]*plugin.Record{r}, []string{"id", "body"})
	if len(out[0]) != 2 || string(out[0]["id"]) != `"r"` || out[0]["body"] == nil {
		t.Fatalf("%v", out[0])
	}
}
