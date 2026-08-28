package mediainfo

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestSQLiteStorePersistsRetainedRecordAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mediainfo.db")
	now := time.Unix(1_800_000_000, 0).UTC()
	record := completeRecord(now)

	store, err := OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenSQLite(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded, err := store.LoadCompatibleRetained(ctx, now, 100, testProviderDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Key != record.Key || loaded[0].Media.Streams[0].Codec != "hevc" {
		t.Fatalf("loaded records = %#v", loaded)
	}
}

func TestSQLiteStoreRecordsSchemaVersion(t *testing.T) {
	store, err := OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "mediainfo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != sqliteDatabaseVersion {
		t.Fatalf("schema version = %d", version)
	}
}

func TestSQLiteStoreRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mediainfo.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = OpenSQLite(t.Context(), path)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("OpenSQLite() error = %v", err)
	}
}

func TestSQLiteStoreRejectsLateOlderProbe(t *testing.T) {
	ctx := context.Background()
	store, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "mediainfo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Unix(1_800_000_000, 0).UTC()
	newer := completeRecord(now)
	newer.Media.Streams[0].Codec = "hevc"
	older := completeRecord(now.Add(-time.Hour))
	older.Media.Streams[0].Codec = "h264"
	if err := store.Put(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, older); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(ctx, newer.Key)
	if err != nil || !ok {
		t.Fatalf("Get() = %#v, %v, %v", got, ok, err)
	}
	if got.Media.Streams[0].Codec != "hevc" {
		t.Fatalf("late older probe overwrote record: %#v", got)
	}
}

func TestSQLiteStoreDeletesUnretainedWithoutAffectingStaleOrFresh(t *testing.T) {
	ctx := context.Background()
	store, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "mediainfo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Unix(1_800_000_000, 0).UTC()
	unretained := completeRecord(now.Add(-200 * 24 * time.Hour))
	unretained.ExpiresAt = now.Add(-170 * 24 * time.Hour)
	unretained.LastAccessedAt = now.Add(-200 * 24 * time.Hour)
	unretained.RetainUntil = now.Add(-20 * 24 * time.Hour)
	stale := completeRecord(now.Add(-48 * 24 * time.Hour))
	stale.Key.PartID = "stale"
	stale.ExpiresAt = now.Add(-18 * 24 * time.Hour)
	fresh := completeRecord(now)
	fresh.Key.PartID = "fresh"
	if err := store.Put(ctx, unretained); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteUnretained(ctx, now)
	if err != nil || deleted != 1 {
		t.Fatalf("DeleteExpired() = %d, %v", deleted, err)
	}
	loaded, err := store.LoadCompatibleRetained(ctx, now, 100, testProviderDescriptor())
	if err != nil || len(loaded) != 2 {
		t.Fatalf("LoadCompatibleRetained() = %#v, %v", loaded, err)
	}
}

func TestSQLiteStoreLoadsMostRecentlyAccessedWithinLimit(t *testing.T) {
	store, err := OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "mediainfo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Unix(1_800_000_000, 0).UTC()
	for index, age := range []time.Duration{3 * time.Hour, 2 * time.Hour, time.Hour} {
		record := completeRecord(now.Add(-age))
		record.Key.PartID = string(rune('A' + index))
		if err := store.Put(t.Context(), record); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := store.LoadCompatibleRetained(t.Context(), now, 2, testProviderDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 || loaded[0].Key.PartID != "C" || loaded[1].Key.PartID != "B" {
		t.Fatalf("loaded records = %#v", loaded)
	}
}

func testProviderDescriptor() ProviderDescriptor {
	return ProviderDescriptor{Name: ProviderMediaVaultFFProbe, Revision: ProviderRevisionFFProbeJSONV1}
}

func TestSQLiteStoreDoesNotRestoreOtherProviderRevision(t *testing.T) {
	store, err := OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "mediainfo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Unix(1_800_000_000, 0).UTC()
	if err := store.Put(t.Context(), completeRecord(now)); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadCompatibleRetained(t.Context(), now, 100, ProviderDescriptor{
		Name: ProviderMediaVaultFFProbe, Revision: "ffprobe-json-v2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatalf("incompatible records restored = %d", len(loaded))
	}
}

func TestSQLiteStoreTouchExtendsRetentionMonotonically(t *testing.T) {
	store, err := OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "mediainfo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Unix(1_800_000_000, 0).UTC()
	record := completeRecord(now)
	if err := store.Put(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	accessedAt := now.Add(24 * time.Hour)
	retainUntil := accessedAt.Add(180 * 24 * time.Hour)
	if err := store.Touch(t.Context(), record.Key, accessedAt, retainUntil); err != nil {
		t.Fatal(err)
	}
	if err := store.Touch(t.Context(), record.Key, now, now.Add(31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(t.Context(), record.Key)
	if err != nil || !ok || !got.LastAccessedAt.Equal(accessedAt) || !got.RetainUntil.Equal(retainUntil) {
		t.Fatalf("Get() = %#v, %t, %v", got, ok, err)
	}
}
