package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/Opsmastery-Summit/tvt/pkg/graph"
)

// testdata/schema/vN.sql is graph.Schema as it first shipped at version N,
// frozen. A database built from any of them and upgraded by Open must have
// exactly the columns of a fresh database: that catches a column added to
// schema.sql (or to an existing upgrade step) without a new version, which
// databases already past that version would never get.
//
// When you bump SchemaVersion, copy schema.sql to testdata/schema/vN.sql and
// never edit it again.

func TestCurrentSchemaIsFrozen(t *testing.T) {
	b, err := os.ReadFile(fmt.Sprintf("testdata/schema/v%d.sql", SchemaVersion))
	if err != nil {
		t.Fatalf("no frozen schema for version %d (copy pkg/graph/schema.sql there when you bump SchemaVersion): %v", SchemaVersion, err)
	}
	if string(b) != graph.Schema {
		t.Fatalf("pkg/graph/schema.sql differs from testdata/schema/v%d.sql: a schema change needs a new SchemaVersion, an upgrade step and a new frozen file", SchemaVersion)
	}
}

func TestUpgradeFromEveryShippedSchema(t *testing.T) {
	fresh, _ := open(t)
	want := columns(t, fresh.db)
	for v := 1; v < SchemaVersion; v++ {
		t.Run("v"+strconv.Itoa(v), func(t *testing.T) {
			b, err := os.ReadFile(fmt.Sprintf("testdata/schema/v%d.sql", v))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "old.db")
			db, err := sql.Open("sqlite", path)
			must(t, err)
			_, err = db.Exec(string(b))
			must(t, err)
			_, err = db.Exec(`INSERT INTO meta (key, value) VALUES ('schema_version', ?)`, strconv.Itoa(v))
			must(t, err)
			must(t, db.Close())

			s, err := Open(ctx, path)
			if err != nil {
				t.Fatalf("open version %d database: %v", v, err)
			}
			defer s.Close()
			got := columns(t, s.db)
			if !reflect.DeepEqual(got, want) {
				for table, cols := range want {
					if !reflect.DeepEqual(got[table], cols) {
						t.Errorf("%s columns after upgrade from v%d:\n got %v\nwant %v", table, v, got[table], cols)
					}
				}
			}
		})
	}
}

// columns maps each table to its column names, sorted (ALTER TABLE appends,
// so an upgraded table's order differs from a fresh one's).
func columns(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	rows, err := db.Query(`SELECT m.name, p.name FROM sqlite_schema m, pragma_table_info(m.name) p
		WHERE m.type = 'table' ORDER BY m.name, p.name`)
	must(t, err)
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var table, col string
		must(t, rows.Scan(&table, &col))
		out[table] = append(out[table], col)
	}
	must(t, rows.Err())
	return out
}
