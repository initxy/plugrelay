package store

import (
	"io/fs"
	"slices"
	"testing"
)

// Every migration ships for every driver under the same name.
func TestMigrationsPerDriver(t *testing.T) {
	names := func(d dialect) []string {
		entries, err := fs.ReadDir(migrations, "migrations/"+string(d))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}
	want := names(sqliteDialect)
	for _, d := range []dialect{postgresDialect, mysqlDialect} {
		if got := names(d); !slices.Equal(got, want) {
			t.Errorf("%s migrations %v, sqlite has %v", d, got, want)
		}
	}
}
