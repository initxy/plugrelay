package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/initxy/plugrelay/internal/store"
)

func TestIdentitiesSQLite(t *testing.T) { identityConformance(t, sqliteOpener) }
func TestIdentitiesMySQL(t *testing.T)  { identityConformance(t, mysqlFromEnv(t)) }

func TestIdentitiesPostgres(t *testing.T) {
	dsn := os.Getenv("PLUGRELAY_TEST_PG")
	if dsn == "" {
		t.Skip("PLUGRELAY_TEST_PG not set")
	}
	identityConformance(t, pgOpener(dsn))
}

func identityConformance(t *testing.T, open opener) {
	ctx := context.Background()
	s := open(t, newBox(t))
	u := &store.User{Name: "Ann", Email: "Ann@Example.com", Role: store.RoleUser}
	must(t, s.CreateUser(ctx, u, ""))

	got, err := s.UserByEmail(ctx, "ann@example.COM")
	must(t, err)
	if got.ID != u.ID {
		t.Fatalf("user by email: %+v", got)
	}
	if _, err := s.UserByEmail(ctx, "nobody@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing email: %v", err)
	}
	if _, err := s.UserByEmail(ctx, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty email: %v", err)
	}

	id := &store.Identity{Provider: "google", Subject: "123", UserID: u.ID, Email: "ann@example.com"}
	must(t, s.LinkIdentity(ctx, id))
	if id.ID == "" {
		t.Fatal("no id")
	}
	if err := s.LinkIdentity(ctx, &store.Identity{Provider: "google", Subject: "123", UserID: "other"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate subject: %v", err)
	}
	must(t, s.LinkIdentity(ctx, &store.Identity{Provider: "github", Subject: "123", UserID: u.ID}))

	gi, err := s.GetIdentity(ctx, "google", "123")
	must(t, err)
	if gi.UserID != u.ID || gi.Email != "ann@example.com" || !gi.LastLoginAt.IsZero() {
		t.Fatalf("%+v", gi)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	must(t, s.TouchIdentity(ctx, gi.ID, at))
	gi, _ = s.GetIdentity(ctx, "google", "123")
	if !gi.LastLoginAt.Equal(at) {
		t.Fatalf("last login %v, want %v", gi.LastLoginAt, at)
	}
	if _, err := s.GetIdentity(ctx, "google", "999"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing identity: %v", err)
	}
	ids, err := s.ListIdentities(ctx, u.ID)
	must(t, err)
	if len(ids) != 2 {
		t.Fatalf("list: %+v", ids)
	}
	all, _ := s.ListIdentities(ctx, "")
	if len(all) != 2 {
		t.Fatalf("list all: %+v", all)
	}

	must(t, s.DeleteIdentity(ctx, gi.ID))
	if err := s.DeleteIdentity(ctx, gi.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	must(t, s.DeleteUser(ctx, u.ID))
	if ids, _ := s.ListIdentities(ctx, u.ID); len(ids) != 0 {
		t.Fatalf("identities outlive their user: %+v", ids)
	}
}
