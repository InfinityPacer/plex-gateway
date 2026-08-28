// Package database owns the Gateway SQLite connection and cross-domain
// migration registry. Feature packages own their tables and submit migrations
// through this package.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const migrationRegistrySchema = `
CREATE TABLE IF NOT EXISTS gateway_schema_migrations (
    module TEXT NOT NULL,
    version INTEGER NOT NULL,
    applied_at_ms INTEGER NOT NULL,
    PRIMARY KEY (module, version)
);
`

// Migration is one ordered schema transition owned by a feature module.
// Versions must start at one and remain contiguous within that module.
type Migration struct {
	Version int
	SQL     string
}

// SQLite owns the process-wide Gateway database connection.
type SQLite struct {
	db *sql.DB
}

// OpenSQLite opens the Gateway database and establishes connection-wide
// durability and contention settings. The path must be a filesystem path, not
// a caller-controlled SQLite URI.
func OpenSQLite(ctx context.Context, path string) (*SQLite, error) {
	path = strings.TrimSpace(path)
	if path == "" || strings.IndexByte(path, 0) >= 0 || strings.HasPrefix(path, "file:") {
		return nil, errors.New("Gateway database path is invalid")
	}
	cleanPath := filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(cleanPath), 0o750); err != nil {
		return nil, fmt.Errorf("create Gateway database directory: %w", err)
	}
	file, err := os.OpenFile(cleanPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create Gateway database: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close Gateway database file: %w", err)
	}

	db, err := sql.Open("sqlite", cleanPath)
	if err != nil {
		return nil, fmt.Errorf("open Gateway database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &SQLite{db: db}
	if err := store.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (store *SQLite) initialize(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure Gateway database: %w", err)
		}
	}
	if _, err := store.db.ExecContext(ctx, migrationRegistrySchema); err != nil {
		return fmt.Errorf("initialize Gateway migration registry: %w", err)
	}
	return nil
}

// ApplyMigrations advances one feature module without changing the schema
// version of any other module. A newer on-disk version is rejected so an older
// binary cannot silently operate on an unknown schema.
func (store *SQLite) ApplyMigrations(ctx context.Context, module string, migrations []Migration) error {
	if store == nil || store.db == nil {
		return errors.New("Gateway database is unavailable")
	}
	module = strings.TrimSpace(module)
	if module == "" {
		return errors.New("Gateway migration module is required")
	}
	for index, migration := range migrations {
		if migration.Version != index+1 || strings.TrimSpace(migration.SQL) == "" {
			return fmt.Errorf("Gateway migration %s version sequence is invalid", module)
		}
	}

	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Gateway database migration: %w", err)
	}
	defer transaction.Rollback()

	var applied, current int
	if err := transaction.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MAX(version), 0)
		FROM gateway_schema_migrations
		WHERE module = ?
	`, module).Scan(&applied, &current); err != nil {
		return fmt.Errorf("read %s schema version: %w", module, err)
	}
	if applied != current {
		return fmt.Errorf("%s schema migration history is not contiguous", module)
	}
	if current > len(migrations) {
		return fmt.Errorf("%s schema version %d is newer than supported version %d", module, current, len(migrations))
	}
	for _, migration := range migrations[current:] {
		if _, err := transaction.ExecContext(ctx, migration.SQL); err != nil {
			return fmt.Errorf("apply %s schema v%d: %w", module, migration.Version, err)
		}
		if _, err := transaction.ExecContext(ctx, `
			INSERT INTO gateway_schema_migrations (module, version, applied_at_ms)
			VALUES (?, ?, ?)
		`, module, migration.Version, time.Now().UTC().UnixMilli()); err != nil {
			return fmt.Errorf("record %s schema v%d: %w", module, migration.Version, err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit Gateway database migration: %w", err)
	}
	return nil
}

// SQLDB returns the shared handle for feature stores. Callers must not close it.
func (store *SQLite) SQLDB() *sql.DB {
	if store == nil {
		return nil
	}
	return store.db
}

// Close flushes and closes the process-wide Gateway database.
func (store *SQLite) Close() error {
	if store == nil || store.db == nil {
		return nil
	}
	return store.db.Close()
}
