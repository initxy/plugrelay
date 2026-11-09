package app

import (
	"context"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/state"
)

// TestPluginStateIsScoped checks that two plugins writing the same key see
// separate values, so one cannot change another's counters.
func TestPluginStateIsScoped(t *testing.T) {
	ctx := context.Background()
	backend := state.NewMemory(time.Minute)
	defer backend.Close()
	a, b := newScopedState(backend, "meter.quota"), newScopedState(backend, "middleware.ratelimit")
	if _, err := a.Incr(ctx, "held:u1", 5, 0); err != nil {
		t.Fatal(err)
	}
	if n, err := b.Incr(ctx, "held:u1", 1, 0); err != nil || n != 1 {
		t.Fatalf("other plugin saw %d, %v; want its own counter at 1", n, err)
	}
	if _, ok, _ := backend.Get(ctx, "held:u1"); ok {
		t.Fatal("unprefixed key written to the shared backend")
	}
	if v, ok, _ := backend.Get(ctx, "plugin:meter.quota:held:u1"); !ok || string(v) != "5" {
		t.Fatalf("scoped key = %q, %v", v, ok)
	}
}
