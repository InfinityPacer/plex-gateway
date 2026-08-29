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

	"github.com/InfinityPacer/plex-gateway/internal/database"
)

const (
	mediaInfoMigrationModule = "mediainfo"
	mediaInfoSchemaV1        = `
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

// SQLiteStore persists successful MediaInfo results in the shared Gateway
// database without owning the connection lifecycle.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore attaches the MediaInfo domain to an open Gateway database and
// applies only the migrations owned by this domain.
func NewSQLiteStore(ctx context.Context, gatewayDB *database.SQLite) (*SQLiteStore, error) {
	if gatewayDB == nil || gatewayDB.SQLDB() == nil {
		return nil, errors.New("Gateway database is unavailable")
	}
	if err := gatewayDB.ApplyMigrations(ctx, mediaInfoMigrationModule, []database.Migration{
		{Version: 1, SQL: mediaInfoSchemaV1},
	}); err != nil {
		return nil, err
	}
	return &SQLiteStore{db: gatewayDB.SQLDB()}, nil
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

// BackupAndDeleteAll creates a transactionally consistent full-database
// backup, then removes only rebuildable MediaInfo rows while retaining the
// shared schema registry and tables owned by other modules.
func (store *SQLiteStore) BackupAndDeleteAll(
	ctx context.Context,
	backupDir string,
	now time.Time,
) (ResetResult, error) {
	if store == nil || store.db == nil {
		return ResetResult{}, errors.New("MediaInfo store is unavailable")
	}
	backupDir = filepath.Clean(backupDir)
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return ResetResult{}, fmt.Errorf("create Gateway backup directory: %w", err)
	}
	if err := os.Chmod(backupDir, 0o700); err != nil {
		return ResetResult{}, fmt.Errorf("protect Gateway backup directory: %w", err)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	backupPath := filepath.Join(
		backupDir,
		"plex-gateway-before-mediainfo-reset-"+now.Format("20060102T150405.000000000Z")+".db",
	)

	connection, err := store.db.Conn(ctx)
	if err != nil {
		return ResetResult{}, fmt.Errorf("reserve Gateway database connection: %w", err)
	}
	defer connection.Close()
	var quotedPath string
	if err := connection.QueryRowContext(ctx, "SELECT quote(?)", backupPath).Scan(&quotedPath); err != nil {
		return ResetResult{}, fmt.Errorf("quote Gateway backup path: %w", err)
	}
	if _, err := connection.ExecContext(ctx, "VACUUM INTO "+quotedPath); err != nil {
		return ResetResult{}, fmt.Errorf("backup Gateway database: %w", err)
	}
	if err := os.Chmod(backupPath, 0o600); err != nil {
		return ResetResult{}, fmt.Errorf("protect Gateway database backup: %w", err)
	}

	transaction, err := connection.BeginTx(ctx, nil)
	if err != nil {
		return ResetResult{}, fmt.Errorf("begin MediaInfo cache reset: %w", err)
	}
	defer transaction.Rollback()
	result, err := transaction.ExecContext(ctx, "DELETE FROM media_info_records")
	if err != nil {
		return ResetResult{}, fmt.Errorf("delete all MediaInfo records: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return ResetResult{}, fmt.Errorf("read deleted MediaInfo count: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return ResetResult{}, fmt.Errorf("commit MediaInfo cache reset: %w", err)
	}
	return ResetResult{DeletedRecords: deleted, BackupPath: backupPath}, nil
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
