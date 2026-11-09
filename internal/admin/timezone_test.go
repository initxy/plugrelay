package admin

import (
	"context"
	"testing"
	"time"
)

// site.timezone is the one time zone: unset is UTC, a bad name is refused,
// and month and day starts follow it.
func TestSiteTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skip("no tzdata")
	}
	e := newEnv(t, sqliteStore, false)
	c := e.setup()
	ctx := context.Background()
	if got := Timezone(ctx, e.st); got != time.UTC {
		t.Fatalf("unset: %v", got)
	}
	if code, _ := c.do("PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"site.timezone": "Mars/Base"}}); code != 422 {
		t.Fatalf("bad zone: %d", code)
	}
	c.must(200, "PATCH", "/api/v1/settings", map[string]any{"values": map[string]any{"site.timezone": "Asia/Shanghai"}})
	if got := Timezone(ctx, e.st); got.String() != "Asia/Shanghai" {
		t.Fatalf("set: %v", got)
	}
	now := time.Date(2026, 10, 31, 17, 0, 0, 0, time.UTC) // 01:00 on 1 Nov in Shanghai
	if got := monthStart(now, loc); !got.Equal(time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("month start: %v", got)
	}
	if got := dayStart(now, loc); !got.Equal(time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("day start: %v", got)
	}
}
