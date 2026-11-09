package importnewapi_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/initxy/plugrelay/internal/secret"
	"github.com/initxy/plugrelay/internal/store"
	plug "github.com/initxy/plugrelay/plugins/admin/importnewapi"
)

// Two new-api users with one email (any case) in one import: the first
// is imported, the second reported as a conflict with its keys and
// balance skipped, on every database.
func TestImportDuplicateEmails(t *testing.T) {
	data := `{"channels":[],"options":{},
"users":[{"id":1,"username":"ann","role":1,"status":1,"email":"Dup@Example.com","quota":500000,"group":"default"},
 {"id":2,"username":"bob","role":1,"status":1,"email":"dup@example.com","quota":500000,"group":"default"}],
"tokens":[{"id":1,"user_id":1,"key":"AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGhIjK1","status":1,"name":"a","unlimited_quota":true},
 {"id":2,"user_id":2,"key":"AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGhIjK2","status":1,"name":"b","unlimited_quota":true}]}`
	for name, open := range map[string]func(t *testing.T) store.Store{"sqlite": openStore, "postgres": pgStore, "mysql": mysqlStore} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := open(t)
			rep, err := importer(st).ImportNewAPI(ctx, plug.Source{Data: json.RawMessage(data)}, false)
			if err != nil {
				t.Fatal(err)
			}
			skipped := items(rep, "user", "skip")
			if len(skipped) != 1 || skipped[0].ID != "newapi-u2" || !strings.Contains(skipped[0].Reason, "newapi-u1") {
				t.Fatalf("users: %+v", rep.Items)
			}
			if k := items(rep, "key", "skip"); len(k) != 1 || k[0].ID != "newapi-t2" {
				t.Fatalf("keys: %+v", rep.Items)
			}
			if u, err := st.GetUser(ctx, "newapi-u2"); err == nil {
				t.Fatalf("second user imported: %+v", u)
			}
			if k, err := st.GetKey(ctx, "newapi-t2"); err == nil {
				t.Fatalf("its key imported: %+v", k)
			}
			l, _ := store.LedgerOf(st)
			if a, _ := l.Account(ctx, "user:newapi-u2"); a.BalanceMicro != 0 {
				t.Fatalf("its balance credited: %+v", a)
			}
		})
	}
}

func testBox() *secret.Box {
	k, _ := secret.GenerateMasterKey()
	mk, _ := secret.ParseMasterKey(k)
	box, _ := secret.NewBox(mk)
	return box
}

func pgStore(t *testing.T) store.Store {
	dsn := os.Getenv("PLUGRELAY_TEST_PG")
	if dsn == "" {
		t.Skip("PLUGRELAY_TEST_PG not set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("import_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	s, err := store.Open(ctx, store.Config{Driver: "postgres", DSN: dsn + sep + "search_path=" + schema, Secrets: testBox()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		_, _ = admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return s
}

func mysqlStore(t *testing.T) store.Store {
	dsn := os.Getenv("PLUGRELAY_TEST_MYSQL")
	if dsn == "" {
		t.Skip("PLUGRELAY_TEST_MYSQL not set")
	}
	ctx := context.Background()
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = fmt.Sprintf("import_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+cfg.DBName); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, store.Config{Driver: "mysql", DSN: cfg.FormatDSN(), Secrets: testBox()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		_, _ = admin.ExecContext(ctx, "DROP DATABASE "+cfg.DBName)
		admin.Close()
	})
	return s
}
