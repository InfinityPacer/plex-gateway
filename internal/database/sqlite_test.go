package database

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteTracksModuleMigrationsIndependently(t *testing.T) {
	store, err := OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "plex-gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.ApplyMigrations(t.Context(), "media", []Migration{{Version: 1, SQL: "CREATE TABLE media_records (id TEXT PRIMARY KEY)"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyMigrations(t.Context(), "jobs", []Migration{{Version: 1, SQL: "CREATE TABLE jobs (id TEXT PRIMARY KEY)"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.db.QueryContext(t.Context(), "SELECT module, version FROM gateway_schema_migrations ORDER BY module")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var module string
		var version int
		if err := rows.Scan(&module, &version); err != nil {
			t.Fatal(err)
		}
		got = append(got, module)
	}
	if strings.Join(got, ",") != "jobs,media" {
		t.Fatalf("migration modules = %v", got)
	}
}

func TestSQLiteRejectsNewerModuleWithoutAffectingOthers(t *testing.T) {
	store, err := OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "plex-gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.ExecContext(t.Context(), `
		INSERT INTO gateway_schema_migrations (module, version, applied_at_ms)
		VALUES ('mediainfo', 1, 0), ('mediainfo', 2, 0), ('jobs', 1, 0)
	`); err != nil {
		t.Fatal(err)
	}

	err = store.ApplyMigrations(t.Context(), "mediainfo", []Migration{{Version: 1, SQL: "SELECT 1"}})
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("ApplyMigrations() error = %v", err)
	}
	if err := store.ApplyMigrations(t.Context(), "jobs", []Migration{{Version: 1, SQL: "SELECT 1"}}); err != nil {
		t.Fatalf("unrelated module migration failed: %v", err)
	}
}
