package requestlog

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/internal/testkit"
)

type writer struct{ got []*plugin.Record }

func (w *writer) InsertRecord(_ context.Context, r *plugin.Record) error {
	w.got = append(w.got, r)
	return nil
}

type host struct {
	testkit.Host
	w      *writer
	groups map[string]testkit.Settings
}

func (h *host) RecordWriter() RecordWriter { return h.w }

type perCaller struct{ *host }

func (h perCaller) SettingsFor(_ string, c plugin.Caller) plugin.Settings { return h.groups[c.GroupID] }

func record(group string) *plugin.Record {
	return &plugin.Record{ID: "r1", Caller: plugin.Caller{GroupID: group},
		Body: &plugin.RecordBody{Request: json.RawMessage(`{"q":1}`)}}
}

func TestBodyLogging(t *testing.T) {
	tests := []struct {
		name     string
		global   testkit.Settings
		groups   map[string]testkit.Settings
		group    string
		wantBody bool
	}{
		{name: "default off", global: testkit.Settings{}},
		{name: "global on", global: testkit.Settings{"body_logging": true}, wantBody: true},
		{name: "per group on", groups: map[string]testkit.Settings{"g": {"body_logging": true}}, group: "g", wantBody: true},
		{name: "per group off", groups: map[string]testkit.Settings{"g": {"body_logging": false}}, group: "g"},
		{name: "unknown group", groups: map[string]testkit.Settings{}, group: "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &host{Host: testkit.Host{Set: tt.global}, w: &writer{}, groups: tt.groups}
			var ph plugin.Host = h
			if tt.groups != nil {
				ph = perCaller{h}
			}
			s := New(nil)
			if err := s.Init(context.Background(), ph); err != nil {
				t.Fatal(err)
			}
			in := record(tt.group)
			if err := s.Consume(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			if got := h.w.got[0].Body != nil; got != tt.wantBody {
				t.Fatalf("body stored = %v", got)
			}
			if in.Body == nil {
				t.Fatal("caller's record was modified")
			}
		})
	}
}

func TestInitNeedsWriter(t *testing.T) {
	if err := New(nil).Init(context.Background(), &testkit.Host{}); err == nil {
		t.Fatal("expected error")
	}
}

type purger struct{ before []time.Time }

func (p *purger) Purge(_ context.Context, b time.Time) error {
	p.before = append(p.before, b)
	return nil
}

func TestRetention(t *testing.T) {
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		set  testkit.Settings
		want []time.Time
	}{
		{testkit.Settings{}, nil},
		{testkit.Settings{"retention_days": 7}, []time.Time{now.AddDate(0, 0, -7)}},
		{testkit.Settings{"retention_days": 0}, nil},
	}
	for _, tt := range tests {
		p := &purger{}
		j := NewRetention(p)
		j.now = func() time.Time { return now }
		if err := j.Init(context.Background(), &testkit.Host{Set: tt.set}); err != nil {
			t.Fatal(err)
		}
		if err := j.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(p.before) != len(tt.want) || (len(tt.want) == 1 && !p.before[0].Equal(tt.want[0])) {
			t.Errorf("%v: purged before %v, want %v", tt.set, p.before, tt.want)
		}
	}
}
