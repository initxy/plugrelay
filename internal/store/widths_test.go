package store_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/initxy/plugrelay/internal/store"
	"github.com/initxy/plugrelay/pkg/plugin"
)

// onEveryDB runs f on SQLite and on the PostgreSQL and MySQL test
// databases when configured.
func onEveryDB(t *testing.T, f func(t *testing.T, open opener)) {
	t.Run("sqlite", func(t *testing.T) { f(t, sqliteOpener) })
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("PLUGRELAY_TEST_PG")
		if dsn == "" {
			t.Skip("PLUGRELAY_TEST_PG not set")
		}
		f(t, pgOpener(dsn))
	})
	t.Run("mysql", func(t *testing.T) { f(t, mysqlFromEnv(t)) })
}

// A client's model name longer than the indexed column is cut alike on
// every database; the record and its roll-up are kept.
func TestRecordLongModel(t *testing.T) {
	onEveryDB(t, func(t *testing.T, open opener) {
		s := open(t, newBox(t))
		ctx := context.Background()
		model := strings.Repeat("模", 300)
		must(t, s.InsertRecords(ctx, []*plugin.Record{{ID: "r1", Time: time.Now(), Model: model, Status: 404}}))
		rec, err := s.GetRecord(ctx, "r1")
		must(t, err)
		if utf8.RuneCountInString(rec.Model) != 191 || !strings.HasPrefix(model, rec.Model) {
			t.Fatalf("stored model of %d characters", utf8.RuneCountInString(rec.Model))
		}
	})
}

// Importing a user whose email another user has is a conflict on every
// database, never a silent skip.
func TestImportUserEmailConflict(t *testing.T) {
	onEveryDB(t, func(t *testing.T, open opener) {
		s := open(t, newBox(t))
		ctx := context.Background()
		ri, ok := store.RowImporterOf(s)
		if !ok {
			t.Fatal("no row importer")
		}
		a := &store.User{ID: "u1", Name: "a", Email: "x@y.z", Role: store.RoleUser}
		b := &store.User{ID: "u2", Name: "b", Email: "x@y.z", Role: store.RoleUser}
		if created, err := ri.ImportUser(ctx, a, ""); err != nil || !created {
			t.Fatalf("first: %v %v", created, err)
		}
		if created, err := ri.ImportUser(ctx, a, ""); err != nil || created {
			t.Fatalf("same id again: %v %v", created, err)
		}
		if created, err := ri.ImportUser(ctx, b, ""); created || !errors.Is(err, store.ErrConflict) {
			t.Fatalf("same email: %v %v", created, err)
		}
	})
}
