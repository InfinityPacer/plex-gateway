package mediainfo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	sqliteDatabaseVersion = 1
	sqliteSchemaV1        = `
CREATE TABLE IF NOT EXISTS media_info_records (
    plex_server_id TEXT NOT NULL,
    part_id TEXT NOT NULL,
	strm_fingerprint TEXT NOT NULL,
	rating_key TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL,
	provider_revision TEXT NOT NULL,
	content_fingerprint TEXT NOT NULL DEFAULT '',
	schema_version INTEGER NOT NULL,
    media_json BLOB NOT NULL,
    probed_at_ms INTEGER NOT NULL,
    expires_at_ms INTEGER NOT NULL,
    last_accessed_at_ms INTEGER NOT NULL,
    retain_until_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    PRIMARY KEY (plex_server_id, part_id, strm_fingerprint)
);
CREATE INDEX IF NOT EXISTS media_info_records_retain_until
    ON media_info_records (retain_until_ms);
`
)

// SQLiteStore persists successful MediaInfo results. It uses one database
// connection because SQLite is the single-instance writer in this design.
type SQLiteStore struct {
	db *sql.DB
}

// OpenSQLite creates or opens a private SQLite database, enables WAL mode, and
// applies versioned migrations. The path is a filesystem path, not a
// caller-controlled SQLite URI.
func OpenSQLite(ctx context.Context, path string) (*SQLiteStore, error) {
	path = strings.TrimSpace(path)
	if path == "" || strings.IndexByte(path, 0) >= 0 || strings.HasPrefix(path, "file:") {
		return nil, errors.New("MediaInfo database path is invalid")
	}
	cleanPath := filepath.Clean(path)
	directory := filepath.Dir(cleanPath)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, fmt.Errorf("create MediaInfo database directory: %w", err)
	}
	file, err := os.OpenFile(cleanPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create MediaInfo database: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close MediaInfo database file: %w", err)
	}

	db, err := sql.Open("sqlite", cleanPath)
	if err != nil {
		return nil, fmt.Errorf("open MediaInfo database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &SQLiteStore{db: db}
	if err := store.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (store *SQLiteStore) initialize(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure MediaInfo database: %w", err)
		}
	}
	return store.migrate(ctx)
}

func (store *SQLiteStore) migrate(ctx context.Context) error {
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin MediaInfo database migration: %w", err)
	}
	defer transaction.Rollback()

	var version int
	if err := transaction.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read MediaInfo database version: %w", err)
	}
	if version > sqliteDatabaseVersion {
		return fmt.Errorf("MediaInfo database version %d is newer than supported version %d", version, sqliteDatabaseVersion)
	}
	if version < 1 {
		if _, err := transaction.ExecContext(ctx, sqliteSchemaV1); err != nil {
			return fmt.Errorf("apply MediaInfo database schema v1: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
			return fmt.Errorf("record MediaInfo database schema v1: %w", err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit MediaInfo database migration: %w", err)
	}
	return nil
}

// Put inserts or refreshes one complete record. A late older probe cannot
// replace a newer result for the same stable key.
func (store *SQLiteStore) Put(ctx context.Context, record Record) error {
	if store == nil || store.db == nil {
		return errors.New("MediaInfo store is unavailable")
	}
	if err := record.Key.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(record.Provider) == "" || !record.Media.Complete ||
		strings.TrimSpace(record.ProviderRevision) == "" || record.SchemaVersion != SchemaVersion {
		return errors.New("MediaInfo record is incomplete or incompatible")
	}
	if record.ProbedAt.IsZero() || record.LastAccessedAt.IsZero() ||
		!record.ExpiresAt.After(record.ProbedAt) || record.RetainUntil.Before(record.ExpiresAt) ||
		!record.RetainUntil.After(record.LastAccessedAt) {
		return errors.New("MediaInfo record timestamps are invalid")
	}
	mediaJSON, err := json.Marshal(record.Media)
	if err != nil {
		return fmt.Errorf("encode MediaInfo record: %w", err)
	}
	now := time.Now().UTC().UnixMilli()
	_, err = store.db.ExecContext(ctx, `
INSERT INTO media_info_records (
	plex_server_id, part_id, strm_fingerprint, rating_key, provider, provider_revision,
	content_fingerprint, schema_version, media_json,
	probed_at_ms, expires_at_ms, last_accessed_at_ms, retain_until_ms, updated_at_ms
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (plex_server_id, part_id, strm_fingerprint) DO UPDATE SET
	rating_key = excluded.rating_key,
	provider = excluded.provider,
	provider_revision = excluded.provider_revision,
	content_fingerprint = excluded.content_fingerprint,
	schema_version = excluded.schema_version,
    media_json = excluded.media_json,
    probed_at_ms = excluded.probed_at_ms,
    expires_at_ms = excluded.expires_at_ms,
    last_accessed_at_ms = excluded.last_accessed_at_ms,
    retain_until_ms = excluded.retain_until_ms,
    updated_at_ms = excluded.updated_at_ms
WHERE excluded.probed_at_ms >= media_info_records.probed_at_ms
	`, record.Key.PlexServerID, record.Key.PartID, record.Key.STRMFingerprint,
		record.RatingKey, record.Provider, record.ProviderRevision,
		record.ContentFingerprint, record.SchemaVersion, mediaJSON,
		record.ProbedAt.UTC().UnixMilli(), record.ExpiresAt.UTC().UnixMilli(),
		record.LastAccessedAt.UTC().UnixMilli(), record.RetainUntil.UTC().UnixMilli(), now)
	if err != nil {
		return fmt.Errorf("write MediaInfo record: %w", err)
	}
	return nil
}

// Get returns the exact persisted record even when it is expired. Callers use
// Record.Fresh to decide whether it can satisfy a current request.
func (store *SQLiteStore) Get(ctx context.Context, key Key) (Record, bool, error) {
	if store == nil || store.db == nil {
		return Record{}, false, errors.New("MediaInfo store is unavailable")
	}
	if err := key.Validate(); err != nil {
		return Record{}, false, err
	}
	row := store.db.QueryRowContext(ctx, `
	SELECT rating_key, provider, provider_revision, content_fingerprint, schema_version, media_json,
       probed_at_ms, expires_at_ms, last_accessed_at_ms, retain_until_ms
FROM media_info_records
WHERE plex_server_id = ? AND part_id = ? AND strm_fingerprint = ?
`, key.PlexServerID, key.PartID, key.STRMFingerprint)
	record, err := scanRecord(row, key)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	return record, true, nil
}

// LoadCompatibleRetained restores fresh and stale records for one provider
// contract so obsolete revisions cannot consume the bounded L1 capacity.
func (store *SQLiteStore) LoadCompatibleRetained(
	ctx context.Context,
	now time.Time,
	limit int,
	provider ProviderDescriptor,
) ([]Record, error) {
	if store == nil || store.db == nil {
		return nil, errors.New("MediaInfo store is unavailable")
	}
	if limit <= 0 {
		return nil, errors.New("MediaInfo restore limit must be positive")
	}
	if err := provider.validate(); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `
	SELECT plex_server_id, part_id, strm_fingerprint, rating_key, provider, provider_revision,
	       content_fingerprint, schema_version, media_json, probed_at_ms, expires_at_ms,
	       last_accessed_at_ms, retain_until_ms
	FROM media_info_records
	WHERE retain_until_ms > ? AND schema_version = ? AND provider = ? AND provider_revision = ?
	ORDER BY last_accessed_at_ms DESC
	LIMIT ?
	`, now.UTC().UnixMilli(), SchemaVersion, provider.Name, provider.Revision, limit)
	if err != nil {
		return nil, fmt.Errorf("load MediaInfo records: %w", err)
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		var key Key
		var record Record
		var mediaJSON []byte
		var probedAtMS, expiresAtMS, lastAccessedAtMS, retainUntilMS int64
		if err := rows.Scan(
			&key.PlexServerID, &key.PartID, &key.STRMFingerprint, &record.RatingKey,
			&record.Provider, &record.ProviderRevision, &record.ContentFingerprint,
			&record.SchemaVersion, &mediaJSON, &probedAtMS, &expiresAtMS,
			&lastAccessedAtMS, &retainUntilMS,
		); err != nil {
			return nil, fmt.Errorf("scan MediaInfo record: %w", err)
		}
		record.Key = key
		record.ProbedAt = time.UnixMilli(probedAtMS).UTC()
		record.ExpiresAt = time.UnixMilli(expiresAtMS).UTC()
		record.LastAccessedAt = time.UnixMilli(lastAccessedAtMS).UTC()
		record.RetainUntil = time.UnixMilli(retainUntilMS).UTC()
		if err := json.Unmarshal(mediaJSON, &record.Media); err != nil {
			return nil, fmt.Errorf("decode MediaInfo record: %w", err)
		}
		if record.Retained(now) {
			records = append(records, record)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate MediaInfo records: %w", err)
	}
	return records, nil
}

// DeleteUnretained removes records beyond their stale fallback window.
func (store *SQLiteStore) DeleteUnretained(ctx context.Context, now time.Time) (int64, error) {
	if store == nil || store.db == nil {
		return 0, errors.New("MediaInfo store is unavailable")
	}
	result, err := store.db.ExecContext(ctx, "DELETE FROM media_info_records WHERE retain_until_ms <= ?", now.UTC().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("delete expired MediaInfo records: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read deleted MediaInfo count: %w", err)
	}
	return deleted, nil
}

// Touch extends retention without rewriting the MediaInfo JSON payload.
func (store *SQLiteStore) Touch(ctx context.Context, key Key, accessedAt, retainUntil time.Time) error {
	if store == nil || store.db == nil {
		return errors.New("MediaInfo store is unavailable")
	}
	if err := key.Validate(); err != nil {
		return err
	}
	if accessedAt.IsZero() || !retainUntil.After(accessedAt) {
		return errors.New("MediaInfo access timestamps are invalid")
	}
	now := time.Now().UTC().UnixMilli()
	_, err := store.db.ExecContext(ctx, `
UPDATE media_info_records
SET last_accessed_at_ms = MAX(last_accessed_at_ms, ?),
    retain_until_ms = MAX(retain_until_ms, ?),
    updated_at_ms = ?
WHERE plex_server_id = ? AND part_id = ? AND strm_fingerprint = ?
  AND last_accessed_at_ms <= ?
`, accessedAt.UTC().UnixMilli(), retainUntil.UTC().UnixMilli(), now,
		key.PlexServerID, key.PartID, key.STRMFingerprint, accessedAt.UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("touch MediaInfo record: %w", err)
	}
	return nil
}

// Close flushes and closes the SQLite database.
func (store *SQLiteStore) Close() error {
	if store == nil || store.db == nil {
		return nil
	}
	return store.db.Close()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(row rowScanner, key Key) (Record, error) {
	record := Record{Key: key}
	var mediaJSON []byte
	var probedAtMS, expiresAtMS, lastAccessedAtMS, retainUntilMS int64
	if err := row.Scan(
		&record.RatingKey, &record.Provider, &record.ProviderRevision,
		&record.ContentFingerprint, &record.SchemaVersion,
		&mediaJSON, &probedAtMS, &expiresAtMS, &lastAccessedAtMS, &retainUntilMS,
	); err != nil {
		return Record{}, err
	}
	record.ProbedAt = time.UnixMilli(probedAtMS).UTC()
	record.ExpiresAt = time.UnixMilli(expiresAtMS).UTC()
	record.LastAccessedAt = time.UnixMilli(lastAccessedAtMS).UTC()
	record.RetainUntil = time.UnixMilli(retainUntilMS).UTC()
	if err := json.Unmarshal(mediaJSON, &record.Media); err != nil {
		return Record{}, fmt.Errorf("decode MediaInfo record: %w", err)
	}
	return record, nil
}
